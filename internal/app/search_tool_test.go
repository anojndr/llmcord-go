package app

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
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
			ID:   "call_2",
			Name: providers.WebSearchToolName,
			Arguments: `{"objective": "Find second query results", "search_queries": ["` +
				testWebSearchQueryTwo + `", "` + testWebSearchQueryOne + `"]}`,
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

// toolCallDelta is the final stream delta of a response that requested the
// given function calls.
func toolCallDelta(calls ...providers.FunctionToolCall) streamDelta {
	return streamDelta{
		Thinking:           "",
		Content:            "",
		FinishReason:       "tool_calls",
		ProviderResponseID: "",
		SearchMetadata:     nil,
		ToolCallResponse:   &providers.ToolCallResponse{Calls: calls},
	}
}

// toolRoundOutput returns the output a tool round carries for a call.
func toolRoundOutput(round providers.ToolRound, callID string) (string, bool) {
	for _, output := range round.Outputs {
		if output.CallID == callID {
			return output.Output, true
		}
	}

	return "", false
}

// assertToolRoundOutputContains fails unless the round answers the call with
// an output containing want.
func assertToolRoundOutputContains(t *testing.T, round providers.ToolRound, callID string, want string) {
	t.Helper()

	output, found := toolRoundOutput(round, callID)
	if !found {
		t.Fatalf("expected an output for call %q, got %#v", callID, round.Outputs)
	}

	if !strings.Contains(output, want) {
		t.Fatalf("expected the output for call %q to contain %q, got %q", callID, want, output)
	}
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
			return handle(toolCallDelta(parallelWebSearchToolCalls()...))
		}

		if len(request.Tools) == 0 {
			t.Error("expected the follow-up request to keep the tool definitions")
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

	followUp := chatClient.requests[1]
	if len(followUp.ToolRounds) != 1 {
		t.Fatalf("expected the follow-up to replay 1 tool round, got %d", len(followUp.ToolRounds))
	}

	round := followUp.ToolRounds[0]
	if round.Response == nil || len(round.Response.Calls) != 2 {
		t.Fatalf("expected the tool round to replay both calls, got %#v", round.Response)
	}

	// Every call gets its own output with the results for its queries.
	assertToolRoundOutputContains(t, round, "call_1", testWebSearchResultText)
	assertToolRoundOutputContains(t, round, "call_2", "Second search result text")
	assertToolRoundOutputContains(t, round, "call_2", testWebSearchResultText)

	if !reflect.DeepEqual(followUp.Messages, chatClient.requests[0].Messages) {
		t.Fatalf(
			"expected the follow-up to keep the conversation unchanged, got %#v",
			followUp.Messages,
		)
	}

	firstText := latestChatMessageText(chatClient.requests[0].Messages)
	if strings.Contains(firstText, testWebSearchResultText) {
		t.Fatalf(
			"expected the first request to be sent without search results, got: %q",
			firstText,
		)
	}
}

func TestRespondToMessageForcesFinalAnswerAfterOneWebSearchToolRound(t *testing.T) {
	t.Parallel()

	var roundRequests []chatCompletionRequest

	// The model would keep searching if it could: it answers only once
	// tool_choice "none" forbids new tool calls.
	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		roundRequests = append(roundRequests, request)

		if request.ToolChoice == providers.ToolChoiceNone {
			return handle(newStreamDelta(testWebSearchToolAnswer, finishReasonStop))
		}

		return handle(toolCallDelta(providers.FunctionToolCall{
			ID:        "call_round_" + strconv.Itoa(len(roundRequests)),
			Name:      providers.WebSearchToolName,
			Arguments: `{"objective": "Find query results", "search_queries": ["` + testWebSearchQueryOne + `"]}`,
		}))
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

	respondWithWebSearchTool(t, instance, newWebSearchToolTestConfig())

	if len(roundRequests) != 2 {
		t.Fatalf("expected 2 generation rounds (one tool round + forced final answer), got %d", len(roundRequests))
	}

	if len(webSearch.calls) != 1 {
		t.Fatalf("expected exactly 1 web search call, got %d", len(webSearch.calls))
	}

	toolRequest, finalRequest := roundRequests[0], roundRequests[1]
	if toolRequest.ToolChoice != "" {
		t.Fatalf("expected the tool round to leave tool_choice to the model, got %q", toolRequest.ToolChoice)
	}

	// The final round keeps the same tool definitions, so the prompt prefix
	// stays cacheable, but forbids new calls.
	if finalRequest.ToolChoice != providers.ToolChoiceNone || len(finalRequest.Tools) == 0 ||
		!reflect.DeepEqual(finalRequest.Tools, toolRequest.Tools) {
		t.Fatalf(
			"expected the final round to keep the tools with tool_choice none, got choice %q tools %#v",
			finalRequest.ToolChoice,
			finalRequest.Tools,
		)
	}

	if len(finalRequest.ToolRounds) != 1 {
		t.Fatalf("expected the final round to replay 1 tool round, got %d", len(finalRequest.ToolRounds))
	}

	assertToolRoundOutputContains(t, finalRequest.ToolRounds[0], "call_round_1", testWebSearchResultText)
}

