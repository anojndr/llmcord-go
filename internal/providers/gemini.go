package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"google.golang.org/genai"

	searchtypes "llmcord-go/internal/searchtypes"
	support "llmcord-go/internal/support"
)

var geminiAPIVersionPattern = regexp.MustCompile(`^v[0-9]+(?:(?:alpha|beta)[0-9]*)?$`)

const (
	// GeminiNoToolsInstruction is appended when no tools are available.
	GeminiNoToolsInstruction = "No functions or tools are available in this request. " +
		"Do not call any function or tool. Answer directly in text using the provided context."
	// GeminiGroundingInstruction is appended when Google Search grounding is on.
	GeminiGroundingInstruction = "Google Search is the only available tool in this request " +
		"and is executed automatically by the provider. Do not call custom functions, " +
		"invent tool names, or emit function-call syntax."
	geminiThoughtMarkerOpen  = "<<<"
	geminiThoughtMarkerClose = ">>>"
	geminiSplitDeltaCapacity = 3
)

type geminiContentStreamer interface {
	GenerateContentStream(
		ctx context.Context,
		model string,
		contents []*genai.Content,
		config *genai.GenerateContentConfig,
	) iter.Seq2[*genai.GenerateContentResponse, error]
}

type geminiFilesClient interface {
	UploadFile(
		ctx context.Context,
		reader io.Reader,
		config *genai.UploadFileConfig,
	) (*genai.File, error)
	GetFile(ctx context.Context, name string, config *genai.GetFileConfig) (*genai.File, error)
}

type geminiAPIClient interface {
	geminiContentStreamer
	geminiFilesClient
}

type liveGeminiAPIClient struct {
	client *genai.Client
}

func (client liveGeminiAPIClient) GenerateContentStream(
	ctx context.Context,
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
) iter.Seq2[*genai.GenerateContentResponse, error] {
	return client.client.Models.GenerateContentStream(ctx, model, contents, config)
}

func (client liveGeminiAPIClient) UploadFile(
	ctx context.Context,
	reader io.Reader,
	config *genai.UploadFileConfig,
) (*genai.File, error) {
	file, err := client.client.Files.Upload(ctx, reader, config)
	if err != nil {
		return nil, fmt.Errorf("upload gemini file: %w", err)
	}

	return file, nil
}

func (client liveGeminiAPIClient) GetFile(
	ctx context.Context,
	name string,
	config *genai.GetFileConfig,
) (*genai.File, error) {
	file, err := client.client.Files.Get(ctx, name, config)
	if err != nil {
		return nil, fmt.Errorf("get gemini file: %w", err)
	}

	return file, nil
}

func (client liveGeminiAPIClient) CreateCachedContent(
	ctx context.Context,
	model string,
	config *genai.CreateCachedContentConfig,
) (*genai.CachedContent, error) {
	cachedContent, err := client.client.Caches.Create(ctx, model, config)
	if err != nil {
		return nil, fmt.Errorf("create gemini cached content: %w", err)
	}

	return cachedContent, nil
}

type geminiClientFactory func(
	context.Context,
	*genai.ClientConfig,
) (geminiAPIClient, error)

type geminiClient struct {
	httpClient *http.Client
	newClient  geminiClientFactory
}

// GeminiAPIStatusCode extracts the HTTP status code from a Gemini API error
// when the error chain contains one. The genai SDK surfaces server failures
// as *genai.APIError values, so callers can distinguish model-level rejections
// (e.g. audio sent to a model without audio support, which the API reports as
// 500 INTERNAL) from client-side failures.
func GeminiAPIStatusCode(err error) (int, bool) {
	var apiErr *genai.APIError
	if !errors.As(err, &apiErr) {
		return 0, false
	}

	return apiErr.Code, true
}

func newGeminiClient(httpClient *http.Client) geminiClient {
	return geminiClient{
		httpClient: httpClient,
		newClient: func(
			ctx context.Context,
			config *genai.ClientConfig,
		) (geminiAPIClient, error) {
			client, err := genai.NewClient(ctx, config)
			if err != nil {
				return nil, fmt.Errorf("new genai client: %w", err)
			}

			return liveGeminiAPIClient{client: client}, nil
		},
	}
}

func (client geminiClient) streamChatCompletion(
	ctx context.Context,
	request ChatCompletionRequest,
	handle func(StreamDelta) error,
) error {
	clientConfig, err := buildGeminiClientConfig(request.Provider, client.httpClient)
	if err != nil {
		return fmt.Errorf("build gemini client config: %w", err)
	}

	apiClient, err := client.newClient(ctx, clientConfig)
	if err != nil {
		return fmt.Errorf("create gemini client: %w", err)
	}

	contents, generateConfig, err := buildGeminiGenerateContentRequestWithCaching(
		ctx,
		request,
		apiClient,
		apiClient,
	)
	if err != nil {
		return fmt.Errorf("build gemini request: %w", err)
	}

	finishSeen := false
	hasContent := false

	splitter := newGeminiStreamHandleSplitter(handle)

	for response, streamErr := range apiClient.GenerateContentStream(
		ctx,
		request.Model,
		contents,
		generateConfig,
	) {
		finished, contentProduced, err := processGeminiStreamResponse(splitter.handleDelta, response, streamErr)
		if err != nil {
			return err
		}

		finishSeen = finishSeen || finished
		hasContent = hasContent || contentProduced
	}

	err = splitter.finalize()
	if err != nil {
		return err
	}

	if !finishSeen {
		return fmt.Errorf("gemini stream ended without finish reason: %w", io.ErrUnexpectedEOF)
	}

	if !hasContent {
		return ErrEmptyModelResponse
	}

	return nil
}

