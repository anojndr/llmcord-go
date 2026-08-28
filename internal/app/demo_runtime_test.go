package app

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// TestRuntimeLogOutputOncePerMessage drives one Discord message through the
// exact production pipeline — handleMessageCreate → respondToMessage →
// buildMessageConversation → web-search decide → buildSearchDeciderConversation
// — with the production log handler writing to a buffer, then asserts the
// buffer contains exactly one "message received" line. This is runtime-shaped
// evidence: the same slog handler and output format the live bot uses.
func TestRuntimeLogOutputOncePerMessage(t *testing.T) {
	t.Setenv(logLevelEnvironmentVariable, "")

	const (
		botUserID = "bot-user"
		channelID = "channel-1"
		userID    = "user-1"
	)

	configPath := filepath.Join(t.TempDir(), "config.yaml")

	err := os.WriteFile(configPath, []byte("bot_token: discord-token\n"+
		"allow_dms: true\n"+
		"providers:\n"+
		"  openai:\n"+
		"    base_url: https://api.example.com/v1\n"+
		"    api_key: test-key\n"+
		"web_search:\n"+
		"  exa:\n"+
		"    api_key: test-key\n"+
		"models:\n"+
		"  openai/main-model:\n"+
		"  openai/decider-model:\n"), 0o600)
	if err != nil {
		t.Fatalf("write test config: %v", err)
	}

	var output bytes.Buffer

	previousDefault := slog.Default()
	handler := newLogHandler(&output, "", slog.LevelInfo)
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(previousDefault)
	})

	instance := new(bot)
	instance.configPath = configPath
	instance.session = newAcceptingMessageSession(t, channelID, botUserID)
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if request.ConfiguredModel == "openai/decider-model" {
			return handle(newStreamDelta(`{"needs_search":false}`, finishReasonStop))
		}

		return handle(newStreamDelta("hello", finishReasonStop))
	})
	instance.currentModel = "openai/main-model"
	instance.currentSearchDeciderModel = "openai/decider-model"

	sourceMessage := newPromptMessage("user-message-1", channelID, userID, botUserID)

	instance.handleMessageCreate(nil, &discordgo.MessageCreate{Message: sourceMessage})

	logText := output.String()

	receivedCount := strings.Count(logText, `msg="message received"`)
	if receivedCount != 1 {
		t.Fatalf("runtime log contains %d message received lines, want 1; full output:\n%s", receivedCount, logText)
	}

	if !strings.Contains(logText, "message_id=user-message-1") {
		t.Fatalf("runtime log missing message_id attribute:\n%s", logText)
	}

	t.Logf("production log output for one message (exactly %d lines):\n%s", receivedCount, logText)
}
