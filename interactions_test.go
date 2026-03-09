package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/bwmarrin/discordgo"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestHandleModelCommandAllowsNonAdminSwitch(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfig(t)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath, "openai/first-model")
	interaction := newModelCommandInteraction("member-user", "openai/second-model")

	err := instance.handleModelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle model command: %v", err)
	}

	if instance.currentModel != "openai/second-model" {
		t.Fatalf("unexpected current model: %q", instance.currentModel)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Model switched to: `openai/second-model`"
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleSearchDeciderModelCommandAllowsSwitch(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfig(t)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath, "openai/first-model")
	instance.currentSearchDeciderModel = "openai/first-model"
	interaction := newSearchDeciderModelCommandInteraction("member-user", "openai/second-model")

	err := instance.handleSearchDeciderModelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle search decider model command: %v", err)
	}

	if instance.currentSearchDeciderModel != "openai/second-model" {
		t.Fatalf("unexpected current search decider model: %q", instance.currentSearchDeciderModel)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Search decider model switched to: `openai/second-model`"
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleInteractionCreateRespondsToShowSourcesButton(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.searchMetadata = &searchMetadata{
		Queries: []string{"latest ai news"},
		Results: []webSearchResult{
			{
				Query: "latest ai news",
				Text: "Title: Example Source\n" +
					"URL: https://example.com/source\n",
			},
		},
	}
	node.mu.Unlock()

	interaction := newShowSourcesInteraction("response-message")

	instance.handleInteractionCreate(session, interaction)

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if response.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected response flags: %v", response.Data.Flags)
	}

	if !containsFold(response.Data.Content, "latest ai news") {
		t.Fatalf("expected query in response content: %q", response.Data.Content)
	}

	if !containsFold(response.Data.Content, "https://example.com/source") {
		t.Fatalf("expected source URL in response content: %q", response.Data.Content)
	}
}

func writeModelConfig(t *testing.T) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configText := `
bot_token: discord-token
permissions:
  users:
    admin_ids: []
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

	return configPath
}

func newInteractionTestSession(
	t *testing.T,
	response *discordgo.InteractionResponse,
) *discordgo.Session {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", request.Method)
		}

		expectedPath := "/api/v9/interactions/interaction-id/interaction-token/callback"
		if request.URL.Path != expectedPath {
			t.Fatalf("unexpected request path: got %q want %q", request.URL.Path, expectedPath)
		}

		responseBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}

		err = json.Unmarshal(responseBody, response)
		if err != nil {
			t.Fatalf("decode interaction response: %v", err)
		}

		return newNoContentResponse(request), nil
	})
	session.Client = client

	return session
}

func newNoContentResponse(request *http.Request) *http.Response {
	response := new(http.Response)
	response.Status = "204 No Content"
	response.StatusCode = http.StatusNoContent
	response.Body = http.NoBody
	response.ContentLength = 0
	response.Header = make(http.Header)
	response.Request = request

	return response
}

func newModelTestBot(configPath string, currentModel string) *bot {
	instance := new(bot)
	instance.configPath = configPath
	instance.currentModel = currentModel

	return instance
}

func newModelCommandInteraction(userID string, modelName string) *discordgo.InteractionCreate {
	return newConfiguredModelCommandInteraction(userID, modelName, modelCommandName, modelOptionName)
}

func newSearchDeciderModelCommandInteraction(
	userID string,
	modelName string,
) *discordgo.InteractionCreate {
	return newConfiguredModelCommandInteraction(
		userID,
		modelName,
		searchDeciderModelCommandName,
		searchDeciderModelOptionName,
	)
}

func newConfiguredModelCommandInteraction(
	userID string,
	modelName string,
	commandName string,
	optionName string,
) *discordgo.InteractionCreate {
	user := new(discordgo.User)
	user.ID = userID

	member := new(discordgo.Member)
	member.User = user

	option := new(discordgo.ApplicationCommandInteractionDataOption)
	option.Name = optionName
	option.Type = discordgo.ApplicationCommandOptionString
	option.Value = modelName

	var commandData discordgo.ApplicationCommandInteractionData

	commandData.Name = commandName
	commandData.Options = []*discordgo.ApplicationCommandInteractionDataOption{option}

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.Token = "interaction-token"
	interaction.Type = discordgo.InteractionApplicationCommand
	interaction.Member = member
	interaction.Data = commandData

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}

func newShowSourcesInteraction(messageID string) *discordgo.InteractionCreate {
	message := new(discordgo.Message)
	message.ID = messageID

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.Token = "interaction-token"
	interaction.Type = discordgo.InteractionMessageComponent
	interaction.Message = message

	componentData := new(discordgo.MessageComponentInteractionData)
	componentData.CustomID = showSourcesButtonCustomID
	interaction.Data = *componentData

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}
