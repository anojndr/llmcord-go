package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errSearchBackendUnavailable = errors.New("search backend unavailable")

const (
	testExaPrimaryValue        = "exa-primary-value"
	testTavilyPrimaryAPIKey    = "primary-key"
	testFirecrawlPrimaryAPIKey = "firecrawl-primary-key"
	testWebSearchMaxURLs       = 7
)

func testExaAPIWebSearchConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.MaxURLs = testWebSearchMaxURLs
	loadedConfig.WebSearch.Exa = exaSearchConfig{
		APIKey:             testExaPrimaryValue,
		APIKeys:            []string{testExaPrimaryValue},
		SearchType:         defaultExaSearchType,
		TextMaxCharacters:  defaultExaSearchTextMaxCharacters,
		LivecrawlTimeoutMS: defaultExaContentsLivecrawlTimeoutMS,
	}

	return loadedConfig
}

func testTavilySearchConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.MaxURLs = testWebSearchMaxURLs
	loadedConfig.WebSearch.Tavily = tavilySearchConfig{
		APIKey:  testTavilyPrimaryAPIKey,
		APIKeys: []string{testTavilyPrimaryAPIKey},
	}

	return loadedConfig
}

func TestSearchQueriesConcurrentlyLimitsFanoutAndCancelsQueuedQueries(t *testing.T) {
	t.Parallel()

	queries := make([]string, externalRequestConcurrency*3)
	for index := range queries {
		queries[index] = fmt.Sprintf("query-%d", index)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	allWorkersStarted := make(chan struct{})

	var (
		startedCount int
		startedMu    sync.Mutex
	)

	results, err := searchQueriesConcurrently(
		ctx,
		queries,
		func(queryContext context.Context, query string) (webSearchResult, error) {
			startedMu.Lock()
			startedCount++

			if startedCount == externalRequestConcurrency {
				close(allWorkersStarted)
			}
			startedMu.Unlock()

			if query == queries[0] {
				select {
				case <-allWorkersStarted:
					return webSearchResult{}, errSearchBackendUnavailable
				case <-queryContext.Done():
					return webSearchResult{}, queryContext.Err()
				}
			}

			<-queryContext.Done()

			return webSearchResult{}, queryContext.Err()
		},
	)

	if !errors.Is(err, errSearchBackendUnavailable) {
		t.Fatalf("search error = %v", err)
	}

	if results != nil {
		t.Fatalf("results = %#v, want nil", results)
	}

	startedMu.Lock()
	actualStartedCount := startedCount
	startedMu.Unlock()

	if actualStartedCount != externalRequestConcurrency {
		t.Fatalf(
			"started queries = %d, want %d",
			actualStartedCount,
			externalRequestConcurrency,
		)
	}
}

func newExaAPISearchTestClient(handler http.HandlerFunc) (exaSearchClient, func()) {
	httpServer := httptest.NewServer(handler)

	return exaSearchClient{
		apiEndpoint: httpServer.URL,
		mcpEndpoint: defaultExaMCPEndpoint,
		httpClient:  httpServer.Client(),
		keys:        newAPIKeyRotator(),
	}, httpServer.Close
}

func assertExaSearchRequest(t *testing.T, args map[string]any) {
	t.Helper()

	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		t.Fatalf("unexpected Exa query argument: %#v", args["query"])
	}

	switch value := args["numResults"].(type) {
	case int:
		if value != testWebSearchMaxURLs {
			t.Fatalf("unexpected Exa numResults: %d", value)
		}
	case float64:
		if value != float64(testWebSearchMaxURLs) {
			t.Fatalf("unexpected Exa numResults: %v", value)
		}
	default:
		t.Fatalf("unexpected Exa numResults type %T with value %#v", value, value)
	}
}

func assertExaAPISearchRequest(
	t *testing.T,
	request exaSearchRequest,
	expectedType string,
	expectedMaxCharacters int,
) {
	t.Helper()

	if strings.TrimSpace(request.Query) == "" {
		t.Fatal("expected Exa API query to be set")
	}

	if request.Type != expectedType {
		t.Fatalf("unexpected Exa API type: %q", request.Type)
	}

	if request.NumResults != testWebSearchMaxURLs {
		t.Fatalf("unexpected Exa API num results: %d", request.NumResults)
	}

	if request.Contents.Text.MaxCharacters != expectedMaxCharacters {
		t.Fatalf(
			"unexpected Exa text max characters: %d",
			request.Contents.Text.MaxCharacters,
		)
	}

	if request.Contents.Text.Verbosity != "full" {
		t.Fatalf("unexpected Exa text verbosity: %q", request.Contents.Text.Verbosity)
	}

	if !request.Contents.Highlights {
		t.Fatal("expected Exa API highlights to be true")
	}
}

func testExaAPISearchSuccessResponse() map[string]any {
	publishedDate := "2026-03-20T00:00:00.000Z"
	author := "Example Author"

	return map[string]any{
		"error": "",
		"results": []map[string]any{
			{
				"title":         "Example Source",
				"url":           "https://example.com/source",
				"publishedDate": publishedDate,
				"author":        author,
				"text":          "# Example Source\n\nFull article text\nURL: https://example.com/not-a-source",
			},
		},
	}
}

