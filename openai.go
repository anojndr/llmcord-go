package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	openAIStreamErrorEventType           = "error"
	openAIStreamMessagePartsCapacity     = 3
	openAIContentFilterFinishReason      = "content_filter"
	openAIStreamToolCallsFinishReason    = "tool_calls"
	openAIStreamFunctionCallFinishReason = "function_call"
	openAIClientRequestIDHeader          = "X-Client-Request-Id"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatCompletionRequest struct {
	Provider           providerRequestConfig
	Model              string
	ConfiguredModel    string
	ContextWindow      int
	SessionID          string
	PreviousResponseID string
	RequestID          string
	Messages           []chatMessage
}

type providerRequestConfig struct {
	APIKind         providerAPIKind
	BaseURL         string
	APIKey          string
	APIKeys         []string
	UseResponsesAPI bool
	EnableGrounding bool
	ExtraHeaders    map[string]any
	ExtraQuery      map[string]any
	ExtraBody       map[string]any
}

type streamDelta struct {
	Thinking           string
	Content            string
	FinishReason       string
	Usage              *tokenUsage
	ReasoningTokens    int
	ProviderResponseID string
	SearchMetadata     *searchMetadata
}

type tokenUsage struct {
	Input            int
	Output           int
	CachedInput      int
	CacheWriteTokens int
}

type openAIClient struct {
	httpClient *http.Client
}

func newOpenAIClient(httpClient *http.Client) openAIClient {
	if httpClient == nil {
		httpClient = newOptimizedHTTPClient()
	}

	return openAIClient{httpClient: httpClient}
}

func (client openAIClient) streamChatCompletion(
	ctx context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	if request.Provider.UseResponsesAPI {
		return client.streamResponses(ctx, request, handle)
	}

	requestURL, err := buildChatCompletionURL(request.Provider.BaseURL, request.Provider.ExtraQuery)
	if err != nil {
		return fmt.Errorf("build chat completion url: %w", err)
	}

	requestBody := buildChatCompletionRequestBody(request)

	statusCode, statusText, responseHeaders, responseBody, err := client.streamChatCompletionAttempt(
		ctx,
		request,
		requestURL,
		requestBody,
		handle,
	)
	if err != nil {
		return err
	}

	if statusCode == 0 {
		return nil
	}

	return newOpenAIProviderStatusError(
		"chat completion request failed",
		statusCode,
		statusText,
		responseHeaders,
		responseBody,
		false,
	)
}

func (client openAIClient) streamChatCompletionAttempt(
	ctx context.Context,
	request chatCompletionRequest,
	requestURL string,
	requestBody map[string]any,
	handle func(streamDelta) error,
) (int, string, http.Header, []byte, error) {
	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return 0, "", nil, nil, fmt.Errorf("marshal chat completion request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		requestURL,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return 0, "", nil, nil, fmt.Errorf("create chat completion request: %w", err)
	}

	httpRequest.Header.Set("Accept", "text/event-stream")

	apiKey := request.Provider.primaryAPIKey()
	if apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+apiKey)
	} else if !isOpenAIRouterRequest(request) {
		httpRequest.Header.Set("Authorization", "Bearer sk-no-key-required")
	}

	httpRequest.Header.Set("Content-Type", "application/json")
	setOpenAIClientRequestIDHeader(httpRequest, request)

	for key, value := range request.Provider.ExtraHeaders {
		httpRequest.Header.Set(key, stringifyValue(value))
	}

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return 0, "", nil, nil, fmt.Errorf("send chat completion request: %w", err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)

		_ = httpResponse.Body.Close()

		if readErr != nil {
			return 0, "", nil, nil, fmt.Errorf(
				"read chat completion error response after status %d: %w",
				httpResponse.StatusCode,
				readErr,
			)
		}

		return httpResponse.StatusCode, httpResponse.Status, httpResponse.Header.Clone(), responseBody, nil
	}

	choicesSeen := false

	wrappedHandle := func(delta streamDelta) error {
		choicesSeen = true

		return handle(delta)
	}

	doneSeen, err := consumeServerSentEvents(httpResponse.Body, func(payload []byte) error {
		return handleStreamPayload(payload, wrappedHandle)
	})

	_ = httpResponse.Body.Close()

	if err != nil {
		return 0, "", nil, nil, fmt.Errorf("consume chat completion stream: %w", err)
	}

	if !doneSeen {
		if choicesSeen && openAIProviderOmitDoneMarker(request) {
			return 0, "", nil, nil, nil
		}

		return 0, "", nil, nil, fmt.Errorf("chat completion stream ended before [DONE]: %w", io.ErrUnexpectedEOF)
	}

	return 0, "", nil, nil, nil
}

