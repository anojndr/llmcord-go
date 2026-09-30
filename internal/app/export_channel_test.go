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

	return func(request *http.Request) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/callback"):
			return captureDeferredInteractionRequest(t, request, &capture.deferredResponse)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v9/channels/channel-1":
			body := `{"message":"Unknown Channel","code":10003}`

			return newInteractionJSONResponse(request, http.StatusNotFound, body), nil
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

	oldestTokens := estimateExportMessageTokens("bob", "oldest hello")
	newestTokens := estimateExportMessageTokens("alice", "newest hello")

	onPageCalls := 0

	onPage := func(_, _, _ int) {
		onPageCalls++
	}

	messages, tokens, err := fetchExportChannelMessages(session, "channel-1", newestTokens+oldestTokens, onPage)
	if err != nil {
		t.Fatalf("fetch export channel messages: %v", err)
	}

	if onPageCalls != 2 {
		t.Fatalf("expected progress callback per page, got %d calls", onPageCalls)
	}

	if tokens != newestTokens+oldestTokens {
		t.Fatalf("unexpected token total: got %d want %d", tokens, newestTokens+oldestTokens)
	}

	if len(messages) != 2 {
		t.Fatalf("expected only user messages, got %+v", messages)
	}

	if messages[0].ID != "newest" || messages[1].ID != "oldest" {
		t.Fatalf("expected newest-to-oldest order, got %+v", messages)
	}

	if messages[0].Username != "alice" || messages[0].Timestamp != second.UTC().Format(time.RFC3339) {
		t.Fatalf("expected username and timestamp on newest message, got %+v", messages[0])
	}

	// A budget below the newest message stops before including anything.
	messages, tokens, err = fetchExportChannelMessages(session, "channel-1", newestTokens-1, nil)
	if err != nil {
		t.Fatalf("fetch export channel messages with tight budget: %v", err)
	}

	if len(messages) != 0 || tokens != 0 {
		t.Fatalf("expected empty export under budget, got %+v (%d tokens)", messages, tokens)
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

	if estimateExportMessageTokens("alice", "hello") !=
		estimateOpenAITextTokens("hello")+estimateOpenAITextTokens("alice")+exportChannelTokensPerMessage {
		t.Fatal("expected username plus per-message overhead in message estimate")
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

	if len(payload.Messages) != 2 || payload.Messages[0].ID != "newest" || payload.Messages[1].ID != "oldest" {
		t.Fatalf("expected newest-to-oldest user messages, got %+v", payload.Messages)
	}

	if payload.Messages[0].Username != "alice" || payload.Messages[0].AuthorID != "user-1" {
		t.Fatalf("expected username attribution, got %+v", payload.Messages[0])
	}

	if payload.Messages[0].Timestamp != second.UTC().Format(time.RFC3339) {
		t.Fatalf("expected message timestamp, got %+v", payload.Messages[0])
	}

	if payload.Tokens != payload.Messages[0].Tokens+payload.Messages[1].Tokens {
		t.Fatalf("expected token total to match entries: %+v", payload)
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

	wrapped := fmt.Errorf("load channel messages for export: %w", missingAccess)
	if got := describeExportChannelError("channel-1", wrapped); !strings.Contains(got, "lacks access") {
		t.Fatalf("expected wrapped REST error to map, got %q", got)
	}

	other := newDiscordRESTError(discordgo.ErrCodeUnknownMessage, "Unknown Message")
	if got := describeExportChannelError("channel-1", other); !strings.Contains(got, "Discord says") {
		t.Fatalf("expected raw Discord message surfaced, got %q", got)
	}

	got := describeExportChannelError("channel-1", errUnexpectedTestRequest)
	if got != "Failed to export channel `channel-1`." {
		t.Fatalf("expected generic fallback, got %q", got)
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
