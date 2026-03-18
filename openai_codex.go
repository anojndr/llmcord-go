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
	"strings"
)

const (
	defaultOpenAICodexBaseURL  = "https://chatgpt.com/backend-api"
	openAICodexJWTClaimPath    = "https://api.openai.com/auth"
	openAICodexOriginator      = "pi"
	openAICodexUserAgent       = "llmcord-go"
	openAICodexHeaderBeta      = "Openai-Beta"
	openAICodexHeaderAccount   = "Chatgpt-Account-Id"
	openAICodexHeaderOrigin    = "Originator"
	openAICodexRequestFields   = 7
	openAICodexJWTPartCount    = 3
	openAICodexRoleSystem      = "system"
	openAICodexEventError      = "error"
	openAICodexAuto            = "auto"
	openAICodexReasoningHigh   = "high"
	openAICodexVerbosityMedium = "medium"
)

type openAICodexClient struct {
	httpClient *http.Client
}

func newOpenAICodexClient(httpClient *http.Client) openAICodexClient {
	return openAICodexClient{httpClient: httpClient}
}

func (client openAICodexClient) streamChatCompletion(
	ctx context.Context,
	request chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	requestBody, err := buildOpenAICodexRequestBody(request)
	if err != nil {
		return fmt.Errorf("build codex request body: %w", err)
	}

	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("marshal codex request: %w", err)
	}

	requestURL, err := buildOpenAICodexURL(request.Provider.BaseURL, request.Provider.ExtraQuery)
	if err != nil {
		return fmt.Errorf("build codex request url: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		requestURL,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return fmt.Errorf("create codex request: %w", err)
	}

	err = populateOpenAICodexHeaders(httpRequest, request.Provider)
	if err != nil {
		return fmt.Errorf("build codex headers: %w", err)
	}

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("send codex request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return fmt.Errorf(
				"read codex error response after status %d: %w",
				httpResponse.StatusCode,
				readErr,
			)
		}

		return providerStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"codex request failed with status %d: %s",
				httpResponse.StatusCode,
				strings.TrimSpace(string(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	terminalEventSeen := false

	doneSeen, err := consumeServerSentEvents(httpResponse.Body, func(payload []byte) error {
		terminal, payloadErr := handleOpenAICodexStreamPayload(payload, handle)
		if terminal {
			terminalEventSeen = true
		}

		return payloadErr
	})
	if err != nil {
		return fmt.Errorf("consume codex stream: %w", err)
	}

	if !doneSeen && !terminalEventSeen {
		return fmt.Errorf(
			"codex stream ended before a terminal response event or [DONE]: %w",
			io.ErrUnexpectedEOF,
		)
	}

	return nil
}

func buildOpenAICodexRequestBody(request chatCompletionRequest) (map[string]any, error) {
	input, instructions, err := openAICodexInputAndInstructions(request.Messages)
	if err != nil {
		return nil, err
	}

	requestBody := make(map[string]any, len(request.Provider.ExtraBody)+openAICodexRequestFields)
	requestBody["model"] = request.Model
	requestBody["store"] = false
	requestBody["stream"] = true
	requestBody["input"] = input
	requestBody["include"] = []string{"reasoning.encrypted_content"}
	requestBody["tool_choice"] = openAICodexAuto
	requestBody["parallel_tool_calls"] = true

	if instructions != "" {
		requestBody["instructions"] = instructions
	}

	maps.Copy(requestBody, request.Provider.ExtraBody)
	normalizeOpenAICodexRequestBody(requestBody, request.Model)

	return requestBody, nil
}

func normalizeOpenAICodexModelAlias(model string, extraBody map[string]any) (string, map[string]any) {
	resolvedModel, reasoningEffort, hasAlias := openAICodexReasoningEffortAlias(model)
	if !hasAlias {
		return model, extraBody
	}

	normalizedExtraBody := maps.Clone(extraBody)
	if normalizedExtraBody == nil {
		normalizedExtraBody = make(map[string]any, 1)
	}

	reasoningConfig := openAICodexReasoningConfigExtraBody(normalizedExtraBody)
	reasoningConfig["effort"] = normalizeOpenAICodexReasoningEffort(resolvedModel, reasoningEffort)
	normalizedExtraBody["reasoning"] = reasoningConfig
	delete(normalizedExtraBody, "reasoning_effort")

	return resolvedModel, normalizedExtraBody
}

func openAICodexReasoningEffortAlias(model string) (string, string, bool) {
	lowerModel := strings.ToLower(model)
	for _, alias := range []struct {
		suffix          string
		reasoningEffort string
	}{
		{
			suffix:          "-none",
			reasoningEffort: "none",
		},
		{
			suffix:          "-minimal",
			reasoningEffort: "minimal",
		},
		{
			suffix:          "-low",
			reasoningEffort: "low",
		},
		{
			suffix:          "-medium",
			reasoningEffort: "medium",
		},
		{
			suffix:          "-high",
			reasoningEffort: "high",
		},
		{
			suffix:          "-xhigh",
			reasoningEffort: "xhigh",
		},
	} {
		if !strings.HasSuffix(lowerModel, alias.suffix) || len(model) <= len(alias.suffix) {
			continue
		}

		return model[:len(model)-len(alias.suffix)], alias.reasoningEffort, true
	}

	return "", "", false
}

func normalizeOpenAICodexReasoningEffort(model string, effort string) string {
	normalizedEffort := strings.ToLower(strings.TrimSpace(effort))
	if normalizedEffort == "" {
		return ""
	}

	modelID := strings.TrimSpace(model)
	if slashIndex := strings.LastIndex(modelID, "/"); slashIndex >= 0 {
		modelID = modelID[slashIndex+1:]
	}

	switch {
	case modelID == "gpt-5.1-codex-mini":
		switch normalizedEffort {
		case openAICodexReasoningHigh, "xhigh":
			return openAICodexReasoningHigh
		default:
			return openAICodexVerbosityMedium
		}
	case modelID == "gpt-5.1" && normalizedEffort == "xhigh":
		return openAICodexReasoningHigh
	case (strings.HasPrefix(modelID, "gpt-5.2") ||
		strings.HasPrefix(modelID, "gpt-5.3") ||
		strings.HasPrefix(modelID, "gpt-5.4")) && normalizedEffort == "minimal":
		return "low"
	default:
		return normalizedEffort
	}
}

func normalizeOpenAICodexRequestBody(requestBody map[string]any, model string) {
	if verbosity, ok := requestBody["verbosity"]; ok {
		textConfig := nestedRequestBodyMap(requestBody, "text")
		textConfig["verbosity"] = verbosity

		delete(requestBody, "verbosity")
	}

	textConfig := nestedRequestBodyMap(requestBody, "text")
	if _, exists := textConfig["verbosity"]; !exists {
		textConfig["verbosity"] = openAICodexVerbosityMedium
	}

	if reasoningEffort, ok := requestBody["reasoning_effort"]; ok {
		reasoningConfig := nestedRequestBodyMap(requestBody, "reasoning")
		if _, exists := reasoningConfig["effort"]; !exists {
			reasoningConfig["effort"] = reasoningEffort
		}

		delete(requestBody, "reasoning_effort")
	}

	if reasoningSummary, ok := requestBody["reasoning_summary"]; ok {
		reasoningConfig := nestedRequestBodyMap(requestBody, "reasoning")
		if _, exists := reasoningConfig["summary"]; !exists {
			reasoningConfig["summary"] = reasoningSummary
		}

		delete(requestBody, "reasoning_summary")
	}

	if _, reasoningConfigOK := requestBody["reasoning"].(map[string]any); !reasoningConfigOK {
		return
	}

	reasoningConfig := nestedRequestBodyMap(requestBody, "reasoning")

	effort, effortOK := reasoningConfig["effort"].(string)
	if !effortOK {
		return
	}

	reasoningConfig["effort"] = normalizeOpenAICodexReasoningEffort(model, effort)
	if _, exists := reasoningConfig["summary"]; !exists {
		reasoningConfig["summary"] = openAICodexAuto
	}
}

func openAICodexReasoningConfigExtraBody(extraBody map[string]any) map[string]any {
	existingReasoningConfig, reasoningConfigExists := extraBody["reasoning"]
	if !reasoningConfigExists || existingReasoningConfig == nil {
		return make(map[string]any, 1)
	}

	reasoningConfig, ok := existingReasoningConfig.(map[string]any)
	if !ok {
		return make(map[string]any, 1)
	}

	return maps.Clone(reasoningConfig)
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

func openAICodexInputAndInstructions(messages []chatMessage) ([]map[string]any, string, error) {
	input := make([]map[string]any, 0, len(messages))
	systemPrompts := make([]string, 0, 1)

	for index, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))

		switch role {
		case openAICodexRoleSystem:
			text, err := openAICodexSystemInstruction(message.Content)
			if err != nil {
				return nil, "", fmt.Errorf("convert system message %d: %w", index, err)
			}

			if text != "" {
				systemPrompts = append(systemPrompts, text)
			}
		case messageRoleUser:
			convertedMessage, ok, err := openAICodexUserMessage(message.Content)
			if err != nil {
				return nil, "", fmt.Errorf("convert user message %d: %w", index, err)
			}

			if ok {
				input = append(input, convertedMessage)
			}
		case messageRoleAssistant:
			convertedMessage, ok, err := openAICodexAssistantMessage(message.Content)
			if err != nil {
				return nil, "", fmt.Errorf("convert assistant message %d: %w", index, err)
			}

			if ok {
				input = append(input, convertedMessage)
			}
		default:
			return nil, "", fmt.Errorf("unsupported codex chat role %q: %w", message.Role, os.ErrInvalid)
		}
	}

	return input, strings.Join(systemPrompts, "\n\n"), nil
}

