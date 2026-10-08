package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type exportChannelUploadCapture struct {
	deferred     discordgo.InteractionResponse
	edits        []string
	filename     string
	fileContents string
}

func newExportChannelCommandInteraction(channelID string, tokens int, userID string) *discordgo.InteractionCreate {
	user := new(discordgo.User)
	user.ID = userID

	member := new(discordgo.Member)
	member.User = user

	channelIDOption := new(discordgo.ApplicationCommandInteractionDataOption)
	channelIDOption.Name = exportChannelChannelIDOptionName
	channelIDOption.Type = discordgo.ApplicationCommandOptionString
	channelIDOption.Value = channelID

	tokensOption := new(discordgo.ApplicationCommandInteractionDataOption)
	tokensOption.Name = exportChannelTokensOptionName
	tokensOption.Type = discordgo.ApplicationCommandOptionInteger
	tokensOption.Value = float64(tokens)

	var commandData discordgo.ApplicationCommandInteractionData

	commandData.Name = exportChannelCommandName
	commandData.Options = []*discordgo.ApplicationCommandInteractionDataOption{
		channelIDOption,
		tokensOption,
	}

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.AppID = "application-id"
	interaction.Token = "interaction-token"
	interaction.Type = discordgo.InteractionApplicationCommand
	interaction.GuildID = "guild-id"
	interaction.Member = member
	interaction.Data = commandData

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}

func newExportChannelMessage(id, userID, username, content string, bot bool, timestamp time.Time) *discordgo.Message {
	message := new(discordgo.Message)
	message.ID = id
	message.ChannelID = "channel-1"
	message.Content = content
	message.Timestamp = timestamp
	message.Author = newDiscordUser(userID, bot)
	message.Author.Username = username

	return message
}

// newExportChannelHistorySession serves ChannelMessages pages newest-first by
// call order, captures the deferred ack, and records every follow-up edit
// (progress JSON edits plus the final multipart JSON file upload).
func newExportChannelHistorySession(
	t *testing.T,
	pages [][]*discordgo.Message,
	capture *exportChannelUploadCapture,
) *discordgo.Session {
	t.Helper()

	server := &exportChannelTestServer{t: t, capture: capture, channelBody: `{"id":"channel-1","name":"general"}`}

	server.remaining = append([][]*discordgo.Message(nil), pages...)

	return newInteractionTestSessionWithTransport(t, server.roundTrip)
}

type exportChannelTestServer struct {
	t             *testing.T
	capture       *exportChannelUploadCapture
	remaining     [][]*discordgo.Message
	channelBody   string
	channelStatus int
	historyBody   string
	historyStatus int
}

func (server *exportChannelTestServer) roundTrip(request *http.Request) (*http.Response, error) {
	server.t.Helper()

	switch {
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/callback"):
		return captureDeferredInteractionRequest(server.t, request, &server.capture.deferred)
	case request.Method == http.MethodGet && request.URL.Path == "/api/v9/channels/channel-1":
		return server.serveChannel(request)
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/channels/channel-1/messages"):
		return server.serveHistory(request)
	case request.Method == http.MethodPatch && strings.HasSuffix(request.URL.Path, "/messages/@original"):
		return server.serveEdit(request)
	default:
		server.t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

		return nil, errUnexpectedTestRequest
	}
}

func (server *exportChannelTestServer) serveChannel(request *http.Request) (*http.Response, error) {
	server.t.Helper()

	if server.channelStatus != 0 {
		return newInteractionJSONResponse(request, server.channelStatus, server.channelBody), nil
	}

	return newInteractionJSONResponse(request, http.StatusOK, server.channelBody), nil
}

func exportChannelUnknownChannelRoundTrip(
	t *testing.T,
	capture *deferredInteractionCapture,
) roundTripFunc {
	t.Helper()

	return exportChannelStatusRoundTrip(t, capture, http.StatusNotFound, `{"message":"Unknown Channel","code":10003}`)
}

func exportChannelForumRoundTrip(
	t *testing.T,
	capture *deferredInteractionCapture,
) roundTripFunc {
	t.Helper()

	return exportChannelStatusRoundTrip(t, capture, http.StatusOK, `{"id":"channel-1","name":"forum","type":15}`)
}

func exportChannelStatusRoundTrip(
	t *testing.T,
	capture *deferredInteractionCapture,
	status int,
	body string,
) roundTripFunc {
	t.Helper()

	return func(request *http.Request) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/callback"):
			return captureDeferredInteractionRequest(t, request, &capture.deferredResponse)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v9/channels/channel-1":
			return newInteractionJSONResponse(request, status, body), nil
		case request.Method == http.MethodPatch && strings.HasSuffix(request.URL.Path, "/messages/@original"):
			return captureEditedInteractionRequest(t, request, &capture.editedResponse)
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}
}

func (server *exportChannelTestServer) serveHistory(request *http.Request) (*http.Response, error) {
	server.t.Helper()

	if server.historyStatus != 0 {
		return newInteractionJSONResponse(request, server.historyStatus, server.historyBody), nil
	}

	page := []*discordgo.Message{}

	if len(server.remaining) > 0 {
		page = server.remaining[0]
		server.remaining = server.remaining[1:]
	}

	return newJSONResponse(server.t, request, page), nil
}

func (server *exportChannelTestServer) serveEdit(request *http.Request) (*http.Response, error) {
	server.t.Helper()

	if strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/form-data") {
		return server.serveUpload(request)
	}

	var edited editedInteractionResponse

	response, editErr := captureEditedInteractionRequest(server.t, request, &edited)
	if editErr != nil {
		server.t.Fatalf("capture export progress edit: %v", editErr)
	}

	server.capture.edits = append(server.capture.edits, edited.Content)

	return response, nil
}

