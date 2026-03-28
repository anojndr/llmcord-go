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

type stubWebsiteContentClient struct {
	mu      sync.Mutex
	calls   []string
	fetchFn func(context.Context, config, string) (websitePageContent, error)
}

func (client *stubWebsiteContentClient) fetch(
	ctx context.Context,
	loadedConfig config,
	rawURL string,
) (websitePageContent, error) {
	client.mu.Lock()
	client.calls = append(client.calls, rawURL)
	client.mu.Unlock()

	return client.fetchFn(ctx, loadedConfig, rawURL)
}

func newStubWebsiteContentClient(
	fetchFn func(context.Context, config, string) (websitePageContent, error),
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

func newWebsiteTestClient(httpClient *http.Client, exaURL string, tavilyURL string) websiteClient {
	return websiteClient{
		httpClient:            httpClient,
		userAgent:             youtubeUserAgent,
		exaContentsEndpoint:   exaURL,
		tavilyExtractEndpoint: tavilyURL,
	}
}

func testWebsiteExaAndTavilyConfig() config {
	loadedConfig := testExaAPIWebSearchConfig()
	loadedConfig.WebSearch.Exa = exaSearchConfig{
		APIKey:            testExaPrimaryValue,
		APIKeys:           []string{testExaPrimaryValue},
		SearchType:        defaultExaSearchType,
		TextMaxCharacters: defaultExaSearchTextMaxCharacters,
	}
	loadedConfig.WebSearch.Tavily = tavilySearchConfig{
		APIKey:  testTavilyPrimaryAPIKey,
		APIKeys: []string{testTavilyPrimaryAPIKey},
	}

	return loadedConfig
}

func testWebsiteTavilyOnlyConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.Tavily = tavilySearchConfig{
		APIKey:  testTavilyPrimaryAPIKey,
		APIKeys: []string{testTavilyPrimaryAPIKey},
	}

	return loadedConfig
}

func mustFetchWebsite(
	t *testing.T,
	client websiteClient,
	loadedConfig config,
	rawURL string,
) websitePageContent {
	t.Helper()

	result, err := client.fetch(context.Background(), loadedConfig, rawURL)
	if err != nil {
		t.Fatalf("fetch website content: %v", err)
	}

	return result
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
		_ config,
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
		testSearchConfig(),
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
		_ config,
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
				testSearchConfig(),
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
		httpClient:            server.Client(),
		userAgent:             youtubeUserAgent,
		exaContentsEndpoint:   defaultExaContentsEndpoint,
		tavilyExtractEndpoint: defaultTavilyExtractEndpoint,
	}

	result, err := client.fetch(
		context.Background(),
		testSearchConfig(),
		server.URL+"/wiki/Go_(programming_language)",
	)
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
		httpClient:            server.Client(),
		userAgent:             youtubeUserAgent,
		exaContentsEndpoint:   defaultExaContentsEndpoint,
		tavilyExtractEndpoint: defaultTavilyExtractEndpoint,
	}

	_, err := client.fetch(context.Background(), testSearchConfig(), server.URL+"/file.bin")
	if err == nil {
		t.Fatal("expected unsupported content type to fail")
	}
}

func TestWebsiteClientFetchUsesExaContentsWhenConfigured(t *testing.T) {
	t.Parallel()

	var (
		exaCallCount    int
		tavilyCallCount int
	)

	exaServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		exaCallCount++

		if request.Header.Get("X-Api-Key") != testExaPrimaryAuthHeader {
			t.Fatalf("unexpected Exa auth header: %q", request.Header.Get("X-Api-Key"))
		}

		var body map[string]any

		err := json.NewDecoder(request.Body).Decode(&body)
		if err != nil {
			t.Fatalf("decode Exa contents request: %v", err)
		}

		assertExaContentsRequest(t, body, "https://example.com/article")

		responseWriter.Header().Set("Content-Type", "application/json")

		responseBody := map[string]any{
			"results": []map[string]any{{
				"title": "Example Article",
				"url":   "https://example.com/article",
				"id":    "https://example.com/article",
				"text":  "# Example Article\n\nExa extracted body.",
			}},
			"statuses": []map[string]any{{
				"id":     "https://example.com/article",
				"status": "success",
			}},
		}

		err = json.NewEncoder(responseWriter).Encode(responseBody)
		if err != nil {
			t.Fatalf("encode Exa contents response: %v", err)
		}
	}))
	defer exaServer.Close()

	tavilyServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		tavilyCallCount++

		http.Error(responseWriter, "unexpected Tavily call", http.StatusInternalServerError)
	}))
	defer tavilyServer.Close()

	loadedConfig := testWebsiteExaAndTavilyConfig()
	client := newWebsiteTestClient(exaServer.Client(), exaServer.URL, tavilyServer.URL)

	result := mustFetchWebsite(t, client, loadedConfig, "https://example.com/article")

	if exaCallCount != 1 {
		t.Fatalf("unexpected Exa call count: %d", exaCallCount)
	}

	if tavilyCallCount != 0 {
		t.Fatalf("unexpected Tavily call count: %d", tavilyCallCount)
	}

	if result.Title != "Example Article" {
		t.Fatalf("unexpected title: %q", result.Title)
	}

	if !containsFold(result.Content, "Exa extracted body.") {
		t.Fatalf("unexpected content: %q", result.Content)
	}
}

