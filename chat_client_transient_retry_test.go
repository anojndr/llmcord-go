package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestChatCompletionRouterRetriesEOFBeforeDoneUntilSuccess verifies that a
// chat-completions stream that ends before [DONE] (a dropped connection
// mid-stream, which surfaces as
// "chat completion stream ended before [DONE]: unexpected EOF") is retried
// once, and the eventual completed stream is returned as success. No content
// had streamed on the failing attempt, so the retry is safe.
func TestChatCompletionRouterRetriesEOFBeforeDoneUntilSuccess(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		t.Helper()

		attempts.Add(1)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		if attempts.Load() == 1 {
			// First attempt yields a 200 with no SSE body at all: the reader
			// hits EOF before ever seeing [DONE], with nothing streamed.
			return
		}

		writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
	}))
	defer server.Close()

	router := chatCompletionRouterRetryTestClient(server)

	var joined strings.Builder

	err := router.streamChatCompletion(
		context.Background(),
		openAIChatCompletionRetryTestRequest(server.URL),
		func(delta streamDelta) error {
			joined.WriteString(delta.Content)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 (one transient failure, one retry)", attempts.Load())
	}

	if joined.String() != "partial" {
		t.Fatalf("unexpected streamed content: %q", joined.String())
	}
}

// TestChatCompletionRouterRetriesResponsesEOFUntilSuccess verifies that a
// Responses-API stream ending without a response.completed event (wrapped as
// io.ErrUnexpectedEOF) is retried once and then succeeds.
func TestChatCompletionRouterRetriesResponsesEOFUntilSuccess(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		t.Helper()

		attempts.Add(1)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		if attempts.Load() == 1 {
			// Drop mid-stream before any content: no response.completed, no
			// [DONE]. Nothing was delivered, so the retry is safe.
			return
		}

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n",
		)
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_x\","+
				"\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
		)
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
	}))
	defer server.Close()

	router := chatCompletionRouterRetryTestClient(server)
	request := openAIChatCompletionsRetryTestRequest(server.URL)

	var joined strings.Builder

	err := router.streamChatCompletion(
		context.Background(),
		request,
		func(delta streamDelta) error {
			joined.WriteString(delta.Content)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream responses chat completion: %v", err)
	}

	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 (transient EOF then retry)", attempts.Load())
	}

	if joined.String() != "Hello" {
		t.Fatalf("unexpected streamed content: %q", joined.String())
	}
}

// TestChatCompletionRouterExhaustsTransientStreamRetries verifies that a
// stream that keeps ending before [DONE] gives up after the retry budget and
// surfaces the underlying EOF error.
func TestChatCompletionRouterExhaustsTransientStreamRetries(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		t.Helper()

		attempts.Add(1)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		// Never write [DONE], never write content: EOF before [DONE] on every
		// attempt, with nothing streamed.
	}))
	defer server.Close()

	router := chatCompletionRouterRetryTestClient(server)

	err := router.streamChatCompletion(
		context.Background(),
		openAIChatCompletionRetryTestRequest(server.URL),
		func(streamDelta) error { return nil },
	)
	if err == nil {
		t.Fatal("expected exhausted retries to fail")
	}

	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 (retry budget exhausted)", attempts.Load())
	}

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestChatCompletionRouterDoesNotRetryTransientErrorAfterContent verifies
// that a stream that already emitted content is never re-sent, so users never
// see a partial response duplicated by a retry.
func TestChatCompletionRouterDoesNotRetryTransientErrorAfterContent(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		t.Helper()

		attempts.Add(1)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		// The stream emits content before dropping, so no retry should follow.
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
		)
	}))
	defer server.Close()

	router := chatCompletionRouterRetryTestClient(server)

	err := router.streamChatCompletion(
		context.Background(),
		openAIChatCompletionRetryTestRequest(server.URL),
		func(streamDelta) error { return nil },
	)
	if err == nil {
		t.Fatal("expected missing [DONE] error")
	}

	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry after content streamed)", attempts.Load())
	}

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestChatCompletionRouterRetriesEmptyModelResponse verifies that an empty
// model response (a stream that completes without producing content) is
// retried once, and then the retried content wins.
func TestChatCompletionRouterRetriesEmptyModelResponse(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		t.Helper()

		attempts.Add(1)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		if attempts.Load() == 1 {
			// Completes cleanly ([DONE]) but produces no content: empty model
			// response.
			writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")

			return
		}

		writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"retried\"}}]}\n\n")
		writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
	}))
	defer server.Close()

	router := chatCompletionRouterRetryTestClient(server)

	var joined strings.Builder

	err := router.streamChatCompletion(
		context.Background(),
		openAIChatCompletionRetryTestRequest(server.URL),
		func(delta streamDelta) error {
			joined.WriteString(delta.Content)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 (empty response then retry)", attempts.Load())
	}

	if joined.String() != "retried" {
		t.Fatalf("unexpected streamed content: %q", joined.String())
	}
}

