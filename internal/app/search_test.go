package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	providers "llmcord-go/internal/providers"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	loadedConfig.WebSearch.PrimaryProvider = webSearchProviderKindMCP
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

func newWebSearchTestSourceMessage(t *testing.T, content string) *discordgo.Message {
	t.Helper()

	message := new(discordgo.Message)
	message.ID = "web-search-test-source"
	message.ChannelID = "channel-1"
	message.Author = newDiscordUser("user-1", false)
	message.Content = content

	return message
}

// newSearchDeciderSourceMessageForQuery seeds a source user message in the
// instance's node store and returns it, so the search decider pipeline can
// walk a real reply chain for the given latest query.
func newSearchDeciderSourceMessageForQuery(
	t *testing.T,
	instance *bot,
	query string,
) *discordgo.Message {
	t.Helper()

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "web-search-source-message"
	sourceMessage.ChannelID = "channel-1"
	sourceMessage.Author = newDiscordUser("user-1", false)
	sourceMessage.Content = query

	setCachedUserNode(instance, sourceMessage, nil, query)

	return sourceMessage
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

func TestParseSearchDecisionRobustness(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		responseText  string
		expectSearch  bool
		expectQueries []string
	}{
		{
			name: "conversational prefix",
			responseText: "Sure! Here is the JSON decision:\n" +
				"```json\n{\"needs_search\":true,\"queries\":[\"test query\"]}\n```",
			expectSearch:  true,
			expectQueries: []string{"test query"},
		},
		{
			name:          "conversational suffix",
			responseText:  "{\"needs_search\":true,\"queries\":[\"test query\"]} -- hope this helps!",
			expectSearch:  true,
			expectQueries: []string{"test query"},
		},
		{
			name:          "conversational prefix and suffix",
			responseText:  "Here it is:\n{\"needs_search\":false}\nLet me know if you need more help.",
			expectSearch:  false,
			expectQueries: nil,
		},
		{
			name: "extra closing brace at end (ZoicWare log issue)",
			responseText: "{\"needs_search\": true, " +
				"\"queries\": [\"ZoicWare Windows 11 debloater\", \"ZoicWare Windows debloater comparison\"]}}",
			expectSearch:  true,
			expectQueries: []string{"ZoicWare Windows 11 debloater", "ZoicWare Windows debloater comparison"},
		},
		{
			name:          "extra closing brace with needs_search false",
			responseText:  "{\"needs_search\": false}}",
			expectSearch:  false,
			expectQueries: nil,
		},
		{
			name:          "trailing text containing braces",
			responseText:  "{\"needs_search\": true, \"queries\": [\"test query\"]} -- note: (see details {here})",
			expectSearch:  true,
			expectQueries: []string{"test query"},
		},
		{
			name:          "camelCase needsSearch with extra trailing brace",
			responseText:  "{\"needsSearch\": true, \"queries\": [\"test query\"]}}",
			expectSearch:  true,
			expectQueries: []string{"test query"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			decision, err := parseSearchDecision(testCase.responseText)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if decision.NeedsSearch != testCase.expectSearch {
				t.Fatalf("expected NeedsSearch to be %v, got %v", testCase.expectSearch, decision.NeedsSearch)
			}

			if testCase.expectSearch {
				if len(decision.Queries) != len(testCase.expectQueries) {
					t.Fatalf("expected %d queries, got %d (%#v)", len(testCase.expectQueries), len(decision.Queries), decision.Queries)
				}

				for idx, q := range testCase.expectQueries {
					if decision.Queries[idx] != q {
						t.Fatalf("expected query[%d] %q, got %q", idx, q, decision.Queries[idx])
					}
				}
			}
		})
	}
}

func TestSearchDeciderPromptMatchesTextFile(t *testing.T) {
	t.Parallel()

	promptBytes, err := os.ReadFile("../searchtypes/searchDeciderPrompt.txt")
	if err != nil {
		t.Fatalf("read search decider prompt file: %v", err)
	}

	instant := time.Date(2026, time.March, 9, 13, 14, 15, 0, time.FixedZone("PHT", 8*60*60))
	expectedPrompt := systemPromptNow(strings.TrimSpace(string(promptBytes)), instant)

	if searchDeciderPrompt(instant) != expectedPrompt {
		t.Fatal("expected embedded search decider prompt to match searchDeciderPrompt.txt")
	}
}

