package main

import (
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

	"github.com/bwmarrin/discordgo"
)

const (
	xAIProviderName                    = "x-ai"
	xAIResponsesRequestBodyBaseFields  = 4
	xAIResponsesStreamEventCompleted   = "response.completed"
	xAIResponsesStreamEventError       = "error"
	xAIResponsesStreamEventFailed      = "response.failed"
	xAIResponsesStreamEventIncomplete  = "response.incomplete"
	xAIResponsesStreamEventOutputDelta = "response.output_text.delta"
	xAIResponsesImageDetailAuto        = "auto"
	xAIResponsesInputImageType         = "input_image"
	xAIResponsesInputTextType          = "input_text"
	xAIResponsesStatusCompleted        = "completed"
)

type xAIResponsesUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type xAIResponsesError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

type xAIResponsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

type xAIResponsesStreamResponse struct {
	ID                string                         `json:"id"`
	Status            string                         `json:"status"`
	Usage             *xAIResponsesUsage             `json:"usage"`
	Error             *xAIResponsesError             `json:"error"`
	IncompleteDetails *xAIResponsesIncompleteDetails `json:"incomplete_details"`
}

type xAIResponsesStreamEvent struct {
	Type     string                      `json:"type"`
	Delta    string                      `json:"delta"`
	Message  string                      `json:"message"`
	Code     any                         `json:"code"`
	Error    *xAIResponsesError          `json:"error"`
	Response *xAIResponsesStreamResponse `json:"response"`
}

func providerUsesResponsesAPI(providerName string, provider providerConfig) bool {
	if provider.apiKind() != providerAPIKindOpenAI {
		return false
	}

	if strings.EqualFold(strings.TrimSpace(providerName), xAIProviderName) {
		return true
	}

	parsedURL, err := url.Parse(strings.TrimSpace(provider.BaseURL))
	if err != nil {
		return false
	}

	host := strings.ToLower(strings.TrimSpace(parsedURL.Hostname()))

	return host == "api.x.ai" || strings.HasSuffix(host, ".x.ai")
}

func assignXAIPreviousResponseID(
	request *chatCompletionRequest,
	sourceMessage *discordgo.Message,
	store *messageNodeStore,
	maxMessages int,
) {
	if request == nil || !request.Provider.UseResponsesAPI {
		return
	}

	previousResponseID := xAIConversationPreviousResponseID(
		request.ConfiguredModel,
		sourceMessage,
		store,
		maxMessages,
	)
	if previousResponseID == "" {
		return
	}

	continuationMessages, ok := xAIContinuationMessages(request.Messages)
	if !ok {
		return
	}

	request.PreviousResponseID = previousResponseID
	request.Messages = continuationMessages
}

func xAIConversationPreviousResponseID(
	configuredModel string,
	sourceMessage *discordgo.Message,
	store *messageNodeStore,
	maxMessages int,
) string {
	if sourceMessage == nil || store == nil {
		return ""
	}

	if maxMessages <= 0 {
		maxMessages = 1
	}

	currentMessage := sourceMessage

	for step := 0; currentMessage != nil && step < maxMessages; step++ {
		currentMessageID := strings.TrimSpace(currentMessage.ID)
		if currentMessageID == "" {
			break
		}

		node, ok := store.get(currentMessageID)
		if !ok || node == nil {
			break
		}

		node.mu.Lock()
		role := strings.TrimSpace(node.role)
		providerResponseID := strings.TrimSpace(node.providerResponseID)
		providerResponseModel := strings.TrimSpace(node.providerResponseModel)
		parentMessage := node.parentMessage
		node.mu.Unlock()

		if role == messageRoleAssistant {
			if providerResponseID != "" &&
				strings.EqualFold(providerResponseModel, strings.TrimSpace(configuredModel)) {
				return providerResponseID
			}

			return ""
		}

		currentMessage = parentMessage
	}

	return ""
}

