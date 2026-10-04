package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testSearchYouTubeVideoURL = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	testSearchRedditThreadURL = "https://www.reddit.com/r/golang/comments/1abc123/example_thread/"
)

func newSearchFetcherEnrichmentBot(webSearch webSearcher, youtube youtubeFetcher, reddit redditFetcher) *bot {
	instance := newSearchTestBot(nil, webSearch)
	instance.youtube = youtube
	instance.reddit = reddit

	return instance
}

func runSearchFetcherEnrichmentQuery(
	t *testing.T,
	instance *bot,
	query string,
) []webSearchResult {
	t.Helper()

	results, err := instance.runWebSearchQueries(t.Context(), testSearchConfig(), "openai/main-model", []string{query})
	if err != nil {
		t.Fatalf("runWebSearchQueries() error = %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("results = %#v, want 1 result", results)
	}

	return results
}

func TestEnrichWebSearchResultsRoutesSpecialistURLs(t *testing.T) {
	t.Parallel()

	tests := []searchSpecialistURLCase{
		{
			name:         "youtube",
			resultURL:    testSearchYouTubeVideoURL,
			fetchBody:    "transcript body from youtube fetcher",
			wantFragment: "transcript body from youtube fetcher",
		},
		{
			name:         "reddit",
			resultURL:    testSearchRedditThreadURL,
			fetchBody:    "thread body from reddit fetcher",
			wantFragment: "thread body from reddit fetcher",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			runSearchSpecialistURLCase(t, testCase)
		})
	}
}

type searchSpecialistURLCase struct {
	name         string
	resultURL    string
	fetchBody    string
	wantFragment string
}

func runSearchSpecialistURLCase(t *testing.T, testCase searchSpecialistURLCase) {
	t.Helper()

	var fetchCalls atomic.Int64

	youtube := newStubYouTubeContentClient(func(_ context.Context, rawURL string) (youtubeVideoContent, error) {
		fetchCalls.Add(1)

		return youtubeVideoContent{
			URL:         rawURL,
			VideoID:     "dQw4w9WgXcQ",
			Title:       "Video title",
			ChannelName: "Some channel",
			Transcript:  testCase.fetchBody,
			Comments:    []youtubeComment{{Author: "viewer", Text: "great video"}},
		}, nil
	})
	reddit := newStubRedditContentClient(func(_ context.Context, rawURL string) (redditThreadContent, error) {
		fetchCalls.Add(1)

		return redditThreadContent{
			URL:       rawURL,
			Subreddit: "golang",
			Title:     "Example thread",
			Author:    "someuser",
			Body:      testCase.fetchBody,
			Comments: []redditThreadComment{{
				Author: "commenter",
				Body:   "helpful comment",
			}},
		}, nil
	})
	webSearch := newStubWebSearchClient(func(_ context.Context, _ config, queries []string) ([]webSearchResult, error) {
		return []webSearchResult{{
			Query: queries[0],
			Text: "Title: Result title\nURL: " + testCase.resultURL +
				"\nSnippet:\n| generic search snippet",
		}}, nil
	})

	results := runSearchFetcherEnrichmentQuery(
		t,
		newSearchFetcherEnrichmentBot(webSearch, youtube, reddit),
		testCase.name+" query",
	)

	if got := fetchCalls.Load(); got != 1 {
		t.Fatalf("specialist fetch calls = %d, want 1", got)
	}

	if !strings.Contains(results[0].Text, testCase.wantFragment) {
		t.Fatalf("enriched result missing %q: %q", testCase.wantFragment, results[0].Text)
	}

	if !strings.Contains(results[0].Text, "generic search snippet") {
		t.Fatalf("enriched result dropped the generic snippet: %q", results[0].Text)
	}
}

