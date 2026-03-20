package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	websiteWarningText                  = "Warning: website content unavailable"
	maxWebsiteContentRunes              = 12000
	maxWebsiteDescriptionRunes          = 500
	maxWebsiteResponseBytes             = 2 * 1024 * 1024
	minimumWebsiteContentSelectionRunes = 300
	websiteFetchAttemptCapacity         = 3
	websiteContentCandidateCapacity     = 7
	websiteSegmentCapacity              = 32
)

var websiteURLRegexp = regexp.MustCompile(
	`(?i)\bhttps?://(?:[\w-]+\.)+[a-z]{2,}(?:/[^\s<>]*)?`,
)

type websiteContentClient interface {
	fetch(ctx context.Context, loadedConfig config, rawURL string) (websitePageContent, error)
}

type websiteClient struct {
	httpClient            *http.Client
	userAgent             string
	exaContentsEndpoint   string
	tavilyExtractEndpoint string
}

type websitePageContent struct {
	URL         string
	Title       string
	Description string
	Content     string
}

func newWebsiteClient(httpClient *http.Client) websiteClient {
	return websiteClient{
		httpClient:            httpClient,
		userAgent:             youtubeUserAgent,
		exaContentsEndpoint:   defaultExaContentsEndpoint,
		tavilyExtractEndpoint: defaultTavilyExtractEndpoint,
	}
}

func (instance *bot) maybeAugmentConversationWithWebsite(
	ctx context.Context,
	loadedConfig config,
	conversation []chatMessage,
	urlExtractionText string,
) ([]chatMessage, []string, error) {
	if instance.website == nil {
		return conversation, nil, nil
	}

	websiteURLs := extractWebsiteURLs(urlExtractionText)
	if len(websiteURLs) == 0 {
		return conversation, nil, nil
	}

	augmentedConversation, _, warnings, err := augmentConversationWithConcurrentURLContent(
		ctx,
		conversation,
		websiteURLs,
		func(fetchContext context.Context, rawURL string) (websitePageContent, error) {
			return instance.website.fetch(fetchContext, loadedConfig, rawURL)
		},
		"fetch website content",
		websiteWarningText,
		formatWebsiteURLContent,
		appendWebsiteContentToConversation,
		"append website content to conversation",
	)

	return augmentedConversation, warnings, err
}

func latestUserPromptQuery(conversation []chatMessage) (string, error) {
	text, err := latestUserMessageText(conversation)
	if err != nil {
		return "", err
	}

	return parseAugmentedUserPrompt(text).UserQuery, nil
}

type exaContentsResponse struct {
	Results  []exaContentsResponseResult
	Statuses []exaContentsResponseStatus
}

type exaContentsResponseResult struct {
	Title string
	URL   string
	ID    string
	Text  string
}

type exaContentsResponseStatus struct {
	ID     string
	Status string
	Error  *exaContentsResponseErrorInfo
}

type exaContentsResponseErrorInfo struct {
	Tag            string
	HTTPStatusCode *int
}

type tavilyExtractResponse struct {
	Results       []tavilyExtractResponseResult
	FailedResults []tavilyFailedExtractResult
}

type tavilyExtractResponseResult struct {
	URL        string
	RawContent string
}

type tavilyFailedExtractResult struct {
	URL   string
	Error string
}

func (client websiteClient) fetch(
	ctx context.Context,
	loadedConfig config,
	rawURL string,
) (websitePageContent, error) {
	normalizedURL, err := normalizeWebsiteURL(rawURL)
	if err != nil {
		return websitePageContent{}, err
	}

	requestContext, cancel := context.WithTimeout(ctx, websiteRequestTimeout)
	defer cancel()

	attemptErrors := make([]error, 0, websiteFetchAttemptCapacity)

	if loadedConfig.WebSearch.exaUsesAPI() {
		pageContent, exaErr := client.fetchWithExaContents(
			requestContext,
			normalizedURL,
			loadedConfig.WebSearch.Exa.apiKeysForAttempts(),
		)
		if exaErr == nil {
			return pageContent, nil
		}

		attemptErrors = append(attemptErrors, fmt.Errorf("exa contents API: %w", exaErr))
	}

	if tavilyAPIKeys := loadedConfig.WebSearch.Tavily.apiKeysForAttempts(); len(tavilyAPIKeys) > 0 {
		pageContent, tavilyErr := client.fetchWithTavilyExtract(
			requestContext,
			normalizedURL,
			tavilyAPIKeys,
		)
		if tavilyErr == nil {
			return pageContent, nil
		}

		attemptErrors = append(attemptErrors, fmt.Errorf("tavily extract: %w", tavilyErr))
	}

	pageContent, err := client.fetchWithCurrentImplementation(requestContext, normalizedURL)
	if err == nil {
		return pageContent, nil
	}

	if len(attemptErrors) == 0 {
		return websitePageContent{}, fmt.Errorf("fetch website %q: %w", rawURL, err)
	}

	attemptErrors = append(attemptErrors, fmt.Errorf("current implementation: %w", err))

	return websitePageContent{}, fmt.Errorf("fetch website %q: %w", rawURL, errors.Join(attemptErrors...))
}

