package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type fakeGistClient struct {
	url       string
	err       error
	callCount int
	texts     []string
}

type editedInteractionResponse struct {
	Content string `json:"content"`
}

type deferredInteractionCapture struct {
	requestCount     int
	deferredResponse discordgo.InteractionResponse
	editedResponse   editedInteractionResponse
}

var errFakeGistUnavailable = errors.New("gist unavailable")

func (client *fakeGistClient) createGist(_ context.Context, text string) (string, error) {
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

func TestHandleEditChannelNameCommandRenamesChannel(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newEditChannelNameTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"new-name"}`,
	)
	instance := new(bot)
	interaction := newEditChannelNameCommandInteraction("channel-id", "new-name")

	err := instance.handleEditChannelNameCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle edit channel name command: %v", err)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Renamed channel to `new-name`."
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleEditChannelNameCommandRequiresOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		channelID string
		newName   string
	}{
		{name: "missing channel id", channelID: "", newName: "new-name"},
		{name: "missing new name", channelID: "channel-id", newName: ""},
		{name: "missing both", channelID: "", newName: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var response discordgo.InteractionResponse

			session := newInteractionTestSession(t, &response)
			instance := new(bot)
			interaction := newEditChannelNameCommandInteraction(test.channelID, test.newName)

			err := instance.handleEditChannelNameCommand(session, interaction)
			if err != nil {
				t.Fatalf("handle edit channel name command: %v", err)
			}

			if response.Data == nil {
				t.Fatal("expected interaction response data")
			}

			expectedContent := "Both `channelid` and `newchannelname` are required."
			if response.Data.Content != expectedContent {
				t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
			}
		})
	}
}

func TestHandleEditChannelNameCommandReportsChannelEditFailure(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newEditChannelNameTestSession(
		t,
		&response,
		http.StatusForbidden,
		`{"message":"Missing Permissions","code":50013}`,
	)
	instance := new(bot)
	interaction := newEditChannelNameCommandInteraction("channel-id", "new-name")

	err := instance.handleEditChannelNameCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle edit channel name command: %v", err)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Failed to rename channel `channel-id`."
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleApplicationCommandInteractionDispatchesEditChannelName(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newEditChannelNameTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"new-name"}`,
	)
	instance := new(bot)
	interaction := newEditChannelNameCommandInteraction("channel-id", "new-name")

	err := instance.handleApplicationCommandInteraction(session, interaction)
	if err != nil {
		t.Fatalf("handle application command interaction: %v", err)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Renamed channel to `new-name`."
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestNewEditChannelNameCommand(t *testing.T) {
	t.Parallel()

	command := newEditChannelNameCommand()

	if command.Name != editChannelNameCommandName {
		t.Fatalf("unexpected command name: got %q want %q", command.Name, editChannelNameCommandName)
	}

	if command.Description != editChannelNameCommandDescription {
		t.Fatalf("unexpected command description: got %q want %q", command.Description, editChannelNameCommandDescription)
	}

	if len(command.Options) != 2 {
		t.Fatalf("unexpected option count: got %d want 2", len(command.Options))
	}

	channelIDOption := command.Options[0]
	if channelIDOption.Name != editChannelNameChannelIDOptionName ||
		channelIDOption.Type != discordgo.ApplicationCommandOptionString ||
		!channelIDOption.Required {
		t.Fatalf("unexpected channel id option: %+v", channelIDOption)
	}

	newNameOption := command.Options[1]
	if newNameOption.Name != editChannelNameOptionName ||
		newNameOption.Type != discordgo.ApplicationCommandOptionString ||
		!newNameOption.Required {
		t.Fatalf("unexpected new name option: %+v", newNameOption)
	}
}

func TestHandleMoveChannelCommandMovesChannelUp(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	var capture moveChannelTestCapture

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"general","position":5}`,
		http.StatusOK,
		`{"id":"channel-id","name":"general","position":3}`,
		&capture,
	)
	instance := new(bot)
	interaction := newMoveChannelCommandInteraction("channel-id", "up", 2)

	err := instance.handleMoveChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle move channel command: %v", err)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Moved channel `general` up 2 position(s) to position `3`."
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}

	if !strings.Contains(capture.editBody, `"position":3`) {
		t.Fatalf("expected position 3 in channel edit body: %q", capture.editBody)
	}
}

func TestHandleMoveChannelCommandMovesChannelDown(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	var capture moveChannelTestCapture

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"general","position":1}`,
		http.StatusOK,
		`{"id":"channel-id","name":"general","position":4}`,
		&capture,
	)
	instance := new(bot)
	interaction := newMoveChannelCommandInteraction("channel-id", "down", 3)

	err := instance.handleMoveChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle move channel command: %v", err)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Moved channel `general` down 3 position(s) to position `4`."
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}

	if !strings.Contains(capture.editBody, `"position":4`) {
		t.Fatalf("expected position 4 in channel edit body: %q", capture.editBody)
	}
}