func TestWebsiteClientFetchUsesTavilyWhenNoExaAPIKeyConfigured(t *testing.T) {
	t.Parallel()

	var (
		exaCallCount    int
		tavilyCallCount int
	)

	exaServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		exaCallCount++

		http.Error(responseWriter, "unexpected Exa call", http.StatusInternalServerError)
	}))
	defer exaServer.Close()

	tavilyServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		tavilyCallCount++

		if request.Header.Get("Authorization") != testTavilyPrimaryAuthHeader {
			t.Fatalf("unexpected Tavily auth header: %q", request.Header.Get("Authorization"))
		}

		var body map[string]any

		err := json.NewDecoder(request.Body).Decode(&body)
		if err != nil {
			t.Fatalf("decode Tavily extract request: %v", err)
		}

		assertTavilyExtractRequest(t, body, "https://example.com/article")

		responseWriter.Header().Set("Content-Type", "application/json")

		responseBody := map[string]any{
			"results": []map[string]any{{
				"url":         "https://example.com/article",
				"raw_content": "# Tavily Heading\n\nTavily extracted body.",
			}},
			"failed_results": []any{},
		}

		err = json.NewEncoder(responseWriter).Encode(responseBody)
		if err != nil {
			t.Fatalf("encode Tavily extract response: %v", err)
		}
	}))
	defer tavilyServer.Close()

	loadedConfig := testWebsiteTavilyOnlyConfig()
	client := newWebsiteTestClient(tavilyServer.Client(), exaServer.URL, tavilyServer.URL)

	result := mustFetchWebsite(t, client, loadedConfig, "https://example.com/article")

	if exaCallCount != 0 {
		t.Fatalf("unexpected Exa call count: %d", exaCallCount)
	}

	if tavilyCallCount != 1 {
		t.Fatalf("unexpected Tavily call count: %d", tavilyCallCount)
	}

	if result.Title != "https://example.com/article" {
		t.Fatalf("unexpected title: %q", result.Title)
	}

	if !containsFold(result.Content, "Tavily extracted body.") {
		t.Fatalf("unexpected content: %q", result.Content)
	}
}

func TestWebsiteClientFetchFallsBackToTavilyWhenExaContentsFails(t *testing.T) {
	t.Parallel()

	var (
		exaCallCount    int
		tavilyCallCount int
	)

	exaServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		exaCallCount++

		var body map[string]any

		err := json.NewDecoder(request.Body).Decode(&body)
		if err != nil {
			t.Fatalf("decode Exa contents request: %v", err)
		}

		assertExaContentsRequest(t, body, "https://example.com/article")

		responseWriter.Header().Set("Content-Type", "application/json")

		responseBody := map[string]any{
			"results": []any{},
			"statuses": []map[string]any{{
				"id":     "https://example.com/article",
				"status": "error",
				"error": map[string]any{
					"tag": "CRAWL_TIMEOUT",
				},
			}},
		}

		err = json.NewEncoder(responseWriter).Encode(responseBody)
		if err != nil {
			t.Fatalf("encode Exa contents error response: %v", err)
		}
	}))
	defer exaServer.Close()

	tavilyServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		tavilyCallCount++

		var body map[string]any

		err := json.NewDecoder(request.Body).Decode(&body)
		if err != nil {
			t.Fatalf("decode Tavily extract request: %v", err)
		}

		assertTavilyExtractRequest(t, body, "https://example.com/article")

		responseWriter.Header().Set("Content-Type", "application/json")

		responseBody := map[string]any{
			"results": []map[string]any{{
				"url":         "https://example.com/article",
				"raw_content": "Tavily fallback body.",
			}},
			"failed_results": []any{},
		}

		err = json.NewEncoder(responseWriter).Encode(responseBody)
		if err != nil {
			t.Fatalf("encode Tavily extract response: %v", err)
		}
	}))
	defer tavilyServer.Close()

	loadedConfig := testWebsiteExaAndTavilyConfig()
	client := newWebsiteTestClient(exaServer.Client(), exaServer.URL, tavilyServer.URL)

	result := mustFetchWebsite(t, client, loadedConfig, "https://example.com/article")

	if exaCallCount != 1 {
		t.Fatalf("unexpected Exa call count: %d", exaCallCount)
	}

	if tavilyCallCount != 1 {
		t.Fatalf("unexpected Tavily call count: %d", tavilyCallCount)
	}

	if !containsFold(result.Content, "Tavily fallback body.") {
		t.Fatalf("unexpected content: %q", result.Content)
	}
}

