package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	providers "llmcord-go/internal/providers"
	"net"
	"net/http"
	"net/netip"
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
	minimumWebsiteContentSelectionRunes = 300
	websiteContentCandidateCapacity     = 7
	websiteSegmentCapacity              = 32
)

var (
	errUnsafeWebsiteAddress = errors.New("unsafe website address")
	websiteURLRegexp        = regexp.MustCompile(
		`(?i)\bhttps?://(?:[\w-]+\.)+[a-z]{2,}(?:/[^\s<>]*)?`,
	)
)

const (
	websiteFetchErrorFormat       = "fetch website %q: %w"
	websiteResolveHostErrorFormat = "resolve website host %q: %w"
)

type websiteFetcher interface {
	fetch(ctx context.Context, loadedConfig config, rawURL string) (websitePageContent, error)
}

type websiteLookupIPFunc func(context.Context, string) ([]netip.Addr, error)

type websiteClient struct {
	httpClient              *http.Client
	userAgent               string
	exaContentsEndpoint     string
	tavilyExtractEndpoint   string
	firecrawlScrapeEndpoint string
	tinyFishFetchEndpoint   string
	lookupIP                websiteLookupIPFunc
	keys                    *apiKeyRotator
}

type websitePageContent struct {
	URL         string
	Title       string
	Description string
	Content     string
}

func newWebsiteClient(httpClient *http.Client) websiteClient {
	return websiteClient{
		httpClient:              httpClient,
		userAgent:               youtubeUserAgent,
		exaContentsEndpoint:     defaultExaContentsEndpoint,
		tavilyExtractEndpoint:   defaultTavilyExtractEndpoint,
		firecrawlScrapeEndpoint: defaultFirecrawlScrapeEndpoint,
		tinyFishFetchEndpoint:   defaultTinyFishFetchEndpoint,
		lookupIP:                defaultWebsiteLookupIP,
		keys:                    newAPIKeyRotator(),
	}
}

func (instance *bot) prepareWebsiteAugmentation(
	ctx context.Context,
	loadedConfig config,
	urlExtractionText string,
) (preparedConversationAugmentation, error) {
	if instance.website == nil {
		return emptyPreparedConversationAugmentation(), nil
	}

	websiteURLs := extractWebsiteURLsForProvider(urlExtractionText)
	if len(websiteURLs) == 0 {
		return emptyPreparedConversationAugmentation(), nil
	}

	extractionOrder := loadedConfig.WebSearch.ExtractionOrder
	if len(extractionOrder) == 0 {
		extractionOrder = defaultWebExtractionOrder
	}

	// If the first configured extraction provider in the order is TinyFish, use batch fetch
	if len(extractionOrder) > 0 && extractionOrder[0] == webExtractionProviderTinyFish && len(loadedConfig.WebSearch.TinyFish.apiKeys()) > 0 {
		switch wc := instance.website.(type) {
		case websiteClient:
			return instance.prepareTinyFishWebsiteAugmentationBatch(ctx, loadedConfig, websiteURLs, wc)
		case *websiteClient:
			if wc != nil {
				return instance.prepareTinyFishWebsiteAugmentationBatch(ctx, loadedConfig, websiteURLs, *wc)
			}
		}
	}

	return prepareConcurrentURLContentAugmentation(
		ctx,
		websiteURLs,
		func(fetchContext context.Context, rawURL string) (websitePageContent, error) {
			return instance.website.fetch(fetchContext, loadedConfig, rawURL)
		},
		"fetch website content",
		websiteWarningText,
		formatWebsiteURLContent,
		appendWebsiteContentToConversation,
	)
}

