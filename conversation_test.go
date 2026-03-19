package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	testAssistantReply = "assistant reply"
	testThinkingReply  = "thinking reply"
	testVideoBody      = "video-bytes"
	testVideoMIMEType  = "video/mp4"
	testDOCXFilename   = "report.docx"
	testPPTXFilename   = "slides.pptx"
)

func TestBuildMessageTextReadsTextDisplayInsideSection(t *testing.T) {
	t.Parallel()

	message := new(discordgo.Message)

	button := new(discordgo.Button)
	button.CustomID = showSourcesButtonCustomID
	button.Label = showSourcesButtonLabel
	button.Style = discordgo.SecondaryButton

	section := new(discordgo.Section)
	section.Components = []discordgo.MessageComponent{
		discordgo.TextDisplay{Content: testAssistantReply},
	}
	section.Accessory = button

	message.Components = []discordgo.MessageComponent{section}

	text := buildMessageText(message, "", nil)
	if text != testAssistantReply {
		t.Fatalf("unexpected message text: %q", text)
	}
}

func TestFetchSupportedAttachmentsRetriesTransientDownloadError(t *testing.T) {
	t.Parallel()

	const attachmentURL = "https://cdn.discordapp.com/attachments/test/context.txt"

	attemptCount := 0

	instance := new(bot)
	instance.httpClient = new(http.Client)
	instance.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodGet || request.URL.String() != attachmentURL {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.String())
		}

		attemptCount++
		if attemptCount == 1 {
			return nil, temporaryAttachmentNetError{}
		}

		return newTextResponse(request, "retried attachment body"), nil
	})

	attachment := new(discordgo.MessageAttachment)
	attachment.ContentType = "text/plain"
	attachment.Filename = "context.txt"
	attachment.URL = attachmentURL

	payloads, failed := instance.fetchSupportedAttachments(
		context.Background(),
		[]*discordgo.MessageAttachment{attachment},
	)

	if failed {
		t.Fatal("expected attachment retry to succeed")
	}

	if attemptCount != 2 {
		t.Fatalf("unexpected download attempts: %d", attemptCount)
	}

	if len(payloads) != 1 {
		t.Fatalf("unexpected payload count: %d", len(payloads))
	}

	if got := string(payloads[0].body); got != "retried attachment body" {
		t.Fatalf("unexpected payload body: %q", got)
	}
}

func TestBuildConversationAddsFallbackTextWhenAttachmentDownloadFails(t *testing.T) {
	t.Parallel()

	const (
		botUserID     = "bot-user"
		channelID     = "channel-1"
		userID        = "user-1"
		messageID     = "user-message-1"
		attachmentURL = "https://cdn.discordapp.com/attachments/test/context.txt"
	)

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.State.User = newDiscordUser(botUserID, true)

	channel := new(discordgo.Channel)
	channel.ID = channelID
	channel.Type = discordgo.ChannelTypeDM

	err = session.State.ChannelAdd(channel)
	if err != nil {
		t.Fatalf("add channel to state: %v", err)
	}

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.httpClient = new(http.Client)
	instance.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodGet || request.URL.String() != attachmentURL {
			t.Fatalf("unexpected attachment request: %s %s", request.Method, request.URL.String())
		}

		return nil, temporaryAttachmentNetError{}
	})

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = messageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "<@" + botUserID + ">"
	sourceMessage.Mentions = []*discordgo.User{newDiscordUser(botUserID, false)}
	sourceMessage.Attachments = []*discordgo.MessageAttachment{{
		ContentType: "text/plain",
		Filename:    "context.txt",
		URL:         attachmentURL,
	}}

	conversation, warnings := instance.buildConversation(
		context.Background(),
		sourceMessage,
		defaultMaxText,
		messageContentOptions{
			maxImages:                defaultMaxImages,
			allowAudio:               false,
			allowDocuments:           false,
			allowedDocumentMIMETypes: nil,
			allowVideo:               false,
		},
		defaultMaxMessages,
		false,
		false,
	)

	if len(conversation) != 1 {
		t.Fatalf("unexpected conversation length: %d", len(conversation))
	}

	content, ok := conversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected conversation content type: %T", conversation[0].Content)
	}

	expectedContent := "<@" + userID + ">: " + attachmentDownloadFallbackText
	if content != expectedContent {
		t.Fatalf("unexpected conversation content: %q", content)
	}

	if !slicesContainsString(warnings, attachmentDownloadWarningText) {
		t.Fatalf("expected attachment download warning: %#v", warnings)
	}
}

