package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	searchtypes "llmcord-go/internal/searchtypes"
)

func claudeTestRequest(baseURL string) ChatCompletionRequest {
	return ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindClaude,
			API:             ClaudeAPIName,
			BaseURL:         baseURL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
		},
		Model:           "claude-sonnet-5",
		ConfiguredModel: "claude/claude-sonnet-5",
		Messages:        []ChatMessage{{Role: "user", Content: "hello"}},
	}
}

func writeClaudeChunk(t *testing.T, responseWriter http.ResponseWriter, content string) {
	t.Helper()

	if _, err := io.WriteString(responseWriter, content); err != nil {
		t.Fatalf("write claude chunk: %v", err)
	}
}

func TestClaudeClientStreamsTextAndThinking(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}

		if request.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("unexpected api key header: %q", request.Header.Get("X-Api-Key"))
		}

		if request.Header.Get("Anthropic-Version") != claudeDefaultVersion {
			t.Errorf("unexpected version header: %q", request.Header.Get("Anthropic-Version"))
		}

		var payload map[string]any

		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode claude payload: %v", err)
		}

		if payload["model"] != "claude-sonnet-5" {
			t.Errorf("unexpected model: %#v", payload["model"])
		}

		if payload["stream"] != true {
			t.Errorf("unexpected stream flag: %#v", payload["stream"])
		}

		if _, ok := payload["max_tokens"]; !ok {
			t.Errorf("expected max_tokens in payload: %#v", payload)
		}

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := responseWriter.(http.Flusher)
		if !ok {
			t.Error("expected flusher")

			return
		}

		writeClaudeChunk(t, responseWriter, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"sig\"}}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Plan first.\"}}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Final answer.\"}}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	client := newClaudeClient(server.Client())
	// Route the SDK through the test server: BaseURL override is honored
	// per-request via the client factory.
	client.newStreamer = func(apiKey string, provider ProviderRequestConfig) claudeMessagesStreamer {
		provider.BaseURL = server.URL

		return liveClaudeMessagesClient(apiKey, provider, server.Client())
	}

	var thinking, content, finish, responseID string

	err := client.streamChatCompletion(context.Background(), claudeTestRequest(""), func(delta StreamDelta) error {
		thinking += delta.Thinking
		content += delta.Content

		if delta.FinishReason != "" {
			finish = delta.FinishReason
		}

		if delta.ProviderResponseID != "" {
			responseID = delta.ProviderResponseID
		}

		return nil
	})
	if err != nil {
		t.Fatalf("stream claude completion: %v", err)
	}

	if thinking != "Plan first." {
		t.Fatalf("unexpected thinking: %q", thinking)
	}

	if content != "Final answer." {
		t.Fatalf("unexpected content: %q", content)
	}

	if finish != finishReasonStop {
		t.Fatalf("unexpected finish reason: %q", finish)
	}

	if responseID != "msg_123" {
		t.Fatalf("unexpected response id: %q", responseID)
	}
}