type geminiStreamHandleSplitter struct {
	emit     func(StreamDelta) error
	splitter *geminiThoughtSplitter
}

func newGeminiStreamHandleSplitter(handle func(StreamDelta) error) *geminiStreamHandleSplitter {
	return &geminiStreamHandleSplitter{
		emit:     handle,
		splitter: newGeminiThoughtSplitter(),
	}
}

func (splitter *geminiStreamHandleSplitter) handleDelta(delta StreamDelta) error {
	if delta.Content == "" {
		return splitter.emit(delta)
	}

	splitThinking, splitAnswer := splitter.splitter.split(delta.Content)

	parts := make([]StreamDelta, 0, geminiSplitDeltaCapacity)
	if delta.Thinking != "" {
		parts = append(parts, StreamDelta{
			Thinking:           delta.Thinking,
			Content:            "",
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
	}

	if splitThinking != "" {
		parts = append(parts, StreamDelta{
			Thinking:           splitThinking,
			Content:            "",
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
	}

	if splitAnswer != "" {
		parts = append(parts, StreamDelta{
			Thinking:           "",
			Content:            splitAnswer,
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
	}

	if len(parts) == 0 {
		if delta.SearchMetadata == nil {
			return nil
		}

		parts = append(parts, StreamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata:     delta.SearchMetadata,
		})
	}

	for index, part := range parts {
		if index == 0 {
			part.SearchMetadata = delta.SearchMetadata
		}

		err := splitter.emit(part)
		if err != nil {
			return fmt.Errorf(handleStreamDeltaErrorFormat, err)
		}
	}

	return nil
}

func (splitter *geminiStreamHandleSplitter) finalize() error {
	splitThinking, splitAnswer := splitter.splitter.finalize()
	if splitThinking != "" {
		err := splitter.emit(StreamDelta{
			Thinking:           splitThinking,
			Content:            "",
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
		if err != nil {
			return fmt.Errorf(handleStreamDeltaErrorFormat, err)
		}
	}

	if splitAnswer != "" {
		err := splitter.emit(StreamDelta{
			Thinking:           "",
			Content:            splitAnswer,
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
		if err != nil {
			return fmt.Errorf(handleStreamDeltaErrorFormat, err)
		}
	}

	return nil
}

type geminiThoughtSplitter struct {
	inThinking bool
	thinking   strings.Builder
	pending    string
}

func newGeminiThoughtSplitter() *geminiThoughtSplitter {
	return &geminiThoughtSplitter{
		inThinking: false,
		thinking:   strings.Builder{},
		pending:    "",
	}
}

// split separates Gemini inline thought blocks (wrapped in <<< and >>> markers)
// from visible answer text. State persists across chunks so markers split
// between stream chunks are still recognized.
func (splitter *geminiThoughtSplitter) split(content string) (string, string) {
	if content == "" {
		return splitter.flushThinking(), ""
	}

	text := splitter.pending + content
	splitter.pending = ""

	var answer strings.Builder

	for text != "" {
		if splitter.inThinking {
			if markerIndex := strings.Index(text, geminiThoughtMarkerClose); markerIndex >= 0 {
				splitter.thinking.WriteString(text[:markerIndex])
				text = text[markerIndex+len(geminiThoughtMarkerClose):]
				splitter.inThinking = false

				continue
			}

			keep := geminiThoughtMarkerTrailingRunes(text, '>')
			splitter.thinking.WriteString(text[:len(text)-keep])
			splitter.pending = text[len(text)-keep:]

			text = ""

			continue
		}

		if markerIndex := strings.Index(text, geminiThoughtMarkerOpen); markerIndex >= 0 {
			answer.WriteString(text[:markerIndex])
			text = text[markerIndex+len(geminiThoughtMarkerOpen):]
			splitter.inThinking = true

			continue
		}

		keep := geminiThoughtMarkerTrailingRunes(text, '<')
		answer.WriteString(text[:len(text)-keep])
		splitter.pending = text[len(text)-keep:]

		text = ""
	}

	return splitter.flushThinking(), answer.String()
}

// finalize returns any thinking or answer text still buffered when the stream
// ends, such as a partial delimiter that never completed into a full marker.
func (splitter *geminiThoughtSplitter) finalize() (string, string) {
	if splitter.inThinking {
		splitter.thinking.WriteString(splitter.pending)
		splitter.pending = ""

		return splitter.flushThinking(), ""
	}

	answer := splitter.pending
	splitter.pending = ""

	return "", answer
}

func (splitter *geminiThoughtSplitter) flushThinking() string {
	if splitter.thinking.Len() == 0 {
		return ""
	}

	text := splitter.thinking.String()
	splitter.thinking.Reset()

	return text
}

// geminiThoughtMarkerTrailingRunes returns how many trailing marker runes could
// form the start of a three-rune delimiter split across stream chunks.
func geminiThoughtMarkerTrailingRunes(text string, markerRune byte) int {
	count := 0
	for index := len(text) - 1; index >= 0 && count < 2 && text[index] == markerRune; index-- {
		count++
	}

	return count
}

func processGeminiStreamResponse(
	handle func(StreamDelta) error,
	response *genai.GenerateContentResponse,
	streamErr error,
) (bool, bool, error) {
	if streamErr != nil {
		return false, false, fmt.Errorf("stream gemini content: %w", streamErr)
	}

	delta, processErr := geminiStreamDelta(response)

	contentProduced := delta.Thinking != "" || delta.Content != "" || delta.SearchMetadata != nil

	err := geminiHandleStreamUpdate(handle, delta)
	if err != nil {
		return false, false, err
	}

	if processErr != nil {
		return false, contentProduced, fmt.Errorf("process gemini stream response: %w", processErr)
	}

	if delta.FinishReason == "" {
		return false, contentProduced, nil
	}

	err = geminiHandleFinishReason(handle, delta.FinishReason)
	if err != nil {
		return false, contentProduced, err
	}

	return true, contentProduced, nil
}

func geminiHandleStreamUpdate(handle func(StreamDelta) error, delta StreamDelta) error {
	if delta.Thinking != "" || delta.Content != "" || delta.SearchMetadata != nil {
		err := handle(StreamDelta{
			Thinking:           delta.Thinking,
			Content:            delta.Content,
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata:     delta.SearchMetadata,
		})
		if err != nil {
			return fmt.Errorf(handleStreamDeltaErrorFormat, err)
		}
	}

	return nil
}

func geminiHandleFinishReason(handle func(StreamDelta) error, finishReason string) error {
	err := handle(StreamDelta{
		Thinking:           "",
		Content:            "",
		FinishReason:       finishReason,
		ProviderResponseID: "",
		SearchMetadata:     nil,
	})
	if err != nil {
		return fmt.Errorf(handleStreamDeltaErrorFormat, err)
	}

	return nil
}

// BuildGeminiGenerateContentRequest converts messages into a genai request.
func BuildGeminiGenerateContentRequest(
	ctx context.Context,
	request ChatCompletionRequest,
	files geminiFilesClient,
) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	contents, systemInstruction, err := geminiContentsAndSystemInstruction(
		ctx,
		RequestMessagesWithFileOrImageOnlyQueryPlaceholder(request.Messages),
		files,
	)
	if err != nil {
		return nil, nil, err
	}

	if len(contents) == 0 {
		return nil, nil, fmt.Errorf("missing gemini contents: %w", os.ErrInvalid)
	}

	extraBody, err := geminiExtraBody(request.Provider.ExtraBody)
	if err != nil {
		return nil, nil, err
	}

	// context_caching controls the local explicit-cache setup and is not a
	// GenerateContent API field.
	delete(extraBody, geminiCacheOptionKey)

	extraBody, err = defaultGeminiThoughtSummaries(extraBody)
	if err != nil {
		return nil, nil, err
	}

	extraBody = defaultGeminiServiceTier(extraBody)

	thinkingConfig, extraBody, err := geminiThinkingConfig(extraBody)
	if err != nil {
		return nil, nil, err
	}

	config := new(genai.GenerateContentConfig)

	geminiSystemInstruction := geminiRequestSystemInstruction(
		systemInstruction,
		request.Provider.EnableGrounding,
	)

	config.SystemInstruction = genai.NewContentFromText(geminiSystemInstruction, "")
	if thinkingConfig != nil {
		config.ThinkingConfig = thinkingConfig
	}

	if request.Provider.EnableGrounding {
		googleSearchTool := new(genai.Tool)
		googleSearchTool.GoogleSearch = new(genai.GoogleSearch)
		config.Tools = append(config.Tools, googleSearchTool)
	}

	if len(extraBody) > 0 {
		httpOptions := new(genai.HTTPOptions)
		httpOptions.ExtraBody = extraBody
		config.HTTPOptions = httpOptions
	}

	return contents, config, nil
}

func geminiRequestSystemInstruction(systemInstruction string, enableGrounding bool) string {
	// GeminiNoToolsInstruction is appended when no tools are available.
	toolInstruction := GeminiNoToolsInstruction
	if enableGrounding {
		toolInstruction = GeminiGroundingInstruction
	}

	if strings.TrimSpace(systemInstruction) == "" {
		return toolInstruction
	}

	return systemInstruction + "\n\n" + toolInstruction
}

// NormalizeGeminiModelAlias resolves a model alias with extra-body options.
func NormalizeGeminiModelAlias(
	model string,
	extraBody map[string]any,
) (string, map[string]any, error) {
	resolvedModel, thinkingLevel, hasAlias := geminiThinkingLevelAlias(model)
	if !hasAlias {
		return model, extraBody, nil
	}

	normalizedExtraBody := maps.Clone(extraBody)
	if normalizedExtraBody == nil {
		normalizedExtraBody = make(map[string]any, 1)
	}

	thinkingConfig, err := geminiThinkingConfigExtraBody(normalizedExtraBody)
	if err != nil {
		return "", nil, err
	}

	thinkingConfig["thinkingLevel"] = thinkingLevel
	normalizedExtraBody["thinkingConfig"] = thinkingConfig

	return resolvedModel, normalizedExtraBody, nil
}

func geminiThinkingLevelAlias(model string) (string, genai.ThinkingLevel, bool) {
	model = strings.TrimSuffix(model, ":vision")

	lowerModel := strings.ToLower(model)
	for _, alias := range []struct {
		suffix        string
		thinkingLevel genai.ThinkingLevel
	}{
		{
			suffix:        "-minimal",
			thinkingLevel: genai.ThinkingLevelMinimal,
		},
		{
			suffix:        "-low",
			thinkingLevel: genai.ThinkingLevelLow,
		},
		{
			suffix:        "-medium",
			thinkingLevel: genai.ThinkingLevelMedium,
		},
		{
			suffix:        "-high",
			thinkingLevel: genai.ThinkingLevelHigh,
		},
	} {
		if !strings.HasSuffix(lowerModel, alias.suffix) || len(model) <= len(alias.suffix) {
			continue
		}

		return model[:len(model)-len(alias.suffix)], alias.thinkingLevel, true
	}

	return "", genai.ThinkingLevelUnspecified, false
}

func geminiThinkingConfigExtraBody(extraBody map[string]any) (map[string]any, error) {
	existingThinkingConfig, thinkingConfigExists := extraBody["thinkingConfig"]
	if !thinkingConfigExists || existingThinkingConfig == nil {
		return make(map[string]any, 1), nil
	}

	thinkingConfig, ok := existingThinkingConfig.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(
			"gemini extra_body thinkingConfig must be an object when using model aliases: %w",
			os.ErrInvalid,
		)
	}

	return maps.Clone(thinkingConfig), nil
}

func geminiThinkingConfig(
	extraBody map[string]any,
) (*genai.ThinkingConfig, map[string]any, error) {
	existingThinkingConfig, thinkingConfigExists := extraBody["thinkingConfig"]
	if !thinkingConfigExists || existingThinkingConfig == nil {
		return nil, extraBody, nil
	}

	thinkingConfigMap, ok := existingThinkingConfig.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf(
			"gemini extra_body thinkingConfig must be an object: %w",
			os.ErrInvalid,
		)
	}

	thinkingConfigJSON, err := json.Marshal(thinkingConfigMap)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"marshal gemini extra_body thinkingConfig: %w",
			err,
		)
	}

	var thinkingConfig genai.ThinkingConfig

	err = json.Unmarshal(thinkingConfigJSON, &thinkingConfig)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"decode gemini extra_body thinkingConfig: %w",
			err,
		)
	}

	normalizedExtraBody := maps.Clone(extraBody)
	delete(normalizedExtraBody, "thinkingConfig")

	return &thinkingConfig, normalizedExtraBody, nil
}

func defaultGeminiThoughtSummaries(extraBody map[string]any) (map[string]any, error) {
	normalizedExtraBody := maps.Clone(extraBody)
	if normalizedExtraBody == nil {
		normalizedExtraBody = make(map[string]any, 1)
	}

	existingThinkingConfig, thinkingConfigExists := normalizedExtraBody["thinkingConfig"]
	if !thinkingConfigExists || existingThinkingConfig == nil {
		normalizedExtraBody["thinkingConfig"] = map[string]any{
			"includeThoughts": true,
		}

		return normalizedExtraBody, nil
	}

	thinkingConfig, ok := existingThinkingConfig.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(
			"gemini extra_body thinkingConfig must be an object: %w",
			os.ErrInvalid,
		)
	}

	clonedThinkingConfig := maps.Clone(thinkingConfig)
	if _, exists := clonedThinkingConfig["includeThoughts"]; !exists {
		clonedThinkingConfig["includeThoughts"] = true
	}

	normalizedExtraBody["thinkingConfig"] = clonedThinkingConfig

	return normalizedExtraBody, nil
}

func defaultGeminiServiceTier(extraBody map[string]any) map[string]any {
	normalizedExtraBody := maps.Clone(extraBody)
	if normalizedExtraBody == nil {
		normalizedExtraBody = make(map[string]any, 1)
	}

	_, hasServiceTierSnake := normalizedExtraBody["service_tier"]
	_, hasServiceTierCamel := normalizedExtraBody["serviceTier"]

	if !hasServiceTierSnake && !hasServiceTierCamel {
		normalizedExtraBody["service_tier"] = "priority"
	}

	return normalizedExtraBody
}

func buildGeminiClientConfig(
	provider ProviderRequestConfig,
	httpClient *http.Client,
) (*genai.ClientConfig, error) {
	httpOptions, err := buildGeminiClientHTTPOptions(provider)
	if err != nil {
		return nil, err
	}

	clientHTTPClient := httpClient
	if clientHTTPClient == nil {
		clientHTTPClient = http.DefaultClient
	}

	apiKey := firstAPIKey(provider.APIKeys)
	if apiKey == "" {
		apiKey = provider.APIKey
	}

	return &genai.ClientConfig{
		APIKey:      apiKey,
		Backend:     genai.BackendGeminiAPI,
		Project:     "",
		Location:    "",
		Credentials: nil,
		HTTPClient:  clientHTTPClient,
		HTTPOptions: httpOptions,
	}, nil
}

func buildGeminiClientHTTPOptions(
	provider ProviderRequestConfig,
) (genai.HTTPOptions, error) {
	baseURL, apiVersion, err := normalizeGeminiBaseURL(provider.BaseURL, provider.ExtraQuery)
	if err != nil {
		return genai.HTTPOptions{}, err
	}

	headers := geminiHeaders(provider.ExtraHeaders)

	return genai.HTTPOptions{
		BaseURL:               baseURL,
		BaseURLResourceScope:  "",
		APIVersion:            apiVersion,
		Headers:               headers,
		Timeout:               nil,
		ExtraBody:             nil,
		ExtrasRequestProvider: nil,
	}, nil
}

func normalizeGeminiBaseURL(
	baseURL string,
	extraQuery map[string]any,
) (string, string, error) {
	normalizedBaseURL, versionFromPath, err := geminiBaseURLParts(baseURL)
	if err != nil {
		return "", "", err
	}

	versionFromQuery, err := geminiAPIVersionFromQuery(extraQuery)
	if err != nil {
		return "", "", err
	}

	if versionFromPath != "" && versionFromQuery != "" && versionFromPath != versionFromQuery {
		return "", "", fmt.Errorf(
			"gemini base_url version %q does not match extra_query version %q: %w",
			versionFromPath,
			versionFromQuery,
			os.ErrInvalid,
		)
	}

	if versionFromPath == "" {
		versionFromPath = versionFromQuery
	}

	return normalizedBaseURL, versionFromPath, nil
}

func geminiBaseURLParts(baseURL string) (string, string, error) {
	trimmedBaseURL := strings.TrimSpace(baseURL)
	if trimmedBaseURL == "" {
		return "", "", nil
	}

	parsedURL, err := url.Parse(trimmedBaseURL)
	if err != nil {
		return "", "", fmt.Errorf("parse gemini base_url %q: %w", baseURL, err)
	}

	pathSegments := strings.FieldsFunc(strings.Trim(parsedURL.Path, "/"), func(r rune) bool {
		return r == '/'
	})

	versionIndex := -1
	apiVersion := ""

	for index, segment := range pathSegments {
		if !geminiAPIVersionPattern.MatchString(strings.ToLower(segment)) {
			continue
		}

		versionIndex = index
		apiVersion = strings.ToLower(segment)

		break
	}

	if versionIndex >= 0 {
		trailingSegments := pathSegments[versionIndex+1:]
		if len(trailingSegments) > 1 ||
			(len(trailingSegments) == 1 && strings.ToLower(trailingSegments[0]) != "openai") {
			return "", "", fmt.Errorf(
				"unsupported gemini base_url path %q: %w",
				baseURL,
				os.ErrInvalid,
			)
		}

		if versionIndex == 0 {
			parsedURL.Path = ""
		} else {
			parsedURL.Path = "/" + strings.Join(pathSegments[:versionIndex], "/")
		}
	} else {
		parsedURL.Path = strings.TrimRight(parsedURL.Path, "/")
	}

	parsedURL.RawQuery = ""
	parsedURL.Fragment = ""

	return strings.TrimRight(parsedURL.String(), "/"), apiVersion, nil
}

func geminiAPIVersionFromQuery(extraQuery map[string]any) (string, error) {
	apiVersion := ""

	for key, value := range extraQuery {
		normalizedKey := strings.ToLower(strings.TrimSpace(key))

		switch normalizedKey {
		case "api-version", "version":
			candidate := strings.ToLower(stringifyValue(value))
			if candidate == "" {
				continue
			}

			if !geminiAPIVersionPattern.MatchString(candidate) {
				return "", fmt.Errorf(
					"unsupported gemini API version %q in extra_query: %w",
					candidate,
					os.ErrInvalid,
				)
			}

			if apiVersion != "" && apiVersion != candidate {
				return "", fmt.Errorf(
					"conflicting gemini API versions %q and %q in extra_query: %w",
					apiVersion,
					candidate,
					os.ErrInvalid,
				)
			}

			apiVersion = candidate
		default:
			return "", fmt.Errorf(
				"unsupported gemini extra_query key %q: %w",
				key,
				os.ErrInvalid,
			)
		}
	}

	return apiVersion, nil
}

func geminiExtraBody(extraBody map[string]any) (map[string]any, error) {
	if len(extraBody) == 0 {
		return map[string]any{}, nil
	}

	clonedBody := maps.Clone(extraBody)
	for _, reservedKey := range []string{"contents", "model", "systemInstruction"} {
		if _, ok := clonedBody[reservedKey]; ok {
			return nil, fmt.Errorf(
				"gemini extra_body must not override %q: %w",
				reservedKey,
				os.ErrInvalid,
			)
		}
	}

	return clonedBody, nil
}

func geminiHeaders(extraHeaders map[string]any) http.Header {
	if len(extraHeaders) == 0 {
		return nil
	}

	headers := make(http.Header, len(extraHeaders))
	for key, value := range extraHeaders {
		headers.Set(key, stringifyValue(value))
	}

	return headers
}

func geminiContentsAndSystemInstruction(
	ctx context.Context,
	messages []ChatMessage,
	files geminiFilesClient,
) ([]*genai.Content, string, error) {
	contents := make([]*genai.Content, 0, len(messages))
	systemInstructions := make([]string, 0, 1)

	for index, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == searchtypes.MessageRoleSystem {
			text, err := geminiSystemInstructionText(message.Content)
			if err != nil {
				return nil, "", fmt.Errorf(
					"convert system message %d: %w",
					index,
					err,
				)
			}

			if text != "" {
				systemInstructions = append(systemInstructions, text)
			}

			continue
		}

		content, ok, err := geminiContentFromChatMessage(ctx, message, files)
		if err != nil {
			return nil, "", fmt.Errorf("convert message %d: %w", index, err)
		}

		if ok {
			contents = append(contents, content)
		}
	}

	if len(systemInstructions) == 0 {
		return contents, "", nil
	}

	return contents, strings.Join(systemInstructions, "\n\n"), nil
}

