package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
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

func newWebsiteTestBot(website websiteFetcher) *bot {
	instance := new(bot)
	instance.website = website

	return instance
}

func newWebsiteTestClient(httpClient *http.Client, exaURL string, tavilyURL string) websiteClient {
	return websiteClient{
		httpClient:              httpClient,
		userAgent:               youtubeUserAgent,
		exaContentsEndpoint:     exaURL,
		tavilyExtractEndpoint:   tavilyURL,
		firecrawlScrapeEndpoint: "",
		tinyFishFetchEndpoint:   defaultTinyFishFetchEndpoint,
		lookupIP:                testWebsiteLookupIP,
		keys:                    newAPIKeyRotator(),
	}
}

func newWebsiteTestClientWithFirecrawl(
	httpClient *http.Client,
	firecrawlURL string,
	exaURL string,
	tavilyURL string,
) websiteClient {
	client := newWebsiteTestClient(httpClient, exaURL, tavilyURL)
	client.firecrawlScrapeEndpoint = firecrawlURL

	return client
}

func newWebsiteTestClientWithTinyFish(
	httpClient *http.Client,
	tinyFishURL string,
	exaURL string,
	tavilyURL string,
) websiteClient {
	client := newWebsiteTestClient(httpClient, exaURL, tavilyURL)
	client.tinyFishFetchEndpoint = tinyFishURL

	return client
}

func newWebsiteTestClientWithFirecrawlAndTinyFish(
	httpClient *http.Client,
	firecrawlURL string,
	tinyFishURL string,
	exaURL string,
	tavilyURL string,
) websiteClient {
	client := newWebsiteTestClient(httpClient, exaURL, tavilyURL)
	client.firecrawlScrapeEndpoint = firecrawlURL
	client.tinyFishFetchEndpoint = tinyFishURL

	return client
}

func testWebsiteLookupIP(_ context.Context, host string) ([]netip.Addr, error) {
	normalizedHost := normalizeWebsiteHost(host)
	if normalizedHost == "" {
		return nil, fmt.Errorf("resolve website host %q: %w", host, os.ErrInvalid)
	}

	switch normalizedHost {
	case "example.com",
		"redirect.example.com",
		"target.example.com",
		"allowed.example.com",
		"resolved.example.com":
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	case "metadata.example.com":
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	default:
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
}

func newWebsiteForwardingHTTPClient(
	t *testing.T,
	server *httptest.Server,
	forwardedHosts ...string,
) *http.Client {
	t.Helper()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server url %q: %v", server.URL, err)
	}

	forwardedHostSet := make(map[string]struct{}, len(forwardedHosts))
	for _, forwardedHost := range forwardedHosts {
		forwardedHostSet[normalizeWebsiteHost(forwardedHost)] = struct{}{}
	}

	httpClient := new(http.Client)
	httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if _, ok := forwardedHostSet[normalizeWebsiteHost(request.URL.Hostname())]; !ok {
			response, err := http.DefaultTransport.RoundTrip(request)
			if err != nil {
				return nil, fmt.Errorf("forward website test request %q: %w", request.URL.String(), err)
			}

			return response, nil
		}

		forwarded := request.Clone(request.Context())
		forwardedURL := *request.URL
		forwardedURL.Scheme = serverURL.Scheme
		forwardedURL.Host = serverURL.Host
		forwarded.URL = &forwardedURL
		forwarded.Host = request.Host

		response, err := server.Client().Transport.RoundTrip(forwarded)
		if err != nil {
			return nil, fmt.Errorf("forward website test request %q: %w", request.URL.String(), err)
		}

		response.Request = request

		return response, nil
	})

	return httpClient
}

