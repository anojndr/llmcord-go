package main

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestBuildMessageTextReadsTextDisplayInsideSection(t *testing.T) {
	t.Parallel()

	message := new(discordgo.Message)

	button := new(discordgo.Button)
	button.CustomID = showSourcesButtonCustomID
	button.Label = showSourcesButtonLabel
	button.Style = discordgo.SecondaryButton

	section := new(discordgo.Section)
	section.Components = []discordgo.MessageComponent{
		discordgo.TextDisplay{Content: "assistant reply"},
	}
	section.Accessory = button

	message.Components = []discordgo.MessageComponent{section}

	text := buildMessageText(message, "", nil)
	if text != "assistant reply" {
		t.Fatalf("unexpected message text: %q", text)
	}
}
