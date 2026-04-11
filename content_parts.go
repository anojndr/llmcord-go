package main

import "strings"

const imageOnlyQueryPlaceholder = "."

func requestMessagesWithImageOnlyQueryPlaceholder(messages []chatMessage) []chatMessage {
	if len(messages) == 0 {
		return messages
	}

	normalizedMessages := make([]chatMessage, len(messages))
	changed := false

	for index, message := range messages {
		normalizedContent, contentChanged := messageContentWithImageOnlyQueryPlaceholder(
			message.Role,
			message.Content,
		)
		if contentChanged {
			changed = true
		}

		normalizedMessages[index] = chatMessage{
			Role:    message.Role,
			Content: normalizedContent,
		}
	}

	if !changed {
		return messages
	}

	return normalizedMessages
}

func messageContentWithImageOnlyQueryPlaceholder(role string, content any) (any, bool) {
	if !strings.EqualFold(strings.TrimSpace(role), messageRoleUser) {
		return content, false
	}

	parts, ok := content.([]contentPart)
	if !ok || !contentPartsNeedImageOnlyQueryPlaceholder(parts) {
		return content, false
	}

	normalizedParts := make([]contentPart, 0, len(parts)+1)
	normalizedParts = append(normalizedParts, contentPart{
		"type": contentTypeText,
		"text": imageOnlyQueryPlaceholder,
	})

	for _, part := range parts {
		partType, _ := part["type"].(string)
		if partType == contentTypeText {
			textValue, _ := part["text"].(string)
			if strings.TrimSpace(textValue) == "" {
				continue
			}
		}

		normalizedParts = append(normalizedParts, cloneContentPart(part))
	}

	return normalizedParts, true
}

func contentPartsNeedImageOnlyQueryPlaceholder(parts []contentPart) bool {
	hasImage := false

	for _, part := range parts {
		partType, _ := part["type"].(string)

		switch partType {
		case contentTypeText:
			textValue, _ := part["text"].(string)
			if strings.TrimSpace(textValue) != "" {
				return false
			}
		case contentTypeImageURL:
			hasImage = true
		default:
			return false
		}
	}

	return hasImage
}
