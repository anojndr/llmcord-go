package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errSearchBackendUnavailable = errors.New("search backend unavailable")

type stubChatCompletionClient struct {
	mu       sync.Mutex
	requests []chatCompletionRequest
	stream   func(context.Context, chatCompletionRequest, func(streamDelta) error) error
}

func (client *stubChatCompletionClient) streamChatCompletion(
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
	searchFn func(context.Context, []string) ([]webSearchResult, error)
}

func (client *stubWebSearchClient) search(
	ctx context.Context,
	queries []string,
) ([]webSearchResult, error) {
	client.mu.Lock()
	copiedQueries := make([]string, len(queries))
	copy(copiedQueries, queries)
	client.calls = append(client.calls, copiedQueries)
	client.mu.Unlock()

	return client.searchFn(ctx, queries)
}

func newStubChatClient(
	stream func(context.Context, chatCompletionRequest, func(streamDelta) error) error,
) *stubChatCompletionClient {
	client := new(stubChatCompletionClient)
	client.stream = stream

	return client
}

func newStubWebSearchClient(
	searchFn func(context.Context, []string) ([]webSearchResult, error),
) *stubWebSearchClient {
	client := new(stubWebSearchClient)
	client.searchFn = searchFn

	return client
}

func newSearchTestBot(chatCompletions chatCompletionClient, webSearch webSearchClient) *bot {
	instance := new(bot)
	instance.chatCompletions = chatCompletions
	instance.webSearch = webSearch

	return instance
}

func TestParseSearchDecisionNormalizesQueries(t *testing.T) {
	t.Parallel()

	responseText := "```json\n" +
		"{\"needs_search\":true,\"queries\":[\" latest news \",\"Latest News\",\" \"]}\n" +
		"```"

	decision, err := parseSearchDecision(responseText)
	if err != nil {
		t.Fatalf("parse search decision: %v", err)
	}

	if !decision.NeedsSearch {
		t.Fatal("expected search to be required")
	}

	if len(decision.Queries) != 1 || decision.Queries[0] != "latest news" {
		t.Fatalf("unexpected normalized queries: %#v", decision.Queries)
	}
}

func TestSearchDeciderConversationStripsImagesForTextOnlyModels(t *testing.T) {
	t.Parallel()

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": "<@123>: what is this?"},
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

	sanitizedConversation, err := searchDeciderConversation(
		conversation,
		testSearchConfig(),
		"openai/text-only-model",
	)
	if err != nil {
		t.Fatalf("search decider conversation: %v", err)
	}

	content, ok := sanitizedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected sanitized content type: %T", sanitizedConversation[0].Content)
	}

	if content != "<@123>: what is this?" {
		t.Fatalf("unexpected sanitized content: %q", content)
	}
}

func TestSearchDeciderConversationPreservesGeminiMedia(t *testing.T) {
	t.Parallel()

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": "<@123>: summarize these"},
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

	sanitizedConversation, err := searchDeciderConversation(
		conversation,
		testGeminiSearchConfig(),
		"google/gemini-3-flash-preview",
	)
	if err != nil {
		t.Fatalf("search decider conversation: %v", err)
	}

	parts, ok := sanitizedConversation[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected sanitized content type: %T", sanitizedConversation[0].Content)
	}

	if len(parts) != 4 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	if parts[3]["type"] != contentTypeVideoData {
		t.Fatalf("expected video to be preserved: %#v", parts[3])
	}
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

func TestMaybeAugmentConversationWithWebSearchAddsResultsWhenNeeded(t *testing.T) {
	t.Parallel()

	openAI := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		delta := new(streamDelta)
		delta.Content = `{"needs_search":true,"queries":["latest ai news","openai pricing"]}`

		return handle(*delta)
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{
			{Query: queries[0], Text: "AI news context"},
			{Query: queries[1], Text: "Pricing context"},
		}, nil
	})

	instance := newSearchTestBot(openAI, webSearch)

	conversation := []chatMessage{
		{Role: "assistant", Content: "Earlier answer"},
		{Role: messageRoleUser, Content: "<@123>: what changed?"},
	}

	augmentedConversation, searchMetadata, warnings, err := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		testSearchConfig(),
		conversation,
	)
	if err != nil {
		t.Fatalf("maybe augment conversation with web search: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if searchMetadata == nil {
		t.Fatal("expected search metadata")
	}

	if len(searchMetadata.Queries) != 2 {
		t.Fatalf("unexpected search metadata queries: %#v", searchMetadata.Queries)
	}

	if len(searchMetadata.Results) != 2 {
		t.Fatalf("unexpected search metadata results: %#v", searchMetadata.Results)
	}

	if len(openAI.requests) != 1 {
		t.Fatalf("unexpected decider request count: %d", len(openAI.requests))
	}

	requestMessages := openAI.requests[0].Messages
	if len(requestMessages) != 3 {
		t.Fatalf("unexpected decider message count: %d", len(requestMessages))
	}

	if requestMessages[0].Role != "system" {
		t.Fatalf("expected search decider system prompt, got role %q", requestMessages[0].Role)
	}

	if len(webSearch.calls) != 1 {
		t.Fatalf("unexpected web search call count: %d", len(webSearch.calls))
	}

	if got := len(webSearch.calls[0]); got != 2 {
		t.Fatalf("unexpected query count: %d", got)
	}

	latestContent, ok := augmentedConversation[1].Content.(string)
	if !ok {
		t.Fatalf("unexpected latest content type: %T", augmentedConversation[1].Content)
	}

	if latestContent == conversation[1].Content {
		t.Fatal("expected latest user message to be rewritten with search context")
	}
}

