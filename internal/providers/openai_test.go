package providers

import (
	"context"
	"encoding/json"
	"fmt"
	searchtypes "llmcord-go/internal/searchtypes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	testOpenAIAuthHeader            = "Bearer test-key"
	testOpenAIClientRequestID       = "discord-message-1"
	testOpenAIPromptCacheKey        = "openai-session-123"
	testOpenAIAPIVersion            = "2024-12-01-preview"
	testOpenAIResponsesPath         = "/v1/responses"
	testOpenAIResponsesResponseID   = "resp_test_123"
	testOpenAIResponsesSystemPrompt = "You are concise."
	testOpenAIResponsesVisionPrompt = "What is in this image?"
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
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":34}}\n\n",
		)
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

	if request.URL.Query().Get("api-version") != testOpenAIAPIVersion {
		t.Fatalf("unexpected query string: %s", request.URL.RawQuery)
	}

	if request.Header.Get("Authorization") != testOpenAIAuthHeader {
		t.Fatalf("unexpected authorization header: %q", request.Header.Get("Authorization"))
	}

	if request.Header.Get("X-Test") != testHeaderPresent {
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

	streamOptions, streamOptionsOK := payload["stream_options"].(map[string]any)
	if !streamOptionsOK {
		t.Fatalf("unexpected stream_options Payload: %#v", payload["stream_options"])
	}

	if got, ok := streamOptions["include_usage"].(bool); !ok || !got {
		t.Fatalf("unexpected include_usage Payload: %#v", streamOptions["include_usage"])
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
	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         server.URL + "/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders: map[string]any{
				"X-Test": testHeaderPresent,
			},
			ExtraQuery: map[string]any{
				"api-version": testOpenAIAPIVersion,
			},
			ExtraBody: map[string]any{
				"temperature": 0.2,
			},
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages: []ChatMessage{
			{Role: "user", Content: "hello"},
		},
	}

	var (
		joinedContent strings.Builder
		finishReason  string
	)

	err := client.streamChatCompletion(context.Background(), request, func(delta StreamDelta) error {
		joinedContent.WriteString(delta.Content)

		if delta.FinishReason != "" {
			finishReason = delta.FinishReason
		}

		return nil
	})
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if joinedContent.String() != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", joinedContent.String())
	}

	if finishReason != "stop" {
		t.Fatalf("unexpected finish reason: %q", finishReason)
	}
}

func TestBuildChatCompletionRequestBodyAddsPlaceholderForImageOnlyUserMessage(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages: []ChatMessage{{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": ""},
				{
					"type":      searchtypes.ContentTypeImageURL,
					"image_url": map[string]string{"url": "data:image/png;base64,abc"},
				},
			},
		}},
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 1 {
		t.Fatalf("unexpected messages Payload: %#v", requestBody["messages"])
	}

	parts, partsOK := messages[0].Content.([]ContentPart)
	if !partsOK || len(parts) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", messages[0].Content)
	}

	if parts[0]["type"] != searchtypes.ContentTypeText || parts[0]["text"] != FileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected placeholder text part: %#v", parts[0])
	}

	originalParts, originalPartsOK := request.Messages[0].Content.([]ContentPart)
	if !originalPartsOK || originalParts[0]["text"] != "" {
		t.Fatalf("expected original request messages to remain unchanged: %#v", request.Messages[0].Content)
	}
}

func TestBuildChatCompletionRequestBodyAddsPlaceholderForDocumentOnlyUserMessage(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages: []ChatMessage{{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": ""},
				{
					"type":                           searchtypes.ContentTypeDocument,
					searchtypes.ContentFieldBytes:    []byte("document-bytes"),
					searchtypes.ContentFieldMIMEType: searchtypes.MimeTypePDF,
					searchtypes.ContentFieldFilename: testPDFFilename,
				},
			},
		}},
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 1 {
		t.Fatalf("unexpected messages Payload: %#v", requestBody["messages"])
	}

	parts, partsOK := messages[0].Content.([]ContentPart)
	if !partsOK || len(parts) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", messages[0].Content)
	}

	if parts[0]["type"] != searchtypes.ContentTypeText || parts[0]["text"] != FileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected placeholder text part: %#v", parts[0])
	}

	if parts[1]["type"] != searchtypes.ContentTypeDocument ||
		parts[1][searchtypes.ContentFieldFilename] != testPDFFilename {
		t.Fatalf("unexpected document part: %#v", parts[1])
	}
}

