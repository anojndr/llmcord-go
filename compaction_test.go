package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
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

func TestAutoCompactRequestAddsSummaryLastAndKeepsNewestUserMessages(t *testing.T) {
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
	originalRequest.AutoCompactTokenLimit = 0
	originalRequest.Messages = []chatMessage{
		{Role: messageRoleSystem, Content: "Always be helpful."},
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

	// System prompt first, then the newest user messages, then the summary
	// LAST (codex: replacement history ends with the summary message).
	if compactedRequest.Messages[0] != originalRequest.Messages[0] {
		t.Fatalf("expected leading system message to be preserved: %#v", compactedRequest.Messages[0])
	}

	lastIndex := len(compactedRequest.Messages) - 1
	assertAutoCompactSummaryContains(t, compactedRequest.Messages[lastIndex], testAutoCompactSummaryText)

	// "Latest question." is the newest user message and must be retained before
	// the summary; assistant messages are dropped like codex does.
	foundLatest := false
	hasAssistant := false

	for _, message := range compactedRequest.Messages {
		if chatMessageText(message) == "Latest question." {
			foundLatest = true
		}

		if message.Role == messageRoleAssistant {
			hasAssistant = true
		}
	}

	if !foundLatest {
		t.Fatalf("expected latest user message to be retained: %#v", compactedRequest.Messages)
	}

	if hasAssistant {
		t.Fatalf("expected assistant messages to be dropped: %#v", compactedRequest.Messages)
	}
}

func TestAutoCompactRequestUsesConfiguredTokenLimit(t *testing.T) {
	t.Parallel()

	originalRequest := newConfiguredTokenLimitAutoCompactRequest()

	estimatedTokens := estimateChatCompletionRequestTokens(originalRequest)
	customLimit := autoCompactTokenLimit(
		originalRequest.ContextWindow,
		originalRequest.AutoCompactTokenLimit,
	)

	if estimatedTokens <= customLimit {
		t.Fatalf(
			"unexpected test setup: estimated=%d custom_limit=%d",
			estimatedTokens,
			customLimit,
		)
	}

	noCompactInstance := new(bot)
	noCompactInstance.chatCompletions = newUnexpectedCompactionClient(t)

	defaultLimitRequest := originalRequest
	defaultLimitRequest.AutoCompactTokenLimit = 0

	uncompactedRequest, defaultResult := noCompactInstance.autoCompactRequest(
		context.Background(),
		defaultLimitRequest,
	)
	if defaultResult.Applied {
		t.Fatal("did not expect auto compaction to apply with the derived limit")
	}

	if !chatMessagesEqual(uncompactedRequest.Messages, defaultLimitRequest.Messages) {
		t.Fatalf("unexpected request mutation without compaction: %#v", uncompactedRequest.Messages)
	}

	instance := new(bot)
	instance.chatCompletions = newThresholdCompactionClient(t)

	compactedRequest, result := instance.autoCompactRequest(context.Background(), originalRequest)
	if !result.Applied {
		t.Fatal("expected auto compaction to apply with the configured limit")
	}

	if result.Strategy != autoCompactStrategySummary {
		t.Fatalf(testUnexpectedAutoCompactStrategyFormat, result.Strategy)
	}

	lastIndex := len(compactedRequest.Messages) - 1
	assertAutoCompactSummaryContains(t, compactedRequest.Messages[lastIndex], testAutoCompactSummaryText)
}

func TestAutoCompactRequestPreservesNewestUserMessageUnderConfiguredLimit(t *testing.T) {
	t.Parallel()

	originalRequest := newConfiguredTokenLimitAutoCompactRequest()
	customLimit := autoCompactTokenLimit(originalRequest.ContextWindow, originalRequest.AutoCompactTokenLimit)

	estimatedTokens := estimateChatCompletionRequestTokens(originalRequest)
	if estimatedTokens <= customLimit {
		t.Fatalf("unexpected test setup: estimated=%d custom_limit=%d", estimatedTokens, customLimit)
	}

	instance := new(bot)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		return handle(newStreamDelta(testAutoCompactSummaryText, ""))
	})

	compactedRequest, result := instance.autoCompactRequest(context.Background(), originalRequest)
	if !result.Applied {
		t.Fatal("expected auto compaction to apply with the configured limit")
	}

	if result.Strategy != autoCompactStrategySummary {
		t.Fatalf(testUnexpectedAutoCompactStrategyFormat, result.Strategy)
	}

	lastIndex := len(compactedRequest.Messages) - 1
	assertAutoCompactSummaryContains(t, compactedRequest.Messages[lastIndex], testAutoCompactSummaryText)

	latestText := chatMessageText(compactedRequest.Messages[0])
	if !strings.Contains(latestText, "older details") && !strings.Contains(latestText, "Latest question") {
		t.Fatalf("expected newest user messages retained: %#v", compactedRequest.Messages)
	}
}

