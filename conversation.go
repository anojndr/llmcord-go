package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bwmarrin/discordgo"
)

type attachmentPayload struct {
	attachment *discordgo.MessageAttachment
	body       []byte
}

type messageContentOptions struct {
	maxImages  int
	allowAudio bool
	allowVideo bool
}

type messageContentSummary struct {
	imageCount               int
	unsupportedAttachmentCnt int
}

func (instance *bot) buildConversation(
	ctx context.Context,
	sourceMessage *discordgo.Message,
	maxText int,
	contentOptions messageContentOptions,
	maxMessages int,
) ([]chatMessage, []string) {
	messages := make([]chatMessage, 0, maxMessages)
	warningSet := make(map[string]struct{})

	currentMessage := sourceMessage
	for currentMessage != nil && len(messages) < maxMessages {
		node := instance.nodes.getOrCreate(currentMessage.ID)
		node.mu.Lock()

		if !node.initialized {
			instance.initializeNode(ctx, currentMessage, node)
		}

		content, summary := buildMessageContent(node, maxText, contentOptions)
		if content != nil {
			message := chatMessage{
				Role:    node.role,
				Content: content,
			}
			messages = append(messages, message)
		}

		if runeCount(node.text) > maxText {
			appendUniqueWarning(
				warningSet,
				fmt.Sprintf("Warning: max %d characters per message", maxText),
			)
		}

		if summary.imageCount > contentOptions.maxImages {
			warningText := "Warning: can't see images"
			if contentOptions.maxImages > 0 {
				warningText = fmt.Sprintf(
					"Warning: max %d images per message",
					contentOptions.maxImages,
				)
			}

			appendUniqueWarning(warningSet, warningText)
		}

		if node.hasBadAttachments || summary.unsupportedAttachmentCnt > 0 {
			appendUniqueWarning(warningSet, "Warning: unsupported attachments")
		}

		if node.fetchParentFailed ||
			(node.parentMessage != nil && len(messages) == maxMessages) {
			appendUniqueWarning(
				warningSet,
				fmt.Sprintf("Warning: only using last %d messages", len(messages)),
			)
		}

		parentMessage := node.parentMessage
		node.mu.Unlock()

		currentMessage = parentMessage
	}

	reverseChatMessages(messages)

	return messages, sortedWarnings(warningSet)
}

func buildMessageContent(
	node *messageNode,
	maxText int,
	options messageContentOptions,
) (any, messageContentSummary) {
	selectedMedia, summary := selectMessageMedia(node.media, options)
	if len(selectedMedia) > 0 {
		parts := make([]contentPart, 0, len(selectedMedia)+1)

		textPart := make(contentPart)
		textPart["type"] = contentTypeText
		textPart["text"] = truncateRunes(node.text, maxText)
		parts = append(parts, textPart)
		parts = append(parts, selectedMedia...)

		return parts, summary
	}

	truncatedText := truncateRunes(node.text, maxText)
	if truncatedText == "" {
		return nil, summary
	}

	return truncatedText, summary
}

func selectMessageMedia(
	media []contentPart,
	options messageContentOptions,
) ([]contentPart, messageContentSummary) {
	selectedMedia := make([]contentPart, 0, len(media))

	var summary messageContentSummary

	imageCount := 0

	for _, part := range media {
		partType, _ := part["type"].(string)

		switch partType {
		case contentTypeImageURL:
			summary.imageCount++
			if imageCount >= options.maxImages {
				continue
			}

			selectedMedia = append(selectedMedia, part)
			imageCount++
		case contentTypeAudioData:
			if !options.allowAudio {
				summary.unsupportedAttachmentCnt++

				continue
			}

			selectedMedia = append(selectedMedia, part)
		case contentTypeVideoData:
			if !options.allowVideo {
				summary.unsupportedAttachmentCnt++

				continue
			}

			selectedMedia = append(selectedMedia, part)
		default:
			summary.unsupportedAttachmentCnt++
		}
	}

	return selectedMedia, summary
}

