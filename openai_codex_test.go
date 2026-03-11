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
)

const (
	testOpenAICodexAccountID   = "account-123"
	testOpenAICodexHeaderValue = "present"
	testOpenAICodexHelloText   = "Hello"
	testOpenAICodexModel       = "gpt-5.2-codex"
)

func TestOpenAICodexClientStreamChatCompletion(t *testing.T) {
	t.Parallel()

	apiKey := testOpenAICodexJWT(t, testOpenAICodexAccountID)

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
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: testOpenAICodexHelloText},
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
		assertOpenAICodexRequest(t, request, apiKey)

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

func assertOpenAICodexRequest(t *testing.T, request *http.Request, apiKey string) {
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

	var payload map[string]any

	err := json.NewDecoder(request.Body).Decode(&payload)
	if err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	assertOpenAICodexPayload(t, payload)
}

func assertOpenAICodexPayload(t *testing.T, payload map[string]any) {
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

func testOpenAICodexJWT(t *testing.T, accountID string) string {
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
			"chatgpt_account_id": accountID,
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
