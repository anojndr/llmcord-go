package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

const (
	autoCompactContextWindowPercent  = 90
	autoCompactPercentBase           = 100
	autoCompactMinimumMessages       = 2
	autoCompactMaxTailMessages       = 4
	autoCompactMinChunkTokens        = 512
	autoCompactMaxChunkTokens        = 3000
	autoCompactChunkDivisor          = 3
	autoCompactCharsPerToken         = 4
	autoCompactMessageOverheadTokens = 8
	autoCompactImageTokens           = 1024
	autoCompactAudioTokens           = 4096
	autoCompactDocumentTokens        = 4096
	autoCompactVideoTokens           = 8192
	autoCompactSummaryPrefix         = "Earlier conversation summary " +
		"(auto-compacted to fit the model context window):"
	autoCompactSummaryUserPrefix     = "Summarize this earlier conversation context so it can be carried forward:\n\n"
	autoCompactMergeUserPrefix       = "Merge these partial conversation summaries into one concise summary:\n\n"
	autoCompactMessageBlockSeparator = "\n\n"
	autoCompactImagePlaceholder      = "[image attachment]"
	autoCompactAudioPlaceholder      = "[audio attachment]"
	autoCompactDocumentPlaceholder   = "[document attachment]"
	autoCompactVideoPlaceholder      = "[video attachment]"
)

var errAutoCompactRequestTooLarge = errors.New("unable to auto-compact request within token limit")

type autoCompactStrategy string

const (
	autoCompactStrategySummary autoCompactStrategy = "summary"
	autoCompactStrategyTrimmed autoCompactStrategy = "trimmed"
)

type autoCompactResult struct {
	Applied  bool
	Strategy autoCompactStrategy
}

func autoCompactTokenLimit(contextWindow int) int {
	if contextWindow <= 0 {
		return 0
	}

	return (contextWindow * autoCompactContextWindowPercent) / autoCompactPercentBase
}

func (instance *bot) autoCompactRequest(
	ctx context.Context,
	request chatCompletionRequest,
) (chatCompletionRequest, autoCompactResult) {
	limit := autoCompactTokenLimit(request.ContextWindow)
	if limit <= 0 {
		return request, autoCompactResult{
			Applied:  false,
			Strategy: "",
		}
	}

	estimatedTokens := estimateChatCompletionRequestTokens(request)
	if estimatedTokens <= limit {
		return request, autoCompactResult{
			Applied:  false,
			Strategy: "",
		}
	}

	systemMessages, conversationMessages := splitLeadingSystemMessages(request.Messages)
	if len(conversationMessages) < autoCompactMinimumMessages {
		return request, autoCompactResult{
			Applied:  false,
			Strategy: "",
		}
	}

	compactedMessages, warning, err := instance.compactMessagesForRequest(
		ctx,
		request,
		systemMessages,
		conversationMessages,
		limit,
	)
	if err != nil {
		slog.Warn(
			"auto compact request",
			"configured_model",
			request.ConfiguredModel,
			"context_window",
			request.ContextWindow,
			"estimated_tokens",
			estimatedTokens,
			"token_limit",
			limit,
			"error",
			err,
		)

		return request, autoCompactResult{
			Applied:  false,
			Strategy: "",
		}
	}

	if len(compactedMessages) == 0 || chatMessagesEqual(compactedMessages, request.Messages) {
		return request, autoCompactResult{
			Applied:  false,
			Strategy: "",
		}
	}

	request.Messages = compactedMessages

	return request, autoCompactResult{
		Applied:  true,
		Strategy: warning,
	}
}

func (instance *bot) compactMessagesForRequest(
	ctx context.Context,
	request chatCompletionRequest,
	systemMessages []chatMessage,
	conversationMessages []chatMessage,
	limit int,
) ([]chatMessage, autoCompactStrategy, error) {
	maxTailMessages := minInt(autoCompactMaxTailMessages, len(conversationMessages)-1)

	var lastErr error

	for tailMessages := maxTailMessages; tailMessages >= 1; tailMessages-- {
		summarySource := conversationMessages[:len(conversationMessages)-tailMessages]

		summaryText, err := instance.summarizeMessagesForAutoCompaction(
			ctx,
			request,
			summarySource,
			limit,
		)
		if err != nil {
			lastErr = err

			continue
		}

		candidateMessages, fits := buildAutoCompactedMessages(
			systemMessages,
			conversationMessages[len(conversationMessages)-tailMessages:],
			summaryText,
			limit,
		)
		if fits {
			return candidateMessages, autoCompactStrategySummary, nil
		}
	}

	tailOnlyMessages, fits := trimConversationTailToFit(
		systemMessages,
		conversationMessages,
		limit,
	)
	if fits {
		return tailOnlyMessages, autoCompactStrategyTrimmed, nil
	}

	if lastErr != nil {
		return nil, "", lastErr
	}

	return nil, "", errAutoCompactRequestTooLarge
}

