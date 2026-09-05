package app

import (
	"context"
	"errors"
	"fmt"
	providers "llmcord-go/internal/providers"
	searchtypes "llmcord-go/internal/searchtypes"
	"regexp"
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
	showImages   bool
	showThinking bool
	showGist     bool
}

type pendingResponse struct {
	messageID string
	node      *messageNode
}

type responseTracker struct {
	sourceMessage         *discordgo.Message
	searchMetadata        *searchMetadata
	modelName             string
	originalModel         string
	providerResponseID    string
	responseMessages      []*discordgo.Message
	pendingResponses      []pendingResponse
	renderedSpecs         []renderSpec
	pixelVaultRepliedURLs map[string]struct{}
	progressActive        bool
	responseVisible       bool
	originalMessages      []chatMessage
}

const (
	discordMessageContentMaxLength = 2000
	userFacingErrorMaxRunes        = 1500
)

var pixelVaultResponseURLRegexp = regexp.MustCompile(`(?i)\bhttps?://img\.pixelvault\.dev/[^\s<>\]\)]+`)

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

var (
	errStreamedAnswerVisibilityRegressed = errors.New("streamed answer visibility regressed")
	errEmptyModelResponse                = errors.New("model returned an empty response")
	errNilSession                        = errors.New("session is nil")
)

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

// splitInlineThinkingAnswer separates a model-emitted "**Thinking** ...
// **Answer** ..." prefix in Content from the visible answer. Provider-native
// reasoning arrives on the Thinking channel and never hits Content, but
// stored history used to feed the "**Thinking**/**Answer**" wrapper back to
// the model, teaching it to reproduce the markers inline. The split is
// streaming-safe: a marker fragment or a trailing partial "**Answer**"
// separator is withheld until more deltas arrive, so thinking never flashes
// into the visible answer.
func splitInlineThinkingAnswer(raw string) (string, string) {
	if strings.TrimSpace(raw) == "" {
		return "", ""
	}

	trimmedLeading := strings.TrimLeft(raw, " \t\r\n")
	if trimmedLeading == "" {
		return "", ""
	}

	if len(trimmedLeading) < len(thinkingResponsePrefix) {
		if strings.HasPrefix(thinkingResponsePrefix, trimmedLeading) {
			return "", ""
		}

		return "", raw
	}

	if !strings.HasPrefix(trimmedLeading, thinkingResponsePrefix) {
		return "", raw
	}

	body := strings.TrimPrefix(trimmedLeading, thinkingResponsePrefix)
	if thinking, answer, found := strings.Cut(body, answerResponseSeparator); found {
		return thinking, answer
	}

	if pending := trailingSeparatorPrefixLength(body, answerResponseSeparator); pending > 0 {
		return body[:len(body)-pending], ""
	}

	return body, ""
}

// assistantHistoryAnswerText returns the answer-only text fed to the model
// for an assistant history turn. Stored assistant text uses the
// "**Thinking**/**Answer**" wrapper; sending it verbatim teaches the model
// to reproduce the markers inline.
func assistantHistoryAnswerText(fullText string) string {
	_, answer := splitInlineThinkingAnswer(fullText)

	return answer
}

func trailingSeparatorPrefixLength(text, separator string) int {
	maxLength := min(len(text), len(separator)-1)
	for length := maxLength; length > 0; length-- {
		if strings.HasSuffix(text, separator[:length]) {
			return length
		}
	}

	return 0
}

