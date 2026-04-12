package main

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
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
)

var (
	xAISourceAppendixNumberedLinePattern = regexp.MustCompile(`^\d+\.\s+(.*)$`)
	xAISourceAppendixMarkdownLinkPattern = regexp.MustCompile(`^\[(.+?)\]\((https?://[^)]+)\)(.*)$`)
	xAISourceAppendixInlineQueryPattern  = regexp.MustCompile("`([^`]+)`")
)

const (
	xAIProviderName                    = "x-ai"
	xAIResponsesRequestBodyBaseFields  = 4
	xAIResponsesStreamEventCompleted   = "response.completed"
	xAIResponsesStreamEventError       = "error"
	xAIResponsesStreamEventFailed      = "response.failed"
	xAIResponsesStreamEventIncomplete  = "response.incomplete"
	xAIResponsesStreamEventOutputDone  = "response.output_item.done"
	xAIResponsesStreamEventOutputDelta = "response.output_text.delta"
	xAIResponsesImageDetailAuto        = "auto"
	xAIResponsesInputFileType          = "input_file"
	xAIResponsesInputImageType         = "input_image"
	xAIResponsesInputTextType          = "input_text"
	xAIResponsesOutputTypeImage        = "image_generation_call"
	xAIResponsesStatusCompleted        = "completed"
	xAIMarkdownLinkMatchParts          = 4
	xAINumberedLineMatchParts          = 2
	xAISourceAppendixHeader            = "Sources\n"
	xAISourceAppendixParagraphHeader   = "\n\nSources\n"
	xAISourceQueriesAppendixSeparator  = "\n\nSearch Queries\n"
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

type xAIResponsesOutputItem struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	Result        string `json:"result"`
	ResultURL     string `json:"result_url"`
	MIMEType      string `json:"mime_type"`
	Action        string `json:"action"`
	Prompt        string `json:"prompt"`
	RevisedPrompt string `json:"revised_prompt"`
}

type xAISourceAttribution struct {
	Sources       []xAISourceAttributionSource `json:"sources"`
	SearchQueries []string                     `json:"search_queries"`
}

type xAISourceAttributionSource struct {
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	SearchQueries []string `json:"search_queries"`
}

type xAIResponsesStreamResponse struct {
	ID                string                         `json:"id"`
	Status            string                         `json:"status"`
	Output            []xAIResponsesOutputItem       `json:"output"`
	Usage             *xAIResponsesUsage             `json:"usage"`
	Error             *xAIResponsesError             `json:"error"`
	IncompleteDetails *xAIResponsesIncompleteDetails `json:"incomplete_details"`
	SourceAttribution *xAISourceAttribution          `json:"source_attribution"`
}

type xAIResponsesStreamEvent struct {
	Type              string                      `json:"type"`
	Delta             string                      `json:"delta"`
	Message           string                      `json:"message"`
	Code              any                         `json:"code"`
	Item              *xAIResponsesOutputItem     `json:"item"`
	Error             *xAIResponsesError          `json:"error"`
	Response          *xAIResponsesStreamResponse `json:"response"`
	SourceAttribution *xAISourceAttribution       `json:"source_attribution"`
}

type xAIResponsesStreamState struct {
	seenOutputItemIDs  map[string]struct{}
	seenOutputItemURLs map[string]struct{}
	hasVisibleContent  bool
}