func TestBuildConversationStopsAtDirectRepliedUserMessage(t *testing.T) {
	t.Parallel()

	const (
		botUserID       = "bot-user"
		channelID       = "channel-1"
		userID          = "user-1"
		rootMessageID   = "root-message"
		assistantID     = "assistant-message"
		replyTargetID   = "reply-target-message"
		followUpID      = "follow-up-message"
		replyTargetText = "test"
	)

	instance := newHistoryRetentionTestBot(t, botUserID, channelID)

	rootMessage := new(discordgo.Message)
	rootMessage.ID = rootMessageID
	rootMessage.ChannelID = channelID
	rootMessage.Author = newDiscordUser(userID, false)
	rootMessage.Content = "at ai original prompt"

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.MessageReference = rootMessage.Reference()
	assistantMessage.Type = discordgo.MessageTypeReply
	setCachedAssistantNode(instance, assistantMessage, rootMessage)

	replyTargetMessage := new(discordgo.Message)
	replyTargetMessage.ID = replyTargetID
	replyTargetMessage.ChannelID = channelID
	replyTargetMessage.Author = newDiscordUser(userID, false)
	replyTargetMessage.Content = replyTargetText
	replyTargetMessage.MessageReference = assistantMessage.Reference()
	replyTargetMessage.ReferencedMessage = assistantMessage
	setCachedUserNode(
		instance,
		replyTargetMessage,
		assistantMessage,
		"<@"+userID+">: "+replyTargetText,
	)

	followUpMessage := new(discordgo.Message)
	followUpMessage.ID = followUpID
	followUpMessage.ChannelID = channelID
	followUpMessage.Author = newDiscordUser(userID, false)
	followUpMessage.Content = "at ai hi"
	followUpMessage.MessageReference = replyTargetMessage.Reference()
	followUpMessage.ReferencedMessage = replyTargetMessage

	conversation, warnings := instance.buildConversation(
		context.Background(),
		followUpMessage,
		defaultMaxText,
		messageContentOptions{
			maxImages:                defaultMaxImages,
			allowAudio:               false,
			allowDocuments:           false,
			allowedDocumentMIMETypes: nil,
			allowVideo:               false,
		},
		defaultMaxMessages,
		false,
		false,
	)

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if len(conversation) != 2 {
		t.Fatalf("unexpected conversation length: %d", len(conversation))
	}

	if conversation[0].Role != messageRoleUser ||
		conversation[0].Content != "<@"+userID+">: "+replyTargetText {
		t.Fatalf("unexpected reply target message: %#v", conversation[0])
	}

	if conversation[1].Role != messageRoleUser ||
		conversation[1].Content != "<@"+userID+">: hi" {
		t.Fatalf("unexpected follow-up message: %#v", conversation[1])
	}
}

func slicesContainsString(items []string, target string) bool {
	for _, item := range items {
		if strings.TrimSpace(item) == target {
			return true
		}
	}

	return false
}

type temporaryAttachmentNetError struct{}

func (temporaryAttachmentNetError) Error() string {
	return "temporary attachment transport error"
}

func (temporaryAttachmentNetError) Timeout() bool {
	return false
}

func (temporaryAttachmentNetError) Temporary() bool {
	return true
}

func TestBuildMediaPartsSupportsGeminiBinaryAttachments(t *testing.T) {
	t.Parallel()

	parts := buildMediaParts(testGeminiBinaryAttachmentPayloads())
	assertGeminiBinaryAttachmentParts(t, parts)
}

