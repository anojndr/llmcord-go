package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	testOpenAICodexAccountID   = "account-123"
	testOpenAICodexHeaderValue = "present"
	testOpenAICodexHelloText   = "Hello"
	testOpenAICodexModel       = "gpt-5.2-codex"
)

func TestOpenAICodexClientStreamChatCompletion(t *testing.T) {
	t.Parallel()

	apiKey := testOpenAICodexJWT(t)

	server := newOpenAICodexStreamingTestServer(t, apiKey)
	defer server.Close()

	client := newOpenAICodexClient(server.Client())
	request := chatCompletionRequest{
		Provider: newOpenAICodexProviderRequestConfig(
			apiKey,
			server.URL+"/backend-api",
			map[string]any{"X-Test": testOpenAICodexHeaderValue},
			map[string]any{"feature": "enabled"},
			map[string]any{"verbosity": "high", "reasoning_effort": "medium"},
		),
		Model:           testOpenAICodexModel,
		ConfiguredModel: "",
		SessionID:       "",
		Messages: []chatMessage{
			{Role: openAICodexRoleSystem, Content: "Be brief."},
			{
				Role: messageRoleUser,
				Content: []contentPart{
					{"type": contentTypeText, "text": testOpenAICodexHelloText},
					{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
				},
			},
			{Role: messageRoleAssistant, Content: "Previous answer"},
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
		t.Fatalf("stream codex chat completion: %v", err)
	}

	if joinedContent.String() != testOpenAICodexHelloText {
		t.Fatalf("unexpected streamed content: %q", joinedContent.String())
	}

	if finishReason != finishReasonStop {
		t.Fatalf("unexpected finish reason: %q", finishReason)
	}
}

func TestOpenAICodexClientRejectsInvalidTokenWithoutAccountHeader(t *testing.T) {
	t.Parallel()

	client := newOpenAICodexClient(new(http.Client))
	request := chatCompletionRequest{
		Provider:        newOpenAICodexProviderRequestConfig("not-a-jwt", "", nil, nil, nil),
		Model:           testOpenAICodexModel,
		ConfiguredModel: "",
		SessionID:       "",
		Messages: []chatMessage{
			{Role: openAICodexRoleSystem, Content: "Be brief."},
			{Role: messageRoleUser, Content: testOpenAICodexHelloText},
			{Role: messageRoleAssistant, Content: "Previous answer"},
		},
	}

	err := client.streamChatCompletion(context.Background(), request, func(streamDelta) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected invalid token to return an error")
	}

	if !strings.Contains(err.Error(), "extract codex account id") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestOpenAICodexClientStreamChatCompletionIncludesCacheMetadata(t *testing.T) {
	t.Parallel()

	apiKey := testOpenAICodexJWT(t)
	sessionID := "codex-session-123"

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		if request.Header.Get(openAICodexHeaderSessionID) != sessionID {
			t.Fatalf("unexpected session_id header: %q", request.Header.Get(openAICodexHeaderSessionID))
		}

		assertOpenAICodexRequest(t, request, apiKey, sessionID)
		streamOpenAICodexHello(t, responseWriter)
	}))
	defer server.Close()

	client := newOpenAICodexClient(server.Client())
	request := chatCompletionRequest{
		Provider: newOpenAICodexProviderRequestConfig(
			apiKey,
			server.URL+"/backend-api",
			map[string]any{"X-Test": testOpenAICodexHeaderValue},
			map[string]any{"feature": "enabled"},
			map[string]any{"verbosity": "high", "reasoning_effort": "medium"},
		),
		Model:           testOpenAICodexModel,
		ConfiguredModel: "codex/gpt-5.2-codex",
		SessionID:       sessionID,
		Messages: []chatMessage{
			{Role: openAICodexRoleSystem, Content: "Be brief."},
			{
				Role: messageRoleUser,
				Content: []contentPart{
					{"type": contentTypeText, "text": testOpenAICodexHelloText},
					{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
				},
			},
			{Role: messageRoleAssistant, Content: "Previous answer"},
		},
	}

	err := client.streamChatCompletion(context.Background(), request, func(streamDelta) error {
		return nil
	})
	if err != nil {
		t.Fatalf("stream codex chat completion: %v", err)
	}
}

func TestBuildOpenAICodexRequestBodyPreservesNestedReasoningConfig(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: newOpenAICodexProviderRequestConfig(
			"",
			"",
			nil,
			nil,
			map[string]any{
				"reasoning": map[string]any{
					"effort":  "none",
					"summary": "concise",
				},
			},
		),
		Model:           "gpt-5.4",
		ConfiguredModel: "",
		SessionID:       "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: testOpenAICodexHelloText},
		},
	}

	requestBody, err := buildOpenAICodexRequestBody(request)
	if err != nil {
		t.Fatalf("build codex request body: %v", err)
	}

	if requestBody["model"] != "gpt-5.4" {
		t.Fatalf("unexpected model: %#v", requestBody["model"])
	}

	reasoningConfig, ok := requestBody["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected reasoning config type: %T", requestBody["reasoning"])
	}

	if reasoningConfig["effort"] != "none" {
		t.Fatalf("unexpected reasoning effort: %#v", reasoningConfig["effort"])
	}

	if reasoningConfig["summary"] != "concise" {
		t.Fatalf("unexpected reasoning summary: %#v", reasoningConfig["summary"])
	}
}