func TestAutoCompactTokenLimitFollowsCodexDerivation(t *testing.T) {
	t.Parallel()

	const contextWindow = 1_000

	// Codex derives the auto-compact limit as (window * 9) / 10 when no
	// explicit config limit is set.
	derivedLimit := autoCompactDerivedTokenLimit(contextWindow)
	if derivedLimit != 900 {
		t.Fatalf(
			"expected derived limit to equal Codex's (window*9)/10: got %d want 900",
			derivedLimit,
		)
	}

	// A configured limit is clamped at the derived 90% value, mirroring
	// ModelInfo::auto_compact_token_limit's min(config, context*9/10).
	if limit := autoCompactTokenLimit(contextWindow, 1000); limit != 900 {
		t.Fatalf("expected configured limit above 90%% to clamp: got %d want 900", limit)
	}

	if limit := autoCompactTokenLimit(contextWindow, 700); limit != 700 {
		t.Fatalf("expected configured limit below 90%% to be honored: got %d want 700", limit)
	}

	if limit := autoCompactTokenLimit(contextWindow, 0); limit != 900 {
		t.Fatalf("expected zero configured limit to derive 900: got %d", limit)
	}

	// No window configured → no scoped trigger at all.
	if limit := autoCompactTokenLimit(0, 0); limit != 0 {
		t.Fatalf("expected zero window to yield zero limit: got %d", limit)
	}
}

func TestAutoCompactFullContextWindowLimitFollowsCodexEffectivePercent(t *testing.T) {
	t.Parallel()

	const contextWindow = 1_000

	// Codex treats 95% of the context window as the usable hard cap.
	if limit := autoCompactFullWindowTokenLimit(contextWindow); limit != 950 {
		t.Fatalf("expected full-window limit to equal Codex's 95%% cap: got %d want 950", limit)
	}

	if limit := autoCompactFullWindowTokenLimit(0); limit != 0 {
		t.Fatalf("expected zero window to yield zero full-window limit: got %d", limit)
	}
}

func TestAutoCompactSingleMessageLimitStaysBelowDerivedLimit(t *testing.T) {
	t.Parallel()

	const contextWindow = 1_000

	// The latest-message truncation budget stays 10 percentage points below the
	// derived 90% limit: 80% of the window.
	singleMessageLimit := autoCompactSingleMessageTokenLimit(contextWindow)

	expectedLimit := (contextWindow * (autoCompactDerivePercent - autoCompactSingleMessageMargin)) /
		autoCompactPercentBase
	if singleMessageLimit != expectedLimit {
		t.Fatalf(
			"unexpected single-message limit: got %d want %d",
			singleMessageLimit,
			expectedLimit,
		)
	}
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
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                      "",
		ConfiguredModel:            testAutoCompactMainModel,
		ContextWindow:              1_000,
		AutoCompactTokenLimit:      0,
		AutoCompactTokenLimitScope: autoCompactTokenLimitScopeTotal,
		CompactPrompt:              "",
		SessionID:                  "",
		PreviousResponseID:         "",
		RequestID:                  "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: autoCompactSizedASCIIText(810)},
		},
	}

	singleMessageLimit := autoCompactSingleMessageTokenLimit(request.ContextWindow)
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

