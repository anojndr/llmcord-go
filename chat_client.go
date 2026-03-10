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
}

func newChatCompletionRouter(httpClient *http.Client) chatCompletionRouter {
	return chatCompletionRouter{
		openAI: newOpenAIClient(httpClient),
		gemini: newGeminiClient(httpClient),
	}
}

func (client chatCompletionRouter) streamChatCompletion(
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
