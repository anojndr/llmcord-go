package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/genai"
)

type segmentAccumulator struct {
	maxLength int
	segments  []string
}

type renderSpec struct {
	content string
	color   int
	actions responseActions
}

type responseActions struct {
	showSources  bool
	showThinking bool
	showRentry   bool
}

type pendingResponse struct {
	messageID string
	node      *messageNode
}

type responseTracker struct {
	sourceMessage    *discordgo.Message
	searchMetadata   *searchMetadata
	modelName        string
	responseMessages []*discordgo.Message
	pendingResponses []pendingResponse
	renderedSpecs    []renderSpec
	progressActive   bool
	responseVisible  bool
}

func newSegmentAccumulator(maxLength int) segmentAccumulator {
	return segmentAccumulator{
		maxLength: maxLength,
		segments:  []string{""},
	}
}

func (accumulator *segmentAccumulator) appendText(text string) bool {
	splitOccurred := false
	remainingText := text

	for remainingText != "" {
		lastIndex := len(accumulator.segments) - 1

		availableRunes := accumulator.maxLength - runeCount(accumulator.segments[lastIndex])
		if availableRunes == 0 {
			accumulator.segments = append(accumulator.segments, "")
			lastIndex = len(accumulator.segments) - 1
			availableRunes = accumulator.maxLength
			splitOccurred = true
		}

		prefix, suffix := splitRunesPrefix(remainingText, availableRunes)
		accumulator.segments[lastIndex] += prefix
		remainingText = suffix

		if remainingText != "" {
			accumulator.segments = append(accumulator.segments, "")
			splitOccurred = true
		}
	}

	return splitOccurred
}

func (accumulator *segmentAccumulator) joined() string {
	return strings.Join(accumulator.segments, "")
}

func visibleResponseText(thinkingText string, answerText string) string {
	switch {
	case thinkingText == "":
		return answerText
	case answerText == "":
		return "**Thinking**\n" + thinkingText
	default:
		return "**Thinking**\n" + thinkingText + "\n\n**Answer**\n" + answerText
	}
}

const thinkingResponsePrefix = "**Thinking**\n"
const answerResponseSeparator = "\n\n**Answer**\n"

func extractThinkingText(fullText string) string {
	trimmedText := strings.TrimSpace(fullText)
	if !strings.HasPrefix(trimmedText, thinkingResponsePrefix) {
		return ""
	}

	thinkingBody := strings.TrimPrefix(trimmedText, thinkingResponsePrefix)

	thinkingOnly, _, found := strings.Cut(thinkingBody, answerResponseSeparator)
	if !found {
		return strings.TrimSpace(thinkingBody)
	}

	return strings.TrimSpace(thinkingOnly)
}

func visibleResponseSegments(thinkingText string, answerText string, maxLength int) []string {
	displayText := visibleResponseText(thinkingText, answerText)
	if displayText == "" {
		return nil
	}

	accumulator := newSegmentAccumulator(maxLength)
	_ = accumulator.appendText(displayText)

	return accumulator.renderSegments()
}

func (accumulator *segmentAccumulator) renderSegments() []string {
	if len(accumulator.segments) == 0 {
		return nil
	}

	segments := make([]string, 0, len(accumulator.segments))
	segments = append(segments, accumulator.segments...)

	if len(segments) == 1 && segments[0] == "" {
		return nil
	}

	return segments
}

func newResponseTracker(
	sourceMessage *discordgo.Message,
	modelName string,
) *responseTracker {
	tracker := new(responseTracker)
	tracker.sourceMessage = sourceMessage
	tracker.modelName = strings.TrimSpace(modelName)

	return tracker
}

func (tracker *responseTracker) release(store *messageNodeStore, fullText string, thinkingText string) {
	for _, pending := range tracker.pendingResponses {
		pending.node.role = messageRoleAssistant
		pending.node.text = fullText
		pending.node.thinkingText = thinkingText
		pending.node.urlScanText = ""
		pending.node.searchMetadata = cloneSearchMetadata(tracker.searchMetadata)
		pending.node.parentMessage = tracker.sourceMessage
		pending.node.initialized = true

		if store != nil {
			store.cacheLockedNode(pending.messageID, pending.node)
		}

		pending.node.mu.Unlock()
	}
}