// newToolChoiceIgnoringChatClient mimics a backend that does not enforce
// tool_choice "none" (for example a proxy that rewrites it to "auto"): it
// requests another web search whenever tools are offered, and answers only
// when none are. With keepCallingTools it calls a tool even then, like a
// backend that injects tools of its own.
func newToolChoiceIgnoringChatClient(keepCallingTools bool) *stubChatCompletionClient {
	var rounds int

	return newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		rounds++

		if len(request.Tools) == 0 && !keepCallingTools {
			return handle(newStreamDelta(testWebSearchToolAnswer, finishReasonStop))
		}

		return handle(toolCallDelta(providers.FunctionToolCall{
			ID:        "call_round_" + strconv.Itoa(rounds),
			Name:      providers.WebSearchToolName,
			Arguments: `{"objective": "Find query results", "search_queries": ["` + testWebSearchQueryOne + `"]}`,
		}))
	})
}

func newSingleResultWebSearchClient() *stubWebSearchClient {
	return newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{
			{Query: queries[0], Text: testWebSearchResultText},
		}, nil
	})
}

// assertToolFreeFinalAnswerRequest fails unless the final answer request
// offers no tools and carries the search results as text in the latest
// user message of the tool round's conversation.
func assertToolFreeFinalAnswerRequest(
	t *testing.T,
	finalRequest chatCompletionRequest,
	toolRequest chatCompletionRequest,
) {
	t.Helper()

	if len(finalRequest.Tools) != 0 || finalRequest.ToolChoice != "" || len(finalRequest.ToolRounds) != 0 {
		t.Fatalf(
			"expected the final answer without tools, got %d tools, choice %q, %d tool rounds",
			len(finalRequest.Tools),
			finalRequest.ToolChoice,
			len(finalRequest.ToolRounds),
		)
	}

	if len(finalRequest.Messages) != len(toolRequest.Messages) {
		t.Fatalf(
			"expected the final answer to keep the %d conversation messages, got %d",
			len(toolRequest.Messages),
			len(finalRequest.Messages),
		)
	}

	finalText := latestChatMessageText(finalRequest.Messages)
	if !strings.Contains(finalText, webSearchSectionName) || !strings.Contains(finalText, testWebSearchResultText) {
		t.Fatalf("expected the search results as text in the latest user message, got %q", finalText)
	}
}