func geminiSystemInstructionText(content any) (string, error) {
	switch typedContent := content.(type) {
	case nil:
		return "", nil
	case string:
		return strings.TrimSpace(typedContent), nil
	case []ContentPart:
		return strings.TrimSpace(support.ContentPartsText(typedContent)), nil
	default:
		return "", fmt.Errorf(
			"unsupported system message content type %T: %w",
			content,
			os.ErrInvalid,
		)
	}
}

func geminiContentFromChatMessage(
	ctx context.Context,
	message ChatMessage,
	files geminiFilesClient,
) (*genai.Content, bool, error) {
	role, err := geminiRoleFromChatRole(message.Role)
	if err != nil {
		return nil, false, err
	}

	parts, err := geminiPartsFromMessageContent(ctx, message.Content, files)
	if err != nil {
		return nil, false, err
	}

	if len(parts) == 0 {
		return nil, false, nil
	}

	if role == genai.RoleUser {
		parts = reorderGeminiSingleImagePromptParts(parts)
	}

	return genai.NewContentFromParts(parts, role), true, nil
}

func geminiRoleFromChatRole(role string) (genai.Role, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant":
		return genai.RoleModel, nil
	case searchtypes.MessageRoleUser:
		return genai.RoleUser, nil
	default:
		return "", fmt.Errorf("unsupported gemini chat role %q: %w", role, os.ErrInvalid)
	}
}

