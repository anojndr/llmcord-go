package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"google.golang.org/genai"
)

var geminiAPIVersionPattern = regexp.MustCompile(`^v[0-9]+(?:(?:alpha|beta)[0-9]*)?$`)

type geminiModelsClient interface {
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
	geminiModelsClient
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

type geminiClientFactory func(
	context.Context,
	*genai.ClientConfig,
) (geminiAPIClient, error)

type geminiClient struct {
	httpClient *http.Client
	newClient  geminiClientFactory
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
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	clientConfig, err := buildGeminiClientConfig(request.Provider, client.httpClient)
	if err != nil {
		return fmt.Errorf("build gemini client config: %w", err)
	}

	apiClient, err := client.newClient(ctx, clientConfig)
	if err != nil {
		return fmt.Errorf("create gemini client: %w", err)
	}

	contents, generateConfig, err := buildGeminiGenerateContentRequest(
		ctx,
		request,
		apiClient,
	)
	if err != nil {
		return fmt.Errorf("build gemini request: %w", err)
	}

	finishSeen := false

	for response, streamErr := range apiClient.GenerateContentStream(
		ctx,
		request.Model,
		contents,
		generateConfig,
	) {
		if streamErr != nil {
			return fmt.Errorf("stream gemini content: %w", streamErr)
		}

		delta, processErr := geminiStreamDelta(response)
		if processErr != nil {
			err = geminiHandleStreamUpdate(handle, delta)
			if err != nil {
				return err
			}

			return fmt.Errorf("process gemini stream response: %w", processErr)
		}

		err = geminiHandleStreamUpdate(handle, delta)
		if err != nil {
			return err
		}

		if delta.FinishReason == "" {
			continue
		}

		finishSeen = true

		err = geminiHandleFinishReason(handle, delta.FinishReason)
		if err != nil {
			return err
		}
	}

	if !finishSeen {
		return fmt.Errorf("gemini stream ended without finish reason: %w", io.ErrUnexpectedEOF)
	}

	return nil
}

func geminiHandleStreamUpdate(handle func(streamDelta) error, delta streamDelta) error {
	if delta.Thinking != "" || delta.Content != "" {
		err := handle(streamDelta{
			Thinking:     delta.Thinking,
			Content:      delta.Content,
			FinishReason: "",
			Usage:        nil,
		})
		if err != nil {
			return fmt.Errorf("handle stream delta: %w", err)
		}
	}

	if delta.Usage != nil {
		err := handle(streamDelta{
			Thinking:     "",
			Content:      "",
			FinishReason: "",
			Usage:        cloneTokenUsage(delta.Usage),
		})
		if err != nil {
			return fmt.Errorf("handle stream delta: %w", err)
		}
	}

	return nil
}

func geminiHandleFinishReason(handle func(streamDelta) error, finishReason string) error {
	err := handle(streamDelta{
		Thinking:     "",
		Content:      "",
		FinishReason: finishReason,
		Usage:        nil,
	})
	if err != nil {
		return fmt.Errorf("handle stream delta: %w", err)
	}

	return nil
}

func buildGeminiGenerateContentRequest(
	ctx context.Context,
	request chatCompletionRequest,
	files geminiFilesClient,
) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	contents, systemInstruction, err := geminiContentsAndSystemInstruction(
		ctx,
		request.Messages,
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

	extraBody, err = defaultGeminiThoughtSummaries(extraBody)
	if err != nil {
		return nil, nil, err
	}

	thinkingConfig, extraBody, err := geminiThinkingConfig(extraBody)
	if err != nil {
		return nil, nil, err
	}

	config := new(genai.GenerateContentConfig)
	if systemInstruction != "" {
		config.SystemInstruction = genai.NewContentFromText(systemInstruction, "")
	}

	if thinkingConfig != nil {
		config.ThinkingConfig = thinkingConfig
	}

	if len(extraBody) > 0 {
		httpOptions := new(genai.HTTPOptions)
		httpOptions.ExtraBody = extraBody
		config.HTTPOptions = httpOptions
	}

	return contents, config, nil
}

func normalizeGeminiModelAlias(
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

func buildGeminiClientConfig(
	provider providerRequestConfig,
	httpClient *http.Client,
) (*genai.ClientConfig, error) {
	httpOptions, err := buildGeminiClientHTTPOptions(provider)
	if err != nil {
		return nil, err
	}

	return &genai.ClientConfig{
		APIKey:      provider.primaryAPIKey(),
		Backend:     genai.BackendGeminiAPI,
		Project:     "",
		Location:    "",
		Credentials: nil,
		HTTPClient:  httpClient,
		HTTPOptions: httpOptions,
	}, nil
}

func buildGeminiClientHTTPOptions(
	provider providerRequestConfig,
) (genai.HTTPOptions, error) {
	baseURL, apiVersion, err := normalizeGeminiBaseURL(provider.BaseURL, provider.ExtraQuery)
	if err != nil {
		return genai.HTTPOptions{}, err
	}

	headers := geminiHeaders(provider.ExtraHeaders)

	return genai.HTTPOptions{
		BaseURL:               baseURL,
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
	messages []chatMessage,
	files geminiFilesClient,
) ([]*genai.Content, string, error) {
	contents := make([]*genai.Content, 0, len(messages))
	systemInstructions := make([]string, 0, 1)

	for index, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "system" {
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
	case []contentPart:
		return strings.TrimSpace(contentPartsText(typedContent)), nil
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
	message chatMessage,
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

	return genai.NewContentFromParts(parts, role), true, nil
}

func geminiRoleFromChatRole(role string) (genai.Role, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant":
		return genai.RoleModel, nil
	case messageRoleUser:
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
	case []contentPart:
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
	contentParts []contentPart,
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
	part contentPart,
	files geminiFilesClient,
) (*genai.Part, bool, error) {
	partType, _ := part["type"].(string)

	switch partType {
	case contentTypeText:
		return geminiTextPart(part)
	case contentTypeImageURL:
		convertedPart, ok, err := geminiImagePart(part)
		if err != nil {
			return nil, false, err
		}

		return convertedPart, ok, nil
	case contentTypeDocument:
		if !geminiSupportsDocumentPart(part) {
			return nil, false, nil
		}

		convertedPart, err := geminiUploadedMediaPart(ctx, files, part)
		if err != nil {
			return nil, false, err
		}

		return convertedPart, convertedPart != nil, nil
	case contentTypeAudioData, contentTypeVideoData:
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

func geminiTextPart(part contentPart) (*genai.Part, bool, error) {
	textValue, _ := part["text"].(string)
	if textValue == "" {
		return nil, false, nil
	}

	return genai.NewPartFromText(textValue), true, nil
}

func geminiImagePart(part contentPart) (*genai.Part, bool, error) {
	imageURL, err := geminiImageURL(part)
	if err != nil {
		return nil, false, err
	}

	if imageURL == "" {
		return nil, false, nil
	}

	imageBytes, mimeType, err := geminiInlineImage(imageURL)
	if err != nil {
		return nil, false, err
	}

	return genai.NewPartFromBytes(imageBytes, mimeType), true, nil
}

func geminiSupportsDocumentPart(part contentPart) bool {
	mimeType, _ := part[contentFieldMIMEType].(string)

	return normalizedMIMEType(mimeType) == mimeTypePDF
}

func geminiUploadedMediaPart(
	ctx context.Context,
	files geminiFilesClient,
	part contentPart,
) (*genai.Part, error) {
	if files == nil {
		return nil, fmt.Errorf("missing gemini file client: %w", os.ErrInvalid)
	}

	mediaBytes, mimeType, filename, err := attachmentBinaryData(part)
	if err != nil {
		return nil, err
	}

	if len(mediaBytes) == 0 {
		return nil, fmt.Errorf("empty gemini media bytes: %w", os.ErrInvalid)
	}

	uploadedFile, err := files.UploadFile(ctx, bytes.NewReader(mediaBytes), &genai.UploadFileConfig{
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

func geminiImageURL(part contentPart) (string, error) {
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

func geminiInlineImage(imageURL string) ([]byte, string, error) {
	if !strings.HasPrefix(imageURL, "data:") {
		return nil, "", fmt.Errorf(
			"unsupported gemini image URL %q: %w",
			imageURL,
			os.ErrInvalid,
		)
	}

	metadata, payload, found := strings.Cut(strings.TrimPrefix(imageURL, "data:"), ",")
	if !found {
		return nil, "", fmt.Errorf("parse gemini data URL %q: %w", imageURL, os.ErrInvalid)
	}

	segments := strings.Split(metadata, ";")
	mimeType := "application/octet-stream"

	if len(segments) > 0 && strings.TrimSpace(segments[0]) != "" {
		parsedMediaType, _, err := mime.ParseMediaType(strings.TrimSpace(segments[0]))
		if err != nil {
			return nil, "", fmt.Errorf(
				"parse gemini data URL media type %q: %w",
				imageURL,
				err,
			)
		}

		mimeType = parsedMediaType
	}

	hasBase64Encoding := false

	for _, segment := range segments[1:] {
		if strings.EqualFold(strings.TrimSpace(segment), "base64") {
			hasBase64Encoding = true

			break
		}
	}

	if !hasBase64Encoding {
		return nil, "", fmt.Errorf(
			"unsupported gemini image URL encoding %q: %w",
			imageURL,
			os.ErrInvalid,
		)
	}

	imageBytes, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", fmt.Errorf("decode gemini image data: %w", err)
	}

	return imageBytes, mimeType, nil
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

	waitContext, cancel := context.WithTimeout(ctx, geminiFileProcessingTimeout)
	defer cancel()

	ticker := time.NewTicker(geminiFilePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-waitContext.Done():
			return nil, fmt.Errorf(
				"wait for gemini file %q to become active: %w",
				currentFile.Name,
				waitContext.Err(),
			)
		case <-ticker.C:
			updatedFile, err := files.GetFile(waitContext, currentFile.Name, nil)
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

func geminiStreamDelta(response *genai.GenerateContentResponse) (streamDelta, error) {
	var delta streamDelta
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

		err = geminiFinishReasonError(candidate)
		if err != nil {
			return streamDelta{
				Thinking:     delta.Thinking,
				Content:      delta.Content,
				FinishReason: "",
				Usage:        nil,
			}, err
		}
	}

	delta.Usage = geminiStreamUsage(response.UsageMetadata)

	return delta, nil
}

func geminiStreamUsage(metadata *genai.GenerateContentResponseUsageMetadata) *tokenUsage {
	if metadata == nil {
		return nil
	}

	return &tokenUsage{
		Input:  int(metadata.PromptTokenCount + metadata.ToolUsePromptTokenCount),
		Output: int(metadata.CandidatesTokenCount + metadata.ThoughtsTokenCount),
	}
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