func (instance *bot) generateAndSendResponse(
	ctx context.Context,
	request chatCompletionRequest,
	tracker *responseTracker,
	warnings []string,
	usePlainResponses bool,
) error {
	maxLength := embedResponseMaxLength
	if usePlainResponses {
		maxLength = plainResponseMaxLength
	}

	accumulator := newSegmentAccumulator(maxLength)
	thinkingAccumulator := newSegmentAccumulator(maxLength)
	tracker.modelName = strings.TrimSpace(request.ConfiguredModel)

	var finishReason string

	lastRenderTime := time.Time{}

	streamErr := instance.chatCompletions.streamChatCompletion(ctx, request, func(delta streamDelta) error {
		return instance.handleGeneratedStreamDelta(
			ctx,
			tracker,
			warnings,
			&accumulator,
			&thinkingAccumulator,
			delta,
			&finishReason,
			&lastRenderTime,
			maxLength,
			usePlainResponses,
		)
	})
	if streamErr != nil && finishReason == "" {
		finishReason = "error"
	}

	responseErr := instance.renderFinalResponse(
		ctx,
		tracker,
		warnings,
		&accumulator,
		thinkingAccumulator.joined(),
		finishReason,
		usePlainResponses,
	)
	if responseErr == nil && streamErr != nil {
		responseErr = fmt.Errorf("stream response: %w", streamErr)
	}

	finalText := visibleResponseText(thinkingAccumulator.joined(), accumulator.joined())

	if responseErr != nil {
		errorText := userFacingResponseError(responseErr)

		renderErr := instance.renderFailureResponse(tracker, errorText, usePlainResponses)
		if renderErr != nil {
			responseErr = errors.Join(responseErr, fmt.Errorf("render failure response: %w", renderErr))
		} else {
			finalText = responseTextWithError(finalText, errorText)
		}
	}

	tracker.release(instance.nodes, finalText, thinkingAccumulator.joined())
	instance.nodes.persistBestEffort()

	return responseErr
}

func (instance *bot) handleGeneratedStreamDelta(
	ctx context.Context,
	tracker *responseTracker,
	warnings []string,
	answerAccumulator *segmentAccumulator,
	thinkingAccumulator *segmentAccumulator,
	delta streamDelta,
	finishReason *string,
	lastRenderTime *time.Time,
	maxLength int,
	usePlainResponses bool,
) error {
	splitOccurred := false
	if delta.Thinking != "" {
		splitOccurred = thinkingAccumulator.appendText(delta.Thinking) || splitOccurred
	}

	if delta.Content != "" {
		splitOccurred = answerAccumulator.appendText(delta.Content) || splitOccurred
	}

	if delta.FinishReason != "" {
		*finishReason = delta.FinishReason
	}

	if usePlainResponses {
		return nil
	}

	segments := visibleResponseSegments(
		thinkingAccumulator.joined(),
		answerAccumulator.joined(),
		maxLength,
	)
	if !shouldRenderProgress(segments, splitOccurred, *lastRenderTime) {
		return nil
	}

	err := instance.renderEmbedResponse(
		ctx,
		tracker,
		warnings,
		segments,
		*finishReason,
		false,
		false,
	)
	if err != nil {
		return fmt.Errorf("render embed response: %w", err)
	}

	*lastRenderTime = time.Now()

	return nil
}

func responseTextWithError(responseText string, errorText string) string {
	trimmedResponseText := strings.TrimSpace(responseText)
	trimmedErrorText := strings.TrimSpace(errorText)

	if trimmedResponseText == "" {
		return trimmedErrorText
	}

	if trimmedErrorText == "" {
		return trimmedResponseText
	}

	return trimmedResponseText + "\n\n" + trimmedErrorText
}

func (instance *bot) renderFinalResponse(
	ctx context.Context,
	tracker *responseTracker,
	warnings []string,
	accumulator *segmentAccumulator,
	thinkingText string,
	finishReason string,
	usePlainResponses bool,
) error {
	if usePlainResponses {
		err := instance.sendPlainResponse(
			ctx,
			tracker,
			accumulator.renderSegments(),
			strings.TrimSpace(thinkingText) != "",
		)
		if err != nil {
			return fmt.Errorf("send plain response: %w", err)
		}

		return nil
	}

	err := instance.renderEmbedResponse(
		ctx,
		tracker,
		warnings,
		accumulator.renderSegments(),
		finishReason,
		true,
		strings.TrimSpace(thinkingText) != "",
	)
	if err != nil {
		return fmt.Errorf("render final embed response: %w", err)
	}

	return nil
}