func (server *exportChannelTestServer) serveUpload(request *http.Request) (*http.Response, error) {
	server.t.Helper()

	err := request.ParseMultipartForm(10 * 1024 * 1024)
	if err != nil {
		server.t.Fatalf("parse multipart export upload: %v", err)
	}

	payloadJSON := request.FormValue("payload_json")

	var parsed discordgo.WebhookEdit
	if payloadErr := json.Unmarshal([]byte(payloadJSON), &parsed); payloadErr != nil {
		server.t.Fatalf("decode export payload: %v", payloadErr)
	}

	if parsed.Content != nil {
		server.capture.edits = append(server.capture.edits, *parsed.Content)
	}

	files := request.MultipartForm.File["files[0]"]
	if len(files) != 1 {
		server.t.Fatalf("expected one export file, got %d", len(files))
	}

	server.capture.filename = files[0].Filename

	uploaded, openErr := files[0].Open()
	if openErr != nil {
		server.t.Fatalf("open export upload: %v", openErr)
	}

	data, readErr := io.ReadAll(uploaded)
	_ = uploaded.Close()

	if readErr != nil {
		server.t.Fatalf("read export upload: %v", readErr)
	}

	server.capture.fileContents = string(data)

	return newInteractionJSONResponse(request, http.StatusOK, `{"id":"edited-message"}`), nil
}

func TestNewExportChannelCommand(t *testing.T) {
	t.Parallel()

	command := newExportChannelCommand()

	if command.Name != exportChannelCommandName {
		t.Fatalf("unexpected command name: got %q want %q", command.Name, exportChannelCommandName)
	}

	if len(command.Options) != 2 {
		t.Fatalf("expected 2 options, got %d", len(command.Options))
	}

	channelIDOption := command.Options[0]
	if channelIDOption.Name != exportChannelChannelIDOptionName ||
		channelIDOption.Type != discordgo.ApplicationCommandOptionString ||
		!channelIDOption.Required {
		t.Fatalf("unexpected channelid option: %+v", channelIDOption)
	}

	tokensOption := command.Options[1]
	if tokensOption.Name != exportChannelTokensOptionName ||
		tokensOption.Type != discordgo.ApplicationCommandOptionInteger ||
		!tokensOption.Required {
		t.Fatalf("unexpected tokens option: %+v", tokensOption)
	}

	if tokensOption.MinValue == nil || *tokensOption.MinValue != 1 {
		t.Fatalf("expected tokens minimum of 1, got %+v", tokensOption.MinValue)
	}
}

func TestHandleExportChannelCommandRequiresOwner(t *testing.T) {
	t.Parallel()

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)
	interaction := newExportChannelCommandInteraction("channel-1", 100, "some-other-user")

	instance := new(bot)
	instance.session = session

	err := instance.handleExportChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle export channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &capture.deferredResponse)

	if capture.requestCount != 2 {
		t.Fatalf("unexpected interaction request count: got %d want 2", capture.requestCount)
	}

	if !strings.Contains(capture.editedResponse.Content, "do not have permission") {
		t.Fatalf("unexpected edited response: %q", capture.editedResponse.Content)
	}
}

func TestHandleExportChannelCommandValidatesInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		channelID string
		tokens    int
		want      string
	}{
		{name: "missing channel id", channelID: "", tokens: 100, want: "`channelid` is required."},
		{name: "zero tokens", channelID: "channel-1", tokens: 0, want: "`tokens` must be a positive integer."},
		{name: "negative tokens", channelID: "channel-1", tokens: -5, want: "`tokens` must be a positive integer."},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var capture deferredInteractionCapture

			session := newDeferredInteractionTestSession(t, &capture)
			interaction := newExportChannelCommandInteraction(test.channelID, test.tokens, maintenanceOwnerID)

			instance := new(bot)
			instance.session = session

			err := instance.handleExportChannelCommand(session, interaction)
			if err != nil {
				t.Fatalf("handle export channel command: %v", err)
			}

			assertDeferredInteractionResponse(t, &capture.deferredResponse)

			if capture.editedResponse.Content != test.want {
				t.Fatalf("unexpected edited response: got %q want %q", capture.editedResponse.Content, test.want)
			}
		})
	}
}

func TestFetchExportChannelMessagesSkipsBotsAndRespectsBudget(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	second := time.Date(2026, time.September, 30, 12, 5, 0, 0, time.UTC)

	newestUser := newExportChannelMessage("newest", "user-1", "alice", "newest hello", false, second)
	botMessage := newExportChannelMessage("bot-1", "bot-id", "helper", "bot reply", true, second)
	oldestUser := newExportChannelMessage("oldest", "user-2", "bob", "oldest hello", false, first)
	webhookMessage := newExportChannelMessage("webhook-1", "user-3", "carol", "webhook note", false, first)
	webhookMessage.WebhookID = "webhook-id"

	pages := [][]*discordgo.Message{{newestUser, botMessage}, {oldestUser, webhookMessage}}

	var capture exportChannelUploadCapture

	session := newExportChannelHistorySession(t, pages, &capture)

	const tokenLimit = 100000

	headerTokens := exportChannelPayloadHeaderTokens("channel-1", tokenLimit)
	newestEntry, newestOK := exportChannelEntry(newestUser)
	oldestEntry, oldestOK := exportChannelEntry(oldestUser)

	if !newestOK || !oldestOK {
		t.Fatal("expected user messages to export")
	}

	fullBudget := headerTokens + newestEntry.tokens + oldestEntry.tokens

	onPageCalls := 0

	onPage := func(_, _, _ int) {
		onPageCalls++
	}

	messages, tokens, err := fetchExportChannelMessages(session, "channel-1", tokenLimit, onPage)
	if err != nil {
		t.Fatalf("fetch export channel messages: %v", err)
	}

	if onPageCalls != 2 {
		t.Fatalf("expected progress callback per page, got %d calls", onPageCalls)
	}

	if tokens != fullBudget {
		t.Fatalf("unexpected token total: got %d want %d", tokens, fullBudget)
	}

	if len(messages) != 2 {
		t.Fatalf("expected only user messages, got %+v", messages)
	}

	if messages[0].Username != "alice" || messages[1].Username != "bob" {
		t.Fatalf("expected newest-to-oldest order, got %+v", messages)
	}

	if messages[0].Username != "alice" || messages[0].Timestamp != second.UTC().Format(time.RFC3339) {
		t.Fatalf("expected username and timestamp on newest message, got %+v", messages[0])
	}

	if messages[0].tokens+messages[1].tokens+headerTokens != tokens {
		t.Fatalf("expected running total to sum header plus entries, got %+v (%d tokens)", messages, tokens)
	}

	// A budget below the header plus newest message stops before including anything.
	// The history session is single-pass (pages drain as they are served), so
	// rebuild it: reusing the exhausted session would return zero messages
	// regardless of the budget and never exercise the boundary. The tight
	// limit needs its own header reserve: the shared headerTokens was
	// measured at the full limit's digit width, which overcounts a small
	// budget and would admit the message.
	tightLimit := headerTokens + newestEntry.tokens - 1
	tightHeader := exportChannelPayloadHeaderTokens("channel-1", tightLimit)

	session = newExportChannelHistorySession(t, pages, &capture)

	messages, tokens, err = fetchExportChannelMessages(session, "channel-1", tightHeader+newestEntry.tokens-1, nil)
	if err != nil {
		t.Fatalf("fetch export channel messages with tight budget: %v", err)
	}

	if len(messages) != 0 {
		t.Fatalf("expected empty export under budget, got %+v (%d tokens)", messages, tokens)
	}
}

