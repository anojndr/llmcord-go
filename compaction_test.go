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

func TestAutoCompactThresholdMatchesCodexWindowCap(t *testing.T) {
	t.Parallel()

	const (
		contextWindow      = 1_000
		aboveCodexPercent  = 95
		codexExpectedLimit = 900 // Codex auto-compacts at (window * 9) / 10.
	)

	limit := autoCompactTokenLimit(contextWindow, aboveCodexPercent)
	if limit != codexExpectedLimit {
		t.Fatalf(
			"expected threshold above 90%% to clamp to Codex's (window*9)/10: got %d want %d",
			limit,
			codexExpectedLimit,
		)
	}

	defaultLimit := autoCompactTokenLimit(contextWindow, 0)
	if defaultLimit != codexExpectedLimit {
		t.Fatalf(
			"expected default threshold to equal Codex's (window*9)/10: got %d want %d",
			defaultLimit,
			codexExpectedLimit,
		)
	}
}

func TestAutoCompactSingleMessageLimitFollowsCodexCappedThreshold(t *testing.T) {
	t.Parallel()

	const contextWindow = 1_000

	// At the Codex cap the latest-message truncation budget stays 10
	// percentage points below the capped threshold: 80% of the window.
	cappedPercent := autoCompactCodexCappedThresholdPercent(95)
	if cappedPercent != autoCompactCodexCapPercent {
		t.Fatalf("unexpected capped threshold percent: %d", cappedPercent)
	}

	singleMessageLimit := autoCompactSingleMessageTokenLimit(contextWindow, 95)

	expectedLimit := (contextWindow * (autoCompactCodexCapPercent - autoCompactSingleMessageMargin)) /
		autoCompactPercentBase
	if singleMessageLimit != expectedLimit {
		t.Fatalf(
			"unexpected single-message limit for capped threshold: got %d want %d",
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
		Model:                       "",
		ConfiguredModel:             testAutoCompactMainModel,
		ContextWindow:               1_000,
		AutoCompactThresholdPercent: 90,
		SessionID:                   "",
		PreviousResponseID:          "",
		RequestID:                   "",
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
		Model:                       "",
		ConfiguredModel:             testAutoCompactMainModel,
		ContextWindow:               1_000,
		AutoCompactThresholdPercent: 90,
		SessionID:                   "",
		PreviousResponseID:          "",
		RequestID:                   "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: csvText},
		},
	}

	singleMessageLimit := autoCompactSingleMessageTokenLimit(
		request.ContextWindow,
		request.AutoCompactThresholdPercent,
	)
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
		Model:                       "",
		ConfiguredModel:             testAutoCompactMainModel,
		ContextWindow:               1_000,
		AutoCompactThresholdPercent: 90,
		SessionID:                   "",
		PreviousResponseID:          "",
		RequestID:                   "",
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

	assertAutoCompactSummaryContains(t, compactedRequest.Messages[1], testAutoCompactOlderSummaryText)

	latestMessage := compactedRequest.Messages[len(compactedRequest.Messages)-1]
	if estimateChatMessageTokens(latestMessage) > autoCompactSingleMessageTokenLimit(
		request.ContextWindow,
		request.AutoCompactThresholdPercent,
	) {
		t.Fatalf("expected latest message to fit the single-message limit: %#v", latestMessage)
	}
}