func (instance *bot) prepareTinyFishWebsiteAugmentationBatch(
	ctx context.Context,
	loadedConfig config,
	rawURLs []string,
	wc websiteClient,
) (preparedConversationAugmentation, error) {
	apiKeys := loadedConfig.WebSearch.TinyFish.apiKeys()
	if len(apiKeys) == 0 {
		return emptyPreparedConversationAugmentation(), nil
	}

	type validationResult struct {
		raw        string
		normalized string
		err        error
	}

	validationResults := runTasksConcurrently(
		ctx,
		externalRequestConcurrency,
		len(rawURLs),
		func(taskCtx context.Context, index int) (validationResult, error) {
			rawURL := rawURLs[index]

			normalizedURL, err := normalizeWebsiteURL(rawURL)
			if err != nil {
				return validationResult{raw: rawURL, err: err}, nil
			}

			parsedURL, err := url.Parse(normalizedURL)
			if err != nil {
				return validationResult{raw: rawURL, err: fmt.Errorf("parse normalized website url %q: %w", normalizedURL, err)}, nil
			}

			if err := validateWebsiteRequestURL(taskCtx, parsedURL, wc.lookupWebsiteIP()); err != nil {
				return validationResult{raw: rawURL, err: fmt.Errorf("validate website url %q: %w", rawURL, err)}, nil
			}

			return validationResult{raw: rawURL, normalized: normalizedURL}, nil
		},
	)
	validNormalized := make([]string, 0, len(rawURLs))
	rawForNormalized := make(map[string]string)
	seenNormalized := make(map[string]struct{})
	hasValidationFailure := false

	for _, res := range validationResults {
		if res.value.err != nil {
			logWarn("fetch website content", res.value.err, "url", res.value.raw)

			hasValidationFailure = true

			continue
		}

		lowerNorm := strings.ToLower(strings.TrimSpace(res.value.normalized))
		if _, seen := seenNormalized[lowerNorm]; seen {
			continue
		}

		seenNormalized[lowerNorm] = struct{}{}

		validNormalized = append(validNormalized, res.value.normalized)
		rawForNormalized[res.value.normalized] = res.value.raw
	}

	if len(validNormalized) == 0 {
		warnings := []string(nil)
		if hasValidationFailure {
			warnings = []string{websiteWarningText}
		}

		return warningPreparedConversationAugmentation(warnings), nil
	}

	batchCount := (len(validNormalized) + 9) / 10

	type batchFetchResult struct {
		response  tinyFishFetchResponse
		batchURLs []string
	}

	batchResults := runTasksConcurrently(
		ctx,
		externalRequestConcurrency,
		batchCount,
		func(taskCtx context.Context, index int) (batchFetchResult, error) {
			start := index * 10

			end := start + 10
			if end > len(validNormalized) {
				end = len(validNormalized)
			}

			batch := validNormalized[start:end]
			apiKey := firstAPIKey(wc.keys.rotate(apiKeys))

			resp, err := wc.fetchTinyFishBatch(taskCtx, apiKey, batch)
			if err != nil {
				for _, u := range batch {
					raw := rawForNormalized[u]
					logWarn("fetch website content", err, "url", raw)
				}

				return batchFetchResult{}, err
			}

			return batchFetchResult{response: resp, batchURLs: batch}, nil
		},
	)
	hasFetchFailure := hasValidationFailure
	fetchedPageMap := make(map[string]websitePageContent)

	for _, br := range batchResults {
		if br.err != nil {
			hasFetchFailure = true
			continue
		}

		resp := br.value.response
		errorMap := make(map[string]string)

		for _, fe := range resp.Errors {
			key := strings.ToLower(strings.TrimSpace(fe.URL))
			if key != "" {
				errorMap[key] = fe.Error
				if raw, ok := rawForNormalized[fe.URL]; ok {
					logWarn("fetch website content", fmt.Errorf("TinyFish fetch reported an error for %q: %s: %w", fe.URL, fe.Error, os.ErrInvalid), "url", raw)
				} else {
					for _, batchURL := range br.value.batchURLs {
						if strings.EqualFold(batchURL, fe.URL) {
							raw := rawForNormalized[batchURL]
							logWarn("fetch website content", fmt.Errorf("TinyFish fetch reported an error for %q: %s: %w", fe.URL, fe.Error, os.ErrInvalid), "url", raw)

							break
						}
					}
				}
			}
		}

		for _, result := range resp.Results {
			rawText := tinyFishFetchResultText(result.Text)

			rawText = truncateRunes(strings.TrimSpace(rawText), loadedConfig.WebSearch.TinyFish.maxCharsPerResult())

			if rawText == "" {
				if raw, ok := rawForNormalized[result.URL]; ok {
					logWarn("fetch website content", fmt.Errorf("TinyFish fetch returned empty content for %q: %w", result.URL, os.ErrInvalid), "url", raw)
				} else if raw, ok := rawForNormalized[result.FinalURL]; ok {
					logWarn("fetch website content", fmt.Errorf("TinyFish fetch returned empty content for %q: %w", result.FinalURL, os.ErrInvalid), "url", raw)
				} else {
					for _, batchURL := range br.value.batchURLs {
						if strings.EqualFold(batchURL, result.URL) || strings.EqualFold(batchURL, result.FinalURL) {
							raw := rawForNormalized[batchURL]
							logWarn("fetch website content", fmt.Errorf("TinyFish fetch returned empty content for %q: %w", result.URL, os.ErrInvalid), "url", raw)

							break
						}
					}
				}

				hasFetchFailure = true

				continue
			}

			isErrored := false

			for _, candidateURL := range []string{result.URL, result.FinalURL} {
				trimmed := strings.TrimSpace(candidateURL)
				if trimmed == "" {
					continue
				}

				lower := strings.ToLower(trimmed)
				if _, exists := errorMap[lower]; exists {
					isErrored = true
					break
				}
			}

			if isErrored {
				hasFetchFailure = true
				continue
			}

			title := ""
			if result.Title != nil {
				title = strings.TrimSpace(*result.Title)
			}

			description := ""
			if result.Description != nil {
				description = strings.TrimSpace(*result.Description)
			}

			resultURL := firstNonEmptyString(result.FinalURL, result.URL, "")
			if strings.TrimSpace(resultURL) == "" {
				continue
			}

			var (
				pageContent websitePageContent
				err         error
			)

			candidateTitle := title
			if candidateTitle == "" {
				candidateTitle = resultURL
			}

			pageContent, err = newWebsitePageContent(resultURL, candidateTitle, description, rawText)
			if err != nil {
				hasFetchFailure = true
				continue
			}
			// Map via batch input URLs for canonicalization tolerance (slash-insensitive)
			matchedInputLower := ""

			for _, batchURL := range br.value.batchURLs {
				trimBatch := strings.TrimSpace(batchURL)
				trimResultURL := strings.TrimSpace(result.URL)

				trimFinalURL := strings.TrimSpace(result.FinalURL)
				if strings.EqualFold(trimBatch, trimResultURL) || strings.EqualFold(trimBatch, trimFinalURL) ||
					strings.EqualFold(strings.TrimSuffix(trimBatch, "/"), strings.TrimSuffix(trimResultURL, "/")) ||
					strings.EqualFold(strings.TrimSuffix(trimBatch, "/"), strings.TrimSuffix(trimFinalURL, "/")) {
					matchedInputLower = strings.ToLower(trimBatch)
					break
				}
			}

			if matchedInputLower != "" {
				if _, already := fetchedPageMap[matchedInputLower]; !already {
					fetchedPageMap[matchedInputLower] = pageContent
				}
				// Also map slash variants for lookup tolerance
				trimmedMatched := strings.TrimSuffix(matchedInputLower, "/")
				if trimmedMatched != matchedInputLower {
					if _, already := fetchedPageMap[trimmedMatched]; !already {
						fetchedPageMap[trimmedMatched] = pageContent
					}
				} else {
					withSlash := matchedInputLower + "/"
					if _, already := fetchedPageMap[withSlash]; !already {
						fetchedPageMap[withSlash] = pageContent
					}
				}
			}

			for _, origURL := range []string{result.URL, result.FinalURL} {
				trimmedOrig := strings.TrimSpace(origURL)
				if trimmedOrig == "" {
					continue
				}

				lowerOrig := strings.ToLower(trimmedOrig)
				if _, already := fetchedPageMap[lowerOrig]; !already {
					fetchedPageMap[lowerOrig] = pageContent
				}
			}

			lowerResultURL := strings.ToLower(strings.TrimSpace(resultURL))
			if _, already := fetchedPageMap[lowerResultURL]; !already {
				fetchedPageMap[lowerResultURL] = pageContent
			}
		}

		for key := range errorMap {
			hasFetchFailure = true
			_ = key
		}

		if len(resp.Results) == 0 && len(resp.Errors) > 0 {
			hasFetchFailure = true
		}
	}

	orderedContents := make([]websitePageContent, 0, len(validNormalized))
	seenContent := make(map[string]struct{})

	for _, normalized := range validNormalized {
		lower := strings.ToLower(strings.TrimSpace(normalized))

		pageContent, ok := fetchedPageMap[lower]
		if !ok {
			// Try slash-insensitive lookup
			trimmed := strings.TrimSuffix(lower, "/")
			if trimmed != lower {
				pageContent, ok = fetchedPageMap[trimmed]
			} else {
				pageContent, ok = fetchedPageMap[lower+"/"]
			}

			if !ok {
				continue
			}
		}

		urlKey := strings.ToLower(strings.TrimSpace(pageContent.URL))
		if _, dup := seenContent[urlKey]; dup {
			continue
		}

		seenContent[urlKey] = struct{}{}

		orderedContents = append(orderedContents, pageContent)
	}

	if len(orderedContents) == 0 {
		warnings := []string(nil)
		if hasFetchFailure {
			warnings = []string{websiteWarningText}
		}

		return warningPreparedConversationAugmentation(warnings), nil
	}

	formattedContent := formatWebsiteURLContent(orderedContents)

	warnings := []string(nil)
	if hasFetchFailure {
		warnings = []string{websiteWarningText}
	}

	return newPreparedConversationAugmentation(
		warnings,
		nil,
		func(conversation []chatMessage) ([]chatMessage, error) {
			return appendWebsiteContentToConversation(conversation, formattedContent)
		},
	), nil
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

	parsedURL, err := url.Parse(normalizedURL)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("parse normalized website url %q: %w", normalizedURL, err)
	}

	err = validateWebsiteRequestURL(ctx, parsedURL, client.lookupWebsiteIP())
	if err != nil {
		return websitePageContent{}, fmt.Errorf("validate website url %q: %w", rawURL, err)
	}

	extractionOrder := loadedConfig.WebSearch.ExtractionOrder
	if len(extractionOrder) == 0 {
		extractionOrder = defaultWebExtractionOrder
	}

	var (
		attemptErrs   []error
		attemptsCount int
	)

	for _, provider := range extractionOrder {
		switch provider {
		case webExtractionProviderFirecrawl:
			firecrawlAPIKeys := loadedConfig.WebSearch.Firecrawl.apiKeys()
			if len(firecrawlAPIKeys) == 0 {
				continue
			}

			attemptsCount++
			firecrawlAPIKey := firstAPIKey(client.keys.rotate(firecrawlAPIKeys))

			pageContent, firecrawlErr := client.fetchWithFirecrawlScrape(
				ctx,
				normalizedURL,
				firecrawlAPIKey,
				loadedConfig.WebSearch.Firecrawl.maxMarkdownCharacters(),
			)
			if firecrawlErr == nil {
				return pageContent, nil
			}

			attemptErrs = append(attemptErrs, firecrawlErr)

		case webExtractionProviderTinyFish:
			tinyFishAPIKeys := loadedConfig.WebSearch.TinyFish.apiKeys()
			if len(tinyFishAPIKeys) == 0 {
				continue
			}

			attemptsCount++
			tinyFishAPIKey := firstAPIKey(client.keys.rotate(tinyFishAPIKeys))

			pageContent, tinyFishErr := client.fetchWithTinyFishFetch(
				ctx,
				normalizedURL,
				tinyFishAPIKey,
				loadedConfig.WebSearch.TinyFish.maxCharsPerResult(),
			)
			if tinyFishErr == nil {
				return pageContent, nil
			}

			attemptErrs = append(attemptErrs, tinyFishErr)

		case webExtractionProviderExa:
			if !loadedConfig.WebSearch.exaUsesAPI() {
				continue
			}

			attemptsCount++
			exaAPIKey := firstAPIKey(client.keys.rotate(loadedConfig.WebSearch.Exa.apiKeys()))

			pageContent, exaErr := client.fetchWithExaContents(
				ctx,
				normalizedURL,
				exaAPIKey,
				loadedConfig.WebSearch.Exa.livecrawlTimeoutMS(),
				loadedConfig.WebSearch.Exa.textMaxCharacters(),
			)
			if exaErr == nil {
				return pageContent, nil
			}

			attemptErrs = append(attemptErrs, exaErr)

		case webExtractionProviderTavily:
			tavilyAPIKeys := loadedConfig.WebSearch.Tavily.apiKeys()
			if len(tavilyAPIKeys) == 0 {
				continue
			}

			attemptsCount++
			tavilyAPIKey := firstAPIKey(client.keys.rotate(tavilyAPIKeys))

			pageContent, tavilyErr := client.fetchWithTavilyExtract(
				ctx,
				normalizedURL,
				tavilyAPIKey,
				loadedConfig.WebSearch.Tavily.maxCharsPerResult(),
			)
			if tavilyErr == nil {
				return pageContent, nil
			}

			attemptErrs = append(attemptErrs, tavilyErr)
		}
	}

	if attemptsCount == 0 {
		return websitePageContent{}, fmt.Errorf("no website fetch provider configured for %q: %w", rawURL, os.ErrNotExist)
	}

	return websitePageContent{}, fmt.Errorf(websiteFetchErrorFormat, rawURL, errors.Join(attemptErrs...))
}

