package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
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
	modelName          string
	contextWindow      int
	usage              *tokenUsage
	contextUsage       *tokenUsage
	providerResponseID string
	responseMessages   []*discordgo.Message
	pendingResponses   []pendingResponse
	renderedSpecs      []renderSpec
	progressActive     bool
	responseVisible    bool
}

const (
	discordMessageContentMaxLength = 2000
	userFacingErrorMaxRunes        = 1500
)

var imgbbResponseURLRegexp = regexp.MustCompile(`(?i)\bhttps?://i\.ibb\.co/[^\s<>\]\)]+`)

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

func streamChatCompletionContext(
	ctx context.Context,
	request chatCompletionRequest,
) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}

	return context.WithTimeout(ctx, streamChatCompletionTimeout(request))
}

func streamChatCompletionTimeout(request chatCompletionRequest) time.Duration {
	if request.Provider.APIKind == providerAPIKindOpenAI &&
		request.Provider.UseResponsesAPI &&
		openAIConfiguredModel(request.ConfiguredModel) {
		return openAIResponsesChatCompletionTimeout
	}

	return chatCompletionTimeout
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

func getFallbackModel(currentModel string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(currentModel))

	if normalized == "gemini-search/gemini-3.1-flash-lite-high:vision" ||
		normalized == "gemini-search/gemini-3.1-flash-lite-high" {
		return "openrouter/openrouter/free:vision", true
	}

	if normalized == "openrouter/openrouter/free:vision" ||
		normalized == "openrouter/openrouter/free" {
		return "", false
	}

	return "gemini-search/gemini-3.1-flash-lite-high:vision", true
}

func emptyChatCompletionRequest() chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                       "",
		ConfiguredModel:             "",
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		SessionID:                   "",
		PreviousResponseID:          "",
		RequestID:                   "",
		Messages:                    nil,
	}
}