func TestMarshalExportChannelPayloadStampsFileTokens(t *testing.T) {
	t.Parallel()

	messages := []exportChannelMessage{{
		Username:  "alice",
		Content:   "hello",
		Timestamp: "2026-09-30T12:00:00Z",
	}}

	payload := buildExportChannelPayload("channel-1", 100000, messages, 0)

	encoded, err := marshalExportChannelPayload(&payload)
	if err != nil {
		t.Fatalf("marshal export channel payload: %v", err)
	}

	if payload.Tokens != estimateOpenAITextTokens(string(encoded)) {
		t.Fatalf("expected stamped total to match file bytes, got %+v", payload)
	}
}

func TestTrimExportChannelPayloadEnforcesFileLimit(t *testing.T) {
	t.Parallel()

	newest := exportChannelMessage{
		Username:  "alice",
		Content:   "newest hello",
		Timestamp: "2026-09-30T12:05:00Z",
	}
	newest.tokens = estimateExportMessageTokens(newest)

	oldest := exportChannelMessage{
		Username:  "bob",
		Content:   "oldest hello",
		Timestamp: "2026-09-30T12:00:00Z",
	}
	oldest.tokens = estimateExportMessageTokens(oldest)

	probe := buildExportChannelPayload("channel-1", 100000, []exportChannelMessage{newest}, 0)
	if _, err := marshalExportChannelPayload(&probe); err != nil {
		t.Fatalf("marshal probe payload: %v", err)
	}

	// Limit fits the newest message but not both: the trim must drop the
	// oldest and keep the stamped file within budget.
	limit := probe.Tokens

	trimmed, payload, encoded := trimExportChannelPayload("channel-1", limit, []exportChannelMessage{newest, oldest})

	if len(trimmed) != 1 || trimmed[0].Content != "newest hello" {
		t.Fatalf("expected oldest-first trim to keep newest, got %+v", trimmed)
	}

	if payload.Tokens > limit || estimateOpenAITextTokens(string(encoded)) > limit {
		t.Fatalf("expected stamped file within limit %d, got %+v", limit, payload)
	}
}

func TestEstimateOpenAITextTokens(t *testing.T) {
	t.Parallel()

	if estimateOpenAITextTokens("") != 0 || estimateOpenAITextTokens("   ") != 0 {
		t.Fatal("expected blank text to cost zero tokens")
	}

	if estimateOpenAITextTokens("a") != 1 {
		t.Fatalf("expected short text to floor at one token, got %d", estimateOpenAITextTokens("a"))
	}

	if estimateOpenAITextTokens("abcdefgh") != 2 {
		t.Fatalf("expected eight characters to cost two tokens, got %d", estimateOpenAITextTokens("abcdefgh"))
	}

	entry := exportChannelMessage{
		Username:  "alice",
		Content:   "hello",
		Timestamp: "2026-09-30T12:00:00Z",
	}

	stampedTokens := estimateExportMessageTokens(entry)

	encoded, err := json.MarshalIndent(entry, "    ", "  ")
	if err != nil {
		t.Fatalf("marshal export entry: %v", err)
	}

	if stampedTokens != estimateOpenAITextTokens(string(encoded)+",") {
		t.Fatal("expected message estimate to cover the full serialized JSON entry")
	}

	contentOnly := estimateOpenAITextTokens("hello") + estimateOpenAITextTokens("alice")
	if estimateExportMessageTokens(entry) <= contentOnly {
		t.Fatal("expected JSON keys and metadata to cost more than content plus username")
	}
}

func TestExportChannelEntryRejectsNonUserMessages(t *testing.T) {
	t.Parallel()

	if _, ok := exportChannelEntry(nil); ok {
		t.Fatal("expected nil message to be rejected")
	}

	if _, ok := exportChannelEntry(&discordgo.Message{}); ok {
		t.Fatal("expected unattributed message to be rejected")
	}

	botMessage := newExportChannelMessage("bot-1", "bot-id", "helper", "bot reply", true, time.Now().UTC())
	if _, ok := exportChannelEntry(botMessage); ok {
		t.Fatal("expected bot message to be rejected")
	}
}