func testWebsiteExaAndTavilyConfig() config {
	loadedConfig := testExaAPIWebSearchConfig()
	loadedConfig.WebSearch.Exa = exaSearchConfig{
		APIKey:             testExaPrimaryValue,
		APIKeys:            []string{testExaPrimaryValue},
		SearchType:         defaultExaSearchType,
		TextMaxCharacters:  defaultExaSearchTextMaxCharacters,
		LivecrawlTimeoutMS: defaultExaContentsLivecrawlTimeoutMS,
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

func testWebsiteFirecrawlConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.Firecrawl = firecrawlSearchConfig{
		APIKey:                testFirecrawlPrimaryAPIKey,
		APIKeys:               []string{testFirecrawlPrimaryAPIKey},
		MaxMarkdownCharacters: defaultFirecrawlMaxMarkdownCharacters,
	}

	return loadedConfig
}

func mustFetchWebsiteArticle(
	t *testing.T,
	client websiteClient,
	loadedConfig config,
) websitePageContent {
	t.Helper()

	result, err := client.fetch(context.Background(), loadedConfig, "https://example.com/article")
	if err != nil {
		t.Fatalf("fetch website content: %v", err)
	}

	return result
}

func newWebsiteTestResponse(
	statusCode int,
	headers http.Header,
	body string,
	request *http.Request,
) *http.Response {
	response := new(http.Response)
	response.StatusCode = statusCode
	response.Status = fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode))
	response.Proto = "HTTP/1.1"
	response.ProtoMajor = 1
	response.ProtoMinor = 1
	response.Header = headers
	response.Body = io.NopCloser(strings.NewReader(body))
	response.Request = request

	return response
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

func TestExtractWebsiteURLsKeepsXHosts(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"https://x.com/example/status/123",
		"https://twitter.com/example/status/456",
		"https://t.co/example",
	}, " ")

	urls := extractWebsiteURLsForProvider(text)

	expected := []string{
		"https://x.com/example/status/123",
		"https://twitter.com/example/status/456",
		"https://t.co/example",
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

	prepared, err := instance.prepareWebsiteAugmentation(
		ctx,
		testSearchConfig(),
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with website: %v", err)
	}

	augmentedConversation, err := applyPreparedConversationAugmentation(
		conversation,
		prepared,
	)
	if err != nil {
		t.Fatalf("augment conversation with website: %v", err)
	}

	warnings := prepared.warnings

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
			prepared, err := instance.prepareWebsiteAugmentation(
				ctx,
				testSearchConfig(),
				urlExtractionText,
			)
			if err != nil {
				return nil, nil, err
			}

			augmentedConversation, err := applyPreparedConversationAugmentation(
				conversation,
				prepared,
			)
			if err != nil {
				return nil, nil, err
			}

			return augmentedConversation, prepared.warnings, nil
		},
	)

	if len(website.calls) != 0 {
		t.Fatalf("unexpected fetch call count: %d", len(website.calls))
	}
}

func TestWebsiteClientFetchExtractsMainContentAndIgnoresChrome(t *testing.T) {
	t.Parallel()

	htmlBody := strings.Join([]string{
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
	}, "")

	// Local fallback removed: validate extraction via direct HTML parsing.
	result, err := parseWebsiteHTML("https://example.com/wiki/Go_(programming_language)", []byte(htmlBody))
	if err != nil {
		t.Fatalf("parse website content: %v", err)
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

	client := newWebsiteTestClient(
		newWebsiteForwardingHTTPClient(t, server, "example.com"),
		defaultExaContentsEndpoint,
		defaultTavilyExtractEndpoint,
	)

	_, err := client.fetch(context.Background(), testSearchConfig(), "https://example.com/file.bin")
	if err == nil {
		t.Fatal("expected unsupported content type to fail")
	}
}

func TestWebsiteClientFetchRejectsResolvedPrivateHosts(t *testing.T) {
	t.Parallel()

	httpClient := new(http.Client)
	httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected request to %q", request.URL.String())

		return nil, os.ErrInvalid
	})

	client := newWebsiteTestClient(httpClient, defaultExaContentsEndpoint, defaultTavilyExtractEndpoint)
	client.lookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		if normalizeWebsiteHost(host) != "resolved.example.com" {
			t.Fatalf("unexpected host lookup %q", host)
		}

		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}

	_, err := client.fetch(context.Background(), testSearchConfig(), "https://resolved.example.com/secret")
	if err == nil {
		t.Fatal("expected private host resolution to fail")
	}

	if !errors.Is(err, errUnsafeWebsiteAddress) {
		t.Fatalf("expected unsafe address error, got %v", err)
	}
}

