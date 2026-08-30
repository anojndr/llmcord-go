package providers

import (
	"reflect"

	searchtypes "llmcord-go/internal/searchtypes"
	"testing"
)

func TestBuildChatCompletionRequestBodySkipsCacheOptionsWithoutSessionID(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
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
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
		RequestID: "",
		Tools:     nil,
	}

	requestBody := buildChatCompletionRequestBody(request)

	if _, exists := requestBody["prompt_cache_options"]; exists {
		t.Fatalf("unexpected prompt_cache_options without session id: %#v", requestBody["prompt_cache_options"])
	}

	if _, exists := requestBody["prompt_cache_key"]; exists {
		t.Fatalf("unexpected prompt_cache_key without session id: %#v", requestBody["prompt_cache_key"])
	}

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 1 {
		t.Fatalf("unexpected messages Payload: %#v", requestBody["messages"])
	}

	if _, exists := messages[0].Content.(string); !exists {
		t.Fatalf("expected plain string content without cache session: %#v", messages[0].Content)
	}
}

func TestBuildChatCompletionRequestBodySkipsCacheBreakpointWithoutSessionID(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
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
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
		RequestID: "",
		Tools:     nil,
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 2 {
		t.Fatalf("unexpected messages Payload: %#v", requestBody["messages"])
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

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
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
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
		RequestID: "",
		Tools:     nil,
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 2 {
		t.Fatalf("unexpected messages Payload: %#v", requestBody["messages"])
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

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
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
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
		RequestID: "",
		Tools:     nil,
	}

	requestBody := buildChatCompletionRequestBody(request)

	cacheOptions, cacheOptionsOK := requestBody["prompt_cache_options"].(map[string]any)
	if !cacheOptionsOK {
		t.Fatalf("unexpected prompt_cache_options Payload: %#v", requestBody["prompt_cache_options"])
	}

	if cacheOptions["mode"] != "implicit" {
		t.Fatalf("unexpected prompt_cache_options mode: %#v", cacheOptions["mode"])
	}

	if cacheOptions["ttl"] != "30m" {
		t.Fatalf("unexpected prompt_cache_options ttl: %#v", cacheOptions["ttl"])
	}

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 2 {
		t.Fatalf("unexpected messages Payload: %#v", requestBody["messages"])
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

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
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
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{Role: searchtypes.MessageRoleAssistant, Content: "previous answer"},
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
		RequestID: "",
		Tools:     nil,
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 3 {
		t.Fatalf("unexpected messages Payload: %#v", requestBody["messages"])
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

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
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
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
		RequestID: "",
		Tools:     nil,
	}

	requestBody := buildChatCompletionRequestBody(request)

	cacheOptions, cacheOptionsOK := requestBody["prompt_cache_options"].(map[string]any)
	if !cacheOptionsOK || cacheOptions["mode"] != "explicit" {
		t.Fatalf("unexpected prompt_cache_options Payload: %#v", requestBody["prompt_cache_options"])
	}

	if _, exists := requestBody["prompt_cache_breakpoint"]; exists {
		t.Fatalf("unexpected request-level prompt_cache_breakpoint: %#v", requestBody["prompt_cache_breakpoint"])
	}

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 1 {
		t.Fatalf("unexpected messages Payload: %#v", requestBody["messages"])
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

			request := ChatCompletionRequest{
				Provider: ProviderRequestConfig{
					API:             "",
					APIKind:         ProviderAPIKindOpenAI,
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
				ConfiguredModel: testCase.configuredModel,
				SessionID:       testOpenAIPromptCacheKey,
				RequestID:       "",
				Tools:           nil,
				Messages: []ChatMessage{
					{Role: searchtypes.MessageRoleUser, Content: "hello"},
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

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
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
		RequestID:       "",
		Tools:           nil,
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
	}

	requestBody := buildChatCompletionRequestBody(request)

	if _, exists := requestBody["prompt_cache_options"]; exists {
		t.Fatalf("unexpected prompt_cache_options for responses api: %#v", requestBody["prompt_cache_options"])
	}
}

func TestBuildChatCompletionRequestBodyRewritesSystemRoleForGPT56(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
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
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
		RequestID: "",
		Tools:     nil,
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 2 {
		t.Fatalf("unexpected messages Payload: %#v", requestBody["messages"])
	}

	if messages[0].Role != "developer" {
		t.Fatalf("expected developer role for gpt-5.6: %#v", messages[0])
	}

	if messages[1].Role != searchtypes.MessageRoleUser {
		t.Fatalf("unexpected user role: %#v", messages[1])
	}
}

func TestBuildChatCompletionRequestBodyKeepsSystemRoleForOlderModels(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
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
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
		RequestID: "",
		Tools:     nil,
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 2 {
		t.Fatalf("unexpected messages Payload: %#v", requestBody["messages"])
	}

	if messages[0].Role != searchtypes.MessageRoleSystem {
		t.Fatalf("expected system role for gpt-5.4: %#v", messages[0])
	}
}

func TestBuildResponsesRequestBodyAddsCacheOptionsForOpenAIProvider(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
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
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
		RequestID: "",
		Tools:     nil,
	}

	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build responses request body: %v", err)
	}

	cacheOptions, cacheOptionsOK := requestBody["prompt_cache_options"].(map[string]any)
	if !cacheOptionsOK {
		t.Fatalf("unexpected prompt_cache_options Payload: %#v", requestBody["prompt_cache_options"])
	}

	if cacheOptions["mode"] != "implicit" || cacheOptions["ttl"] != "30m" {
		t.Fatalf("unexpected prompt_cache_options: %#v", cacheOptions)
	}

	input, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(input) != 2 {
		t.Fatalf("unexpected input Payload: %#v", requestBody["input"])
	}

	if _, exists := input[0]["content"].(string); !exists {
		t.Fatalf("expected system message content string: %#v", input[0]["content"])
	}
}

func TestBuildResponsesRequestBodySkipsCacheOptionsForNonOpenAIModels(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			API:             "",
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: true,
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody: map[string]any{
				"prompt_cache_options": map[string]any{
					"mode": "implicit",
				},
			},
		},
		Model:           "gpt-test",
		ConfiguredModel: "compatible/gpt-test",
		SessionID:       testOpenAIPromptCacheKey,
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
		},
		RequestID: "",
		Tools:     nil,
	}

	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build responses request body: %v", err)
	}

	// A non-built-in-OpenAI configured model gets no derived caching: no
	// prompt_cache_key and no cache breakpoint inserted into the input.
	// (prompt_cache_options in the body would only echo the user's own
	// extra_body verbatim, so it is not asserted here.)
	if _, exists := requestBody["prompt_cache_key"]; exists {
		t.Fatalf("unexpected prompt_cache_key: %#v", requestBody["prompt_cache_key"])
	}

	input, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(input) != 1 {
		t.Fatalf("unexpected input Payload: %#v", requestBody["input"])
	}

	content, contentOK := input[0]["content"].(string)
	if !contentOK || content != "hello" {
		t.Fatalf("expected plain user content without a cache breakpoint: %#v", input[0]["content"])
	}
}