func TestHandleMoveChannelCommandClampsToTop(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	var capture moveChannelTestCapture

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"general","position":1}`,
		http.StatusOK,
		`{"id":"channel-id","name":"general","position":0}`,
		&capture,
	)
	instance := new(bot)
	interaction := newMoveChannelCommandInteraction("channel-id", "up", 5)

	err := instance.handleMoveChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle move channel command: %v", err)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Moved channel `general` up 5 position(s) to position `0`."
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}

	if !strings.Contains(capture.editBody, `"position":0`) {
		t.Fatalf("expected position 0 in channel edit body: %q", capture.editBody)
	}
}

func TestHandleMoveChannelCommandRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		channelID string
		movement  string
		howMany   int
		want      string
	}{
		{
			name:      "missing channel id",
			channelID: "",
			movement:  "up",
			howMany:   1,
			want:      "`channelid` is required.",
		},
		{
			name:      "invalid movement",
			channelID: "channel-id",
			movement:  "left",
			howMany:   1,
			want:      "`movement` must be `up` or `down`.",
		},
		{
			name:      "howmany zero",
			channelID: "channel-id",
			movement:  "up",
			howMany:   0,
			want:      "`howmany` must be a positive integer.",
		},
		{
			name:      "howmany negative",
			channelID: "channel-id",
			movement:  "down",
			howMany:   -2,
			want:      "`howmany` must be a positive integer.",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var response discordgo.InteractionResponse

			session := newInteractionTestSession(t, &response)
			instance := new(bot)
			interaction := newMoveChannelCommandInteraction(test.channelID, test.movement, test.howMany)

			err := instance.handleMoveChannelCommand(session, interaction)
			if err != nil {
				t.Fatalf("handle move channel command: %v", err)
			}

			if response.Data == nil {
				t.Fatal("expected interaction response data")
			}

			if response.Data.Content != test.want {
				t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, test.want)
			}
		})
	}
}

func TestHandleMoveChannelCommandReportsChannelLoadFailure(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusNotFound,
		`{"message":"Unknown Channel","code":10003}`,
		0,
		"",
		nil,
	)
	instance := new(bot)
	interaction := newMoveChannelCommandInteraction("channel-id", "up", 1)

	err := instance.handleMoveChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle move channel command: %v", err)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Failed to load channel `channel-id`."
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleMoveChannelCommandReportsMoveFailure(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"general","position":5}`,
		http.StatusForbidden,
		`{"message":"Missing Permissions","code":50013}`,
		nil,
	)
	instance := new(bot)
	interaction := newMoveChannelCommandInteraction("channel-id", "up", 1)

	err := instance.handleMoveChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle move channel command: %v", err)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Failed to move channel `channel-id`."
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleApplicationCommandInteractionDispatchesMoveChannel(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"general","position":5}`,
		http.StatusOK,
		`{"id":"channel-id","name":"general","position":3}`,
		nil,
	)
	instance := new(bot)
	interaction := newMoveChannelCommandInteraction("channel-id", "up", 2)

	err := instance.handleApplicationCommandInteraction(session, interaction)
	if err != nil {
		t.Fatalf("handle application command interaction: %v", err)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Moved channel `general` up 2 position(s) to position `3`."
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestNewMoveChannelCommand(t *testing.T) {
	t.Parallel()

	command := newMoveChannelCommand()

	if command.Name != moveChannelCommandName {
		t.Fatalf("unexpected command name: got %q want %q", command.Name, moveChannelCommandName)
	}

	if command.Description != moveChannelCommandDescription {
		t.Fatalf("unexpected command description: got %q want %q", command.Description, moveChannelCommandDescription)
	}

	if len(command.Options) != 3 {
		t.Fatalf("unexpected option count: got %d want 3", len(command.Options))
	}

	channelIDOption := command.Options[0]
	if channelIDOption.Name != moveChannelChannelIDOptionName ||
		channelIDOption.Type != discordgo.ApplicationCommandOptionString ||
		!channelIDOption.Required {
		t.Fatalf("unexpected channel id option: %+v", channelIDOption)
	}

	movementOption := command.Options[1]
	if movementOption.Name != moveChannelMovementOptionName ||
		movementOption.Type != discordgo.ApplicationCommandOptionString ||
		!movementOption.Required ||
		len(movementOption.Choices) != 2 {
		t.Fatalf("unexpected movement option: %+v", movementOption)
	}

	if movementOption.Choices[0].Name != "up" || movementOption.Choices[0].Value != "up" {
		t.Fatalf("unexpected up choice: %+v", movementOption.Choices[0])
	}

	if movementOption.Choices[1].Name != "down" || movementOption.Choices[1].Value != "down" {
		t.Fatalf("unexpected down choice: %+v", movementOption.Choices[1])
	}

	howManyOption := command.Options[2]
	if howManyOption.Name != moveChannelHowManyOptionName ||
		howManyOption.Type != discordgo.ApplicationCommandOptionInteger ||
		!howManyOption.Required {
		t.Fatalf("unexpected how many option: %+v", howManyOption)
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

func TestHandleSearchTypeCommandAllowsSwitchWhenExaAPIConfigured(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfigWithExtra(
		t,
		`