func TestAutoCompactRequestTruncatesCSVLikeLatestMessageConservatively(t *testing.T) {
	t.Parallel()

	csvText := repeatedAutoCompactCSVRows(600)
	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                      "",
		ConfiguredModel:            testAutoCompactMainModel,
		ContextWindow:              1_000,
		AutoCompactTokenLimit:      0,
		AutoCompactTokenLimitScope: autoCompactTokenLimitScopeTotal,
		CompactPrompt:              "",
		SessionID:                  "",
		PreviousResponseID:         "",
		RequestID:                  "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: csvText},
		},
	}

	singleMessageLimit := autoCompactSingleMessageTokenLimit(request.ContextWindow)
	if estimateChatMessageTokens(request.Messages[0]) <= singleMessageLimit {
		t.Fatalf("unexpected test setup: csv message already fits %d", singleMessageLimit)
	}

	instance := new(bot)
	instance.chatCompletions = newUnexpectedCompactionClient(t)

	compactedRequest, result := instance.autoCompactRequest(context.Background(), request)
	if !result.TruncatedMessage {
		t.Fatal("expected csv-like latest message truncation")
	}

	truncatedText, ok := compactedRequest.Messages[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected truncated content type: %T", compactedRequest.Messages[0].Content)
	}

	if estimateChatMessageTokens(compactedRequest.Messages[0]) > singleMessageLimit {
		t.Fatalf(
			"expected csv-like latest message to fit the single-message limit: %d > %d",
			estimateChatMessageTokens(compactedRequest.Messages[0]),
			singleMessageLimit,
		)
	}

	if runeCount(truncatedText) >= singleMessageLimit*autoCompactCharsPerToken {
		t.Fatalf("expected csv-like text to truncate below the prose character budget: %d", runeCount(truncatedText))
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
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                      "",
		ConfiguredModel:            testAutoCompactMainModel,
		ContextWindow:              1_000,
		AutoCompactTokenLimit:      0,
		AutoCompactTokenLimitScope: autoCompactTokenLimitScopeTotal,
		CompactPrompt:              "",
		SessionID:                  "",
		PreviousResponseID:         "",
		RequestID:                  "",
		Messages: []chatMessage{
			{Role: messageRoleSystem, Content: "Always be helpful."},
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

	assertAutoCompactSummaryContains(t, compactedRequest.Messages[3], testAutoCompactOlderSummaryText)

	// The newest user message is retained before the summary and must have been
	// truncated to the single-message budget.
	latestMessage := compactedRequest.Messages[len(compactedRequest.Messages)-2]
	if estimateChatMessageTokens(latestMessage) > autoCompactSingleMessageTokenLimit(request.ContextWindow) {
		t.Fatalf("expected latest message to fit the single-message limit: %#v", latestMessage)
	}
}

func TestEstimateTextTokensMatchesCodexByteRatio(t *testing.T) {
	t.Parallel()

	csvText := repeatedAutoCompactCSVRows(20)

	// Codex estimates ceil(bytes/4) for all text, including punctuation-heavy.
	naiveCSVTokens := approxTokensFromBytes(len(strings.TrimSpace(csvText)))
	if estimateTextTokens(csvText) != naiveCSVTokens {
		t.Fatalf(
			"expected codex byte ratio for csv-like text: got %d want %d",
			estimateTextTokens(csvText),
			naiveCSVTokens,
		)
	}

	proseText := repeatedAutoCompactText("average frame pacing stayed steady during the capture", 20)

	naiveProseTokens := approxTokensFromBytes(len(strings.TrimSpace(proseText)))
	if estimateTextTokens(proseText) != naiveProseTokens {
		t.Fatalf(
			"expected codex byte ratio for prose: got %d want %d",
			estimateTextTokens(proseText),
			naiveProseTokens,
		)
	}
}

func TestSplitTextToApproxTokenChunksKeepsCSVLikeChunksWithinBudget(t *testing.T) {
	t.Parallel()

	const chunkTokenLimit = autoCompactMinChunkTokens

	chunks := splitTextToApproxTokenChunks(repeatedAutoCompactCSVRows(50), chunkTokenLimit)
	if len(chunks) < 2 {
		t.Fatalf("expected csv-like text to split into multiple chunks: %d", len(chunks))
	}

	for index, chunk := range chunks {
		if estimateTextTokens(chunk) > chunkTokenLimit {
			t.Fatalf(
				"expected chunk %d to fit token budget: %d > %d",
				index,
				estimateTextTokens(chunk),
				chunkTokenLimit,
			)
		}
	}
}

func newConfiguredTokenLimitAutoCompactRequest() chatCompletionRequest {
	var request chatCompletionRequest

	request.ConfiguredModel = testAutoCompactMainModel
	request.ContextWindow = 2_000
	request.AutoCompactTokenLimit = 100
	request.Messages = []chatMessage{
		{Role: messageRoleUser, Content: repeatedAutoCompactText("older details", 30)},
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

func TestDecideAutoCompactsSearchDeciderRequest(t *testing.T) {
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

	instance := newSearchTestBot(client, newNoOpWebSearchClient())
	loadedConfig := testSearchConfig()
	loadedConfig.ModelContextWindows = map[string]int{
		testSearchDeciderModel: 2800,
	}

	sourceMessage := newDeciderTestConversationChain(
		instance,
		repeatedAutoCompactText("very old context", 400),
		"Assistant reply about the older context.",
		"Should we search for the latest version?",
	)

	decision, warnings, err := instance.decideWebSearch(
		context.Background(),
		loadedConfig,
		"openai/main-model",
		sourceMessage,
		nil,
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

func newNoOpWebSearchClient() *stubWebSearchClient {
	return newStubWebSearchClient(func(
		context.Context,
		config,
		[]string,
	) ([]webSearchResult, error) {
		return nil, nil
	})
}

// newDeciderTestConversationChain seeds a reply chain (oldest user -> assistant
// -> latest user) in the instance's node store and returns the latest source
// message, which is what the search decider rebuilds its conversation from.
func newDeciderTestConversationChain(
	instance *bot,
	oldestText string,
	assistantText string,
	latestText string,
) *discordgo.Message {
	oldestMessage := new(discordgo.Message)
	oldestMessage.ID = "oldest-user-message"
	oldestMessage.ChannelID = "channel-1"
	oldestMessage.Author = newDiscordUser("user-1", false)
	oldestMessage.Content = oldestText

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = "decider-assistant-message"
	assistantMessage.ChannelID = "channel-1"
	assistantMessage.Author = newDiscordUser("bot-user", true)
	assistantMessage.Type = discordgo.MessageTypeReply
	assistantMessage.Content = assistantText
	assistantMessage.MessageReference = oldestMessage.Reference()
	assistantMessage.ReferencedMessage = oldestMessage

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "search-decider-latest-user-message"
	sourceMessage.ChannelID = "channel-1"
	sourceMessage.Author = newDiscordUser("user-1", false)
	sourceMessage.Content = latestText
	sourceMessage.MessageReference = assistantMessage.Reference()
	sourceMessage.ReferencedMessage = assistantMessage

	setCachedUserNode(instance, oldestMessage, nil, oldestMessage.Content)
	setCachedAssistantNode(instance, assistantMessage, oldestMessage)
	setCachedUserNode(instance, sourceMessage, assistantMessage, sourceMessage.Content)

	return sourceMessage
}

// TestDecideWebSearchAutoCompactsNearWindowConversation asserts that a
// near-window conversation is auto-compacted for the search decider exactly
// like the main model: the decider pipeline runs the same conversation build
// and augmentation, then auto-compacts against the decider model's own
// context window before streaming the decision request.
func TestDecideWebSearchAutoCompactsNearWindowConversation(t *testing.T) {
	t.Parallel()

	const (
		contextWindow  = 200_000
		historyTokens  = 139_000
		newInputTokens = 100_000
	)

	client, counter := newAutoCompactingSearchDeciderClient(t)

	instance := newSearchTestBot(client, newNoOpWebSearchClient())

	loadedConfig := testSearchConfig()
	loadedConfig.ModelContextWindows = map[string]int{
		testAutoCompactMainModel: contextWindow,
		testSearchDeciderModel:   contextWindow,
	}

	sourceMessage := newDeciderTestConversationChain(
		instance,
		repeatedAutoCompactText("older context", historyTokens),
		"Earlier answer.",
		repeatedAutoCompactText("new input", newInputTokens),
	)

	decision, warnings, err := instance.decideWebSearch(
		context.Background(),
		loadedConfig,
		testAutoCompactMainModel,
		sourceMessage,
		nil,
	)
	if err != nil {
		t.Fatalf("decide web search: %v", err)
	}

	if decision.NeedsSearch {
		t.Fatal("expected decider to skip web search")
	}

	if deciderCalls := counter.deciderCalls.Load(); deciderCalls == 0 {
		t.Fatal("expected at least one search decider request")
	}

	if len(warnings) == 0 {
		t.Fatal("expected auto-compaction warnings for the search decider")
	}
}

type autoCompactingSearchDeciderCounter struct {
	compactionCalls atomic.Int64
	deciderCalls    atomic.Int64
}

func newAutoCompactingSearchDeciderClient(
	t *testing.T,
) (*stubChatCompletionClient, *autoCompactingSearchDeciderCounter) {
	t.Helper()

	counter := new(autoCompactingSearchDeciderCounter)

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
				counter.compactionCalls.Add(1)

				return handle(newStreamDelta("Condensed history.", ""))
			}
		}

		if request.ConfiguredModel != testSearchDeciderModel {
			t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
		}

		counter.deciderCalls.Add(1)

		return handle(newStreamDelta(`{"needs_search":false,"queries":[]}`, ""))
	})

	return client, counter
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

	if request.Messages[0].Role != messageRoleSystem {
		t.Fatalf("expected system prompt to stay first: %#v", request.Messages[0])
	}

	summaryText, ok := request.Messages[len(request.Messages)-1].Content.(string)
	if !ok {
		t.Fatalf("unexpected main summary content type: %T", request.Messages[len(request.Messages)-1].Content)
	}

	if !strings.Contains(summaryText, autoCompactSummaryPrefix) {
		t.Fatalf("expected main request summary prefix in last message: %q", summaryText)
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

func TestAutoCompactRequestReliesOnContextWindowForLimit(t *testing.T) {
	t.Parallel()

	originalRequest := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                      "",
		ConfiguredModel:            testAutoCompactMainModel,
		ContextWindow:              1_000,
		AutoCompactTokenLimit:      0,
		AutoCompactTokenLimitScope: autoCompactTokenLimitScopeTotal,
		CompactPrompt:              "",
		SessionID:                  "",
		PreviousResponseID:         "",
		RequestID:                  "",
		Messages: []chatMessage{
			{Role: messageRoleSystem, Content: "Always be helpful."},
			{Role: messageRoleUser, Content: repeatedAutoCompactText("very old details", 120)},
			{Role: messageRoleAssistant, Content: "Earlier answer."},
			{Role: messageRoleUser, Content: "Latest question."},
		},
	}

	tokenLimit := autoCompactTokenLimit(originalRequest.ContextWindow, 0)
	if tokenLimit != 900 {
		t.Fatalf("unexpected default limit for context window 1000: %d", tokenLimit)
	}

	instance := new(bot)
	instance.chatCompletions = newUnexpectedCompactionClient(t)

	uncompactedRequest, result := instance.autoCompactRequest(context.Background(), originalRequest)
	if result.Applied {
		t.Fatal("did not expect auto compaction with a small context window at the default threshold")
	}

	if uncompactedRequest.ContextWindow != originalRequest.ContextWindow {
		t.Fatalf("context window must stay untouched: %d", uncompactedRequest.ContextWindow)
	}
}

func TestAutoCompactRequestCompactsWhenContextWindowIsLarge(t *testing.T) {
	t.Parallel()

	originalRequest := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                      "",
		ConfiguredModel:            testAutoCompactMainModel,
		ContextWindow:              1_000,
		AutoCompactTokenLimit:      0,
		AutoCompactTokenLimitScope: autoCompactTokenLimitScopeTotal,
		CompactPrompt:              "",
		SessionID:                  "",
		PreviousResponseID:         "",
		RequestID:                  "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: repeatedAutoCompactText("user details", 5_000)},
			{Role: messageRoleAssistant, Content: "Assistant answer."},
			{Role: messageRoleUser, Content: "Follow-up question."},
		},
	}

	tokenLimit := autoCompactTokenLimit(originalRequest.ContextWindow, 0)
	if estimateChatCompletionRequestTokens(originalRequest) <= tokenLimit {
		t.Fatalf(
			"unexpected test setup: estimated %d <= limit %d",
			estimateChatCompletionRequestTokens(originalRequest),
			tokenLimit,
		)
	}

	instance := new(bot)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if len(request.Messages) != 2 {
			t.Fatalf("unexpected compaction request message count: %d", len(request.Messages))
		}

		return handle(newStreamDelta(testAutoCompactSummaryText, ""))
	})

	compactedRequest, result := instance.autoCompactRequest(context.Background(), originalRequest)
	if !result.Applied {
		t.Fatal("expected auto compaction to apply for a large context window")
	}

	if result.Strategy != autoCompactStrategySummary {
		t.Fatalf(testUnexpectedAutoCompactStrategyFormat, result.Strategy)
	}

	if len(compactedRequest.Messages) != 3 {
		t.Fatalf(testUnexpectedCompactedRequestLengthFormat, len(compactedRequest.Messages))
	}

	// The summary is appended LAST; the newest user message is retained before
	// it and the assistant message is dropped.
	if chatMessageText(compactedRequest.Messages[1]) != "Follow-up question." {
		t.Fatalf("expected newest user message retained: %#v", compactedRequest.Messages[1])
	}

	assertAutoCompactSummaryContains(t, compactedRequest.Messages[2], testAutoCompactSummaryText)
}

