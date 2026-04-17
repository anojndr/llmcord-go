package main

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

const (
	openAICodexPromptCacheKeyPrefix    = "llmcord-go-codex"
	openAIProviderPromptCacheKeyPrefix = "llmcord-go-openai"
)

func assignOpenAIPromptCacheKey(
	request *chatCompletionRequest,
	sourceMessage *discordgo.Message,
	store *messageNodeStore,
	maxMessages int,
) {
	if request == nil {
		return
	}

	cacheKeyPrefix := openAIRequestPromptCacheKeyPrefix(*request)
	if cacheKeyPrefix == "" {
		return
	}

	request.SessionID = openAIConversationPromptCacheKey(
		cacheKeyPrefix,
		request.ConfiguredModel,
		sourceMessage,
		store,
		maxMessages,
	)
}

func assignOpenAICodexSessionID(
	request *chatCompletionRequest,
	sourceMessage *discordgo.Message,
	store *messageNodeStore,
	maxMessages int,
) {
	if request == nil || request.Provider.APIKind != providerAPIKindOpenAICodex {
		return
	}

	assignOpenAIPromptCacheKey(request, sourceMessage, store, maxMessages)
}

func addOpenAIPromptCacheKey(requestBody map[string]any, request chatCompletionRequest) {
	if requestBody == nil || strings.TrimSpace(request.SessionID) == "" {
		return
	}

	if openAIRequestPromptCacheKeyPrefix(request) == "" {
		return
	}

	requestBody["prompt_cache_key"] = request.SessionID
}

func openAIConversationPromptCacheKey(
	cacheKeyPrefix string,
	configuredModel string,
	sourceMessage *discordgo.Message,
	store *messageNodeStore,
	maxMessages int,
) string {
	anchorMessageID := openAIConversationAnchorMessageID(sourceMessage, store, maxMessages)
	if cacheKeyPrefix == "" || anchorMessageID == "" {
		return ""
	}

	hash := sha256.Sum256([]byte(strings.TrimSpace(configuredModel) + "\n" + anchorMessageID))

	return fmt.Sprintf("%s-%x", cacheKeyPrefix, hash[:12])
}

func openAIRequestPromptCacheKeyPrefix(request chatCompletionRequest) string {
	switch {
	case request.Provider.APIKind == providerAPIKindOpenAICodex:
		return openAICodexPromptCacheKeyPrefix
	case request.Provider.APIKind == providerAPIKindOpenAI &&
		openAIConfiguredModel(request.ConfiguredModel):
		return openAIProviderPromptCacheKeyPrefix
	default:
		return ""
	}
}

func openAIConfiguredModel(configuredModel string) bool {
	providerName, _, err := splitConfiguredModel(strings.TrimSpace(configuredModel))
	if err != nil {
		return false
	}

	return strings.EqualFold(providerName, defaultOpenAIProviderName)
}

func openAIConversationAnchorMessageID(
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