type firecrawlScrapeResponse struct {
	Success bool
	Data    *firecrawlScrapeData
	Error   string
}

type firecrawlScrapeData struct {
	Markdown string
	Metadata firecrawlScrapeMetadata
}

type firecrawlScrapeMetadata struct {
	Title       string
	SourceURL   string
	Description string
}

func (client websiteClient) fetchWithFirecrawlScrape(
	ctx context.Context,
	requestURL string,
	apiKey string,
	maxMarkdownCharacters int,
) (websitePageContent, error) {
	requestBytes, err := json.Marshal(firecrawlScrapeRequestBody(requestURL))
	if err != nil {
		return websitePageContent{}, fmt.Errorf("marshal Firecrawl scrape request for %q: %w", requestURL, err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.firecrawlScrapeEndpoint,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("create Firecrawl scrape request for %q: %w", requestURL, err)
	}

	httpRequest.Header.Set("Accept", applicationJSONContentType)
	httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	httpRequest.Header.Set(contentTypeHeader, applicationJSONContentType)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("send Firecrawl scrape request for %q: %w", requestURL, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return websitePageContent{}, fmt.Errorf(
				"read Firecrawl scrape error response for %q after status %d: %w",
				requestURL,
				httpResponse.StatusCode,
				readErr,
			)
		}

		return websitePageContent{}, firecrawlStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"firecrawl scrape request failed for %q with status %d: %s",
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
		return websitePageContent{}, fmt.Errorf("decode firecrawl scrape response for %q: %w", requestURL, err)
	}

	response, err := parseFirecrawlScrapeResponse(rawResponse)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("parse firecrawl scrape response for %q: %w", requestURL, err)
	}

	err = firecrawlScrapeResponseError(response, requestURL)
	if err != nil {
		return websitePageContent{}, err
	}

	return newFirecrawlPageContent(response, maxMarkdownCharacters)
}