func TestSearchDeciderPromptReplacesDateAndTime(t *testing.T) {
	t.Parallel()

	instant := time.Date(2026, time.March, 9, 13, 14, 15, 0, time.FixedZone("PHT", 8*60*60))
	prompt := searchDeciderPrompt(instant)

	expectedLine := "Today's date is March 09 2026. The current time is 13:14:15 PHT+0800."
	if !strings.Contains(prompt, expectedLine) {
		t.Fatalf("expected rendered search decider prompt to contain %q", expectedLine)
	}

	if strings.Contains(prompt, "{date}") || strings.Contains(prompt, "{time}") {
		t.Fatalf("expected rendered search decider prompt to replace placeholders: %q", prompt)
	}
}

func TestSearchDeciderPromptRetainsCriticalInstructions(t *testing.T) {
	t.Parallel()

	expectedSnippets := []string{
		`You are a search-decision model.`,
		`1. Check explicit search instructions first.`,
		`2. Use conversation context to resolve references.`,
		`3. Use both text and images.`,
		`4. Return {"needs_search": false} when the answer can be produced from what is already given.`,
		`5. Return {"needs_search": true, "queries": [...]} in all other cases, especially when the request involves:`,
		`8. Never search for content that is private, local, or otherwise unsearchable.`,
		`12. Weigh the date of the claimed facts against the freshness of the request.`,
		`18. Preserve the substance of the claim when the user asks to verify it.`,
	}

	instant := time.Date(2026, time.March, 9, 13, 14, 15, 0, time.FixedZone("PHT", 8*60*60))
	prompt := searchDeciderPrompt(instant)

	for _, expectedSnippet := range expectedSnippets {
		if !strings.Contains(prompt, expectedSnippet) {
			t.Fatalf("expected search decider prompt to contain %q", expectedSnippet)
		}
	}
}

func TestPrependSearchDeciderPrompt(t *testing.T) {
	t.Parallel()

	deciderPrompt := "Decider prompt content"

	t.Run("EmptyPrompt", func(t *testing.T) {
		t.Parallel()

		msgs := []chatMessage{{Role: messageRoleUser, Content: "query"}}
		res := prependSearchDeciderPrompt(msgs, "")

		if len(res) != 1 || res[0].Content != "query" {
			t.Fatalf("expected messages unchanged, got %#v", res)
		}
	})

	t.Run("EmptyMessages", func(t *testing.T) {
		t.Parallel()

		res := prependSearchDeciderPrompt(nil, deciderPrompt)

		if len(res) != 1 || res[0].Role != messageRoleUser || res[0].Content != deciderPrompt {
			t.Fatalf("unexpected result for empty messages: %#v", res)
		}
	})

	t.Run("LastMessageAssistant", func(t *testing.T) {
		t.Parallel()

		msgs := []chatMessage{{Role: messageRoleAssistant, Content: "Hello"}}
		res := prependSearchDeciderPrompt(msgs, deciderPrompt)

		if len(res) != 2 || res[1].Role != messageRoleUser || res[1].Content != deciderPrompt {
			t.Fatalf("unexpected result for assistant last message: %#v", res)
		}
	})

	t.Run("LastMessageUserString", func(t *testing.T) {
		t.Parallel()

		msgs := []chatMessage{
			{Role: messageRoleAssistant, Content: "Hello"},
			{Role: messageRoleUser, Content: "What is the capital of France?"},
		}
		res := prependSearchDeciderPrompt(msgs, deciderPrompt)

		if len(res) != 2 {
			t.Fatalf("unexpected message count: %d", len(res))
		}

		expectedContent := deciderPrompt + "\n\nLatest user query:\nWhat is the capital of France?"

		if res[1].Content != expectedContent {
			t.Fatalf("expected content %q, got %q", expectedContent, res[1].Content)
		}
	})

	t.Run("LastMessageUserParts", func(t *testing.T) {
		t.Parallel()

		parts := []contentPart{
			{messageTypeKey: contentTypeText, messageTextKey: "Check this image"},
			{messageTypeKey: contentTypeImageURL, messageTextKey: "http://example.com/img.png"},
		}
		msgs := []chatMessage{{Role: messageRoleUser, Content: parts}}
		res := prependSearchDeciderPrompt(msgs, deciderPrompt)

		if len(res) != 1 {
			t.Fatalf("unexpected message count: %d", len(res))
		}

		resParts, ok := res[0].Content.([]contentPart)
		if !ok {
			t.Fatalf("unexpected content type: %T", res[0].Content)
		}

		if len(resParts) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(resParts))
		}

		expectedPartText := deciderPrompt + "\n\nLatest user query:\nCheck this image"

		if resParts[0][messageTypeKey] != contentTypeText || resParts[0][messageTextKey] != expectedPartText {
			t.Fatalf("unexpected first part: %#v", resParts[0])
		}

		if resParts[1][messageTypeKey] != contentTypeImageURL {
			t.Fatalf("unexpected second part: %#v", resParts[1])
		}
	})
}

