package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIStreamPayloadDeltaParsesCachedTokenUsage(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"choices": [],
		"usage": {
			"prompt_tokens": 2006,
			"completion_tokens": 300,
			"prompt_tokens_details": {
				"cached_tokens": 1920,
				"cache_write_tokens": 0
			},
			"completion_tokens_details": {
				"reasoning_tokens": 84
			}
		}
	}`)

	delta, err := openAIStreamPayloadDelta(payload)
	if err != nil {
		t.Fatalf("decode stream payload: %v", err)
	}

	if delta.Usage == nil {
		t.Fatal("expected usage payload")
	}

	if delta.Usage.Input != 2006 || delta.Usage.Output != 300 {
		t.Fatalf("unexpected usage: %#v", delta.Usage)
	}

	if delta.Usage.CachedInput != 1920 {
		t.Fatalf("unexpected cached input tokens: %d", delta.Usage.CachedInput)
	}

	if delta.Usage.CacheWriteTokens != 0 {
		t.Fatalf("unexpected cache write tokens: %d", delta.Usage.CacheWriteTokens)
	}

	if delta.ReasoningTokens != 84 {
		t.Fatalf("unexpected reasoning tokens: %d", delta.ReasoningTokens)
	}
}

func TestOpenAIStreamPayloadDeltaZeroCachedTokensWhenDetailsMissing(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"choices": [],
		"usage": {
			"prompt_tokens": 12,
			"completion_tokens": 34
		}
	}`)

	delta, err := openAIStreamPayloadDelta(payload)
	if err != nil {
		t.Fatalf("decode stream payload: %v", err)
	}

	if delta.Usage == nil {
		t.Fatal("expected usage payload")
	}

	if delta.Usage.CachedInput != 0 || delta.Usage.CacheWriteTokens != 0 {
		t.Fatalf("unexpected cached usage: %#v", delta.Usage)
	}

	if delta.ReasoningTokens != 0 {
		t.Fatalf("unexpected reasoning tokens: %d", delta.ReasoningTokens)
	}
}

func TestBuildChatCompletionRequestBodySkipsCacheOptionsWithoutSessionID(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: false,
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "openai/gpt-test",
		SessionID:       "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: "hello"},
		},
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		PreviousResponseID:          "",
		RequestID:                   "",
	}

	requestBody := buildChatCompletionRequestBody(request)

	if _, exists := requestBody["prompt_cache_options"]; exists {
		t.Fatalf("unexpected prompt_cache_options without session id: %#v", requestBody["prompt_cache_options"])
	}

	if _, exists := requestBody["prompt_cache_key"]; exists {
		t.Fatalf("unexpected prompt_cache_key without session id: %#v", requestBody["prompt_cache_key"])
	}

	messages, messagesOK := requestBody["messages"].([]chatMessage)
	if !messagesOK || len(messages) != 1 {
		t.Fatalf("unexpected messages payload: %#v", requestBody["messages"])
	}

	if _, exists := messages[0].Content.(string); !exists {
		t.Fatalf("expected plain string content without cache session: %#v", messages[0].Content)
	}
}

func TestBuildChatCompletionRequestBodySkipsCacheBreakpointWithoutSessionID(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: false,
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "openai/gpt-test",
		SessionID:       "",
		Messages: []chatMessage{
			{Role: messageRoleSystem, Content: "You are concise."},
			{Role: messageRoleUser, Content: "hello"},
		},
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		PreviousResponseID:          "",
		RequestID:                   "",
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]chatMessage)
	if !messagesOK || len(messages) != 2 {
		t.Fatalf("unexpected messages payload: %#v", requestBody["messages"])
	}

	if _, exists := messages[0].Content.(string); !exists {
		t.Fatalf("expected system message content string: %#v", messages[0].Content)
	}

	if _, exists := messages[1].Content.(string); !exists {
		t.Fatalf("expected user message content string: %#v", messages[1].Content)
	}
}