func TestEstimateTextTokensCountsCSVLikeTextMoreConservativelyThanProse(t *testing.T) {
	t.Parallel()

	csvText := repeatedAutoCompactCSVRows(20)

	naiveCSVTokens := ceilDivPositive(len(strings.TrimSpace(csvText)), autoCompactCharsPerToken)
	if estimateTextTokens(csvText) <= naiveCSVTokens {
		t.Fatalf(
			"expected csv-like token estimate above naive character ratio: got %d want > %d",
			estimateTextTokens(csvText),
			naiveCSVTokens,
		)
	}

	proseText := repeatedAutoCompactText("average frame pacing stayed steady during the capture", 20)

	naiveProseTokens := ceilDivPositive(len(strings.TrimSpace(proseText)), autoCompactCharsPerToken)
	if estimateTextTokens(proseText) != naiveProseTokens {
		t.Fatalf(
			"expected prose estimate to keep the character ratio: got %d want %d",
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
		testSearchDeciderModel: 2200,
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

func TestDecideWebSearchTruncatesNearWindowConversation(t *testing.T) {
	t.Parallel()

	const (
		contextWindow              = 200_000
		historyTokens              = 139_000
		newInputTokens             = 100_000
		expectedCompactionCalls    = 0
		expectedSearchDeciderCalls = 1
	)

	client, counter := newTruncatedSearchDeciderClient(t)

	instance := newSearchTestBot(client, newStubWebSearchClient(func(
		context.Context,
		config,
		[]string,
	) ([]webSearchResult, error) {
		return nil, nil
	}))

	loadedConfig := testSearchConfig()
	loadedConfig.ModelContextWindows = map[string]int{
		testAutoCompactMainModel: contextWindow,
		testSearchDeciderModel:   contextWindow,
	}

	conversation := []chatMessage{
		{Role: messageRoleUser, Content: repeatedAutoCompactText("older context", historyTokens)},
		{Role: messageRoleAssistant, Content: "Earlier answer."},
		{Role: messageRoleUser, Content: repeatedAutoCompactText("new input", newInputTokens)},
	}

	decision, _, err := instance.decideWebSearch(
		context.Background(),
		loadedConfig,
		testAutoCompactMainModel,
		nil,
		conversation,
	)
	if err != nil {
		t.Fatalf("decide web search: %v", err)
	}

	if deciderCalls := counter.deciderCalls.Load(); deciderCalls != expectedSearchDeciderCalls {
		t.Fatalf(
			"expected exactly one search decider request: %d",
			deciderCalls,
		)
	}

	if compactionCalls := counter.compactionCalls.Load(); compactionCalls != expectedCompactionCalls {
		t.Fatalf(
			"near-window conversations must not trigger search-decider compaction: %d calls",
			compactionCalls,
		)
	}

	if decision.NeedsSearch {
		t.Fatal("expected decider to skip web search")
	}
}

type truncatedSearchDeciderCounter struct {
	compactionCalls atomic.Int64
	deciderCalls    atomic.Int64
}

func newTruncatedSearchDeciderClient(
	t *testing.T,
) (*stubChatCompletionClient, *truncatedSearchDeciderCounter) {
	t.Helper()

	counter := new(truncatedSearchDeciderCounter)

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

		requestTokens := estimateChatMessagesTokens(request.Messages)
		if requestTokens > searchDeciderMaxRequestTokens {
			t.Fatalf(
				"search decider request exceeds bounded context budget: %d > %d",
				requestTokens,
				searchDeciderMaxRequestTokens,
			)
		}

		return handle(newStreamDelta(`{"needs_search":false,"queries":[]}`, ""))
	})

	return client, counter
}

func TestTruncateSearchDeciderConversationBoundsOversizedLatestMessage(t *testing.T) {
	t.Parallel()

	conversation := []chatMessage{
		{Role: messageRoleUser, Content: "Earlier question."},
		{Role: messageRoleAssistant, Content: "Earlier answer."},
		{Role: messageRoleUser, Content: repeatedAutoCompactText("pasted report", 8_000)},
	}

	truncated := truncateSearchDeciderConversation(conversation)

	if len(truncated) == 0 {
		t.Fatal("expected at least the latest message to survive truncation")
	}

	if truncated[len(truncated)-1].Role != messageRoleUser {
		t.Fatalf("expected latest user message to be preserved: %#v", truncated[len(truncated)-1])
	}

	originalText, foundOriginal := conversation[2].Content.(string)
	if !foundOriginal {
		t.Fatalf("unexpected original content type: %T", conversation[2].Content)
	}

	latestText, foundLatest := truncated[len(truncated)-1].Content.(string)
	if !foundLatest {
		t.Fatalf("unexpected latest content type: %T", truncated[len(truncated)-1].Content)
	}

	if latestText == originalText {
		t.Fatal("expected oversized latest message text to be truncated for the decider")
	}

	if estimateChatMessageTokens(truncated[len(truncated)-1]) >
		searchDeciderMaxRequestTokens-searchDeciderPromptOverheadTokens {
		t.Fatalf(
			"expected latest message to fit the decider context budget: %d",
			estimateChatMessageTokens(truncated[len(truncated)-1]),
		)
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

	if request.Messages[0].Role != messageRoleSystem {
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
		Model:                       "",
		ConfiguredModel:             testAutoCompactMainModel,
		ContextWindow:               1_000,
		AutoCompactThresholdPercent: 0,
		SessionID:                   "",
		PreviousResponseID:          "",
		RequestID:                   "",
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
		Model:                       "",
		ConfiguredModel:             testAutoCompactMainModel,
		ContextWindow:               1_000,
		AutoCompactThresholdPercent: 0,
		SessionID:                   "",
		PreviousResponseID:          "",
		RequestID:                   "",
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

	assertAutoCompactSummaryContains(t, compactedRequest.Messages[0], testAutoCompactSummaryText)
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
		Model:                       "",
		ConfiguredModel:             testAutoCompactMainModel,
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		SessionID:                   "",
		PreviousResponseID:          "",
		RequestID:                   "",
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
		Model:                       "",
		ConfiguredModel:             testAutoCompactMainModel,
		ContextWindow:               5_000,
		AutoCompactThresholdPercent: 50,
		SessionID:                   "",
		PreviousResponseID:          "",
		RequestID:                   "",
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
		request.AutoCompactThresholdPercent,
	) {
		t.Fatalf(
			"unexpected test setup: estimated %d <= limit %d",
			estimateChatCompletionRequestTokens(request),
			autoCompactTokenLimit(request.ContextWindow, request.AutoCompactThresholdPercent),
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

	limit := autoCompactTokenLimit(request.ContextWindow, request.AutoCompactThresholdPercent)
	if estimateChatMessagesTokens(compactedRequest.Messages) > limit {
		t.Fatalf(
			"compacted request exceeds token limit: %d > %d",
			estimateChatMessagesTokens(compactedRequest.Messages),
			limit,
		)
	}
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