func (client websiteClient) fetchWithCurrentImplementation(
	ctx context.Context,
	requestURL string,
) (websitePageContent, error) {
	responseBody, responseURL, contentType, err := client.doRequest(ctx, requestURL)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("fetch website %q: %w", requestURL, err)
	}

	switch {
	case isHTMLContentType(contentType):
		pageContent, parseErr := parseWebsiteHTML(responseURL, responseBody)
		if parseErr != nil {
			return websitePageContent{}, fmt.Errorf("parse website html %q: %w", requestURL, parseErr)
		}

		return pageContent, nil
	case isPlainTextContentType(contentType):
		return newWebsitePageContent(responseURL, responseURL, "", string(responseBody))
	default:
		return websitePageContent{}, fmt.Errorf(
			"unsupported website content type %q for %q: %w",
			contentType,
			requestURL,
			os.ErrInvalid,
		)
	}
}

func (client websiteClient) fetchWithExaContents(
	ctx context.Context,
	requestURL string,
	apiKeys []string,
) (websitePageContent, error) {
	attemptErrors := make([]error, 0, len(apiKeys))

	for index, apiKey := range apiKeys {
		pageContent, err := client.fetchWithExaContentsOnce(ctx, requestURL, apiKey)
		if err == nil {
			return pageContent, nil
		}

		attemptErrors = append(attemptErrors, err)
		if ctx.Err() != nil || index == len(apiKeys)-1 {
			if len(attemptErrors) == 1 {
				return websitePageContent{}, err
			}

			if ctx.Err() != nil {
				return websitePageContent{}, err
			}

			return websitePageContent{}, fmt.Errorf(
				"all configured Exa API keys failed for %q: %w",
				requestURL,
				errors.Join(attemptErrors...),
			)
		}
	}

	return websitePageContent{}, fmt.Errorf("missing Exa API key attempt for %q: %w", requestURL, os.ErrInvalid)
}