func (instance *bot) initializeNode(
	ctx context.Context,
	message *discordgo.Message,
	node *messageNode,
) {
	cleanedContent := trimBotMention(message.Content, instance.session.State.User.ID)
	payloads := instance.fetchSupportedAttachments(ctx, message.Attachments)

	node.role = messageRole(message, instance.session.State.User.ID)
	node.text = buildMessageText(message, cleanedContent, payloads)

	node.media = buildMediaParts(payloads)

	if node.role == "user" && (node.text != "" || len(node.media) > 0) {
		node.text = fmt.Sprintf("<@%s>: %s", message.Author.ID, node.text)
	}

	node.hasBadAttachments = len(message.Attachments) > supportedAttachmentCount(message.Attachments)
	node.parentMessage, node.fetchParentFailed = instance.resolveParentMessage(message)
	node.initialized = true
}

func messageRole(message *discordgo.Message, botUserID string) string {
	if message.Author != nil && message.Author.ID == botUserID {
		return messageRoleAssistant
	}

	return "user"
}

func supportedAttachmentCount(attachments []*discordgo.MessageAttachment) int {
	count := 0

	for _, attachment := range attachments {
		if attachment == nil {
			continue
		}

		if attachmentIsSupported(attachment.ContentType) {
			count++
		}
	}

	return count
}

func buildMessageText(
	message *discordgo.Message,
	cleanedContent string,
	payloads []attachmentPayload,
) string {
	textParts := make([]string, 0, 1+len(message.Embeds)+len(message.Components)+len(payloads))
	if cleanedContent != "" {
		textParts = append(textParts, cleanedContent)
	}

	for _, embed := range message.Embeds {
		if embed == nil {
			continue
		}

		footerText := ""
		if embed.Footer != nil {
			footerText = embed.Footer.Text
		}

		embedText := joinNonEmpty([]string{
			embed.Title,
			embed.Description,
			footerText,
		})
		if embedText != "" {
			textParts = append(textParts, embedText)
		}
	}

	for _, component := range message.Components {
		textParts = append(textParts, messageComponentTextParts(component)...)
	}

	for _, payload := range payloads {
		if attachmentIsText(payload.attachment.ContentType) {
			textParts = append(textParts, string(payload.body))
		}
	}

	return joinNonEmpty(textParts)
}

func messageComponentTextParts(component discordgo.MessageComponent) []string {
	switch typedComponent := component.(type) {
	case *discordgo.TextDisplay:
		if typedComponent.Content == "" {
			return nil
		}

		return []string{typedComponent.Content}
	case discordgo.TextDisplay:
		if typedComponent.Content == "" {
			return nil
		}

		return []string{typedComponent.Content}
	case *discordgo.ActionsRow:
		return nestedMessageComponentTextParts(typedComponent.Components)
	case discordgo.ActionsRow:
		return nestedMessageComponentTextParts(typedComponent.Components)
	case *discordgo.Section:
		textParts := nestedMessageComponentTextParts(typedComponent.Components)
		textParts = append(textParts, messageComponentTextParts(typedComponent.Accessory)...)

		return textParts
	case discordgo.Section:
		textParts := nestedMessageComponentTextParts(typedComponent.Components)
		textParts = append(textParts, messageComponentTextParts(typedComponent.Accessory)...)

		return textParts
	case *discordgo.Container:
		return nestedMessageComponentTextParts(typedComponent.Components)
	case discordgo.Container:
		return nestedMessageComponentTextParts(typedComponent.Components)
	default:
		return nil
	}
}

func nestedMessageComponentTextParts(components []discordgo.MessageComponent) []string {
	textParts := make([]string, 0, len(components))

	for _, component := range components {
		textParts = append(textParts, messageComponentTextParts(component)...)
	}

	return textParts
}

