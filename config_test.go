package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

	assertDefaultLoadedConfig(t, loadedConfig)

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

	if loadedConfig.WebSearch.Exa.TextMaxCharacters != defaultExaSearchTextMaxCharacters {
		t.Fatalf(
			"unexpected default Exa text max characters: %d",
			loadedConfig.WebSearch.Exa.TextMaxCharacters,
		)
	}

	if loadedConfig.Database.ConnectionString != "" {
		t.Fatalf(
			"unexpected default database connection string: %q",
			loadedConfig.Database.ConnectionString,
		)
	}

	if loadedConfig.Database.StoreKey != "" {
		t.Fatalf("unexpected default database store key: %q", loadedConfig.Database.StoreKey)
	}

	if loadedConfig.AutoCompactThresholdPercent != autoCompactDefaultThresholdPercent {
		t.Fatalf(
			"unexpected default auto compact threshold percent: %d",
			loadedConfig.AutoCompactThresholdPercent,
		)
	}
}

func assertDefaultLoadedConfig(t *testing.T, loadedConfig config) {
	t.Helper()

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
}

func TestLoadConfigUsesConfiguredDatabaseConnectionString(t *testing.T) {
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
database:
  connection_string: postgresql://localhost:5432/llmcordgo?sslmode=disable
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.Database.ConnectionString !=
		"postgresql://localhost:5432/llmcordgo?sslmode=disable" {
		t.Fatalf(
			"unexpected database connection string: %q",
			loadedConfig.Database.ConnectionString,
		)
	}
}

func TestLoadConfigUsesConfiguredDatabaseStoreKey(t *testing.T) {
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
database:
  store_key: ` + testSharedHomeBotsStoreKey + `
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.Database.StoreKey != testSharedHomeBotsStoreKey {
		t.Fatalf("unexpected database store key: %q", loadedConfig.Database.StoreKey)
	}
}

func TestLoadConfigRejectsWhitespaceOnlyDatabaseStoreKey(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := "\n" +
		"bot_token: discord-token\n" +
		"providers:\n" +
		"  openai:\n" +
		"    base_url: https://api.example.com/v1\n" +
		"models:\n" +
		"  openai/first-model:\n" +
		"database:\n" +
		"  store_key: \"   \"\n"

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected whitespace-only database store key to fail validation")
	}
}

