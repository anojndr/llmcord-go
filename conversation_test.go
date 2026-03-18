package main

import (
	"encoding/base64"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	testAssistantReply = "assistant reply"
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