func TestBuildChatCompletionRequestBodySkipsCacheBreakpointForNonExplicitMode(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: false,
			ExtraBody: map[string]any{
				"prompt_cache_options": map[string]any{
					"mode": "implicit",
				},
			},
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "openai/gpt-test",
		SessionID:       testOpenAIPromptCacheKey,
		Messages: []chatMessage{
			{Role: messageRoleSystem, Content: "You are concise."},
			{Role: messageRoleUser, Content: "hello"},
		},
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		PreviousResponseID:          "",
		RequestID:                   "",
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]chatMessage)
	if !messagesOK || len(messages) != 2 {
		t.Fatalf("unexpected messages payload: %#v", requestBody["messages"])
	}

	if _, exists := messages[0].Content.(string); !exists {
		t.Fatalf("expected system message content string: %#v", messages[0].Content)
	}

	if _, exists := messages[1].Content.(string); !exists {
		t.Fatalf("expected user message content string: %#v", messages[1].Content)
	}
}

func TestBuildChatCompletionRequestBodyAddsCacheOptionsForOpenAIProvider(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: false,
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "openai/gpt-test",
		SessionID:       testOpenAIPromptCacheKey,
		Messages: []chatMessage{
			{Role: messageRoleSystem, Content: "You are concise."},
			{Role: messageRoleUser, Content: "hello"},
		},
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		PreviousResponseID:          "",
		RequestID:                   "",
	}

	requestBody := buildChatCompletionRequestBody(request)

	cacheOptions, cacheOptionsOK := requestBody["prompt_cache_options"].(map[string]any)
	if !cacheOptionsOK {
		t.Fatalf("unexpected prompt_cache_options payload: %#v", requestBody["prompt_cache_options"])
	}

	if cacheOptions["mode"] != "implicit" {
		t.Fatalf("unexpected prompt_cache_options mode: %#v", cacheOptions["mode"])
	}

	if cacheOptions["ttl"] != "30m" {
		t.Fatalf("unexpected prompt_cache_options ttl: %#v", cacheOptions["ttl"])
	}

	messages, messagesOK := requestBody["messages"].([]chatMessage)
	if !messagesOK || len(messages) != 2 {
		t.Fatalf("unexpected messages payload: %#v", requestBody["messages"])
	}

	systemPart, systemOK := messages[0].Content.([]map[string]any)
	if !systemOK || len(systemPart) != 1 {
		t.Fatalf("expected system breakpoint part: %#v", messages[0].Content)
	}

	breakpoint, breakpointOK := systemPart[0]["prompt_cache_breakpoint"].(map[string]any)
	if !breakpointOK || breakpoint["mode"] != "explicit" {
		t.Fatalf("unexpected prompt_cache_breakpoint: %#v", systemPart[0]["prompt_cache_breakpoint"])
	}

	if _, exists := messages[1].Content.(string); !exists {
		t.Fatalf("expected user message content string: %#v", messages[1].Content)
	}
}

func TestBuildChatCompletionRequestBodyPlacesStablePrefixCacheBreakpoint(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: false,
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "openai/gpt-test",
		SessionID:       testOpenAIPromptCacheKey,
		Messages: []chatMessage{
			{Role: messageRoleSystem, Content: "You are concise."},
			{Role: messageRoleAssistant, Content: "previous answer"},
			{Role: messageRoleUser, Content: "hello"},
		},
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		PreviousResponseID:          "",
		RequestID:                   "",
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]chatMessage)
	if !messagesOK || len(messages) != 3 {
		t.Fatalf("unexpected messages payload: %#v", requestBody["messages"])
	}

	if _, exists := messages[0].Content.(string); !exists {
		t.Fatalf("expected system message content string: %#v", messages[0].Content)
	}

	if _, exists := messages[1].Content.(string); !exists {
		t.Fatalf("expected assistant message content string: %#v", messages[1].Content)
	}

	userParts, userPartsOK := messages[2].Content.([]map[string]any)
	if !userPartsOK || len(userParts) != 1 {
		t.Fatalf("expected user breakpoint part: %#v", messages[2].Content)
	}

	breakpoint, breakpointOK := userParts[0]["prompt_cache_breakpoint"].(map[string]any)
	if !breakpointOK || breakpoint["mode"] != "explicit" {
		t.Fatalf("unexpected prompt_cache_breakpoint: %#v", userParts[0]["prompt_cache_breakpoint"])
	}
}

