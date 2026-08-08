package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"

	searchtypes "llmcord-go/internal/searchtypes"
	support "llmcord-go/internal/support"
)

var (
	xAISourceAppendixNumberedLinePattern = regexp.MustCompile(`^(?:\d+[\.\)]|\[\d+\]:?|[\-\*\+])\s+(.*)$`)
	xAISourceAppendixMarkdownLinkPattern = regexp.MustCompile(`^\[(.+?)\]\((https?://[^\s)]+)\)(.*)$`)
	xAISourceAppendixTitleURLPattern     = regexp.MustCompile(`^(.+?)\s*[:\-\(]\s*<?(https?://[^\s>)]+)>?\)?(.*)$`)
	xAISourceAppendixBareURLPattern      = regexp.MustCompile(`^<?(https?://[^\s>)]+)>?\s*(.*)$`)
	xAISourceAppendixInlineQueryPattern  = regexp.MustCompile("`([^`]+)`")
)

const (
	sourceAppendixNumberedMatchParts     = 2
	sourceAppendixMarkdownLinkMatchParts = 4
	sourceAppendixTitleURLMatchParts     = 4
	sourceAppendixBareURLMatchParts      = 3
	doubleNewlineSeparatorLength         = 2
)

func sourceAppendixHeaderPrefixesList() []string {
	return []string{
		"sources",
		"source urls",
		"references",
		"citations",
	}
}

const (
	// XAIProviderName is the built-in x-ai provider name.
	XAIProviderName                                  = "x-ai"
	xAIResponsesRequestBodyBaseFields                = 4
	xAIResponsesStreamEventCompleted                 = "response.completed"
	xAIResponsesStreamEventError                     = "error"
	xAIResponsesStreamEventFailed                    = "response.failed"
	xAIResponsesStreamEventIncomplete                = "response.incomplete"
	xAIResponsesStreamEventOutputDone                = "response.output_item.done"
	xAIResponsesStreamEventOutputDelta               = "response.output_text.delta"
	xAIResponsesStreamEventReasoningTextDone         = "response.reasoning_text.done"
	xAIResponsesStreamEventReasoningTextDelta        = "response.reasoning_text.delta"
	xAIResponsesStreamEventReasoningSummaryPartDone  = "response.reasoning_summary_part.done"
	xAIResponsesStreamEventReasoningSummaryTextDelta = "response.reasoning_summary_text.delta"
	xAIResponsesImageDetailAuto                      = "auto"
	xAIResponsesInputFileType                        = "input_file"
	xAIResponsesInputImageType                       = "input_image"
	xAIResponsesInputTextType                        = "input_text"
	xAIResponsesOutputTypeImage                      = "image_generation_call"
	xAIResponsesOutputTypeReasoning                  = "reasoning"
	xAIResponsesReasoningSummaryTextType             = "summary_text"
	xAIResponsesStatusCompleted                      = "completed"
	// XAIResponsesUploadPurposeUserData is the file upload purpose.
	XAIResponsesUploadPurposeUserData = "user_data"
	xAIMarkdownLinkMatchParts         = 4
	xAINumberedLineMatchParts         = 2
	xAISourceAppendixHeader           = "Sources\n"
	xAISourceAppendixParagraphHeader  = "\n\nSources\n"
	xAISourceQueriesAppendixSeparator = "\n\nSearch Queries\n"
	xAIInputImageUploadFilename       = "input-image"
	// XAIInlineImageByteLimit is the inline image upload threshold.
	XAIInlineImageByteLimit = 4 * 1024 * 1024
)

type xAIResponsesError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

type xAIResponsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

type xAIResponsesOutputItem struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Status        string          `json:"status"`
	Result        string          `json:"result"`
	ResultURL     string          `json:"result_url"`
	MIMEType      string          `json:"mime_type"`
	Action        string          `json:"action"`
	Prompt        string          `json:"prompt"`
	RevisedPrompt string          `json:"revised_prompt"`
	Summary       json.RawMessage `json:"summary"`
}

type xAIResponsesFile struct {
	ID string `json:"id"`
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
	seenOutputItemIDs    map[string]struct{}
	seenOutputItemURLs   map[string]struct{}
	seenReasoningItemIDs map[string]struct{}
	hasVisibleContent    bool
}

// ProviderUsesResponsesAPI reports whether a provider uses the Responses API.
func ProviderUsesResponsesAPI(providerName string, provider ProviderRequestConfig) bool {
	apiKind := provider.APIKind
	if apiKind != ProviderAPIKindOpenAI {
		return false
	}

	if usesBuiltInOpenAIProvider(providerName, apiKind) {
		return true
	}

	trimmedProviderName := strings.ToLower(strings.TrimSpace(providerName))
	// XAIProviderName is the built-in x-ai provider name.
	if trimmedProviderName == XAIProviderName || strings.Contains(trimmedProviderName, "grok") {
		return true
	}

	return xAIBaseURLUsesOfficialAPI(provider.BaseURL)
}

func responsesRequestLabel(request ChatCompletionRequest) string {
	if XAIConfiguredModel(request.ConfiguredModel) || xAIBaseURLUsesOfficialAPI(request.Provider.BaseURL) {
		return "xAI responses"
	}

	return "responses"
}

