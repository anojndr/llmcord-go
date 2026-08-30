package app

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

	var (
		response discordgo.InteractionResponse
		capture  editChannelNameTestCapture
	)

	session := newEditChannelNameTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"new-name"}`,
		&capture,
	)
	instance := new(bot)
	interaction := newEditChannelNameCommandInteraction("channel-id", "new-name")

	err := instance.handleEditChannelNameCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle edit channel name command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	expectedContent := "Renamed channel to `new-name`."
	if capture.editedResponse.Content != expectedContent {
		t.Fatalf("unexpected edited response content: got %q want %q", capture.editedResponse.Content, expectedContent)
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

	var (
		response discordgo.InteractionResponse
		capture  editChannelNameTestCapture
	)

	session := newEditChannelNameTestSession(
		t,
		&response,
		http.StatusForbidden,
		`{"message":"Missing Permissions","code":50013}`,
		&capture,
	)
	instance := new(bot)
	interaction := newEditChannelNameCommandInteraction("channel-id", "new-name")

	err := instance.handleEditChannelNameCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle edit channel name command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	expectedContent := "Failed to rename channel `channel-id`."
	if capture.editedResponse.Content != expectedContent {
		t.Fatalf("unexpected edited response content: got %q want %q", capture.editedResponse.Content, expectedContent)
	}
}

func TestHandleApplicationCommandInteractionDispatchesEditChannelName(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  editChannelNameTestCapture
	)

	session := newEditChannelNameTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"new-name"}`,
		&capture,
	)
	instance := new(bot)
	interaction := newEditChannelNameCommandInteraction("channel-id", "new-name")

	err := instance.handleApplicationCommandInteraction(session, interaction)
	if err != nil {
		t.Fatalf("handle application command interaction: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	expectedContent := "Renamed channel to `new-name`."
	if capture.editedResponse.Content != expectedContent {
		t.Fatalf("unexpected edited response content: got %q want %q", capture.editedResponse.Content, expectedContent)
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

func TestHandleCreateChannelCommandCreatesChannelInCurrentCategory(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  createChannelTestCapture
	)

	session := newCreateChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"current-channel-id","guild_id":"guild-id","parent_id":"section-id"}`,
		http.StatusOK,
		`{"id":"new-channel-id","name":"new-name","guild_id":"guild-id","parent_id":"section-id"}`,
		&capture,
	)

	err := new(bot).handleCreateChannelCommand(
		session,
		newCreateChannelCommandInteraction("new-name"),
	)
	if err != nil {
		t.Fatalf("handle create channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	if capture.editedResponse.Content != "Created channel `new-name`." {
		t.Fatalf("unexpected edited response content: %q", capture.editedResponse.Content)
	}

	var createBody struct {
		Name     string `json:"name"`
		Type     int    `json:"type"`
		ParentID string `json:"parent_id"`
	}

	err = json.Unmarshal([]byte(capture.createBody), &createBody)
	if err != nil {
		t.Fatalf("decode create body: %v", err)
	}

	if createBody.Name != "new-name" ||
		createBody.Type != int(discordgo.ChannelTypeGuildText) ||
		createBody.ParentID != "section-id" {
		t.Fatalf("unexpected create body: %+v", createBody)
	}
}

func TestHandleCreateChannelCommandCreatesChannelAtGuildRoot(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  createChannelTestCapture
	)

	session := newCreateChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"current-channel-id","guild_id":"guild-id"}`,
		http.StatusOK,
		`{"id":"new-channel-id","name":"new-name","guild_id":"guild-id"}`,
		&capture,
	)

	err := new(bot).handleCreateChannelCommand(
		session,
		newCreateChannelCommandInteraction("new-name"),
	)
	if err != nil {
		t.Fatalf("handle create channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	if capture.editedResponse.Content != "Created channel `new-name`." {
		t.Fatalf("unexpected edited response content: %q", capture.editedResponse.Content)
	}

	var createBody struct {
		Name     string `json:"name"`
		Type     int    `json:"type"`
		ParentID string `json:"parent_id"`
	}

	err = json.Unmarshal([]byte(capture.createBody), &createBody)
	if err != nil {
		t.Fatalf("decode create body: %v", err)
	}

	if createBody.Name != "new-name" ||
		createBody.Type != int(discordgo.ChannelTypeGuildText) ||
		createBody.ParentID != "" {
		t.Fatalf("unexpected create body: %+v", createBody)
	}
}

func TestHandleCreateChannelCommandRequiresChannelName(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newCreateChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"current-channel-id","guild_id":"guild-id"}`,
		http.StatusOK,
		`{"id":"new-channel-id","name":"new-name","guild_id":"guild-id"}`,
		nil,
	)

	err := new(bot).handleCreateChannelCommand(
		session,
		newCreateChannelCommandInteraction(""),
	)
	if err != nil {
		t.Fatalf("handle create channel command: %v", err)
	}

	if response.Data == nil || response.Data.Content != "`channelname` is required." {
		t.Fatalf("unexpected response: %+v", response.Data)
	}
}

func TestHandleCreateChannelCommandRejectsNonGuildInteraction(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newCreateChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"current-channel-id","guild_id":"guild-id"}`,
		http.StatusOK,
		`{"id":"new-channel-id","name":"new-name","guild_id":"guild-id"}`,
		nil,
	)

	interaction := newCreateChannelCommandInteraction("new-name")
	interaction.GuildID = ""

	err := new(bot).handleCreateChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle create channel command: %v", err)
	}

	if response.Data == nil || response.Data.Content != "This command can only be used in a guild." {
		t.Fatalf("unexpected response: %+v", response.Data)
	}
}

