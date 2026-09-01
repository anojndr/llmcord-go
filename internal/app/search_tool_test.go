package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	providers "llmcord-go/internal/providers"

	"github.com/bwmarrin/discordgo"
)

const (
	testWebSearchResultText = "First search result text"
	testWebSearchToolAnswer = "The answer after searching."
	testWebSearchTavilyKey  = "tavily-test-key"
	testWebSearchQueryOne   = "first query"
	testWebSearchQueryTwo   = "second query"
	testWebSearchChannelID  = "channel-1"
	testWebSearchBotUserID  = "bot-user"
	testWebSearchUserID     = "user-1"
	testWebSearchMessageID  = "user-message-1"
	testWebSearchMainModel  = "openai/main-model"
)

var errWebSearchBackendDown = errors.New("search backend down")

func newWebSearchToolTestConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.Models = map[string]map[string]any{
		testWebSearchMainModel: nil,
	}
	loadedConfig.ModelOrder = []string{testWebSearchMainModel}
	loadedConfig.WebSearch.Tavily = tavilySearchConfig{
		APIKey:  testWebSearchTavilyKey,
		APIKeys: []string{testWebSearchTavilyKey},
	}

	return loadedConfig
}

func newWebSearchToolTestSession(t *testing.T) *discordgo.Session {
	t.Helper()

	return newDirectMessageTestSession(t, testWebSearchChannelID, testWebSearchBotUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		switch {
		case request.Method == http.MethodPost &&
			strings.HasSuffix(request.URL.Path, "/typing"):
			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost &&
			strings.HasSuffix(request.URL.Path, "/messages"):
			response := new(discordgo.Message)
			response.ID = "response-message"
			response.ChannelID = testWebSearchChannelID

			return newJSONResponse(t, request, response), nil
		case request.Method == http.MethodPatch &&
			strings.HasSuffix(request.URL.Path, "/messages/response-message"):
			return newJSONResponse(t, request, new(discordgo.Message)), nil
		default:
			return newNoContentResponse(request), nil
		}
	}))
}

func newWebSearchToolSourceMessage() *discordgo.Message {
	return newPromptMessage(
		testWebSearchMessageID,
		testWebSearchChannelID,
		testWebSearchUserID,
		testWebSearchBotUserID,
	)
}

func newSearchToolTestBot(
	t *testing.T,
	chatClient *stubChatCompletionClient,
	webSearch *stubWebSearchClient,
) *bot {
	t.Helper()

	instance := newSearchTestBot(chatClient, webSearch)
	instance.session = newWebSearchToolTestSession(t)

	return instance
}

func parallelWebSearchToolCalls() []providers.FunctionToolCall {
	return []providers.FunctionToolCall{
		{
			ID:        "call_1",
			Name:      providers.WebSearchToolName,
			Arguments: `{"objective": "Find first query results", "search_queries": ["` + testWebSearchQueryOne + `"]}`,
		},
		{
			ID:        "call_2",
			Name:      providers.WebSearchToolName,
			Arguments: `{"objective": "Find second query results", "search_queries": ["` + testWebSearchQueryTwo + `", "` + testWebSearchQueryOne + `"]}`,
		},
	}
}

func respondWithWebSearchTool(t *testing.T, instance *bot, loadedConfig config) {
	t.Helper()

	err := instance.respondToMessage(
		context.Background(),
		loadedConfig,
		newWebSearchToolSourceMessage(),
		testWebSearchMainModel,
	)
	if err != nil {
		t.Fatalf("respond to message: %v", err)
	}
}

func latestChatMessageText(messages []chatMessage) string {
	for _, message := range slices.Backward(messages) {
		if text, ok := message.Content.(string); ok {
			return text
		}
	}

	return ""
}

