package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type stubRedditContentClient struct {
	mu      sync.Mutex
	calls   []string
	fetchFn func(context.Context, string) (redditThreadContent, error)
}

func (client *stubRedditContentClient) fetch(
	ctx context.Context,
	rawURL string,
) (redditThreadContent, error) {
	client.mu.Lock()
	client.calls = append(client.calls, rawURL)
	client.mu.Unlock()

	return client.fetchFn(ctx, rawURL)
}

func newStubRedditContentClient(
	fetchFn func(context.Context, string) (redditThreadContent, error),
) *stubRedditContentClient {
	client := new(stubRedditContentClient)
	client.fetchFn = fetchFn

	return client
}

func newRedditTestBot(reddit redditContentClient) *bot {
	instance := new(bot)
	instance.reddit = reddit

	return instance
}

func TestAugmentedUserPromptRenderUsesRedditTemplate(t *testing.T) {
	t.Parallel()

	prompt := augmentedUserPrompt{
		RepliedMessage:   "",
		UserQuery:        "<@123>: summarize this thread",
		YouTubeContent:   "",
		RedditContent:    "Thread URL: https://www.reddit.com/r/testing/comments/abc123/thread-title/",
		WebsiteContent:   "",
		DocumentContent:  "",
		VisualSearch:     "",
		WebSearchResults: "",
	}

	renderedPrompt := prompt.render()

	expectedPrompt := strings.Join([]string{
		"Answer the user's query based on the extracted Reddit URL content.",
		"User query:\n<@123>: summarize this thread",
		"Reddit URL content:\nThread URL: https://www.reddit.com/r/testing/comments/abc123/thread-title/",
	}, "\n\n")

	if renderedPrompt != expectedPrompt {
		t.Fatalf("unexpected rendered prompt: got %q want %q", renderedPrompt, expectedPrompt)
	}
}

func TestExtractRedditURLsNormalizesAndDeduplicates(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"check https://www.reddit.com/r/testing/comments/abc123/thread-title/",
		"and old.reddit.com/r/testing/comments/abc123/thread-title/?utm_source=share",
		"plus https://www.reddit.com/r/testing/comments/def456/second-thread",
	}, " ")

	urls := extractRedditURLs(text)

	expected := []string{
		"https://www.reddit.com/r/testing/comments/abc123/thread-title.json?depth=10&limit=500&raw_json=1",
		"https://www.reddit.com/r/testing/comments/def456/second-thread.json?depth=10&limit=500&raw_json=1",
	}

	if len(urls) != len(expected) {
		t.Fatalf("unexpected url count: got %d want %d (%#v)", len(urls), len(expected), urls)
	}

	for index, expectedURL := range expected {
		if urls[index] != expectedURL {
			t.Fatalf("unexpected url at index %d: got %q want %q", index, urls[index], expectedURL)
		}
	}
}

func TestExtractRedditURLsIgnoresURLsInAugmentedPromptSections(t *testing.T) {
	t.Parallel()

	text := augmentedUserPrompt{
		RepliedMessage:   "",
		UserQuery:        "<@123>: summarize this thread",
		YouTubeContent:   "Thread URL: https://www.reddit.com/r/testing/comments/abc123/thread-title/",
		RedditContent:    "Thread URL: https://www.reddit.com/r/testing/comments/def456/second-thread/",
		WebsiteContent:   "Discussed on https://www.reddit.com/r/testing/comments/ghi789/third-thread/",
		DocumentContent:  "Archive: https://www.reddit.com/r/testing/comments/pqr678/sixth-thread/",
		VisualSearch:     "Site match: old.reddit.com/r/testing/comments/jkl012/fourth-thread/",
		WebSearchResults: "1. https://www.reddit.com/r/testing/comments/mno345/fifth-thread/",
	}.render()

	urls := extractRedditURLs(text)
	if len(urls) != 0 {
		t.Fatalf("unexpected urls: %#v", urls)
	}
}

func TestAppendWebSearchResultsKeepsOriginalUserQueryAfterRedditAugmentation(t *testing.T) {
	t.Parallel()

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: "<@123>: summarize this reddit thread",
		},
	}

	augmentedConversation, err := appendRedditContentToConversation(
		conversation,
		"Thread URL: https://www.reddit.com/r/testing/comments/abc123/thread-title/\nTitle: Example",
	)
	if err != nil {
		t.Fatalf("append reddit content: %v", err)
	}

	augmentedConversation, err = appendWebSearchResultsToConversation(
		augmentedConversation,
		"Query: example\nResults:\ncontext",
	)
	if err != nil {
		t.Fatalf("append web search results: %v", err)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	prompt := parseAugmentedUserPrompt(content)

	if prompt.UserQuery != "<@123>: summarize this reddit thread" {
		t.Fatalf("unexpected user query: %q", prompt.UserQuery)
	}

	if !containsFold(prompt.RedditContent, "Title: Example") {
		t.Fatalf("expected reddit content to be preserved: %q", prompt.RedditContent)
	}

	if !containsFold(prompt.WebSearchResults, "Query: example") {
		t.Fatalf("expected web search results to be appended: %q", prompt.WebSearchResults)
	}
}