func geminiPartsFromMessageContent(
	ctx context.Context,
	content any,
	files geminiFilesClient,
) ([]*genai.Part, error) {
	switch typedContent := content.(type) {
	case nil:
		return nil, nil
	case string:
		if typedContent == "" {
			return nil, nil
		}

		return []*genai.Part{genai.NewPartFromText(typedContent)}, nil
	case []ContentPart:
		return geminiPartsFromContentParts(ctx, typedContent, files)
	default:
		return nil, fmt.Errorf(
			"unsupported gemini content type %T: %w",
			content,
			os.ErrInvalid,
		)
	}
}

func geminiPartsFromContentParts(
	ctx context.Context,
	contentParts []ContentPart,
	files geminiFilesClient,
) ([]*genai.Part, error) {
	parts := make([]*genai.Part, 0, len(contentParts))

	for _, part := range contentParts {
		convertedPart, ok, err := geminiPartFromContentPart(ctx, part, files)
		if err != nil {
			return nil, err
		}

		if !ok {
			continue
		}

		parts = append(parts, convertedPart)
	}

	return parts, nil
}

func geminiPartFromContentPart(
	ctx context.Context,
	part ContentPart,
	files geminiFilesClient,
) (*genai.Part, bool, error) {
	partType, _ := part["type"].(string)

	switch partType {
	case searchtypes.ContentTypeText:
		return geminiTextPart(part)
	case searchtypes.ContentTypeImageURL:
		convertedPart, ok, err := geminiImagePart(ctx, files, part)
		if err != nil {
			return nil, false, err
		}

		return convertedPart, ok, nil
	case searchtypes.ContentTypeDocument:
		if !geminiSupportsDocumentPart(part) {
			return nil, false, nil
		}

		convertedPart, err := geminiUploadedMediaPart(ctx, files, part)
		if err != nil {
			return nil, false, err
		}

		return convertedPart, convertedPart != nil, nil
	case searchtypes.ContentTypeFileData:
		convertedPart, err := geminiUploadedMediaPart(ctx, files, part)
		if err != nil {
			return nil, false, err
		}

		return convertedPart, convertedPart != nil, nil
	case searchtypes.ContentTypeAudioData, searchtypes.ContentTypeVideoData:
		convertedPart, err := geminiUploadedMediaPart(ctx, files, part)
		if err != nil {
			return nil, false, err
		}

		return convertedPart, convertedPart != nil, nil
	default:
		return nil, false, fmt.Errorf(
			"unsupported gemini content part type %q: %w",
			partType,
			os.ErrInvalid,
		)
	}
}