func TestBuildOpenAICodexRequestBodyDefaultsReasoningSummaryWithoutEffort(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: newOpenAICodexProviderRequestConfig(
			"",
			"",
			nil,
			nil,
			nil,
		),
		Model:           "gpt-5.4",
		ConfiguredModel: "",
		SessionID:       "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: testOpenAICodexHelloText},
		},
	}

	requestBody, err := buildOpenAICodexRequestBody(request)
	if err != nil {
		t.Fatalf("build codex request body: %v", err)
	}

	reasoningConfig, ok := requestBody["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected reasoning config type: %T", requestBody["reasoning"])
	}

	if reasoningConfig["summary"] != openAICodexAuto {
		t.Fatalf("unexpected reasoning summary: %#v", reasoningConfig["summary"])
	}

	if _, exists := reasoningConfig["effort"]; exists {
		t.Fatalf("unexpected reasoning effort: %#v", reasoningConfig["effort"])
	}
}

func TestBuildOpenAICodexRequestBodyClampsNestedReasoningConfigWithoutMutatingOriginal(t *testing.T) {
	t.Parallel()

	originalReasoningConfig := map[string]any{
		"effort": "minimal",
	}

	request := chatCompletionRequest{
		Provider: newOpenAICodexProviderRequestConfig(
			"",
			"",
			nil,
			nil,
			map[string]any{
				"reasoning": originalReasoningConfig,
			},
		),
		Model:           "gpt-5.4",
		ConfiguredModel: "",
		SessionID:       "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: testOpenAICodexHelloText},
		},
	}

	requestBody, err := buildOpenAICodexRequestBody(request)
	if err != nil {
		t.Fatalf("build codex request body: %v", err)
	}

	reasoningConfig, ok := requestBody["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected reasoning config type: %T", requestBody["reasoning"])
	}

	if reasoningConfig["effort"] != "low" {
		t.Fatalf("unexpected clamped reasoning effort: %#v", reasoningConfig["effort"])
	}

	if reasoningConfig["summary"] != openAICodexAuto {
		t.Fatalf("unexpected default reasoning summary: %#v", reasoningConfig["summary"])
	}

	if originalReasoningConfig["effort"] != "minimal" {
		t.Fatalf("unexpected mutation of original reasoning effort: %#v", originalReasoningConfig)
	}

	if _, exists := originalReasoningConfig["summary"]; exists {
		t.Fatalf("unexpected mutation of original reasoning summary: %#v", originalReasoningConfig)
	}
}

func TestNormalizeOpenAICodexReasoningEffort(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		model    string
		effort   string
		expected string
	}{
		{
			name:     "gpt-5.4 minimal clamps to low",
			model:    "gpt-5.4",
			effort:   "minimal",
			expected: "low",
		},
		{
			name:     "gpt-5.1 xhigh clamps to high",
			model:    "gpt-5.1",
			effort:   "xhigh",
			expected: "high",
		},
		{
			name:     "gpt-5.1-codex-mini none clamps to medium",
			model:    "gpt-5.1-codex-mini",
			effort:   "none",
			expected: "medium",
		},
		{
			name:     "gpt-5.1-codex-mini xhigh clamps to high",
			model:    "gpt-5.1-codex-mini",
			effort:   "xhigh",
			expected: "high",
		},
		{
			name:     "unrecognized effort normalizes casing",
			model:    "gpt-5.2-codex",
			effort:   "MEDIUM",
			expected: "medium",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			actual := normalizeOpenAICodexReasoningEffort(testCase.model, testCase.effort)
			if actual != testCase.expected {
				t.Fatalf("unexpected reasoning effort: got %q want %q", actual, testCase.expected)
			}
		})
	}
}

