package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	testCompactionBoundarySystemPrompt = "Always be helpful."
	compactionBoundarySummarizerText   = "Condensed handoff summary."
	compactionBoundaryTestModel        = "openai/main-model"
	compactionBoundaryDeciderModel     = "openai/decider-model"
)

// countCompactionBoundaryCalls counts the number of model API calls made while
// summarizing conversation history for auto-compaction, and the number of
// model requests total. A stable compaction boundary means exactly one
// summarization call on the request that compacts and zero on every later
// request that reuses the boundary.
type countCompactionBoundaryClient struct {
	summarizeCalls atomic.Int64
	requests       atomic.Int64
}

func (client *countCompactionBoundaryClient) streamChatCompletion(
	_ context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	client.requests.Add(1)

	if isCompactionBoundarySummaryPrompt(request.Messages) {
		client.summarizeCalls.Add(1)

		return handle(newStreamDelta(compactionBoundarySummarizerText, ""))
	}

	if request.ConfiguredModel == compactionBoundaryDeciderModel {
		return handle(newStreamDelta(`{"needs_search":false,"queries":[]}`, ""))
	}

	return handle(newStreamDelta("model answer.", "stop"))
}

func isCompactionBoundarySummaryPrompt(messages []chatMessage) bool {
	if len(messages) == 0 {
		return false
	}

	first, _ := messages[0].Content.(string)

	return first == autoCompactSummarySystemPrompt() || first == autoCompactMergeSystemPrompt()
}

func newCompactionBoundaryTestFixture(
	t *testing.T,
	client *countCompactionBoundaryClient,
) (*bot, config, *discordgo.Message) {
	t.Helper()

	session := newCompactionBoundaryTestSession(t)

	instance := new(bot)
	instance.session = session
	instance.chatCompletions = client
	instance.nodes = newMessageNodeStore(10)
	instance.currentGroundingEnabledValue = new(bool)

	loadedConfig := testSearchConfig()
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages
	loadedConfig.SystemPrompt = testCompactionBoundarySystemPrompt
	loadedConfig.ModelContextWindows = map[string]int{
		compactionBoundaryTestModel:    1000,
		compactionBoundaryDeciderModel: 100000,
	}

	sourceMessage := newCompactionBoundarySourceMessage()

	return instance, loadedConfig, sourceMessage
}

func newCompactionBoundaryTestSession(t *testing.T) *discordgo.Session {
	t.Helper()

	progressMessage := new(discordgo.Message)
	progressMessage.ID = "progress-message"
	progressMessage.ChannelID = "channel-1"
	progressMessage.Author = newDiscordUser("bot-1", true)

	return newDirectMessageTestSession(t, "channel-1", "bot-1", roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/channel-1/messages":
			return newJSONResponse(t, request, progressMessage), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/channel-1/messages/progress-message":
			return newJSONResponse(t, request, progressMessage), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}))
}

// newCompactionBoundarySourceMessage builds a reply chain (user -> assistant ->
// user newest-last) whose history is large enough to trigger auto-compaction
// for a 300-token context window.
func newCompactionBoundarySourceMessage() *discordgo.Message {
	oldestMessage := new(discordgo.Message)
	oldestMessage.ID = "user-message-1"
	oldestMessage.ChannelID = "channel-1"
	oldestMessage.Author = newDiscordUser("user-1", false)
	oldestMessage.Content = "at ai " + repeatedCompactionBoundaryText("older context", 140)

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = "assistant-message-1"
	assistantMessage.ChannelID = "channel-1"
	assistantMessage.Author = newDiscordUser("bot-1", true)
	assistantMessage.Type = discordgo.MessageTypeReply
	assistantMessage.Content = repeatedCompactionBoundaryText("assistant answer", 40)
	assistantMessage.MessageReference = oldestMessage.Reference()
	assistantMessage.ReferencedMessage = oldestMessage

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "user-message-2"
	sourceMessage.ChannelID = "channel-1"
	sourceMessage.Author = newDiscordUser("user-1", false)
	sourceMessage.Content = "at ai " + repeatedCompactionBoundaryText("follow up question", 60)
	sourceMessage.Mentions = []*discordgo.User{newDiscordUser("bot-1", false)}
	sourceMessage.MessageReference = assistantMessage.Reference()
	sourceMessage.ReferencedMessage = assistantMessage

	return sourceMessage
}

func repeatedCompactionBoundaryText(fragment string, repeats int) string {
	return strings.TrimSpace(strings.Repeat(fragment+" ", repeats))
}