func TestBuildExportProgressBar(t *testing.T) {
	t.Parallel()

	got := buildExportProgressBar(0, exportChannelProgressBarWidth)
	want := "[" + strings.Repeat("░", exportChannelProgressBarWidth) + "]"

	if got != want {
		t.Fatalf("unexpected empty bar: %q", got)
	}

	if got := buildExportProgressBar(1, 4); got != "[████]" {
		t.Fatalf("unexpected full bar: %q", got)
	}

	if got := buildExportProgressBar(0.5, 4); got != "[██░░]" {
		t.Fatalf("unexpected half bar: %q", got)
	}

	if got := buildExportProgressBar(2, 4); got != "[████]" {
		t.Fatalf("expected overflow to clamp full, got %q", got)
	}

	if got := buildExportProgressBar(0.5, 0); got != "[]" {
		t.Fatalf("expected empty width to render empty brackets, got %q", got)
	}
}

func TestFormatExportRemaining(t *testing.T) {
	t.Parallel()

	if got := formatExportRemaining(-time.Second); got != "0s" {
		t.Fatalf("expected negative duration to clamp, got %q", got)
	}

	if got := formatExportRemaining(45 * time.Second); got != "45s" {
		t.Fatalf("unexpected seconds format: %q", got)
	}

	if got := formatExportRemaining(90 * time.Second); got != "1m30s" {
		t.Fatalf("unexpected minutes format: %q", got)
	}
}

func TestFormatExportProgressContent(t *testing.T) {
	t.Parallel()

	progress := exportChannelProgress{kept: 3, scanned: 10, tokens: 50}
	content := formatExportProgressContent(progress, "channel-1", 100)

	fragments := []string{"Exporting <#channel-1>", "10 scanned", "3 kept", "50/100 tokens", "50%", "remaining"}
	for _, fragment := range fragments {
		if !strings.Contains(content, fragment) {
			t.Fatalf("expected fragment %q in progress content %q", fragment, content)
		}
	}
}

func TestHandleExportChannelCommandUploadsJSON(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	second := time.Date(2026, time.September, 30, 12, 5, 0, 0, time.UTC)

	newestUser := newExportChannelMessage("newest", "user-1", "alice", "newest hello", false, second)
	botMessage := newExportChannelMessage("bot-1", "bot-id", "helper", "bot reply", true, second)
	oldestUser := newExportChannelMessage("oldest", "user-2", "bob", "oldest hello", false, first)

	pages := [][]*discordgo.Message{{newestUser, botMessage}, {oldestUser}}

	var capture exportChannelUploadCapture

	session := newExportChannelHistorySession(t, pages, &capture)

	interaction := newExportChannelCommandInteraction("channel-1", 100000, maintenanceOwnerID)

	instance := new(bot)
	instance.session = session

	err := instance.handleExportChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle export channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &capture.deferred)

	if len(capture.edits) == 0 {
		t.Fatal("expected progress bar edits before the final upload")
	}

	if !strings.Contains(capture.edits[0], "Exporting <#channel-1>") || !strings.Contains(capture.edits[0], "[") {
		t.Fatalf("expected countdown progress bar, got %q", capture.edits[0])
	}

	finalContent := capture.edits[len(capture.edits)-1]
	if !strings.Contains(finalContent, "Exported 2 user messages") {
		t.Fatalf("expected export summary, got %q", finalContent)
	}

	if capture.filename != "export-channel-1.json" {
		t.Fatalf("unexpected export filename: %q", capture.filename)
	}

	var payload exportChannelPayload
	if err := json.Unmarshal([]byte(capture.fileContents), &payload); err != nil {
		t.Fatalf("decode export JSON: %v", err)
	}

	if payload.ChannelID != "channel-1" || payload.Order != exportChannelOrder || payload.MessageCount != 2 {
		t.Fatalf("unexpected export payload header: %+v", payload)
	}

	if len(payload.Messages) != 2 ||
		payload.Messages[0].Content != "newest hello" ||
		payload.Messages[1].Content != "oldest hello" {
		t.Fatalf("expected newest-to-oldest user messages, got %+v", payload.Messages)
	}

	if payload.Messages[0].Username != "alice" {
		t.Fatalf("expected username attribution, got %+v", payload.Messages[0])
	}

	assertExportMessagesOnlyIncludeKeys(t, capture.fileContents)

	if payload.Messages[0].Timestamp != second.UTC().Format(time.RFC3339) {
		t.Fatalf("expected message timestamp, got %+v", payload.Messages[0])
	}

	if payload.Tokens != estimateOpenAITextTokens(capture.fileContents) {
		t.Fatalf("expected token total to match final JSON file, got %+v", payload)
	}
}

func assertExportMessagesOnlyIncludeKeys(t *testing.T, fileContents string) {
	t.Helper()

	var rawPayload struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(fileContents), &rawPayload); err != nil {
		t.Fatalf("decode raw export payload: %v", err)
	}

	for index, msg := range rawPayload.Messages {
		if len(msg) != 3 {
			t.Fatalf("expected message %d to have exactly 3 keys, got %d: %+v", index, len(msg), msg)
		}

		for _, key := range []string{"username", "content", "timestamp"} {
			if _, ok := msg[key]; !ok {
				t.Fatalf("message %d missing expected key %q: %+v", index, key, msg)
			}
		}
	}
}

func TestHandleApplicationCommandInteractionRoutesExport(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	pages := [][]*discordgo.Message{{newExportChannelMessage("only", "user-1", "alice", "hello", false, first)}}

	var capture exportChannelUploadCapture

	session := newExportChannelHistorySession(t, pages, &capture)
	interaction := newExportChannelCommandInteraction("channel-1", 100000, maintenanceOwnerID)

	instance := new(bot)
	instance.session = session

	err := instance.handleApplicationCommandInteraction(session, interaction)
	if err != nil {
		t.Fatalf("handle application command interaction: %v", err)
	}

	if capture.filename != "export-channel-1.json" {
		t.Fatalf("expected export dispatch to upload JSON, got %q", capture.filename)
	}
}

func TestRunExportChannelRequiresBotSession(t *testing.T) {
	t.Parallel()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	interaction := &discordgo.Interaction{ID: "interaction-id"}

	err = new(bot).runExportChannel(session, interaction, "channel-1", 100)
	if err == nil || !strings.Contains(err.Error(), "without bot session") {
		t.Fatalf("expected missing session error, got %v", err)
	}
}

