package app

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
)

// searchSpecialistHeaderLine reports whether one formatted fetcher output
// line is a redundant leading link/title line. Search result blocks keep the
// search API's own Title:/URL: lines, so the fetcher's copies would only
// duplicate Show Sources entries. Stripping applies to leading lines only:
// post, transcript, and comment bodies are kept verbatim.
func searchSpecialistHeaderLine(trimmedLine string) bool {
	for _, prefix := range []string{"URL:", "Title:", "Thread URL:", "JSON URL:"} {
		if strings.HasPrefix(trimmedLine, prefix) {
			return true
		}
	}

	return false
}

// searchSpecialistFetchTask is one YouTube or Reddit URL to enrich.
type searchSpecialistFetchTask struct {
	rawURL string
	// isReddit selects the Reddit fetcher; otherwise the YouTube fetcher runs.
	isReddit bool
}

// enrichWebSearchResults routes YouTube and Reddit URLs from search API
// results through the dedicated YouTube and Reddit fetchers instead of
// leaving the generic page extract in place. Both fetchers share one
// searchURLFetcherTimeout deadline; URLs that miss it (or fail) keep their
// search snippet. Results without YouTube or Reddit URLs, and bots without
// the fetchers configured, are returned unchanged.
func (instance *bot) enrichWebSearchResults(
	ctx context.Context,
	loadedConfig config,
	results []webSearchResult,
) []webSearchResult {
	youtubeURLs, redditURLs := collectSearchSpecialistURLs(instance, results)
	if len(youtubeURLs)+len(redditURLs) == 0 {
		return results
	}

	fetchCtx, cancel := context.WithTimeout(ctx, searchURLFetcherTimeout)
	defer cancel()

	maxChars := searchSpecialistContentMaxChars(loadedConfig)

	enriched := fetchSearchSpecialistContent(fetchCtx, instance, youtubeURLs, redditURLs, maxChars)

	return mergeSpecialistContent(results, enriched)
}

// specialistURLKind classifies one candidate URL for the early-enrichment
// prefetch: video URLs go to the YouTube fetcher, thread URLs to Reddit.
type specialistURLKind int

const (
	specialistURLKindNone specialistURLKind = iota
	specialistURLKindYouTube
	specialistURLKindReddit
)

// classifySpecialistURL reports which specialist fetcher (if any) can handle
// one search result URL: YouTube video URLs and Reddit thread URLs route to
// their dedicated fetcher, everything else (channels, subreddit listings,
// plain articles) stays on the generic page extract.
func (instance *bot) classifySpecialistURL(rawURL string) specialistURLKind {
	switch {
	case instance.youtube != nil && isYouTubeVideoURL(rawURL):
		return specialistURLKindYouTube
	case instance.reddit != nil && isRedditThreadURL(rawURL):
		return specialistURLKindReddit
	default:
		return specialistURLKindNone
	}
}

// mergeSpecialistContent appends specialist content to each result block
// whose URL was enriched, leaving all other blocks untouched. It is the
// shared merge step for the sequential tail enrichment and the early
// prefetch: both produce the same text for the same enriched URLs.
func mergeSpecialistContent(results []webSearchResult, enriched map[string]string) []webSearchResult {
	if len(enriched) == 0 {
		return results
	}

	mergedResults := make([]webSearchResult, len(results))
	for index, result := range results {
		mergedResults[index] = webSearchResult{
			Query: result.Query,
			Text:  insertSearchSpecialistContent(result.Text, enriched),
		}
	}

	return mergedResults
}

// collectSearchSpecialistURLs returns the deduplicated YouTube video and
// Reddit thread URLs across all results, in first-seen order. URLs for
// unconfigured fetchers are skipped, and non-video YouTube URLs (channels,
// search pages) and non-thread Reddit URLs (subreddit listings, user pages)
// stay on the generic page extract because the specialist fetchers cannot
// parse them.
func collectSearchSpecialistURLs(instance *bot, results []webSearchResult) ([]string, []string) {
	var youtubeURLs, redditURLs []string

	seenURLs := make(map[string]struct{})

	for _, result := range results {
		for _, source := range extractSearchSources(result.Text) {
			rawURL := strings.TrimSpace(source.URL)
			if rawURL == "" {
				continue
			}

			key := strings.ToLower(rawURL)
			if _, seen := seenURLs[key]; seen {
				continue
			}

			seenURLs[key] = struct{}{}

			switch instance.classifySpecialistURL(rawURL) {
			case specialistURLKindYouTube:
				youtubeURLs = append(youtubeURLs, rawURL)
			case specialistURLKindReddit:
				redditURLs = append(redditURLs, rawURL)
			case specialistURLKindNone:
			}
		}
	}

	return youtubeURLs, redditURLs
}

