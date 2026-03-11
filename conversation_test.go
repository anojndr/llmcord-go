package main

import (
	"encoding/base64"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	testAssistantReply = "assistant reply"
	testVideoMIMEType  = "video/mp4"
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

	imageAttachment := new(discordgo.MessageAttachment)
	imageAttachment.ContentType = mimeTypePNG
	imageAttachment.Filename = "image.png"

	audioAttachment := new(discordgo.MessageAttachment)
	audioAttachment.ContentType = "audio/mpeg"
	audioAttachment.Filename = "clip.mp3"

	documentAttachment := new(discordgo.MessageAttachment)
	documentAttachment.ContentType = mimeTypePDF
	documentAttachment.Filename = testPDFFilename

	videoAttachment := new(discordgo.MessageAttachment)
	videoAttachment.ContentType = testVideoMIMEType
	videoAttachment.Filename = "clip.mp4"

	payloads := []attachmentPayload{
		{
			attachment: imageAttachment,
			body:       []byte("image-bytes"),
		},
		{
			attachment: audioAttachment,
			body:       []byte("audio-bytes"),
		},
		{
			attachment: documentAttachment,
			body:       []byte("document-bytes"),
		},
		{
			attachment: videoAttachment,
			body:       []byte("video-bytes"),
		},
	}

	parts := buildMediaParts(payloads)
	if len(parts) != 4 {
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

	if parts[1]["type"] != contentTypeAudioData ||
		string(audioBytes) != "audio-bytes" {
		t.Fatalf("unexpected audio part: %#v", parts[1])
	}

	documentBytes, documentOK := parts[2][contentFieldBytes].([]byte)
	if !documentOK {
		t.Fatalf("unexpected document bytes: %#v", parts[2][contentFieldBytes])
	}

	if parts[2]["type"] != contentTypeDocument ||
		string(documentBytes) != "document-bytes" {
		t.Fatalf("unexpected document part: %#v", parts[2])
	}

	videoBytes, videoOK := parts[3][contentFieldBytes].([]byte)
	if !videoOK {
		t.Fatalf("unexpected video bytes: %#v", parts[3][contentFieldBytes])
	}

	if parts[3]["type"] != contentTypeVideoData ||
		string(videoBytes) != "video-bytes" {
		t.Fatalf("unexpected video part: %#v", parts[3])
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
			"type":               contentTypeVideoData,
			contentFieldBytes:    []byte("video-bytes"),
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

	if summary.unsupportedAttachmentCnt != 3 {
		t.Fatalf("unexpected unsupported count: %d", summary.unsupportedAttachmentCnt)
	}

	var geminiOptions messageContentOptions

	geminiOptions.maxImages = 1
	geminiOptions.allowAudio = true
	geminiOptions.allowDocuments = true
	geminiOptions.allowVideo = true

	content, summary = buildMessageContent(node, defaultMaxText, geminiOptions)

	contentParts, contentPartsOK = content.([]contentPart)
	if !contentPartsOK {
		t.Fatalf("unexpected content type: %T", content)
	}

	if len(contentParts) != 5 {
		t.Fatalf("unexpected part count with gemini media: %d", len(contentParts))
	}

	if summary.unsupportedAttachmentCnt != 0 {
		t.Fatalf("unexpected unsupported count with gemini media: %d", summary.unsupportedAttachmentCnt)
	}
}
