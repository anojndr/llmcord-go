// Responses API client machinery shared by the built-in OpenAI provider.
package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"reflect"
	"strings"

	searchtypes "llmcord-go/internal/searchtypes"
	support "llmcord-go/internal/support"
)

const (
	responsesRequestBodyBaseFields                = 4
	responsesStreamEventCompleted                 = "response.completed"
	responsesStreamEventError                     = "error"
	responsesStreamEventFailed                    = "response.failed"
	responsesStreamEventIncomplete                = "response.incomplete"
	responsesStreamEventOutputDone                = "response.output_item.done"
	responsesStreamEventOutputDelta               = "response.output_text.delta"
	responsesStreamEventReasoningTextDone         = "response.reasoning_text.done"
	responsesStreamEventReasoningTextDelta        = "response.reasoning_text.delta"
	responsesStreamEventReasoningSummaryPartDone  = "response.reasoning_summary_part.done"
	responsesStreamEventReasoningSummaryTextDelta = "response.reasoning_summary_text.delta"
	responsesImageDetailAuto                      = "auto"
	responsesInputFileType                        = "input_file"
	responsesInputImageType                       = "input_image"
	responsesInputTextType                        = "input_text"
	responsesOutputTypeImage                      = "image_generation_call"
	responsesOutputTypeReasoning                  = "reasoning"
	responsesReasoningSummaryTextType             = "summary_text"
	responsesOutputTypeFunctionCall               = "function_call"
	responsesStatusCompleted                      = "completed"
)

type responsesError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

type responsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

type responsesOutputItem struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Status        string          `json:"status"`
	CallID        string          `json:"call_id"`
	Name          string          `json:"name"`
	Arguments     string          `json:"arguments"`
	Result        string          `json:"result"`
	ResultURL     string          `json:"result_url"`
	MIMEType      string          `json:"mime_type"`
	Action        string          `json:"action"`
	Prompt        string          `json:"prompt"`
	RevisedPrompt string          `json:"revised_prompt"`
	Summary       json.RawMessage `json:"summary"`
}

type responsesStreamResponse struct {
	ID                string                      `json:"id"`
	Status            string                      `json:"status"`
	Output            []responsesOutputItem       `json:"output"`
	Error             *responsesError             `json:"error"`
	IncompleteDetails *responsesIncompleteDetails `json:"incomplete_details"`
}

type responsesStreamEvent struct {
	Type     string                   `json:"type"`
	Delta    string                   `json:"delta"`
	Message  string                   `json:"message"`
	Code     any                      `json:"code"`
	Item     *responsesOutputItem     `json:"item"`
	Error    *responsesError          `json:"error"`
	Response *responsesStreamResponse `json:"response"`
}

type responsesStreamState struct {
	seenOutputItemIDs    map[string]struct{}
	seenOutputItemURLs   map[string]struct{}
	seenReasoningItemIDs map[string]struct{}
	seenToolCallIDs      map[string]struct{}
	toolCalls            []FunctionToolCall
	hasVisibleContent    bool
}

// ProviderUsesResponsesAPI reports whether a provider uses the Responses API.
func ProviderUsesResponsesAPI(providerName string, provider ProviderRequestConfig) bool {
	apiKind := provider.APIKind
	if apiKind != ProviderAPIKindOpenAI {
		return false
	}

	normalizedAPI := strings.ToLower(strings.TrimSpace(provider.API))
	switch normalizedAPI {
	case OpenAIAPIResponses:
		return true
	case OpenAIAPIChatCompletions:
		return false
	case "":
		return usesBuiltInOpenAIProvider(providerName, apiKind)
	default:
		return false
	}
}

