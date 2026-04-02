package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	testAutoCompactMainModel                   = "openai/main-model"
	testAutoCompactMainPath                    = "main model"
	testAutoCompactSummaryText                 = "Condensed earlier context."
	testAutoCompactOlderSummaryText            = "Condensed older context."
	testUnexpectedAutoCompactStrategyFormat    = "unexpected auto compaction strategy: %q"
	testUnexpectedCompactedRequestLengthFormat = "unexpected compacted request length: %d"
	testUnexpectedSummaryContentTypeFormat     = "unexpected summary content type: %T"
	testExpectedSummarizedContentFormat        = "expected summarized content in compacted message: %q"
)

func TestAutoCompactRequestAddsSummaryAndPreservesRecentMessages(t *testing.T) {
	t.Parallel()

	client := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if len(request.Messages) != 2 {
			t.Fatalf("unexpected compaction request message count: %d", len(request.Messages))
		}

		systemPrompt, _ := request.Messages[0].Content.(string)
		if systemPrompt != autoCompactSummarySystemPrompt() &&
			systemPrompt != autoCompactMergeSystemPrompt() {
			t.Fatalf("unexpected compaction system prompt: %q", systemPrompt)
		}

		return handle(newStreamDelta(testAutoCompactSummaryText, ""))
	})

	instance := new(bot)
	instance.chatCompletions = client

	var originalRequest chatCompletionRequest

	originalRequest.ConfiguredModel = testAutoCompactMainModel
	originalRequest.ContextWindow = 200
	originalRequest.Messages = []chatMessage{
		{Role: openAICodexRoleSystem, Content: "Always be helpful."},
		{Role: messageRoleUser, Content: repeatedAutoCompactText("older details", 80)},
		{Role: messageRoleAssistant, Content: "Earlier answer."},
		{Role: messageRoleUser, Content: "Second question."},
		{Role: messageRoleAssistant, Content: "Second answer."},
		{Role: messageRoleUser, Content: "Latest question."},
	}

	compactedRequest, result := instance.autoCompactRequest(context.Background(), originalRequest)
	if !result.Applied {
		t.Fatal("expected auto compaction to apply")
	}

	if result.Strategy != autoCompactStrategySummary {
		t.Fatalf(testUnexpectedAutoCompactStrategyFormat, result.Strategy)
	}

	if len(client.requests) == 0 {
		t.Fatal("expected at least one compaction request")
	}

	if len(compactedRequest.Messages) != 6 {
		t.Fatalf(testUnexpectedCompactedRequestLengthFormat, len(compactedRequest.Messages))
	}

	if compactedRequest.Messages[0] != originalRequest.Messages[0] {
		t.Fatalf("expected leading system message to be preserved: %#v", compactedRequest.Messages[0])
	}

	assertAutoCompactSummaryContains(t, compactedRequest.Messages[1], testAutoCompactSummaryText)

	for index := 2; index < len(compactedRequest.Messages); index++ {
		if compactedRequest.Messages[index] != originalRequest.Messages[index] {
			t.Fatalf(
				"expected tail message %d to be preserved: got %#v want %#v",
				index,
				compactedRequest.Messages[index],
				originalRequest.Messages[index],
			)
		}
	}
}