func geminiTextPart(part ContentPart) (*genai.Part, bool, error) {
	textValue, _ := part["text"].(string)
	if textValue == "" {
		return nil, false, nil
	}

	return genai.NewPartFromText(textValue), true, nil
}

func geminiImagePart(
	ctx context.Context,
	files geminiFilesClient,
	part ContentPart,
) (*genai.Part, bool, error) {
	imageURL, err := geminiImageURL(part)
	if err != nil {
		return nil, false, err
	}

	if imageURL == "" {
		return nil, false, nil
	}

	imageData, err := ParseBase64ImageDataURL(imageURL)
	if err != nil {
		return nil, false, fmt.Errorf("parse gemini image: %w", err)
	}

	decodedLength := imageData.DecodedLengthEstimate()
	if decodedLength > geminiInlineImageByteLimit && files != nil {
		uploadedPart, uploadErr := geminiUploadedReaderPart(
			ctx,
			files,
			imageData.Decoder(),
			decodedLength,
			imageData.MimeType,
			"",
		)
		if uploadErr != nil {
			return nil, false, uploadErr
		}

		return uploadedPart, uploadedPart != nil, nil
	}

	imageBytes, err := imageData.Decode()
	if err != nil {
		return nil, false, fmt.Errorf("decode gemini image data: %w", err)
	}

	return genai.NewPartFromBytes(imageBytes, imageData.MimeType), true, nil
}