func openAIProviderOmitDoneMarker(request chatCompletionRequest) bool {
	if isOpenAIRouterRequest(request) {
		return true
	}

	baseURL := strings.ToLower(strings.TrimSpace(request.Provider.BaseURL))

	return strings.Contains(baseURL, "abc-tunnel")
}

func isOpenAIRouterRequest(request chatCompletionRequest) bool {
	configuredModel := strings.ToLower(strings.TrimSpace(request.ConfiguredModel))
	if strings.HasPrefix(configuredModel, "9router/") {
		return true
	}

	baseURL := strings.ToLower(strings.TrimSpace(request.Provider.BaseURL))

	return strings.Contains(baseURL, "9router")
}

func buildChatCompletionRequestBody(request chatCompletionRequest) map[string]any {
	return buildChatCompletionRequestBodyWithUsageOption(request, true)
}

func buildChatCompletionRequestBodyWithUsageOption(
	request chatCompletionRequest,
	includeStreamingUsage bool,
) map[string]any {
	requestBody := make(map[string]any, len(request.Provider.ExtraBody)+requestBodyBaseFields)
	requestBody["messages"] = openAINormalizeRequestMessages(
		openAIReplaceSystemRoleWithDeveloper(
			openAICacheBreakpointMessages(
				requestMessagesWithFileOrImageOnlyQueryPlaceholder(request.Messages),
				request,
			),
			request.Model,
		),
	)
	requestBody["model"] = request.Model
	requestBody["stream"] = true
	addOpenAIPromptCacheKey(requestBody, request)

	maps.Copy(requestBody, request.Provider.ExtraBody)

	if request.Provider.APIKind == providerAPIKindOpenAI && !request.Provider.UseResponsesAPI {
		defaultOpenAIServiceTier(requestBody)
		addOpenAICacheOptions(requestBody, request)
	}

	if includeStreamingUsage {
		ensureOpenAIStreamingUsageOption(requestBody)
	}

	return requestBody
}

func openAIReplaceSystemRoleWithDeveloper(messages []chatMessage, model string) []chatMessage {
	if !openAIModelIsGPT56Family(model) {
		return messages
	}

	changed := false
	normalizedMessages := make([]chatMessage, len(messages))

	for index, message := range messages {
		if message.Role == messageRoleSystem {
			message.Role = "developer"
			changed = true
		}

		normalizedMessages[index] = message
	}

	if !changed {
		return messages
	}

	return normalizedMessages
}

func openAICacheBreakpointMessages(
	messages []chatMessage,
	request chatCompletionRequest,
) []chatMessage {
	if request.Provider.UseResponsesAPI {
		return messages
	}

	if openAIRequestPromptCacheKeyPrefix(request) == "" || strings.TrimSpace(request.SessionID) == "" {
		return messages
	}

	mode, hasExplicitOptions := openAICacheOptionsMode(request.Provider.ExtraBody)
	if len(messages) == 0 || (hasExplicitOptions && !strings.EqualFold(mode, openAICacheBreakpointModeExplicit)) {
		return messages
	}

	breakpointIndex := openAIStablePrefixBreakpointIndex(messages)

	content, breakpointAdded := openAIContentPartWithCacheBreakpoint(messages[breakpointIndex].Content)
	if !breakpointAdded {
		return messages
	}

	normalizedMessages := append([]chatMessage(nil), messages...)
	normalizedMessages[breakpointIndex].Content = content

	return normalizedMessages
}

