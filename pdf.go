package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	pdfreader "github.com/ledongthuc/pdf"
	pdfcpuapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

const (
	pdfContentCloseTag = "</pdf_content>"
	pdfContentOpenTag  = "<pdf_content>"
)

type extractedPDFContent struct {
	filename   string
	imageParts []contentPart
	text       string
}

func (instance *bot) maybeAugmentConversationWithPDFContents(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) ([]chatMessage, error) {
	canExtractPDFs, err := canExtractPDFContents(
		loadedConfig,
		providerSlashModel,
	)
	if err != nil {
		return nil, err
	}

	if !canExtractPDFs {
		return conversation, nil
	}

	documentParts, err := instance.documentPartsForMessages(
		ctx,
		instance.attachmentAugmentationMessages(ctx, sourceMessage),
	)
	if err != nil {
		return nil, fmt.Errorf("load pdf parts for extraction: %w", err)
	}

	if len(documentParts) == 0 {
		return conversation, nil
	}

	contentOptions, err := messageContentOptionsForModel(
		loadedConfig,
		providerSlashModel,
	)
	if err != nil {
		return nil, fmt.Errorf("build pdf extraction content options: %w", err)
	}

	remainingImageSlots, err := remainingImageSlotsForConversation(
		conversation,
		contentOptions.maxImages,
	)
	if err != nil {
		return nil, fmt.Errorf("calculate remaining image slots for pdf extraction: %w", err)
	}

	extractions := make([]extractedPDFContent, 0, len(documentParts))
	imageParts := make([]contentPart, 0)

	for index, documentPart := range documentParts {
		extraction, extractionErr := extractPDFContent(documentPart)
		if extractionErr != nil {
			return nil, fmt.Errorf("extract pdf file %d: %w", index+1, extractionErr)
		}

		attachCount := minInt(len(extraction.imageParts), remainingImageSlots)
		if attachCount > 0 {
			imageParts = append(imageParts, extraction.imageParts[:attachCount]...)
			remainingImageSlots -= attachCount
		}

		extractions = append(extractions, extraction)
	}

	augmentedConversation, err := appendPDFContentsToConversation(
		conversation,
		extractions,
	)
	if err != nil {
		return nil, fmt.Errorf("append extracted pdf contents: %w", err)
	}

	augmentedConversation, err = appendMediaPartsToConversation(
		augmentedConversation,
		imageParts,
	)
	if err != nil {
		return nil, fmt.Errorf("append extracted pdf images: %w", err)
	}

	return augmentedConversation, nil
}

func canExtractPDFContents(
	loadedConfig config,
	providerSlashModel string,
) (bool, error) {
	apiKind, err := configuredModelAPIKind(loadedConfig, providerSlashModel)
	if err != nil {
		return false, err
	}

	return apiKind != providerAPIKindGemini, nil
}

func (instance *bot) documentPartsForMessage(
	ctx context.Context,
	message *discordgo.Message,
) ([]contentPart, error) {
	return instance.messagePartsForMessage(ctx, message, partNeedsPDFExtraction)
}

func (instance *bot) documentPartsForMessages(
	ctx context.Context,
	messages []*discordgo.Message,
) ([]contentPart, error) {
	return instance.messagePartsForMessages(ctx, messages, partNeedsPDFExtraction)
}

func partNeedsPDFExtraction(part contentPart) bool {
	partType, _ := part["type"].(string)

	return partType == contentTypeDocument
}

func extractPDFContent(documentPart contentPart) (extractedPDFContent, error) {
	documentBytes, mimeType, filename, err := attachmentBinaryData(documentPart)
	if err != nil {
		return extractedPDFContent{}, err
	}

	if !strings.HasPrefix(mimeType, mimeTypePDF) {
		return extractedPDFContent{}, fmt.Errorf(
			"unsupported pdf mime type %q: %w",
			mimeType,
			os.ErrInvalid,
		)
	}

	text, err := extractPDFText(documentBytes)
	if err != nil {
		return extractedPDFContent{}, err
	}

	imageParts, err := extractPDFImages(documentBytes)
	if err != nil {
		return extractedPDFContent{}, err
	}

	return extractedPDFContent{
		filename:   filename,
		imageParts: imageParts,
		text:       text,
	}, nil
}

