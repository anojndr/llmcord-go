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
	pdfcpuapi "github.com/pdfcpu/pdfcpu/pkg/api"
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

func TestExtractPDFContentDecryptsEncryptedPDFWithoutUserPassword(t *testing.T) {
	t.Parallel()

	documentPart := testEncryptedPDFDocumentPart(
		t,
		"Quarterly revenue grew by 12 percent in the encrypted report.",
		true,
		"",
		"owner-secret",
	)

	extraction, err := extractPDFContent(documentPart)
	if err != nil {
		t.Fatalf("extract encrypted pdf content: %v", err)
	}

	if !strings.Contains(
		extraction.text,
		"Quarterly revenue grew by 12 percent in the encrypted report.",
	) {
		t.Fatalf("unexpected extracted text: %q", extraction.text)
	}

	if extraction.notice != "" {
		t.Fatalf("unexpected extraction notice: %q", extraction.notice)
	}

	if len(extraction.imageParts) != 1 {
		t.Fatalf("unexpected extracted image count: %d", len(extraction.imageParts))
	}
}

func TestExtractPDFContentReturnsNoticeForPasswordProtectedPDF(t *testing.T) {
	t.Parallel()

	documentPart := testEncryptedPDFDocumentPart(
		t,
		"Quarterly revenue grew by 12 percent in the protected report.",
		true,
		"open-secret",
		"owner-secret",
	)

	extraction, err := extractPDFContent(documentPart)
	if err != nil {
		t.Fatalf("extract password-protected pdf content: %v", err)
	}

	if extraction.text != "" {
		t.Fatalf("unexpected extracted text: %q", extraction.text)
	}

	if extraction.notice != encryptedPDFExtractionNotice {
		t.Fatalf("unexpected extraction notice: %q", extraction.notice)
	}

	if len(extraction.imageParts) != 0 {
		t.Fatalf("unexpected extracted image count: %d", len(extraction.imageParts))
	}
}

func TestExtractPDFContentReturnsDOCXTextAndImages(t *testing.T) {
	t.Parallel()

	documentPart := testDOCXDocumentPart(
		t,
		"DOCX quarterly revenue grew by 12 percent.",
	)

	extraction, err := extractPDFContent(documentPart)
	if err != nil {
		t.Fatalf("extract docx content: %v", err)
	}

	if extraction.filename != testDOCXFilename {
		t.Fatalf("unexpected filename: %q", extraction.filename)
	}

	if extraction.mimeType != mimeTypeDOCX {
		t.Fatalf("unexpected mime type: %q", extraction.mimeType)
	}

	if !strings.Contains(extraction.text, "DOCX quarterly revenue grew by 12 percent.") {
		t.Fatalf("unexpected extracted text: %q", extraction.text)
	}

	if len(extraction.imageParts) != 1 {
		t.Fatalf("unexpected extracted image count: %d", len(extraction.imageParts))
	}
}

func TestExtractPDFContentReturnsPPTXTextAndImages(t *testing.T) {
	t.Parallel()

	documentPart := testPPTXDocumentPart(
		t,
		"PPTX quarterly revenue grew by 18 percent.",
	)

	extraction, err := extractPDFContent(documentPart)
	if err != nil {
		t.Fatalf("extract pptx content: %v", err)
	}

	if extraction.filename != testPPTXFilename {
		t.Fatalf("unexpected filename: %q", extraction.filename)
	}

	if extraction.mimeType != mimeTypePPTX {
		t.Fatalf("unexpected mime type: %q", extraction.mimeType)
	}

	if !strings.Contains(extraction.text, "PPTX quarterly revenue grew by 18 percent.") {
		t.Fatalf("unexpected extracted text: %q", extraction.text)
	}

	if len(extraction.imageParts) != 1 {
		t.Fatalf("unexpected extracted image count: %d", len(extraction.imageParts))
	}
}

func TestExtractableDocumentPartsForAPIKindSkipsPDFForGemini(t *testing.T) {
	t.Parallel()

	documentParts := []contentPart{
		testPDFDocumentPart(t, "PDF text", true),
		testDOCXDocumentPart(t, "DOCX text"),
		testPPTXDocumentPart(t, "PPTX text"),
	}

	extractableParts, err := extractableDocumentPartsForAPIKind(
		documentParts,
		providerAPIKindGemini,
	)
	if err != nil {
		t.Fatalf("filter extractable parts: %v", err)
	}

	if len(extractableParts) != 2 {
		t.Fatalf("unexpected extractable part count: %d", len(extractableParts))
	}

	firstMIMEType := extractablePartMIMEType(t, extractableParts[0])
	secondMIMEType := extractablePartMIMEType(t, extractableParts[1])

	if firstMIMEType != mimeTypeDOCX || secondMIMEType != mimeTypePPTX {
		t.Fatalf("unexpected extractable MIME types: %q, %q", firstMIMEType, secondMIMEType)
	}
}

