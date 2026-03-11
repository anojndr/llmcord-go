package main

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/bwmarrin/discordgo"
)

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

func TestStartTypingSendsInitialIndicatorBeforeReturning(t *testing.T) {
	t.Parallel()

	const channelID = "channel-1"

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	var typingSent atomic.Bool

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodPost ||
			request.URL.Path != "/api/v9/channels/"+channelID+"/typing" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
		}

		typingSent.Store(true)

		return newNoContentResponse(request), nil
	})
	session.Client = client

	instance := new(bot)
	instance.session = session

	stopTyping := instance.startTyping(t.Context(), channelID)
	defer stopTyping()

	if !typingSent.Load() {
		t.Fatal("expected initial typing indicator before startTyping returned")
	}
}
