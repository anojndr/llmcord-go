package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	searchtypes "llmcord-go/internal/searchtypes"
)

// claudeStreamAccumulator folds Messages SSE events into provider-neutral
// deltas: thinking text, answer text, tool calls, citations-as-sources,
// message ID, and the terminal stop reason.
type claudeStreamAccumulator struct {
	handle func(StreamDelta) error

	messageID string
	finished  bool

	thinkingIDs []string
	thinkingSet map[string]struct{}
	thinkingBuf map[string]*strings.Builder

	textIDs []string
	textSet map[string]struct{}
	textBuf map[string]*strings.Builder

	toolCalls   map[string]*claudeStreamToolCall
	toolOrder   []string
	toolJSONBuf map[string]*strings.Builder

	citations []searchtypes.SearchSource

	stopReason string
	sawContent bool
	sawStop    bool
}

type claudeStreamToolCall struct {
	id    string
	name  string
	index int64
}

// observe processes one stream event; finished reports message_stop.
func (acc *claudeStreamAccumulator) observe(event anthropic.MessageStreamEventUnion) (bool, error) {
	switch event.Type {
	case "message_start":
		if id := strings.TrimSpace(event.Message.ID); id != "" {
			acc.messageID = id
		}

		return false, acc.emit(StreamDelta{ProviderResponseID: acc.messageID})
	case "content_block_start":
		return false, acc.blockStart(event)
	case "content_block_delta":
		return false, acc.blockDelta(event)
	case "content_block_stop":
		return false, acc.blockStop(event)
	case "message_delta":
		return false, acc.messageDelta(event)
	case "message_stop":
		return true, acc.messageStop()
	default:
		return false, nil
	}
}

func (acc *claudeStreamAccumulator) blockStart(event anthropic.MessageStreamEventUnion) error {
	blockType := strings.TrimSpace(event.ContentBlock.Type)
	index := event.Index

	switch blockType {
	case "thinking":
		signature := strings.TrimSpace(event.ContentBlock.Signature)
		key := claudeBlockKey("thinking", index, signature)
		acc.trackThinking(key)

		if signature != "" {
			acc.thinkingSet[claudeBlockKey("thinking", index, "")] = struct{}{}
		}
	case "redacted_thinking":
		key := claudeBlockKey("redacted", index, "")
		acc.trackThinking(key)
	case "text":
		key := claudeBlockKey("text", index, "")
		acc.trackText(key)
	case "tool_use":
		id := strings.TrimSpace(event.ContentBlock.ID)
		name := strings.TrimSpace(event.ContentBlock.Name)

		if id == "" {
			id = fmt.Sprintf("call_%d", index)
		}

		if acc.toolCalls == nil {
			acc.toolCalls = make(map[string]*claudeStreamToolCall)
			acc.toolJSONBuf = make(map[string]*strings.Builder)
		}

		if _, exists := acc.toolCalls[id]; !exists {
			acc.toolCalls[id] = &claudeStreamToolCall{id: id, name: name, index: index}
			acc.toolOrder = append(acc.toolOrder, id)
			acc.toolJSONBuf[id] = new(strings.Builder)
		} else if name != "" {
			acc.toolCalls[id].name = name
		}
	case "web_search_tool_result", "web_fetch_tool_result":
		// Server-side search results arrive as result blocks; citations
		// inside text blocks carry the sources.
		return nil
	default:
		return nil
	}

	return nil
}

func claudeBlockKey(kind string, index int64, signature string) string {
	if signature != "" {
		return kind + ":" + signature
	}

	return fmt.Sprintf("%s:%d", kind, index)
}

func (acc *claudeStreamAccumulator) trackThinking(key string) {
	if acc.thinkingSet == nil {
		acc.thinkingSet = make(map[string]struct{})
		acc.thinkingBuf = make(map[string]*strings.Builder)
	}

	if _, exists := acc.thinkingSet[key]; !exists {
		acc.thinkingSet[key] = struct{}{}
		acc.thinkingIDs = append(acc.thinkingIDs, key)
		acc.thinkingBuf[key] = new(strings.Builder)
	}
}

