package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

type attachmentPayload struct {
	attachment  *discordgo.MessageAttachment
	body        []byte
	contentType string
}

type messageContentOptions struct {
	maxImages                int
	allowAudio               bool
	allowDocuments           bool
	allowFiles               bool
	allowedDocumentMIMETypes map[string]struct{}
	allowVideo               bool
}

type messageContentSummary struct {
	imageCount               int
	unsupportedAttachmentCnt int
}

const (
	attachmentDownloadWarningText  = "Warning: failed to download some attachments"
	attachmentDownloadFallbackText = "I couldn't download one or more attachments, so I may miss attachment details."
	maxAttachmentRetryShift        = 6
	discordAttachmentCDNHost       = "cdn.discordapp.com"
	discordAttachmentMediaHost     = "media.discordapp.net"
	attachmentDownloadConcurrency  = 4
	nodeInitializationTaskCount    = 2
)

var errAttachmentRetryContextDone = errors.New("attachment retry context done")

func (instance *bot) buildConversation(
	ctx context.Context,
	sourceMessage *discordgo.Message,
	maxText int,
	contentOptions messageContentOptions,
	maxMessages int,
	useGeminiMediaAnalysis bool,
	usePDFExtraction bool,
) ([]chatMessage, []string) {
	messages := make([]chatMessage, 0, maxMessages)
	warningSet := make(map[string]struct{})
	preprocessedMessageIDs := instance.attachmentPreprocessingMessageIDSet(
		ctx,
		sourceMessage,
	)

	currentMessage := sourceMessage
	for currentMessage != nil && len(messages) < maxMessages {
		node := instance.nodes.getOrCreate(currentMessage.ID)
		node.mu.Lock()

		if !node.initialized {
			instance.initializeNode(ctx, currentMessage, node)
		}

		content, summary := buildMessageContent(node, maxText, contentOptions)
		if _, ok := preprocessedMessageIDs[strings.TrimSpace(currentMessage.ID)]; ok &&
			(useGeminiMediaAnalysis || usePDFExtraction) {
			summary.unsupportedAttachmentCnt -= unsupportedPreprocessedPartCount(
				node.media,
				contentOptions,
				useGeminiMediaAnalysis,
				usePDFExtraction,
			)
			if summary.unsupportedAttachmentCnt < 0 {
				summary.unsupportedAttachmentCnt = 0
			}
		}

		if content != nil {
			message := chatMessage{
				Role:    node.role,
				Content: content,
			}
			messages = append(messages, message)
		}

		appendConversationWarnings(
			warningSet,
			node,
			summary,
			maxText,
			contentOptions,
			len(messages),
			maxMessages,
		)

		parentMessage := node.parentMessage
		if stopsAtDirectReplyTarget(sourceMessage, currentMessage, node.role) {
			parentMessage = nil
		}
		node.mu.Unlock()

		currentMessage = parentMessage
	}

	reverseChatMessages(messages)

	return messages, sortedWarnings(warningSet)
}

func appendConversationWarnings(
	warningSet map[string]struct{},
	node *messageNode,
	summary messageContentSummary,
	maxText int,
	contentOptions messageContentOptions,
	messageCount int,
	maxMessages int,
) {
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

	if node.attachmentDownloadFailed {
		appendUniqueWarning(warningSet, attachmentDownloadWarningText)
	}

	if node.fetchParentFailed || (node.parentMessage != nil && messageCount == maxMessages) {
		appendUniqueWarning(
			warningSet,
			fmt.Sprintf("Warning: only using last %d messages", messageCount),
		)
	}
}

func stopsAtDirectReplyTarget(
	sourceMessage *discordgo.Message,
	currentMessage *discordgo.Message,
	currentRole string,
) bool {
	if sourceMessage == nil || sourceMessage.MessageReference == nil || currentMessage == nil {
		return false
	}

	replyTargetID := strings.TrimSpace(sourceMessage.MessageReference.MessageID)
	if replyTargetID == "" || strings.TrimSpace(currentMessage.ID) != replyTargetID {
		return false
	}

	return currentRole != messageRoleAssistant
}