func TestResponsesStreamPayloadDeltaMarksCompletedEventTerminal(t *testing.T) {
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

	_, terminal, err := responsesStreamPayloadDelta(payload, newResponsesStreamState())
	if err != nil {
		t.Fatalf("decode responses stream Payload: %v", err)
	}

	if !terminal {
		t.Fatal("expected terminal delta")
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

	delta, err := openAIStreamPayloadDelta(payload, newChatCompletionsToolCallAccumulator())
	if err != nil {
		t.Fatalf("decode stream Payload: %v", err)
	}

	if delta.Content != "Hel" {
		t.Fatalf("unexpected content: %q", delta.Content)
	}
}

func TestOpenAIResponsesCacheBreakpointMessagesUncomparableContent(t *testing.T) {
	t.Parallel()

	// Regression test: when the breakpoint message's Content is a slice
	// ([]map[string]any or []ContentPart), the breakpoint comparison used to
	// panic with "runtime error: comparing uncomparable type ...". Slices
	// that are already fully marked must pass through unchanged instead.
	messages := []ChatMessage{
		{Role: searchtypes.MessageRoleUser, Content: "older user"},
		{Role: searchtypes.MessageRoleAssistant, Content: "assistant turn"},
		{Role: searchtypes.MessageRoleUser, Content: []map[string]any{
			{"type": responsesInputTextType, "text": "latest user"},
			{
				"type": responsesInputTextType,
				"text": "second part",
				openAICacheBreakpointKey: map[string]any{
					openAICacheOptionsModeKey: openAICacheBreakpointModeExplicit,
				},
			},
		}},
	}

	assertSameContent := func(t *testing.T, before, after []ChatMessage) {
		t.Helper()

		if len(before) != len(after) {
			t.Fatalf(
				"expected post-breakpoint message count to match input: got %d, want %d",
				len(after),
				len(before),
			)
		}

		for index := range before {
			if after[index].Role != before[index].Role {
				t.Fatalf("message %d role changed: got %q, want %q", index, after[index].Role, before[index].Role)
			}

			if !reflect.DeepEqual(after[index].Content, before[index].Content) {
				t.Fatalf("message %d content changed: got %#v, want %#v", index, after[index].Content, before[index].Content)
			}
		}
	}

	// The stable-prefix breakpoint lands on the message after the last
	// assistant turn, i.e. the tail user slice message. Its last part is
	// already marked, so the conversion is a pass-through that must not
	// panic while comparing the uncomparable slice.
	normalized := openAIResponsesCacheBreakpointMessages(messages)
	assertSameContent(t, messages, normalized)
}

func TestOpenAIResponsesCacheBreakpointMessagesThreadsThroughMapSlice(t *testing.T) {
	t.Parallel()

	// An unmarked tail part gets marked once; the breakpoint is sitting on
	// the message after the last assistant turn, whose Content is an
	// uncomparable []map[string]any slice. The comparison must not panic.
	messages := []ChatMessage{
		{Role: searchtypes.MessageRoleUser, Content: "older user"},
		{Role: searchtypes.MessageRoleAssistant, Content: "assistant turn"},
		{Role: searchtypes.MessageRoleUser, Content: []map[string]any{
			{"type": responsesInputTextType, "text": "latest user"},
			{"type": responsesInputTextType, "text": "plain tail"},
		}},
	}

	normalized := openAIResponsesCacheBreakpointMessages(messages)

	content, ok := normalized[2].Content.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map[string]any content, got %T", normalized[2].Content)
	}

	lastPart := content[len(content)-1]
	if _, marked := lastPart[openAICacheBreakpointKey]; !marked {
		t.Fatal("expected last content part to be marked with a cache breakpoint")
	}
}
