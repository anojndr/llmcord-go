package main

import "testing"

func TestBuildChatCompletionRequestPreservesConfiguredModelForDisplay(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = "https://api.example.com/v1"

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