web_search:
  exa:
    api_key: exa-key
`,
	)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	interaction := newSearchTypeCommandInteraction("member-user", exaSearchTypeDeepReasoning)

	err := instance.handleSearchTypeCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle search type command: %v", err)
	}

	if instance.currentExaSearchTypeValue != exaSearchTypeDeepReasoning {
		t.Fatalf("unexpected current Exa search type: %q", instance.currentExaSearchTypeValue)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := fmt.Sprintf("Exa search type switched to: `%s`", exaSearchTypeDeepReasoning)
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleSearchTypeCommandRejectsWhenExaAPIIsNotConfigured(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfig(t)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	interaction := newSearchTypeCommandInteraction("member-user", exaSearchTypeFast)

	err := instance.handleSearchTypeCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle search type command: %v", err)
	}

	if instance.currentExaSearchTypeValue != defaultExaSearchType {
		t.Fatalf("unexpected current Exa search type: %q", instance.currentExaSearchTypeValue)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Exa Search API is not configured. Set `web_search.exa.api_key` to use `/searchtype`."
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleSearchTypeAutocompleteListsAllOptions(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.currentExaSearchTypeValue = defaultExaSearchType

	err := instance.handleSearchTypeAutocomplete(
		session,
		newSearchTypeAutocompleteInteraction("member-user", ""),
	)
	if err != nil {
		t.Fatalf("handle search type autocomplete: %v", err)
	}

	if response.Type != discordgo.InteractionApplicationCommandAutocompleteResult {
		t.Fatalf("unexpected autocomplete response type: %v", response.Type)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	searchTypes := exaSearchTypes()

	if len(response.Data.Choices) != len(searchTypes) {
		t.Fatalf("unexpected choice count: %d", len(response.Data.Choices))
	}

	expectedNames := []string{
		"* auto (current)",
		"o instant",
		"o fast",
		"o deep-lite",
		"o deep",
		"o deep-reasoning",
	}
	expectedValues := []string{
		exaSearchTypeAuto,
		exaSearchTypeInstant,
		exaSearchTypeFast,
		exaSearchTypeDeepLite,
		exaSearchTypeDeep,
		exaSearchTypeDeepReasoning,
	}

	for index, choice := range response.Data.Choices {
		if choice.Name != expectedNames[index] {
			t.Fatalf("unexpected choice name at %d: got %q want %q", index, choice.Name, expectedNames[index])
		}

		if choice.Value != expectedValues[index] {
			t.Fatalf("unexpected choice value at %d: got %#v want %q", index, choice.Value, expectedValues[index])
		}
	}
}

func TestHandleSearchTypeCommandRejectsUnknownTypeWithLatencyOrderMessage(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfigWithExtra(
		t,
		`
