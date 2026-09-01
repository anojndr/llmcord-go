package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestNewMaintenanceCommand(t *testing.T) {
	t.Parallel()

	command := newMaintenanceCommand()

	if command.Name != maintenanceCommandName {
		t.Fatalf("unexpected command name: got %q want %q", command.Name, maintenanceCommandName)
	}

	if command.Description != maintenanceCommandDescription {
		t.Fatalf("unexpected command description: got %q want %q", command.Description, maintenanceCommandDescription)
	}

	if len(command.Options) != 2 {
		t.Fatalf("expected 2 subcommands, got %d", len(command.Options))
	}

	startOption := command.Options[0]
	if startOption.Name != maintenanceStartSubcommandName {
		t.Fatalf("unexpected start subcommand name: %q", startOption.Name)
	}

	if startOption.Type != discordgo.ApplicationCommandOptionSubCommand {
		t.Fatalf("unexpected start subcommand type: %v", startOption.Type)
	}

	if len(startOption.Options) != 1 || startOption.Options[0].Name != maintenanceChannelIDOptionName {
		t.Fatalf("unexpected start subcommand options: %#v", startOption.Options)
	}

	stopOption := command.Options[1]
	if stopOption.Name != maintenanceStopSubcommandName {
		t.Fatalf("unexpected stop subcommand name: %q", stopOption.Name)
	}

	if len(stopOption.Options) != 1 || stopOption.Options[0].Name != maintenanceChannelIDOptionName {
		t.Fatalf("unexpected stop subcommand options: %#v", stopOption.Options)
	}
}

func TestHandleMaintenanceCommandRequiresOwner(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)

	instance := new(bot)
	instance.maintenanceChannels = make(map[string]struct{})

	interaction := newMaintenanceCommandInteraction("start", "channel-123", "some-other-user")

	err := instance.handleMaintenanceCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle maintenance command: %v", err)
	}

	if response.Data == nil || !strings.Contains(response.Data.Content, "do not have permission") {
		t.Fatalf("unexpected response: %+v", response.Data)
	}

	if instance.isMaintenanceChannel("channel-123") {
		t.Fatal("expected channel not to be in maintenance")
	}
}

func TestHandleMaintenanceCommandStartEnablesMaintenance(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  maintenanceTestCapture
	)

	session := newMaintenanceTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-123","guild_id":"guild-123"}`,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		&capture,
	)

	instance := new(bot)
	instance.maintenanceChannels = make(map[string]struct{})
	instance.session = session

	interaction := newMaintenanceCommandInteraction("start", "channel-123", maintenanceOwnerID)

	err := instance.handleMaintenanceCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle maintenance command: %v", err)
	}

	if !instance.isMaintenanceChannel("channel-123") {
		t.Fatal("expected channel to be in maintenance")
	}

	if capture.permissionPuts["guild-123"] != discordgo.PermissionSendMessages {
		t.Fatalf("expected @everyone deny SendMessages, got %v", capture.permissionPuts["guild-123"])
	}

	if capture.permissionPuts[maintenanceOwnerID] != discordgo.PermissionViewChannel|discordgo.PermissionSendMessages {
		t.Fatalf("expected owner allow View+Send, got %v", capture.permissionPuts[maintenanceOwnerID])
	}

	if capture.permissionPuts[maintenanceBotID] != discordgo.PermissionViewChannel|discordgo.PermissionSendMessages {
		t.Fatalf("expected bot allow View+Send, got %v", capture.permissionPuts[maintenanceBotID])
	}

	if capture.editedResponse.Content == "" || !strings.Contains(capture.editedResponse.Content, "Maintenance enabled") {
		t.Fatalf("unexpected edited response: %q", capture.editedResponse.Content)
	}
}

func TestHandleMaintenanceCommandStartAlreadyEnabled(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  maintenanceTestCapture
	)

	session := newMaintenanceTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-123","guild_id":"guild-123"}`,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		&capture,
	)

	instance := new(bot)
	instance.maintenanceChannels = map[string]struct{}{"channel-123": {}}
	instance.session = session

	interaction := newMaintenanceCommandInteraction("start", "channel-123", maintenanceOwnerID)

	err := instance.handleMaintenanceCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle maintenance command: %v", err)
	}

	if len(capture.permissionPuts) != 0 {
		t.Fatalf("expected no permission puts when already enabled, got %v", capture.permissionPuts)
	}

	if !strings.Contains(capture.editedResponse.Content, "already in maintenance") {
		t.Fatalf("unexpected response: %q", capture.editedResponse.Content)
	}
}