func (client openAIClient) streamResponses(
	ctx context.Context,
	request ChatCompletionRequest,
	handle func(StreamDelta) error,
) error {
	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		return fmt.Errorf("build responses request body: %w", err)
	}

	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("marshal responses request: %w", err)
	}

	requestURL, err := buildResponsesURL(request.Provider.BaseURL, request.Provider.ExtraQuery)
	if err != nil {
		return fmt.Errorf("build responses url: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		requestURL,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return fmt.Errorf("create responses request: %w", err)
	}

	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("Authorization", "Bearer "+openAIAPIKey(requestKey(request.Provider)))
	httpRequest.Header.Set("Content-Type", "application/json")
	setOpenAIClientRequestIDHeader(httpRequest, request)

	for key, value := range request.Provider.ExtraHeaders {
		httpRequest.Header.Set(key, stringifyValue(value))
	}

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("send responses request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return fmt.Errorf(
				"read responses error response after status %d: %w",
				httpResponse.StatusCode,
				readErr,
			)
		}

		return NewOpenAIProviderStatusError(
			"responses request failed",
			httpResponse.StatusCode,
			httpResponse.Status,
			httpResponse.Header.Clone(),
			responseBody,
			false,
		)
	}

	terminalEventSeen, err := client.consumeResponsesStream(httpResponse.Body, handle)
	if err != nil {
		return fmt.Errorf("consume responses stream: %w", err)
	}

	if !terminalEventSeen {
		return fmt.Errorf("responses stream ended before response.completed: %w", io.ErrUnexpectedEOF)
	}

	return nil
}

func (client openAIClient) consumeResponsesStream(
	stream io.Reader,
	handle func(StreamDelta) error,
) (bool, error) {
	terminalEventSeen := false
	streamState := newResponsesStreamState()

	_, err := consumeServerSentEvents(stream, func(payload []byte) error {
		terminal, payloadErr := handleResponsesStreamPayload(payload, handle, streamState)
		if terminal {
			terminalEventSeen = true
		}

		return payloadErr
	})
	if err != nil {
		return false, err
	}

	return terminalEventSeen, nil
}

func buildResponsesRequestBody(request ChatCompletionRequest) (map[string]any, error) {
	messages := RequestMessagesWithFileOrImageOnlyQueryPlaceholder(request.Messages)
	messages = openAIReplaceSystemRoleWithDeveloper(messages, request.Model)

	if openAIRequestPromptCacheKeyPrefix(request) != "" &&
		!openAICacheOptionsModeIsExplicit(request.Provider.ExtraBody) {
		messages = openAIResponsesCacheBreakpointMessages(messages)
	}

	input, err := responsesInput(messages)
	if err != nil {
		return nil, err
	}

	requestBody := make(map[string]any, len(request.Provider.ExtraBody)+responsesRequestBodyBaseFields)
	requestBody["model"] = request.Model
	requestBody["stream"] = true
	requestBody["input"] = input
	addOpenAIPromptCacheKey(requestBody, request)
	addResponsesTools(requestBody, request)

	extraBody := request.Provider.ExtraBody
	if request.Provider.APIKind == ProviderAPIKindOpenAI &&
		OpenAIConfiguredModel(request.ConfiguredModel) {
		extraBody = NormalizeOpenAIResponsesExtraBody(request.Model, extraBody)
	}

	maps.Copy(requestBody, extraBody)

	if request.Provider.APIKind == ProviderAPIKindOpenAI && OpenAIConfiguredModel(request.ConfiguredModel) {
		addOpenAICacheOptions(requestBody, request)
	}

	return requestBody, nil
}

func openAICacheOptionsModeIsExplicit(extraBody map[string]any) bool {
	mode, hasMode := openAICacheOptionsMode(extraBody)

	return hasMode && strings.EqualFold(mode, openAICacheBreakpointModeExplicit)
}