func TestBuildChatCompletionRequestBodyExplicitModePlacesBreakpointOnFirstMessage(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: false,
			ExtraBody: map[string]any{
				"prompt_cache_options": map[string]any{
					"mode": "explicit",
				},
			},
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "openai/gpt-test",
		SessionID:       testOpenAIPromptCacheKey,
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: "hello"},
		},
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		PreviousResponseID:          "",
		RequestID:                   "",
	}

	requestBody := buildChatCompletionRequestBody(request)

	cacheOptions, cacheOptionsOK := requestBody["prompt_cache_options"].(map[string]any)
	if !cacheOptionsOK || cacheOptions["mode"] != "explicit" {
		t.Fatalf("unexpected prompt_cache_options payload: %#v", requestBody["prompt_cache_options"])
	}

	if _, exists := requestBody["prompt_cache_breakpoint"]; exists {
		t.Fatalf("unexpected request-level prompt_cache_breakpoint: %#v", requestBody["prompt_cache_breakpoint"])
	}

	messages, messagesOK := requestBody["messages"].([]chatMessage)
	if !messagesOK || len(messages) != 1 {
		t.Fatalf("unexpected messages payload: %#v", requestBody["messages"])
	}

	userParts, userPartsOK := messages[0].Content.([]map[string]any)
	if !userPartsOK || len(userParts) != 1 {
		t.Fatalf("expected user breakpoint part: %#v", messages[0].Content)
	}

	breakpoint, breakpointOK := userParts[0]["prompt_cache_breakpoint"].(map[string]any)
	if !breakpointOK || breakpoint["mode"] != "explicit" {
		t.Fatalf("unexpected prompt_cache_breakpoint: %#v", userParts[0]["prompt_cache_breakpoint"])
	}
}

func TestBuildChatCompletionRequestBodySkipsCacheOptionsForOtherProviders(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name            string
		configuredModel string
	}{
		{
			name:            "groq",
			configuredModel: "groq/gpt-test",
		},
		{
			name:            "azure",
			configuredModel: "azure-openai/gpt-test",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			request := chatCompletionRequest{
				Provider: providerRequestConfig{
					APIKind:         providerAPIKindOpenAI,
					BaseURL:         "https://example.com/v1",
					APIKey:          "test-key",
					UseResponsesAPI: false,
					APIKeys:         nil,
					EnableGrounding: false,
					ExtraHeaders:    nil,
					ExtraQuery:      nil,
					ExtraBody:       nil,
				},
				Model:                       "gpt-test",
				ConfiguredModel:             testCase.configuredModel,
				ContextWindow:               0,
				AutoCompactThresholdPercent: 0,
				SessionID:                   testOpenAIPromptCacheKey,
				PreviousResponseID:          "",
				RequestID:                   "",
				Messages: []chatMessage{
					{Role: messageRoleUser, Content: "hello"},
				},
			}

			requestBody := buildChatCompletionRequestBody(request)

			if _, exists := requestBody["prompt_cache_options"]; exists {
				t.Fatalf(
					"unexpected prompt_cache_options for %s: %#v",
					testCase.configuredModel,
					requestBody["prompt_cache_options"],
				)
			}

			if _, exists := requestBody["prompt_cache_key"]; exists {
				t.Fatalf(
					"unexpected prompt_cache_key for %s: %#v",
					testCase.configuredModel,
					requestBody["prompt_cache_key"],
				)
			}
		})
	}
}

func TestBuildChatCompletionRequestBodySkipsCacheOptionsForResponsesAPI(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: true,
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                       "gpt-test",
		ConfiguredModel:             "openai/gpt-test",
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		SessionID:                   testOpenAIPromptCacheKey,
		PreviousResponseID:          "",
		RequestID:                   "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: "hello"},
		},
	}

	requestBody := buildChatCompletionRequestBody(request)

	if _, exists := requestBody["prompt_cache_options"]; exists {
		t.Fatalf("unexpected prompt_cache_options for responses api: %#v", requestBody["prompt_cache_options"])
	}
}

