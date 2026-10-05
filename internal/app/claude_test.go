package app

import (
	"context"
	"os"
	"strings"
	"testing"

	providers "llmcord-go/internal/providers"
)

func newClaudeTestConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.Providers = map[string]providerConfig{
		"claude": {
			Name:                  "claude",
			API:                   providers.ClaudeAPIName,
			APIKey:                "test-key",
			APIKeys:               []string{"test-key"},
			ChainPreviousResponse: true,
		},
	}
	loadedConfig.Models = map[string]map[string]any{
		"claude/claude-sonnet-5": nil,
	}
	loadedConfig.ModelOrder = []string{"claude/claude-sonnet-5"}
	loadedConfig.WebSearch.Tavily = tavilySearchConfig{
		APIKey:  testWebSearchTavilyKey,
		APIKeys: []string{testWebSearchTavilyKey},
	}

	return loadedConfig
}

func TestClaudeProviderAPIKind(t *testing.T) {
	t.Parallel()

	provider := providerConfig{Name: "claude", API: providers.ClaudeAPIName}
	if provider.apiKind() != providerAPIKindClaude {
		t.Fatalf("expected claude api kind, got %q", provider.apiKind())
	}

	named := providerConfig{Name: "my-claude"}
	if named.apiKind() != providerAPIKindClaude {
		t.Fatalf("expected name-based claude api kind, got %q", named.apiKind())
	}

	if providers.ProviderAPIKind(provider.apiKind()) != providers.ProviderAPIKindClaude {
		t.Fatalf("app/provider claude kinds diverged: %q", provider.apiKind())
	}
}

func TestClaudeProviderValidation(t *testing.T) {
	t.Parallel()

	provider := providerConfig{Name: "claude", API: providers.ClaudeAPIName, ReasoningEffort: "medium"}
	if err := provider.validate("claude"); err != nil {
		t.Fatalf("expected claude provider to validate: %v", err)
	}

	badAPI := providerConfig{Name: "claude", API: "openai-chat-completions"}
	if err := badAPI.validate("claude"); err == nil {
		t.Fatal("expected wrong api value to fail")
	}

	badEffort := providerConfig{Name: "claude", ReasoningEffort: "turbo"}
	if err := badEffort.validate("claude"); err == nil {
		t.Fatal("expected invalid claude effort to fail")
	}
}

func TestBuildChatCompletionRequestForClaude(t *testing.T) {
	t.Parallel()

	loadedConfig := newClaudeTestConfig()

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"claude/claude-sonnet-5",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
		false,
	)
	if err != nil {
		t.Fatalf("build claude request: %v", err)
	}

	if request.Provider.APIKind != providers.ProviderAPIKindClaude {
		t.Fatalf("unexpected provider kind: %q", request.Provider.APIKind)
	}

	if request.Model != "claude-sonnet-5" {
		t.Fatalf("unexpected model: %q", request.Model)
	}

	if request.Provider.APIKey != "test-key" {
		t.Fatalf("unexpected api key: %q", request.Provider.APIKey)
	}
}

func TestMessageContentOptionsForClaude(t *testing.T) {
	t.Parallel()

	loadedConfig := newClaudeTestConfig()
	loadedConfig.Models["claude/claude-sonnet-5:vision"] = nil

	options, err := messageContentOptionsForModel(loadedConfig, "claude/claude-sonnet-5:vision")
	if err != nil {
		t.Fatalf("content options: %v", err)
	}

	if !options.allowDocuments || !options.allowFiles {
		t.Fatalf("expected claude documents+files enabled: %#v", options)
	}

	if options.allowAudio || options.allowVideo {
		t.Fatalf("expected claude audio/video disabled: %#v", options)
	}

	if _, ok := options.allowedDocumentMIMETypes[mimeTypePDF]; !ok {
		t.Fatalf("expected PDF allowed: %#v", options.allowedDocumentMIMETypes)
	}
}

func TestClaudeWebSearchToolEnabled(t *testing.T) {
	t.Parallel()

	loadedConfig := newClaudeTestConfig()
	instance := newSearchTestBot(nil, nil)

	provider := loadedConfig.Providers["claude"]
	if !instance.webSearchToolEnabled(loadedConfig, provider) {
		t.Fatal("expected web_search offered for claude")
	}

	provider.DisableWebSearch = true
	if instance.webSearchToolEnabled(loadedConfig, provider) {
		t.Fatal("expected opt-out to disable web_search")
	}
}

func TestRespondToMessageWithClaudeWebSearchTool(t *testing.T) {
	t.Parallel()

	loadedConfig := newClaudeTestConfig()
	loadedConfig.Models = map[string]map[string]any{
		"claude/claude-sonnet-5": nil,
	}
	loadedConfig.ModelOrder = []string{"claude/claude-sonnet-5"}

	var requestTools [][]providers.FunctionTool

	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		requestTools = append(requestTools, request.Tools)

		if len(request.ToolRounds) > 0 {
			return handle(newStreamDelta(testWebSearchToolAnswer, finishReasonStop))
		}

		if request.Provider.APIKind != providers.ProviderAPIKindClaude {
			t.Errorf("expected claude provider kind, got %q", request.Provider.APIKind)
		}

		return handle(toolCallDelta(providers.FunctionToolCall{
			ID:        "call_1",
			Name:      providers.WebSearchToolName,
			Arguments: `{"objective": "Find first query results", "search_queries": ["` + testWebSearchQueryOne + `"]}`,
		}))
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		if len(queries) != 1 || queries[0] != testWebSearchQueryOne {
			t.Errorf("unexpected claude search queries: %#v", queries)
		}

		return []webSearchResult{{Query: testWebSearchQueryOne, Text: testWebSearchResultText}}, nil
	})

	instance := newSearchToolTestBot(t, chatClient, webSearch)

	sourceMessage := newWebSearchToolSourceMessage()
	sourceMessage.Content = "<@bot-user> search for " + testWebSearchQueryOne

	err := instance.respondToMessage(context.Background(), loadedConfig, sourceMessage, "claude/claude-sonnet-5")
	if err != nil {
		t.Fatalf("respond to message: %v", err)
	}

	if len(requestTools) != 2 {
		t.Fatalf("expected 2 claude requests, got %d", len(requestTools))
	}

	if len(requestTools[0]) != 1 || requestTools[0][0].Name != providers.WebSearchToolName {
		t.Fatalf("expected web_search on first request: %#v", requestTools[0])
	}

	finalRequest := chatClient.requests[len(chatClient.requests)-1]
	if len(finalRequest.ToolRounds) != 1 {
		t.Fatalf("expected final round to replay 1 tool round, got %d", len(finalRequest.ToolRounds))
	}

	assertToolRoundOutputContains(t, finalRequest.ToolRounds[0], "call_1", testWebSearchResultText)
}

func TestClaudeConfigFileRoundTrip(t *testing.T) {
	t.Parallel()

	configText := `bot_token: test-token
max_messages: 25
providers:
  claude:
    api: claude-messages
    api_key: test-key
    reasoning_effort: medium
models:
  claude/claude-sonnet-5: {}
`
	configPath := t.TempDir() + "/config.yaml"

	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	provider := loadedConfig.Providers["claude"]
	if provider.apiKind() != providerAPIKindClaude {
		t.Fatalf("unexpected api kind: %q", provider.apiKind())
	}

	if provider.ReasoningEffort != "medium" {
		t.Fatalf("unexpected effort: %q", provider.ReasoningEffort)
	}

	if !strings.Contains(loadedConfig.ModelOrder[0], "claude") {
		t.Fatalf("unexpected model order: %#v", loadedConfig.ModelOrder)
	}
}