func TestClaudeClientStreamsToolCalls(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := responseWriter.(http.Flusher)
		if !ok {
			t.Error("expected flusher")

			return
		}

		writeClaudeChunk(t, responseWriter, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_tool\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"web_search\",\"input\":{}}}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"objective\\\": \\\"x\\\"\"}}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\", \\\"search_queries\\\": [\\\"q\\\"]}\" }}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n")
		flusher.Flush()
		writeClaudeChunk(t, responseWriter, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	client := newClaudeClient(server.Client())
	client.newStreamer = func(apiKey string, provider ProviderRequestConfig) claudeMessagesStreamer {
		provider.BaseURL = server.URL

		return liveClaudeMessagesClient(apiKey, provider, server.Client())
	}

	request := claudeTestRequest("")
	request.Tools = []FunctionTool{WebSearchTool(0)}

	var toolResponse *ToolCallResponse

	var finish string

	err := client.streamChatCompletion(context.Background(), request, func(delta StreamDelta) error {
		if delta.ToolCallResponse != nil {
			toolResponse = delta.ToolCallResponse
		}

		if delta.FinishReason != "" {
			finish = delta.FinishReason
		}

		return nil
	})
	if err != nil {
		t.Fatalf("stream claude tool completion: %v", err)
	}

	if toolResponse == nil || len(toolResponse.Calls) != 1 {
		t.Fatalf("expected one tool call, got %#v", toolResponse)
	}

	call := toolResponse.Calls[0]
	if call.ID != "toolu_1" || call.Name != WebSearchToolName {
		t.Fatalf("unexpected call: %#v", call)
	}

	if !strings.Contains(call.Arguments, "search_queries") {
		t.Fatalf("unexpected arguments: %q", call.Arguments)
	}

	if finish != string(anthropic.StopReasonToolUse) {
		t.Fatalf("unexpected finish reason: %q", finish)
	}
}

func TestClaudeClientSendsWebSearchToolDefinition(t *testing.T) {
	t.Parallel()

	var payload map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode claude payload: %v", err)
		}

		responseWriter.Header().Set("Content-Type", "text/event-stream")
		writeClaudeChunk(t, responseWriter, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		writeClaudeChunk(t, responseWriter, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	client := newClaudeClient(server.Client())
	client.newStreamer = func(apiKey string, provider ProviderRequestConfig) claudeMessagesStreamer {
		provider.BaseURL = server.URL

		return liveClaudeMessagesClient(apiKey, provider, server.Client())
	}

	request := claudeTestRequest("")
	request.Tools = []FunctionTool{WebSearchTool(0)}

	if err := client.streamChatCompletion(context.Background(), request, func(StreamDelta) error { return nil }); err != nil {
		t.Fatalf("stream claude completion: %v", err)
	}

	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected one tool, got %#v", payload["tools"])
	}

	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool shape: %#v", tools[0])
	}

	if tool["name"] != WebSearchToolName {
		t.Fatalf("unexpected tool name: %#v", tool["name"])
	}

	schema, ok := tool["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("expected input_schema, got %#v", tool["input_schema"])
	}

	if _, ok := schema["properties"]; !ok {
		t.Fatalf("expected schema properties: %#v", schema)
	}
}

