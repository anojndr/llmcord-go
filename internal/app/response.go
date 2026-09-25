package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	providers "llmcord-go/internal/providers"
	searchtypes "llmcord-go/internal/searchtypes"
	"net/http"
	"regexp"
	"slices"
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
	showExport   bool
}

type pendingResponse struct {
	messageID string
	node      *messageNode
}

type responseTracker struct {
	sourceMessage      *discordgo.Message
	searchMetadata     *searchMetadata
	modelName          string
	originalModel      string
	providerResponseID string
	responseMessages   []*discordgo.Message
	pendingResponses   []pendingResponse
	renderedSpecs      []renderSpec
	iiliRepliedURLs    map[string]struct{}
	progressActive     bool
	responseVisible    bool
	originalMessages   []chatMessage
	tableImages        [][]renderedTableImage
	tableImagesSent    []bool
	// toolSearchResults accumulates the web_search tool results of the
	// current attempt; they are retained in the source message's history so
	// later turns keep the searched context.
	toolSearchResults []webSearchResult
	// progress is the live progress card handed off with the tracker. Its
	// card loop owns the card message until settleProgress.
	progress *requestProgress
	// finalRender is what the latest render applied to each response
	// message when that render was final; any later render clears it (see
	// confirmFinalRender).
	finalRender []renderedEmbedMessage
}

const (
	discordMessageContentMaxLength = 2000
	userFacingErrorMaxRunes        = 1500
)

var iiliResponseURLRegexp = regexp.MustCompile(`(?i)\bhttps?://iili\.io/[^\s<>\]\)]+`)

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

// visibleResponseText returns the stored assistant text. Reasoning never
// belongs in the visible answer: the OpenAI APIs expose reasoning tokens
// only as a separate thinking channel (Responses reasoning summaries /
// reasoning text, or a reasoning_content delta extension on
// OpenAI-compatible Chat Completions backends), surfaced behind the Show
// Thinking button from the dedicated thinking store. Stored history keeps
// the answer only so the model never learns a custom marker format.
func visibleResponseText(_ string, answerText string) string {
	return answerText
}

var (
	errStreamedAnswerVisibilityRegressed = errors.New("streamed answer visibility regressed")
	errEmptyModelResponse                = errors.New("model returned an empty response")
	errNilSession                        = errors.New("session is nil")
)