func TestAutoCompactRequestWithoutContextWindowLeavesRequestUnchanged(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                      "",
		ConfiguredModel:            testAutoCompactMainModel,
		ContextWindow:              0,
		AutoCompactTokenLimit:      0,
		AutoCompactTokenLimitScope: autoCompactTokenLimitScopeTotal,
		CompactPrompt:              "",
		SessionID:                  "",
		PreviousResponseID:         "",
		RequestID:                  "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: repeatedAutoCompactText("older context", 120)},
			{Role: messageRoleAssistant, Content: "Assistant answer."},
			{Role: messageRoleUser, Content: "Latest follow-up."},
		},
	}

	instance := new(bot)
	instance.chatCompletions = newUnexpectedCompactionClient(t)

	compactedRequest, result := instance.autoCompactRequest(context.Background(), request)
	if result.Applied {
		t.Fatal("did not expect auto compaction without a context window")
	}

	if !chatMessagesEqual(compactedRequest.Messages, request.Messages) {
		t.Fatalf("request must be unchanged without a context window: %#v", compactedRequest.Messages)
	}
}

func TestAutoCompactRequestForcesAtFullWindowCap(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                      "",
		ConfiguredModel:            testAutoCompactMainModel,
		ContextWindow:              1_000,
		AutoCompactTokenLimit:      0,
		AutoCompactTokenLimitScope: autoCompactTokenLimitScopeTotal,
		CompactPrompt:              "",
		SessionID:                  "",
		PreviousResponseID:         "",
		RequestID:                  "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: repeatedAutoCompactText("grows", 200)},
			{Role: messageRoleAssistant, Content: repeatedAutoCompactText("answers", 200)},
			{Role: messageRoleUser, Content: repeatedAutoCompactText("latest", 200)},
		},
	}

	estimated := estimateChatCompletionRequestTokens(request)

	hardCap := autoCompactFullWindowTokenLimit(request.ContextWindow)
	if estimated <= hardCap {
		t.Fatalf("unexpected test setup: estimated %d <= full-window cap %d", estimated, hardCap)
	}

	instance := new(bot)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		return handle(newStreamDelta(testAutoCompactSummaryText, ""))
	})

	compactedRequest, result := instance.autoCompactRequest(context.Background(), request)
	if !result.Applied {
		t.Fatal("expected full-window cap to trigger compaction")
	}

	if estimateChatMessagesTokens(compactedRequest.Messages) > hardCap {
		t.Fatalf(
			"compacted request exceeds full-window cap: %d > %d",
			estimateChatMessagesTokens(compactedRequest.Messages),
			hardCap,
		)
	}

	lastIndex := len(compactedRequest.Messages) - 1
	assertAutoCompactSummaryContains(t, compactedRequest.Messages[lastIndex], testAutoCompactSummaryText)
}