func assertWebsiteTestCookie(
	t *testing.T,
	cookie *http.Cookie,
	err error,
	expectedName string,
	expectedValue string,
) {
	t.Helper()

	if err != nil {
		t.Fatalf("read website test cookie %q: %v", expectedName, err)
	}

	if cookie.Name != expectedName || cookie.Value != expectedValue {
		t.Fatalf("unexpected website test cookie: %#v", cookie)
	}
}

func TestWebsiteClientFetchUsesConfiguredExaLivecrawlTimeout(t *testing.T) {
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

		if request.Header.Get("X-Api-Key") != testExaPrimaryValue {
			t.Fatalf("unexpected Exa auth header: %q", request.Header.Get("X-Api-Key"))
		}

		var body map[string]any

		err := json.NewDecoder(request.Body).Decode(&body)
		if err != nil {
			t.Fatalf("decode Exa contents request: %v", err)
		}

		if mapIntValue(body, "livecrawlTimeout") != 6000 {
			t.Fatalf("unexpected Exa livecrawl timeout: %d", mapIntValue(body, "livecrawlTimeout"))
		}

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
	loadedConfig.WebSearch.Exa.LivecrawlTimeoutMS = 6000
	client := newWebsiteTestClient(exaServer.Client(), exaServer.URL, tavilyServer.URL)

	mustFetchWebsiteArticle(t, client, loadedConfig)

	if exaCallCount != 1 {
		t.Fatalf("unexpected Exa call count: %d", exaCallCount)
	}

	if tavilyCallCount != 0 {
		t.Fatalf("unexpected Tavily call count: %d", tavilyCallCount)
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

		if request.Header.Get("X-Api-Key") != testExaPrimaryValue {
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

	result := mustFetchWebsiteArticle(t, client, loadedConfig)

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

func TestWebsiteClientFetchRetriesExaLivecrawlTimeoutThenFallsBackToCache(t *testing.T) {
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

		responseWriter.Header().Set("Content-Type", "application/json")

		var body map[string]any

		err := json.NewDecoder(request.Body).Decode(&body)
		if err != nil {
			t.Fatalf("decode Exa contents request: %v", err)
		}

		switch exaCallCount {
		case 1:
			assertExaLivecrawlTimeout(t, body, defaultExaContentsLivecrawlTimeoutMS)
			writeExaContentsTimeoutResponse(t, responseWriter, testWebsiteArticleURL)
		case 2:
			assertExaLivecrawlTimeout(
				t,
				body,
				exaContentsLivecrawlExtendedTimeoutMultiplier*defaultExaContentsLivecrawlTimeoutMS,
			)
			writeExaContentsTimeoutResponse(t, responseWriter, testWebsiteArticleURL)
		case 3:
			assertExaOmitsLivecrawlTimeout(t, body)
			writeExaContentsSuccessResponse(
				t,
				responseWriter,
				testWebsiteArticleURL,
				"Cached Article",
				"Exa cached body.",
			)
		default:
			t.Fatalf("unexpected Exa contents call count: %d", exaCallCount)
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

	result := mustFetchWebsiteArticle(t, client, loadedConfig)

	if exaCallCount != 3 {
		t.Fatalf("unexpected Exa call count: %d", exaCallCount)
	}

	if tavilyCallCount != 0 {
		t.Fatalf("unexpected Tavily call count: %d", tavilyCallCount)
	}

	if result.Title != "Cached Article" {
		t.Fatalf("unexpected title: %q", result.Title)
	}

	if !containsFold(result.Content, "Exa cached body.") {
		t.Fatalf("unexpected content: %q", result.Content)
	}
}

func TestWebsiteClientFetchSurfacesPersistentExaLivecrawlTimeout(t *testing.T) {
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

		responseWriter.Header().Set("Content-Type", "application/json")

		writeExaContentsTimeoutResponse(t, responseWriter, testWebsiteArticleURL)
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

	_, err := client.fetch(context.Background(), loadedConfig, testWebsiteArticleURL)
	if err == nil {
		t.Fatal("expected persistent Exa livecrawl timeout to surface as an error")
	}

	if !strings.Contains(err.Error(), "CRAWL_LIVECRAWL_TIMEOUT") {
		t.Fatalf("unexpected error: %v", err)
	}

	if exaCallCount != exaContentsLivecrawlRetryMaxAttempts {
		t.Fatalf("unexpected Exa call count: %d", exaCallCount)
	}

	if tavilyCallCount != 0 {
		t.Fatalf("unexpected Tavily call count: %d", tavilyCallCount)
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

		if request.Header.Get("Authorization") != "Bearer "+testTavilyPrimaryAPIKey {
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

	result := mustFetchWebsiteArticle(t, client, loadedConfig)

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

func newFirecrawlScrapeSuccessServer(
	t *testing.T,
	requestBody map[string]any,
	title string,
	markdown string,
	description string,
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/v2/scrape" {
			t.Fatalf("unexpected Firecrawl path: %q", request.URL.Path)
		}

		if request.Header.Get("Authorization") != "Bearer "+testFirecrawlPrimaryAPIKey {
			t.Fatalf("unexpected Firecrawl auth header: %q", request.Header.Get("Authorization"))
		}

		requestURL, ok := requestBody["url"].(string)
		if !ok || requestURL == "" {
			t.Fatalf("unexpected Firecrawl request url: %#v", requestBody["url"])
		}

		var body map[string]any

		err := json.NewDecoder(request.Body).Decode(&body)
		if err != nil {
			t.Fatalf("decode Firecrawl scrape request: %v", err)
		}

		assertFirecrawlScrapeRequest(t, body, requestURL)

		responseWriter.Header().Set("Content-Type", "application/json")

		responseBody := map[string]any{
			"success": true,
			"data": map[string]any{
				"markdown": markdown,
				"metadata": map[string]any{
					"title":       title,
					"sourceURL":   requestURL,
					"description": description,
				},
			},
		}

		err = json.NewEncoder(responseWriter).Encode(responseBody)
		if err != nil {
			t.Fatalf("encode Firecrawl scrape response: %v", err)
		}
	}))
}

func TestWebsiteClientFetchUsesFirecrawlScrapeWhenConfigured(t *testing.T) {
	t.Parallel()

	var (
		exaCallCount    int
		tavilyCallCount int
	)

	firecrawlServer := newFirecrawlScrapeSuccessServer(
		t,
		map[string]any{"url": "https://example.com/article"},
		"Example Article",
		"# Firecrawl Heading\n\nFirecrawl extracted body.",
		"Example description.",
	)
	defer firecrawlServer.Close()

	exaServer := newRejectedWebsiteCallServer(&exaCallCount)
	defer exaServer.Close()

	tavilyServer := newRejectedWebsiteCallServer(&tavilyCallCount)
	defer tavilyServer.Close()

	loadedConfig := testWebsiteFirecrawlConfig()
	client := newWebsiteTestClientWithFirecrawl(
		firecrawlServer.Client(),
		firecrawlServer.URL+"/v2/scrape",
		exaServer.URL,
		tavilyServer.URL,
	)

	result := mustFetchWebsiteArticle(t, client, loadedConfig)

	if exaCallCount != 0 {
		t.Fatalf("unexpected Exa call count: %d", exaCallCount)
	}

	if tavilyCallCount != 0 {
		t.Fatalf("unexpected Tavily call count: %d", tavilyCallCount)
	}

	if result.Title != "Example Article" {
		t.Fatalf("unexpected title: %q", result.Title)
	}

	if result.Description != "Example description." {
		t.Fatalf("unexpected description: %q", result.Description)
	}

	if !containsFold(result.Content, "Firecrawl extracted body.") {
		t.Fatalf("unexpected content: %q", result.Content)
	}
}

func newRejectedWebsiteCallServer(callCount *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		*callCount++

		http.Error(responseWriter, "unexpected call", http.StatusInternalServerError)
	}))
}