func openAICodexSystemInstruction(content any) (string, error) {
	switch typedContent := content.(type) {
	case nil:
		return "", nil
	case string:
		return strings.TrimSpace(typedContent), nil
	case []contentPart:
		return strings.TrimSpace(contentPartsText(typedContent)), nil
	default:
		return "", fmt.Errorf("unsupported system message content type %T: %w", content, os.ErrInvalid)
	}
}

func openAICodexUserMessage(content any) (map[string]any, bool, error) {
	parts, err := openAICodexUserParts(content)
	if err != nil {
		return nil, false, err
	}

	if len(parts) == 0 {
		return nil, false, nil
	}

	return map[string]any{
		"type":    "message",
		"role":    messageRoleUser,
		"content": parts,
	}, true, nil
}

func openAICodexUserParts(content any) ([]map[string]any, error) {
	switch typedContent := content.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(typedContent) == "" {
			return nil, nil
		}

		return []map[string]any{{
			"type": "input_text",
			"text": typedContent,
		}}, nil
	case []contentPart:
		parts := make([]map[string]any, 0, len(typedContent))
		for _, part := range typedContent {
			partType, _ := part["type"].(string)

			switch partType {
			case contentTypeText:
				textValue, _ := part["text"].(string)
				if strings.TrimSpace(textValue) == "" {
					continue
				}

				parts = append(parts, map[string]any{
					"type": "input_text",
					"text": textValue,
				})
			case contentTypeImageURL:
				imageURL, err := geminiImageURL(part)
				if err != nil {
					return nil, err
				}

				if imageURL == "" {
					continue
				}

				parts = append(parts, map[string]any{
					"type":      "input_image",
					"image_url": imageURL,
					"detail":    openAICodexAuto,
				})
			default:
				return nil, fmt.Errorf(
					"unsupported codex content part type %q: %w",
					partType,
					os.ErrInvalid,
				)
			}
		}

		return parts, nil
	default:
		return nil, fmt.Errorf("unsupported codex user content type %T: %w", content, os.ErrInvalid)
	}
}