func TestBuildChatCompletionRequestBodyAddsPlaceholderForFileOnlyUserMessage(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages: []ChatMessage{{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": ""},
				{
					"type":                           searchtypes.ContentTypeFileData,
					searchtypes.ContentFieldBytes:    []byte("document-bytes"),
					searchtypes.ContentFieldMIMEType: searchtypes.MimeTypeOctetStream,
					searchtypes.ContentFieldFilename: "payload.bin",
				},
			},
		}},
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 1 {
		t.Fatalf("unexpected messages Payload: %#v", requestBody["messages"])
	}

	parts, partsOK := messages[0].Content.([]ContentPart)
	if !partsOK || len(parts) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", messages[0].Content)
	}

	if parts[0]["type"] != searchtypes.ContentTypeText || parts[0]["text"] != FileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected placeholder text part: %#v", parts[0])
	}

	if parts[1]["type"] != searchtypes.ContentTypeFileData || parts[1][searchtypes.ContentFieldFilename] != "payload.bin" {
		t.Fatalf("unexpected file part: %#v", parts[1])
	}
}

func TestBuildChatCompletionRequestBodyIncludesPromptCacheKeyForOpenAIProvider(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         testOpenAIBaseURL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "openai/gpt-test",
		SessionID:       testOpenAIPromptCacheKey,
		RequestID:       "",
		Tools:           nil,
		Messages:        []ChatMessage{{Role: searchtypes.MessageRoleUser, Content: "hello"}},
	}

	requestBody := buildChatCompletionRequestBody(request)

	if requestBody["prompt_cache_key"] != testOpenAIPromptCacheKey {
		t.Fatalf("unexpected prompt_cache_key: %#v", requestBody["prompt_cache_key"])
	}
}

func TestBuildChatCompletionRequestBodySkipsPromptCacheKeyForNonOpenAIProvider(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         testOpenAIBaseURL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "compatible/gpt-test",
		SessionID:       testOpenAIPromptCacheKey,
		RequestID:       "",
		Tools:           nil,
		Messages:        []ChatMessage{{Role: searchtypes.MessageRoleUser, Content: "hello"}},
	}

	requestBody := buildChatCompletionRequestBody(request)

	if _, exists := requestBody["prompt_cache_key"]; exists {
		t.Fatalf("unexpected prompt_cache_key: %#v", requestBody["prompt_cache_key"])
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
	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         server.URL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages:        []ChatMessage{{Role: "user", Content: "hello"}},
	}

	err := client.streamChatCompletion(context.Background(), request, func(StreamDelta) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected non-2xx status to return an error")
	}

	if !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestOpenAIClientStreamChatCompletionParsesJSONStatusErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.Header().Set("Content-Type", "application/json")
		responseWriter.WriteHeader(http.StatusBadRequest)
		writeStreamChunk(
			t,
			responseWriter,
			`{"error":{"message":"Unsupported response_format","type":"invalid_request_error",`+
				`"param":"response_format","code":"unsupported_parameter"}}`,
		)
	}))
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         server.URL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages:        []ChatMessage{{Role: "user", Content: "hello"}},
	}

	err := client.streamChatCompletion(context.Background(), request, func(StreamDelta) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected JSON status error")
	}

	for _, fragment := range []string{
		"status 400",
		"Unsupported response_format",
		"code=unsupported_parameter",
		"type=invalid_request_error",
		"param=response_format",
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("expected %q in error, got %v", fragment, err)
		}
	}
}

