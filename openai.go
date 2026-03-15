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
	"strings"
)

const (
	openAIStreamErrorEventType           = "error"
	openAIStreamMessagePartsCapacity     = 3
	openAIContentFilterFinishReason      = "content_filter"
	openAIStreamToolCallsFinishReason    = "tool_calls"
	openAIStreamFunctionCallFinishReason = "function_call"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatCompletionRequest struct {
	Provider        providerRequestConfig
	Model           string
	ConfiguredModel string
	Messages        []chatMessage
}

type providerRequestConfig struct {
	APIKind      providerAPIKind
	BaseURL      string
	APIKey       string
	APIKeys      []string
	ExtraHeaders map[string]any
	ExtraQuery   map[string]any
	ExtraBody    map[string]any
}

type streamDelta struct {
	Content      string
	FinishReason string
}

type openAIClient struct {
	httpClient *http.Client
}

func newOpenAIClient(httpClient *http.Client) openAIClient {
	return openAIClient{httpClient: httpClient}
}

func (client openAIClient) streamChatCompletion(
	ctx context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	requestBody := buildChatCompletionRequestBody(request)

	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("marshal chat completion request: %w", err)
	}

	requestURL, err := buildChatCompletionURL(request.Provider.BaseURL, request.Provider.ExtraQuery)
	if err != nil {
		return fmt.Errorf("build chat completion url: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		requestURL,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return fmt.Errorf("create chat completion request: %w", err)
	}

	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("Authorization", "Bearer "+openAIAPIKey(request.Provider.primaryAPIKey()))
	httpRequest.Header.Set("Content-Type", "application/json")

	for key, value := range request.Provider.ExtraHeaders {
		httpRequest.Header.Set(key, stringifyValue(value))
	}

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("send chat completion request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return fmt.Errorf(
				"read chat completion error response after status %d: %w",
				httpResponse.StatusCode,
				readErr,
			)
		}

		return providerStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"chat completion request failed with status %d: %s",
				httpResponse.StatusCode,
				strings.TrimSpace(string(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	doneSeen, err := consumeServerSentEvents(httpResponse.Body, func(payload []byte) error {
		return handleStreamPayload(payload, handle)
	})
	if err != nil {
		return fmt.Errorf("consume chat completion stream: %w", err)
	}

	if !doneSeen {
		return fmt.Errorf("chat completion stream ended before [DONE]: %w", io.ErrUnexpectedEOF)
	}

	return nil
}

func buildChatCompletionRequestBody(request chatCompletionRequest) map[string]any {
	requestBody := make(map[string]any, len(request.Provider.ExtraBody)+requestBodyBaseFields)
	requestBody["messages"] = request.Messages
	requestBody["model"] = request.Model
	requestBody["stream"] = true

	maps.Copy(requestBody, request.Provider.ExtraBody)

	return requestBody
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
		if strings.TrimSpace(line) == "" {
			err := flushEvent()
			if err != nil {
				return doneSeen, err
			}

			continue
		}

		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			doneSeen = true

			return doneSeen, nil
		}

		if eventData.Len() > 0 {
			eventData.WriteByte('\n')
		}

		eventData.WriteString(payload)
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

func handleStreamPayload(payload []byte, handle func(streamDelta) error) error {
	delta, err := openAIStreamPayloadDelta(payload)
	if err != nil {
		return err
	}

	if delta.Content != "" {
		err = handle(streamDelta{Content: delta.Content, FinishReason: ""})
		if err != nil {
			return fmt.Errorf("handle stream delta: %w", err)
		}
	}

	if delta.FinishReason != "" {
		err = openAIStreamFinishReasonError(delta.FinishReason)
		if err != nil {
			return err
		}

		err = handle(streamDelta{Content: "", FinishReason: delta.FinishReason})
		if err != nil {
			return fmt.Errorf("handle stream delta: %w", err)
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
	}

	var envelope streamEnvelope

	err := json.Unmarshal(payload, &envelope)
	if err != nil {
		return streamDelta{Content: "", FinishReason: ""}, fmt.Errorf("decode stream payload: %w", err)
	}

	if envelope.Error != nil {
		return streamDelta{Content: "", FinishReason: ""}, openAIStreamEventError(
			envelope.Error.Message,
			envelope.Error.Type,
			envelope.Error.Code,
		)
	}

	if len(envelope.Choices) == 0 {
		return streamDelta{Content: "", FinishReason: ""}, nil
	}

	delta := streamDelta{
		Content:      envelope.Choices[0].Delta.Content,
		FinishReason: "",
	}
	if envelope.Choices[0].FinishReason != nil {
		delta.FinishReason = strings.TrimSpace(*envelope.Choices[0].FinishReason)
	}

	return delta, nil
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
	case "", finishReasonStop, "end_turn", "length":
		return nil
	case openAIContentFilterFinishReason:
		return fmt.Errorf("provider blocked the response (finish_reason=%s): %w", finishReason, os.ErrInvalid)
	case openAIStreamToolCallsFinishReason, openAIStreamFunctionCallFinishReason, openAIStreamErrorEventType:
		return fmt.Errorf("provider ended the stream with finish_reason=%s: %w", finishReason, os.ErrInvalid)
	default:
		return nil
	}
}