func (client websiteClient) fetchWithExaContentsOnce(
	ctx context.Context,
	requestURL string,
	apiKey string,
) (websitePageContent, error) {
	requestBytes, err := json.Marshal(exaContentsRequestBody(requestURL))
	if err != nil {
		return websitePageContent{}, fmt.Errorf("marshal Exa contents request for %q: %w", requestURL, err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.exaContentsEndpoint,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("create Exa contents request for %q: %w", requestURL, err)
	}

	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("X-Api-Key", strings.TrimSpace(apiKey))

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("send Exa contents request for %q: %w", requestURL, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return websitePageContent{}, fmt.Errorf(
				"read Exa contents error response for %q after status %d: %w",
				requestURL,
				httpResponse.StatusCode,
				readErr,
			)
		}

		return websitePageContent{}, exaStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"exa contents request failed for %q with status %d: %s",
				requestURL,
				httpResponse.StatusCode,
				strings.TrimSpace(extractStructuredAPIErrorMessage(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	var rawResponse map[string]any

	err = json.NewDecoder(httpResponse.Body).Decode(&rawResponse)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("decode exa contents response for %q: %w", requestURL, err)
	}

	response, err := parseExaContentsResponse(rawResponse)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("parse exa contents response for %q: %w", requestURL, err)
	}

	err = exaContentsResponseError(response, requestURL)
	if err != nil {
		return websitePageContent{}, err
	}

	result, resultFound := exaContentsResultForURL(response, requestURL)
	if !resultFound {
		return websitePageContent{}, fmt.Errorf(
			"exa contents response contained no result for %q: %w",
			requestURL,
			os.ErrNotExist,
		)
	}

	resultURL := firstNonEmptyString(result.URL, result.ID, requestURL)

	return newWebsitePageContent(resultURL, result.Title, "", result.Text)
}

func exaContentsRequestBody(requestURL string) map[string]any {
	return map[string]any{
		"urls": []string{requestURL},
		"text": map[string]any{
			"maxCharacters": maxWebsiteContentRunes,
			"verbosity":     "compact",
			"excludeSections": []string{
				"header",
				"navigation",
				"banner",
				"sidebar",
				"footer",
			},
		},
		"livecrawlTimeout": exaContentsLivecrawlTimeoutMS,
	}
}

func parseExaContentsResponse(rawResponse map[string]any) (exaContentsResponse, error) {
	response := exaContentsResponse{
		Results:  nil,
		Statuses: nil,
	}

	rawResults, hasResults := rawResponse["results"]
	if hasResults && rawResults != nil {
		results, isList := rawResults.([]any)
		if !isList {
			return exaContentsResponse{}, fmt.Errorf("decode Exa contents results: %w", os.ErrInvalid)
		}

		response.Results = make([]exaContentsResponseResult, 0, len(results))

		for _, rawResult := range results {
			resultMap, ok := rawResult.(map[string]any)
			if !ok {
				return exaContentsResponse{}, fmt.Errorf("decode Exa contents result: %w", os.ErrInvalid)
			}

			response.Results = append(response.Results, exaContentsResponseResult{
				Title: mapStringValue(resultMap, "title"),
				URL:   mapStringValue(resultMap, "url"),
				ID:    mapStringValue(resultMap, "id"),
				Text:  mapStringValue(resultMap, "text"),
			})
		}
	}

	rawStatuses, hasStatuses := rawResponse["statuses"]
	if !hasStatuses || rawStatuses == nil {
		return response, nil
	}

	statuses, isList := rawStatuses.([]any)
	if !isList {
		return exaContentsResponse{}, fmt.Errorf("decode Exa contents statuses: %w", os.ErrInvalid)
	}

	response.Statuses = make([]exaContentsResponseStatus, 0, len(statuses))

	for _, rawStatus := range statuses {
		statusMap, ok := rawStatus.(map[string]any)
		if !ok {
			return exaContentsResponse{}, fmt.Errorf("decode Exa contents status: %w", os.ErrInvalid)
		}

		response.Statuses = append(response.Statuses, exaContentsResponseStatus{
			ID:     mapStringValue(statusMap, "id"),
			Status: mapStringValue(statusMap, "status"),
			Error:  exaContentsResponseErrorInfoValue(statusMap),
		})
	}

	return response, nil
}

func exaContentsResponseErrorInfoValue(values map[string]any) *exaContentsResponseErrorInfo {
	rawError, ok := values["error"].(map[string]any)
	if !ok {
		return nil
	}

	errorInfo := &exaContentsResponseErrorInfo{
		Tag:            mapStringValue(rawError, "tag"),
		HTTPStatusCode: mapOptionalIntValue(rawError, "httpStatusCode"),
	}

	if strings.TrimSpace(errorInfo.Tag) == "" && errorInfo.HTTPStatusCode == nil {
		return nil
	}

	return errorInfo
}

func exaContentsResponseError(response exaContentsResponse, requestURL string) error {
	for _, status := range response.Statuses {
		if !exaContentsStatusMatchesURL(status, requestURL) {
			continue
		}

		if !strings.EqualFold(strings.TrimSpace(status.Status), "error") {
			return nil
		}

		if status.Error == nil {
			return fmt.Errorf("exa contents reported an error for %q: %w", requestURL, os.ErrInvalid)
		}

		errorParts := []string{strings.TrimSpace(status.Error.Tag)}
		if status.Error.HTTPStatusCode != nil {
			errorParts = append(errorParts, fmt.Sprintf("HTTP %d", *status.Error.HTTPStatusCode))
		}

		return fmt.Errorf(
			"exa contents reported an error for %q: %s: %w",
			requestURL,
			strings.Join(errorParts, ", "),
			os.ErrInvalid,
		)
	}

	return nil
}

func exaContentsStatusMatchesURL(status exaContentsResponseStatus, requestURL string) bool {
	statusID := strings.TrimSpace(status.ID)
	if statusID == "" {
		return false
	}

	return strings.EqualFold(statusID, requestURL)
}

func exaContentsResultForURL(
	response exaContentsResponse,
	requestURL string,
) (exaContentsResponseResult, bool) {
	for _, result := range response.Results {
		if strings.EqualFold(strings.TrimSpace(result.URL), requestURL) ||
			strings.EqualFold(strings.TrimSpace(result.ID), requestURL) {
			return result, true
		}
	}

	if len(response.Results) == 0 {
		var emptyResult exaContentsResponseResult

		return emptyResult, false
	}

	return response.Results[0], true
}

func (client websiteClient) fetchWithTavilyExtract(
	ctx context.Context,
	requestURL string,
	apiKeys []string,
) (websitePageContent, error) {
	attemptErrors := make([]error, 0, len(apiKeys))

	for index, apiKey := range apiKeys {
		pageContent, err := client.fetchWithTavilyExtractOnce(ctx, requestURL, apiKey)
		if err == nil {
			return pageContent, nil
		}

		attemptErrors = append(attemptErrors, err)
		if ctx.Err() != nil || index == len(apiKeys)-1 {
			if len(attemptErrors) == 1 {
				return websitePageContent{}, err
			}

			if ctx.Err() != nil {
				return websitePageContent{}, err
			}

			return websitePageContent{}, fmt.Errorf(
				"all configured Tavily API keys failed for %q: %w",
				requestURL,
				errors.Join(attemptErrors...),
			)
		}
	}

	return websitePageContent{}, fmt.Errorf("missing Tavily API key attempt for %q: %w", requestURL, os.ErrInvalid)
}

func (client websiteClient) fetchWithTavilyExtractOnce(
	ctx context.Context,
	requestURL string,
	apiKey string,
) (websitePageContent, error) {
	requestBytes, err := json.Marshal(tavilyExtractRequestBody(requestURL))
	if err != nil {
		return websitePageContent{}, fmt.Errorf("marshal Tavily extract request for %q: %w", requestURL, err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.tavilyExtractEndpoint,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("create Tavily extract request for %q: %w", requestURL, err)
	}

	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	httpRequest.Header.Set("Content-Type", "application/json")

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("send Tavily extract request for %q: %w", requestURL, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return websitePageContent{}, fmt.Errorf(
				"read Tavily extract error response for %q after status %d: %w",
				requestURL,
				httpResponse.StatusCode,
				readErr,
			)
		}

		return websitePageContent{}, tavilyStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"tavily extract request failed for %q with status %d: %s",
				requestURL,
				httpResponse.StatusCode,
				strings.TrimSpace(extractStructuredAPIErrorMessage(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	var rawResponse map[string]any

	err = json.NewDecoder(httpResponse.Body).Decode(&rawResponse)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("decode tavily extract response for %q: %w", requestURL, err)
	}

	response, err := parseTavilyExtractResponse(rawResponse)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("parse tavily extract response for %q: %w", requestURL, err)
	}

	err = tavilyExtractResponseError(response, requestURL)
	if err != nil {
		return websitePageContent{}, err
	}

	result, resultFound := tavilyExtractResultForURL(response, requestURL)
	if !resultFound {
		return websitePageContent{}, fmt.Errorf(
			"tavily extract response contained no result for %q: %w",
			requestURL,
			os.ErrNotExist,
		)
	}

	return newWebsitePageContent(firstNonEmptyString(result.URL, requestURL), "", "", result.RawContent)
}

func tavilyExtractRequestBody(requestURL string) map[string]any {
	return map[string]any{
		"urls":          []string{requestURL},
		"extract_depth": "advanced",
		"format":        "markdown",
		"timeout":       tavilyExtractTimeoutSeconds,
	}
}

func parseTavilyExtractResponse(rawResponse map[string]any) (tavilyExtractResponse, error) {
	response := tavilyExtractResponse{
		Results:       nil,
		FailedResults: nil,
	}

	rawResults, hasResults := rawResponse["results"]
	if hasResults && rawResults != nil {
		results, isList := rawResults.([]any)
		if !isList {
			return tavilyExtractResponse{}, fmt.Errorf("decode Tavily extract results: %w", os.ErrInvalid)
		}

		response.Results = make([]tavilyExtractResponseResult, 0, len(results))

		for _, rawResult := range results {
			resultMap, ok := rawResult.(map[string]any)
			if !ok {
				return tavilyExtractResponse{}, fmt.Errorf("decode Tavily extract result: %w", os.ErrInvalid)
			}

			response.Results = append(response.Results, tavilyExtractResponseResult{
				URL:        mapStringValue(resultMap, "url"),
				RawContent: mapStringValue(resultMap, "raw_content"),
			})
		}
	}

	rawFailedResults, hasFailedResults := rawResponse["failed_results"]
	if !hasFailedResults || rawFailedResults == nil {
		return response, nil
	}

	failedResults, isList := rawFailedResults.([]any)
	if !isList {
		return tavilyExtractResponse{}, fmt.Errorf("decode Tavily extract failed results: %w", os.ErrInvalid)
	}

	response.FailedResults = make([]tavilyFailedExtractResult, 0, len(failedResults))

	for _, rawFailedResult := range failedResults {
		failedResultMap, ok := rawFailedResult.(map[string]any)
		if !ok {
			return tavilyExtractResponse{}, fmt.Errorf("decode Tavily extract failed result: %w", os.ErrInvalid)
		}

		response.FailedResults = append(response.FailedResults, tavilyFailedExtractResult{
			URL:   mapStringValue(failedResultMap, "url"),
			Error: mapStringValue(failedResultMap, "error"),
		})
	}

	return response, nil
}

func tavilyExtractResponseError(response tavilyExtractResponse, requestURL string) error {
	for _, failedResult := range response.FailedResults {
		if !strings.EqualFold(strings.TrimSpace(failedResult.URL), requestURL) {
			continue
		}

		return fmt.Errorf(
			"tavily extract reported an error for %q: %s: %w",
			requestURL,
			strings.TrimSpace(failedResult.Error),
			os.ErrInvalid,
		)
	}

	return nil
}

func tavilyExtractResultForURL(
	response tavilyExtractResponse,
	requestURL string,
) (tavilyExtractResponseResult, bool) {
	for _, result := range response.Results {
		if strings.EqualFold(strings.TrimSpace(result.URL), requestURL) {
			return result, true
		}
	}

	if len(response.Results) == 0 {
		var emptyResult tavilyExtractResponseResult

		return emptyResult, false
	}

	return response.Results[0], true
}

func mapOptionalIntValue(values map[string]any, key string) *int {
	value, exists := values[key]
	if !exists || value == nil {
		return nil
	}

	switch typedValue := value.(type) {
	case float64:
		intValue := int(typedValue)

		return &intValue
	case int:
		intValue := typedValue

		return &intValue
	default:
		return nil
	}
}

func extractStructuredAPIErrorMessage(responseBody []byte) string {
	var response map[string]any

	err := json.Unmarshal(responseBody, &response)
	if err != nil {
		return string(responseBody)
	}

	if detail, ok := response["detail"].(map[string]any); ok {
		if detailError := mapStringValue(detail, "error"); detailError != "" {
			return detailError
		}
	}

	for _, key := range []string{"error", "message"} {
		if message := mapStringValue(response, key); message != "" {
			return message
		}
	}

	return string(responseBody)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		trimmedValue := strings.TrimSpace(value)
		if trimmedValue != "" {
			return trimmedValue
		}
	}

	return ""
}

func (client websiteClient) doRequest(
	ctx context.Context,
	requestURL string,
) ([]byte, string, string, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("create website request %q: %w", requestURL, err)
	}

	httpRequest.Header.Set(
		"Accept",
		"text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.1",
	)
	httpRequest.Header.Set("Accept-Language", youtubeAcceptLanguage)
	httpRequest.Header.Set("User-Agent", client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return nil, "", "", fmt.Errorf("send website request %q: %w", requestURL, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxWebsiteResponseBytes+1))
	if err != nil {
		return nil, "", "", fmt.Errorf("read website response %q: %w", requestURL, err)
	}

	if len(responseBody) > maxWebsiteResponseBytes {
		return nil, "", "", fmt.Errorf(
			"website response %q exceeds %d bytes: %w",
			requestURL,
			maxWebsiteResponseBytes,
			os.ErrInvalid,
		)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, "", "", fmt.Errorf(
			"website request %q failed with status %d: %s: %w",
			requestURL,
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	responseURL := requestURL
	if httpResponse.Request != nil && httpResponse.Request.URL != nil {
		responseURL = httpResponse.Request.URL.String()
	}

	return responseBody, responseURL, httpResponse.Header.Get("Content-Type"), nil
}

func extractWebsiteURLs(text string) []string {
	text = normalizedURLExtractionText(text)

	matchIndices := websiteURLRegexp.FindAllStringIndex(text, -1)
	normalizedURLs := make([]string, 0, len(matchIndices))
	seenURLs := make(map[string]struct{}, len(matchIndices))

	for _, matchIndex := range matchIndices {
		rawURL := text[matchIndex[0]:matchIndex[1]]

		normalizedURL, err := normalizeWebsiteURL(rawURL)
		if err != nil {
			continue
		}

		if _, ok := seenURLs[normalizedURL]; ok {
			continue
		}

		seenURLs[normalizedURL] = struct{}{}
		normalizedURLs = append(normalizedURLs, normalizedURL)
	}

	return normalizedURLs
}

func normalizeWebsiteURL(rawURL string) (string, error) {
	cleanedURL := cleanWebsiteURL(rawURL)

	if cleanedURL == "" {
		return "", fmt.Errorf("empty website url: %w", os.ErrInvalid)
	}

	if !strings.Contains(cleanedURL, "://") {
		cleanedURL = "https://" + cleanedURL
	}

	parsedURL, err := url.Parse(cleanedURL)
	if err != nil {
		return "", fmt.Errorf("parse website url %q: %w", rawURL, err)
	}

	if !isWebsiteScheme(parsedURL.Scheme) || strings.TrimSpace(parsedURL.Hostname()) == "" {
		return "", fmt.Errorf("unsupported website url %q: %w", rawURL, os.ErrInvalid)
	}

	if isExcludedWebsiteHost(parsedURL.Hostname()) {
		return "", fmt.Errorf("excluded website host in %q: %w", rawURL, os.ErrInvalid)
	}

	parsedURL.Scheme = strings.ToLower(parsedURL.Scheme)
	parsedURL.Host = strings.ToLower(parsedURL.Host)
	parsedURL.Fragment = ""

	return parsedURL.String(), nil
}

func cleanWebsiteURL(rawURL string) string {
	cleanedURL := strings.TrimSpace(rawURL)
	cleanedURL = strings.Trim(cleanedURL, `"'<>[]`)
	cleanedURL = strings.TrimRight(cleanedURL, ".,!?;:")

	for strings.HasSuffix(cleanedURL, ")") &&
		strings.Count(cleanedURL, "(") < strings.Count(cleanedURL, ")") {
		cleanedURL = strings.TrimSuffix(cleanedURL, ")")
	}

	return cleanedURL
}

func isWebsiteScheme(scheme string) bool {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http", "https":
		return true
	default:
		return false
	}
}

func isExcludedWebsiteHost(host string) bool {
	return isTikTokHost(host) ||
		isYouTubeHost(host) ||
		isRedditHost(host) ||
		isFacebookHost(host)
}

func isTikTokHost(host string) bool {
	normalizedHost := strings.ToLower(strings.TrimSpace(host))
	normalizedHost = strings.TrimPrefix(normalizedHost, "www.")

	return normalizedHost == "tiktok.com" ||
		normalizedHost == "tnktok.com" ||
		strings.HasSuffix(normalizedHost, ".tiktok.com") ||
		strings.HasSuffix(normalizedHost, ".tnktok.com")
}

func isYouTubeHost(host string) bool {
	normalizedHost := strings.ToLower(strings.TrimSpace(host))
	normalizedHost = strings.TrimPrefix(normalizedHost, "www.")

	return normalizedHost == "youtu.be" ||
		normalizedHost == "youtube.com" ||
		normalizedHost == "youtube-nocookie.com" ||
		strings.HasSuffix(normalizedHost, ".youtube.com") ||
		strings.HasSuffix(normalizedHost, ".youtube-nocookie.com")
}

func isFacebookHost(host string) bool {
	normalizedHost := strings.ToLower(strings.TrimSpace(host))
	normalizedHost = strings.TrimPrefix(normalizedHost, "www.")

	return normalizedHost == "facebook.com" ||
		normalizedHost == "fb.watch" ||
		strings.HasSuffix(normalizedHost, ".facebook.com") ||
		strings.HasSuffix(normalizedHost, ".fb.watch")
}

func isHTMLContentType(contentType string) bool {
	trimmedContentType := strings.ToLower(strings.TrimSpace(contentType))

	return trimmedContentType == "" ||
		strings.HasPrefix(trimmedContentType, "text/html") ||
		strings.HasPrefix(trimmedContentType, "application/xhtml+xml")
}

func isPlainTextContentType(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/plain")
}

func parseWebsiteHTML(pageURL string, responseBody []byte) (websitePageContent, error) {
	document, err := html.Parse(bytes.NewReader(responseBody))
	if err != nil {
		return websitePageContent{}, fmt.Errorf("parse html document: %w", err)
	}

	title := normalizeWebsiteText(extractWebsiteTitle(document))
	description := truncateRunes(
		normalizeWebsiteText(extractWebsiteDescription(document)),
		maxWebsiteDescriptionRunes,
	)

	return newWebsitePageContent(
		pageURL,
		title,
		description,
		extractWebsiteBodyText(document),
	)
}

func newWebsitePageContent(
	pageURL string,
	title string,
	description string,
	content string,
) (websitePageContent, error) {
	trimmedURL := strings.TrimSpace(pageURL)
	if trimmedURL == "" {
		return websitePageContent{}, fmt.Errorf("missing website url: %w", os.ErrInvalid)
	}

	trimmedTitle := strings.TrimSpace(title)
	trimmedDescription := truncateRunes(strings.TrimSpace(description), maxWebsiteDescriptionRunes)

	trimmedContent := truncateRunes(strings.TrimSpace(content), maxWebsiteContentRunes)
	if trimmedContent == "" {
		trimmedContent = trimmedDescription
	}

	if trimmedContent == "" {
		return websitePageContent{}, fmt.Errorf("extract website content: %w", os.ErrInvalid)
	}

	if trimmedTitle == "" {
		trimmedTitle = trimmedURL
	}

	return websitePageContent{
		URL:         trimmedURL,
		Title:       trimmedTitle,
		Description: trimmedDescription,
		Content:     trimmedContent,
	}, nil
}

func extractWebsiteTitle(document *html.Node) string {
	titleNode := findWebsiteNode(document, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.DataAtom == atom.Title
	})
	if titleNode == nil || titleNode.FirstChild == nil {
		return ""
	}

	return titleNode.FirstChild.Data
}

