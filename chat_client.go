package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

type chatCompletionRouter struct {
	openAI               openAIClient
	openAICodex          openAICodexClient
	gemini               geminiClient
	waitForRetry         func(context.Context, time.Duration) error
	firstResponseTimeout time.Duration
}

const (
	sameKeyRetryDelayLimit      = time.Minute
	attemptTimeoutDivisor       = 2
	minAttemptTimeout           = 20 * time.Second
	maxAttemptTimeout           = 90 * time.Second
	defaultFirstResponseTimeout = 20 * time.Second
)

func newChatCompletionRouter(httpClient *http.Client) chatCompletionRouter {
	return chatCompletionRouter{
		openAI:               newOpenAIClient(httpClient),
		openAICodex:          newOpenAICodexClient(httpClient),
		gemini:               newGeminiClient(httpClient),
		waitForRetry:         waitForRetryDelay,
		firstResponseTimeout: defaultFirstResponseTimeout,
	}
}

func (client chatCompletionRouter) streamChatCompletion(
	ctx context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	apiKeys := request.Provider.apiKeysForAttempts()
	attemptErrors := make([]error, 0, len(apiKeys))

	globalStreamStarted := false

	for index, apiKey := range apiKeys {
		keyedRequest := request
		keyedRequest.Provider = request.Provider.withSingleAPIKey(apiKey)

		if index > 0 {
			_ = handle(streamDelta{
				Thinking:           "",
				Content:            "",
				FinishReason:       finishReasonRetryReset,
				Usage:              nil,
				ProviderResponseID: "",
				SearchMetadata:     nil,
			})
		}

		wrappedHandle := func(delta streamDelta) error {
			if delta.Content != "" {
				globalStreamStarted = true
			}

			return handle(delta)
		}

		streamStarted, err := client.streamChatCompletionForKey(
			ctx,
			keyedRequest,
			index < len(apiKeys)-1,
			wrappedHandle,
		)
		if err == nil {
			return nil
		}

		attemptErrors = append(attemptErrors, err)
		if globalStreamStarted || streamStarted || ctx.Err() != nil || index == len(apiKeys)-1 {
			if len(attemptErrors) == 1 {
				return err
			}

			if globalStreamStarted || streamStarted || ctx.Err() != nil {
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
	hasFallbackKey bool,
	handle func(streamDelta) error,
) (bool, error) {
	waitForRetry := client.retryDelayWaiter()
	retrySameKey := true
	attemptNumber := 0

	for {
		streamStarted := false
		attemptNumber++
		attemptCtx, attemptCancel := context.WithCancel(ctx)

		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			attemptTimeout := remaining / attemptTimeoutDivisor
			attemptTimeout = max(attemptTimeout, minAttemptTimeout)
			attemptTimeout = min(attemptTimeout, maxAttemptTimeout)

			if attemptTimeout < remaining {
				var cancel context.CancelFunc

				attemptCtx, cancel = context.WithTimeout(ctx, attemptTimeout)
				attemptCancel = cancel
			}
		}

		if attemptNumber > 1 {
			_ = handle(streamDelta{
				Thinking:           "",
				Content:            "",
				FinishReason:       finishReasonRetryReset,
				Usage:              nil,
				ProviderResponseID: "",
				SearchMetadata:     nil,
			})
		}

		err := client.streamChatCompletionOnce(attemptCtx, request, func(delta streamDelta) error {
			if delta.Content != "" {
				streamStarted = true
			}

			return handle(delta)
		})

		attemptCancel()

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

		if retryDelay > sameKeyRetryDelayLimit && hasFallbackKey {
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
	timeoutCtx, cancelTimeout := context.WithCancel(ctx)
	defer cancelTimeout()

	var responded atomic.Bool

	timeout := client.firstResponseTimeout
	if timeout <= 0 {
		timeout = defaultFirstResponseTimeout
	}

	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		select {
		case <-timer.C:
			if !responded.Load() {
				cancelTimeout()
			}
		case <-timeoutCtx.Done():
		}
	}()

	wrappedHandle := func(delta streamDelta) error {
		responded.Store(true)

		return handle(delta)
	}

	err := client.streamChatCompletionOnceNoTimeout(timeoutCtx, request, wrappedHandle)
	if err != nil {
		if ctx.Err() == nil && timeoutCtx.Err() != nil && !responded.Load() {
			return fmt.Errorf("model did not respond within 20 seconds: %w", context.DeadlineExceeded)
		}

		return err
	}

	return nil
}

func (client chatCompletionRouter) streamChatCompletionOnceNoTimeout(
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