func TestWebsiteClientFetchTruncatesFirecrawlMarkdownToConfiguredLimit(t *testing.T) {
	t.Parallel()

	longMarkdown := strings.Repeat("word ", 1000)

	firecrawlServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.Header().Set("Content-Type", "application/json")

		responseBody := map[string]any{
			"success": true,
			"data": map[string]any{
				"markdown": longMarkdown,
				"metadata": map[string]any{
					"title":     "Long Article",
					"sourceURL": "https://example.com/article",
				},
			},
		}

		err := json.NewEncoder(responseWriter).Encode(responseBody)
		if err != nil {
			t.Fatalf("encode Firecrawl scrape response: %v", err)
		}
	}))
	defer firecrawlServer.Close()

	loadedConfig := testWebsiteFirecrawlConfig()
	loadedConfig.WebSearch.Firecrawl.MaxMarkdownCharacters = 50
	client := newWebsiteTestClientWithFirecrawl(
		firecrawlServer.Client(),
		firecrawlServer.URL+"/v2/scrape",
		"",
		"",
	)

	result := mustFetchWebsiteArticle(t, client, loadedConfig)

	if runeCount(result.Content) > 50 {
		t.Fatalf("unexpected content length: %d", runeCount(result.Content))
	}
}

func TestWebsiteClientFetchSurfacesFirecrawlScrapeFailure(t *testing.T) {
	t.Parallel()

	firecrawlServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.Header().Set("Content-Type", "application/json")

		responseBody := map[string]any{
			"success": false,
			"error":   "Failed to load URL",
		}

		err := json.NewEncoder(responseWriter).Encode(responseBody)
		if err != nil {
			t.Fatalf("encode Firecrawl scrape response: %v", err)
		}
	}))
	defer firecrawlServer.Close()

	loadedConfig := testWebsiteFirecrawlConfig()
	client := newWebsiteTestClientWithFirecrawl(
		firecrawlServer.Client(),
		firecrawlServer.URL+"/v2/scrape",
		"",
		"",
	)

	_, err := client.fetch(context.Background(), loadedConfig, "https://example.com/article")
	if err == nil {
		t.Fatal("expected Firecrawl scrape failure to surface")
	}

	if !strings.Contains(err.Error(), "Failed to load URL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWebsiteClientFetchSurfacesFirecrawlHTTPStatusError(t *testing.T) {
	t.Parallel()

	firecrawlServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.Header().Set("Content-Type", "application/json")

		http.Error(responseWriter, `{"error": "Payment required to access this resource."}`, http.StatusPaymentRequired)
	}))
	defer firecrawlServer.Close()

	loadedConfig := testWebsiteFirecrawlConfig()
	client := newWebsiteTestClientWithFirecrawl(
		firecrawlServer.Client(),
		firecrawlServer.URL+"/v2/scrape",
		"",
		"",
	)

	_, err := client.fetch(context.Background(), loadedConfig, "https://example.com/article")
	if err == nil {
		t.Fatal("expected Firecrawl HTTP status error to surface")
	}

	if !strings.Contains(err.Error(), "402") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWebsiteClientFetchUsesTinyFishWhenConfigured(t *testing.T) {
	t.Parallel()

	var tinyFishCallCount int

	tinyFishServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		tinyFishCallCount++

		if request.Header.Get("X-API-Key") != "tf-test-key" {
			t.Fatalf("unexpected TinyFish API key header: %q", request.Header.Get("X-API-Key"))
		}
		if request.Method != http.MethodPost {
			t.Fatalf("unexpected TinyFish method: %q", request.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode TinyFish fetch request: %v", err)
		}
		assertTinyFishFetchRequest(t, body, "https://example.com/article")

		responseWriter.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"results": []map[string]any{{
				"url":         "https://example.com/article",
				"final_url":   "https://example.com/article",
				"title":       "Example Article",
				"description": "Example desc",
				"language":    "en",
				"format":      "markdown",
				"text":        "# Example Article\n\nTinyFish extracted body.",
			}},
			"errors": []any{},
		}
		if err := json.NewEncoder(responseWriter).Encode(resp); err != nil {
			t.Fatalf("encode TinyFish response: %v", err)
		}
	}))
	defer tinyFishServer.Close()

	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.TinyFish = tinyFishSearchConfig{
		APIKey:  "tf-test-key",
		APIKeys: []string{"tf-test-key"},
	}
	client := newWebsiteTestClientWithTinyFish(
		tinyFishServer.Client(),
		tinyFishServer.URL,
		"",
		"",
	)

	result, err := client.fetch(context.Background(), loadedConfig, "https://example.com/article")
	if err != nil {
		t.Fatalf("fetch with TinyFish: %v", err)
	}
	if tinyFishCallCount != 1 {
		t.Fatalf("unexpected TinyFish call count: %d", tinyFishCallCount)
	}
	if result.Title != "Example Article" {
		t.Fatalf("unexpected title: %q", result.Title)
	}
	if !strings.Contains(result.Content, "TinyFish extracted body.") {
		t.Fatalf("unexpected content: %q", result.Content)
	}
}

