package main

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestSegmentAccumulatorSplitsByRunes(t *testing.T) {
	t.Parallel()

	accumulator := newSegmentAccumulator(4)
	splitOccurred := accumulator.appendText("abecd")

	if !splitOccurred {
		t.Fatal("expected content to split across segments")
	}

	segments := accumulator.renderSegments(true)
	if len(segments) != 2 {
		t.Fatalf("unexpected segment count: %#v", segments)
	}

	if segments[0] != "abec" || segments[1] != "d" {
		t.Fatalf("unexpected segments: %#v", segments)
	}
}

func TestBuildRenderSpecsMarksSettledAndStreamingSegments(t *testing.T) {
	t.Parallel()

	specs := buildRenderSpecs([]string{"first", "second"}, "length", false, false)
	if len(specs) != 2 {
		t.Fatalf("unexpected spec count: %#v", specs)
	}

	if specs[0].content != "first" || specs[0].color != embedColorComplete {
		t.Fatalf("unexpected first spec: %#v", specs[0])
	}

	if specs[1].content != "second"+streamingIndicator ||
		specs[1].color != embedColorIncomplete {
		t.Fatalf("unexpected second spec: %#v", specs[1])
	}

	finalSpecs := buildRenderSpecs([]string{"only"}, "stop", true, false)
	if len(finalSpecs) != 1 {
		t.Fatalf("unexpected final spec count: %#v", finalSpecs)
	}

	if finalSpecs[0].content != "only" || finalSpecs[0].color != embedColorComplete {
		t.Fatalf("unexpected final spec: %#v", finalSpecs[0])
	}
}

func TestBuildRenderSpecsAddsSourcesButtonOnlyToFinalSearchedSegment(t *testing.T) {
	t.Parallel()

	specs := buildRenderSpecs([]string{"first", "second"}, "stop", true, true)
	if len(specs) != 2 {
		t.Fatalf("unexpected spec count: %#v", specs)
	}

	if specs[0].showSourcesButton {
		t.Fatalf("expected no sources button on first segment: %#v", specs[0])
	}

	if !specs[1].showSourcesButton {
		t.Fatalf("expected sources button on final segment: %#v", specs[1])
	}
}

func TestNewReplyMessageDisablesReplyAuthorMention(t *testing.T) {
	t.Parallel()

	reference := new(discordgo.Message)
	reference.ID = "source-message"
	reference.ChannelID = "source-channel"

	send := newReplyMessage(reference)
	if send.AllowedMentions == nil {
		t.Fatal("expected allowed mentions to be configured")
	}

	if send.AllowedMentions.RepliedUser {
		t.Fatal("expected replied user mentions to be disabled")
	}

	expectedParse := []discordgo.AllowedMentionType{
		discordgo.AllowedMentionTypeRoles,
		discordgo.AllowedMentionTypeUsers,
		discordgo.AllowedMentionTypeEveryone,
	}

	if len(send.AllowedMentions.Parse) != len(expectedParse) {
		t.Fatalf("unexpected allowed mention parse count: %#v", send.AllowedMentions.Parse)
	}

	for index, mentionType := range expectedParse {
		if send.AllowedMentions.Parse[index] != mentionType {
			t.Fatalf(
				"unexpected allowed mention parse at %d: got %q want %q",
				index,
				send.AllowedMentions.Parse[index],
				mentionType,
			)
		}
	}

	if send.Reference == nil {
		t.Fatal("expected message reference to be set")
	}
}

func TestBuildPlainComponentsAddsShowSourcesButton(t *testing.T) {
	t.Parallel()

	components := buildPlainComponents("hello", true)
	if len(components) != 1 {
		t.Fatalf("unexpected component count: %#v", components)
	}

	section, sectionOK := components[0].(*discordgo.Section)
	if !sectionOK {
		t.Fatalf("expected section component, got %T", components[0])
	}

	button, buttonOK := section.Accessory.(*discordgo.Button)
	if !buttonOK {
		t.Fatalf("expected button accessory, got %T", section.Accessory)
	}

	if button.CustomID != showSourcesButtonCustomID {
		t.Fatalf("unexpected button custom id: %q", button.CustomID)
	}
}
