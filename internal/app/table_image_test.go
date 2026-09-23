package app

import (
	"bytes"
	"context"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestParseMarkdownTableBlocksFindsSimpleTable(t *testing.T) {
	t.Parallel()

	text := "Intro.\n\n| Name | Age |\n| --- | ---: |\n| Ada | 36 |\n| Grace | 85 |\n\nOutro."

	blocks := parseMarkdownTableBlocks(text)
	if len(blocks) != 1 {
		t.Fatalf("expected one table block, got %d", len(blocks))
	}

	if len(blocks[0].table.header) != 2 || len(blocks[0].table.rows) != 2 {
		t.Fatalf("unexpected table shape: %#v", blocks[0].table)
	}

	if extracted := text[blocks[0].start:blocks[0].end]; !strings.Contains(extracted, "Grace") {
		t.Fatalf("unexpected block offsets: %q", extracted)
	}
}

func TestParseMarkdownTableBlocksRejectsNonTables(t *testing.T) {
	t.Parallel()

	for _, text := range []string{
		"no pipes here",
		"| lonely | header |\nnot a delimiter",
		"| a | b |\n| --- | --- |\n",
	} {
		if blocks := parseMarkdownTableBlocks(text); len(blocks) != 0 {
			t.Fatalf("expected no table in %q, got %#v", text, blocks)
		}
	}
}

func TestRenderMarkdownTablePNGProducesImage(t *testing.T) {
	t.Parallel()

	table := markdownTable{
		header: []string{"Name", "Age"},
		rows:   [][]string{{"Ada Lovelace", "36"}, {"Grace Hopper", "85"}},
	}

	imageBytes, err := renderMarkdownTablePNG(table)
	if err != nil {
		t.Fatalf("render table png: %v", err)
	}

	decoded, err := png.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		t.Fatalf("decode table png: %v", err)
	}

	if decoded.Bounds().Dx() < 100 || decoded.Bounds().Dy() < 20 {
		t.Fatalf("unexpected table image bounds: %v", decoded.Bounds())
	}
}

func TestRenderMarkdownTablePNGCoversAllFallbackScripts(t *testing.T) {
	t.Parallel()

	missing := make([]string, 0, len(tableImageFallbackFontOrder))

	for _, name := range tableImageFallbackFontOrder {
		if _, ok := firstTableImageFontPath(tableImageSystemFontDir, name); !ok {
			missing = append(missing, name)
		}
	}

	if len(missing) != 0 {
		t.Fatalf("missing fallback fonts for full script coverage: %v", missing)
	}

	table := markdownTable{
		header: []string{"Script", "Sample"},
		rows: [][]string{
			{"Arabic", "مرحبا"},
			{"Hebrew", "שלום"},
			{"Thai", "สวัสดี"},
			{"Hindi", "नमस्ते"},
			{"Bengali", "হ্যালো"},
			{"Tamil", "வணக்கம்"},
			{"Telugu", "హలో"},
			{"Chinese", "你好"},
			{"Japanese", "こんにちは"},
			{"Korean", "안녕하세요"},
		},
	}

	imageBytes, err := renderMarkdownTablePNG(table)
	if err != nil {
		t.Fatalf("render multilingual table png: %v", err)
	}

	if _, err := png.Decode(bytes.NewReader(imageBytes)); err != nil {
		t.Fatalf("decode multilingual table png: %v", err)
	}

	fonts, err := loadTableImageFonts()
	if err != nil {
		t.Fatalf("load table fonts: %v", err)
	}

	if len(fonts.fallbacks) == 0 {
		t.Fatal("expected curated fallback faces for full script coverage")
	}
}

func TestRenderFinalResponseSendsTableImagesOnce(t *testing.T) {
	t.Parallel()

	const answerText = "Tables:\n\n| A | B |\n| --- | --- |\n| 1 | 2 |\n\n| C | D |\n| --- | --- |\n| 3 | 4 |"

	fixture := newTableImageHistoryFixture(t, answerText)

	for range 2 {
		if err := fixture.instance.renderFinalResponse(
			context.Background(),
			fixture.tracker,
			nil,
			fixture.accumulator,
			"",
			finishReasonStop,
		); err != nil {
			t.Fatalf("render final response: %v", err)
		}
	}

	if *fixture.tablePosts != 2 {
		t.Fatalf("expected one reply per table without resending on retry, got %d", *fixture.tablePosts)
	}
}
func TestRenderFinalResponseSendsTableImageWithoutTouchingHistory(t *testing.T) {
	t.Parallel()

	const answerText = "Here is the table:\n\n| Name | Age |\n| --- | --- |\n| Ada | 36 |\n\nDone."

	fixture := newTableImageHistoryFixture(t, answerText)

	if err := fixture.instance.renderFinalResponse(
		context.Background(),
		fixture.tracker,
		nil,
		fixture.accumulator,
		"",
		finishReasonStop,
	); err != nil {
		t.Fatalf("render final response: %v", err)
	}

	assertTableImageHistory(t, fixture, answerText)
}

type tableImageHistoryFixture struct {
	instance    *bot
	tracker     *responseTracker
	accumulator *segmentAccumulator
	tablePosts  *int
	source      *discordgo.Message
	response    *discordgo.Message
	tableReply  *discordgo.Message
}

func newTableImageHistoryFixture(t *testing.T, answerText string) tableImageHistoryFixture {
	t.Helper()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		tableMessageID     = "assistant-message-2"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	responseMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)
	tableMessage := newAssistantReplyMessage(
		tableMessageID,
		newDiscordUser(botUserID, true),
		responseMessage,
	)

	tablePosts := 0

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.State.User = newDiscordUser(botUserID, true)

	channel := new(discordgo.Channel)
	channel.ID = channelID
	channel.Type = discordgo.ChannelTypeDM

	if err := session.State.ChannelAdd(channel); err != nil {
		t.Fatalf("add channel to state: %v", err)
	}

	session.Client = new(http.Client)
	session.Client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodPost ||
			request.URL.Path != "/api/v9/channels/"+channelID+"/messages" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
		}

		if strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/form-data") {
			tablePosts++

			return newJSONResponse(t, request, tableMessage), nil
		}

		return newJSONResponse(t, request, responseMessage), nil
	})

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)

	tracker := newResponseTracker(sourceMessage, "openai/gpt-5")

	accumulator := newSegmentAccumulator(embedResponseMaxLength)
	_ = accumulator.appendText(answerText)

	return tableImageHistoryFixture{
		instance:    instance,
		tracker:     tracker,
		accumulator: &accumulator,
		tablePosts:  &tablePosts,
		source:      sourceMessage,
		response:    responseMessage,
		tableReply:  tableMessage,
	}
}

func assertTableImageHistory(t *testing.T, fixture tableImageHistoryFixture, answerText string) {
	t.Helper()

	if *fixture.tablePosts != 1 {
		t.Fatalf("expected one table image reply, got %d", *fixture.tablePosts)
	}

	fixture.tracker.release(fixture.instance.nodes, answerText, "")

	followUpMessage := newFollowUpReplyMessage(
		"user-message-2",
		"channel-1",
		"user-1",
		fixture.tableReply,
	)

	conversation, warnings := fixture.instance.buildConversation(
		context.Background(),
		followUpMessage,
		messageContentOptions{},
		defaultMaxMessages,
		false,
		false,
	)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if len(conversation) != 3 {
		t.Fatalf("unexpected conversation length: %#v", conversation)
	}

	if conversation[1].Role != messageRoleAssistant || conversation[1].Content != answerText {
		t.Fatalf("table image reply leaked into history: %#v", conversation[1])
	}
}
