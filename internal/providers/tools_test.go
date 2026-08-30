package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// readAllAndClose drains and closes a request body, returning its bytes.
func readAllAndClose(request *http.Request) ([]byte, error) {
	defer func() {
		_ = request.Body.Close()
	}()

	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}

	return body, nil
}

func testWebSearchTools() []FunctionTool {
	return []FunctionTool{
		WebSearchTool(3),
	}
}

func newToolCallTestServer(
	t *testing.T,
	handler func(responseWriter http.ResponseWriter, request *http.Request),
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(handler))
}

func writeSSE(t *testing.T, responseWriter http.ResponseWriter, payload string) {
	t.Helper()

	flusher, flusherOK := responseWriter.(http.Flusher)
	if !flusherOK {
		t.Fatal("expected response writer to support flushing")
	}

	writeStreamChunk(t, responseWriter, payload)
	flusher.Flush()
}

func chatCompletionsToolCallChunk(index int, callID string, toolName string, arguments string) string {
	function := map[string]any{}

	if toolName != "" {
		function["name"] = toolName
	}

	if arguments != "" {
		function["arguments"] = arguments
	}

	toolCall := map[string]any{"index": index}

	if callID != "" {
		toolCall["id"] = callID
	}

	if len(function) > 0 {
		toolCall["function"] = function
	}

	chunk := map[string]any{
		"choices": []map[string]any{{
			"delta": map[string]any{
				"tool_calls": []any{toolCall},
			},
		}},
	}

	payload, err := json.Marshal(chunk)
	if err != nil {
		panic(err)
	}

	return "data: " + string(payload) + "\n\n"
}

func chatCompletionsFinishChunk(finishReason string) string {
	return `data: {"choices":[{"delta":{},"finish_reason":"` + finishReason + `"}]}` + "\n\n"
}

func chatCompletionsContentChunk(content string) string {
	return `data: {"choices":[{"delta":{"content":"` + content + `"}}]}` + "\n\n"
}

// responsesOutputItemChunk renders one Responses stream output item event.
func responsesOutputItemChunk(itemType string, item map[string]any) string {
	payload, err := json.Marshal(map[string]any{
		"type":         itemType,
		"output_index": 0,
		"item":         item,
	})
	if err != nil {
		panic(err)
	}

	return "data: " + string(payload) + "\n\n"
}

func responsesFunctionCallItem(
	itemID string,
	callID string,
	status string,
	arguments string,
) map[string]any {
	return map[string]any{
		"id":        itemID,
		"type":      "function_call",
		"status":    status,
		"call_id":   callID,
		"name":      WebSearchToolName,
		"arguments": arguments,
	}
}

func toolStreamRequest(serverURL string, useResponses bool) ChatCompletionRequest {
	return ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			API:             "",
			BaseURL:         serverURL + "/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: useResponses,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Messages:        []ChatMessage{{Role: "user", Content: "hello"}},
		Tools:           testWebSearchTools(),
	}
}

func plainStreamRequest(serverURL string) ChatCompletionRequest {
	request := toolStreamRequest(serverURL, false)
	request.Tools = nil

	return request
}

func TestWebSearchToolDefinition(t *testing.T) {
	t.Parallel()

	tool := WebSearchTool(3)

	if tool.Name != WebSearchToolName {
		t.Fatalf("unexpected tool name: %q", tool.Name)
	}

	if tool.Description == "" {
		t.Fatal("expected non-empty tool description")
	}

	if tool.Parameters["type"] != "object" {
		t.Fatalf("unexpected parameters type: %#v", tool.Parameters["type"])
	}

	properties, propertiesOK := tool.Parameters["properties"].(map[string]any)
	if !propertiesOK {
		t.Fatalf("unexpected parameters properties: %#v", tool.Parameters["properties"])
	}

	queries, queriesOK := properties["queries"].(map[string]any)
	if !queriesOK {
		t.Fatalf("unexpected queries property: %#v", properties["queries"])
	}

	items, itemsOK := queries["items"].(map[string]any)
	if !itemsOK || items["type"] != "string" {
		t.Fatalf("unexpected queries items: %#v", queries["items"])
	}

	if queries["maxItems"] != 3 {
		t.Fatalf("unexpected queries maxItems: %#v", queries["maxItems"])
	}

	required, requiredOK := tool.Parameters["required"].([]string)
	if !requiredOK || len(required) != 1 || required[0] != "queries" {
		t.Fatalf("unexpected required properties: %#v", tool.Parameters["required"])
	}
}