func TestOpenAIClientStreamChatCompletionParsesCachedUsage(t *testing.T) {
	t.Parallel()

	server := newCachedUsageStreamingTestServer(t)
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         server.URL + "/v1",
			APIKey:          "test-key",
			UseResponsesAPI: false,
			ExtraQuery: map[string]any{
				"api-version": testXAIAPIVersion,
			},
			ExtraHeaders: map[string]any{
				"X-Test": testHeaderPresent,
			},
			ExtraBody: map[string]any{
				"temperature": 0.2,
			},
			APIKeys:         nil,
			EnableGrounding: false,
		},
		Model:                       "gpt-test",
		ConfiguredModel:             "",
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		SessionID:                   "",
		PreviousResponseID:          "",
		RequestID:                   "",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: "hello"},
		},
	}

	var (
		cachedUsage     *tokenUsage
		reasoningTokens int
	)

	err := client.streamChatCompletion(context.Background(), request, func(delta streamDelta) error {
		if delta.Usage != nil {
			cachedUsage = cloneTokenUsage(delta.Usage)
		}

		reasoningTokens = delta.ReasoningTokens

		return nil
	})
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if cachedUsage == nil {
		t.Fatal("expected usage payload")
	}

	if cachedUsage.Input != 2006 || cachedUsage.Output != 300 || cachedUsage.CachedInput != 1920 {
		t.Fatalf("unexpected cached usage: %#v", cachedUsage)
	}

	if reasoningTokens != 84 {
		t.Fatalf("unexpected reasoning tokens: %d", reasoningTokens)
	}
}

func newCachedUsageStreamingTestServer(t *testing.T) *httptest.Server {
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

		writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		flusher.Flush()
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2006,\"completion_tokens\":300,"+
				"\"prompt_tokens_details\":{\"cached_tokens\":1920,\"cache_write_tokens\":0},"+
				"\"completion_tokens_details\":{\"reasoning_tokens\":84}}}\n\n",
		)
		flusher.Flush()
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func TestHandleStreamPayloadPassesReasoningTokens(t *testing.T) {
	t.Parallel()

	var (
		receivedContent   string
		receivedUsage     *tokenUsage
		receivedReasoning int
	)

	err := handleStreamPayload([]byte(`{
		"choices": [],
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 20,
			"prompt_tokens_details": {"cached_tokens": 7},
			"completion_tokens_details": {"reasoning_tokens": 5}
		}
	}`), func(delta streamDelta) error {
		if delta.Content != "" {
			receivedContent += delta.Content
		}

		if delta.Usage != nil {
			receivedUsage = cloneTokenUsage(delta.Usage)
		}

		receivedReasoning = delta.ReasoningTokens

		return nil
	})
	if err != nil {
		t.Fatalf("handle stream payload: %v", err)
	}

	if receivedContent != "" {
		t.Fatalf("unexpected content: %q", receivedContent)
	}

	if receivedUsage == nil || receivedUsage.CachedInput != 7 {
		t.Fatalf("unexpected cached usage: %#v", receivedUsage)
	}

	if receivedReasoning != 5 {
		t.Fatalf("unexpected reasoning tokens: %d", receivedReasoning)
	}
}

func TestBuildChatCompletionRequestBodyRewritesSystemRoleForGPT56(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: false,
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-5.6",
		ConfiguredModel: "openai/gpt-5.6",
		SessionID:       testOpenAIPromptCacheKey,
		Messages: []chatMessage{
			{Role: messageRoleSystem, Content: "You are concise."},
			{Role: messageRoleUser, Content: "hello"},
		},
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		PreviousResponseID:          "",
		RequestID:                   "",
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]chatMessage)
	if !messagesOK || len(messages) != 2 {
		t.Fatalf("unexpected messages payload: %#v", requestBody["messages"])
	}

	if messages[0].Role != "developer" {
		t.Fatalf("expected developer role for gpt-5.6: %#v", messages[0])
	}

	if messages[1].Role != messageRoleUser {
		t.Fatalf("unexpected user role: %#v", messages[1])
	}
}

func TestBuildChatCompletionRequestBodyKeepsSystemRoleForOlderModels(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: false,
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-5.4",
		ConfiguredModel: "openai/gpt-5.4",
		SessionID:       testOpenAIPromptCacheKey,
		Messages: []chatMessage{
			{Role: messageRoleSystem, Content: "You are concise."},
			{Role: messageRoleUser, Content: "hello"},
		},
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		PreviousResponseID:          "",
		RequestID:                   "",
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]chatMessage)
	if !messagesOK || len(messages) != 2 {
		t.Fatalf("unexpected messages payload: %#v", requestBody["messages"])
	}

	if messages[0].Role != messageRoleSystem {
		t.Fatalf("expected system role for gpt-5.4: %#v", messages[0])
	}
}