func TestAutoCompactRequestWithBodyAfterPrefixScopeWithoutConfigLimitOnlyHardCaps(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                      "",
		ConfiguredModel:            testAutoCompactMainModel,
		ContextWindow:              1_000,
		AutoCompactTokenLimit:      0,
		AutoCompactTokenLimitScope: autoCompactTokenLimitScopeBodyAfterPrefix,
		CompactPrompt:              "",
		SessionID:                  "",
		PreviousResponseID:         "",
		RequestID:                  "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: repeatedAutoCompactText("older context", 129)},
			{Role: messageRoleAssistant, Content: repeatedAutoCompactText("answer", 129)},
			{Role: messageRoleUser, Content: repeatedAutoCompactText("latest", 129)},
		},
	}

	estimated := estimateChatCompletionRequestTokens(request)

	fullWindow := autoCompactFullWindowTokenLimit(request.ContextWindow)
	if estimated >= fullWindow {
		t.Fatalf("unexpected test setup: estimated %d >= full-window cap %d", estimated, fullWindow)
	}

	// With scope body_after_prefix and no explicit config limit, only the hard
	// cap applies: the request sits below it, so nothing fires even though it
	// exceeds the derived 90% total limit.
	derivedLimit := autoCompactDerivedTokenLimit(request.ContextWindow)
	if estimated <= derivedLimit {
		t.Fatalf("unexpected test setup: estimated %d <= derived limit %d", estimated, derivedLimit)
	}

	// The truncation pre-step must not fire either: the latest message alone is
	// below the single-message budget.
	singleMessageLimit := autoCompactSingleMessageTokenLimit(request.ContextWindow)
	if estimateChatMessageTokens(request.Messages[len(request.Messages)-1]) > singleMessageLimit {
		t.Fatalf("unexpected test setup: latest message over single-message limit %d", singleMessageLimit)
	}

	instance := new(bot)
	instance.chatCompletions = newUnexpectedCompactionClient(t)

	uncompactedRequest, result := instance.autoCompactRequest(context.Background(), request)
	if result.Applied {
		t.Fatal("did not expect compaction to apply with only the hard cap active")
	}

	if !chatMessagesEqual(uncompactedRequest.Messages, request.Messages) {
		t.Fatalf("unexpected request mutation without compaction: %#v", uncompactedRequest.Messages)
	}
}

