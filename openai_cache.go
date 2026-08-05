package main

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

const (
	openAIProviderPromptCacheKeyPrefix = "llmcord-go-openai"
	openAICacheBreakpointKey           = "prompt_cache_breakpoint"
	openAICacheOptionsModeKey          = "mode"
)

func openAIModelIsGPT56Family(model string) bool {
	return strings.HasPrefix(openAIReasoningModelID(model), "gpt-5.6")
}

func openAICacheOptionsMode(extraBody map[string]any) (string, bool) {
	rawCacheOptions, cacheOptionsOK := extraBody["prompt_cache_options"]
	if !cacheOptionsOK || rawCacheOptions == nil {
		return "", false
	}

	cacheOptions, cacheOptionsMapOK := rawCacheOptions.(map[string]any)
	if !cacheOptionsMapOK {
		return "", false
	}

	mode, modeOK := cacheOptions[openAICacheOptionsModeKey].(string)

	return strings.TrimSpace(mode), modeOK
}

func assignOpenAIPromptCacheKey(
	request *chatCompletionRequest,
	sourceMessage *discordgo.Message,
	store *messageNodeStore,
	maxMessages int,
) {
	assignOpenAIPromptCacheKeyWithScope(request, sourceMessage, store, maxMessages, "")
}

func assignOpenAIPromptCacheKeyWithScope(
	request *chatCompletionRequest,
	sourceMessage *discordgo.Message,
	store *messageNodeStore,
	maxMessages int,
	scope string,
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
		scope,
	)
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
	scope string,
) string {
	anchorMessageID := openAIConversationAnchorMessageID(sourceMessage, store, maxMessages)
	if cacheKeyPrefix == "" || anchorMessageID == "" {
		return ""
	}

	hashInput := strings.TrimSpace(configuredModel)
	if trimmedScope := strings.TrimSpace(scope); trimmedScope != "" {
		hashInput += "\n" + trimmedScope
	}

	hashInput += "\n" + anchorMessageID

	hash := sha256.Sum256([]byte(hashInput))

	return fmt.Sprintf("%s-%x", cacheKeyPrefix, hash[:12])
}

func openAIRequestPromptCacheKeyPrefix(request chatCompletionRequest) string {
	switch request.Provider.APIKind {
	case providerAPIKindOpenAI:
		if openAIConfiguredModel(request.ConfiguredModel) {
			return openAIProviderPromptCacheKeyPrefix
		}

		return ""
	case providerAPIKindGemini:
		return ""
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