func TestAutoCompactRequestUsesConfiguredThresholdPercent(t *testing.T) {
	t.Parallel()

	originalRequest := newConfiguredThresholdAutoCompactRequest()

	estimatedTokens := estimateChatCompletionRequestTokens(originalRequest)
	customLimit := autoCompactTokenLimit(
		originalRequest.ContextWindow,
		originalRequest.AutoCompactThresholdPercent,
	)
	defaultLimit := autoCompactTokenLimit(originalRequest.ContextWindow, 0)

	if estimatedTokens <= customLimit || estimatedTokens > defaultLimit {
		t.Fatalf(
			"unexpected test setup: estimated=%d custom_limit=%d default_limit=%d",
			estimatedTokens,
			customLimit,
			defaultLimit,
		)
	}

	noCompactInstance := new(bot)
	noCompactInstance.chatCompletions = newUnexpectedCompactionClient(t)

	defaultThresholdRequest := originalRequest
	defaultThresholdRequest.AutoCompactThresholdPercent = 0

	uncompactedRequest, defaultResult := noCompactInstance.autoCompactRequest(
		context.Background(),
		defaultThresholdRequest,
	)
	if defaultResult.Applied {
		t.Fatal("did not expect auto compaction to apply with the default threshold")
	}

	if !chatMessagesEqual(uncompactedRequest.Messages, defaultThresholdRequest.Messages) {
		t.Fatalf("unexpected request mutation without compaction: %#v", uncompactedRequest.Messages)
	}

	instance := new(bot)
	instance.chatCompletions = newThresholdCompactionClient(t)

	compactedRequest, result := instance.autoCompactRequest(context.Background(), originalRequest)
	if !result.Applied {
		t.Fatal("expected auto compaction to apply with the configured threshold")
	}

	if result.Strategy != autoCompactStrategySummary {
		t.Fatalf(testUnexpectedAutoCompactStrategyFormat, result.Strategy)
	}

	if len(compactedRequest.Messages) != 3 {
		t.Fatalf(testUnexpectedCompactedRequestLengthFormat, len(compactedRequest.Messages))
	}

	assertAutoCompactSummaryContains(t, compactedRequest.Messages[0], testAutoCompactSummaryText)
}

func TestAutoCompactRequestTruncatesLatestOversizedMessage(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                       "",
		ConfiguredModel:             testAutoCompactMainModel,
		ContextWindow:               1_000,
		AutoCompactThresholdPercent: 90,
		SessionID:                   "",
		PreviousResponseID:          "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: autoCompactSizedASCIIText(810)},
		},
	}

	singleMessageLimit := autoCompactSingleMessageTokenLimit(
		request.ContextWindow,
		request.AutoCompactThresholdPercent,
	)
	if estimateChatMessageTokens(request.Messages[0]) <= singleMessageLimit {
		t.Fatalf("unexpected test setup: latest message already fits %d", singleMessageLimit)
	}

	instance := new(bot)
	instance.chatCompletions = newUnexpectedCompactionClient(t)

	compactedRequest, result := instance.autoCompactRequest(context.Background(), request)
	if !result.Applied {
		t.Fatal("expected oversized latest message truncation to apply")
	}

	if !result.TruncatedMessage {
		t.Fatal("expected latest message truncation to be recorded")
	}

	if result.Strategy != "" {
		t.Fatalf(testUnexpectedAutoCompactStrategyFormat, result.Strategy)
	}

	if len(compactedRequest.Messages) != 1 {
		t.Fatalf("unexpected truncated request length: %d", len(compactedRequest.Messages))
	}

	originalText, _ := request.Messages[0].Content.(string)

	truncatedText, ok := compactedRequest.Messages[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected truncated content type: %T", compactedRequest.Messages[0].Content)
	}

	if truncatedText == originalText {
		t.Fatal("expected latest message text to be truncated")
	}

	if estimateChatMessageTokens(compactedRequest.Messages[0]) > singleMessageLimit {
		t.Fatalf(
			"expected latest message to fit the single-message limit: %d > %d",
			estimateChatMessageTokens(compactedRequest.Messages[0]),
			singleMessageLimit,
		)
	}

	expectedWarnings := []string{
		autoCompactWarningMessage(
			testAutoCompactMainPath,
			"truncated an oversized message to fit the model context window.",
		),
	}
	if !slices.Equal(result.warningsForPath(testAutoCompactMainPath), expectedWarnings) {
		t.Fatalf("unexpected truncation warnings: %#v", result.warningsForPath(testAutoCompactMainPath))
	}
}