func TestTruncateTextToApproxTokenBudgetPreservesPrefixAndSuffixWithMarker(t *testing.T) {
	t.Parallel()

	text := repeatedAutoCompactText("word", 500)

	const tokenLimit = 50

	truncated := truncateTextToApproxTokenBudget(text, tokenLimit)

	if !strings.Contains(truncated, "…") {
		t.Fatalf("expected middle-out truncation marker, got %q", truncated)
	}

	if !strings.Contains(truncated, "tokens truncated…") {
		t.Fatalf("expected codex-style tokens-truncated marker, got %q", truncated)
	}

	if !strings.HasPrefix(truncated, "word ") {
		t.Fatalf("expected beginning preserved, got prefix %q", truncated[:min(len(truncated), 10)])
	}

	if !strings.HasSuffix(truncated, "word") {
		t.Fatalf("expected ending preserved, got suffix %q", truncated[max(0, len(truncated)-10):])
	}

	// Codex's budget applies to the surviving text before the marker is
	// appended; the marker is extra on top, so the whole string can exceed the
	// budget by the marker's tokens.
	if truncateTextToApproxTokenBudget(text, 0) != "" {
		t.Fatal("expected zero budget to truncate to empty")
	}
}

func TestAutoCompactRetainsNewestUserMessagesWithinCodexBudget(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                      "",
		ConfiguredModel:            testAutoCompactMainModel,
		ContextWindow:              1_000,
		AutoCompactTokenLimit:      0,
		AutoCompactTokenLimitScope: autoCompactTokenLimitScopeTotal,
		CompactPrompt:              "",
		SessionID:                  "",
		PreviousResponseID:         "",
		RequestID:                  "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: repeatedAutoCompactText("very old details", 5_000)},
			{Role: messageRoleAssistant, Content: "old answer"},
			{Role: messageRoleUser, Content: repeatedAutoCompactText("recent", 80)},
			{Role: messageRoleAssistant, Content: "recent answer"},
			{Role: messageRoleUser, Content: "latest question"},
		},
	}

	instance := new(bot)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		return handle(newStreamDelta(testAutoCompactSummaryText, ""))
	})

	compactedRequest, result := instance.autoCompactRequest(context.Background(), request)
	if !result.Applied {
		t.Fatal("expected auto compaction to apply")
	}

	if result.Strategy != autoCompactStrategySummary {
		t.Fatalf(testUnexpectedAutoCompactStrategyFormat, result.Strategy)
	}

	// The summary is the LAST message.
	last := compactedRequest.Messages[len(compactedRequest.Messages)-1]
	assertAutoCompactSummaryContains(t, last, testAutoCompactSummaryText)

	// The newest user messages (both small) are retained, the oldest
	// over-budget one and its assistant reply are replaced by the summary.
	hasOldest := false
	retainedUserTokens := 0

	for _, message := range compactedRequest.Messages {
		text := retainedChatMessageText(message)
		if text == "old answer" {
			hasOldest = true
		}

		if message.Role == messageRoleUser && !strings.Contains(text, autoCompactSummaryPrefix) {
			// The codex budget covers surviving text before the truncation
			// marker; strip any marker before counting tokens.
			retainedUserTokens += estimateTextTokens(strings.Split(text, "…")[0])
		}
	}

	if retainedUserTokens > autoCompactUserMessageMaxTokens {
		t.Fatalf(
			"retained user messages exceed codex budget: %d > %d",
			retainedUserTokens,
			autoCompactUserMessageMaxTokens,
		)
	}

	if hasOldest {
		t.Fatal("expected oldest history to be replaced by the summary")
	}
}

