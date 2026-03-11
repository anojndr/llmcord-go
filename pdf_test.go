package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"strconv"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const testPDFFilename = "report.pdf"

func TestExtractPDFContentReturnsTextAndImages(t *testing.T) {
	t.Parallel()

	documentPart := testPDFDocumentPart(
		t,
		"Quarterly revenue grew by 12 percent.",
		true,
	)

	extraction, err := extractPDFContent(documentPart)
	if err != nil {
		t.Fatalf("extract pdf content: %v", err)
	}

	if extraction.filename != testPDFFilename {
		t.Fatalf("unexpected filename: %q", extraction.filename)
	}

	if !strings.Contains(extraction.text, "Quarterly revenue grew by 12 percent.") {
		t.Fatalf("unexpected extracted text: %q", extraction.text)
	}

	if len(extraction.imageParts) != 1 {
		t.Fatalf("unexpected extracted image count: %d", len(extraction.imageParts))
	}

	imageURL, ok := extraction.imageParts[0]["image_url"].(map[string]string)
	if !ok {
		t.Fatalf("unexpected extracted image part: %#v", extraction.imageParts[0])
	}

	if !strings.HasPrefix(imageURL["url"], "data:image/jpeg;base64,") {
		t.Fatalf("unexpected extracted image URL: %q", imageURL["url"])
	}
}

func TestExtractPDFContentReturnsTextWithoutImages(t *testing.T) {
	t.Parallel()

	documentPart := testPDFDocumentPart(
		t,
		"Quarterly revenue grew by 12 percent.",
		false,
	)

	extraction, err := extractPDFContent(documentPart)
	if err != nil {
		t.Fatalf("extract pdf content: %v", err)
	}

	if !strings.Contains(extraction.text, "Quarterly revenue grew by 12 percent.") {
		t.Fatalf("unexpected extracted text: %q", extraction.text)
	}

	if len(extraction.imageParts) != 0 {
		t.Fatalf("unexpected extracted image count: %d", len(extraction.imageParts))
	}
}

func TestMaybeAugmentConversationWithPDFContentsAppendsTextAndImagesForNonGeminiModel(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newPDFExtractionTestBot(
		"message-1",
		"<@123>: summarize the report",
		[]contentPart{
			testPDFDocumentPart(t, "Quarterly revenue grew by 12 percent.", true),
		},
	)

	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.MaxImages = 2

	augmentedConversation, err := instance.maybeAugmentConversationWithPDFContents(
		context.Background(),
		loadedConfig,
		"openai/gpt-5",
		sourceMessage,
		[]chatMessage{
			{Role: messageRoleAssistant, Content: "Earlier answer"},
			{Role: messageRoleUser, Content: "<@123>: summarize the report"},
		},
	)
	if err != nil {
		t.Fatalf("augment conversation with extracted pdf content: %v", err)
	}

	parts, ok := augmentedConversation[1].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[1].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	textValue, _ := parts[0]["text"].(string)
	if !strings.Contains(textValue, pdfContentOpenTag) {
		t.Fatalf("expected extracted pdf block in text: %q", textValue)
	}

	if !strings.Contains(textValue, "Quarterly revenue grew by 12 percent.") {
		t.Fatalf("expected extracted pdf text in prompt: %q", textValue)
	}

	if !strings.Contains(textValue, "Extracted images: 1 total.") {
		t.Fatalf("expected extracted image summary in prompt: %q", textValue)
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected extracted image part: %#v", parts[1])
	}
}