func openAIResponsesCacheBreakpointMessages(messages []ChatMessage) []ChatMessage {
	if len(messages) == 0 {
		return messages
	}

	breakpointIndex := openAIStablePrefixBreakpointIndex(messages)

	breakpointContent := openAIResponsesMessageContentWithCacheBreakpoint(
		messages[breakpointIndex].Content,
	)
	// Content is an interface{} that may hold uncomparable slices
	// ([]map[string]any or []ContentPart); direct == comparison panics on
	// such values, so compare with a non-panicking deep equality instead.
	if chatMessageContentsEqual(breakpointContent, messages[breakpointIndex].Content) {
		return messages
	}

	normalizedMessages := append([]ChatMessage(nil), messages...)
	normalizedMessages[breakpointIndex].Content = breakpointContent

	return normalizedMessages
}

func openAIStablePrefixBreakpointIndex(messages []ChatMessage) int {
	breakpointIndex := 0

	for index, message := range messages {
		if index == 0 {
			continue
		}

		if message.Role == searchtypes.MessageRoleAssistant {
			breakpointIndex = index + 1
		}
	}

	if breakpointIndex >= len(messages) {
		return len(messages) - 1
	}

	return breakpointIndex
}

func openAIResponsesMessageContentWithCacheBreakpoint(content any) any {
	switch typedContent := content.(type) {
	case string:
		if strings.TrimSpace(typedContent) == "" {
			return content
		}

		return []map[string]any{
			{
				searchtypes.MessageTypeKey: responsesInputTextType,
				searchtypes.MessageTextKey: typedContent,
				openAICacheBreakpointKey:   map[string]any{openAICacheOptionsModeKey: openAICacheBreakpointModeExplicit},
			},
		}

	case []map[string]any:
		if len(typedContent) == 0 {
			return content
		}

		parts := make([]map[string]any, len(typedContent))
		copy(parts, typedContent)

		if _, alreadyMarked := parts[len(parts)-1][openAICacheBreakpointKey]; alreadyMarked {
			return content
		}

		markedPart := maps.Clone(parts[len(parts)-1])
		markedPart[openAICacheBreakpointKey] = map[string]any{openAICacheOptionsModeKey: openAICacheBreakpointModeExplicit}
		parts[len(parts)-1] = markedPart

		return parts

	default:
		return content
	}
}

func responsesInput(messages []ChatMessage) ([]map[string]any, error) {
	input := make([]map[string]any, 0, len(messages))

	for index, message := range messages {
		convertedMessage, ok, err := responsesMessage(message)
		if err != nil {
			return nil, fmt.Errorf("convert responses input message %d: %w", index, err)
		}

		if !ok {
			continue
		}

		input = append(input, convertedMessage)
	}

	return input, nil
}

func responsesMessage(message ChatMessage) (map[string]any, bool, error) {
	role := strings.TrimSpace(message.Role)
	if role == "" {
		return nil, false, nil
	}

	switch role {
	case searchtypes.MessageRoleSystem, "developer":
		content, ok, err := responsesTextContent(message.Content)
		if err != nil {
			return nil, false, err
		}

		if !ok {
			return nil, false, nil
		}

		return map[string]any{
			searchtypes.MessageRoleKey:    role,
			searchtypes.MessageContentKey: content,
		}, true, nil
	case searchtypes.MessageRoleAssistant:
		content, ok, err := responsesTextContent(message.Content)
		if err != nil {
			return nil, false, err
		}

		if !ok {
			return nil, false, nil
		}

		return map[string]any{
			"role":    role,
			"content": content,
		}, true, nil
	case searchtypes.MessageRoleUser:
		content, contentOK, contentErr := responsesUserContent(message.Content)
		if contentErr != nil {
			breakpointParts, isBreakpointParts := message.Content.([]map[string]any)
			if !isBreakpointParts {
				return nil, false, contentErr
			}

			content, contentOK, contentErr = openAIResponsesBreakpointTextContent(
				breakpointParts,
				message.Content,
			)
			if contentErr != nil {
				return nil, false, contentErr
			}
		}

		if !contentOK {
			return nil, false, nil
		}

		return map[string]any{
			"role":    role,
			"content": content,
		}, true, nil
	default:
		return nil, false, fmt.Errorf("unsupported responses chat role %q: %w", role, os.ErrInvalid)
	}
}