func xAIContinuationMessages(messages []chatMessage) ([]chatMessage, bool) {
	_, conversationMessages := splitLeadingSystemMessages(messages)
	if len(conversationMessages) == 0 {
		return nil, false
	}

	lastAssistantIndex := -1

	for index := len(conversationMessages) - 1; index >= 0; index-- {
		if conversationMessages[index].Role != messageRoleAssistant {
			continue
		}

		lastAssistantIndex = index

		break
	}

	if lastAssistantIndex < 0 || lastAssistantIndex == len(conversationMessages)-1 {
		return nil, false
	}

	return append([]chatMessage(nil), conversationMessages[lastAssistantIndex+1:]...), true
}

func (client openAIClient) streamResponses(
	ctx context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		return fmt.Errorf("build xAI responses request body: %w", err)
	}

	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("marshal xAI responses request: %w", err)
	}

	requestURL, err := buildXAIResponsesURL(request.Provider.BaseURL, request.Provider.ExtraQuery)
	if err != nil {
		return fmt.Errorf("build xAI responses url: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		requestURL,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return fmt.Errorf("create xAI responses request: %w", err)
	}

	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("Authorization", "Bearer "+openAIAPIKey(request.Provider.primaryAPIKey()))
	httpRequest.Header.Set("Content-Type", "application/json")

	for key, value := range request.Provider.ExtraHeaders {
		httpRequest.Header.Set(key, stringifyValue(value))
	}

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("send xAI responses request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return fmt.Errorf(
				"read xAI responses error response after status %d: %w",
				httpResponse.StatusCode,
				readErr,
			)
		}

		return newOpenAIProviderStatusError(
			"xAI responses request failed",
			httpResponse.StatusCode,
			httpResponse.Status,
			httpResponse.Header.Clone(),
			responseBody,
			false,
		)
	}

	terminalEventSeen := false

	_, err = consumeServerSentEvents(httpResponse.Body, func(payload []byte) error {
		terminal, payloadErr := handleXAIResponsesStreamPayload(payload, handle)
		if terminal {
			terminalEventSeen = true
		}

		return payloadErr
	})
	if err != nil {
		return fmt.Errorf("consume xAI responses stream: %w", err)
	}

	if !terminalEventSeen {
		return fmt.Errorf(
			"xAI responses stream ended before response.completed: %w",
			io.ErrUnexpectedEOF,
		)
	}

	return nil
}

func buildXAIResponsesRequestBody(request chatCompletionRequest) (map[string]any, error) {
	input, err := xAIResponsesInput(request.Messages)
	if err != nil {
		return nil, err
	}

	requestBody := make(map[string]any, len(request.Provider.ExtraBody)+xAIResponsesRequestBodyBaseFields)
	requestBody["model"] = request.Model
	requestBody["stream"] = true
	requestBody["input"] = input

	if strings.TrimSpace(request.PreviousResponseID) != "" {
		requestBody["previous_response_id"] = request.PreviousResponseID
	}

	maps.Copy(requestBody, request.Provider.ExtraBody)

	return requestBody, nil
}

func xAIResponsesInput(messages []chatMessage) ([]map[string]any, error) {
	input := make([]map[string]any, 0, len(messages))

	for index, message := range messages {
		convertedMessage, ok, err := xAIResponsesMessage(message)
		if err != nil {
			return nil, fmt.Errorf("convert xAI input message %d: %w", index, err)
		}

		if !ok {
			continue
		}

		input = append(input, convertedMessage)
	}

	return input, nil
}

func xAIResponsesMessage(message chatMessage) (map[string]any, bool, error) {
	role := strings.TrimSpace(message.Role)
	if role == "" {
		return nil, false, nil
	}

	switch role {
	case openAICodexRoleSystem, "developer":
		content, ok, err := xAIResponsesTextContent(message.Content)
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
	case messageRoleAssistant:
		content, ok, err := xAIResponsesTextContent(message.Content)
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
	case messageRoleUser:
		content, ok, err := xAIResponsesUserContent(message.Content)
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
	default:
		return nil, false, fmt.Errorf("unsupported xAI chat role %q: %w", role, os.ErrInvalid)
	}
}