func (acc *claudeStreamAccumulator) trackText(key string) {
	if acc.textSet == nil {
		acc.textSet = make(map[string]struct{})
		acc.textBuf = make(map[string]*strings.Builder)
	}

	if _, exists := acc.textSet[key]; !exists {
		acc.textSet[key] = struct{}{}
		acc.textIDs = append(acc.textIDs, key)
		acc.textBuf[key] = new(strings.Builder)
	}
}

func (acc *claudeStreamAccumulator) blockDelta(event anthropic.MessageStreamEventUnion) error {
	deltaType := strings.TrimSpace(event.Delta.Type)

	switch deltaType {
	case "text_delta":
		text := event.Delta.Text
		if text == "" {
			return nil
		}

		key := acc.latestTextKey(event.Index)
		acc.trackText(key)
		acc.textBuf[key].WriteString(text)

		return acc.emit(StreamDelta{Content: text})
	case "thinking_delta":
		thinking := event.Delta.Thinking
		if thinking == "" {
			return nil
		}

		key := acc.latestThinkingKey(event.Index)
		acc.trackThinking(key)
		acc.thinkingBuf[key].WriteString(thinking)

		return acc.emit(StreamDelta{Thinking: thinking})
	case "signature_delta":
		return nil
	case "input_json_delta":
		fragment := event.Delta.PartialJSON
		if fragment == "" {
			return nil
		}

		call := acc.latestToolCall(event.Index)
		if call == nil {
			return nil
		}

		acc.toolJSONBuf[call.id].WriteString(fragment)

		return nil
	case "citations_delta":
		source := claudeCitationSource(event)
		if source == nil {
			return nil
		}

		acc.citations = append(acc.citations, *source)

		return nil
	default:
		return nil
	}
}

func (acc *claudeStreamAccumulator) latestTextKey(index int64) string {
	if len(acc.textIDs) > 0 {
		return acc.textIDs[len(acc.textIDs)-1]
	}

	return claudeBlockKey("text", index, "")
}

func (acc *claudeStreamAccumulator) latestThinkingKey(index int64) string {
	if len(acc.thinkingIDs) > 0 {
		return acc.thinkingIDs[len(acc.thinkingIDs)-1]
	}

	return claudeBlockKey("thinking", index, "")
}

func (acc *claudeStreamAccumulator) latestToolCall(index int64) *claudeStreamToolCall {
	if acc.toolCalls == nil {
		acc.toolCalls = make(map[string]*claudeStreamToolCall)
		acc.toolJSONBuf = make(map[string]*strings.Builder)
	}

	if len(acc.toolOrder) > 0 {
		return acc.toolCalls[acc.toolOrder[len(acc.toolOrder)-1]]
	}

	id := fmt.Sprintf("call_%d", index)
	call := &claudeStreamToolCall{id: id, index: index}
	acc.toolCalls[id] = call
	acc.toolOrder = append(acc.toolOrder, id)
	acc.toolJSONBuf[id] = new(strings.Builder)

	return call
}

func (acc *claudeStreamAccumulator) blockStop(event anthropic.MessageStreamEventUnion) error {
	_ = event

	return nil
}

func (acc *claudeStreamAccumulator) messageDelta(event anthropic.MessageStreamEventUnion) error {
	if reason := strings.TrimSpace(string(event.Delta.StopReason)); reason != "" {
		acc.stopReason = claudeNormalizeStopReason(reason)
	}

	return nil
}

func (acc *claudeStreamAccumulator) messageStop() error {
	acc.finished = true

	delta := StreamDelta{
		FinishReason:       acc.finalStopReason(),
		ProviderResponseID: acc.messageID,
	}

	if len(acc.citations) > 0 {
		delta.SearchMetadata = claudeSearchMetadata(acc.citations)
	}

	if response := acc.toolCallResponse(); response != nil {
		delta.ToolCallResponse = response
	}

	acc.sawStop = true
	if delta.FinishReason != "" || delta.ToolCallResponse != nil || delta.SearchMetadata != nil || delta.ProviderResponseID != "" {
		acc.sawContent = acc.sawContent || deltaReferencesContent(delta)
	}

	return acc.emit(delta)
}

