package main

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

func assignOpenAICodexSessionID(
	request *chatCompletionRequest,
	sourceMessage *discordgo.Message,
	store *messageNodeStore,
	maxMessages int,
) {
	if request == nil || request.Provider.APIKind != providerAPIKindOpenAICodex {
		return
	}

	request.SessionID = openAICodexConversationSessionID(
		request.ConfiguredModel,
		sourceMessage,
		store,
		maxMessages,
	)
}

func openAICodexConversationSessionID(
	configuredModel string,
	sourceMessage *discordgo.Message,
	store *messageNodeStore,
	maxMessages int,
) string {
	anchorMessageID := openAICodexConversationAnchorMessageID(sourceMessage, store, maxMessages)
	if anchorMessageID == "" {
		return ""
	}

	hash := sha256.Sum256([]byte(strings.TrimSpace(configuredModel) + "\n" + anchorMessageID))

	return fmt.Sprintf("llmcord-go-codex-%x", hash[:12])
}

func openAICodexConversationAnchorMessageID(
	sourceMessage *discordgo.Message,
	store *messageNodeStore,
	maxMessages int,
) string {
	if sourceMessage == nil {
		return ""
	}

	if maxMessages <= 0 {
		maxMessages = 1
	}

	currentMessage := sourceMessage
	anchorMessageID := strings.TrimSpace(sourceMessage.ID)

	for step := 0; currentMessage != nil && step < maxMessages; step++ {
		currentMessageID := strings.TrimSpace(currentMessage.ID)
		if currentMessageID != "" {
			anchorMessageID = currentMessageID
		}

		if store == nil || currentMessageID == "" {
			break
		}

		node, ok := store.get(currentMessageID)
		if !ok || node == nil {
			break
		}

		node.mu.Lock()
		parentMessage := node.parentMessage
		node.mu.Unlock()

		currentMessage = parentMessage
	}

	return anchorMessageID
}