func firecrawlScrapeRequestBody(requestURL string) map[string]any {
	return map[string]any{
		"url":     requestURL,
		"formats": []string{"markdown"},
	}
}

func parseFirecrawlScrapeResponse(rawResponse map[string]any) (firecrawlScrapeResponse, error) {
	response := firecrawlScrapeResponse{
		Success: false,
		Data:    nil,
		Error:   mapStringValue(rawResponse, "error"),
	}

	if success, ok := rawResponse["success"].(bool); ok {
		response.Success = success
	}

	rawData, hasData := rawResponse["data"]
	if !hasData || rawData == nil {
		return response, nil
	}

	dataMap, ok := rawData.(map[string]any)
	if !ok {
		return firecrawlScrapeResponse{}, fmt.Errorf("decode Firecrawl scrape data: %w", os.ErrInvalid)
	}

	data := firecrawlScrapeData{
		Markdown: mapStringValue(dataMap, "markdown"),
		Metadata: firecrawlScrapeMetadata{
			Title:       "",
			SourceURL:   "",
			Description: "",
		},
	}

	rawMetadata, hasMetadata := dataMap["metadata"]
	if hasMetadata && rawMetadata != nil {
		metadataMap, ok := rawMetadata.(map[string]any)
		if !ok {
			return firecrawlScrapeResponse{}, fmt.Errorf("decode Firecrawl scrape metadata: %w", os.ErrInvalid)
		}

		data.Metadata = firecrawlScrapeMetadata{
			Title:       mapStringValue(metadataMap, "title"),
			SourceURL:   mapStringValue(metadataMap, "sourceURL"),
			Description: mapStringValue(metadataMap, "description"),
		}
	}

	response.Data = &data

	return response, nil
}

