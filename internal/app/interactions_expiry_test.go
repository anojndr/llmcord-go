package app

import (
	"path/filepath"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestHandleGroundingCommandDefersBeforeResponding(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfig(t)

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)
	instance := newModelTestBot(configPath)
	interaction := newGroundingCommandInteraction("member-user")

	if err := instance.handleGroundingCommand(session, interaction); err != nil {
		t.Fatalf("handle grounding command: %v", err)
	}

	assertDeferredInteractionResponse(t, &capture.deferredResponse)

	expectedContent := "Grounding is only supported for Gemini models."
	if capture.editedResponse.Content != expectedContent {
		t.Fatalf("unexpected edited response content: got %q want %q", capture.editedResponse.Content, expectedContent)
	}
}

func TestHandleModelCommandEditsFailureAfterDeferWhenConfigMissing(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "missing.yaml")

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)
	instance := newModelTestBot(configPath)
	interaction := newModelCommandInteraction("member-user", secondTestModel)

	if err := instance.handleModelCommand(session, interaction); err != nil {
		t.Fatalf("handle model command: %v", err)
	}

	assertDeferredInteractionResponse(t, &capture.deferredResponse)

	if capture.requestCount != 2 {
		t.Fatalf("unexpected interaction request count: got %d want 2", capture.requestCount)
	}

	expectedContent := "Failed to load configuration."
	if capture.editedResponse.Content != expectedContent {
		t.Fatalf("unexpected edited response content: got %q want %q", capture.editedResponse.Content, expectedContent)
	}
}

func newGroundingCommandInteraction(userID string) *discordgo.InteractionCreate {
	user := new(discordgo.User)
	user.ID = userID

	member := new(discordgo.Member)
	member.User = user

	var commandData discordgo.ApplicationCommandInteractionData

	commandData.Name = groundingCommandName

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

func TestExpiredInteractionDetailNamesSlashCommandOrComponent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		interaction *discordgo.InteractionCreate
		want        []any
	}{
		{
			name: "slash command",
			interaction: newInteractionCreateWithData(
				discordgo.InteractionApplicationCommand,
				discordgo.ApplicationCommandInteractionData{Name: modelCommandName},
			),
			want: []any{"command", modelCommandName},
		},
		{
			name: "autocomplete",
			interaction: newInteractionCreateWithData(
				discordgo.InteractionApplicationCommandAutocomplete,
				discordgo.ApplicationCommandInteractionData{Name: searchTypeCommandName},
			),
			want: []any{"command", searchTypeCommandName},
		},
		{
			name: "message component",
			interaction: newInteractionCreateWithData(
				discordgo.InteractionMessageComponent,
				discordgo.MessageComponentInteractionData{CustomID: showSourcesButtonCustomID},
			),
			want: []any{"custom_id", showSourcesButtonCustomID},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assertExpiredInteractionDetail(t, test.interaction, test.want)
		})
	}
}

func TestExpiredInteractionDetailReturnsNilWithoutIdentifiableTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		interaction *discordgo.InteractionCreate
		want        []any
	}{
		{
			name:        "nil interaction",
			interaction: nil,
			want:        nil,
		},
		{
			name: "missing data",
			interaction: func() *discordgo.InteractionCreate {
				interaction := new(discordgo.Interaction)
				interaction.Type = discordgo.InteractionApplicationCommand

				result := new(discordgo.InteractionCreate)
				result.Interaction = interaction

				return result
			}(),
			want: nil,
		},
		{
			name: "unrelated type",
			interaction: newInteractionCreateWithData(
				discordgo.InteractionPing,
				discordgo.ApplicationCommandInteractionData{Name: modelCommandName},
			),
			want: nil,
		},
		{
			name: "mismatched data type",
			interaction: newInteractionCreateWithData(
				discordgo.InteractionApplicationCommand,
				discordgo.MessageComponentInteractionData{CustomID: showSourcesButtonCustomID},
			),
			want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assertExpiredInteractionDetail(t, test.interaction, test.want)
		})
	}
}

func newInteractionCreateWithData(
	interactionType discordgo.InteractionType,
	data discordgo.InteractionData,
) *discordgo.InteractionCreate {
	interaction := new(discordgo.Interaction)
	interaction.Type = interactionType
	interaction.Data = data

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}

func assertExpiredInteractionDetail(
	t *testing.T,
	interaction *discordgo.InteractionCreate,
	want []any,
) {
	t.Helper()

	got := expiredInteractionDetail(interaction)

	if len(got) != len(want) {
		t.Fatalf("unexpected detail: got %v want %v", got, want)
	}

	for index := range got {
		if got[index] != want[index] {
			t.Fatalf("unexpected detail: got %v want %v", got, want)
		}
	}
}
