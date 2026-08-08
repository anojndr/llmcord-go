package support

import (
	"fmt"
	"mime"
	"os"
	"strings"

	searchtypes "llmcord-go/internal/searchtypes"
)

// ContentPart is a single multimodal message part carried inside a chat
// message (text, image URL/data, audio, video, document, or file payload).
type ContentPart = searchtypes.ContentPart

// AttachmentBytes extracts the raw bytes, mime type, and filename from a
// binary attachment content part.
func AttachmentBytes(part ContentPart) ([]byte, string, string, error) {
	attachmentBytes, ok := part[searchtypes.ContentFieldBytes].([]byte)
	if !ok {
		return nil, "", "", fmt.Errorf("decode attachment bytes: %w", os.ErrInvalid)
	}

	mimeType, _ := part[searchtypes.ContentFieldMIMEType].(string)

	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		return nil, "", "", fmt.Errorf("decode attachment mime type: %w", os.ErrInvalid)
	}

	filename, _ := part[searchtypes.ContentFieldFilename].(string)

	return attachmentBytes, mimeType, filename, nil
}

// ContentPartsText joins the text parts of a content part slice.
func ContentPartsText(parts []ContentPart) string {
	textParts := make([]string, 0, len(parts))

	for _, part := range parts {
		partType, _ := part[searchtypes.MessageTypeKey].(string)
		if partType != searchtypes.ContentTypeText {
			continue
		}

		textValue, _ := part[searchtypes.MessageTextKey].(string)
		if strings.TrimSpace(textValue) == "" {
			continue
		}

		textParts = append(textParts, textValue)
	}

	return JoinNonEmpty(textParts)
}

// NormalizedMIMEType lowercases and strips parameters from a content type.
func NormalizedMIMEType(contentType string) string {
	trimmedContentType := strings.TrimSpace(contentType)
	if trimmedContentType == "" {
		return ""
	}

	mediaType, _, err := mime.ParseMediaType(trimmedContentType)
	if err == nil {
		normalizedMediaType := strings.ToLower(strings.TrimSpace(mediaType))
		if normalizedMediaType != "" {
			return normalizedMediaType
		}
	}

	fallbackMediaType, _, _ := strings.Cut(trimmedContentType, ";")

	return strings.ToLower(strings.TrimSpace(fallbackMediaType))
}