func TestEnrichWebSearchResultsSharesFiveSecondDeadline(t *testing.T) {
	t.Parallel()

	var (
		youtubeDeadline time.Time
		redditDeadline  time.Time
		youtubeOK       bool
		redditOK        bool
	)

	youtube := newStubYouTubeContentClient(func(ctx context.Context, _ string) (youtubeVideoContent, error) {
		youtubeDeadline, youtubeOK = ctx.Deadline()

		return youtubeVideoContent{
			Title:       "Video title",
			ChannelName: "Some channel",
			Transcript:  "transcript",
		}, nil
	})
	reddit := newStubRedditContentClient(func(ctx context.Context, _ string) (redditThreadContent, error) {
		redditDeadline, redditOK = ctx.Deadline()

		return redditThreadContent{
			Subreddit: "golang",
			Title:     "Example thread",
			Body:      "body",
		}, nil
	})
	webSearch := newStubWebSearchClient(func(_ context.Context, _ config, queries []string) ([]webSearchResult, error) {
		return []webSearchResult{{
			Query: queries[0],
			Text: "Title: Video title\nURL: " + testSearchYouTubeVideoURL +
				"\n\nTitle: Example thread\nURL: " + testSearchRedditThreadURL,
		}}, nil
	})

	instance := newSearchTestBot(nil, webSearch)
	instance.youtube = youtube
	instance.reddit = reddit

	if _, err := instance.runWebSearchQueries(t.Context(), testSearchConfig(), "openai/main-model", []string{"mixed query"}); err != nil {
		t.Fatalf("runWebSearchQueries() error = %v", err)
	}

	if !youtubeOK || !redditOK {
		t.Fatalf("fetchers missing deadline: youtube=%v reddit=%v", youtubeOK, redditOK)
	}

	for name, deadline := range map[string]time.Time{"youtube": youtubeDeadline, "reddit": redditDeadline} {
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > searchURLFetcherTimeout {
			t.Fatalf(
				"%s fetch deadline remaining = %s, want within (0, %s]",
				name,
				remaining,
				searchURLFetcherTimeout,
			)
		}
	}
}