func decodeExaSearchRequest(t *testing.T, requestBody io.Reader) exaSearchRequest {
	t.Helper()

	var rawRequest map[string]any

	err := json.NewDecoder(requestBody).Decode(&rawRequest)
	if err != nil {
		t.Fatalf("decode Exa request body: %v", err)
	}

	request := exaSearchRequest{
		Query:      mapStringValue(rawRequest, "query"),
		Type:       mapStringValue(rawRequest, "type"),
		NumResults: mapIntValue(rawRequest, "numResults"),
		Contents: exaSearchRequestContents{
			Text:       exaSearchTextRequest{MaxCharacters: 0, Verbosity: ""},
			Highlights: false,
		},
	}

	rawContents, hasContents := rawRequest["contents"].(map[string]any)
	if !hasContents {
		return request
	}

	if highlightsVal, ok := rawContents["highlights"].(bool); ok {
		request.Contents.Highlights = highlightsVal
	}

	rawText, hasText := rawContents["text"].(map[string]any)
	if !hasText {
		return request
	}

	request.Contents.Text.MaxCharacters = mapIntValue(rawText, "maxCharacters")
	request.Contents.Text.Verbosity = mapStringValue(rawText, "verbosity")

	return request
}

func mapIntValue(values map[string]any, key string) int {
	switch value := values[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}

type stubChatCompletionClient struct {
	mu       sync.Mutex
	requests []chatCompletionRequest
	stream   func(context.Context, chatCompletionRequest, func(streamDelta) error) error
}

func (client *stubChatCompletionClient) StreamChatCompletion(
	ctx context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	client.mu.Lock()
	client.requests = append(client.requests, request)
	client.mu.Unlock()

	return client.stream(ctx, request, handle)
}

type stubWebSearchClient struct {
	mu       sync.Mutex
	calls    [][]string
	searchFn func(context.Context, config, []string) ([]webSearchResult, error)
}

func (client *stubWebSearchClient) search(
	ctx context.Context,
	loadedConfig config,
	queries []string,
) ([]webSearchResult, error) {
	client.mu.Lock()
	copiedQueries := make([]string, len(queries))
	copy(copiedQueries, queries)
	client.calls = append(client.calls, copiedQueries)
	client.mu.Unlock()

	return client.searchFn(ctx, loadedConfig, queries)
}

func newStubChatClient(
	stream func(context.Context, chatCompletionRequest, func(streamDelta) error) error,
) *stubChatCompletionClient {
	client := new(stubChatCompletionClient)
	client.stream = stream

	return client
}

func newStubWebSearchClient(
	searchFn func(context.Context, config, []string) ([]webSearchResult, error),
) *stubWebSearchClient {
	client := new(stubWebSearchClient)
	client.searchFn = searchFn

	return client
}

func newSearchTestBot(chatCompletions chatCompletionStreamer, webSearch webSearcher) *bot {
	instance := new(bot)
	instance.chatCompletions = chatCompletions
	instance.webSearch = webSearch
	instance.nodes = newMessageNodeStore(maxMessageNodes)

	session, err := discordgo.New("Bot discord-token")
	if err == nil {
		session.State.User = newDiscordUser("bot-user", true)
		instance.session = session
	}

	return instance
}

func TestAppendWebSearchResultsToConversationPreservesMultimodalParts(t *testing.T) {
	t.Parallel()

	conversation := []chatMessage{
		{Role: "assistant", Content: "Earlier answer"},
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": "<@123>: identify this"},
				{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
				{
					"type":               contentTypeAudioData,
					contentFieldBytes:    []byte("audio-bytes"),
					contentFieldMIMEType: "audio/mpeg",
				},
				{
					"type":               contentTypeVideoData,
					contentFieldBytes:    []byte("video-bytes"),
					contentFieldMIMEType: "video/mp4",
				},
			},
		},
	}

	augmentedConversation, err := appendWebSearchResultsToConversation(
		conversation,
		"Query: image\nResults:\ncat",
	)
	if err != nil {
		t.Fatalf("append web search results: %v", err)
	}

	parts, ok := augmentedConversation[1].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected augmented content type: %T", augmentedConversation[1].Content)
	}

	if len(parts) != 4 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	if parts[0]["type"] != contentTypeText {
		t.Fatalf("expected first part to be text: %#v", parts[0])
	}

	textValue, _ := parts[0]["text"].(string)
	if textValue == "" || textValue == "<@123>: identify this" {
		t.Fatalf("unexpected prompt text: %q", textValue)
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected image to be preserved: %#v", parts[1])
	}

	if parts[2]["type"] != contentTypeAudioData {
		t.Fatalf("expected audio to be preserved: %#v", parts[2])
	}

	if parts[3]["type"] != contentTypeVideoData {
		t.Fatalf("expected video to be preserved: %#v", parts[3])
	}
}