func TestLoadConfigRejectsUnsupportedDatabaseConnectionStringScheme(t *testing.T) {
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
database:
  connection_string: mysql://localhost:3306/llmcordgo
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected unsupported database scheme to fail validation")
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

func TestLoadConfigAllowsExaProviderWithoutBaseURL(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  exa:
    type: exa
    api_key: exa-key
models:
  exa/exa-research-pro:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	provider := loadedConfig.Providers["exa"]
	if provider.apiKind() != providerAPIKindOpenAI {
		t.Fatalf("unexpected provider API kind: %q", provider.apiKind())
	}

	if provider.BaseURL != defaultExaResearchBaseURL {
		t.Fatalf("unexpected Exa base URL: %q", provider.BaseURL)
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"exa/exa-research-pro",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Model != "exa-research-pro" {
		t.Fatalf("unexpected request model: %q", request.Model)
	}

	if request.Provider.APIKind != providerAPIKindOpenAI {
		t.Fatalf("unexpected request provider API kind: %q", request.Provider.APIKind)
	}

	if request.Provider.BaseURL != defaultExaResearchBaseURL {
		t.Fatalf("unexpected request provider base URL: %q", request.Provider.BaseURL)
	}

	requestURL, err := buildChatCompletionURL(request.Provider.BaseURL, request.Provider.ExtraQuery)
	if err != nil {
		t.Fatalf("build chat completion URL: %v", err)
	}

	if requestURL != defaultExaResearchBaseURL+"/chat/completions" {
		t.Fatalf("unexpected Exa chat completion URL: %q", requestURL)
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
      - ` + testTavilyPrimaryAPIKey + `
      - ` + testTavilyBackupAPIKey + `
      - ` + testTavilyPrimaryAPIKey + `
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

	if loadedConfig.Providers["openai"].APIKey != testTavilyPrimaryAPIKey {
		t.Fatalf("unexpected primary provider API key: %q", loadedConfig.Providers["openai"].APIKey)
	}

	if !slices.Equal(
		loadedConfig.Providers["openai"].APIKeys,
		[]string{testTavilyPrimaryAPIKey, testTavilyBackupAPIKey},
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

func TestLoadConfigAllowsExaWebSearchAPIKeyLists(t *testing.T) {
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
  exa:
    api_key:
      - exa-primary
      - exa-backup
      - exa-primary
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.WebSearch.Exa.APIKey != "exa-primary" {
		t.Fatalf("unexpected primary Exa API key: %q", loadedConfig.WebSearch.Exa.APIKey)
	}

	if !slices.Equal(
		loadedConfig.WebSearch.Exa.APIKeys,
		[]string{"exa-primary", "exa-backup"},
	) {
		t.Fatalf("unexpected Exa API keys: %#v", loadedConfig.WebSearch.Exa.APIKeys)
	}
}

func TestLoadConfigAllowsSerpAPIVisualSearchAPIKeyLists(t *testing.T) {
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
visual_search:
  serpapi:
    api_key:
      - serp-primary
      - serp-backup
      - serp-primary
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.VisualSearch.SerpAPI.APIKey != "serp-primary" {
		t.Fatalf("unexpected primary SerpApi visual search API key: %q", loadedConfig.VisualSearch.SerpAPI.APIKey)
	}

	if !slices.Equal(
		loadedConfig.VisualSearch.SerpAPI.APIKeys,
		[]string{"serp-primary", "serp-backup"},
	) {
		t.Fatalf(
			"unexpected SerpApi visual search API keys: %#v",
			loadedConfig.VisualSearch.SerpAPI.APIKeys,
		)
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

func TestLoadConfigUsesConfiguredExaWebSearchTextMaxCharacters(t *testing.T) {
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
  exa:
    text_max_characters: 9000
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.WebSearch.Exa.TextMaxCharacters != 9000 {
		t.Fatalf("unexpected Exa text max characters: %d", loadedConfig.WebSearch.Exa.TextMaxCharacters)
	}
}

func TestLoadConfigInheritsContextWindowAcrossAliasModels(t *testing.T) {
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
  codex/gpt-5.4:
    context_window: 400000
  codex/gpt-5.4-none:
  codex/gpt-5.4-high:
    context_window: 400000
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	for _, modelName := range []string{
		"codex/gpt-5.4",
		"codex/gpt-5.4-none",
		"codex/gpt-5.4-high",
	} {
		if loadedConfig.modelContextWindow(modelName) != 400_000 {
			t.Fatalf("unexpected context window for %s: %d", modelName, loadedConfig.modelContextWindow(modelName))
		}
	}
}

func TestLoadConfigInheritsContextWindowAcrossOpenAIAliases(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.openai.com/v1
models:
  openai/gpt-5.4:
    context_window: 1050000
  openai/gpt-5.4-low:
  openai/gpt-5.4-none:
  openai/gpt-5.4-xhigh:
    context_window: 1050000
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	for _, modelName := range []string{
		"openai/gpt-5.4",
		"openai/gpt-5.4-low",
		"openai/gpt-5.4-none",
		"openai/gpt-5.4-xhigh",
	} {
		if loadedConfig.modelContextWindow(modelName) != 1_050_000 {
			t.Fatalf("unexpected context window for %s: %d", modelName, loadedConfig.modelContextWindow(modelName))
		}
	}
}

func TestLoadConfigInheritsContextWindowAcrossOpenAICompatibleAliases(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/gpt-5.4:
    context_window: 1050000
  openai/gpt-5.4-low:
  openai/gpt-5.4-none:
    context_window: 1050000
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	for _, modelName := range []string{
		"openai/gpt-5.4",
		"openai/gpt-5.4-low",
		"openai/gpt-5.4-none",
	} {
		if loadedConfig.modelContextWindow(modelName) != 1_050_000 {
			t.Fatalf("unexpected context window for %s: %d", modelName, loadedConfig.modelContextWindow(modelName))
		}
	}
}

func TestLoadConfigInheritsContextWindowAcrossGeminiAliases(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  google:
    type: gemini
    api_key: test-token
models:
  google/gemini-3.1-flash-lite-preview:
    context_window: 1000000
  google/gemini-3.1-flash-lite-preview-minimal:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.modelContextWindow("google/gemini-3.1-flash-lite-preview-minimal") != 1_000_000 {
		t.Fatalf(
			"unexpected gemini alias context window: %d",
			loadedConfig.modelContextWindow("google/gemini-3.1-flash-lite-preview-minimal"),
		)
	}
}

func TestLoadConfigUsesConfiguredAutoCompactThresholdPercent(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
auto_compact_threshold_percent: 75
models:
  openai/gpt-5.1:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.AutoCompactThresholdPercent != 75 {
		t.Fatalf(
			"unexpected auto compact threshold percent: %d",
			loadedConfig.AutoCompactThresholdPercent,
		)
	}
}

func TestLoadConfigRejectsModelLocalAutoCompactThresholdPercent(t *testing.T) {
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
  codex/gpt-5.4:
    auto_compact_threshold_percent: 80
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected model-local auto compact threshold percent to fail validation")
	}
}

func TestLoadConfigRejectsMismatchedContextWindowAcrossAliasModels(t *testing.T) {
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
  codex/gpt-5.4:
    context_window: 400000
  codex/gpt-5.4-none:
    context_window: 200000
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected mismatched alias context window to fail validation")
	}
}

func TestLoadConfigRejectsMismatchedContextWindowAcrossOpenAIAliases(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.openai.com/v1
models:
  openai/gpt-5.4:
    context_window: 1050000
  openai/gpt-5.4-none:
    context_window: 400000
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected mismatched OpenAI alias context window to fail validation")
	}
}

func TestLoadConfigRejectsNonPositiveContextWindow(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/gpt-5.1:
    context_window: 0
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected non-positive context window to fail validation")
	}
}

func TestLoadConfigRejectsOutOfRangeAutoCompactThresholdPercent(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
auto_compact_threshold_percent: 101
models:
  openai/gpt-5.1:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected out-of-range auto compact threshold percent to fail validation")
	}
}

func TestLoadConfigRejectsNonPositiveAutoCompactThresholdPercent(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
auto_compact_threshold_percent: 0
models:
  openai/gpt-5.1:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected non-positive auto compact threshold percent to fail validation")
	}
}

func TestAnyPositiveIntValueRejectsNonPositiveValues(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		value any
	}{
		{name: "int zero", value: 0},
		{name: "int64 zero", value: int64(0)},
		{name: "uint64 zero", value: uint64(0)},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := anyPositiveIntValue(testCase.value)
			if err == nil {
				t.Fatalf("expected non-positive value %T to fail validation", testCase.value)
			}
		})
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

func TestLoadConfigRejectsDeprecatedFacebookSettingsSection(t *testing.T) {
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
facebook:
  deprecated: true
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected deprecated facebook settings section to fail validation")
	}

	if !strings.Contains(err.Error(), "facebook settings are no longer supported") {
		t.Fatalf("unexpected error: %v", err)
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

func TestLoadConfigRejectsNonPositiveExaWebSearchTextMaxCharacters(t *testing.T) {
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
  exa:
    text_max_characters: 0
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected non-positive Exa text max characters to fail validation")
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