func TestEnrichWebSearchResultsKeepsSnippetOnFetcherFailure(t *testing.T) {
	t.Parallel()

	youtube := newStubYouTubeContentClient(func(_ context.Context, _ string) (youtubeVideoContent, error) {
		return youtubeVideoContent{}, errSearchBackendUnavailable
	})
	reddit := newStubRedditContentClient(func(_ context.Context, _ string) (redditThreadContent, error) {
		return redditThreadContent{}, errSearchBackendUnavailable
	})
	webSearch := newStubWebSearchClient(func(_ context.Context, _ config, queries []string) ([]webSearchResult, error) {
		return []webSearchResult{{
			Query: queries[0],
			Text: "Title: Video title\nURL: " + testSearchYouTubeVideoURL +
				"\nSnippet:\n| generic search snippet",
		}}, nil
	})

	instance := newSearchTestBot(nil, webSearch)
	instance.youtube = youtube
	instance.reddit = reddit

	results, err := instance.runWebSearchQueries(t.Context(), testSearchConfig(), "openai/main-model", []string{"video query"})
	if err != nil {
		t.Fatalf("runWebSearchQueries() error = %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("results = %#v, want 1 result", results)
	}

	if !strings.Contains(results[0].Text, "generic search snippet") {
		t.Fatalf("failed fetch dropped the generic snippet: %q", results[0].Text)
	}
}

func TestEnrichWebSearchResultsSkipsUnfetchableURLs(t *testing.T) {
	t.Parallel()

	var (
		youtubeCalls atomic.Int64
		redditCalls  atomic.Int64
	)

	youtube := newStubYouTubeContentClient(func(_ context.Context, _ string) (youtubeVideoContent, error) {
		youtubeCalls.Add(1)

		return youtubeVideoContent{Transcript: "transcript"}, nil
	})
	reddit := newStubRedditContentClient(func(_ context.Context, _ string) (redditThreadContent, error) {
		redditCalls.Add(1)

		return redditThreadContent{Body: "body"}, nil
	})
	webSearch := newStubWebSearchClient(func(_ context.Context, _ config, queries []string) ([]webSearchResult, error) {
		return []webSearchResult{{
			Query: queries[0],
			Text: "Title: Some channel\nURL: https://www.youtube.com/@somechannel\n" +
				"\n\nTitle: Subreddit page\nURL: https://www.reddit.com/r/golang/\n" +
				"\n\nTitle: Plain article\nURL: https://example.com/article\nSnippet:\n| article snippet",
		}}, nil
	})

	instance := newSearchTestBot(nil, webSearch)
	instance.youtube = youtube
	instance.reddit = reddit

	results, err := instance.runWebSearchQueries(t.Context(), testSearchConfig(), "openai/main-model", []string{"mixed query"})
	if err != nil {
		t.Fatalf("runWebSearchQueries() error = %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("results = %#v, want 1 result", results)
	}

	if got := youtubeCalls.Load(); got != 0 {
		t.Fatalf("youtube fetch calls = %d, want 0", got)
	}

	if got := redditCalls.Load(); got != 0 {
		t.Fatalf("reddit fetch calls = %d, want 0", got)
	}

	if !strings.Contains(results[0].Text, "article snippet") {
		t.Fatalf("unfetchable URLs changed the result text: %q", results[0].Text)
	}
}

func TestSpecialistPrefetchOverlapsSearchFetch(t *testing.T) {
	t.Parallel()

	const (
		youtubeURL = testSearchYouTubeVideoURL
		redditURL  = testSearchRedditThreadURL
	)

	searchRelease := make(chan struct{})
	fetchStarted := make(chan string, 2)
	fetchRelease := make(chan struct{})

	youtube := newStubYouTubeContentClient(newOverlapYouTubeFetch(fetchStarted, fetchRelease))
	reddit := newStubRedditContentClient(newOverlapRedditFetch(fetchStarted, fetchRelease))
	instance, loadedConfig := newOverlapSearchFixture(
		t, youtube, reddit, searchRelease, youtubeURL, redditURL,
	)

	queryDone := make(chan []webSearchResult, 1)
	errDone := make(chan error, 1)

	go func() {
		results, err := instance.runWebSearchQueries(
			context.Background(),
			loadedConfig,
			"openai/main-model",
			[]string{"first query", "second query"},
		)
		if err != nil {
			errDone <- err

			return
		}

		queryDone <- results
	}()

	// The first query's search blocks until searchRelease closes, while the
	// second query's search returns immediately and fires the prefetch: the
	// first specialist fetch must start before searchRelease closes, so
	// enrichment demonstrably overlaps the search phase instead of
	// serializing after it.
	select {
	case <-fetchStarted:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for the prefetch fetch to start during search")
	}

	close(searchRelease)
	close(fetchRelease)

	var results []webSearchResult

	select {
	case err := <-errDone:
		t.Fatalf("runWebSearchQueries() error = %v", err)
	case results = <-queryDone:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for runWebSearchQueries")
	}

	if len(results) != 2 {
		t.Fatalf("results = %#v, want 2 results", results)
	}

	if !strings.Contains(results[0].Text, "prefetched transcript") {
		t.Fatalf("first result missing prefetched youtube content: %q", results[0].Text)
	}

	if !strings.Contains(results[1].Text, "prefetched thread body") {
		t.Fatalf("second result missing prefetched reddit content: %q", results[1].Text)
	}
}

func newOverlapSearchFixture(
	t *testing.T,
	youtube youtubeFetcher,
	reddit redditFetcher,
	searchRelease chan struct{},
	youtubeURL string,
	redditURL string,
) (*bot, config) {
	t.Helper()

	searchServer, fetchServer := newSpecialistPrefetchSearchFetchServers(t, map[string]string{
		"first query":  youtubeURL,
		"second query": redditURL,
	}, func(query string) {
		if query == "first query" {
			select {
			case <-searchRelease:
			case <-time.After(10 * time.Second):
			}
		}
	})
	t.Cleanup(searchServer.Close)
	t.Cleanup(fetchServer.Close)

	tinyFish := newTinyFishSearchClient(searchServer.Client())
	tinyFish.searchEndpoint = searchServer.URL
	tinyFish.fetchEndpoint = fetchServer.URL
	tinyFish.fetchDeadline = 10 * time.Second

	instance := newSearchTestBot(nil, routedWebSearchClient{tinyFish: tinyFish})
	instance.youtube = youtube
	instance.reddit = reddit

	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.TinyFish = tinyFishSearchConfig{
		APIKey:            "tf-test-key",
		APIKeys:           []string{"tf-test-key"},
		MaxCharsPerResult: defaultTinyFishMaxCharsPerResult,
	}

	return instance, loadedConfig
}

func newOverlapYouTubeFetch(
	fetchStarted chan string,
	fetchRelease chan struct{},
) func(context.Context, string) (youtubeVideoContent, error) {
	return func(_ context.Context, rawURL string) (youtubeVideoContent, error) {
		select {
		case fetchStarted <- "youtube:" + rawURL:
		default:
		}

		select {
		case <-fetchRelease:
		case <-time.After(10 * time.Second):
		}

		return youtubeVideoContent{
			URL:         rawURL,
			VideoID:     "dQw4w9WgXcQ",
			Title:       "Video title",
			ChannelName: "Some channel",
			Transcript:  "prefetched transcript",
		}, nil
	}
}

func newOverlapRedditFetch(
	fetchStarted chan string,
	fetchRelease chan struct{},
) func(context.Context, string) (redditThreadContent, error) {
	return func(_ context.Context, rawURL string) (redditThreadContent, error) {
		select {
		case fetchStarted <- "reddit:" + rawURL:
		default:
		}

		select {
		case <-fetchRelease:
		case <-time.After(10 * time.Second):
		}

		return redditThreadContent{
			URL:       rawURL,
			Subreddit: "golang",
			Title:     "Example thread",
			Author:    "someuser",
			Body:      "prefetched thread body",
		}, nil
	}
}
func TestSpecialistPrefetchDedupesSharedURLs(t *testing.T) {
	t.Parallel()

	var fetchCalls atomic.Int64

	youtube := newStubYouTubeContentClient(func(_ context.Context, rawURL string) (youtubeVideoContent, error) {
		fetchCalls.Add(1)

		return youtubeVideoContent{
			URL:         rawURL,
			VideoID:     "dQw4w9WgXcQ",
			Title:       "Video title",
			ChannelName: "Some channel",
			Transcript:  "shared transcript",
		}, nil
	})

	searchServer, fetchServer := newSpecialistPrefetchSearchFetchServers(t, map[string]string{
		"first query":  testSearchYouTubeVideoURL,
		"second query": testSearchYouTubeVideoURL,
	}, nil)
	defer searchServer.Close()
	defer fetchServer.Close()

	tinyFish := newTinyFishSearchClient(searchServer.Client())
	tinyFish.searchEndpoint = searchServer.URL
	tinyFish.fetchEndpoint = fetchServer.URL
	tinyFish.fetchDeadline = 10 * time.Second

	instance := newSearchTestBot(nil, routedWebSearchClient{tinyFish: tinyFish})
	instance.youtube = youtube

	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.TinyFish = tinyFishSearchConfig{
		APIKey:            "tf-test-key",
		APIKeys:           []string{"tf-test-key"},
		MaxCharsPerResult: defaultTinyFishMaxCharsPerResult,
	}

	results, err := instance.runWebSearchQueries(t.Context(), loadedConfig, "openai/main-model", []string{"first query", "second query"})
	if err != nil {
		t.Fatalf("runWebSearchQueries() error = %v", err)
	}

	// Both queries share one URL: the prefetch dedupes it to a single
	// background fetch, and the sequential tail sees the prefetched content
	// and fetches nothing more.
	if got := fetchCalls.Load(); got != 1 {
		t.Fatalf("specialist fetch calls = %d, want 1", got)
	}

	if len(results) != 2 {
		t.Fatalf("results = %#v, want 2 results", results)
	}

	for index, result := range results {
		if !strings.Contains(result.Text, "shared transcript") {
			t.Fatalf("result %d missing prefetched content: %q", index, result.Text)
		}
	}
}

func TestSpecialistPrefetchFallsBackToSequentialTail(t *testing.T) {
	t.Parallel()

	var fetchCalls atomic.Int64

	youtube := newStubYouTubeContentClient(func(_ context.Context, rawURL string) (youtubeVideoContent, error) {
		if fetchCalls.Add(1) == 1 {
			return youtubeVideoContent{}, errSearchBackendUnavailable
		}

		return youtubeVideoContent{
			URL:         rawURL,
			VideoID:     "dQw4w9WgXcQ",
			Title:       "Video title",
			ChannelName: "Some channel",
			Transcript:  "tail transcript",
		}, nil
	})

	searchServer, fetchServer := newSpecialistPrefetchSearchFetchServers(t, map[string]string{
		"flaky query": testSearchYouTubeVideoURL,
	}, nil)
	defer searchServer.Close()
	defer fetchServer.Close()

	tinyFish := newTinyFishSearchClient(searchServer.Client())
	tinyFish.searchEndpoint = searchServer.URL
	tinyFish.fetchEndpoint = fetchServer.URL
	tinyFish.fetchDeadline = 10 * time.Second

	instance := newSearchTestBot(nil, routedWebSearchClient{tinyFish: tinyFish})
	instance.youtube = youtube

	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.TinyFish = tinyFishSearchConfig{
		APIKey:            "tf-test-key",
		APIKeys:           []string{"tf-test-key"},
		MaxCharsPerResult: defaultTinyFishMaxCharsPerResult,
	}

	results, err := instance.runWebSearchQueries(t.Context(), loadedConfig, "openai/main-model", []string{"flaky query"})
	if err != nil {
		t.Fatalf("runWebSearchQueries() error = %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("results = %#v, want 1 result", results)
	}

	// The prefetch fails and the prefetch-aware tail retries once and
	// succeeds: two fetches total, with the answer intact.
	if got := fetchCalls.Load(); got != 2 {
		t.Fatalf("specialist fetch calls = %d, want 2", got)
	}

	if !strings.Contains(results[0].Text, "tail transcript") {
		t.Fatalf("enriched result missing tail content: %q", results[0].Text)
	}

	if !strings.Contains(results[0].Text, "Snippet for flaky query") {
		t.Fatalf("enriched result dropped the generic snippet: %q", results[0].Text)
	}
}

func newSpecialistPrefetchSearchFetchServers(
	t *testing.T,
	urlByQuery map[string]string,
	recordSearch func(string),
) (*httptest.Server, *httptest.Server) {
	t.Helper()

	searchServer := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")

		rawURL, ok := urlByQuery[query]
		if !ok {
			rawURL = ""
		}

		results := make([]any, 0, 1)
		if rawURL != "" {
			results = append(results, map[string]any{
				"position":  1,
				"site_name": "example.com",
				"title":     "Title for " + query,
				"snippet":   "Snippet for " + query,
				"url":       rawURL,
			})
		}

		if recordSearch != nil {
			recordSearch(query)
		}

		responseWriter.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(responseWriter).Encode(map[string]any{
			"query":         query,
			"results":       results,
			"total_results": len(results),
			"page":          0,
		}); err != nil {
			t.Errorf("encode specialist prefetch search response: %v", err)
		}
	}))

	// The TinyFish page fetch finds no extractable body for the specialist
	// URLs, so the result texts keep their snippets and the prefetch is the
	// only enrichment source under test.
	fetchServer := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(responseWriter).Encode(map[string]any{
			"results": []any{},
			"errors":  []any{},
		}); err != nil {
			t.Errorf("encode specialist prefetch fetch response: %v", err)
		}
	}))

	return searchServer, fetchServer
}