func TestHandleExportChannelCommandReportsUnknownChannel(t *testing.T) {
	t.Parallel()

	var capture deferredInteractionCapture

	session := newInteractionTestSessionWithTransport(t, exportChannelUnknownChannelRoundTrip(t, &capture))

	interaction := newExportChannelCommandInteraction("channel-1", 100, maintenanceOwnerID)

	instance := new(bot)
	instance.session = session

	err := instance.handleExportChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle export channel command: %v", err)
	}

	assertDeferredInteractionResponse(t, &capture.deferredResponse)

	if !strings.Contains(capture.editedResponse.Content, "was not found") {
		t.Fatalf("expected unknown-channel guidance, got %q", capture.editedResponse.Content)
	}
}

func TestHandleExportChannelCommandReportsMissingAccess(t *testing.T) {
	t.Parallel()

	var capture exportChannelUploadCapture

	session := newExportChannelHistorySession(t, nil, &capture)

	instance := new(bot)
	instance.session = session

	server := &exportChannelTestServer{
		t:             t,
		capture:       &capture,
		channelBody:   `{"id":"channel-1","name":"general"}`,
		historyStatus: http.StatusForbidden,
		historyBody:   `{"message":"Missing Access","code":50001}`,
	}
	session.Client.Transport = roundTripFunc(server.roundTrip)

	interaction := newExportChannelCommandInteraction("channel-1", 100, maintenanceOwnerID)

	err := instance.handleExportChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle export channel command: %v", err)
	}

	if len(capture.edits) == 0 || !strings.Contains(capture.edits[len(capture.edits)-1], "lacks access") {
		t.Fatalf("expected missing-access guidance, got %#v", capture.edits)
	}
}

func TestHandleExportChannelCommandRejectsForumChannel(t *testing.T) {
	t.Parallel()

	var capture deferredInteractionCapture

	session := newInteractionTestSessionWithTransport(t, exportChannelForumRoundTrip(t, &capture))

	interaction := newExportChannelCommandInteraction("channel-1", 100, maintenanceOwnerID)

	instance := new(bot)
	instance.session = session

	err := instance.handleExportChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle export channel command: %v", err)
	}

	if !strings.Contains(capture.editedResponse.Content, "forum channel") {
		t.Fatalf("expected forum guidance, got %q", capture.editedResponse.Content)
	}
}

func TestFetchExportChannelMessagesRetriesTransientPage(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	message := newExportChannelMessage("only", "user-1", "alice", "hello", false, first)

	calls := 0

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.Client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		calls++

		// 500 (not 502) bypasses discordgo's internal 502 retry, so the
		// first page attempt surfaces to loadExportChannelPage, which
		// retries once with backoff before succeeding.
		if calls == 1 {
			return newInteractionJSONResponse(request, http.StatusInternalServerError, `{"message":"Server Error"}`), nil
		}

		if calls == 2 {
			body, marshalErr := json.Marshal([]*discordgo.Message{message})
			if marshalErr != nil {
				t.Fatalf("marshal history page: %v", marshalErr)
			}

			return newInteractionJSONResponse(request, http.StatusOK, string(body)), nil
		}

		return newInteractionJSONResponse(request, http.StatusOK, `[]`), nil
	})

	messages, _, err := fetchExportChannelMessages(session, "channel-1", 100000, nil)
	if err != nil {
		t.Fatalf("fetch export channel messages: %v", err)
	}

	// 1 failed page + 1 successful page + 1 empty tail page: the middle
	// call proves the outer retry loop, not discordgo internals.
	if calls != 3 {
		t.Fatalf("expected outer page retry, got %d calls", calls)
	}

	if len(messages) != 1 || messages[0].Content != "hello" {
		t.Fatalf("expected retried page kept, got %+v", messages)
	}
}

func TestHandleExportChannelCommandUnwrapsChannelMention(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	pages := [][]*discordgo.Message{{newExportChannelMessage("only", "user-1", "alice", "hello", false, first)}}

	var capture exportChannelUploadCapture

	session := newExportChannelHistorySession(t, pages, &capture)
	interaction := newExportChannelCommandInteraction("<#channel-1>", 100000, maintenanceOwnerID)

	instance := new(bot)
	instance.session = session

	err := instance.handleExportChannelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle export channel command: %v", err)
	}

	if capture.filename != "export-channel-1.json" {
		t.Fatalf("expected mention to resolve to channel-1, got %q", capture.filename)
	}
}

func TestNormalizeExportChannelID(t *testing.T) {
	t.Parallel()

	if got := normalizeExportChannelID("  <#channel-1>  "); got != "channel-1" {
		t.Fatalf("expected mention unwrapped, got %q", got)
	}

	if got := normalizeExportChannelID("  channel-1  "); got != "channel-1" {
		t.Fatalf("expected whitespace trimmed, got %q", got)
	}
}