func TestClaudeClientSendsToolChoiceNone(t *testing.T) {
	t.Parallel()

	var payload map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode claude payload: %v", err)
		}

		responseWriter.Header().Set("Content-Type", "text/event-stream")
		writeClaudeChunk(t, responseWriter, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		writeClaudeChunk(t, responseWriter, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	client := newClaudeClient(server.Client())
	client.newStreamer = func(apiKey string, provider ProviderRequestConfig) claudeMessagesStreamer {
		provider.BaseURL = server.URL

		return liveClaudeMessagesClient(apiKey, provider, server.Client())
	}

	request := claudeTestRequest("")
	request.Tools = []FunctionTool{WebSearchTool(0)}
	request.ToolChoice = ToolChoiceNone

	if err := client.streamChatCompletion(context.Background(), request, func(StreamDelta) error { return nil }); err != nil {
		t.Fatalf("stream claude completion: %v", err)
	}

	choice, ok := payload["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("expected tool_choice, got %#v", payload["tool_choice"])
	}

	if choice["type"] != ToolChoiceNone {
		t.Fatalf("unexpected tool_choice: %#v", choice)
	}
}

func TestClaudeClientOmitsToolChoiceNoneWithoutTools(t *testing.T) {
	t.Parallel()

	params, err := claudeMessageParams(ChatCompletionRequest{
		Model:      "claude-sonnet-5",
		Messages:   []ChatMessage{{Role: "user", Content: "hello"}},
		Provider:   ProviderRequestConfig{APIKind: ProviderAPIKindClaude},
		ToolChoice: ToolChoiceNone,
	})
	if err != nil {
		t.Fatalf("build claude params: %v", err)
	}

	if params.ToolChoice.OfNone != nil {
		t.Fatalf("expected tool_choice omitted without tools, got %#v", params.ToolChoice)
	}
}

func TestClaudeClientReplaysToolRounds(t *testing.T) {
	t.Parallel()

	params, err := claudeMessageParams(ChatCompletionRequest{
		Model:    "claude-sonnet-5",
		Messages: []ChatMessage{{Role: "user", Content: "search please"}},
		Tools:    []FunctionTool{WebSearchTool(0)},
		ToolRounds: []ToolRound{{
			Response: NewClaudeToolCallResponse(
				[]FunctionToolCall{{ID: "toolu_1", Name: WebSearchToolName, Arguments: `{"search_queries":["q"]}`}},
				"",
				"",
			),
			Outputs: []FunctionToolOutput{{CallID: "toolu_1", Output: "result text"}},
		}},
	})
	if err != nil {
		t.Fatalf("build claude params: %v", err)
	}

	if len(params.Messages) != 3 {
		t.Fatalf("expected user + tool_use + tool_result, got %d", len(params.Messages))
	}

	assistant := params.Messages[1]
	if len(assistant.Content) != 1 {
		t.Fatalf("expected one assistant block, got %d", len(assistant.Content))
	}

	results := params.Messages[2]
	if len(results.Content) != 1 {
		t.Fatalf("expected one result block, got %d", len(results.Content))
	}
}

func TestClaudeClientConvertsMultimodalContent(t *testing.T) {
	t.Parallel()

	params, err := claudeMessageParams(ChatCompletionRequest{
		Model: "claude-sonnet-5",
		Messages: []ChatMessage{
			{Role: "system", Content: "Be concise."},
			{Role: "user", Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": "What is this?"},
				{"type": searchtypes.ContentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
				{
					"type":                           searchtypes.ContentTypeDocument,
					searchtypes.ContentFieldBytes:    []byte("%PDF-1.4"),
					searchtypes.ContentFieldMIMEType: searchtypes.MimeTypePDF,
					searchtypes.ContentFieldFilename: "doc.pdf",
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("build claude params: %v", err)
	}

	if len(params.System) != 1 || params.System[0].Text != "Be concise." {
		t.Fatalf("expected system prompt, got %#v", params.System)
	}

	if len(params.Messages) != 1 {
		t.Fatalf("expected one message, got %d", len(params.Messages))
	}

	if len(params.Messages[0].Content) != 3 {
		t.Fatalf("expected text+image+document, got %d blocks", len(params.Messages[0].Content))
	}
}

func TestClaudeClientRejectsOfficeDocsWithoutExtraction(t *testing.T) {
	t.Parallel()

	_, err := claudeMessageParams(ChatCompletionRequest{
		Model: "claude-sonnet-5",
		Messages: []ChatMessage{
			{Role: "user", Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": "Summarize"},
				{
					"type":                           searchtypes.ContentTypeDocument,
					searchtypes.ContentFieldBytes:    []byte("docx-bytes"),
					searchtypes.ContentFieldMIMEType: searchtypes.MimeTypeDOCX,
					searchtypes.ContentFieldFilename: "doc.docx",
				},
			}},
		},
	})
	if err == nil {
		t.Fatal("expected raw DOCX to fail without extraction")
	}
}

func TestClaudeClientRetriesTransientStatusError(t *testing.T) {
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			responseWriter.Header().Set("Content-Type", "application/json")
			responseWriter.WriteHeader(529)

			return
		}

		responseWriter.Header().Set("Content-Type", "text/event-stream")
		writeClaudeChunk(t, responseWriter, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_r\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"recovered\"}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		writeClaudeChunk(t, responseWriter, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
		writeClaudeChunk(t, responseWriter, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	router := ChatCompletionRouter{
		openAI: newOpenAIClient(server.Client()),
		gemini: newGeminiClient(nil),
		claude: func() claudeClient {
			client := newClaudeClient(server.Client())
			client.newStreamer = func(apiKey string, provider ProviderRequestConfig) claudeMessagesStreamer {
				provider.BaseURL = server.URL

				return liveClaudeMessagesClient(apiKey, provider, server.Client())
			}

			return client
		}(),
		keys: NewAPIKeyRotator(),
	}

	var content strings.Builder

	err := router.StreamChatCompletion(context.Background(), claudeTestRequest(server.URL), func(delta StreamDelta) error {
		content.WriteString(delta.Content)

		return nil
	})
	if err != nil {
		t.Fatalf("stream with transient retry: %v", err)
	}

	if content.String() != "recovered" {
		t.Fatalf("unexpected content: %q", content.String())
	}

	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
}

func TestClaudeClientRejectsReservedExtraBodyKeys(t *testing.T) {
	t.Parallel()

	_, err := claudeMessageParams(ChatCompletionRequest{
		Model: "claude-sonnet-5",
		Messages: []ChatMessage{
			{Role: "user", Content: "hello"},
		},
		Provider: ProviderRequestConfig{
			APIKind:   ProviderAPIKindClaude,
			ExtraBody: map[string]any{"model": "other-model"},
		},
	})
	if err == nil {
		t.Fatal("expected reserved extra_body key to fail")
	}

	for _, key := range []string{"reasoningEffort", "output_config.effort"} {
		_, err := claudeMessageParams(ChatCompletionRequest{
			Model:    "claude-sonnet-5",
			Messages: []ChatMessage{{Role: "user", Content: "hello"}},
			Provider: ProviderRequestConfig{
				APIKind:   ProviderAPIKindClaude,
				ExtraBody: map[string]any{key: "turbo"},
			},
		})
		if err == nil {
			t.Fatalf("expected invalid effort under %q to fail", key)
		}
	}
}

func TestClaudeToolParamsForwardsStrict(t *testing.T) {
	t.Parallel()

	definitions, err := claudeToolParams([]FunctionTool{WebSearchTool(0)})
	if err != nil {
		t.Fatalf("build claude tools: %v", err)
	}

	if len(definitions) != 1 || definitions[0].OfTool == nil {
		t.Fatalf("expected one custom tool, got %#v", definitions)
	}

	if !definitions[0].OfTool.Strict.Valid() || !definitions[0].OfTool.Strict.Value {
		t.Fatalf("expected strict forwarded, got %#v", definitions[0].OfTool.Strict)
	}
}

func TestClaudeProviderSelected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		provider     ProviderRequestConfig
		model        string
		providerName string
		want         bool
	}{
		{
			name:         "explicit api",
			provider:     ProviderRequestConfig{API: ClaudeAPIName, BaseURL: "https://example.com"},
			model:        "anything",
			providerName: "custom",
			want:         true,
		},
		{
			name:         "claude model on default endpoint",
			provider:     ProviderRequestConfig{BaseURL: ""},
			model:        "claude-sonnet-5",
			providerName: "custom",
			want:         true,
		},
		{
			name:         "other api wins",
			provider:     ProviderRequestConfig{API: OpenAIAPIChatCompletions, BaseURL: ""},
			model:        "claude-sonnet-5",
			providerName: "custom",
			want:         false,
		},
		{
			name:         "non-claude model",
			provider:     ProviderRequestConfig{BaseURL: ""},
			model:        "gpt-5",
			providerName: "custom",
			want:         false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := ClaudeProviderSelected(testCase.provider, testCase.model, testCase.providerName); got != testCase.want {
				t.Fatalf("ClaudeProviderSelected = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestClaudeProviderBaseURL(t *testing.T) {
	t.Parallel()

	if got := claudeProviderBaseURL(ProviderRequestConfig{}); got != claudeDefaultBaseURL {
		t.Fatalf("expected default base url, got %q", got)
	}

	if got := claudeProviderBaseURL(ProviderRequestConfig{BaseURL: "http://localhost:20127/v1"}); got != "http://localhost:20127" {
		t.Fatalf("expected trailing /v1 stripped, got %q", got)
	}

	if got := claudeProviderBaseURL(ProviderRequestConfig{BaseURL: "http://localhost:20127/v1/"}); got != "http://localhost:20127" {
		t.Fatalf("expected trailing /v1/ stripped, got %q", got)
	}

	if got := claudeProviderBaseURL(ProviderRequestConfig{BaseURL: "https://api.anthropic.com"}); got != "https://api.anthropic.com" {
		t.Fatalf("unexpected base url rewrite: %q", got)
	}
}

func TestIsValidClaudeEffort(t *testing.T) {
	t.Parallel()

	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if !IsValidClaudeEffort(effort) {
			t.Fatalf("expected %q to be valid", effort)
		}
	}

	if IsValidClaudeEffort("turbo") {
		t.Fatal("expected turbo to be invalid")
	}
}

func TestIsClaudeTransientError(t *testing.T) {
	t.Parallel()

	transient := StatusError{StatusCode: 429, Message: "claude request failed with status 429: rate limited", Err: os.ErrInvalid}
	if !IsClaudeTransientError(transient) {
		t.Fatal("expected 429 claude error to be transient")
	}

	plain := fmt.Errorf("consume chat completion stream: %w", errClaudeTestSentinel)
	if IsClaudeTransientError(plain) {
		t.Fatal("expected non-claude error to stay non-transient")
	}
}

var errClaudeTestSentinel = errors.New("rate limited")