func TestOpenAIClientStreamChatCompletionReturnsStreamEventErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"error\":{\"message\":\"rate limited\",\"type\":\"server_error\",\"code\":\"rate_limit_exceeded\"}}\n\n",
		)
	}))
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         server.URL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages:        []ChatMessage{{Role: "user", Content: "hello"}},
	}

	err := client.streamChatCompletion(context.Background(), request, func(StreamDelta) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected stream event error")
	}

	if !strings.Contains(err.Error(), "rate limited") ||
		!strings.Contains(err.Error(), "rate_limit_exceeded") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestOpenAIClientStreamChatCompletionReturnsBlockedFinishReasonErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\n",
		)
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         server.URL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages:        []ChatMessage{{Role: "user", Content: "hello"}},
	}

	err := client.streamChatCompletion(context.Background(), request, func(StreamDelta) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected blocked finish reason error")
	}

	if !strings.Contains(err.Error(), "content_filter") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestOpenAIClientStreamChatCompletionReturnsErrorWithoutDoneMarker(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"choices\":[{\"delta\":{\"content\":\""+testStreamedHelloText+"\"}}]}\n\n",
		)
	}))
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         server.URL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages:        []ChatMessage{{Role: "user", Content: "hello"}},
	}

	err := client.streamChatCompletion(context.Background(), request, func(StreamDelta) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected missing [DONE] error")
	}

	if !strings.Contains(err.Error(), "before [DONE]") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func decodeOpenAIRequestBody(t *testing.T, request *http.Request) map[string]any {
	t.Helper()

	var payload map[string]any

	err := json.NewDecoder(request.Body).Decode(&payload)
	if err != nil {
		t.Fatalf("decode openai request body: %v", err)
	}

	return payload
}

func TestOpenAIClientStreamResponses(t *testing.T) {
	t.Parallel()

	server := newOpenAIResponsesStreamingTestServer(t)
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         server.URL + "/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders: map[string]any{
				"X-Test": testHeaderPresent,
			},
			ExtraQuery: nil,
			ExtraBody:  nil,
		},
		Model:           "gpt-5",
		ConfiguredModel: "openai/gpt-5",
		SessionID:       "",
		RequestID:       testOpenAIClientRequestID,
		Tools:           nil,
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: testOpenAIResponsesSystemPrompt},
			{
				Role: searchtypes.MessageRoleUser,
				Content: []ContentPart{
					{"type": searchtypes.ContentTypeText, "text": testOpenAIResponsesVisionPrompt},
					{"type": searchtypes.ContentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
				},
			},
		},
	}

	var (
		joinedContent      strings.Builder
		joinedThinking     strings.Builder
		finishReason       string
		providerResponseID string
	)

	err := client.streamChatCompletion(context.Background(), request, func(delta StreamDelta) error {
		joinedContent.WriteString(delta.Content)
		joinedThinking.WriteString(delta.Thinking)

		if delta.FinishReason != "" {
			finishReason = delta.FinishReason
		}

		if delta.ProviderResponseID != "" {
			providerResponseID = delta.ProviderResponseID
		}

		return nil
	})
	if err != nil {
		t.Fatalf("stream responses completion: %v", err)
	}

	if joinedContent.String() != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", joinedContent.String())
	}

	if joinedThinking.String() != "Inspecting...\n\nNeed more steps\n\nChecking raw-compatible event...\n\n" {
		t.Fatalf("unexpected streamed thinking: %q", joinedThinking.String())
	}

	if finishReason != finishReasonStop {
		t.Fatalf("unexpected finish reason: %q", finishReason)
	}

	if providerResponseID != testOpenAIResponsesResponseID {
		t.Fatalf("unexpected provider response id: %q", providerResponseID)
	}
}