func attachmentBinaryData(part contentPart) ([]byte, string, string, error) {
	attachmentBytes, ok := part[contentFieldBytes].([]byte)
	if !ok {
		return nil, "", "", fmt.Errorf("decode attachment bytes: %w", os.ErrInvalid)
	}

	mimeType, _ := part[contentFieldMIMEType].(string)

	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		return nil, "", "", fmt.Errorf("decode attachment mime type: %w", os.ErrInvalid)
	}

	filename, _ := part[contentFieldFilename].(string)

	return attachmentBytes, mimeType, filename, nil
}

func extractPDFText(documentBytes []byte) (string, error) {
	reader, err := pdfreader.NewReader(
		bytes.NewReader(documentBytes),
		int64(len(documentBytes)),
	)
	if err != nil {
		return "", fmt.Errorf("open pdf text reader: %w", err)
	}

	plainTextReader, err := reader.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("extract pdf text: %w", err)
	}

	textBytes, err := io.ReadAll(plainTextReader)
	if err != nil {
		return "", fmt.Errorf("read pdf text: %w", err)
	}

	return strings.TrimSpace(string(textBytes)), nil
}

func extractPDFImages(documentBytes []byte) ([]contentPart, error) {
	extractedImages, err := pdfcpuapi.ExtractImagesRaw(
		bytes.NewReader(documentBytes),
		nil,
		newPDFCPUConfiguration(),
	)
	if err != nil {
		return nil, fmt.Errorf("extract pdf images: %w", err)
	}

	imageParts := make([]contentPart, 0)

	for _, pageImages := range extractedImages {
		objectNumbers := make([]int, 0, len(pageImages))
		for objectNumber := range pageImages {
			objectNumbers = append(objectNumbers, objectNumber)
		}

		sort.Ints(objectNumbers)

		for _, objectNumber := range objectNumbers {
			imagePart, imageErr := pdfImagePart(pageImages[objectNumber])
			if imageErr != nil {
				return nil, fmt.Errorf(
					"extract pdf image object %d: %w",
					objectNumber,
					imageErr,
				)
			}

			if len(imagePart) == 0 {
				continue
			}

			imageParts = append(imageParts, imagePart)
		}
	}

	return imageParts, nil
}

func newPDFCPUConfiguration() *model.Configuration {
	configuration := new(model.Configuration)
	configuration.Path = "disable"
	configuration.CreationDate = time.Now().Format("2006-01-02 15:04")
	configuration.Version = model.VersionStr
	configuration.CheckFileNameExt = true
	configuration.Reader15 = true
	configuration.DecodeAllStreams = false
	configuration.ValidationMode = model.ValidationRelaxed
	configuration.PostProcessValidate = false
	configuration.ValidateLinks = false
	configuration.Eol = types.EolLF
	configuration.WriteObjectStream = true
	configuration.WriteXRefStream = true
	configuration.CollectStats = false
	configuration.StatsFileName = ""
	configuration.UserPW = ""
	configuration.UserPWNew = nil
	configuration.OwnerPW = ""
	configuration.OwnerPWNew = nil
	configuration.EncryptUsingAES = true
	configuration.EncryptKeyLength = 256
	configuration.Permissions = model.PermissionsPrint
	configuration.Cmd = model.EXTRACTIMAGES
	configuration.Unit = 0
	configuration.TimestampFormat = "2006-01-02 15:04"
	configuration.DateFormat = "2006-01-02"
	configuration.Optimize = true
	configuration.OptimizeBeforeWriting = true
	configuration.OptimizeResourceDicts = true
	configuration.OptimizeDuplicateContentStreams = false
	configuration.CreateBookmarks = true
	configuration.NeedAppearances = false
	configuration.Offline = false
	configuration.Timeout = 5
	configuration.TimeoutCRL = 0
	configuration.TimeoutOCSP = 0
	configuration.PreferredCertRevocationChecker = model.CRL
	configuration.FormFieldListMaxColWidth = 0

	return configuration
}