func openAIContentPartWithCacheBreakpoint(content any) (any, bool) {
	switch typedContent := content.(type) {
	case string:
		if strings.TrimSpace(typedContent) == "" {
			return content, false
		}

		return []map[string]any{
			{
				messageTypeKey:           contentTypeText,
				messageTextKey:           typedContent,
				openAICacheBreakpointKey: map[string]any{openAICacheOptionsModeKey: openAICacheBreakpointModeExplicit},
			},
		}, true

	case []map[string]any:
		if len(typedContent) == 0 {
			return content, false
		}

		parts := make([]map[string]any, len(typedContent))
		copy(parts, typedContent)

		if _, alreadyMarked := parts[len(parts)-1][openAICacheBreakpointKey]; alreadyMarked {
			return content, false
		}

		markedPart := maps.Clone(parts[len(parts)-1])
		markedPart[openAICacheBreakpointKey] = map[string]any{openAICacheOptionsModeKey: openAICacheBreakpointModeExplicit}
		parts[len(parts)-1] = markedPart

		return parts, true

	default:
		return content, false
	}
}

func addOpenAICacheOptions(requestBody map[string]any, request chatCompletionRequest) {
	if requestBody == nil ||
		openAIRequestPromptCacheKeyPrefix(request) == "" ||
		strings.TrimSpace(request.SessionID) == "" {
		return
	}

	if _, exists := requestBody["prompt_cache_options"]; exists {
		return
	}

	requestBody["prompt_cache_options"] = map[string]any{
		"mode": "implicit",
		"ttl":  "30m",
	}
}