func TestOpenAIClientStreamResponsesSuppressesStreamedReasoningSummary(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		name             string
		events           []string
		completedEvent   string
		expectedThinking string
	}{
		{
			name: "emits full summary without streamed deltas",
			events: []string{
				`{"type":"response.output_item.done","item":{"id":"rs_1",` +
					`"type":"reasoning","summary":[{"type":"summary_text",` +
					`"text":"Plan the search."}]}}`,
			},
			expectedThinking: "Plan the search.\n\n",
		},
		{
			name: "suppresses output_item.done restating streamed summary",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta",` +
					`"item_id":"rs_1","delta":"Plan the search."}`,
				`{"type":"response.reasoning_summary_part.done","item_id":"rs_1"}`,
				`{"type":"response.output_item.done","item":{"id":"rs_1",` +
					`"type":"reasoning","summary":[{"type":"summary_text",` +
					`"text":"Plan the search."}]}}`,
			},
			expectedThinking: "Plan the search.\n\n",
		},
		{
			name: "suppresses response.completed restating streamed summary",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta",` +
					`"item_id":"rs_1","delta":"Plan the search."}`,
				`{"type":"response.reasoning_summary_part.done","item_id":"rs_1"}`,
				`{"type":"response.completed","response":{"id":"` + testOpenAIResponsesResponseID +
					`","status":"completed","output":[{"id":"rs_1","type":"reasoning",` +
					`"summary":[{"type":"summary_text","text":"Plan the search."}]}]}}`,
			},
			expectedThinking: "Plan the search.\n\n",
		},
		{
			name: "emits remainder of summary extending streamed text",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta",` +
					`"item_id":"rs_1","delta":"Part one."}`,
				`{"type":"response.reasoning_summary_part.done","item_id":"rs_1"}`,
				`{"type":"response.output_item.done","item":{"id":"rs_1",` +
					`"type":"reasoning","summary":[{"type":"summary_text",` +
					`"text":"Part one."},{"type":"summary_text","text":"Part two."}]}}`,
			},
			expectedThinking: "Part one.\n\nPart two.\n\n",
		},
		{
			name: "keeps divergent summary text verbatim",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta",` +
					`"item_id":"rs_1","delta":"Inspecting..."}`,
				`{"type":"response.reasoning_summary_part.done","item_id":"rs_1"}`,
				`{"type":"response.output_item.done","item":{"id":"rs_1",` +
					`"type":"reasoning","summary":[{"type":"summary_text",` +
					`"text":"Need more steps"}]}}`,
			},
			expectedThinking: "Inspecting...\n\nNeed more steps\n\n",
		},
		{
			name: "suppresses restatement of fully streamed multi-part summary",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta",` +
					`"item_id":"rs_1","delta":"Part one."}`,
				`{"type":"response.reasoning_summary_part.done","item_id":"rs_1"}`,
				`{"type":"response.reasoning_summary_text.delta",` +
					`"item_id":"rs_1","summary_index":1,"delta":"Part two."}`,
				`{"type":"response.reasoning_summary_part.done","item_id":"rs_1"}`,
				`{"type":"response.output_item.done","item":{"id":"rs_1",` +
					`"type":"reasoning","summary":[{"type":"summary_text",` +
					`"text":"Part one."},{"type":"summary_text","text":"Part two."}]}}`,
			},
			expectedThinking: "Part one.\n\nPart two.\n\n",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()

			server := newOpenAIResponsesReasoningStreamTestServer(
				t,
				scenario.events,
				scenario.completedEvent,
			)
			defer server.Close()

			client := newOpenAIClient(server.Client())
			request := ChatCompletionRequest{
				Provider: ProviderRequestConfig{
					APIKind:         ProviderAPIKindOpenAI,
					BaseURL:         server.URL + "/v1",
					APIKey:          "test-key",
					UseResponsesAPI: true,
				},
				Model:           "gpt-5",
				ConfiguredModel: "openai/gpt-5",
				Messages: []ChatMessage{
					{Role: searchtypes.MessageRoleUser, Content: "hello"},
				},
			}

			var joinedThinking strings.Builder

			err := client.streamChatCompletion(context.Background(), request, func(delta StreamDelta) error {
				joinedThinking.WriteString(delta.Thinking)

				return nil
			})
			if err != nil {
				t.Fatalf("stream responses completion: %v", err)
			}

			if joinedThinking.String() != scenario.expectedThinking {
				t.Fatalf(
					"unexpected streamed thinking: %q want %q",
					joinedThinking.String(),
					scenario.expectedThinking,
				)
			}
		})
	}
}

