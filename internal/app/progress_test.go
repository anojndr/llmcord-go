package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

const (
	testProgressChannelID = "channel-1"
	testProgressBotUserID = "bot-user"
	testProgressMessageID = "prog-1"
)

// newTestRequestProgress returns a progress card whose card message is
// already posted, with its refresh loop running.
func newTestRequestProgress(t *testing.T, instance *bot) *requestProgress {
	t.Helper()

	progress := newTestRequestProgressStopped(instance)

	go progress.run(t.Context())

	return progress
}

// newTestRequestProgressStopped returns a progress card whose card message
// is already posted, without starting its refresh loop.
func newTestRequestProgressStopped(instance *bot) *requestProgress {
	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "src-1"
	sourceMessage.ChannelID = testProgressChannelID

	return &requestProgress{
		instance:      instance,
		tracker:       newResponseTracker(sourceMessage, "model-1"),
		sourceMessage: sourceMessage,
		modelName:     "model-1",
		startedAt:     time.Now(),
		stages:        make(chan requestProgressStage, 1),
		searches:      make(chan requestProgressSearch, 1),
		posted:        make(chan struct{}),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		stopOnce:      sync.Once{},
		settled:       false,
		message:       newTestProgressCardMessage(),
		pending:       pendingResponse{messageID: "", node: nil},
		ticks:         0,
		nextEditAt:    time.Time{},
	}
}

func newTestProgressCardMessage() *discordgo.Message {
	cardMessage := new(discordgo.Message)
	cardMessage.ID = testProgressMessageID
	cardMessage.ChannelID = testProgressChannelID

	return cardMessage
}

// newProgressTestBot returns a bot whose Discord session answers card edits
// through onPatch and everything else with 204 No Content.
func newProgressTestBot(t *testing.T, onPatch func(*http.Request)) *bot {
	t.Helper()

	session := newDirectMessageTestSession(t, testProgressChannelID, testProgressBotUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodPatch {
			onPatch(request)

			return newJSONResponse(t, request, newTestProgressCardMessage()), nil
		}

		return newNoContentResponse(request), nil
	}))

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)

	return instance
}

func TestRequestProgressHandoffDoesNotWaitForCardEdit(t *testing.T) {
	t.Parallel()

	editStarted := make(chan struct{})
	releaseEdit := make(chan struct{})

	var (
		editStartedOnce   sync.Once
		patchDescriptions []string
	)

	instance := newProgressTestBot(t, func(request *http.Request) {
		description := requestEmbedDescription(t, request)

		editStartedOnce.Do(func() { close(editStarted) })
		<-releaseEdit

		patchDescriptions = append(patchDescriptions, description)
	})

	progress := newTestRequestProgress(t, instance)
	progress.advance(requestProgressStageGeneratingResponse)

	select {
	case <-editStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the card loop to start editing the card")
	}

	handedOff := make(chan *responseTracker, 1)

	go func() {
		handedOff <- progress.handoff("model-1", nil)
	}()

	var tracker *responseTracker

	select {
	case tracker = <-handedOff:
	case <-time.After(2 * time.Second):
		close(releaseEdit)
		t.Fatal("handoff waited for the card edit")
	}

	if tracker == nil || tracker.modelName != "model-1" || tracker.progress != progress {
		t.Fatalf("unexpected handed-off tracker: %#v", tracker)
	}

	close(releaseEdit)
	tracker.settleProgress()

	if len(tracker.responseMessages) != 1 || tracker.responseMessages[0].ID != testProgressMessageID ||
		len(tracker.pendingResponses) != 1 || !tracker.progressActive {
		t.Fatalf("expected settle to hand the card message to the tracker: %#v", tracker.responseMessages)
	}

	if len(patchDescriptions) == 0 || !strings.Contains(patchDescriptions[0], "**Generating response**") {
		t.Fatalf("expected the card to render the newest stage in the background, got %#v", patchDescriptions)
	}
}

func TestRequestProgressSettleWaitsForCardEditInFlight(t *testing.T) {
	t.Parallel()

	editStarted := make(chan struct{})
	releaseEdit := make(chan struct{})

	var (
		editStartedOnce sync.Once
		editFinished    atomic.Bool
	)

	instance := newProgressTestBot(t, func(*http.Request) {
		editStartedOnce.Do(func() { close(editStarted) })
		<-releaseEdit
		editFinished.Store(true)
	})

	progress := newTestRequestProgress(t, instance)
	progress.advance(requestProgressStageGatheringContext)

	select {
	case <-editStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the card loop to start editing the card")
	}

	settled := make(chan struct{})

	go func() {
		progress.settle()
		close(settled)
	}()

	select {
	case <-settled:
		close(releaseEdit)
		t.Fatal("settle returned while a card edit was still being sent")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseEdit)

	select {
	case <-settled:
	case <-time.After(2 * time.Second):
		t.Fatal("settle did not return after the card edit finished")
	}

	if !editFinished.Load() {
		t.Fatal("expected the card edit to finish before settle returned")
	}
}