func firecrawlScrapeResponseError(response firecrawlScrapeResponse, requestURL string) error {
	if response.Success {
		return nil
	}

	return fmt.Errorf(
		"firecrawl scrape reported an error for %q: %s: %w",
		requestURL,
		strings.TrimSpace(response.Error),
		os.ErrInvalid,
	)
}

func newFirecrawlPageContent(
	response firecrawlScrapeResponse,
	maxMarkdownCharacters int,
) (websitePageContent, error) {
	if response.Data == nil {
		return websitePageContent{}, fmt.Errorf("firecrawl scrape returned no data: %w", os.ErrInvalid)
	}

	content := truncateRunes(
		strings.TrimSpace(response.Data.Markdown),
		maxMarkdownCharacters,
	)

	return newWebsitePageContent(
		firstNonEmptyString(response.Data.Metadata.SourceURL, response.Data.Metadata.Title),
		response.Data.Metadata.Title,
		response.Data.Metadata.Description,
		content,
	)
}

type firecrawlStatusError struct {
	StatusCode int
	Message    string
	Err        error
}

func (err firecrawlStatusError) Error() string {
	return err.Message
}

func (client websiteClient) fetchWithExaContents(
	ctx context.Context,
	requestURL string,
	apiKey string,
	livecrawlTimeoutMS int,
	textMaxCharacters int,
) (websitePageContent, error) {
	pageContent, err := client.fetchWithExaContentsOnce(ctx, requestURL, apiKey, livecrawlTimeoutMS, textMaxCharacters)
	if err == nil || !isExaLivecrawlTimeoutError(err) {
		return pageContent, err
	}

	logWarn(
		"Exa contents livecrawl timed out; retrying with extended timeout",
		err,
		"url",
		requestURL,
	)

	extendedTimeout := exaContentsLivecrawlExtendedTimeoutMultiplier * livecrawlTimeoutMS

	pageContent, err = client.fetchWithExaContentsOnce(ctx, requestURL, apiKey, extendedTimeout, textMaxCharacters)
	if err == nil || !isExaLivecrawlTimeoutError(err) {
		return pageContent, err
	}

	logWarn(
		"Exa contents livecrawl timed out with extended timeout; falling back to cached content",
		err,
		"url",
		requestURL,
	)

	return client.fetchWithExaContentsOnce(ctx, requestURL, apiKey, 0, textMaxCharacters)
}