func responsesTextContent(content any) (string, bool, error) {
	switch typedContent := content.(type) {
	case nil:
		return "", false, nil
	case string:
		if strings.TrimSpace(typedContent) == "" {
			return "", false, nil
		}

		return typedContent, true, nil
	case []ContentPart:
		if contentPartsContainNonText(typedContent) {
			return "", false, fmt.Errorf("unsupported responses text content type %T: %w", content, os.ErrInvalid)
		}

		textContent := support.ContentPartsText(typedContent)
		if strings.TrimSpace(textContent) == "" {
			return "", false, nil
		}

		return textContent, true, nil
	case []map[string]any:
		return openAIResponsesBreakpointTextContent(typedContent, content)
	default:
		return "", false, fmt.Errorf("unsupported responses text content type %T: %w", content, os.ErrInvalid)
	}
}

func openAIResponsesBreakpointTextContent(
	parts []map[string]any,
	originalContent any,
) (string, bool, error) {
	if len(parts) == 0 {
		return "", false, nil
	}

	textParts := make([]string, 0, len(parts))

	for _, part := range parts {
		partType, _ := part["type"].(string)
		if partType != responsesInputTextType && partType != searchtypes.ContentTypeText {
			return "", false, fmt.Errorf(
				"unsupported responses text content part type %T: %w",
				originalContent,
				os.ErrInvalid,
			)
		}

		textValue, _ := part["text"].(string)
		if strings.TrimSpace(textValue) == "" {
			return "", false, nil
		}

		textParts = append(textParts, textValue)
	}

	if len(textParts) == 0 {
		return "", false, nil
	}

	return strings.Join(textParts, "\n\n"), true, nil
}

func responsesUserContent(content any) (any, bool, error) {
	switch typedContent := content.(type) {
	case nil:
		return "", false, nil
	case string:
		if strings.TrimSpace(typedContent) == "" {
			return "", false, nil
		}

		return typedContent, true, nil
	case []ContentPart:
		if !contentPartsContainNonText(typedContent) {
			textContent := support.ContentPartsText(typedContent)
			if strings.TrimSpace(textContent) == "" {
				return "", false, nil
			}

			return textContent, true, nil
		}

		parts := make([]map[string]any, 0, len(typedContent))
		for _, part := range typedContent {
			convertedPart, ok, err := responsesUserPart(part)
			if err != nil {
				return nil, false, err
			}

			if !ok {
				continue
			}

			parts = append(parts, convertedPart)
		}

		if len(parts) == 0 {
			return "", false, nil
		}

		return parts, true, nil
	default:
		return nil, false, fmt.Errorf("unsupported responses user content type %T: %w", content, os.ErrInvalid)
	}
}

func responsesUserPart(part ContentPart) (map[string]any, bool, error) {
	partType, _ := part["type"].(string)

	switch partType {
	case searchtypes.ContentTypeText:
		textValue, _ := part[searchtypes.MessageTextKey].(string)
		if strings.TrimSpace(textValue) == "" {
			return nil, false, nil
		}

		return map[string]any{
			searchtypes.MessageTypeKey: responsesInputTextType,
			searchtypes.MessageTextKey: textValue,
		}, true, nil
	case searchtypes.ContentTypeImageURL:
		imageURL := ""
		switch typedImageURL := part["image_url"].(type) {
		case map[string]string:
			imageURL = typedImageURL["url"]
		case map[string]any:
			imageURL, _ = typedImageURL["url"].(string)
		default:
			return nil, false, fmt.Errorf("decode responses image_url content part: %w", os.ErrInvalid)
		}

		imageURL = strings.TrimSpace(imageURL)
		if imageURL == "" {
			return nil, false, nil
		}

		return map[string]any{
			searchtypes.MessageTypeKey:   responsesInputImageType,
			"image_url":                  imageURL,
			searchtypes.MessageDetailKey: responsesImageDetailAuto,
		}, true, nil
	case searchtypes.ContentTypeDocument, searchtypes.ContentTypeFileData:
		documentBytes, mimeType, filename, err := support.AttachmentBytes(part)
		if err != nil {
			return nil, false, fmt.Errorf("decode document part: %w", err)
		}

		if len(documentBytes) == 0 {
			return nil, false, nil
		}

		filePart := map[string]any{
			"type": responsesInputFileType,
			"file_data": fmt.Sprintf(
				"data:%s;base64,%s",
				mimeType,
				base64.StdEncoding.EncodeToString(documentBytes),
			),
		}

		if strings.TrimSpace(filename) != "" {
			filePart["filename"] = filename
		}

		return filePart, true, nil
	default:
		return nil, false, fmt.Errorf("unsupported responses content part type %q: %w", partType, os.ErrInvalid)
	}
}