func newOpenAIResponsesReasoningStreamTestServer(
	t *testing.T,
	events []string,
	completedEvent string,
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := responseWriter.(http.Flusher)
		if !ok {
			t.Fatal("expected response writer to support flushing")
		}

		for _, event := range events {
			writeStreamChunk(t, responseWriter, "data: "+event+"\n\n")
			flusher.Flush()
		}

		if completedEvent == "" {
			completedEvent = openAIResponsesCompletedChunk()
		}

		writeStreamChunk(t, responseWriter, completedEvent)
		flusher.Flush()

		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func TestSetOpenAIClientRequestIDHeaderUsesOpenAIProviderOnly(t *testing.T) {
	t.Parallel()

	openAIRequest, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		testOpenAIBaseURL+"/responses",
		nil,
	)
	if err != nil {
		t.Fatalf("create openai request: %v", err)
	}

	setOpenAIClientRequestIDHeader(
		openAIRequest,
		newOpenAIClientRequestIDTestRequest(testOpenAIBaseURL, "openai/gpt-5"),
	)

	if got := openAIRequest.Header.Get(openAIClientRequestIDHeader); got != testOpenAIClientRequestID {
		t.Fatalf("unexpected openai request id header: %q", got)
	}

	compatRequest, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		testOfficialOpenAIBaseURL+"/responses",
		nil,
	)
	if err != nil {
		t.Fatalf("create compatibility request: %v", err)
	}

	setOpenAIClientRequestIDHeader(
		compatRequest,
		newOpenAIClientRequestIDTestRequest(testOfficialOpenAIBaseURL, "compat/gpt-5"),
	)

	if got := compatRequest.Header.Get(openAIClientRequestIDHeader); got != "" {
		t.Fatalf("unexpected compatibility request id header: %q", got)
	}
}

func TestBuildOpenAIResponsesRequestBodyNormalizesReasoningConfig(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         testOpenAIBaseURL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody: map[string]any{
				"reasoning_effort":  OpenAIReasoningEffortMinimal,
				"reasoning_summary": OpenAIReasoningSummaryConcise,
			},
		},
		Model:           OpenAIReasoningModelGPT54,
		ConfiguredModel: "openai/gpt-5.4",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
	}

	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build responses request body: %v", err)
	}

	if _, exists := requestBody["reasoning_effort"]; exists {
		t.Fatalf("unexpected top-level reasoning_effort: %#v", requestBody["reasoning_effort"])
	}

	if _, exists := requestBody["reasoning_summary"]; exists {
		t.Fatalf("unexpected top-level reasoning_summary: %#v", requestBody["reasoning_summary"])
	}

	reasoningConfig, ok := requestBody["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected reasoning config type: %T", requestBody["reasoning"])
	}

	if reasoningConfig["effort"] != OpenAIReasoningEffortLow {
		t.Fatalf("unexpected reasoning effort: %#v", reasoningConfig["effort"])
	}

	if reasoningConfig["summary"] != OpenAIReasoningSummaryConcise {
		t.Fatalf("unexpected reasoning summary: %#v", reasoningConfig["summary"])
	}

	if request.Provider.ExtraBody["reasoning_effort"] != OpenAIReasoningEffortMinimal {
		t.Fatalf("unexpected mutation of original reasoning config: %#v", request.Provider.ExtraBody)
	}
}

func TestBuildOpenAIResponsesRequestBodyDefaultsReasoningSummary(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         testOpenAIBaseURL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           OpenAIReasoningModelGPT54,
		ConfiguredModel: "openai/gpt-5.4",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
	}

	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build responses request body: %v", err)
	}

	reasoningConfig, ok := requestBody["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected reasoning config type: %T", requestBody["reasoning"])
	}

	if reasoningConfig["summary"] != OpenAIReasoningSummaryAuto {
		t.Fatalf("unexpected reasoning summary: %#v", reasoningConfig["summary"])
	}
}

func TestBuildOpenAIResponsesRequestBodyIncludesPromptCacheKeyForOpenAIProvider(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         testOpenAIBaseURL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-5",
		ConfiguredModel: "openai/gpt-5",
		SessionID:       testOpenAIPromptCacheKey,
		RequestID:       "",
		Tools:           nil,
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
	}

	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build responses request body: %v", err)
	}

	if requestBody["prompt_cache_key"] != testOpenAIPromptCacheKey {
		t.Fatalf("unexpected prompt_cache_key: %#v", requestBody["prompt_cache_key"])
	}
}

func newOpenAIClientRequestIDTestRequest(
	baseURL string,
	configuredModel string,
) ChatCompletionRequest {
	return ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         baseURL,
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "",
		ConfiguredModel: configuredModel,
		SessionID:       "",
		RequestID:       testOpenAIClientRequestID,
		Tools:           nil,
		Messages:        nil,
	}
}

func newOpenAIResponsesStreamingTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		assertOpenAIResponsesRequest(t, request)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := responseWriter.(http.Flusher)
		if !ok {
			t.Fatal("expected response writer to support flushing")
		}

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Inspecting...\"}\n\n",
		)
		flusher.Flush()
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.reasoning_summary_part.done\"}\n\n",
		)
		flusher.Flush()
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\","+
				"\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\","+
				"\"text\":\"Need more steps\"}]}}\n\n",
		)
		flusher.Flush()
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"Checking raw-compatible event...\"}\n\n",
		)
		flusher.Flush()
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.reasoning_text.done\"}\n\n",
		)
		flusher.Flush()
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n",
		)
		flusher.Flush()
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n",
		)
		flusher.Flush()
		writeStreamChunk(
			t,
			responseWriter,
			openAIResponsesCompletedChunk(),
		)
		flusher.Flush()
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func openAIResponsesCompletedChunk() string {
	return "data: {\"type\":\"response.completed\",\"response\":{" +
		"\"id\":\"" + testOpenAIResponsesResponseID + "\"," +
		"\"status\":\"completed\"," +
		"\"usage\":{\"input_tokens\":12,\"output_tokens\":34}}}\n\n"
}

func assertOpenAIResponsesRequest(t *testing.T, request *http.Request) {
	t.Helper()

	assertOpenAIResponsesRequestHeaders(t, request)
	payload := decodeOpenAIRequestBody(t, request)
	assertOpenAIResponsesRequestPayload(t, payload)
}

func assertOpenAIResponsesRequestHeaders(t *testing.T, request *http.Request) {
	t.Helper()

	if request.URL.Path != testOpenAIResponsesPath {
		t.Fatalf("unexpected path: %s", request.URL.Path)
	}

	if request.Header.Get("Authorization") != "Bearer test-key" {
		t.Fatalf("unexpected authorization header: %q", request.Header.Get("Authorization"))
	}

	if request.Header.Get("X-Test") != testHeaderPresent {
		t.Fatalf("unexpected extra header: %q", request.Header.Get("X-Test"))
	}

	if request.Header.Get(openAIClientRequestIDHeader) != testOpenAIClientRequestID {
		t.Fatalf(
			"unexpected request id header: %q",
			request.Header.Get(openAIClientRequestIDHeader),
		)
	}
}

func assertOpenAIResponsesRequestPayload(t *testing.T, payload map[string]any) {
	t.Helper()

	if payload["model"] != "gpt-5" {
		t.Fatalf("unexpected model: %#v", payload["model"])
	}

	if payload["stream"] != true {
		t.Fatalf("unexpected stream flag: %#v", payload["stream"])
	}

	if _, exists := payload["source_attribution"]; exists {
		t.Fatalf("did not expect request-level source attribution: %#v", payload["source_attribution"])
	}

	inputPayload, inputOK := payload["input"].([]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input Payload: %#v", payload["input"])
	}

	assertOpenAIResponsesSystemMessage(t, inputPayload[0])
	assertOpenAIResponsesUserMessage(t, inputPayload[1])
}

func assertOpenAIResponsesSystemMessage(t *testing.T, rawMessage any) {
	t.Helper()

	systemMessage, systemOK := rawMessage.(map[string]any)
	if !systemOK {
		t.Fatalf("unexpected system message Payload: %#v", rawMessage)
	}

	if systemMessage["role"] != searchtypes.MessageRoleSystem ||
		systemMessage["content"] != testOpenAIResponsesSystemPrompt {
		t.Fatalf("unexpected system message: %#v", systemMessage)
	}
}

func assertOpenAIResponsesUserMessage(t *testing.T, rawMessage any) {
	t.Helper()

	userMessage, userOK := rawMessage.(map[string]any)
	if !userOK {
		t.Fatalf("unexpected user message Payload: %#v", rawMessage)
	}

	userContent, contentOK := userMessage["content"].([]any)
	if !contentOK || len(userContent) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", userMessage["content"])
	}

	firstPart, firstPartOK := userContent[0].(map[string]any)
	if !firstPartOK {
		t.Fatalf("unexpected first user content part: %#v", userContent[0])
	}

	if firstPart["type"] != responsesInputTextType ||
		firstPart["text"] != testOpenAIResponsesVisionPrompt {
		t.Fatalf("unexpected first user content part: %#v", firstPart)
	}

	secondPart, secondPartOK := userContent[1].(map[string]any)
	if !secondPartOK {
		t.Fatalf("unexpected second user content part: %#v", userContent[1])
	}

	if secondPart["type"] != responsesInputImageType {
		t.Fatalf("unexpected second user content part: %#v", secondPart)
	}

	if secondPart["image_url"] != "data:image/png;base64,abc" {
		t.Fatalf("unexpected image_url: %#v", secondPart["image_url"])
	}

	if secondPart["detail"] != responsesImageDetailAuto {
		t.Fatalf("unexpected image detail: %#v", secondPart["detail"])
	}
}