// AssignXAIPreviousResponseID wires the previous response id continuation.
func AssignXAIPreviousResponseID(
	request *ChatCompletionRequest,
	sourceMessage *discordgo.Message,
	store NodeStore,
	maxMessages int,
) {
	if request == nil || !requestUsesXAIPreviousResponseID(*request) {
		return
	}

	previousResponseID := XAIConversationPreviousResponseID(
		request.ConfiguredModel,
		sourceMessage,
		store,
		maxMessages,
	)
	if previousResponseID == "" {
		return
	}

	systemMessages, _ := splitLeadingSystemMessages(request.Messages)

	continuationMessages, ok := xAIContinuationMessages(request.Messages)
	if !ok {
		return
	}

	// Re-attach the leading system prompt: request.Messages is replaced with
	// the continuation handoff below, so the system prompt must survive into
	// the follow-up request sent to the provider.
	request.Messages = make([]ChatMessage, 0, len(systemMessages)+len(continuationMessages))
	request.Messages = append(request.Messages, systemMessages...)
	request.Messages = append(request.Messages, continuationMessages...)
	request.PreviousResponseID = previousResponseID
}

func requestUsesXAIPreviousResponseID(request ChatCompletionRequest) bool {
	if !request.Provider.UseResponsesAPI {
		return false
	}

	return XAIConfiguredModel(request.ConfiguredModel) ||
		xAIBaseURLUsesOfficialAPI(request.Provider.BaseURL)
}

