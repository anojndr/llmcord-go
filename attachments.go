package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/bwmarrin/discordgo"
)

const (
	attachmentArchiveEntrySummaryLimit = 8
	attachmentSniffLength              = 512
)

func attachmentPayloadContentType(payload attachmentPayload) string {
	if normalizedType := normalizedMIMEType(payload.contentType); normalizedType != "" {
		return normalizedType
	}

	return attachmentClassification(payload.attachment, payload.body)
}

func attachmentClassification(
	attachment *discordgo.MessageAttachment,
	body []byte,
) string {
	discordType := ""
	inferredType := ""

	if attachment != nil {
		discordType = normalizedMIMEType(strings.TrimSpace(attachment.ContentType))
		inferredType = normalizedMIMEType(
			inferredAttachmentContentType(attachment.Filename, attachment.URL),
		)
	}

	detectedType := detectedAttachmentContentType(body)

	return resolvedAttachmentContentType(discordType, inferredType, detectedType)
}

func detectedAttachmentContentType(body []byte) string {
	if len(body) == 0 {
		return mimeTypeOctetStream
	}

	return normalizedMIMEType(http.DetectContentType(body[:min(len(body), attachmentSniffLength)]))
}

func resolvedAttachmentContentType(
	discordType string,
	inferredType string,
	detectedType string,
) string {
	for _, candidate := range []string{
		detectedAttachmentBinaryType(detectedType),
		specificAttachmentContentType(discordType),
		specificAttachmentContentType(inferredType),
		specificAttachmentContentType(detectedType),
		inferredType,
		discordType,
		detectedType,
	} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}

	return mimeTypeOctetStream
}

func detectedAttachmentBinaryType(contentType string) string {
	normalizedType := normalizedMIMEType(contentType)
	if normalizedType == "" {
		return ""
	}

	switch {
	case normalizedType == mimeTypePDF:
		return normalizedType
	case strings.HasPrefix(normalizedType, "image/"):
		return normalizedType
	case strings.HasPrefix(normalizedType, "audio/"):
		return normalizedType
	case strings.HasPrefix(normalizedType, "video/"):
		return normalizedType
	default:
		return ""
	}
}

func specificAttachmentContentType(contentType string) string {
	normalizedType := normalizedMIMEType(contentType)
	if normalizedType == "" || normalizedType == mimeTypeOctetStream {
		return ""
	}

	return normalizedType
}

func attachmentContentPartType(contentType string) string {
	normalizedType := normalizedMIMEType(contentType)

	switch {
	case strings.HasPrefix(normalizedType, "image/"):
		return contentTypeImageURL
	case strings.HasPrefix(normalizedType, "audio/"):
		return contentTypeAudioData
	case attachmentIsDocument(normalizedType):
		return contentTypeDocument
	case strings.HasPrefix(normalizedType, "video/"):
		return contentTypeVideoData
	default:
		return contentTypeFileData
	}
}

func attachmentIsTextLike(contentType string, filename string) bool {
	if attachmentIsText(contentType) {
		return true
	}

	if attachmentMIMETypeIsTextLike(contentType) {
		return true
	}

	return attachmentExtensionIsTextLike(attachmentExtension(filename))
}

func attachmentExtension(filename string) string {
	base := strings.ToLower(strings.TrimSpace(path.Base(filename)))
	if base == "" {
		return ""
	}

	if extension := strings.ToLower(strings.TrimSpace(path.Ext(base))); extension != "" {
		return extension
	}

	if base == "dockerfile" {
		return base
	}

	return ""
}

func attachmentInlineText(body []byte, contentType string, filename string) string {
	if !attachmentIsTextLike(contentType, filename) {
		return ""
	}

	textValue := strings.TrimSpace(string(bytes.ToValidUTF8(body, []byte{})))
	if textValue == "" {
		return ""
	}

	return textValue
}

func attachmentMIMETypeIsTextLike(contentType string) bool {
	switch normalizedMIMEType(contentType) {
	case "application/ecmascript",
		"application/graphql-response+json",
		"application/javascript",
		"application/json",
		"application/ld+json",
		"application/sql",
		"application/toml",
		"application/x-httpd-php",
		"application/x-javascript",
		"application/x-ndjson",
		"application/x-sh",
		"application/x-shellscript",
		"application/xml",
		"application/yaml",
		"text/csv":
		return true
	default:
		return false
	}
}

func attachmentExtensionIsTextLike(extension string) bool {
	switch extension {
	case ".bash",
		".c",
		".cc",
		".cfg",
		".conf",
		".cpp",
		".cs",
		".css",
		".csv",
		".dockerignore",
		".env",
		".gitignore",
		".go",
		".graphql",
		".h",
		".hpp",
		".html",
		".ini",
		".java",
		".js",
		".json",
		".jsx",
		".kt",
		".log",
		".lua",
		".markdown",
		".md",
		".py",
		".rb",
		".rs",
		".scss",
		".sh",
		".sql",
		".svg",
		".toml",
		".ts",
		".tsx",
		".txt",
		".xml",
		".yaml",
		".yml",
		".zsh",
		"dockerfile":
		return true
	default:
		return false
	}
}

func attachmentSummaryText(payloads []attachmentPayload) string {
	lines := make([]string, 0, len(payloads)+1)

	for _, payload := range payloads {
		line := attachmentSummaryLine(payload)
		if line == "" {
			continue
		}

		if len(lines) == 0 {
			lines = append(lines, "Attachments:")
		}

		lines = append(lines, line)
	}

	return joinNonEmpty(lines)
}

func attachmentSummaryLine(payload attachmentPayload) string {
	contentType := attachmentPayloadContentType(payload)
	if attachmentContentPartType(contentType) != contentTypeFileData {
		return ""
	}

	filename := ""
	if payload.attachment != nil {
		filename = strings.TrimSpace(payload.attachment.Filename)
	}

	if attachmentInlineText(payload.body, contentType, filename) != "" {
		return ""
	}

	description := "raw binary file attached"
	if archiveSummary := attachmentArchiveSummary(payload.body, contentType, filename); archiveSummary != "" {
		description = archiveSummary
	}

	label := filename
	if label == "" {
		label = "unnamed attachment"
	}

	return fmt.Sprintf(
		"- %s (%s, %d bytes): %s",
		label,
		contentType,
		len(payload.body),
		description,
	)
}

func attachmentArchiveSummary(body []byte, contentType string, filename string) string {
	if normalizedMIMEType(contentType) != mimeTypeZIP &&
		attachmentExtension(filename) != ".zip" {
		return ""
	}

	archiveReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "zip archive attached"
	}

	if len(archiveReader.File) == 0 {
		return "empty zip archive"
	}

	entryNames := make([]string, 0, min(len(archiveReader.File), attachmentArchiveEntrySummaryLimit))
	for _, archiveFile := range archiveReader.File {
		if archiveFile == nil {
			continue
		}

		entryNames = append(entryNames, archiveFile.Name)
		if len(entryNames) == attachmentArchiveEntrySummaryLimit {
			break
		}
	}

	if len(entryNames) == 0 {
		return "zip archive attached"
	}

	summary := "zip entries: " + strings.Join(entryNames, ", ")
	if len(archiveReader.File) > len(entryNames) {
		summary += fmt.Sprintf(" (+%d more)", len(archiveReader.File)-len(entryNames))
	}

	return summary
}