func buildMessageContent(
	node *messageNode,
	maxText int,
	options messageContentOptions,
) (any, messageContentSummary) {
	selectedMedia, summary := selectMessageMedia(node.media, options)
	truncatedText := truncateRunes(
		appendInlineAttachmentText(
			node.text,
			inlineTextAttachmentContent(node.media, options),
		),
		maxText,
	)

	if len(selectedMedia) > 0 {
		parts := make([]contentPart, 0, len(selectedMedia)+1)

		textPart := make(contentPart)
		textPart["type"] = contentTypeText
		textPart["text"] = truncatedText
		parts = append(parts, textPart)
		parts = append(parts, selectedMedia...)

		return parts, summary
	}

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
			if options.allowAudio {
				selectedMedia = append(selectedMedia, part)
			}
		case contentTypeDocument:
			if attachmentPartShouldInlineAsText(part, options) {
				continue
			}

			if messageContentOptionsAllowsDocumentPart(options, part) {
				selectedMedia = append(selectedMedia, part)
			}
		case contentTypeFileData:
			if attachmentPartShouldInlineAsText(part, options) {
				continue
			}

			if options.allowFiles {
				selectedMedia = append(selectedMedia, part)
			}
		case contentTypeVideoData:
			if options.allowVideo {
				selectedMedia = append(selectedMedia, part)
			}
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
	botUserID := instance.session.State.User.ID
	cleanedContent := trimBotMention(message.Content, botUserID)

	var (
		payloads                 []attachmentPayload
		attachmentDownloadFailed bool
		parentMessage            *discordgo.Message
		fetchParentFailed        bool
		initializationWaitGroup  sync.WaitGroup
	)

	initializationWaitGroup.Add(nodeInitializationTaskCount)

	go func() {
		defer initializationWaitGroup.Done()

		payloads, attachmentDownloadFailed = instance.fetchSupportedAttachments(
			ctx,
			message.Attachments,
		)
	}()

	go func() {
		defer initializationWaitGroup.Done()

		parentMessage, fetchParentFailed = instance.resolveParentMessage(message)
	}()

	initializationWaitGroup.Wait()

	node.role = messageRole(message, botUserID)
	node.text = buildMessageText(message, cleanedContent, payloads)
	node.urlScanText = ""
	node.attachmentDownloadFailed = attachmentDownloadFailed

	if node.role == messageRoleUser {
		node.urlScanText = normalizedURLExtractionText(
			buildMessageText(message, cleanedContent, nil),
		)
	}

	node.media = buildMediaParts(payloads)

	if node.role == messageRoleUser &&
		node.attachmentDownloadFailed &&
		strings.TrimSpace(node.text) == "" &&
		len(node.media) == 0 {
		node.text = attachmentDownloadFallbackText
	}

	node.hasBadAttachments = len(message.Attachments) > supportedAttachmentCount(message.Attachments)
	node.parentMessage = parentMessage
	node.fetchParentFailed = fetchParentFailed
	node.initialized = true
	instance.nodes.cacheLockedNode(message.ID, node)
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

		_, err := discordAttachmentDownloadURL(attachment.URL)
		if err == nil {
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
	textParts := make([]string, 0, 1+len(message.Embeds)+len(message.Components))
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

	if attachmentText := attachmentSummaryText(payloads); attachmentText != "" {
		textParts = append(textParts, attachmentText)
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
	contentType := attachmentPayloadContentType(payload)
	partType := attachmentContentPartType(contentType)

	switch partType {
	case contentTypeImageURL:
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
	case contentTypeAudioData, contentTypeDocument, contentTypeFileData, contentTypeVideoData:
		return binaryAttachmentContentPart(partType, payload, contentType), true
	default:
		return nil, false
	}
}

func binaryAttachmentContentPart(
	partType string,
	payload attachmentPayload,
	contentType string,
) contentPart {
	part := make(contentPart)
	part["type"] = partType
	part[contentFieldBytes] = payload.body
	part[contentFieldMIMEType] = strings.TrimSpace(contentType)

	if payload.attachment.Filename != "" {
		part[contentFieldFilename] = payload.attachment.Filename
	}

	return part
}

func (instance *bot) fetchSupportedAttachments(
	ctx context.Context,
	attachments []*discordgo.MessageAttachment,
) ([]attachmentPayload, bool) {
	timeoutContext, cancelTimeout := context.WithTimeout(ctx, attachmentRequestTimeout)
	defer cancelTimeout()

	results := runTasksConcurrently(
		timeoutContext,
		attachmentDownloadConcurrency,
		len(attachments),
		func(taskContext context.Context, index int) (attachmentPayload, error) {
			attachment := attachments[index]
			if attachment == nil {
				return attachmentPayload{}, fmt.Errorf("missing attachment: %w", os.ErrInvalid)
			}

			downloadURL, err := discordAttachmentDownloadURL(attachment.URL)
			if err != nil {
				slog.Warn("invalid attachment url", "error", err)

				return attachmentPayload{}, err
			}

			body, err := instance.fetchAttachmentWithRetry(taskContext, downloadURL)
			if err != nil {
				return attachmentPayload{}, err
			}

			return attachmentPayload{
				attachment:  attachment,
				body:        body,
				contentType: attachmentClassification(attachment, body),
			}, nil
		},
	)

	payloads := make([]attachmentPayload, 0, len(results))
	anyDownloadFailed := false

	for _, result := range results {
		if result.err != nil {
			anyDownloadFailed = true

			continue
		}

		payloads = append(payloads, result.value)
	}

	return payloads, anyDownloadFailed
}

func (instance *bot) fetchAttachmentWithRetry(
	ctx context.Context,
	attachmentURL string,
) ([]byte, error) {
	maxAttempts := attachmentDownloadMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		body, err := instance.fetchAttachmentAttempt(ctx, attachmentURL)
		if err == nil {
			return body, nil
		}

		lastErr = err

		slog.Warn(
			"download attachment",
			"attempt",
			attempt,
			"max_attempts",
			maxAttempts,
			"error",
			lastErr,
		)

		if !attachmentDownloadShouldRetry(lastErr) || attempt == maxAttempts {
			return nil, lastErr
		}

		retryDelay := attachmentRetryDelay(attempt)
		if retryDelay <= 0 {
			continue
		}

		timer := time.NewTimer(retryDelay)

		select {
		case <-ctx.Done():
			timer.Stop()

			return nil, fmt.Errorf("%w: %w", errAttachmentRetryContextDone, ctx.Err())
		case <-timer.C:
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, os.ErrInvalid
}

func (instance *bot) fetchAttachmentAttempt(
	ctx context.Context,
	attachmentURL string,
) ([]byte, error) {
	httpRequest, requestErr := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		attachmentURL,
		nil,
	)
	if requestErr != nil {
		return nil, fmt.Errorf("create attachment request: %w", requestErr)
	}

	httpResponse, err := instance.performAttachmentRequest(httpRequest)
	if err != nil {
		return nil, err
	}

	body, readErr := io.ReadAll(httpResponse.Body)
	closeErr := httpResponse.Body.Close()

	if readErr != nil {
		slog.Warn("read attachment", "error", readErr)

		return nil, fmt.Errorf("read attachment response body: %w", readErr)
	}

	if closeErr != nil {
		slog.Warn("close attachment body", "error", closeErr)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, attachmentDownloadStatusError{statusCode: httpResponse.StatusCode}
	}

	return body, nil
}

func (instance *bot) performAttachmentRequest(httpRequest *http.Request) (*http.Response, error) {
	if instance.httpClient == nil {
		return nil, fmt.Errorf("missing attachment http client: %w", os.ErrInvalid)
	}

	transport := instance.httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	httpResponse, err := transport.RoundTrip(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("send attachment request: %w", err)
	}

	return httpResponse, nil
}

func attachmentDownloadShouldRetry(err error) bool {
	var statusErr attachmentDownloadStatusError
	if errors.As(err, &statusErr) {
		if statusErr.statusCode == http.StatusTooManyRequests {
			return true
		}

		return statusErr.statusCode >= http.StatusInternalServerError
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}

	var netErr net.Error

	return errors.As(err, &netErr)
}

func attachmentRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}

	baseDelay := attachmentRetryBaseDelay
	if baseDelay <= 0 {
		return 0
	}

	shift := min(attempt-1, maxAttachmentRetryShift)

	return baseDelay << shift
}