func userFacingResponseError(err error) string {
	const (
		genericResponseErrorText     = "Couldn't generate a response right now. Try again."
		rateLimitResponseErrorText   = "Usage limit reached for this model. Try other models."
		accessResponseErrorText      = "This model is unavailable right now. Try other models."
		missingResponseErrorText     = "A required resource was not found. Try again."
		unavailableResponseErrorText = "The model provider is temporarily unavailable. Try again."
		timeoutResponseErrorText     = "Request timed out. Try again."
		canceledResponseErrorText    = "Request cancelled."
	)

	switch {
	case err == nil:
		return genericResponseErrorText
	case errors.Is(err, context.Canceled):
		return canceledResponseErrorText
	case isTimeoutResponseError(err):
		return timeoutResponseErrorText
	case isRateLimitResponseError(err):
		return rateLimitResponseErrorText
	case isAccessResponseError(err):
		return accessResponseErrorText
	case isMissingResponseError(err):
		return missingResponseErrorText
	case isUnavailableResponseError(err):
		return unavailableResponseErrorText
	default:
		return genericResponseErrorText
	}
}

func isTimeoutResponseError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	statusCode, ok := responseErrorStatusCode(err)
	if ok && statusCode == http.StatusGatewayTimeout {
		return true
	}

	return responseErrorContains(
		err,
		"deadline exceeded",
		"timed out",
		"timeout",
	)
}

func isRateLimitResponseError(err error) bool {
	statusCode, ok := responseErrorStatusCode(err)
	if ok && statusCode == http.StatusTooManyRequests {
		return true
	}

	return responseErrorContains(
		err,
		"quota exceeded",
		"quota reached",
		"rate limit",
		"rate limited",
		"resource exhausted",
		"too many requests",
		"usage limit",
	)
}

func isAccessResponseError(err error) bool {
	statusCode, ok := responseErrorStatusCode(err)
	if ok {
		switch statusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return true
		}
	}

	var apiKeyErr providerAPIKeyError
	if errors.As(err, &apiKeyErr) {
		return true
	}

	return responseErrorContains(
		err,
		"access denied",
		"api key",
		"api_key",
		"forbidden",
		"model not found",
		"permission denied",
		"unauthorized",
	)
}

func isMissingResponseError(err error) bool {
	statusCode, ok := responseErrorStatusCode(err)
	if ok && statusCode == http.StatusNotFound {
		return true
	}

	return responseErrorContains(
		err,
		"attachment not found",
		"file not found",
		"not found",
		"resource not found",
	)
}

func isUnavailableResponseError(err error) bool {
	statusCode, ok := responseErrorStatusCode(err)
	if ok {
		switch statusCode {
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
			return true
		}
	}

	return responseErrorContains(
		err,
		"bad gateway",
		"internal server error",
		"service unavailable",
		"temporarily unavailable",
	)
}

func responseErrorStatusCode(err error) (int, bool) {
	var statusErr providerStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode, true
	}

	var geminiErrPtr *genai.APIError
	if errors.As(err, &geminiErrPtr) && geminiErrPtr != nil {
		return geminiErrPtr.Code, true
	}

	var geminiErr genai.APIError
	if errors.As(err, &geminiErr) {
		return geminiErr.Code, true
	}

	return 0, false
}

func responseErrorContains(err error, fragments ...string) bool {
	if err == nil {
		return false
	}

	errorText := strings.ToLower(strings.Join(strings.Fields(err.Error()), " "))
	for _, fragment := range fragments {
		if strings.Contains(errorText, fragment) {
			return true
		}
	}

	return false
}

