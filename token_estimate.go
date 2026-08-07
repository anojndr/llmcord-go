package main

import (
	"fmt"
	"strings"
)

const (
	// tokenEstimateBytesPerToken mirrors Codex's approx_token_count byte
	// ratio: each token is assumed to take about four bytes.
	tokenEstimateBytesPerToken = 4

	// Approximate token overhead for non-text request parts, used when
	// estimating a request's total size against the model context window.
	estimatedImagePartTokens    = 1844 // codex RESIZED_IMAGE_BYTES_ESTIMATE (7373) / 4, ceil
	estimatedAudioPartTokens    = 4096
	estimatedDocumentPartTokens = 4096
	estimatedFilePartTokens     = 4096
	estimatedVideoPartTokens    = 8192
)

// splitLeadingSystemMessages separates the contiguous leading system messages
// (the system prompt) from the conversation messages that follow them.
func splitLeadingSystemMessages(messages []chatMessage) ([]chatMessage, []chatMessage) {
	splitIndex := 0
	for splitIndex < len(messages) &&
		strings.EqualFold(strings.TrimSpace(messages[splitIndex].Role), messageRoleSystem) {
		splitIndex++
	}

	systemMessages := append([]chatMessage(nil), messages[:splitIndex]...)
	conversationMessages := append([]chatMessage(nil), messages[splitIndex:]...)

	return systemMessages, conversationMessages
}

// estimateChatCompletionRequestTokens estimates the total token count of a
// request's messages using Codex's byte ratio.
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
	return estimateChatMessageContentTokens(message.Content)
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
		return estimatedImagePartTokens
	case contentTypeAudioData:
		return estimatedAudioPartTokens
	case contentTypeDocument:
		return estimatedDocumentPartTokens
	case contentTypeFileData:
		return estimatedFilePartTokens
	case contentTypeVideoData:
		return estimatedVideoPartTokens
	default:
		return 0
	}
}

// estimateTextTokens mirrors Codex's approx_token_count: ceil(bytes/4),
// floored at one token for non-empty text.
func estimateTextTokens(text string) int {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" {
		return 0
	}

	return max(approxTokensFromBytes(len(trimmedText)), 1)
}

// appendChatMessages concatenates message groups in order.
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

// approxTokensFromBytes mirrors Codex's approx_token_count: ceil(bytes/4),
// floored at one token for non-empty text.
func approxTokensFromBytes(bytes int) int {
	if bytes <= 0 {
		return 0
	}

	return (bytes + tokenEstimateBytesPerToken - 1) / tokenEstimateBytesPerToken
}
