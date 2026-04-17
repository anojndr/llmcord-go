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
	"reflect"
	"regexp"
	"strings"
)

const (
	openAIStreamErrorEventType           = "error"
	openAIStreamMessagePartsCapacity     = 3
	openAIContentFilterFinishReason      = "content_filter"
	openAIStreamToolCallsFinishReason    = "tool_calls"
	openAIStreamFunctionCallFinishReason = "function_call"
	openAIDegradedFunctionIDMatchParts   = 2
	openAIClientRequestIDHeader          = "X-Client-Request-Id"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatCompletionRequest struct {
	Provider                    providerRequestConfig
	Model                       string
	ConfiguredModel             string
	ContextWindow               int
	AutoCompactThresholdPercent int
	SessionID                   string
	PreviousResponseID          string
	RequestID                   string
	Messages                    []chatMessage
}

type providerRequestConfig struct {
	APIKind         providerAPIKind
	BaseURL         string
	APIKey          string
	APIKeys         []string
	UseResponsesAPI bool
	ExtraHeaders    map[string]any
	ExtraQuery      map[string]any
	ExtraBody       map[string]any
}

type streamDelta struct {
	Thinking           string
	Content            string
	FinishReason       string
	Usage              *tokenUsage
	ProviderResponseID string
	SearchMetadata     *searchMetadata
}

type tokenUsage struct {
	Input  int
	Output int
}

type openAIClient struct {
	httpClient *http.Client
}

var openAIDegradedFunctionIDPattern = regexp.MustCompile(
	`(?i)function id ['"]?([^'":]+)['"]?:\s*degraded function cannot be invoked`,
)

func newOpenAIClient(httpClient *http.Client) openAIClient {
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

	excludedFunctionIDs := make(map[string]struct{})
	includeStreamingUsage := true

	for {
		requestBody := openAIStreamRequestBody(request, includeStreamingUsage, excludedFunctionIDs)

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

		retry, nextIncludeStreamingUsage := openAIShouldRetryChatCompletion(
			statusCode,
			statusText,
			responseHeaders,
			responseBody,
			requestBody,
			includeStreamingUsage,
			excludedFunctionIDs,
		)
		includeStreamingUsage = nextIncludeStreamingUsage

		if retry {
			continue
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
}

func openAIStreamRequestBody(
	request chatCompletionRequest,
	includeStreamingUsage bool,
	excludedFunctionIDs map[string]struct{},
) map[string]any {
	requestBody := buildChatCompletionRequestBodyWithUsageOption(request, includeStreamingUsage)
	if len(excludedFunctionIDs) == 0 {
		return requestBody
	}

	sanitizedRequestBody, changed := excludeDegradedFunctionsFromChatCompletionRequestBody(
		requestBody,
		excludedFunctionIDs,
	)
	if !changed {
		return requestBody
	}

	return sanitizedRequestBody
}

func openAIShouldRetryChatCompletion(
	statusCode int,
	statusText string,
	responseHeaders http.Header,
	responseBody []byte,
	requestBody map[string]any,
	includeStreamingUsage bool,
	excludedFunctionIDs map[string]struct{},
) (bool, bool) {
	if statusCode != http.StatusBadRequest {
		return false, includeStreamingUsage
	}

	if includeStreamingUsage &&
		openAIShouldRetryWithoutStreamingUsage(
			statusCode,
			statusText,
			responseHeaders,
			responseBody,
		) {
		return true, false
	}

	degradedFunctionIDs := openAIDegradedFunctionIDs(responseBody)
	if addOpenAIExcludedFunctionIDs(excludedFunctionIDs, degradedFunctionIDs) {
		_, changed := excludeDegradedFunctionsFromChatCompletionRequestBody(
			requestBody,
			excludedFunctionIDs,
		)
		if changed {
			return true, includeStreamingUsage
		}
	}

	return false, includeStreamingUsage
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
	httpRequest.Header.Set("Authorization", "Bearer "+openAIAPIKey(request.Provider.primaryAPIKey()))
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

	doneSeen, err := consumeServerSentEvents(httpResponse.Body, func(payload []byte) error {
		return handleStreamPayload(payload, handle)
	})

	_ = httpResponse.Body.Close()

	if err != nil {
		return 0, "", nil, nil, fmt.Errorf("consume chat completion stream: %w", err)
	}

	if !doneSeen {
		return 0, "", nil, nil, fmt.Errorf("chat completion stream ended before [DONE]: %w", io.ErrUnexpectedEOF)
	}

	return 0, "", nil, nil, nil
}

func buildChatCompletionRequestBody(request chatCompletionRequest) map[string]any {
	return buildChatCompletionRequestBodyWithUsageOption(request, true)
}

func buildChatCompletionRequestBodyWithUsageOption(
	request chatCompletionRequest,
	includeStreamingUsage bool,
) map[string]any {
	requestBody := make(map[string]any, len(request.Provider.ExtraBody)+requestBodyBaseFields)
	requestBody["messages"] = requestMessagesWithFileOrImageOnlyQueryPlaceholder(request.Messages)
	requestBody["model"] = request.Model
	requestBody["stream"] = true
	addOpenAIPromptCacheKey(requestBody, request)

	maps.Copy(requestBody, request.Provider.ExtraBody)

	if includeStreamingUsage {
		ensureOpenAIStreamingUsageOption(requestBody)
	}

	return requestBody
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

func openAIShouldRetryWithoutStreamingUsage(
	statusCode int,
	statusText string,
	responseHeaders http.Header,
	responseBody []byte,
) bool {
	errorInfo := parseOpenAIHTTPErrorResponse(
		statusCode,
		statusText,
		responseHeaders,
		responseBody,
		false,
	)

	normalizedParam := strings.ToLower(strings.TrimSpace(errorInfo.Param))
	switch normalizedParam {
	case "stream_options", "stream_options.include_usage":
		return true
	}

	normalizedMessage := strings.ToLower(strings.TrimSpace(errorInfo.Message))
	if !strings.Contains(normalizedMessage, "stream_options") &&
		!strings.Contains(normalizedMessage, "include_usage") {
		return false
	}

	return strings.EqualFold(errorInfo.Code, "unsupported_parameter") ||
		strings.Contains(normalizedMessage, "unknown") ||
		strings.Contains(normalizedMessage, "unsupported") ||
		strings.Contains(normalizedMessage, "invalid")
}

func excludeDegradedFunctionsFromChatCompletionRequestBody(
	requestBody map[string]any,
	excludedFunctionIDs map[string]struct{},
) (map[string]any, bool) {
	if len(excludedFunctionIDs) == 0 {
		return requestBody, false
	}

	sanitizedBody := maps.Clone(requestBody)
	changed := false

	filteredTools, toolsChanged := filterExcludedOpenAIRequestValues(
		requestBody["tools"],
		excludedFunctionIDs,
	)
	if toolsChanged {
		changed = true

		if len(filteredTools) == 0 {
			delete(sanitizedBody, "tools")
		} else {
			sanitizedBody["tools"] = filteredTools
		}
	}

	filteredFunctions, functionsChanged := filterExcludedOpenAIRequestValues(
		requestBody["functions"],
		excludedFunctionIDs,
	)
	if functionsChanged {
		changed = true

		if len(filteredFunctions) == 0 {
			delete(sanitizedBody, "functions")
		} else {
			sanitizedBody["functions"] = filteredFunctions
		}
	}

	if openAIRequestValueReferencesExcludedFunction(requestBody["tool_choice"], excludedFunctionIDs) {
		changed = true

		delete(sanitizedBody, "tool_choice")
	}

	if openAIRequestValueReferencesExcludedFunction(requestBody["function_call"], excludedFunctionIDs) {
		changed = true

		delete(sanitizedBody, "function_call")
	}

	if !changed {
		return requestBody, false
	}

	if _, ok := sanitizedBody["tools"]; !ok {
		delete(sanitizedBody, "tool_choice")
	}

	if _, ok := sanitizedBody["functions"]; !ok {
		delete(sanitizedBody, "function_call")
	}

	return sanitizedBody, true
}

func filterExcludedOpenAIRequestValues(
	rawValues any,
	excludedFunctionIDs map[string]struct{},
) ([]any, bool) {
	values, ok := openAIRequestValueSlice(rawValues)
	if !ok {
		return nil, false
	}

	filteredValues := make([]any, 0, len(values))
	changed := false

	for _, value := range values {
		if openAIRequestValueReferencesExcludedFunction(value, excludedFunctionIDs) {
			changed = true

			continue
		}

		filteredValues = append(filteredValues, value)
	}

	return filteredValues, changed
}

func openAIRequestValueReferencesExcludedFunction(
	value any,
	excludedFunctionIDs map[string]struct{},
) bool {
	if value == nil || len(excludedFunctionIDs) == 0 {
		return false
	}

	object, mapOK := openAIRequestValueMap(value)
	if mapOK {
		return openAIRequestMapReferencesExcludedFunction(object, excludedFunctionIDs)
	}

	values, ok := openAIRequestValueSlice(value)
	if !ok {
		return false
	}

	return openAIRequestSliceReferencesExcludedFunction(values, excludedFunctionIDs)
}

func isOpenAIFunctionIDField(field string) bool {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "id", "function_id", "tool_id":
		return true
	default:
		return false
	}
}

func openAIRequestValueMap(value any) (map[string]any, bool) {
	typedValue, ok := value.(map[string]any)
	if ok {
		return typedValue, true
	}

	reflectedValue := reflect.ValueOf(value)
	if !reflectedValue.IsValid() || reflectedValue.Kind() != reflect.Map {
		return nil, false
	}

	if reflectedValue.Type().Key().Kind() != reflect.String {
		return nil, false
	}

	converted := make(map[string]any, reflectedValue.Len())

	iterator := reflectedValue.MapRange()
	for iterator.Next() {
		converted[iterator.Key().String()] = iterator.Value().Interface()
	}

	return converted, true
}

func openAIRequestValueSlice(value any) ([]any, bool) {
	typedValue, ok := value.([]any)
	if ok {
		return typedValue, true
	}

	reflectedValue := reflect.ValueOf(value)
	if !reflectedValue.IsValid() {
		return nil, false
	}

	if reflectedValue.Kind() != reflect.Array && reflectedValue.Kind() != reflect.Slice {
		return nil, false
	}

	converted := make([]any, 0, reflectedValue.Len())
	for index := range reflectedValue.Len() {
		converted = append(converted, reflectedValue.Index(index).Interface())
	}

	return converted, true
}

func openAIDegradedFunctionIDs(responseBody []byte) []string {
	matches := openAIDegradedFunctionIDPattern.FindAllStringSubmatch(string(responseBody), -1)
	if len(matches) == 0 {
		return nil
	}

	functionIDs := make([]string, 0, len(matches))

	seenFunctionIDs := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if len(match) < openAIDegradedFunctionIDMatchParts {
			continue
		}

		functionID := strings.TrimSpace(match[1])
		if functionID == "" {
			continue
		}

		if _, seen := seenFunctionIDs[functionID]; seen {
			continue
		}

		seenFunctionIDs[functionID] = struct{}{}
		functionIDs = append(functionIDs, functionID)
	}

	return functionIDs
}

func addOpenAIExcludedFunctionIDs(excludedFunctionIDs map[string]struct{}, functionIDs []string) bool {
	changed := false

	for _, functionID := range functionIDs {
		trimmedFunctionID := strings.TrimSpace(functionID)
		if trimmedFunctionID == "" {
			continue
		}

		if _, found := excludedFunctionIDs[trimmedFunctionID]; found {
			continue
		}

		excludedFunctionIDs[trimmedFunctionID] = struct{}{}
		changed = true
	}

	return changed
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

func openAIRequestMapReferencesExcludedFunction(
	object map[string]any,
	excludedFunctionIDs map[string]struct{},
) bool {
	for key, child := range object {
		if openAIRequestFieldReferencesExcludedFunction(key, child, excludedFunctionIDs) {
			return true
		}
	}

	return false
}

func openAIRequestFieldReferencesExcludedFunction(
	key string,
	child any,
	excludedFunctionIDs map[string]struct{},
) bool {
	if isOpenAIFunctionIDField(key) {
		if _, found := excludedFunctionIDs[strings.TrimSpace(stringifyValue(child))]; found {
			return true
		}
	}

	return openAIRequestValueReferencesExcludedFunction(child, excludedFunctionIDs)
}

func openAIRequestSliceReferencesExcludedFunction(
	values []any,
	excludedFunctionIDs map[string]struct{},
) bool {
	for _, child := range values {
		if openAIRequestValueReferencesExcludedFunction(child, excludedFunctionIDs) {
			return true
		}
	}

	return false
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
		Usage   *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}

	var envelope streamEnvelope

	err := json.Unmarshal(payload, &envelope)
	if err != nil {
		return streamDelta{
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

func openAIStreamUsage(usage *struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}) *tokenUsage {
	if usage == nil {
		return nil
	}

	return &tokenUsage{
		Input:  usage.PromptTokens,
		Output: usage.CompletionTokens,
	}
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

	return fmt.Errorf("%s: %w", strings.Join(messageParts, " "), os.ErrInvalid)
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
