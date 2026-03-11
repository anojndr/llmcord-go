package main

import (
	"slices"
	"testing"

	"google.golang.org/genai"
)

func TestBuildChatCompletionRequestPreservesConfiguredModelForDisplay(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = testOpenAIBaseURL

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/gpt-5.1": {
			"temperature": 0.2,
		},
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"openai/gpt-5.1",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Model != "gpt-5.1" {
		t.Fatalf("unexpected request model: %q", request.Model)
	}

	if request.ConfiguredModel != "openai/gpt-5.1" {
		t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
	}
}

func TestBuildChatCompletionRequestPreservesProviderAPIKeys(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = testOpenAIBaseURL
	provider.APIKeys = []string{"primary-key", "backup-key"}

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/gpt-5.1": nil,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"openai/gpt-5.1",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Provider.APIKey != "primary-key" {
		t.Fatalf("unexpected primary API key: %q", request.Provider.APIKey)
	}

	if !slices.Equal(request.Provider.APIKeys, []string{"primary-key", "backup-key"}) {
		t.Fatalf("unexpected provider API keys: %#v", request.Provider.APIKeys)
	}
}

func TestBuildChatCompletionRequestNormalizesGeminiThinkingAlias(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.Type = string(providerAPIKindGemini)
	provider.ExtraBody = map[string]any{
		"temperature": 0.2,
	}

	modelParameters := map[string]any{
		"thinkingConfig": map[string]any{
			"includeThoughts": true,
		},
	}

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"google": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"google/gemini-3.1-flash-lite-preview-minimal": modelParameters,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"google/gemini-3.1-flash-lite-preview-minimal",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Model != "gemini-3.1-flash-lite-preview" {
		t.Fatalf("unexpected request model: %q", request.Model)
	}

	if request.ConfiguredModel != "google/gemini-3.1-flash-lite-preview-minimal" {
		t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
	}

	if got, ok := request.Provider.ExtraBody["temperature"].(float64); !ok || got != 0.2 {
		t.Fatalf("unexpected provider extra body: %#v", request.Provider.ExtraBody)
	}

	thinkingConfig, thinkingConfigOK := request.Provider.ExtraBody["thinkingConfig"].(map[string]any)
	if !thinkingConfigOK {
		t.Fatalf("unexpected thinking config: %#v", request.Provider.ExtraBody["thinkingConfig"])
	}

	if thinkingConfig["includeThoughts"] != true {
		t.Fatalf("unexpected thinking config contents: %#v", thinkingConfig)
	}

	if thinkingConfig["thinkingLevel"] != genai.ThinkingLevelMinimal {
		t.Fatalf("unexpected thinking level: %#v", thinkingConfig["thinkingLevel"])
	}

	if _, ok := modelParameters["thinkingLevel"]; ok {
		t.Fatalf("unexpected mutation of model parameters: %#v", modelParameters)
	}

	originalThinkingConfig, ok := modelParameters["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected original thinking config: %#v", modelParameters["thinkingConfig"])
	}

	if _, ok := originalThinkingConfig["thinkingLevel"]; ok {
		t.Fatalf("unexpected mutation of original thinking config: %#v", originalThinkingConfig)
	}
}

func TestBuildChatCompletionRequestRejectsGeminiThinkingAliasWithInvalidThinkingConfig(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.Type = string(providerAPIKindGemini)

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"google": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"google/gemini-3.1-flash-lite-preview-minimal": {
			"thinkingConfig": "invalid",
		},
	}

	_, err := buildChatCompletionRequest(
		loadedConfig,
		"google/gemini-3.1-flash-lite-preview-minimal",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err == nil {
		t.Fatal("expected invalid thinkingConfig to fail")
	}
}

func TestBuildChatCompletionRequestNormalizesOpenAICodexReasoningAlias(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.Type = string(providerAPIKindOpenAICodex)
	provider.ExtraBody = map[string]any{
		"verbosity":        "medium",
		"reasoning_effort": "medium",
	}

	modelParameters := map[string]any{
		"reasoning": map[string]any{
			"summary": "concise",
			"effort":  "high",
		},
	}

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"codex": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"codex/gpt-5.4-none": modelParameters,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"codex/gpt-5.4-none",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Model != "gpt-5.4" {
		t.Fatalf("unexpected request model: %q", request.Model)
	}

	if request.ConfiguredModel != "codex/gpt-5.4-none" {
		t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
	}

	if request.Provider.ExtraBody["verbosity"] != "medium" {
		t.Fatalf("unexpected provider extra body: %#v", request.Provider.ExtraBody)
	}

	if _, ok := request.Provider.ExtraBody["reasoning_effort"]; ok {
		t.Fatalf("unexpected top-level reasoning_effort: %#v", request.Provider.ExtraBody)
	}

	reasoningConfig, reasoningConfigOK := request.Provider.ExtraBody["reasoning"].(map[string]any)
	if !reasoningConfigOK {
		t.Fatalf("unexpected reasoning config: %#v", request.Provider.ExtraBody["reasoning"])
	}

	if reasoningConfig["summary"] != "concise" {
		t.Fatalf("unexpected reasoning summary: %#v", reasoningConfig["summary"])
	}

	if reasoningConfig["effort"] != "none" {
		t.Fatalf("unexpected reasoning effort: %#v", reasoningConfig["effort"])
	}

	originalReasoningConfig, ok := modelParameters["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected original reasoning config: %#v", modelParameters["reasoning"])
	}

	if originalReasoningConfig["effort"] != "high" {
		t.Fatalf("unexpected mutation of original reasoning config: %#v", originalReasoningConfig)
	}
}