func extractWebsiteDescription(document *html.Node) string {
	metaNode := findWebsiteNode(document, func(node *html.Node) bool {
		if node.Type != html.ElementNode || node.DataAtom != atom.Meta {
			return false
		}

		name := strings.ToLower(strings.TrimSpace(htmlAttribute(node, "name")))
		property := strings.ToLower(strings.TrimSpace(htmlAttribute(node, "property")))

		return name == "description" || property == "og:description" || name == "twitter:description"
	})
	if metaNode == nil {
		return ""
	}

	return htmlAttribute(metaNode, "content")
}

func extractWebsiteBodyText(document *html.Node) string {
	candidates := websiteContentCandidates(document)
	fallback := ""

	for index, candidate := range candidates {
		text := renderWebsiteText(candidate)
		if text == "" {
			continue
		}

		if index == len(candidates)-1 || runeCount(text) >= minimumWebsiteContentSelectionRunes {
			return text
		}

		if fallback == "" || runeCount(text) > runeCount(fallback) {
			fallback = text
		}
	}

	return fallback
}

func websiteContentCandidates(document *html.Node) []*html.Node {
	candidates := make([]*html.Node, 0, websiteContentCandidateCapacity)

	appendCandidate := func(candidate *html.Node) {
		if candidate == nil || slices.Contains(candidates, candidate) {
			return
		}

		candidates = append(candidates, candidate)
	}

	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return htmlAttribute(node, "id") == "mw-content-text"
	}))
	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "mw-parser-output")
	}))
	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.DataAtom == atom.Article
	}))
	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.DataAtom == atom.Main
	}))
	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return strings.EqualFold(htmlAttribute(node, "role"), "main")
	}))
	appendCandidate(findWebsiteNode(document, hasWebsiteContentHint))
	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.DataAtom == atom.Body
	}))

	return candidates
}