func TestMaybeAugmentConversationWithPDFContentsAppendsReplyTargetPDFForNonGeminiModel(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newPDFExtractionTestBot(
		"message-1-reply",
		"<@123>: summarize the replied report",
		nil,
	)

	replyTargetMessage := new(discordgo.Message)
	replyTargetMessage.ID = "message-1-parent"

	sourceMessage.MessageReference = replyTargetMessage.Reference()

	sourceNode := instance.nodes.getOrCreate(sourceMessage.ID)
	sourceNode.parentMessage = replyTargetMessage

	replyTargetNode := instance.nodes.getOrCreate(replyTargetMessage.ID)
	replyTargetNode.initialized = true
	replyTargetNode.role = messageRoleUser
	replyTargetNode.media = []contentPart{
		testPDFDocumentPart(t, "Reply-target quarterly revenue grew by 18 percent.", true),
	}

	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.MaxImages = 2

	augmentedConversation, err := instance.maybeAugmentConversationWithPDFContents(
		context.Background(),
		loadedConfig,
		"openai/gpt-5",
		sourceMessage,
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: summarize the replied report"}},
	)
	if err != nil {
		t.Fatalf("augment conversation with replied pdf content: %v", err)
	}

	parts, ok := augmentedConversation[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	textValue, _ := parts[0]["text"].(string)
	if !strings.Contains(textValue, "Reply-target quarterly revenue grew by 18 percent.") {
		t.Fatalf("expected replied pdf text in prompt: %q", textValue)
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected extracted replied pdf image part: %#v", parts[1])
	}
}

func TestMaybeAugmentConversationWithPDFContentsAppendsAssistantReplyTargetSourcePDFForNonGeminiModel(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newPDFExtractionTestBot(
		"message-1-assistant-reply",
		"<@123>: what does the report say?",
		nil,
	)

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = "message-1-assistant"

	originalSourceMessage := new(discordgo.Message)
	originalSourceMessage.ID = "message-1-original"

	sourceMessage.MessageReference = assistantMessage.Reference()

	sourceNode := instance.nodes.getOrCreate(sourceMessage.ID)
	sourceNode.parentMessage = assistantMessage

	assistantNode := instance.nodes.getOrCreate(assistantMessage.ID)
	assistantNode.initialized = true
	assistantNode.role = messageRoleAssistant
	assistantNode.text = "Earlier answer"
	assistantNode.parentMessage = originalSourceMessage

	originalSourceNode := instance.nodes.getOrCreate(originalSourceMessage.ID)
	originalSourceNode.initialized = true
	originalSourceNode.role = messageRoleUser
	originalSourceNode.text = "<@123>: here is the original report"
	originalSourceNode.media = []contentPart{
		testPDFDocumentPart(t, "Reply-chain quarterly revenue grew by 18 percent.", true),
	}

	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.MaxImages = 2

	augmentedConversation, err := instance.maybeAugmentConversationWithPDFContents(
		context.Background(),
		loadedConfig,
		"openai/gpt-5",
		sourceMessage,
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: what does the report say?"}},
	)
	if err != nil {
		t.Fatalf("augment conversation with assistant reply target pdf: %v", err)
	}

	parts, ok := augmentedConversation[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	textValue, _ := parts[0]["text"].(string)
	if !strings.Contains(textValue, "Reply-chain quarterly revenue grew by 18 percent.") {
		t.Fatalf("expected original pdf text in prompt: %q", textValue)
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected extracted original pdf image part: %#v", parts[1])
	}
}

func TestMaybeAugmentConversationWithPDFContentsHonorsImageLimit(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newPDFExtractionTestBot(
		"message-2",
		"<@123>: describe the PDF",
		[]contentPart{
			testPDFDocumentPart(t, "A single embedded chart.", true),
		},
	)

	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.MaxImages = 1

	augmentedConversation, err := instance.maybeAugmentConversationWithPDFContents(
		context.Background(),
		loadedConfig,
		"openai/gpt-5",
		sourceMessage,
		[]chatMessage{
			{
				Role: messageRoleUser,
				Content: []contentPart{
					{"type": contentTypeText, "text": "<@123>: describe the PDF"},
					{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("augment conversation with extracted pdf content: %v", err)
	}

	parts, ok := augmentedConversation[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	if messageImageCount(parts) != 1 {
		t.Fatalf("unexpected image count after applying limit: %d", messageImageCount(parts))
	}

	textValue, _ := parts[0]["text"].(string)
	if !strings.Contains(textValue, "Extracted images: 1 total.") {
		t.Fatalf("expected image limit note in prompt: %q", textValue)
	}
}

func TestMaybeAugmentConversationWithPDFContentsSkipsGeminiModel(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newPDFExtractionTestBot(
		"message-3",
		"<@123>: summarize the report",
		[]contentPart{
			testPDFDocumentPart(t, "Quarterly revenue grew by 12 percent.", true),
		},
	)

	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.MaxImages = 2

	augmentedConversation, err := instance.maybeAugmentConversationWithPDFContents(
		context.Background(),
		loadedConfig,
		testMediaAnalysisModel,
		sourceMessage,
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: summarize the report"}},
	)
	if err != nil {
		t.Fatalf("augment conversation with extracted pdf content: %v", err)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if content != "<@123>: summarize the report" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestBuildConversationSuppressesUnsupportedWarningForExtractedPDFs(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newPDFExtractionTestBot(
		"message-4",
		"<@123>: summarize the report",
		[]contentPart{
			{
				"type":               contentTypeDocument,
				contentFieldBytes:    []byte("document-bytes"),
				contentFieldMIMEType: mimeTypePDF,
				contentFieldFilename: testPDFFilename,
			},
		},
	)

	conversation, warnings := instance.buildConversation(
		context.Background(),
		sourceMessage,
		defaultMaxText,
		messageContentOptions{
			maxImages:      0,
			allowAudio:     false,
			allowDocuments: false,
			allowVideo:     false,
		},
		defaultMaxMessages,
		false,
		true,
	)

	if len(conversation) != 1 {
		t.Fatalf("unexpected conversation length: %d", len(conversation))
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
}

func TestBuildConversationSuppressesUnsupportedWarningForReplyTargetPDFs(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newPDFExtractionTestBot(
		"message-5",
		"<@123>: summarize the replied report",
		nil,
	)

	replyTargetMessage := new(discordgo.Message)
	replyTargetMessage.ID = "message-5-parent"

	sourceMessage.MessageReference = replyTargetMessage.Reference()

	sourceNode := instance.nodes.getOrCreate(sourceMessage.ID)
	sourceNode.parentMessage = replyTargetMessage

	replyTargetNode := instance.nodes.getOrCreate(replyTargetMessage.ID)
	replyTargetNode.initialized = true
	replyTargetNode.role = messageRoleUser
	replyTargetNode.media = []contentPart{
		{
			"type":               contentTypeDocument,
			contentFieldBytes:    []byte("document-bytes"),
			contentFieldMIMEType: mimeTypePDF,
			contentFieldFilename: testPDFFilename,
		},
	}

	conversation, warnings := instance.buildConversation(
		context.Background(),
		sourceMessage,
		defaultMaxText,
		messageContentOptions{
			maxImages:      0,
			allowAudio:     false,
			allowDocuments: false,
			allowVideo:     false,
		},
		defaultMaxMessages,
		false,
		true,
	)

	if len(conversation) != 1 {
		t.Fatalf("unexpected conversation length: %d", len(conversation))
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
}

func TestBuildConversationSuppressesUnsupportedWarningForAssistantReplyTargetSourcePDFs(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newPDFExtractionTestBot(
		"message-6",
		"<@123>: summarize the report again",
		nil,
	)

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = "message-6-assistant"

	originalSourceMessage := new(discordgo.Message)
	originalSourceMessage.ID = "message-6-original"

	sourceMessage.MessageReference = assistantMessage.Reference()

	sourceNode := instance.nodes.getOrCreate(sourceMessage.ID)
	sourceNode.parentMessage = assistantMessage

	assistantNode := instance.nodes.getOrCreate(assistantMessage.ID)
	assistantNode.initialized = true
	assistantNode.role = messageRoleAssistant
	assistantNode.text = "Here is the summary."
	assistantNode.parentMessage = originalSourceMessage

	originalSourceNode := instance.nodes.getOrCreate(originalSourceMessage.ID)
	originalSourceNode.initialized = true
	originalSourceNode.role = messageRoleUser
	originalSourceNode.text = "<@123>: here is the report"
	originalSourceNode.media = []contentPart{
		testPDFDocumentPart(t, "Quarterly revenue grew by 12 percent.", true),
	}

	conversation, warnings := instance.buildConversation(
		context.Background(),
		sourceMessage,
		defaultMaxText,
		messageContentOptions{
			maxImages:      0,
			allowAudio:     false,
			allowDocuments: false,
			allowVideo:     false,
		},
		defaultMaxMessages,
		false,
		true,
	)

	if len(conversation) != 3 {
		t.Fatalf("unexpected conversation length: %d", len(conversation))
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
}

func newPDFExtractionTestBot(
	messageID string,
	text string,
	media []contentPart,
) (*bot, *discordgo.Message) {
	instance := new(bot)
	instance.nodes = newMessageNodeStore(maxMessageNodes)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = messageID

	sourceNode := instance.nodes.getOrCreate(sourceMessage.ID)
	sourceNode.initialized = true
	sourceNode.role = messageRoleUser
	sourceNode.text = text
	sourceNode.media = media

	return instance, sourceMessage
}

func testPDFDocumentPart(
	t *testing.T,
	text string,
	includeImage bool,
) contentPart {
	t.Helper()

	pdfBytes := buildTestPDF(t, text, includeImage)

	return contentPart{
		"type":               contentTypeDocument,
		contentFieldBytes:    pdfBytes,
		contentFieldMIMEType: mimeTypePDF,
		contentFieldFilename: testPDFFilename,
	}
}

func buildTestPDF(t *testing.T, text string, includeImage bool) []byte {
	t.Helper()

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		testPDFPageObject(includeImage),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	if includeImage {
		jpegBytes := testJPEGBytes(t)
		imageObject := strings.Join(
			[]string{
				"<< /Type /XObject /Subtype /Image /Width 1 /Height 1",
				fmt.Sprintf(
					"/ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>",
					len(jpegBytes),
				),
				"stream",
				string(jpegBytes),
				"endstream",
			},
			"\n",
		)

		objects = append(objects, imageObject)
	}

	contentStream := "BT /F1 12 Tf 72 720 Td (" + escapePDFText(text) + ") Tj ET"
	if includeImage {
		contentStream += "\nq 64 0 0 64 72 620 cm /Im0 Do Q"
	}

	objects = append(objects, fmt.Sprintf(
		"<< /Length %d >>\nstream\n%s\nendstream",
		len(contentStream),
		contentStream,
	))

	return buildTestPDFBytes(objects)
}

func testPDFPageObject(includeImage bool) string {
	resourceObject := "/Resources << /Font << /F1 4 0 R >> >>"
	if includeImage {
		resourceObject = "/Resources << /Font << /F1 4 0 R >> /XObject << /Im0 5 0 R >> >>"
	}

	contentsObjectNumber := 5
	if includeImage {
		contentsObjectNumber = 6
	}

	return fmt.Sprintf(
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] %s /Contents %d 0 R >>",
		resourceObject,
		contentsObjectNumber,
	)
}

func buildTestPDFBytes(objects []string) []byte {
	var buffer bytes.Buffer

	buffer.WriteString("%PDF-1.4\n")
	buffer.Write([]byte{'%', 0x93, 0x8c, 0x8b, 0x9e, '\n'})

	offsets := make([]int, 0, len(objects)+1)
	offsets = append(offsets, 0)

	for index, object := range objects {
		offsets = append(offsets, buffer.Len())
		buffer.WriteString(strconv.Itoa(index + 1))
		buffer.WriteString(" 0 obj\n")
		buffer.WriteString(object)
		buffer.WriteString("\nendobj\n")
	}

	xrefOffset := buffer.Len()
	buffer.WriteString("xref\n")
	buffer.WriteString("0 ")
	buffer.WriteString(strconv.Itoa(len(offsets)))
	buffer.WriteString("\n")
	buffer.WriteString("0000000000 65535 f \n")

	for _, offset := range offsets[1:] {
		_, _ = fmt.Fprintf(&buffer, "%010d 00000 n \n", offset)
	}

	buffer.WriteString("trailer\n")
	buffer.WriteString("<< /Size ")
	buffer.WriteString(strconv.Itoa(len(offsets)))
	buffer.WriteString(" /Root 1 0 R >>\n")
	buffer.WriteString("startxref\n")
	buffer.WriteString(strconv.Itoa(xrefOffset))
	buffer.WriteString("\n%%EOF\n")

	return buffer.Bytes()
}

func escapePDFText(text string) string {
	replacedText := strings.ReplaceAll(text, "\\", "\\\\")
	replacedText = strings.ReplaceAll(replacedText, "(", "\\(")

	return strings.ReplaceAll(replacedText, ")", "\\)")
}

func testJPEGBytes(t *testing.T) []byte {
	t.Helper()

	var buffer bytes.Buffer

	imageRect := image.NewRGBA(image.Rect(0, 0, 1, 1))
	imageRect.Set(0, 0, color.RGBA{R: 220, G: 30, B: 40, A: 255})

	err := jpeg.Encode(&buffer, imageRect, nil)
	if err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}

	return buffer.Bytes()
}
