package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	testFinalRenderChannelID = "channel-1"
	testFinalRenderMessageID = "response-message"
	testFinalRenderAnswer    = "The finished answer, all of it."
)

// finalRenderTestDiscord fakes the Discord message endpoints the final
// render check uses: GET returns the message as it is shown, PATCH records
// the edit and shows it from then on. With keepFrame set, the first edit to
// keepFrameOver shows keepFrame instead, as when Discord applies an earlier
// streaming edit after the final edit.
type finalRenderTestDiscord struct {
	mu            sync.Mutex
	shown         string
	getStatus     int
	getBody       string
	keepFrameOver string
	keepFrame     string
	patches       []string
	patchedParts  []int
	gets          int
}

func (discord *finalRenderTestDiscord) roundTrip(request *http.Request) (*http.Response, error) {
	messagePath := "/api/v9/channels/" + testFinalRenderChannelID + "/messages/" + testFinalRenderMessageID

	discord.mu.Lock()
	defer discord.mu.Unlock()

	switch {
	case request.Method == http.MethodGet && request.URL.Path == messagePath:
		discord.gets++

		if discord.getStatus != 0 {
			return newInteractionJSONResponse(request, discord.getStatus, discord.getBody), nil
		}

		return discord.messageResponse(request, discord.shown), nil
	case request.Method == http.MethodPatch && request.URL.Path == messagePath:
		var payload struct {
			Embeds []struct {
				Description string `json:"description"`
			} `json:"embeds"`
			Components []json.RawMessage `json:"components"`
		}

		err := json.NewDecoder(request.Body).Decode(&payload)
		if err != nil || len(payload.Embeds) != 1 {
			return newInteractionJSONResponse(request, http.StatusBadRequest, `{"message":"bad edit","code":0}`), nil
		}

		description := payload.Embeds[0].Description
		discord.patches = append(discord.patches, description)
		discord.patchedParts = append(discord.patchedParts, len(payload.Components))
		discord.shown = description

		if discord.keepFrame != "" && description == discord.keepFrameOver {
			discord.shown = discord.keepFrame
			discord.keepFrame = ""
		}

		return discord.messageResponse(request, description), nil
	default:
		return newInteractionJSONResponse(request, http.StatusNotFound, `{"message":"unexpected","code":0}`), nil
	}
}

// newMessageShowing returns the response message with one embed showing
// description.
func newMessageShowing(description string) *discordgo.Message {
	embed := new(discordgo.MessageEmbed)
	embed.Description = description

	message := new(discordgo.Message)
	message.ID = testFinalRenderMessageID
	message.ChannelID = testFinalRenderChannelID
	message.Embeds = []*discordgo.MessageEmbed{embed}

	return message
}

func (discord *finalRenderTestDiscord) messageResponse(request *http.Request, description string) *http.Response {
	body, err := json.Marshal(newMessageShowing(description))
	if err != nil {
		return newInteractionJSONResponse(request, http.StatusInternalServerError, `{"message":"marshal","code":0}`)
	}

	return newInteractionJSONResponse(request, http.StatusOK, string(body))
}

func (discord *finalRenderTestDiscord) show(description string) {
	discord.mu.Lock()
	defer discord.mu.Unlock()

	discord.shown = description
}

func (discord *finalRenderTestDiscord) recorded() ([]string, []int, int) {
	discord.mu.Lock()
	defer discord.mu.Unlock()

	return append([]string(nil), discord.patches...), append([]int(nil), discord.patchedParts...), discord.gets
}

func (discord *finalRenderTestDiscord) shownDescription() string {
	discord.mu.Lock()
	defer discord.mu.Unlock()

	return discord.shown
}

func newFinalRenderTestBot(t *testing.T, discord *finalRenderTestDiscord) *bot {
	t.Helper()

	instance := new(bot)
	instance.session = newDirectMessageTestSession(
		t,
		testFinalRenderChannelID,
		"bot-user",
		roundTripFunc(discord.roundTrip),
	)
	instance.nodes = newMessageNodeStore(10)

	return instance
}

