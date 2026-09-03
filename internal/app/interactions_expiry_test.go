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