func TestBuildChatCompletionRequestBodyIncludesTools(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			API:             "",
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
		Messages:        []ChatMessage{{Role: "user", Content: "hello"}},
		Tools:           testWebSearchTools(),
	}

	requestBody := buildChatCompletionRequestBody(request)

	tools, toolsOK := requestBody["tools"].([]map[string]any)
	if !toolsOK || len(tools) != 1 {
		t.Fatalf("unexpected tools payload: %#v", requestBody["tools"])
	}

	if tools[0]["type"] != "function" {
		t.Fatalf("unexpected tool type: %#v", tools[0]["type"])
	}

	function, functionOK := tools[0]["function"].(map[string]any)
	if !functionOK {
		t.Fatalf("unexpected tool function payload: %#v", tools[0]["function"])
	}

	if function["name"] != WebSearchToolName {
		t.Fatalf("unexpected tool function name: %#v", function["name"])
	}

	if _, hasParameters := function["parameters"]; !hasParameters {
		t.Fatal("expected tool function parameters")
	}

	if requestBody["tool_choice"] != "auto" {
		t.Fatalf("unexpected tool choice: %#v", requestBody["tool_choice"])
	}
}

func TestBuildChatCompletionRequestBodyOmitsToolsWhenEmpty(t *testing.T) {
	t.Parallel()

	request := plainStreamRequest("https://example.com")

	requestBody := buildChatCompletionRequestBody(request)

	if _, hasTools := requestBody["tools"]; hasTools {
		t.Fatalf("unexpected tools payload: %#v", requestBody["tools"])
	}

	if _, hasToolChoice := requestBody["tool_choice"]; hasToolChoice {
		t.Fatalf("unexpected tool choice: %#v", requestBody["tool_choice"])
	}
}

func TestOpenAIStreamChatCompletionAccumulatesToolCalls(t *testing.T) {
	t.Parallel()

	server := newToolCallTestServer(t, func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")

		// Parallel tool calls: fragments for the two calls interleave.
		writeSSE(t, responseWriter, chatCompletionsToolCallChunk(0, "call_a", "web_search", `{"queries": ["fir`))
		writeSSE(t, responseWriter, chatCompletionsToolCallChunk(1, "call_b", "web_search", `{"queries"`))
		writeSSE(t, responseWriter, chatCompletionsToolCallChunk(0, "", "", `st"]}`))
		writeSSE(t, responseWriter, chatCompletionsToolCallChunk(1, "", "", `: ["second"]}`))
		writeSSE(t, responseWriter, chatCompletionsFinishChunk("tool_calls"))
		writeSSE(t, responseWriter, "data: [DONE]\n\n")
	})
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := toolStreamRequest(server.URL, false)

	var toolCalls []FunctionToolCall

	err := client.streamChatCompletion(context.Background(), request, func(delta StreamDelta) error {
		toolCalls = append(toolCalls, delta.ToolCalls...)

		return nil
	})
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if len(toolCalls) != 2 {
		t.Fatalf("unexpected tool calls: %#v", toolCalls)
	}

	if toolCalls[0].ID != "call_a" || toolCalls[0].Name != "web_search" ||
		toolCalls[0].Arguments != `{"queries": ["first"]}` {
		t.Fatalf("unexpected first tool call: %#v", toolCalls[0])
	}

	if toolCalls[1].ID != "call_b" || toolCalls[1].Name != "web_search" ||
		toolCalls[1].Arguments != `{"queries": ["second"]}` {
		t.Fatalf("unexpected second tool call: %#v", toolCalls[1])
	}
}

func TestOpenAIStreamChatCompletionFinishReasonToolCallsIsNotAnError(t *testing.T) {
	t.Parallel()

	server := newToolCallTestServer(t, func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")

		writeSSE(t, responseWriter, chatCompletionsToolCallChunk(0, "call_a", "web_search", "{}"))
		writeSSE(t, responseWriter, chatCompletionsFinishChunk("tool_calls"))
		writeSSE(t, responseWriter, "data: [DONE]\n\n")
	})
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := toolStreamRequest(server.URL, false)

	var finishReason string

	err := client.streamChatCompletion(context.Background(), request, func(delta StreamDelta) error {
		if delta.FinishReason != "" {
			finishReason = delta.FinishReason
		}

		return nil
	})
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if finishReason != "tool_calls" {
		t.Fatalf("unexpected finish reason: %q", finishReason)
	}
}

func TestBuildResponsesRequestBodyIncludesFlatTools(t *testing.T) {
	t.Parallel()

	request := toolStreamRequest("https://api.openai.com", true)

	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build responses request body: %v", err)
	}

	tools, toolsOK := requestBody["tools"].([]map[string]any)
	if !toolsOK || len(tools) != 1 {
		t.Fatalf("unexpected tools payload: %#v", requestBody["tools"])
	}

	if tools[0]["type"] != "function" {
		t.Fatalf("unexpected tool type: %#v", tools[0]["type"])
	}

	// Responses API function tools are flat: name/description/parameters sit
	// on the tool object itself instead of a nested "function" key.
	if tools[0]["name"] != WebSearchToolName {
		t.Fatalf("unexpected tool name: %#v", tools[0]["name"])
	}

	if _, hasParameters := tools[0]["parameters"]; !hasParameters {
		t.Fatal("expected tool parameters")
	}

	if _, hasNestedFunction := tools[0]["function"]; hasNestedFunction {
		t.Fatalf("unexpected nested function key: %#v", tools[0]["function"])
	}

	if requestBody["tool_choice"] != "auto" {
		t.Fatalf("unexpected tool choice: %#v", requestBody["tool_choice"])
	}
}