// finish completes a stream that closed without message_stop: if a stop
// reason was seen the stream is complete, otherwise it ended prematurely.
func (acc *claudeStreamAccumulator) finish() (bool, error) {
	if acc.finished {
		return true, nil
	}

	if acc.stopReason != "" {
		return true, acc.messageStop()
	}

	if acc.sawContent {
		return true, nil
	}

	return false, nil
}

func (acc *claudeStreamAccumulator) finalStopReason() string {
	switch acc.stopReason {
	case "":
		if acc.hasToolCalls() {
			return string(anthropic.StopReasonToolUse)
		}

		return finishReasonStop
	default:
		return acc.stopReason
	}
}

func (acc *claudeStreamAccumulator) hasToolCalls() bool {
	return len(acc.toolOrder) > 0
}

// toolCallResponse builds the provider-neutral tool-call response from
// accumulated tool_use blocks, preserving raw JSON arguments.
func (acc *claudeStreamAccumulator) toolCallResponse() *ToolCallResponse {
	if len(acc.toolOrder) == 0 {
		return nil
	}

	calls := make([]FunctionToolCall, 0, len(acc.toolOrder))

	for _, id := range acc.toolOrder {
		call := acc.toolCalls[id]
		arguments := ""

		if buf := acc.toolJSONBuf[id]; buf != nil {
			arguments = functionCallArguments(buf.String())
		}

		calls = append(calls, FunctionToolCall{
			ID:        call.id,
			Name:      call.name,
			Arguments: arguments,
		})
	}

	return NewClaudeToolCallResponse(calls, acc.joinedThinking(), acc.joinedText())
}

func (acc *claudeStreamAccumulator) joinedThinking() string {
	var builder strings.Builder

	for _, key := range acc.thinkingIDs {
		builder.WriteString(acc.thinkingBuf[key].String())
	}

	return builder.String()
}

func (acc *claudeStreamAccumulator) joinedText() string {
	var builder strings.Builder

	for _, key := range acc.textIDs {
		builder.WriteString(acc.textBuf[key].String())
	}

	return builder.String()
}

func (acc *claudeStreamAccumulator) emit(delta StreamDelta) error {
	if delta.Content == "" && delta.Thinking == "" && delta.FinishReason == "" &&
		delta.ProviderResponseID == "" && delta.SearchMetadata == nil && delta.ToolCallResponse == nil {
		return nil
	}

	if delta.Content != "" || delta.Thinking != "" {
		acc.sawContent = true
	}

	if acc.handle == nil {
		return nil
	}

	if err := acc.handle(delta); err != nil {
		return fmt.Errorf(handleStreamDeltaErrorFormat, err)
	}

	return nil
}

// claudeNormalizeStopReason maps Messages stop reasons onto the neutral
// finish reasons the app renders: end_turn renders as stop.
func claudeNormalizeStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "end_turn", "stop_sequence", "stop":
		return finishReasonStop
	case "max_tokens", "length":
		return finishReasonLength
	case "tool_use":
		return string(anthropic.StopReasonToolUse)
	case "refusal":
		return string(anthropic.StopReasonRefusal)
	case "pause_turn":
		return string(anthropic.StopReasonPauseTurn)
	case "model_context_window_exceeded":
		return string(anthropic.StopReasonModelContextWindowExceeded)
	default:
		return strings.ToLower(strings.TrimSpace(reason))
	}
}

// claudeFinishReasonError surfaces terminal refusals and safety stops as
// errors so they render instead of passing as clean empty replies.
func claudeFinishReasonError(stopReason string) error {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "refusal", "model_context_window_exceeded":
		return fmt.Errorf("provider ended the response (stop_reason=%s): %w", stopReason, os.ErrInvalid)
	default:
		return nil
	}
}

// claudeCitationSource extracts a source from a citations_delta event.
func claudeCitationSource(event anthropic.MessageStreamEventUnion) *searchtypes.SearchSource {
	raw := event.Delta.Citation.RawJSON()
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	var citation struct {
		Type   string `json:"type"`
		URL    string `json:"url"`
		Title  string `json:"title"`
		Source string `json:"source"`
		Text   string `json:"cited_text"`
	}

	if err := json.Unmarshal([]byte(raw), &citation); err != nil {
		return nil
	}

	url := strings.TrimSpace(citation.URL)
	if url == "" {
		url = strings.TrimSpace(citation.Source)
	}

	if url == "" {
		return nil
	}

	title := strings.TrimSpace(citation.Title)
	if title == "" {
		title = url
	}

	return &searchtypes.SearchSource{Title: title, URL: url}
}