func (instance *bot) renderFailureResponse(
	tracker *responseTracker,
	errorText string,
	usePlainResponses bool,
) error {
	if tracker == nil {
		return nil
	}

	errorText = strings.TrimSpace(errorText)
	if errorText == "" {
		errorText = userFacingResponseError(nil)
	}

	failureEmbed := buildRequestProgressFailureEmbed(tracker.modelName, errorText)

	var renderErr error

	if tracker.progressActive && len(tracker.responseMessages) > 0 {
		tracker.progressActive = false

		err := instance.editEmbedMessage(
			tracker.responseMessages[0],
			failureEmbed,
			nil,
		)
		if err == nil {
			tracker.responseVisible = true

			return nil
		}

		renderErr = fmt.Errorf("edit progress message: %w", err)
	}

	fallbackTracker := newResponseTracker(tracker.sourceMessage, tracker.modelName)
	fallbackTracker.searchMetadata = cloneSearchMetadata(tracker.searchMetadata)
	fallbackTracker.responseMessages = append(fallbackTracker.responseMessages, tracker.responseMessages...)

	var (
		sentMessage *discordgo.Message
		pending     pendingResponse
		err         error
	)

	if usePlainResponses {
		sentMessage, pending, err = instance.sendPlainMessage(
			fallbackTracker,
			errorText,
			responseActions{showSources: false, showThinking: false, showRentry: false},
		)
	} else {
		sentMessage, pending, err = instance.sendEmbedMessage(
			fallbackTracker,
			failureEmbed,
			responseActions{showSources: false, showThinking: false, showRentry: false},
		)
	}

	if err != nil {
		if renderErr != nil {
			return errors.Join(renderErr, fmt.Errorf("send failure response: %w", err))
		}

		return fmt.Errorf("send failure response: %w", err)
	}

	tracker.progressActive = false
	tracker.responseVisible = true
	tracker.responseMessages = append(tracker.responseMessages, sentMessage)
	tracker.pendingResponses = append(tracker.pendingResponses, pending)

	if renderErr != nil {
		return renderErr
	}

	return nil
}

func shouldRenderProgress(
	segments []string,
	splitOccurred bool,
	lastRenderTime time.Time,
) bool {
	if len(segments) == 0 {
		return false
	}

	if splitOccurred {
		return true
	}

	if lastRenderTime.IsZero() {
		return true
	}

	return time.Since(lastRenderTime) >= editDelay
}

func buildRenderSpecs(
	segments []string,
	finishReason string,
	final bool,
	hasSearchMetadata bool,
	hasThinking bool,
) []renderSpec {
	specs := make([]renderSpec, 0, len(segments))

	for index, segment := range segments {
		settled := index < len(segments)-1 || final

		spec := renderSpec{
			content: segment,
			color:   0,
			actions: responseActions{
				showSources:  final && hasSearchMetadata && index == len(segments)-1,
				showThinking: final && hasThinking && index == len(segments)-1,
				showRentry:   final && index == len(segments)-1,
			},
		}

		switch {
		case !settled:
			spec.content += streamingIndicator
			spec.color = embedColorIncomplete
		case index < len(segments)-1 || isGoodFinishReason(finishReason):
			spec.color = embedColorComplete
		default:
			spec.color = embedColorIncomplete
		}

		specs = append(specs, spec)
	}

	return specs
}

func (instance *bot) renderEmbedResponse(
	ctx context.Context,
	tracker *responseTracker,
	warnings []string,
	segments []string,
	finishReason string,
	final bool,
	hasThinking bool,
) error {
	if len(segments) == 0 {
		return nil
	}

	desiredSpecs := buildRenderSpecs(
		segments,
		finishReason,
		final,
		tracker.searchMetadata != nil,
		hasThinking,
	)
	for index, spec := range desiredSpecs {
		if index < len(tracker.renderedSpecs) && tracker.renderedSpecs[index] == spec {
			continue
		}

		embed := buildResponseEmbed(
			spec.content,
			tracker.modelName,
			spec.color,
			warnings,
		)

		err := instance.waitForEditSlot(ctx)
		if err != nil {
			return fmt.Errorf("wait before embed update: %w", err)
		}

		if index < len(tracker.responseMessages) {
			err := instance.editEmbedMessage(
				tracker.responseMessages[index],
				embed,
				buildEmbedComponents(spec.actions),
			)
			if err != nil {
				return fmt.Errorf("edit embed message: %w", err)
			}

			if index == 0 {
				tracker.progressActive = false
			}
		} else {
			sentMessage, pending, err := instance.sendEmbedMessage(
				tracker,
				embed,
				spec.actions,
			)
			if err != nil {
				return fmt.Errorf("send embed message: %w", err)
			}

			tracker.responseMessages = append(tracker.responseMessages, sentMessage)
			tracker.pendingResponses = append(tracker.pendingResponses, pending)
		}

		if index < len(tracker.renderedSpecs) {
			tracker.renderedSpecs[index] = spec
		} else {
			tracker.renderedSpecs = append(tracker.renderedSpecs, spec)
		}
	}

	err := instance.trimExtraEmbedResponses(ctx, tracker, len(desiredSpecs))
	if err != nil {
		return err
	}

	tracker.responseVisible = true

	return nil
}

