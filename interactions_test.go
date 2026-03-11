package main

import (
	"context"
	"encoding/json"
	"fmt"
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

type fakeRentryClient struct {
	url       string
	err       error
	callCount int
	texts     []string
}

func (client *fakeRentryClient) createEntry(_ context.Context, text string) (string, error) {
	client.callCount++
	client.texts = append(client.texts, text)

	return client.url, client.err
}

func TestHandleModelCommandAllowsNonAdminSwitch(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfig(t)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	interaction := newModelCommandInteraction("member-user", secondTestModel)

	err := instance.handleModelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle model command: %v", err)
	}

	if instance.currentModel != secondTestModel {
		t.Fatalf("unexpected current model: %q", instance.currentModel)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := fmt.Sprintf("Model switched to: `%s`", secondTestModel)
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleSearchDeciderModelCommandAllowsSwitch(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfig(t)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	instance.currentSearchDeciderModel = firstTestModel
	interaction := newSearchDeciderModelCommandInteraction("member-user", secondTestModel)

	err := instance.handleSearchDeciderModelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle search decider model command: %v", err)
	}

	if instance.currentSearchDeciderModel != secondTestModel {
		t.Fatalf("unexpected current search decider model: %q", instance.currentSearchDeciderModel)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := fmt.Sprintf("Search decider model switched to: `%s`", secondTestModel)
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleModelCommandRejectsLockedChannelSwitch(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfigWithExtra(
		t,
		fmt.Sprintf(
			`
channel_model_locks:
  locked-channel: %s
`,
			secondTestModel,
		),
	)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	interaction := newModelCommandInteractionInChannel(
		"member-user",
		firstTestModel,
		"locked-channel",
	)

	err := instance.handleModelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle model command: %v", err)
	}

	if instance.currentModel != firstTestModel {
		t.Fatalf("unexpected current model: %q", instance.currentModel)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := fmt.Sprintf(
		"This channel is locked to `%s`. `/model` is disabled here.",
		secondTestModel,
	)
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleSearchDeciderModelCommandAllowsSwitchInLockedChannel(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfigWithExtra(
		t,
		fmt.Sprintf(
			`
channel_model_locks:
  locked-channel: %s
`,
			secondTestModel,
		),
	)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	instance.currentSearchDeciderModel = firstTestModel
	interaction := newSearchDeciderModelCommandInteractionInChannel(
		"member-user",
		secondTestModel,
		"locked-channel",
	)

	err := instance.handleSearchDeciderModelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle search decider model command: %v", err)
	}

	if instance.currentSearchDeciderModel != secondTestModel {
		t.Fatalf("unexpected current search decider model: %q", instance.currentSearchDeciderModel)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := fmt.Sprintf("Search decider model switched to: `%s`", secondTestModel)
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestLockedModelAutocompleteChoices(t *testing.T) {
	t.Parallel()

	choices := lockedModelAutocompleteChoices(secondTestModel, "second")
	if len(choices) != 1 {
		t.Fatalf("unexpected choice count: %d", len(choices))
	}

	if choices[0].Name != fmt.Sprintf("x %s (locked)", secondTestModel) {
		t.Fatalf("unexpected choice name: %q", choices[0].Name)
	}

	if choices[0].Value != secondTestModel {
		t.Fatalf("unexpected choice value: %#v", choices[0].Value)
	}

	choices = lockedModelAutocompleteChoices(secondTestModel, "first")
	if len(choices) != 0 {
		t.Fatalf("unexpected filtered choice count: %d", len(choices))
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

func TestHandleInteractionCreateRespondsToViewOnRentryButton(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	rentry := new(fakeRentryClient)
	rentry.url = "https://rentry.co/example"

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.rentry = rentry

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = testAssistantReply
	node.initialized = true
	node.mu.Unlock()

	interaction := newComponentInteraction("response-message", viewOnRentryButtonCustomID)

	instance.handleInteractionCreate(session, interaction)

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if response.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected response flags: %v", response.Data.Flags)
	}

	if !containsFold(response.Data.Content, rentry.url) {
		t.Fatalf("expected Rentry url in response content: %q", response.Data.Content)
	}

	if rentry.callCount != 1 {
		t.Fatalf("unexpected Rentry call count: %d", rentry.callCount)
	}

	if len(rentry.texts) != 1 || rentry.texts[0] != testAssistantReply {
		t.Fatalf("unexpected Rentry request texts: %#v", rentry.texts)
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	if node.rentryURL != rentry.url {
		t.Fatalf("unexpected cached Rentry url: %q", node.rentryURL)
	}
}

func TestHandleInteractionCreateReusesCachedRentryURL(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	rentry := new(fakeRentryClient)

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.rentry = rentry

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = testAssistantReply
	node.initialized = true
	node.rentryURL = "https://rentry.co/cached"
	node.mu.Unlock()

	interaction := newComponentInteraction("response-message", viewOnRentryButtonCustomID)

	instance.handleInteractionCreate(session, interaction)

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if !containsFold(response.Data.Content, node.rentryURL) {
		t.Fatalf("expected cached Rentry url in response content: %q", response.Data.Content)
	}

	if rentry.callCount != 0 {
		t.Fatalf("expected cached Rentry url to skip creation, got %d calls", rentry.callCount)
	}
}

func writeModelConfig(t *testing.T) string {
	t.Helper()

	return writeModelConfigWithExtra(t, "")
}

func writeModelConfigWithExtra(t *testing.T, extraText string) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configText := fmt.Sprintf(`
bot_token: discord-token
permissions:
  users:
    admin_ids: []
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  %s:
  %s:
`, firstTestModel, secondTestModel) + extraText

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

func newModelTestBot(configPath string) *bot {
	instance := new(bot)
	instance.configPath = configPath
	instance.currentModel = firstTestModel

	return instance
}

func newModelCommandInteraction(userID string, modelName string) *discordgo.InteractionCreate {
	return newModelCommandInteractionInChannel(userID, modelName, "")
}

func newModelCommandInteractionInChannel(
	userID string,
	modelName string,
	channelID string,
) *discordgo.InteractionCreate {
	return newConfiguredModelCommandInteraction(
		userID,
		modelName,
		modelCommandName,
		modelOptionName,
		channelID,
	)
}

func newSearchDeciderModelCommandInteraction(
	userID string,
	modelName string,
) *discordgo.InteractionCreate {
	return newSearchDeciderModelCommandInteractionInChannel(userID, modelName, "")
}

func newSearchDeciderModelCommandInteractionInChannel(
	userID string,
	modelName string,
	channelID string,
) *discordgo.InteractionCreate {
	return newConfiguredModelCommandInteraction(
		userID,
		modelName,
		searchDeciderModelCommandName,
		searchDeciderModelOptionName,
		channelID,
	)
}

func newConfiguredModelCommandInteraction(
	userID string,
	modelName string,
	commandName string,
	optionName string,
	channelID string,
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
	interaction.ChannelID = channelID
	interaction.Member = member
	interaction.Data = commandData

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}

func newShowSourcesInteraction(messageID string) *discordgo.InteractionCreate {
	return newComponentInteraction(messageID, showSourcesButtonCustomID)
}

func newComponentInteraction(messageID string, customID string) *discordgo.InteractionCreate {
	message := new(discordgo.Message)
	message.ID = messageID

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.Token = "interaction-token"
	interaction.Type = discordgo.InteractionMessageComponent
	interaction.Message = message

	componentData := new(discordgo.MessageComponentInteractionData)
	componentData.CustomID = customID
	interaction.Data = *componentData

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}
