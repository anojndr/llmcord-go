package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func seedExportChain(
	instance *bot,
	parentMessage *discordgo.Message,
	replyMessage *discordgo.Message,
	responseText string,
) {
	parentMessage.Content = "what is the export format?"
	parentNode := instance.nodes.getOrCreate(parentMessage.ID)
	parentNode.role = messageRoleUser
	parentNode.text = parentMessage.Content
	parentNode.initialized = true
	parentNode.parentMessage = nil

	replyMessage.ReferencedMessage = parentMessage
	responseNode := instance.nodes.getOrCreate(replyMessage.ID)
	responseNode.role = messageRoleAssistant
	responseNode.text = responseText
	responseNode.thinkingText = "checked the chain"
	responseNode.providerResponseModel = "openai/gpt-test"
	responseNode.searchMetadata = &searchMetadata{
		Queries: []string{"export format"},
		Results: []webSearchResult{{
			Query: "export format",
			Text:  "Title: Example Source\nURL: https://example.com/source\n",
		}},
		MaxURLs:             defaultWebSearchMaxURLs,
		VisualSearchSources: nil,
	}
	responseNode.initialized = true
	responseNode.parentMessage = parentMessage
}

func testExportTime() time.Time {
	return time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
}

func newExportInteractionSession(
	t *testing.T,
	capture *deferredInteractionCapture,
	editedHTML *string,
	editedFilename *string,
) *discordgo.Session {
	t.Helper()

	return newInteractionTestSessionWithTransport(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		capture.requestCount++

		switch capture.requestCount {
		case 1:
			return captureDeferredInteractionRequest(t, request, &capture.deferredResponse)
		case 2:
			contentType := request.Header.Get("Content-Type")
			if !strings.HasPrefix(contentType, "multipart/form-data") {
				t.Fatalf("expected multipart export upload, got content type %q", contentType)
			}

			err := request.ParseMultipartForm(10 * 1024 * 1024)
			if err != nil {
				t.Fatalf("parse multipart export upload: %v", err)
			}

			payloadJSON := request.FormValue("payload_json")

			var parsed discordgo.WebhookEdit
			if payloadErr := json.Unmarshal([]byte(payloadJSON), &parsed); payloadErr != nil {
				t.Fatalf("decode export payload: %v", payloadErr)
			}

			if parsed.Content != nil {
				capture.editedResponse.Content = *parsed.Content
			}

			files := request.MultipartForm.File["files[0]"]
			if len(files) != 1 {
				t.Fatalf("expected one export file, got %d", len(files))
			}

			*editedFilename = files[0].Filename

			uploaded, openErr := files[0].Open()
			if openErr != nil {
				t.Fatalf("open export upload: %v", openErr)
			}

			data, readErr := io.ReadAll(uploaded)
			_ = uploaded.Close()

			if readErr != nil {
				t.Fatalf("read export upload: %v", readErr)
			}

			*editedHTML = string(data)

			return newInteractionJSONResponse(request, http.StatusOK, `{"id":"edited-message"}`), nil
		default:
			t.Fatalf("unexpected interaction request count: %d", capture.requestCount)

			return nil, errUnexpectedTestRequest
		}
	}))
}

func TestExportSessionEntryKeepsFullAugmentedPrompt(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	augmentedText := "Answer the user's query based on the web search results.\n\n" +
		"User query:\nnow make a random markdown table\n\nsearch the web.\n\n" +
		"Web search results:\nQuery: planets\nResults:\nTitle: Example\nURL: https://example.com/source\n"

	sourceMessage := &discordgo.Message{ID: "user-1", Author: newDiscordUser("user-1", false)}
	node := instance.nodes.getOrCreate(sourceMessage.ID)
	node.role = messageRoleUser
	node.text = augmentedText
	node.initialized = true

	entry, _ := instance.exportSessionEntry(sourceMessage, "")
	if entry.Text != strings.TrimSpace(augmentedText) {
		t.Fatalf("expected full augmented prompt, got %q", entry.Text)
	}
}

func TestNormalizeExportThinkingRepairsDeltaSeam(t *testing.T) {
	t.Parallel()

	got := normalizeExportThinking("accurate table.User wants a random table.")
	if got != "accurate table. User wants a random table." {
		t.Fatalf("expected repaired seam, got %q", got)
	}

	if got := normalizeExportThinking("Step 5.Next step"); got != "Step 5. Next step" {
		t.Fatalf("expected numeric seam repaired, got %q", got)
	}

	for _, untouched := range []string{"value 3.14Next", "wait...Next", "v2.6Flash"} {
		if normalizeExportThinking(untouched) != untouched {
			t.Fatalf("expected %q untouched, got %q", untouched, normalizeExportThinking(untouched))
		}
	}
}

func TestExportSessionAuthorStripsVisionSuffix(t *testing.T) {
	t.Parallel()

	message := &discordgo.Message{ID: "reply-1", Author: newDiscordUser("bot-1", true)}
	if got := exportSessionAuthor(message, messageRoleAssistant, "xiaomi/oc/mimo-v2.6-flash-free:vision"); got != "xiaomi/oc/mimo-v2.6-flash-free" {
		t.Fatalf("expected vision suffix stripped, got %q", got)
	}
}