// isRedditThreadURL reports whether the Reddit fetcher can parse the URL.
func isRedditThreadURL(rawURL string) bool {
	_, err := parseRedditThreadURL(rawURL)

	return err == nil
}

// specialistEnrichPrefetch tracks in-flight YouTube/Reddit enrichment
// started while the TinyFish search/fetch work is still running. Query
// search callbacks register each query's freshly searched URLs; the
// sequential tail still merges them into the final texts, so firing the
// prefetch early only overlaps the network waits instead of changing
// results. The mutex guards the dedup set and the merged output map because
// per-query search callbacks run on concurrent task workers.
type specialistEnrichPrefetch struct {
	instance *bot
	maxChars int
	mu       sync.Mutex
	seenURLs map[string]struct{}
	tasks    chan searchSpecialistFetchTask
	enriched map[string]string
	done     chan struct{}
	wait     func()
}

// newSpecialistEnrichPrefetch snapshots the fetchers enabled for this search.
// Nil fetchers stay nil: the callback later drops URLs for unconfigured
// fetchers, matching collectSearchSpecialistURLs. Construction starts one
// background worker that drains queued URLs until drain or cancel; every
// specialist fetch overlaps the TinyFish search/fetch work instead of
// running after it.
func newSpecialistEnrichPrefetch(instance *bot, maxChars int) *specialistEnrichPrefetch {
	prefetch := &specialistEnrichPrefetch{
		instance: instance,
		maxChars: maxChars,
		seenURLs: make(map[string]struct{}),
		tasks:    make(chan searchSpecialistFetchTask, specialistEnrichPrefetchQueueSize),
		enriched: make(map[string]string),
		done:     make(chan struct{}),
	}
	fetchCtx, cancel := prefetch.lifetimeContext()
	prefetch.wait = startSpecialistEnrichWorker(fetchCtx, cancel, prefetch)

	return prefetch
}

// lifetimeContext bounds the background worker: it must cover the whole
// search window (slowest search query plus the bounded TinyFish fetch phase
// plus the prefetch's own fetch batches) without leaking when the caller
// moves on. drain and cancel stop it early.
func (prefetch *specialistEnrichPrefetch) lifetimeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), specialistEnrichPrefetchLifetime)
}

// prefetchURLs queues the specialist URLs of one query's search outcome for
// background enrichment while the rest of the search pipeline runs. Dedup is
// by lower-cased URL, so repeat URLs across related queries fetch once.
// Fetch failures keep the generic snippet, as in the sequential tail. Calls
// after drain or cancel are dropped.
func (prefetch *specialistEnrichPrefetch) prefetchURLs(_ context.Context, urls []string) {
	if prefetch == nil {
		return
	}

	prefetch.mu.Lock()
	if prefetch.instance == nil ||
		(prefetch.instance.youtube == nil && prefetch.instance.reddit == nil) ||
		prefetch.tasks == nil {
		prefetch.mu.Unlock()

		return
	}

	fetchInstance := prefetch.instance
	prefetch.mu.Unlock()

	for _, rawURL := range urls {
		trimmedURL := strings.TrimSpace(rawURL)
		if trimmedURL == "" {
			continue
		}

		var kind specialistURLKind
		if fetchInstance != nil {
			kind = fetchInstance.classifySpecialistURL(trimmedURL)
		}

		var isReddit bool

		switch kind {
		case specialistURLKindYouTube:
		case specialistURLKindReddit:
			isReddit = true
		case specialistURLKindNone:
			continue
		}

		key := strings.ToLower(trimmedURL)

		prefetch.mu.Lock()
		if _, seen := prefetch.seenURLs[key]; seen {
			prefetch.mu.Unlock()

			continue
		}

		prefetch.seenURLs[key] = struct{}{}
		tasks := prefetch.tasks
		prefetch.mu.Unlock()

		if tasks == nil {
			return
		}

		select {
		case tasks <- searchSpecialistFetchTask{rawURL: trimmedURL, isReddit: isReddit}:
		default:
			return
		}
	}
}

