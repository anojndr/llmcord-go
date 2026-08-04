package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"
)

type transientTextError string

func (text transientTextError) Error() string {
	return string(text)
}

func TestIsTransientProviderStatusCode(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		statusCode int
		expected   bool
	}{
		{name: "ok", statusCode: http.StatusOK, expected: false},
		{name: "bad request", statusCode: http.StatusBadRequest, expected: false},
		{name: "unauthorized", statusCode: http.StatusUnauthorized, expected: false},
		{name: "forbidden", statusCode: http.StatusForbidden, expected: false},
		{name: "not found", statusCode: http.StatusNotFound, expected: false},
		{name: "request timeout", statusCode: http.StatusRequestTimeout, expected: true},
		{name: "conflict", statusCode: http.StatusConflict, expected: true},
		{name: "too many requests", statusCode: http.StatusTooManyRequests, expected: true},
		{name: "internal server error", statusCode: http.StatusInternalServerError, expected: true},
		{name: "bad gateway", statusCode: http.StatusBadGateway, expected: true},
		{name: "service unavailable", statusCode: http.StatusServiceUnavailable, expected: true},
		{name: "gateway timeout", statusCode: http.StatusGatewayTimeout, expected: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if actual := isTransientProviderStatusCode(testCase.statusCode); actual != testCase.expected {
				t.Fatalf(
					"isTransientProviderStatusCode(%d) = %t, want %t",
					testCase.statusCode,
					actual,
					testCase.expected,
				)
			}
		})
	}
}

func TestIsTransientErrorClassifiesProviderStatusErrors(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		statusCode int
		expected   bool
	}{
		{name: "zero", statusCode: 0, expected: false},
		{name: "bad request", statusCode: http.StatusBadRequest, expected: false},
		{name: "unauthorized", statusCode: http.StatusUnauthorized, expected: false},
		{name: "forbidden", statusCode: http.StatusForbidden, expected: false},
		{name: "not found", statusCode: http.StatusNotFound, expected: false},
		{name: "request timeout", statusCode: http.StatusRequestTimeout, expected: true},
		{name: "conflict", statusCode: http.StatusConflict, expected: true},
		{name: "too many requests", statusCode: http.StatusTooManyRequests, expected: true},
		{name: "internal server error", statusCode: http.StatusInternalServerError, expected: true},
		{name: "bad gateway", statusCode: http.StatusBadGateway, expected: true},
		{name: "service unavailable", statusCode: http.StatusServiceUnavailable, expected: true},
		{name: "gateway timeout", statusCode: http.StatusGatewayTimeout, expected: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := providerStatusError{
				StatusCode: testCase.statusCode,
				Message:    "test provider error",
				RetryDelay: 0,
				Err:        os.ErrInvalid,
			}

			if actual := isTransientError(providerAPIKindOpenAI, err); actual != testCase.expected {
				t.Fatalf(
					"isTransientError with status %d = %t, want %t",
					testCase.statusCode,
					actual,
					testCase.expected,
				)
			}
		})
	}
}

func TestIsTransientErrorMatchesTransientErrorMessageText(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		err      error
		expected bool
	}{
		{name: "nil error", err: nil, expected: false},
		{name: "unrelated error", err: transientTextError("you have exceeded your quota"), expected: false},
		{name: "bad request", err: transientTextError("bad request"), expected: false},
		{
			name:     "queue full",
			err:      transientTextError("Streaming response failed: [503] The request queue is full."),
			expected: true,
		},
		{
			name:     "overloaded",
			err:      transientTextError("the api is overloaded right now"),
			expected: true,
		},
		{
			name:     "temporarily unavailable",
			err:      transientTextError("service temporarily unavailable"),
			expected: true,
		},
		{
			name:     "try again later",
			err:      transientTextError("please try again later"),
			expected: true,
		},
		{
			name:     "connection refused",
			err:      transientTextError("connection refused"),
			expected: true,
		},
		{
			name:     "empty model response",
			err:      transientTextError("model returned an empty response"),
			expected: true,
		},
		{
			name:     "context deadline exceeded is not retried",
			err:      context.DeadlineExceeded,
			expected: false,
		},
		{
			name:     "wrapped context deadline exceeded is not retried",
			err:      fmt.Errorf("stream gemini content: %w", context.DeadlineExceeded),
			expected: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if actual := isTransientError(providerAPIKindOpenAI, testCase.err); actual != testCase.expected {
				t.Fatalf(
					"isTransientError(%q) = %t, want %t",
					testCase.err,
					actual,
					testCase.expected,
				)
			}
		})
	}
}

func TestRetryDelayForProviderUsesOneSecondForTransientStatus(t *testing.T) {
	t.Parallel()

	err := providerStatusError{
		StatusCode: http.StatusServiceUnavailable,
		Message:    "The request queue is full.",
		RetryDelay: 0,
		Err:        os.ErrInvalid,
	}

	delay, ok := retryDelayForProvider(providerAPIKindOpenAI, err)
	if !ok {
		t.Fatal("expected transient provider status error to be retryable")
	}

	if delay != time.Second {
		t.Fatalf("retry delay = %s, want %s", delay, time.Second)
	}
}