func buildMediaParts(payloads []attachmentPayload) []contentPart {
	parts := make([]contentPart, 0, len(payloads))

	for _, payload := range payloads {
		part, ok := attachmentPayloadToContentPart(payload)
		if !ok {
			continue
		}

		parts = append(parts, part)
	}

	return parts
}

func attachmentPayloadToContentPart(payload attachmentPayload) (contentPart, bool) {
	contentType := strings.TrimSpace(payload.attachment.ContentType)

	switch {
	case strings.HasPrefix(contentType, "image/"):
		part := make(contentPart)
		part["type"] = contentTypeImageURL
		part["image_url"] = map[string]string{
			"url": fmt.Sprintf(
				"data:%s;base64,%s",
				contentType,
				base64.StdEncoding.EncodeToString(payload.body),
			),
		}

		return part, true
	case strings.HasPrefix(contentType, "audio/"):
		return binaryAttachmentContentPart(contentTypeAudioData, payload), true
	case strings.HasPrefix(contentType, "video/"):
		return binaryAttachmentContentPart(contentTypeVideoData, payload), true
	default:
		return nil, false
	}
}

func binaryAttachmentContentPart(partType string, payload attachmentPayload) contentPart {
	part := make(contentPart)
	part["type"] = partType
	part[contentFieldBytes] = payload.body
	part[contentFieldMIMEType] = strings.TrimSpace(payload.attachment.ContentType)

	if payload.attachment.Filename != "" {
		part[contentFieldFilename] = payload.attachment.Filename
	}

	return part
}

func (instance *bot) fetchSupportedAttachments(
	ctx context.Context,
	attachments []*discordgo.MessageAttachment,
) []attachmentPayload {
	timeoutContext, cancelTimeout := context.WithTimeout(ctx, attachmentRequestTimeout)
	defer cancelTimeout()

	payloads := make([]attachmentPayload, 0, len(attachments))

	for _, attachment := range attachments {
		if attachment == nil {
			continue
		}

		if !attachmentIsSupported(attachment.ContentType) {
			continue
		}

		httpRequest, err := http.NewRequestWithContext(
			timeoutContext,
			http.MethodGet,
			attachment.URL,
			nil,
		)
		if err != nil {
			slog.Warn("create attachment request", "url", attachment.URL, "error", err)

			continue
		}

		httpResponse, err := instance.httpClient.Do(httpRequest)
		if err != nil {
			slog.Warn("download attachment", "url", attachment.URL, "error", err)

			continue
		}

		body, readErr := io.ReadAll(httpResponse.Body)
		closeErr := httpResponse.Body.Close()

		if readErr != nil {
			slog.Warn("read attachment", "url", attachment.URL, "error", readErr)

			continue
		}

		if closeErr != nil {
			slog.Warn("close attachment body", "url", attachment.URL, "error", closeErr)
		}

		if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
			slog.Warn(
				"attachment request failed",
				"url",
				attachment.URL,
				"status",
				httpResponse.StatusCode,
			)

			continue
		}

		payloads = append(payloads, attachmentPayload{
			attachment: attachment,
			body:       body,
		})
	}

	return payloads
}

func attachmentIsSupported(contentType string) bool {
	return attachmentIsText(contentType) ||
		strings.HasPrefix(strings.TrimSpace(contentType), "image/") ||
		strings.HasPrefix(strings.TrimSpace(contentType), "audio/") ||
		strings.HasPrefix(strings.TrimSpace(contentType), "video/")
}

func attachmentIsText(contentType string) bool {
	return strings.HasPrefix(strings.TrimSpace(contentType), "text/")
}