func TestAutoCompactRequestTruncatesLatestOversizedMessageBeforeSummarizingHistory(t *testing.T) {
	t.Parallel()

	client := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if len(request.Messages) != 2 {
			t.Fatalf("unexpected compaction request length: %d", len(request.Messages))
		}

		return handle(newStreamDelta(testAutoCompactOlderSummaryText, ""))
	})

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                       "",
		ConfiguredModel:             testAutoCompactMainModel,
		ContextWindow:               1_000,
		AutoCompactThresholdPercent: 90,
		SessionID:                   "",
		PreviousResponseID:          "",
		Messages: []chatMessage{
			{Role: openAICodexRoleSystem, Content: "Always be helpful."},
			{Role: messageRoleUser, Content: autoCompactSizedASCIIText(250)},
			{Role: messageRoleAssistant, Content: autoCompactSizedASCIIText(40)},
			{Role: messageRoleUser, Content: autoCompactSizedASCIIText(810)},
		},
	}

	instance := new(bot)
	instance.chatCompletions = client

	compactedRequest, result := instance.autoCompactRequest(context.Background(), request)
	if !result.Applied {
		t.Fatal("expected truncation and compaction to apply")
	}

	if !result.TruncatedMessage {
		t.Fatal("expected latest message truncation to be recorded")
	}

	if result.Strategy != autoCompactStrategySummary {
		t.Fatalf(testUnexpectedAutoCompactStrategyFormat, result.Strategy)
	}

	expectedWarnings := []string{
		autoCompactWarningMessage(
			testAutoCompactMainPath,
			"truncated an oversized message to fit the model context window.",
		),
		autoCompactWarningMessage(
			testAutoCompactMainPath,
			"auto-compacted older conversation context to fit the model context window.",
		),
	}
	if !slices.Equal(result.warningsForPath(testAutoCompactMainPath), expectedWarnings) {
		t.Fatalf("unexpected compaction warnings: %#v", result.warningsForPath(testAutoCompactMainPath))
	}

	if len(client.requests) == 0 {
		t.Fatal("expected a compaction request after truncating the latest message")
	}

	if len(compactedRequest.Messages) != 4 {
		t.Fatalf(testUnexpectedCompactedRequestLengthFormat, len(compactedRequest.Messages))
	}

	assertAutoCompactSummaryContains(t, compactedRequest.Messages[1], testAutoCompactOlderSummaryText)

	latestMessage := compactedRequest.Messages[len(compactedRequest.Messages)-1]
	if estimateChatMessageTokens(latestMessage) > autoCompactSingleMessageTokenLimit(
		request.ContextWindow,
		request.AutoCompactThresholdPercent,
	) {
		t.Fatalf("expected latest message to fit the single-message limit: %#v", latestMessage)
	}
}

func newConfiguredThresholdAutoCompactRequest() chatCompletionRequest {
	var request chatCompletionRequest

	request.ConfiguredModel = testAutoCompactMainModel
	request.ContextWindow = 200
	request.AutoCompactThresholdPercent = 50
	request.Messages = []chatMessage{
		{Role: messageRoleUser, Content: repeatedAutoCompactText("older details", 32)},
		{Role: messageRoleAssistant, Content: "Earlier answer."},
		{Role: messageRoleUser, Content: "Latest question."},
	}

	return request
}

func newUnexpectedCompactionClient(t *testing.T) *stubChatCompletionClient {
	t.Helper()

	return newStubChatClient(func(
		context.Context,
		chatCompletionRequest,
		func(streamDelta) error,
	) error {
		t.Fatal("did not expect compaction request when using the default threshold")

		return nil
	})
}

func newThresholdCompactionClient(t *testing.T) *stubChatCompletionClient {
	t.Helper()

	return newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if len(request.Messages) != 2 {
			t.Fatalf("unexpected compaction request length: %d", len(request.Messages))
		}

		return handle(newStreamDelta(testAutoCompactSummaryText, ""))
	})
}

func assertAutoCompactSummaryContains(t *testing.T, message chatMessage, want string) {
	t.Helper()

	summaryText, ok := message.Content.(string)
	if !ok {
		t.Fatalf(testUnexpectedSummaryContentTypeFormat, message.Content)
	}

	if !strings.Contains(summaryText, autoCompactSummaryPrefix) {
		t.Fatalf("expected summary prefix in compacted message: %q", summaryText)
	}

	if !strings.Contains(summaryText, want) {
		t.Fatalf(testExpectedSummarizedContentFormat, summaryText)
	}
}