func discordAttachmentDownloadURL(rawURL string) (string, error) {
	trimmedURL := strings.TrimSpace(rawURL)
	if trimmedURL == "" {
		return "", fmt.Errorf("empty attachment url: %w", os.ErrInvalid)
	}

	parsedURL, err := url.Parse(trimmedURL)
	if err != nil {
		return "", fmt.Errorf("parse attachment url %q: %w", rawURL, err)
	}

	if !strings.EqualFold(parsedURL.Scheme, "https") {
		return "", fmt.Errorf("unsupported attachment scheme %q: %w", parsedURL.Scheme, os.ErrInvalid)
	}

	host := strings.ToLower(strings.TrimSpace(parsedURL.Hostname()))
	if !discordAttachmentHostAllowed(host) {
		return "", fmt.Errorf("unsupported attachment host %q: %w", host, os.ErrInvalid)
	}

	parsedURL.Fragment = ""

	return parsedURL.String(), nil
}

func discordAttachmentHostAllowed(host string) bool {
	switch host {
	case discordAttachmentCDNHost, discordAttachmentMediaHost:
		return true
	default:
		return false
	}
}

type attachmentDownloadStatusError struct {
	statusCode int
}

func (err attachmentDownloadStatusError) Error() string {
	return fmt.Sprintf("attachment request failed with status %d", err.statusCode)
}