func TestExaSearchClientSearchRunsMCPQueriesConcurrentlyAndKeepsOrderWhenNoAPIKeysConfigured(t *testing.T) {
	t.Parallel()

	var (
		startedCount int
		startedMu    sync.Mutex
		release      = make(chan struct{})
	)

	implementation := new(mcp.Implementation)
	implementation.Name = "exa-test"
	implementation.Version = "1.0.0"

	server := mcp.NewServer(implementation, nil)

	tool := new(mcp.Tool)
	tool.Name = exaSearchToolName

	mcp.AddTool(server, tool, func(
		ctx context.Context,
		_ *mcp.CallToolRequest,
		args map[string]any,
	) (*mcp.CallToolResult, any, error) {
		assertExaSearchRequest(t, args)

		query, _ := args["query"].(string)

		startedMu.Lock()
		startedCount++

		if startedCount == 2 {
			close(release)
		}
		startedMu.Unlock()

		select {
		case <-release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}

		result := new(mcp.CallToolResult)
		textContent := new(mcp.TextContent)
		textContent.Text = "result for " + query
		result.Content = []mcp.Content{textContent}

		return result, nil, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, newStreamableHTTPOptions())

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	client := exaSearchClient{
		apiEndpoint: defaultExaSearchEndpoint,
		mcpEndpoint: httpServer.URL,
		httpClient:  httpServer.Client(),
		keys:        newAPIKeyRotator(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results, err := client.search(ctx, testTavilySearchConfig(), []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("unexpected result count: %d", len(results))
	}

	if results[0].Query != "alpha" || results[0].Text != "result for alpha" {
		t.Fatalf("unexpected first result: %#v", results[0])
	}

	if results[1].Query != "beta" || results[1].Text != "result for beta" {
		t.Fatalf("unexpected second result: %#v", results[1])
	}
}

func TestExaSearchClientSearchUsesConfiguredSearchType(t *testing.T) {
	t.Parallel()

	var searchBodies []exaSearchRequest

	client, closeServer := newExaAPISearchTestClient(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		searchBodies = append(searchBodies, decodeExaSearchRequest(t, request.Body))

		responseWriter.Header().Set("Content-Type", "application/json")

		err := json.NewEncoder(responseWriter).Encode(testExaAPISearchSuccessResponse())
		if err != nil {
			t.Errorf("encode Exa response: %v", err)
		}
	}))
	defer closeServer()

	loadedConfig := testExaAPIWebSearchConfig()
	loadedConfig.WebSearch.Exa.SearchType = exaSearchTypeDeepReasoning

	_, err := client.search(context.Background(), loadedConfig, []string{"latest ai news"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(searchBodies) != 1 {
		t.Fatalf("unexpected Exa API request count: %d", len(searchBodies))
	}

	assertExaAPISearchRequest(
		t,
		searchBodies[0],
		exaSearchTypeDeepReasoning,
		loadedConfig.WebSearch.Exa.TextMaxCharacters,
	)
}

func TestExaSearchClientSearchUsesConfiguredTextMaxCharacters(t *testing.T) {
	t.Parallel()

	var searchBodies []exaSearchRequest

	client, closeServer := newExaAPISearchTestClient(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		searchBodies = append(searchBodies, decodeExaSearchRequest(t, request.Body))

		responseWriter.Header().Set("Content-Type", "application/json")

		err := json.NewEncoder(responseWriter).Encode(testExaAPISearchSuccessResponse())
		if err != nil {
			t.Errorf("encode Exa response: %v", err)
		}
	}))
	defer closeServer()

	loadedConfig := testExaAPIWebSearchConfig()
	loadedConfig.WebSearch.Exa.TextMaxCharacters = 9000

	_, err := client.search(context.Background(), loadedConfig, []string{"latest ai news"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(searchBodies) != 1 {
		t.Fatalf("unexpected Exa API request count: %d", len(searchBodies))
	}

	assertExaAPISearchRequest(
		t,
		searchBodies[0],
		defaultExaSearchType,
		loadedConfig.WebSearch.Exa.TextMaxCharacters,
	)
}

func TestExaSearchClientSearchSendsAuthorizationBearerHeader(t *testing.T) {
	t.Parallel()

	var authHeader string

	client, closeServer := newExaAPISearchTestClient(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		authHeader = request.Header.Get("Authorization")

		responseWriter.Header().Set("Content-Type", "application/json")

		err := json.NewEncoder(responseWriter).Encode(testExaAPISearchSuccessResponse())
		if err != nil {
			t.Errorf("encode Exa response: %v", err)
		}
	}))
	defer closeServer()

	_, err := client.search(context.Background(), testExaAPIWebSearchConfig(), []string{"latest ai news"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	expectedAuthHeader := "Bearer " + testExaPrimaryValue
	if authHeader != expectedAuthHeader {
		t.Fatalf("unexpected Authorization header: got %q, want %q", authHeader, expectedAuthHeader)
	}
}

func TestExaSearchClientSearchRotatesAPIKeysAcrossCalls(t *testing.T) {
	t.Parallel()

	var (
		authHeaders []string
		headerMu    sync.Mutex
	)

	client, closeServer := newExaAPISearchTestClient(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		headerMu.Lock()

		authHeaders = append(authHeaders, request.Header.Get("Authorization"))
		headerMu.Unlock()

		responseWriter.Header().Set("Content-Type", "application/json")

		err := json.NewEncoder(responseWriter).Encode(testExaAPISearchSuccessResponse())
		if err != nil {
			t.Errorf("encode Exa response: %v", err)
		}
	}))
	defer closeServer()

	loadedConfig := testExaAPIWebSearchConfig()
	loadedConfig.WebSearch.Exa.APIKey = "exa-key-1"
	loadedConfig.WebSearch.Exa.APIKeys = []string{
		"exa-key-1",
		"exa-key-2",
		"exa-key-3",
	}

	for range 3 {
		_, err := client.search(context.Background(), loadedConfig, []string{"latest ai news"})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
	}

	headerMu.Lock()
	defer headerMu.Unlock()

	expectedHeaders := []string{"Bearer exa-key-1", "Bearer exa-key-2", "Bearer exa-key-3"}
	for index, expected := range expectedHeaders {
		if authHeaders[index] != expected {
			t.Fatalf("search %d: expected Authorization header %q, got %q", index, expected, authHeaders[index])
		}
	}
}

func TestRoutedWebSearchClientFallsBackToTavilyWhenMCPFails(t *testing.T) {
	t.Parallel()

	exaClient := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		return nil, errSearchBackendUnavailable
	})
	tavilyClient := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{
			{
				Query: queries[0],
				Text:  "Title: Tavily Source\nURL: https://example.com/fallback\nText: fallback result",
			},
		}, nil
	})

	client := routedWebSearchClient{
		exa:    exaClient,
		tavily: tavilyClient,
	}

	const query = "latest ai news"

	results, err := client.search(context.Background(), testSearchConfig(), []string{query})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(exaClient.calls) != 1 {
		t.Fatalf("unexpected Exa call count: %d", len(exaClient.calls))
	}

	if len(tavilyClient.calls) != 1 {
		t.Fatalf("unexpected Tavily call count: %d", len(tavilyClient.calls))
	}

	if len(results) != 1 || results[0].Query != query {
		t.Fatalf("unexpected fallback results: %#v", results)
	}
}

