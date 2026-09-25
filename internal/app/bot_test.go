package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestSyncCommandsRegistersChannelCommand(t *testing.T) {
	t.Parallel()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	user := new(discordgo.User)
	user.ID = "application-id"
	session.State.User = user

	var registeredCommands []struct {
		Name string `json:"name"`
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodPut {
			t.Fatalf("unexpected method: %s", request.Method)
		}

		expectedPath := "/api/v9/applications/application-id/commands"
		if request.URL.Path != expectedPath {
			t.Fatalf("unexpected request path: got %q want %q", request.URL.Path, expectedPath)
		}

		responseBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}

		err = json.Unmarshal(responseBody, &registeredCommands)
		if err != nil {
			t.Fatalf("decode registered commands: %v", err)
		}

		return newInteractionJSONResponse(request, http.StatusOK, `[]`), nil
	})
	session.Client = client

	instance := new(bot)
	instance.session = session

	err = instance.syncCommands()
	if err != nil {
		t.Fatalf("sync commands: %v", err)
	}

	for _, expectedName := range []string{createChannelCommandName, editChannelNameCommandName, moveChannelCommandName} {
		found := false

		for _, command := range registeredCommands {
			if command.Name == expectedName {
				found = true

				break
			}
		}

		if !found {
			t.Fatalf("expected %q among registered commands, got %+v", expectedName, registeredCommands)
		}
	}
}

func TestCurrentModelForChannelIDsUsesLockedModelWithoutChangingGlobalModel(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.currentModel = firstTestModel

	var loadedConfig config

	loadedConfig.Models = map[string]map[string]any{
		firstTestModel:  nil,
		secondTestModel: nil,
	}
	loadedConfig.ModelOrder = []string{firstTestModel, secondTestModel}
	loadedConfig.ChannelModelLocks = map[string]string{"locked-channel": secondTestModel}

	currentModel := instance.currentModelForChannelIDs(loadedConfig, []string{"locked-channel"})
	if currentModel != secondTestModel {
		t.Fatalf("unexpected current model: %q", currentModel)
	}

	if instance.currentModel != firstTestModel {
		t.Fatalf("unexpected global current model: %q", instance.currentModel)
	}
}

func TestCurrentModelForChannelIDsUsesFirstMatchingLock(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.currentModel = firstTestModel

	var loadedConfig config

	loadedConfig.Models = map[string]map[string]any{
		firstTestModel:  nil,
		secondTestModel: nil,
	}
	loadedConfig.ModelOrder = []string{firstTestModel, secondTestModel}
	loadedConfig.ChannelModelLocks = map[string]string{
		"thread-channel": secondTestModel,
		"parent-channel": firstTestModel,
	}

	currentModel := instance.currentModelForChannelIDs(
		loadedConfig,
		[]string{"thread-channel", "parent-channel"},
	)
	if currentModel != secondTestModel {
		t.Fatalf("unexpected current model: %q", currentModel)
	}
}

// newTypingTestBot returns a bot whose Discord session answers typing
// indicators for channelID, holding each one until release is closed.
func newTypingTestBot(t *testing.T, channelID string, release <-chan struct{}, typingSent *atomic.Int64) *bot {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodPost ||
			request.URL.Path != "/api/v9/channels/"+channelID+"/typing" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}

		<-release
		typingSent.Add(1)

		return newNoContentResponse(request), nil
	})
	session.Client = client

	instance := new(bot)
	instance.session = session

	return instance
}

func TestStartTypingSendsInitialIndicatorInBackground(t *testing.T) {
	t.Parallel()

	const channelID = "channel-1"

	release := make(chan struct{})

	var typingSent atomic.Int64

	instance := newTypingTestBot(t, channelID, release, &typingSent)

	returned := make(chan func(), 1)

	go func() {
		returned <- instance.startTyping(t.Context(), channelID)
	}()

	var stopTyping func()

	select {
	case stopTyping = <-returned:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("startTyping waited for Discord to accept the typing indicator")
	}

	close(release)
	stopTyping()

	if typingSent.Load() != 1 {
		t.Fatalf("expected stop to wait for the initial typing indicator, sent %d", typingSent.Load())
	}
}

