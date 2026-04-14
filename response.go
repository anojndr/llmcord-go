package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

type segmentAccumulator struct {
	maxLength int
	segments  []string
}

type renderSpec struct {
	content    string
	color      int
	actions    responseActions
	footerText string
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
	sourceMessage      *discordgo.Message
	searchMetadata     *searchMetadata
	responseImages     []responseImageAsset
	modelName          string
	contextWindow      int
	usage              *tokenUsage
	providerResponseID string
	responseMessages   []*discordgo.Message
	pendingResponses   []pendingResponse
	renderedSpecs      []renderSpec
	progressActive     bool
	responseVisible    bool
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

func visibleResponseText(thinkingText, answerText string) string {
	switch {
	case thinkingText == "":
		return answerText
	case answerText == "":
		return thinkingResponsePrefix + thinkingText
	default:
		return thinkingResponsePrefix + thinkingText + answerResponseSeparator + answerText
	}
}

const thinkingResponsePrefix = "**Thinking**\n"
const answerResponseSeparator = "\n\n**Answer**\n"

const (
	contextWindowPercentScale     = 100
	compactTokenCountBase         = 1_000
	compactTokenCountMillion      = 1_000_000
	compactTokenCountBillion      = 1_000_000_000
	compactTokenCountSmallCutoff  = 10
	compactTokenCountMediumCutoff = 100
)

var errStreamedAnswerVisibilityRegressed = errors.New("streamed answer visibility regressed")
var errResponseEmbedImageUnavailable = errors.New("response embed image unavailable")

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

func withoutCancelContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}

	return context.WithoutCancel(ctx)
}

