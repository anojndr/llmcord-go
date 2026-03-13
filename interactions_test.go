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
		MaxURLs: defaultWebSearchMaxURLs,
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

func TestHandleInteractionCreateRespondsToShowSourcesButtonAfterPendingRelease(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "source-message"

	tracker := newResponseTracker(sourceMessage, "")
	tracker.searchMetadata = &searchMetadata{
		Queries: []string{"latest ai news"},
		Results: []webSearchResult{
			{
				Query: "latest ai news",
				Text: "Title: Example Source\n" +
					"URL: https://example.com/source\n",
			},
		},
		MaxURLs: defaultWebSearchMaxURLs,
	}
	tracker.pendingResponses = []pendingResponse{
		{
			node: instance.nodes.addPending("response-message", sourceMessage),
		},
	}

	tracker.release("assistant reply")

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

func TestHandleInteractionCreateRespondsToPaginatedShowSourcesButton(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.searchMetadata = testPaginatedSearchMetadata()
	node.mu.Unlock()

	interaction := newShowSourcesInteraction("response-message")

	instance.handleInteractionCreate(session, interaction)

	if response.Type != discordgo.InteractionResponseChannelMessageWithSource {
		t.Fatalf("unexpected response type: %v", response.Type)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if response.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected response flags: %v", response.Data.Flags)
	}

	if !containsFold(response.Data.Content, "page 1/") {
		t.Fatalf("expected first page indicator in response content: %q", response.Data.Content)
	}

	if containsFold(response.Data.Content, "... truncated") {
		t.Fatalf("expected paginated response to avoid truncation marker: %q", response.Data.Content)
	}

	if len(response.Data.Components) != 1 {
		t.Fatalf("unexpected component count: %d", len(response.Data.Components))
	}

	row, rowOK := response.Data.Components[0].(*discordgo.ActionsRow)
	if !rowOK {
		t.Fatalf("expected actions row, got %T", response.Data.Components[0])
	}

	if len(row.Components) != 2 {
		t.Fatalf("unexpected pagination button count: %d", len(row.Components))
	}

	previousButton, previousOK := row.Components[0].(*discordgo.Button)
	if !previousOK {
		t.Fatalf("expected previous button, got %T", row.Components[0])
	}

	if previousButton.Label != showSourcesPreviousButtonLabel {
		t.Fatalf("unexpected previous button label: %q", previousButton.Label)
	}

	if !previousButton.Disabled {
		t.Fatal("expected previous button to be disabled on the first page")
	}

	nextButton, nextOK := row.Components[1].(*discordgo.Button)
	if !nextOK {
		t.Fatalf("expected next button, got %T", row.Components[1])
	}

	if nextButton.Label != showSourcesNextButtonLabel {
		t.Fatalf("unexpected next button label: %q", nextButton.Label)
	}

	if nextButton.Disabled {
		t.Fatal("expected next button to be enabled on the first page")
	}

	messageID, pageIndex, ok := parseShowSourcesPageButtonCustomID(nextButton.CustomID)
	if !ok {
		t.Fatalf("expected parsable next button custom id: %q", nextButton.CustomID)
	}

	if messageID != "response-message" || pageIndex != 1 {
		t.Fatalf("unexpected next button target: message=%q page=%d", messageID, pageIndex)
	}
}

func TestHandleInteractionCreateUpdatesShowSourcesPaginationPage(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	metadata := testPaginatedSearchMetadata()
	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.searchMetadata = metadata
	node.mu.Unlock()

	pageCount := len(formatSearchSourcesPages(metadata))
	if pageCount < 2 {
		t.Fatalf("expected multiple pages, got %d", pageCount)
	}

	targetPageIndex := pageCount - 1
	interaction := newComponentInteraction(
		"ephemeral-message",
		showSourcesPageButtonCustomID("response-message", targetPageIndex),
	)

	instance.handleInteractionCreate(session, interaction)

	if response.Type != discordgo.InteractionResponseUpdateMessage {
		t.Fatalf("unexpected response type: %v", response.Type)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedPageIndicator := fmt.Sprintf("page %d/%d", targetPageIndex+1, pageCount)
	if !containsFold(response.Data.Content, expectedPageIndicator) {
		t.Fatalf("expected page indicator %q in response content: %q", expectedPageIndicator, response.Data.Content)
	}

	if !containsFold(response.Data.Content, "https://example.com/agent-frameworks/5") {
		t.Fatalf("expected last source URL in final page content: %q", response.Data.Content)
	}

	if len(response.Data.Components) != 1 {
		t.Fatalf("unexpected component count: %d", len(response.Data.Components))
	}

	row, rowOK := response.Data.Components[0].(*discordgo.ActionsRow)
	if !rowOK {
		t.Fatalf("expected actions row, got %T", response.Data.Components[0])
	}

	if len(row.Components) != 2 {
		t.Fatalf("unexpected pagination button count: %d", len(row.Components))
	}

	previousButton, previousOK := row.Components[0].(*discordgo.Button)
	if !previousOK {
		t.Fatalf("expected previous button, got %T", row.Components[0])
	}

	if previousButton.Disabled {
		t.Fatal("expected previous button to be enabled on the final page")
	}

	nextButton, nextOK := row.Components[1].(*discordgo.Button)
	if !nextOK {
		t.Fatalf("expected next button, got %T", row.Components[1])
	}

	if !nextButton.Disabled {
		t.Fatal("expected next button to be disabled on the final page")
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

		var decoded struct {
			Type discordgo.InteractionResponseType `json:"type"`
			Data *struct {
				Content    string                                      `json:"content"`
				Flags      discordgo.MessageFlags                      `json:"flags,omitempty"`
				Choices    []*discordgo.ApplicationCommandOptionChoice `json:"choices,omitempty"`
				Components []json.RawMessage                           `json:"components"`
			} `json:"data"`
		}

		err = json.Unmarshal(responseBody, &decoded)
		if err != nil {
			t.Fatalf("decode interaction response: %v", err)
		}

		response.Type = decoded.Type
		response.Data = nil

		if decoded.Data != nil {
			response.Data = new(discordgo.InteractionResponseData)
			response.Data.Content = decoded.Data.Content
			response.Data.Flags = decoded.Data.Flags
			response.Data.Choices = decoded.Data.Choices

			if decoded.Data.Components != nil {
				response.Data.Components = make([]discordgo.MessageComponent, 0, len(decoded.Data.Components))
				for _, rawComponent := range decoded.Data.Components {
					component, componentErr := discordgo.MessageComponentFromJSON(rawComponent)
					if componentErr != nil {
						t.Fatalf("decode interaction component: %v", componentErr)
					}

					response.Data.Components = append(response.Data.Components, component)
				}
			}
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