func TestWebsiteClientFetchTinyFishBatchSendsRequestContract(t *testing.T) {
	t.Parallel()

	var receivedRequest map[string]any

	tinyFishServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method != http.MethodPost {
			t.Fatalf("unexpected TinyFish method: %q", request.Method)
		}
		if request.Header.Get("X-API-Key") != "tf-test-key" {
			t.Fatalf("unexpected TinyFish API key header: %q", request.Header.Get("X-API-Key"))
		}
		if err := json.NewDecoder(request.Body).Decode(&receivedRequest); err != nil {
			t.Fatalf("decode TinyFish fetch request: %v", err)
		}

		responseWriter.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"results": []any{},
			"errors":  []any{},
		}
		if err := json.NewEncoder(responseWriter).Encode(resp); err != nil {
			t.Fatalf("encode TinyFish response: %v", err)
		}
	}))
	defer tinyFishServer.Close()

	client := newWebsiteTestClientWithTinyFish(
		tinyFishServer.Client(),
		tinyFishServer.URL,
		"",
		"",
	)

	batchResponse, err := client.fetchTinyFishBatch(
		t.Context(),
		"tf-test-key",
		[]string{"https://example.com/a", "https://example.com/b"},
	)
	if err != nil {
		t.Fatalf("fetchTinyFishBatch returned error: %v", err)
	}
	if len(batchResponse.Results) != 0 || len(batchResponse.Errors) != 0 {
		t.Fatalf("unexpected TinyFish fetch response: %#v", batchResponse)
	}

	rawURLs, ok := receivedRequest["urls"].([]any)
	if !ok || len(rawURLs) != 2 {
		t.Fatalf("unexpected TinyFish urls: %#v", receivedRequest["urls"])
	}
	if fmt.Sprint(receivedRequest["format"]) != "markdown" {
		t.Fatalf("unexpected TinyFish format: %#v", receivedRequest["format"])
	}
	rawTimeout, ok := receivedRequest["per_url_timeout_ms"].(float64)
	if !ok || int(rawTimeout) != tinyFishFetchPerURLTimeoutMS {
		t.Fatalf("unexpected TinyFish per_url_timeout_ms: %#v", receivedRequest["per_url_timeout_ms"])
	}
}