func (tracker *responseTracker) release(store *messageNodeStore, fullText string, thinkingText string) {
	for _, pending := range tracker.pendingResponses {
		pending.node.role = messageRoleAssistant
		pending.node.text = fullText
		pending.node.thinkingText = thinkingText
		pending.node.urlScanText = ""
		pending.node.searchMetadata = cloneSearchMetadata(tracker.searchMetadata)
		pending.node.providerResponseID = strings.TrimSpace(tracker.providerResponseID)
		pending.node.providerResponseModel = strings.TrimSpace(tracker.modelName)
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
	tracker.contextWindow = request.ContextWindow

	var finishReason string

	lastRenderTime := time.Time{}
	streamState := generatedStreamState{
		request:             request,
		warnings:            warnings,
		answerAccumulator:   &accumulator,
		thinkingAccumulator: &thinkingAccumulator,
		finishReason:        &finishReason,
		lastRenderTime:      &lastRenderTime,
		maxLength:           maxLength,
		usePlainResponses:   usePlainResponses,
		rawAnswerText:       "",
		renderedAnswerText:  "",
	}

	streamErr := instance.chatCompletions.streamChatCompletion(ctx, request, func(delta streamDelta) error {
		return instance.handleGeneratedStreamDelta(ctx, tracker, &streamState, delta)
	})
	if streamErr != nil && finishReason == "" {
		finishReason = "error"
	}

	finalAnswerText := streamState.rawAnswerText

	cleanedAnswerText, parsedSearchMetadata := finalizeXAIResponseAnswer(
		request,
		finalAnswerText,
		tracker.searchMetadata,
	)
	if parsedSearchMetadata != nil {
		tracker.searchMetadata = mergeSearchMetadata(tracker.searchMetadata, parsedSearchMetadata)
	}

	finalAccumulator := accumulator
	if cleanedAnswerText != finalAnswerText {
		finalAccumulator = newSegmentAccumulator(maxLength)
		_ = finalAccumulator.appendText(cleanedAnswerText)
	}

	responseErr := instance.renderFinalResponse(
		ctx,
		tracker,
		warnings,
		&finalAccumulator,
		thinkingAccumulator.joined(),
		finishReason,
		usePlainResponses,
	)
	if responseErr == nil && streamErr != nil {
		responseErr = fmt.Errorf("stream response: %w", streamErr)
	}

	finalText := visibleResponseText(thinkingAccumulator.joined(), cleanedAnswerText)

	if responseErr != nil {
		errorText := userFacingResponseError(responseErr)

		renderErr := instance.renderFailureResponse(ctx, tracker, errorText, usePlainResponses)
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

type generatedStreamState struct {
	request             chatCompletionRequest
	warnings            []string
	answerAccumulator   *segmentAccumulator
	thinkingAccumulator *segmentAccumulator
	finishReason        *string
	lastRenderTime      *time.Time
	maxLength           int
	usePlainResponses   bool
	rawAnswerText       string
	renderedAnswerText  string
}

func (instance *bot) handleGeneratedStreamDelta(
	ctx context.Context,
	tracker *responseTracker,
	state *generatedStreamState,
	delta streamDelta,
) error {
	splitOccurred := false
	if delta.Thinking != "" {
		splitOccurred = state.thinkingAccumulator.appendText(delta.Thinking) || splitOccurred
	}

	if delta.Content != "" {
		answerSplitOccurred, err := state.appendAnswerText(delta.Content)
		if err != nil {
			return err
		}

		splitOccurred = answerSplitOccurred || splitOccurred
	}

	if delta.FinishReason != "" {
		*state.finishReason = delta.FinishReason
	}

	if delta.Usage != nil {
		tracker.usage = cloneTokenUsage(delta.Usage)
	}

	if strings.TrimSpace(delta.ProviderResponseID) != "" {
		tracker.providerResponseID = strings.TrimSpace(delta.ProviderResponseID)
	}

	if delta.SearchMetadata != nil {
		tracker.searchMetadata = mergeSearchMetadata(tracker.searchMetadata, delta.SearchMetadata)
	}

	if len(delta.ResponseImages) > 0 {
		tracker.responseImages = mergeResponseImageAssets(tracker.responseImages, delta.ResponseImages)
	}

	if state.usePlainResponses {
		return nil
	}

	segments := visibleResponseSegments(
		state.thinkingAccumulator.joined(),
		state.answerAccumulator.joined(),
		state.maxLength,
	)
	if !shouldRenderProgress(segments, splitOccurred, *state.lastRenderTime) {
		return nil
	}

	err := instance.renderEmbedResponse(
		ctx,
		tracker,
		state.warnings,
		segments,
		*state.finishReason,
		false,
		false,
	)
	if err != nil {
		return fmt.Errorf("render embed response: %w", err)
	}

	*state.lastRenderTime = time.Now()

	return nil
}

func (state *generatedStreamState) appendAnswerText(answerDelta string) (bool, error) {
	state.rawAnswerText += answerDelta

	visibleAnswerText := xAIStreamingVisibleAnswerText(state.request, state.rawAnswerText)
	if !strings.HasPrefix(visibleAnswerText, state.renderedAnswerText) {
		return false, errStreamedAnswerVisibilityRegressed
	}

	renderedDelta := strings.TrimPrefix(visibleAnswerText, state.renderedAnswerText)
	state.renderedAnswerText = visibleAnswerText

	if renderedDelta == "" {
		return false, nil
	}

	return state.answerAccumulator.appendText(renderedDelta), nil
}

func responseTextWithError(responseText, errorText string) string {
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
	const genericResponseErrorText = "Couldn't generate a response right now. Try again."

	if err == nil {
		return genericResponseErrorText
	}

	errorText := strings.TrimSpace(err.Error())
	if errorText == "" {
		return genericResponseErrorText
	}

	return errorText
}

func (instance *bot) renderFailureResponse(
	ctx context.Context,
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

	handled, renderErr := instance.renderFailureOnProgressMessage(ctx, tracker, failureEmbed)
	if handled {
		return nil
	}

	return instance.sendFallbackFailureResponse(
		ctx,
		tracker,
		errorText,
		failureEmbed,
		usePlainResponses,
		renderErr,
	)
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
			footerText: "",
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
	if final && len(desiredSpecs) > 0 {
		desiredSpecs[len(desiredSpecs)-1].footerText = contextWindowFooter(
			tracker.usage,
			tracker.contextWindow,
		)
	}

	for index, spec := range desiredSpecs {
		if index < len(tracker.renderedSpecs) && tracker.renderedSpecs[index] == spec {
			continue
		}

		embed := buildResponseEmbed(
			spec.content,
			tracker.modelName,
			spec.color,
			warnings,
			spec.footerText,
		)

		err := instance.renderEmbedSpec(ctx, tracker, index, embed, spec.actions, final)
		if err != nil {
			return err
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

func (instance *bot) renderEmbedSpec(
	ctx context.Context,
	tracker *responseTracker,
	index int,
	embed *discordgo.MessageEmbed,
	actions responseActions,
	final bool,
) error {
	if index >= len(tracker.responseMessages) {
		sentMessage, pending, err := instance.sendEmbedMessage(
			ctx,
			tracker,
			embed,
			actions,
			final,
		)
		if err != nil {
			return fmt.Errorf("send embed message: %w", err)
		}

		tracker.responseMessages = append(tracker.responseMessages, sentMessage)
		tracker.pendingResponses = append(tracker.pendingResponses, pending)

		return nil
	}

	err := instance.waitForEditSlotForMessage(
		ctx,
		tracker.responseMessages[index].ID,
	)
	if err != nil {
		return fmt.Errorf("wait before embed update: %w", err)
	}

	err = instance.editEmbedMessage(
		ctx,
		tracker.responseMessages[index],
		embed,
		buildEmbedComponents(actions),
		tracker.responseImages,
		final,
	)
	if err != nil {
		return fmt.Errorf("edit embed message: %w", err)
	}

	if index == 0 {
		tracker.progressActive = false
	}

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

		err := instance.waitForEditSlotForMessage(ctx, message.ID)
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
	ctx context.Context,
	tracker *responseTracker,
	segments []string,
	hasThinking bool,
) error {
	for index, segment := range segments {
		actions := plainResponseActions(tracker, index, len(segments), hasThinking)
		if index < len(tracker.responseMessages) {
			err := instance.updatePlainResponseMessage(ctx, tracker, index, segment, actions)
			if err != nil {
				return err
			}

			continue
		}

		sentMessage, pending, err := instance.sendPlainResponseMessage(tracker, segment, actions)
		if err != nil {
			return err
		}

		tracker.responseMessages = append(tracker.responseMessages, sentMessage)
		tracker.pendingResponses = append(tracker.pendingResponses, pending)
	}

	tracker.responseVisible = true

	return nil
}

func (instance *bot) renderFailureOnProgressMessage(
	ctx context.Context,
	tracker *responseTracker,
	failureEmbed *discordgo.MessageEmbed,
) (bool, error) {
	if !tracker.progressActive || len(tracker.responseMessages) == 0 {
		tracker.progressActive = false

		return false, nil
	}

	tracker.progressActive = false

	err := instance.waitForEditSlotForMessage(
		withoutCancelContext(ctx),
		tracker.responseMessages[0].ID,
	)
	if err != nil {
		return false, fmt.Errorf("wait before progress failure edit: %w", err)
	}

	err = instance.editEmbedMessage(
		ctx,
		tracker.responseMessages[0],
		failureEmbed,
		nil,
		nil,
		false,
	)
	if err != nil {
		return false, fmt.Errorf("edit progress message: %w", err)
	}

	tracker.responseVisible = true

	return true, nil
}

func (instance *bot) sendFallbackFailureResponse(
	ctx context.Context,
	tracker *responseTracker,
	errorText string,
	failureEmbed *discordgo.MessageEmbed,
	usePlainResponses bool,
	renderErr error,
) error {
	fallbackTracker := newResponseTracker(tracker.sourceMessage, tracker.modelName)
	fallbackTracker.searchMetadata = cloneSearchMetadata(tracker.searchMetadata)
	fallbackTracker.responseMessages = append(fallbackTracker.responseMessages, tracker.responseMessages...)

	sentMessage, pending, err := instance.sendFailureResponseMessage(
		ctx,
		fallbackTracker,
		errorText,
		failureEmbed,
		usePlainResponses,
	)
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

	return renderErr
}

func (instance *bot) sendFailureResponseMessage(
	ctx context.Context,
	fallbackTracker *responseTracker,
	errorText string,
	failureEmbed *discordgo.MessageEmbed,
	usePlainResponses bool,
) (*discordgo.Message, pendingResponse, error) {
	if usePlainResponses {
		return instance.sendPlainMessage(
			fallbackTracker,
			errorText,
			responseActions{showSources: false, showThinking: false, showRentry: false},
		)
	}

	return instance.sendEmbedMessage(
		ctx,
		fallbackTracker,
		failureEmbed,
		responseActions{showSources: false, showThinking: false, showRentry: false},
		false,
	)
}

func plainResponseActions(
	tracker *responseTracker,
	index int,
	totalSegments int,
	hasThinking bool,
) responseActions {
	return responseActions{
		showSources:  tracker.searchMetadata != nil && index == totalSegments-1,
		showThinking: hasThinking && index == totalSegments-1,
		showRentry:   index == totalSegments-1,
	}
}

func (instance *bot) updatePlainResponseMessage(
	ctx context.Context,
	tracker *responseTracker,
	index int,
	segment string,
	actions responseActions,
) error {
	err := instance.waitForEditSlotForMessage(
		ctx,
		tracker.responseMessages[index].ID,
	)
	if err != nil {
		return fmt.Errorf("wait before plain message update: %w", err)
	}

	err = instance.editPlainMessage(
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

	return nil
}

func (instance *bot) sendPlainResponseMessage(
	tracker *responseTracker,
	segment string,
	actions responseActions,
) (*discordgo.Message, pendingResponse, error) {
	sentMessage, pending, err := instance.sendPlainMessage(
		tracker,
		segment,
		actions,
	)
	if err != nil {
		return nil, pendingResponse{}, fmt.Errorf("send plain message: %w", err)
	}

	return sentMessage, pending, nil
}

func (instance *bot) sendEmbedMessage(
	ctx context.Context,
	tracker *responseTracker,
	embed *discordgo.MessageEmbed,
	actions responseActions,
	final bool,
) (*discordgo.Message, pendingResponse, error) {
	send := newReplyMessage(referenceTarget(tracker))
	files := instance.prepareResponseEmbedFiles(ctx, embed, tracker.responseImages, final)
	send.Embeds = append(send.Embeds, embed)
	send.Files = files
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
	ctx context.Context,
	message *discordgo.Message,
	embed *discordgo.MessageEmbed,
	components []discordgo.MessageComponent,
	responseImages []responseImageAsset,
	final bool,
) error {
	edit := discordgo.NewMessageEdit(message.ChannelID, message.ID)
	files := instance.prepareResponseEmbedFiles(ctx, embed, responseImages, final)
	edit.SetEmbeds([]*discordgo.MessageEmbed{embed})
	edit.Components = &components
	edit.Files = files

	if len(files) > 0 {
		attachments := []*discordgo.MessageAttachment{}
		edit.Attachments = &attachments
	}

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
	footerText string,
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

	if strings.TrimSpace(footerText) != "" {
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text:         footerText,
			IconURL:      "",
			ProxyIconURL: "",
		}
	}

	return embed
}

func (instance *bot) prepareResponseEmbedFiles(
	ctx context.Context,
	embed *discordgo.MessageEmbed,
	responseImages []responseImageAsset,
	final bool,
) []*discordgo.File {
	if !final || embed == nil {
		return nil
	}

	sanitizedDescription, imageURL := responseEmbedContent(embed.Description)
	if imageURL == "" {
		if imageFile := responseEmbedFileFromAssets(responseImages); imageFile != nil {
			embed.Image = &discordgo.MessageEmbedImage{
				URL:      "attachment://" + imageFile.Name,
				ProxyURL: "",
				Width:    0,
				Height:   0,
			}

			return []*discordgo.File{imageFile}
		}

		return nil
	}

	file := responseEmbedFileFromAssets(responseImages)
	if file == nil {
		var err error

		file, err = instance.responseEmbedFile(ctx, imageURL)
		if err != nil {
			if errors.Is(err, errResponseEmbedImageUnavailable) {
				setResponseEmbedImageURL(embed, sanitizedDescription, imageURL)

				return nil
			}

			slog.Warn("fetch response embed image", "url", imageURL, "error", err)

			setResponseEmbedImageURL(embed, sanitizedDescription, imageURL)

			return nil
		}
	}

	if file == nil {
		return nil
	}

	embed.Description = sanitizedDescription
	embed.Image = &discordgo.MessageEmbedImage{
		URL:      "attachment://" + file.Name,
		ProxyURL: "",
		Width:    0,
		Height:   0,
	}

	return []*discordgo.File{file}
}

func setResponseEmbedImageURL(embed *discordgo.MessageEmbed, description string, imageURL string) {
	if embed == nil {
		return
	}

	embed.Description = description
	embed.Image = &discordgo.MessageEmbedImage{
		URL:      imageURL,
		ProxyURL: "",
		Width:    0,
		Height:   0,
	}
}

func responseEmbedFileFromAssets(responseImages []responseImageAsset) *discordgo.File {
	for _, responseImage := range responseImages {
		if file := responseEmbedFileFromAsset(responseImage); file != nil {
			return file
		}
	}

	return nil
}

func responseEmbedFileFromAsset(responseImage responseImageAsset) *discordgo.File {
	if len(responseImage.Data) == 0 {
		return nil
	}

	fileName := responseEmbedFilename(responseImage.URL, responseImage.ContentType)
	if fileName == "response-image" {
		fileName += defaultResponseImageExtension(responseImage.ContentType)
	}

	return &discordgo.File{
		Name:        fileName,
		ContentType: responseImage.ContentType,
		Reader:      bytes.NewReader(responseImage.Data),
	}
}

func defaultResponseImageExtension(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case mimeTypeJPEG:
		return fileExtensionJPG
	case "image/png":
		return fileExtensionPNG
	case mimeTypeWEBP:
		return fileExtensionWEBP
	case "image/gif":
		return fileExtensionGIF
	case "image/avif":
		return fileExtensionAVIF
	default:
		return ""
	}
}

func mergeResponseImageAssets(
	existing []responseImageAsset,
	incoming []responseImageAsset,
) []responseImageAsset {
	if len(incoming) == 0 {
		return existing
	}

	if len(existing) == 0 {
		return cloneResponseImageAssets(incoming)
	}

	merged := cloneResponseImageAssets(existing)
	indexByKey := make(map[string]int, len(merged))

	for index, image := range merged {
		if key := responseImageAssetKey(image); key != "" {
			indexByKey[key] = index
		}
	}

	for _, image := range incoming {
		if key := responseImageAssetKey(image); key != "" {
			if existingIndex, ok := indexByKey[key]; ok {
				merged[existingIndex] = mergeResponseImageAsset(merged[existingIndex], image)

				continue
			}

			indexByKey[key] = len(merged)
		}

		merged = append(merged, cloneResponseImageAsset(image))
	}

	return merged
}

func responseImageAssetKey(image responseImageAsset) string {
	if id := strings.TrimSpace(image.ID); id != "" {
		return "id:" + id
	}

	if imageURL := strings.TrimSpace(image.URL); imageURL != "" {
		return "url:" + strings.ToLower(imageURL)
	}

	return ""
}

func mergeResponseImageAsset(
	existing responseImageAsset,
	incoming responseImageAsset,
) responseImageAsset {
	if strings.TrimSpace(existing.ID) == "" {
		existing.ID = strings.TrimSpace(incoming.ID)
	}

	if strings.TrimSpace(existing.URL) == "" {
		existing.URL = strings.TrimSpace(incoming.URL)
	}

	if strings.TrimSpace(existing.ContentType) == "" {
		existing.ContentType = strings.TrimSpace(incoming.ContentType)
	}

	if len(existing.Data) == 0 && len(incoming.Data) > 0 {
		existing.Data = append([]byte(nil), incoming.Data...)
	}

	return existing
}

func cloneResponseImageAssets(images []responseImageAsset) []responseImageAsset {
	if len(images) == 0 {
		return nil
	}

	cloned := make([]responseImageAsset, 0, len(images))
	for _, image := range images {
		cloned = append(cloned, cloneResponseImageAsset(image))
	}

	return cloned
}

func cloneResponseImageAsset(image responseImageAsset) responseImageAsset {
	return responseImageAsset{
		ID:          image.ID,
		URL:         image.URL,
		ContentType: image.ContentType,
		Data:        append([]byte(nil), image.Data...),
	}
}

func (instance *bot) responseEmbedFile(
	ctx context.Context,
	imageURL string,
) (*discordgo.File, error) {
	if instance == nil {
		return nil, errResponseEmbedImageUnavailable
	}

	imageAsset, err := responseImageAssetFromURL(ctx, instance.httpClient, imageURL)
	if err != nil {
		return nil, err
	}

	file := responseEmbedFileFromAsset(imageAsset)
	if file == nil {
		return nil, errResponseEmbedImageUnavailable
	}

	return file, nil
}

func responseEmbedFilename(imageURL string, contentType string) string {
	const responseEmbedFilenameBase = "response-image"

	if ext := responseEmbedImageExtension(imageURL, contentType); ext != "" {
		return responseEmbedFilenameBase + ext
	}

	return responseEmbedFilenameBase
}

func responseEmbedImageExtension(imageURL string, contentType string) string {
	parsedURL, err := url.Parse(strings.TrimSpace(imageURL))
	if err == nil {
		switch ext := strings.ToLower(path.Ext(parsedURL.Path)); ext {
		case fileExtensionPNG,
			fileExtensionJPG,
			fileExtensionJPEG,
			fileExtensionWEBP,
			fileExtensionGIF,
			fileExtensionAVIF:
			return ext
		}
	}

	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return ""
	}

	extensions, err := mime.ExtensionsByType(mediaType)
	if err != nil {
		return ""
	}

	for _, ext := range extensions {
		switch strings.ToLower(strings.TrimSpace(ext)) {
		case fileExtensionPNG,
			fileExtensionJPG,
			fileExtensionJPEG,
			fileExtensionWEBP,
			fileExtensionGIF,
			fileExtensionAVIF:
			return ext
		}
	}

	return ""
}

func responseEmbedContent(content string) (string, string) {
	const responseEmbedImageBlockLineCount = 2

	if strings.TrimSpace(content) == "" {
		return content, ""
	}

	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) < responseEmbedImageBlockLineCount {
		return content, ""
	}

	filteredLines := make([]string, 0, len(lines))
	imageURL := ""

	for index := range len(lines) - 1 {
		line := lines[index]
		if !responseImageLabelLine(line) {
			filteredLines = append(filteredLines, line)

			continue
		}

		candidateURL := strings.TrimSpace(lines[index+1])
		if !isResponseImageURL(candidateURL) {
			filteredLines = append(filteredLines, line)

			continue
		}

		if imageURL == "" {
			imageURL = candidateURL
		}

		filteredLines = append(filteredLines, line)
		lines[index+1] = ""
	}

	if imageURL == "" {
		return content, ""
	}

	lastLine := lines[len(lines)-1]
	if strings.TrimSpace(lastLine) != "" {
		filteredLines = append(filteredLines, lastLine)
	}

	return strings.TrimSpace(strings.Join(filteredLines, "\n")), imageURL
}

func responseImageLabelLine(line string) bool {
	switch strings.TrimSpace(line) {
	case "Generated image:", "Edited image:", "Image output:":
		return true
	default:
		return false
	}
}

func isResponseImageURL(rawURL string) bool {
	parsedURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(parsedURL.Scheme)) {
	case "http", "https":
	default:
		return false
	}

	return strings.TrimSpace(parsedURL.Host) != ""
}

func responseImageAssetFromURL(
	ctx context.Context,
	httpClient *http.Client,
	imageURL string,
) (responseImageAsset, error) {
	contentType, body, err := responseImagePayload(ctx, httpClient, imageURL)
	if err != nil {
		return responseImageAsset{}, err
	}

	return responseImageAsset{
		ID:          "",
		URL:         imageURL,
		ContentType: contentType,
		Data:        body,
	}, nil
}

func responseImagePayload(
	ctx context.Context,
	httpClient *http.Client,
	imageURL string,
) (string, []byte, error) {
	const responseEmbedImageMaxBytes = 20 * 1024 * 1024

	if httpClient == nil {
		return "", nil, errResponseEmbedImageUnavailable
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("create response embed image request: %w", err)
	}

	httpRequest.Header.Set("Accept", "image/*")

	httpResponse, err := httpClient.Do(httpRequest)
	if err != nil {
		return "", nil, fmt.Errorf("fetch response embed image: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return "", nil, fmt.Errorf(
			"fetch response embed image status %d: %w",
			httpResponse.StatusCode,
			os.ErrInvalid,
		)
	}

	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, responseEmbedImageMaxBytes+1))
	if err != nil {
		return "", nil, fmt.Errorf("read response embed image: %w", err)
	}

	if len(body) > responseEmbedImageMaxBytes {
		return "", nil, fmt.Errorf(
			"response embed image exceeds %d bytes: %w",
			responseEmbedImageMaxBytes,
			os.ErrInvalid,
		)
	}

	contentType := normalizedMIMEType(httpResponse.Header.Get(contentTypeHeader))
	detectedContentType := normalizedMIMEType(http.DetectContentType(body))

	switch {
	case strings.HasPrefix(contentType, "image/"):
	case strings.HasPrefix(detectedContentType, "image/"):
		contentType = detectedContentType
	default:
		return "", nil, fmt.Errorf(
			"response embed image content type %q: %w",
			strings.TrimSpace(httpResponse.Header.Get(contentTypeHeader)),
			os.ErrInvalid,
		)
	}

	return contentType, body, nil
}