func openAICodexAssistantMessage(content any) (map[string]any, bool, error) {
	text, err := openAICodexAssistantText(content)
	if err != nil {
		return nil, false, err
	}

	if text == "" {
		return nil, false, nil
	}

	return map[string]any{
		"type":   "message",
		"role":   messageRoleAssistant,
		"status": "completed",
		"content": []map[string]any{{
			"type": "output_text",
			"text": text,
		}},
	}, true, nil
}

func openAICodexAssistantText(content any) (string, error) {
	switch typedContent := content.(type) {
	case nil:
		return "", nil
	case string:
		return strings.TrimSpace(typedContent), nil
	case []contentPart:
		if contentPartsContainNonText(typedContent) {
			return "", fmt.Errorf("unsupported codex assistant content type %T: %w", content, os.ErrInvalid)
		}

		return strings.TrimSpace(contentPartsText(typedContent)), nil
	default:
		return "", fmt.Errorf("unsupported codex assistant content type %T: %w", content, os.ErrInvalid)
	}
}

func buildOpenAICodexURL(baseURL string, extraQuery map[string]any) (string, error) {
	trimmedBaseURL := strings.TrimSpace(baseURL)
	if trimmedBaseURL == "" {
		trimmedBaseURL = defaultOpenAICodexBaseURL
	}

	parsedURL, err := url.Parse(trimmedBaseURL)
	if err != nil {
		return "", fmt.Errorf("parse codex base url %q: %w", trimmedBaseURL, err)
	}

	trimmedPath := strings.TrimRight(parsedURL.Path, "/")
	switch {
	case strings.HasSuffix(trimmedPath, "/codex/responses"):
		parsedURL.Path = trimmedPath
	case strings.HasSuffix(trimmedPath, "/codex"):
		parsedURL.Path = path.Join(trimmedPath, "responses")
	default:
		parsedURL.Path = path.Join(trimmedPath, "codex", "responses")
	}

	queryValues := parsedURL.Query()
	for key, value := range extraQuery {
		queryValues.Set(key, stringifyValue(value))
	}

	parsedURL.RawQuery = queryValues.Encode()

	return parsedURL.String(), nil
}