func geminiSupportsDocumentPart(part ContentPart) bool {
	mimeType, _ := part[searchtypes.ContentFieldMIMEType].(string)

	return support.NormalizedMIMEType(mimeType) == searchtypes.MimeTypePDF
}

func geminiUploadedMediaPart(
	ctx context.Context,
	files geminiFilesClient,
	part ContentPart,
) (*genai.Part, error) {
	if files == nil {
		return nil, fmt.Errorf("missing gemini file client: %w", os.ErrInvalid)
	}

	mediaBytes, mimeType, filename, err := support.AttachmentBytes(part)
	if err != nil {
		return nil, fmt.Errorf("decode gemini media part: %w", err)
	}

	if len(mediaBytes) == 0 {
		return nil, fmt.Errorf("empty gemini media bytes: %w", os.ErrInvalid)
	}

	return geminiUploadedBytesPart(ctx, files, mediaBytes, mimeType, filename)
}

func geminiUploadedBytesPart(
	ctx context.Context,
	files geminiFilesClient,
	mediaBytes []byte,
	mimeType string,
	filename string,
) (*genai.Part, error) {
	if files == nil {
		return nil, fmt.Errorf("missing gemini file client: %w", os.ErrInvalid)
	}

	if len(mediaBytes) == 0 {
		return nil, fmt.Errorf("empty gemini media bytes: %w", os.ErrInvalid)
	}

	return geminiUploadedReaderPart(
		ctx,
		files,
		bytes.NewReader(mediaBytes),
		len(mediaBytes),
		mimeType,
		filename,
	)
}

