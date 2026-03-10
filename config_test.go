package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const (
	firstTestModel  = "openai/first-model"
	secondTestModel = "openai/second-model"
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