func retainedChatMessageText(message chatMessage) string {
	switch typed := message.Content.(type) {
	case string:
		return typed
	case []contentPart:
		texts := make([]string, 0, len(typed))
		for _, part := range typed {
			if partText, ok := part["text"].(string); ok {
				texts = append(texts, partText)
			}
		}

		return strings.Join(texts, " ")
	default:
		return ""
	}
}

func TestAutoCompactRequestWithoutWindowSkipsCompactionEntirely(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                      "",
		ConfiguredModel:            testAutoCompactMainModel,
		ContextWindow:              0,
		AutoCompactTokenLimit:      0,
		AutoCompactTokenLimitScope: autoCompactTokenLimitScopeTotal,
		CompactPrompt:              "",
		SessionID:                  "",
		PreviousResponseID:         "",
		RequestID:                  "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: repeatedAutoCompactText("older context", 200)},
			{Role: messageRoleUser, Content: repeatedAutoCompactText("latest", 200)},
		},
	}

	instance := new(bot)
	instance.chatCompletions = newUnexpectedCompactionClient(t)

	compactedRequest, result := instance.autoCompactRequest(context.Background(), request)
	if result.Applied {
		t.Fatal("did not expect auto compaction without a context window")
	}

	if !chatMessagesEqual(compactedRequest.Messages, request.Messages) {
		t.Fatalf("request must be unchanged without a context window: %#v", compactedRequest.Messages)
	}
}