// XAIConversationPreviousResponseID walks the chain for a response id.
func XAIConversationPreviousResponseID(
	configuredModel string,
	sourceMessage *discordgo.Message,
	store NodeStore,
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

		node, ok := store.Get(currentMessageID)
		if !ok {
			break
		}

		role, providerResponseID, providerResponseModel, parentMessage := node.Get()
		role = strings.TrimSpace(role)
		providerResponseID = strings.TrimSpace(providerResponseID)
		providerResponseModel = strings.TrimSpace(providerResponseModel)

		if role == searchtypes.MessageRoleAssistant {
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

// splitLeadingSystemMessages separates the contiguous leading system messages
// (the system prompt) from the conversation messages that follow them.
func splitLeadingSystemMessages(messages []ChatMessage) ([]ChatMessage, []ChatMessage) {
	splitIndex := 0
	for splitIndex < len(messages) &&
		strings.EqualFold(strings.TrimSpace(messages[splitIndex].Role), searchtypes.MessageRoleSystem) {
		splitIndex++
	}

	systemMessages := append([]ChatMessage(nil), messages[:splitIndex]...)
	conversationMessages := append([]ChatMessage(nil), messages[splitIndex:]...)

	return systemMessages, conversationMessages
}

func xAIContinuationMessages(messages []ChatMessage) ([]ChatMessage, bool) {
	_, conversationMessages := splitLeadingSystemMessages(messages)
	if len(conversationMessages) == 0 {
		return nil, false
	}

	lastAssistantIndex := -1

	for index, msg := range slices.Backward(conversationMessages) {
		if msg.Role != searchtypes.MessageRoleAssistant {
			continue
		}

		lastAssistantIndex = index

		break
	}

	if lastAssistantIndex < 0 || lastAssistantIndex == len(conversationMessages)-1 {
		return nil, false
	}

	return append([]ChatMessage(nil), conversationMessages[lastAssistantIndex+1:]...), true
}

func (client openAIClient) streamResponses(
	ctx context.Context,
	request ChatCompletionRequest,
	handle func(StreamDelta) error,
) error {
	requestLabel := responsesRequestLabel(request)

	requestBody, err := prepareXAIResponsesRequestBody(ctx, client.httpClient, request)
	if err != nil {
		return fmt.Errorf("build %s request body: %w", requestLabel, err)
	}

	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", requestLabel, err)
	}

	requestURL, err := buildXAIResponsesURL(request.Provider.BaseURL, request.Provider.ExtraQuery)
	if err != nil {
		return fmt.Errorf("build %s url: %w", requestLabel, err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		requestURL,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return fmt.Errorf("create %s request: %w", requestLabel, err)
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
		return fmt.Errorf("send %s request: %w", requestLabel, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return fmt.Errorf(
				"read %s error response after status %d: %w",
				requestLabel,
				httpResponse.StatusCode,
				readErr,
			)
		}

		return NewOpenAIProviderStatusError(
			requestLabel+" request failed",
			httpResponse.StatusCode,
			httpResponse.Status,
			httpResponse.Header.Clone(),
			responseBody,
			false,
		)
	}

	terminalEventSeen, err := client.consumeXAIResponsesStream(httpResponse.Body, handle)
	if err != nil {
		return fmt.Errorf("consume %s stream: %w", requestLabel, err)
	}

	if !terminalEventSeen {
		return fmt.Errorf(
			"%s stream ended before response.completed: %w",
			requestLabel,
			io.ErrUnexpectedEOF,
		)
	}

	return nil
}

func (client openAIClient) consumeXAIResponsesStream(
	stream io.Reader,
	handle func(StreamDelta) error,
) (bool, error) {
	terminalEventSeen := false
	streamState := newXAIResponsesStreamState()

	_, err := consumeServerSentEvents(stream, func(payload []byte) error {
		terminal, payloadErr := handleXAIResponsesStreamPayload(payload, handle, streamState)
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

func prepareXAIResponsesRequestBody(
	ctx context.Context,
	httpClient *http.Client,
	request ChatCompletionRequest,
) (map[string]any, error) {
	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		return nil, err
	}

	if !xAIResponsesShouldUploadImageFiles(request) {
		return requestBody, nil
	}

	err = xAIResponsesReplaceLargeInlineImagesWithFileIDs(ctx, httpClient, request, requestBody)
	if err != nil {
		return nil, err
	}

	return requestBody, nil
}

func buildXAIResponsesRequestBody(request ChatCompletionRequest) (map[string]any, error) {
	messages := RequestMessagesWithFileOrImageOnlyQueryPlaceholder(request.Messages)
	messages = openAIReplaceSystemRoleWithDeveloper(messages, request.Model)

	if openAIRequestPromptCacheKeyPrefix(request) != "" &&
		!openAICacheOptionsModeIsExplicit(request.Provider.ExtraBody) {
		messages = openAIResponsesCacheBreakpointMessages(messages)
	}

	input, err := xAIResponsesInput(messages)
	if err != nil {
		return nil, err
	}

	requestBody := make(map[string]any, len(request.Provider.ExtraBody)+xAIResponsesRequestBodyBaseFields)
	requestBody["model"] = request.Model
	requestBody["stream"] = true
	requestBody["input"] = input
	addOpenAIPromptCacheKey(requestBody, request)

	if strings.TrimSpace(request.PreviousResponseID) != "" {
		requestBody["previous_response_id"] = request.PreviousResponseID
	}

	extraBody := request.Provider.ExtraBody
	if request.Provider.APIKind == ProviderAPIKindOpenAI &&
		(OpenAIConfiguredModel(request.ConfiguredModel) || XAIConfiguredModel(request.ConfiguredModel)) {
		extraBody = NormalizeOpenAIResponsesExtraBody(request.Model, extraBody)
	}

	maps.Copy(requestBody, extraBody)

	if request.Provider.APIKind == ProviderAPIKindOpenAI && OpenAIConfiguredModel(request.ConfiguredModel) {
		addOpenAICacheOptions(requestBody, request)
	}

	if shouldDefaultXAIBridgeSourceAttribution(request) {
		requestBody["source_attribution"] = defaultXAIBridgeSourceAttributionRequest()
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
				searchtypes.MessageTypeKey: xAIResponsesInputTextType,
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

func xAIResponsesShouldUploadImageFiles(request ChatCompletionRequest) bool {
	if XAIConfiguredModel(request.ConfiguredModel) {
		return true
	}

	return strings.Contains(strings.ToLower(strings.TrimSpace(request.Model)), "grok")
}

func xAIResponsesInput(messages []ChatMessage) ([]map[string]any, error) {
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

func xAIResponsesMessage(message ChatMessage) (map[string]any, bool, error) {
	role := strings.TrimSpace(message.Role)
	if role == "" {
		return nil, false, nil
	}

	switch role {
	case searchtypes.MessageRoleSystem, "developer":
		content, ok, err := xAIResponsesTextContent(message.Content)
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
	case searchtypes.MessageRoleUser:
		content, contentOK, contentErr := xAIResponsesUserContent(message.Content)
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
	case []ContentPart:
		if contentPartsContainNonText(typedContent) {
			return "", false, fmt.Errorf("unsupported xAI text content type %T: %w", content, os.ErrInvalid)
		}

		textContent := support.ContentPartsText(typedContent)
		if strings.TrimSpace(textContent) == "" {
			return "", false, nil
		}

		return textContent, true, nil
	case []map[string]any:
		return openAIResponsesBreakpointTextContent(typedContent, content)
	default:
		return "", false, fmt.Errorf("unsupported xAI text content type %T: %w", content, os.ErrInvalid)
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
		if partType != xAIResponsesInputTextType && partType != searchtypes.ContentTypeText {
			return "", false, fmt.Errorf(
				"unsupported xAI text content part type %T: %w",
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

func xAIResponsesUserContent(content any) (any, bool, error) {
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

func xAIResponsesUserPart(part ContentPart) (map[string]any, bool, error) {
	partType, _ := part["type"].(string)

	switch partType {
	case searchtypes.ContentTypeText:
		textValue, _ := part[searchtypes.MessageTextKey].(string)
		if strings.TrimSpace(textValue) == "" {
			return nil, false, nil
		}

		return map[string]any{
			searchtypes.MessageTypeKey: xAIResponsesInputTextType,
			searchtypes.MessageTextKey: textValue,
		}, true, nil
	case searchtypes.ContentTypeImageURL:
		imageURL, fileID, err := xAIResponsesImageReference(part)
		if err != nil {
			return nil, false, err
		}

		if fileID != "" {
			return map[string]any{
				searchtypes.MessageTypeKey:   xAIResponsesInputImageType,
				"file_id":                    fileID,
				searchtypes.MessageDetailKey: xAIResponsesImageDetailAuto,
			}, true, nil
		}

		if imageURL == "" {
			return nil, false, nil
		}

		return map[string]any{
			searchtypes.MessageTypeKey:   xAIResponsesInputImageType,
			"image_url":                  imageURL,
			searchtypes.MessageDetailKey: xAIResponsesImageDetailAuto,
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

func xAIResponsesImageReference(part ContentPart) (string, string, error) {
	stringMap, foundStringMap := part["image_url"].(map[string]string)
	if foundStringMap {
		return strings.TrimSpace(stringMap["url"]), strings.TrimSpace(stringMap["file_id"]), nil
	}

	rawImageURL, foundMap := part["image_url"].(map[string]any)
	if !foundMap {
		return "", "", fmt.Errorf("decode xAI image_url content part: %w", os.ErrInvalid)
	}

	imageURL, _ := rawImageURL["url"].(string)
	fileID, _ := rawImageURL["file_id"].(string)

	return strings.TrimSpace(imageURL), strings.TrimSpace(fileID), nil
}

func xAIResponsesReplaceLargeInlineImagesWithFileIDs(
	ctx context.Context,
	httpClient *http.Client,
	request ChatCompletionRequest,
	requestBody map[string]any,
) error {
	if httpClient == nil {
		return fmt.Errorf("missing xAI file upload client: %w", os.ErrInvalid)
	}

	input, ok := requestBody["input"].([]map[string]any)
	if !ok {
		return nil
	}

	uploadedFileIDs := make(map[string]string)

	for messageIndex, message := range input {
		content, contentOK := message["content"].([]map[string]any)
		if !contentOK {
			continue
		}

		for partIndex, part := range content {
			fileID, replace, err := xAIResponsesPreparedImageFileID(
				ctx,
				httpClient,
				request,
				part,
				uploadedFileIDs,
			)
			if err != nil {
				return fmt.Errorf("prepare xAI image input %d.%d: %w", messageIndex, partIndex, err)
			}

			if !replace {
				continue
			}

			delete(part, "image_url")
			part["file_id"] = fileID
		}
	}

	return nil
}

func xAIResponsesPreparedImageFileID(
	ctx context.Context,
	httpClient *http.Client,
	request ChatCompletionRequest,
	part map[string]any,
	uploadedFileIDs map[string]string,
) (string, bool, error) {
	partType, _ := part["type"].(string)
	if partType != xAIResponsesInputImageType {
		return "", false, nil
	}

	fileIDValue, _ := part["file_id"].(string)
	if strings.TrimSpace(fileIDValue) != "" {
		return "", false, nil
	}

	imageURL, _ := part["image_url"].(string)
	imageURL = strings.TrimSpace(imageURL)

	shouldUpload, err := xAIResponsesShouldUploadInlineImage(request, imageURL)
	if err != nil {
		return "", false, err
	}

	if !shouldUpload {
		return "", false, nil
	}

	if cachedFileID, exists := uploadedFileIDs[imageURL]; exists {
		return cachedFileID, true, nil
	}

	imageData, err := ParseBase64ImageDataURL(imageURL)
	if err != nil {
		return "", false, fmt.Errorf("parse xAI inline image: %w", err)
	}

	fileID, err := uploadXAIResponsesInputFile(
		ctx,
		httpClient,
		request,
		imageData.Decoder(),
		imageData.DecodedLengthEstimate(),
		imageData.MimeType,
	)
	if err != nil {
		return "", false, err
	}

	uploadedFileIDs[imageURL] = fileID

	return fileID, true, nil
}

func xAIResponsesShouldUploadInlineImage(
	request ChatCompletionRequest,
	imageURL string,
) (bool, error) {
	if !strings.HasPrefix(imageURL, "data:") {
		return false, nil
	}

	metadata, payload, found := strings.Cut(strings.TrimPrefix(imageURL, "data:"), ",")
	if !found {
		return false, fmt.Errorf("parse xAI data URL %q: %w", imageURL, os.ErrInvalid)
	}

	if !strings.Contains(strings.ToLower(metadata), "base64") {
		return false, nil
	}

	if !xAIBaseURLUsesOfficialAPI(request.Provider.BaseURL) {
		return true, nil
	}

	return base64DecodedLengthEstimate(payload) > XAIInlineImageByteLimit, nil
}

func uploadXAIResponsesInputFile(
	ctx context.Context,
	httpClient *http.Client,
	request ChatCompletionRequest,
	fileReader io.Reader,
	fileByteCount int,
	mimeType string,
) (string, error) {
	requestURL, err := buildXAIFileUploadURL(request.Provider.BaseURL, request.Provider.ExtraQuery)
	if err != nil {
		return "", fmt.Errorf("build xAI file upload url: %w", err)
	}

	requestBody, contentType, contentLength, err := XAIFileUploadPayload(
		fileReader,
		fileByteCount,
		mimeType,
	)
	if err != nil {
		return "", err
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		requestURL,
		requestBody,
	)
	if err != nil {
		return "", fmt.Errorf("create xAI file upload request: %w", err)
	}

	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+openAIAPIKey(requestKey(request.Provider)))
	httpRequest.Header.Set("Content-Type", contentType)
	httpRequest.ContentLength = contentLength
	setOpenAIClientRequestIDHeader(httpRequest, request)

	for key, value := range request.Provider.ExtraHeaders {
		httpRequest.Header.Set(key, stringifyValue(value))
	}

	httpResponse, err := httpClient.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("send xAI file upload request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	return xAIUploadedFileIDFromResponse(httpResponse)
}

type redirectWriter struct {
	destination io.Writer
}

func (writer *redirectWriter) Write(data []byte) (int, error) {
	written, err := writer.destination.Write(data)
	if err != nil {
		return written, fmt.Errorf("redirect xAI upload body: %w", err)
	}

	return written, nil
}

// XAIFileUploadPayload streams a multipart file upload body.
func XAIFileUploadPayload(
	fileReader io.Reader,
	fileByteCount int,
	mimeType string,
) (io.Reader, string, int64, error) {
	if fileReader == nil || fileByteCount <= 0 {
		return nil, "", 0, fmt.Errorf("empty xAI file upload: %w", os.ErrInvalid)
	}

	var (
		requestPrefix bytes.Buffer
		requestSuffix bytes.Buffer
	)

	bodyWriter := &redirectWriter{destination: &requestPrefix}
	multipartWriter := multipart.NewWriter(bodyWriter)

	err := multipartWriter.WriteField("purpose", XAIResponsesUploadPurposeUserData)
	if err != nil {
		return nil, "", 0, fmt.Errorf("write xAI file upload purpose: %w", err)
	}

	filePartHeaders := make(textproto.MIMEHeader)
	filePartHeaders.Set(
		"Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename="%s"`, xAIInputImageUploadFilename),
	)
	filePartHeaders.Set("Content-Type", mimeType)

	_, err = multipartWriter.CreatePart(filePartHeaders)
	if err != nil {
		return nil, "", 0, fmt.Errorf("create xAI file upload part: %w", err)
	}

	bodyWriter.destination = &requestSuffix

	err = multipartWriter.Close()
	if err != nil {
		return nil, "", 0, fmt.Errorf("close xAI file upload body: %w", err)
	}

	contentLength := int64(requestPrefix.Len()) + int64(fileByteCount) + int64(requestSuffix.Len())

	return io.MultiReader(&requestPrefix, fileReader, &requestSuffix),
		multipartWriter.FormDataContentType(),
		contentLength,
		nil
}

func xAIUploadedFileIDFromResponse(httpResponse *http.Response) (string, error) {
	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return "", fmt.Errorf("read xAI file upload response after status %d: %w", httpResponse.StatusCode, err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return "", NewOpenAIProviderStatusError(
			"xAI file upload request failed",
			httpResponse.StatusCode,
			httpResponse.Status,
			httpResponse.Header.Clone(),
			responseBody,
			false,
		)
	}

	var uploadedFile xAIResponsesFile

	err = json.Unmarshal(responseBody, &uploadedFile)
	if err != nil {
		return "", fmt.Errorf("decode xAI file upload response: %w", err)
	}

	if strings.TrimSpace(uploadedFile.ID) == "" {
		return "", fmt.Errorf("missing xAI file upload id: %w", os.ErrInvalid)
	}

	return uploadedFile.ID, nil
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

func buildXAIFileUploadURL(baseURL string, extraQuery map[string]any) (string, error) {
	parsedURL, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse xAI base url %q: %w", baseURL, err)
	}

	parsedURL.Path = path.Join(parsedURL.Path, "files")

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

// XAIConfiguredModel reports whether a configured model is an xAI model.
func XAIConfiguredModel(configuredModel string) bool {
	providerName, err := splitConfiguredModel(strings.TrimSpace(configuredModel))
	if err != nil {
		return false
	}

	trimmedProviderName := strings.ToLower(strings.TrimSpace(providerName))

	// XAIProviderName is the built-in x-ai provider name.
	return trimmedProviderName == XAIProviderName || strings.Contains(trimmedProviderName, "grok")
}

func shouldDefaultXAIBridgeSourceAttribution(request ChatCompletionRequest) bool {
	if !XAIConfiguredModel(request.ConfiguredModel) || xAIBaseURLUsesOfficialAPI(request.Provider.BaseURL) {
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
	handle func(StreamDelta) error,
	state *xAIResponsesStreamState,
) (bool, error) {
	delta, terminal, err := xAIResponsesStreamPayloadDelta(payload, state)
	if err != nil {
		return terminal, err
	}

	if delta.Content == "" &&
		delta.Thinking == "" &&
		delta.FinishReason == "" &&
		delta.ProviderResponseID == "" {
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
		seenOutputItemIDs:    make(map[string]struct{}),
		seenOutputItemURLs:   make(map[string]struct{}),
		seenReasoningItemIDs: make(map[string]struct{}),
		hasVisibleContent:    false,
	}
}

func xAIResponsesStreamPayloadDelta(
	payload []byte,
	state *xAIResponsesStreamState,
) (StreamDelta, bool, error) {
	var event xAIResponsesStreamEvent

	emptyDelta := emptyStreamDelta()

	err := json.Unmarshal(payload, &event)
	if err != nil {
		return emptyDelta, false, fmt.Errorf("decode xAI responses stream Payload: %w", err)
	}

	eventType := strings.TrimSpace(event.Type)

	switch eventType {
	case xAIResponsesStreamEventReasoningSummaryTextDelta,
		xAIResponsesStreamEventReasoningTextDelta:
		delta := emptyDelta
		delta.Thinking = event.Delta

		return delta, false, nil
	case xAIResponsesStreamEventReasoningSummaryPartDone,
		xAIResponsesStreamEventReasoningTextDone:
		delta := emptyDelta
		delta.Thinking = "\n\n"

		return delta, false, nil
	case xAIResponsesStreamEventOutputDelta:
		if state != nil && event.Delta != "" {
			state.hasVisibleContent = true
		}

		delta := emptyDelta
		delta.Content = event.Delta

		return delta, false, nil
	case xAIResponsesStreamEventOutputDone:
		delta := emptyDelta

		delta.Thinking = xAIResponsesOutputItemThinking(event.Item, state)
		if delta.Thinking == "" {
			delta.Content = xAIResponsesOutputItemText(event.Item, state, false)
		}

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

func emptyStreamDelta() StreamDelta {
	return StreamDelta{
		Thinking:           "",
		Content:            "",
		FinishReason:       "",
		ProviderResponseID: "",
		SearchMetadata:     nil,
	}
}

func xAIResponsesCompletedDelta(
	response *xAIResponsesStreamResponse,
	eventSourceAttribution *xAISourceAttribution,
	state *xAIResponsesStreamState,
) (StreamDelta, error) {
	if response == nil {
		return StreamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       finishReasonStop,
			ProviderResponseID: "",
			SearchMetadata:     xAISourceAttributionSearchMetadata(eventSourceAttribution),
		}, nil
	}

	if response.Error != nil {
		return StreamDelta{
				Thinking:           "",
				Content:            "",
				FinishReason:       "",
				ProviderResponseID: "",
				SearchMetadata:     nil,
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

		return StreamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata:     nil,
		}, xAIResponsesStatusError(status, reason)
	}

	thinking := xAIResponsesOutputItemsThinking(response.Output, state)
	content := xAIResponsesOutputItemsText(response.Output, state, true)

	return StreamDelta{
		Thinking:           thinking,
		Content:            content,
		FinishReason:       finishReasonStop,
		ProviderResponseID: strings.TrimSpace(response.ID),
		SearchMetadata: xAISourceAttributionSearchMetadata(
			mergeXAISourceAttribution(
				response.SourceAttribution,
				eventSourceAttribution,
			),
		),
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

func xAIResponsesOutputItemsThinking(
	items []xAIResponsesOutputItem,
	state *xAIResponsesStreamState,
) string {
	if len(items) == 0 {
		return ""
	}

	var builder strings.Builder

	for index := range items {
		builder.WriteString(xAIResponsesOutputItemThinking(&items[index], state))
	}

	return builder.String()
}

func xAIResponsesOutputItemThinking(
	item *xAIResponsesOutputItem,
	state *xAIResponsesStreamState,
) string {
	summaryText := xAIResponsesReasoningSummaryText(item)
	if summaryText == "" || xAIResponsesReasoningItemSeen(state, item) {
		return ""
	}

	xAIResponsesMarkReasoningItemSeen(state, item)

	return summaryText + "\n\n"
}

func xAIResponsesReasoningSummaryText(item *xAIResponsesOutputItem) string {
	if item == nil || !strings.EqualFold(strings.TrimSpace(item.Type), xAIResponsesOutputTypeReasoning) {
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
			(partType != "" && !strings.EqualFold(partType, xAIResponsesReasoningSummaryTextType)) {
			continue
		}

		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}

		builder.WriteString(text)
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

func xAIResponsesReasoningItemSeen(
	state *xAIResponsesStreamState,
	item *xAIResponsesOutputItem,
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

func xAIResponsesMarkReasoningItemSeen(
	state *xAIResponsesStreamState,
	item *xAIResponsesOutputItem,
) {
	if state == nil || item == nil {
		return
	}

	itemID := strings.TrimSpace(item.ID)
	if itemID != "" {
		state.seenReasoningItemIDs[itemID] = struct{}{}
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

func xAISourceAttributionSearchMetadata(attribution *xAISourceAttribution) *searchtypes.SearchMetadata {
	if attribution == nil {
		return nil
	}

	queries := normalizeSearchQueries(attribution.SearchQueries)
	querySources := make(map[string][]searchtypes.SearchSource, len(queries))
	seenURLsByQuery := make(map[string]map[string]struct{}, len(queries))

	for _, query := range queries {
		querySources[query] = nil
		seenURLsByQuery[query] = make(map[string]struct{})
	}

	unscopedSources := make([]searchtypes.SearchSource, 0, len(attribution.Sources))
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

	results := make([]searchtypes.WebSearchResult, 0, len(queries)+1)

	for _, query := range queries {
		sources := querySources[query]
		if len(sources) == 0 {
			continue
		}

		results = append(results, searchtypes.WebSearchResult{
			Query: query,
			Text:  xAISearchSourcesResultText(sources),
		})
	}

	if len(unscopedSources) > 0 {
		results = append(results, searchtypes.WebSearchResult{
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
			sourceCount := len(searchtypes.ExtractSearchSources(result.Text))
			if sourceCount > maxURLs {
				maxURLs = sourceCount
			}
		}
	}

	return &searchtypes.SearchMetadata{
		Queries:             queries,
		Results:             results,
		MaxURLs:             maxURLs,
		VisualSearchSources: nil,
	}
}

// FinalizeXAIResponseAnswer strips the bridge source appendix from an answer.
func FinalizeXAIResponseAnswer(
	_ ChatCompletionRequest,
	answerText string,
	existingMetadata *searchtypes.SearchMetadata,
) (string, *searchtypes.SearchMetadata) {
	cleanedAnswerText, attribution, ok := parseXAIBridgeSourceAttributionAppendix(answerText)
	if !ok {
		return answerText, nil
	}

	if searchtypes.SearchMetadataHasWebSources(existingMetadata) {
		return cleanedAnswerText, nil
	}

	return cleanedAnswerText, xAISourceAttributionSearchMetadata(attribution)
}

// XAIStreamingVisibleAnswerText returns the visible answer text for a request.
func XAIStreamingVisibleAnswerText(request ChatCompletionRequest, answerText string) string {
	if xAIBaseURLUsesOfficialAPI(request.Provider.BaseURL) {
		return answerText
	}

	appendixStart, ok := xAIStreamingSourceAppendixStart(answerText)
	if !ok {
		return answerText
	}

	return strings.TrimRight(answerText[:appendixStart], "\r\n")
}

func normalizedSourceAppendixHeader(line string) string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimLeft(trimmed, "#")
	trimmed = strings.TrimSpace(trimmed)
	trimmed = strings.Trim(trimmed, "*_:")
	trimmed = strings.TrimSpace(trimmed)

	return strings.ToLower(trimmed)
}

func isSourceAppendixHeaderLine(line string) bool {
	return slices.Contains(sourceAppendixHeaderPrefixesList(), normalizedSourceAppendixHeader(line))
}

func isSourceAppendixHeaderPartial(line string) bool {
	partialHeader := normalizedSourceAppendixHeader(line)
	if partialHeader == "" {
		return true
	}

	for _, prefix := range sourceAppendixHeaderPrefixesList() {
		if strings.HasPrefix(prefix, partialHeader) {
			return true
		}
	}

	return false
}

func isSearchQueriesHeaderLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}

	trimmed = strings.TrimLeft(trimmed, "#")
	trimmed = strings.TrimSpace(trimmed)
	trimmed = strings.Trim(trimmed, "*_:")
	trimmed = strings.TrimSpace(trimmed)

	lower := strings.ToLower(trimmed)

	return lower == "search queries" || lower == "search query" || lower == "queries"
}

func xAIStreamingSourceAppendixStart(answerText string) (int, bool) {
	if answerText == "" {
		return 0, false
	}

	normalized, originalOffsets := normalizeNewlinesWithOriginalOffsets(answerText)
	lastIdx := -1
	idx := 0

	for {
		nextSep := strings.Index(normalized[idx:], "\n\n")
		if nextSep < 0 {
			break
		}

		pos := idx + nextSep
		afterSep := normalized[pos+doubleNewlineSeparatorLength:]

		firstLine, _, _ := strings.Cut(afterSep, "\n")
		if isSourceAppendixHeaderLine(firstLine) {
			lastIdx = pos
		}

		idx = pos + 1
	}

	if lastIdx >= 0 {
		return originalOffsets[lastIdx], true
	}

	firstLine, _, _ := strings.Cut(normalized, "\n")
	if isSourceAppendixHeaderLine(firstLine) {
		return 0, true
	}

	if lastDoubleNewline := strings.LastIndex(normalized, "\n\n"); lastDoubleNewline >= 0 {
		tail := normalized[lastDoubleNewline+doubleNewlineSeparatorLength:]
		if !strings.Contains(tail, "\n") && isSourceAppendixHeaderPartial(tail) {
			return originalOffsets[lastDoubleNewline], true
		}
	} else if !strings.Contains(normalized, "\n") && isSourceAppendixHeaderPartial(normalized) {
		return 0, true
	}

	if strings.HasSuffix(normalized, "\n") {
		return originalOffsets[len(normalized)-1], true
	}

	return 0, false
}

func normalizeNewlinesWithOriginalOffsets(text string) (string, []int) {
	var normalized strings.Builder

	normalized.Grow(len(text))
	originalOffsets := make([]int, 1, len(text)+1)

	for index := 0; index < len(text); index++ {
		if text[index] == '\r' {
			if index+1 < len(text) && text[index+1] == '\n' {
				index++
			}

			normalized.WriteByte('\n')
		} else {
			normalized.WriteByte(text[index])
		}

		originalOffsets = append(originalOffsets, index+1)
	}

	return normalized.String(), originalOffsets
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

func xAISourceAttributionSearchSource(source xAISourceAttributionSource) searchtypes.SearchSource {
	return searchtypes.SearchSource{
		Title: source.Title,
		URL:   source.URL,
	}
}

func appendXAISourceIfUnique(
	sources []searchtypes.SearchSource,
	seenURLs map[string]struct{},
	source searchtypes.SearchSource,
) []searchtypes.SearchSource {
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

func xAISearchSourcesResultText(sources []searchtypes.SearchSource) string {
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

	appendixStart, ok := xAIStreamingSourceAppendixStart(normalizedAnswerText)
	if !ok {
		return answerText, nil, false
	}

	cleanedAnswerText := strings.TrimSpace(normalizedAnswerText[:appendixStart])

	appendix := strings.TrimLeft(normalizedAnswerText[appendixStart:], "\n")

	firstLine, restOfAppendix, _ := strings.Cut(appendix, "\n")
	if !isSourceAppendixHeaderLine(firstLine) {
		return answerText, nil, false
	}

	sourcesSection := restOfAppendix
	queriesSection := ""

	lines := strings.Split(restOfAppendix, "\n")
	for i, line := range lines {
		if isSearchQueriesHeaderLine(line) {
			sourcesSection = strings.Join(lines[:i], "\n")
			queriesSection = strings.Join(lines[i+1:], "\n")

			break
		}
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
		lineText := line
		if match := xAISourceAppendixNumberedLinePattern.FindStringSubmatch(
			strings.TrimSpace(line),
		); len(match) == sourceAppendixNumberedMatchParts {
			lineText = strings.TrimSpace(match[1])
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
		lineText := line
		if match := xAISourceAppendixNumberedLinePattern.FindStringSubmatch(
			strings.TrimSpace(line),
		); len(match) == sourceAppendixNumberedMatchParts {
			lineText = strings.TrimSpace(match[1])
		}

		queries = append(queries, parseXAIBridgeQueryList(lineText)...)
	}

	return normalizeSearchQueries(queries)
}

func parseXAIBridgeSourceLine(line string) (xAISourceAttributionSource, bool) {
	var emptySource xAISourceAttributionSource

	trimmedLine := strings.TrimSpace(line)
	if trimmedLine == "" {
		return emptySource, false
	}

	var title, rawURL, remainder string

	if match := xAISourceAppendixMarkdownLinkPattern.FindStringSubmatch(
		trimmedLine,
	); len(match) == sourceAppendixMarkdownLinkMatchParts {
		title = strings.TrimSpace(match[1])
		rawURL = strings.TrimSpace(match[2])
		remainder = match[3]
	} else if match := xAISourceAppendixTitleURLPattern.FindStringSubmatch(
		trimmedLine,
	); len(match) == sourceAppendixTitleURLMatchParts {
		title = strings.TrimSpace(match[1])
		rawURL = strings.TrimSpace(match[2])
		remainder = match[3]
	} else if match := xAISourceAppendixBareURLPattern.FindStringSubmatch(
		trimmedLine,
	); len(match) == sourceAppendixBareURLMatchParts {
		rawURL = strings.TrimSpace(match[1])
		title = rawURL
		remainder = match[2]
	} else {
		return emptySource, false
	}

	title = strings.Trim(title, "`\"'")

	source, ok := normalizeXAISourceAttributionSource(xAISourceAttributionSource{
		Title:         title,
		URL:           rawURL,
		SearchQueries: parseXAIBridgeSourceQueries(remainder),
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

const openAICacheBreakpointModeExplicit = "explicit"

const splitConfiguredModelParts = 2

func splitConfiguredModel(configuredModel string) (string, error) {
	parts := strings.SplitN(strings.TrimSuffix(configuredModel, ":vision"), "/", splitConfiguredModelParts)
	if len(parts) != splitConfiguredModelParts {
		return "", fmt.Errorf("split configured model %q: %w", configuredModel, os.ErrInvalid)
	}

	return parts[0], nil
}

func usesBuiltInOpenAIProvider(providerName string, apiKind ProviderAPIKind) bool {
	if apiKind != ProviderAPIKindOpenAI {
		return false
	}

	return XAIConfiguredModel(providerName) || providerName == "openai"
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

func normalizeSearchQueries(queries []string) []string {
	seenQueries := make(map[string]struct{}, len(queries))
	normalizedQueries := make([]string, 0, len(queries))

	for _, query := range queries {
		trimmedQuery := strings.TrimSpace(query)
		if trimmedQuery == "" {
			continue
		}

		foldedQuery := strings.ToLower(trimmedQuery)
		if _, ok := seenQueries[foldedQuery]; ok {
			continue
		}

		seenQueries[foldedQuery] = struct{}{}

		normalizedQueries = append(normalizedQueries, trimmedQuery)
	}

	return normalizedQueries
}

func requestKey(provider ProviderRequestConfig) string {
	if key := firstAPIKey(provider.APIKeys); key != "" {
		return key
	}

	return provider.APIKey
}
