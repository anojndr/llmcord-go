// Package providers implements the streaming provider clients.
package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	queueFullRetryMaxAttempts = 2
	queueFullRetryFixedDelay  = 3 * time.Second

	// transientRetryMaxAttempts bounds re-sends for streams that fail without
	// producing any visible content: dropped mid-stream connections (EOF
	// before [DONE] / before response.completed), Gemini upstream NetworkError
	// interruptions, and clean-but-empty model responses. Once any content
	// has been delivered a stream is never re-sent, so a partial reply is
	// never duplicated.
	transientRetryMaxAttempts = 2
	transientRetryFixedDelay  = 1 * time.Second
)

// ChatCompletionRouter dispatches a completion to the right provider client.
type ChatCompletionRouter struct {
	openAI openAIClient
	gemini geminiClient
	keys   *APIKeyRotator
}

// NewChatCompletionRouter builds the router over the shared HTTP client.
func NewChatCompletionRouter(httpClient *http.Client) ChatCompletionRouter {
	return ChatCompletionRouter{
		openAI: newOpenAIClient(httpClient),
		gemini: newGeminiClient(httpClient),
		keys:   NewAPIKeyRotator(),
	}
}

// StreamChatCompletion streams a completion with key rotation and retries.
func (client ChatCompletionRouter) StreamChatCompletion(
	ctx context.Context,
	request ChatCompletionRequest,
	handle func(StreamDelta) error,
) error {
	rotatedKeys := client.keys.Rotate(request.Provider.APIKeys)
	request.Provider.APIKeys = rotatedKeys
	request.Provider.APIKey = firstAPIKey(rotatedKeys)

	for attempt := 1; ; attempt++ {
		contentSent := false
		wrappedHandle := func(delta StreamDelta) error {
			if deltaReferencesContent(delta) {
				contentSent = true
			}

			return handle(delta)
		}

		err := client.streamChatCompletionOnce(ctx, request, wrappedHandle)

		// A stream that delivered any content is final: retrying would
		// duplicate a partial reply in the user's chat.
		if contentSent {
			if err == nil {
				return nil
			}

			return err
		}

		// A clean-but-empty stream is an empty model response, classified as a
		// transient retry: it is re-sent once before being surfaced as
		// ErrEmptyModelResponse.
		retry := streamRetryAction(err)
		if retry.giveUp {
			if err == nil {
				return ErrEmptyModelResponse
			}

			return err
		}

		if attempt >= retry.maxAttempts {
			if err == nil {
				return ErrEmptyModelResponse
			}

			return err
		}

		logWarn(
			"stream result; retrying chat completion",
			err,
			"attempt",
			attempt,
			"max_attempts",
			retry.maxAttempts,
			"kind",
			retry.kind,
		)

		sleepErr := sleepStreamRetryDelay(ctx, retry.fixedDelay)
		if sleepErr != nil {
			return sleepErr
		}

		if len(request.Provider.APIKeys) > 1 {
			request.Provider.APIKeys = client.keys.Rotate(request.Provider.APIKeys)
			request.Provider.APIKey = firstAPIKey(request.Provider.APIKeys)
		}
	}
}

const (
	streamRetryKindTransient = "transient"
	streamRetryKindQueueFull = "queue-full"
	streamRetryKindEmpty     = "empty"
)

type streamRetry struct {
	kind        string
	maxAttempts int
	fixedDelay  time.Duration
	giveUp      bool
}

// streamRetryAction classifies a stream attempt that delivered no content
// into a retry policy. Provider-side request-queue saturation keeps its
// dedicated budget and delay; a clean-but-empty response (err == nil) is an
// empty model response and reuses the transient budget, as do disconnected
// streams before [DONE] / before response.completed and Gemini upstream
// NetworkError interruptions or transient server errors (503 UNAVAILABLE,
// 504 DEADLINE_EXCEEDED, 429 RESOURCE_EXHAUSTED, 502, 500, deadline expired).
// Anything else is returned unchanged.
func streamRetryAction(err error) streamRetry {
	transient := streamRetry{
		kind:        streamRetryKindTransient,
		maxAttempts: transientRetryMaxAttempts,
		fixedDelay:  transientRetryFixedDelay,
		giveUp:      false,
	}

	switch {
	case err == nil:
		transient.kind = streamRetryKindEmpty

		return transient
	case IsQueueFullQueueError(err):
		return streamRetry{
			kind:        streamRetryKindQueueFull,
			maxAttempts: queueFullRetryMaxAttempts,
			fixedDelay:  queueFullRetryFixedDelay,
			giveUp:      false,
		}
	case IsTransientStreamError(err) || errors.Is(err, ErrEmptyModelResponse):
		return transient
	default:
		return streamRetry{
			kind:        "",
			maxAttempts: 0,
			fixedDelay:  0,
			giveUp:      true,
		}
	}
}

func deltaReferencesContent(delta StreamDelta) bool {
	return delta.Thinking != "" || delta.Content != "" || delta.SearchMetadata != nil
}

// IsTransientStreamError reports whether a stream failure is safe to retry:
// the proxy connection dropped mid-stream (chat completions ending before
// [DONE], Responses ending before response.completed), Gemini upstream
// NetworkError interruptions or server-side transient errors (503 UNAVAILABLE,
// 504 DEADLINE_EXCEEDED, 429 RESOURCE_EXHAUSTED, 502, 500, deadline expired),
// and connection drops or unexpected EOFs before delivery of visible content.
// Explicit non-transient client errors (e.g. 400 Bad Request, 401 Unauthorized,
// 403 Forbidden, 404 Not Found, unsupported parameters) are not retried.
func IsTransientStreamError(err error) bool {
	if err == nil {
		return false
	}

	if IsGeminiTransientError(err) {
		return true
	}

	if errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}

	text := strings.ToLower(err.Error())

	return strings.Contains(text, "ended before") ||
		strings.Contains(text, "stream interrupted: networkerror") ||
		strings.Contains(text, "deadline expired before operation could complete") ||
		strings.Contains(text, "the model is overloaded") ||
		strings.Contains(text, "status: unavailable") ||
		strings.Contains(text, "status: deadline_exceeded") ||
		strings.Contains(text, "connection reset") ||
		strings.Contains(text, "connection refused") ||
		strings.Contains(text, "i/o timeout") ||
		strings.Contains(text, "tls handshake timeout")
}

// IsQueueFullQueueError reports whether an error is the proxy queue-full signal.
func IsQueueFullQueueError(err error) bool {
	if err == nil {
		return false
	}

	var statusErr StatusError
	if !errors.As(err, &statusErr) {
		return false
	}

	if statusErr.StatusCode != http.StatusServiceUnavailable {
		return false
	}

	return strings.Contains(strings.ToLower(statusErr.Message), "request queue is full")
}

func sleepStreamRetryDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for stream retry: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (client ChatCompletionRouter) streamChatCompletionOnce(
	ctx context.Context,
	request ChatCompletionRequest,
	handle func(StreamDelta) error,
) error {
	switch request.Provider.APIKind {
	case ProviderAPIKindGemini:
		return client.gemini.streamChatCompletion(ctx, request, handle)
	case ProviderAPIKindOpenAI:
		return client.openAI.streamChatCompletion(ctx, request, handle)
	default:
		return fmt.Errorf(
			"unsupported provider API kind %q: %w",
			request.Provider.APIKind,
			os.ErrInvalid,
		)
	}
}