func xAIResponsesTextContent(content any) (string, bool, error) {
	switch typedContent := content.(type) {
	case nil:
		return "", false, nil
	case string:
		if strings.TrimSpace(typedContent) == "" {
			return "", false, nil
		}

		return typedContent, true, nil
	case []contentPart:
		if contentPartsContainNonText(typedContent) {
			return "", false, fmt.Errorf("unsupported xAI text content type %T: %w", content, os.ErrInvalid)
		}

		textContent := contentPartsText(typedContent)
		if strings.TrimSpace(textContent) == "" {
			return "", false, nil
		}

		return textContent, true, nil
	default:
		return "", false, fmt.Errorf("unsupported xAI text content type %T: %w", content, os.ErrInvalid)
	}
}

func xAIResponsesUserContent(content any) (any, bool, error) {
	switch typedContent := content.(type) {
	case nil:
		return "", false, nil
	case string:
		if strings.TrimSpace(typedContent) == "" {
			return "", false, nil
		}

		return typedContent, true, nil
	case []contentPart:
		if !contentPartsContainNonText(typedContent) {
			textContent := contentPartsText(typedContent)
			if strings.TrimSpace(textContent) == "" {
				return "", false, nil
			}

			return textContent, true, nil
		}

		parts := make([]map[string]any, 0, len(typedContent))
		for _, part := range typedContent {
			convertedPart, ok, err := xAIResponsesUserPart(part)
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
		return nil, false, fmt.Errorf("unsupported xAI user content type %T: %w", content, os.ErrInvalid)
	}
}

func xAIResponsesUserPart(part contentPart) (map[string]any, bool, error) {
	partType, _ := part["type"].(string)

	switch partType {
	case contentTypeText:
		textValue, _ := part["text"].(string)
		if strings.TrimSpace(textValue) == "" {
			return nil, false, nil
		}

		return map[string]any{
			"type": xAIResponsesInputTextType,
			"text": textValue,
		}, true, nil
	case contentTypeImageURL:
		imageURL, err := geminiImageURL(part)
		if err != nil {
			return nil, false, err
		}

		if imageURL == "" {
			return nil, false, nil
		}

		return map[string]any{
			"type":      xAIResponsesInputImageType,
			"image_url": imageURL,
			"detail":    xAIResponsesImageDetailAuto,
		}, true, nil
	default:
		return nil, false, fmt.Errorf("unsupported xAI content part type %q: %w", partType, os.ErrInvalid)
	}
}

func buildXAIResponsesURL(baseURL string, extraQuery map[string]any) (string, error) {
	parsedURL, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse xAI base url %q: %w", baseURL, err)
	}

	parsedURL.Path = path.Join(parsedURL.Path, "responses")

	queryValues := parsedURL.Query()
	for key, value := range extraQuery {
		queryValues.Set(key, stringifyValue(value))
	}

	parsedURL.RawQuery = queryValues.Encode()

	return parsedURL.String(), nil
}

func handleXAIResponsesStreamPayload(
	payload []byte,
	handle func(streamDelta) error,
) (bool, error) {
	delta, terminal, err := xAIResponsesStreamPayloadDelta(payload)
	if err != nil {
		return terminal, err
	}

	if delta.Content == "" &&
		delta.FinishReason == "" &&
		delta.Usage == nil &&
		delta.ProviderResponseID == "" {
		return terminal, nil
	}

	err = handle(delta)
	if err != nil {
		return terminal, fmt.Errorf(handleStreamDeltaErrorFormat, err)
	}

	return terminal, nil
}

