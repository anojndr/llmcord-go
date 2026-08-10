package config

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
	testMediaAnalysisModel = "gemini/gemini-3.5-flash-lite"
)

func TestLoadConfigAppliesDefaultsAndPreservesModelOrder(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
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

	if loadedConfig.WebSearch.PrimaryProvider != WebSearchProviderKindMCP {
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

	if loadedConfig.WebSearch.Exa.LivecrawlTimeoutMS != defaultExaContentsLivecrawlTimeoutMS {
		t.Fatalf(
			"unexpected default Exa livecrawl timeout: %d",
			loadedConfig.WebSearch.Exa.LivecrawlTimeoutMS,
		)
	}

	if len(loadedConfig.WebSearch.Firecrawl.APIKeys) != 0 {
		t.Fatalf(
			"unexpected default Firecrawl API keys: %#v",
			loadedConfig.WebSearch.Firecrawl.APIKeys,
		)
	}

	if loadedConfig.WebSearch.Firecrawl.MaxMarkdownCharacters != defaultFirecrawlMaxMarkdownCharacters {
		t.Fatalf(
			"unexpected default Firecrawl max markdown characters: %d",
			loadedConfig.WebSearch.Firecrawl.MaxMarkdownCharacters,
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
}

func assertDefaultLoadedConfig(t *testing.T, loadedConfig Config) {
	t.Helper()

	if loadedConfig.BotToken != "discord-token" {
		t.Fatalf("unexpected bot token: %q", loadedConfig.BotToken)
	}

	if loadedConfig.ClientID != "123456789" {
		t.Fatalf("unexpected client id: %q", loadedConfig.ClientID)
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
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.Database.ConnectionString !=
		"postgresql://localhost:5432/llmcordgo?sslmode=disable" {
		t.Fatalf(
			"unexpected database connection string: %q",
			loadedConfig.Database.ConnectionString,
		)
	}
}

func TestLoadConfigUsesConfiguredGistSettings(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
gist:
  endpoint: https://gist.example.com/api/v3/gists
  api_key: abc123
  public: true
  description: llmcord-go reply
  filename: reply.md
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.Gist.Endpoint != "https://gist.example.com/api/v3/gists" {
		t.Fatalf("unexpected gist endpoint: %q", loadedConfig.Gist.Endpoint)
	}

	if loadedConfig.Gist.APIKey != "abc123" {
		t.Fatalf("unexpected gist api key: %q", loadedConfig.Gist.APIKey)
	}

	if !loadedConfig.Gist.Public {
		t.Fatal("expected public gist setting")
	}

	if loadedConfig.Gist.Description != "llmcord-go reply" {
		t.Fatalf("unexpected gist description: %q", loadedConfig.Gist.Description)
	}

	if loadedConfig.Gist.Filename != "reply.md" {
		t.Fatalf("unexpected gist filename: %q", loadedConfig.Gist.Filename)
	}
}

func TestLoadConfigDefaultsGistSettingsWhenUnset(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.Gist.Endpoint != defaultGithubGistEndpoint {
		t.Fatalf("unexpected default gist endpoint: %q", loadedConfig.Gist.Endpoint)
	}

	if loadedConfig.Gist.APIKey != "" {
		t.Fatalf("unexpected default gist api key: %q", loadedConfig.Gist.APIKey)
	}

	if loadedConfig.Gist.Public {
		t.Fatal("expected private gist by default")
	}

	if loadedConfig.Gist.Description != "" {
		t.Fatalf("unexpected default gist description: %q", loadedConfig.Gist.Description)
	}

	if loadedConfig.Gist.Filename != defaultGistFilename {
		t.Fatalf("unexpected default gist filename: %q", loadedConfig.Gist.Filename)
	}
}

func TestLoadConfigUsesConfiguredDatabaseStoreKey(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
database:
  store_key: shared-home-bots
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.Database.StoreKey != "shared-home-bots" {
		t.Fatalf("unexpected database store key: %q", loadedConfig.Database.StoreKey)
	}
}

func TestLoadConfigRejectsWhitespaceOnlyDatabaseStoreKey(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected whitespace-only database store key to fail validation")
	}
}

func TestLoadConfigRejectsUnsupportedDatabaseConnectionStringScheme(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected unsupported database scheme to fail validation")
	}
}

func TestLoadConfigRejectsMissingModels(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models: {}
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected missing models to fail validation")
	}
}

func TestLoadConfigUsesConfiguredSearchDeciderModel(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.SearchDeciderModel != secondTestModel {
		t.Fatalf("unexpected search decider model: %q", loadedConfig.SearchDeciderModel)
	}
}

func TestLoadConfigUsesConfiguredChannelModelLocks(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if lockedModel, ok := loadedConfig.ChannelModelLocks["channel-1"]; !ok || lockedModel != secondTestModel {
		t.Fatalf("unexpected channel model locks: %#v", loadedConfig.ChannelModelLocks)
	}
}

func TestLoadConfigUsesConfiguredMediaAnalysisModel(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  gemini:
models:
  ` + testMediaAnalysisModel + `:
media_analysis_model: ` + testMediaAnalysisModel + `
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.MediaAnalysisModel != testMediaAnalysisModel {
		t.Fatalf("unexpected media analysis model: %q", loadedConfig.MediaAnalysisModel)
	}
}

func TestLoadConfigAllowsGeminiProviderWithoutBaseURL(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  gemini:
    api_key: gemini-key
models:
  gemini/gemini-3-flash-preview:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.Providers["gemini"].apiKind() != ProviderAPIKindGemini {
		t.Fatalf("unexpected provider API kind: %q", loadedConfig.Providers["gemini"].apiKind())
	}
}

func TestLoadConfigAllowsExaProviderWithoutBaseURL(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  exa:
    api_key: exa-key
models:
  exa/exa-research-pro:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	provider := loadedConfig.Providers["exa"]
	if provider.apiKind() != ProviderAPIKindOpenAI {
		t.Fatalf("unexpected provider API kind: %q", provider.apiKind())
	}

	if provider.BaseURL != defaultExaResearchBaseURL {
		t.Fatalf("unexpected Exa base URL: %q", provider.BaseURL)
	}

	providerAPIKind, modelName, err := ResolveProviderModelAPIKind(loadedConfig, "exa/exa-research-pro")
	if err != nil {
		t.Fatalf("resolve provider model api kind: %v", err)
	}

	if modelName != "exa-research-pro" {
		t.Fatalf("unexpected request model: %q", modelName)
	}

	if providerAPIKind != ProviderAPIKindOpenAI {
		t.Fatalf("unexpected request provider API kind: %q", providerAPIKind)
	}

	if provider.BaseURL != defaultExaResearchBaseURL {
		t.Fatalf("unexpected request provider base URL: %q", provider.BaseURL)
	}
}

func TestLoadConfigDisablesSearchDeciderPerProvider(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
    disable_search_decider: true
  gemini:
    api_key: gemini-key
models:
  openai/first-model:
  gemini/gemini-3-flash-preview:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if !loadedConfig.Providers["openai"].DisableSearchDecider {
		t.Fatal("expected disable_search_decider to be true for openai")
	}

	if loadedConfig.Providers["gemini"].DisableSearchDecider {
		t.Fatal("expected disable_search_decider to default to false for gemini")
	}
}

func TestLoadConfigAllowsProviderAPIKeyLists(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
    api_key:
      - ` + testTavilyPrimaryAPIKey + `
models:
  openai/first-model:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.Providers["openai"].APIKey != testTavilyPrimaryAPIKey {
		t.Fatalf("unexpected primary provider API key: %q", loadedConfig.Providers["openai"].APIKey)
	}

	if !slices.Equal(
		loadedConfig.Providers["openai"].APIKeys,
		[]string{testTavilyPrimaryAPIKey},
	) {
		t.Fatalf("unexpected provider API keys: %#v", loadedConfig.Providers["openai"].APIKeys)
	}
}

func TestLoadConfigPreservesNestedModelPayloadParameters(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	_, modelName, err := ResolveModelAPIKind(loadedConfig, "nvidia/qwen/qwen3.5-397b-a17b:vision")
	if err != nil {
		t.Fatalf("resolve model api kind: %v", err)
	}

	if modelName != "qwen/qwen3.5-397b-a17b" {
		t.Fatalf("unexpected request model: %q", modelName)
	}

	loadedModelParameters, found := loadedConfig.Models["nvidia/qwen/qwen3.5-397b-a17b:vision"]
	if !found {
		t.Fatalf("missing model parameters for %q", "nvidia/qwen/qwen3.5-397b-a17b:vision")
	}

	chatTemplateKwargs, ok := loadedModelParameters["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected chat_template_kwargs payload: %#v", loadedModelParameters["chat_template_kwargs"])
	}

	if got, ok := chatTemplateKwargs["enable_thinking"].(bool); !ok || got {
		t.Fatalf("unexpected enable_thinking payload: %#v", chatTemplateKwargs["enable_thinking"])
	}
}