func hasWebsiteContentHint(node *html.Node) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}

	if node.DataAtom != atom.Div && node.DataAtom != atom.Section {
		return false
	}

	for _, keyword := range []string{
		"article",
		"content",
		"entry",
		"main",
		"post",
		"story",
		"wiki",
	} {
		if containsFold(htmlAttribute(node, "id"), keyword) ||
			containsFold(htmlAttribute(node, "class"), keyword) {
			return true
		}
	}

	return false
}

func findWebsiteNode(node *html.Node, predicate func(*html.Node) bool) *html.Node {
	if node == nil {
		return nil
	}

	if predicate(node) {
		return node
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		match := findWebsiteNode(child, predicate)
		if match != nil {
			return match
		}
	}

	return nil
}

func renderWebsiteText(root *html.Node) string {
	segments := make([]string, 0, websiteSegmentCapacity)

	var current strings.Builder

	flush := func() {
		text := normalizeWebsiteText(current.String())
		if text == "" {
			current.Reset()

			return
		}

		segments = append(segments, text)

		current.Reset()
	}

	var walk func(*html.Node)

	walk = func(node *html.Node) {
		if node == nil {
			return
		}

		if node.Type == html.TextNode {
			appendWebsiteTextChunk(&current, node.Data)

			return
		}

		if node.Type != html.ElementNode {
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				walk(child)
			}

			return
		}

		if shouldSkipWebsiteNode(node) {
			return
		}

		if node.DataAtom == atom.Br || isWebsiteBlockNode(node) {
			flush()
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}

		if isWebsiteBlockNode(node) {
			flush()
		}
	}

	walk(root)
	flush()

	return truncateRunes(strings.Join(segments, "\n"), maxWebsiteContentRunes)
}