func filterContentPartsForOptions(
	parts []contentPart,
	options messageContentOptions,
) []contentPart {
	filteredParts := make([]contentPart, 0, len(parts))
	imageCount := 0

	for _, part := range parts {
		partType, _ := part["type"].(string)

		switch partType {
		case contentTypeText:
			filteredParts = append(filteredParts, part)
		case contentTypeImageURL:
			if imageCount >= options.maxImages {
				continue
			}

			filteredParts = append(filteredParts, part)
			imageCount++
		case contentTypeAudioData:
			if options.allowAudio {
				filteredParts = append(filteredParts, part)
			}
		case contentTypeVideoData:
			if options.allowVideo {
				filteredParts = append(filteredParts, part)
			}
		}
	}

	return filteredParts
}

func (instance *bot) resolveParentMessage(message *discordgo.Message) (*discordgo.Message, bool) {
	if message.MessageReference == nil && !messageMentionsUser(message, instance.session.State.User.ID) {
		previousMessage, found, err := instance.previousMessageInChannel(message)
		if err != nil {
			slog.Warn("fetch previous message", "channel_id", message.ChannelID, "error", err)

			return nil, true
		}

		expectedAuthorID := message.Author.ID
		if isDirectMessage(message) {
			expectedAuthorID = instance.session.State.User.ID
		}

		if found &&
			messageCanChain(previousMessage.Type) &&
			previousMessage.Author != nil &&
			previousMessage.Author.ID == expectedAuthorID {
			return previousMessage, false
		}
	}

	channel, err := instance.channelByID(message.ChannelID)
	if err != nil {
		slog.Warn("fetch current channel", "channel_id", message.ChannelID, "error", err)

		return nil, true
	}

	if channel.Type == discordgo.ChannelTypeGuildPublicThread &&
		message.MessageReference == nil &&
		channel.ParentID != "" {
		parentChannel, parentErr := instance.channelByID(channel.ParentID)
		if parentErr != nil {
			slog.Warn("fetch parent channel", "channel_id", channel.ParentID, "error", parentErr)

			return nil, true
		}

		if parentChannel.Type == discordgo.ChannelTypeGuildText {
			parentMessage, messageErr := instance.session.ChannelMessage(parentChannel.ID, channel.ID)
			if messageErr != nil {
				slog.Warn(
					"fetch thread starter message",
					"channel_id",
					parentChannel.ID,
					"message_id",
					channel.ID,
					"error",
					messageErr,
				)

				return nil, true
			}

			return parentMessage, false
		}
	}

	if message.MessageReference == nil {
		return nil, false
	}

	if message.ReferencedMessage != nil {
		return message.ReferencedMessage, false
	}

	referenceChannelID := message.MessageReference.ChannelID
	if referenceChannelID == "" {
		referenceChannelID = message.ChannelID
	}

	referencedMessage, err := instance.session.ChannelMessage(
		referenceChannelID,
		message.MessageReference.MessageID,
	)
	if err != nil {
		slog.Warn(
			"fetch referenced message",
			"channel_id",
			referenceChannelID,
			"message_id",
			message.MessageReference.MessageID,
			"error",
			err,
		)

		return nil, true
	}

	return referencedMessage, false
}

func messageCanChain(messageType discordgo.MessageType) bool {
	return messageType == discordgo.MessageTypeDefault ||
		messageType == discordgo.MessageTypeReply
}

func (instance *bot) previousMessageInChannel(
	message *discordgo.Message,
) (*discordgo.Message, bool, error) {
	messages, err := instance.session.ChannelMessages(message.ChannelID, 1, message.ID, "", "")
	if err != nil {
		return nil, false, fmt.Errorf("load channel messages: %w", err)
	}

	if len(messages) == 0 {
		return nil, false, nil
	}

	return messages[0], true, nil
}

func (instance *bot) channelByID(channelID string) (*discordgo.Channel, error) {
	if instance.session.State != nil {
		channel, err := instance.session.State.Channel(channelID)
		if err == nil {
			return channel, nil
		}
	}

	channel, err := instance.session.Channel(channelID)
	if err != nil {
		return nil, fmt.Errorf("fetch channel %s: %w", channelID, err)
	}

	return channel, nil
}