func TestOpenAIClientStreamChatCompletionSucceedsWithoutDoneIfChoicesSeen(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		responseWriter.WriteHeader(http.StatusOK)
		writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"success\"}}]}\n\n")
	}))
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         server.URL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "9router/unli_free",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages:        nil,
	}

	var responseText strings.Builder

	err := client.streamChatCompletion(context.Background(), request, func(delta StreamDelta) error {
		responseText.WriteString(delta.Content)

		return nil
	})
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	if responseText.String() != "success" {
		t.Fatalf("expected response 'success', got: %q", responseText.String())
	}
}

type authHeaderTestCase struct {
	name            string
	baseURL         string
	configuredModel string
	apiKey          string
	expectedAuth    string
	expectAuthSent  bool
}

func TestOpenAIClientAuthorizationHeaderFor9Router(t *testing.T) {
	t.Parallel()

	tests := []authHeaderTestCase{
		{
			name:            "9router request with empty API key",
			baseURL:         "http://localhost:20128/v1",
			configuredModel: "9router/openai/gpt-5",
			apiKey:          "",
			expectedAuth:    "",
			expectAuthSent:  false,
		},
		{
			name:            "9router request with non-empty API key",
			baseURL:         "http://localhost:20128/v1",
			configuredModel: "9router/openai/gpt-5",
			apiKey:          "sk-test-key",
			expectedAuth:    "Bearer sk-test-key",
			expectAuthSent:  true,
		},
		{
			name:            "Non-9router request with empty API key",
			baseURL:         "https://api.openai.com/v1",
			configuredModel: "openai/gpt-4",
			apiKey:          "",
			expectedAuth:    "Bearer sk-no-key-required",
			expectAuthSent:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runOpenAIClientAuthorizationHeaderFor9RouterSubtest(t, tt)
		})
	}
}

func runOpenAIClientAuthorizationHeaderFor9RouterSubtest(t *testing.T, testCase authHeaderTestCase) {
	t.Helper()

	var (
		capturedAuth string
		authSent     bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		httpRequest *http.Request,
	) {
		capturedAuth = httpRequest.Header.Get("Authorization")
		_, authSent = httpRequest.Header["Authorization"]

		responseWriter.Header().Set("Content-Type", "text/event-stream")
		responseWriter.WriteHeader(http.StatusOK)
		writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"success\"}}]}\n\n")
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newOpenAIClient(server.Client())
	baseURL := testCase.baseURL

	u, err := url.Parse(testCase.baseURL)
	if err == nil {
		serverURL, _ := url.Parse(server.URL)
		serverURL.Path = u.Path
		baseURL = serverURL.String()
	}

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         baseURL,
			APIKey:          testCase.apiKey,
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: testCase.configuredModel,
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages:        nil,
	}

	err = client.streamChatCompletion(context.Background(), request, func(_ StreamDelta) error {
		return nil
	})
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	if authSent != testCase.expectAuthSent {
		t.Fatalf("expected auth sent to be %t, got %t", testCase.expectAuthSent, authSent)
	}

	if testCase.expectAuthSent && capturedAuth != testCase.expectedAuth {
		t.Fatalf("expected auth header %q, got %q", testCase.expectedAuth, capturedAuth)
	}
}