func TestHandleCreateChannelCommandReportsChannelCreateFailure(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  createChannelTestCapture
	)

	session := newCreateChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"current-channel-id","guild_id":"guild-id","parent_id":"section-id"}`,
		http.StatusForbidden,
		`{"message":"Missing Permissions","code":50013}`,
		&capture,
	)

	err := new(bot).handleCreateChannelCommand(
		session,
		newCreateChannelCommandInteraction("new-name"),
	)
	if err != nil {
		t.Fatalf("handle create channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	if capture.editedResponse.Content != "Failed to create channel `new-name`." {
		t.Fatalf("unexpected edited response content: %q", capture.editedResponse.Content)
	}
}

func TestHandleCreateChannelCommandReportsCurrentChannelLoadFailure(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  createChannelTestCapture
	)

	session := newCreateChannelTestSession(
		t,
		&response,
		http.StatusInternalServerError,
		`{"message":"Internal Server Error","code":0}`,
		http.StatusOK,
		`{"id":"new-channel-id","name":"new-name","guild_id":"guild-id"}`,
		&capture,
	)

	err := new(bot).handleCreateChannelCommand(
		session,
		newCreateChannelCommandInteraction("new-name"),
	)
	if err != nil {
		t.Fatalf("handle create channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	if capture.editedResponse.Content != "Failed to load channel `current-channel-id`." {
		t.Fatalf("unexpected edited response content: %q", capture.editedResponse.Content)
	}
}

func TestHandleApplicationCommandInteractionDispatchesCreateChannel(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  createChannelTestCapture
	)

	session := newCreateChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"current-channel-id","guild_id":"guild-id"}`,
		http.StatusOK,
		`{"id":"new-channel-id","name":"new-name","guild_id":"guild-id"}`,
		&capture,
	)

	err := new(bot).handleApplicationCommandInteraction(
		session,
		newCreateChannelCommandInteraction("new-name"),
	)
	if err != nil {
		t.Fatalf("handle application command interaction: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	if capture.editedResponse.Content != "Created channel `new-name`." {
		t.Fatalf("unexpected edited response content: %q", capture.editedResponse.Content)
	}
}

func TestNewCreateChannelCommand(t *testing.T) {
	t.Parallel()

	command := newCreateChannelCommand()

	if command.Name != createChannelCommandName {
		t.Fatalf("unexpected command name: got %q want %q", command.Name, createChannelCommandName)
	}

	if command.Description != createChannelCommandDescription {
		t.Fatalf("unexpected command description: got %q want %q", command.Description, createChannelCommandDescription)
	}

	if len(command.Options) != 1 {
		t.Fatalf("unexpected option count: got %d want 1", len(command.Options))
	}

	nameOption := command.Options[0]
	if nameOption.Name != createChannelNameOptionName ||
		nameOption.Type != discordgo.ApplicationCommandOptionString ||
		!nameOption.Required {
		t.Fatalf("unexpected channel name option: %+v", nameOption)
	}
}