func openAINormalizeRequestMessages(messages []chatMessage) []chatMessage {
	if len(messages) == 0 {
		return messages
	}

	normalizedMessages := make([]chatMessage, len(messages))
	changed := false

	for index, message := range messages {
		normalizedContent, contentChanged := openAINormalizeMessageContent(message.Content)
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

func openAINormalizeMessageContent(content any) (any, bool) {
	switch typedContent := content.(type) {
	case []contentPart:
		normalizedParts := make([]contentPart, len(typedContent))
		changed := false

		for index, part := range typedContent {
			normalizedPart, partChanged := openAINormalizeContentPart(part)
			if partChanged {
				changed = true
			}

			normalizedParts[index] = normalizedPart
		}

		if !changed {
			return content, false
		}

		return normalizedParts, true

	case []map[string]any:
		normalizedParts := make([]map[string]any, len(typedContent))
		changed := false

		for index, part := range typedContent {
			normalizedPart, partChanged := openAINormalizeContentPartMap(part)
			if partChanged {
				changed = true
			}

			normalizedParts[index] = normalizedPart
		}

		if !changed {
			return content, false
		}

		return normalizedParts, true

	default:
		return content, false
	}
}

func nestedRequestBodyMap(requestBody map[string]any, key string) map[string]any {
	existing, found := requestBody[key]
	if !found {
		nested := make(map[string]any)
		requestBody[key] = nested

		return nested
	}

	nested, typeOK := existing.(map[string]any)
	if typeOK {
		cloned := maps.Clone(nested)
		requestBody[key] = cloned

		return cloned
	}

	nested = make(map[string]any)
	requestBody[key] = nested

	return nested
}

func openAINormalizeContentPart(part contentPart) (contentPart, bool) {
	partType, _ := part["type"].(string)
	if partType != contentTypeImageURL {
		return part, false
	}

	normalizedPart := cloneContentPart(part)

	imageURLVal, exists := normalizedPart["image_url"]
	if !exists || imageURLVal == nil {
		return part, false
	}

	switch typed := imageURLVal.(type) {
	case string:
		normalizedPart["image_url"] = map[string]string{
			messageURLKey:    typed,
			messageDetailKey: xAIResponsesImageDetailAuto,
		}

		return normalizedPart, true

	case map[string]string:
		if _, hasDetail := typed[messageDetailKey]; !hasDetail {
			clonedMap := maps.Clone(typed)
			clonedMap[messageDetailKey] = xAIResponsesImageDetailAuto
			normalizedPart["image_url"] = clonedMap

			return normalizedPart, true
		}

		return part, false

	case map[string]any:
		if _, hasDetail := typed[messageDetailKey]; !hasDetail {
			clonedMap := maps.Clone(typed)
			clonedMap[messageDetailKey] = xAIResponsesImageDetailAuto
			normalizedPart["image_url"] = clonedMap

			return normalizedPart, true
		}

		return part, false

	default:
		return part, false
	}
}

func openAINormalizeContentPartMap(part map[string]any) (map[string]any, bool) {
	partType, _ := part["type"].(string)
	if partType != contentTypeImageURL {
		return part, false
	}

	normalizedPart := maps.Clone(part)

	imageURLVal, exists := normalizedPart["image_url"]
	if !exists || imageURLVal == nil {
		return part, false
	}

	switch typed := imageURLVal.(type) {
	case string:
		normalizedPart["image_url"] = map[string]string{
			messageURLKey:    typed,
			messageDetailKey: xAIResponsesImageDetailAuto,
		}

		return normalizedPart, true

	case map[string]string:
		if _, hasDetail := typed[messageDetailKey]; !hasDetail {
			clonedMap := maps.Clone(typed)
			clonedMap[messageDetailKey] = "auto"
			normalizedPart["image_url"] = clonedMap

			return normalizedPart, true
		}

		return part, false

	case map[string]any:
		if _, hasDetail := typed[messageDetailKey]; !hasDetail {
			clonedMap := maps.Clone(typed)
			clonedMap[messageDetailKey] = "auto"
			normalizedPart["image_url"] = clonedMap

			return normalizedPart, true
		}

		return part, false

	default:
		return part, false
	}
}

func defaultOpenAIServiceTier(requestBody map[string]any) {
	if requestBody == nil {
		return
	}

	_, hasServiceTierSnake := requestBody["service_tier"]
	_, hasServiceTierCamel := requestBody["serviceTier"]

	if !hasServiceTierSnake && !hasServiceTierCamel {
		requestBody["service_tier"] = "priority"
	}
}

func ensureOpenAIStreamingUsageOption(requestBody map[string]any) {
	rawStreamOptions, hasStreamOptions := requestBody["stream_options"]
	if !hasStreamOptions || rawStreamOptions == nil {
		requestBody["stream_options"] = map[string]any{"include_usage": true}

		return
	}

	streamOptions, streamOptionsOK := rawStreamOptions.(map[string]any)
	if !streamOptionsOK {
		return
	}

	clonedStreamOptions := maps.Clone(streamOptions)
	clonedStreamOptions["include_usage"] = true
	requestBody["stream_options"] = clonedStreamOptions
}

func buildChatCompletionURL(baseURL string, extraQuery map[string]any) (string, error) {
	parsedURL, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse base url %q: %w", baseURL, err)
	}

	parsedURL.Path = path.Join(parsedURL.Path, "chat", "completions")

	queryValues := parsedURL.Query()
	for key, value := range extraQuery {
		queryValues.Set(key, stringifyValue(value))
	}

	parsedURL.RawQuery = queryValues.Encode()

	return parsedURL.String(), nil
}

func stringifyValue(value any) string {
	return strings.TrimSpace(fmt.Sprint(value))
}

func openAIAPIKey(apiKey string) string {
	if apiKey == "" {
		return "sk-no-key-required"
	}

	return apiKey
}

func setOpenAIClientRequestIDHeader(
	httpRequest *http.Request,
	request chatCompletionRequest,
) {
	if httpRequest == nil || request.RequestID == "" {
		return
	}

	if request.Provider.APIKind != providerAPIKindOpenAI ||
		!openAIConfiguredModel(request.ConfiguredModel) {
		return
	}

	httpRequest.Header.Set(openAIClientRequestIDHeader, request.RequestID)
}

func consumeServerSentEvents(reader io.Reader, handle func([]byte) error) (bool, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, sseScannerInitialBuffer), sseScannerMaxBuffer)

	var eventData strings.Builder

	doneSeen := false

	flushEvent := func() error {
		if eventData.Len() == 0 {
			return nil
		}

		payload := eventData.String()
		eventData.Reset()

		if payload == "[DONE]" {
			doneSeen = true

			return nil
		}

		return handle([]byte(payload))
	}

	for scanner.Scan() {
		line := scanner.Text()

		doneReached, err := appendServerSentEventLine(line, &eventData, &doneSeen, flushEvent)
		if err != nil {
			return doneSeen, err
		}

		if doneReached {
			return doneSeen, nil
		}
	}

	err := scanner.Err()
	if err != nil {
		return doneSeen, fmt.Errorf("scan server-sent events: %w", err)
	}

	err = flushEvent()
	if err != nil {
		return doneSeen, fmt.Errorf("flush server-sent events: %w", err)
	}

	return doneSeen, nil
}