func prepareCompactionBoundaryResponse(
	t *testing.T,
	instance *bot,
	loadedConfig config,
	sourceMessage *discordgo.Message,
) (chatCompletionRequest, *responseTracker, []string, error) {
	t.Helper()

	progress := instance.startRequestProgress(
		context.Background(),
		sourceMessage,
		compactionBoundaryTestModel,
	)

	return instance.prepareMessageResponse(
		context.Background(),
		loadedConfig,
		sourceMessage,
		compactionBoundaryTestModel,
		progress,
	)
}

func requestCarriesCompactionBoundary(request chatCompletionRequest) bool {
	if len(request.Messages) < 2 {
		return false
	}

	summary, ok := request.Messages[1].Content.(string)
	if !ok {
		return false
	}

	return strings.Contains(summary, autoCompactSummaryPrefix)
}

// TestRequestsAfterAutoCompactionReuseTheBoundary reproduces the slow
// post-compaction behavior: after the first request auto-compacts the
// conversation and produces a handoff summary, a follow-up request that walks
// the same reply chain must reuse that boundary instead of re-summarizing the
// entire history with model calls again.
func TestRequestsAfterAutoCompactionReuseTheBoundary(t *testing.T) {
	t.Parallel()

	client := new(countCompactionBoundaryClient)

	instance, loadedConfig, sourceMessage := newCompactionBoundaryTestFixture(t, client)

	firstRequest, firstTracker, _, err := prepareCompactionBoundaryResponse(
		t,
		instance,
		loadedConfig,
		sourceMessage,
	)
	if err != nil {
		t.Fatalf("prepare first response: %v", err)
	}

	if firstTracker != nil {
		firstTracker.release(instance.nodes, "first answer.", "")
	}

	// First request must compact and pay at least one summarization call: the
	// full chain still has to be summarized once to produce the boundary.
	if got := client.summarizeCalls.Load(); got < 1 {
		t.Fatalf("expected first request to summarize history: %d calls", got)
	}

	// The first request compacts the full chain and carries the handoff
	// summary as the second message. The summary's exact text is truncated to
	// the tiny test window, but the summary turn must be present.
	if len(firstRequest.Messages) < 2 {
		t.Fatalf(
			"expected the compacted first request to carry a summary turn: %d messages",
			len(firstRequest.Messages),
		)
	}

	firstSummary, ok := firstRequest.Messages[1].Content.(string)
	if !ok || strings.TrimSpace(firstSummary) == "" {
		t.Fatalf("expected the compacted first request to carry a handoff summary: %#v", firstRequest.Messages[1])
	}

	summarizationCountAfterFirst := client.summarizeCalls.Load()

	// A second request over the same thread reuses the boundary: re-walking
	// history, re-augmenting, and re-summarizing all of it is exactly what
	// made post-compaction requests stall for minutes. No new summarization
	// calls may happen once the first request has recorded the boundary.
	second := newCompactionBoundaryFollowUpMessage(
		"user-message-3",
		sourceMessage,
		"more follow up",
	)

	secondRequest, secondTracker, _, err := prepareCompactionBoundaryResponse(
		t,
		instance,
		loadedConfig,
		second,
	)
	if err != nil {
		t.Fatalf("prepare second response: %v", err)
	}

	if secondTracker != nil {
		secondTracker.release(instance.nodes, "second answer.", "")
	}

	if got := client.summarizeCalls.Load(); got != summarizationCountAfterFirst {
		t.Fatalf(
			"expected the compaction boundary to be reused without re-summarizing history: "+
				"got %d summarization calls after the first request recorded %d",
			got,
			summarizationCountAfterFirst,
		)
	}

	if !requestCarriesCompactionBoundary(secondRequest) {
		t.Fatal("expected the follow-up request to carry the reused handoff summary")
	}

	if estimateChatCompletionRequestTokens(secondRequest) >= estimateChatCompletionRequestTokens(firstRequest) {
		t.Fatalf(
			"expected follow-up request to stay bounded by the summary instead of "+
				"re-carrying full history: %d tokens >= %d tokens",
			estimateChatCompletionRequestTokens(secondRequest),
			estimateChatCompletionRequestTokens(firstRequest),
		)
	}
}

func newCompactionBoundaryFollowUpMessage(
	messageID string,
	parent *discordgo.Message,
	content string,
) *discordgo.Message {
	followUp := new(discordgo.Message)
	followUp.ID = messageID
	followUp.ChannelID = "channel-1"
	followUp.Author = newDiscordUser("user-1", false)
	followUp.Content = "<@bot-1> " + content
	followUp.Mentions = []*discordgo.User{newDiscordUser("bot-1", false)}
	followUp.MessageReference = parent.Reference()
	followUp.ReferencedMessage = parent

	return followUp
}