func providerUsesResponsesAPI(providerName string, provider providerConfig) bool {
	if provider.apiKind() != providerAPIKindOpenAI {
		return false
	}

	if strings.EqualFold(strings.TrimSpace(providerName), xAIProviderName) {
		return true
	}

	return xAIBaseURLUsesOfficialAPI(provider.BaseURL)
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
	streamState := newXAIResponsesStreamState()

	_, err = consumeServerSentEvents(httpResponse.Body, func(payload []byte) error {
		terminal, payloadErr := handleXAIResponsesStreamPayload(payload, handle, streamState)
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
	input, err := xAIResponsesInput(
		requestMessagesWithFileOrImageOnlyQueryPlaceholder(request.Messages),
	)
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

	if shouldDefaultXAIBridgeSourceAttribution(request) {
		requestBody["source_attribution"] = defaultXAIBridgeSourceAttributionRequest()
	}

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
	case contentTypeDocument:
		documentBytes, mimeType, filename, err := attachmentBinaryData(part)
		if err != nil {
			return nil, false, err
		}

		if len(documentBytes) == 0 {
			return nil, false, nil
		}

		filePart := map[string]any{
			"type": xAIResponsesInputFileType,
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

func xAIBaseURLUsesOfficialAPI(baseURL string) bool {
	parsedURL, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}

	host := strings.ToLower(strings.TrimSpace(parsedURL.Hostname()))

	return host == "api.x.ai" || strings.HasSuffix(host, ".x.ai")
}

func xAIConfiguredModel(configuredModel string) bool {
	providerName, _, err := splitConfiguredModel(strings.TrimSpace(configuredModel))
	if err != nil {
		return false
	}

	return strings.EqualFold(providerName, xAIProviderName)
}

func shouldDefaultXAIBridgeSourceAttribution(request chatCompletionRequest) bool {
	if !xAIConfiguredModel(request.ConfiguredModel) || xAIBaseURLUsesOfficialAPI(request.Provider.BaseURL) {
		return false
	}

	if len(request.Provider.ExtraBody) == 0 {
		return true
	}

	_, exists := request.Provider.ExtraBody["source_attribution"]

	return !exists
}

func defaultXAIBridgeSourceAttributionRequest() map[string]any {
	return map[string]any{
		"inline_citations":       true,
		"include_sources":        true,
		"include_search_queries": true,
	}
}

func handleXAIResponsesStreamPayload(
	payload []byte,
	handle func(streamDelta) error,
	state *xAIResponsesStreamState,
) (bool, error) {
	delta, terminal, err := xAIResponsesStreamPayloadDelta(payload, state)
	if err != nil {
		return terminal, err
	}

	if delta.Content == "" &&
		delta.FinishReason == "" &&
		delta.Usage == nil &&
		delta.ProviderResponseID == "" &&
		len(delta.ResponseImages) == 0 {
		return terminal, nil
	}

	err = handle(delta)
	if err != nil {
		return terminal, fmt.Errorf(handleStreamDeltaErrorFormat, err)
	}

	return terminal, nil
}

func newXAIResponsesStreamState() *xAIResponsesStreamState {
	return &xAIResponsesStreamState{
		seenOutputItemIDs:  make(map[string]struct{}),
		seenOutputItemURLs: make(map[string]struct{}),
		hasVisibleContent:  false,
	}
}

func xAIResponsesStreamPayloadDelta(
	payload []byte,
	state *xAIResponsesStreamState,
) (streamDelta, bool, error) {
	var event xAIResponsesStreamEvent

	emptyDelta := emptyStreamDelta()

	err := json.Unmarshal(payload, &event)
	if err != nil {
		return emptyDelta, false, fmt.Errorf("decode xAI responses stream payload: %w", err)
	}

	eventType := strings.TrimSpace(event.Type)

	switch eventType {
	case xAIResponsesStreamEventOutputDelta:
		if state != nil && event.Delta != "" {
			state.hasVisibleContent = true
		}

		delta := emptyDelta
		delta.Content = event.Delta

		return delta, false, nil
	case xAIResponsesStreamEventOutputDone:
		delta := emptyDelta
		delta.Content = xAIResponsesOutputItemText(event.Item, state, false)
		delta.ResponseImages = xAIResponsesOutputImageAssets(event.Item)

		return delta, false, nil
	case xAIResponsesStreamEventCompleted:
		delta, completedErr := xAIResponsesCompletedDelta(
			event.Response,
			event.SourceAttribution,
			state,
		)

		return delta, true, completedErr
	case xAIResponsesStreamEventFailed, xAIResponsesStreamEventIncomplete:
		return emptyDelta, true, xAIResponsesTerminalError(eventType, event)
	case xAIResponsesStreamEventError:
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

func emptyStreamDelta() streamDelta {
	return streamDelta{
		Thinking:           "",
		Content:            "",
		FinishReason:       "",
		Usage:              nil,
		ProviderResponseID: "",
		SearchMetadata:     nil,
		ResponseImages:     nil,
	}
}

func xAIResponsesCompletedDelta(
	response *xAIResponsesStreamResponse,
	eventSourceAttribution *xAISourceAttribution,
	state *xAIResponsesStreamState,
) (streamDelta, error) {
	if response == nil {
		return streamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       finishReasonStop,
			Usage:              nil,
			ProviderResponseID: "",
			SearchMetadata:     xAISourceAttributionSearchMetadata(eventSourceAttribution),
			ResponseImages:     nil,
		}, nil
	}

	if response.Error != nil {
		return streamDelta{
				Thinking:           "",
				Content:            "",
				FinishReason:       "",
				Usage:              nil,
				ProviderResponseID: "",
				SearchMetadata:     nil,
				ResponseImages:     nil,
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
			SearchMetadata:     nil,
			ResponseImages:     nil,
		}, xAIResponsesStatusError(status, reason)
	}

	content := xAIResponsesOutputItemsText(response.Output, state, true)

	return streamDelta{
		Thinking:           "",
		Content:            content,
		FinishReason:       finishReasonStop,
		Usage:              xAIResponsesTokenUsage(response.Usage),
		ProviderResponseID: strings.TrimSpace(response.ID),
		SearchMetadata: xAISourceAttributionSearchMetadata(
			mergeXAISourceAttribution(
				response.SourceAttribution,
				eventSourceAttribution,
			),
		),
		ResponseImages: xAIResponsesOutputImages(response.Output),
	}, nil
}

func xAIResponsesOutputItemsText(
	items []xAIResponsesOutputItem,
	state *xAIResponsesStreamState,
	final bool,
) string {
	if len(items) == 0 {
		return ""
	}

	var builder strings.Builder

	for index := range items {
		builder.WriteString(xAIResponsesOutputItemText(&items[index], state, final))
	}

	return builder.String()
}

func xAIResponsesOutputItemText(
	item *xAIResponsesOutputItem,
	state *xAIResponsesStreamState,
	final bool,
) string {
	if item == nil {
		return ""
	}

	normalizedItem, ok := normalizeXAIResponsesOutputItem(*item)
	if !ok || xAIResponsesOutputItemSeen(state, normalizedItem) {
		return ""
	}

	content := xAIResponsesOutputItemContent(normalizedItem, final)
	if content == "" {
		return ""
	}

	xAIResponsesMarkOutputItemSeen(state, normalizedItem)

	if state != nil {
		if state.hasVisibleContent {
			content = "\n\n" + content
		}

		state.hasVisibleContent = true
	}

	return content
}

func normalizeXAIResponsesOutputItem(item xAIResponsesOutputItem) (xAIResponsesOutputItem, bool) {
	var emptyItem xAIResponsesOutputItem

	item.ID = strings.TrimSpace(item.ID)
	item.Type = strings.TrimSpace(item.Type)
	item.Result = strings.TrimSpace(item.Result)
	item.ResultURL = strings.TrimSpace(item.ResultURL)
	item.MIMEType = strings.TrimSpace(item.MIMEType)
	item.Action = strings.ToLower(strings.TrimSpace(item.Action))

	if !strings.EqualFold(item.Type, xAIResponsesOutputTypeImage) {
		return emptyItem, false
	}

	if item.ResultURL == "" && item.Result == "" {
		return emptyItem, false
	}

	return item, true
}

func xAIResponsesOutputItemContent(item xAIResponsesOutputItem, final bool) string {
	label := xAIResponsesOutputItemLabel(item.Action)

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

func xAIResponsesOutputImages(items []xAIResponsesOutputItem) []responseImageAsset {
	if len(items) == 0 {
		return nil
	}

	images := make([]responseImageAsset, 0, len(items))

	for index := range items {
		images = append(images, xAIResponsesOutputImageAssets(&items[index])...)
	}

	return images
}

func xAIResponsesOutputImageAssets(item *xAIResponsesOutputItem) []responseImageAsset {
	if item == nil {
		return nil
	}

	normalizedItem, ok := normalizeXAIResponsesOutputItem(*item)
	if !ok {
		return nil
	}

	return []responseImageAsset{xAIResponsesOutputImageAsset(normalizedItem)}
}

func xAIResponsesOutputImageAsset(item xAIResponsesOutputItem) responseImageAsset {
	return responseImageAsset{
		ID:          item.ID,
		URL:         item.ResultURL,
		ContentType: item.MIMEType,
		Data:        xAIResponsesOutputImageBytes(item.Result),
	}
}

func xAIResponsesOutputImageBytes(encoded string) []byte {
	trimmed := strings.TrimSpace(encoded)
	if trimmed == "" {
		return nil
	}

	imageBytes, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil
	}

	return imageBytes
}

func xAIResponsesOutputItemLabel(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "edit":
		return "Edited image"
	case "generate":
		return "Generated image"
	default:
		return "Image output"
	}
}

func xAIResponsesOutputItemSeen(state *xAIResponsesStreamState, item xAIResponsesOutputItem) bool {
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

func xAIResponsesMarkOutputItemSeen(state *xAIResponsesStreamState, item xAIResponsesOutputItem) {
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

func xAISourceAttributionSearchMetadata(attribution *xAISourceAttribution) *searchMetadata {
	if attribution == nil {
		return nil
	}

	queries := normalizeSearchQueries(attribution.SearchQueries)
	querySources := make(map[string][]searchSource, len(queries))
	seenURLsByQuery := make(map[string]map[string]struct{}, len(queries))

	for _, query := range queries {
		querySources[query] = nil
		seenURLsByQuery[query] = make(map[string]struct{})
	}

	unscopedSources := make([]searchSource, 0, len(attribution.Sources))
	seenUnscopedURLs := make(map[string]struct{}, len(attribution.Sources))

	for _, rawSource := range attribution.Sources {
		normalizedSource, ok := normalizeXAISourceAttributionSource(rawSource)
		if !ok {
			continue
		}

		source := xAISourceAttributionSearchSource(normalizedSource)

		sourceQueries := normalizedSource.SearchQueries
		if len(sourceQueries) == 0 {
			unscopedSources = appendXAISourceIfUnique(unscopedSources, seenUnscopedURLs, source)

			continue
		}

		for _, query := range sourceQueries {
			if _, ok := querySources[query]; !ok {
				queries = append(queries, query)
				querySources[query] = nil
				seenURLsByQuery[query] = make(map[string]struct{})
			}

			querySources[query] = appendXAISourceIfUnique(
				querySources[query],
				seenURLsByQuery[query],
				source,
			)
		}
	}

	results := make([]webSearchResult, 0, len(queries)+1)

	for _, query := range queries {
		sources := querySources[query]
		if len(sources) == 0 {
			continue
		}

		results = append(results, webSearchResult{
			Query: query,
			Text:  xAISearchSourcesResultText(sources),
		})
	}

	if len(unscopedSources) > 0 {
		results = append(results, webSearchResult{
			Query: "",
			Text:  xAISearchSourcesResultText(unscopedSources),
		})
	}

	if len(queries) == 0 && len(results) == 0 {
		return nil
	}

	maxURLs := len(attribution.Sources)
	if maxURLs == 0 {
		for _, result := range results {
			sourceCount := len(extractSearchSources(result.Text))
			if sourceCount > maxURLs {
				maxURLs = sourceCount
			}
		}
	}

	return &searchMetadata{
		Queries:             queries,
		Results:             results,
		MaxURLs:             maxURLs,
		VisualSearchSources: nil,
	}
}

func finalizeXAIResponseAnswer(
	request chatCompletionRequest,
	answerText string,
	existingMetadata *searchMetadata,
) (string, *searchMetadata) {
	if !xAIConfiguredModel(request.ConfiguredModel) || xAIBaseURLUsesOfficialAPI(request.Provider.BaseURL) {
		return answerText, nil
	}

	cleanedAnswerText, attribution, ok := parseXAIBridgeSourceAttributionAppendix(answerText)
	if !ok {
		return answerText, nil
	}

	if searchMetadataHasWebSources(existingMetadata) {
		return cleanedAnswerText, nil
	}

	return cleanedAnswerText, xAISourceAttributionSearchMetadata(attribution)
}

func xAIStreamingVisibleAnswerText(request chatCompletionRequest, answerText string) string {
	if !xAIConfiguredModel(request.ConfiguredModel) || xAIBaseURLUsesOfficialAPI(request.Provider.BaseURL) {
		return answerText
	}

	appendixStart, ok := xAIStreamingSourceAppendixStart(answerText)
	if !ok {
		return answerText
	}

	return strings.TrimRight(answerText[:appendixStart], "\n")
}

func xAIStreamingSourceAppendixStart(answerText string) (int, bool) {
	if answerText == "" {
		return 0, false
	}

	if appendixStart, ok := xAIStreamingSourceAppendixMarkerStart(
		answerText,
		xAISourceAppendixParagraphHeader,
		false,
	); ok {
		return appendixStart, true
	}

	if appendixStart, ok := xAIStreamingSourceAppendixMarkerStart(
		answerText,
		xAISourceAppendixHeader,
		true,
	); ok {
		return appendixStart, true
	}

	return 0, false
}

func xAIStreamingSourceAppendixMarkerStart(
	answerText string,
	marker string,
	atStartOnly bool,
) (int, bool) {
	if strings.HasPrefix(answerText, marker) {
		return 0, true
	}

	if atStartOnly {
		if len(answerText) < len(marker) && strings.HasPrefix(marker, answerText) {
			return 0, true
		}

		return 0, false
	}

	if appendixStart := strings.LastIndex(answerText, marker); appendixStart >= 0 {
		return appendixStart, true
	}

	maxPrefixLength := minInt(len(marker)-1, len(answerText))
	for prefixLength := maxPrefixLength; prefixLength > 0; prefixLength-- {
		suffixStart := len(answerText) - prefixLength
		if strings.HasPrefix(marker, answerText[suffixStart:]) {
			return suffixStart, true
		}
	}

	return 0, false
}

func mergeXAISourceAttribution(
	left *xAISourceAttribution,
	right *xAISourceAttribution,
) *xAISourceAttribution {
	switch {
	case left == nil:
		return cloneXAISourceAttribution(right)
	case right == nil:
		return cloneXAISourceAttribution(left)
	}

	merged := cloneXAISourceAttribution(left)
	merged.SearchQueries = normalizeSearchQueries(append(merged.SearchQueries, right.SearchQueries...))

	seenURLs := make(map[string]int, len(merged.Sources))
	for index, source := range merged.Sources {
		foldedURL := strings.ToLower(strings.TrimSpace(source.URL))
		if foldedURL == "" {
			continue
		}

		seenURLs[foldedURL] = index
	}

	for _, source := range right.Sources {
		normalizedSource, ok := normalizeXAISourceAttributionSource(source)
		if !ok {
			continue
		}

		foldedURL := strings.ToLower(normalizedSource.URL)
		if existingIndex, ok := seenURLs[foldedURL]; ok {
			existingSource := merged.Sources[existingIndex]
			if strings.TrimSpace(existingSource.Title) == "" {
				existingSource.Title = normalizedSource.Title
			}

			existingSource.SearchQueries = normalizeSearchQueries(
				append(existingSource.SearchQueries, normalizedSource.SearchQueries...),
			)
			merged.Sources[existingIndex] = existingSource

			continue
		}

		seenURLs[foldedURL] = len(merged.Sources)
		merged.Sources = append(merged.Sources, normalizedSource)
	}

	return merged
}

func cloneXAISourceAttribution(attribution *xAISourceAttribution) *xAISourceAttribution {
	if attribution == nil {
		return nil
	}

	cloned := new(xAISourceAttribution)

	cloned.SearchQueries = append([]string(nil), attribution.SearchQueries...)
	cloned.Sources = make([]xAISourceAttributionSource, 0, len(attribution.Sources))

	for _, source := range attribution.Sources {
		cloned.Sources = append(cloned.Sources, xAISourceAttributionSource{
			Title:         source.Title,
			URL:           source.URL,
			SearchQueries: append([]string(nil), source.SearchQueries...),
		})
	}

	return cloned
}

func normalizeXAISourceAttributionSource(
	source xAISourceAttributionSource,
) (xAISourceAttributionSource, bool) {
	var emptySource xAISourceAttributionSource

	source.URL = strings.TrimSpace(source.URL)
	source.Title = strings.TrimSpace(source.Title)
	source.SearchQueries = normalizeSearchQueries(source.SearchQueries)

	if source.URL == "" {
		return emptySource, false
	}

	return source, true
}

func xAISourceAttributionSearchSource(source xAISourceAttributionSource) searchSource {
	return searchSource{
		Title: source.Title,
		URL:   source.URL,
	}
}

func appendXAISourceIfUnique(
	sources []searchSource,
	seenURLs map[string]struct{},
	source searchSource,
) []searchSource {
	foldedURL := strings.ToLower(strings.TrimSpace(source.URL))
	if foldedURL == "" {
		return sources
	}

	if _, ok := seenURLs[foldedURL]; ok {
		return sources
	}

	seenURLs[foldedURL] = struct{}{}

	return append(sources, source)
}

func xAISearchSourcesResultText(sources []searchSource) string {
	var builder strings.Builder

	for index, source := range sources {
		if index > 0 {
			builder.WriteString("\n\n")
		}

		title := strings.TrimSpace(source.Title)
		if title != "" {
			builder.WriteString("Title: ")
			builder.WriteString(title)
			builder.WriteString("\n")
		}

		builder.WriteString("URL: ")
		builder.WriteString(strings.TrimSpace(source.URL))
		builder.WriteString("\n")
	}

	return builder.String()
}

func parseXAIBridgeSourceAttributionAppendix(
	answerText string,
) (string, *xAISourceAttribution, bool) {
	normalizedAnswerText := strings.ReplaceAll(answerText, "\r\n", "\n")

	appendixStart := strings.LastIndex(normalizedAnswerText, xAISourceAppendixParagraphHeader)
	if appendixStart < 0 {
		if strings.HasPrefix(normalizedAnswerText, xAISourceAppendixHeader) {
			appendixStart = 0
		} else {
			return answerText, nil, false
		}
	}

	cleanedAnswerText := strings.TrimSpace(normalizedAnswerText[:appendixStart])

	appendix := strings.TrimLeft(normalizedAnswerText[appendixStart:], "\n")
	if !strings.HasPrefix(appendix, xAISourceAppendixHeader) {
		return answerText, nil, false
	}

	sourcesSection := strings.TrimPrefix(appendix, xAISourceAppendixHeader)
	queriesSection := ""

	if sourcePart, queryPart, found := strings.Cut(sourcesSection, xAISourceQueriesAppendixSeparator); found {
		sourcesSection = sourcePart
		queriesSection = queryPart
	}

	attribution := &xAISourceAttribution{
		Sources:       parseXAIBridgeSourcesSection(sourcesSection),
		SearchQueries: parseXAIBridgeQueriesSection(queriesSection),
	}

	if len(attribution.Sources) == 0 && len(attribution.SearchQueries) == 0 {
		return answerText, nil, false
	}

	if cleanedAnswerText == "" {
		return answerText, attribution, true
	}

	return cleanedAnswerText, attribution, true
}

func parseXAIBridgeSourcesSection(section string) []xAISourceAttributionSource {
	lines := strings.Split(strings.TrimSpace(section), "\n")
	sources := make([]xAISourceAttributionSource, 0, len(lines))

	for _, line := range lines {
		lineText, parsed := parseXAIBridgeNumberedLine(line)
		if !parsed {
			continue
		}

		source, parsed := parseXAIBridgeSourceLine(lineText)
		if !parsed {
			continue
		}

		sources = append(sources, source)
	}

	return sources
}

func parseXAIBridgeQueriesSection(section string) []string {
	if strings.TrimSpace(section) == "" {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(section), "\n")
	queries := make([]string, 0, len(lines))

	for _, line := range lines {
		lineText, parsed := parseXAIBridgeNumberedLine(line)
		if !parsed {
			continue
		}

		queries = append(queries, parseXAIBridgeQueryList(lineText)...)
	}

	return normalizeSearchQueries(queries)
}

func parseXAIBridgeNumberedLine(line string) (string, bool) {
	match := xAISourceAppendixNumberedLinePattern.FindStringSubmatch(strings.TrimSpace(line))
	if len(match) != xAINumberedLineMatchParts {
		return "", false
	}

	return strings.TrimSpace(match[1]), true
}

func parseXAIBridgeSourceLine(line string) (xAISourceAttributionSource, bool) {
	var emptySource xAISourceAttributionSource

	match := xAISourceAppendixMarkdownLinkPattern.FindStringSubmatch(strings.TrimSpace(line))
	if len(match) != xAIMarkdownLinkMatchParts {
		return emptySource, false
	}

	source, ok := normalizeXAISourceAttributionSource(xAISourceAttributionSource{
		Title:         strings.TrimSpace(match[1]),
		URL:           strings.TrimSpace(match[2]),
		SearchQueries: parseXAIBridgeSourceQueries(match[3]),
	})
	if !ok {
		return emptySource, false
	}

	return source, true
}

func parseXAIBridgeSourceQueries(remainder string) []string {
	_, queryText, found := strings.Cut(remainder, " via ")
	if !found {
		return nil
	}

	return parseXAIBridgeQueryList(queryText)
}

func parseXAIBridgeQueryList(text string) []string {
	queryMatches := xAISourceAppendixInlineQueryPattern.FindAllStringSubmatch(text, -1)
	if len(queryMatches) > 0 {
		queries := make([]string, 0, len(queryMatches))
		for _, match := range queryMatches {
			if len(match) != xAINumberedLineMatchParts {
				continue
			}

			queries = append(queries, match[1])
		}

		return normalizeSearchQueries(queries)
	}

	trimmedText := strings.Trim(strings.TrimSpace(text), "`")
	if trimmedText == "" {
		return nil
	}

	return normalizeSearchQueries(strings.Split(trimmedText, ";"))
}