func attachmentIsText(contentType string) bool {
	return strings.HasPrefix(strings.TrimSpace(contentType), "text/")
}

func attachmentIsDocument(contentType string) bool {
	switch normalizedMIMEType(contentType) {
	case mimeTypePDF, mimeTypeDOCX, mimeTypePPTX:
		return true
	default:
		return false
	}
}

func messageContentOptionsAllowsDocumentPart(
	options messageContentOptions,
	part contentPart,
) bool {
	if !options.allowDocuments {
		return false
	}

	if len(options.allowedDocumentMIMETypes) == 0 {
		return true
	}

	mimeType, _ := part[contentFieldMIMEType].(string)

	_, allowed := options.allowedDocumentMIMETypes[normalizedMIMEType(mimeType)]

	return allowed
}

func normalizedMIMEType(contentType string) string {
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

func documentMIMETypeSet(mimeTypes ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(mimeTypes))

	for _, mimeType := range mimeTypes {
		normalized := normalizedMIMEType(mimeType)
		if normalized == "" {
			continue
		}

		set[normalized] = struct{}{}
	}

	return set
}

func allowedGeminiDocumentMIMETypes() map[string]struct{} {
	return documentMIMETypeSet(mimeTypePDF)
}

func attachmentContentType(attachment *discordgo.MessageAttachment) string {
	if attachment == nil {
		return ""
	}

	return resolvedAttachmentContentType(
		strings.TrimSpace(attachment.ContentType),
		inferredAttachmentContentType(attachment.Filename, attachment.URL),
		"",
	)
}

func inferredAttachmentContentType(filename, rawURL string) string {
	for _, candidate := range []string{filename, attachmentURLPath(rawURL)} {
		extension := strings.ToLower(strings.TrimSpace(path.Ext(candidate)))
		if extension == "" {
			continue
		}

		if inferredContentType := strings.TrimSpace(mime.TypeByExtension(extension)); inferredContentType != "" {
			return inferredContentType
		}
	}

	return ""
}