func TestOpenAICodexConsumeServerSentEventsAcceptsTerminalIncompleteEventWithoutDone(t *testing.T) {
	t.Parallel()

	var (
		joinedContent strings.Builder
		finishReason  string
		terminalSeen  bool
	)

	doneSeen, err := consumeServerSentEvents(
		strings.NewReader(
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n"+
				"data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n",
		),
		func(payload []byte) error {
			terminal, payloadErr := handleOpenAICodexStreamPayload(payload, func(delta streamDelta) error {
				joinedContent.WriteString(delta.Content)

				if delta.FinishReason != "" {
					finishReason = delta.FinishReason
				}

				return nil
			})
			if terminal {
				terminalSeen = true
			}

			return payloadErr
		},
	)
	if err != nil {
		t.Fatalf("consume codex stream: %v", err)
	}

	if doneSeen {
		t.Fatal("did not expect [DONE] marker")
	}

	if !terminalSeen {
		t.Fatal("expected terminal response event to be observed")
	}

	if joinedContent.String() != testOpenAICodexHelloText {
		t.Fatalf("unexpected streamed content: %q", joinedContent.String())
	}

	if finishReason != "length" {
		t.Fatalf("unexpected finish reason: %q", finishReason)
	}
}

func TestHandleOpenAICodexStreamPayloadUsesIncompleteDetailsReason(t *testing.T) {
	t.Parallel()

	terminal, err := handleOpenAICodexStreamPayload(
		[]byte(`{"type":"response.failed","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`),
		func(delta streamDelta) error {
			t.Fatalf("unexpected stream delta: %#v", delta)

			return nil
		},
	)
	if err == nil {
		t.Fatal("expected response.failed to return an error")
	}

	if terminal {
		t.Fatal("expected response.failed not to be marked terminal")
	}

	if !strings.Contains(err.Error(), "incomplete: max_output_tokens") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandleOpenAICodexStreamPayloadReturnsFailedTerminalStatusErrors(t *testing.T) {
	t.Parallel()

	terminal, err := handleOpenAICodexStreamPayload(
		[]byte(
			`{"type":"response.done","response":{"status":"failed","error":{"code":"server_error",`+
				`"message":"agent backend crashed"}}}`,
		),
		func(delta streamDelta) error {
			t.Fatalf("unexpected stream delta: %#v", delta)

			return nil
		},
	)
	if err == nil {
		t.Fatal("expected failed terminal status to return an error")
	}

	if !terminal {
		t.Fatal("expected response.done to be marked terminal")
	}

	if !strings.Contains(err.Error(), "server_error: agent backend crashed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandleOpenAICodexStreamPayloadReturnsContentFilterIncompleteErrors(t *testing.T) {
	t.Parallel()

	terminal, err := handleOpenAICodexStreamPayload(
		[]byte(
			`{"type":"response.incomplete","response":{"status":"incomplete",`+
				`"incomplete_details":{"reason":"content_filter"}}}`,
		),
		func(delta streamDelta) error {
			t.Fatalf("unexpected stream delta: %#v", delta)

			return nil
		},
	)
	if err == nil {
		t.Fatal("expected content filter incomplete response to return an error")
	}

	if !terminal {
		t.Fatal("expected response.incomplete to be marked terminal")
	}

	if !strings.Contains(err.Error(), "incomplete: content_filter") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenAICodexClientStreamChatCompletionParsesJSONStatusErrors(t *testing.T) {
	t.Parallel()

	apiKey := testOpenAICodexJWT(t)

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.Header().Set("Content-Type", "application/json")
		responseWriter.WriteHeader(http.StatusTooManyRequests)
		writeStreamChunk(
			t,
			responseWriter,
			`{"error":{"code":"usage_limit_reached","message":"Usage limit reached","plan_type":"PLUS"}}`,
		)
	}))
	defer server.Close()

	client := newOpenAICodexClient(server.Client())
	request := chatCompletionRequest{
		Provider:        newOpenAICodexProviderRequestConfig(apiKey, server.URL, nil, nil, nil),
		Model:           testOpenAICodexModel,
		ConfiguredModel: "",
		SessionID:       "",
		Messages:        []chatMessage{{Role: messageRoleUser, Content: testOpenAICodexHelloText}},
	}

	err := client.streamChatCompletion(context.Background(), request, func(streamDelta) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected JSON status error")
	}

	for _, fragment := range []string{
		"status 429",
		"You have hit your ChatGPT usage limit",
		"code=usage_limit_reached",
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("expected %q in error, got %v", fragment, err)
		}
	}
}