// TestSearchDeciderConversationStripsImagesForTextOnlyModels asserts that
// the search decider conversation filters media through the decider model's
// own content options, exactly like the main model pipeline: a text-only
// decider model receives the message text without any media parts.
func TestSearchDeciderConversationStripsImagesForTextOnlyModels(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newSearchDeciderMediaTestBot(
		t,
		"message-search-text-only",
		"<@123>: what is this?",
		[]contentPart{
			{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
		},
	)

	loadedConfig := testSearchConfig()
	loadedConfig.SearchDeciderModel = "openai/text-only-model"
	loadedConfig.Models["openai/text-only-model"] = nil

	deciderMessages, err := instance.buildSearchDeciderConversation(
		context.Background(),
		loadedConfig,
		"openai/main-model",
		loadedConfig.SearchDeciderModel,
		sourceMessage,
		nil,
	)
	if err != nil {
		t.Fatalf("build search decider conversation: %v", err)
	}

	content, ok := deciderMessages[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected decider content type: %T", deciderMessages[0].Content)
	}

	if content != "<@123>: what is this?" {
		t.Fatalf("unexpected decider content: %q", content)
	}
}

// TestSearchDeciderConversationPreservesGeminiMedia asserts that a gemini
// search decider model keeps its media parts through the same content
// options path as the main model.
func TestSearchDeciderConversationPreservesGeminiMedia(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newSearchDeciderMediaTestBot(
		t,
		"message-search-gemini-media",
		"<@123>: summarize these",
		[]contentPart{
			{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
		},
	)

	loadedConfig := testGeminiSearchConfig()

	deciderMessages, err := instance.buildSearchDeciderConversation(
		context.Background(),
		loadedConfig,
		"gemini/gemini-3-flash-preview",
		"gemini/gemini-3-flash-preview",
		sourceMessage,
		nil,
	)
	if err != nil {
		t.Fatalf("build search decider conversation: %v", err)
	}

	parts, ok := deciderMessages[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected decider content type: %T", deciderMessages[0].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected image to be preserved: %#v", parts[1])
	}
}

// newSearchDeciderMediaTestBot builds a bot with a session and a source
// message whose node carries the given media, so the search decider
// conversation pipeline can walk the source message and filter its media by
// the decider model's content options.
func newSearchDeciderMediaTestBot(
	t *testing.T,
	messageID string,
	text string,
	media []contentPart,
) (*bot, *discordgo.Message) {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.State.User = newDiscordUser("bot-user", true)

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(maxMessageNodes)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = messageID
	sourceMessage.ChannelID = "channel-1"
	sourceMessage.Author = newDiscordUser("user-1", false)
	sourceMessage.Content = text

	sourceNode := instance.nodes.getOrCreate(sourceMessage.ID)
	sourceNode.initialized = true
	sourceNode.role = messageRoleUser
	sourceNode.text = text
	sourceNode.media = media

	return instance, sourceMessage
}

func TestBuildSearchDeciderConversationAppendsPDFImagesForVisionDecider(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newSearchDeciderMediaTestBot(
		t,
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

	if !strings.Contains(mainContent, documentContentSectionName+":") {
		t.Fatalf("expected extracted document section in main conversation: %q", mainContent)
	}

	searchConversation, err := instance.buildSearchDeciderConversation(
		context.Background(),
		loadedConfig,
		"openai/main-model",
		loadedConfig.SearchDeciderModel,
		sourceMessage,
		nil,
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

	if !strings.Contains(textValue, documentContentSectionName+":") {
		t.Fatalf("expected extracted document section in decider prompt: %q", textValue)
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected extracted pdf image part for vision decider: %#v", parts[1])
	}
}

func TestBuildSearchDeciderConversationAppendsPPTXImagesForVisionDecider(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newSearchDeciderMediaTestBot(
		t,
		"message-search-pptx",
		"<@123>: summarize the slides",
		[]contentPart{
			testPPTXDocumentPart(t, "Slide text about quarterly revenue growth."),
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
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: summarize the slides"}},
	)
	if err != nil {
		t.Fatalf("augment main conversation with pptx contents: %v", err)
	}

	mainContent, contentOK := mainConversation[0].Content.(string)
	if !contentOK {
		t.Fatalf("unexpected main content type: %T", mainConversation[0].Content)
	}

	if !strings.Contains(mainContent, ooxmlContentOpenTag) {
		t.Fatalf("expected extracted OOXML text in main conversation: %q", mainContent)
	}

	searchConversation, err := instance.buildSearchDeciderConversation(
		context.Background(),
		loadedConfig,
		"openai/main-model",
		loadedConfig.SearchDeciderModel,
		sourceMessage,
		nil,
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
		t.Fatalf("expected extracted pptx image summary in decider prompt: %q", textValue)
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected extracted pptx image part for vision decider: %#v", parts[1])
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

	sourceMessage := newSearchDeciderSourceMessageForQuery(t, instance, "<@123>: what changed?")

	augmentedConversation, searchMetadata, warnings := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		testSearchConfig(),
		"openai/main-model",
		sourceMessage,
		conversation,
	)

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

func TestMaybeAugmentConversationWithWebSearchPassesCurrentExaSearchType(t *testing.T) {
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

	var capturedSearchType string

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		loadedConfig config,
		queries []string,
	) ([]webSearchResult, error) {
		capturedSearchType = loadedConfig.WebSearch.Exa.SearchType

		return []webSearchResult{{Query: queries[0], Text: "AI news context"}}, nil
	})

	instance := newSearchTestBot(openAI, webSearch)
	instance.setCurrentExaSearchType(exaSearchTypeDeep)

	augmentedConversation, searchMetadata, warnings := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		testExaAPIWebSearchConfig(),
		"openai/main-model",
		newWebSearchTestSourceMessage(t, "<@123>: what changed?"),
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: what changed?"}},
	)

	if len(augmentedConversation) != 1 {
		t.Fatalf("unexpected augmented conversation length: %d", len(augmentedConversation))
	}

	if searchMetadata == nil {
		t.Fatal("expected search metadata")
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if capturedSearchType != exaSearchTypeDeep {
		t.Fatalf("unexpected Exa search type: %q", capturedSearchType)
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
	if len(requestMessages) == 0 {
		t.Fatal("expected decider request messages")
	}

	latestIndex := len(requestMessages) - 1

	if requestMessages[latestIndex].Role != messageRoleUser {
		t.Fatalf(
			"expected latest query user message last, got role %q",
			requestMessages[latestIndex].Role,
		)
	}

	userContent, userContentOK := requestMessages[latestIndex].Content.(string)
	if !userContentOK {
		t.Fatalf("unexpected user message content type: %T", requestMessages[1].Content)
	}

	if !strings.Contains(userContent, "Today's date is ") {
		t.Fatalf("expected rendered date in prepended search decider prompt: %q", userContent)
	}

	if strings.Contains(userContent, "{date}") || strings.Contains(userContent, "{time}") {
		t.Fatalf("expected rendered search decider prompt without placeholders: %q", userContent)
	}

	if !strings.Contains(userContent, "You are a search-decision model.") {
		t.Fatalf("expected search decider prompt in latest query: %q", userContent)
	}

	if !strings.Contains(userContent, "Latest user query:\n") {
		t.Fatalf("expected latest user query label: %q", userContent)
	}

	if !strings.Contains(userContent, "<@123>: what changed?") {
		t.Fatalf("expected user query in latest query: %q", userContent)
	}

	expectedInstructionSuffix := "\n\n" + searchDeciderDecisionInstruction
	if !strings.HasSuffix(userContent, expectedInstructionSuffix) {
		t.Fatalf("expected decider instruction suffix: %q, got %q", expectedInstructionSuffix, userContent)
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

	augmentedConversation, searchMetadata, warnings := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		testSearchConfig(),
		"openai/main-model",
		newWebSearchTestSourceMessage(t, messageContentText(conversation[0].Content)),
		conversation,
	)

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

func TestMaybeAugmentConversationWithWebSearchSkipsDeciderWhenProviderDisabled(t *testing.T) {
	t.Parallel()

	openAI := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		t.Fatal("expected search decider to be skipped when disable_search_decider is set")

		return nil
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		t.Fatal("expected web search to be skipped when disable_search_decider is set")

		return nil, nil
	})

	instance := newSearchTestBot(openAI, webSearch)

	loadedConfig := testSearchConfig()
	openAIProvider := loadedConfig.Providers["openai"]
	openAIProvider.DisableSearchDecider = true
	loadedConfig.Providers["openai"] = openAIProvider

	conversation := []chatMessage{{Role: messageRoleUser, Content: "<@123>: latest ai news"}}

	augmentedConversation, searchMetadata, warnings := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		loadedConfig,
		"openai/main-model",
		newWebSearchTestSourceMessage(t, messageContentText(conversation[0].Content)),
		conversation,
	)

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if searchMetadata != nil {
		t.Fatalf("expected search metadata to be nil: %#v", searchMetadata)
	}

	if augmentedConversation[0].Content != conversation[0].Content {
		t.Fatal("expected conversation to remain unchanged")
	}

	if len(openAI.requests) != 0 {
		t.Fatalf("expected no search decider requests, got %d", len(openAI.requests))
	}

	if len(webSearch.calls) != 0 {
		t.Fatalf("expected no web search calls, got %d", len(webSearch.calls))
	}
}

func TestMaybeAugmentConversationWithWebSearchSkipsDeciderForExaResearchPro(t *testing.T) {
	t.Parallel()

	openAI := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		t.Fatal("expected search decider to be skipped for exa/exa-research-pro")

		return nil
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		t.Fatal("expected web search to be skipped for exa/exa-research-pro")

		return nil, nil
	})

	instance := newSearchTestBot(openAI, webSearch)

	loadedConfig := testSearchConfig()
	loadedConfig.Providers["exa"] = providerConfig{
		Name:                 "exa",
		BaseURL:              "",
		APIKey:               "",
		APIKeys:              nil,
		EnableGrounding:      false,
		DisableSearchDecider: false,
		ExtraHeaders:         nil,
		ExtraQuery:           nil,
		ExtraBody:            nil,
	}
	loadedConfig.Models["exa/exa-research-pro"] = nil
	loadedConfig.ModelOrder = append([]string{"exa/exa-research-pro"}, loadedConfig.ModelOrder...)

	conversation := []chatMessage{{Role: messageRoleUser, Content: "<@123>: latest ai news"}}

	augmentedConversation, searchMetadata, warnings := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		loadedConfig,
		"exa/exa-research-pro",
		newWebSearchTestSourceMessage(t, messageContentText(conversation[0].Content)),
		conversation,
	)

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if searchMetadata != nil {
		t.Fatalf("expected search metadata to be nil: %#v", searchMetadata)
	}

	if augmentedConversation[0].Content != conversation[0].Content {
		t.Fatal("expected conversation to remain unchanged")
	}

	if len(openAI.requests) != 0 {
		t.Fatalf("expected no search decider requests, got %d", len(openAI.requests))
	}

	if len(webSearch.calls) != 0 {
		t.Fatalf("expected no web search calls, got %d", len(webSearch.calls))
	}
}