func TestTinyFishFetchBatchSendsPerURLTimeout(t *testing.T) {
	t.Parallel()

	var receivedRequest map[string]any
	var capturedDeadline time.Time

	server := httptest.NewServer(http.HandlerFunc(func(
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
		response := map[string]any{
			"results": []any{},
			"errors":  []any{},
		}
		if err := json.NewEncoder(responseWriter).Encode(response); err != nil {
			t.Fatalf("encode TinyFish response: %v", err)
		}
	}))
	defer server.Close()

	baseClient := server.Client()
	client := newTinyFishSearchClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			capturedDeadline, _ = request.Context().Deadline()
			transport := baseClient.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			return transport.RoundTrip(request)
		}),
	})
	client.fetchEndpoint = server.URL

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

	remaining := time.Until(capturedDeadline)
	if remaining <= tinyFishSearchRequestTimeout || remaining > tinyFishFetchRequestTimeout {
		t.Fatalf(
			"unexpected fetch request deadline remaining: %s, want within (%s, %s]",
			remaining,
			tinyFishSearchRequestTimeout,
			tinyFishFetchRequestTimeout,
		)
	}
}

func TestTinyFishSearchQuerySendsBoundedDeadline(t *testing.T) {
	t.Parallel()

	var capturedDeadline time.Time

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected TinyFish search method: %q", request.Method)
		}
		if request.Header.Get("X-API-Key") != "tf-test-key" {
			t.Fatalf("unexpected TinyFish search API key header: %q", request.Header.Get("X-API-Key"))
		}
		if request.URL.Query().Get("query") != "test query" {
			t.Fatalf("unexpected TinyFish search query: %q", request.URL.Query().Get("query"))
		}

		responseWriter.Header().Set("Content-Type", "application/json")
		response := map[string]any{
			"query":         "test query",
			"results":       []any{},
			"total_results": 0,
			"page":          0,
		}
		if err := json.NewEncoder(responseWriter).Encode(response); err != nil {
			t.Fatalf("encode TinyFish search response: %v", err)
		}
	}))
	defer server.Close()

	baseClient := server.Client()
	client := newTinyFishSearchClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			capturedDeadline, _ = request.Context().Deadline()
			transport := baseClient.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			return transport.RoundTrip(request)
		}),
	})
	client.searchEndpoint = server.URL

	results, err := client.searchQuery(t.Context(), "tf-test-key", "test query")
	if err != nil {
		t.Fatalf("searchQuery returned error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("unexpected TinyFish search results: %#v", results)
	}

	remaining := time.Until(capturedDeadline)
	if remaining <= 0 || remaining > tinyFishSearchRequestTimeout {
		t.Fatalf(
			"unexpected search request deadline remaining: %s, want within (0, %s]",
			remaining,
			tinyFishSearchRequestTimeout,
		)
	}
}