func TestBuildXAIResponsesRequestBodyAddsCacheOptionsForOpenAIProvider(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: true,
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "openai/gpt-test",
		SessionID:       testOpenAIPromptCacheKey,
		Messages: []chatMessage{
			{Role: messageRoleSystem, Content: "You are concise."},
			{Role: messageRoleUser, Content: "hello"},
		},
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		PreviousResponseID:          "",
		RequestID:                   "",
	}

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	cacheOptions, cacheOptionsOK := requestBody["prompt_cache_options"].(map[string]any)
	if !cacheOptionsOK {
		t.Fatalf("unexpected prompt_cache_options payload: %#v", requestBody["prompt_cache_options"])
	}

	if cacheOptions["mode"] != "implicit" || cacheOptions["ttl"] != "30m" {
		t.Fatalf("unexpected prompt_cache_options: %#v", cacheOptions)
	}

	input, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(input) != 2 {
		t.Fatalf("unexpected input payload: %#v", requestBody["input"])
	}

	if _, exists := input[0]["content"].(string); !exists {
		t.Fatalf("expected system message content string: %#v", input[0]["content"])
	}
}

func TestBuildXAIResponsesRequestBodySkipsCacheOptionsForXAIModels(t *testing.T) {
	t.Parallel()

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://api.x.ai/v1",
			APIKey:          "test-key",
			UseResponsesAPI: true,
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "grok-test",
		ConfiguredModel: "x-ai/grok-test",
		SessionID:       testOpenAIPromptCacheKey,
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: "hello"},
		},
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		PreviousResponseID:          "",
		RequestID:                   "",
	}

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	if _, exists := requestBody["prompt_cache_options"]; exists {
		t.Fatalf("unexpected prompt_cache_options for xAI: %#v", requestBody["prompt_cache_options"])
	}
}

func TestXAIResponsesStreamPayloadDeltaParsesCachedUsage(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"type": "response.completed",
		"response": {
			"id": "resp_test_123",
			"status": "completed",
			"output": [],
			"usage": {
				"input_tokens": 2006,
				"output_tokens": 300,
				"input_tokens_details": {
					"cached_tokens": 1920
				}
			}
		}
	}`)

	delta, terminal, err := xAIResponsesStreamPayloadDelta(payload, newXAIResponsesStreamState())
	if err != nil {
		t.Fatalf("decode xAI responses stream payload: %v", err)
	}

	if !terminal {
		t.Fatal("expected terminal delta")
	}

	if delta.Usage == nil {
		t.Fatal("expected usage payload")
	}

	if delta.Usage.Input != 2006 || delta.Usage.Output != 300 {
		t.Fatalf("unexpected usage: %#v", delta.Usage)
	}

	if delta.Usage.CachedInput != 1920 {
		t.Fatalf("unexpected cached input tokens: %d", delta.Usage.CachedInput)
	}
}

func TestJSONMarshalTokenUsageIncludesCacheFields(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(&struct {
		Input            int `json:"input"`
		Output           int `json:"output"`
		CachedInput      int `json:"cached_input"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	}{
		Input:            12,
		Output:           34,
		CachedInput:      5,
		CacheWriteTokens: 2,
	})
	if err != nil {
		t.Fatalf("marshal token usage: %v", err)
	}

	encodedText := string(encoded)
	if !strings.Contains(encodedText, `"cached_input":5`) {
		t.Fatalf("expected cached_input in JSON: %s", encodedText)
	}

	if !strings.Contains(encodedText, `"cache_write_tokens":2`) {
		t.Fatalf("expected cache_write_tokens in JSON: %s", encodedText)
	}
}

func TestOpenAIStreamPayloadDeltaContentFields(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"choices": [
			{
				"delta": {
					"content": "Hel"
				},
				"finish_reason": null
			}
		]
	}`)

	delta, err := openAIStreamPayloadDelta(payload)
	if err != nil {
		t.Fatalf("decode stream payload: %v", err)
	}

	if delta.Content != "Hel" {
		t.Fatalf("unexpected content: %q", delta.Content)
	}

	if delta.Usage != nil {
		t.Fatalf("unexpected usage: %#v", delta.Usage)
	}
}
