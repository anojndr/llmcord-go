package app

import (
	"context"
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