// TestChatCompletionRouterExhaustsEmptyModelResponseRetries verifies that a
// stream which keeps completing without content retries once and then
// surfaces errEmptyModelResponse, which renders as
// "The model returned an empty response. Try again.".
func TestChatCompletionRouterExhaustsEmptyModelResponseRetries(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		t.Helper()

		attempts.Add(1)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		// Completes cleanly ([DONE]) but never produces content.
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
	}))
	defer server.Close()

	router := chatCompletionRouterRetryTestClient(server)

	err := router.streamChatCompletion(
		context.Background(),
		openAIChatCompletionRetryTestRequest(server.URL),
		func(streamDelta) error { return nil },
	)
	if !errors.Is(err, errEmptyModelResponse) {
		t.Fatalf("expected errEmptyModelResponse, got: %v", err)
	}

	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 (empty retry budget exhausted)", attempts.Load())
	}

	if userFacingResponseError(err) != "The model returned an empty response. Try again." {
		t.Fatalf("unexpected user-facing error: %q", userFacingResponseError(err))
	}
}

// TestChatCompletionRouterDoesNotRetryProviderStatusError verifies that a
// provider status error (e.g. rate limit) is not retried by the transient
// retry path.
func TestChatCompletionRouterDoesNotRetryProviderStatusError(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		t.Helper()

		attempts.Add(1)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"error\":{\"message\":\"rate limited\",\"type\":\"server_error\",\"code\":\"rate_limit_exceeded\"}}\n\n",
		)
	}))
	defer server.Close()

	router := chatCompletionRouterRetryTestClient(server)

	err := router.streamChatCompletion(
		context.Background(),
		openAIChatCompletionRetryTestRequest(server.URL),
		func(streamDelta) error { return nil },
	)
	if err == nil {
		t.Fatal("expected rate-limited stream error")
	}

	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry for provider status error)", attempts.Load())
	}
}

// chatCompletionRouterRetryTestClient builds a router that dispatches every
// request to the openai client backed by the passed httptest server. The
// gemini client is left disconnected (an unused stub) to prove retries reach
// the right provider family.
func chatCompletionRouterRetryTestClient(server *httptest.Server) chatCompletionRouter {
	return chatCompletionRouter{
		openAI: newOpenAIClient(server.Client()),
		gemini: newGeminiClient(nil),
		keys:   newAPIKeyRotator(),
	}
}

// openAIChatCompletionRetryTestRequest builds a chat-completions request the
// same way openai_test.go's tests do.
func openAIChatCompletionRetryTestRequest(baseURL string) chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         baseURL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "gpt-test",
		ConfiguredModel:    "",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages:           []chatMessage{{Role: "user", Content: "hello"}},
	}
}

// openAIChatCompletionsRetryTestRequest builds a Responses-API request (no
// images so no file-upload stub is needed).
func openAIChatCompletionsRetryTestRequest(baseURL string) chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         baseURL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "gpt-5",
		ConfiguredModel:    "openai/gpt-5",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: "hello"},
		},
	}
}