func TestHandleOpenAICodexStreamPayloadIncludesEventErrorCode(t *testing.T) {
	t.Parallel()

	terminal, err := handleOpenAICodexStreamPayload(
		[]byte(`{"type":"error","message":"usage exhausted","code":"usage_limit_reached"}`),
		func(delta streamDelta) error {
			t.Fatalf("unexpected stream delta: %#v", delta)

			return nil
		},
	)
	if err == nil {
		t.Fatal("expected error event to return an error")
	}

	if terminal {
		t.Fatal("expected error event not to be marked terminal")
	}

	if !strings.Contains(err.Error(), "usage_limit_reached: usage exhausted") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandleOpenAICodexStreamPayloadEmitsReasoningSummaries(t *testing.T) {
	t.Parallel()

	var thoughtText strings.Builder

	terminal, err := handleOpenAICodexStreamPayload(
		[]byte(`{"type":"response.reasoning_summary_text.delta","delta":"Inspecting..."}`),
		func(delta streamDelta) error {
			thoughtText.WriteString(delta.Thinking)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("handle reasoning summary delta: %v", err)
	}

	if terminal {
		t.Fatal("expected reasoning summary delta not to be terminal")
	}

	terminal, err = handleOpenAICodexStreamPayload(
		[]byte(`{"type":"response.reasoning_summary_part.done"}`),
		func(delta streamDelta) error {
			thoughtText.WriteString(delta.Thinking)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("handle reasoning summary part done: %v", err)
	}

	if terminal {
		t.Fatal("expected reasoning summary part done not to be terminal")
	}

	if thoughtText.String() != "Inspecting...\n\n" {
		t.Fatalf("unexpected reasoning summary text: %q", thoughtText.String())
	}
}

func TestAssignOpenAICodexSessionIDUsesConversationAnchor(t *testing.T) {
	t.Parallel()

	const testChannelID = "channel-1"

	store := newMessageNodeStore(8)
	rootMessage := new(discordgo.Message)
	rootMessage.ID = "100"
	rootMessage.ChannelID = testChannelID

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = "200"
	assistantMessage.ChannelID = testChannelID

	followUpMessage := new(discordgo.Message)
	followUpMessage.ID = "300"
	followUpMessage.ChannelID = testChannelID

	store.getOrCreate(assistantMessage.ID).parentMessage = rootMessage
	store.getOrCreate(followUpMessage.ID).parentMessage = assistantMessage

	var rootRequest chatCompletionRequest

	rootRequest.Provider.APIKind = providerAPIKindOpenAICodex
	rootRequest.ConfiguredModel = "codex/gpt-5.4"

	followUpRequest := rootRequest

	assignOpenAICodexSessionID(&rootRequest, assistantMessage, store, 10)
	assignOpenAICodexSessionID(&followUpRequest, followUpMessage, store, 10)

	if rootRequest.SessionID == "" {
		t.Fatal("expected non-empty session id")
	}

	if rootRequest.SessionID != followUpRequest.SessionID {
		t.Fatalf("expected shared session id, got %q and %q", rootRequest.SessionID, followUpRequest.SessionID)
	}
}

func newOpenAICodexProviderRequestConfig(
	apiKey string,
	baseURL string,
	extraHeaders map[string]any,
	extraQuery map[string]any,
	extraBody map[string]any,
) providerRequestConfig {
	return providerRequestConfig{
		APIKind:      providerAPIKindOpenAICodex,
		BaseURL:      baseURL,
		APIKey:       apiKey,
		APIKeys:      nil,
		ExtraHeaders: extraHeaders,
		ExtraQuery:   extraQuery,
		ExtraBody:    extraBody,
	}
}

func newOpenAICodexStreamingTestServer(t *testing.T, apiKey string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()
		assertOpenAICodexRequest(t, request, apiKey, "")

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		flusher, typeOK := responseWriter.(http.Flusher)
		if !typeOK {
			t.Fatal("expected response writer to support flushing")
		}

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
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
		)
		flusher.Flush()
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func assertOpenAICodexRequest(
	t *testing.T,
	request *http.Request,
	apiKey string,
	expectedSessionID string,
) {
	t.Helper()

	if request.URL.Path != "/backend-api/codex/responses" {
		t.Fatalf("unexpected path: %s", request.URL.Path)
	}

	if request.URL.Query().Get("feature") != "enabled" {
		t.Fatalf("unexpected query string: %s", request.URL.RawQuery)
	}

	if request.Header.Get("Authorization") != "Bearer "+apiKey {
		t.Fatalf("unexpected authorization header: %q", request.Header.Get("Authorization"))
	}

	if request.Header.Get(openAICodexHeaderAccount) != testOpenAICodexAccountID {
		t.Fatalf("unexpected chatgpt-account-id header: %q", request.Header.Get(openAICodexHeaderAccount))
	}

	if request.Header.Get(openAICodexHeaderBeta) != "responses=experimental" {
		t.Fatalf("unexpected OpenAI-Beta header: %q", request.Header.Get(openAICodexHeaderBeta))
	}

	if request.Header.Get(openAICodexHeaderOrigin) != openAICodexOriginator {
		t.Fatalf("unexpected originator header: %q", request.Header.Get(openAICodexHeaderOrigin))
	}

	if request.Header.Get("X-Test") != testOpenAICodexHeaderValue {
		t.Fatalf("unexpected X-Test header: %q", request.Header.Get("X-Test"))
	}

	if expectedSessionID == "" {
		if got := request.Header.Get(openAICodexHeaderSessionID); got != "" {
			t.Fatalf("unexpected session_id header: %q", got)
		}
	} else if request.Header.Get(openAICodexHeaderSessionID) != expectedSessionID {
		t.Fatalf("unexpected session_id header: %q", request.Header.Get(openAICodexHeaderSessionID))
	}

	var payload map[string]any

	err := json.NewDecoder(request.Body).Decode(&payload)
	if err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	assertOpenAICodexPayload(t, payload, expectedSessionID)
}

func assertOpenAICodexPayload(t *testing.T, payload map[string]any, expectedSessionID string) {
	t.Helper()

	if payload["model"] != testOpenAICodexModel {
		t.Fatalf("unexpected model: %#v", payload["model"])
	}

	if payload["store"] != false {
		t.Fatalf("unexpected store flag: %#v", payload["store"])
	}

	if payload["stream"] != true {
		t.Fatalf("unexpected stream flag: %#v", payload["stream"])
	}

	if payload["instructions"] != "Be brief." {
		t.Fatalf("unexpected instructions: %#v", payload["instructions"])
	}

	if _, exists := payload["verbosity"]; exists {
		t.Fatalf("unexpected top-level verbosity key: %#v", payload["verbosity"])
	}

	if _, exists := payload["reasoning_effort"]; exists {
		t.Fatalf("unexpected top-level reasoning_effort key: %#v", payload["reasoning_effort"])
	}

	textConfig, textTypeOK := payload["text"].(map[string]any)
	if !textTypeOK {
		t.Fatalf("unexpected text config type: %T", payload["text"])
	}

	if textConfig["verbosity"] != "high" {
		t.Fatalf("unexpected text verbosity: %#v", textConfig["verbosity"])
	}

	reasoningConfig, reasoningTypeOK := payload["reasoning"].(map[string]any)
	if !reasoningTypeOK {
		t.Fatalf("unexpected reasoning config type: %T", payload["reasoning"])
	}

	if reasoningConfig["effort"] != "medium" {
		t.Fatalf("unexpected reasoning effort: %#v", reasoningConfig["effort"])
	}

	if reasoningConfig["summary"] != openAICodexAuto {
		t.Fatalf("unexpected reasoning summary: %#v", reasoningConfig["summary"])
	}

	include, includeOK := payload["include"].([]any)
	if !includeOK {
		t.Fatalf("unexpected include type: %T", payload["include"])
	}

	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("unexpected include field: %#v", include)
	}

	if payload["tool_choice"] != openAICodexAuto {
		t.Fatalf("unexpected tool_choice: %#v", payload["tool_choice"])
	}

	if payload["parallel_tool_calls"] != true {
		t.Fatalf("unexpected parallel_tool_calls flag: %#v", payload["parallel_tool_calls"])
	}

	assertOpenAICodexPromptCacheKey(t, payload, expectedSessionID)

	input, typeOK := payload["input"].([]any)
	if !typeOK {
		t.Fatalf("unexpected input type: %T", payload["input"])
	}

	if len(input) != 2 {
		t.Fatalf("unexpected input length: %d", len(input))
	}

	assertOpenAICodexUserMessage(t, input[0])
	assertOpenAICodexAssistantMessage(t, input[1])
}

func assertOpenAICodexPromptCacheKey(
	t *testing.T,
	payload map[string]any,
	expectedSessionID string,
) {
	t.Helper()

	if expectedSessionID == "" {
		if _, exists := payload["prompt_cache_key"]; exists {
			t.Fatalf("unexpected prompt_cache_key: %#v", payload["prompt_cache_key"])
		}

		return
	}

	if payload["prompt_cache_key"] != expectedSessionID {
		t.Fatalf("unexpected prompt_cache_key: %#v", payload["prompt_cache_key"])
	}
}

func assertOpenAICodexUserMessage(t *testing.T, rawMessage any) {
	t.Helper()

	userMessage, typeOK := rawMessage.(map[string]any)
	if !typeOK {
		t.Fatalf("unexpected user message type: %T", rawMessage)
	}

	if userMessage["role"] != messageRoleUser {
		t.Fatalf("unexpected user role: %#v", userMessage["role"])
	}

	userContent, typeOK := userMessage["content"].([]any)
	if !typeOK {
		t.Fatalf("unexpected user content type: %T", userMessage["content"])
	}

	if len(userContent) != 2 {
		t.Fatalf("unexpected user content length: %d", len(userContent))
	}

	firstPart, typeOK := userContent[0].(map[string]any)
	if !typeOK {
		t.Fatalf("unexpected first user part type: %T", userContent[0])
	}

	if firstPart["type"] != "input_text" || firstPart["text"] != testOpenAICodexHelloText {
		t.Fatalf("unexpected first user part: %#v", firstPart)
	}

	secondPart, typeOK := userContent[1].(map[string]any)
	if !typeOK {
		t.Fatalf("unexpected second user part type: %T", userContent[1])
	}

	if secondPart["type"] != "input_image" {
		t.Fatalf("unexpected second user part type: %#v", secondPart["type"])
	}

	if secondPart["image_url"] != "data:image/png;base64,abc" {
		t.Fatalf("unexpected image_url: %#v", secondPart["image_url"])
	}
}

func assertOpenAICodexAssistantMessage(t *testing.T, rawMessage any) {
	t.Helper()

	assistantMessage, typeOK := rawMessage.(map[string]any)
	if !typeOK {
		t.Fatalf("unexpected assistant message type: %T", rawMessage)
	}

	if assistantMessage["role"] != messageRoleAssistant {
		t.Fatalf("unexpected assistant role: %#v", assistantMessage["role"])
	}

	assistantContent, typeOK := assistantMessage["content"].([]any)
	if !typeOK {
		t.Fatalf("unexpected assistant content type: %T", assistantMessage["content"])
	}

	if len(assistantContent) != 1 {
		t.Fatalf("unexpected assistant content length: %d", len(assistantContent))
	}

	assistantPart, typeOK := assistantContent[0].(map[string]any)
	if !typeOK {
		t.Fatalf("unexpected assistant part type: %T", assistantContent[0])
	}

	if assistantPart["type"] != "output_text" || assistantPart["text"] != "Previous answer" {
		t.Fatalf("unexpected assistant part: %#v", assistantPart)
	}
}

func testOpenAICodexJWT(t *testing.T) string {
	t.Helper()

	headerBytes, err := json.Marshal(map[string]any{
		"alg": "HS256",
		"typ": "JWT",
	})
	if err != nil {
		t.Fatalf("marshal jwt header: %v", err)
	}

	payloadBytes, err := json.Marshal(map[string]any{
		openAICodexJWTClaimPath: map[string]any{
			"chatgpt_account_id": testOpenAICodexAccountID,
		},
	})
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}

	encode := func(data []byte) string {
		return base64.RawURLEncoding.EncodeToString(data)
	}

	return fmt.Sprintf("%s.%s.signature", encode(headerBytes), encode(payloadBytes))
}