func handleResponsesStreamPayload(
	payload []byte,
	handle func(StreamDelta) error,
	state *responsesStreamState,
) (bool, error) {
	delta, terminal, err := responsesStreamPayloadDelta(payload, state)
	if err != nil {
		return terminal, err
	}

	if delta.Content == "" &&
		delta.Thinking == "" &&
		delta.FinishReason == "" &&
		delta.ProviderResponseID == "" &&
		len(delta.ToolCalls) == 0 {
		return terminal, nil
	}

	err = handle(delta)
	if err != nil {
		return terminal, fmt.Errorf(handleStreamDeltaErrorFormat, err)
	}

	return terminal, nil
}

func newResponsesStreamState() *responsesStreamState {
	return &responsesStreamState{
		seenOutputItemIDs:    make(map[string]struct{}),
		seenOutputItemURLs:   make(map[string]struct{}),
		seenReasoningItemIDs: make(map[string]struct{}),
		seenToolCallIDs:      make(map[string]struct{}),
		toolCalls:            nil,
		hasVisibleContent:    false,
	}
}

// collectResponsesFunctionCall records a complete function_call output item.
// Items arrive via response.output_item.done (and again inside the final
// response.completed output), so they are deduplicated by call id.
func collectResponsesFunctionCall(state *responsesStreamState, item responsesOutputItem) {
	if state == nil {
		return
	}

	callID := strings.TrimSpace(item.CallID)
	if callID == "" {
		callID = strings.TrimSpace(item.ID)
	}

	name := strings.TrimSpace(item.Name)
	if callID == "" || name == "" {
		return
	}

	if _, seen := state.seenToolCallIDs[callID]; seen {
		return
	}

	state.seenToolCallIDs[callID] = struct{}{}
	state.toolCalls = append(state.toolCalls, FunctionToolCall{
		ID:        callID,
		Name:      name,
		Arguments: item.Arguments,
	})
}

func responsesStateToolCalls(state *responsesStreamState) []FunctionToolCall {
	if state == nil || len(state.toolCalls) == 0 {
		return nil
	}

	return append([]FunctionToolCall(nil), state.toolCalls...)
}