func TestDescribeExportChannelError(t *testing.T) {
	t.Parallel()

	unknownChannel := newDiscordRESTError(discordgo.ErrCodeUnknownChannel, "Unknown Channel")
	if got := describeExportChannelError("channel-1", unknownChannel); !strings.Contains(got, "was not found") {
		t.Fatalf("expected unknown-channel guidance, got %q", got)
	}

	missingAccess := newDiscordRESTError(discordgo.ErrCodeMissingAccess, "Missing Access")
	if got := describeExportChannelError("channel-1", missingAccess); !strings.Contains(got, "lacks access") {
		t.Fatalf("expected missing-access guidance, got %q", got)
	}

	missingPermissions := newDiscordRESTError(discordgo.ErrCodeMissingPermissions, "Missing Permissions")
	if got := describeExportChannelError("channel-1", missingPermissions); !strings.Contains(got, "lacks access") {
		t.Fatalf("expected missing-permissions guidance, got %q", got)
	}

	rateLimited := &discordgo.RateLimitError{RateLimit: &discordgo.RateLimit{}}
	if got := describeExportChannelError("channel-1", rateLimited); !strings.Contains(got, "rate-limited") {
		t.Fatalf("expected rate-limit guidance, got %q", got)
	}

	wrapped := fmt.Errorf("load channel messages for export: %w", missingAccess)
	if got := describeExportChannelError("channel-1", wrapped); !strings.Contains(got, "lacks access") {
		t.Fatalf("expected wrapped REST error to map, got %q", got)
	}

	other := newDiscordRESTError(discordgo.ErrCodeUnknownMessage, "Unknown Message")
	if got := describeExportChannelError("channel-1", other); !strings.Contains(got, "Discord says") {
		t.Fatalf("expected raw Discord message surfaced, got %q", got)
	}

	got := describeExportChannelError("channel-1", errUnexpectedTestRequest)
	if !strings.Contains(got, "Failed to export channel `channel-1`") {
		t.Fatalf("expected fallback to keep transport detail, got %q", got)
	}

	if !strings.Contains(got, "unexpected test request") {
		t.Fatalf("expected fallback to keep transport detail, got %q", got)
	}

	if got := describeExportChannelError("channel-1", nil); got != "Failed to export channel `channel-1`." {
		t.Fatalf("expected nil fallback, got %q", got)
	}
}

func TestValidateExportChannel(t *testing.T) {
	t.Parallel()

	if got := validateExportChannel(nil, "channel-1"); !strings.Contains(got, "was not found") {
		t.Fatalf("expected nil channel to report not found, got %q", got)
	}

	for channelType, label := range map[discordgo.ChannelType]string{
		discordgo.ChannelTypeGuildCategory:   "category",
		discordgo.ChannelTypeGuildVoice:      "voice channel",
		discordgo.ChannelTypeGuildStageVoice: "stage channel",
		discordgo.ChannelTypeGuildForum:      "forum channel",
		discordgo.ChannelTypeGuildMedia:      "media channel",
	} {
		channel := &discordgo.Channel{ID: "channel-1", Type: channelType}
		if got := validateExportChannel(channel, "channel-1"); !strings.Contains(got, label) {
			t.Fatalf("expected %s guidance, got %q", label, got)
		}
	}

	for _, channelType := range []discordgo.ChannelType{
		discordgo.ChannelTypeGuildText,
		discordgo.ChannelTypeDM,
		discordgo.ChannelTypeGuildPublicThread,
	} {
		channel := &discordgo.Channel{ID: "channel-1", Type: channelType}
		if got := validateExportChannel(channel, "channel-1"); got != "" {
			t.Fatalf("expected text-like channel accepted, got %q", got)
		}
	}
}

func TestIsRetryableExportChannelError(t *testing.T) {
	t.Parallel()

	if !isRetryableExportChannelError(&discordgo.RateLimitError{RateLimit: &discordgo.RateLimit{}}) {
		t.Fatal("expected rate limit error to be retryable")
	}

	retryableErr := &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusBadGateway}}
	if !isRetryableExportChannelError(retryableErr) {
		t.Fatal("expected 5xx REST error to be retryable")
	}

	fatalErr := newDiscordRESTError(discordgo.ErrCodeUnknownChannel, "Unknown Channel")
	if isRetryableExportChannelError(fatalErr) {
		t.Fatal("expected unknown channel to be fatal")
	}

	exhausted := fmt.Errorf("load channel messages for export: %w", errExceededExportMaxRetries)
	if !isRetryableExportChannelError(exhausted) {
		t.Fatal("expected exhausted discordgo 502 retry to stay retryable")
	}

	if !isRetryableExportChannelError(io.EOF) {
		t.Fatal("expected EOF to be retryable")
	}

	if isRetryableExportChannelError(errUnexpectedTestRequest) {
		t.Fatal("expected opaque error to be fatal")
	}
}

func TestDecodeExportChannelMessagesToleratesUnknownComponent(t *testing.T) {
	t.Parallel()

	body := `[{
		"id": "msg-1",
		"channel_id": "channel-1",
		"author": {"id": "user-1", "username": "alice"},
		"content": "hello",
		"timestamp": "2026-09-30T12:00:00Z",
		"components": [{"type": 20, "id": 1, "label": "future"}]
	}]`

	messages, err := decodeExportChannelMessages([]byte(body))
	if err != nil {
		t.Fatalf("decode export channel messages: %v", err)
	}

	if len(messages) != 1 || messages[0].ID != "msg-1" || messages[0].Content != "hello" {
		t.Fatalf("expected tolerant decode to keep message fields, got %+v", messages)
	}

	if messages[0].Author == nil || messages[0].Author.Username != "alice" {
		t.Fatalf("expected author preserved, got %+v", messages[0].Author)
	}
}

func TestDecodeExportChannelMessagesKeepsKnownComponents(t *testing.T) {
	t.Parallel()

	body := `[{
		"id": "msg-1",
		"channel_id": "channel-1",
		"author": {"id": "user-1", "username": "alice"},
		"content": "hello",
		"timestamp": "2026-09-30T12:00:00Z",
		"components": [{"type": 1, "components": [{"type": 2, "style": 1, "label": "ok", "custom_id": "ok"}]}]
	}]`

	messages, err := decodeExportChannelMessages([]byte(body))
	if err != nil {
		t.Fatalf("decode export channel messages: %v", err)
	}

	if len(messages) != 1 || len(messages[0].Components) != 1 {
		t.Fatalf("expected known components preserved, got %+v", messages)
	}
}

func TestDecodeExportChannelMessagesRejectsGarbage(t *testing.T) {
	t.Parallel()

	if _, err := decodeExportChannelMessages([]byte(`not json`)); err == nil {
		t.Fatal("expected invalid page to fail")
	}

	if _, err := decodeExportChannelMessages([]byte(`[{"id":}`)); err == nil {
		t.Fatal("expected invalid message to fail")
	}
}