// startSpecialistEnrichWorker drains queued specialist URLs one fetch batch
// at a time until drain or cancel closes the queue. Each batch shares one
// searchURLFetcherTimeout deadline; URLs that miss it (or fail) keep their
// search snippet, matching the sequential tail.
func startSpecialistEnrichWorker(
	ctx context.Context,
	cancel context.CancelFunc,
	prefetch *specialistEnrichPrefetch,
) func() {
	workerDone := make(chan struct{})

	safeGo(func() {
		defer close(workerDone)
		defer cancel()

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			prefetch.mu.Lock()
			tasks := prefetch.tasks
			done := prefetch.done
			prefetch.mu.Unlock()

			if tasks == nil || done == nil {
				return
			}

			batch, ok := nextSpecialistEnrichBatch(ctx, tasks, done)
			if !ok {
				return
			}

			prefetch.mu.Lock()
			maxChars := prefetch.maxChars
			fetchInstance := prefetch.instance
			prefetch.mu.Unlock()

			batchCtx, batchCancel := context.WithTimeout(ctx, searchURLFetcherTimeout)
			enriched := fetchSearchSpecialistContent(
				batchCtx, fetchInstance, specialistTaskURLs(batch, false), specialistTaskURLs(batch, true), maxChars,
			)

			batchCancel()
			prefetch.mu.Lock()
			maps.Copy(prefetch.enriched, enriched)
			prefetch.mu.Unlock()
		}
	})

	return func() {
		<-workerDone
	}
}

// nextSpecialistEnrichBatch collects the next queued batch: it blocks for
// the first URL, then drains whatever else already queued without blocking,
// so a burst of per-query callbacks still fetches in one round trip wave.
// A closed tasks channel ends the worker; done only wakes a worker blocked
// waiting for its first URL. Checking tasks first keeps a URL queued just
// before finish from being dropped.
func nextSpecialistEnrichBatch(
	ctx context.Context, tasks chan searchSpecialistFetchTask, done chan struct{},
) ([]searchSpecialistFetchTask, bool) {
	var batch []searchSpecialistFetchTask

	select {
	case firstTask, ok := <-tasks:
		if !ok {
			return nil, false
		}

		batch = append(batch, firstTask)
	case <-done:
		select {
		case firstTask, ok := <-tasks:
			if !ok {
				return nil, false
			}

			batch = append(batch, firstTask)
		default:
			return nil, false
		}
	case <-ctx.Done():
		return nil, false
	}

	for {
		select {
		case task, ok := <-tasks:
			if !ok {
				return batch, true
			}

			batch = append(batch, task)
		default:
			return batch, true
		}
	}
}

// specialistTaskURLs selects the raw URLs of one fetcher kind from a fetch
// batch, preserving first-seen order.
func specialistTaskURLs(tasks []searchSpecialistFetchTask, isReddit bool) []string {
	urls := make([]string, 0, len(tasks))

	for _, task := range tasks {
		if task.isReddit == isReddit {
			urls = append(urls, task.rawURL)
		}
	}

	return urls
}

// finish stops the worker after it drains the queued URLs and merges every
// prefetched result into the search texts, falling back to the sequential
// tail for URLs that arrived too late to prefetch. drain runs first and
// closes the queue under the same mutex that prefetchURLs holds while
// queueing, so no late search callback can queue after the close.
// Results are identical to the sequential path: every enriched URL merges
// the same content, and unenriched URLs keep their generic snippet.
func (prefetch *specialistEnrichPrefetch) finish(
	ctx context.Context, loadedConfig config, results []webSearchResult,
) []webSearchResult {
	if prefetch == nil {
		return results
	}

	enriched := prefetch.drain()
	merged := mergeSpecialistContent(results, enriched)

	return enrichRemainingSpecialistURLs(ctx, prefetch.instance, loadedConfig, merged, enriched)
}

// drain stops the worker after it drains the queued URLs and returns every
// prefetched result without merging, so callers can overlap the drain with
// other work (the TinyFish page fetch) before merging.
func (prefetch *specialistEnrichPrefetch) drain() map[string]string {
	if prefetch == nil {
		return nil
	}

	prefetch.mu.Lock()
	wait := prefetch.wait
	prefetch.wait = nil
	done := prefetch.done
	prefetch.done = nil
	tasks := prefetch.tasks
	prefetch.tasks = nil

	if wait != nil {
		close(done)
		close(tasks)
	}
	prefetch.mu.Unlock()

	if wait == nil {
		prefetch.mu.Lock()
		defer prefetch.mu.Unlock()

		if len(prefetch.enriched) == 0 {
			return nil
		}

		enriched := make(map[string]string, len(prefetch.enriched))
		maps.Copy(enriched, prefetch.enriched)

		return enriched
	}

	wait()

	prefetch.mu.Lock()
	defer prefetch.mu.Unlock()

	enriched := make(map[string]string, len(prefetch.enriched))
	maps.Copy(enriched, prefetch.enriched)
	prefetch.enriched = make(map[string]string)

	return enriched
}