func responsesStreamPayloadDelta(
	payload []byte,
	state *responsesStreamState,
) (StreamDelta, bool, error) {
	var event responsesStreamEvent

	emptyDelta := emptyStreamDelta()

	err := json.Unmarshal(payload, &event)
	if err != nil {
		return emptyDelta, false, fmt.Errorf("decode responses stream Payload: %w", err)
	}

	eventType := strings.TrimSpace(event.Type)

	switch eventType {
	case responsesStreamEventReasoningSummaryTextDelta,
		responsesStreamEventReasoningTextDelta:
		delta := emptyDelta
		delta.Thinking = event.Delta

		return delta, false, nil
	case responsesStreamEventReasoningSummaryPartDone,
		responsesStreamEventReasoningTextDone:
		delta := emptyDelta
		delta.Thinking = "\n\n"

		return delta, false, nil
	case responsesStreamEventOutputDelta:
		if state != nil && event.Delta != "" {
			state.hasVisibleContent = true
		}

		delta := emptyDelta
		delta.Content = event.Delta

		return delta, false, nil
	case responsesStreamEventOutputDone:
		delta := emptyDelta

		if event.Item != nil &&
			strings.EqualFold(strings.TrimSpace(event.Item.Type), responsesOutputTypeFunctionCall) {
			collectResponsesFunctionCall(state, *event.Item)

			return delta, false, nil
		}

		delta.Thinking = responsesOutputItemThinking(event.Item, state)
		if delta.Thinking == "" {
			delta.Content = responsesOutputItemText(event.Item, state, false)
		}

		return delta, false, nil
	case responsesStreamEventCompleted:
		delta, completedErr := responsesCompletedDelta(
			event.Response,
			state,
		)

		return delta, true, completedErr
	case responsesStreamEventFailed, responsesStreamEventIncomplete:
		return emptyDelta, true, responsesTerminalError(eventType, event)
	case responsesStreamEventError:
		if event.Error != nil {
			return emptyDelta, true, openAIStreamEventError(
				event.Error.Message,
				event.Error.Type,
				event.Error.Code,
			)
		}

		return emptyDelta, true, openAIStreamEventError(event.Message, eventType, event.Code)
	default:
		return emptyDelta, false, nil
	}
}

func emptyStreamDelta() StreamDelta {
	return StreamDelta{
		Thinking:           "",
		Content:            "",
		FinishReason:       "",
		ProviderResponseID: "",
		SearchMetadata:     nil,
		ToolCalls:          nil,
	}
}

func responsesCompletedDelta(
	response *responsesStreamResponse,
	state *responsesStreamState,
) (StreamDelta, error) {
	if response == nil {
		return StreamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       finishReasonStop,
			ProviderResponseID: "",
			SearchMetadata:     nil,
			ToolCalls:          nil,
		}, nil
	}

	if response.Error != nil {
		return StreamDelta{
				Thinking:           "",
				Content:            "",
				FinishReason:       "",
				ProviderResponseID: "",
				SearchMetadata:     nil,
				ToolCalls:          nil,
			}, openAIStreamEventError(
				response.Error.Message,
				response.Error.Type,
				response.Error.Code,
			)
	}

	status := strings.TrimSpace(response.Status)
	if status != "" && !strings.EqualFold(status, responsesStatusCompleted) {
		reason := ""
		if response.IncompleteDetails != nil {
			reason = strings.TrimSpace(response.IncompleteDetails.Reason)
		}

		return StreamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata:     nil,
			ToolCalls:          nil,
		}, responsesStatusError(status, reason)
	}

	thinking := responsesOutputItemsThinking(response.Output, state)
	content := responsesOutputItemsText(response.Output, state, true)

	// Any function_call items in the final output that were not already
	// collected from output_item.done events are gathered here, so callers
	// see every tool call exactly once.
	for index := range response.Output {
		item := &response.Output[index]

		if strings.EqualFold(strings.TrimSpace(item.Type), responsesOutputTypeFunctionCall) {
			collectResponsesFunctionCall(state, *item)
		}
	}

	return StreamDelta{
		Thinking:           thinking,
		Content:            content,
		FinishReason:       finishReasonStop,
		ProviderResponseID: strings.TrimSpace(response.ID),
		SearchMetadata:     nil,
		ToolCalls:          responsesStateToolCalls(state),
	}, nil
}

func responsesOutputItemsText(
	items []responsesOutputItem,
	state *responsesStreamState,
	final bool,
) string {
	if len(items) == 0 {
		return ""
	}

	var builder strings.Builder

	for index := range items {
		builder.WriteString(responsesOutputItemText(&items[index], state, final))
	}

	return builder.String()
}

func responsesOutputItemsThinking(
	items []responsesOutputItem,
	state *responsesStreamState,
) string {
	if len(items) == 0 {
		return ""
	}

	var builder strings.Builder

	for index := range items {
		builder.WriteString(responsesOutputItemThinking(&items[index], state))
	}

	return builder.String()
}

