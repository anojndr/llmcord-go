package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

const testOpenAIDegradedFunctionID = "degraded-function-id"

type openAIRequestBodyCapture struct {
	mutex         sync.Mutex
	requestBodies []map[string]any
}

func (capture *openAIRequestBodyCapture) append(requestBody map[string]any) int {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()

	capture.requestBodies = append(capture.requestBodies, requestBody)

	return len(capture.requestBodies)
}

func (capture *openAIRequestBodyCapture) snapshot() []map[string]any {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()

	return append([]map[string]any(nil), capture.requestBodies...)
}

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
		Provider: providerRequestConfig{
			APIKind: providerAPIKindOpenAI,
			BaseURL: server.URL + "/v1",
			APIKey:  "test-key",
			APIKeys: nil,
			ExtraHeaders: map[string]any{
				"X-Test": "present",
			},
			ExtraQuery: map[string]any{
				"api-version": "2024-12-01-preview",
			},
			ExtraBody: map[string]any{
				"temperature": 0.2,
			},
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		Messages: []chatMessage{
			{Role: "user", Content: "hello"},
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
		Provider: providerRequestConfig{
			APIKind:      providerAPIKindOpenAI,
			BaseURL:      server.URL,
			APIKey:       "test-key",
			APIKeys:      nil,
			ExtraHeaders: nil,
			ExtraQuery:   nil,
			ExtraBody:    nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		Messages:        []chatMessage{{Role: "user", Content: "hello"}},
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
	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:      providerAPIKindOpenAI,
			BaseURL:      server.URL,
			APIKey:       "test-key",
			APIKeys:      nil,
			ExtraHeaders: nil,
			ExtraQuery:   nil,
			ExtraBody:    nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		Messages:        []chatMessage{{Role: "user", Content: "hello"}},
	}

	err := client.streamChatCompletion(context.Background(), request, func(streamDelta) error {
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
	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:      providerAPIKindOpenAI,
			BaseURL:      server.URL,
			APIKey:       "test-key",
			APIKeys:      nil,
			ExtraHeaders: nil,
			ExtraQuery:   nil,
			ExtraBody:    nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		Messages:        []chatMessage{{Role: "user", Content: "hello"}},
	}

	err := client.streamChatCompletion(context.Background(), request, func(streamDelta) error {
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
		writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
	}))
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:      providerAPIKindOpenAI,
			BaseURL:      server.URL,
			APIKey:       "test-key",
			APIKeys:      nil,
			ExtraHeaders: nil,
			ExtraQuery:   nil,
			ExtraBody:    nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		Messages:        []chatMessage{{Role: "user", Content: "hello"}},
	}

	err := client.streamChatCompletion(context.Background(), request, func(streamDelta) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected missing [DONE] error")
	}

	if !strings.Contains(err.Error(), "before [DONE]") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestOpenAIClientStreamChatCompletionRetriesWithoutDegradedFunctions(t *testing.T) {
	t.Parallel()

	capture := new(openAIRequestBodyCapture)

	server := newOpenAIDegradedFunctionRetryServer(t, capture)
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := newOpenAIDegradedFunctionRetryRequest(server.URL)

	joinedContent, finishReason, err := collectOpenAIStreamResult(
		context.Background(),
		client,
		request,
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	assertOpenAIDegradedFunctionRetryRequests(t, capture.snapshot())

	if joinedContent != "Hello" {
		t.Fatalf("unexpected streamed content: %q", joinedContent)
	}

	if finishReason != finishReasonStop {
		t.Fatalf("unexpected finish reason: %q", finishReason)
	}
}

func newOpenAIDegradedFunctionRetryServer(
	t *testing.T,
	capture *openAIRequestBodyCapture,
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		requestCount := capture.append(decodeOpenAIRequestBody(t, request))

		switch requestCount {
		case 1:
			http.Error(
				responseWriter,
				fmt.Sprintf(
					`{"status":400,"title":"Bad Request","detail":"Function id '%s': DEGRADED function cannot be invoked"}`,
					testOpenAIDegradedFunctionID,
				),
				http.StatusBadRequest,
			)
		case 2:
			streamOpenAIHello(t, responseWriter)
		default:
			t.Fatalf("unexpected request count: %d", requestCount)
		}
	}))
}

func newOpenAIDegradedFunctionRetryRequest(baseURL string) chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:      providerAPIKindOpenAI,
			BaseURL:      baseURL,
			APIKey:       "test-key",
			APIKeys:      nil,
			ExtraHeaders: nil,
			ExtraQuery:   nil,
			ExtraBody: map[string]any{
				"tools": []map[string]any{
					{
						"type": "function",
						"function": map[string]any{
							"id":   "healthy-function-id",
							"name": "healthy_function",
						},
					},
					{
						"type": "function",
						"function": map[string]any{
							"id":   testOpenAIDegradedFunctionID,
							"name": "degraded_function",
						},
					},
				},
				"tool_choice": map[string]any{
					"type": "function",
					"function": map[string]any{
						"id": testOpenAIDegradedFunctionID,
					},
				},
			},
		},
		Model:           "gpt-test",
		ConfiguredModel: "openai/gpt-test",
		Messages:        []chatMessage{{Role: messageRoleUser, Content: "hello"}},
	}
}

