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

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatCompletionRequest struct {
	Provider providerRequestConfig
	Model    string
	Messages []chatMessage
}

type providerRequestConfig struct {
	APIKind      providerAPIKind
	BaseURL      string
	APIKey       string
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
	httpRequest.Header.Set("Authorization", "Bearer "+openAIAPIKey(request.Provider.APIKey))
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

		return fmt.Errorf(
			"chat completion request failed with status %d: %s: %w",
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	err = consumeServerSentEvents(httpResponse.Body, func(payload []byte) error {
		return handleStreamPayload(payload, handle)
	})
	if err != nil {
		return fmt.Errorf("consume chat completion stream: %w", err)
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

func consumeServerSentEvents(reader io.Reader, handle func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, sseScannerInitialBuffer), sseScannerMaxBuffer)

	var eventData strings.Builder

	flushEvent := func() error {
		if eventData.Len() == 0 {
			return nil
		}

		payload := eventData.String()
		eventData.Reset()

		if payload == "[DONE]" {
			return nil
		}

		return handle([]byte(payload))
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			err := flushEvent()
			if err != nil {
				return err
			}

			continue
		}

		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return nil
		}

		if eventData.Len() > 0 {
			eventData.WriteByte('\n')
		}

		eventData.WriteString(payload)
	}

	err := scanner.Err()
	if err != nil {
		return fmt.Errorf("scan server-sent events: %w", err)
	}

	err = flushEvent()
	if err != nil {
		return fmt.Errorf("flush server-sent events: %w", err)
	}

	return nil
}

func handleStreamPayload(payload []byte, handle func(streamDelta) error) error {
	type streamChoiceDelta struct {
		Content string `json:"content"`
	}

	type streamChoice struct {
		Delta        streamChoiceDelta `json:"delta"`
		FinishReason *string           `json:"finish_reason"`
	}

	type streamEnvelope struct {
		Choices []streamChoice `json:"choices"`
	}

	var envelope streamEnvelope

	err := json.Unmarshal(payload, &envelope)
	if err != nil {
		return fmt.Errorf("decode stream payload: %w", err)
	}

	if len(envelope.Choices) == 0 {
		return nil
	}

	delta := streamDelta{
		Content:      envelope.Choices[0].Delta.Content,
		FinishReason: "",
	}
	if envelope.Choices[0].FinishReason != nil {
		delta.FinishReason = *envelope.Choices[0].FinishReason
	}

	if delta.Content == "" && delta.FinishReason == "" {
		return nil
	}

	err = handle(delta)
	if err != nil {
		return fmt.Errorf("handle stream delta: %w", err)
	}

	return nil
}