func TestRespondToMessageExecutesWebSearchToolCalls(t *testing.T) {
	t.Parallel()

	var requestTools [][]providers.FunctionTool

	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		requestTools = append(requestTools, request.Tools)

		if len(requestTools) == 1 {
			return handle(streamDelta{
				ToolCalls:    parallelWebSearchToolCalls(),
				FinishReason: "tool_calls",
			})
		}

		if len(request.Tools) != 0 {
			t.Errorf("expected the follow-up request to carry no tools, got %#v", request.Tools)
		}

		return handle(newStreamDelta(testWebSearchToolAnswer, finishReasonStop))
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{
			{Query: queries[0], Text: testWebSearchResultText},
			{Query: queries[1], Text: "Second search result text"},
		}, nil
	})

	instance := newSearchToolTestBot(t, chatClient, webSearch)

	respondWithWebSearchTool(t, instance, newWebSearchToolTestConfig())

	if len(requestTools) != 2 {
		t.Fatalf("expected 2 chat completion requests, got %d", len(requestTools))
	}

	if len(requestTools[0]) != 1 || requestTools[0][0].Name != providers.WebSearchToolName {
		t.Fatalf("expected the web search tool on the first request, got %#v", requestTools[0])
	}

	if len(webSearch.calls) != 1 {
		t.Fatalf("expected exactly 1 web search call, got %d", len(webSearch.calls))
	}

	if !slices.Equal(webSearch.calls[0], []string{testWebSearchQueryOne, testWebSearchQueryTwo}) {
		t.Fatalf("unexpected search queries: %#v", webSearch.calls[0])
	}

	if len(chatClient.requests) != 2 {
		t.Fatalf("expected the follow-up request, got %d requests", len(chatClient.requests))
	}

	followUpText := latestChatMessageText(chatClient.requests[1].Messages)
	if !strings.Contains(followUpText, testWebSearchResultText) {
		t.Fatalf("expected search results in the follow-up request, got: %q", followUpText)
	}

	firstText := latestChatMessageText(chatClient.requests[0].Messages)
	if strings.Contains(firstText, testWebSearchResultText) {
		t.Fatalf(
			"expected the first request to be sent without search results, got: %q",
			firstText,
		)
	}
}

func TestRespondToMessageRetainsWebSearchResultsInConversationHistory(t *testing.T) {
	t.Parallel()

	var requestTools [][]providers.FunctionTool

	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		requestTools = append(requestTools, request.Tools)

		if len(requestTools) == 1 {
			return handle(streamDelta{
				ToolCalls:    parallelWebSearchToolCalls(),
				FinishReason: "tool_calls",
			})
		}

		return handle(newStreamDelta(testWebSearchToolAnswer, finishReasonStop))
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{
			{Query: queries[0], Text: testWebSearchResultText},
		}, nil
	})

	instance := newSearchToolTestBot(t, chatClient, webSearch)
	sourceMessage := newWebSearchToolSourceMessage()

	err := instance.respondToMessage(
		context.Background(),
		newWebSearchToolTestConfig(),
		sourceMessage,
		testWebSearchMainModel,
	)
	if err != nil {
		t.Fatalf("respond to message: %v", err)
	}

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = "response-message"
	assistantMessage.ChannelID = testWebSearchChannelID
	assistantMessage.Author = newDiscordUser(testWebSearchBotUserID, true)
	assistantMessage.MessageReference = sourceMessage.Reference()
	assistantMessage.Type = discordgo.MessageTypeReply

	followUpMessage := new(discordgo.Message)
	followUpMessage.ID = "follow-up-message"
	followUpMessage.ChannelID = testWebSearchChannelID
	followUpMessage.Author = newDiscordUser(testWebSearchUserID, false)
	followUpMessage.MessageReference = assistantMessage.Reference()
	followUpMessage.Type = discordgo.MessageTypeReply
	followUpMessage.Content = "what did the search say?"

	setCachedUserNode(
		instance,
		followUpMessage,
		assistantMessage,
		followUpMessage.Content,
	)

	history, _ := instance.buildConversation(
		context.Background(),
		followUpMessage,
		messageContentOptions{
			maxImages: defaultMaxImages,
		},
		defaultMaxMessages,
		false,
		false,
	)

	if len(history) != 3 {
		t.Fatalf("expected 3 messages in history (user, assistant, follow-up), got %d", len(history))
	}

	initialUserTurnText := messageContentText(history[0].Content)
	if !strings.Contains(initialUserTurnText, testWebSearchResultText) {
		t.Fatalf("expected search results to be retained in conversation history, got: %q", initialUserTurnText)
	}
	if !strings.Contains(initialUserTurnText, webSearchSectionName) {
		t.Fatalf("expected web search section heading in conversation history, got: %q", initialUserTurnText)
	}
}