func responsesOutputItemThinking(
	item *responsesOutputItem,
	state *responsesStreamState,
) string {
	summaryText := responsesReasoningSummaryText(item)
	if summaryText == "" || responsesReasoningItemSeen(state, item) {
		return ""
	}

	responsesMarkReasoningItemSeen(state, item)

	return summaryText + "\n\n"
}

func responsesReasoningSummaryText(item *responsesOutputItem) string {
	if item == nil || !strings.EqualFold(strings.TrimSpace(item.Type), responsesOutputTypeReasoning) {
		return ""
	}

	if len(item.Summary) == 0 || string(item.Summary) == "null" {
		return ""
	}

	var summaryText string

	err := json.Unmarshal(item.Summary, &summaryText)
	if err == nil {
		return strings.TrimSpace(summaryText)
	}

	var summaryParts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	err = json.Unmarshal(item.Summary, &summaryParts)
	if err != nil {
		return ""
	}

	var builder strings.Builder

	for _, part := range summaryParts {
		partType := strings.TrimSpace(part.Type)

		text := strings.TrimSpace(part.Text)
		if text == "" ||
			(partType != "" && !strings.EqualFold(partType, responsesReasoningSummaryTextType)) {
			continue
		}

		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}

		builder.WriteString(text)
	}

	return builder.String()
}

func responsesOutputItemText(
	item *responsesOutputItem,
	state *responsesStreamState,
	final bool,
) string {
	if item == nil {
		return ""
	}

	normalizedItem, ok := normalizeResponsesOutputItem(*item)
	if !ok || responsesOutputItemSeen(state, normalizedItem) {
		return ""
	}

	content := responsesOutputItemContent(normalizedItem, final)
	if content == "" {
		return ""
	}

	responsesMarkOutputItemSeen(state, normalizedItem)

	if state != nil {
		if state.hasVisibleContent {
			content = "\n\n" + content
		}

		state.hasVisibleContent = true
	}

	return content
}

func normalizeResponsesOutputItem(item responsesOutputItem) (responsesOutputItem, bool) {
	var emptyItem responsesOutputItem

	item.ID = strings.TrimSpace(item.ID)
	item.Type = strings.TrimSpace(item.Type)
	item.Result = strings.TrimSpace(item.Result)
	item.ResultURL = strings.TrimSpace(item.ResultURL)
	item.MIMEType = strings.TrimSpace(item.MIMEType)
	item.Action = strings.ToLower(strings.TrimSpace(item.Action))

	if !strings.EqualFold(item.Type, responsesOutputTypeImage) {
		return emptyItem, false
	}

	if item.ResultURL == "" && item.Result == "" {
		return emptyItem, false
	}

	return item, true
}

func responsesOutputItemContent(item responsesOutputItem, final bool) string {
	label := responsesOutputItemLabel(item.Action)

	if item.ResultURL != "" {
		return label + ":\n" + item.ResultURL
	}

	if !final || item.Result == "" {
		return ""
	}

	if item.MIMEType != "" {
		return label + " returned as " + item.MIMEType + ", but the provider did not expose a result URL."
	}

	return label + " returned, but the provider did not expose a result URL."
}

func responsesOutputItemLabel(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "edit":
		return "Edited image"
	case "generate":
		return "Generated image"
	default:
		return "Image output"
	}
}

func responsesOutputItemSeen(state *responsesStreamState, item responsesOutputItem) bool {
	if state == nil {
		return false
	}

	itemID := strings.TrimSpace(item.ID)
	if itemID != "" {
		if _, ok := state.seenOutputItemIDs[itemID]; ok {
			return true
		}
	}

	resultURL := strings.ToLower(strings.TrimSpace(item.ResultURL))
	if resultURL != "" {
		if _, ok := state.seenOutputItemURLs[resultURL]; ok {
			return true
		}
	}

	return false
}