func appendServerSentEventLine(
	line string,
	eventData *strings.Builder,
	doneSeen *bool,
	flushEvent func() error,
) (bool, error) {
	if strings.TrimSpace(line) == "" {
		return false, flushEvent()
	}

	if !strings.HasPrefix(line, "data:") {
		return false, nil
	}

	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "[DONE]" {
		*doneSeen = true

		return true, nil
	}

	if eventData.Len() > 0 {
		eventData.WriteByte('\n')
	}

	eventData.WriteString(payload)

	return false, nil
}

func handleStreamPayload(payload []byte, handle func(streamDelta) error) error {
	delta, err := openAIStreamPayloadDelta(payload)
	if err != nil {
		return err
	}

	if delta.Content != "" {
		err = handle(streamDelta{
			Thinking:           "",
			Content:            delta.Content,
			FinishReason:       "",
			Usage:              nil,
			ReasoningTokens:    delta.ReasoningTokens,
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
		if err != nil {
			return fmt.Errorf(handleStreamDeltaErrorFormat, err)
		}
	}

	if delta.Usage != nil {
		err = handle(streamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       "",
			Usage:              cloneTokenUsage(delta.Usage),
			ReasoningTokens:    delta.ReasoningTokens,
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
		if err != nil {
			return fmt.Errorf(handleStreamDeltaErrorFormat, err)
		}
	}

	if delta.FinishReason != "" {
		err = openAIStreamFinishReasonError(delta.FinishReason)
		if err != nil {
			return err
		}

		err = handle(streamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       delta.FinishReason,
			Usage:              nil,
			ReasoningTokens:    delta.ReasoningTokens,
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
		if err != nil {
			return fmt.Errorf(handleStreamDeltaErrorFormat, err)
		}
	}

	return nil
}

func openAIStreamPayloadDelta(payload []byte) (streamDelta, error) {
	type streamChoiceDelta struct {
		Content string `json:"content"`
	}

	type streamChoice struct {
		Delta        streamChoiceDelta `json:"delta"`
		FinishReason *string           `json:"finish_reason"`
	}

	type streamError struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}

	type streamEnvelope struct {
		Choices []streamChoice `json:"choices"`
		Error   *streamError   `json:"error"`
		Usage   *streamUsage   `json:"usage"`
	}

	var envelope streamEnvelope

	err := json.Unmarshal(payload, &envelope)
	if err != nil {
		return streamDelta{
			ReasoningTokens:    0,
			Thinking:           "",
			Content:            "",
			FinishReason:       "",
			Usage:              nil,
			ProviderResponseID: "",
			SearchMetadata:     nil,
		}, fmt.Errorf("decode stream payload: %w", err)
	}

	if envelope.Error != nil {
		return streamDelta{
				ReasoningTokens:    0,
				Thinking:           "",
				Content:            "",
				FinishReason:       "",
				Usage:              nil,
				ProviderResponseID: "",
				SearchMetadata:     nil,
			}, openAIStreamEventError(
				envelope.Error.Message,
				envelope.Error.Type,
				envelope.Error.Code,
			)
	}

	delta := streamDelta{
		Thinking:           "",
		Content:            "",
		FinishReason:       "",
		Usage:              openAIStreamUsage(envelope.Usage),
		ReasoningTokens:    openAIStreamReasoningTokens(envelope.Usage),
		ProviderResponseID: "",
		SearchMetadata:     nil,
	}

	if len(envelope.Choices) == 0 {
		return delta, nil
	}

	delta.Content = envelope.Choices[0].Delta.Content
	if envelope.Choices[0].FinishReason != nil {
		delta.FinishReason = strings.TrimSpace(*envelope.Choices[0].FinishReason)
	}

	return delta, nil
}

type streamUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func openAIStreamUsage(usage *streamUsage) *tokenUsage {
	if usage == nil {
		return nil
	}

	convertedUsage := &tokenUsage{
		Input:            usage.PromptTokens,
		Output:           usage.CompletionTokens,
		CachedInput:      0,
		CacheWriteTokens: 0,
	}

	if usage.PromptTokensDetails != nil {
		convertedUsage.CachedInput = usage.PromptTokensDetails.CachedTokens
		convertedUsage.CacheWriteTokens = usage.PromptTokensDetails.CacheWriteTokens
	}

	return convertedUsage
}

func openAIStreamReasoningTokens(usage *streamUsage) int {
	if usage == nil || usage.CompletionTokensDetails == nil {
		return 0
	}

	return usage.CompletionTokensDetails.ReasoningTokens
}

func cloneTokenUsage(usage *tokenUsage) *tokenUsage {
	if usage == nil {
		return nil
	}

	clonedUsage := *usage

	return &clonedUsage
}

func openAIStreamEventError(message string, eventType string, code any) error {
	messageParts := make([]string, 0, openAIStreamMessagePartsCapacity)

	message = strings.TrimSpace(message)
	if message != "" {
		messageParts = append(messageParts, message)
	}

	typ := strings.TrimSpace(eventType)
	if typ != "" && !strings.EqualFold(typ, message) {
		messageParts = append(messageParts, "type="+typ)
	}

	if code != nil {
		codeText := strings.TrimSpace(fmt.Sprint(code))
		if codeText != "" && codeText != "<nil>" {
			messageParts = append(messageParts, "code="+codeText)
		}
	}

	if len(messageParts) == 0 {
		messageParts = append(messageParts, "chat completion stream error")
	}

	errorText := strings.Join(messageParts, " ")

	statusCode := openAIStreamErrorStatusCode(message, eventType, code)
	if statusCode == 0 {
		return fmt.Errorf("%s: %w", errorText, os.ErrInvalid)
	}

	return providerStatusError{
		StatusCode: statusCode,
		Message:    errorText,
		Err:        os.ErrInvalid,
	}
}

const openAIStreamStatusMatchParts = 4

var openAIStreamErrorStatusCodePattern = regexp.MustCompile(`\[?\s*([0-9]{3})\s*\]?`)

func openAIStreamErrorStatusCode(message string, eventType string, code any) int {
	statusCode := openAIStreamStatusCodeFromText(stringifyValue(code))
	if statusCode != 0 {
		return statusCode
	}

	statusCode = openAIStreamStatusCodeFromText(message)
	if statusCode != 0 {
		return statusCode
	}

	if strings.EqualFold(strings.TrimSpace(eventType), "server_error") {
		return http.StatusServiceUnavailable
	}

	return 0
}

func openAIStreamStatusCodeFromText(text string) int {
	for _, match := range openAIStreamErrorStatusCodePattern.FindAllStringSubmatchIndex(text, -1) {
		if len(match) != openAIStreamStatusMatchParts {
			continue
		}

		if match[2] > 0 && isASCIIDigit(text[match[2]-1]) {
			continue
		}

		if match[3] < len(text) && isASCIIDigit(text[match[3]]) {
			continue
		}

		statusCode, err := strconv.Atoi(text[match[2]:match[3]])
		if err == nil && statusCode >= http.StatusBadRequest && statusCode <= 599 {
			return statusCode
		}
	}

	return 0
}

func isASCIIDigit(char byte) bool {
	return char >= '0' && char <= '9'
}

func openAIStreamFinishReasonError(finishReason string) error {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "", finishReasonStop, "end_turn", finishReasonLength:
		return nil
	case openAIContentFilterFinishReason:
		return fmt.Errorf("provider blocked the response (finish_reason=%s): %w", finishReason, os.ErrInvalid)
	case openAIStreamToolCallsFinishReason, openAIStreamFunctionCallFinishReason, openAIStreamErrorEventType:
		return fmt.Errorf("provider ended the stream with finish_reason=%s: %w", finishReason, os.ErrInvalid)
	default:
		return nil
	}
}