func (instance *bot) validateFallbackModel(
	loadedConfig config,
	fallbackModel string,
) (providerConfig, bool) {
	fallbackProviderName, _, splitErr := splitConfiguredModel(fallbackModel)
	if splitErr != nil {
		return providerConfig{
			Type:            "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		}, false
	}

	provider, providerExists := loadedConfig.Providers[fallbackProviderName]
	if !providerExists {
		return providerConfig{
			Type:            "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		}, false
	}

	return provider, true
}

func (instance *bot) prepareFallbackRequest(
	ctx context.Context,
	loadedConfig config,
	provider providerConfig,
	fallbackModel string,
	currentRequest chatCompletionRequest,
	tracker *responseTracker,
) (chatCompletionRequest, error) {
	groundingEnabled := instance.currentGroundingEnabled(provider)

	newReq, buildErr := buildChatCompletionRequest(
		loadedConfig,
		fallbackModel,
		currentRequest.Messages,
		groundingEnabled,
	)
	if buildErr != nil {
		return emptyChatCompletionRequest(), buildErr
	}

	newReq.RequestID = currentRequest.RequestID
	newReq.SessionID = currentRequest.SessionID
	newReq.PreviousResponseID = currentRequest.PreviousResponseID

	newReq, _ = instance.autoCompactRequest(ctx, newReq)

	if len(tracker.responseMessages) > 0 && tracker.progressActive {
		progressMessage := tracker.responseMessages[0]

		if progressMessage != nil {
			embed := buildRequestProgressEmbed(
				requestProgressStageGeneratingResponse,
				strings.TrimSpace(fallbackModel),
			)

			_ = instance.editEmbedMessage(progressMessage, embed, nil)
		}
	}

	if len(tracker.responseMessages) > 1 {
		_ = instance.trimExtraEmbedResponses(ctx, tracker, 1)
	}

	tracker.usage = nil
	tracker.providerResponseID = ""
	tracker.renderedSpecs = nil
	tracker.progressActive = true

	return newReq, nil
}

func (instance *bot) attemptFallback(
	ctx context.Context,
	currentRequest chatCompletionRequest,
	tracker *responseTracker,
) (chatCompletionRequest, bool) {
	fallbackModel := currentRequest.ConfiguredModel

	for {
		var hasFallback bool

		fallbackModel, hasFallback = getFallbackModel(fallbackModel)
		if !hasFallback {
			return emptyChatCompletionRequest(), false
		}

		if instance.configPath == "" {
			return emptyChatCompletionRequest(), false
		}

		loadedConfig, configErr := loadConfig(instance.configPath)
		if configErr != nil {
			slog.Error("failed to load config for fallback", "error", configErr)

			return emptyChatCompletionRequest(), false
		}

		provider, providerExists := instance.validateFallbackModel(loadedConfig, fallbackModel)
		if !providerExists {
			continue
		}

		newReq, buildErr := instance.prepareFallbackRequest(
			ctx,
			loadedConfig,
			provider,
			fallbackModel,
			currentRequest,
			tracker,
		)
		if buildErr != nil {
			slog.Error("failed to prepare fallback request", "model", fallbackModel, "error", buildErr)

			continue
		}

		slog.Info("Falling back to model", "from", tracker.modelName, "to", fallbackModel)

		return newReq, true
	}
}

func (instance *bot) runGenerationAttempt(
	ctx context.Context,
	request chatCompletionRequest,
	tracker *responseTracker,
	warnings []string,
	maxLength int,
	usePlainResponses bool,
) (string, string, *searchMetadata, error) {
	accumulator := newSegmentAccumulator(maxLength)
	thinkingAccumulator := newSegmentAccumulator(maxLength)

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

	streamContext, cancelStream := streamChatCompletionContext(ctx, request)
	streamErr := instance.chatCompletions.streamChatCompletion(
		streamContext,
		request,
		func(delta streamDelta) error {
			return instance.handleGeneratedStreamDelta(ctx, tracker, &streamState, delta)
		},
	)

	cancelStream()

	if streamErr != nil && finishReason == "" {
		finishReason = "error"
	}

	finalAnswerText := streamState.rawAnswerText
	cleanedAnswerText, parsedSearchMetadata := finalizeXAIResponseAnswer(
		request,
		finalAnswerText,
		tracker.searchMetadata,
	)

	finalAccumulator := accumulator

	if cleanedAnswerText != finalAnswerText {
		finalAccumulator = newSegmentAccumulator(maxLength)

		_ = finalAccumulator.appendText(cleanedAnswerText)
	}

	responseErr := instance.renderFinalResponse(
		ctx,
		request,
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

	return cleanedAnswerText, thinkingAccumulator.joined(), parsedSearchMetadata, responseErr
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

	currentRequest := request

	for {
		tracker.modelName = strings.TrimSpace(currentRequest.ConfiguredModel)
		tracker.contextWindow = currentRequest.ContextWindow

		cleanedText, thinkingText, parsedMetadata, responseErr := instance.runGenerationAttempt(
			ctx,
			currentRequest,
			tracker,
			warnings,
			maxLength,
			usePlainResponses,
		)
		if responseErr == nil {
			if parsedMetadata != nil {
				tracker.searchMetadata = mergeSearchMetadata(tracker.searchMetadata, parsedMetadata)
			}

			finalText := visibleResponseText(thinkingText, cleanedText)

			tracker.release(instance.nodes, finalText, thinkingText)

			instance.nodes.persistBestEffort()

			return nil
		}

		var hasFallback bool

		currentRequest, hasFallback = instance.attemptFallback(ctx, currentRequest, tracker)

		if hasFallback {
			warnings = append(
				warnings,
				fmt.Sprintf("Warning: fell back to %s due to generation error", currentRequest.ConfiguredModel),
			)

			continue
		}

		errorText := userFacingResponseError(responseErr)

		renderErr := instance.renderFailureResponse(ctx, tracker, errorText, usePlainResponses)

		var finalText string

		if renderErr != nil {
			responseErr = errors.Join(responseErr, fmt.Errorf("render failure response: %w", renderErr))
			finalText = visibleResponseText(thinkingText, cleanedText)
		} else {
			finalText = responseTextWithError(
				visibleResponseText(thinkingText, cleanedText),
				errorText,
			)
		}

		tracker.release(instance.nodes, finalText, thinkingText)

		instance.nodes.persistBestEffort()

		return responseErr
	}
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
	if delta.FinishReason == finishReasonRetryReset {
		state.answerAccumulator.segments = []string{""}
		state.thinkingAccumulator.segments = []string{""}
		state.rawAnswerText = ""
		state.renderedAnswerText = ""
		*state.finishReason = ""
		tracker.usage = nil
		tracker.providerResponseID = ""
		tracker.searchMetadata = nil

		return nil
	}

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
	request chatCompletionRequest,
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

		instance.sendImgbbURLReplies(tracker, accumulator.joined())

		return nil
	}

	tracker.contextUsage = retainedContextWindowUsage(
		request,
		visibleResponseText(thinkingText, accumulator.joined()),
	)

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

	instance.sendImgbbURLReplies(tracker, accumulator.joined())

	return nil
}

func userFacingResponseError(err error) string {
	const (
		genericResponseErrorText  = "Couldn't generate a response right now. Try again."
		invalidProviderErrorText  = "The provider returned an invalid or oversized error response. Try again."
		timedOutResponseErrorText = "The model timed out while processing the request. Try again."
		truncatedErrorSuffix      = " [truncated]"
	)

	if err == nil {
		return genericResponseErrorText
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return timedOutResponseErrorText
	}

	errorText := strings.TrimSpace(err.Error())
	if errorText == "" {
		return genericResponseErrorText
	}

	if openAIHTTPErrorBodyLooksOpaque(errorText) {
		return invalidProviderErrorText
	}

	if runeCount(errorText) > userFacingErrorMaxRunes {
		truncateAt := max(0, userFacingErrorMaxRunes-runeCount(truncatedErrorSuffix))
		if truncateAt == 0 {
			return invalidProviderErrorText
		}

		return truncateRunes(errorText, truncateAt) + truncatedErrorSuffix
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
			tracker.footerUsage(),
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

		err := instance.renderEmbedSpec(ctx, tracker, index, embed, spec.actions)
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

func (tracker *responseTracker) footerUsage() *tokenUsage {
	if tracker == nil {
		return nil
	}

	if tracker.contextUsage != nil {
		return tracker.contextUsage
	}

	return tracker.usage
}

func (instance *bot) renderEmbedSpec(
	ctx context.Context,
	tracker *responseTracker,
	index int,
	embed *discordgo.MessageEmbed,
	actions responseActions,
) error {
	if index >= len(tracker.responseMessages) {
		sentMessage, pending, err := instance.sendEmbedMessage(
			tracker,
			embed,
			actions,
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
		tracker.responseMessages[index],
		embed,
		buildEmbedComponents(actions),
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
		tracker.responseMessages[0],
		failureEmbed,
		nil,
	)
	if err != nil {
		return false, fmt.Errorf("edit progress message: %w", err)
	}

	tracker.responseVisible = true

	return true, nil
}

func (instance *bot) sendFallbackFailureResponse(
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
		fallbackTracker,
		failureEmbed,
		responseActions{showSources: false, showThinking: false, showRentry: false},
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

func imgbbResponseURLs(text string) []string {
	rawURLs := imgbbResponseURLRegexp.FindAllString(text, -1)
	urls := make([]string, 0, len(rawURLs))
	seenURLs := make(map[string]struct{}, len(rawURLs))

	for _, rawURL := range rawURLs {
		normalizedURL, err := normalizeWebsiteURL(rawURL)
		if err != nil {
			continue
		}

		if _, ok := seenURLs[normalizedURL]; ok {
			continue
		}

		seenURLs[normalizedURL] = struct{}{}
		urls = append(urls, normalizedURL)
	}

	return urls
}

func contentBatchesForLines(lines []string, maxLength int) []string {
	if maxLength <= 0 {
		return nil
	}

	batches := make([]string, 0, len(lines))
	currentBatch := ""

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if currentBatch == "" {
			currentBatch = line

			continue
		}

		nextBatch := currentBatch + "\n" + line
		if runeCount(nextBatch) > maxLength {
			batches = append(batches, currentBatch)
			currentBatch = line

			continue
		}

		currentBatch = nextBatch
	}

	if currentBatch != "" {
		batches = append(batches, currentBatch)
	}

	return batches
}

func (instance *bot) sendImgbbURLReplies(tracker *responseTracker, answerText string) {
	if instance == nil || instance.session == nil || tracker == nil || len(tracker.responseMessages) == 0 {
		return
	}

	responseMessage := tracker.responseMessages[len(tracker.responseMessages)-1]

	replyBatches := contentBatchesForLines(
		imgbbResponseURLs(answerText),
		discordMessageContentMaxLength,
	)
	if len(replyBatches) == 0 {
		return
	}

	for _, replyBatch := range replyBatches {
		send := newReplyMessage(responseMessage)
		send.Content = replyBatch

		sentMessage, err := instance.session.ChannelMessageSendComplex(responseMessage.ChannelID, send)
		if err != nil {
			slog.Warn(
				"send imgbb url reply",
				"channel_id",
				responseMessage.ChannelID,
				"message_id",
				responseMessage.ID,
				"error",
				err,
			)

			return
		}

		instance.cacheAuxiliaryAssistantReply(sentMessage, responseMessage, tracker)
	}
}

func (instance *bot) cacheAuxiliaryAssistantReply(
	sentMessage *discordgo.Message,
	parentMessage *discordgo.Message,
	tracker *responseTracker,
) {
	if instance == nil || instance.nodes == nil || sentMessage == nil {
		return
	}

	node := instance.nodes.getOrCreate(sentMessage.ID)
	node.mu.Lock()
	defer node.mu.Unlock()

	node.role = messageRoleAssistant
	node.text = ""
	node.thinkingText = ""
	node.urlScanText = ""
	node.rentryURL = ""
	node.providerResponseID = strings.TrimSpace(tracker.providerResponseID)
	node.providerResponseModel = strings.TrimSpace(tracker.modelName)
	node.media = nil
	node.searchMetadata = cloneSearchMetadata(tracker.searchMetadata)
	node.hasBadAttachments = false
	node.attachmentDownloadFailed = false
	node.fetchParentFailed = false
	node.parentMessage = parentMessage
	node.initialized = true

	instance.nodes.cacheLockedNode(sentMessage.ID, node)
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

func retainedContextWindowUsage(request chatCompletionRequest, responseText string) *tokenUsage {
	usage := new(tokenUsage)
	usage.Input = estimateChatCompletionRequestTokens(request)
	usage.Output = estimateChatMessageTokens(chatMessage{
		Role:    messageRoleAssistant,
		Content: responseText,
	})

	return usage
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