func TestMaybeAugmentConversationWithWebSearchSkipsDeciderForXAIProvider(t *testing.T) {
	t.Parallel()

	openAI := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		t.Fatal("expected search decider to be skipped for x-ai provider")

		return nil
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		t.Fatal("expected web search to be skipped for x-ai provider")

		return nil, nil
	})

	instance := newSearchTestBot(openAI, webSearch)

	loadedConfig := testSearchConfig()
	loadedConfig.Providers["x-ai"] = providerConfig{
		Name:                 providers.XAIProviderName,
		BaseURL:              "https://api.x.ai/v1",
		APIKey:               "",
		APIKeys:              nil,
		EnableGrounding:      false,
		DisableSearchDecider: false,
		ExtraHeaders:         nil,
		ExtraQuery:           nil,
		ExtraBody:            nil,
	}
	loadedConfig.Models["x-ai/grok-4"] = nil
	loadedConfig.ModelOrder = append([]string{"x-ai/grok-4"}, loadedConfig.ModelOrder...)

	conversation := []chatMessage{{Role: messageRoleUser, Content: "<@123>: latest ai news"}}

	augmentedConversation, searchMetadata, warnings := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		loadedConfig,
		"x-ai/grok-4",
		newWebSearchTestSourceMessage(t, messageContentText(conversation[0].Content)),
		conversation,
	)

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if searchMetadata != nil {
		t.Fatalf("expected search metadata to be nil: %#v", searchMetadata)
	}

	if augmentedConversation[0].Content != conversation[0].Content {
		t.Fatal("expected conversation to remain unchanged")
	}

	if len(openAI.requests) != 0 {
		t.Fatalf("expected no search decider requests, got %d", len(openAI.requests))
	}

	if len(webSearch.calls) != 0 {
		t.Fatalf("expected no web search calls, got %d", len(webSearch.calls))
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

	augmentedConversation, searchMetadata, warnings := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		testSearchConfig(),
		"openai/main-model",
		newWebSearchTestSourceMessage(t, messageContentText(conversation[0].Content)),
		conversation,
	)

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