func contextWindowFooter(usage *tokenUsage, contextWindow int) string {
	if usage == nil || contextWindow <= 0 {
		return ""
	}

	usedTokens := usage.Input + usage.Output
	if usedTokens < 0 {
		return ""
	}

	return fmt.Sprintf(
		"context window: %s/%s (%s used)",
		formatCompactTokenCount(usedTokens),
		formatCompactTokenCount(contextWindow),
		formatContextWindowUsagePercent(usedTokens, contextWindow),
	)
}

func formatContextWindowUsagePercent(usedTokens, contextWindow int) string {
	if usedTokens <= 0 || contextWindow <= 0 {
		return "0%"
	}

	percentage := float64(usedTokens) * contextWindowPercentScale / float64(contextWindow)

	precision := 0
	if percentage < compactTokenCountSmallCutoff &&
		math.Abs(percentage-math.Round(percentage)) >= 0.05 {
		precision = 1
	}

	return formatRoundedFloat(percentage, precision) + "%"
}

func formatCompactTokenCount(count int) string {
	if count < compactTokenCountBase {
		return strconv.Itoa(count)
	}

	value := float64(count)
	for _, unit := range []struct {
		threshold float64
		suffix    string
	}{
		{threshold: compactTokenCountBillion, suffix: "B"},
		{threshold: compactTokenCountMillion, suffix: "M"},
		{threshold: compactTokenCountBase, suffix: "k"},
	} {
		if value < unit.threshold {
			continue
		}

		normalized := value / unit.threshold
		precision := 0

		switch {
		case normalized < compactTokenCountSmallCutoff:
			precision = 2
		case normalized < compactTokenCountMediumCutoff:
			precision = 1
		}

		return formatRoundedFloat(normalized, precision) + unit.suffix
	}

	return strconv.Itoa(count)
}

func formatRoundedFloat(value float64, precision int) string {
	if precision <= 0 {
		return fmt.Sprintf("%.0f", value)
	}

	return strings.TrimRight(
		strings.TrimRight(fmt.Sprintf("%.*f", precision, value), "0"),
		".",
	)
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
