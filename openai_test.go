package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newStreamingTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		assertStreamingRequest(t, request)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := responseWriter.(http.Flusher)
		if !ok {
			t.Fatal("expected response writer to support flushing")
		}

		writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		flusher.Flush()
		writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		flusher.Flush()
		writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		flusher.Flush()
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func assertStreamingRequest(t *testing.T, request *http.Request) {
	t.Helper()

	if request.URL.Path != "/v1/chat/completions" {
		t.Fatalf("unexpected path: %s", request.URL.Path)
	}

	if request.URL.Query().Get("api-version") != "2024-12-01-preview" {
		t.Fatalf("unexpected query string: %s", request.URL.RawQuery)
	}

	if request.Header.Get("Authorization") != "Bearer test-key" {
		t.Fatalf("unexpected authorization header: %q", request.Header.Get("Authorization"))
	}

	if request.Header.Get("X-Test") != "present" {
		t.Fatalf("unexpected extra header: %q", request.Header.Get("X-Test"))
	}

	var payload map[string]any

	err := json.NewDecoder(request.Body).Decode(&payload)
	if err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	if payload["model"] != "gpt-test" {
		t.Fatalf("unexpected model: %#v", payload["model"])
	}

	if payload["stream"] != true {
		t.Fatalf("unexpected stream flag: %#v", payload["stream"])
	}

	if payload["temperature"] != float64(0.2) {
		t.Fatalf("unexpected temperature: %#v", payload["temperature"])
	}
}

func writeStreamChunk(
	t *testing.T,
	responseWriter http.ResponseWriter,
	content string,
) {
	t.Helper()

	_, err := fmt.Fprint(responseWriter, content)
	if err != nil {
		t.Fatalf("write stream chunk: %v", err)
	}
}

func TestOpenAIClientStreamChatCompletion(t *testing.T) {
	t.Parallel()

	server := newStreamingTestServer(t)
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := chatCompletionRequest{
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		Model:   "gpt-test",
		Messages: []chatMessage{
			{Role: "user", Content: "hello"},
		},
		ExtraHeaders: map[string]any{
			"X-Test": "present",
		},
		ExtraQuery: map[string]any{
			"api-version": "2024-12-01-preview",
		},
		ExtraBody: map[string]any{
			"temperature": 0.2,
		},
	}

	var (
		joinedContent strings.Builder
		finishReason  string
	)

	err := client.streamChatCompletion(context.Background(), request, func(delta streamDelta) error {
		joinedContent.WriteString(delta.Content)

		if delta.FinishReason != "" {
			finishReason = delta.FinishReason
		}

		return nil
	})
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if joinedContent.String() != "Hello" {
		t.Fatalf("unexpected streamed content: %q", joinedContent.String())
	}

	if finishReason != "stop" {
		t.Fatalf("unexpected finish reason: %q", finishReason)
	}
}

func TestOpenAIClientStreamChatCompletionReturnsStatusErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		http.Error(responseWriter, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := chatCompletionRequest{
		BaseURL:      server.URL,
		APIKey:       "test-key",
		Model:        "gpt-test",
		Messages:     []chatMessage{{Role: "user", Content: "hello"}},
		ExtraHeaders: nil,
		ExtraQuery:   nil,
		ExtraBody:    nil,
	}

	err := client.streamChatCompletion(context.Background(), request, func(streamDelta) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected non-2xx status to return an error")
	}

	if !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("unexpected error text: %v", err)
	}
}
