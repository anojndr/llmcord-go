package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type stubWebsiteContentClient struct {
	mu      sync.Mutex
	calls   []string
	fetchFn func(context.Context, string) (websitePageContent, error)
}

func (client *stubWebsiteContentClient) fetch(
	ctx context.Context,
	rawURL string,
) (websitePageContent, error) {
	client.mu.Lock()
	client.calls = append(client.calls, rawURL)
	client.mu.Unlock()

	return client.fetchFn(ctx, rawURL)
}

func newStubWebsiteContentClient(
	fetchFn func(context.Context, string) (websitePageContent, error),
) *stubWebsiteContentClient {
	client := new(stubWebsiteContentClient)
	client.fetchFn = fetchFn

	return client
}

func newWebsiteTestBot(website websiteContentClient) *bot {
	instance := new(bot)
	instance.website = website

	return instance
}

func TestExtractWebsiteURLsNormalizesDeduplicatesAndSkipsSpecializedHosts(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"read https://en.wikipedia.org/wiki/Go_(programming_language)#History",
		"and https://en.wikipedia.org/wiki/Go_(programming_language),",
		"plus https://example.com/article?ref=1.",
		"ignore https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"ignore https://www.reddit.com/r/testing/comments/abc123/thread-title/",
		"ignore https://www.tiktok.com/@example/video/1234567890123456789",
		"ignore https://www.facebook.com/reel/823513456342882",
	}, " ")

	urls := extractWebsiteURLs(text)

	expected := []string{
		"https://en.wikipedia.org/wiki/Go_(programming_language)",
		"https://example.com/article?ref=1",
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

func TestExtractWebsiteURLsRequiresExplicitScheme(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"ignore google.com and www.google.com/search?q=test",
		"but keep https://www.google.com/ and http://example.com/path.",
	}, " ")

	urls := extractWebsiteURLs(text)

	expected := []string{
		"https://www.google.com/",
		"http://example.com/path",
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

func TestExtractWebsiteURLsIgnoresURLsInAugmentedPromptSections(t *testing.T) {
	t.Parallel()

	text := augmentedUserPrompt{
		RepliedMessage:   "",
		UserQuery:        "<@123>: summarize these sources",
		YouTubeContent:   "Transcript source: https://example.com/from-youtube",
		RedditContent:    "Linked article: https://example.com/from-reddit",
		WebsiteContent:   "URL: https://example.com/original",
		DocumentContent:  "Extracted source: https://example.com/from-document",
		VisualSearch:     "Site match: https://example.com/from-visual",
		WebSearchResults: "1. https://example.com/from-search",
	}.render()

	urls := extractWebsiteURLs(text)
	if len(urls) != 0 {
		t.Fatalf("unexpected urls: %#v", urls)
	}
}

func TestExtractWebsiteURLsIgnoresNonURLLogIdentifiers(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"sweetpotet@DESKTOP-FRKOURV:~$ kitty",
		"libEGL warning: failed to get driver name for fd -1",
		"",
		"libEGL warning: MESA-LOADER: failed to retrieve device information",
		"",
		"libEGL warning: failed to get driver name for fd -1",
		"",
		"MESA: error: ZINK: failed to choose pdev",
		"libEGL warning: egl: failed to create dri2 screen",
		"[0.942] [glfw error 65544]: Notify: Failed to get server capabilities error: " +
			"org.freedesktop.DBus.Error.NoReply: Did not receive a reply. Possible causes include: " +
			"the remote application did not send a reply, the message bus security policy blocked the " +
			"reply, the reply timeout expired, or the network connection was broken.",
		"[0.942] [glfw error 65544]: process_desktop_settings: failed with error: " +
			"org.freedesktop.DBus.Error.ServiceUnknown: The name org.freedesktop.portal.Desktop " +
			"was not provided by any .service files",
		"ignoreboth or ignorespace present in bash HISTCONTROL setting, showing running command will " +
			"not be robust",
	}, "\n")

	urls := extractWebsiteURLs(text)
	if len(urls) != 0 {
		t.Fatalf("unexpected urls: %#v", urls)
	}
}

func TestAppendWebsiteContentToConversationPreservesImages(t *testing.T) {
	t.Parallel()

	assertContextAugmentationPreservesImages(
		t,
		"<@123>: summarize https://en.wikipedia.org/wiki/Go_(programming_language)",
		"URL: https://en.wikipedia.org/wiki/Go_(programming_language)\nTitle: Go\nContent:\nGo is a programming language.",
		websiteSectionName,
		appendWebsiteContentToConversation,
	)
}

