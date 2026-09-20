package app

import (
	"context"
	"net/url"
	"strings"
	"sync"
)

// collectVisualSearchFetchURLs returns the content-bearing URLs from visual
// search results in first-seen order: top matches plus site matches.
// Search pages, similar-image pagination, and related-content search links
// carry no page content of their own, so they are excluded.
func collectVisualSearchFetchURLs(results []visualSearchResult) []string {
	urls := make([]string, 0, maxVisualSearchFetchURLs)
	seen := make(map[string]struct{})

	for _, result := range results {
		if !appendVisualSearchFetchURL(&urls, seen, result.TopMatch.URL) {
			return urls
		}

		for _, match := range result.SiteMatches {
			if !appendVisualSearchFetchURL(&urls, seen, match.URL) {
				return urls
			}
		}
	}

	return urls
}

// appendVisualSearchFetchURL adds one normalized URL unless empty, duplicate,
// or the maxVisualSearchFetchURLs budget is reached. It reports whether more
// URLs fit.
func appendVisualSearchFetchURL(urls *[]string, seen map[string]struct{}, rawURL string) bool {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed != "" {
		key := strings.ToLower(trimmed)

		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}

			*urls = append(*urls, trimmed)
		}
	}

	return len(*urls) < maxVisualSearchFetchURLs
}

type partitionedVisualSearchURLs struct {
	facebook []string
	tiktok   []string
	shorts   []string
	youtube  []string
	reddit   []string
	website  []string
}

// partitionVisualSearchFetchURLs routes each URL to its dedicated fetcher by
// host. YouTube Shorts split from long-form YouTube because they use
// different download pipelines. Anything without a special fetcher falls
// through to generic website extraction.
func partitionVisualSearchFetchURLs(urls []string) partitionedVisualSearchURLs {
	var partitioned partitionedVisualSearchURLs

	for _, rawURL := range urls {
		parsed, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil || strings.TrimSpace(parsed.Hostname()) == "" {
			logWarn("skip visual search result url", err, "url", rawURL)

			continue
		}

		host := parsed.Hostname()

		switch {
		case isFacebookHost(host):
			partitioned.facebook = append(partitioned.facebook, rawURL)
		case isTikTokHost(host):
			partitioned.tiktok = append(partitioned.tiktok, rawURL)
		case isRedditHost(host):
			partitioned.reddit = append(partitioned.reddit, rawURL)
		case isYouTubeHost(host):
			switch {
			case isYouTubeShortsURL(rawURL):
				partitioned.shorts = append(partitioned.shorts, rawURL)
			case isYouTubeVideoURL(rawURL):
				partitioned.youtube = append(partitioned.youtube, rawURL)
			default:
				logWarn("skip non-video youtube result url", nil, "url", rawURL)
			}
		default:
			partitioned.website = append(partitioned.website, rawURL)
		}
	}

	return partitioned
}

func isYouTubeVideoURL(rawURL string) bool {
	_, _, err := parseYouTubeVideoURL(rawURL)

	return err == nil
}

type visualSearchURLContents struct {
	website  []websitePageContent
	youtube  []youtubeVideoContent
	reddit   []redditThreadContent
	media    []contentPart
	analyses []string
	warnings []string
}

// visualSearchContentCollector guards concurrent fetch-group writes.
type visualSearchContentCollector struct {
	mutex    sync.Mutex
	contents visualSearchURLContents
}

func newVisualSearchContentCollector() *visualSearchContentCollector {
	return new(visualSearchContentCollector)
}

func (collector *visualSearchContentCollector) appendWarnings(warnings []string) {
	if len(warnings) == 0 {
		return
	}

	collector.mutex.Lock()
	defer collector.mutex.Unlock()

	collector.contents.warnings = append(collector.contents.warnings, warnings...)
}

func (collector *visualSearchContentCollector) setWebsite(contents []websitePageContent) {
	collector.mutex.Lock()
	defer collector.mutex.Unlock()

	collector.contents.website = contents
}

func (collector *visualSearchContentCollector) setYouTube(contents []youtubeVideoContent) {
	collector.mutex.Lock()
	defer collector.mutex.Unlock()

	collector.contents.youtube = contents
}

func (collector *visualSearchContentCollector) setReddit(contents []redditThreadContent) {
	collector.mutex.Lock()
	defer collector.mutex.Unlock()

	collector.contents.reddit = contents
}

func (collector *visualSearchContentCollector) addVideos(media []contentPart, analyses []string, warnings []string) {
	collector.mutex.Lock()
	defer collector.mutex.Unlock()

	collector.contents.media = append(collector.contents.media, media...)
	collector.contents.analyses = append(collector.contents.analyses, analyses...)
	collector.contents.warnings = append(collector.contents.warnings, warnings...)
}

func (collector *visualSearchContentCollector) result() visualSearchURLContents {
	return collector.contents
}

