package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"iter"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

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

type geminiModelsClientFactory func(
	context.Context,
	*genai.ClientConfig,
) (geminiModelsClient, error)

type geminiClient struct {
	httpClient *http.Client
	newClient  geminiModelsClientFactory
}

func newGeminiClient(httpClient *http.Client) geminiClient {
	return geminiClient{
		httpClient: httpClient,
		newClient: func(
			ctx context.Context,
			config *genai.ClientConfig,
		) (geminiModelsClient, error) {
			client, err := genai.NewClient(ctx, config)
			if err != nil {
				return nil, fmt.Errorf("new genai client: %w", err)
			}

			return client.Models, nil
		},
	}
}

func (client geminiClient) streamChatCompletion(
	ctx context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	contents, generateConfig, err := buildGeminiGenerateContentRequest(request)
	if err != nil {
		return fmt.Errorf("build gemini request: %w", err)
	}

	modelsClient, err := client.newClient(ctx, &genai.ClientConfig{
		APIKey:      strings.TrimSpace(request.Provider.APIKey),
		Backend:     genai.BackendGeminiAPI,
		Project:     "",
		Location:    "",
		Credentials: nil,
		HTTPClient:  client.httpClient,
		HTTPOptions: genai.HTTPOptions{
			BaseURL:               "",
			APIVersion:            "",
			Headers:               nil,
			Timeout:               nil,
			ExtraBody:             nil,
			ExtrasRequestProvider: nil,
		},
	})
	if err != nil {
		return fmt.Errorf("create gemini client: %w", err)
	}

	for response, streamErr := range modelsClient.GenerateContentStream(
		ctx,
		request.Model,
		contents,
		generateConfig,
	) {
		if streamErr != nil {
			return fmt.Errorf("stream gemini content: %w", streamErr)
		}

		delta := geminiStreamDelta(response)
		if delta.Content == "" && delta.FinishReason == "" {
			continue
		}

		err = handle(delta)
		if err != nil {
			return fmt.Errorf("handle stream delta: %w", err)
		}
	}

	return nil
}

func buildGeminiGenerateContentRequest(
	request chatCompletionRequest,
) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	contents, systemInstruction, err := geminiContentsAndSystemInstruction(request.Messages)
	if err != nil {
		return nil, nil, err
	}

	if len(contents) == 0 {
		return nil, nil, fmt.Errorf("missing gemini contents: %w", os.ErrInvalid)
	}

	httpOptions, err := buildGeminiHTTPOptions(request.Provider)
	if err != nil {
		return nil, nil, err
	}

	config := new(genai.GenerateContentConfig)
	if systemInstruction != "" {
		config.SystemInstruction = genai.NewContentFromText(systemInstruction, "")
	}

	if !geminiHTTPOptionsIsZero(httpOptions) {
		config.HTTPOptions = &httpOptions
	}

	return contents, config, nil
}

func buildGeminiHTTPOptions(provider providerRequestConfig) (genai.HTTPOptions, error) {
	baseURL, apiVersion, err := normalizeGeminiBaseURL(provider.BaseURL, provider.ExtraQuery)
	if err != nil {
		return genai.HTTPOptions{}, err
	}

	extraBody, err := geminiExtraBody(provider.ExtraBody)
	if err != nil {
		return genai.HTTPOptions{}, err
	}

	headers := geminiHeaders(provider.ExtraHeaders)

	options := genai.HTTPOptions{
		BaseURL:               "",
		APIVersion:            "",
		Headers:               nil,
		Timeout:               nil,
		ExtraBody:             nil,
		ExtrasRequestProvider: nil,
	}
	options.BaseURL = baseURL
	options.APIVersion = apiVersion
	options.Headers = headers
	options.ExtraBody = extraBody

	return options, nil
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
	messages []chatMessage,
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

		content, ok, err := geminiContentFromChatMessage(message)
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

func geminiContentFromChatMessage(message chatMessage) (*genai.Content, bool, error) {
	role, err := geminiRoleFromChatRole(message.Role)
	if err != nil {
		return nil, false, err
	}

	parts, err := geminiPartsFromMessageContent(message.Content)
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

func geminiPartsFromMessageContent(content any) ([]*genai.Part, error) {
	switch typedContent := content.(type) {
	case nil:
		return nil, nil
	case string:
		if typedContent == "" {
			return nil, nil
		}

		return []*genai.Part{genai.NewPartFromText(typedContent)}, nil
	case []contentPart:
		parts := make([]*genai.Part, 0, len(typedContent))

		for _, part := range typedContent {
			partType, _ := part["type"].(string)

			switch partType {
			case contentTypeText:
				textValue, _ := part["text"].(string)
				if textValue == "" {
					continue
				}

				parts = append(parts, genai.NewPartFromText(textValue))
			case contentTypeImageURL:
				imageURL, err := geminiImageURL(part)
				if err != nil {
					return nil, err
				}

				if imageURL == "" {
					continue
				}

				imageBytes, mimeType, err := geminiInlineImage(imageURL)
				if err != nil {
					return nil, err
				}

				parts = append(parts, genai.NewPartFromBytes(imageBytes, mimeType))
			default:
				return nil, fmt.Errorf(
					"unsupported gemini content part type %q: %w",
					partType,
					os.ErrInvalid,
				)
			}
		}

		return parts, nil
	default:
		return nil, fmt.Errorf(
			"unsupported gemini content type %T: %w",
			content,
			os.ErrInvalid,
		)
	}
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

func geminiStreamDelta(response *genai.GenerateContentResponse) streamDelta {
	var delta streamDelta
	if response == nil {
		return delta
	}

	delta.Content = response.Text()
	if len(response.Candidates) > 0 {
		delta.FinishReason = strings.ToLower(string(response.Candidates[0].FinishReason))
	}

	return delta
}

func geminiHTTPOptionsIsZero(options genai.HTTPOptions) bool {
	return options.BaseURL == "" &&
		options.APIVersion == "" &&
		len(options.Headers) == 0 &&
		len(options.ExtraBody) == 0 &&
		options.Timeout == nil &&
		options.ExtrasRequestProvider == nil
}