func TestWebsiteClientFetchFallsBackToTinyFishOnFirecrawlFailure(t *testing.T) {
	t.Parallel()

	var firecrawlCalls int
	var tinyFishCalls int

	firecrawlServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		firecrawlCalls++
		responseWriter.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"success": false,
			"error":   "Failed to load URL",
		}
		_ = json.NewEncoder(responseWriter).Encode(resp)
	}))
	defer firecrawlServer.Close()

	tinyFishServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		tinyFishCalls++
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode TinyFish fallback request: %v", err)
		}
		assertTinyFishFetchRequest(t, body, "https://example.com/article")
		responseWriter.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"results": []map[string]any{{
				"url":       "https://example.com/article",
				"final_url": "https://example.com/article",
				"title":     "Fallback Article",
				"format":    "markdown",
				"text":      "Fallback body via TinyFish.",
			}},
			"errors": []any{},
		}
		_ = json.NewEncoder(responseWriter).Encode(resp)
	}))
	defer tinyFishServer.Close()

	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.Firecrawl = firecrawlSearchConfig{
		APIKey:                "fc-test-key",
		APIKeys:               []string{"fc-test-key"},
		MaxMarkdownCharacters: defaultFirecrawlMaxMarkdownCharacters,
	}
	loadedConfig.WebSearch.TinyFish = tinyFishSearchConfig{
		APIKey:  "tf-test-key",
		APIKeys: []string{"tf-test-key"},
	}
	client := newWebsiteTestClientWithFirecrawlAndTinyFish(
		firecrawlServer.Client(),
		firecrawlServer.URL+"/v2/scrape",
		tinyFishServer.URL,
		"",
		"",
	)

	result, err := client.fetch(context.Background(), loadedConfig, "https://example.com/article")
	if err != nil {
		t.Fatalf("fallback to TinyFish failed: %v", err)
	}
	if firecrawlCalls != 1 {
		t.Fatalf("unexpected Firecrawl calls: %d", firecrawlCalls)
	}
	if tinyFishCalls != 1 {
		t.Fatalf("unexpected TinyFish fallback calls: %d", tinyFishCalls)
	}
	if result.Title != "Fallback Article" {
		t.Fatalf("unexpected fallback title: %q", result.Title)
	}
	if !strings.Contains(result.Content, "Fallback body via TinyFish") {
		t.Fatalf("unexpected fallback content: %q", result.Content)
	}
}

