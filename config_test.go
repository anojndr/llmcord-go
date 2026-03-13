package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const (
	firstTestModel         = "openai/first-model"
	secondTestModel        = "openai/second-model"
	testMediaAnalysisModel = "google/gemini-3.1-flash-lite-preview"
)

func TestLoadConfigAppliesDefaultsAndPreservesModelOrder(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
client_id: 123456789
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
  openai/second-model:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.BotToken != "discord-token" {
		t.Fatalf("unexpected bot token: %q", loadedConfig.BotToken)
	}

	if loadedConfig.ClientID != "123456789" {
		t.Fatalf("unexpected client id: %q", loadedConfig.ClientID)
	}

	if loadedConfig.MaxText != defaultMaxText {
		t.Fatalf("unexpected max text: %d", loadedConfig.MaxText)
	}

	if loadedConfig.MaxImages != defaultMaxImages {
		t.Fatalf("unexpected max images: %d", loadedConfig.MaxImages)
	}

	if loadedConfig.MaxMessages != defaultMaxMessages {
		t.Fatalf("unexpected max messages: %d", loadedConfig.MaxMessages)
	}

	if !loadedConfig.AllowDMs {
		t.Fatal("expected allow_dms to default to true")
	}

	if !slices.Equal(
		loadedConfig.ModelOrder,
		[]string{firstTestModel, secondTestModel},
	) {
		t.Fatalf("unexpected model order: %#v", loadedConfig.ModelOrder)
	}

	if loadedConfig.SearchDeciderModel != firstTestModel {
		t.Fatalf("unexpected default search decider model: %q", loadedConfig.SearchDeciderModel)
	}

	if loadedConfig.MediaAnalysisModel != "" {
		t.Fatalf("unexpected default media analysis model: %q", loadedConfig.MediaAnalysisModel)
	}

	if loadedConfig.WebSearch.PrimaryProvider != webSearchProviderKindMCP {
		t.Fatalf("unexpected default web search primary provider: %q", loadedConfig.WebSearch.PrimaryProvider)
	}

	if loadedConfig.WebSearch.MaxURLs != defaultWebSearchMaxURLs {
		t.Fatalf("unexpected default web search max URLs: %d", loadedConfig.WebSearch.MaxURLs)
	}
}

func TestLoadConfigRejectsMissingModels(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models: {}
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected missing models to fail validation")
	}
}

func TestLoadConfigUsesConfiguredSearchDeciderModel(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
  openai/second-model:
search_decider_model: openai/second-model
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.SearchDeciderModel != secondTestModel {
		t.Fatalf("unexpected search decider model: %q", loadedConfig.SearchDeciderModel)
	}
}

func TestLoadConfigUsesConfiguredChannelModelLocks(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
  openai/second-model:
channel_model_locks:
  channel-1: openai/second-model
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if lockedModel, ok := loadedConfig.ChannelModelLocks["channel-1"]; !ok || lockedModel != secondTestModel {
		t.Fatalf("unexpected channel model locks: %#v", loadedConfig.ChannelModelLocks)
	}
}

func TestLoadConfigUsesConfiguredMediaAnalysisModel(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  google:
    type: gemini
models:
  ` + testMediaAnalysisModel + `:
media_analysis_model: ` + testMediaAnalysisModel + `
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.MediaAnalysisModel != testMediaAnalysisModel {
		t.Fatalf("unexpected media analysis model: %q", loadedConfig.MediaAnalysisModel)
	}
}

func TestLoadConfigAllowsGeminiProviderWithoutBaseURL(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  google:
    type: gemini
    api_key: gemini-key
models:
  google/gemini-3-flash-preview:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.Providers["google"].apiKind() != providerAPIKindGemini {
		t.Fatalf("unexpected provider API kind: %q", loadedConfig.Providers["google"].apiKind())
	}
}

func TestLoadConfigAllowsProviderAPIKeyLists(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
    api_key:
      - primary-key
      - backup-key
      - primary-key
models:
  openai/first-model:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.Providers["openai"].APIKey != "primary-key" {
		t.Fatalf("unexpected primary provider API key: %q", loadedConfig.Providers["openai"].APIKey)
	}

	if !slices.Equal(
		loadedConfig.Providers["openai"].APIKeys,
		[]string{"primary-key", "backup-key"},
	) {
		t.Fatalf("unexpected provider API keys: %#v", loadedConfig.Providers["openai"].APIKeys)
	}
}