func TestRespondToMessageWebSearchFailureAnswersWithoutResults(t *testing.T) {
	t.Parallel()

	var followUpHasResults bool

	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		if len(request.Tools) == 0 {
			for _, message := range request.Messages {
				if text, ok := message.Content.(string); ok &&
					strings.Contains(text, testWebSearchResultText) {
					followUpHasResults = true
				}
			}

			return handle(newStreamDelta(testWebSearchToolAnswer, finishReasonStop))
		}

		return handle(streamDelta{
			ToolCalls: []providers.FunctionToolCall{{
				ID:        "call_1",
				Name:      providers.WebSearchToolName,
				Arguments: `{"objective": "Find first query results", "search_queries": ["` + testWebSearchQueryOne + `"]}`,
			}},
			FinishReason: "tool_calls",
		})
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		return nil, errWebSearchBackendDown
	})

	instance := newSearchToolTestBot(t, chatClient, webSearch)

	respondWithWebSearchTool(t, instance, newWebSearchToolTestConfig())

	if followUpHasResults {
		t.Fatal("expected no search results in the follow-up request after a failed search")
	}

	if len(chatClient.requests) != 2 {
		t.Fatalf("expected a follow-up request without tools, got %d requests", len(chatClient.requests))
	}
}

type searchToolAttachCase struct {
	name         string
	modifyConfig func(config) config
	expectTool   bool
}

func runSearchToolAttachCase(t *testing.T, testCase searchToolAttachCase) {
	t.Helper()

	var requestTools [][]providers.FunctionTool

	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		requestTools = append(requestTools, request.Tools)

		return handle(newStreamDelta(testWebSearchToolAnswer, finishReasonStop))
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		t.Error("unexpected web search call")

		return nil, nil
	})

	instance := newSearchToolTestBot(t, chatClient, webSearch)

	respondWithWebSearchTool(t, instance, testCase.modifyConfig(newWebSearchToolTestConfig()))

	toolAttached := len(requestTools) > 0 && len(requestTools[0]) > 0
	if toolAttached != testCase.expectTool {
		t.Fatalf(
			"unexpected tool attachment: got %v, want %v (tools %#v)",
			toolAttached,
			testCase.expectTool,
			requestTools,
		)
	}
}

func TestRespondToMessageSearchToolAttachConditions(t *testing.T) {
	t.Parallel()

	disableWebSearch := func(loadedConfig config) config {
		provider := loadedConfig.Providers["openai"]
		provider.DisableWebSearch = true
		loadedConfig.Providers["openai"] = provider

		return loadedConfig
	}

	noAPIKeys := func(loadedConfig config) config {
		loadedConfig.WebSearch.Tavily = tavilySearchConfig{}

		return loadedConfig
	}

	groundingEnabled := func(loadedConfig config) config {
		provider := loadedConfig.Providers["openai"]
		provider.EnableGrounding = true
		loadedConfig.Providers["openai"] = provider

		return loadedConfig
	}

	testCases := []searchToolAttachCase{
		{
			name:         "attached when search keys are configured",
			modifyConfig: func(loadedConfig config) config { return loadedConfig },
			expectTool:   true,
		},
		{
			name:         "skipped when the provider disables web search",
			modifyConfig: disableWebSearch,
			expectTool:   false,
		},
		{
			name:         "skipped when no search API keys are configured",
			modifyConfig: noAPIKeys,
			expectTool:   false,
		},
		{
			name:         "skipped when grounding is enabled",
			modifyConfig: groundingEnabled,
			expectTool:   false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			runSearchToolAttachCase(t, testCase)
		})
	}
}