func testGeminiBinaryAttachmentPayloads() []attachmentPayload {
	imageAttachment := new(discordgo.MessageAttachment)
	imageAttachment.ContentType = mimeTypePNG
	imageAttachment.Filename = "image.png"

	audioAttachment := new(discordgo.MessageAttachment)
	audioAttachment.ContentType = "audio/mpeg"
	audioAttachment.Filename = "clip.mp3"

	documentAttachment := new(discordgo.MessageAttachment)
	documentAttachment.ContentType = mimeTypePDF
	documentAttachment.Filename = testPDFFilename

	docxAttachment := new(discordgo.MessageAttachment)
	docxAttachment.ContentType = mimeTypeDOCX
	docxAttachment.Filename = testDOCXFilename

	pptxAttachment := new(discordgo.MessageAttachment)
	pptxAttachment.ContentType = mimeTypePPTX
	pptxAttachment.Filename = testPPTXFilename

	videoAttachment := new(discordgo.MessageAttachment)
	videoAttachment.ContentType = testVideoMIMEType
	videoAttachment.Filename = "clip.mp4"

	return []attachmentPayload{
		{attachment: imageAttachment, body: []byte("image-bytes")},
		{attachment: audioAttachment, body: []byte("audio-bytes")},
		{attachment: documentAttachment, body: []byte("document-bytes")},
		{attachment: docxAttachment, body: []byte("docx-bytes")},
		{attachment: pptxAttachment, body: []byte("pptx-bytes")},
		{attachment: videoAttachment, body: []byte(testVideoBody)},
	}
}

func assertGeminiBinaryAttachmentParts(t *testing.T, parts []contentPart) {
	t.Helper()

	if len(parts) != 6 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	imageURL, ok := parts[0]["image_url"].(map[string]string)
	if !ok {
		t.Fatalf("unexpected image part: %#v", parts[0])
	}

	expectedImageURL := "data:image/png;base64," +
		base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	if imageURL["url"] != expectedImageURL {
		t.Fatalf("unexpected image data URL: %q", imageURL["url"])
	}

	audioBytes, audioOK := parts[1][contentFieldBytes].([]byte)
	if !audioOK {
		t.Fatalf("unexpected audio bytes: %#v", parts[1][contentFieldBytes])
	}

	if parts[1]["type"] != contentTypeAudioData || string(audioBytes) != "audio-bytes" {
		t.Fatalf("unexpected audio part: %#v", parts[1])
	}

	documentBytes, documentOK := parts[2][contentFieldBytes].([]byte)
	if !documentOK {
		t.Fatalf("unexpected document bytes: %#v", parts[2][contentFieldBytes])
	}

	if parts[2]["type"] != contentTypeDocument || string(documentBytes) != "document-bytes" {
		t.Fatalf("unexpected document part: %#v", parts[2])
	}

	docxBytes, docxOK := parts[3][contentFieldBytes].([]byte)
	if !docxOK {
		t.Fatalf("unexpected docx bytes: %#v", parts[3][contentFieldBytes])
	}

	if parts[3]["type"] != contentTypeDocument || string(docxBytes) != "docx-bytes" {
		t.Fatalf("unexpected docx document part: %#v", parts[3])
	}

	pptxBytes, pptxOK := parts[4][contentFieldBytes].([]byte)
	if !pptxOK {
		t.Fatalf("unexpected pptx bytes: %#v", parts[4][contentFieldBytes])
	}

	if parts[4]["type"] != contentTypeDocument || string(pptxBytes) != "pptx-bytes" {
		t.Fatalf("unexpected pptx document part: %#v", parts[4])
	}

	videoBytes, videoOK := parts[5][contentFieldBytes].([]byte)
	if !videoOK {
		t.Fatalf("unexpected video bytes: %#v", parts[5][contentFieldBytes])
	}

	if parts[5]["type"] != contentTypeVideoData || string(videoBytes) != testVideoBody {
		t.Fatalf("unexpected video part: %#v", parts[5])
	}
}