func TestMaybeAugmentConversationWithWebSearchFallsBackOnAppendError(t *testing.T) {
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
		return []webSearchResult{{Query: "latest ai news", Text: "some news"}}, nil
	})

	instance := newSearchTestBot(openAI, webSearch)

	// Empty conversation has no user message, so appending web search results fails.
	emptyConversation := []chatMessage{}

	augmentedConversation, searchMetadata, warnings := instance.maybeAugmentConversationWithWebSearch(
		context.Background(),
		testSearchConfig(),
		"openai/main-model",
		newWebSearchTestSourceMessage(t, ""),
		emptyConversation,
	)

	if len(warnings) != 1 || warnings[0] != searchWarningText {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if searchMetadata != nil {
		t.Fatalf("expected search metadata to be nil: %#v", searchMetadata)
	}

	if len(augmentedConversation) != 0 {
		t.Fatalf("expected empty conversation on append fallback, got: %#v", augmentedConversation)
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
		exa:    mcpClient,
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
		exa:    mcpClient,
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
		"openai/main-model":    nil,
		"openai/decider-model": nil,
	}
	loadedConfig.WebSearch.PrimaryProvider = webSearchProviderKindMCP
	loadedConfig.WebSearch.MaxURLs = defaultWebSearchMaxURLs
	loadedConfig.WebSearch.Exa = exaSearchConfig{
		APIKey:             "",
		APIKeys:            nil,
		SearchType:         defaultExaSearchType,
		TextMaxCharacters:  defaultExaSearchTextMaxCharacters,
		LivecrawlTimeoutMS: defaultExaContentsLivecrawlTimeoutMS,
	}
	loadedConfig.ModelOrder = []string{"openai/main-model", "openai/decider-model"}
	loadedConfig.SearchDeciderModel = "openai/decider-model"
	loadedConfig.MaxMessages = defaultMaxMessages

	return *loadedConfig
}

