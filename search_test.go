package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errSearchBackendUnavailable = errors.New("search backend unavailable")

const (
	testTavilyPrimaryAPIKey     = "primary-key"
	testTavilyBackupAPIKey      = "backup-key"
	testTavilyPrimaryAuthHeader = "Bearer " + testTavilyPrimaryAPIKey
	testTavilyBackupAuthHeader  = "Bearer " + testTavilyBackupAPIKey
	testWebSearchMaxURLs        = 7
)

func testTavilySearchConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.PrimaryProvider = webSearchProviderKindMCP
	loadedConfig.WebSearch.MaxURLs = testWebSearchMaxURLs
	loadedConfig.WebSearch.Tavily = tavilySearchConfig{
		APIKey:  testTavilyPrimaryAPIKey,
		APIKeys: []string{testTavilyPrimaryAPIKey, testTavilyBackupAPIKey},
	}

	return loadedConfig
}

func newTavilySearchTestClient(handler http.HandlerFunc) (tavilySearchClient, func()) {
	httpServer := httptest.NewServer(handler)

	return tavilySearchClient{
		endpoint:   httpServer.URL,
		httpClient: httpServer.Client(),
	}, httpServer.Close
}

func writeTavilySearchResponse(
	t *testing.T,
	responseWriter http.ResponseWriter,
	response tavilySearchResponse,
) {
	t.Helper()

	responseWriter.Header().Set("Content-Type", "application/json")

	err := json.NewEncoder(responseWriter).Encode(response)
	if err != nil {
		t.Errorf("encode Tavily response: %v", err)
	}
}

func assertTavilyAuthHeaders(t *testing.T, authHeaders []string) {
	t.Helper()

	if len(authHeaders) != 2 {
		t.Fatalf("unexpected Tavily attempt count: %d", len(authHeaders))
	}

	if authHeaders[0] != testTavilyPrimaryAuthHeader || authHeaders[1] != testTavilyBackupAuthHeader {
		t.Fatalf("unexpected Tavily auth headers: %#v", authHeaders)
	}
}

func testTavilySearchSuccessResponse() tavilySearchResponse {
	return tavilySearchResponse{
		Results: []tavilySearchResponseResult{
			{
				Title:   "Example Source",
				URL:     "https://example.com/source",
				Content: "A relevant snippet",
				RawContent: "Full article text\n" +
					"URL: https://example.com/not-a-source\n" +
					"Title: Embedded heading",
			},
		},
	}
}

