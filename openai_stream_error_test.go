package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAIStreamEventErrorPreservesStatusCodeFromMessage(t *testing.T) {
	t.Parallel()

	err := openAIStreamEventError(
		"Streaming response failed: [503] The request queue is full.",
		"server_error",
		nil,
	)

	var statusErr providerStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected provider status error, got %T: %v", err, err)
	}

	if statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf(
			"status code = %d, want %d",
			statusErr.StatusCode,
			http.StatusServiceUnavailable,
		)
	}

	if !strings.Contains(err.Error(), "request queue is full") ||
		!strings.Contains(err.Error(), "type=server_error") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestOpenAIStreamEventErrorPreservesStatusCodeFromCode(t *testing.T) {
	t.Parallel()

	err := openAIStreamEventError("rate limited", "server_error", 429)

	var statusErr providerStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected provider status error, got %T: %v", err, err)
	}

	if statusErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf(
			"status code = %d, want %d",
			statusErr.StatusCode,
			http.StatusTooManyRequests,
		)
	}
}

func TestOpenAIStreamEventErrorDefaultsServerErrorToServiceUnavailable(t *testing.T) {
	t.Parallel()

	err := openAIStreamEventError("provider failed unexpectedly", "server_error", nil)

	var statusErr providerStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected provider status error, got %T: %v", err, err)
	}

	if statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf(
			"status code = %d, want %d",
			statusErr.StatusCode,
			http.StatusServiceUnavailable,
		)
	}
}

func TestOpenAIStreamEventErrorWithoutStatusCodeStaysPlain(t *testing.T) {
	t.Parallel()

	err := openAIStreamEventError("unsupported parameter", "invalid_request_error", "unsupported_parameter")

	var statusErr providerStatusError
	if errors.As(err, &statusErr) {
		t.Fatalf("expected plain error, got provider status error: %#v", statusErr)
	}

	if !strings.Contains(err.Error(), "unsupported parameter") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestOpenAIStreamStatusCodeFromText(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		text     string
		expected int
	}{
		{name: "bracketed status", text: "[503]", expected: http.StatusServiceUnavailable},
		{
			name:     "bracketed status in message",
			text:     "Streaming response failed: [503] The request queue is full.",
			expected: http.StatusServiceUnavailable,
		},
		{name: "bare status", text: "503 Service Unavailable", expected: http.StatusServiceUnavailable},
		{name: "bare numeric code", text: "429", expected: http.StatusTooManyRequests},
		{name: "surrounded by letters", text: "abc503def", expected: http.StatusServiceUnavailable},
		{name: "inside longer number", text: "15039", expected: 0},
		{name: "low status", text: "200 OK", expected: 0},
		{name: "no status", text: "rate limited", expected: 0},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if actual := openAIStreamStatusCodeFromText(testCase.text); actual != testCase.expected {
				t.Fatalf(
					"openAIStreamStatusCodeFromText(%q) = %d, want %d",
					testCase.text,
					actual,
					testCase.expected,
				)
			}
		})
	}
}