func (instance *bot) trimExtraEmbedResponses(
	ctx context.Context,
	tracker *responseTracker,
	keepCount int,
) error {
	for len(tracker.responseMessages) > keepCount {
		lastIndex := len(tracker.responseMessages) - 1
		message := tracker.responseMessages[lastIndex]
		pending := tracker.pendingResponses[lastIndex]

		err := instance.waitForEditSlot(ctx)
		if err != nil {
			return fmt.Errorf("wait before embed cleanup: %w", err)
		}

		err = instance.session.ChannelMessageDelete(message.ChannelID, message.ID)
		if err != nil {
			return fmt.Errorf("delete extra embed message: %w", err)
		}

		tracker.responseMessages = tracker.responseMessages[:lastIndex]
		tracker.pendingResponses = tracker.pendingResponses[:lastIndex]

		discardPendingResponse(instance.nodes, pending)
	}

	if len(tracker.renderedSpecs) > keepCount {
		tracker.renderedSpecs = tracker.renderedSpecs[:keepCount]
	}

	return nil
}

func discardPendingResponse(store *messageNodeStore, pending pendingResponse) {
	if pending.node == nil {
		return
	}

	if store != nil {
		store.mu.Lock()
		if currentNode, ok := store.nodes[pending.messageID]; ok && currentNode == pending.node {
			delete(store.nodes, pending.messageID)
		}
		store.mu.Unlock()
		store.deleteCachedSnapshot(pending.messageID)
	}

	pending.node.mu.Unlock()
}

func (instance *bot) sendPlainResponse(
	_ context.Context,
	tracker *responseTracker,
	segments []string,
	hasThinking bool,
) error {
	for index, segment := range segments {
		actions := responseActions{
			showSources:  tracker.searchMetadata != nil && index == len(segments)-1,
			showThinking: hasThinking && index == len(segments)-1,
			showRentry:   index == len(segments)-1,
		}

		if index < len(tracker.responseMessages) {
			err := instance.editPlainMessage(
				tracker.responseMessages[index],
				segment,
				actions,
			)
			if err != nil {
				return fmt.Errorf("edit plain message: %w", err)
			}

			if index == 0 {
				tracker.progressActive = false
			}

			continue
		}

		sentMessage, pending, err := instance.sendPlainMessage(
			tracker,
			segment,
			actions,
		)
		if err != nil {
			return fmt.Errorf("send plain message: %w", err)
		}

		tracker.responseMessages = append(tracker.responseMessages, sentMessage)
		tracker.pendingResponses = append(tracker.pendingResponses, pending)
	}

	tracker.responseVisible = true

	return nil
}

func (instance *bot) sendEmbedMessage(
	tracker *responseTracker,
	embed *discordgo.MessageEmbed,
	actions responseActions,
) (*discordgo.Message, pendingResponse, error) {
	send := newReplyMessage(referenceTarget(tracker))
	send.Embeds = append(send.Embeds, embed)
	send.Components = buildEmbedComponents(actions)

	return instance.sendReplyMessage(tracker, send)
}

func (instance *bot) sendPlainMessage(
	tracker *responseTracker,
	content string,
	actions responseActions,
) (*discordgo.Message, pendingResponse, error) {
	send := newReplyMessage(referenceTarget(tracker))
	send.Flags |= discordgo.MessageFlagsIsComponentsV2
	send.Components = buildPlainComponents(content, actions)

	return instance.sendReplyMessage(tracker, send)
}

func referenceTarget(tracker *responseTracker) *discordgo.Message {
	if len(tracker.responseMessages) == 0 {
		return tracker.sourceMessage
	}

	return tracker.responseMessages[len(tracker.responseMessages)-1]
}

func newReplyMessage(reference *discordgo.Message) *discordgo.MessageSend {
	send := new(discordgo.MessageSend)

	allowedMentions := new(discordgo.MessageAllowedMentions)
	allowedMentions.Parse = []discordgo.AllowedMentionType{
		discordgo.AllowedMentionTypeRoles,
		discordgo.AllowedMentionTypeUsers,
		discordgo.AllowedMentionTypeEveryone,
	}
	allowedMentions.RepliedUser = false

	send.AllowedMentions = allowedMentions
	send.Reference = reference.Reference()
	send.Flags = discordgo.MessageFlagsSuppressNotifications

	return send
}