func TestHandleMoveChannelCommandMovesChannelUpAcrossTwoSiblings(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  moveChannelTestCapture
	)

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"general","guild_id":"guild-id","parent_id":"section-id","position":20}`,
		http.StatusOK,
		`[{"id":"first","name":"first","parent_id":"section-id","position":10},{"id":"second","name":"second","parent_id":"section-id","position":20},{"id":"channel-id","name":"general","guild_id":"guild-id","parent_id":"section-id","position":30},{"id":"last","name":"last","parent_id":"section-id","position":40}]`,
		http.StatusNoContent,
		"",
		&capture,
	)

	err := new(bot).handleMoveChannelCommand(session, newMoveChannelCommandInteraction("channel-id", "up", 2))
	if err != nil {
		t.Fatalf("handle move channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	if capture.editedResponse.Content != "Moved channel `general` up 2 visible channel(s)." {
		t.Fatalf("unexpected edited response content: %q", capture.editedResponse.Content)
	}

	var updates []struct {
		ID       string `json:"id"`
		Position int    `json:"position"`
	}
	if err := json.Unmarshal([]byte(capture.editBody), &updates); err != nil {
		t.Fatalf("decode reorder body: %v", err)
	}

	want := []struct {
		id       string
		position int
	}{{"channel-id", 10}, {"first", 20}, {"second", 30}}
	if len(updates) != len(want) {
		t.Fatalf("unexpected reorder update count: got %d want %d", len(updates), len(want))
	}

	for index, expected := range want {
		if updates[index].ID != expected.id || updates[index].Position != expected.position {
			t.Fatalf("unexpected reorder update %d: got %+v want id=%q position=%d", index, updates[index], expected.id, expected.position)
		}
	}
}

func TestHandleMoveChannelCommandMovesChannelDown(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  moveChannelTestCapture
	)

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"general","guild_id":"guild-id","parent_id":"section-id","position":10}`,
		http.StatusOK,
		`[{"id":"channel-id","name":"general","parent_id":"section-id","position":10},{"id":"middle","name":"middle","parent_id":"section-id","position":20},{"id":"last","name":"last","parent_id":"section-id","position":30}]`,
		http.StatusNoContent,
		"",
		&capture,
	)

	err := new(bot).handleMoveChannelCommand(session, newMoveChannelCommandInteraction("channel-id", "down", 2))
	if err != nil {
		t.Fatalf("handle move channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	if capture.editedResponse.Content != "Moved channel `general` down 2 visible channel(s)." {
		t.Fatalf("unexpected edited response content: %q", capture.editedResponse.Content)
	}

	var updates []struct {
		ID       string `json:"id"`
		Position int    `json:"position"`
	}
	if err := json.Unmarshal([]byte(capture.editBody), &updates); err != nil {
		t.Fatalf("decode reorder body: %v", err)
	}

	want := []struct {
		id       string
		position int
	}{{"middle", 10}, {"last", 20}, {"channel-id", 30}}
	if len(updates) != len(want) {
		t.Fatalf("unexpected reorder update count: got %d want %d", len(updates), len(want))
	}

	for index, expected := range want {
		if updates[index].ID != expected.id || updates[index].Position != expected.position {
			t.Fatalf("unexpected reorder update %d: got %+v want id=%q position=%d", index, updates[index], expected.id, expected.position)
		}
	}
}

func TestHandleMoveChannelCommandClampsAtSectionBoundary(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  moveChannelTestCapture
	)

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"general","guild_id":"guild-id","parent_id":"section-id","position":20}`,
		http.StatusOK,
		`[{"id":"first","name":"first","parent_id":"section-id","position":10},{"id":"channel-id","name":"general","parent_id":"section-id","position":20},{"id":"other","name":"other","parent_id":"other-section","position":30}]`,
		http.StatusNoContent,
		"",
		&capture,
	)

	err := new(bot).handleMoveChannelCommand(session, newMoveChannelCommandInteraction("channel-id", "up", 5))
	if err != nil {
		t.Fatalf("handle move channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	if capture.editedResponse.Content != "Moved channel `general` up 1 visible channel(s)." {
		t.Fatalf("unexpected edited response content: %q", capture.editedResponse.Content)
	}

	var updates []struct {
		ID       string `json:"id"`
		Position int    `json:"position"`
	}
	if err := json.Unmarshal([]byte(capture.editBody), &updates); err != nil {
		t.Fatalf("decode reorder body: %v", err)
	}

	if len(updates) != 2 || updates[0].ID != "channel-id" || updates[0].Position != 10 || updates[1].ID != "first" || updates[1].Position != 20 {
		t.Fatalf("unexpected boundary reorder updates: %+v", updates)
	}
}

func TestHandleMoveChannelCommandDoesNotCrossDifferentParent(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  moveChannelTestCapture
	)

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"general","guild_id":"guild-id","parent_id":"section-id","position":20}`,
		http.StatusOK,
		`[{"id":"channel-id","name":"general","parent_id":"section-id","position":20},{"id":"other","name":"other","parent_id":"other-section","position":10},{"id":"category","name":"category","type":4,"position":30}]`,
		http.StatusNoContent,
		"",
		&capture,
	)

	err := new(bot).handleMoveChannelCommand(session, newMoveChannelCommandInteraction("channel-id", "down", 1))
	if err != nil {
		t.Fatalf("handle move channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	if capture.editedResponse.Content != "Channel `general` is already as far down as possible." {
		t.Fatalf("unexpected edited response content: %q", capture.editedResponse.Content)
	}

	if capture.editBody != "" {
		t.Fatalf("expected no reorder across parent/category, got %q", capture.editBody)
	}
}