func TestRoutedWebSearchClientHardcodedTinyFishExaTavilyChain(t *testing.T) {
	t.Parallel()

	tinyFishResult := []webSearchResult{{Query: "q", Text: "tinyfish result"}}
	exaResult := []webSearchResult{{Query: "q", Text: "exa result"}}
	tavilyResult := []webSearchResult{{Query: "q", Text: "tavily result"}}

	tests := []struct {
		name             string
		tinyFishAPIKeys  []string
		tinyFishSucceeds bool
		exaSucceeds      bool
		tavilySucceeds   bool
		wantText         string
		wantTinyCalls    int
		wantExaCalls     int
		wantTavilyCalls  int
		wantErr          bool
	}{
		{
			name:             "tinyfish success short-circuits exa and tavily",
			tinyFishAPIKeys:  []string{"tf-key"},
			tinyFishSucceeds: true,
			exaSucceeds:      true,
			tavilySucceeds:   true,
			wantText:         "tinyfish result",
			wantTinyCalls:    1,
			wantExaCalls:     0,
			wantTavilyCalls:  0,
		},
		{
			name:             "tinyfish fails falls back to exa",
			tinyFishAPIKeys:  []string{"tf-key"},
			tinyFishSucceeds: false,
			exaSucceeds:      true,
			tavilySucceeds:   true,
			wantText:         "exa result",
			wantTinyCalls:    1,
			wantExaCalls:     1,
			wantTavilyCalls:  0,
		},
		{
			name:             "tinyfish and exa fail falls back to tavily",
			tinyFishAPIKeys:  []string{"tf-key"},
			tinyFishSucceeds: false,
			exaSucceeds:      false,
			tavilySucceeds:   true,
			wantText:         "tavily result",
			wantTinyCalls:    1,
			wantExaCalls:     1,
			wantTavilyCalls:  1,
		},
		{
			name:            "no tinyfish keys skips tinyfish and uses exa",
			tinyFishAPIKeys: nil,
			exaSucceeds:     true,
			tavilySucceeds:  true,
			wantText:        "exa result",
			wantTinyCalls:   0,
			wantExaCalls:    1,
			wantTavilyCalls: 0,
		},
		{
			name:            "no tinyfish exa fails uses tavily",
			tinyFishAPIKeys: nil,
			exaSucceeds:     false,
			tavilySucceeds:  true,
			wantText:        "tavily result",
			wantTinyCalls:   0,
			wantExaCalls:    1,
			wantTavilyCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tinyFishClient := newStubWebSearchClient(func(_ context.Context, _ config, _ []string) ([]webSearchResult, error) {
				if tc.tinyFishSucceeds {
					return tinyFishResult, nil
				}
				return nil, errSearchBackendUnavailable
			})
			exaClient := newStubWebSearchClient(func(_ context.Context, _ config, _ []string) ([]webSearchResult, error) {
				if tc.exaSucceeds {
					return exaResult, nil
				}
				return nil, errSearchBackendUnavailable
			})
			tavilyClient := newStubWebSearchClient(func(_ context.Context, _ config, _ []string) ([]webSearchResult, error) {
				if tc.tavilySucceeds {
					return tavilyResult, nil
				}
				return nil, errSearchBackendUnavailable
			})

			loadedConfig := testSearchConfig()
			if len(tc.tinyFishAPIKeys) > 0 {
				loadedConfig.WebSearch.TinyFish.APIKey = tc.tinyFishAPIKeys[0]
				loadedConfig.WebSearch.TinyFish.APIKeys = tc.tinyFishAPIKeys
			}

			client := routedWebSearchClient{
				tinyFish: tinyFishClient,
				exa:      exaClient,
				tavily:   tavilyClient,
			}

			results, err := client.search(context.Background(), loadedConfig, []string{"q"})
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("search: %v", err)
			}
			if !tc.wantErr {
				if len(results) != 1 || results[0].Text != tc.wantText {
					t.Fatalf("unexpected results: %#v want %q", results, tc.wantText)
				}
			}
			if len(tinyFishClient.calls) != tc.wantTinyCalls {
				t.Fatalf("tinyfish calls %d want %d", len(tinyFishClient.calls), tc.wantTinyCalls)
			}
			if len(exaClient.calls) != tc.wantExaCalls {
				t.Fatalf("exa calls %d want %d", len(exaClient.calls), tc.wantExaCalls)
			}
			if len(tavilyClient.calls) != tc.wantTavilyCalls {
				t.Fatalf("tavily calls %d want %d", len(tavilyClient.calls), tc.wantTavilyCalls)
			}
		})
	}
}

func TestRoutedWebSearchClientHardcodedErrorMessagesAndExaName(t *testing.T) {
	t.Parallel()

	errTinyFish := errors.New("tinyfish boom")
	errExa := errors.New("exa boom")
	errTavily := errors.New("tavily boom")

	t.Run("all fail with tinyfish uses TinyFish Search and Exa branch", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name       string
			exaAPIKeys []string
			wantExa    string
		}{
			{"exa MCP when no api key", nil, "Exa MCP"},
			{"exa Search API when key set", []string{"exa-key"}, "Exa Search API"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				tinyFishClient := newStubWebSearchClient(func(_ context.Context, _ config, _ []string) ([]webSearchResult, error) {
					return nil, errTinyFish
				})
				exaClient := newStubWebSearchClient(func(_ context.Context, _ config, _ []string) ([]webSearchResult, error) {
					return nil, errExa
				})
				tavilyClient := newStubWebSearchClient(func(_ context.Context, _ config, _ []string) ([]webSearchResult, error) {
					return nil, errTavily
				})

				loadedConfig := testSearchConfig()
				loadedConfig.WebSearch.TinyFish.APIKey = "tf-key"
				loadedConfig.WebSearch.TinyFish.APIKeys = []string{"tf-key"}
				if len(tc.exaAPIKeys) > 0 {
					loadedConfig.WebSearch.Exa.APIKey = tc.exaAPIKeys[0]
					loadedConfig.WebSearch.Exa.APIKeys = tc.exaAPIKeys
				}

				client := routedWebSearchClient{tinyFish: tinyFishClient, exa: exaClient, tavily: tavilyClient}
				_, err := client.search(context.Background(), loadedConfig, []string{"q"})
				if err == nil {
					t.Fatal("expected error")
				}
				msg := err.Error()
				if !strings.Contains(msg, "TinyFish Search") {
					t.Fatalf("error missing TinyFish Search: %q", msg)
				}
				if !strings.Contains(msg, tc.wantExa) {
					t.Fatalf("error missing %q: %q", tc.wantExa, msg)
				}
				if !strings.Contains(msg, "Tavily") {
					t.Fatalf("error missing Tavily: %q", msg)
				}
				if !errors.Is(err, errTinyFish) || !errors.Is(err, errExa) || !errors.Is(err, errTavily) {
					t.Fatalf("expected joined errors, got %v", err)
				}
			})
		}
	})

	t.Run("all fail without tinyfish omits tinyfish and uses Exa branch", func(t *testing.T) {
		t.Parallel()

		exaClient := newStubWebSearchClient(func(_ context.Context, _ config, _ []string) ([]webSearchResult, error) {
			return nil, errExa
		})
		tavilyClient := newStubWebSearchClient(func(_ context.Context, _ config, _ []string) ([]webSearchResult, error) {
			return nil, errTavily
		})

		loadedConfig := testSearchConfig()
		loadedConfig.WebSearch.Exa.APIKeys = nil
		loadedConfig.WebSearch.Exa.APIKey = ""

		client := routedWebSearchClient{exa: exaClient, tavily: tavilyClient}
		_, err := client.search(context.Background(), loadedConfig, []string{"q"})
		if err == nil {
			t.Fatal("expected error")
		}
		msg := err.Error()
		if strings.Contains(msg, "TinyFish") {
			t.Fatalf("error should not contain TinyFish when not configured: %q", msg)
		}
		if !strings.Contains(msg, "Exa MCP") {
			t.Fatalf("error missing Exa MCP: %q", msg)
		}
		if !strings.Contains(msg, "Tavily") {
			t.Fatalf("error missing Tavily: %q", msg)
		}
	})
}