func TestDecodeExportChannelMessagesToleratesNestedUnknownComponent(t *testing.T) {
	t.Parallel()

	body := `[{
		"id": "msg-1",
		"channel_id": "channel-1",
		"author": {"id": "user-1", "username": "alice"},
		"content": "hello",
		"timestamp": "2026-09-30T12:00:00Z",
		"referenced_message": {
			"id": "msg-0",
			"channel_id": "channel-1",
			"author": {"id": "user-2", "username": "bob"},
			"content": "parent",
			"timestamp": "2026-09-30T11:00:00Z",
			"components": [{"type": 20, "id": 2, "label": "future"}]
		}
	}]`

	messages, err := decodeExportChannelMessages([]byte(body))
	if err != nil {
		t.Fatalf("decode export channel messages: %v", err)
	}

	if len(messages) != 1 || messages[0].ReferencedMessage == nil || messages[0].ReferencedMessage.Content != "parent" {
		t.Fatalf("expected nested message preserved, got %+v", messages)
	}
}

func TestDecodeExportChannelMessagesToleratesSnapshotUnknownComponent(t *testing.T) {
	t.Parallel()

	body := `[{
		"id": "msg-1",
		"channel_id": "channel-1",
		"author": {"id": "user-1", "username": "alice"},
		"content": "hello",
		"timestamp": "2026-09-30T12:00:00Z",
		"message_snapshots": [{
			"message": {
				"id": "msg-0",
				"channel_id": "channel-1",
				"author": {"id": "user-2", "username": "bob"},
				"content": "forwarded",
				"timestamp": "2026-09-30T11:00:00Z",
				"components": [{"type": 20, "id": 3, "label": "future"}]
			}
		}]
	}]`

	messages, err := decodeExportChannelMessages([]byte(body))
	if err != nil {
		t.Fatalf("decode export channel messages: %v", err)
	}

	if len(messages) != 1 || len(messages[0].MessageSnapshots) != 1 {
		t.Fatalf("expected snapshot preserved, got %+v", messages)
	}

	if messages[0].MessageSnapshots[0].Message == nil || messages[0].MessageSnapshots[0].Message.Content != "forwarded" {
		t.Fatalf("expected inner snapshot message preserved, got %+v", messages[0].MessageSnapshots[0])
	}
}

func TestSyncCommandsRegistersExportCommand(t *testing.T) {
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

	for _, expectedName := range []string{exportChannelCommandName} {
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
func TestIsExpiredInteractionTokenErrorDetectsWebhookExpiry(t *testing.T) {
	t.Parallel()

	expired := fmt.Errorf(
		"edit interaction response: %w",
		newDiscordRESTError(discordInvalidWebhookTokenCode, "Invalid Webhook Token"),
	)

	if !isExpiredInteractionTokenError(expired) {
		t.Fatal("expected wrapped 50027 edit failure to count as expired")
	}

	unknown := newDiscordRESTError(discordUnknownInteractionCode, "Unknown interaction")
	if !isExpiredInteractionTokenError(unknown) {
		t.Fatal("expected 10062 to keep counting as expired")
	}

	other := newDiscordRESTError(discordgo.ErrCodeUnknownChannel, "Unknown Channel")
	if isExpiredInteractionTokenError(other) {
		t.Fatal("expected unrelated REST code to stay live")
	}
}

func TestEditExportProgressReportsExpiredToken(t *testing.T) {
	t.Parallel()

	var capture deferredInteractionCapture

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/callback") {
			return captureDeferredInteractionRequest(t, request, &capture.deferredResponse)
		}

		if request.Method == http.MethodPatch && strings.HasSuffix(request.URL.Path, "/messages/@original") {
			return newInteractionJSONResponse(
				request,
				http.StatusUnauthorized,
				`{"message":"Invalid Webhook Token","code":50027}`,
			), nil
		}

		t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

		return nil, errUnexpectedTestRequest
	})

	session := newInteractionTestSessionWithTransport(t, transport)

	interaction := &discordgo.Interaction{ID: "interaction-id", AppID: "application-id", Token: "interaction-token"}

	err := new(bot).editExportProgress(
		session,
		interaction,
		exportChannelProgress{kept: 1, scanned: 2, tokens: 3},
		"channel-1",
		100,
	)
	if !isExpiredInteractionTokenError(err) {
		t.Fatalf("expected expired token error, got %v", err)
	}
}

func TestFetchExportWithProgressStopsEditingAfterTokenDeath(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	second := time.Date(2026, time.September, 30, 12, 5, 0, 0, time.UTC)
	third := time.Date(2026, time.September, 30, 12, 10, 0, 0, time.UTC)

	firstMessage := newExportChannelMessage("newest", "user-1", "alice", "newest hello", false, third)
	secondMessage := newExportChannelMessage("middle", "user-2", "bob", "middle hello", false, second)
	thirdMessage := newExportChannelMessage("oldest", "user-3", "carol", "oldest hello", false, first)

	var capture exportChannelUploadCapture
	captureEdits := 0

	server := &exportChannelTestServer{
		t:           t,
		capture:     &capture,
		channelBody: `{"id":"channel-1","name":"general"}`,
	}
	server.remaining = [][]*discordgo.Message{{firstMessage}, {secondMessage}, {thirdMessage}}

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/callback") {
			return captureDeferredInteractionRequest(t, request, &capture.deferred)
		}

		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/channels/channel-1/messages") {
			return server.serveHistory(request)
		}

		if request.Method == http.MethodPatch && strings.HasSuffix(request.URL.Path, "/messages/@original") {
			captureEdits++

			return newInteractionJSONResponse(
				request,
				http.StatusUnauthorized,
				`{"message":"Invalid Webhook Token","code":50027}`,
			), nil
		}

		t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

		return nil, errUnexpectedTestRequest
	})

	session := newInteractionTestSessionWithTransport(t, transport)

	interaction := &discordgo.Interaction{ID: "interaction-id", AppID: "application-id", Token: "interaction-token"}

	messages, _, outcome := new(bot).fetchExportWithProgress(session, interaction, "channel-1", 100000)
	if outcome.err != nil {
		t.Fatalf("fetch export with progress: %v", outcome.err)
	}

	if !outcome.tokenDead {
		t.Fatal("expected dead token flag after 50027 progress edit")
	}

	if len(messages) != 3 {
		t.Fatalf("expected history fetch to continue past expiry, got %+v", messages)
	}

	if captureEdits != 1 {
		t.Fatalf("expected exactly one dying edit and no retries, got %d", captureEdits)
	}
}

