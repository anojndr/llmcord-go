package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	queueFullRetryMaxAttempts = 2
	queueFullRetryFixedDelay  = 3 * time.Second
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

	attempt := 1

	for {
		anyDeltaSent := false
		wrappedHandle := func(delta streamDelta) error {
			anyDeltaSent = true

			return handle(delta)
		}

		err := client.streamChatCompletionOnce(ctx, request, wrappedHandle)

		if err != nil && (attempt >= queueFullRetryMaxAttempts || anyDeltaSent || !isQueueFullQueueError(err)) {
			return err
		}

		if err == nil {
			return nil
		}

		logWarn(
			"queue-full stream error; retrying chat completion",
			err,
			"attempt",
			attempt,
			"max_attempts",
			queueFullRetryMaxAttempts,
		)

		sleepErr := sleepQueueFullRetryDelay(ctx)
		if sleepErr != nil {
			return sleepErr
		}

		attempt++
	}
}

func isQueueFullQueueError(err error) bool {
	if err == nil {
		return false
	}

	var statusErr providerStatusError
	if !errors.As(err, &statusErr) {
		return false
	}

	if statusErr.StatusCode != http.StatusServiceUnavailable {
		return false
	}

	return strings.Contains(strings.ToLower(statusErr.Message), "request queue is full")
}

func sleepQueueFullRetryDelay(ctx context.Context) error {
	timer := time.NewTimer(queueFullRetryFixedDelay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for queue-full retry: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
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