func TestLoadConfigAllowsTavilyWebSearchAPIKeyLists(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
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
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
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

func TestLoadConfigAllowsFirecrawlWebSearchAPIKeyLists(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
web_search:
  firecrawl:
    api_key:
      - firecrawl-primary
      - firecrawl-backup
      - firecrawl-primary
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.WebSearch.Firecrawl.APIKey != "firecrawl-primary" {
		t.Fatalf("unexpected primary Firecrawl API key: %q", loadedConfig.WebSearch.Firecrawl.APIKey)
	}

	if !slices.Equal(
		loadedConfig.WebSearch.Firecrawl.APIKeys,
		[]string{"firecrawl-primary", "firecrawl-backup"},
	) {
		t.Fatalf("unexpected Firecrawl API keys: %#v", loadedConfig.WebSearch.Firecrawl.APIKeys)
	}
}

func TestLoadConfigUsesConfiguredFirecrawlMaxMarkdownCharacters(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
web_search:
  firecrawl:
    max_markdown_characters: 20000
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.WebSearch.Firecrawl.MaxMarkdownCharacters != 20000 {
		t.Fatalf(
			"unexpected Firecrawl max markdown characters: %d",
			loadedConfig.WebSearch.Firecrawl.MaxMarkdownCharacters,
		)
	}
}

func TestLoadConfigRejectsNonPositiveFirecrawlMaxMarkdownCharacters(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
web_search:
  firecrawl:
    max_markdown_characters: 0
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected Config load to fail")
	}
}