func TestRequestProgressSettleDropsCardEditWaitingForItsSlot(t *testing.T) {
	t.Parallel()

	var patches atomic.Int64

	instance := newProgressTestBot(t, func(*http.Request) {
		patches.Add(1)
	})

	progress := newTestRequestProgressStopped(instance)
	// The previous card edit was just sent, so the next one has to wait.
	progress.nextEditAt = time.Now().Add(time.Hour)

	go progress.run(t.Context())

	progress.advance(requestProgressStageGeneratingResponse)

	settled := make(chan struct{})

	go func() {
		progress.settle()
		close(settled)
	}()

	select {
	case <-settled:
	case <-time.After(2 * time.Second):
		t.Fatal("settle waited for a card edit that had not been sent")
	}

	if got := patches.Load(); got != 0 {
		t.Fatalf("expected the waiting card edit to be dropped, got %d edits", got)
	}
}

func TestRequestProgressRunRefreshesPeriodicallyUntilGenerating(t *testing.T) {
	t.Parallel()

	patches := make(chan string, 16)

	instance := newProgressTestBot(t, func(request *http.Request) {
		patches <- requestEmbedDescription(t, request)
	})

	progress := newTestRequestProgress(t, instance)
	defer progress.settle()

	select {
	case <-patches:
	case <-time.After(3 * requestProgressRefreshInterval):
		t.Fatal("expected periodic progress refresh without stage changes")
	}

	progress.advance(requestProgressStageGeneratingResponse)

	deadline := time.After(3 * requestProgressRefreshInterval)

	for generating := false; !generating; {
		select {
		case description := <-patches:
			generating = strings.Contains(description, "**Generating response**")
		case <-deadline:
			t.Fatal("expected the card to render the generating stage")
		}
	}

	// Refresh ticks would only make the reply's first edit wait for its slot.
	select {
	case description := <-patches:
		t.Fatalf("unexpected periodic refresh while generating: %q", description)
	case <-time.After(requestProgressRefreshInterval + editDelay):
	}
}

func TestRequestProgressSettleHandsCardEditPacingToReply(t *testing.T) {
	t.Parallel()

	edited := make(chan struct{}, 4)

	instance := newProgressTestBot(t, func(*http.Request) {
		edited <- struct{}{}
	})

	progress := newTestRequestProgress(t, instance)
	progress.advance(requestProgressStageGatheringContext)

	select {
	case <-edited:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the card loop to edit the card")
	}

	progress.settle()

	if wait := instance.reserveEditDelay(testProgressMessageID); wait <= 0 || wait > editDelay {
		t.Fatalf("expected the reply's first edit to wait out the card edit's slot, got %s", wait)
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
		instance:  instance,
		tracker:   newResponseTracker(sourceMessage, "model-1"),
		modelName: "model-1",
		stages:    make(chan requestProgressStage, 1),
		message:   progressMsg,
	}

	ctx := t.Context()
	view := newRequestProgressView(requestProgressStageReadingConversation)
	progress.render(ctx, &view)
	// Skip the card's own edit pacing between the two renders.
	progress.nextEditAt = time.Time{}
	progress.render(ctx, &view)

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
				newRequestProgressView(test.stage),
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
		newRequestProgressView(requestProgressStageReadingConversation),
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

// newCardBlockingRespondSession answers the progress card POST with
// cardMessage and holds every card edit until cardEditsMayFinish closes (or
// a safety timeout passes); answer edits pass straight through.
func newCardBlockingRespondSession(
	t *testing.T,
	cardMessage *discordgo.Message,
	cardEditsMayFinish <-chan struct{},
	cardEditsDone *atomic.Int64,
	answerEdits chan<- string,
) *discordgo.Session {
	t.Helper()

	return newDirectMessageTestSession(t, testProgressChannelID, testProgressBotUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/typing"):
			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/messages"):
			return newJSONResponse(t, request, cardMessage), nil
		case request.Method == http.MethodPatch:
			description := requestEmbedDescription(t, request)
			if strings.HasPrefix(description, "### ") {
				select {
				case <-cardEditsMayFinish:
				case <-time.After(5 * time.Second):
				}

				cardEditsDone.Add(1)
			} else {
				answerEdits <- description
			}

			return newJSONResponse(t, request, cardMessage), nil
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}))
}