func buildAutoCompactedMessages(
	systemMessages []chatMessage,
	tailMessages []chatMessage,
	summaryText string,
	limit int,
) ([]chatMessage, bool) {
	trimmedSummary := strings.TrimSpace(summaryText)
	if trimmedSummary == "" {
		return nil, false
	}

	summaryMessageText := autoCompactSummaryMessageText(trimmedSummary)
	summaryMessage := chatMessage{
		Role:    messageRoleUser,
		Content: summaryMessageText,
	}

	candidateMessages := appendChatMessages(systemMessages, []chatMessage{summaryMessage}, tailMessages)
	if estimateChatMessagesTokens(candidateMessages) <= limit {
		return candidateMessages, true
	}

	availableSummaryTokens := limit -
		estimateChatMessagesTokens(systemMessages) -
		estimateChatMessagesTokens(tailMessages) -
		autoCompactMessageOverheadTokens
	if availableSummaryTokens <= 0 {
		return nil, false
	}

	truncatedSummaryMessageText := truncateTextToApproxTokens(
		summaryMessageText,
		availableSummaryTokens,
	)
	if strings.TrimSpace(truncatedSummaryMessageText) == "" {
		return nil, false
	}

	summaryMessage.Content = truncatedSummaryMessageText
	candidateMessages = appendChatMessages(systemMessages, []chatMessage{summaryMessage}, tailMessages)

	return candidateMessages, estimateChatMessagesTokens(candidateMessages) <= limit
}

func trimConversationTailToFit(
	systemMessages []chatMessage,
	conversationMessages []chatMessage,
	limit int,
) ([]chatMessage, bool) {
	for tailMessages := len(conversationMessages); tailMessages >= 1; tailMessages-- {
		candidateMessages := appendChatMessages(
			systemMessages,
			conversationMessages[len(conversationMessages)-tailMessages:],
		)
		if estimateChatMessagesTokens(candidateMessages) <= limit {
			return candidateMessages, true
		}
	}

	return nil, false
}

func (instance *bot) summarizeMessagesForAutoCompaction(
	ctx context.Context,
	request chatCompletionRequest,
	messages []chatMessage,
	limit int,
) (string, error) {
	blocks := renderMessagesForAutoCompaction(messages)
	if len(blocks) == 0 {
		return "", nil
	}

	chunkBudget := autoCompactChunkTokenBudget(limit)

	summaryText, err := instance.reduceAutoCompactionBlocks(
		ctx,
		request,
		blocks,
		chunkBudget,
		autoCompactSummarySystemPrompt(),
		autoCompactSummaryUserPrefix,
	)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(summaryText), nil
}

func (instance *bot) reduceAutoCompactionBlocks(
	ctx context.Context,
	request chatCompletionRequest,
	blocks []string,
	chunkBudget int,
	systemPrompt string,
	userPrefix string,
) (string, error) {
	if len(blocks) == 0 {
		return "", nil
	}

	currentBlocks := append([]string(nil), blocks...)
	currentPrompt := systemPrompt
	currentUserPrefix := userPrefix

	for {
		chunks := chunkAutoCompactionBlocks(currentBlocks, chunkBudget)
		if len(chunks) == 0 {
			return "", nil
		}

		summaries := make([]string, 0, len(chunks))
		for _, chunk := range chunks {
			summaryText, err := instance.runAutoCompactionPrompt(
				ctx,
				request,
				currentPrompt,
				currentUserPrefix+chunk,
			)
			if err != nil {
				return "", err
			}

			normalizedSummary := strings.TrimSpace(summaryText)
			if normalizedSummary == "" {
				continue
			}

			summaries = append(summaries, normalizedSummary)
		}

		if len(summaries) == 0 {
			return "", nil
		}

		if len(summaries) == 1 {
			return summaries[0], nil
		}

		currentBlocks = make([]string, 0, len(summaries))
		for index, summary := range summaries {
			currentBlocks = append(
				currentBlocks,
				fmt.Sprintf("PARTIAL SUMMARY %d:\n%s", index+1, summary),
			)
		}

		currentPrompt = autoCompactMergeSystemPrompt()
		currentUserPrefix = autoCompactMergeUserPrefix
	}
}