func TestChannelPositionUpdatesBreaksDuplicatePositions(t *testing.T) {
	t.Parallel()

	before := []*discordgo.Channel{
		{ID: "100000000000000000", Position: 10},
		{ID: "99999999999999999", Position: 10},
	}
	after := []*discordgo.Channel{before[1], before[0]}

	updates := channelPositionUpdates(before, after)
	if len(updates) != 2 {
		t.Fatalf("unexpected update count: got %d want 2", len(updates))
	}

	if updates[0].ID != "99999999999999999" || updates[0].Position != 0 {
		t.Fatalf("unexpected first update: %+v", updates[0])
	}

	if updates[1].ID != "100000000000000000" || updates[1].Position != 1 {
		t.Fatalf("unexpected second update: %+v", updates[1])
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

	var (
		response discordgo.InteractionResponse
		capture  moveChannelTestCapture
	)

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusNotFound,
		`{"message":"Unknown Channel","code":10003}`,
		0,
		"",
		0,
		"",
		&capture,
	)
	instance := new(bot)
	interaction := newMoveChannelCommandInteraction("channel-id", "up", 1)

	err := instance.handleMoveChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle move channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	expectedContent := "Failed to load channel `channel-id`."
	if capture.editedResponse.Content != expectedContent {
		t.Fatalf("unexpected edited response content: got %q want %q", capture.editedResponse.Content, expectedContent)
	}
}

func TestHandleMoveChannelCommandReportsMoveFailure(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  moveChannelTestCapture
	)

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"general","guild_id":"guild-id","position":5}`,
		http.StatusOK,
		`[{"id":"first","name":"first","position":4},{"id":"channel-id","name":"general","position":5}]`,
		http.StatusForbidden,
		`{"message":"Missing Permissions","code":50013}`,
		&capture,
	)
	instance := new(bot)
	interaction := newMoveChannelCommandInteraction("channel-id", "up", 1)

	err := instance.handleMoveChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle move channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	expectedContent := "Failed to move channel `channel-id`."
	if capture.editedResponse.Content != expectedContent {
		t.Fatalf("unexpected edited response content: got %q want %q", capture.editedResponse.Content, expectedContent)
	}
}

func TestHandleApplicationCommandInteractionDispatchesMoveChannel(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  moveChannelTestCapture
	)

	session := newMoveChannelTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-id","name":"general","guild_id":"guild-id","position":5}`,
		http.StatusOK,
		`[{"id":"first","name":"first","position":4},{"id":"channel-id","name":"general","position":5}]`,
		http.StatusNoContent,
		"",
		&capture,
	)
	instance := new(bot)
	interaction := newMoveChannelCommandInteraction("channel-id", "up", 2)

	err := instance.handleApplicationCommandInteraction(session, interaction)
	if err != nil {
		t.Fatalf("handle application command interaction: %v", err)
	}

	assertDeferredInteractionResponse(t, &response)

	expectedContent := "Moved channel `general` up 1 visible channel(s)."
	if capture.editedResponse.Content != expectedContent {
		t.Fatalf("unexpected edited response content: got %q want %q", capture.editedResponse.Content, expectedContent)
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

	if !strings.Contains(capture.editedResponse.Content, "<"+gist.url+">") {
		t.Fatalf("expected angle-bracket-wrapped gist url in edited response content: %q", capture.editedResponse.Content)
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

	if !strings.Contains(response.Data.Content, "<"+node.gistURL+">") {
		t.Fatalf("expected angle-bracket-wrapped cached gist url in response content: %q", response.Data.Content)
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

type editChannelNameTestCapture struct {
	editedResponse editedInteractionResponse
}

func newEditChannelNameTestSession(
	t *testing.T,
	response *discordgo.InteractionResponse,
	channelEditStatusCode int,
	channelEditBody string,
	capture *editChannelNameTestCapture,
) *discordgo.Session {
	t.Helper()

	return newInteractionTestSessionWithTransport(
		t,
		roundTripFunc(func(request *http.Request) (*http.Response, error) {
			t.Helper()

			switch {
			case request.Method == http.MethodPatch &&
				request.URL.Path == "/api/v9/channels/channel-id":
				return newInteractionJSONResponse(request, channelEditStatusCode, channelEditBody), nil
			case request.Method == http.MethodPatch &&
				strings.HasSuffix(request.URL.Path, "/messages/@original"):
				if capture == nil {
					t.Fatal("unexpected interaction response edit")
				}

				return captureEditedInteractionRequest(t, request, &capture.editedResponse)
			default:
				return captureInteractionCallbackRequest(t, request, response)
			}
		}),
	)
}

type moveChannelTestCapture struct {
	editBody       string
	editedResponse editedInteractionResponse
}

func newMoveChannelTestSession(
	t *testing.T,
	response *discordgo.InteractionResponse,
	channelStatusCode int,
	channelBody string,
	channelsStatusCode int,
	channelsBody string,
	reorderStatusCode int,
	reorderBody string,
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
			case request.Method == http.MethodGet && request.URL.Path == "/api/v9/guilds/guild-id/channels":
				return newInteractionJSONResponse(request, channelsStatusCode, channelsBody), nil
			case request.Method == http.MethodPatch && request.URL.Path == "/api/v9/guilds/guild-id/channels":
				if capture != nil {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Fatalf("read channel reorder request body: %v", err)
					}

					capture.editBody = string(body)
				}

				if reorderStatusCode == http.StatusNoContent {
					return newNoContentResponse(request), nil
				}

				return newInteractionJSONResponse(request, reorderStatusCode, reorderBody), nil
			case request.Method == http.MethodPatch &&
				strings.HasSuffix(request.URL.Path, "/messages/@original"):
				if capture == nil {
					t.Fatal("unexpected interaction response edit")
				}

				return captureEditedInteractionRequest(t, request, &capture.editedResponse)
			default:
				return captureInteractionCallbackRequest(t, request, response)
			}
		}),
	)
}