func TestRespondToMessageStartsGenerationBeforeProgressCardEditsFinish(t *testing.T) {
	t.Parallel()

	const answerText = "The answer."

	llmCalled := make(chan struct{})
	answerEdits := make(chan string, 8)

	var (
		llmCalledOnce      sync.Once
		cardEditsDone      atomic.Int64
		cardEditsAtLLMCall atomic.Int64
	)

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.session = newCardBlockingRespondSession(
		t,
		newTestProgressCardMessage(),
		llmCalled,
		&cardEditsDone,
		answerEdits,
	)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		llmCalledOnce.Do(func() {
			cardEditsAtLLMCall.Store(cardEditsDone.Load())
			close(llmCalled)
		})

		return handle(newStreamDelta(answerText, finishReasonStop))
	})

	started := time.Now()

	err := instance.respondToMessage(
		t.Context(),
		newRateLimitedRespondToMessageConfig(),
		newRateLimitedRespondToMessageSourceMessage(testProgressChannelID, "user-message-1", "user-1"),
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("respond to message: %v", err)
	}

	if got := cardEditsAtLLMCall.Load(); got != 0 {
		t.Fatalf("expected the model request before any card edit finished, %d had finished", got)
	}

	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("expected the reply not to wait for card edits, took %s", elapsed)
	}

	select {
	case description := <-answerEdits:
		if !strings.Contains(description, answerText) {
			t.Fatalf("expected the answer to replace the card, got %q", description)
		}
	default:
		t.Fatal("expected the answer to be rendered over the progress card")
	}
}

func TestBuildRequestProgressEmbedShowsWebSearch(t *testing.T) {
	t.Parallel()

	queries := []string{"first *query*", "second query"}

	const (
		rail = "✓ ~~Reading conversation~~\n\n" +
			"✓ ~~Gathering context~~\n\n"
		searches = "**Searches**\n" +
			"• first \\*query\\*\n" +
			"• second query"
	)

	tests := []struct {
		name            string
		search          requestProgressSearch
		wantDescription string
	}{
		{
			name:   "searching",
			search: requestProgressSearch{queries: queries, sourceCount: 0, finished: false, failed: false},
			wantDescription: "### ⠙ Searching the web\n\n" + rail +
				"› **Generating response** — *Running 2 searches*\n\n" + searches,
		},
		{
			name:   "reading sources",
			search: requestProgressSearch{queries: queries, sourceCount: 12, finished: true, failed: false},
			wantDescription: "### ⠙ Writing the answer\n\n" + rail +
				"› **Generating response** — *Reading 12 sources*\n\n" + searches,
		},
		{
			name:   "reading one source",
			search: requestProgressSearch{queries: queries, sourceCount: 1, finished: true, failed: false},
			wantDescription: "### ⠙ Writing the answer\n\n" + rail +
				"› **Generating response** — *Reading 1 source*\n\n" + searches,
		},
		{
			name:   "no sources",
			search: requestProgressSearch{queries: queries, sourceCount: 0, finished: true, failed: false},
			wantDescription: "### ⠙ Writing the answer\n\n" + rail +
				"› **Generating response** — *No sources found*\n\n" + searches,
		},
		{
			name:   "failed",
			search: requestProgressSearch{queries: queries, sourceCount: 0, finished: true, failed: true},
			wantDescription: "### ⠙ Writing the answer\n\n" + rail +
				"› **Generating response** — *Web search unavailable*\n\n" + searches,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			view := newRequestProgressView(requestProgressStageGeneratingResponse)
			view.search = test.search

			embed := buildRequestProgressEmbed(
				view,
				"model-1",
				3*time.Second,
				time.Time{},
				requestProgressSpinnerFrame(1),
			)

			if embed.Description != test.wantDescription {
				t.Fatalf("unexpected embed description:\n%q\nwant:\n%q", embed.Description, test.wantDescription)
			}

			if embed.Footer == nil || embed.Footer.Text != "Step 3 of 3 · 0:03" {
				t.Fatalf("unexpected embed footer: %#v", embed.Footer)
			}
		})
	}
}

func TestBuildRequestProgressEmbedCountsQueriesPastTheLimit(t *testing.T) {
	t.Parallel()

	queries := make([]string, 0, requestProgressMaxQueries+3)
	for index := range requestProgressMaxQueries + 3 {
		queries = append(queries, strings.Repeat("query ", 40)+strconv.Itoa(index))
	}

	view := newRequestProgressView(requestProgressStageGeneratingResponse)
	view.search = requestProgressSearch{queries: queries, sourceCount: 40, finished: true, failed: false}

	embed := buildRequestProgressEmbed(
		view,
		"model-1",
		0,
		time.Time{},
		requestProgressSpinnerFrame(0),
	)

	if got := strings.Count(embed.Description, "\n• "); got != requestProgressMaxQueries {
		t.Fatalf("expected %d listed queries, got %d: %q", requestProgressMaxQueries, got, embed.Description)
	}

	if !strings.HasSuffix(embed.Description, "…\n+3 more") {
		t.Fatalf("expected shortened queries and the rest counted: %q", embed.Description)
	}

	if got := runeCount(embed.Description); got > embedResponseMaxLength {
		t.Fatalf("expected the card within %d runes, got %d", embedResponseMaxLength, got)
	}
}