// fetchVisualSearchURLContents fetches every vsearch result URL with its
// dedicated fetcher. Groups run concurrently; URLs inside each group run
// concurrently via the shared helpers. Missing clients skip silently so
// unconfigured fetchers never warn.
func (instance *bot) fetchVisualSearchURLContents(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	urls []string,
) visualSearchURLContents {
	partitioned := partitionVisualSearchFetchURLs(urls)
	collector := newVisualSearchContentCollector()

	var fetchGroup sync.WaitGroup

	if len(partitioned.website) > 0 && instance.website != nil {
		websiteURLs := partitioned.website

		fetchGroup.Add(1)

		safeGo(func() {
			defer fetchGroup.Done()

			fetched, warnings := fetchConcurrentURLContent(
				ctx,
				websiteURLs,
				func(fetchCtx context.Context, rawURL string) (websitePageContent, error) {
					return instance.website.fetch(fetchCtx, loadedConfig, rawURL)
				},
				"fetch visual search website content",
				websiteWarningText,
			)

			collector.setWebsite(fetched)
			collector.appendWarnings(warnings)
		})
	}

	if len(partitioned.youtube) > 0 && instance.youtube != nil {
		fetchGroup.Add(1)

		safeGo(func() {
			defer fetchGroup.Done()

			fetched, warnings := fetchConcurrentURLContent(
				ctx,
				partitioned.youtube,
				instance.youtube.fetch,
				"fetch visual search youtube content",
				youtubeWarningText,
			)

			collector.setYouTube(fetched)
			collector.appendWarnings(warnings)
		})
	}

	if len(partitioned.reddit) > 0 && instance.reddit != nil {
		fetchGroup.Add(1)

		safeGo(func() {
			defer fetchGroup.Done()

			fetched, warnings := fetchConcurrentURLContent(
				ctx,
				partitioned.reddit,
				instance.reddit.fetch,
				"fetch visual search reddit content",
				redditWarningText,
			)

			collector.setReddit(fetched)
			collector.appendWarnings(warnings)
		})
	}

	if len(partitioned.facebook) > 0 && instance.facebook != nil {
		fetchGroup.Add(1)

		safeGo(func() {
			defer fetchGroup.Done()

			fetchVisualSearchVideos(
				ctx,
				instance,
				loadedConfig,
				providerSlashModel,
				partitioned.facebook,
				instance.facebook.fetch,
				facebookWarningText,
				"facebook",
				collector,
			)
		})
	}

	if len(partitioned.tiktok) > 0 && instance.tiktok != nil {
		fetchGroup.Add(1)

		safeGo(func() {
			defer fetchGroup.Done()

			fetchVisualSearchVideos(
				ctx,
				instance,
				loadedConfig,
				providerSlashModel,
				partitioned.tiktok,
				instance.tiktok.fetch,
				tikTokWarningText,
				"tiktok",
				collector,
			)
		})
	}

	if len(partitioned.shorts) > 0 && instance.youtubeShorts != nil {
		fetchGroup.Add(1)

		safeGo(func() {
			defer fetchGroup.Done()

			fetchVisualSearchVideos(
				ctx,
				instance,
				loadedConfig,
				providerSlashModel,
				partitioned.shorts,
				instance.youtubeShorts.fetch,
				youtubeShortsWarningText,
				"youtube shorts",
				collector,
			)
		})
	}

	fetchGroup.Wait()

	return collector.result()
}

// fetchVisualSearchVideos downloads one video group, resolves media parts
// plus text analyses for the reply model, and merges them into contents.
// The generic constraint keeps the fetcher typed while the pipeline only
// needs resolved URLs and media parts. Free function instead of a bot method:
// Go forbids type parameters on methods.
func fetchVisualSearchVideos[T downloadedURLVideoContent](
	ctx context.Context,
	instance *bot,
	loadedConfig config,
	providerSlashModel string,
	videoURLs []string,
	fetcher urlContentFetcher[T],
	warningText string,
	label string,
	collector *visualSearchContentCollector,
) {
	videoContents, fetchWarnings := fetchDownloadedVideos(
		ctx,
		videoURLs,
		fetcher,
		"fetch visual search "+label+" content",
		warningText,
	)

	media, analyses, resolvedWarnings, err := resolveDownloadedVideoAugmentation(
		ctx,
		downloadedVideoAugmentationRequest[T]{
			instance:           instance,
			loadedConfig:       loadedConfig,
			providerSlashModel: providerSlashModel,
			videoContents:      videoContents,
			warnings:           fetchWarnings,
			warningText:        warningText,
			label:              label,
		},
	)
	if err != nil {
		logWarn("resolve visual search "+label+" videos", err)
		collector.appendWarnings([]string{warningText})

		return
	}

	collector.addVideos(media, analyses, resolvedWarnings)
}

// formatVisualSearchFetchedContents renders generic, YouTube, and Reddit page
// bodies for inclusion in the Visual search results section. Video media
// travels as message parts plus analyses instead of text. Output is capped at
// maxVisualSearchFetchedContentRunes so one image cannot flood the prompt.
func formatVisualSearchFetchedContents(contents visualSearchURLContents) string {
	sections := make([]string, 0, visualSearchFetchedSectionCapacity)

	if len(contents.website) > 0 {
		sections = append(sections, formatWebsiteURLContent(contents.website))
	}

	if len(contents.youtube) > 0 {
		sections = append(sections, formatYouTubeURLContent(contents.youtube))
	}

	if len(contents.reddit) > 0 {
		sections = append(sections, formatRedditURLContent(contents.reddit))
	}

	return truncateRunes(strings.Join(sections, "\n\n"), maxVisualSearchFetchedContentRunes)
}