func TestFetchExportWithProgressKeepsEditingAfterTransientEditFailure(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	second := time.Date(2026, time.September, 30, 12, 5, 0, 0, time.UTC)
	third := time.Date(2026, time.September, 30, 12, 10, 0, 0, time.UTC)

	firstMessage := newExportChannelMessage("newest", "user-1", "alice", "newest hello", false, third)
	secondMessage := newExportChannelMessage("middle", "user-2", "bob", "middle hello", false, second)
	thirdMessage := newExportChannelMessage("oldest", "user-3", "carol", "oldest hello", false, first)

	var capture exportChannelUploadCapture

	captureEdits := 0

	server := &exportChannelTestServer{
		t:           t,
		capture:     &capture,
		channelBody: `{"id":"channel-1","name":"general"}`,
	}
	server.remaining = [][]*discordgo.Message{{firstMessage}, {secondMessage}, {thirdMessage}}

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/callback") {
			return captureDeferredInteractionRequest(t, request, &capture.deferred)
		}

		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/channels/channel-1/messages") {
			return server.serveHistory(request)
		}

		if request.Method == http.MethodPatch && strings.HasSuffix(request.URL.Path, "/messages/@original") {
			captureEdits++

			if captureEdits == 1 {
				return newInteractionJSONResponse(
					request,
					http.StatusTooManyRequests,
					`{"message":"You are being rate limited.","code":0,"retry_after":0.1}`,
				), nil
			}

			return newInteractionJSONResponse(request, http.StatusOK, `{}`), nil
		}

		t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

		return nil, errUnexpectedTestRequest
	})

	session := newInteractionTestSessionWithTransport(t, transport)

	interaction := &discordgo.Interaction{ID: "interaction-id", AppID: "application-id", Token: "interaction-token"}

	messages, _, outcome := new(bot).fetchExportWithProgress(session, interaction, "channel-1", 100000)
	if outcome.err != nil {
		t.Fatalf("fetch export with progress: %v", outcome.err)
	}

	if outcome.tokenDead {
		t.Fatal("expected transient progress edit failure to keep the token alive")
	}

	if len(messages) != 3 {
		t.Fatalf("expected history fetch to continue past transient failure, got %+v", messages)
	}

	if captureEdits < 2 {
		t.Fatalf("expected progress edits to continue after transient failure, got %d", captureEdits)
	}
}

func TestSendExportChannelFileFallsBackToChannelMessage(t *testing.T) {
	t.Parallel()

	fallbackBodies := []string{}
	fallbackFiles := 0
	editCalls := 0

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPatch && strings.HasSuffix(request.URL.Path, "/messages/@original"):
			editCalls++

			return newInteractionJSONResponse(
				request,
				http.StatusUnauthorized,
				`{"message":"Invalid Webhook Token","code":50027}`,
			), nil
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/channels/guild-channel/messages"):
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				t.Fatalf("read fallback message body: %v", readErr)
			}

			fallbackBodies = append(fallbackBodies, string(body))
			fallbackFiles++

			return newInteractionJSONResponse(
				request,
				http.StatusOK,
				`{"id":"fallback-message","channel_id":"guild-channel"}`,
			), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	})

	session := newInteractionTestSessionWithTransport(t, transport)

	interaction := &discordgo.Interaction{
		ID:        "interaction-id",
		AppID:     "application-id",
		Token:     "interaction-token",
		ChannelID: "guild-channel",
	}
	interaction.Member = &discordgo.Member{User: &discordgo.User{ID: "invoker-id"}}

	exportJSON := []byte(`{"messages":[]}`)

	err := sendExportChannelFile(
		session,
		interaction,
		"channel-1",
		[]exportChannelMessage{{Username: "alice", Content: "hi"}},
		10,
		100,
		exportJSON,
		false,
	)
	if err != nil {
		t.Fatalf("send export channel file: %v", err)
	}

	if editCalls != 1 {
		t.Fatalf("expected one interaction edit attempt, got %d", editCalls)
	}

	if fallbackFiles != 1 || len(fallbackBodies) != 1 {
		t.Fatalf(
			"expected one fallback channel message, got %d uploads in %d bodies",
			fallbackFiles,
			len(fallbackBodies),
		)
	}

	if !strings.Contains(fallbackBodies[0], "Exported 1 user messages") ||
		!strings.Contains(fallbackBodies[0], "invoker-id") {
		t.Fatalf("expected fallback to carry summary and invoker mention, got %q", fallbackBodies[0])
	}
}

func TestFinishExportChannelReplySkipsDeadTokenEdits(t *testing.T) {
	t.Parallel()

	fallbackCalls := 0

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/channels/guild-channel/messages") {
			fallbackCalls++

			return newInteractionJSONResponse(
				request,
				http.StatusOK,
				`{"id":"fallback-message","channel_id":"guild-channel"}`,
			), nil
		}

		t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

		return nil, errUnexpectedTestRequest
	})

	session := newInteractionTestSessionWithTransport(t, transport)

	interaction := &discordgo.Interaction{
		ID:        "interaction-id",
		AppID:     "application-id",
		Token:     "interaction-token",
		ChannelID: "guild-channel",
	}

	err := finishExportChannelReply(
		session,
		interaction,
		"channel-1",
		"No user messages fit within the token limit.",
		true,
	)
	if err != nil {
		t.Fatalf("finish export channel reply: %v", err)
	}

	if fallbackCalls != 1 {
		t.Fatalf("expected dead-token reply to go straight to the channel, got %d calls", fallbackCalls)
	}
}