func TestBuildMessageContentFiltersUnsupportedMedia(t *testing.T) {
	t.Parallel()

	node := new(messageNode)
	node.role = messageRoleUser
	node.text = "<@123>: summarize these"
	node.media = []contentPart{
		{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
		{
			"type":               contentTypeAudioData,
			contentFieldBytes:    []byte("audio-bytes"),
			contentFieldMIMEType: "audio/mpeg",
		},
		{
			"type":               contentTypeDocument,
			contentFieldBytes:    []byte("document-bytes"),
			contentFieldMIMEType: mimeTypePDF,
		},
		{
			"type":               contentTypeDocument,
			contentFieldBytes:    []byte("docx-bytes"),
			contentFieldMIMEType: mimeTypeDOCX,
		},
		{
			"type":               contentTypeDocument,
			contentFieldBytes:    []byte("pptx-bytes"),
			contentFieldMIMEType: mimeTypePPTX,
		},
		{
			"type":               contentTypeVideoData,
			contentFieldBytes:    []byte(testVideoBody),
			contentFieldMIMEType: testVideoMIMEType,
		},
	}

	var textOnlyOptions messageContentOptions

	textOnlyOptions.maxImages = 1

	content, summary := buildMessageContent(node, defaultMaxText, textOnlyOptions)

	contentParts, contentPartsOK := content.([]contentPart)
	if !contentPartsOK {
		t.Fatalf("unexpected content type: %T", content)
	}

	if len(contentParts) != 2 {
		t.Fatalf("unexpected part count: %d", len(contentParts))
	}

	if summary.imageCount != 1 {
		t.Fatalf("unexpected image count: %d", summary.imageCount)
	}

	if summary.unsupportedAttachmentCnt != 5 {
		t.Fatalf("unexpected unsupported count: %d", summary.unsupportedAttachmentCnt)
	}

	var geminiOptions messageContentOptions

	geminiOptions.maxImages = 1
	geminiOptions.allowAudio = true
	geminiOptions.allowDocuments = true
	geminiOptions.allowedDocumentMIMETypes = allowedGeminiDocumentMIMETypes()
	geminiOptions.allowVideo = true

	content, summary = buildMessageContent(node, defaultMaxText, geminiOptions)

	contentParts, contentPartsOK = content.([]contentPart)
	if !contentPartsOK {
		t.Fatalf("unexpected content type: %T", content)
	}

	if len(contentParts) != 5 {
		t.Fatalf("unexpected part count with gemini media: %d", len(contentParts))
	}

	if summary.unsupportedAttachmentCnt != 2 {
		t.Fatalf("unexpected unsupported count with gemini media: %d", summary.unsupportedAttachmentCnt)
	}
}

func TestAttachmentIsDocumentSupportsDOCXAndPPTX(t *testing.T) {
	t.Parallel()

	for _, mimeType := range []string{mimeTypePDF, mimeTypeDOCX, mimeTypePPTX} {
		if !attachmentIsDocument(mimeType) {
			t.Fatalf("expected MIME type %q to be treated as document", mimeType)
		}
	}
}

func TestMessageContentOptionsAllowsDocumentPartRespectsAllowedMIMETypes(t *testing.T) {
	t.Parallel()

	options := messageContentOptions{
		maxImages:                0,
		allowAudio:               false,
		allowDocuments:           true,
		allowedDocumentMIMETypes: allowedGeminiDocumentMIMETypes(),
		allowVideo:               false,
	}

	pdfPart := contentPart{
		"type":               contentTypeDocument,
		contentFieldMIMEType: mimeTypePDF,
	}
	docxPart := contentPart{
		"type":               contentTypeDocument,
		contentFieldMIMEType: mimeTypeDOCX,
	}

	if !messageContentOptionsAllowsDocumentPart(options, pdfPart) {
		t.Fatalf("expected PDF part to be allowed: %#v", pdfPart)
	}

	if messageContentOptionsAllowsDocumentPart(options, docxPart) {
		t.Fatalf("expected DOCX part to be disallowed: %#v", docxPart)
	}
}