func testGeminiSearchConfig() config {
	loadedConfig := new(config)
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.WebSearch.PrimaryProvider = webSearchProviderKindMCP
	loadedConfig.WebSearch.MaxURLs = defaultWebSearchMaxURLs
	loadedConfig.WebSearch.Exa = exaSearchConfig{
		APIKey:             "",
		APIKeys:            nil,
		SearchType:         defaultExaSearchType,
		TextMaxCharacters:  defaultExaSearchTextMaxCharacters,
		LivecrawlTimeoutMS: defaultExaContentsLivecrawlTimeoutMS,
	}

	geminiProvider := new(providerConfig)
	geminiProvider.Name = "gemini"
	geminiProvider.EnableGrounding = true

	loadedConfig.Providers = map[string]providerConfig{
		"gemini": *geminiProvider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"gemini/gemini-3-flash-preview": nil,
	}
	loadedConfig.MaxMessages = defaultMaxMessages

	return *loadedConfig
}

func newStreamableHTTPOptions() *mcp.StreamableHTTPOptions {
	options := new(mcp.StreamableHTTPOptions)
	options.Stateless = true
	options.JSONResponse = true

	return options
}

// TestDecideWebSearchRetriesEmptyDeciderResponse verifies that an empty model
// response from the search-decider stream is retried end to end (through the
// router's transient retry loop) and the retried decision is used.
func TestDecideWebSearchRetriesEmptyDeciderResponse(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		t.Helper()

		attempts.Add(1)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		if attempts.Load() == 1 {
			// Completes cleanly but with an empty response: no output text,
			// no usage. This is the "empty model response" the decider sees.
			writeStreamChunk(
				t,
				responseWriter,
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_x\","+
					"\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			)
			writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")

			return
		}

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"{\\\"needs_search\\\":false}\"}\n\n",
		)
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_x\","+
				"\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
		)
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
	}))
	defer server.Close()

	router := providers.NewChatCompletionRouter(server.Client())

	instance := newSearchTestBot(router, newStubWebSearchClient(func(
		context.Context,
		config,
		[]string,
	) ([]webSearchResult, error) {
		t.Fatalf("did not expect web search to run")

		return nil, nil
	}))
	instance.nodes = newMessageNodeStore(maxMessageNodes)

	loadedConfig := testSearchConfig()

	// Point the openai provider at the test server so the decider stream
	// reaches it and the main-model call stays within context.
	openAIProvider := loadedConfig.Providers["openai"]
	openAIProvider.BaseURL = server.URL
	loadedConfig.Providers["openai"] = openAIProvider

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "search-decider-retry-source"
	sourceMessage.ChannelID = "channel-1"
	sourceMessage.Author = newDiscordUser("user-1", false)
	sourceMessage.Content = "Check the latest news."

	decision, _, err := instance.decideWebSearch(
		context.Background(),
		loadedConfig,
		"openai/main-model",
		sourceMessage,
		[]chatMessage{{Role: messageRoleUser, Content: "Check the latest news."}},
	)
	if err != nil {
		t.Fatalf("decide web search: %v", err)
	}

	if decision.NeedsSearch {
		t.Fatal("expected decider to skip web search after retry")
	}

	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 (empty decider response then retry)", attempts.Load())
	}
}

func writeStreamChunk(
	t *testing.T,
	responseWriter http.ResponseWriter,
	content string,
) {
	t.Helper()

	_, err := fmt.Fprint(responseWriter, content)
	if err != nil {
		t.Fatalf("write stream chunk: %v", err)
	}
}
