package main

import (
	"errors"
	"net/http"
	"strings"
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

func TestRequestProgressRunPeriodicallyRefreshes(t *testing.T) {
	t.Parallel()

	const (
		channelID = "channel-1"
		botUserID = "bot-user"
		messageID = "prog-1"
	)

	patches := make(chan struct{}, 16)

	session := newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodPatch {
			patches <- struct{}{}

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

	select {
	case <-patches:
	case <-time.After(3 * requestProgressRefreshInterval):
		t.Fatal("expected periodic progress refresh without stage changes")
	}

	tracker := progress.handoff("model-1", nil)
	if tracker == nil {
		t.Fatal("expected non-nil tracker from handoff")
	}

	select {
	case <-patches:
		t.Fatal("unexpected progress refresh after handoff")
	case <-time.After(requestProgressRefreshInterval / 2):
	}
}

func TestBuildRequestProgressEmbed(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	elapsed := 83 * time.Second

	tests := []struct {
		name            string
		stage           requestProgressStage
		wantDescription string
	}{
		{
			name:  "reading conversation",
			stage: requestProgressStageReadingConversation,
			wantDescription: strings.Join([]string{
				"▰▱▱▱▱▱▱▱▱▱ **10%** · ⏱ **1:23**",
				"",
				"⏳ Reading conversation — Scanning the message, attachments, and reply history",
				"⬜ Gathering context",
				"⬜ Generating response",
			}, "\n"),
		},
		{
			name:  "gathering context",
			stage: requestProgressStageGatheringContext,
			wantDescription: strings.Join([]string{
				"▰▰▰▰▰▱▱▱▱▱ **45%** · ⏱ **1:23**",
				"",
				"✅ Reading conversation",
				"⏳ Gathering context — Collecting links, documents, and search results",
				"⬜ Generating response",
			}, "\n"),
		},
		{
			name:  "generating response",
			stage: requestProgressStageGeneratingResponse,
			wantDescription: strings.Join([]string{
				"▰▰▰▰▰▰▰▰▱▱ **80%** · ⏱ **1:23**",
				"",
				"✅ Reading conversation",
				"✅ Gathering context",
				"⏳ Generating response — Waiting for the model to respond",
			}, "\n"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			embed := buildRequestProgressEmbed(test.stage, "model-1", elapsed, startedAt)

			if embed.Title != requestProgressTitleText {
				t.Fatalf("unexpected embed title: %q", embed.Title)
			}

			if embed.Description != test.wantDescription {
				t.Fatalf("unexpected embed description:\n%q\nwant:\n%q", embed.Description, test.wantDescription)
			}

			if embed.Timestamp != startedAt.Format(time.RFC3339) {
				t.Fatalf("unexpected embed timestamp: %q", embed.Timestamp)
			}
		})
	}
}

func TestBuildRequestProgressEmbedOmitsTimestampWhenStartIsZero(t *testing.T) {
	t.Parallel()

	embed := buildRequestProgressEmbed(
		requestProgressStageReadingConversation,
		"model-1",
		0,
		time.Time{},
	)

	if embed.Timestamp != "" {
		t.Fatalf("unexpected embed timestamp: %q", embed.Timestamp)
	}
}

func TestFormatRequestProgressLine(t *testing.T) {
	t.Parallel()

	info := requestProgressStageInfo{
		label:   "Gathering context",
		detail:  "Collecting links, documents, and search results",
		percent: 45,
	}

	tests := []struct {
		name         string
		lineStage    requestProgressStage
		currentStage requestProgressStage
		info         requestProgressStageInfo
		want         string
	}{
		{
			name:         "completed",
			lineStage:    requestProgressStageReadingConversation,
			currentStage: requestProgressStageGatheringContext,
			info: requestProgressStageInfo{
				label:   "Reading conversation",
				detail:  "",
				percent: 10,
			},
			want: "✅ Reading conversation",
		},
		{
			name:         "current with detail",
			lineStage:    requestProgressStageGatheringContext,
			currentStage: requestProgressStageGatheringContext,
			info:         info,
			want:         "⏳ Gathering context — Collecting links, documents, and search results",
		},
		{
			name:         "current without detail",
			lineStage:    requestProgressStageGatheringContext,
			currentStage: requestProgressStageGatheringContext,
			info: requestProgressStageInfo{
				label:   "Gathering context",
				detail:  "",
				percent: 45,
			},
			want: "⏳ Gathering context",
		},
		{
			name:         "pending",
			lineStage:    requestProgressStageGeneratingResponse,
			currentStage: requestProgressStageGatheringContext,
			info: requestProgressStageInfo{
				label:   "Generating response",
				detail:  "",
				percent: 80,
			},
			want: "⬜ Generating response",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := formatRequestProgressLine(test.lineStage, test.currentStage, test.info)
			if got != test.want {
				t.Fatalf("unexpected progress line:\n%q\nwant:\n%q", got, test.want)
			}
		})
	}
}

func TestBuildRequestProgressBar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		percent int
		want    string
	}{
		{name: "zero", percent: 0, want: "▱▱▱▱▱▱▱▱▱▱"},
		{name: "negative", percent: -10, want: "▱▱▱▱▱▱▱▱▱▱"},
		{name: "ten percent", percent: 10, want: "▰▱▱▱▱▱▱▱▱▱"},
		{name: "forty five percent", percent: 45, want: "▰▰▰▰▰▱▱▱▱▱"},
		{name: "eighty percent", percent: 80, want: "▰▰▰▰▰▰▰▰▱▱"},
		{name: "ninety nine percent", percent: 99, want: "▰▰▰▰▰▰▰▰▰▰"},
		{name: "one hundred percent", percent: 100, want: "▰▰▰▰▰▰▰▰▰▰"},
		{name: "over one hundred percent", percent: 150, want: "▰▰▰▰▰▰▰▰▰▰"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := buildRequestProgressBar(test.percent)
			if got != test.want {
				t.Fatalf("unexpected progress bar: %q, want %q", got, test.want)
			}
		})
	}
}