func TestTavilySearchClientSearchRotatesAPIKeysAcrossCalls(t *testing.T) {
	t.Parallel()

	var (
		authHeaders []string
		headerMu    sync.Mutex
	)

	httpServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		headerMu.Lock()

		authHeaders = append(authHeaders, request.Header.Get("Authorization"))
		headerMu.Unlock()

		responseWriter.Header().Set("Content-Type", "application/json")

		err := json.NewEncoder(responseWriter).Encode(tavilySearchResponse{
			Results: []tavilySearchResponseResult{
				{
					Title:      "Example",
					URL:        "https://example.com",
					Content:    "content",
					RawContent: "",
				},
			},
		})
		if err != nil {
			t.Errorf("encode Tavily response: %v", err)
		}
	}))
	defer httpServer.Close()

	client := tavilySearchClient{
		endpoint:   httpServer.URL,
		httpClient: httpServer.Client(),
		keys:       newAPIKeyRotator(),
	}

	loadedConfig := testTavilySearchConfig()
	loadedConfig.WebSearch.Tavily.APIKey = "tavily-key-1"
	loadedConfig.WebSearch.Tavily.APIKeys = []string{
		"tavily-key-1",
		"tavily-key-2",
		"tavily-key-3",
	}

	for range 3 {
		_, err := client.search(context.Background(), loadedConfig, []string{"latest ai news"})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
	}

	headerMu.Lock()
	defer headerMu.Unlock()

	expectedHeaders := []string{"Bearer tavily-key-1", "Bearer tavily-key-2", "Bearer tavily-key-3"}
	for index, expected := range expectedHeaders {
		if authHeaders[index] != expected {
			t.Fatalf("search %d: expected Authorization header %q, got %q", index, expected, authHeaders[index])
		}
	}
}

func TestFormatSearchSourcesMessageIncludesQueriesAndSources(t *testing.T) {
	t.Parallel()

	metadata := &searchMetadata{
		Queries: []string{"latest ai news"},
		Results: []webSearchResult{{
			Query: "latest ai news",
			Text: "Title: Example Source\n" +
				"URL: https://example.com/source\n" +
				"Text: body\n\n" +
				"Title: Second Source\n" +
				"URL: https://example.com/second\n",
		}},
		MaxURLs:             defaultWebSearchMaxURLs,
		VisualSearchSources: nil,
	}

	message := formatSearchSourcesMessage(metadata)

	if !containsFold(message, "Search queries:\n1. latest ai news") {
		t.Fatalf("unexpected queries section: %q", message)
	}

	if !strings.Contains(
		message,
		"Sources for \"latest ai news\":\n"+
			"1. Example Source <https://example.com/source>\n"+
			"2. Second Source <https://example.com/second>",
	) {
		t.Fatalf("unexpected sources section: %q", message)
	}
}

func TestFormatSearchSourcesMessageUsesAngleBracketURLWithoutDuplicateTitle(t *testing.T) {
	t.Parallel()

	metadata := &searchMetadata{
		Queries:             []string{"latest ai news"},
		Results:             []webSearchResult{{Query: "latest ai news", Text: "URL: https://example.com/source\n"}},
		MaxURLs:             defaultWebSearchMaxURLs,
		VisualSearchSources: nil,
	}

	message := formatSearchSourcesMessage(metadata)

	if !strings.Contains(message, "<https://example.com/source>") {
		t.Fatalf("expected angle-bracketed URL in message: %q", message)
	}

	if !strings.Contains(message, "1. <https://example.com/source>") {
		t.Fatalf("expected numbered source line in message: %q", message)
	}

	if strings.Contains(message, "https://example.com/source <https://example.com/source>") {
		t.Fatalf("expected source URL to be shown once when title is unavailable: %q", message)
	}
}