func attachmentURLPath(rawURL string) string {
	parsedURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return rawURL
	}

	return parsedURL.Path
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
		case contentTypeDocument:
			if attachmentPartShouldInlineAsText(part, options) {
				continue
			}

			if messageContentOptionsAllowsDocumentPart(options, part) {
				filteredParts = append(filteredParts, part)
			}
		case contentTypeFileData:
			if attachmentPartShouldInlineAsText(part, options) {
				continue
			}

			if options.allowFiles {
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

func inlineTextAttachmentContent(parts []contentPart, options messageContentOptions) string {
	textParts := make([]string, 0, len(parts))

	for _, part := range parts {
		if !attachmentPartShouldInlineAsText(part, options) {
			continue
		}

		attachmentBytes, mimeType, filename, err := attachmentBinaryData(part)
		if err != nil {
			continue
		}

		textValue := attachmentInlineText(attachmentBytes, mimeType, filename)
		if textValue == "" {
			continue
		}

		textParts = append(textParts, textValue)
	}

	return joinNonEmpty(textParts)
}

func appendInlineAttachmentText(messageText, attachmentText string) string {
	trimmedAttachmentText := strings.TrimSpace(attachmentText)
	if trimmedAttachmentText == "" {
		return messageText
	}

	trimmedMessageText := strings.TrimSpace(messageText)
	if trimmedMessageText == "" {
		return trimmedAttachmentText
	}

	if strings.HasSuffix(messageText, ": ") {
		return messageText + trimmedAttachmentText
	}

	return joinNonEmpty([]string{messageText, trimmedAttachmentText})
}

func attachmentPartShouldInlineAsText(
	part contentPart,
	options messageContentOptions,
) bool {
	partType, _ := part["type"].(string)
	if partType != contentTypeDocument && partType != contentTypeFileData {
		return false
	}

	if partType == contentTypeDocument && messageContentOptionsAllowsDocumentPart(options, part) {
		return false
	}

	if partType == contentTypeFileData && options.allowFiles {
		return false
	}

	_, mimeType, filename, err := attachmentBinaryData(part)
	if err != nil {
		return false
	}

	return attachmentIsTextLike(mimeType, filename)
}

func unsupportedPreprocessedPartCount(
	media []contentPart,
	options messageContentOptions,
	useGeminiMediaAnalysis bool,
	usePDFExtraction bool,
) int {
	count := 0

	for _, part := range media {
		partType, _ := part["type"].(string)

		switch partType {
		case contentTypeAudioData:
			if useGeminiMediaAnalysis && !options.allowAudio {
				count++
			}
		case contentTypeVideoData:
			if useGeminiMediaAnalysis && !options.allowVideo {
				count++
			}
		case contentTypeDocument:
			if usePDFExtraction && !messageContentOptionsAllowsDocumentPart(options, part) {
				count++
			}
		case contentTypeFileData:
			if !options.allowFiles && !attachmentPartShouldInlineAsText(part, options) {
				count++
			}
		}
	}

	return count
}

func (instance *bot) resolveParentMessage(message *discordgo.Message) (*discordgo.Message, bool) {
	implicitParent, failed := instance.resolveImplicitParentMessage(message)
	if failed || implicitParent != nil {
		return implicitParent, failed
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
			return instance.threadStarterMessage(parentChannel.ID, channel.ID)
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

	return instance.fetchReferencedParentMessage(referenceChannelID, message.MessageReference.MessageID)
}

func (instance *bot) resolveImplicitParentMessage(message *discordgo.Message) (*discordgo.Message, bool) {
	if message.MessageReference != nil || messageMentionsBot(message, instance.session.State.User.ID) {
		return nil, false
	}

	previousMessage, found, err := instance.previousMessageInChannel(message)
	if err != nil {
		slog.Warn("fetch previous message", "channel_id", message.ChannelID, "error", err)

		return nil, true
	}

	if !found || !previousMessageCanChain(message, previousMessage, instance.session.State.User.ID) {
		return nil, false
	}

	return previousMessage, false
}

func previousMessageCanChain(
	message *discordgo.Message,
	previousMessage *discordgo.Message,
	botUserID string,
) bool {
	expectedAuthorID := message.Author.ID
	if isDirectMessage(message) {
		expectedAuthorID = botUserID
	}

	return messageCanChain(previousMessage.Type) &&
		previousMessage.Author != nil &&
		previousMessage.Author.ID == expectedAuthorID
}

func (instance *bot) threadStarterMessage(parentChannelID, messageID string) (*discordgo.Message, bool) {
	parentMessage, err := instance.session.ChannelMessage(parentChannelID, messageID)
	if err != nil {
		slog.Warn(
			"fetch thread starter message",
			"channel_id",
			parentChannelID,
			"message_id",
			messageID,
			"error",
			err,
		)

		return nil, true
	}

	return parentMessage, false
}

func (instance *bot) fetchReferencedParentMessage(
	referenceChannelID, messageID string,
) (*discordgo.Message, bool) {
	referencedMessage, err := instance.session.ChannelMessage(referenceChannelID, messageID)
	if err != nil {
		slog.Warn(
			"fetch referenced message",
			"channel_id",
			referenceChannelID,
			"message_id",
			messageID,
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
