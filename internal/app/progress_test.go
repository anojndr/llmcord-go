package app

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

	patchDescriptions := make([]string, 0, 4)

	session := newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodPatch {
			patchDescriptions = append(
				patchDescriptions,
				requestEmbedDescription(t, request),
			)

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

	progress := &requestProgress{
		instance: instance,
		tracker:  newResponseTracker(sourceMessage, "model-1"),
		stages:   make(chan requestProgressStage, 1),
		handoffs: make(chan requestProgressHandoff),
		failures: make(chan requestProgressFailure),
		message:  progressMsg,
	}

	go progress.run(t.Context())

	progress.advance(requestProgressStageGatheringContext)
	progress.advance(requestProgressStageGeneratingResponse)

	tracker := progress.handoff("model-1", nil)
	if tracker == nil {
		t.Fatal("expected non-nil tracker from handoff")
	}

	if tracker.modelName != "model-1" {
		t.Fatalf("unexpected tracker model: %q", tracker.modelName)
	}

	rendered := false

	for _, description := range patchDescriptions {
		if strings.Contains(description, "**Generating response**") {
			rendered = true
		}
	}

	if !rendered {
		t.Fatalf(
			"expected handoff drain to render the newest stage, got %#v",
			patchDescriptions,
		)
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

	progress := &requestProgress{
		instance: instance,
		tracker:  newResponseTracker(sourceMessage, "model-1"),
		stages:   make(chan requestProgressStage, 1),
		handoffs: make(chan requestProgressHandoff),
		failures: make(chan requestProgressFailure),
		message:  progressMsg,
	}

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

func TestRequestProgressSpinnerFrameCycles(t *testing.T) {
	t.Parallel()

	total := len(requestProgressSpinnerFrames)
	if total == 0 {
		t.Fatal("expected spinner frames to be configured")
	}

	for ticks := range 2 * total {
		want := string(requestProgressSpinnerFrames[ticks%total])

		if got := requestProgressSpinnerFrame(ticks); got != want {
			t.Fatalf("spinner frame %d: got %q, want %q", ticks, got, want)
		}
	}
}
func TestRequestProgressRenderAdvancesSpinnerFrame(t *testing.T) {
	t.Parallel()

	const (
		channelID = "channel-1"
		botUserID = "bot-user"
		messageID = "prog-1"
	)

	patchDescriptions := make([]string, 0, 2)

	session := newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodPatch {
			patchDescriptions = append(
				patchDescriptions,
				requestEmbedDescription(t, request),
			)

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

	progress := &requestProgress{
		instance: instance,
		tracker:  newResponseTracker(sourceMessage, "model-1"),
		stages:   make(chan requestProgressStage, 1),
		handoffs: make(chan requestProgressHandoff),
		failures: make(chan requestProgressFailure),
		message:  progressMsg,
	}

	ctx := t.Context()
	progress.render(ctx, requestProgressStageReadingConversation)
	progress.render(ctx, requestProgressStageReadingConversation)

	if len(patchDescriptions) != 2 {
		t.Fatalf("unexpected patch count: %d", len(patchDescriptions))
	}

	first := string(requestProgressSpinnerFrames[0])
	second := string(requestProgressSpinnerFrames[1])

	if !strings.Contains(patchDescriptions[0], "### "+first) {
		t.Fatalf("expected first render to embed %q: %#v", first, patchDescriptions[0])
	}

	if !strings.Contains(patchDescriptions[1], "### "+second) {
		t.Fatalf("expected second render to advance the frame to %q: %#v", second, patchDescriptions[1])
	}
}

func TestBuildRequestProgressEmbed(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	elapsed := 83 * time.Second

	tests := []struct {
		name            string
		stage           requestProgressStage
		spinnerFrame    string
		wantDescription string
		wantFooter      string
	}{
		{
			name:         "reading conversation",
			stage:        requestProgressStageReadingConversation,
			spinnerFrame: "⠋",
			wantDescription: strings.Join([]string{
				"### ⠋ Reading conversation",
				"",
				"› **Reading conversation** — *Scanning the message, attachments, and reply history*",
				"",
				"○ Gathering context",
				"",
				"○ Generating response",
			}, "\n"),
			wantFooter: "Step 1 of 3 · 1:23",
		},
		{
			name:         "gathering context",
			stage:        requestProgressStageGatheringContext,
			spinnerFrame: "⠙",
			wantDescription: strings.Join([]string{
				"### ⠙ Gathering context",
				"",
				"✓ ~~Reading conversation~~",
				"",
				"› **Gathering context** — *Collecting links, documents, and search results*",
				"",
				"○ Generating response",
			}, "\n"),
			wantFooter: "Step 2 of 3 · 1:23",
		},
		{
			name:         "generating response",
			stage:        requestProgressStageGeneratingResponse,
			spinnerFrame: "⠹",
			wantDescription: strings.Join([]string{
				"### ⠹ Generating response",
				"",
				"✓ ~~Reading conversation~~",
				"",
				"✓ ~~Gathering context~~",
				"",
				"› **Generating response** — *Waiting for the model to respond*",
			}, "\n"),
			wantFooter: "Step 3 of 3 · 1:23",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			embed := buildRequestProgressEmbed(
				test.stage,
				"model-1",
				elapsed,
				startedAt,
				test.spinnerFrame,
			)

			if embed.Description != test.wantDescription {
				t.Fatalf("unexpected embed description:\n%q\nwant:\n%q",
					embed.Description, test.wantDescription)
			}

			if embed.Footer == nil || embed.Footer.Text != test.wantFooter {
				t.Fatalf("unexpected embed footer: %#v", embed.Footer)
			}

			if embed.Timestamp != startedAt.Format(time.RFC3339) {
				t.Fatalf("unexpected embed timestamp: %q", embed.Timestamp)
			}

			if embed.Color != embedColorIncomplete {
				t.Fatalf("unexpected embed color: %#x", embed.Color)
			}

			if embed.Author == nil || embed.Author.Name != "model-1" {
				t.Fatalf("unexpected embed author: %#v", embed.Author)
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
		requestProgressSpinnerFrame(0),
	)

	if embed.Timestamp != "" {
		t.Fatalf("unexpected embed timestamp: %q", embed.Timestamp)
	}
}

func TestFormatRequestProgressStepLine(t *testing.T) {
	t.Parallel()

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
				label: "Reading conversation",
			},
			want: "✓ ~~Reading conversation~~",
		},
		{
			name:         "current with detail",
			lineStage:    requestProgressStageGatheringContext,
			currentStage: requestProgressStageGatheringContext,
			info: requestProgressStageInfo{
				label:  "Gathering context",
				detail: "Collecting links, documents, and search results",
			},
			want: "› **Gathering context** — *Collecting links, documents, and search results*",
		},
		{
			name:         "current without detail",
			lineStage:    requestProgressStageGatheringContext,
			currentStage: requestProgressStageGatheringContext,
			info: requestProgressStageInfo{
				label: "Gathering context",
			},
			want: "› **Gathering context**",
		},
		{
			name:         "pending",
			lineStage:    requestProgressStageGeneratingResponse,
			currentStage: requestProgressStageGatheringContext,
			info: requestProgressStageInfo{
				label: "Generating response",
			},
			want: "○ Generating response",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := formatRequestProgressStepLine(test.lineStage, test.currentStage, test.info)
			if got != test.want {
				t.Fatalf("unexpected step line:\n%q\nwant:\n%q", got, test.want)
			}
		})
	}
}

func TestFormatRequestProgressFooterClampsStageBounds(t *testing.T) {
	t.Parallel()

	if got := formatRequestProgressFooter(requestProgressStage(-1), 0); got != "Step 1 of 3 · 0:00" {
		t.Fatalf("unexpected clamped low footer: %q", got)
	}

	outOfRange := requestProgressStage(len(requestProgressStageTable) + 3)
	if got := formatRequestProgressFooter(outOfRange, 0); got != "Step 3 of 3 · 0:00" {
		t.Fatalf("unexpected clamped high footer: %q", got)
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
