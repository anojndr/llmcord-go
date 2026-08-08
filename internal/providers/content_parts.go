package providers

import (
	"strings"

	searchtypes "llmcord-go/internal/searchtypes"
)

// FileOrImageOnlyQueryPlaceholder is the placeholder query for file-only turns.
const FileOrImageOnlyQueryPlaceholder = "."

// RequestMessagesWithFileOrImageOnlyQueryPlaceholder adds the placeholder query.
func RequestMessagesWithFileOrImageOnlyQueryPlaceholder(messages []ChatMessage) []ChatMessage {
	if len(messages) == 0 {
		return messages
	}

	normalizedMessages := make([]ChatMessage, len(messages))
	changed := false

	for index, message := range messages {
		normalizedContent, contentChanged := messageContentWithFileOrImageOnlyQueryPlaceholder(
			message.Role,
			message.Content,
		)
		if contentChanged {
			changed = true
		}

		normalizedMessages[index] = ChatMessage{
			Role:    message.Role,
			Content: normalizedContent,
		}
	}

	if !changed {
		return messages
	}

	return normalizedMessages
}

func messageContentWithFileOrImageOnlyQueryPlaceholder(role string, content any) (any, bool) {
	if !strings.EqualFold(strings.TrimSpace(role), searchtypes.MessageRoleUser) {
		return content, false
	}

	switch typedContent := content.(type) {
	case nil:
		return FileOrImageOnlyQueryPlaceholder, true
	case string:
		if strings.TrimSpace(typedContent) == "" {
			return FileOrImageOnlyQueryPlaceholder, true
		}

		return content, false
	case []ContentPart:
		if !contentPartsNeedFileOrImageOnlyQueryPlaceholder(typedContent) {
			return content, false
		}

		normalizedParts := make([]ContentPart, 0, len(typedContent)+1)
		normalizedParts = append(normalizedParts, ContentPart{
			searchtypes.MessageTypeKey: searchtypes.ContentTypeText,
			searchtypes.MessageTextKey: FileOrImageOnlyQueryPlaceholder,
		})

		for _, part := range typedContent {
			partType, _ := part[searchtypes.MessageTypeKey].(string)
			if partType == searchtypes.ContentTypeText {
				textValue, _ := part[searchtypes.MessageTextKey].(string)
				if strings.TrimSpace(textValue) == "" {
					continue
				}
			}

			normalizedParts = append(normalizedParts, cloneContentPart(part))
		}

		return normalizedParts, true
	default:
		return content, false
	}
}

func contentPartsNeedFileOrImageOnlyQueryPlaceholder(parts []ContentPart) bool {
	hasFileOrImage := false

	for _, part := range parts {
		partType, _ := part["type"].(string)

		switch partType {
		case searchtypes.ContentTypeText:
			textValue, _ := part["text"].(string)
			if strings.TrimSpace(textValue) != "" {
				return false
			}
		case searchtypes.ContentTypeDocument, searchtypes.ContentTypeFileData, searchtypes.ContentTypeImageURL:
			hasFileOrImage = true
		default:
			return false
		}
	}

	return hasFileOrImage
}

func cloneContentPart(part ContentPart) ContentPart {
	clonedPart := make(ContentPart, len(part))

	for key, value := range part {
		if bytesValue, ok := value.([]byte); ok {
			clonedBytes := make([]byte, len(bytesValue))
			copy(clonedBytes, bytesValue)

			clonedPart[key] = clonedBytes

			continue
		}

		clonedPart[key] = value
	}

	return clonedPart
}