func TestAppendRedditContentToConversationPreservesImages(t *testing.T) {
	t.Parallel()

	assertContextAugmentationPreservesImages(
		t,
		"<@123>: summarize https://www.reddit.com/r/testing/comments/abc123/thread-title/",
		"Thread URL: https://www.reddit.com/r/testing/comments/abc123/thread-title/\nTitle: Example",
		redditSectionName,
		appendRedditContentToConversation,
	)
}

func TestMaybeAugmentConversationWithRedditFetchesMultipleURLsConcurrentlyAndKeepsOrder(t *testing.T) {
	t.Parallel()

	var (
		startedCount int
		startedMu    sync.Mutex
		release      = make(chan struct{})
	)

	reddit := newStubRedditContentClient(func(
		_ context.Context,
		rawURL string,
	) (redditThreadContent, error) {
		startedMu.Lock()

		startedCount++
		if startedCount == 2 {
			close(release)
		}
		startedMu.Unlock()

		<-release

		title := "First Thread"
		if strings.Contains(rawURL, "def456") {
			title = "Second Thread"
		}

		return redditThreadContent{
			URL:         strings.TrimSuffix(rawURL, ".json?depth=10&limit=500&raw_json=1"),
			JSONURL:     rawURL,
			Subreddit:   "r/testing",
			Title:       title,
			Author:      "poster",
			Body:        "Post body",
			Score:       10,
			UpvoteRatio: 0.9,
			NumComments: 2,
			CreatedUTC:  1735179615,
			LinkedURL:   "",
			Comments:    nil,
		}, nil
	})

	instance := newRedditTestBot(reddit)

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: strings.Join([]string{
				"<@123>: summarize these",
				"https://www.reddit.com/r/testing/comments/abc123/thread-title/",
				"and https://www.reddit.com/r/testing/comments/def456/second-thread/",
			}, " "),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithReddit(
		ctx,
		conversation,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with reddit: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	firstIndex := strings.Index(content, "Title: First Thread")

	secondIndex := strings.Index(content, "Title: Second Thread")
	if firstIndex == -1 || secondIndex == -1 || firstIndex >= secondIndex {
		t.Fatalf("expected reddit results to preserve url order: %q", content)
	}

	if len(reddit.calls) != 2 {
		t.Fatalf("unexpected fetch call count: %d", len(reddit.calls))
	}
}

func TestMaybeAugmentConversationWithRedditIgnoresURLsOnlyPresentInDocumentContent(t *testing.T) {
	t.Parallel()

	reddit := newStubRedditContentClient(func(
		_ context.Context,
		rawURL string,
	) (redditThreadContent, error) {
		t.Fatalf("unexpected reddit fetch for %q", rawURL)

		return redditThreadContent{
			URL:         "",
			JSONURL:     "",
			Subreddit:   "",
			Title:       "",
			Author:      "",
			Body:        "",
			Score:       0,
			UpvoteRatio: 0,
			NumComments: 0,
			CreatedUTC:  0,
			LinkedURL:   "",
			Comments:    nil,
		}, nil
	})

	instance := newRedditTestBot(reddit)

	assertURLAugmentationIgnoresDocumentOnlyURLs(
		t,
		"https://www.reddit.com/r/testing/comments/abc123/thread-title/",
		func(
			ctx context.Context,
			conversation []chatMessage,
			urlExtractionText string,
		) ([]chatMessage, []string, error) {
			return instance.maybeAugmentConversationWithReddit(
				ctx,
				conversation,
				urlExtractionText,
			)
		},
	)

	if len(reddit.calls) != 0 {
		t.Fatalf("unexpected fetch call count: %d", len(reddit.calls))
	}
}

func TestRedditClientFetchAppendsJSONSuffixAndCapturesComments(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/r/testing/comments/abc123/thread-title.json" {
			t.Fatalf("unexpected request path: %q", request.URL.Path)
		}

		if request.URL.Query().Get("depth") != redditDefaultDepth {
			t.Fatalf("unexpected depth query: %q", request.URL.Query().Get("depth"))
		}

		if request.URL.Query().Get("limit") != redditDefaultLimit {
			t.Fatalf("unexpected limit query: %q", request.URL.Query().Get("limit"))
		}

		if request.URL.Query().Get("raw_json") != redditRawJSONValue {
			t.Fatalf("unexpected raw_json query: %q", request.URL.Query().Get("raw_json"))
		}

		if request.Header.Get("User-Agent") == "" {
			t.Fatal("expected user agent header")
		}

		writer.Header().Set("Content-Type", "application/json")

		_, err := writer.Write([]byte(mockRedditThreadResponse(t)))
		if err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))
	defer server.Close()

	client := redditClient{
		httpClient: server.Client(),
		baseURL:    server.URL,
		userAgent:  youtubeUserAgent,
	}

	result, err := client.fetch(
		context.Background(),
		"https://www.reddit.com/r/testing/comments/abc123/thread-title/",
	)
	if err != nil {
		t.Fatalf("fetch reddit content: %v", err)
	}

	if result.URL != "https://www.reddit.com/r/testing/comments/abc123/thread-title/" {
		t.Fatalf("unexpected canonical url: %q", result.URL)
	}

	expectedJSONURL := "https://www.reddit.com/r/testing/comments/abc123/thread-title.json?depth=10&limit=500&raw_json=1"
	if result.JSONURL != expectedJSONURL {
		t.Fatalf("unexpected json url: got %q want %q", result.JSONURL, expectedJSONURL)
	}

	if result.Title != "Thread title" {
		t.Fatalf("unexpected title: %q", result.Title)
	}

	if result.Author != "poster" {
		t.Fatalf("unexpected author: %q", result.Author)
	}

	if len(result.Comments) != 2 {
		t.Fatalf("unexpected top-level comment count: %d", len(result.Comments))
	}

	if len(result.Comments[0].Replies) != 1 {
		t.Fatalf("unexpected nested reply count: %d", len(result.Comments[0].Replies))
	}

	formattedContent := formatRedditURLContent([]redditThreadContent{result})
	if !containsFold(formattedContent, "1.1. Author: reply") {
		t.Fatalf("expected nested reply in formatted content: %q", formattedContent)
	}

	if !containsFold(formattedContent, "Linked URL: https://example.com/article") {
		t.Fatalf("expected linked url in formatted content: %q", formattedContent)
	}
}