func TestRespondToMessageAnswersWithoutToolsWhenBackendIgnoresToolChoiceNone(t *testing.T) {
	t.Parallel()

	chatClient := newToolChoiceIgnoringChatClient(false)
	webSearch := newSingleResultWebSearchClient()
	instance := newSearchToolTestBot(t, chatClient, webSearch)

	respondWithWebSearchTool(t, instance, newWebSearchToolTestConfig())

	// Tool round, forced final answer (tool_choice ignored), then the
	// final answer without tools.
	if len(chatClient.requests) != 3 {
		t.Fatalf("expected 3 generation rounds, got %d", len(chatClient.requests))
	}

	// Calls made despite tool_choice "none" are never executed.
	if len(webSearch.calls) != 1 {
		t.Fatalf("expected exactly 1 web search call, got %d", len(webSearch.calls))
	}

	toolRequest, forcedRequest := chatClient.requests[0], chatClient.requests[1]
	if forcedRequest.ToolChoice != providers.ToolChoiceNone || len(forcedRequest.ToolRounds) != 1 {
		t.Fatalf(
			"expected the forced final answer first, got choice %q with %d tool rounds",
			forcedRequest.ToolChoice,
			len(forcedRequest.ToolRounds),
		)
	}

	assertToolFreeFinalAnswerRequest(t, chatClient.requests[2], toolRequest)

	if strings.Contains(latestChatMessageText(toolRequest.Messages), testWebSearchResultText) {
		t.Fatal("expected the tool round's conversation to stay unchanged")
	}

	responseNode := instance.nodes.getOrCreate("response-message")
	responseNode.mu.Lock()
	responseText := responseNode.text
	responseNode.mu.Unlock()

	if !strings.Contains(responseText, testWebSearchToolAnswer) {
		t.Fatalf("expected the final answer in the response, got %q", responseText)
	}
}

func TestRespondToMessageSkipsIgnoredToolChoiceNoneOnLaterReplies(t *testing.T) {
	t.Parallel()

	chatClient := newToolChoiceIgnoringChatClient(false)
	instance := newSearchToolTestBot(t, chatClient, newSingleResultWebSearchClient())
	loadedConfig := newWebSearchToolTestConfig()

	respondWithWebSearchTool(t, instance, loadedConfig)

	firstReplyRounds := len(chatClient.requests)

	err := instance.respondToMessage(
		context.Background(),
		loadedConfig,
		newPromptMessage("user-message-2", testWebSearchChannelID, testWebSearchUserID, testWebSearchBotUserID),
		testWebSearchMainModel,
	)
	if err != nil {
		t.Fatalf("respond to the later message: %v", err)
	}

	// The model is known to ignore tool_choice "none", so the later reply
	// goes from its tool round straight to the final answer without tools.
	laterRequests := chatClient.requests[firstReplyRounds:]
	if len(laterRequests) != 2 {
		t.Fatalf("expected 2 generation rounds for the later reply, got %d", len(laterRequests))
	}

	if len(laterRequests[0].Tools) == 0 || laterRequests[0].ToolChoice != "" {
		t.Fatalf(
			"expected the later tool round to offer the tools, got %d tools with choice %q",
			len(laterRequests[0].Tools),
			laterRequests[0].ToolChoice,
		)
	}

	assertToolFreeFinalAnswerRequest(t, laterRequests[1], laterRequests[0])
}

func TestRespondToMessageSurfacesEmptyResponseWhenToolFreeFinalAnswerCallsTools(t *testing.T) {
	t.Parallel()

	chatClient := newToolChoiceIgnoringChatClient(true)
	webSearch := newSingleResultWebSearchClient()
	instance := newSearchToolTestBot(t, chatClient, webSearch)

	err := instance.respondToMessage(
		context.Background(),
		newWebSearchToolTestConfig(),
		newWebSearchToolSourceMessage(),
		testWebSearchMainModel,
	)
	if !errors.Is(err, errEmptyModelResponse) {
		t.Fatalf("expected the empty-response error when even the tool-free answer calls tools, got %v", err)
	}

	if len(chatClient.requests) != 3 || len(webSearch.calls) != 1 {
		t.Fatalf(
			"expected 3 generation rounds and 1 web search call, got %d rounds and %d searches",
			len(chatClient.requests),
			len(webSearch.calls),
		)
	}

	assertToolFreeFinalAnswerRequest(t, chatClient.requests[2], chatClient.requests[0])
}

