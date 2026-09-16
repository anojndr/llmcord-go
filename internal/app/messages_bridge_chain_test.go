package app

import (
	"context"
	"testing"
)

func TestPrepareMessageResponseChainsBridgeResponsesProvider(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-2"
		assistantMessageID = "assistant-message-1"
		configuredModel    = "chatgpt_to_openai/auto:vision"
		previousResponseID = "resp_parent_123"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		newPromptMessage("user-message-1", channelID, userID, botUserID),
	)
	sourceMessage.MessageReference = assistantMessage.Reference()
	sourceMessage.ReferencedMessage = assistantMessage
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "follow-up question"

	instance := newHistoryRetentionTestBot(t)

	sourceNode := instance.nodes.getOrCreate(sourceMessageID)
	sourceNode.mu.Lock()
	sourceNode.role = messageRoleUser
	sourceNode.text = "follow-up question"
	sourceNode.parentMessage = assistantMessage
	sourceNode.initialized = true
	sourceNode.mu.Unlock()
	instance.nodes.cacheLockedNode(sourceMessageID, sourceNode)

	assistantNode := instance.nodes.getOrCreate(assistantMessageID)
	assistantNode.mu.Lock()
	assistantNode.role = messageRoleAssistant
	assistantNode.text = "first answer"
	assistantNode.providerResponseID = previousResponseID
	assistantNode.providerResponseModel = configuredModel
	assistantNode.parentMessage = newPromptMessage("user-message-1", channelID, userID, botUserID)
	assistantNode.initialized = true
	assistantNode.mu.Unlock()
	instance.nodes.cacheLockedNode(assistantMessageID, assistantNode)

	loadedConfig := testSearchConfig()
	provider := loadedConfig.Providers["openai"]
	provider.API = "openai-responses"
	loadedConfig.Providers = map[string]providerConfig{"chatgpt_to_openai": provider}
	loadedConfig.Models = map[string]map[string]any{configuredModel: nil}
	loadedConfig.ModelOrder = []string{configuredModel}
	loadedConfig.MaxMessages = defaultMaxMessages

	request, _, _, err := instance.prepareMessageResponse(
		context.Background(),
		loadedConfig,
		sourceMessage,
		configuredModel,
		nil,
	)
	if err != nil {
		t.Fatalf("prepare message response: %v", err)
	}

	if request.PreviousResponseID != previousResponseID {
		t.Fatalf(
			"expected chained previous response %q, got %q",
			previousResponseID,
			request.PreviousResponseID,
		)
	}

	if request.PreviousResponseCount != len(request.Messages)-1 {
		t.Fatalf(
			"expected only the new tail chained, got stored count %d for %d messages",
			request.PreviousResponseCount,
			len(request.Messages),
		)
	}
}
