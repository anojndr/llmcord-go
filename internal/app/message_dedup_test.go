package app

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestHandleMessageCreateDeduplicatesSameMessageID(t *testing.T) {
	t.Parallel()

	const (
		botUserID = "bot-user"
		channelID = "channel-1"
		userID    = "user-1"
	)

	var responseSends atomic.Int64

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configText := []byte("bot_token: discord-token\n" +
		"providers:\n" +
		"  openai:\n" +
		"    base_url: https://api.example.com/v1\n" +
		"    api_key: test-key\n" +
		"models:\n" +
		"  openai/gpt-test:\n")

	err := os.WriteFile(configPath, configText, 0o600)
	if err != nil {
		t.Fatalf("write test config: %v", err)
	}

	instance := new(bot)
	instance.configPath = configPath
	instance.session = newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/typing":
			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages":
			responseSends.Add(1)

			response := new(discordgo.Message)
			response.ID = "response-message"
			response.ChannelID = channelID

			return newJSONResponse(t, request, response), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/response-message":
			return newJSONResponse(t, request, new(discordgo.Message)), nil
		case request.Method == http.MethodDelete:
			return newNoContentResponse(request), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}))
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		return handle(newStreamDelta("hello", "stop"))
	})

	sourceMessage := newPromptMessage("user-message-1", channelID, userID, botUserID)

	instance.handleMessageCreate(nil, &discordgo.MessageCreate{Message: sourceMessage})
	instance.handleMessageCreate(nil, &discordgo.MessageCreate{Message: sourceMessage})

	if got := responseSends.Load(); got != 1 {
		t.Fatalf("message handled %d times, want 1 (duplicate delivery must be skipped)", got)
	}
}

func TestMarkMessageSeenExpiresAfterWindow(t *testing.T) {
	t.Parallel()

	instance := new(bot)

	if !instance.markMessageSeen("message-1") {
		t.Fatal("expected first sighting to be accepted")
	}

	if instance.markMessageSeen("message-1") {
		t.Fatal("expected duplicate sighting within window to be rejected")
	}

	instance.messageDedupMu.Lock()
	instance.messageProcessedAt["message-1"] = time.Now().Add(-messageSeenWindow - time.Second)
	instance.messageDedupMu.Unlock()

	if !instance.markMessageSeen("message-1") {
		t.Fatal("expected sighting after window expiry to be accepted")
	}
}

func TestMarkMessageSeenAllowsDifferentMessageIDs(t *testing.T) {
	t.Parallel()

	instance := new(bot)

	if !instance.markMessageSeen("message-1") {
		t.Fatal("expected first message to be accepted")
	}

	if !instance.markMessageSeen("message-2") {
		t.Fatal("expected a different message ID to be accepted")
	}
}

func TestExpireMessageSeenRemovesStaleEntries(t *testing.T) {
	t.Parallel()

	instance := new(bot)

	if !instance.markMessageSeen("message-1") {
		t.Fatal("expected first sighting to be accepted")
	}

	instance.messageDedupMu.Lock()
	instance.messageProcessedAt["message-2"] = time.Now().Add(-messageSeenWindow - time.Second)
	instance.messageDedupMu.Unlock()

	instance.messageDedupMu.Lock()
	instance.expireMessageSeen(time.Now())
	instance.messageDedupMu.Unlock()

	instance.messageDedupMu.Lock()
	_, stalePresent := instance.messageProcessedAt["message-2"]
	_, freshPresent := instance.messageProcessedAt["message-1"]
	instance.messageDedupMu.Unlock()

	if stalePresent {
		t.Fatal("expected stale entry to be removed")
	}

	if !freshPresent {
		t.Fatal("expected fresh entry to remain")
	}
}

func TestRespondToMessageLogsMessageReceivedExactlyOnce(t *testing.T) {
	// This test swaps the global slog default via captureLogs, so it must
	// not run parallel with other tests that capture logs. The t.Setenv
	// marks the test as serial for the paralleltest linter, matching the
	// existing log-capture tests in logging_test.go.
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
		"models:\n"+
		"  openai/gpt-test:\n"), 0o600)
	if err != nil {
		t.Fatalf("write test config: %v", err)
	}

	handler := captureLogs(t, func(*captureLogHandler) {
		instance := new(bot)
		instance.configPath = configPath
		instance.session = newAcceptingMessageSession(t, channelID, botUserID)
		instance.nodes = newMessageNodeStore(10)
		instance.chatCompletions = newStubChatClient(func(
			_ context.Context,
			_ chatCompletionRequest,
			handle func(streamDelta) error,
		) error {
			t.Helper()

			return handle(newStreamDelta("hello", "stop"))
		})
		instance.currentModel = "openai/gpt-test"

		sourceMessage := newPromptMessage("user-message-1", channelID, userID, botUserID)

		loadedConfig, loadErr := loadConfig(configPath)
		if loadErr != nil {
			t.Fatalf("load test config: %v", loadErr)
		}

		// An accepted message flows through respondToMessage. The
		// incoming-message log must be emitted exactly once.
		err = instance.respondToMessage(
			context.Background(),
			loadedConfig,
			sourceMessage,
			"openai/gpt-test",
		)
		if err != nil {
			t.Fatalf("respond to message: %v", err)
		}
	})

	records := waitForLogRecords(t, handler, 1)

	if got := countMessageReceivedLogs(records); got != 1 {
		t.Fatalf("message received logged %d times, want 1", got)
	}
}

// countMessageReceivedLogs tallies "message received" records.
func countMessageReceivedLogs(records []capturedLog) int {
	count := 0

	for _, record := range records {
		if record.message == "message received" {
			count++
		}
	}

	return count
}

func TestHandleMessageCreateLogsExactlyOnceThroughWebSearchDecision(t *testing.T) {
	// End-to-end: the full production entry path (handleMessageCreate →
	// respondToMessage → buildMessageConversation → web-search decide →
	// buildSearchDeciderConversation) must log "message received" exactly
	// once for one Discord message.
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

	handler := captureLogs(t, func(*captureLogHandler) {
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

			// The search decider answers with needs_search=false so the
			// pipeline resolves without a second web-search round.
			if request.ConfiguredModel == "openai/decider-model" {
				return handle(newStreamDelta(`{"needs_search":false}`, finishReasonStop))
			}

			return handle(newStreamDelta("hello", finishReasonStop))
		})
		instance.currentModel = "openai/main-model"
		instance.currentSearchDeciderModel = "openai/decider-model"

		sourceMessage := newPromptMessage("user-message-1", channelID, userID, botUserID)

		instance.handleMessageCreate(nil, &discordgo.MessageCreate{Message: sourceMessage})
	})

	records := waitForLogRecords(t, handler, 1)

	if got := countMessageReceivedLogs(records); got != 1 {
		t.Fatalf("message received logged %d times, want 1 (production pipeline must log once per message)", got)
	}
}

// newAcceptingMessageSession builds a Discord session that accepts the
// typing indicator, message sends, and embed edits a response produces.
func newAcceptingMessageSession(t *testing.T, channelID, botUserID string) *discordgo.Session {
	t.Helper()

	return newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/typing":
			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages":
			response := new(discordgo.Message)
			response.ID = "response-message"
			response.ChannelID = channelID

			return newJSONResponse(t, request, response), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/response-message":
			return newJSONResponse(t, request, new(discordgo.Message)), nil
		case request.Method == http.MethodDelete:
			return newNoContentResponse(request), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}))
}