type createChannelTestCapture struct {
	createBody     string
	editedResponse editedInteractionResponse
}

func newCreateChannelTestSession(
	t *testing.T,
	response *discordgo.InteractionResponse,
	currentChannelStatusCode int,
	currentChannelBody string,
	channelCreateStatusCode int,
	channelCreateBody string,
	capture *createChannelTestCapture,
) *discordgo.Session {
	t.Helper()

	return newInteractionTestSessionWithTransport(
		t,
		roundTripFunc(func(request *http.Request) (*http.Response, error) {
			t.Helper()

			switch {
			case request.Method == http.MethodGet && request.URL.Path == "/api/v9/channels/current-channel-id":
				return newInteractionJSONResponse(request, currentChannelStatusCode, currentChannelBody), nil
			case request.Method == http.MethodPost && request.URL.Path == "/api/v9/guilds/guild-id/channels":
				if capture != nil {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Fatalf("read channel create request body: %v", err)
					}

					capture.createBody = string(body)
				}

				return newInteractionJSONResponse(request, channelCreateStatusCode, channelCreateBody), nil
			case request.Method == http.MethodPatch &&
				strings.HasSuffix(request.URL.Path, "/messages/@original"):
				if capture == nil {
					t.Fatal("unexpected interaction response edit")
				}

				return captureEditedInteractionRequest(t, request, &capture.editedResponse)
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

func assertDeferredInteractionResponse(
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
	interaction.GuildID = "guild-id"
	interaction.Member = member
	interaction.Data = commandData

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}

func newCreateChannelCommandInteraction(channelName string) *discordgo.InteractionCreate {
	user := new(discordgo.User)
	user.ID = "member-user"

	member := new(discordgo.Member)
	member.User = user

	nameOption := new(discordgo.ApplicationCommandInteractionDataOption)
	nameOption.Name = createChannelNameOptionName
	nameOption.Type = discordgo.ApplicationCommandOptionString
	nameOption.Value = channelName

	var commandData discordgo.ApplicationCommandInteractionData

	commandData.Name = createChannelCommandName
	commandData.Options = []*discordgo.ApplicationCommandInteractionDataOption{nameOption}

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.AppID = "application-id"
	interaction.Token = "interaction-token"
	interaction.Type = discordgo.InteractionApplicationCommand
	interaction.GuildID = "guild-id"
	interaction.ChannelID = "current-channel-id"
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