func TestOpenAIClientStreamChatCompletionDefaultsServiceTierToPriority(t *testing.T) {
	t.Parallel()

	var capturedServiceTier string

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, httpRequest *http.Request) {
		var payload map[string]any

		_ = json.NewDecoder(httpRequest.Body).Decode(&payload)

		if st, ok := payload["service_tier"].(string); ok {
			capturedServiceTier = st
		}

		responseWriter.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	client := newOpenAIClient(server.Client())
	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         server.URL,
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-4o",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages:        nil,
	}

	err := client.streamChatCompletion(context.Background(), request, func(_ StreamDelta) error {
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	if capturedServiceTier != "priority" {
		t.Fatalf("expected service_tier priority, got %q", capturedServiceTier)
	}
}

func TestOpenAIClientStreamChatCompletionPreservesCustomServiceTier(t *testing.T) {
	t.Parallel()

	var capturedServiceTier string

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, httpRequest *http.Request) {
		var payload map[string]any

		_ = json.NewDecoder(httpRequest.Body).Decode(&payload)

		if st, ok := payload["service_tier"].(string); ok {
			capturedServiceTier = st
		}

		responseWriter.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	client := newOpenAIClient(server.Client())
	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         server.URL,
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody: map[string]any{
				"service_tier": "standard",
			},
		},
		Model:           "gpt-4o",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages:        nil,
	}

	err := client.streamChatCompletion(context.Background(), request, func(_ StreamDelta) error {
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	if capturedServiceTier != "standard" {
		t.Fatalf("expected service_tier standard, got %q", capturedServiceTier)
	}
}

func TestOpenAINormalizeRequestMessages(t *testing.T) {
	t.Parallel()

	messages := []ChatMessage{
		{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": "Describe this image:"},
				{
					"type":      searchtypes.ContentTypeImageURL,
					"image_url": map[string]string{"url": "https://example.com/test.png"},
				},
				{
					"type": searchtypes.ContentTypeImageURL,
					"image_url": map[string]string{
						"url":    "https://example.com/test2.png",
						"detail": "high",
					},
				},
				{
					"type":      searchtypes.ContentTypeImageURL,
					"image_url": "https://example.com/test3.png",
				},
			},
		},
	}

	normalized := openAINormalizeRequestMessages(messages)
	if len(normalized) != 1 {
		t.Fatalf("expected 1 message, got %d", len(normalized))
	}

	parts, partsOK := normalized[0].Content.([]ContentPart)
	if !partsOK || len(parts) != 4 {
		t.Fatalf("unexpected content parts: %#v", normalized[0].Content)
	}

	image1, ok1 := parts[1]["image_url"].(map[string]string)
	if !ok1 || image1["url"] != "https://example.com/test.png" || image1["detail"] != "auto" {
		t.Fatalf("unexpected image 1 normalized image_url: %#v", parts[1]["image_url"])
	}

	image2, ok2 := parts[2]["image_url"].(map[string]string)
	if !ok2 || image2["url"] != "https://example.com/test2.png" || image2["detail"] != "high" {
		t.Fatalf("unexpected image 2 normalized image_url: %#v", parts[2]["image_url"])
	}

	image3, ok3 := parts[3]["image_url"].(map[string]string)
	if !ok3 || image3["url"] != "https://example.com/test3.png" || image3["detail"] != "auto" {
		t.Fatalf("unexpected image 3 normalized image_url: %#v", parts[3]["image_url"])
	}
}

func TestBuildChatCompletionRequestBodyNormalizesImagesForOpenAICompatible(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "https://api.openrouter.ai/api/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "anthropic/claude-3.5-sonnet",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Tools:           nil,
		Messages: []ChatMessage{
			{
				Role: searchtypes.MessageRoleUser,
				Content: []ContentPart{
					{"type": searchtypes.ContentTypeText, "text": "Analyze image:"},
					{
						"type":      searchtypes.ContentTypeImageURL,
						"image_url": map[string]string{"url": "data:image/png;base64,abc"},
					},
				},
			},
		},
	}

	body := buildChatCompletionRequestBody(request)

	messages, ok := body["messages"].([]ChatMessage)
	if !ok || len(messages) != 1 {
		t.Fatalf("unexpected messages in body: %#v", body["messages"])
	}

	parts, partsOK := messages[0].Content.([]ContentPart)
	if !partsOK || len(parts) != 2 {
		t.Fatalf("unexpected parts in content: %#v", messages[0].Content)
	}

	imagePart, imageOK := parts[1]["image_url"].(map[string]string)
	if !imageOK || imagePart["url"] != "data:image/png;base64,abc" || imagePart["detail"] != "auto" {
		t.Fatalf("expected OpenAI-compatible provider request to include detail: auto, got: %#v", parts[1]["image_url"])
	}
}

const testOpenAIBaseURL = "https://api.example.com/v1"

const testOfficialOpenAIBaseURL = "https://api.openai.com/v1"