func populateOpenAICodexHeaders(httpRequest *http.Request, provider providerRequestConfig) error {
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set(openAICodexHeaderBeta, "responses=experimental")
	httpRequest.Header.Set("User-Agent", openAICodexUserAgent)
	httpRequest.Header.Set(openAICodexHeaderOrigin, openAICodexOriginator)

	if provider.primaryAPIKey() != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+provider.primaryAPIKey())
	}

	for key, value := range provider.ExtraHeaders {
		httpRequest.Header.Set(key, stringifyValue(value))
	}

	if strings.TrimSpace(httpRequest.Header.Get("Authorization")) == "" {
		return providerAPIKeyError{
			Err: fmt.Errorf("missing codex authorization token: %w", os.ErrInvalid),
		}
	}

	if strings.TrimSpace(httpRequest.Header.Get(openAICodexHeaderAccount)) != "" {
		return nil
	}

	accountID, err := openAICodexAccountIDFromAuthorization(httpRequest.Header.Get("Authorization"))
	if err != nil {
		return providerAPIKeyError{Err: err}
	}

	httpRequest.Header.Set(openAICodexHeaderAccount, accountID)

	return nil
}

func openAICodexAccountIDFromAuthorization(authorization string) (string, error) {
	token := strings.TrimSpace(authorization)
	if token == "" {
		return "", fmt.Errorf("missing codex authorization token: %w", os.ErrInvalid)
	}

	if bearerToken, found := strings.CutPrefix(token, "Bearer "); found {
		token = strings.TrimSpace(bearerToken)
	}

	accountID, err := openAICodexAccountID(token)
	if err != nil {
		return "", fmt.Errorf("extract codex account id: %w", err)
	}

	return accountID, nil
}

func openAICodexAccountID(token string) (string, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != openAICodexJWTPartCount {
		return "", fmt.Errorf("decode JWT payload: %w", os.ErrInvalid)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}

	var payload map[string]any

	err = json.Unmarshal(payloadBytes, &payload)
	if err != nil {
		return "", fmt.Errorf("parse JWT payload: %w", err)
	}

	rawAuth, found := payload[openAICodexJWTClaimPath]
	if !found {
		return "", fmt.Errorf("missing JWT auth claim: %w", os.ErrInvalid)
	}

	authClaim, ok := rawAuth.(map[string]any)
	if !ok {
		return "", fmt.Errorf("decode JWT auth claim: %w", os.ErrInvalid)
	}

	accountID, _ := authClaim["chatgpt_account_id"].(string)
	accountID = strings.TrimSpace(accountID)

	if accountID == "" {
		return "", fmt.Errorf("missing chatgpt_account_id: %w", os.ErrInvalid)
	}

	return accountID, nil
}

func handleOpenAICodexStreamPayload(payload []byte, handle func(streamDelta) error) (bool, error) {
	var envelope struct {
		Type     string `json:"type"`
		Delta    string `json:"delta"`
		Message  string `json:"message"`
		Code     string `json:"code"`
		Response struct {
			Status string `json:"status"`
			Error  struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			IncompleteDetails struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}

	err := json.Unmarshal(payload, &envelope)
	if err != nil {
		return false, fmt.Errorf("decode codex stream payload: %w", err)
	}

	switch envelope.Type {
	case "response.output_text.delta", "response.refusal.delta":
		if envelope.Delta == "" {
			return false, nil
		}

		return false, handle(streamDelta{Content: envelope.Delta, FinishReason: ""})
	case "response.completed", "response.done", "response.incomplete":
		status := strings.TrimSpace(envelope.Response.Status)
		if status == "" && envelope.Type == "response.incomplete" {
			status = "incomplete"
		}

		finishReason := finishReasonStop
		if status != "" {
			finishReason = openAICodexFinishReason(status)
		}

		return true, handle(streamDelta{
			Content:      "",
			FinishReason: finishReason,
		})
	case "response.failed":
		errorCode := strings.TrimSpace(envelope.Response.Error.Code)
		errorText := strings.TrimSpace(envelope.Response.Error.Message)

		if errorCode != "" && errorText != "" {
			errorText = errorCode + ": " + errorText
		}

		if errorText == "" {
			incompleteReason := strings.TrimSpace(envelope.Response.IncompleteDetails.Reason)
			if incompleteReason != "" {
				errorText = "incomplete: " + incompleteReason
			}
		}

		if errorText == "" {
			errorText = "codex response failed"
		}

		return false, fmt.Errorf("%s: %w", errorText, os.ErrInvalid)
	case openAICodexEventError:
		errorText := strings.TrimSpace(envelope.Message)
		if errorText == "" {
			errorText = strings.TrimSpace(envelope.Code)
		}

		if errorText == "" {
			errorText = "codex stream error"
		}

		return false, fmt.Errorf("%s: %w", errorText, os.ErrInvalid)
	default:
		return false, nil
	}
}

func openAICodexFinishReason(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "completed", "in_progress", "queued":
		return finishReasonStop
	case "incomplete":
		return finishReasonLength
	default:
		return "error"
	}
}