func TestMaybeAugmentConversationWithWebsiteFetchesMultipleURLsConcurrentlyAndKeepsOrder(t *testing.T) {
	t.Parallel()

	var (
		startedCount int
		startedMu    sync.Mutex
		release      = make(chan struct{})
	)

	website := newStubWebsiteContentClient(func(
		_ context.Context,
		rawURL string,
	) (websitePageContent, error) {
		startedMu.Lock()

		startedCount++
		if startedCount == 2 {
			close(release)
		}
		startedMu.Unlock()

		<-release

		title := "Example Article"
		if strings.Contains(rawURL, "wikipedia") {
			title = "Wikipedia Entry"
		}

		return websitePageContent{
			URL:         rawURL,
			Title:       title,
			Description: "",
			Content:     "Content for " + rawURL,
		}, nil
	})

	instance := newWebsiteTestBot(website)

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: strings.Join([]string{
				"<@123>: compare these",
				"https://en.wikipedia.org/wiki/Go_(programming_language)",
				"and https://example.com/article",
			}, " "),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithWebsite(
		ctx,
		conversation,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with website: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	firstIndex := strings.Index(content, "Title: Wikipedia Entry")

	secondIndex := strings.Index(content, "Title: Example Article")
	if firstIndex == -1 || secondIndex == -1 || firstIndex >= secondIndex {
		t.Fatalf("expected website results to preserve url order: %q", content)
	}

	if len(website.calls) != 2 {
		t.Fatalf("unexpected fetch call count: %d", len(website.calls))
	}
}

func TestMaybeAugmentConversationWithWebsiteIgnoresURLsOnlyPresentInDocumentContent(t *testing.T) {
	t.Parallel()

	website := newStubWebsiteContentClient(func(
		_ context.Context,
		rawURL string,
	) (websitePageContent, error) {
		t.Fatalf("unexpected website fetch for %q", rawURL)

		return websitePageContent{
			URL:         "",
			Title:       "",
			Description: "",
			Content:     "",
		}, nil
	})

	instance := newWebsiteTestBot(website)

	assertURLAugmentationIgnoresDocumentOnlyURLs(
		t,
		"https://example.com/from-document",
		func(
			ctx context.Context,
			conversation []chatMessage,
			urlExtractionText string,
		) ([]chatMessage, []string, error) {
			return instance.maybeAugmentConversationWithWebsite(
				ctx,
				conversation,
				urlExtractionText,
			)
		},
	)

	if len(website.calls) != 0 {
		t.Fatalf("unexpected fetch call count: %d", len(website.calls))
	}
}

func TestWebsiteClientFetchExtractsMainContentAndIgnoresChrome(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte(strings.Join([]string{
			"<!doctype html>",
			"<html>",
			"<head>",
			"<title>Go - Wikipedia</title>",
			`<meta name="description" content="Go is a statically typed programming language.">`,
			"</head>",
			"<body>",
			"<header>Site header</header>",
			"<nav>Navigation links</nav>",
			`<main id="mw-content-text">`,
			"<p>Go is a statically typed programming language designed at Google.</p>",
			"<p>It is syntactically similar to C and focuses on simplicity.</p>",
			"</main>",
			"<footer>Footer links</footer>",
			"</body>",
			"</html>",
		}, "")))
	}))
	defer server.Close()

	client := websiteClient{
		httpClient: server.Client(),
		userAgent:  youtubeUserAgent,
	}

	result, err := client.fetch(context.Background(), server.URL+"/wiki/Go_(programming_language)")
	if err != nil {
		t.Fatalf("fetch website content: %v", err)
	}

	if result.Title != "Go - Wikipedia" {
		t.Fatalf("unexpected title: %q", result.Title)
	}

	if result.Description != "Go is a statically typed programming language." {
		t.Fatalf("unexpected description: %q", result.Description)
	}

	if !containsFold(result.Content, "Go is a statically typed programming language designed at Google.") {
		t.Fatalf("expected main content in extraction: %q", result.Content)
	}

	if !containsFold(result.Content, "It is syntactically similar to C") {
		t.Fatalf("expected second paragraph in extraction: %q", result.Content)
	}

	if containsFold(result.Content, "Navigation links") || containsFold(result.Content, "Footer links") {
		t.Fatalf("expected chrome to be skipped: %q", result.Content)
	}
}

func TestWebsiteClientFetchRejectsUnsupportedContentType(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = writer.Write([]byte("binary"))
	}))
	defer server.Close()

	client := websiteClient{
		httpClient: server.Client(),
		userAgent:  youtubeUserAgent,
	}

	_, err := client.fetch(context.Background(), server.URL+"/file.bin")
	if err == nil {
		t.Fatal("expected unsupported content type to fail")
	}
}
