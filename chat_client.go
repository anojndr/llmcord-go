package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
)

type chatCompletionRouter struct {
	openAI openAIClient
	gemini geminiClient
	keys   *apiKeyRotator
}

func newChatCompletionRouter(httpClient *http.Client) chatCompletionRouter {
	return chatCompletionRouter{
		openAI: newOpenAIClient(httpClient),
		gemini: newGeminiClient(httpClient),
		keys:   newAPIKeyRotator(),
	}
}

func (client chatCompletionRouter) streamChatCompletion(
	ctx context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	rotatedKeys := client.keys.rotate(request.Provider.apiKeys())
	request.Provider.APIKeys = rotatedKeys
	request.Provider.APIKey = firstAPIKey(rotatedKeys)

	return client.streamChatCompletionOnce(ctx, request, handle)
}

func (client chatCompletionRouter) streamChatCompletionOnce(
	ctx context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	switch request.Provider.APIKind {
	case providerAPIKindGemini:
		return client.gemini.streamChatCompletion(ctx, request, handle)
	case providerAPIKindOpenAI:
		return client.openAI.streamChatCompletion(ctx, request, handle)
	default:
		return fmt.Errorf(
			"unsupported provider API kind %q: %w",
			request.Provider.APIKind,
			os.ErrInvalid,
		)
	}
}