func TestTruncateContentPartsToApproxTokensPreservesNonTextParts(t *testing.T) {
	t.Parallel()

	parts := []contentPart{
		{
			"type":      contentTypeImageURL,
			"image_url": map[string]string{"url": "data:image/png;base64,abc"},
		},
		{
			"type": contentTypeText,
			"text": autoCompactSizedASCIIText(40),
		},
	}

	truncatedParts, truncated := truncateContentPartsToApproxTokens(parts, autoCompactImageTokens+4)
	if !truncated {
		t.Fatal("expected text content to be truncated while preserving the image part")
	}

	if len(truncatedParts) != len(parts) {
		t.Fatalf("unexpected truncated content part count: %d", len(truncatedParts))
	}

	if truncatedParts[0]["type"] != contentTypeImageURL {
		t.Fatalf("expected leading image part to be preserved: %#v", truncatedParts[0])
	}

	truncatedText, ok := truncatedParts[1]["text"].(string)
	if !ok {
		t.Fatalf("unexpected truncated text content type: %T", truncatedParts[1]["text"])
	}

	originalText, _ := parts[1]["text"].(string)
	if truncatedText == originalText {
		t.Fatal("expected text content to be shortened")
	}
}

func TestDecideWebSearchAutoCompactsSearchDeciderRequest(t *testing.T) {
	t.Parallel()

	client := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if len(request.Messages) >= 1 {
			systemPrompt, _ := request.Messages[0].Content.(string)
			if systemPrompt == autoCompactSummarySystemPrompt() ||
				systemPrompt == autoCompactMergeSystemPrompt() {
				return handle(newStreamDelta("Earlier thread summary.", ""))
			}
		}

		if request.ConfiguredModel != testSearchDeciderModel {
			t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
		}

		renderedConversation := renderMessagesForAutoCompaction(request.Messages)
		if !containsFold(strings.Join(renderedConversation, "\n\n"), autoCompactSummaryPrefix) {
			t.Fatalf("expected compacted summary in search decider request: %#v", request.Messages)
		}

		return handle(newStreamDelta(`{"needs_search":false,"queries":[]}`, ""))
	})

	instance := newSearchTestBot(client, newStubWebSearchClient(func(
		context.Context,
		config,
		[]string,
	) ([]webSearchResult, error) {
		return nil, nil
	}))
	loadedConfig := testSearchConfig()
	loadedConfig.ModelContextWindows = map[string]int{
		testSearchDeciderModel: 1700,
	}

	conversation := []chatMessage{
		{Role: messageRoleUser, Content: repeatedAutoCompactText("very old context", 100)},
		{Role: messageRoleAssistant, Content: "Assistant reply about the older context."},
		{Role: messageRoleUser, Content: "Should we search for the latest version?"},
	}

	decision, warnings, err := instance.decideWebSearch(
		context.Background(),
		loadedConfig,
		"openai/main-model",
		nil,
		conversation,
	)
	if err != nil {
		t.Fatalf("decide web search: %v", err)
	}

	if decision.NeedsSearch {
		t.Fatal("expected decider to skip web search")
	}

	expectedWarning := autoCompactResult{
		Applied:          true,
		Strategy:         autoCompactStrategySummary,
		TruncatedMessage: false,
	}.warningsForPath("search decider")
	if !slices.Equal(warnings, expectedWarning) {
		t.Fatalf("unexpected compaction warnings: %#v", warnings)
	}

	if len(client.requests) < 2 {
		t.Fatalf("expected compaction and final search decider requests: %d", len(client.requests))
	}
}

type prepareMessageResponseAutoCompactFixture struct {
	instance      *bot
	loadedConfig  config
	sourceMessage *discordgo.Message
}

func TestPrepareMessageResponseAutoCompactsMainRequest(t *testing.T) {
	t.Parallel()

	fixture := newPrepareMessageResponseAutoCompactFixture(t)

	progress := fixture.instance.startRequestProgress(
		context.Background(),
		fixture.sourceMessage,
		testAutoCompactMainModel,
	)

	request, tracker, warnings, err := fixture.instance.prepareMessageResponse(
		context.Background(),
		fixture.loadedConfig,
		fixture.sourceMessage,
		testAutoCompactMainModel,
		progress,
	)

	if tracker != nil {
		tracker.release(fixture.instance.nodes, "test response", "")
	}

	if err != nil {
		t.Fatalf("prepare message response: %v", err)
	}

	expectedWarning := autoCompactResult{
		Applied:          true,
		Strategy:         autoCompactStrategySummary,
		TruncatedMessage: false,
	}.warningsForPath(testAutoCompactMainPath)
	if len(warnings) < len(expectedWarning) ||
		!slices.Equal(warnings[len(warnings)-len(expectedWarning):], expectedWarning) {
		t.Fatalf("expected auto compaction warning in response warnings: %#v", warnings)
	}

	if len(request.Messages) < 3 {
		t.Fatalf("unexpected compacted main request length: %d", len(request.Messages))
	}

	if request.Messages[0].Role != openAICodexRoleSystem {
		t.Fatalf("expected system prompt to stay first: %#v", request.Messages[0])
	}

	summaryText, ok := request.Messages[1].Content.(string)
	if !ok {
		t.Fatalf("unexpected main summary content type: %T", request.Messages[1].Content)
	}

	if !strings.Contains(summaryText, autoCompactSummaryPrefix) {
		t.Fatalf("expected main request summary prefix: %q", summaryText)
	}

	if !strings.Contains(summaryText, "Main request summary.") {
		t.Fatalf("expected main request summary text: %q", summaryText)
	}
}