func TestWebsiteClientFetchSurfacesTinyFishError(t *testing.T) {
	t.Parallel()

	tinyFishServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"results": []any{},
			"errors": []map[string]any{{
				"url":   "https://example.com/article",
				"error": "bot_blocked",
			}},
		}
		_ = json.NewEncoder(responseWriter).Encode(resp)
	}))
	defer tinyFishServer.Close()

	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.TinyFish = tinyFishSearchConfig{
		APIKey:  "tf-test-key",
		APIKeys: []string{"tf-test-key"},
	}
	client := newWebsiteTestClientWithTinyFish(
		tinyFishServer.Client(),
		tinyFishServer.URL,
		"",
		"",
	)

	_, err := client.fetch(context.Background(), loadedConfig, "https://example.com/article")
	if err == nil {
		t.Fatal("expected TinyFish per-URL error to surface")
	}
	if !strings.Contains(err.Error(), "bot_blocked") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertTinyFishFetchRequest(t *testing.T, request map[string]any, requestURL string) {
	t.Helper()

	rawURLs, ok := request["urls"].([]any)
	if !ok || len(rawURLs) != 1 || rawURLs[0] != requestURL {
		t.Fatalf("unexpected TinyFish urls: %#v", request["urls"])
	}
	if fmt.Sprint(request["format"]) != "markdown" {
		t.Fatalf("unexpected TinyFish format: %#v", request["format"])
	}
	if rawTimeout, ok := request["per_url_timeout_ms"].(float64); !ok || int(rawTimeout) != tinyFishFetchPerURLTimeoutMS {
		t.Fatalf("unexpected TinyFish per_url_timeout_ms: %#v", request["per_url_timeout_ms"])
	}
}