// TestCompactThenTwoFollowUpsReuseBoundary checks the boundary keeps summary
// work at one model call across several follow-ups until the tail itself grows
// large enough to need its own compaction.
func TestCompactThenTwoFollowUpsReuseBoundary(t *testing.T) {
	t.Parallel()

	client := new(countCompactionBoundaryClient)

	instance, loadedConfig, sourceMessage := newCompactionBoundaryTestFixture(t, client)

	_, firstTracker, _, err := prepareCompactionBoundaryResponse(
		t,
		instance,
		loadedConfig,
		sourceMessage,
	)
	if err != nil {
		t.Fatalf("prepare first response: %v", err)
	}

	if firstTracker != nil {
		firstTracker.release(instance.nodes, "first answer.", "")
	}

	if got := client.summarizeCalls.Load(); got < 1 {
		t.Fatalf("expected first request to summarize history: %d calls", got)
	}

	summarizationCountAfterFirst := client.summarizeCalls.Load()

	firstFollowUp := newCompactionBoundaryFollowUpMessage(
		"user-message-3",
		sourceMessage,
		"another follow up",
	)
	secondFollowUp := newCompactionBoundaryFollowUpMessage(
		"user-message-4",
		firstFollowUp,
		"yet another follow up",
	)

	for _, followUp := range []*discordgo.Message{firstFollowUp, secondFollowUp} {
		_, tracker, _, err := prepareCompactionBoundaryResponse(
			t,
			instance,
			loadedConfig,
			followUp,
		)
		if err != nil {
			t.Fatalf("prepare follow-up response: %v", err)
		}

		if tracker != nil {
			tracker.release(instance.nodes, "follow-up response", "")
		}
	}

	if got := client.summarizeCalls.Load(); got != summarizationCountAfterFirst {
		t.Fatalf(
			"expected follow-ups to reuse the boundary without extra summarizations: "+
				"got %d calls after the first request recorded %d",
			got,
			summarizationCountAfterFirst,
		)
	}
}

// TestCompactionBoundaryPersistsThroughSnapshotRoundTrip locks in that the
// auto-compaction boundary survives the message node persistence round-trip,
// so a restarted bot still reuses the boundary instead of re-summarizing the
// entire history.
func TestCompactionBoundaryPersistsThroughSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	node := new(messageNode)
	node.role = messageRoleUser
	node.text = "latest user message"
	node.compactionSummary = &messageNodeCompactionSummary{
		text:    "Condensed earlier history.",
		anchor:  "source-message-2",
		applied: true,
	}
	node.initialized = true

	snapshot, ok := messageNodeSnapshotFromLockedNode(node)
	if !ok {
		t.Fatal("expected locked node snapshot")
	}

	restored := snapshot.messageNode()
	if restored.compactionSummary == nil {
		t.Fatal("expected restored node to carry the compaction boundary")
	}

	if restored.compactionSummary.text != node.compactionSummary.text ||
		restored.compactionSummary.anchor != node.compactionSummary.anchor ||
		!restored.compactionSummary.applied {
		t.Fatalf(
			"unexpected restored compaction boundary: %#v",
			restored.compactionSummary,
		)
	}
}

// TestCompactionBoundarySnapshotSanitizesNULs verifies the sanitizer passes the
// compaction boundary through unchanged for Postgres persistence.
func TestCompactionBoundarySnapshotSanitizesNULs(t *testing.T) {
	t.Parallel()

	summary := &messageNodeCompactionSummaryJSON{
		Text:    "summary\x00text",
		Anchor:  "anchor\x00id",
		Applied: true,
	}

	sanitized := sanitizeCompactionSummary(summary)
	if sanitized == nil {
		t.Fatal("expected a sanitized compaction summary")
	}

	if sanitized.Text != "summary�text" || sanitized.Anchor != "anchor�id" {
		t.Fatalf("expected NULs replaced with the replacement character: %#v", sanitized)
	}

	if !sanitized.Applied {
		t.Fatal("expected sanitized summary to stay applied")
	}
}

// TestCompactBoundaryWarningFormat locks in the user-facing warning format so
// the persisted boundary keeps rendering the same warning.
func TestCompactBoundaryWarningFormat(t *testing.T) {
	t.Parallel()

	result := autoCompactResult{
		Applied:          true,
		Strategy:         autoCompactStrategySummary,
		TruncatedMessage: false,
	}

	warnings := result.warningsForPath(testAutoCompactMainPath)
	if len(warnings) == 0 {
		t.Fatal("expected a compaction warning")
	}

	want := fmt.Sprintf(
		"Warning: %s %s",
		testAutoCompactMainPath,
		"auto-compacted older conversation context to fit the model context window.",
	)
	if !slices.Contains(warnings, want) {
		t.Fatalf("expected warning %q in %#v", want, warnings)
	}
}