func TestStartTypingAfterWaitsForReady(t *testing.T) {
	t.Parallel()

	const channelID = "channel-1"

	release := make(chan struct{})
	close(release)

	var typingSent atomic.Int64

	instance := newTypingTestBot(t, channelID, release, &typingSent)
	ready := make(chan struct{})

	stopTyping := instance.startTypingAfter(t.Context(), channelID, ready)

	time.Sleep(50 * time.Millisecond)

	if got := typingSent.Load(); got != 0 {
		t.Fatalf("expected no typing indicator before ready, sent %d", got)
	}

	close(ready)
	stopTyping()

	if got := typingSent.Load(); got != 1 {
		t.Fatalf("expected the typing indicator once ready closed, sent %d", got)
	}
}

func TestReadyAnnouncementPrintsOnceAfterReadyAndConfiguration(t *testing.T) {
	t.Parallel()

	buffer := new(bytes.Buffer)
	instance := new(bot)
	instance.onlineOutput = buffer

	instance.handleReady(nil, nil)

	if buffer.Len() != 0 {
		t.Fatalf("expected no announcement before configuration, got %q", buffer.String())
	}

	instance.markSessionConfigured()

	if buffer.String() != readyMessage+"\n" {
		t.Fatalf("unexpected announcement after configuration: %q", buffer.String())
	}

	instance.handleReady(nil, nil)
	instance.markSessionConfigured()

	if buffer.String() != readyMessage+"\n" {
		t.Fatalf("expected announcement only once, got %q", buffer.String())
	}
}

func TestReadyAnnouncementWaitsForDiscordReadyWhenConfiguredFirst(t *testing.T) {
	t.Parallel()

	buffer := new(bytes.Buffer)
	instance := new(bot)
	instance.onlineOutput = buffer

	instance.markSessionConfigured()

	if buffer.Len() != 0 {
		t.Fatalf("expected no announcement before ready event, got %q", buffer.String())
	}

	instance.handleReady(nil, nil)

	if buffer.String() != readyMessage+"\n" {
		t.Fatalf("unexpected announcement after ready event: %q", buffer.String())
	}
}

func TestResumedAnnouncementPrintsAfterConfiguration(t *testing.T) {
	t.Parallel()

	buffer := new(bytes.Buffer)
	instance := new(bot)
	instance.onlineOutput = buffer

	instance.markSessionConfigured()

	if buffer.Len() != 0 {
		t.Fatalf("expected no announcement before resumed event, got %q", buffer.String())
	}

	instance.handleResumed(nil, nil)

	if buffer.String() != readyMessage+"\n" {
		t.Fatalf("unexpected announcement after resumed event: %q", buffer.String())
	}

	instance.handleResumed(nil, nil)

	if buffer.String() != readyMessage+"\n" {
		t.Fatalf("expected announcement only once, got %q", buffer.String())
	}
}

func TestReserveEditDelayUsesSeparateMessageBuckets(t *testing.T) {
	t.Parallel()

	instance := new(bot)

	firstMessageWait := instance.reserveEditDelay("message-1")
	secondMessageWait := instance.reserveEditDelay("message-2")
	repeatedFirstMessageWait := instance.reserveEditDelay("message-1")

	if firstMessageWait != 0 {
		t.Fatalf("unexpected initial wait for first message: %s", firstMessageWait)
	}

	if secondMessageWait != 0 {
		t.Fatalf("unexpected initial wait for second message: %s", secondMessageWait)
	}

	if repeatedFirstMessageWait <= 0 {
		t.Fatalf("expected repeated first-message wait to be throttled, got %s", repeatedFirstMessageWait)
	}
}