// assistantHistoryAnswerText returns the text fed to the model for an
// assistant history turn. Stored assistant text is answer-only; thinking
// lives in the dedicated thinking store and is never sent back as
// conversation history.
func assistantHistoryAnswerText(fullText string) string {
	return fullText
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

// settleProgress stops the live progress card and adds the card message to
// the response messages. Rendering the reply or its failure and releasing
// the reply run it first, because the card loop owns the card message until
// then; after the first call it does nothing.
func (tracker *responseTracker) settleProgress() {
	if tracker == nil {
		return
	}

	tracker.progress.settle()
}

func (tracker *responseTracker) release(store *messageNodeStore, fullText string, thinkingText string) {
	tracker.settleProgress()

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
	rawAnswer        string
	thinking         string
	metadata         *searchMetadata
	toolCallResponse *providers.ToolCallResponse
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
		request:             request,
		warnings:            warnings,
		answerAccumulator:   &accumulator,
		thinkingAccumulator: &thinkingAccumulator,
		finishReason:        &finishReason,
		lastRenderTime:      &lastRenderTime,
		rawAnswerText:       "",
		renderedAnswerText:  "",
		toolCallResponse:    nil,
	}

	if prefill.rawAnswer != "" {
		streamState.rawAnswerText = prefill.rawAnswer
		streamState.renderedAnswerText = providers.StreamingBridgeSourceAppendixVisibleText(prefill.rawAnswer)

		_ = accumulator.appendText(streamState.renderedAnswerText)
	}

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

	if streamErr == nil && streamState.toolCallResponse != nil {
		// Tool round: hand off to the web search phase without finalizing
		// the response render. The round's text is raw answer content;
		// strip the bridge source appendix (carried raw, it would sit
		roundAnswerText, parsedMetadata := providers.FinalizeBridgeSourceAppendixAnswer(
			streamState.rawAnswerText,
			tracker.searchMetadata,
		)

		if parsedMetadata != nil {
			tracker.searchMetadata = searchtypes.MergeSearchMetadata(tracker.searchMetadata, parsedMetadata)
		}

		return generatedRoundResult{
			rawAnswer:        roundAnswerText,
			thinking:         thinkingAccumulator.joined(),
			metadata:         nil,
			toolCallResponse: streamState.toolCallResponse,
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

// finalizeGenerationRound finalizes a non-tool streaming round: it strips
// the bridge source appendix, merges its metadata into the tracker,
// renders the final embeds, and reports empty responses as errors.
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
	finalAnswerText := streamState.rawAnswerText

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
		strings.TrimSpace(cleanedAnswerText) == "" && streamState.toolCallResponse == nil {
		responseErr = errEmptyModelResponse
	}

	return generatedRoundResult{
		rawAnswer:        cleanedAnswerText,
		thinking:         thinkingAccumulator.joined(),
		metadata:         parsedSearchMetadata,
		toolCallResponse: nil,
	}, finishReason, responseErr
}

// runGenerationRoundWithRetry runs a generation round (with the stream
// retries of runGenerationRoundWithStreamRetry) and attempts it again while
// the model spends it on tool calls its request never offered (see
// calledUnofferedTools), up to unofferedToolCallMaxAttempts attempts. Each
// attempt re-renders over the tracker's messages in place, so text streamed
// before such calls is replaced. When every attempt calls such tools, the
// round fails with errEmptyModelResponse.
func (instance *bot) runGenerationRoundWithRetry(
	ctx context.Context,
	request chatCompletionRequest,
	tracker *responseTracker,
	warnings []string,
	prefill generatedPrefill,
) (generatedRoundResult, error) {
	for attempt := 1; ; attempt++ {
		round, err := instance.runGenerationRoundWithStreamRetry(ctx, request, tracker, warnings, prefill)
		if !calledUnofferedTools(request, round, err) {
			return round, err
		}

		if attempt >= unofferedToolCallMaxAttempts {
			logWarn(
				"model kept calling tools that were not offered; giving up",
				nil,
				"configured_model",
				request.ConfiguredModel,
				"tool_calls",
				toolCallNames(round.toolCallResponse),
				"max_attempts",
				unofferedToolCallMaxAttempts,
			)

			return round, errEmptyModelResponse
		}

		logWarn(
			"model called tools that were not offered; retrying generation",
			nil,
			"configured_model",
			request.ConfiguredModel,
			"tool_calls",
			toolCallNames(round.toolCallResponse),
			"attempt",
			attempt+1,
			"max_attempts",
			unofferedToolCallMaxAttempts,
		)

		sleepErr := sleepPrematureStreamRetry(ctx, prematureStreamRetryFixedDelay)
		if sleepErr != nil {
			return round, sleepErr
		}
	}
}

// calledUnofferedTools reports whether a round ended in tool calls although
// its request offered no tools. A backend can add tools of its own: 9router
// adds decoy tools to every OpenCode request (the OpenCode free tier rejects
// requests without them), and OpenCode lets the model call them despite
// tool_choice "none". Nothing can execute such calls, so the round produced
// no answer, but another attempt usually does.
func calledUnofferedTools(request chatCompletionRequest, round generatedRoundResult, err error) bool {
	return err == nil && round.toolCallResponse != nil && len(request.Tools) == 0
}

// toolCallNames lists the function names of a tool-call response in call
// order.
func toolCallNames(response *providers.ToolCallResponse) []string {
	if response == nil {
		return nil
	}

	names := make([]string, 0, len(response.Calls))
	for _, call := range response.Calls {
		names = append(names, call.Name)
	}

	return names
}

// runGenerationRoundWithStreamRetry wraps runGenerationRound with a bounded
// same-model retry for streams that end without delivering a finish reason
// or fail transiently mid-stream (for example a Responses stream dropped
// before response.completed/response.done, surfaced as unexpected EOF):
// the provider closed the stream mid-response, the partial text renders as an
// incomplete message, and nothing else surfaces the failure. Each retry runs
// with fresh accumulators and re-renders over the tracker's existing messages
// in place, so a truncated reply is replaced rather than duplicated, and
// auxiliary iili.io url replies are claimed once per response. Up to
// prematureStreamRetryMaxAttempts streams are attempted in total; exhausted
// retries keep the existing behavior of releasing the truncated reply.
func (instance *bot) runGenerationRoundWithStreamRetry(
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

// isInvalidPreviousResponseError reports whether err rejects the chained
// previous_response_id (unknown, expired, or evicted server-side response).
// The match requires the exact parameter token: provider status errors wrap
// the raw response body, and only this narrow signal may trigger a stateless
// retry. A status-carrying error must be a 400/404 rejection; other statuses
// (auth, rate limits, server failures) keep their normal handling even when
// they quote the parameter.
func isInvalidPreviousResponseError(err error) bool {
	if err == nil {
		return false
	}

	normalized := strings.ToLower(err.Error())
	if !strings.Contains(normalized, "previous_response_id") {
		return false
	}

	var statusErr providers.StatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusBadRequest ||
			statusErr.StatusCode == http.StatusNotFound
	}

	return true
}

// Tool calling follows the providers' function calling flow, forced to a
// single tool round per attempt: when the model calls web_search, every
// call of that round is executed (all their queries run as one search
// batch), the round's tool-call response and one output per call are
// appended as a tool round, and the only follow-up runs with tool_choice
// "none", so the model must answer from those results instead of searching
// again. The provider replays the round after the conversation in its
// native wire format (assistant tool_calls plus tool messages on Chat
// Completions; output items plus function_call_output items, or
// previous_response_id chaining, on the Responses API). The conversation
// messages and tool definitions never change, so the follow-up extends a
// byte-identical prefix and the provider's prompt cache keeps matching.
// A backend that does not enforce tool_choice "none" gets the follow-up
// without tools instead (see runForcedFinalAnswerRound).
func (instance *bot) generateResponseWithWebSearchTool(
	ctx context.Context,
	loadedConfig config,
	request chatCompletionRequest,
	tracker *responseTracker,
	warnings []string,
) (string, string, error) {
	if tracker != nil {
		// Each attempt (primary, stateless retry, fallback) runs its own
		// tool round from scratch.
		tracker.toolSearchResults = nil
	}

	round, roundErr := instance.runGenerationRoundWithRetry(
		ctx,
		request,
		tracker,
		warnings,
		generatedPrefill{rawAnswer: "", thinking: ""},
	)
	if roundErr != nil {
		return round.rawAnswer, round.thinking, roundErr
	}

	if round.toolCallResponse == nil {
		return round.rawAnswer, round.thinking, nil
	}

	outputs, warnings, _ := instance.runWebSearchToolPhase(
		ctx,
		loadedConfig,
		request.ConfiguredModel,
		tracker,
		request.Messages,
		warnings,
		round.toolCallResponse.Calls,
	)

	request.ToolRounds = append(slices.Clone(request.ToolRounds), providers.ToolRound{
		Response: round.toolCallResponse,
		Outputs:  outputs,
	})

	return instance.runForcedFinalAnswerRound(ctx, request, tracker, warnings, round)
}

// runForcedFinalAnswerRound runs the follow-up to the attempt's single tool
// round with tool_choice "none": the model must answer from the search
// results instead of calling tools again. The tool definitions stay in the
// request, as the function calling guide recommends, so the prompt prefix
// is unchanged. Text and thinking streamed before the tool calls carry over
// into the final render.
//
// tool_choice "none" only imitates passing no functions, and not every
// OpenAI-compatible backend enforces it (a proxy may rewrite it to "auto"
// for an upstream that accepts nothing else). Tool calls made anyway are
// never executed: the answer is requested again without tools, and the
// model is remembered so its later final answers skip the ignored
// tool_choice. OpenCode models (see isOpenCodeModel) skip it from their
// first reply.
func (instance *bot) runForcedFinalAnswerRound(
	ctx context.Context,
	request chatCompletionRequest,
	tracker *responseTracker,
	warnings []string,
	toolRound generatedRoundResult,
) (string, string, error) {
	finalPrefill := generatedPrefill{
		rawAnswer: toolRound.rawAnswer,
		thinking:  toolRound.thinking,
	}

	if instance.ignoresToolChoiceNone(request.ConfiguredModel) {
		return instance.runToolFreeFinalAnswerRound(ctx, request, tracker, warnings, finalPrefill)
	}

	request.ToolChoice = providers.ToolChoiceNone

	final, finalErr := instance.runGenerationRoundWithRetry(
		ctx,
		request,
		tracker,
		warnings,
		finalPrefill,
	)
	if finalErr != nil {
		return final.rawAnswer, final.thinking, finalErr
	}

	if final.toolCallResponse == nil {
		return final.rawAnswer, final.thinking, nil
	}

	logWarn(
		"model emitted tool calls although tool_choice was none; answering without tools",
		nil,
		"configured_model",
		request.ConfiguredModel,
		"tool_calls",
		len(final.toolCallResponse.Calls),
	)

	instance.rememberToolChoiceNoneIgnored(request.ConfiguredModel)

	return instance.runToolFreeFinalAnswerRound(ctx, request, tracker, warnings, finalPrefill)
}

// runToolFreeFinalAnswerRound requests the final answer with no tools
// offered, the behavior tool_choice "none" imitates. The tool rounds travel
// as text instead (see toolFreeFinalAnswerRequest). A backend that adds
// tools of its own can still get calls to them back; the round is then
// attempted again (see calledUnofferedTools).
func (instance *bot) runToolFreeFinalAnswerRound(
	ctx context.Context,
	request chatCompletionRequest,
	tracker *responseTracker,
	warnings []string,
	prefill generatedPrefill,
) (string, string, error) {
	toolFreeRequest, err := toolFreeFinalAnswerRequest(request)
	if err != nil {
		return prefill.rawAnswer, prefill.thinking, err
	}

	final, finalErr := instance.runGenerationRoundWithRetry(
		ctx,
		toolFreeRequest,
		tracker,
		warnings,
		prefill,
	)

	return final.rawAnswer, final.thinking, finalErr
}

// toolFreeFinalAnswerRequest drops the tool definitions, tool_choice, and
// tool rounds from a follow-up request and appends the rounds' outputs to
// the latest user message as web search results, the form the reply-chain
// history keeps them in. A tool-call history replayed without tool
// definitions is not portable across OpenAI-compatible backends, so the
// rounds are not replayed natively. previous_response_id chaining is kept:
// the chained input is the new tail, which now carries the results.
func toolFreeFinalAnswerRequest(request chatCompletionRequest) (chatCompletionRequest, error) {
	messages, err := appendWebSearchResultsToConversation(
		request.Messages,
		toolRoundOutputsText(request.ToolRounds),
	)
	if err != nil {
		return chatCompletionRequest{}, fmt.Errorf("append tool round outputs to conversation: %w", err)
	}

	request.Messages = messages
	request.Tools = nil
	request.ToolChoice = ""
	request.ToolRounds = nil

	return request, nil
}

// toolRoundOutputsText joins the non-empty outputs of the tool rounds in
// round and call order, the text the model received for its calls.
func toolRoundOutputsText(rounds []providers.ToolRound) string {
	var outputs []string

	for _, round := range rounds {
		for _, output := range round.Outputs {
			if text := strings.TrimSpace(output.Output); text != "" {
				outputs = append(outputs, text)
			}
		}
	}

	return strings.Join(outputs, "\n\n")
}

// ignoresToolChoiceNone reports whether the configured model's backend does
// not enforce tool_choice "none": OpenCode models always, other models once
// their backend has streamed tool calls although tool_choice was "none".
func (instance *bot) ignoresToolChoiceNone(configuredModel string) bool {
	if isOpenCodeModel(configuredModel) {
		return true
	}

	instance.toolChoiceMu.Lock()
	defer instance.toolChoiceMu.Unlock()

	_, ignored := instance.toolChoiceNoneIgnored[strings.TrimSpace(configuredModel)]

	return ignored
}

// rememberToolChoiceNoneIgnored records that the configured model's backend
// does not enforce tool_choice "none", so its later final answers are
// requested without tools right away instead of first spending a round on
// the ignored tool_choice.
func (instance *bot) rememberToolChoiceNoneIgnored(configuredModel string) {
	instance.toolChoiceMu.Lock()
	defer instance.toolChoiceMu.Unlock()

	if instance.toolChoiceNoneIgnored == nil {
		instance.toolChoiceNoneIgnored = make(map[string]struct{})
	}

	instance.toolChoiceNoneIgnored[strings.TrimSpace(configuredModel)] = struct{}{}
}

// isOpenCodeModel reports whether a configured model is served by OpenCode:
// one of its slash-separated name segments is "oc", as in
// "xiaomi/oc/mimo-v2.6-flash-free:vision". OpenCode answers tool_choice
// "none" with another tool call, so these models get their final answer
// without tools from their first reply instead of spending a round on the
// ignored tool_choice. Names that merely contain the letters, such as
// "local" or "ocr", do not match.
func isOpenCodeModel(configuredModel string) bool {
	providerName, modelName, err := splitConfiguredModel(strings.TrimSpace(configuredModel))
	if err != nil {
		return false
	}

	for segment := range strings.SplitSeq(providerName+"/"+modelName, "/") {
		if strings.EqualFold(strings.TrimSpace(segment), openCodeModelSegment) {
			return true
		}
	}

	return false
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
	// A chained follow-up depends on the parent response still being
	// stored server-side (retention, ZDR, or eviction can invalidate it).
	// On a previous_response_id rejection, retry once statelessly with the
	// full history already in request.Messages before failing over.
	if responseErr != nil && strings.TrimSpace(request.PreviousResponseID) != "" &&
		isInvalidPreviousResponseError(responseErr) {
		logWarn("retry chained follow-up statelessly", responseErr)

		request.PreviousResponseID = ""
		request.PreviousResponseCount = 0
		cleanedText, thinkingText, responseErr = instance.generateResponseWithWebSearchTool(
			ctx,
			loadedConfig,
			request,
			tracker,
			warnings,
		)
	}

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
	tracker.tableImages = nil
	tracker.tableImagesSent = nil

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
	request             chatCompletionRequest
	warnings            []string
	answerAccumulator   *segmentAccumulator
	thinkingAccumulator *segmentAccumulator
	finishReason        *string
	lastRenderTime      *time.Time
	rawAnswerText       string
	renderedAnswerText  string
	toolCallResponse    *providers.ToolCallResponse
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

	if delta.ToolCallResponse != nil {
		state.toolCallResponse = delta.ToolCallResponse
	}

	if delta.FinishReason != "" {
		*state.finishReason = delta.FinishReason
	}

	hasThinking := strings.TrimSpace(state.thinkingAccumulator.joined()) != ""

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
		hasThinking,
	)
	if err != nil {
		return fmt.Errorf("render streaming response: %w", err)
	}

	*state.lastRenderTime = time.Now()

	return nil
}

func (state *generatedStreamState) appendAnswerText(answerDelta string) (bool, error) {
	state.rawAnswerText += answerDelta

	visibleAnswerText := providers.StreamingBridgeSourceAppendixVisibleText(state.rawAnswerText)
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
) error {
	segments := accumulator.renderSegments()

	if len(tracker.tableImages) != len(segments) || len(tracker.tableImagesSent) != len(segments) {
		tracker.tableImages = make([][]renderedTableImage, len(segments))
		tracker.tableImagesSent = make([]bool, len(segments))

		for index, segment := range segments {
			tracker.tableImages[index] = renderMarkdownTableImages(segment)
		}
	}

	err := instance.renderEmbedResponse(
		ctx,
		tracker,
		warnings,
		segments,
		finishReason,
		true,
		strings.TrimSpace(thinkingText) != "",
	)
	if err != nil {
		return fmt.Errorf("render final embed response: %w", err)
	}

	if isGoodFinishReason(finishReason) {
		instance.sendTableImageReplies(tracker)
	}

	instance.sendIiliURLReplies(tracker, accumulator.joined())

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

	tracker.settleProgress()

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
		responseActions{showSources: false, showThinking: false, showGist: false, showExport: true},
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
				showExport:   final && index == len(segments)-1,
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

	tracker.settleProgress()

	// Any render replaces what the messages show, so an earlier final
	// render is no longer theirs to confirm.
	tracker.finalRender = nil

	desiredSpecs := buildRenderSpecs(
		segments,
		finishReason,
		final,
		tracker.searchMetadata != nil,
		hasThinking,
	)

	var finalRender []renderedEmbedMessage

	for index, spec := range desiredSpecs {
		changed := index >= len(tracker.renderedSpecs) || tracker.renderedSpecs[index] != spec
		if !changed && !final {
			continue
		}

		embed := buildResponseEmbed(
			spec.content,
			tracker.modelName,
			spec.color,
			warnings,
			spec.footerText,
		)

		if changed {
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

		if final {
			finalRender = append(finalRender, renderedEmbedMessage{
				message:    tracker.responseMessages[index],
				embed:      embed,
				components: buildEmbedComponents(spec.actions),
			})
		}
	}

	err := instance.trimExtraEmbedResponses(ctx, tracker, len(desiredSpecs))
	if err != nil {
		return err
	}

	tracker.finalRender = finalRender
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

	if len(tracker.tableImages) > keepCount {
		tracker.tableImages = tracker.tableImages[:keepCount]
	}

	if len(tracker.tableImagesSent) > keepCount {
		tracker.tableImagesSent = tracker.tableImagesSent[:keepCount]
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

func iiliResponseURLs(text string) []string {
	rawURLs := iiliResponseURLRegexp.FindAllString(text, -1)
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

// unrepliedIiliURLs filters urls down to those this response has not
// replied with yet and claims them on the tracker, so a retried generation
// attempt never re-posts duplicate iili.io url replies.
func unrepliedIiliURLs(tracker *responseTracker, urls []string) []string {
	if len(urls) == 0 {
		return urls
	}

	if tracker.iiliRepliedURLs == nil {
		tracker.iiliRepliedURLs = make(map[string]struct{}, len(urls))
	}

	unreplied := make([]string, 0, len(urls))

	for _, url := range urls {
		if _, replied := tracker.iiliRepliedURLs[url]; replied {
			continue
		}

		tracker.iiliRepliedURLs[url] = struct{}{}

		unreplied = append(unreplied, url)
	}

	return unreplied
}

func (instance *bot) sendIiliURLReplies(tracker *responseTracker, answerText string) {
	if instance == nil || instance.session == nil || tracker == nil || len(tracker.responseMessages) == 0 {
		return
	}

	responseMessage := tracker.responseMessages[len(tracker.responseMessages)-1]

	replyURLs := unrepliedIiliURLs(tracker, iiliResponseURLs(answerText))
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
				"send iili.io url reply",
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

// sendTableImageReplies posts rendered markdown-table PNGs as follow-up
// replies after the tracker's final embed. Each image reply is cached as an
// empty auxiliary assistant node, so the images never enter model history:
// buildMessageContent returns nil for empty nodes and buildConversation
// omits them from the assembled messages. Each segment is flagged sent only
// after all of its images post, so a partial failure retries just the
// segments that never completed instead of resending or dropping images.
func (instance *bot) sendTableImageReplies(tracker *responseTracker) {
	if instance == nil || instance.session == nil || tracker == nil {
		return
	}

	if len(tracker.responseMessages) == 0 {
		return
	}

	parentMessage := tracker.responseMessages[len(tracker.responseMessages)-1]
	embedParent := parentMessage

	for segmentIndex, images := range tracker.tableImages {
		if segmentIndex < len(tracker.tableImagesSent) && tracker.tableImagesSent[segmentIndex] {
			continue
		}

		segmentComplete := true

		for imageIndex, tableImage := range images {
			if len(tableImage.data) == 0 {
				continue
			}

			send := newReplyMessage(parentMessage)
			send.Files = []*discordgo.File{{
				Name:        tableImage.filename,
				ContentType: "image/png",
				Reader:      bytes.NewReader(tableImage.data),
			}}

			sentMessage, err := instance.session.ChannelMessageSendComplex(parentMessage.ChannelID, send)
			if err != nil {
				logWarn("send table image reply", err, "table_index", imageIndex)

				segmentComplete = false

				break
			}

			parentMessage = sentMessage
			instance.cacheAuxiliaryAssistantReply(sentMessage, embedParent, tracker)
		}

		if segmentComplete && segmentIndex < len(tracker.tableImagesSent) {
			tracker.tableImagesSent[segmentIndex] = true
		}
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
	const maxResponseButtons = 5

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

	if actions.showExport {
		button := new(discordgo.Button)
		button.CustomID = exportSessionButtonCustomID
		button.Label = exportSessionButtonLabel
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