func visibleResponseSegments(answerText string, maxLength int) []string {
	if answerText == "" {
		return nil
	}

	accumulator := newSegmentAccumulator(maxLength)
	_ = accumulator.appendText(answerText)

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
	tracker.originalModel = tracker.modelName

	return tracker
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

// generatedPrefill carries text accumulated during a web_search tool round
// into the follow-up stream, so thinking and partial answers produced before
// the tool call are not lost from the final render.
type generatedPrefill struct {
	rawAnswer string
	thinking  string
}

type generatedRoundResult struct {
	rawAnswer string
	thinking  string
	metadata  *searchMetadata
	toolCalls []providers.FunctionToolCall
}

func (instance *bot) runGenerationRound(
	ctx context.Context,
	request chatCompletionRequest,
	tracker *responseTracker,
	warnings []string,
	prefill generatedPrefill,
) (generatedRoundResult, string, error) {
	accumulator := newSegmentAccumulator(embedResponseMaxLength)
	thinkingAccumulator := newSegmentAccumulator(embedResponseMaxLength)

	var finishReason string

	lastRenderTime := time.Time{}

	streamState := generatedStreamState{
		request:                request,
		warnings:               warnings,
		answerAccumulator:      &accumulator,
		thinkingAccumulator:    &thinkingAccumulator,
		finishReason:           &finishReason,
		lastRenderTime:         &lastRenderTime,
		rawAnswerText:          "",
		renderedAnswerText:     "",
		inlineThinkingRendered: "",
		inlineBaseLength:       0,
		toolCalls:              nil,
	}

	if prefill.rawAnswer != "" {
		streamState.rawAnswerText = prefill.rawAnswer
		streamState.renderedAnswerText = providers.StreamingBridgeSourceAppendixVisibleText(prefill.rawAnswer)

		_ = accumulator.appendText(streamState.renderedAnswerText)
	}

	streamState.inlineBaseLength = len(streamState.rawAnswerText)

	if prefill.thinking != "" {
		_ = thinkingAccumulator.appendText(prefill.thinking)
	}

	streamErr := instance.chatCompletions.StreamChatCompletion(
		ctx,
		request,
		func(delta streamDelta) error {
			return instance.handleGeneratedStreamDelta(ctx, tracker, &streamState, delta)
		},
	)

	if streamErr != nil && finishReason == "" {
		finishReason = providers.OpenAIStreamErrorEventType
	}

	if streamErr == nil && len(streamState.toolCalls) > 0 {
		// Tool round: hand off to the web search phase without finalizing
		// the response render. The round's text is stripped of any inline
		// "**Thinking**/**Answer**" prefix first, then of any bridge source
		// appendix — carried raw, either would sit mid-text after
		// concatenation with the follow-up round and leak into the visible
		// answer or make the final finalize treat the real answer as
		// appendix content.
		appendUnstreamedInlineThinking(&streamState, &thinkingAccumulator)

		_, inlineAnswer := roundInlineThinkingAndAnswer(&streamState)

		roundAnswerText, parsedMetadata := providers.FinalizeBridgeSourceAppendixAnswer(
			inlineAnswer,
			tracker.searchMetadata,
		)

		if parsedMetadata != nil {
			tracker.searchMetadata = searchtypes.MergeSearchMetadata(tracker.searchMetadata, parsedMetadata)
		}

		return generatedRoundResult{
			rawAnswer: roundAnswerText,
			thinking:  thinkingAccumulator.joined(),
			metadata:  nil,
			toolCalls: streamState.toolCalls,
		}, finishReason, nil
	}

	return instance.finalizeGenerationRound(
		ctx,
		tracker,
		warnings,
		streamState,
		&accumulator,
		&thinkingAccumulator,
		finishReason,
		streamErr,
	)
}

// finalizeGenerationRound finalizes a non-tool streaming round: it strips an
// inline "**Thinking**/**Answer**" prefix into the thinking channel, then the
// bridge source appendix, merges its metadata into the tracker, renders the
// final embeds, and reports empty responses as errors.
func (instance *bot) finalizeGenerationRound(
	ctx context.Context,
	tracker *responseTracker,
	warnings []string,
	streamState generatedStreamState,
	accumulator *segmentAccumulator,
	thinkingAccumulator *segmentAccumulator,
	finishReason string,
	streamErr error,
) (generatedRoundResult, string, error) {
	appendUnstreamedInlineThinking(&streamState, thinkingAccumulator)

	_, inlineAnswer := roundInlineThinkingAndAnswer(&streamState)

	finalAnswerText := inlineAnswer

	cleanedAnswerText, parsedSearchMetadata := providers.FinalizeBridgeSourceAppendixAnswer(
		finalAnswerText,
		tracker.searchMetadata,
	)

	if parsedSearchMetadata != nil {
		tracker.searchMetadata = searchtypes.MergeSearchMetadata(tracker.searchMetadata, parsedSearchMetadata)
	}

	finalAccumulator := *accumulator

	if cleanedAnswerText != finalAnswerText {
		finalAccumulator = newSegmentAccumulator(embedResponseMaxLength)

		_ = finalAccumulator.appendText(cleanedAnswerText)
	}

	responseErr := instance.renderFinalResponse(
		ctx,
		tracker,
		warnings,
		&finalAccumulator,
		thinkingAccumulator.joined(),
		finishReason,
	)

	if responseErr == nil && streamErr != nil {
		responseErr = fmt.Errorf("stream response: %w", streamErr)
	}

	if responseErr == nil &&
		strings.TrimSpace(visibleResponseText(thinkingAccumulator.joined(), cleanedAnswerText)) == "" {
		responseErr = errEmptyModelResponse
	}

	return generatedRoundResult{
		rawAnswer: cleanedAnswerText,
		thinking:  thinkingAccumulator.joined(),
		metadata:  parsedSearchMetadata,
		toolCalls: nil,
	}, finishReason, responseErr
}

// runGenerationRoundWithRetry wraps runGenerationRound with a bounded
// same-model retry for streams that end without delivering a finish reason
// or fail transiently mid-stream (for example a Responses stream dropped
// before response.completed/response.done, surfaced as unexpected EOF):
// the provider closed the stream mid-response, the partial text renders as an
// incomplete message, and nothing else surfaces the failure. Each retry runs
// with fresh accumulators and re-renders over the tracker's existing messages
// in place, so a truncated reply is replaced rather than duplicated, and
// auxiliary PixelVault url replies are claimed once per response. Up to
// prematureStreamRetryMaxAttempts streams are attempted in total; exhausted
// retries keep the existing behavior of releasing the truncated reply.
func (instance *bot) runGenerationRoundWithRetry(
	ctx context.Context,
	request chatCompletionRequest,
	tracker *responseTracker,
	warnings []string,
	prefill generatedPrefill,
) (generatedRoundResult, error) {
	round, finishReason, attemptErr := instance.runGenerationRound(
		ctx,
		request,
		tracker,
		warnings,
		prefill,
	)

	for attempt := 2; attempt <= prematureStreamRetryMaxAttempts; attempt++ {
		if !shouldRetryGenerationRound(finishReason, attemptErr) {
			break
		}

		if providers.IsTransientStreamError(attemptErr) {
			logWarn(
				"transient stream error; retrying generation",
				attemptErr,
				"attempt",
				attempt,
				"max_attempts",
				prematureStreamRetryMaxAttempts,
			)
		} else {
			logWarn(
				"stream ended without finish reason; retrying generation",
				nil,
				"attempt",
				attempt,
				"max_attempts",
				prematureStreamRetryMaxAttempts,
			)
		}

		sleepErr := sleepPrematureStreamRetry(ctx, prematureStreamRetryFixedDelay)
		if sleepErr != nil {
			return round, sleepErr
		}

		round, finishReason, attemptErr = instance.runGenerationRound(
			ctx,
			request,
			tracker,
			warnings,
			prefill,
		)
	}

	if attemptErr == nil && finishReason == "" {
		logWarn(
			"stream kept ending without finish reason; giving up",
			nil,
			"max_attempts",
			prematureStreamRetryMaxAttempts,
		)
	}

	if providers.IsTransientStreamError(attemptErr) {
		logWarn(
			"stream kept failing transiently; giving up",
			attemptErr,
			"max_attempts",
			prematureStreamRetryMaxAttempts,
		)
	}

	return round, attemptErr
}

// shouldRetryGenerationRound reports whether another same-model generation
// attempt may recover a complete reply. A clean close without a finish
// reason is always retried. A transient stream failure (dropped connection
// before [DONE] or before response.completed, surfaced as unexpected EOF
// and classified by providers.IsTransientStreamError) is also retried: the
// provider layer never re-sends after visible content to avoid duplicating
// a partial reply, but each generation round uses fresh accumulators and
// re-renders in place, so retrying here replaces the truncated reply.
// Anything else (including non-transient provider errors) is returned
// unchanged.
func shouldRetryGenerationRound(finishReason string, err error) bool {
	if err == nil && finishReason == "" {
		return true
	}

	return err != nil && providers.IsTransientStreamError(err)
}

// generateResponseWithWebSearchTool streams the request; when the model
// responds with web_search tool calls, it executes them through the routed
// TinyFish -> Exa -> Tavily clients, appends the results to the request
// messages, and streams a final answer.
//
// Tool calling is never disabled: every round re-offers the tools with
// tool_choice "auto" (and a byte-identical tool prefix, so the provider's
// prompt cache keeps matching). The loop is bounded by
// maxWebSearchToolRounds so a model that keeps calling tools without
// answering cannot run away; hitting the cap surfaces the empty-response
// error instead of silently refusing further tool calls.
func (instance *bot) generateResponseWithWebSearchTool(
	ctx context.Context,
	loadedConfig config,
	request chatCompletionRequest,
	tracker *responseTracker,
	warnings []string,
) (string, string, error) {
	var prefill generatedPrefill

	for roundIndex := 0; ; roundIndex++ {
		round, roundErr := instance.runGenerationRoundWithRetry(
			ctx,
			request,
			tracker,
			warnings,
			prefill,
		)
		if roundErr != nil {
			return round.rawAnswer, round.thinking, roundErr
		}

		if len(round.toolCalls) == 0 {
			return round.rawAnswer, round.thinking, nil
		}

		if len(request.Tools) == 0 {
			// The provider emitted tool calls although none were offered;
			// there is nothing to execute, so surface the empty response.
			logWarn(
				"model emitted tool calls without offered tools",
				nil,
				"tool_calls",
				len(round.toolCalls),
			)

			return round.rawAnswer, round.thinking, errEmptyModelResponse
		}

		if roundIndex >= maxWebSearchToolRounds {
			logWarn(
				"web_search tool round cap reached without a final answer",
				nil,
				"rounds",
				roundIndex+1,
				"max_rounds",
				maxWebSearchToolRounds,
			)

			return round.rawAnswer, round.thinking, errEmptyModelResponse
		}

		var searchWarnings []string

		var augmentedMessages []chatMessage

		var searched bool

		augmentedMessages, searchWarnings, searched = instance.runWebSearchToolPhase(
			ctx,
			loadedConfig,
			tracker,
			request.Messages,
			warnings,
			round.toolCalls,
		)
		warnings = searchWarnings

		if searched {
			request.Messages = augmentedMessages
		}

		prefill = generatedPrefill{
			rawAnswer: round.rawAnswer,
			thinking:  round.thinking,
		}
	}
}

func sleepPrematureStreamRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for generation retry: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (instance *bot) generateAndSendResponse(
	ctx context.Context,
	loadedConfig config,
	request chatCompletionRequest,
	tracker *responseTracker,
	warnings []string,
) error {
	tracker.modelName = strings.TrimSpace(request.ConfiguredModel)

	cleanedText, thinkingText, responseErr := instance.generateResponseWithWebSearchTool(
		ctx,
		loadedConfig,
		request,
		tracker,
		warnings,
	)
	if responseErr == nil {
		finalText := visibleResponseText(thinkingText, cleanedText)

		tracker.release(instance.nodes, finalText, thinkingText)

		instance.nodes.persistBestEffort()

		return nil
	}

	fallbackModel := strings.TrimSpace(loadedConfig.FallbackModel)

	if fallbackModel != "" &&
		fallbackModel != strings.TrimSpace(request.ConfiguredModel) &&
		loadedConfig.hasModel(fallbackModel) {
		cleanedText, thinkingText, responseErr = instance.attemptFallbackResponse(
			ctx,
			loadedConfig,
			request,
			tracker,
			warnings,
			fallbackModel,
			responseErr,
			cleanedText,
			thinkingText,
		)
		if responseErr == nil {
			return nil
		}
	}

	errorText := userFacingResponseError(responseErr)

	renderErr := instance.renderFailureResponse(ctx, tracker, errorText)

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

// attemptFallbackResponse retries a failed generation with the fallback
// model. It returns the resulting error and text; a nil error means the
// fallback reply rendered successfully.
func (instance *bot) attemptFallbackResponse(
	ctx context.Context,
	loadedConfig config,
	request chatCompletionRequest,
	tracker *responseTracker,
	warnings []string,
	fallbackModel string,
	primaryErr error,
	primaryText string,
	primaryThinking string,
) (string, string, error) {
	fallbackRequest, buildErr := instance.buildFallbackRequest(
		loadedConfig,
		fallbackModel,
		request,
		tracker,
	)
	if buildErr != nil {
		logWarn(
			"failed to build fallback request",
			buildErr,
			"configured_model",
			request.ConfiguredModel,
			"fallback_model",
			fallbackModel,
		)

		return primaryText, primaryThinking, primaryErr
	}

	logWarn(
		"chat completion failed; retrying with fallback model",
		primaryErr,
		"configured_model",
		request.ConfiguredModel,
		"fallback_model",
		fallbackModel,
	)

	tracker.modelName = fallbackModel
	tracker.providerResponseID = ""
	tracker.renderedSpecs = nil

	fallbackWarnings := appendFallbackWarning(warnings, fallbackModel)

	cleanedText, thinkingText, fallbackErr := instance.generateResponseWithWebSearchTool(
		ctx,
		loadedConfig,
		fallbackRequest,
		tracker,
		fallbackWarnings,
	)
	if fallbackErr == nil {
		finalText := visibleResponseText(thinkingText, cleanedText)

		tracker.release(instance.nodes, finalText, thinkingText)

		instance.nodes.persistBestEffort()

		return cleanedText, thinkingText, nil
	}

	return cleanedText, thinkingText, fallbackErr
}

type generatedStreamState struct {
	request                chatCompletionRequest
	warnings               []string
	answerAccumulator      *segmentAccumulator
	thinkingAccumulator    *segmentAccumulator
	finishReason           *string
	lastRenderTime         *time.Time
	rawAnswerText          string
	renderedAnswerText     string
	inlineThinkingRendered string
	inlineBaseLength       int
	toolCalls              []providers.FunctionToolCall
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

	if len(delta.ToolCalls) > 0 {
		state.toolCalls = append(state.toolCalls, delta.ToolCalls...)
	}

	if delta.FinishReason != "" {
		*state.finishReason = delta.FinishReason
	}

	if strings.TrimSpace(delta.ProviderResponseID) != "" {
		tracker.providerResponseID = strings.TrimSpace(delta.ProviderResponseID)
	}

	if delta.SearchMetadata != nil {
		tracker.searchMetadata = searchtypes.MergeSearchMetadata(tracker.searchMetadata, delta.SearchMetadata)
	}

	segments := visibleResponseSegments(
		state.answerAccumulator.joined(),
		embedResponseMaxLength,
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
		return fmt.Errorf("render streaming response: %w", err)
	}

	*state.lastRenderTime = time.Now()

	return nil
}

// roundInlineThinkingAndAnswer splits the round-local stream tail — the bytes
// after the tool prefill base — into thinking and full answer parts. The
// prefill base is already-clean answer text from a prior tool round, so only
// the tail can open a new inline "**Thinking**/**Answer**" block; splitting
// the concatenated string would miss follow-up markers sitting mid-text.
func roundInlineThinkingAndAnswer(streamState *generatedStreamState) (string, string) {
	baseLength := min(streamState.inlineBaseLength, len(streamState.rawAnswerText))
	base := streamState.rawAnswerText[:baseLength]
	thinking, tailAnswer := splitInlineThinkingAnswer(streamState.rawAnswerText[baseLength:])

	return thinking, base + tailAnswer
}

func (state *generatedStreamState) appendAnswerText(answerDelta string) (bool, error) {
	state.rawAnswerText += answerDelta

	inlineThinking, inlineAnswer := roundInlineThinkingAndAnswer(state)
	if len(inlineThinking) > len(state.inlineThinkingRendered) &&
		strings.HasPrefix(inlineThinking, state.inlineThinkingRendered) {
		remainder := strings.TrimPrefix(inlineThinking, state.inlineThinkingRendered)
		state.inlineThinkingRendered = inlineThinking

		if remainder != "" {
			_ = state.thinkingAccumulator.appendText(remainder)
		}
	}

	visibleAnswerText := providers.StreamingBridgeSourceAppendixVisibleText(inlineAnswer)
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

func appendUnstreamedInlineThinking(
	streamState *generatedStreamState,
	thinkingAccumulator *segmentAccumulator,
) {
	inlineThinking, _ := roundInlineThinkingAndAnswer(streamState)
	if len(inlineThinking) > len(streamState.inlineThinkingRendered) &&
		strings.HasPrefix(inlineThinking, streamState.inlineThinkingRendered) {
		remainder := strings.TrimPrefix(inlineThinking, streamState.inlineThinkingRendered)
		streamState.inlineThinkingRendered = inlineThinking

		if remainder != "" {
			_ = thinkingAccumulator.appendText(remainder)
		}
	}
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
) error {
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

	instance.sendPixelVaultURLReplies(tracker, accumulator.joined())

	return nil
}

func userFacingResponseError(err error) string {
	const (
		genericResponseErrorText = "Couldn't generate a response right now. Try again."
		invalidProviderErrorText = "The provider returned an invalid or oversized error response. Try again."
		truncatedErrorSuffix     = " [truncated]"
	)

	if err == nil {
		return genericResponseErrorText
	}

	if errors.Is(err, errEmptyModelResponse) {
		return "The model returned an empty response. Try again."
	}

	errorText := strings.TrimSpace(err.Error())
	if errorText == "" {
		return genericResponseErrorText
	}

	if providers.OpenAIHTTPErrorBodyLooksOpaque(errorText) {
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
) error {
	if instance == nil || instance.session == nil || tracker == nil {
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

	return instance.sendFailureResponse(tracker, failureEmbed, renderErr)
}

func (instance *bot) sendFailureResponse(
	tracker *responseTracker,
	failureEmbed *discordgo.MessageEmbed,
	renderErr error,
) error {
	failureTracker := newResponseTracker(tracker.sourceMessage, tracker.modelName)
	failureTracker.originalModel = tracker.originalModel
	failureTracker.searchMetadata = cloneSearchMetadata(tracker.searchMetadata)
	failureTracker.responseMessages = append(failureTracker.responseMessages, tracker.responseMessages...)

	sentMessage, pending, err := instance.sendEmbedMessage(
		failureTracker,
		failureEmbed,
		responseActions{showSources: false, showThinking: false, showGist: false},
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
				showImages:   final && index == len(segments)-1,
				showThinking: final && hasThinking && index == len(segments)-1,
				showGist:     final && index == len(segments)-1,
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
	err := instance.trimExtraResponseMessages(ctx, tracker, keepCount)
	if err != nil {
		return err
	}

	if len(tracker.renderedSpecs) > keepCount {
		tracker.renderedSpecs = tracker.renderedSpecs[:keepCount]
	}

	return nil
}

func (instance *bot) trimExtraResponseMessages(
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
		ctx,
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

func pixelVaultResponseURLs(text string) []string {
	rawURLs := pixelVaultResponseURLRegexp.FindAllString(text, -1)
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

// unrepliedPixelVaultURLs filters urls down to those this response has not
// replied with yet and claims them on the tracker, so a retried generation
// attempt never re-posts duplicate PixelVault url replies.
func unrepliedPixelVaultURLs(tracker *responseTracker, urls []string) []string {
	if len(urls) == 0 {
		return urls
	}

	if tracker.pixelVaultRepliedURLs == nil {
		tracker.pixelVaultRepliedURLs = make(map[string]struct{}, len(urls))
	}

	unreplied := make([]string, 0, len(urls))

	for _, url := range urls {
		if _, replied := tracker.pixelVaultRepliedURLs[url]; replied {
			continue
		}

		tracker.pixelVaultRepliedURLs[url] = struct{}{}

		unreplied = append(unreplied, url)
	}

	return unreplied
}

func (instance *bot) sendPixelVaultURLReplies(tracker *responseTracker, answerText string) {
	if instance == nil || instance.session == nil || tracker == nil || len(tracker.responseMessages) == 0 {
		return
	}

	responseMessage := tracker.responseMessages[len(tracker.responseMessages)-1]

	replyURLs := unrepliedPixelVaultURLs(tracker, pixelVaultResponseURLs(answerText))
	if len(replyURLs) == 0 {
		return
	}

	replyBatches := contentBatchesForLines(
		replyURLs,
		discordMessageContentMaxLength,
	)

	for _, replyBatch := range replyBatches {
		send := newReplyMessage(responseMessage)
		send.Content = replyBatch

		sentMessage, err := instance.session.ChannelMessageSendComplex(responseMessage.ChannelID, send)
		if err != nil {
			logWarn(
				"send PixelVault url reply",
				err,
				"channel_id",
				responseMessage.ChannelID,
				"message_id",
				responseMessage.ID,
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
	node.gistURL = ""
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
	if instance == nil || instance.session == nil {
		return nil, pendingResponse{}, errNilSession
	}

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
	if instance == nil || instance.session == nil {
		return errNilSession
	}

	edit := discordgo.NewMessageEdit(message.ChannelID, message.ID)
	edit.SetEmbeds([]*discordgo.MessageEmbed{embed})
	edit.Components = &components

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

func buildEmbedComponents(actions responseActions) []discordgo.MessageComponent {
	buttons := buildResponseButtons(actions)
	if len(buttons) == 0 {
		return nil
	}

	row := new(discordgo.ActionsRow)
	row.Components = buttons

	return []discordgo.MessageComponent{row}
}

func buildResponseButtons(actions responseActions) []discordgo.MessageComponent {
	const maxResponseButtons = 4

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

	if actions.showImages {
		button := new(discordgo.Button)
		button.CustomID = showImagesButtonCustomID
		button.Label = showImagesButtonLabel
		button.Style = discordgo.SecondaryButton

		buttons = append(buttons, button)
	}

	if actions.showGist {
		button := new(discordgo.Button)
		button.CustomID = createGistButtonCustomID
		button.Label = createGistButtonLabel
		button.Style = discordgo.SecondaryButton

		buttons = append(buttons, button)
	}

	return buttons
}

func fallbackModelWarning(fallbackModel string) string {
	trimmed := strings.TrimSpace(fallbackModel)
	if trimmed == "" {
		return ""
	}

	return fmt.Sprintf("Warning: fallback to %s", trimmed)
}

func appendFallbackWarning(warnings []string, fallbackModel string) []string {
	warningText := fallbackModelWarning(fallbackModel)
	if warningText == "" {
		return warnings
	}

	warningSet := make(map[string]struct{}, len(warnings)+1)
	for _, warning := range warnings {
		appendUniqueWarning(warningSet, warning)
	}

	appendUniqueWarning(warningSet, warningText)

	return sortedWarnings(warningSet)
}