func TestHandleMaintenanceCommandStopDisablesMaintenance(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  maintenanceTestCapture
	)

	session := newMaintenanceTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-123","guild_id":"guild-123"}`,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		&capture,
	)

	instance := new(bot)
	instance.maintenanceChannels = map[string]struct{}{"channel-123": {}}
	instance.session = session

	interaction := newMaintenanceCommandInteraction("stop", "channel-123", maintenanceOwnerID)

	err := instance.handleMaintenanceCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle maintenance command: %v", err)
	}

	if instance.isMaintenanceChannel("channel-123") {
		t.Fatal("expected channel not to be in maintenance")
	}

	if len(capture.permissionDeletes) != 3 {
		t.Fatalf("expected 3 deletes, got %v", capture.permissionDeletes)
	}

	if !capture.permissionDeletes["guild-123"] || !capture.permissionDeletes[maintenanceOwnerID] || !capture.permissionDeletes[maintenanceBotID] {
		t.Fatalf("expected deletes for everyone, owner and bot, got %v", capture.permissionDeletes)
	}

	if !strings.Contains(capture.editedResponse.Content, "Maintenance disabled") {
		t.Fatalf("unexpected response: %q", capture.editedResponse.Content)
	}
}

func TestHandleMaintenanceCommandStopNotEnabled(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  maintenanceTestCapture
	)

	session := newMaintenanceTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-123","guild_id":"guild-123"}`,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		&capture,
	)

	instance := new(bot)
	instance.maintenanceChannels = make(map[string]struct{})
	instance.session = session

	interaction := newMaintenanceCommandInteraction("stop", "channel-123", maintenanceOwnerID)

	err := instance.handleMaintenanceCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle maintenance command: %v", err)
	}

	if !strings.Contains(capture.editedResponse.Content, "not in maintenance") {
		t.Fatalf("unexpected response: %q", capture.editedResponse.Content)
	}
}

func TestHandleMaintenanceCommandRejectsMissingChannelID(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)

	instance := new(bot)
	instance.maintenanceChannels = make(map[string]struct{})

	interaction := newMaintenanceCommandInteraction("start", "", maintenanceOwnerID)

	err := instance.handleMaintenanceCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle maintenance command: %v", err)
	}

	if response.Data == nil || !strings.Contains(response.Data.Content, "channel_id") {
		t.Fatalf("unexpected response: %+v", response.Data)
	}
}

func TestEnforceMaintenanceModeBlocksNonOwner(t *testing.T) {
	t.Parallel()

	var messageDeleteCalled bool

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodDelete && strings.Contains(request.URL.Path, "/messages/") {
			messageDeleteCalled = true

			return newNoContentResponse(request), nil
		}

		if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/v9/channels/") {
			body := `{"id":"channel-123","guild_id":"guild-123","parent_id":""}`
			return newInteractionJSONResponse(request, http.StatusOK, body), nil
		}

		t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

		return nil, nil
	})
	session.Client = client

	instance := new(bot)
	instance.session = session
	instance.maintenanceChannels = map[string]struct{}{"channel-123": {}}

	message := new(discordgo.Message)
	message.ID = "msg-1"
	message.ChannelID = "channel-123"
	message.GuildID = "guild-123"
	message.Author = new(discordgo.User)
	message.Author.ID = "some-user"
	message.Author.Bot = false

	blocked := instance.enforceMaintenanceMode(message)
	if !blocked {
		t.Fatal("expected message to be blocked")
	}

	if !messageDeleteCalled {
		t.Fatal("expected ChannelMessageDelete to be called")
	}
}

func TestEnforceMaintenanceModeAllowsOwner(t *testing.T) {
	t.Parallel()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/v9/channels/") {
			body := `{"id":"channel-123","guild_id":"guild-123","parent_id":""}`
			return newInteractionJSONResponse(request, http.StatusOK, body), nil
		}

		t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

		return nil, nil
	})
	session.Client = client

	instance := new(bot)
	instance.session = session
	instance.maintenanceChannels = map[string]struct{}{"channel-123": {}}

	message := new(discordgo.Message)
	message.ID = "msg-1"
	message.ChannelID = "channel-123"
	message.GuildID = "guild-123"
	message.Author = new(discordgo.User)
	message.Author.ID = maintenanceOwnerID
	message.Author.Bot = false

	blocked := instance.enforceMaintenanceMode(message)
	if blocked {
		t.Fatal("expected owner message not to be blocked")
	}
}