func TestFormatSearchSourcesMessageUsesGenericSourcesLabelWithoutQuery(t *testing.T) {
	t.Parallel()

	metadata := &searchMetadata{
		Queries: nil,
		Results: []webSearchResult{{
			Query: "",
			Text: "Title: Example Source\n" +
				"URL: https://example.com/source\n",
		}},
		MaxURLs:             defaultWebSearchMaxURLs,
		VisualSearchSources: nil,
	}

	message := formatSearchSourcesMessage(metadata)

	if !strings.Contains(message, "Sources:\n1. Example Source <https://example.com/source>") {
		t.Fatalf("unexpected generic sources section: %q", message)
	}

	if strings.Contains(message, "Sources for \"\"") {
		t.Fatalf("expected generic sources label without empty query quoting: %q", message)
	}
}

func TestFormatSearchSourcesMessageIncludesVisualSearchSourceURLs(t *testing.T) {
	t.Parallel()

	metadata := testStructuredVisualSearchMetadata()
	metadata.VisualSearchSources[0].Label = yandexVisualSearchProviderName

	message := formatSearchSourcesMessage(metadata)

	for _, fragment := range []string{
		"Visual search result URLs:",
		yandexVisualSearchProviderName + ":",
		"1. Top match: Sword Art Online (ru.ruwiki.ru) <" + testVisualSearchTopMatchURL + ">",
		"2. Similar image: AnimePTK <" + testVisualSearchSimilarImageURL + ">",
		"3. Site match: AnimePTK (" + testVisualSearchSiteDomain + ") <" +
			testVisualSearchSiteMatchURL + ">",
	} {
		if !strings.Contains(message, fragment) {
			t.Fatalf("expected fragment %q in message: %q", fragment, message)
		}
	}
}

func TestFormatSearchSourcesMessageLimitsSourcesPerQuery(t *testing.T) {
	t.Parallel()

	var resultText strings.Builder

	for index := range 3 {
		sourceNumber := index + 1
		_, _ = fmt.Fprintf(
			&resultText,
			"Title: Source %d\nURL: https://example.com/source-%d\nText: body\n\n",
			sourceNumber,
			sourceNumber,
		)
	}

	metadata := &searchMetadata{
		Queries: []string{"latest ai news"},
		Results: []webSearchResult{{
			Query: "latest ai news",
			Text:  resultText.String(),
		}},
		MaxURLs:             2,
		VisualSearchSources: nil,
	}

	message := formatSearchSourcesMessage(metadata)

	for index := range metadata.MaxURLs {
		sourceNumber := index + 1

		expectedLine := fmt.Sprintf(
			"%d. Source %d <https://example.com/source-%d>",
			sourceNumber,
			sourceNumber,
			sourceNumber,
		)
		if !strings.Contains(message, expectedLine) {
			t.Fatalf("expected source line %q in message: %q", expectedLine, message)
		}
	}

	excludedSource := metadata.MaxURLs + 1
	excludedLine := fmt.Sprintf(
		"Source %d <https://example.com/source-%d>",
		excludedSource,
		excludedSource,
	)

	if strings.Contains(message, excludedLine) {
		t.Fatalf("expected message to exclude source %d: %q", excludedSource, message)
	}
}

func TestFormatSearchSourcesMessageNumbersSourcesAcrossQueries(t *testing.T) {
	t.Parallel()

	results := make([]webSearchResult, 0, 2)
	queries := []string{"latest ai news", "monitor brand reliability"}

	for _, query := range queries {
		var resultText strings.Builder

		for sourceIndex := range 5 {
			sourceNumber := sourceIndex + 1
			_, _ = fmt.Fprintf(
				&resultText,
				"Title: %s source %d\nURL: https://example.com/%s/%d\nText: body\n\n",
				query,
				sourceNumber,
				strings.ReplaceAll(query, " ", "-"),
				sourceNumber,
			)
		}

		results = append(results, webSearchResult{
			Query: query,
			Text:  resultText.String(),
		})
	}

	metadata := &searchMetadata{
		Queries:             queries,
		Results:             results,
		MaxURLs:             5,
		VisualSearchSources: nil,
	}

	message := formatSearchSourcesMessage(metadata)

	for number := range 10 {
		expectedPrefix := fmt.Sprintf("\n%d. ", number+1)
		if !strings.Contains("\n"+message, expectedPrefix) {
			t.Fatalf("expected message to contain source number %d: %q", number+1, message)
		}
	}

	if strings.Contains(message, "Sources for \"monitor brand reliability\":\n1. ") {
		t.Fatalf("expected second query block to continue numbering: %q", message)
	}

	if !strings.Contains(message, "Sources for \"monitor brand reliability\":\n6. ") {
		t.Fatalf("expected second query block to start at 6: %q", message)
	}
}

func TestFormatSearchSourcesPagesSplitsLongMessagesWithoutTruncation(t *testing.T) {
	t.Parallel()

	metadata := testPaginatedSearchMetadata()

	message := formatSearchSourcesMessage(metadata)
	if runeCount(message) <= showSourcesPageBodyMaxLength {
		t.Fatalf("expected test message to exceed page body limit: %d", runeCount(message))
	}

	pages := formatSearchSourcesPages(metadata)
	if len(pages) < 2 {
		t.Fatalf("expected paginated sources, got %d page(s)", len(pages))
	}

	for index, page := range pages {
		if runeCount(page) > showSourcesPageBodyMaxLength {
			t.Fatalf("page %d exceeds body limit: %d", index, runeCount(page))
		}

		if containsFold(page, "... truncated") {
			t.Fatalf("expected page %d to avoid truncation marker: %q", index, page)
		}
	}

	joinedPages := strings.Join(pages, "\n")
	for _, fragment := range []string{
		"Sources for \"latest ai news\":",
		"https://example.com/ai-news/1",
		"https://example.com/agent-frameworks/5",
	} {
		if !containsFold(joinedPages, fragment) {
			t.Fatalf("expected fragment %q in paginated pages: %q", fragment, joinedPages)
		}
	}
}