func newPrepareMessageResponseAutoCompactFixture(
	t *testing.T,
) prepareMessageResponseAutoCompactFixture {
	t.Helper()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		progressMessageID  = "progress-message"
		oldestMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		followUpMessageID  = "user-message-2"
	)

	session := newPrepareMessageResponseAutoCompactSession(
		t,
		channelID,
		botUserID,
		progressMessageID,
	)
	client := newPrepareMessageResponseAutoCompactClient(t)

	instance := new(bot)
	instance.session = session
	instance.chatCompletions = client
	instance.nodes = newMessageNodeStore(10)

	oldestMessage := new(discordgo.Message)
	oldestMessage.ID = oldestMessageID
	oldestMessage.ChannelID = channelID
	oldestMessage.Author = newDiscordUser(userID, false)
	oldestMessage.Content = "at ai " + repeatedAutoCompactText("older context", 100)

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantMessageID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.Type = discordgo.MessageTypeReply
	assistantMessage.Content = repeatedAutoCompactText("assistant answer", 40)
	assistantMessage.MessageReference = oldestMessage.Reference()
	assistantMessage.ReferencedMessage = oldestMessage

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = followUpMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "<@" + botUserID + "> " + repeatedAutoCompactText("follow up question", 60)
	sourceMessage.Mentions = []*discordgo.User{newDiscordUser(botUserID, false)}
	sourceMessage.MessageReference = assistantMessage.Reference()
	sourceMessage.ReferencedMessage = assistantMessage

	loadedConfig := testSearchConfig()
	loadedConfig.MaxText = defaultMaxText
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages
	loadedConfig.SystemPrompt = "Always help the user."
	loadedConfig.ModelContextWindows = map[string]int{
		testAutoCompactMainModel: 800,
		testSearchDeciderModel:   100000,
	}

	return prepareMessageResponseAutoCompactFixture{
		instance:      instance,
		loadedConfig:  loadedConfig,
		sourceMessage: sourceMessage,
	}
}

func newPrepareMessageResponseAutoCompactSession(
	t *testing.T,
	channelID string,
	botUserID string,
	progressMessageID string,
) *discordgo.Session {
	t.Helper()

	progressMessage := new(discordgo.Message)
	progressMessage.ID = progressMessageID
	progressMessage.ChannelID = channelID
	progressMessage.Author = newDiscordUser(botUserID, true)

	return newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages":
			return newJSONResponse(t, request, progressMessage), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/"+progressMessageID:
			return newJSONResponse(t, request, progressMessage), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}))
}

func newPrepareMessageResponseAutoCompactClient(
	t *testing.T,
) *stubChatCompletionClient {
	t.Helper()

	return newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if request.ConfiguredModel == testSearchDeciderModel {
			return handle(newStreamDelta(`{"needs_search":false,"queries":[]}`, ""))
		}

		if len(request.Messages) >= 1 {
			systemPrompt, _ := request.Messages[0].Content.(string)
			if systemPrompt == autoCompactSummarySystemPrompt() ||
				systemPrompt == autoCompactMergeSystemPrompt() {
				return handle(newStreamDelta("Main request summary.", ""))
			}
		}

		t.Fatalf("unexpected request during prepareMessageResponse: %#v", request.Messages)

		return nil
	})
}

func repeatedAutoCompactText(fragment string, repeats int) string {
	return strings.TrimSpace(strings.Repeat(fragment+" ", repeats))
}

func autoCompactSizedASCIIText(tokens int) string {
	return strings.Repeat("a", tokens*autoCompactCharsPerToken)
}
