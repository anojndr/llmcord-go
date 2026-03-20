package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

type chatCompletionRouter struct {
	openAI       openAIClient
	openAICodex  openAICodexClient
	gemini       geminiClient
	waitForRetry func(context.Context, time.Duration) error
}

func newChatCompletionRouter(httpClient *http.Client) chatCompletionRouter {
	return chatCompletionRouter{
		openAI:       newOpenAIClient(httpClient),
		openAICodex:  newOpenAICodexClient(httpClient),
		gemini:       newGeminiClient(httpClient),
		waitForRetry: waitForRetryDelay,
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

		streamStarted, err := client.streamChatCompletionForKey(ctx, keyedRequest, handle)
		if err == nil {
			return nil
		}

		attemptErrors = append(attemptErrors, err)
		if streamStarted || ctx.Err() != nil || index == len(apiKeys)-1 {
			if len(attemptErrors) == 1 {
				return err
			}

			if streamStarted || ctx.Err() != nil {
				return err
			}

			return fmt.Errorf("all configured API keys failed: %w", errors.Join(attemptErrors...))
		}
	}

	return fmt.Errorf("missing API key attempt: %w", os.ErrInvalid)
}

func (client chatCompletionRouter) streamChatCompletionForKey(
	ctx context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) (bool, error) {
	waitForRetry := client.retryDelayWaiter()
	retrySameKey := true

	for {
		streamStarted := false

		err := client.streamChatCompletionOnce(ctx, request, func(delta streamDelta) error {
			streamStarted = true

			return handle(delta)
		})
		if err == nil {
			return streamStarted, nil
		}

		if streamStarted || !retrySameKey || ctx.Err() != nil {
			return streamStarted, err
		}

		retryDelay, ok := retryDelayForProvider(request.Provider.APIKind, err)
		if !ok {
			return streamStarted, err
		}

		retrySameKey = false

		err = waitForRetry(ctx, retryDelay)
		if err != nil {
			return streamStarted, fmt.Errorf("wait for provider retry delay: %w", err)
		}
	}
}

func (client chatCompletionRouter) retryDelayWaiter() func(context.Context, time.Duration) error {
	if client.waitForRetry != nil {
		return client.waitForRetry
	}

	return waitForRetryDelay
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