func TestEnforceMaintenanceModeAllowsBot(t *testing.T) {
	t.Parallel()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/v9/channels/") {
			body := `{"id":"channel-123","guild_id":"guild-123","parent_id":""}`
			return newInteractionJSONResponse(request, http.StatusOK, body), nil
		}

		t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

		return nil, nil
	})
	session.Client = client

	instance := new(bot)
	instance.session = session
	instance.maintenanceChannels = map[string]struct{}{"channel-123": {}}

	message := new(discordgo.Message)
	message.ID = "msg-1"
	message.ChannelID = "channel-123"
	message.GuildID = "guild-123"
	message.Author = new(discordgo.User)
	message.Author.ID = maintenanceBotID
	message.Author.Bot = true

	blocked := instance.enforceMaintenanceMode(message)
	if blocked {
		t.Fatal("expected bot message not to be blocked")
	}
}

func TestEnforceMaintenanceModeAllowsWhenNotInMaintenance(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.maintenanceChannels = make(map[string]struct{})

	message := new(discordgo.Message)
	message.ID = "msg-1"
	message.ChannelID = "channel-123"
	message.Author = new(discordgo.User)
	message.Author.ID = "some-user"

	blocked := instance.enforceMaintenanceMode(message)
	if blocked {
		t.Fatal("expected message not to be blocked when not in maintenance")
	}
}

func TestHandleApplicationCommandInteractionDispatchesMaintenance(t *testing.T) {
	t.Parallel()

	var (
		response discordgo.InteractionResponse
		capture  maintenanceTestCapture
	)

	session := newMaintenanceTestSession(
		t,
		&response,
		http.StatusOK,
		`{"id":"channel-123","guild_id":"guild-123"}`,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		&capture,
	)

	instance := new(bot)
	instance.maintenanceChannels = make(map[string]struct{})
	instance.session = session

	interaction := newMaintenanceCommandInteraction("start", "channel-123", maintenanceOwnerID)

	err := instance.handleApplicationCommandInteraction(session, interaction)
	if err != nil {
		t.Fatalf("handle application command interaction: %v", err)
	}

	if !instance.isMaintenanceChannel("channel-123") {
		t.Fatal("expected maintenance to be enabled via dispatched command")
	}
}

func TestIsMaintenanceChannelAndBypassHelpers(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.maintenanceChannels = make(map[string]struct{})

	if instance.isMaintenanceChannel("channel-123") {
		t.Fatal("expected not in maintenance initially")
	}

	instance.setMaintenanceChannel("channel-123")

	if !instance.isMaintenanceChannel("channel-123") {
		t.Fatal("expected in maintenance after set")
	}

	if !instance.isAllowedMaintenanceBypass(maintenanceOwnerID) {
		t.Fatal("expected owner to be allowed bypass")
	}

	if !instance.isAllowedMaintenanceBypass(maintenanceBotID) {
		t.Fatal("expected bot to be allowed bypass")
	}

	if instance.isAllowedMaintenanceBypass("other-user") {
		t.Fatal("expected other user not to bypass")
	}

	instance.clearMaintenanceChannel("channel-123")

	if instance.isMaintenanceChannel("channel-123") {
		t.Fatal("expected not in maintenance after clear")
	}
}

type maintenanceTestCapture struct {
	permissionPuts    map[string]int64
	permissionDeletes map[string]bool
	editedResponse    editedInteractionResponse
}