func TestLoadConfigPreservesNestedModelPayloadParameters(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  nvidia:
    base_url: https://integrate.api.nvidia.com/v1
models:
  nvidia/qwen/qwen3.5-397b-a17b:vision:
    chat_template_kwargs:
      enable_thinking: false
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"nvidia/qwen/qwen3.5-397b-a17b:vision",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Model != "qwen/qwen3.5-397b-a17b" {
		t.Fatalf("unexpected request model: %q", request.Model)
	}

	requestBody := buildChatCompletionRequestBody(request)

	chatTemplateKwargs, ok := requestBody["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected chat_template_kwargs payload: %#v", requestBody["chat_template_kwargs"])
	}

	if got, ok := chatTemplateKwargs["enable_thinking"].(bool); !ok || got {
		t.Fatalf("unexpected enable_thinking payload: %#v", chatTemplateKwargs["enable_thinking"])
	}
}

func TestLoadConfigAllowsTavilyWebSearchAPIKeyLists(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
web_search:
  tavily:
    api_key:
      - tavily-primary
      - tavily-backup
      - tavily-primary
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.WebSearch.Tavily.APIKey != "tavily-primary" {
		t.Fatalf("unexpected primary Tavily API key: %q", loadedConfig.WebSearch.Tavily.APIKey)
	}

	if !slices.Equal(
		loadedConfig.WebSearch.Tavily.APIKeys,
		[]string{"tavily-primary", "tavily-backup"},
	) {
		t.Fatalf("unexpected Tavily API keys: %#v", loadedConfig.WebSearch.Tavily.APIKeys)
	}
}

func TestLoadConfigUsesConfiguredPrimaryWebSearchProvider(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
web_search:
  primary_provider: tavily
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.WebSearch.PrimaryProvider != webSearchProviderKindTavily {
		t.Fatalf("unexpected web search primary provider: %q", loadedConfig.WebSearch.PrimaryProvider)
	}
}

func TestLoadConfigUsesConfiguredWebSearchMaxURLs(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
web_search:
  max_urls: 9
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.WebSearch.MaxURLs != 9 {
		t.Fatalf("unexpected web search max URLs: %d", loadedConfig.WebSearch.MaxURLs)
	}
}

func TestLoadConfigAllowsOpenAICodexProviderWithoutBaseURL(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  codex:
    type: openai-codex
    api_key: test-token
models:
  codex/gpt-5.2-codex:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.Providers["codex"].apiKind() != providerAPIKindOpenAICodex {
		t.Fatalf("unexpected provider API kind: %q", loadedConfig.Providers["codex"].apiKind())
	}
}

func TestLoadConfigRejectsUnsupportedProviderType(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  provider:
    type: unsupported
    base_url: https://api.example.com/v1
models:
  provider/model:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected unsupported provider type to fail validation")
	}
}

func TestLoadConfigRejectsUnsupportedWebSearchPrimaryProvider(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
web_search:
  primary_provider: unsupported
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected unsupported web search primary provider to fail validation")
	}
}

func TestLoadConfigRejectsNonPositiveWebSearchMaxURLs(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
web_search:
  max_urls: 0
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected non-positive web search max URLs to fail validation")
	}
}

func TestLoadConfigRejectsNonGeminiMediaAnalysisModel(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
media_analysis_model: openai/first-model
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected non-gemini media analysis model to fail validation")
	}
}

func TestLoadConfigRejectsUnknownChannelModelLock(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
channel_model_locks:
  channel-1: openai/second-model
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected unknown channel lock model to fail validation")
	}
}

func TestSystemPromptNowReplacesDateAndTime(t *testing.T) {
	t.Parallel()

	instant := time.Date(2026, time.March, 9, 13, 14, 15, 0, time.FixedZone("PHT", 8*60*60))
	prompt := systemPromptNow(
		"Today is {date} and the time is {time}.",
		instant,
	)

	expectedPrompt := "Today is March 09 2026 and the time is 13:14:15 PHT+0800."
	if prompt != expectedPrompt {
		t.Fatalf("unexpected rendered prompt: %q", prompt)
	}
}