func TestBuildExportSessionHTMLRendersChronologicalTurns(t *testing.T) {
	t.Parallel()

	exportHTML := buildExportSessionHTML([]exportSessionMessage{
		{
			ID: "1", Role: messageRoleUser, Author: "tester", Text: "hello <world>",
			Model: "", Thinking: "", Sources: nil, Timestamp: "",
		},
		{
			ID:        "2",
			Role:      messageRoleAssistant,
			Author:    "openai/gpt-test",
			Text:      "reply with **markdown**",
			Thinking:  "private reasoning",
			Sources:   []exportSessionSource{{Title: "Example", URL: "https://example.com/source"}},
			Model:     "openai/gpt-test:vision",
			Timestamp: "",
		},
	}, testExportTime())

	for _, fragment := range []string{
		"Session export - 2 messages",
		"hello &lt;world&gt;",
		"Thinking",
		"private reasoning",
		"https://example.com/source",
		"theme-select",
		"marked.min.js",
		`title="openai/gpt-test:vision"`,
		"renderMarkdown();",
	} {
		if !strings.Contains(exportHTML, fragment) {
			t.Fatalf("expected fragment %q in export HTML", fragment)
		}
	}

	userIndex := strings.Index(exportHTML, "hello &lt;world&gt;")
	assistantIndex := strings.Index(exportHTML, "reply with")

	if userIndex < 0 || assistantIndex < 0 || userIndex > assistantIndex {
		t.Fatalf("expected chronological turns in export HTML")
	}
}

func TestCollectExportSessionMessagesWalksReplyChain(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	parentMessage := &discordgo.Message{ID: "parent-1", Author: newDiscordUser("user-1", false)}
	replyMessage := &discordgo.Message{ID: "reply-1", Author: newDiscordUser("bot-1", true)}

	seedExportChain(instance, parentMessage, replyMessage, "assistant reply")

	messages := instance.collectExportSessionMessages(replyMessage, defaultMaxMessages)
	if len(messages) != 2 {
		t.Fatalf("unexpected export message count: %#v", messages)
	}

	if messages[0].Role != messageRoleUser || messages[0].Text != "what is the export format?" {
		t.Fatalf("unexpected first export turn: %#v", messages[0])
	}

	if messages[1].Role != messageRoleAssistant || messages[1].Text != "assistant reply" {
		t.Fatalf("unexpected second export turn: %#v", messages[1])
	}

	if messages[1].Thinking != "checked the chain" {
		t.Fatalf("expected thinking in export turn: %#v", messages[1])
	}

	if len(messages[1].Sources) != 1 || messages[1].Sources[0].URL != "https://example.com/source" {
		t.Fatalf("expected sources in export turn: %#v", messages[1].Sources)
	}
}

func TestHandleInteractionCreateRespondsToExportSessionButton(t *testing.T) {
	t.Parallel()

	var (
		capture    deferredInteractionCapture
		editedHTML string
		editedFile string
	)

	session := newExportInteractionSession(t, &capture, &editedHTML, &editedFile)

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.configPath = writeModelConfig(t)
	instance.seedConfigCache(config{MaxMessages: defaultMaxMessages})

	parentMessage := &discordgo.Message{ID: "parent-1", Author: newDiscordUser("user-1", false)}
	replyMessage := &discordgo.Message{ID: "response-message", Author: newDiscordUser("bot-1", true)}
	replyMessage.Content = "assistant reply"

	seedExportChain(instance, parentMessage, replyMessage, "assistant reply")

	interaction := newComponentInteraction("response-message", exportSessionButtonCustomID)

	instance.handleInteractionCreate(session, interaction)

	assertDeferredEphemeralInteractionResponse(t, &capture.deferredResponse)

	if capture.requestCount != 2 {
		t.Fatalf("unexpected request count: %d", capture.requestCount)
	}

	if !strings.Contains(capture.editedResponse.Content, "Exported 2 messages to HTML.") {
		t.Fatalf("unexpected edited response content: %q", capture.editedResponse.Content)
	}

	if editedFile != "session-responsemessage.html" {
		t.Fatalf("unexpected export filename: %q", editedFile)
	}

	for _, fragment := range []string{
		"what is the export format?",
		"assistant reply",
		"https://example.com/source",
	} {
		if !strings.Contains(editedHTML, fragment) {
			t.Fatalf("expected fragment %q in uploaded export HTML", fragment)
		}
	}
}

func TestBuildResponseButtonsIncludesExportButton(t *testing.T) {
	t.Parallel()

	buttons := buildResponseButtons(responseActions{showGist: true, showExport: true})
	if len(buttons) != 2 {
		t.Fatalf("unexpected button count: %#v", buttons)
	}

	button, ok := buttons[1].(*discordgo.Button)
	if !ok {
		t.Fatalf("unexpected component type: %#v", buttons[1])
	}

	if button.CustomID != exportSessionButtonCustomID {
		t.Fatalf("unexpected export button custom ID: %q", button.CustomID)
	}

	if button.Label != exportSessionButtonLabel {
		t.Fatalf("unexpected export button label: got %q, want %q", button.Label, exportSessionButtonLabel)
	}
}