func newFinalRenderRecord(description string) renderedEmbedMessage {
	message := new(discordgo.Message)
	message.ID = testFinalRenderMessageID
	message.ChannelID = testFinalRenderChannelID

	return renderedEmbedMessage{
		message: message,
		embed:   buildResponseEmbed(description, "provider/model", embedColorComplete, nil, ""),
		components: buildEmbedComponents(responseActions{
			showSources:  false,
			showImages:   false,
			showThinking: false,
			showGist:     true,
			showExport:   true,
		}),
	}
}

func TestShowsStreamingFrameInstead(t *testing.T) {
	t.Parallel()

	final := buildResponseEmbed(testFinalRenderAnswer, "provider/model", embedColorComplete, nil, "")

	testCases := []struct {
		name    string
		current *discordgo.Message
		want    bool
	}{
		{name: "streaming frame", current: newMessageShowing("The finished" + streamingIndicator), want: true},
		{name: "final render", current: newMessageShowing(testFinalRenderAnswer), want: false},
		{name: "final render trimmed", current: newMessageShowing(" " + testFinalRenderAnswer + "\n"), want: false},
		{name: "other text without indicator", current: newMessageShowing("Something else"), want: false},
		{name: "no embeds", current: new(discordgo.Message), want: false},
		{name: "no message", current: nil, want: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := showsStreamingFrameInstead(testCase.current, final); got != testCase.want {
				t.Fatalf("showsStreamingFrameInstead() = %t, want %t", got, testCase.want)
			}
		})
	}

	finalEndingInIndicator := buildResponseEmbed("Wait for it"+streamingIndicator, "", embedColorComplete, nil, "")
	if showsStreamingFrameInstead(newMessageShowing("Wait for it"+streamingIndicator), finalEndingInIndicator) {
		t.Fatal("expected a final render that itself ends in the indicator to count as shown")
	}
}

func TestRestoreFinalRenderReappliesFinalRenderOverStreamingFrame(t *testing.T) {
	t.Parallel()

	discord := new(finalRenderTestDiscord)
	discord.show("The finished" + streamingIndicator)
	instance := newFinalRenderTestBot(t, discord)

	instance.restoreFinalRender(context.Background(), newFinalRenderRecord(testFinalRenderAnswer))

	patches, patchedParts, gets := discord.recorded()
	if gets != 1 || len(patches) != 1 || patches[0] != testFinalRenderAnswer {
		t.Fatalf("expected 1 read and the final render edited back in, got %d reads and edits %q", gets, patches)
	}

	if patchedParts[0] != 1 {
		t.Fatalf("expected the restored edit to carry the final buttons row, got %d component rows", patchedParts[0])
	}
}

func TestRestoreFinalRenderLeavesMessageAlone(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		shown     string
		getStatus int
		getBody   string
	}{
		{name: "final render shown", shown: testFinalRenderAnswer, getStatus: 0, getBody: ""},
		{
			name:      "message deleted",
			shown:     "",
			getStatus: http.StatusNotFound,
			getBody:   `{"message":"Unknown Message","code":10008}`,
		},
		{
			name:      "read failed",
			shown:     "",
			getStatus: http.StatusInternalServerError,
			getBody:   `{"message":"Internal Server Error","code":0}`,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			discord := new(finalRenderTestDiscord)
			discord.show(testCase.shown)
			discord.getStatus = testCase.getStatus
			discord.getBody = testCase.getBody
			instance := newFinalRenderTestBot(t, discord)

			instance.restoreFinalRender(context.Background(), newFinalRenderRecord(testFinalRenderAnswer))

			patches, _, gets := discord.recorded()
			if gets != 1 || len(patches) != 0 {
				t.Fatalf("expected 1 read and no edit, got %d reads and edits %q", gets, patches)
			}
		})
	}
}

func TestConfirmFinalRenderAfterRunsEveryCheck(t *testing.T) {
	t.Parallel()

	discord := new(finalRenderTestDiscord)
	discord.show(testFinalRenderAnswer)
	instance := newFinalRenderTestBot(t, discord)

	record := newFinalRenderRecord(testFinalRenderAnswer)

	// The first check finds the final render; a late streaming edit lands
	// only afterwards, and the second check still restores it.
	instance.confirmFinalRenderAfter(context.Background(), []renderedEmbedMessage{record}, 0)
	discord.show("The finished" + streamingIndicator)
	instance.confirmFinalRenderAfter(context.Background(), []renderedEmbedMessage{record}, 0, 0)

	patches, _, gets := discord.recorded()
	if gets != 3 || len(patches) != 1 || patches[0] != testFinalRenderAnswer {
		t.Fatalf("expected 3 reads and 1 restoring edit, got %d reads and edits %q", gets, patches)
	}
}