func pdfImagePart(extractedImage model.Image) (contentPart, error) {
	imageBytes, err := io.ReadAll(extractedImage)
	if err != nil {
		return nil, fmt.Errorf("read extracted pdf image: %w", err)
	}

	if len(imageBytes) == 0 {
		return contentPart{}, nil
	}

	mimeType, err := pdfImageMIMEType(extractedImage.FileType, imageBytes)
	if err != nil {
		return nil, err
	}

	part := make(contentPart)
	part["type"] = contentTypeImageURL
	part["image_url"] = map[string]string{
		"url": fmt.Sprintf(
			"data:%s;base64,%s",
			mimeType,
			base64.StdEncoding.EncodeToString(imageBytes),
		),
	}

	return part, nil
}

func pdfImageMIMEType(fileType string, imageBytes []byte) (string, error) {
	switch strings.ToLower(strings.TrimSpace(fileType)) {
	case "jpeg", "jpg":
		return "image/jpeg", nil
	case "png":
		return mimeTypePNG, nil
	case "tif", "tiff":
		return "image/tiff", nil
	case "webp":
		return "image/webp", nil
	case "jp2", "jpx":
		return "image/jpx", nil
	}

	mimeType := strings.TrimSpace(http.DetectContentType(imageBytes))
	if !strings.HasPrefix(mimeType, "image/") {
		return "", fmt.Errorf(
			"detect extracted pdf image mime type %q: %w",
			mimeType,
			os.ErrInvalid,
		)
	}

	return mimeType, nil
}

func appendPDFContentsToConversation(
	conversation []chatMessage,
	extractions []extractedPDFContent,
) ([]chatMessage, error) {
	renderedContent := renderPDFContents(extractions)
	if renderedContent == "" {
		return conversation, nil
	}

	return appendContextToConversation(conversation, func(prompt *augmentedUserPrompt) {
		prompt.UserQuery = appendPromptUserQuery(prompt.UserQuery, renderedContent)
	})
}

func renderPDFContents(extractions []extractedPDFContent) string {
	blocks := make([]string, 0, len(extractions))

	for _, extraction := range extractions {
		renderedBlock := renderPDFContent(extraction)
		if renderedBlock == "" {
			continue
		}

		blocks = append(blocks, renderedBlock)
	}

	return strings.Join(blocks, "\n\n")
}

func renderPDFContent(extraction extractedPDFContent) string {
	lines := []string{pdfContentOpenTag}

	filename := strings.TrimSpace(extraction.filename)
	if filename != "" {
		lines = append(lines, "Filename: "+filename)
	}

	trimmedText := strings.TrimSpace(extraction.text)
	if trimmedText != "" {
		lines = append(lines, "Extracted text:")
		lines = append(lines, trimmedText)
	}

	if len(extraction.imageParts) > 0 {
		lines = append(lines, fmt.Sprintf(
			"Extracted images: %d total.",
			len(extraction.imageParts),
		))
	}

	if trimmedText == "" && len(extraction.imageParts) == 0 {
		lines = append(lines, "No extractable text or images found.")
	}

	lines = append(lines, pdfContentCloseTag)

	return strings.Join(lines, "\n")
}

func remainingImageSlotsForConversation(
	conversation []chatMessage,
	maxImages int,
) (int, error) {
	if maxImages <= 0 {
		return 0, nil
	}

	index, err := latestUserMessageIndex(conversation)
	if err != nil {
		return 0, err
	}

	usedImages := messageImageCount(conversation[index].Content)

	remainingSlots := maxImages - usedImages
	if remainingSlots < 0 {
		return 0, nil
	}

	return remainingSlots, nil
}

func messageImageCount(content any) int {
	parts, ok := content.([]contentPart)
	if !ok {
		return 0
	}

	imageCount := 0

	for _, part := range parts {
		partType, _ := part["type"].(string)
		if partType == contentTypeImageURL {
			imageCount++
		}
	}

	return imageCount
}
