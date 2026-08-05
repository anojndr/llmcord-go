package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChatCompletionRouterRetriesQueueFull503UntilSuccess(t *testing.T) {
	t.Parallel()

	var (
		sawQueueFull atomic.Bool
		attempts     atomic.Int32
	)

	server := chatCompletionQueueFullRetryServer(t, func(_ int) int {
		if !sawQueueFull.Load() {
			return http.StatusServiceUnavailable
		}

		return http.StatusOK
	}, &attempts, &sawQueueFull)

	defer server.Close()

	client := chatQueueRetryTestClient(t, server)

	request := chatCompletionRetryTestRequest(server.URL)

	var joined strings.Builder

	err := client.streamChatCompletion(
		context.Background(),
		request,
		func(delta streamDelta) error {
			joined.WriteString(delta.Content)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat completion with retry: %v", err)
	}

	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}

	if joined.String() != "success" {
		t.Fatalf("unexpected streamed content: %q", joined.String())
	}
}

func TestChatCompletionRouterGivesUpOnPersistentQueueFull503(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := chatCompletionQueueFullRetryServer(t, func(_ int) int {
		return http.StatusServiceUnavailable
	}, &attempts, nil)

	defer server.Close()

	client := chatQueueRetryTestClient(t, server)

	request := chatCompletionRetryTestRequest(server.URL)

	err := client.streamChatCompletion(
		context.Background(),
		request,
		func(streamDelta) error { return nil },
	)
	if err == nil {
		t.Fatal("expected queue-full retries to exhaust and fail")
	}

	if attempts.Load() != queueFullRetryMaxAttempts {
		t.Fatalf(
			"attempts = %d, want %d (exhausted retry budget)",
			attempts.Load(),
			queueFullRetryMaxAttempts,
		)
	}

	if !strings.Contains(err.Error(), "request queue is full") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestChatCompletionRouterDoesNotRetryNonQueueFullErrors(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		t.Helper()

		attempts.Add(1)

		responseWriter.Header().Set("Content-Type", "text/event-stream")
		responseWriter.WriteHeader(http.StatusOK)

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"error\":{\"message\":\"unsupported parameter\",\"type\":"+
				"\"invalid_request_error\",\"code\":\"unsupported_parameter\"}}\n\n",
		)
	}))
	defer server.Close()

	client := chatQueueRetryTestClient(t, server)

	request := chatCompletionRetryTestRequest(server.URL)

	err := client.streamChatCompletion(
		context.Background(),
		request,
		func(streamDelta) error { return nil },
	)
	if err == nil {
		t.Fatal("expected non-transient error to surface")
	}

	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry for non-queue-full error)", attempts.Load())
	}

	if !strings.Contains(err.Error(), "unsupported parameter") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// chatCompletionQueueFullRetryServer responds to each request with an SSE
// stream. The statusForAttempt callback picks the status; a 503 is emitted as
// a queue-full SSE error event, any other status streams a success chunk.
func chatCompletionQueueFullRetryServer(
	t *testing.T,
	statusForAttempt func(int) int,
	attempts *atomic.Int32,
	sawQueueFull *atomic.Bool,
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		t.Helper()

		status := statusForAttempt(int(attempts.Load()))
		attempts.Add(1)

		responseWriter.Header().Set("Content-Type", "text/event-stream")
		responseWriter.WriteHeader(http.StatusOK)

		if status == http.StatusServiceUnavailable {
			if sawQueueFull != nil {
				sawQueueFull.Store(true)
			}

			writeStreamChunk(
				t,
				responseWriter,
				"data: {\"error\":{\"message\":\"Streaming response failed: "+
					"[503] The request queue is full.\",\"type\":\"server_error\",\"code\":null}}\n\n",
			)

			return
		}

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"choices\":[{\"delta\":{\"content\":\"success\"}}]}\n\n",
		)
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
	}))
}

func chatQueueRetryTestClient(t *testing.T, server *httptest.Server) chatCompletionRouter {
	t.Helper()

	return chatCompletionRouter{
		openAI: newOpenAIClient(server.Client()),
		gemini: newGeminiClient(server.Client()),
		keys:   newAPIKeyRotator(),
	}
}

func chatCompletionRetryTestRequest(baseURL string) chatCompletionRequest {
	request := emptyChatCompletionRequest()
	request.Provider = providerRequestConfig{
		APIKind:         providerAPIKindOpenAI,
		BaseURL:         baseURL,
		APIKey:          "test-key",
		APIKeys:         nil,
		UseResponsesAPI: false,
		EnableGrounding: false,
		ExtraHeaders:    nil,
		ExtraQuery:      nil,
		ExtraBody:       nil,
	}
	request.Model = "stable_model:vision"
	request.ConfiguredModel = "9router/stable_model:vision"
	request.Messages = []chatMessage{
		{Role: messageRoleUser, Content: "hello"},
	}

	return request
}
