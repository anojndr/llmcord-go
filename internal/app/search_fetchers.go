package app

import (
	"context"
	"fmt"
	"strings"
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
	if len(enriched) == 0 {
		return results
	}

	enrichedResults := make([]webSearchResult, len(results))
	for index, result := range results {
		enrichedResults[index] = webSearchResult{
			Query: result.Query,
			Text:  insertSearchSpecialistContent(result.Text, enriched),
		}
	}

	return enrichedResults
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

			switch {
			case instance.youtube != nil && isYouTubeVideoURL(rawURL):
				youtubeURLs = append(youtubeURLs, rawURL)
			case instance.reddit != nil && isRedditThreadURL(rawURL):
				redditURLs = append(redditURLs, rawURL)
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