func appendWebsiteTextChunk(builder *strings.Builder, rawText string) {
	text := normalizeWebsiteText(rawText)
	if text == "" {
		return
	}

	if builder.Len() > 0 {
		builder.WriteByte(' ')
	}

	builder.WriteString(text)
}

func normalizeWebsiteText(text string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
}

func shouldSkipWebsiteNode(node *html.Node) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}

	if hasHTMLAttribute(node, "hidden") || strings.EqualFold(htmlAttribute(node, "aria-hidden"), "true") {
		return true
	}

	if containsFold(htmlAttribute(node, "style"), "display:none") ||
		containsFold(htmlAttribute(node, "style"), "visibility:hidden") {
		return true
	}

	if isIgnoredWebsiteAtom(node.DataAtom) {
		return true
	}

	for _, keyword := range []string{
		"advert",
		"banner",
		"breadcrumb",
		"breadcrumbs",
		"comment",
		"comments",
		"consent",
		"cookie",
		"footer",
		"header",
		"infobox",
		"modal",
		"nav",
		"navbox",
		"reference",
		"references",
		"related",
		"share",
		"sidebar",
		"social",
		"subscribe",
		"toc",
	} {
		if containsFold(htmlAttribute(node, "id"), keyword) ||
			containsFold(htmlAttribute(node, "class"), keyword) {
			return true
		}
	}

	return false
}