func (client websiteClient) fetchWithExaContentsOnce(
	ctx context.Context,
	requestURL string,
	apiKey string,
	livecrawlTimeoutMS int,
	textMaxCharacters int,
) (websitePageContent, error) {
	requestBytes, err := json.Marshal(exaContentsRequestBody(requestURL, livecrawlTimeoutMS, textMaxCharacters))
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

	httpRequest.Header.Set("Accept", applicationJSONContentType)
	httpRequest.Header.Set(contentTypeHeader, applicationJSONContentType)
	httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
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

func exaContentsRequestBody(requestURL string, livecrawlTimeoutMS int, textMaxCharacters int) map[string]any {
	if textMaxCharacters <= 0 {
		textMaxCharacters = defaultExaSearchTextMaxCharacters
	}

	requestBody := map[string]any{
		"urls": []string{requestURL},
		messageTextKey: map[string]any{
			"maxCharacters": textMaxCharacters,
			"verbosity":     "full",
			"excludeSections": []string{
				"header",
				"navigation",
				"banner",
				"sidebar",
				"footer",
			},
		},
	}
	if livecrawlTimeoutMS > 0 {
		requestBody["livecrawlTimeout"] = livecrawlTimeoutMS
	}

	return requestBody
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

func isExaLivecrawlTimeoutError(err error) bool {
	return err != nil && strings.Contains(
		strings.ToLower(err.Error()),
		strings.ToLower("CRAWL_LIVECRAWL_TIMEOUT"),
	)
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
	apiKey string,
	maxCharsPerResult int,
) (websitePageContent, error) {
	return client.fetchWithTavilyExtractOnce(ctx, requestURL, apiKey, maxCharsPerResult)
}

func (client websiteClient) fetchWithTavilyExtractOnce(
	ctx context.Context,
	requestURL string,
	apiKey string,
	maxCharsPerResult int,
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

	httpRequest.Header.Set("Accept", applicationJSONContentType)
	httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	httpRequest.Header.Set(contentTypeHeader, applicationJSONContentType)

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

	rawContent := truncateRunes(strings.TrimSpace(result.RawContent), maxCharsPerResult)
	return newWebsitePageContent(firstNonEmptyString(result.URL, requestURL), "", "", rawContent)
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

func (client websiteClient) fetchWithTinyFishFetch(
	ctx context.Context,
	requestURL string,
	apiKey string,
	maxCharsPerResult int,
) (websitePageContent, error) {
	ctx, cancel := context.WithTimeout(ctx, tinyFishFetchRequestTimeout)
	defer cancel()

	requestBytes, err := json.Marshal(tinyFishFetchRequest{
		URLs:            []string{requestURL},
		Format:          "markdown",
		PerURLTimeoutMS: tinyFishFetchPerURLTimeoutMS,
	})
	if err != nil {
		return websitePageContent{}, fmt.Errorf("marshal TinyFish fetch request for %q: %w", requestURL, err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.tinyFishFetchEndpoint,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("create TinyFish fetch request for %q: %w", requestURL, err)
	}

	httpRequest.Header.Set("Accept", applicationJSONContentType)
	httpRequest.Header.Set(contentTypeHeader, applicationJSONContentType)
	httpRequest.Header.Set("X-API-Key", strings.TrimSpace(apiKey))

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("send TinyFish fetch request for %q: %w", requestURL, err)
	}
	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return websitePageContent{}, fmt.Errorf(
				"read TinyFish fetch error response for %q after status %d: %w",
				requestURL,
				httpResponse.StatusCode,
				readErr,
			)
		}

		return websitePageContent{}, tinyFishStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"TinyFish fetch request failed for %q with status %d: %s",
				requestURL,
				httpResponse.StatusCode,
				strings.TrimSpace(extractStructuredAPIErrorMessage(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	var response tinyFishFetchResponse

	err = json.NewDecoder(httpResponse.Body).Decode(&response)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("decode TinyFish fetch response for %q: %w", requestURL, err)
	}

	for _, fetchErr := range response.Errors {
		if strings.EqualFold(strings.TrimSpace(fetchErr.URL), requestURL) {
			return websitePageContent{}, fmt.Errorf(
				"TinyFish fetch reported an error for %q: %s: %w",
				requestURL,
				strings.TrimSpace(fetchErr.Error),
				os.ErrInvalid,
			)
		}
	}

	if len(response.Results) == 0 {
		var errorsText string
		if len(response.Errors) > 0 {
			errorsText = response.Errors[0].Error
		}

		return websitePageContent{}, fmt.Errorf(
			"TinyFish fetch response contained no result for %q: %s: %w",
			requestURL,
			strings.TrimSpace(errorsText),
			os.ErrNotExist,
		)
	}

	var matchedResult *tinyFishFetchResult

	for index := range response.Results {
		result := &response.Results[index]
		if strings.EqualFold(strings.TrimSpace(result.URL), requestURL) ||
			strings.EqualFold(strings.TrimSpace(result.FinalURL), requestURL) {
			matchedResult = result

			break
		}
	}

	if matchedResult == nil {
		matchedResult = &response.Results[0]
	}

	text := tinyFishFetchResultText(matchedResult.Text)

	text = truncateRunes(strings.TrimSpace(text), maxCharsPerResult)
	if text == "" {
		return websitePageContent{}, fmt.Errorf("TinyFish fetch returned empty content for %q: %w", requestURL, os.ErrInvalid)
	}

	title := ""
	if matchedResult.Title != nil {
		title = strings.TrimSpace(*matchedResult.Title)
	}

	if title == "" {
		title = firstNonEmptyString(matchedResult.URL, matchedResult.FinalURL, requestURL)
	}

	description := ""
	if matchedResult.Description != nil {
		description = strings.TrimSpace(*matchedResult.Description)
	}

	resultURL := firstNonEmptyString(matchedResult.FinalURL, matchedResult.URL, requestURL)

	return newWebsitePageContent(resultURL, title, description, text)
}

func (client websiteClient) fetchTinyFishBatch(
	ctx context.Context,
	apiKey string,
	batch []string,
) (tinyFishFetchResponse, error) {
	if len(batch) == 0 {
		return tinyFishFetchResponse{}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, tinyFishFetchRequestTimeout)
	defer cancel()

	requestBytes, err := json.Marshal(tinyFishFetchRequest{
		URLs:            batch,
		Format:          "markdown",
		PerURLTimeoutMS: tinyFishFetchPerURLTimeoutMS,
	})
	if err != nil {
		return tinyFishFetchResponse{}, fmt.Errorf("marshal TinyFish fetch request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.tinyFishFetchEndpoint,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return tinyFishFetchResponse{}, fmt.Errorf("create TinyFish fetch request: %w", err)
	}

	httpRequest.Header.Set("Accept", applicationJSONContentType)
	httpRequest.Header.Set(contentTypeHeader, applicationJSONContentType)
	httpRequest.Header.Set("X-API-Key", strings.TrimSpace(apiKey))

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return tinyFishFetchResponse{}, fmt.Errorf("send TinyFish fetch request: %w", err)
	}

	defer func() { _ = httpResponse.Body.Close() }()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return tinyFishFetchResponse{}, fmt.Errorf("read TinyFish fetch error response after status %d: %w", httpResponse.StatusCode, readErr)
		}

		return tinyFishFetchResponse{}, tinyFishStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"TinyFish fetch request failed with status %d: %s",
				httpResponse.StatusCode,
				strings.TrimSpace(extractStructuredAPIErrorMessage(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	var batchResponse tinyFishFetchResponse
	if err := json.NewDecoder(httpResponse.Body).Decode(&batchResponse); err != nil {
		return tinyFishFetchResponse{}, fmt.Errorf("decode TinyFish fetch response: %w", err)
	}

	return batchResponse, nil
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

	for _, key := range []string{providers.OpenAIStreamErrorEventType, messageKindValue} {
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

func mustParseNetipPrefix(rawPrefix string) netip.Prefix {
	prefix, err := netip.ParsePrefix(rawPrefix)
	if err != nil {
		panic(fmt.Sprintf("parse website IP prefix %q: %v", rawPrefix, err))
	}

	return prefix
}

func defaultWebsiteLookupIP(ctx context.Context, host string) ([]netip.Addr, error) {
	resolvedAddrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf(websiteResolveHostErrorFormat, host, err)
	}

	addresses := make([]netip.Addr, 0, len(resolvedAddrs))

	for _, resolvedAddr := range resolvedAddrs {
		address, ok := netip.AddrFromSlice(resolvedAddr.IP)
		if !ok {
			continue
		}

		addresses = append(addresses, address.Unmap())
	}

	if len(addresses) == 0 {
		return nil, fmt.Errorf(websiteResolveHostErrorFormat, host, os.ErrNotExist)
	}

	return addresses, nil
}

func blockedWebsiteIPRanges() []netip.Prefix {
	return []netip.Prefix{
		mustParseNetipPrefix("0.0.0.0/8"),
		mustParseNetipPrefix("100.64.0.0/10"),
		mustParseNetipPrefix("127.0.0.0/8"),
		mustParseNetipPrefix("192.0.0.0/24"),
		mustParseNetipPrefix("192.0.2.0/24"),
		mustParseNetipPrefix("192.88.99.0/24"),
		mustParseNetipPrefix("198.18.0.0/15"),
		mustParseNetipPrefix("198.51.100.0/24"),
		mustParseNetipPrefix("203.0.113.0/24"),
		mustParseNetipPrefix("224.0.0.0/4"),
		mustParseNetipPrefix("240.0.0.0/4"),
		mustParseNetipPrefix("100::/64"),
		mustParseNetipPrefix("2001:db8::/32"),
	}
}

func (client websiteClient) lookupWebsiteIP() websiteLookupIPFunc {
	if client.lookupIP != nil {
		return client.lookupIP
	}

	return defaultWebsiteLookupIP
}

func validateWebsiteRequestURL(
	ctx context.Context,
	requestURL *url.URL,
	lookupIP websiteLookupIPFunc,
) error {
	if requestURL == nil {
		return fmt.Errorf("missing website url: %w", os.ErrInvalid)
	}

	if !isWebsiteScheme(requestURL.Scheme) {
		return fmt.Errorf("unsupported website scheme %q: %w", requestURL.Scheme, os.ErrInvalid)
	}

	host := strings.TrimSpace(requestURL.Hostname())
	if host == "" {
		return fmt.Errorf("missing website host: %w", os.ErrInvalid)
	}

	return validateWebsiteHost(ctx, host, lookupIP)
}

func validateWebsiteHost(
	ctx context.Context,
	host string,
	lookupIP websiteLookupIPFunc,
) error {
	normalizedHost := normalizeWebsiteHost(host)
	if normalizedHost == "" {
		return fmt.Errorf("missing website host: %w", os.ErrInvalid)
	}

	if isLocalhostWebsiteHost(normalizedHost) {
		return fmt.Errorf("blocked website host %q: %w", host, errUnsafeWebsiteAddress)
	}

	addresses, err := resolveWebsiteHostAddresses(ctx, normalizedHost, lookupIP)
	if err != nil {
		return err
	}

	for _, address := range addresses {
		if !isPublicWebsiteIP(address) {
			return fmt.Errorf(
				"blocked website host %q resolving to %s: %w",
				host,
				address,
				errUnsafeWebsiteAddress,
			)
		}
	}

	return nil
}

func normalizeWebsiteHost(host string) string {
	normalizedHost := strings.TrimSpace(strings.ToLower(host))
	normalizedHost = strings.TrimSuffix(normalizedHost, ".")

	if zoneIndex := strings.Index(normalizedHost, "%"); zoneIndex >= 0 {
		normalizedHost = normalizedHost[:zoneIndex]
	}

	return normalizedHost
}

func isLocalhostWebsiteHost(host string) bool {
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

func resolveWebsiteHostAddresses(
	ctx context.Context,
	host string,
	lookupIP websiteLookupIPFunc,
) ([]netip.Addr, error) {
	if lookupIP == nil {
		lookupIP = defaultWebsiteLookupIP
	}

	address, err := netip.ParseAddr(host)
	if err == nil {
		return []netip.Addr{address.Unmap()}, nil
	}

	addresses, err := lookupIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf(websiteResolveHostErrorFormat, host, err)
	}

	if len(addresses) == 0 {
		return nil, fmt.Errorf(websiteResolveHostErrorFormat, host, os.ErrNotExist)
	}

	return addresses, nil
}

func isPublicWebsiteIP(address netip.Addr) bool {
	normalizedAddress := address.Unmap()
	if !normalizedAddress.IsValid() {
		return false
	}

	if normalizedAddress.IsLoopback() ||
		normalizedAddress.IsPrivate() ||
		normalizedAddress.IsLinkLocalUnicast() ||
		normalizedAddress.IsLinkLocalMulticast() ||
		normalizedAddress.IsMulticast() ||
		normalizedAddress.IsInterfaceLocalMulticast() ||
		normalizedAddress.IsUnspecified() {
		return false
	}

	for _, blockedRange := range blockedWebsiteIPRanges() {
		if blockedRange.Contains(normalizedAddress) {
			return false
		}
	}

	return true
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

func extractWebsiteURLsForProvider(text string) []string {
	return extractWebsiteURLs(text)
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

// parseWebsiteHTML and its helpers (extractWebsiteTitle, extractWebsiteBodyText,
// websiteContentCandidates, renderWebsiteText etc.) are retained for direct
// unit testing (e.g. TestWebsiteClientFetchExtractsMainContentAndIgnoresChrome
// now calls parseWebsiteHTML directly). The local live-fetch path that used
// doRequest/websiteFetchHTTPClient/SSRF transport was removed — website
// extraction now goes exclusively via provider APIs (Firecrawl → TinyFish →
// Exa → Tavily). Transport helpers below (newSSRFProtectedWebsiteTransport,
// websiteFetchHTTPClient, doRequest, sendWebsiteRequest, redirectWebsiteRequest,
// websiteResponseDetails, etc.) are currently dead code and kept only for
// reference; they will be deleted in a follow-up cleanup.
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
	content := extractWebsiteBodyText(document)

	aliExpressTitle, aliExpressContent, isAliExpressProduct := extractAliExpressProductMetadata(
		pageURL,
		responseBody,
		document,
		title,
	)
	if isAliExpressProduct {
		title = aliExpressTitle
		description = ""
		content = aliExpressContent
	}

	return newWebsitePageContent(
		pageURL,
		title,
		description,
		content,
	)
}

func newWebsitePageContent(
	pageURL, title, description, content string,
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
	if titleNode != nil && titleNode.FirstChild != nil {
		if title := strings.TrimSpace(titleNode.FirstChild.Data); title != "" {
			return title
		}
	}

	return firstNonEmptyString(
		extractWebsiteMetaContent(document, "og:title"),
		extractWebsiteMetaContent(document, "twitter:title"),
	)
}

func extractWebsiteDescription(document *html.Node) string {
	return firstNonEmptyString(
		extractWebsiteMetaContent(document, "description"),
		extractWebsiteMetaContent(document, "og:description"),
		extractWebsiteMetaContent(document, "twitter:description"),
	)
}

func extractWebsiteMetaContent(document *html.Node, key string) string {
	metaNode := findWebsiteNode(document, func(node *html.Node) bool {
		if node.Type != html.ElementNode || node.DataAtom != atom.Meta {
			return false
		}

		name := strings.ToLower(strings.TrimSpace(htmlAttribute(node, "name")))
		property := strings.ToLower(strings.TrimSpace(htmlAttribute(node, "property")))

		return (name == key || property == key) && strings.TrimSpace(htmlAttribute(node, "content")) != ""
	})
	if metaNode == nil {
		return ""
	}

	return htmlAttribute(metaNode, "content")
}

func extractWebsiteMetaContents(document *html.Node, key string) []string {
	contents := make([]string, 0)

	visitWebsiteNodes(document, func(node *html.Node) {
		if node.Type != html.ElementNode || node.DataAtom != atom.Meta {
			return
		}

		name := strings.ToLower(strings.TrimSpace(htmlAttribute(node, "name")))

		property := strings.ToLower(strings.TrimSpace(htmlAttribute(node, "property")))
		if name != key && property != key {
			return
		}

		content := strings.TrimSpace(htmlAttribute(node, "content"))
		if content != "" {
			contents = append(contents, content)
		}
	})

	return contents
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
		messageContentKey,
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

func visitWebsiteNodes(node *html.Node, visit func(*html.Node)) {
	if node == nil {
		return
	}

	visit(node)

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		visitWebsiteNodes(child, visit)
	}
}

func renderWebsiteText(root *html.Node) string {
	segments := make([]string, 0, websiteSegmentCapacity)

	var current strings.Builder

	renderWebsiteNode(root, &current, &segments)
	flushWebsiteTextSegment(&current, &segments)

	return truncateRunes(strings.Join(segments, "\n"), maxWebsiteContentRunes)
}

func renderWebsiteNode(
	node *html.Node,
	current *strings.Builder,
	segments *[]string,
) {
	if node == nil {
		return
	}

	if node.Type == html.TextNode {
		appendWebsiteTextChunk(current, node.Data)

		return
	}

	if node.Type != html.ElementNode {
		renderWebsiteChildren(node, current, segments)

		return
	}

	if shouldSkipWebsiteNode(node) {
		return
	}

	if node.DataAtom == atom.Br || isWebsiteBlockNode(node) {
		flushWebsiteTextSegment(current, segments)
	}

	renderWebsiteChildren(node, current, segments)

	if isWebsiteBlockNode(node) {
		flushWebsiteTextSegment(current, segments)
	}
}

func renderWebsiteChildren(
	node *html.Node,
	current *strings.Builder,
	segments *[]string,
) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		renderWebsiteNode(child, current, segments)
	}
}

func flushWebsiteTextSegment(current *strings.Builder, segments *[]string) {
	text := normalizeWebsiteText(current.String())
	if text == "" {
		current.Reset()

		return
	}

	*segments = append(*segments, text)

	current.Reset()
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