func TestToolFreeFinalAnswerRequestSendsToolRoundOutputsAsText(t *testing.T) {
	t.Parallel()

	const (
		userQuery      = "which mouse has lower latency?"
		searchOutput   = "Query: mouse latency\nResults:\nThird-party latency table"
		failedOutput   = webSearchFailedOutput
		previousID     = "resp_parent"
		previousCount  = 2
		configuredName = "9router/muse-spark:vision"
	)

	var request chatCompletionRequest

	request.ConfiguredModel = configuredName
	request.Messages = []chatMessage{
		{Role: messageRoleSystem, Content: "system prompt"},
		{Role: messageRoleUser, Content: userQuery},
	}
	request.Tools = []providers.FunctionTool{providers.WebSearchTool(webSearchToolMaxQueries)}
	request.ToolChoice = providers.ToolChoiceNone
	request.ToolRounds = []providers.ToolRound{{
		Response: &providers.ToolCallResponse{Calls: parallelWebSearchToolCalls()},
		Outputs: []providers.FunctionToolOutput{
			{CallID: "call_1", Output: searchOutput},
			{CallID: "call_2", Output: " "},
			{CallID: "call_3", Output: failedOutput},
		},
	}}
	request.PreviousResponseID = previousID
	request.PreviousResponseCount = previousCount

	toolFreeRequest, err := toolFreeFinalAnswerRequest(request)
	if err != nil {
		t.Fatalf("build tool-free final answer request: %v", err)
	}

	if toolFreeRequest.Tools != nil || toolFreeRequest.ToolChoice != "" || toolFreeRequest.ToolRounds != nil {
		t.Fatalf("expected no tools, tool_choice, or tool rounds, got %#v", toolFreeRequest)
	}

	if toolFreeRequest.ConfiguredModel != configuredName ||
		toolFreeRequest.PreviousResponseID != previousID ||
		toolFreeRequest.PreviousResponseCount != previousCount {
		t.Fatalf("expected the model and response chaining to be kept, got %#v", toolFreeRequest)
	}

	finalText := latestChatMessageText(toolFreeRequest.Messages)
	for _, want := range []string{userQuery, webSearchSectionName + ":\n" + searchOutput + "\n\n" + failedOutput} {
		if !strings.Contains(finalText, want) {
			t.Fatalf("expected the latest user message to contain %q, got %q", want, finalText)
		}
	}

	if request.Messages[1].Content != userQuery || len(request.ToolRounds) != 1 {
		t.Fatal("expected the original request to stay unchanged")
	}
}

func TestRespondToMessageSurfacesEmptyResponseWhenForcedFinalAnswerIsEmpty(t *testing.T) {
	t.Parallel()

	var roundRequests []chatCompletionRequest

	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		roundRequests = append(roundRequests, request)

		if request.ToolChoice == providers.ToolChoiceNone {
			return handle(newStreamDelta("", finishReasonStop))
		}

		return handle(toolCallDelta(providers.FunctionToolCall{
			ID:        "call_loop_" + strconv.Itoa(len(roundRequests)),
			Name:      providers.WebSearchToolName,
			Arguments: `{"objective": "Find query results", "search_queries": ["` + testWebSearchQueryOne + `"]}`,
		}))
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

	err := instance.respondToMessage(
		context.Background(),
		newWebSearchToolTestConfig(),
		newWebSearchToolSourceMessage(),
		testWebSearchMainModel,
	)
	if !errors.Is(err, errEmptyModelResponse) {
		t.Fatalf("expected the empty-response error when the forced final answer is empty, got %v", err)
	}

	if len(roundRequests) != 2 {
		t.Fatalf("expected 2 generation rounds (one tool round + forced final answer), got %d", len(roundRequests))
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
			return handle(toolCallDelta(parallelWebSearchToolCalls()...))
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

	var requestCount int

	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		requestCount++

		if requestCount == 1 {
			return handle(toolCallDelta(providers.FunctionToolCall{
				ID:        "call_1",
				Name:      providers.WebSearchToolName,
				Arguments: `{"objective": "Find first query results", "search_queries": ["` + testWebSearchQueryOne + `"]}`,
			}))
		}

		// The forced final answer keeps the tool definitions with
		// tool_choice "none".
		if len(request.Tools) == 0 || request.ToolChoice != providers.ToolChoiceNone {
			t.Errorf("expected the follow-up to keep the tools with tool_choice none, got choice %q", request.ToolChoice)
		}

		for _, message := range request.Messages {
			if text, ok := message.Content.(string); ok &&
				strings.Contains(text, testWebSearchResultText) {
				followUpHasResults = true
			}
		}

		return handle(newStreamDelta(testWebSearchToolAnswer, finishReasonStop))
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
		t.Fatalf("expected a follow-up request after the failed search, got %d requests", len(chatClient.requests))
	}

	// The failed call is still answered, so the follow-up stays valid and
	// the model learns the search failed.
	followUpRounds := chatClient.requests[1].ToolRounds
	if len(followUpRounds) != 1 {
		t.Fatalf("expected the follow-up to replay 1 tool round, got %d", len(followUpRounds))
	}

	assertToolRoundOutputContains(t, followUpRounds[0], "call_1", webSearchFailedOutput)
}

