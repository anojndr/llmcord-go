package main

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

var errTestProgressFail = errors.New("test error")

func TestRequestProgressFailNonBlocking(t *testing.T) {
	t.Parallel()

	session := newDirectMessageTestSession(t, "chan-1", "bot-1", roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		return newNoContentResponse(request), nil
	}))

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)

	progressMsg := new(discordgo.Message)
	progressMsg.ID = "msg-1"
	progressMsg.ChannelID = "chan-1"

	progress := new(requestProgress)
	progress.instance = instance
	progress.tracker = newResponseTracker(progressMsg, "model-1")

	done := make(chan struct{})

	go func() {
		progress.fail(t.Context(), errTestProgressFail)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("progress.fail deadlocked when run goroutine was not active")
	}
}

func TestRequestProgressHandoffDrainsPendingStageUpdates(t *testing.T) {
	t.Parallel()

	const (
		channelID = "channel-1"
		botUserID = "bot-user"
		messageID = "prog-1"
	)

	patchCount := 0
	session := newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodPatch {
			patchCount++

			patchMsg := new(discordgo.Message)
			patchMsg.ID = messageID
			patchMsg.ChannelID = channelID

			return newJSONResponse(t, request, patchMsg), nil
		}

		return newNoContentResponse(request), nil
	}))

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "src-1"
	sourceMessage.ChannelID = channelID

	progressMsg := new(discordgo.Message)
	progressMsg.ID = messageID
	progressMsg.ChannelID = channelID

	progress := new(requestProgress)
	progress.instance = instance
	progress.tracker = newResponseTracker(sourceMessage, "model-1")
	progress.stageUpdates = make(chan requestProgressStage, 1)
	progress.handoffs = make(chan requestProgressHandoff)
	progress.failures = make(chan requestProgressFailure)
	progress.message = progressMsg

	go progress.run(t.Context())

	progress.advance(requestProgressStageGatheringContext)
	progress.advance(requestProgressStageGeneratingResponse)

	tracker := progress.handoff("model-1", nil)
	if tracker == nil {
		t.Fatal("expected non-nil tracker from handoff")
	}
}