func assertTavilySearchRequest(t *testing.T, request tavilySearchRequest) {
	t.Helper()

	if request.SearchDepth != "advanced" {
		t.Fatalf("unexpected Tavily search depth: %q", request.SearchDepth)
	}

	if request.MaxResults != testWebSearchMaxURLs {
		t.Fatalf("unexpected Tavily max results: %d", request.MaxResults)
	}

	if request.IncludeRawContent != "text" {
		t.Fatalf("unexpected Tavily raw content setting: %q", request.IncludeRawContent)
	}
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

func assertTavilyRawContentResult(t *testing.T, result webSearchResult) {
	t.Helper()

	if !containsFold(result.Text, "Example Source") {
		t.Fatalf("expected Tavily result title in text: %q", result.Text)
	}

	if !containsFold(result.Text, "https://example.com/source") {
		t.Fatalf("expected Tavily result URL in text: %q", result.Text)
	}

	if !containsFold(result.Text, "Raw Content") {
		t.Fatalf("expected Tavily raw content in text: %q", result.Text)
	}

	if !containsFold(result.Text, "| Full article text") {
		t.Fatalf("expected Tavily raw content block in text: %q", result.Text)
	}

	if !containsFold(result.Text, "| URL: https://example.com/not-a-source") {
		t.Fatalf("expected Tavily raw content line to be escaped: %q", result.Text)
	}

	sources := extractSearchSources(result.Text)
	if len(sources) != 1 {
		t.Fatalf("unexpected source count parsed from Tavily text: %d", len(sources))
	}

	if sources[0].URL != "https://example.com/source" {
		t.Fatalf("unexpected source parsed from Tavily text: %#v", sources[0])
	}
}

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

func TestSearchDeciderPromptMatchesTextFile(t *testing.T) {
	t.Parallel()

	promptBytes, err := os.ReadFile("searchDeciderPrompt.txt")
	if err != nil {
		t.Fatalf("read search decider prompt file: %v", err)
	}

	if searchDeciderPrompt() != strings.TrimSpace(string(promptBytes)) {
		t.Fatal("expected embedded search decider prompt to match searchDeciderPrompt.txt")
	}
}

func TestSearchDeciderPromptRetainsCriticalInstructions(t *testing.T) {
	t.Parallel()

	expectedSnippets := []string{
		`You are a search-gating assistant. Your job is to decide whether a web search ` +
			`is needed to answer the user's latest request.`,
		`2. If the user's latest request includes an explicit instruction not to search ` +
			`the web or not to browse the web, always return:`,
		`7. Use conversation context when resolving references like "this", "that", ` +
			`"verify this", "look it up", or "search everything mentioned here".`,
		`8. If the user refers to content in an attached image, extract the concrete ` +
			`entities/topics shown in the image or named in the accompanying text, and ` +
			`search for those directly.`,
		`- The user asks about a specific webpage, document, product, company, public ` +
			`figure, event, law, schedule, price, or release`,
	}

	prompt := searchDeciderPrompt()

	for _, expectedSnippet := range expectedSnippets {
		if !strings.Contains(prompt, expectedSnippet) {
			t.Fatalf("expected search decider prompt to contain %q", expectedSnippet)
		}
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
					"type":               contentTypeDocument,
					contentFieldBytes:    []byte("document-bytes"),
					contentFieldMIMEType: mimeTypePDF,
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
					"type":               contentTypeDocument,
					contentFieldBytes:    []byte("document-bytes"),
					contentFieldMIMEType: mimeTypePDF,
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

	if len(parts) != 5 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	if parts[3]["type"] != contentTypeDocument {
		t.Fatalf("expected document to be preserved: %#v", parts[3])
	}

	if parts[4]["type"] != contentTypeVideoData {
		t.Fatalf("expected video to be preserved: %#v", parts[4])
	}
}

func TestBuildSearchDeciderConversationAppendsPDFImagesForVisionDecider(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newPDFExtractionTestBot(
		"message-search-pdf",
		"<@123>: summarize the report",
		[]contentPart{
			testPDFDocumentPart(t, "Quarterly revenue grew by 12 percent.", true),
		},
	)

	loadedConfig := testSearchConfig()
	loadedConfig.MaxImages = 1
	loadedConfig.SearchDeciderModel = "openai/decider-model:vision"

	mainConversation, err := instance.maybeAugmentConversationWithPDFContents(
		context.Background(),
		loadedConfig,
		"openai/main-model",
		sourceMessage,
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: summarize the report"}},
	)
	if err != nil {
		t.Fatalf("augment main conversation with pdf contents: %v", err)
	}

	mainContent, contentOK := mainConversation[0].Content.(string)
	if !contentOK {
		t.Fatalf("unexpected main content type: %T", mainConversation[0].Content)
	}

	if !strings.Contains(mainContent, pdfContentOpenTag) {
		t.Fatalf("expected extracted pdf text in main conversation: %q", mainContent)
	}

	searchConversation, err := instance.buildSearchDeciderConversation(
		context.Background(),
		loadedConfig,
		"openai/main-model",
		loadedConfig.SearchDeciderModel,
		sourceMessage,
		mainConversation,
	)
	if err != nil {
		t.Fatalf("build search decider conversation: %v", err)
	}

	parts, ok := searchConversation[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected search decider content type: %T", searchConversation[0].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected search decider part count: %d", len(parts))
	}

	textValue, _ := parts[0]["text"].(string)
	if !strings.Contains(textValue, "Extracted images: 1 total.") {
		t.Fatalf("expected extracted pdf image summary in decider prompt: %q", textValue)
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected extracted pdf image part for vision decider: %#v", parts[1])
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
		_ config,
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
		"openai/main-model",
		nil,
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

	assertSearchDeciderRequestIncludesInstruction(t, openAI.requests)

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

func assertSearchDeciderRequestIncludesInstruction(
	t *testing.T,
	requests []chatCompletionRequest,
) {
	t.Helper()

	if len(requests) != 1 {
		t.Fatalf("unexpected decider request count: %d", len(requests))
	}

	requestMessages := requests[0].Messages
	if len(requestMessages) != 4 {
		t.Fatalf("unexpected decider message count: %d", len(requestMessages))
	}

	if requestMessages[0].Role != "system" {
		t.Fatalf("expected search decider system prompt, got role %q", requestMessages[0].Role)
	}

	if requestMessages[2].Role != messageRoleUser {
		t.Fatalf("expected latest query before decider instruction, got role %q", requestMessages[2].Role)
	}

	if requestMessages[3].Role != messageRoleUser {
		t.Fatalf("expected decider instruction user role, got %q", requestMessages[3].Role)
	}

	instruction, instructionOK := requestMessages[3].Content.(string)
	if !instructionOK {
		t.Fatalf("unexpected decider instruction content type: %T", requestMessages[3].Content)
	}

	if instruction != searchDeciderDecisionInstruction {
		t.Fatalf("unexpected decider instruction: %q", instruction)
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
		_ config,
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
		"openai/main-model",
		nil,
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
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		return nil, errSearchBackendUnavailable
	})

	instance := newSearchTestBot(openAI, webSearch)

	conversation := []chatMessage{{Role: messageRoleUser, Content: "<@123>: latest ai news"}}

	augmentedConversation, searchMetadata, warnings, err := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		testSearchConfig(),
		"openai/main-model",
		nil,
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
		endpoint:   httpServer.URL,
		httpClient: httpServer.Client(),
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
		mcp:    exaClient,
		tavily: tavilyClient,
	}

	results, err := client.search(context.Background(), testSearchConfig(), []string{"latest ai news"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(exaClient.calls) != 1 {
		t.Fatalf("unexpected Exa call count: %d", len(exaClient.calls))
	}

	if len(tavilyClient.calls) != 1 {
		t.Fatalf("unexpected Tavily call count: %d", len(tavilyClient.calls))
	}

	if len(results) != 1 || results[0].Query != "latest ai news" {
		t.Fatalf("unexpected fallback results: %#v", results)
	}
}

func TestRoutedWebSearchClientUsesTavilyAsPrimaryWhenConfigured(t *testing.T) {
	t.Parallel()

	mcpClient := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{{Query: "latest ai news", Text: "mcp result"}}, nil
	})
	tavilyClient := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{{Query: queries[0], Text: "tavily result"}}, nil
	})

	loadedConfig := testTavilySearchConfig()
	loadedConfig.WebSearch.PrimaryProvider = webSearchProviderKindTavily

	client := routedWebSearchClient{
		mcp:    mcpClient,
		tavily: tavilyClient,
	}

	results, err := client.search(context.Background(), loadedConfig, []string{"latest ai news"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(tavilyClient.calls) != 1 {
		t.Fatalf("unexpected Tavily call count: %d", len(tavilyClient.calls))
	}

	if len(mcpClient.calls) != 0 {
		t.Fatalf("expected MCP to be skipped, got %d calls", len(mcpClient.calls))
	}

	if len(results) != 1 || results[0].Text != "tavily result" {
		t.Fatalf("unexpected primary Tavily results: %#v", results)
	}
}

func TestRoutedWebSearchClientFallsBackToMCPWhenTavilyFails(t *testing.T) {
	t.Parallel()

	mcpClient := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{{Query: queries[0], Text: "mcp fallback result"}}, nil
	})
	tavilyClient := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		return nil, errSearchBackendUnavailable
	})

	loadedConfig := testTavilySearchConfig()
	loadedConfig.WebSearch.PrimaryProvider = webSearchProviderKindTavily

	client := routedWebSearchClient{
		mcp:    mcpClient,
		tavily: tavilyClient,
	}

	results, err := client.search(context.Background(), loadedConfig, []string{"latest ai news"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(tavilyClient.calls) != 1 {
		t.Fatalf("unexpected Tavily call count: %d", len(tavilyClient.calls))
	}

	if len(mcpClient.calls) != 1 {
		t.Fatalf("unexpected MCP call count: %d", len(mcpClient.calls))
	}

	if len(results) != 1 || results[0].Text != "mcp fallback result" {
		t.Fatalf("unexpected MCP fallback results: %#v", results)
	}
}

func TestTavilySearchClientSearchRetriesConfiguredAPIKeys(t *testing.T) {
	t.Parallel()

	var (
		requestsMu   sync.Mutex
		authHeaders  []string
		searchBodies []tavilySearchRequest
	)

	client, closeServer := newTavilySearchTestClient(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		var body tavilySearchRequest

		err := json.NewDecoder(request.Body).Decode(&body)
		if err != nil {
			t.Errorf("decode request body: %v", err)
			responseWriter.WriteHeader(http.StatusBadRequest)

			return
		}

		authHeader := request.Header.Get("Authorization")

		requestsMu.Lock()

		defer requestsMu.Unlock()

		authHeaders = append(authHeaders, authHeader)
		searchBodies = append(searchBodies, body)

		switch authHeader {
		case testTavilyPrimaryAuthHeader:
			http.Error(responseWriter, "rate limited", http.StatusTooManyRequests)
		case testTavilyBackupAuthHeader:
			writeTavilySearchResponse(t, responseWriter, testTavilySearchSuccessResponse())
		default:
			http.Error(responseWriter, "unexpected api key", http.StatusUnauthorized)
		}
	}))
	defer closeServer()

	results, err := client.search(context.Background(), testTavilySearchConfig(), []string{"latest ai news"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	assertTavilyAuthHeaders(t, authHeaders)

	if len(searchBodies) != 2 {
		t.Fatalf("unexpected Tavily request count: %d", len(searchBodies))
	}

	assertTavilySearchRequest(t, searchBodies[0])

	if len(results) != 1 {
		t.Fatalf("unexpected result count: %d", len(results))
	}

	assertTavilyRawContentResult(t, results[0])
}

func TestTavilySearchClientSearchAttemptsAllKeysBeforeFailure(t *testing.T) {
	t.Parallel()

	var (
		requestsMu  sync.Mutex
		authHeaders []string
	)

	client, closeServer := newTavilySearchTestClient(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		authHeader := request.Header.Get("Authorization")

		requestsMu.Lock()

		defer requestsMu.Unlock()

		authHeaders = append(authHeaders, authHeader)

		switch authHeader {
		case testTavilyPrimaryAuthHeader:
			http.Error(responseWriter, "invalid key", http.StatusUnauthorized)
		case testTavilyBackupAuthHeader:
			http.Error(responseWriter, "rate limited", http.StatusTooManyRequests)
		default:
			http.Error(responseWriter, "unexpected api key", http.StatusUnauthorized)
		}
	}))
	defer closeServer()

	_, err := client.search(context.Background(), testTavilySearchConfig(), []string{"latest ai news"})
	if err == nil {
		t.Fatal("expected Tavily search to fail after exhausting keys")
	}

	if !strings.Contains(err.Error(), "all configured Tavily API keys failed") {
		t.Fatalf("unexpected Tavily error: %v", err)
	}

	assertTavilyAuthHeaders(t, authHeaders)
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
		MaxURLs: defaultWebSearchMaxURLs,
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
		Queries: []string{"latest ai news"},
		Results: []webSearchResult{
			{
				Query: "latest ai news",
				Text:  "URL: https://example.com/source\n",
			},
		},
		MaxURLs: defaultWebSearchMaxURLs,
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
		Results: []webSearchResult{
			{
				Query: "latest ai news",
				Text:  resultText.String(),
			},
		},
		MaxURLs: 2,
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
	loadedConfig.WebSearch.PrimaryProvider = webSearchProviderKindMCP
	loadedConfig.WebSearch.MaxURLs = defaultWebSearchMaxURLs
	loadedConfig.ModelOrder = []string{"openai/main-model", "openai/decider-model"}
	loadedConfig.SearchDeciderModel = "openai/decider-model"

	return *loadedConfig
}

func testGeminiSearchConfig() config {
	loadedConfig := new(config)
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.WebSearch.PrimaryProvider = webSearchProviderKindMCP
	loadedConfig.WebSearch.MaxURLs = defaultWebSearchMaxURLs

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
