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

type chatCompletionRouter struct {
	openAI       openAIClient
	openAICodex  openAICodexClient
	gemini       geminiClient
	waitForRetry func(context.Context, time.Duration) error
}

const (
	sameKeyRetryDelayLimit   = time.Minute
	attemptTimeoutDivisor    = 2
	minAttemptTimeout        = 20 * time.Second
	maxAttemptTimeout        = 90 * time.Second
	maxEmptyResponseAttempts = 3
)

func newChatCompletionRouter(httpClient *http.Client) chatCompletionRouter {
	return chatCompletionRouter{
		openAI:       newOpenAIClient(httpClient),
		openAICodex:  newOpenAICodexClient(httpClient),
		gemini:       newGeminiClient(httpClient),
		waitForRetry: waitForRetryDelay,
	}
}

func sendRetryResetDelta(handle func(streamDelta) error, message string, attrs ...any) {
	err := handle(streamDelta{
		Thinking:           "",
		Content:            "",
		FinishReason:       finishReasonRetryReset,
		Usage:              nil,
		ProviderResponseID: "",
		SearchMetadata:     nil,
	})
	if err != nil {
		logWarn(message, err, attrs...)
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
			sendRetryResetDelta(
				handle,
				"send key rotation reset delta",
				"provider",
				request.Provider.APIKind,
				"key_index",
				index,
			)
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
		if index < len(apiKeys)-1 {
			logWarn(
				"provider api key attempt failed",
				err,
				"provider",
				request.Provider.APIKind,
				"key_index",
				index,
			)
		}

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
	emptyResponseAttempts := 0

	attemptTimeout := streamAttemptTimeout(ctx, hasFallbackKey)
	if attemptTimeout > 0 {
		var watchdog context.CancelFunc

		ctx, watchdog = context.WithTimeout(ctx, attemptTimeout)
		defer watchdog()
	}

	for {
		streamStarted := false
		attemptNumber++
		attemptCtx, attemptCancel := streamAttemptContext(ctx, hasFallbackKey, attemptTimeout)

		if attemptNumber > 1 {
			sendRetryResetDelta(
				handle,
				"send retry reset delta",
				"provider",
				request.Provider.APIKind,
				"attempt",
				attemptNumber,
			)
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

		if streamStarted || ctx.Err() != nil {
			return streamStarted, err
		}

		if errors.Is(err, context.DeadlineExceeded) || errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			return streamStarted, fmt.Errorf("stream attempt timeout: %w", err)
		}

		emptyResponse := errors.Is(err, errEmptyModelResponse)
		if emptyResponse {
			emptyResponseAttempts++

			if streamEmptyResponseAttemptsExhausted(
				err,
				emptyResponseAttempts,
				request.Provider.APIKind,
			) {
				return streamStarted, err
			}
		} else if !retrySameKey {
			return streamStarted, err
		}

		retryDelay, canRetry := providerRetryDecision(
			request.Provider.APIKind,
			err,
			emptyResponse,
			hasFallbackKey,
		)
		if !canRetry {
			return streamStarted, err
		}

		retrySameKey = false

		logWarn(
			"provider request failed, retrying",
			err,
			"provider",
			request.Provider.APIKind,
			"model",
			strings.TrimSpace(request.ConfiguredModel),
			"attempt",
			attemptNumber,
			"retry_delay",
			retryDelay,
		)

		err = waitForRetry(ctx, retryDelay)
		if err != nil {
			return streamStarted, fmt.Errorf("wait for provider retry delay: %w", err)
		}
	}
}

func providerRetryDecision(
	apiKind providerAPIKind,
	err error,
	emptyResponse bool,
	hasFallbackKey bool,
) (time.Duration, bool) {
	retryDelay, ok := retryDelayForProvider(apiKind, err)
	if !ok {
		return 0, false
	}

	_, hasExplicitRetryDelay := explicitRetryDelayForProvider(apiKind, err)
	if hasFallbackKey && !hasExplicitRetryDelay && !emptyResponse && isTransientStatusError(err) {
		return 0, false
	}

	if retryDelay > sameKeyRetryDelayLimit && hasFallbackKey {
		return 0, false
	}

	return retryDelay, true
}

func streamAttemptContext(
	ctx context.Context,
	hasFallbackKey bool,
	attemptTimeout time.Duration,
) (context.Context, context.CancelFunc) {
	attemptCtx, attemptCancel := context.WithCancel(ctx)

	if attemptTimeout <= 0 {
		return attemptCtx, attemptCancel
	}

	var cancel context.CancelFunc

	attemptCtx, cancel = context.WithTimeout(ctx, attemptTimeout)
	attemptCancel = cancel

	return attemptCtx, attemptCancel
}

// streamAttemptTimeout returns the timeout applied to each individual stream
// attempt, or 0 when the caller has no deadline of its own. With no deadline
// there is nothing to divide, and every attempt runs until the stream finishes
// naturally.
func streamAttemptTimeout(
	ctx context.Context,
	hasFallbackKey bool,
) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}

	remaining := time.Until(deadline)
	attemptTimeout := remaining

	if hasFallbackKey {
		attemptTimeout = remaining / attemptTimeoutDivisor
	}

	attemptTimeout = max(attemptTimeout, minAttemptTimeout)
	attemptTimeout = min(attemptTimeout, maxAttemptTimeout)

	return attemptTimeout
}

func streamEmptyResponseAttemptsExhausted(
	err error,
	attempts int,
	apiKind providerAPIKind,
) bool {
	if !errors.Is(err, errEmptyModelResponse) || attempts < maxEmptyResponseAttempts {
		return false
	}

	logWarn(
		"provider returned empty model responses repeatedly",
		err,
		"provider",
		apiKind,
		"attempts",
		attempts,
	)

	return true
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
