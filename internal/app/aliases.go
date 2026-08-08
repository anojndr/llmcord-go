// Package app implements the llmcord Discord bot runtime.
package app

import (
	providers "llmcord-go/internal/providers"
	searchtypes "llmcord-go/internal/searchtypes"
)

type chatMessage = providers.ChatMessage
type chatCompletionRequest = providers.ChatCompletionRequest
type streamDelta = providers.StreamDelta
type providerRequestConfig = providers.ProviderRequestConfig
type contentPart = searchtypes.ContentPart

func providerRequestPrimaryAPIKey(provider providerRequestConfig) string {
	if len(provider.APIKeys) == 0 {
		return provider.APIKey
	}

	return provider.APIKeys[0]
}