func geminiUploadedReaderPart(
	ctx context.Context,
	files geminiFilesClient,
	mediaReader io.Reader,
	mediaByteCount int,
	mimeType string,
	filename string,
) (*genai.Part, error) {
	if files == nil {
		return nil, fmt.Errorf("missing gemini file client: %w", os.ErrInvalid)
	}

	if mediaReader == nil || mediaByteCount <= 0 {
		return nil, fmt.Errorf("empty gemini media: %w", os.ErrInvalid)
	}

	uploadedFile, err := files.UploadFile(ctx, mediaReader, &genai.UploadFileConfig{
		HTTPOptions: nil,
		Name:        "",
		MIMEType:    mimeType,
		DisplayName: filename,
	})
	if err != nil {
		return nil, fmt.Errorf("upload gemini media %q: %w", filename, err)
	}

	activeFile, err := geminiWaitForFileActive(ctx, files, uploadedFile)
	if err != nil {
		return nil, err
	}

	return genai.NewPartFromFile(*activeFile), nil
}

func geminiImageURL(part ContentPart) (string, error) {
	stringMap, foundStringMap := part["image_url"].(map[string]string)
	if foundStringMap {
		return strings.TrimSpace(stringMap["url"]), nil
	}

	rawImageURL, foundMap := part["image_url"].(map[string]any)
	if !foundMap {
		return "", fmt.Errorf("decode gemini image_url content part: %w", os.ErrInvalid)
	}

	urlValue, _ := rawImageURL["url"].(string)

	return strings.TrimSpace(urlValue), nil
}

func reorderGeminiSingleImagePromptParts(parts []*genai.Part) []*genai.Part {
	const geminiSingleImagePromptPartCount = 2

	if len(parts) != geminiSingleImagePromptPartCount {
		return parts
	}

	firstIsText := geminiPartIsText(parts[0])
	secondIsText := geminiPartIsText(parts[1])
	firstIsImage := geminiPartIsImage(parts[0])
	secondIsImage := geminiPartIsImage(parts[1])

	if firstIsImage && secondIsText {
		return []*genai.Part{parts[1], parts[0]}
	}

	if firstIsText && secondIsImage {
		return parts
	}

	return parts
}

func geminiPartIsText(part *genai.Part) bool {
	if part == nil {
		return false
	}

	return strings.TrimSpace(part.Text) != ""
}

func geminiPartIsImage(part *genai.Part) bool {
	if part == nil {
		return false
	}

	if part.InlineData != nil && strings.HasPrefix(support.NormalizedMIMEType(part.InlineData.MIMEType), "image/") {
		return true
	}

	return part.FileData != nil &&
		strings.HasPrefix(support.NormalizedMIMEType(part.FileData.MIMEType), "image/")
}

func geminiWaitForFileActive(
	ctx context.Context,
	files geminiFilesClient,
	file *genai.File,
) (*genai.File, error) {
	currentFile := file

	active, err := geminiFileActive(currentFile)
	if err != nil {
		return nil, err
	}

	if active {
		return currentFile, nil
	}

	ticker := time.NewTicker(geminiFilePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf(
				"wait for gemini file %q to become active: %w",
				currentFile.Name,
				ctx.Err(),
			)
		case <-ticker.C:
			updatedFile, err := files.GetFile(ctx, currentFile.Name, nil)
			if err != nil {
				return nil, fmt.Errorf("refresh gemini file %q: %w", currentFile.Name, err)
			}

			if updatedFile == nil {
				return nil, fmt.Errorf(
					"refresh gemini file %q: missing file state: %w",
					currentFile.Name,
					os.ErrInvalid,
				)
			}

			currentFile = updatedFile

			active, err = geminiFileActive(currentFile)
			if err != nil {
				return nil, err
			}

			if active {
				return currentFile, nil
			}
		}
	}
}

func geminiFileActive(file *genai.File) (bool, error) {
	if file == nil {
		return false, fmt.Errorf("missing uploaded gemini file: %w", os.ErrInvalid)
	}

	switch file.State {
	case "", genai.FileStateActive:
		return true, nil
	case genai.FileStateProcessing, genai.FileStateUnspecified:
		return false, nil
	case genai.FileStateFailed:
		return false, geminiFileStateError(file)
	default:
		return false, fmt.Errorf(
			"gemini file %q returned unsupported processing state %q: %w",
			file.Name,
			strings.TrimSpace(string(file.State)),
			os.ErrInvalid,
		)
	}
}

func geminiFileStateError(file *genai.File) error {
	if file != nil && file.Error != nil && strings.TrimSpace(file.Error.Message) != "" {
		return fmt.Errorf(
			"gemini file %q failed processing: %s: %w",
			file.Name,
			strings.TrimSpace(file.Error.Message),
			os.ErrInvalid,
		)
	}

	name := ""
	if file != nil {
		name = file.Name
	}

	return fmt.Errorf("gemini file %q failed processing: %w", name, os.ErrInvalid)
}