func newMaintenanceTestSession(
	t *testing.T,
	response *discordgo.InteractionResponse,
	channelStatusCode int,
	channelBody string,
	putEveryoneStatus int,
	putOwnerStatus int,
	deleteEveryoneStatus int,
	deleteOwnerStatus int,
	capture *maintenanceTestCapture,
) *discordgo.Session {
	t.Helper()

	if capture != nil {
		if capture.permissionPuts == nil {
			capture.permissionPuts = make(map[string]int64)
		}

		if capture.permissionDeletes == nil {
			capture.permissionDeletes = make(map[string]bool)
		}
	}

	return newInteractionTestSessionWithTransport(
		t,
		roundTripFunc(func(request *http.Request) (*http.Response, error) {
			t.Helper()

			path := request.URL.Path
			method := request.Method

			// Channel fetch
			if method == http.MethodGet && path == "/api/v9/channels/channel-123" {
				return newInteractionJSONResponse(request, channelStatusCode, channelBody), nil
			}

			// Permission puts
			if method == http.MethodPut && strings.HasPrefix(path, "/api/v9/channels/channel-123/permissions/") {
				targetID := strings.TrimPrefix(path, "/api/v9/channels/channel-123/permissions/")

				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatalf("read permission put body: %v", err)
				}

				var decoded struct {
					Allow int64 `json:"allow,string"`
					Deny  int64 `json:"deny,string"`
					Type  int   `json:"type"`
				}

				// Discordgo sends allow/deny as strings; handle both
				if err := json.Unmarshal(body, &decoded); err != nil {
					var alt struct {
						Allow int64 `json:"allow"`
						Deny  int64 `json:"deny"`
						Type  int   `json:"type"`
					}

					if err2 := json.Unmarshal(body, &alt); err2 != nil {
						t.Fatalf("decode permission put body: %v (original %v) body %s", err2, err, string(body))
					}

					decoded.Allow = alt.Allow
					decoded.Deny = alt.Deny
					decoded.Type = alt.Type
				}

				if capture != nil {
					if decoded.Deny != 0 {
						capture.permissionPuts[targetID] = decoded.Deny
					} else {
						capture.permissionPuts[targetID] = decoded.Allow
					}
				}

				status := putEveryoneStatus
				if targetID == maintenanceOwnerID || targetID == maintenanceBotID {
					status = putOwnerStatus
				}

				if status == http.StatusNoContent {
					return newNoContentResponse(request), nil
				}

				return newInteractionJSONResponse(request, status, `{"message":"error"}`), nil
			}

			// Permission deletes
			if method == http.MethodDelete && strings.HasPrefix(path, "/api/v9/channels/channel-123/permissions/") {
				targetID := strings.TrimPrefix(path, "/api/v9/channels/channel-123/permissions/")
				if capture != nil {
					capture.permissionDeletes[targetID] = true
				}

				status := deleteEveryoneStatus
				if targetID == maintenanceOwnerID || targetID == maintenanceBotID {
					status = deleteOwnerStatus
				}

				if status == http.StatusNoContent {
					return newNoContentResponse(request), nil
				}

				return newInteractionJSONResponse(request, status, `{"message":"error"}`), nil
			}

			// Interaction callback / edit
			if method == http.MethodPost && strings.HasSuffix(path, "/callback") {
				return captureInteractionCallbackRequest(t, request, response)
			}

			if method == http.MethodPatch && strings.HasSuffix(path, "/messages/@original") {
				if capture == nil {
					t.Fatal("unexpected interaction response edit")
				}

				return captureEditedInteractionRequest(t, request, &capture.editedResponse)
			}

			t.Fatalf("unexpected request: %s %s", method, path)

			return nil, nil
		}),
	)
}

func newMaintenanceCommandInteraction(subcommand, channelID, userID string) *discordgo.InteractionCreate {
	t := discordgo.InteractionCreate{}

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.AppID = "application-id"
	interaction.Token = "interaction-token"
	interaction.ChannelID = "interaction-channel-id"
	interaction.GuildID = "guild-123"
	interaction.Type = discordgo.InteractionApplicationCommand

	if userID != "" {
		interaction.Member = new(discordgo.Member)
		interaction.Member.User = new(discordgo.User)
		interaction.Member.User.ID = userID
	}

	interaction.Data = discordgo.ApplicationCommandInteractionData{
		Name: maintenanceCommandName,
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{
				Name: subcommand,
				Type: discordgo.ApplicationCommandOptionSubCommand,
				Options: func() []*discordgo.ApplicationCommandInteractionDataOption {
					if channelID == "" && subcommand == "start" {
						// Simulate missing option case for test
						return []*discordgo.ApplicationCommandInteractionDataOption{}
					}

					if channelID == "" {
						return []*discordgo.ApplicationCommandInteractionDataOption{}
					}

					return []*discordgo.ApplicationCommandInteractionDataOption{
						{
							Name:  maintenanceChannelIDOptionName,
							Type:  discordgo.ApplicationCommandOptionString,
							Value: channelID,
						},
					}
				}(),
			},
		},
	}

	t.Interaction = interaction

	return &t
}