func TestFormatRequestProgressElapsed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		elapsed time.Duration
		want    string
	}{
		{name: "zero", elapsed: 0, want: "0:00"},
		{name: "negative", elapsed: -time.Second, want: "0:00"},
		{name: "seconds", elapsed: 7 * time.Second, want: "0:07"},
		{name: "minute", elapsed: time.Minute + 15*time.Second, want: "1:15"},
		{name: "hour", elapsed: time.Hour, want: "1:00:00"},
		{name: "many hours", elapsed: 25*time.Hour + time.Minute + 2*time.Second, want: "25:01:02"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := formatRequestProgressElapsed(test.elapsed)
			if got != test.want {
				t.Fatalf("unexpected elapsed: %q, want %q", got, test.want)
			}
		})
	}
}

func TestRequestProgressElapsed(t *testing.T) {
	t.Parallel()

	progress := new(requestProgress)

	if elapsed := progress.elapsed(); elapsed != 0 {
		t.Fatalf("unexpected elapsed with zero startedAt: %v", elapsed)
	}

	progress.startedAt = time.Now().Add(-5 * time.Second)

	elapsed := progress.elapsed()
	if elapsed < 5*time.Second || elapsed > 10*time.Second {
		t.Fatalf("unexpected elapsed: %v", elapsed)
	}

	progress.startedAt = time.Now().Add(time.Hour)

	if elapsed := progress.elapsed(); elapsed != 0 {
		t.Fatalf("unexpected elapsed for future startedAt: %v", elapsed)
	}
}

func TestBuildRequestProgressFailureEmbed(t *testing.T) {
	t.Parallel()

	embed := buildRequestProgressFailureEmbed("model-1", "  Something went wrong.  ")

	if embed.Title != requestProgressFailureTitle {
		t.Fatalf("unexpected failure embed title: %q", embed.Title)
	}

	if embed.Description != "Something went wrong." {
		t.Fatalf("unexpected failure embed description: %q", embed.Description)
	}

	if embed.Color != embedColorFailure {
		t.Fatalf("unexpected failure embed color: %#x", embed.Color)
	}
}