// cancel stops the worker and drops queued URLs without fetching: the search
// failed, so there is nothing to merge into.
func (prefetch *specialistEnrichPrefetch) cancel() {
	if prefetch == nil {
		return
	}

	prefetch.mu.Lock()
	wait := prefetch.wait
	prefetch.wait = nil
	done := prefetch.done
	prefetch.done = nil
	tasks := prefetch.tasks
	prefetch.tasks = nil

	if wait != nil {
		close(done)
		close(tasks)
	}
	prefetch.mu.Unlock()

	if wait == nil {
		return
	}

	wait()
}

// enrichRemainingSpecialistURLs fetches the sequential tail for specialist
// URLs the prefetch missed: URLs whose search arrived after the drain, plus
// prefetched URLs whose fetch failed or timed out.
func enrichRemainingSpecialistURLs(
	ctx context.Context,
	instance *bot,
	loadedConfig config,
	results []webSearchResult,
	prefetched map[string]string,
) []webSearchResult {
	youtubeURLs, redditURLs := collectUnenrichedSpecialistURLs(instance, results, prefetched)

	if len(youtubeURLs)+len(redditURLs) == 0 {
		return results
	}

	fetchCtx, cancel := context.WithTimeout(ctx, searchURLFetcherTimeout)
	defer cancel()

	enriched := fetchRemainingSpecialistBatch(fetchCtx, instance, youtubeURLs, redditURLs, loadedConfig)
	if len(enriched) == 0 {
		return results
	}

	return mergeSpecialistContent(results, enriched)
}

// fetchRemainingSpecialistBatch runs one sequential-tail fetch batch under
// the caller's deadline; URLs that miss it (or fail) keep their snippet.
func fetchRemainingSpecialistBatch(
	ctx context.Context,
	instance *bot,
	youtubeURLs []string,
	redditURLs []string,
	loadedConfig config,
) map[string]string {
	maxChars := searchSpecialistContentMaxChars(loadedConfig)

	return fetchSearchSpecialistContent(ctx, instance, youtubeURLs, redditURLs, maxChars)
}

// collectUnenrichedSpecialistURLs returns the specialist URLs with no
// prefetched content yet, deduplicated in first-seen order. Prefetch
// failures (attempted but missing from prefetched) are retried by the tail:
// a transient 403/5xx during the search window may succeed seconds later.
func collectUnenrichedSpecialistURLs(
	instance *bot,
	results []webSearchResult,
	prefetched map[string]string,
) ([]string, []string) {
	youtubeURLs, redditURLs := collectSearchSpecialistURLs(instance, results)

	filterUnenriched := func(urls []string) []string {
		remaining := make([]string, 0, len(urls))

		for _, rawURL := range urls {
			if _, ok := prefetched[strings.ToLower(strings.TrimSpace(rawURL))]; !ok {
				remaining = append(remaining, rawURL)
			}
		}

		return remaining
	}

	return filterUnenriched(youtubeURLs), filterUnenriched(redditURLs)
}

// fetchSearchSpecialistContent fetches every specialist URL concurrently
// under the caller's deadline and returns the formatted, truncated content
// keyed by lower-cased URL. Failures are logged and skipped: the result
// keeps its generic search snippet.
func fetchSearchSpecialistContent(
	ctx context.Context,
	instance *bot,
	youtubeURLs []string,
	redditURLs []string,
	maxChars int,
) map[string]string {
	tasks := make([]searchSpecialistFetchTask, 0, len(youtubeURLs)+len(redditURLs))
	for _, rawURL := range youtubeURLs {
		tasks = append(tasks, searchSpecialistFetchTask{rawURL: rawURL, isReddit: false})
	}

	for _, rawURL := range redditURLs {
		tasks = append(tasks, searchSpecialistFetchTask{rawURL: rawURL, isReddit: true})
	}

	taskResults := runTasksConcurrently(
		ctx,
		externalRequestConcurrency,
		len(tasks),
		func(taskCtx context.Context, index int) (string, error) {
			task := tasks[index]
			if task.isReddit {
				return fetchSearchRedditContent(taskCtx, instance, task.rawURL, maxChars)
			}

			return fetchSearchYouTubeContent(taskCtx, instance, task.rawURL, maxChars)
		},
	)

	enriched := make(map[string]string, len(tasks))

	for index, taskResult := range taskResults {
		if taskResult.err != nil {
			logWarn("fetch search result url with specialist fetcher", taskResult.err, "url", tasks[index].rawURL)

			continue
		}

		if strings.TrimSpace(taskResult.value) == "" {
			continue
		}

		enriched[strings.ToLower(tasks[index].rawURL)] = taskResult.value
	}

	return enriched
}