func (instance *bot) runAutoCompactionPrompt(
	ctx context.Context,
	request chatCompletionRequest,
	systemPrompt string,
	userPrompt string,
) (string, error) {
	compactionRequest := request
	compactionRequest.SessionID = ""
	compactionRequest.Messages = []chatMessage{
		{
			Role:    openAICodexRoleSystem,
			Content: systemPrompt,
		},
		{
			Role:    messageRoleUser,
			Content: userPrompt,
		},
	}

	return collectChatCompletionText(ctx, instance.chatCompletions, compactionRequest)
}

func renderMessagesForAutoCompaction(messages []chatMessage) []string {
	blocks := make([]string, 0, len(messages))

	for _, message := range messages {
		contentText := strings.TrimSpace(renderAutoCompactMessageContent(message.Content))
		if contentText == "" {
			continue
		}

		role := strings.ToUpper(strings.TrimSpace(message.Role))
		if role == "" {
			role = "MESSAGE"
		}

		blocks = append(blocks, role+":\n"+contentText)
	}

	return blocks
}

func renderAutoCompactMessageContent(content any) string {
	switch typedContent := content.(type) {
	case nil:
		return ""
	case string:
		return typedContent
	case []contentPart:
		parts := make([]string, 0, len(typedContent))

		for _, part := range typedContent {
			partText := renderAutoCompactContentPart(part)
			if strings.TrimSpace(partText) == "" {
				continue
			}

			parts = append(parts, partText)
		}

		return strings.Join(parts, "\n")
	default:
		return fmt.Sprint(content)
	}
}

func renderAutoCompactContentPart(part contentPart) string {
	partType, _ := part["type"].(string)

	switch partType {
	case contentTypeText:
		textValue, _ := part["text"].(string)

		return textValue
	case contentTypeImageURL:
		return autoCompactMediaPlaceholder(autoCompactImagePlaceholder, part)
	case contentTypeAudioData:
		return autoCompactMediaPlaceholder(autoCompactAudioPlaceholder, part)
	case contentTypeDocument:
		return autoCompactMediaPlaceholder(autoCompactDocumentPlaceholder, part)
	case contentTypeVideoData:
		return autoCompactMediaPlaceholder(autoCompactVideoPlaceholder, part)
	default:
		return ""
	}
}

func autoCompactMediaPlaceholder(defaultLabel string, part contentPart) string {
	filename, _ := part[contentFieldFilename].(string)
	if strings.TrimSpace(filename) == "" {
		return defaultLabel
	}

	return strings.TrimSuffix(defaultLabel, "]") + ": " + filename + "]"
}

func chunkAutoCompactionBlocks(blocks []string, chunkBudget int) []string {
	if len(blocks) == 0 {
		return nil
	}

	if chunkBudget < autoCompactMinChunkTokens {
		chunkBudget = autoCompactMinChunkTokens
	}

	chunks := make([]string, 0, len(blocks))
	currentChunk := make([]string, 0, len(blocks))
	currentTokens := 0

	flushChunk := func() {
		if len(currentChunk) == 0 {
			return
		}

		chunks = append(chunks, strings.Join(currentChunk, autoCompactMessageBlockSeparator))
		currentChunk = currentChunk[:0]
		currentTokens = 0
	}

	for _, block := range blocks {
		splitBlocks := splitTextToApproxTokenChunks(block, chunkBudget)
		for _, splitBlock := range splitBlocks {
			blockTokens := estimateTextTokens(splitBlock)
			if currentTokens > 0 && currentTokens+blockTokens > chunkBudget {
				flushChunk()
			}

			currentChunk = append(currentChunk, splitBlock)
			currentTokens += blockTokens
		}
	}

	flushChunk()

	return chunks
}

func splitTextToApproxTokenChunks(text string, tokenLimit int) []string {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" {
		return nil
	}

	if tokenLimit < autoCompactMinChunkTokens {
		tokenLimit = autoCompactMinChunkTokens
	}

	charLimit := tokenLimit * autoCompactCharsPerToken
	chunks := make([]string, 0, 1)
	remainingText := trimmedText

	for remainingText != "" {
		if runeCount(remainingText) <= charLimit {
			chunks = append(chunks, remainingText)

			break
		}

		chunkText, nextText := splitRunesPrefix(remainingText, charLimit)
		chunks = append(chunks, strings.TrimSpace(chunkText))
		remainingText = strings.TrimSpace(nextText)
	}

	return chunks
}

func autoCompactChunkTokenBudget(limit int) int {
	if limit <= 0 {
		return autoCompactMinChunkTokens
	}

	chunkBudget := limit / autoCompactChunkDivisor
	if chunkBudget < autoCompactMinChunkTokens {
		return autoCompactMinChunkTokens
	}

	if chunkBudget > autoCompactMaxChunkTokens {
		return autoCompactMaxChunkTokens
	}

	return chunkBudget
}