func collectOpenAIStreamResult(
	ctx context.Context,
	client openAIClient,
	request chatCompletionRequest,
) (string, string, error) {
	var (
		joinedContent strings.Builder
		finishReason  string
	)

	err := client.streamChatCompletion(ctx, request, func(delta streamDelta) error {
		joinedContent.WriteString(delta.Content)

		if delta.FinishReason != "" {
			finishReason = delta.FinishReason
		}

		return nil
	})

	return joinedContent.String(), finishReason, err
}

func assertOpenAIDegradedFunctionRetryRequests(t *testing.T, requestBodies []map[string]any) {
	t.Helper()

	if len(requestBodies) != 2 {
		t.Fatalf("unexpected request count: %d", len(requestBodies))
	}

	firstToolIDs := openAIRequestToolIDs(t, requestBodies[0])
	if !slices.Equal(firstToolIDs, []string{"healthy-function-id", testOpenAIDegradedFunctionID}) {
		t.Fatalf("unexpected initial tool ids: %#v", firstToolIDs)
	}

	secondToolIDs := openAIRequestToolIDs(t, requestBodies[1])
	if !slices.Equal(secondToolIDs, []string{"healthy-function-id"}) {
		t.Fatalf("unexpected retried tool ids: %#v", secondToolIDs)
	}

	if _, exists := requestBodies[1]["tool_choice"]; exists {
		t.Fatalf("expected retried request to drop degraded tool_choice: %#v", requestBodies[1]["tool_choice"])
	}
}

func TestExcludeDegradedFunctionsFromChatCompletionRequestBody(t *testing.T) {
	t.Parallel()

	requestBody := openAIDegradedFunctionRequestBodyFixture()

	sanitizedBody, changed := excludeDegradedFunctionsFromChatCompletionRequestBody(
		requestBody,
		map[string]struct{}{
			"degraded-tool-id":     {},
			"degraded-function-id": {},
		},
	)
	if !changed {
		t.Fatal("expected degraded functions to be removed")
	}

	if len(openAIRequestToolIDs(t, requestBody)) != 2 {
		t.Fatalf("unexpected mutation of original tools: %#v", requestBody["tools"])
	}

	if !slices.Equal(openAIRequestToolIDs(t, sanitizedBody), []string{"healthy-tool-id"}) {
		t.Fatalf("unexpected sanitized tools: %#v", sanitizedBody["tools"])
	}

	functionIDs := openAIRequestFunctionIDs(t, sanitizedBody)
	if !slices.Equal(functionIDs, []string{"healthy-function-id"}) {
		t.Fatalf("unexpected sanitized functions: %#v", functionIDs)
	}

	if _, exists := sanitizedBody["tool_choice"]; exists {
		t.Fatalf("expected degraded tool_choice to be removed: %#v", sanitizedBody["tool_choice"])
	}

	if _, exists := sanitizedBody["function_call"]; exists {
		t.Fatalf("expected degraded function_call to be removed: %#v", sanitizedBody["function_call"])
	}
}

func openAIDegradedFunctionRequestBodyFixture() map[string]any {
	return map[string]any{
		"model":  "gpt-test",
		"stream": true,
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"id":   "healthy-tool-id",
					"name": "healthy_tool",
				},
			},
			{
				"type": "function",
				"function": map[string]any{
					"id":   "degraded-tool-id",
					"name": "degraded_tool",
				},
			},
		},
		"tool_choice": map[string]any{
			"type": "function",
			"function": map[string]any{
				"id": "degraded-tool-id",
			},
		},
		"functions": []map[string]any{
			{"id": "healthy-function-id", "name": "healthy_function"},
			{"id": "degraded-function-id", "name": "degraded_function"},
		},
		"function_call": map[string]any{
			"id": "degraded-function-id",
		},
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

func openAIRequestToolIDs(t *testing.T, requestBody map[string]any) []string {
	t.Helper()

	rawTools, toolsExist := requestBody["tools"]
	if !toolsExist {
		return nil
	}

	toolValues, sliceOK := openAIRequestValueSlice(rawTools)
	if !sliceOK {
		t.Fatalf("unexpected tools type: %T", rawTools)
	}

	toolIDs := make([]string, 0, len(toolValues))
	for _, rawTool := range toolValues {
		tool, mapOK := openAIRequestValueMap(rawTool)
		if !mapOK {
			t.Fatalf("unexpected tool type: %T", rawTool)
		}

		function, functionOK := openAIRequestValueMap(tool["function"])
		if !functionOK {
			t.Fatalf("unexpected tool function type: %T", tool["function"])
		}

		toolID, _ := function["id"].(string)
		toolIDs = append(toolIDs, toolID)
	}

	return toolIDs
}

func openAIRequestFunctionIDs(t *testing.T, requestBody map[string]any) []string {
	t.Helper()

	rawFunctions, functionsExist := requestBody["functions"]
	if !functionsExist {
		return nil
	}

	functionValues, sliceOK := openAIRequestValueSlice(rawFunctions)
	if !sliceOK {
		t.Fatalf("unexpected functions type: %T", rawFunctions)
	}

	functionIDs := make([]string, 0, len(functionValues))
	for _, rawFunction := range functionValues {
		function, mapOK := openAIRequestValueMap(rawFunction)
		if !mapOK {
			t.Fatalf("unexpected function type: %T", rawFunction)
		}

		functionID, _ := function["id"].(string)
		functionIDs = append(functionIDs, functionID)
	}

	return functionIDs
}