func TestCountWebSearchResultSourcesCountsEachPageOnce(t *testing.T) {
	t.Parallel()

	results := []webSearchResult{
		{
			Query: "one",
			Text: "Title: A1\nURL: https://a.example/1\n\n" +
				"Title: A2\nURL: https://a.example/2\n\n" +
				"Title: A3\nURL: https://a.example/3",
		},
		{
			Query: "two",
			Text:  "Title: B1\nURL: https://b.example/1\n\nTitle: A2 again\nURL: https://A.example/2",
		},
	}

	// A3 is past the per-result limit and A2 was found by both queries.
	if got := countWebSearchResultSources(results, 2); got != 3 {
		t.Fatalf("countWebSearchResultSources() = %d, want 3", got)
	}
}

// progressCardEdits records the descriptions of progress card edits and lets
// a test wait for one containing some text.
type progressCardEdits struct {
	mu           sync.Mutex
	descriptions []string
	changed      chan struct{}
}

func newProgressCardEdits() *progressCardEdits {
	return &progressCardEdits{mu: sync.Mutex{}, descriptions: nil, changed: make(chan struct{}, 64)}
}

func (edits *progressCardEdits) add(description string) {
	edits.mu.Lock()
	edits.descriptions = append(edits.descriptions, description)
	edits.mu.Unlock()

	select {
	case edits.changed <- struct{}{}:
	default:
	}
}

func (edits *progressCardEdits) snapshot() []string {
	edits.mu.Lock()
	defer edits.mu.Unlock()

	return slices.Clone(edits.descriptions)
}

// waitFor waits until an edit contains every part, reporting false after
// a few seconds.
func (edits *progressCardEdits) waitFor(parts ...string) bool {
	deadline := time.After(3 * time.Second)

	for {
		for _, description := range edits.snapshot() {
			if containsAll(description, parts...) {
				return true
			}
		}

		select {
		case <-edits.changed:
		case <-deadline:
			return false
		}
	}
}

func containsAll(text string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}

	return true
}

func TestRespondToMessageShowsWebSearchOnProgressCard(t *testing.T) {
	t.Parallel()

	edits := newProgressCardEdits()
	cardMessage := newTestProgressCardMessage()
	cardMessage.ChannelID = testWebSearchChannelID

	var roundCount atomic.Int64

	chatClient := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		if roundCount.Add(1) == 1 {
			return handle(toolCallDelta(parallelWebSearchToolCalls()...))
		}

		if !edits.waitFor("Writing the answer", "Reading 3 sources", "• "+testWebSearchQueryOne) {
			t.Error("expected the card to count the sources while the model writes the answer")
		}

		return handle(newStreamDelta(testWebSearchToolAnswer, finishReasonStop))
	})

	webSearch := newStubWebSearchClient(func(_ context.Context, _ config, queries []string) ([]webSearchResult, error) {
		if !edits.waitFor("Searching the web", "• "+testWebSearchQueryOne, "• "+testWebSearchQueryTwo) {
			t.Error("expected the card to list the queries while they run")
		}

		// Each query finds its own page and a shared one: three sources.
		results := make([]webSearchResult, 0, len(queries))
		for index, query := range queries {
			results = append(results, webSearchResult{
				Query: query,
				Text: "Title: Result title\nURL: https://result.example/" + strconv.Itoa(index) +
					"\nSnippet:\n| " + testWebSearchResultText +
					"\n\nTitle: Shared title\nURL: https://shared.example/page",
			})
		}

		return results, nil
	})

	instance := newSearchTestBot(chatClient, webSearch)
	instance.session = newDirectMessageTestSession(t, testWebSearchChannelID, testWebSearchBotUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/typing"):
			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/messages"):
			return newJSONResponse(t, request, cardMessage), nil
		case request.Method == http.MethodPatch:
			edits.add(requestEmbedDescription(t, request))

			return newJSONResponse(t, request, cardMessage), nil
		default:
			return newNoContentResponse(request), nil
		}
	}))

	respondWithWebSearchTool(t, instance, newWebSearchToolTestConfig())

	descriptions := edits.snapshot()
	if len(descriptions) == 0 || !strings.Contains(descriptions[len(descriptions)-1], testWebSearchToolAnswer) {
		t.Fatalf("expected the answer to replace the card last, got %#v", descriptions)
	}

	for _, description := range descriptions {
		if strings.Contains(description, "Title") || strings.Contains(description, "example/") {
			t.Fatalf("expected the card to count the sources without listing them, got %q", description)
		}
	}
}