// fetchSearchYouTubeContent fetches one search result URL with the YouTube
// fetcher and returns the truncated formatted transcript and comments.
func fetchSearchYouTubeContent(
	ctx context.Context,
	instance *bot,
	rawURL string,
	maxChars int,
) (string, error) {
	content, err := instance.youtube.fetch(ctx, rawURL)
	if err != nil {
		return "", fmt.Errorf("fetch youtube search result %q: %w", rawURL, err)
	}

	formatted := stripSearchSpecialistHeaderLines(formatYouTubeURLContent([]youtubeVideoContent{content}))

	return truncateRunes(formatted, maxChars), nil
}

// fetchSearchRedditContent fetches one search result URL with the Reddit
// fetcher and returns the truncated formatted thread and comments.
func fetchSearchRedditContent(
	ctx context.Context,
	instance *bot,
	rawURL string,
	maxChars int,
) (string, error) {
	content, err := instance.reddit.fetch(ctx, rawURL)
	if err != nil {
		return "", fmt.Errorf("fetch reddit search result %q: %w", rawURL, err)
	}

	formatted := stripSearchSpecialistHeaderLines(formatRedditURLContent([]redditThreadContent{content}))

	return truncateRunes(formatted, maxChars), nil
}

// stripSearchSpecialistHeaderLines drops redundant leading link/title lines
// from formatted fetcher output: the enriched search block already carries
// the search API's own Title:/URL: lines.
func stripSearchSpecialistHeaderLines(formatted string) string {
	lines := strings.Split(formatted, "\n")
	headerEnd := 0

	for headerEnd < len(lines) {
		if !searchSpecialistHeaderLine(strings.TrimSpace(lines[headerEnd])) {
			break
		}

		headerEnd++
	}

	return strings.TrimSpace(strings.Join(lines[headerEnd:], "\n"))
}

// searchSpecialistContentMaxChars returns the truncation budget for embedded
// specialist content: the largest configured per-result cap, so enrichment
// matches the scale the operator already chose for search extracts.
func searchSpecialistContentMaxChars(loadedConfig config) int {
	maxChars := loadedConfig.WebSearch.TinyFish.maxCharsPerResult()
	if parallelChars := loadedConfig.WebSearch.Parallel.maxCharsPerResult(); parallelChars > maxChars {
		maxChars = parallelChars
	}

	if tavilyChars := loadedConfig.WebSearch.Tavily.maxCharsPerResult(); tavilyChars > maxChars {
		maxChars = tavilyChars
	}

	if exaChars := loadedConfig.WebSearch.Exa.textMaxCharacters(); exaChars > maxChars {
		maxChars = exaChars
	}

	if maxChars <= 0 {
		return defaultTinyFishMaxCharsPerResult
	}

	return maxChars
}

// insertSearchSpecialistContent appends specialist content to each result
// block whose URL was enriched, leaving all other blocks untouched.
func insertSearchSpecialistContent(resultText string, enriched map[string]string) string {
	blocks := strings.Split(strings.TrimSpace(resultText), "\n\n")
	changed := false

	for index, block := range blocks {
		blockURL := searchResultBlockURL(block)
		if blockURL == "" {
			continue
		}

		content, ok := enriched[strings.ToLower(blockURL)]
		if !ok {
			continue
		}

		blocks[index] = block + "\n" + content
		changed = true
	}

	if !changed {
		return resultText
	}

	return strings.Join(blocks, "\n\n")
}

// searchResultBlockURL returns the URL of one search result block: the first
// URL: line, mirroring extractSearchSources.
func searchResultBlockURL(block string) string {
	for line := range strings.SplitSeq(block, "\n") {
		trimmedLine := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmedLine, "URL:") {
			continue
		}

		return strings.TrimSpace(strings.TrimPrefix(trimmedLine, "URL:"))
	}

	return ""
}