// claudeSearchMetadata folds streamed citations into search metadata so
// Show Sources works for Claude's citation blocks.
func claudeSearchMetadata(sources []searchtypes.SearchSource) *searchtypes.SearchMetadata {
	if len(sources) == 0 {
		return nil
	}

	var builder strings.Builder

	for _, source := range sources {
		if strings.TrimSpace(source.Title) != "" {
			builder.WriteString("Title: " + strings.TrimSpace(source.Title) + "\n")
		}

		builder.WriteString("URL: " + strings.TrimSpace(source.URL) + "\n\n")
	}

	results := []searchtypes.WebSearchResult{{Query: "", Text: strings.TrimSpace(builder.String())}}

	return searchtypes.NewSearchMetadata(nil, results, defaultWebSearchMaxURLs)
}

// claudeConsumeStream pumps SDK stream events into provider-neutral deltas.
func claudeConsumeStream(
	stream interface {
		Next() bool
		Current() anthropic.MessageStreamEventUnion
		Err() error
		Close() error
	},
	handle func(StreamDelta) error,
) (bool, error) {
	accumulator := claudeStreamAccumulator{
		handle: handle,
	}

	if stream == nil {
		return false, fmt.Errorf("claude stream is nil: %w", os.ErrInvalid)
	}

	for stream.Next() {
		event := stream.Current()

		finished, err := accumulator.observe(event)
		if err != nil {
			_ = stream.Close()

			return finished, err
		}

		if finished {
			_ = stream.Close()

			return true, claudeFinishReasonError(accumulator.stopReason)
		}
	}

	if err := stream.Err(); err != nil {
		return false, claudeStreamError(err)
	}

	finished, err := accumulator.finish()
	if err != nil {
		return false, err
	}

	if !finished {
		return false, nil
	}

	return true, claudeFinishReasonError(accumulator.stopReason)
}

// claudeStreamError maps SDK stream failures onto provider errors with
// status codes so the router retry policy classifies them.
func claudeStreamError(err error) error {
	if err == nil {
		return nil
	}

	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		status := apiErr.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}

		return StatusError{
			StatusCode: status,
			Message: fmt.Sprintf(
				"claude request failed with status %d: %s",
				status,
				strings.TrimSpace(apiErr.Error()),
			),
			Err: os.ErrInvalid,
		}
	}

	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return fmt.Errorf("claude stream ended before message_stop: %w", io.ErrUnexpectedEOF)
	}

	return err
}

// IsClaudeTransientError reports whether a Claude API error is a transient
// failure (429 rate limit, 500/502/503/529 overload delivered as a Claude
// StatusError or SDK *anthropic.Error). It only inspects errors that carry
// the "claude request failed" prefix so OpenAI SSE payloads (e.g. a
// status-less "rate limited" chat-completions error) never match here;
// those keep their existing router classification.
func IsClaudeTransientError(err error) bool {
	if err == nil {
		return false
	}

	if !strings.Contains(strings.ToLower(err.Error()), "claude request failed") {
		return false
	}

	var statusErr StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
			529:
			return true
		default:
			return false
		}
	}

	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
			529:
			return true
		default:
			return false
		}
	}

	return false
}

// NewClaudeToolCallResponse builds a tool-call response captured from a
// Claude turn. Exported for tests in other packages.
func NewClaudeToolCallResponse(calls []FunctionToolCall, thinking, text string) *ToolCallResponse {
	if len(calls) == 0 {
		return nil
	}

	cloned := make([]FunctionToolCall, len(calls))
	copy(cloned, calls)

	return &ToolCallResponse{
		Calls:            cloned,
		text:             text,
		reasoningContent: thinking,
		extraContent:     nil,
		responseID:       "",
		outputItems:      nil,
	}
}