func assertFirecrawlScrapeRequest(t *testing.T, request map[string]any, requestURL string) {
	t.Helper()

	if mapStringValue(request, "url") != requestURL {
		t.Fatalf("unexpected Firecrawl scrape url: %q", mapStringValue(request, "url"))
	}

	rawFormats, formatsOK := request["formats"].([]any)
	if !formatsOK || len(rawFormats) != 1 || rawFormats[0] != "markdown" {
		t.Fatalf("unexpected Firecrawl formats: %#v", request["formats"])
	}

	if rawMaxAge, ok := request["maxAge"]; ok && rawMaxAge != nil {
		t.Fatalf("unexpected Firecrawl maxAge: %#v", rawMaxAge)
	}

	if rawTimeout, ok := request["timeout"]; ok && rawTimeout != nil {
		t.Fatalf("unexpected Firecrawl timeout: %#v", rawTimeout)
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

	if mapStringValue(rawText, "verbosity") != "full" {
		t.Fatalf("unexpected Exa contents verbosity: %q", mapStringValue(rawText, "verbosity"))
	}

	if mapIntValue(request, "livecrawlTimeout") != defaultExaContentsLivecrawlTimeoutMS {
		t.Fatalf("unexpected Exa livecrawl timeout: %d", mapIntValue(request, "livecrawlTimeout"))
	}
}

const testWebsiteArticleURL = "https://example.com/article"

func assertExaLivecrawlTimeout(t *testing.T, request map[string]any, wantTimeout int) {
	t.Helper()

	if mapIntValue(request, "livecrawlTimeout") != wantTimeout {
		t.Fatalf(
			"unexpected Exa livecrawl timeout: %d, want %d",
			mapIntValue(request, "livecrawlTimeout"),
			wantTimeout,
		)
	}
}

func assertExaOmitsLivecrawlTimeout(t *testing.T, request map[string]any) {
	t.Helper()

	if _, hasLivecrawlTimeout := request["livecrawlTimeout"]; hasLivecrawlTimeout {
		t.Fatalf(
			"expected cache fallback Exa contents request to omit livecrawl timeout, got %d",
			mapIntValue(request, "livecrawlTimeout"),
		)
	}
}

func writeExaContentsTimeoutResponse(
	t *testing.T,
	responseWriter http.ResponseWriter,
	requestURL string,
) {
	t.Helper()

	httpStatusCode := http.StatusGatewayTimeout

	responseBody := map[string]any{
		"results": []any{},
		"statuses": []map[string]any{{
			"id":     requestURL,
			"status": "error",
			"error": map[string]any{
				"tag":            "CRAWL_LIVECRAWL_TIMEOUT",
				"httpStatusCode": &httpStatusCode,
			},
		}},
	}

	err := json.NewEncoder(responseWriter).Encode(responseBody)
	if err != nil {
		t.Fatalf("encode Exa contents timeout response: %v", err)
	}
}

func writeExaContentsSuccessResponse(
	t *testing.T,
	responseWriter http.ResponseWriter,
	requestURL string,
	title string,
	content string,
) {
	t.Helper()

	responseBody := map[string]any{
		"results": []map[string]any{{
			"title": title,
			"url":   requestURL,
			"id":    requestURL,
			"text":  "# " + title + "\n\n" + content,
		}},
		"statuses": []map[string]any{{
			"id":     requestURL,
			"status": "success",
		}},
	}

	err := json.NewEncoder(responseWriter).Encode(responseBody)
	if err != nil {
		t.Fatalf("encode Exa contents success response: %v", err)
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
