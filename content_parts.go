package main

import "strings"

const fileOrImageOnlyQueryPlaceholder = "."

func requestMessagesWithFileOrImageOnlyQueryPlaceholder(messages []chatMessage) []chatMessage {
	if len(messages) == 0 {
		return messages
	}

	normalizedMessages := make([]chatMessage, len(messages))
	changed := false

	for index, message := range messages {
		normalizedContent, contentChanged := messageContentWithFileOrImageOnlyQueryPlaceholder(
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

func messageContentWithFileOrImageOnlyQueryPlaceholder(role string, content any) (any, bool) {
	if !strings.EqualFold(strings.TrimSpace(role), messageRoleUser) {
		return content, false
	}

	switch typedContent := content.(type) {
	case nil:
		return fileOrImageOnlyQueryPlaceholder, true
	case string:
		if strings.TrimSpace(typedContent) == "" {
			return fileOrImageOnlyQueryPlaceholder, true
		}

		return content, false
	case []contentPart:
		if !contentPartsNeedFileOrImageOnlyQueryPlaceholder(typedContent) {
			return content, false
		}

		normalizedParts := make([]contentPart, 0, len(typedContent)+1)
		normalizedParts = append(normalizedParts, contentPart{
			"type": contentTypeText,
			"text": fileOrImageOnlyQueryPlaceholder,
		})

		for _, part := range typedContent {
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
	default:
		return content, false
	}
}

func contentPartsNeedFileOrImageOnlyQueryPlaceholder(parts []contentPart) bool {
	hasFileOrImage := false

	for _, part := range parts {
		partType, _ := part["type"].(string)

		switch partType {
		case contentTypeText:
			textValue, _ := part["text"].(string)
			if strings.TrimSpace(textValue) != "" {
				return false
			}
		case contentTypeDocument, contentTypeFileData, contentTypeImageURL:
			hasFileOrImage = true
		default:
			return false
		}
	}

	return hasFileOrImage
}