func TestCountSearchSourcesUsesDisplayedSourceTotal(t *testing.T) {
	t.Parallel()

	metadata := &searchMetadata{
		Queries: []string{"latest ai news"},
		Results: []webSearchResult{{
			Query: "latest ai news",
			Text: "Title: Source 1\nURL: https://example.com/source-1\n\n" +
				"Title: Source 2\nURL: https://example.com/source-2\n\n" +
				"Title: Source 3\nURL: https://example.com/source-3\n",
		}},
		MaxURLs: 2,
		VisualSearchSources: []visualSearchSourceGroup{{
			Label: "Visual search",
			Sources: []searchSource{{
				Title: "Visual Source 1",
				URL:   "https://example.com/visual-1",
			}, {
				Title: "Visual Source 2",
				URL:   "https://example.com/visual-2",
			}},
		}},
	}

	if totalSources := countSearchSources(metadata); totalSources != 4 {
		t.Fatalf("unexpected total displayed source count: %d", totalSources)
	}
}

func TestFormatSearchSourcesPageContentIncludesTotalSourcesOnSinglePage(t *testing.T) {
	t.Parallel()

	metadata := &searchMetadata{
		Queries: []string{"latest ai news"},
		Results: []webSearchResult{{
			Query: "latest ai news",
			Text: "Title: Example Source\nURL: https://example.com/source\n\n" +
				"Title: Second Source\nURL: https://example.com/second\n",
		}},
		MaxURLs:             defaultWebSearchMaxURLs,
		VisualSearchSources: nil,
	}

	content := formatSearchSourcesPageContent(formatSearchSourcesPages(metadata), 0, countSearchSources(metadata))

	if !strings.Contains(content, "Sources (2 total)\n\n") {
		t.Fatalf("expected total source count in page header: %q", content)
	}

	if !strings.Contains(content, "1. Example Source <https://example.com/source>") {
		t.Fatalf("expected source details in single-page content: %q", content)
	}
}

func TestFormatSearchSourcesPageContentIncludesTotalSourcesOnPaginatedContent(t *testing.T) {
	t.Parallel()

	metadata := testPaginatedSearchMetadata()
	pages := formatSearchSourcesPages(metadata)

	if len(pages) < 2 {
		t.Fatalf("expected paginated sources, got %d page(s)", len(pages))
	}

	content := formatSearchSourcesPageContent(pages, 0, countSearchSources(metadata))

	expectedHeader := fmt.Sprintf("Sources (%d total, page 1/%d)", countSearchSources(metadata), len(pages))
	if !strings.Contains(content, expectedHeader) {
		t.Fatalf("expected paginated header %q in content: %q", expectedHeader, content)
	}
}

func testPaginatedSearchMetadata() *searchMetadata {
	searchQueries := []struct {
		query string
		slug  string
	}{
		{query: "latest ai news", slug: "ai-news"},
		{query: "llm benchmarks", slug: "llm-benchmarks"},
		{query: "agent frameworks", slug: "agent-frameworks"},
	}

	queries := make([]string, 0, len(searchQueries))
	results := make([]webSearchResult, 0, len(searchQueries))

	for _, searchQuery := range searchQueries {
		queries = append(queries, searchQuery.query)

		var resultText strings.Builder

		for sourceIndex := range defaultWebSearchMaxURLs {
			sourceNumber := sourceIndex + 1
			_, _ = fmt.Fprintf(
				&resultText,
				"Title: %s source %d %s\nURL: https://example.com/%s/%d\nText: body\n\n",
				searchQuery.query,
				sourceNumber,
				strings.Repeat("detail ", 20),
				searchQuery.slug,
				sourceNumber,
			)
		}

		results = append(results, webSearchResult{
			Query: searchQuery.query,
			Text:  resultText.String(),
		})
	}

	return &searchMetadata{
		Queries:             queries,
		Results:             results,
		MaxURLs:             defaultWebSearchMaxURLs,
		VisualSearchSources: nil,
	}
}

func testSearchConfig() config {
	loadedConfig := new(config)
	provider := new(providerConfig)
	provider.BaseURL = "https://api.example.com/v1"

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/main-model": nil,
	}
	loadedConfig.WebSearch.MaxURLs = defaultWebSearchMaxURLs
	loadedConfig.WebSearch.Exa = exaSearchConfig{
		APIKey:             "",
		APIKeys:            nil,
		SearchType:         defaultExaSearchType,
		TextMaxCharacters:  defaultExaSearchTextMaxCharacters,
		LivecrawlTimeoutMS: defaultExaContentsLivecrawlTimeoutMS,
	}
	loadedConfig.ModelOrder = []string{"openai/main-model"}
	loadedConfig.MaxMessages = defaultMaxMessages

	return *loadedConfig
}

func newStreamableHTTPOptions() *mcp.StreamableHTTPOptions {
	options := new(mcp.StreamableHTTPOptions)
	options.Stateless = true
	options.JSONResponse = true

	return options
}