func TestResponsesStreamCollectsFunctionCalls(t *testing.T) {
	t.Parallel()

	firstCall := responsesFunctionCallItem("fc_1", "call_a", "completed", `{"queries": ["first"]}`)
	secondCall := responsesFunctionCallItem("fc_2", "call_b", "completed", `{"queries": ["second"]}`)

	server := newToolCallTestServer(t, func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")

		addedFirst := responsesFunctionCallItem("fc_1", "call_a", "in_progress", "")
		addedSecond := responsesFunctionCallItem("fc_2", "call_b", "in_progress", "")

		writeSSE(t, responseWriter, responsesOutputItemChunk("response.output_item.added", addedFirst))
		writeSSE(t, responseWriter, responsesOutputItemChunk("response.output_item.added", addedSecond))
		writeSSE(t, responseWriter, responsesOutputItemChunk("response.output_item.done", firstCall))
		writeSSE(t, responseWriter, responsesOutputItemChunk("response.output_item.done", secondCall))

		completed := map[string]any{
			"id":     "resp_1",
			"status": "completed",
			"output": []any{firstCall, secondCall},
		}

		completedPayload, err := json.Marshal(map[string]any{
			"type":     "response.completed",
			"response": completed,
		})
		if err != nil {
			t.Fatalf("marshal completed event: %v", err)
		}

		writeSSE(t, responseWriter, "data: "+string(completedPayload)+"\n\n")
		writeSSE(t, responseWriter, "data: [DONE]\n\n")
	})
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := toolStreamRequest(server.URL, true)

	var toolCalls []FunctionToolCall

	var sawCompleted bool

	err := client.streamResponses(context.Background(), request, func(delta StreamDelta) error {
		if delta.FinishReason == "stop" {
			sawCompleted = true
		}

		toolCalls = append(toolCalls, delta.ToolCalls...)

		return nil
	})
	if err != nil {
		t.Fatalf("stream responses: %v", err)
	}

	if !sawCompleted {
		t.Fatal("expected a completed delta")
	}

	if len(toolCalls) != 2 {
		t.Fatalf("unexpected tool calls: %#v", toolCalls)
	}

	if toolCalls[0].ID != "call_a" || toolCalls[0].Name != "web_search" ||
		toolCalls[0].Arguments != `{"queries": ["first"]}` {
		t.Fatalf("unexpected first tool call: %#v", toolCalls[0])
	}

	if toolCalls[1].ID != "call_b" || toolCalls[1].Name != "web_search" ||
		toolCalls[1].Arguments != `{"queries": ["second"]}` {
		t.Fatalf("unexpected second tool call: %#v", toolCalls[1])
	}
}

func TestOpenAIStreamChatCompletionRequestSendsToolSchema(t *testing.T) {
	t.Parallel()

	requestPayload := make(chan map[string]any, 1)

	server := newToolCallTestServer(t, func(responseWriter http.ResponseWriter, request *http.Request) {
		body, readErr := readAllAndClose(request)
		if readErr != nil {
			t.Errorf("read request body: %v", readErr)

			return
		}

		var payload map[string]any

		decodeErr := json.Unmarshal(body, &payload)
		if decodeErr != nil {
			t.Errorf("decode request body: %v", decodeErr)

			return
		}

		requestPayload <- payload

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		writeSSE(t, responseWriter, chatCompletionsContentChunk("ok"))
		writeSSE(t, responseWriter, chatCompletionsFinishChunk("stop"))
		writeSSE(t, responseWriter, "data: [DONE]\n\n")
	})
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := toolStreamRequest(server.URL, false)

	err := client.streamChatCompletion(context.Background(), request, func(StreamDelta) error {
		return nil
	})
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	payload := <-requestPayload

	tools, toolsOK := payload["tools"].([]any)
	if !toolsOK || len(tools) != 1 {
		t.Fatalf("unexpected tools payload: %#v", payload["tools"])
	}

	tool, toolOK := tools[0].(map[string]any)
	if !toolOK {
		t.Fatalf("unexpected tool payload: %#v", tools[0])
	}

	function, functionOK := tool["function"].(map[string]any)
	if !functionOK || function["name"] != WebSearchToolName {
		t.Fatalf("unexpected tool function payload: %#v", tool["function"])
	}

	parameters, parametersOK := function["parameters"].(map[string]any)
	if !parametersOK || parameters["type"] != "object" {
		t.Fatalf("unexpected tool parameters: %#v", function["parameters"])
	}
}
