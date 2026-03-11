package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
)

type chatCompletionRouter struct {
	openAI      openAIClient
	openAICodex openAICodexClient
	gemini      geminiClient
}

func newChatCompletionRouter(httpClient *http.Client) chatCompletionRouter {
	return chatCompletionRouter{
		openAI:      newOpenAIClient(httpClient),
		openAICodex: newOpenAICodexClient(httpClient),
		gemini:      newGeminiClient(httpClient),
	}
}

func (client chatCompletionRouter) streamChatCompletion(
	ctx context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	apiKeys := request.Provider.apiKeysForAttempts()
	attemptErrors := make([]error, 0, len(apiKeys))

	for index, apiKey := range apiKeys {
		keyedRequest := request
		keyedRequest.Provider = request.Provider.withSingleAPIKey(apiKey)

		emittedDelta := false

		err := client.streamChatCompletionOnce(ctx, keyedRequest, func(delta streamDelta) error {
			emittedDelta = true

			return handle(delta)
		})
		if err == nil {
			return nil
		}

		attemptErrors = append(attemptErrors, err)
		if emittedDelta || !shouldRetryWithNextAPIKey(err) || index == len(apiKeys)-1 {
			if len(attemptErrors) == 1 {
				return err
			}

			if emittedDelta || !shouldRetryWithNextAPIKey(err) {
				return err
			}

			return fmt.Errorf("all configured API keys failed: %w", errors.Join(attemptErrors...))
		}
	}

	return fmt.Errorf("missing API key attempt: %w", os.ErrInvalid)
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
	case providerAPIKindOpenAICodex:
		return client.openAICodex.streamChatCompletion(ctx, request, handle)
	default:
		return fmt.Errorf(
			"unsupported provider API kind %q: %w",
			request.Provider.APIKind,
			os.ErrInvalid,
		)
	}
}