func isIgnoredWebsiteAtom(dataAtom atom.Atom) bool {
	return slices.Contains([]atom.Atom{
		atom.Aside,
		atom.Button,
		atom.Canvas,
		atom.Datalist,
		atom.Dialog,
		atom.Figure,
		atom.Footer,
		atom.Form,
		atom.Header,
		atom.Iframe,
		atom.Img,
		atom.Input,
		atom.Label,
		atom.Menu,
		atom.Nav,
		atom.Noscript,
		atom.Object,
		atom.Option,
		atom.Picture,
		atom.Script,
		atom.Select,
		atom.Style,
		atom.Sup,
		atom.Svg,
		atom.Textarea,
		atom.Video,
	}, dataAtom)
}

func isWebsiteBlockNode(node *html.Node) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}

	return slices.Contains([]atom.Atom{
		atom.Article,
		atom.Blockquote,
		atom.Div,
		atom.H1,
		atom.H2,
		atom.H3,
		atom.H4,
		atom.H5,
		atom.H6,
		atom.Li,
		atom.Main,
		atom.Ol,
		atom.P,
		atom.Pre,
		atom.Section,
		atom.Table,
		atom.Tbody,
		atom.Td,
		atom.Th,
		atom.Thead,
		atom.Tr,
		atom.Ul,
	}, node.DataAtom)
}

func htmlAttribute(node *html.Node, key string) string {
	if node == nil {
		return ""
	}

	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, key) {
			return attribute.Val
		}
	}

	return ""
}

func hasHTMLAttribute(node *html.Node, key string) bool {
	return htmlAttribute(node, key) != ""
}

func hasHTMLClass(node *html.Node, className string) bool {
	return slices.Contains(strings.Fields(htmlAttribute(node, "class")), className)
}

func formatWebsiteURLContent(contents []websitePageContent) string {
	formattedContents := make([]string, 0, len(contents))

	for _, content := range contents {
		lines := []string{
			"URL: " + content.URL,
			"Title: " + content.Title,
		}

		if trimmedDescription := strings.TrimSpace(content.Description); trimmedDescription != "" {
			lines = append(lines, "Description: "+trimmedDescription)
		}

		pageContent := strings.TrimSpace(content.Content)
		if pageContent == "" {
			pageContent = "[No extracted content]"
		}

		lines = append(lines, "Content:\n"+pageContent)
		formattedContents = append(formattedContents, strings.Join(lines, "\n"))
	}

	return strings.Join(formattedContents, "\n\n")
}
