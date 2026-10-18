package app

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestFixupInstagramContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{
			"https://www.instagram.com/reel/DeFVSFvA_AS/ check this out",
			"https://www.eeinstagram.com/reel/DeFVSFvA_AS/ check this out",
		},
		{"https://instagram.com/p/ABC123/", "https://eeinstagram.com/p/ABC123/"},
		{"http://instagram.com/foo", "http://eeinstagram.com/foo"},
		{"https://WWW.INSTAGRAM.COM/foo", "https://WWW.eeinstagram.com/foo"},
		{"https://eeinstagram.com/foo should not change", "https://eeinstagram.com/foo should not change"},
		{"https://www.eeinstagram.com/reel/DeFVSFvA_AS/ stays", "https://www.eeinstagram.com/reel/DeFVSFvA_AS/ stays"},
		{
			"https://eeinstagram.com/foo and https://instagram.com/bar",
			"https://eeinstagram.com/foo and https://eeinstagram.com/bar",
		},
		{"instagram.com", "eeinstagram.com"},
		{"check www.instagram.com/abc123", "check www.eeinstagram.com/abc123"},
		{"Visit https://instagram.com/test!", "Visit https://eeinstagram.com/test!"},
		{"https://toinstagram.com/foo should not change", "https://toinstagram.com/foo should not change"},
		{"https://ddinstagram.com/foo should not change", "https://ddinstagram.com/foo should not change"},
		{"https://instagram.com/foo and https://instagram.com/bar", "https://eeinstagram.com/foo and https://eeinstagram.com/bar"},
		{"ainstagram.com should not change", "ainstagram.com should not change"},
	}
	for _, tc := range tests {
		got := fixupInstagramContent(tc.in)
		if got != tc.want {
			t.Fatalf("fixup mismatch: in %q got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFixupLinkContentCombinesAll(t *testing.T) {
	t.Parallel()

	in := "https://x.com/u/status/55 https://www.tiktok.com/@user/video/123 https://www.instagram.com/reel/DeFVSFvA_AS/"
	want := "https://fixupx.com/u/status/55/en https://www.tnktok.com/@user/video/123 https://www.eeinstagram.com/reel/DeFVSFvA_AS/"

	if got := fixupLinkContent(in); got != want {
		t.Fatalf("combined fixup mismatch: got %q want %q", got, want)
	}
}

func TestShouldHandleInstagramFixup(t *testing.T) {
	t.Parallel()

	botID := "1307756710072549439"

	makeMessage := func(content string, mentions []*discordgo.User, bot bool) *discordgo.Message {
		return &discordgo.Message{
			Content:   content,
			Author:    &discordgo.User{ID: "user1", Username: "Tester", Bot: bot},
			Mentions:  mentions,
			ChannelID: "channel-1",
			ID:        "msg1",
		}
	}
	if !shouldHandleInstagramFixup(makeMessage("https://www.instagram.com/reel/DeFVSFvA_AS/ check", nil, false), botID) {
		t.Fatal("expected to handle basic instagram.com")
	}

	if shouldHandleInstagramFixup(makeMessage("https://www.eeinstagram.com/reel/DeFVSFvA_AS/", nil, false), botID) {
		t.Fatal("should not handle already fixed")
	}

	if shouldHandleInstagramFixup(makeMessage("https://www.instagram.com/reel/DeFVSFvA_AS/ at ai hello", nil, false), botID) {
		t.Fatal("should not handle when contains at ai")
	}

	if shouldHandleInstagramFixup(
		makeMessage("https://www.instagram.com/reel/DeFVSFvA_AS/", []*discordgo.User{{ID: botID}}, false),
		botID,
	) {
		t.Fatal("should not handle bot mention via slice")
	}

	if shouldHandleInstagramFixup(
		makeMessage("https://www.instagram.com/reel/DeFVSFvA_AS/ <@1307756710072549439>", nil, false),
		botID,
	) {
		t.Fatal("should not handle hardcoded mention")
	}

	if shouldHandleInstagramFixup(makeMessage("https://www.instagram.com/reel/DeFVSFvA_AS/", nil, true), botID) {
		t.Fatal("should not handle bot author")
	}

	if shouldHandleInstagramFixup(makeMessage("hello world", nil, false), botID) {
		t.Fatal("should not handle no instagram.com")
	}
}

func TestHandleMessageCreateInstagramFixupDeletesAndResends(t *testing.T) {
	t.Parallel()

	var capture xfixupCapture

	deletes, sends, contents, unexpected := driveFixupMessage(
		t,
		&capture,
		"msg-123",
		"SomeUser",
		"https://www.instagram.com/reel/DeFVSFvA_AS/ check this out",
	)
	if len(unexpected) != 0 {
		t.Fatalf("unexpected requests: %v", unexpected)
	}

	if len(deletes) != 1 {
		t.Fatalf("expected 1 delete, got %d (%v)", len(deletes), deletes)
	}

	if !strings.Contains(deletes[0], "msg-123") {
		t.Fatalf("delete path should contain message id, got %v", deletes[0])
	}

	if len(sends) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sends))
	}

	expectedContent := "SomeUser sent:\n" +
		"https://www.eeinstagram.com/reel/DeFVSFvA_AS/ check this out"
	if len(contents) != 1 || contents[0] != expectedContent {
		t.Fatalf("unexpected send content: got %q want %q", contents[0], expectedContent)
	}
}

func TestHandleInstagramFixupNilSessionReturnsFalse(t *testing.T) {
	t.Parallel()

	inst := &bot{}
	msg := &discordgo.Message{
		ID:        "msg-1",
		ChannelID: "channel-1",
		Author:    &discordgo.User{ID: "user-1", Username: "Tester"},
		Content:   "https://www.instagram.com/reel/DeFVSFvA_AS/",
	}

	if inst.handleInstagramFixup(msg, "bot-id") {
		t.Fatal("expected false with nil session")
	}
}