func TestExtractWebSearchQueries(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		toolCalls []providers.FunctionToolCall
		expected  []string
	}{
		{
			name: "parses search_queries and objective from parallel calls and dedupes",
			toolCalls: []providers.FunctionToolCall{
				{ID: "a", Name: providers.WebSearchToolName, Arguments: `{"objective": "Find latest AI news", "search_queries": ["alpha", "beta"]}`},
				{ID: "b", Name: providers.WebSearchToolName, Arguments: `{"objective": "Find latest AI news", "search_queries": ["beta", "gamma"]}`},
			},
			expected: []string{"alpha", "beta", "gamma"},
		},
		{
			name: "supports legacy queries property for backward compatibility",
			toolCalls: []providers.FunctionToolCall{
				{ID: "a", Name: providers.WebSearchToolName, Arguments: `{"queries": ["alpha", "beta"]}`},
				{ID: "b", Name: providers.WebSearchToolName, Arguments: `{"queries": ["beta", "gamma"]}`},
			},
			expected: []string{"alpha", "beta", "gamma"},
		},
		{
			name: "ignores unknown tools and malformed arguments",
			toolCalls: []providers.FunctionToolCall{
				{ID: "a", Name: "other_tool", Arguments: `{"queries": ["ignored"]}`},
				{ID: "b", Name: providers.WebSearchToolName, Arguments: `not json`},
				{ID: "c", Name: providers.WebSearchToolName, Arguments: `{"queries": ["kept"]}`},
			},
			expected: []string{"kept"},
		},
		{
			name: "trims and drops empty queries",
			toolCalls: []providers.FunctionToolCall{
				{ID: "a", Name: providers.WebSearchToolName, Arguments: `{"queries": ["  spaced  ", "", "kept"]}`},
			},
			expected: []string{"spaced", "kept"},
		},
		{
			name: "allows unlimited number of queries",
			toolCalls: []providers.FunctionToolCall{
				{
					ID:        "a",
					Name:      providers.WebSearchToolName,
					Arguments: `{"search_queries": ["one", "two", "three", "four", "five", "six", "seven"]}`,
				},
			},
			expected: []string{"one", "two", "three", "four", "five", "six", "seven"},
		},
		{
			name:      "no tool calls",
			toolCalls: nil,
			expected:  nil,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			queries := extractWebSearchQueries(testCase.toolCalls)

			if !slices.Equal(queries, testCase.expected) {
				t.Fatalf("unexpected queries: got %#v, want %#v", queries, testCase.expected)
			}
		})
	}
}

func TestRunWebSearchToolPhaseMergesMetadataAndUsesConfiguredExaType(t *testing.T) {
	t.Parallel()

	var seenSearchType string

	chatClient := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		return nil
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		loadedConfig config,
		_ []string,
	) ([]webSearchResult, error) {
		seenSearchType = loadedConfig.WebSearch.Exa.SearchType

		return []webSearchResult{
			{Query: testWebSearchQueryOne, Text: testWebSearchResultText},
		}, nil
	})

	instance := newSearchToolTestBot(t, chatClient, webSearch)
	instance.setCurrentExaSearchType(exaSearchTypeDeep)

	requestMessages := []chatMessage{
		{Role: messageRoleUser, Content: "<@bot-user> " + testWebSearchQueryOne},
	}
	tracker := newResponseTracker(newWebSearchToolSourceMessage(), testWebSearchMainModel)

	_, warnings, searched := instance.runWebSearchToolPhase(
		context.Background(),
		newWebSearchToolTestConfig(),
		tracker,
		requestMessages,
		nil,
		[]providers.FunctionToolCall{{
			ID:        "call_1",
			Name:      providers.WebSearchToolName,
			Arguments: `{"objective": "Find first query results", "search_queries": ["` + testWebSearchQueryOne + `"]}`,
		}},
	)

	if !searched {
		t.Fatal("expected the tool phase to report usable results")
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if seenSearchType != exaSearchTypeDeep {
		t.Fatalf(
			"expected the configured exa search type to reach the search client, got %q",
			seenSearchType,
		)
	}

	if tracker.searchMetadata == nil ||
		!slices.Equal(tracker.searchMetadata.Queries, []string{testWebSearchQueryOne}) ||
		len(tracker.searchMetadata.Results) != 1 {
		t.Fatalf(
			"expected search metadata merged into the tracker, got: %#v",
			tracker.searchMetadata,
		)
	}
}

func TestRunWebSearchToolPhaseWarnsOnSearchFailure(t *testing.T) {
	t.Parallel()

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		return nil, errWebSearchBackendDown
	})

	chatClient := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		t.Error("unexpected chat completion call")
		return nil
	})

	instance := newSearchToolTestBot(t, chatClient, webSearch)
	tracker := newResponseTracker(newWebSearchToolSourceMessage(), testWebSearchMainModel)

	_, warnings, searched := instance.runWebSearchToolPhase(
		context.Background(),
		newWebSearchToolTestConfig(),
		tracker,
		[]chatMessage{{Role: messageRoleUser, Content: "query"}},
		nil,
		[]providers.FunctionToolCall{{
			ID:        "call_1",
			Name:      providers.WebSearchToolName,
			Arguments: `{"objective": "Find first query results", "search_queries": ["` + testWebSearchQueryOne + `"]}`,
		}},
	)

	if searched {
		t.Fatal("expected searched=false after a failed search")
	}

	if len(warnings) != 1 || warnings[0] != searchWarningText {
		t.Fatalf("expected %q warning, got %#v", searchWarningText, warnings)
	}
}