func TestNewRedditClientForcesHTTP1WhenServerBlocksHTTP2(t *testing.T) {
	t.Parallel()

	requestProtoMajor := make(chan int, 1)

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		select {
		case requestProtoMajor <- request.ProtoMajor:
		default:
		}

		if request.ProtoMajor == 2 {
			writer.Header().Set("Content-Type", "text/html")
			writer.WriteHeader(http.StatusForbidden)

			_, err := writer.Write([]byte("<body>You've been blocked by network security.</body>"))
			if err != nil {
				t.Fatalf("write blocked response: %v", err)
			}

			return
		}

		writer.Header().Set("Content-Type", "application/json")

		_, err := writer.Write([]byte(mockRedditThreadResponse(t)))
		if err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))
	server.EnableHTTP2 = true

	server.StartTLS()
	defer server.Close()

	client := newRedditClient(server.Client())
	client.baseURL = server.URL

	result, err := client.fetch(
		context.Background(),
		"https://www.reddit.com/r/testing/comments/abc123/thread-title/",
	)
	if err != nil {
		t.Fatalf("fetch reddit content over forced http/1.1: %v", err)
	}

	if <-requestProtoMajor != 1 {
		t.Fatalf("expected reddit client to use http/1.1")
	}

	if result.Title != "Thread title" {
		t.Fatalf("unexpected title: %q", result.Title)
	}
}

func mockRedditThreadResponse(t *testing.T) string {
	t.Helper()

	payload := []any{
		map[string]any{
			"kind": "Listing",
			"data": map[string]any{
				"children": []any{
					map[string]any{
						"kind": "t3",
						"data": map[string]any{
							"author":                  "poster",
							"created_utc":             1735179615,
							"num_comments":            3,
							"permalink":               "/r/testing/comments/abc123/thread-title/",
							"score":                   42,
							"selftext":                "Original post body",
							"subreddit":               "testing",
							"subreddit_name_prefixed": "r/testing",
							"title":                   "Thread title",
							"upvote_ratio":            0.97,
							"url":                     "https://example.com/article",
						},
					},
				},
			},
		},
		map[string]any{
			"kind": "Listing",
			"data": map[string]any{
				"children": []any{
					map[string]any{
						"kind": "t1",
						"data": map[string]any{
							"author":      "first",
							"body":        "First comment",
							"created_utc": 1735179616,
							"permalink":   "/r/testing/comments/abc123/thread-title/c1/",
							"score":       12,
							"replies": map[string]any{
								"kind": "Listing",
								"data": map[string]any{
									"children": []any{
										map[string]any{
											"kind": "t1",
											"data": map[string]any{
												"author":      "reply",
												"body":        "Nested reply",
												"created_utc": 1735179617,
												"permalink":   "/r/testing/comments/abc123/thread-title/c2/",
												"score":       3,
												"replies":     "",
											},
										},
									},
								},
							},
						},
					},
					map[string]any{
						"kind": "t1",
						"data": map[string]any{
							"author":      "second",
							"body":        "Second comment",
							"created_utc": 1735179618,
							"permalink":   "/r/testing/comments/abc123/thread-title/c3/",
							"score":       8,
							"replies":     "",
						},
					},
				},
			},
		},
	}

	responseBody, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal reddit thread response: %v", err)
	}

	return string(responseBody)
}