func TestMaybeAugmentConversationWithWebSearchSkipsWhenNotNeeded(t *testing.T) {
	t.Parallel()

	openAI := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		delta := new(streamDelta)
		delta.Content = `{"needs_search":false}`

		return handle(*delta)
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ []string,
	) ([]webSearchResult, error) {
		t.Fatal("expected web search to be skipped")

		return nil, nil
	})

	instance := newSearchTestBot(openAI, webSearch)

	conversation := []chatMessage{{Role: messageRoleUser, Content: "<@123>: explain closures"}}

	augmentedConversation, searchMetadata, warnings, err := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		testSearchConfig(),
		conversation,
	)
	if err != nil {
		t.Fatalf("maybe augment conversation with web search: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if searchMetadata != nil {
		t.Fatalf("expected search metadata to be nil: %#v", searchMetadata)
	}

	if augmentedConversation[0].Content != conversation[0].Content {
		t.Fatal("expected conversation to remain unchanged")
	}
}

func TestMaybeAugmentConversationWithWebSearchFallsBackOnSearchError(t *testing.T) {
	t.Parallel()

	openAI := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		delta := new(streamDelta)
		delta.Content = `{"needs_search":true,"queries":["latest ai news"]}`

		return handle(*delta)
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ []string,
	) ([]webSearchResult, error) {
		return nil, errSearchBackendUnavailable
	})

	instance := newSearchTestBot(openAI, webSearch)

	conversation := []chatMessage{{Role: messageRoleUser, Content: "<@123>: latest ai news"}}

	augmentedConversation, searchMetadata, warnings, err := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		testSearchConfig(),
		conversation,
	)
	if err != nil {
		t.Fatalf("maybe augment conversation with web search: %v", err)
	}

	if len(warnings) != 1 || warnings[0] != searchWarningText {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if searchMetadata != nil {
		t.Fatalf("expected search metadata to be nil: %#v", searchMetadata)
	}

	if augmentedConversation[0].Content != conversation[0].Content {
		t.Fatal("expected original conversation when web search fails")
	}
}

func TestExaSearchClientSearchRunsQueriesConcurrentlyAndKeepsOrder(t *testing.T) {
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
		endpoint:   httpServer.URL,
		httpClient: httpServer.Client(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results, err := client.search(ctx, []string{"alpha", "beta"})
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

func TestFormatSearchSourcesMessageIncludesQueriesAndSources(t *testing.T) {
	t.Parallel()

	metadata := &searchMetadata{
		Queries: []string{"latest ai news"},
		Results: []webSearchResult{
			{
				Query: "latest ai news",
				Text: "Title: Example Source\n" +
					"URL: https://example.com/source\n" +
					"Text: body\n\n" +
					"Title: Second Source\n" +
					"URL: https://example.com/second\n",
			},
		},
	}

	message := formatSearchSourcesMessage(metadata)

	if !containsFold(message, "Search queries:\n1. latest ai news") {
		t.Fatalf("unexpected queries section: %q", message)
	}

	if !containsFold(message, "Example Source") {
		t.Fatalf("expected first source title in message: %q", message)
	}

	if !containsFold(message, "https://example.com/source") {
		t.Fatalf("expected first source URL in message: %q", message)
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
		"openai/main-model":    nil,
		"openai/decider-model": nil,
	}
	loadedConfig.ModelOrder = []string{"openai/main-model", "openai/decider-model"}
	loadedConfig.SearchDeciderModel = "openai/decider-model"

	return *loadedConfig
}

func testGeminiSearchConfig() config {
	loadedConfig := new(config)
	loadedConfig.MaxImages = defaultMaxImages

	googleProvider := new(providerConfig)
	googleProvider.Type = string(providerAPIKindGemini)

	loadedConfig.Providers = map[string]providerConfig{
		"google": *googleProvider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"google/gemini-3-flash-preview": nil,
	}

	return *loadedConfig
}

func newStreamableHTTPOptions() *mcp.StreamableHTTPOptions {
	options := new(mcp.StreamableHTTPOptions)
	options.Stateless = true
	options.JSONResponse = true

	return options
}