func TestRunWebSearchToolPhaseAnswersEveryCall(t *testing.T) {
	t.Parallel()

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		if !slices.Equal(queries, []string{testWebSearchQueryOne}) {
			t.Errorf("expected only the valid call's query to be searched, got %#v", queries)
		}

		return []webSearchResult{{Query: testWebSearchQueryOne, Text: testWebSearchResultText}}, nil
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

	outputs, warnings, searched := instance.runWebSearchToolPhase(
		context.Background(),
		newWebSearchToolTestConfig(),
		testWebSearchMainModel,
		tracker,
		[]chatMessage{{Role: messageRoleUser, Content: "query"}},
		nil,
		[]providers.FunctionToolCall{
			{
				ID:        "call_search",
				Name:      providers.WebSearchToolName,
				Arguments: `{"search_queries": ["` + testWebSearchQueryOne + `"]}`,
			},
			{ID: "call_unknown", Name: "get_weather", Arguments: `{"location": "Paris"}`},
			{ID: "call_malformed", Name: providers.WebSearchToolName, Arguments: `not json`},
			{ID: "call_empty", Name: providers.WebSearchToolName, Arguments: `{"search_queries": ["  "]}`},
		},
	)

	if !searched || len(warnings) != 0 {
		t.Fatalf("expected a successful search without warnings, got searched=%v warnings=%#v", searched, warnings)
	}

	round := providers.ToolRound{Response: nil, Outputs: outputs}

	wantOutputs := []struct {
		callID string
		want   string
	}{
		{callID: "call_search", want: testWebSearchResultText},
		{callID: "call_unknown", want: `unknown function "get_weather"`},
		{callID: "call_malformed", want: webSearchInvalidArgumentsOutput},
		{callID: "call_empty", want: webSearchNoQueriesOutput},
	}

	if len(outputs) != len(wantOutputs) {
		t.Fatalf("expected one output per call, got %#v", outputs)
	}

	for index, wantOutput := range wantOutputs {
		if outputs[index].CallID != wantOutput.callID {
			t.Fatalf("expected output %d to answer %q, got %q", index, wantOutput.callID, outputs[index].CallID)
		}

		assertToolRoundOutputContains(t, round, wantOutput.callID, wantOutput.want)
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
				{
					ID:        "a",
					Name:      providers.WebSearchToolName,
					Arguments: `{"objective": "Find latest AI news", "search_queries": ["alpha", "beta"]}`,
				},
				{
					ID:        "b",
					Name:      providers.WebSearchToolName,
					Arguments: `{"objective": "Find latest AI news", "search_queries": ["beta", "gamma"]}`,
				},
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
		testWebSearchMainModel,
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

func TestRunWebSearchToolPhasePrefersProviderExaTypeOverRuntime(t *testing.T) {
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

	loadedConfig := newWebSearchToolTestConfig()
	provider := loadedConfig.Providers["openai"]
	provider.ExaSearchType = exaSearchTypeFast
	loadedConfig.Providers["openai"] = provider

	requestMessages := []chatMessage{
		{Role: messageRoleUser, Content: "<@bot-user> " + testWebSearchQueryOne},
	}
	tracker := newResponseTracker(newWebSearchToolSourceMessage(), testWebSearchMainModel)

	_, _, searched := instance.runWebSearchToolPhase(
		context.Background(),
		loadedConfig,
		testWebSearchMainModel,
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

	if seenSearchType != exaSearchTypeFast {
		t.Fatalf("expected the provider exa search type to win, got %q", seenSearchType)
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
		testWebSearchMainModel,
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
