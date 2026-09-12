package app

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestFixupTikTokContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{
			"https://www.tiktok.com/@user/video/123 check this out",
			"https://www.tnktok.com/@user/video/123 check this out",
		},
		{"https://tiktok.com/@user/video/123", "https://tnktok.com/@user/video/123"},
		{"https://vm.tiktok.com/ZNdNxYZ123/", "https://vm.tnktok.com/ZNdNxYZ123/"},
		{"https://vt.tiktok.com/ZSdAbC123/", "https://vt.tnktok.com/ZSdAbC123/"},
		{"https://m.tiktok.com/@user/video/123", "https://m.tnktok.com/@user/video/123"},
		{"http://tiktok.com/foo", "http://tnktok.com/foo"},
		{"https://WWW.TIKTOK.COM/foo", "https://WWW.tnktok.com/foo"},
		{"https://tnktok.com/foo should not change", "https://tnktok.com/foo should not change"},
		{
			"https://tnktok.com/foo and https://tiktok.com/bar",
			"https://tnktok.com/foo and https://tnktok.com/bar",
		},
		{"tiktok.com", "tnktok.com"},
		{"check vm.tiktok.com/abc123", "check vm.tnktok.com/abc123"},
		{"Visit https://tiktok.com/test!", "Visit https://tnktok.com/test!"},
		{"https://tiktok.com/a?tiktok.com=1", "https://tnktok.com/a?tnktok.com=1"},
		{"https://tiktok.com/foo and https://tiktok.com/bar", "https://tnktok.com/foo and https://tnktok.com/bar"},
		{"atiktok.com should not change", "atiktok.com should not change"},
	}
	for _, tc := range tests {
		got := fixupTikTokContent(tc.in)
		if got != tc.want {
			t.Fatalf("fixup mismatch: in %q got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFixupLinkContentCombinesBoth(t *testing.T) {
	t.Parallel()

	in := "https://x.com/foo and https://www.tiktok.com/@user/video/123"
	want := "https://fixupx.com/foo and https://www.tnktok.com/@user/video/123"

	if got := fixupLinkContent(in); got != want {
		t.Fatalf("combined fixup mismatch: got %q want %q", got, want)
	}
}

func TestShouldHandleTikTokFixup(t *testing.T) {
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
	if !shouldHandleTikTokFixup(makeMessage("https://www.tiktok.com/@user/video/123 check", nil, false), botID) {
		t.Fatal("expected to handle basic tiktok.com")
	}

	if shouldHandleTikTokFixup(makeMessage("https://www.tnktok.com/@user/video/123", nil, false), botID) {
		t.Fatal("should not handle already fixed")
	}

	if shouldHandleTikTokFixup(makeMessage("https://www.tiktok.com/@user/video/123 at ai hello", nil, false), botID) {
		t.Fatal("should not handle when contains at ai")
	}

	if shouldHandleTikTokFixup(
		makeMessage("https://www.tiktok.com/@user/video/123", []*discordgo.User{{ID: botID}}, false),
		botID,
	) {
		t.Fatal("should not handle bot mention via slice")
	}

	if shouldHandleTikTokFixup(
		makeMessage("https://www.tiktok.com/@user/video/123 <@1307756710072549439>", nil, false),
		botID,
	) {
		t.Fatal("should not handle hardcoded mention")
	}

	if shouldHandleTikTokFixup(makeMessage("https://www.tiktok.com/@user/video/123", nil, true), botID) {
		t.Fatal("should not handle bot author")
	}

	if shouldHandleTikTokFixup(makeMessage("hello world", nil, false), botID) {
		t.Fatal("should not handle no tiktok.com")
	}
}

func TestHandleMessageCreateTikTokFixupDeletesAndResends(t *testing.T) {
	t.Parallel()

	var capture xfixupCapture

	deletes, sends, contents, unexpected := driveFixupMessage(
		t,
		&capture,
		"msg-123",
		"SomeUser",
		"https://www.tiktok.com/@user/video/123 check this out",
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
		"https://www.tnktok.com/@user/video/123 check this out"
	if len(contents) != 1 || contents[0] != expectedContent {
		t.Fatalf("unexpected send content: got %q want %q", contents[0], expectedContent)
	}
}

func TestHandleMessageCreateTikTokFixupFixesCombinedLinks(t *testing.T) {
	t.Parallel()

	var capture xfixupCapture

	_, sends, contents, _ := driveFixupMessage(
		t,
		&capture,
		"msg-456",
		"SomeUser",
		"https://x.com/foo https://www.tiktok.com/@user/video/123",
	)
	if len(sends) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sends))
	}

	expectedContent := "SomeUser sent:\n" +
		"https://fixupx.com/foo https://www.tnktok.com/@user/video/123"
	if len(contents) != 1 || contents[0] != expectedContent {
		t.Fatalf("unexpected send content: got %q want %q", contents[0], expectedContent)
	}
}

func TestHandleTikTokFixupNilSessionReturnsFalse(t *testing.T) {
	t.Parallel()

	inst := &bot{}
	msg := &discordgo.Message{
		ID:        "msg-1",
		ChannelID: "channel-1",
		Author:    &discordgo.User{ID: "user-1", Username: "Tester"},
		Content:   "https://www.tiktok.com/@user/video/123",
	}
	if inst.handleTikTokFixup(msg, "bot-id") {
		t.Fatal("expected false with nil session")
	}
}