func TestExtractableDocumentPartsForAPIKindKeepsPDFForNonGemini(t *testing.T) {
	t.Parallel()

	documentParts := []contentPart{
		testPDFDocumentPart(t, "PDF text", true),
		testDOCXDocumentPart(t, "DOCX text"),
		testPPTXDocumentPart(t, "PPTX text"),
	}

	extractableParts, err := extractableDocumentPartsForAPIKind(
		documentParts,
		providerAPIKindOpenAI,
	)
	if err != nil {
		t.Fatalf("filter extractable parts: %v", err)
	}

	if len(extractableParts) != 3 {
		t.Fatalf("unexpected extractable part count: %d", len(extractableParts))
	}

	for index, expectedMIMEType := range []string{mimeTypePDF, mimeTypeDOCX, mimeTypePPTX} {
		mimeType := extractablePartMIMEType(t, extractableParts[index])
		if mimeType != expectedMIMEType {
			t.Fatalf("unexpected extractable MIME type at index %d: %q", index, mimeType)
		}
	}
}

func TestRenderPDFContentUsesOOXMLTagForDOCXAndPPTX(t *testing.T) {
	t.Parallel()

	for _, extraction := range []extractedPDFContent{
		{
			filename: testDOCXFilename,
			imageParts: []contentPart{
				{
					"type":      contentTypeImageURL,
					"image_url": map[string]string{"url": "data:image/png;base64,abc"},
				},
			},
			mimeType: mimeTypeDOCX,
			notice:   "",
			text:     "DOCX content",
		},
		{
			filename: testPPTXFilename,
			imageParts: []contentPart{
				{
					"type":      contentTypeImageURL,
					"image_url": map[string]string{"url": "data:image/png;base64,abc"},
				},
			},
			mimeType: mimeTypePPTX,
			notice:   "",
			text:     "PPTX content",
		},
	} {
		renderedContent := renderPDFContent(extraction)
		if !strings.Contains(renderedContent, ooxmlContentOpenTag) ||
			!strings.Contains(renderedContent, ooxmlContentCloseTag) {
			t.Fatalf("unexpected OOXML rendered content: %q", renderedContent)
		}
	}
}