func (instance *bot) sendReplyMessage(
	tracker *responseTracker,
	send *discordgo.MessageSend,
) (*discordgo.Message, pendingResponse, error) {
	target := referenceTarget(tracker)

	sentMessage, err := instance.session.ChannelMessageSendComplex(target.ChannelID, send)
	if err != nil {
		return nil, pendingResponse{}, fmt.Errorf("send reply message: %w", err)
	}

	pending := pendingResponse{
		messageID: sentMessage.ID,
		node:      instance.nodes.addPending(sentMessage.ID, tracker.sourceMessage),
	}
	pending.node.searchMetadata = cloneSearchMetadata(tracker.searchMetadata)

	return sentMessage, pending, nil
}

func (instance *bot) editEmbedMessage(
	message *discordgo.Message,
	embed *discordgo.MessageEmbed,
	components []discordgo.MessageComponent,
) error {
	edit := discordgo.NewMessageEdit(message.ChannelID, message.ID)
	edit.SetEmbeds([]*discordgo.MessageEmbed{embed})
	edit.Components = &components

	_, err := instance.session.ChannelMessageEditComplex(edit)
	if err != nil {
		return fmt.Errorf("edit message %s: %w", message.ID, err)
	}

	return nil
}

func (instance *bot) editPlainMessage(
	message *discordgo.Message,
	content string,
	actions responseActions,
) error {
	edit := discordgo.NewMessageEdit(message.ChannelID, message.ID)
	edit.SetEmbeds([]*discordgo.MessageEmbed{})

	components := buildPlainComponents(content, actions)
	edit.Components = &components
	edit.Flags = discordgo.MessageFlagsIsComponentsV2 |
		discordgo.MessageFlagsSuppressNotifications

	_, err := instance.session.ChannelMessageEditComplex(edit)
	if err != nil {
		return fmt.Errorf("edit message %s: %w", message.ID, err)
	}

	return nil
}

func buildResponseEmbed(
	content string,
	modelName string,
	color int,
	warnings []string,
) *discordgo.MessageEmbed {
	embed := new(discordgo.MessageEmbed)
	embed.Description = content
	embed.Color = color

	if modelName != "" {
		author := new(discordgo.MessageEmbedAuthor)
		author.Name = modelName
		embed.Author = author
	}

	for _, warning := range warnings {
		field := new(discordgo.MessageEmbedField)
		field.Name = warning
		field.Value = "."
		field.Inline = false
		embed.Fields = append(embed.Fields, field)
	}

	return embed
}

func buildEmbedComponents(actions responseActions) []discordgo.MessageComponent {
	buttons := buildResponseButtons(actions)
	if len(buttons) == 0 {
		return nil
	}

	row := new(discordgo.ActionsRow)
	row.Components = buttons

	return []discordgo.MessageComponent{row}
}

func buildPlainComponents(content string, actions responseActions) []discordgo.MessageComponent {
	textDisplay := new(discordgo.TextDisplay)
	textDisplay.Content = content

	buttons := buildResponseButtons(actions)
	if len(buttons) == 0 {
		return []discordgo.MessageComponent{textDisplay}
	}

	row := new(discordgo.ActionsRow)
	row.Components = buttons

	return []discordgo.MessageComponent{textDisplay, row}
}

func buildResponseButtons(actions responseActions) []discordgo.MessageComponent {
	const maxResponseButtons = 3

	buttons := make([]discordgo.MessageComponent, 0, maxResponseButtons)

	if actions.showThinking {
		button := new(discordgo.Button)
		button.CustomID = showThinkingButtonCustomID
		button.Label = showThinkingButtonLabel
		button.Style = discordgo.SecondaryButton

		buttons = append(buttons, button)
	}

	if actions.showSources {
		button := new(discordgo.Button)
		button.CustomID = showSourcesButtonCustomID
		button.Label = showSourcesButtonLabel
		button.Style = discordgo.SecondaryButton

		buttons = append(buttons, button)
	}

	if actions.showRentry {
		button := new(discordgo.Button)
		button.CustomID = viewOnRentryButtonCustomID
		button.Label = viewOnRentryButtonLabel
		button.Style = discordgo.SecondaryButton

		buttons = append(buttons, button)
	}

	return buttons
}