func TestRenderEmbedResponseRecordsFinalRenderUntilTheNextRender(t *testing.T) {
	t.Parallel()

	session := newDirectMessageTestSession(t, testFinalRenderChannelID, "bot-user", roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		response := new(discordgo.Message)
		response.ID = testFinalRenderMessageID
		response.ChannelID = testFinalRenderChannelID

		return newJSONResponse(t, request, response), nil
	}))

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)

	sourceMessage := newPromptMessage("user-message-1", testFinalRenderChannelID, "user-1", "bot-user")
	tracker := newResponseTracker(sourceMessage, "provider/model")
	ctx := context.Background()

	err := instance.renderEmbedResponse(ctx, tracker, nil, []string{"The finished"}, "", false, false)
	if err != nil {
		t.Fatalf("render streaming frame: %v", err)
	}

	if tracker.finalRender != nil {
		t.Fatalf("expected no final render after a streaming frame, got %#v", tracker.finalRender)
	}

	err = instance.renderEmbedResponse(ctx, tracker, nil, []string{testFinalRenderAnswer}, finishReasonStop, true, false)
	if err != nil {
		t.Fatalf("render final response: %v", err)
	}

	if len(tracker.finalRender) != 1 {
		t.Fatalf("expected the final render of 1 message, got %#v", tracker.finalRender)
	}

	record := tracker.finalRender[0]
	if record.message.ID != testFinalRenderMessageID ||
		record.embed.Description != testFinalRenderAnswer ||
		len(record.components) != 1 {
		t.Fatalf("unexpected final render record: %#v", record)
	}

	// A retry streams over the same message, so the earlier final render
	// no longer describes it.
	err = instance.renderEmbedResponse(ctx, tracker, nil, []string{"Retried"}, "", false, false)
	if err != nil {
		t.Fatalf("render retried streaming frame: %v", err)
	}

	if tracker.finalRender != nil {
		t.Fatalf("expected a later streaming frame to clear the final render, got %#v", tracker.finalRender)
	}
}

func TestRespondToMessageRestoresFinalRenderOverStaleStreamingFrame(t *testing.T) {
	t.Parallel()

	// Discord keeps an earlier streaming frame over the final edit.
	discord := new(finalRenderTestDiscord)
	discord.keepFrameOver = testFinalRenderAnswer
	discord.keepFrame = "The finished" + streamingIndicator

	session := newDirectMessageTestSession(t, testFinalRenderChannelID, "bot-user", roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/typing"):
			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/messages"):
			return discord.messageResponse(request, ""), nil
		default:
			return discord.roundTrip(request)
		}
	}))

	chatClient := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		return handle(newStreamDelta(testFinalRenderAnswer, finishReasonStop))
	})
	instance := newSearchTestBot(chatClient, newSingleResultWebSearchClient())
	instance.session = session
	instance.finalRenderCheckDelays = []time.Duration{time.Millisecond}

	err := instance.respondToMessage(
		context.Background(),
		newWebSearchToolTestConfig(),
		newPromptMessage("user-message-1", testFinalRenderChannelID, "user-1", "bot-user"),
		testWebSearchMainModel,
	)
	if err != nil {
		t.Fatalf("respond to message: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if discord.shownDescription() == testFinalRenderAnswer {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	patches, _, gets := discord.recorded()
	if discord.shownDescription() != testFinalRenderAnswer || gets == 0 {
		t.Fatalf("expected the background check to restore the final render, got %d reads and edits %q", gets, patches)
	}

	finalEdits := 0

	for _, patch := range patches {
		if patch == testFinalRenderAnswer {
			finalEdits++
		}
	}

	if finalEdits != 2 {
		t.Fatalf("expected the final edit and one restoring edit, got edits %q", patches)
	}
}