func TestWebsiteClientFetchFallsBackToCurrentImplementationWhenExaAndTavilyFail(t *testing.T) {
	t.Parallel()

	var (
		exaCallCount    int
		tavilyCallCount int
	)

	localServer := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte(strings.Join([]string{
			"<!doctype html>",
			"<html><head><title>Local Fallback</title></head>",
			"<body><main><p>Current implementation body.</p></main></body></html>",
		}, "")))
	}))
	defer localServer.Close()

	exaServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		exaCallCount++

		responseWriter.Header().Set("Content-Type", "application/json")

		responseBody := map[string]any{
			"results": []any{},
			"statuses": []map[string]any{{
				"id":     localServer.URL + "/article",
				"status": "error",
				"error": map[string]any{
					"tag": "CRAWL_TIMEOUT",
				},
			}},
		}

		err := json.NewEncoder(responseWriter).Encode(responseBody)
		if err != nil {
			t.Fatalf("encode Exa contents error response: %v", err)
		}
	}))
	defer exaServer.Close()

	tavilyServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		tavilyCallCount++

		http.Error(responseWriter, "upstream failure", http.StatusInternalServerError)
	}))
	defer tavilyServer.Close()

	loadedConfig := testWebsiteExaAndTavilyConfig()
	client := newWebsiteTestClient(localServer.Client(), exaServer.URL, tavilyServer.URL)

	result := mustFetchWebsite(t, client, loadedConfig, localServer.URL+"/article")

	if exaCallCount != 1 {
		t.Fatalf("unexpected Exa call count: %d", exaCallCount)
	}

	if tavilyCallCount != 1 {
		t.Fatalf("unexpected Tavily call count: %d", tavilyCallCount)
	}

	if result.Title != "Local Fallback" {
		t.Fatalf("unexpected title: %q", result.Title)
	}

	if !containsFold(result.Content, "Current implementation body.") {
		t.Fatalf("unexpected content: %q", result.Content)
	}
}

func assertExaContentsRequest(t *testing.T, request map[string]any, requestURL string) {
	t.Helper()

	rawURLs, urlsOK := request["urls"].([]any)
	if !urlsOK || len(rawURLs) != 1 || rawURLs[0] != requestURL {
		t.Fatalf("unexpected Exa contents urls: %#v", request["urls"])
	}

	rawText, ok := request["text"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected Exa contents text payload: %#v", request["text"])
	}

	if mapIntValue(rawText, "maxCharacters") != maxWebsiteContentRunes {
		t.Fatalf("unexpected Exa contents max characters: %d", mapIntValue(rawText, "maxCharacters"))
	}

	if mapStringValue(rawText, "verbosity") != "compact" {
		t.Fatalf("unexpected Exa contents verbosity: %q", mapStringValue(rawText, "verbosity"))
	}

	if mapIntValue(request, "livecrawlTimeout") != exaContentsLivecrawlTimeoutMS {
		t.Fatalf("unexpected Exa livecrawl timeout: %d", mapIntValue(request, "livecrawlTimeout"))
	}
}

func assertTavilyExtractRequest(t *testing.T, request map[string]any, requestURL string) {
	t.Helper()

	rawURLs, urlsOK := request["urls"].([]any)
	if !urlsOK || len(rawURLs) != 1 || rawURLs[0] != requestURL {
		t.Fatalf("unexpected Tavily extract urls: %#v", request["urls"])
	}

	if mapStringValue(request, "extract_depth") != "advanced" {
		t.Fatalf("unexpected Tavily extract depth: %q", mapStringValue(request, "extract_depth"))
	}

	if mapStringValue(request, "format") != "markdown" {
		t.Fatalf("unexpected Tavily extract format: %q", mapStringValue(request, "format"))
	}

	timeout, ok := request["timeout"].(float64)
	if !ok || timeout != tavilyExtractTimeoutSeconds {
		t.Fatalf("unexpected Tavily extract timeout: %#v", request["timeout"])
	}
}