func responsesReasoningItemSeen(
	state *responsesStreamState,
	item *responsesOutputItem,
) bool {
	if state == nil || item == nil {
		return false
	}

	itemID := strings.TrimSpace(item.ID)
	if itemID == "" {
		return false
	}

	_, ok := state.seenReasoningItemIDs[itemID]

	return ok
}

func responsesMarkOutputItemSeen(state *responsesStreamState, item responsesOutputItem) {
	if state == nil {
		return
	}

	itemID := strings.TrimSpace(item.ID)
	if itemID != "" {
		state.seenOutputItemIDs[itemID] = struct{}{}
	}

	resultURL := strings.ToLower(strings.TrimSpace(item.ResultURL))
	if resultURL != "" {
		state.seenOutputItemURLs[resultURL] = struct{}{}
	}
}

func responsesMarkReasoningItemSeen(
	state *responsesStreamState,
	item *responsesOutputItem,
) {
	if state == nil || item == nil {
		return
	}

	itemID := strings.TrimSpace(item.ID)
	if itemID != "" {
		state.seenReasoningItemIDs[itemID] = struct{}{}
	}
}

func responsesTerminalError(eventType string, event responsesStreamEvent) error {
	if event.Error != nil {
		return openAIStreamEventError(event.Error.Message, event.Error.Type, event.Error.Code)
	}

	if event.Response != nil {
		if event.Response.Error != nil {
			return openAIStreamEventError(
				event.Response.Error.Message,
				event.Response.Error.Type,
				event.Response.Error.Code,
			)
		}

		status := strings.TrimSpace(event.Response.Status)
		if status == "" {
			status = strings.TrimPrefix(eventType, "response.")
		}

		reason := ""
		if event.Response.IncompleteDetails != nil {
			reason = strings.TrimSpace(event.Response.IncompleteDetails.Reason)
		}

		return responsesStatusError(status, reason)
	}

	return openAIStreamEventError(event.Message, eventType, event.Code)
}

func responsesStatusError(status string, reason string) error {
	message := "provider ended the response with status=" + status
	if reason != "" {
		message += " reason=" + reason
	}

	return fmt.Errorf("%s: %w", message, os.ErrInvalid)
}

func buildResponsesURL(baseURL string, extraQuery map[string]any) (string, error) {
	parsedURL, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse responses base url %q: %w", baseURL, err)
	}

	parsedURL.Path = path.Join(parsedURL.Path, "responses")

	queryValues := parsedURL.Query()
	for key, value := range extraQuery {
		queryValues.Set(key, stringifyValue(value))
	}

	parsedURL.RawQuery = queryValues.Encode()

	return parsedURL.String(), nil
}

func splitConfiguredModel(configuredModel string) (string, error) {
	parts := strings.SplitN(strings.TrimSuffix(configuredModel, ":vision"), "/", splitConfiguredModelParts)
	if len(parts) != splitConfiguredModelParts {
		return "", fmt.Errorf("split configured model %q: %w", configuredModel, os.ErrInvalid)
	}

	return parts[0], nil
}

func usesBuiltInOpenAIProvider(providerName string, apiKind ProviderAPIKind) bool {
	return apiKind == ProviderAPIKindOpenAI && strings.EqualFold(strings.TrimSpace(providerName), "openai")
}

func chatMessageContentsEqual(left, right any) bool {
	return reflect.DeepEqual(left, right)
}

func contentPartsContainNonText(parts []ContentPart) bool {
	for _, part := range parts {
		partType, _ := part[searchtypes.MessageTypeKey].(string)
		if partType != searchtypes.ContentTypeText {
			return true
		}
	}

	return false
}

func requestKey(provider ProviderRequestConfig) string {
	if key := firstAPIKey(provider.APIKeys); key != "" {
		return key
	}

	return provider.APIKey
}

const openAICacheBreakpointModeExplicit = "explicit"

const splitConfiguredModelParts = 2