web_search:
  exa:
    api_key: exa-key
`,
	)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	interaction := newSearchTypeCommandInteraction("member-user", "invalid-type")

	err := instance.handleSearchTypeCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle search type command: %v", err)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Unknown Exa search type. Available options " +
		"(ordered lowest to highest latency): `instant`, `fast`, `auto`, `deep-lite`, `deep`, `deep-reasoning`."
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
		Results: []webSearchResult{{
			Query: "latest ai news",
			Text:  "Title: Example Source\nURL: https://example.com/source\n",
		}},
		MaxURLs:             defaultWebSearchMaxURLs,
		VisualSearchSources: nil,
	}
	node.mu.Unlock()

	interaction := newShowSourcesInteraction()

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

	if !containsFold(response.Data.Content, "sources (1 total)") {
		t.Fatalf("expected total source count in response content: %q", response.Data.Content)
	}

	if !containsFold(response.Data.Content, "https://example.com/source") {
		t.Fatalf("expected source URL in response content: %q", response.Data.Content)
	}
}

func TestHandleInteractionCreateRespondsToShowSourcesButtonForVisualSearch(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.searchMetadata = testStructuredVisualSearchMetadata()
	node.mu.Unlock()

	interaction := newShowSourcesInteraction()

	instance.handleInteractionCreate(session, interaction)

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if response.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected response flags: %v", response.Data.Flags)
	}

	for _, fragment := range []string{
		"visual search result urls",
		"https://ru.ruwiki.ru/wiki/Sword_Art_Online",
		"https://yandex.com/images/search?cbir_page=similar-1",
		"http://vampireknightptk.blogspot.com/2012/09/indonic-hosting.html",
	} {
		if !containsFold(response.Data.Content, fragment) {
			t.Fatalf("expected fragment %q in response content: %q", fragment, response.Data.Content)
		}
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
		Results: []webSearchResult{{
			Query: "latest ai news",
			Text:  "Title: Example Source\nURL: https://example.com/source\n",
		}},
		MaxURLs:             defaultWebSearchMaxURLs,
		VisualSearchSources: nil,
	}
	tracker.pendingResponses = []pendingResponse{
		{
			messageID: "response-message",
			node:      instance.nodes.addPending("response-message", sourceMessage),
		},
	}

	tracker.release(instance.nodes, "assistant reply", "")

	interaction := newShowSourcesInteraction()

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

	if !containsFold(response.Data.Content, "sources (1 total)") {
		t.Fatalf("expected total source count in response content: %q", response.Data.Content)
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

	interaction := newShowSourcesInteraction()

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

	if !containsFold(response.Data.Content, "sources (15 total, page 1/") {
		t.Fatalf("expected total source count in paginated response content: %q", response.Data.Content)
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

func TestHandleInteractionCreateRespondsToShowThinkingButton(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = visibleResponseText("Plan first.", "Final answer.")
	node.thinkingText = "Plan first."
	node.initialized = true
	node.mu.Unlock()

	interaction := newShowThinkingInteraction()

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

	if !containsFold(response.Data.Content, "Thinking Process") {
		t.Fatalf("expected thinking header in response content: %q", response.Data.Content)
	}

	if !containsFold(response.Data.Content, "Plan first.") {
		t.Fatalf("expected thinking content in response: %q", response.Data.Content)
	}
}

func TestHandleInteractionCreateRespondsToShowThinkingButtonUsingPersistedFallback(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = visibleResponseText("Plan first.", "Final answer.")
	node.initialized = true
	node.mu.Unlock()

	interaction := newShowThinkingInteraction()

	instance.handleInteractionCreate(session, interaction)

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if !containsFold(response.Data.Content, "Plan first.") {
		t.Fatalf("expected extracted thinking content in response: %q", response.Data.Content)
	}
}

func TestHandleInteractionCreateRespondsToPaginatedShowThinkingButton(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.thinkingText = strings.Repeat("Thoughts on the problem.\n", 200)
	node.initialized = true
	node.mu.Unlock()

	interaction := newShowThinkingInteraction()

	instance.handleInteractionCreate(session, interaction)

	if response.Type != discordgo.InteractionResponseChannelMessageWithSource {
		t.Fatalf("unexpected response type: %v", response.Type)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if !containsFold(response.Data.Content, "page 1/") {
		t.Fatalf("expected first page indicator in response content: %q", response.Data.Content)
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

	nextButton, nextOK := row.Components[1].(*discordgo.Button)
	if !nextOK {
		t.Fatalf("expected next button, got %T", row.Components[1])
	}

	messageID, pageIndex, ok := parseShowThinkingPageButtonCustomID(nextButton.CustomID)
	if !ok {
		t.Fatalf("expected parsable next button custom id: %q", nextButton.CustomID)
	}

	if messageID != "response-message" || pageIndex != 1 {
		t.Fatalf("unexpected next button target: message=%q page=%d", messageID, pageIndex)
	}
}

func TestHandleInteractionCreateUpdatesShowThinkingPaginationPage(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.thinkingText = strings.Repeat("Thoughts on the problem.\n", 200)
	node.mu.Unlock()

	pageCount := len(formatThinkingPages(node.thinkingText))
	if pageCount < 2 {
		t.Fatalf("expected multiple pages, got %d", pageCount)
	}

	targetPageIndex := pageCount - 1
	interaction := newComponentInteraction(
		"ephemeral-message",
		showThinkingPageButtonCustomID("response-message", targetPageIndex),
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

	expectedTotalSources := fmt.Sprintf(
		"sources (%d total, page %d/%d)",
		countSearchSources(metadata),
		targetPageIndex+1,
		pageCount,
	)
	if !containsFold(response.Data.Content, expectedTotalSources) {
		t.Fatalf("expected total source count %q in response content: %q", expectedTotalSources, response.Data.Content)
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

func TestHandleInteractionCreateRespondsToCreateGistButton(t *testing.T) {
	t.Parallel()

	gist := new(fakeGistClient)
	gist.url = "https://gist.github.com/example"

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.gist = gist

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = testAssistantReply
	node.initialized = true
	node.mu.Unlock()

	interaction := newComponentInteraction("response-message", createGistButtonCustomID)

	instance.handleInteractionCreate(session, interaction)

	assertDeferredEphemeralInteractionResponse(t, &capture.deferredResponse)

	if !containsFold(capture.editedResponse.Content, gist.url) {
		t.Fatalf("expected gist url in edited response content: %q", capture.editedResponse.Content)
	}

	if gist.callCount != 1 {
		t.Fatalf("unexpected gist call count: %d", gist.callCount)
	}

	if len(gist.texts) != 1 || gist.texts[0] != testAssistantReply {
		t.Fatalf("unexpected gist request texts: %#v", gist.texts)
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	if node.gistURL != gist.url {
		t.Fatalf("unexpected cached gist url: %q", node.gistURL)
	}

	if capture.requestCount != 2 {
		t.Fatalf("unexpected request count: %d", capture.requestCount)
	}
}

func TestHandleInteractionCreateReusesCachedGistURL(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	gist := new(fakeGistClient)

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.gist = gist

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = testAssistantReply
	node.initialized = true
	node.gistURL = "https://gist.github.com/cached"
	node.mu.Unlock()

	interaction := newComponentInteraction("response-message", createGistButtonCustomID)

	instance.handleInteractionCreate(session, interaction)

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if !containsFold(response.Data.Content, node.gistURL) {
		t.Fatalf("expected cached gist url in response content: %q", response.Data.Content)
	}

	if gist.callCount != 0 {
		t.Fatalf("expected cached gist url to skip creation, got %d calls", gist.callCount)
	}
}

func TestHandleInteractionCreateRespondsToCreateGistButtonFailure(t *testing.T) {
	t.Parallel()

	gist := new(fakeGistClient)
	gist.err = errFakeGistUnavailable

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.gist = gist

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = testAssistantReply
	node.initialized = true
	node.mu.Unlock()

	interaction := newComponentInteraction("response-message", createGistButtonCustomID)

	instance.handleInteractionCreate(session, interaction)

	assertDeferredEphemeralInteractionResponse(t, &capture.deferredResponse)

	expectedContent := "Couldn't create a GitHub gist right now."
	if capture.editedResponse.Content != expectedContent {
		t.Fatalf(
			"unexpected edited failure response content: got %q want %q",
			capture.editedResponse.Content,
			expectedContent,
		)
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	if node.gistURL != "" {
		t.Fatalf("expected empty cached gist url, got %q", node.gistURL)
	}

	if capture.requestCount != 2 {
		t.Fatalf("unexpected request count: %d", capture.requestCount)
	}
}

func TestIsUnknownInteractionError(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf(
		"wrap: %w",
		newDiscordRESTError(discordUnknownInteractionCode, "Unknown interaction"),
	)

	if !isUnknownInteractionError(err) {
		t.Fatal("expected unknown interaction error to be detected")
	}
}

func TestIsUnknownInteractionErrorIgnoresOtherErrors(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf(
		"wrap: %w",
		newDiscordRESTError(http.StatusNotFound, "Not Found"),
	)

	if isUnknownInteractionError(err) {
		t.Fatal("expected non-interaction error to be ignored")
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

	return newInteractionTestSessionWithTransport(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		return captureInteractionCallbackRequest(t, request, response)
	}))
}

func captureInteractionCallbackRequest(
	t *testing.T,
	request *http.Request,
	response *discordgo.InteractionResponse,
) (*http.Response, error) {
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
}

func newInteractionTestSessionWithTransport(
	t *testing.T,
	transport roundTripFunc,
) *discordgo.Session {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	client := new(http.Client)
	client.Transport = transport
	session.Client = client

	return session
}

func newEditChannelNameTestSession(
	t *testing.T,
	response *discordgo.InteractionResponse,
	channelEditStatusCode int,
	channelEditBody string,
) *discordgo.Session {
	t.Helper()

	return newInteractionTestSessionWithTransport(
		t,
		roundTripFunc(func(request *http.Request) (*http.Response, error) {
			t.Helper()

			if request.Method == http.MethodPatch &&
				request.URL.Path == "/api/v9/channels/channel-id" {
				return newInteractionJSONResponse(request, channelEditStatusCode, channelEditBody), nil
			}

			return captureInteractionCallbackRequest(t, request, response)
		}),
	)
}

type moveChannelTestCapture struct {
	editBody string
}

func newMoveChannelTestSession(
	t *testing.T,
	response *discordgo.InteractionResponse,
	channelStatusCode int,
	channelBody string,
	editStatusCode int,
	editBody string,
	capture *moveChannelTestCapture,
) *discordgo.Session {
	t.Helper()

	return newInteractionTestSessionWithTransport(
		t,
		roundTripFunc(func(request *http.Request) (*http.Response, error) {
			t.Helper()

			switch {
			case request.Method == http.MethodGet && request.URL.Path == "/api/v9/channels/channel-id":
				return newInteractionJSONResponse(request, channelStatusCode, channelBody), nil
			case request.Method == http.MethodPatch && request.URL.Path == "/api/v9/channels/channel-id":
				if capture != nil {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Fatalf("read channel edit request body: %v", err)
					}

					capture.editBody = string(body)
				}

				return newInteractionJSONResponse(request, editStatusCode, editBody), nil
			default:
				return captureInteractionCallbackRequest(t, request, response)
			}
		}),
	)
}

func newDeferredInteractionTestSession(
	t *testing.T,
	capture *deferredInteractionCapture,
) *discordgo.Session {
	t.Helper()

	return newInteractionTestSessionWithTransport(
		t,
		roundTripFunc(func(request *http.Request) (*http.Response, error) {
			t.Helper()

			capture.requestCount++

			switch capture.requestCount {
			case 1:
				return captureDeferredInteractionRequest(t, request, &capture.deferredResponse)
			case 2:
				return captureEditedInteractionRequest(t, request, &capture.editedResponse)
			default:
				t.Fatalf("unexpected interaction request count: %d", capture.requestCount)

				return nil, errUnexpectedTestRequest
			}
		}),
	)
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

func newInteractionJSONResponse(request *http.Request, statusCode int, body string) *http.Response {
	response := new(http.Response)
	response.Status = fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode))
	response.StatusCode = statusCode
	response.Body = io.NopCloser(strings.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header = make(http.Header)
	response.Header.Set("Content-Type", "application/json")
	response.Request = request

	return response
}

func captureDeferredInteractionRequest(
	t *testing.T,
	request *http.Request,
	response *discordgo.InteractionResponse,
) (*http.Response, error) {
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
		t.Fatalf("read deferred response body: %v", err)
	}

	var decoded struct {
		Type discordgo.InteractionResponseType `json:"type"`
		Data *struct {
			Flags discordgo.MessageFlags `json:"flags,omitempty"`
		} `json:"data"`
	}

	err = json.Unmarshal(responseBody, &decoded)
	if err != nil {
		t.Fatalf("decode deferred interaction response: %v", err)
	}

	response.Type = decoded.Type
	if decoded.Data != nil {
		response.Data = new(discordgo.InteractionResponseData)
		response.Data.Flags = decoded.Data.Flags
	}

	return newNoContentResponse(request), nil
}

func captureEditedInteractionRequest(
	t *testing.T,
	request *http.Request,
	response *editedInteractionResponse,
) (*http.Response, error) {
	t.Helper()

	if request.Method != http.MethodPatch {
		t.Fatalf("unexpected edit method: %s", request.Method)
	}

	expectedPath := "/api/v9/webhooks/application-id/interaction-token/messages/@original"
	if request.URL.Path != expectedPath {
		t.Fatalf("unexpected edit path: got %q want %q", request.URL.Path, expectedPath)
	}

	responseBody, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("read edited response body: %v", err)
	}

	err = json.Unmarshal(responseBody, response)
	if err != nil {
		t.Fatalf("decode edited interaction response: %v", err)
	}

	return newInteractionJSONResponse(request, http.StatusOK, `{"id":"edited-message"}`), nil
}

func assertDeferredEphemeralInteractionResponse(
	t *testing.T,
	response *discordgo.InteractionResponse,
) {
	t.Helper()

	if response.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
		t.Fatalf(
			"unexpected deferred response type: got %v want %v",
			response.Type,
			discordgo.InteractionResponseDeferredChannelMessageWithSource,
		)
	}

	if response.Data == nil {
		t.Fatal("expected deferred interaction response data")
	}

	if response.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected deferred response flags: %v", response.Data.Flags)
	}
}

func newDiscordRESTError(code int, message string) *discordgo.RESTError {
	return &discordgo.RESTError{
		Request:      nil,
		Response:     nil,
		ResponseBody: nil,
		Message: &discordgo.APIErrorMessage{
			Code:    code,
			Message: message,
		},
	}
}

func newModelTestBot(configPath string) *bot {
	instance := new(bot)
	instance.configPath = configPath
	instance.currentModel = firstTestModel
	instance.currentExaSearchTypeValue = defaultExaSearchType

	return instance
}

func newModelCommandInteraction(userID, modelName string) *discordgo.InteractionCreate {
	return newModelCommandInteractionInChannel(userID, modelName, "")
}

func newModelCommandInteractionInChannel(
	userID, modelName, channelID string,
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
	userID, modelName string,
) *discordgo.InteractionCreate {
	return newSearchDeciderModelCommandInteractionInChannel(userID, modelName, "")
}

func newSearchDeciderModelCommandInteractionInChannel(
	userID, modelName, channelID string,
) *discordgo.InteractionCreate {
	return newConfiguredModelCommandInteraction(
		userID,
		modelName,
		searchDeciderModelCommandName,
		searchDeciderModelOptionName,
		channelID,
	)
}

func newSearchTypeCommandInteraction(userID, searchType string) *discordgo.InteractionCreate {
	return newConfiguredStringCommandInteraction(
		userID,
		searchType,
		searchTypeCommandName,
		searchTypeOptionName,
		"",
		discordgo.InteractionApplicationCommand,
	)
}

func newSearchTypeAutocompleteInteraction(userID, currentText string) *discordgo.InteractionCreate {
	return newConfiguredStringCommandInteraction(
		userID,
		currentText,
		searchTypeCommandName,
		searchTypeOptionName,
		"",
		discordgo.InteractionApplicationCommandAutocomplete,
	)
}

func newEditChannelNameCommandInteraction(channelID, newName string) *discordgo.InteractionCreate {
	user := new(discordgo.User)
	user.ID = "member-user"

	member := new(discordgo.Member)
	member.User = user

	channelIDOption := new(discordgo.ApplicationCommandInteractionDataOption)
	channelIDOption.Name = editChannelNameChannelIDOptionName
	channelIDOption.Type = discordgo.ApplicationCommandOptionString
	channelIDOption.Value = channelID

	newNameOption := new(discordgo.ApplicationCommandInteractionDataOption)
	newNameOption.Name = editChannelNameOptionName
	newNameOption.Type = discordgo.ApplicationCommandOptionString
	newNameOption.Value = newName

	var commandData discordgo.ApplicationCommandInteractionData

	commandData.Name = editChannelNameCommandName
	commandData.Options = []*discordgo.ApplicationCommandInteractionDataOption{channelIDOption, newNameOption}

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.AppID = "application-id"
	interaction.Token = "interaction-token"
	interaction.Type = discordgo.InteractionApplicationCommand
	interaction.Member = member
	interaction.Data = commandData

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}

func newMoveChannelCommandInteraction(
	channelID, movement string,
	howMany int,
) *discordgo.InteractionCreate {
	user := new(discordgo.User)
	user.ID = "member-user"

	member := new(discordgo.Member)
	member.User = user

	channelIDOption := new(discordgo.ApplicationCommandInteractionDataOption)
	channelIDOption.Name = moveChannelChannelIDOptionName
	channelIDOption.Type = discordgo.ApplicationCommandOptionString
	channelIDOption.Value = channelID

	movementOption := new(discordgo.ApplicationCommandInteractionDataOption)
	movementOption.Name = moveChannelMovementOptionName
	movementOption.Type = discordgo.ApplicationCommandOptionString
	movementOption.Value = movement

	howManyOption := new(discordgo.ApplicationCommandInteractionDataOption)
	howManyOption.Name = moveChannelHowManyOptionName
	howManyOption.Type = discordgo.ApplicationCommandOptionInteger
	howManyOption.Value = float64(howMany)

	var commandData discordgo.ApplicationCommandInteractionData

	commandData.Name = moveChannelCommandName
	commandData.Options = []*discordgo.ApplicationCommandInteractionDataOption{
		channelIDOption,
		movementOption,
		howManyOption,
	}

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.AppID = "application-id"
	interaction.Token = "interaction-token"
	interaction.Type = discordgo.InteractionApplicationCommand
	interaction.Member = member
	interaction.Data = commandData

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}

func newConfiguredModelCommandInteraction(
	userID, modelName, commandName, optionName, channelID string,
) *discordgo.InteractionCreate {
	return newConfiguredStringCommandInteraction(
		userID,
		modelName,
		commandName,
		optionName,
		channelID,
		discordgo.InteractionApplicationCommand,
	)
}

func newConfiguredStringCommandInteraction(
	userID string,
	optionValue string,
	commandName string,
	optionName string,
	channelID string,
	interactionType discordgo.InteractionType,
) *discordgo.InteractionCreate {
	user := new(discordgo.User)
	user.ID = userID

	member := new(discordgo.Member)
	member.User = user

	option := new(discordgo.ApplicationCommandInteractionDataOption)
	option.Name = optionName
	option.Type = discordgo.ApplicationCommandOptionString
	option.Value = optionValue
	option.Focused = interactionType == discordgo.InteractionApplicationCommandAutocomplete

	var commandData discordgo.ApplicationCommandInteractionData

	commandData.Name = commandName
	commandData.Options = []*discordgo.ApplicationCommandInteractionDataOption{option}

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.AppID = "application-id"
	interaction.Token = "interaction-token"
	interaction.Type = interactionType
	interaction.ChannelID = channelID
	interaction.Member = member
	interaction.Data = commandData

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}

func newShowSourcesInteraction() *discordgo.InteractionCreate {
	return newComponentInteraction("response-message", showSourcesButtonCustomID)
}

func newShowThinkingInteraction() *discordgo.InteractionCreate {
	return newComponentInteraction("response-message", showThinkingButtonCustomID)
}

func newComponentInteraction(messageID, customID string) *discordgo.InteractionCreate {
	message := new(discordgo.Message)
	message.ID = messageID

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.AppID = "application-id"
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