func TestLoadConfigAllowsSerpAPIVisualSearchAPIKeyLists(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
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
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.WebSearch.PrimaryProvider != WebSearchProviderKindTavily {
		t.Fatalf("unexpected web search primary provider: %q", loadedConfig.WebSearch.PrimaryProvider)
	}
}

func TestLoadConfigUsesExaAsDefaultPrimaryProvider(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.WebSearch.PrimaryProvider != WebSearchProviderKindMCP {
		t.Fatalf("unexpected web search primary provider: %q", loadedConfig.WebSearch.PrimaryProvider)
	}
}

func TestLoadConfigUsesConfiguredWebSearchMaxURLs(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.WebSearch.MaxURLs != 9 {
		t.Fatalf("unexpected web search max URLs: %d", loadedConfig.WebSearch.MaxURLs)
	}
}

func TestLoadConfigUsesConfiguredExaWebSearchTextMaxCharacters(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.WebSearch.Exa.TextMaxCharacters != 9000 {
		t.Fatalf("unexpected Exa text max characters: %d", loadedConfig.WebSearch.Exa.TextMaxCharacters)
	}
}

func TestLoadConfigUsesConfiguredExaWebSearchLivecrawlTimeout(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
web_search:
  exa:
    livecrawl_timeout_ms: 20000
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load Config: %v", err)
	}

	if loadedConfig.WebSearch.Exa.LivecrawlTimeoutMS != 20000 {
		t.Fatalf("unexpected Exa livecrawl timeout: %d", loadedConfig.WebSearch.Exa.LivecrawlTimeoutMS)
	}
}

func TestLoadConfigRejectsNonPositiveExaWebSearchLivecrawlTimeout(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
web_search:
  exa:
    livecrawl_timeout_ms: 0
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write Config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected non-positive Exa livecrawl timeout to fail validation")
	}
}

func TestLoadConfigRejectsUnsupportedWebSearchPrimaryProvider(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected unsupported web search primary provider to fail validation")
	}
}

func TestLoadConfigRejectsDeprecatedFacebookSettingsSection(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
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
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected non-positive web search max URLs to fail validation")
	}
}

func TestLoadConfigRejectsNonPositiveExaWebSearchTextMaxCharacters(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected non-positive Exa text max characters to fail validation")
	}
}

func TestLoadConfigRejectsNonGeminiMediaAnalysisModel(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected non-gemini media analysis model to fail validation")
	}
}

func TestLoadConfigRejectsUnknownChannelModelLock(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "Config.yaml")
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
		t.Fatalf("write Config file: %v", err)
	}

	_, err = loadConfig(configPath)
	if err == nil {
		t.Fatal("expected unknown channel lock model to fail validation")
	}
}

const (
	testExaPrimaryValue     = "exa-primary-value"
	testTavilyPrimaryAPIKey = "primary-key"
	testWebSearchMaxURLs    = 7
)

func systemPromptNow(template string, now time.Time) string {
	replacedText := strings.ReplaceAll(
		template,
		"{date}",
		now.Format("January 02 2006"),
	)
	replacedText = strings.ReplaceAll(
		replacedText,
		"{time}",
		now.Format("15:04:05 MST-0700"),
	)

	return strings.TrimSpace(replacedText)
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