func extractablePartMIMEType(t *testing.T, part contentPart) string {
	t.Helper()

	mimeType, _ := part[contentFieldMIMEType].(string)

	return mimeType
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
	if !strings.Contains(textValue, documentContentSectionName+":") {
		t.Fatalf("expected extracted document section in prompt: %q", textValue)
	}

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

func TestMaybeAugmentConversationWithPDFContentsAppendsNoticeForPasswordProtectedPDF(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newPDFExtractionTestBot(
		"message-password-protected-pdf",
		"<@123>: summarize the protected report",
		[]contentPart{
			testEncryptedPDFDocumentPart(
				t,
				"Quarterly revenue grew by 12 percent in the protected report.",
				true,
				"open-secret",
				"owner-secret",
			),
		},
	)

	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.MaxImages = 2

	augmentedConversation, err := instance.maybeAugmentConversationWithPDFContents(
		context.Background(),
		loadedConfig,
		"openai/gpt-5",
		sourceMessage,
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: summarize the protected report"}},
	)
	if err != nil {
		t.Fatalf("augment conversation with password-protected pdf content: %v", err)
	}

	textValue, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if !strings.Contains(textValue, documentContentSectionName+":") {
		t.Fatalf("expected extracted document section in prompt: %q", textValue)
	}

	if !strings.Contains(textValue, "Notice: "+encryptedPDFExtractionNotice) {
		t.Fatalf("expected encrypted pdf notice in prompt: %q", textValue)
	}
}

func TestPersistAugmentedSourceMessageRetainsExtractedPDFTextAndImagesInFollowUpHistory(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newPDFExtractionTestBot(
		"message-pdf-retained",
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
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: summarize the report"}},
	)
	if err != nil {
		t.Fatalf("augment conversation with extracted pdf content: %v", err)
	}

	err = instance.persistAugmentedSourceMessage(
		context.Background(),
		sourceMessage,
		augmentedConversation,
	)
	if err != nil {
		t.Fatalf("persist augmented source message: %v", err)
	}

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = "message-pdf-retained-assistant"
	setCachedAssistantNode(instance, assistantMessage, sourceMessage)

	followUpMessage := new(discordgo.Message)
	followUpMessage.ID = "message-pdf-retained-follow-up"
	setCachedUserNode(instance, followUpMessage, assistantMessage, "<@123>: follow-up question")

	history := retainedHistoryForFollowUp(
		t,
		instance,
		followUpMessage,
		messageContentOptions{
			maxImages:                loadedConfig.MaxImages,
			allowAudio:               false,
			allowDocuments:           false,
			allowedDocumentMIMETypes: nil,
			allowVideo:               false,
		},
	)

	sourceParts, ok := history[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected source message content type: %T", history[0].Content)
	}

	if len(sourceParts) != 2 {
		t.Fatalf("unexpected retained source part count: %d", len(sourceParts))
	}

	sourceText := messageContentText(history[0].Content)
	for _, expectedFragment := range []string{
		pdfContentOpenTag,
		"Quarterly revenue grew by 12 percent.",
		"Extracted images: 1 total.",
		pdfContentCloseTag,
	} {
		if !strings.Contains(sourceText, expectedFragment) {
			t.Fatalf("expected retained pdf fragment %q in %q", expectedFragment, sourceText)
		}
	}

	if sourceParts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected retained extracted image part: %#v", sourceParts[1])
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

func TestMaybeAugmentConversationWithPDFContentsSkipsPDFFilesForGeminiModel(t *testing.T) {
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

func TestMaybeAugmentConversationWithPDFContentsExtractsOOXMLForGeminiModel(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		messageID string
		query     string
		document  contentPart
		expected  string
	}{
		{
			name:      "docx",
			messageID: "message-3-docx-gemini",
			query:     "<@123>: summarize the report",
			document:  testDOCXDocumentPart(t, "DOCX quarterly revenue grew by 12 percent."),
			expected:  "DOCX quarterly revenue grew by 12 percent.",
		},
		{
			name:      "pptx",
			messageID: "message-3-pptx-gemini",
			query:     "<@123>: summarize the slides",
			document:  testPPTXDocumentPart(t, "PPTX quarterly revenue grew by 18 percent."),
			expected:  "PPTX quarterly revenue grew by 18 percent.",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			instance, sourceMessage := newPDFExtractionTestBot(
				testCase.messageID,
				testCase.query,
				[]contentPart{testCase.document},
			)

			loadedConfig := testMediaAnalysisConfig()
			loadedConfig.MaxImages = 2

			augmentedConversation, err := instance.maybeAugmentConversationWithPDFContents(
				context.Background(),
				loadedConfig,
				testMediaAnalysisModel,
				sourceMessage,
				[]chatMessage{{Role: messageRoleUser, Content: testCase.query}},
			)
			if err != nil {
				t.Fatalf("augment conversation with extracted ooxml content: %v", err)
			}

			parts, ok := augmentedConversation[0].Content.([]contentPart)
			if !ok {
				t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
			}

			if len(parts) != 2 {
				t.Fatalf("unexpected part count: %d", len(parts))
			}

			textValue, _ := parts[0]["text"].(string)
			if !strings.Contains(textValue, documentContentSectionName+":") {
				t.Fatalf("expected extracted document section in prompt: %q", textValue)
			}

			if !strings.Contains(textValue, ooxmlContentOpenTag) ||
				!strings.Contains(textValue, ooxmlContentCloseTag) {
				t.Fatalf("expected extracted OOXML block in prompt: %q", textValue)
			}

			if !strings.Contains(textValue, testCase.expected) {
				t.Fatalf("expected extracted OOXML text in prompt: %q", textValue)
			}

			if !strings.Contains(textValue, "Extracted images: 1 total.") {
				t.Fatalf("expected extracted image summary in prompt: %q", textValue)
			}

			if parts[1]["type"] != contentTypeImageURL {
				t.Fatalf("expected extracted image part: %#v", parts[1])
			}
		})
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
			maxImages:                0,
			allowAudio:               false,
			allowDocuments:           false,
			allowedDocumentMIMETypes: nil,
			allowVideo:               false,
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
			maxImages:                0,
			allowAudio:               false,
			allowDocuments:           false,
			allowedDocumentMIMETypes: nil,
			allowVideo:               false,
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
			maxImages:                0,
			allowAudio:               false,
			allowDocuments:           false,
			allowedDocumentMIMETypes: nil,
			allowVideo:               false,
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

func testEncryptedPDFDocumentPart(
	t *testing.T,
	text string,
	includeImage bool,
	userPassword string,
	ownerPassword string,
) contentPart {
	t.Helper()

	pdfBytes := buildEncryptedTestPDF(
		t,
		text,
		includeImage,
		userPassword,
		ownerPassword,
	)

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

func buildEncryptedTestPDF(
	t *testing.T,
	text string,
	includeImage bool,
	userPassword string,
	ownerPassword string,
) []byte {
	t.Helper()

	var encrypted bytes.Buffer

	configuration := newPDFCPUConfiguration()
	configuration.UserPW = userPassword
	configuration.OwnerPW = ownerPassword

	err := pdfcpuapi.Encrypt(
		bytes.NewReader(buildTestPDF(t, text, includeImage)),
		&encrypted,
		configuration,
	)
	if err != nil {
		t.Fatalf("encrypt test pdf: %v", err)
	}

	return encrypted.Bytes()
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