func geminiStreamDelta(response *genai.GenerateContentResponse) (StreamDelta, error) {
	var delta StreamDelta
	if response == nil {
		return delta, nil
	}

	err := geminiPromptFeedbackError(response.PromptFeedback)
	if err != nil {
		return delta, err
	}

	if len(response.Candidates) > 0 {
		candidate := response.Candidates[0]
		delta.Thinking = geminiThoughtText(candidate)
		delta.Content = geminiCandidateText(candidate)
		delta.FinishReason = normalizedGeminiFinishReason(candidate.FinishReason)
		delta.SearchMetadata = geminiSearchMetadata(candidate)

		err = geminiFinishReasonError(candidate)
		if err != nil {
			return StreamDelta{
				Thinking:           delta.Thinking,
				Content:            delta.Content,
				FinishReason:       "",
				ProviderResponseID: "",
				SearchMetadata:     nil,
			}, err
		}
	}

	return delta, nil
}

func geminiSearchMetadata(candidate *genai.Candidate) *searchtypes.SearchMetadata {
	if candidate == nil || candidate.GroundingMetadata == nil {
		return nil
	}

	groundingMetadata := candidate.GroundingMetadata
	if len(groundingMetadata.WebSearchQueries) == 0 && len(groundingMetadata.GroundingChunks) == 0 {
		return nil
	}

	var resultsText strings.Builder

	for _, chunk := range groundingMetadata.GroundingChunks {
		if chunk == nil || chunk.Web == nil {
			continue
		}

		uri := strings.TrimSpace(chunk.Web.URI)
		if uri == "" {
			continue
		}

		title := strings.TrimSpace(chunk.Web.Title)
		if title != "" {
			resultsText.WriteString("Title: " + title + "\n")
		}

		resultsText.WriteString("URL: " + uri + "\n\n")
	}

	text := strings.TrimSpace(resultsText.String())
	if text == "" && len(groundingMetadata.WebSearchQueries) == 0 {
		return nil
	}

	results := make([]searchtypes.WebSearchResult, 0, 1)
	if text != "" {
		results = append(results, searchtypes.WebSearchResult{
			Query: "",
			Text:  text,
		})
	}

	return searchtypes.NewSearchMetadata(
		groundingMetadata.WebSearchQueries,
		results,
		defaultWebSearchMaxURLs,
	)
}

func geminiThoughtText(candidate *genai.Candidate) string {
	return geminiCandidatePartText(candidate, true)
}

func geminiCandidateText(candidate *genai.Candidate) string {
	return geminiCandidatePartText(candidate, false)
}

func geminiCandidatePartText(candidate *genai.Candidate, thought bool) string {
	if candidate == nil || candidate.Content == nil || len(candidate.Content.Parts) == 0 {
		return ""
	}

	var builder strings.Builder

	for _, part := range candidate.Content.Parts {
		if part == nil || part.Text == "" || part.Thought != thought {
			continue
		}

		builder.WriteString(part.Text)
	}

	return builder.String()
}

func normalizedGeminiFinishReason(finishReason genai.FinishReason) string {
	if finishReason == "" || finishReason == genai.FinishReasonUnspecified {
		return ""
	}

	return strings.ToLower(strings.TrimSpace(string(finishReason)))
}

func geminiPromptFeedbackError(feedback *genai.GenerateContentResponsePromptFeedback) error {
	if feedback == nil {
		return nil
	}

	message := strings.TrimSpace(feedback.BlockReasonMessage)
	blockReason := strings.ToLower(strings.TrimSpace(string(feedback.BlockReason)))

	if message == "" {
		message = "provider blocked the prompt"
		if blockReason != "" && blockReason != strings.ToLower(string(genai.BlockedReasonUnspecified)) {
			message += " (block_reason=" + blockReason + ")"
		}
	}

	return fmt.Errorf("%s: %w", message, os.ErrInvalid)
}

func geminiFinishReasonError(candidate *genai.Candidate) error {
	if candidate == nil {
		return nil
	}

	switch candidate.FinishReason {
	case "", genai.FinishReasonUnspecified, genai.FinishReasonStop, genai.FinishReasonMaxTokens:
		return nil
	case genai.FinishReasonSafety,
		genai.FinishReasonRecitation,
		genai.FinishReasonLanguage,
		genai.FinishReasonOther,
		genai.FinishReasonBlocklist,
		genai.FinishReasonProhibitedContent,
		genai.FinishReasonSPII,
		genai.FinishReasonMalformedFunctionCall,
		genai.FinishReasonImageSafety,
		genai.FinishReasonUnexpectedToolCall,
		genai.FinishReasonImageProhibitedContent,
		genai.FinishReasonNoImage,
		genai.FinishReasonImageRecitation,
		genai.FinishReasonImageOther:
	}

	finishReason := strings.ToLower(strings.TrimSpace(string(candidate.FinishReason)))
	message := strings.TrimSpace(candidate.FinishMessage)

	if message == "" {
		message = "provider ended the response"
		if finishReason != "" {
			message += " (finish_reason=" + finishReason + ")"
		}
	} else if finishReason != "" && !strings.Contains(strings.ToLower(message), finishReason) {
		message += " (finish_reason=" + finishReason + ")"
	}

	return fmt.Errorf("%s: %w", message, os.ErrInvalid)
}

const defaultWebSearchMaxURLs = 5

const geminiFilePollInterval = 500 * time.Millisecond

const geminiInlineImageByteLimit = 4 * 1024 * 1024