func xAIResponsesStreamPayloadDelta(payload []byte) (streamDelta, bool, error) {
	var event xAIResponsesStreamEvent

	err := json.Unmarshal(payload, &event)
	if err != nil {
		return streamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       "",
			Usage:              nil,
			ProviderResponseID: "",
		}, false, fmt.Errorf("decode xAI responses stream payload: %w", err)
	}

	eventType := strings.TrimSpace(event.Type)

	switch eventType {
	case xAIResponsesStreamEventOutputDelta:
		return streamDelta{
			Thinking:           "",
			Content:            event.Delta,
			FinishReason:       "",
			Usage:              nil,
			ProviderResponseID: "",
		}, false, nil
	case xAIResponsesStreamEventCompleted:
		delta, completedErr := xAIResponsesCompletedDelta(event.Response)

		return delta, true, completedErr
	case xAIResponsesStreamEventFailed, xAIResponsesStreamEventIncomplete:
		return streamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       "",
			Usage:              nil,
			ProviderResponseID: "",
		}, true, xAIResponsesTerminalError(eventType, event)
	case xAIResponsesStreamEventError:
		if event.Error != nil {
			return streamDelta{
					Thinking:           "",
					Content:            "",
					FinishReason:       "",
					Usage:              nil,
					ProviderResponseID: "",
				}, true, openAIStreamEventError(
					event.Error.Message,
					event.Error.Type,
					event.Error.Code,
				)
		}

		return streamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       "",
			Usage:              nil,
			ProviderResponseID: "",
		}, true, openAIStreamEventError(event.Message, eventType, event.Code)
	default:
		return streamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       "",
			Usage:              nil,
			ProviderResponseID: "",
		}, false, nil
	}
}

func xAIResponsesCompletedDelta(response *xAIResponsesStreamResponse) (streamDelta, error) {
	if response == nil {
		return streamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       finishReasonStop,
			Usage:              nil,
			ProviderResponseID: "",
		}, nil
	}

	if response.Error != nil {
		return streamDelta{
				Thinking:           "",
				Content:            "",
				FinishReason:       "",
				Usage:              nil,
				ProviderResponseID: "",
			}, openAIStreamEventError(
				response.Error.Message,
				response.Error.Type,
				response.Error.Code,
			)
	}

	status := strings.TrimSpace(response.Status)
	if status != "" && !strings.EqualFold(status, xAIResponsesStatusCompleted) {
		reason := ""
		if response.IncompleteDetails != nil {
			reason = strings.TrimSpace(response.IncompleteDetails.Reason)
		}

		return streamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       "",
			Usage:              nil,
			ProviderResponseID: "",
		}, xAIResponsesStatusError(status, reason)
	}

	return streamDelta{
		Thinking:           "",
		Content:            "",
		FinishReason:       finishReasonStop,
		Usage:              xAIResponsesTokenUsage(response.Usage),
		ProviderResponseID: strings.TrimSpace(response.ID),
	}, nil
}

func xAIResponsesTerminalError(eventType string, event xAIResponsesStreamEvent) error {
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

		return xAIResponsesStatusError(status, reason)
	}

	return openAIStreamEventError(event.Message, eventType, event.Code)
}

func xAIResponsesStatusError(status string, reason string) error {
	message := "provider ended the response with status=" + status
	if reason != "" {
		message += " reason=" + reason
	}

	return fmt.Errorf("%s: %w", message, os.ErrInvalid)
}

func xAIResponsesTokenUsage(usage *xAIResponsesUsage) *tokenUsage {
	if usage == nil {
		return nil
	}

	inputTokens := usage.InputTokens
	if inputTokens == 0 {
		inputTokens = usage.PromptTokens
	}

	outputTokens := usage.OutputTokens
	if outputTokens == 0 {
		outputTokens = usage.CompletionTokens
	}

	return &tokenUsage{
		Input:  inputTokens,
		Output: outputTokens,
	}
}
