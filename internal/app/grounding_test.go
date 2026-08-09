package app

import (
	"context"
	providers "llmcord-go/internal/providers"
	searchtypes "llmcord-go/internal/searchtypes"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestBuildGeminiGenerateContentRequestWithGrounding(t *testing.T) {
	t.Parallel()

	request := providers.ChatCompletionRequest{
		Provider: providers.ProviderRequestConfig{
			APIKind:         providers.ProviderAPIKindGemini,
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: true,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "gemini-3.6-flash",
		ConfiguredModel:    "",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages: []providers.ChatMessage{
			{Role: searchtypes.MessageRoleUser, Content: "What is the weather in Tokyo?"},
		},
	}

	_, config, err := providers.BuildGeminiGenerateContentRequest(context.Background(), request, nil)
	if err != nil {
		t.Fatalf("buildGeminiGenerateContentRequest failed: %v", err)
	}

	if config == nil {
		t.Fatal("expected non-nil Config")
	}

	found := false

	for _, tool := range config.Tools {
		if tool.GoogleSearch != nil {
			found = true

			break
		}
	}

	if !found {
		t.Error("expected GoogleSearch tool to be present in Gemini request")
	}

	if config.SystemInstruction == nil || len(config.SystemInstruction.Parts) != 1 {
		t.Fatalf("expected grounding system instruction: %#v", config.SystemInstruction)
	}

	if config.SystemInstruction.Parts[0].Text != providers.GeminiGroundingInstruction {
		t.Fatalf("unexpected grounding system instruction: %q", config.SystemInstruction.Parts[0].Text)
	}
}

func TestBuildGeminiGenerateContentRequestWithoutGrounding(t *testing.T) {
	t.Parallel()

	request := providers.ChatCompletionRequest{
		Provider: providers.ProviderRequestConfig{
			APIKind:         providers.ProviderAPIKindGemini,
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "gemini-3.6-flash",
		ConfiguredModel:    "",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages: []providers.ChatMessage{
			{Role: searchtypes.MessageRoleUser, Content: "What is the weather in Tokyo?"},
		},
	}

	_, config, err := providers.BuildGeminiGenerateContentRequest(context.Background(), request, nil)
	if err != nil {
		t.Fatalf("buildGeminiGenerateContentRequest failed: %v", err)
	}

	if config != nil {
		for _, tool := range config.Tools {
			if tool.GoogleSearch != nil {
				t.Error("unexpected GoogleSearch tool in Gemini request")
			}
		}
	}

	if config == nil || config.SystemInstruction == nil || len(config.SystemInstruction.Parts) != 1 {
		t.Fatalf("expected no-tools system instruction: %#v", config)
	}

	if config.SystemInstruction.Parts[0].Text != providers.GeminiNoToolsInstruction {
		t.Fatalf("unexpected no-tools system instruction: %q", config.SystemInstruction.Parts[0].Text)
	}
}

func testGroundingConfig() config {
	return config{
		BotToken:      "",
		ClientID:      "",
		StatusMessage: "",
		MaxImages:     0,
		MaxMessages:   0,
		AllowDMs:      false,
		Permissions: permissionsConfig{
			Users: userPermissions{
				AdminIDs:   nil,
				AllowedIDs: nil,
				BlockedIDs: nil,
			},
			Roles: scopePermissions{
				AllowedIDs: nil,
				BlockedIDs: nil,
			},
			Channels: scopePermissions{
				AllowedIDs: nil,
				BlockedIDs: nil,
			},
		},
		Providers: map[string]providerConfig{
			"gemini": {
				Name:            "gemini",
				BaseURL:         "",
				APIKey:          "",
				APIKeys:         nil,
				EnableGrounding: true,
				ExtraHeaders:    nil,
				ExtraQuery:      nil,
				ExtraBody:       nil,
			},
		},
		WebSearch: webSearchConfig{
			PrimaryProvider: "",
			MaxURLs:         0,
			Exa: exaSearchConfig{
				APIKey:             "",
				APIKeys:            nil,
				SearchType:         "",
				TextMaxCharacters:  0,
				LivecrawlTimeoutMS: 0,
			},
			Firecrawl: firecrawlSearchConfig{
				APIKey:                "",
				APIKeys:               nil,
				MaxMarkdownCharacters: 0,
			},
			Tavily: tavilySearchConfig{
				APIKey:  "",
				APIKeys: nil,
			},
		},
		VisualSearch: visualSearchConfig{
			SerpAPI: serpAPIVisualSearchConfig{
				APIKey:  "",
				APIKeys: nil,
			},
		},
		Database: databaseConfig{
			ConnectionString: "",
			StoreKey:         "",
		},
		Gist: gistConfig{
			Endpoint:    "",
			APIKey:      "",
			APIKeys:     nil,
			Description: "",
			Filename:    defaultGistFilename,
			Public:      false,
		},
		Models: map[string]map[string]any{
			"gemini/gemini-3.6-flash": {},
		},
		ModelOrder:         nil,
		ChannelModelLocks:  nil,
		SearchDeciderModel: "",
		MediaAnalysisModel: "",
		SystemPrompt:       "",
	}
}

func TestAugmentConversationSkipsSearchDeciderWhenGroundingEnabled(t *testing.T) {
	t.Parallel()

	loadedConfig := testGroundingConfig()
	instance := new(bot)
	ctx := context.Background()
	messages := []providers.ChatMessage{{Role: searchtypes.MessageRoleUser, Content: "search for something"}}
	sourceMessage := new(discordgo.Message)

	augmentedMessages, searchMetadata, _, err := instance.augmentConversation(
		ctx,
		loadedConfig,
		"gemini/gemini-3.6-flash",
		sourceMessage,
		messages,
		nil,
		"",
	)
	if err != nil {
		t.Fatalf("augmentConversation failed: %v", err)
	}

	if searchMetadata != nil {
		t.Error("expected searchMetadata to be nil when grounding is enabled (skipping search decider)")
	}

	if len(augmentedMessages) != len(messages) {
		t.Errorf("expected %+v, got %+v", messages, augmentedMessages)
	}
}