func autoCompactSummaryMessageText(summaryText string) string {
	return strings.TrimSpace(autoCompactSummaryPrefix + "\n\n" + strings.TrimSpace(summaryText))
}

func (result autoCompactResult) warningForPath(path string) string {
	switch result.Strategy {
	case autoCompactStrategySummary:
		return "Warning: " + path + " auto-compacted older conversation context to fit the model context window."
	case autoCompactStrategyTrimmed:
		return "Warning: " + path + " trimmed older conversation context to fit the model context window."
	default:
		return ""
	}
}

func truncateTextToApproxTokens(text string, tokenLimit int) string {
	if tokenLimit <= 0 {
		return ""
	}

	charLimit := tokenLimit * autoCompactCharsPerToken

	return strings.TrimSpace(truncateRunes(text, charLimit))
}

func autoCompactSummarySystemPrompt() string {
	return strings.Join([]string{
		"You are compressing earlier conversation context so another assistant can continue helping the user.",
		"",
		"Create a concise plain-text summary that preserves:",
		"- the current topic or request",
		"- important facts, answers, decisions, and conclusions already given",
		"- unresolved questions or next steps",
		"- user preferences or constraints",
		"- notable findings from attachments, websites, visual search, or web search when relevant",
		"",
		"Do not assume this is a coding task. Keep the summary neutral, factual, and compact.",
	}, "\n")
}

func autoCompactMergeSystemPrompt() string {
	return strings.Join([]string{
		"You are merging partial conversation summaries into one plain-text summary for a later assistant.",
		"",
		"Keep only the important facts, decisions, user preferences, unresolved questions, and notable findings.",
		"Do not assume this is a coding task. Keep the result concise and neutral.",
	}, "\n")
}

func estimateChatCompletionRequestTokens(request chatCompletionRequest) int {
	return estimateChatMessagesTokens(request.Messages)
}

func estimateChatMessagesTokens(messages []chatMessage) int {
	totalTokens := 0

	for _, message := range messages {
		totalTokens += estimateChatMessageTokens(message)
	}

	return totalTokens
}

func estimateChatMessageTokens(message chatMessage) int {
	return autoCompactMessageOverheadTokens + estimateChatMessageContentTokens(message.Content)
}

func estimateChatMessageContentTokens(content any) int {
	switch typedContent := content.(type) {
	case nil:
		return 0
	case string:
		return estimateTextTokens(typedContent)
	case []contentPart:
		totalTokens := 0

		for _, part := range typedContent {
			totalTokens += estimateContentPartTokens(part)
		}

		return totalTokens
	default:
		return estimateTextTokens(fmt.Sprint(content))
	}
}

func estimateContentPartTokens(part contentPart) int {
	partType, _ := part["type"].(string)

	switch partType {
	case contentTypeText:
		textValue, _ := part["text"].(string)

		return estimateTextTokens(textValue)
	case contentTypeImageURL:
		return autoCompactImageTokens
	case contentTypeAudioData:
		return autoCompactAudioTokens
	case contentTypeDocument:
		return autoCompactDocumentTokens
	case contentTypeVideoData:
		return autoCompactVideoTokens
	default:
		return 0
	}
}

func estimateTextTokens(text string) int {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" {
		return 0
	}

	textLength := len(trimmedText)
	tokens := textLength / autoCompactCharsPerToken

	if textLength%autoCompactCharsPerToken != 0 {
		tokens++
	}

	if tokens == 0 {
		return 1
	}

	return tokens
}

func splitLeadingSystemMessages(messages []chatMessage) ([]chatMessage, []chatMessage) {
	splitIndex := 0
	for splitIndex < len(messages) &&
		strings.EqualFold(strings.TrimSpace(messages[splitIndex].Role), openAICodexRoleSystem) {
		splitIndex++
	}

	systemMessages := append([]chatMessage(nil), messages[:splitIndex]...)
	conversationMessages := append([]chatMessage(nil), messages[splitIndex:]...)

	return systemMessages, conversationMessages
}

func appendChatMessages(groups ...[]chatMessage) []chatMessage {
	totalMessages := 0
	for _, group := range groups {
		totalMessages += len(group)
	}

	messages := make([]chatMessage, 0, totalMessages)
	for _, group := range groups {
		messages = append(messages, group...)
	}

	return messages
}

func chatMessagesEqual(left []chatMessage, right []chatMessage) bool {
	if len(left) != len(right) {
		return false
	}

	for index := range left {
		if left[index].Role != right[index].Role {
			return false
		}

		if renderAutoCompactMessageContent(left[index].Content) != renderAutoCompactMessageContent(right[index].Content) {
			return false
		}
	}

	return true
}