func TestAutoCompactRequestCountsExternalPartsAgainstLimit(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                      "",
		ConfiguredModel:            testAutoCompactMainModel,
		ContextWindow:              5_000,
		AutoCompactTokenLimit:      3_000,
		AutoCompactTokenLimitScope: autoCompactTokenLimitScopeTotal,
		CompactPrompt:              "",
		SessionID:                  "",
		PreviousResponseID:         "",
		RequestID:                  "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: repeatedAutoCompactText("older context", 600)},
			{Role: messageRoleUser, Content: []contentPart{
				{"type": contentTypeImageURL, "image_url": map[string]string{"url": "https://example.com/image.png"}},
				{"type": contentTypeText, "text": "Latest image query."},
			}},
		},
	}

	if estimateChatCompletionRequestTokens(request) <= autoCompactTokenLimit(
		request.ContextWindow,
		request.AutoCompactTokenLimit,
	) {
		t.Fatalf(
			"unexpected test setup: estimated %d <= limit %d",
			estimateChatCompletionRequestTokens(request),
			autoCompactTokenLimit(request.ContextWindow, request.AutoCompactTokenLimit),
		)
	}

	instance := new(bot)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if len(request.Messages) != 2 {
			t.Fatalf("unexpected compaction request message count: %d", len(request.Messages))
		}

		return handle(newStreamDelta(testAutoCompactSummaryText, ""))
	})

	compactedRequest, result := instance.autoCompactRequest(context.Background(), request)
	if !result.Applied {
		t.Fatal("expected auto compaction to apply")
	}

	// The image-only user message (1844 tokens) plus its text part stays within
	// the retention budget, so the parts are counted and the image message is
	// retained before the summary.
	last := compactedRequest.Messages[len(compactedRequest.Messages)-1]
	assertAutoCompactSummaryContains(t, last, testAutoCompactSummaryText)
}

func TestAutoCompactSummaryPromptIsNeutralForUniversalUse(t *testing.T) {
	t.Parallel()

	systemPrompt := autoCompactSummarySystemPrompt()

	for _, forbidden := range []string{"coding", "code", "programming", "repository", "software"} {
		if containsFold(systemPrompt, forbidden) {
			t.Fatalf("expected neutral universal summary prompt without %q: %q", forbidden, systemPrompt)
		}
	}

	for _, expected := range []string{
		"conversation",
		"topic",
		"facts",
		"decisions",
		"questions",
	} {
		if !containsFold(systemPrompt, expected) {
			t.Fatalf("expected universal summary prompt to include %q: %q", expected, systemPrompt)
		}
	}
}

func TestAutoCompactMergePromptIsNeutralForUniversalUse(t *testing.T) {
	t.Parallel()

	mergePrompt := autoCompactMergeSystemPrompt()

	for _, forbidden := range []string{"coding", "code", "programming", "repository", "software"} {
		if containsFold(mergePrompt, forbidden) {
			t.Fatalf("expected neutral universal merge prompt without %q: %q", forbidden, mergePrompt)
		}
	}

	for _, expected := range []string{
		"summaries",
		"decisions",
		"preferences",
		"questions",
	} {
		if !containsFold(mergePrompt, expected) {
			t.Fatalf("expected universal merge prompt to include %q: %q", expected, mergePrompt)
		}
	}
}

func TestAutoCompactPromptsUseCheckpointHandoffWording(t *testing.T) {
	t.Parallel()

	systemPrompt := autoCompactSummarySystemPrompt()

	if !containsFold(systemPrompt, "checkpoint") {
		t.Fatalf("expected summary prompt to use checkpoint framing: %q", systemPrompt)
	}

	if !containsFold(systemPrompt, "other assistant") && !containsFold(systemPrompt, "next assistant") {
		t.Fatalf("expected summary prompt to hand off to a next assistant: %q", systemPrompt)
	}

	mergePrompt := autoCompactMergeSystemPrompt()

	if !containsFold(mergePrompt, "handoff") && !containsFold(mergePrompt, "carry forward") {
		t.Fatalf("expected merge prompt to carry the summary forward: %q", mergePrompt)
	}

	if !strings.Contains(autoCompactSummaryPrefix, "handoff") &&
		!strings.Contains(autoCompactSummaryPrefix, "checkpoint") {
		t.Fatalf(
			"expected universal summary prefix to use handoff or checkpoint framing: %q",
			autoCompactSummaryPrefix,
		)
	}
}

func repeatedAutoCompactText(fragment string, repeats int) string {
	return strings.TrimSpace(strings.Repeat(fragment+" ", repeats))
}

func autoCompactSizedASCIIText(tokens int) string {
	return strings.Repeat("a", tokens*autoCompactCharsPerToken)
}

func repeatedAutoCompactCSVRows(rows int) string {
	header := strings.Join([]string{
		"Application",
		"ProcessID",
		"SwapChainAddress",
		"PresentRuntime",
		"SyncInterval",
		"PresentFlags",
		"AllowsTearing",
		"PresentMode",
		"PCL",
		"MsPCLatency",
		"FPS",
	}, ",") + "\n"
	row := "Overwatch.exe,10436,0xE84F640,DXGI,0,0,0,Unknown,Application,0.0107,16.6590,15.9657,NA,0.0627,0.0000,NA\n"

	return header + strings.Repeat(row, rows)
}
