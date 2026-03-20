package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"golang.org/x/net/html"
)

const (
	defaultYandexVisualSearchEndpoint = "https://yandex.com/images/search"
	visualSearchPartialWarningText    = "Warning: some visual search results were unavailable"
	visualSearchWarningText           = "Warning: visual search unavailable"
	visualSearchImageWarningText      = "Warning: visual search needs an image attachment"
	visualSearchResponseByteLimit     = 4 * 1024 * 1024
	maxVisualSearchRelatedContent     = 5
	serpAPIGoogleLensMatchDetailCap   = 5
	maxVisualSearchDescriptionRunes   = 500
	maxVisualSearchSnippetRunes       = 280
	maxVisualSearchTitleRunes         = 180
	maxVisualSearchTags               = 5
	maxVisualSearchSimilarItems       = 5
	maxVisualSearchSiteMatches        = 5
	visualSearchLabelPartCapacity     = 2
	visualSearchProviderCapacity      = 2
	visualSearchTopMatchLineCapacity  = 5
	defaultVisualSearchQuery          = "what is in this image?"
	serpAPIVisualSearchProviderName   = "SerpApi Google Lens"
	yandexVisualSearchProviderName    = "Yandex Images"
)

type visualSearchClient interface {
	search(ctx context.Context, imageURL string) (visualSearchResult, error)
}

type serpAPIVisualSearchClient interface {
	search(ctx context.Context, loadedConfig config, imageURL string) (visualSearchResult, error)
}

type visualSearchProvider struct {
	name   string
	search func(context.Context, string) (visualSearchResult, error)
}

type visualSearchTopMatch struct {
	Title       string
	Subtitle    string
	Description string
	Source      string
	URL         string
}

type visualSearchSimilarImage struct {
	Title string
	URL   string
}

type visualSearchSiteMatch struct {
	Title   string
	Domain  string
	Snippet string
	URL     string
}

type visualSearchRelatedContent struct {
	Query string
	URL   string
}

type visualSearchResult struct {
	ImageIndex     int
	Provider       string
	ImageURL       string
	SearchURL      string
	TopMatch       visualSearchTopMatch
	Tags           []string
	TextInImage    []string
	SimilarImages  []visualSearchSimilarImage
	SiteMatches    []visualSearchSiteMatch
	RelatedContent []visualSearchRelatedContent
}

type yandexVisualSearchClient struct {
	endpoint   string
	httpClient *http.Client
	userAgent  string
}

type serpAPIGoogleLensClient struct {
	endpoint   string
	httpClient *http.Client
	userAgent  string
}

type serpAPIGoogleLensResponse struct {
	SearchMetadata serpAPIGoogleLensSearchMetadata  `json:"search_metadata"`
	VisualMatches  []serpAPIGoogleLensVisualMatch   `json:"visual_matches"`
	RelatedContent []serpAPIGoogleLensRelatedResult `json:"related_content"`
	Error          string                           `json:"error"`
}

type serpAPIGoogleLensSearchMetadata struct {
	Status        string `json:"status"`
	JSONEndpoint  string `json:"json_endpoint"`
	GoogleLensURL string `json:"google_lens_url"`
}

type serpAPIGoogleLensVisualMatch struct {
	Title        string                 `json:"title"`
	Link         string                 `json:"link"`
	Source       string                 `json:"source"`
	Rating       float64                `json:"rating"`
	Reviews      int                    `json:"reviews"`
	Price        serpAPIGoogleLensPrice `json:"price"`
	InStock      *bool                  `json:"in_stock"`
	Condition    string                 `json:"condition"`
	ExactMatches bool                   `json:"exact_matches"`
}

type serpAPIGoogleLensPrice struct {
	Value string `json:"value"`
}

type serpAPIGoogleLensRelatedResult struct {
	Query string `json:"query"`
	Link  string `json:"link"`
}

func emptyVisualSearchTopMatch() visualSearchTopMatch {
	return visualSearchTopMatch{
		Title:       "",
		Subtitle:    "",
		Description: "",
		Source:      "",
		URL:         "",
	}
}

func emptyVisualSearchResult() visualSearchResult {
	return visualSearchResult{
		ImageIndex:     0,
		Provider:       "",
		ImageURL:       "",
		SearchURL:      "",
		TopMatch:       emptyVisualSearchTopMatch(),
		Tags:           nil,
		TextInImage:    nil,
		SimilarImages:  nil,
		SiteMatches:    nil,
		RelatedContent: nil,
	}
}

func newVisualSearchResult(imageURL string, searchURL string) visualSearchResult {
	result := emptyVisualSearchResult()
	result.ImageURL = strings.TrimSpace(imageURL)
	result.SearchURL = strings.TrimSpace(searchURL)

	return result
}

func newVisualSearchClient(httpClient *http.Client) yandexVisualSearchClient {
	return yandexVisualSearchClient{
		endpoint:   defaultYandexVisualSearchEndpoint,
		httpClient: httpClient,
		userAgent:  youtubeUserAgent,
	}
}

func newSerpAPIVisualSearchClient(httpClient *http.Client) serpAPIGoogleLensClient {
	return serpAPIGoogleLensClient{
		endpoint:   defaultSerpAPIGoogleLensEndpoint,
		httpClient: httpClient,
		userAgent:  youtubeUserAgent,
	}
}

func (client yandexVisualSearchClient) search(
	ctx context.Context,
	imageURL string,
) (visualSearchResult, error) {
	requestURL, err := client.requestURL(imageURL)
	if err != nil {
		return emptyVisualSearchResult(), err
	}

	requestContext, cancel := context.WithTimeout(ctx, websiteRequestTimeout)
	defer cancel()

	httpRequest, err := http.NewRequestWithContext(
		requestContext,
		http.MethodGet,
		requestURL,
		nil,
	)
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"create visual search request %q: %w",
			requestURL,
			err,
		)
	}

	httpRequest.Header.Set(
		"Accept",
		"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.1",
	)
	httpRequest.Header.Set("Accept-Language", youtubeAcceptLanguage)
	httpRequest.Header.Set("User-Agent", client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"send visual search request %q: %w",
			requestURL,
			err,
		)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, visualSearchResponseByteLimit+1))
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"read visual search response %q: %w",
			requestURL,
			err,
		)
	}

	if len(responseBody) > visualSearchResponseByteLimit {
		return emptyVisualSearchResult(), fmt.Errorf(
			"visual search response %q exceeds %d bytes: %w",
			requestURL,
			visualSearchResponseByteLimit,
			os.ErrInvalid,
		)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return emptyVisualSearchResult(), fmt.Errorf(
			"visual search request %q failed with status %d: %s: %w",
			requestURL,
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	finalURL := requestURL
	if httpResponse.Request != nil && httpResponse.Request.URL != nil {
		finalURL = httpResponse.Request.URL.String()
	}

	result, err := parseYandexVisualSearchHTML(finalURL, imageURL, responseBody)
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"parse visual search HTML %q: %w",
			requestURL,
			err,
		)
	}

	result.Provider = yandexVisualSearchProviderName

	return result, nil
}

func (client serpAPIGoogleLensClient) search(
	ctx context.Context,
	loadedConfig config,
	imageURL string,
) (visualSearchResult, error) {
	apiKeys := loadedConfig.VisualSearch.SerpAPI.apiKeysForAttempts()
	if len(apiKeys) == 0 {
		return emptyVisualSearchResult(), fmt.Errorf(
			"missing SerpApi Google Lens API key for %q: %w",
			imageURL,
			os.ErrNotExist,
		)
	}

	attemptErrors := make([]error, 0, len(apiKeys))

	for index, apiKey := range apiKeys {
		result, err := client.searchOnce(ctx, imageURL, apiKey)
		if err == nil {
			return result, nil
		}

		attemptErrors = append(attemptErrors, err)
		if ctx.Err() != nil {
			return emptyVisualSearchResult(), err
		}

		if index == len(apiKeys)-1 {
			break
		}

		if !shouldRetrySerpAPIAttemptWithNextKey(err) {
			break
		}
	}

	if len(attemptErrors) == 1 {
		return emptyVisualSearchResult(), attemptErrors[0]
	}

	if len(attemptErrors) == len(apiKeys) {
		return emptyVisualSearchResult(), fmt.Errorf(
			"all configured SerpApi Google Lens API keys failed for %q: %w",
			imageURL,
			errors.Join(attemptErrors...),
		)
	}

	return emptyVisualSearchResult(), fmt.Errorf(
		"SerpApi Google Lens attempts failed for %q: %w",
		imageURL,
		errors.Join(attemptErrors...),
	)
}

func (client serpAPIGoogleLensClient) searchOnce(
	ctx context.Context,
	imageURL string,
	apiKey string,
) (visualSearchResult, error) {
	requestURL, err := client.requestURL(imageURL, apiKey)
	if err != nil {
		return emptyVisualSearchResult(), err
	}

	requestContext, cancel := context.WithTimeout(ctx, websiteRequestTimeout)
	defer cancel()

	httpRequest, err := http.NewRequestWithContext(
		requestContext,
		http.MethodGet,
		requestURL,
		nil,
	)
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"create SerpApi Google Lens request %q: %w",
			requestURL,
			err,
		)
	}

	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Accept-Language", youtubeAcceptLanguage)
	httpRequest.Header.Set("User-Agent", client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"send SerpApi Google Lens request %q: %w",
			requestURL,
			err,
		)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, visualSearchResponseByteLimit+1))
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"read SerpApi Google Lens response %q: %w",
			requestURL,
			err,
		)
	}

	if len(responseBody) > visualSearchResponseByteLimit {
		return emptyVisualSearchResult(), fmt.Errorf(
			"SerpApi Google Lens response %q exceeds %d bytes: %w",
			requestURL,
			visualSearchResponseByteLimit,
			os.ErrInvalid,
		)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return emptyVisualSearchResult(), newSerpAPIProviderError(
			fmt.Sprintf("SerpApi Google Lens request %q failed", requestURL),
			httpResponse.StatusCode,
			httpResponse.Status,
			responseBody,
		)
	}

	return client.parseResponse(requestURL, imageURL, responseBody)
}

func (client serpAPIGoogleLensClient) parseResponse(
	requestURL string,
	imageURL string,
	responseBody []byte,
) (visualSearchResult, error) {
	var response serpAPIGoogleLensResponse

	err := json.Unmarshal(responseBody, &response)
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"decode SerpApi Google Lens response %q: %w",
			requestURL,
			err,
		)
	}

	status := strings.TrimSpace(response.SearchMetadata.Status)
	responseError := strings.TrimSpace(response.Error)

	switch {
	case status == "":
		if responseError != "" {
			return emptyVisualSearchResult(), providerStatusError{
				StatusCode: http.StatusBadGateway,
				Message: fmt.Sprintf(
					"SerpApi Google Lens returned an error for %q: %s",
					imageURL,
					responseError,
				),
				RetryDelay: 0,
				Err:        os.ErrInvalid,
			}
		}
	case strings.EqualFold(status, serpAPISearchStatusSuccess):
		return parseSerpAPIGoogleLensResponse(imageURL, response), nil
	case strings.EqualFold(status, serpAPISearchStatusQueued),
		strings.EqualFold(status, serpAPISearchStatusProcessing),
		strings.EqualFold(status, serpAPISearchStatusError):
		return emptyVisualSearchResult(), newSerpAPISearchStatusError(
			imageURL,
			status,
			responseError,
		)
	default:
		return emptyVisualSearchResult(), newSerpAPISearchStatusError(
			imageURL,
			status,
			responseError,
		)
	}

	return parseSerpAPIGoogleLensResponse(imageURL, response), nil
}

func (client serpAPIGoogleLensClient) requestURL(imageURL string, apiKey string) (string, error) {
	parsedURL, err := neturl.Parse(client.endpoint)
	if err != nil {
		return "", fmt.Errorf(
			"parse SerpApi Google Lens endpoint %q: %w",
			client.endpoint,
			err,
		)
	}

	queryValues := parsedURL.Query()
	queryValues.Set("api_key", strings.TrimSpace(apiKey))
	queryValues.Set("engine", "google_lens")
	queryValues.Set("type", "all")
	queryValues.Set("url", strings.TrimSpace(imageURL))
	parsedURL.RawQuery = queryValues.Encode()

	return parsedURL.String(), nil
}

func parseSerpAPIGoogleLensResponse(
	imageURL string,
	response serpAPIGoogleLensResponse,
) visualSearchResult {
	searchURL := strings.TrimSpace(response.SearchMetadata.GoogleLensURL)
	if searchURL == "" {
		searchURL = strings.TrimSpace(response.SearchMetadata.JSONEndpoint)
	}

	result := newVisualSearchResult(imageURL, searchURL)
	result.Provider = serpAPIVisualSearchProviderName
	result.RelatedContent = parseSerpAPIGoogleLensRelatedContent(response.RelatedContent)

	if len(response.VisualMatches) == 0 {
		return result
	}

	result.TopMatch = serpAPIGoogleLensTopMatch(response.VisualMatches[0])
	result.SiteMatches = parseSerpAPIGoogleLensSiteMatches(response.VisualMatches)

	return result
}

func parseSerpAPIGoogleLensRelatedContent(
	items []serpAPIGoogleLensRelatedResult,
) []visualSearchRelatedContent {
	relatedContent := make([]visualSearchRelatedContent, 0, len(items))
	seenURLs := make(map[string]struct{}, len(items))

	for _, item := range items {
		url := strings.TrimSpace(item.Link)
		if url == "" {
			continue
		}

		foldedURL := strings.ToLower(url)
		if _, ok := seenURLs[foldedURL]; ok {
			continue
		}

		seenURLs[foldedURL] = struct{}{}

		relatedContent = append(relatedContent, visualSearchRelatedContent{
			Query: truncateRunes(strings.TrimSpace(item.Query), maxVisualSearchTitleRunes),
			URL:   url,
		})
		if len(relatedContent) == maxVisualSearchRelatedContent {
			break
		}
	}

	return relatedContent
}

func serpAPIGoogleLensTopMatch(match serpAPIGoogleLensVisualMatch) visualSearchTopMatch {
	return visualSearchTopMatch{
		Title:       truncateRunes(strings.TrimSpace(match.Title), maxVisualSearchTitleRunes),
		Subtitle:    serpAPIGoogleLensTopMatchSubtitle(match),
		Description: serpAPIGoogleLensMatchDetails(match),
		Source:      serpAPIGoogleLensMatchSource(match),
		URL:         strings.TrimSpace(match.Link),
	}
}

func serpAPIGoogleLensTopMatchSubtitle(match serpAPIGoogleLensVisualMatch) string {
	if match.ExactMatches {
		return "Exact match"
	}

	return ""
}

func parseSerpAPIGoogleLensSiteMatches(
	items []serpAPIGoogleLensVisualMatch,
) []visualSearchSiteMatch {
	siteMatches := make([]visualSearchSiteMatch, 0, len(items))
	seenURLs := make(map[string]struct{}, len(items))

	for itemIndex, item := range items {
		if itemIndex == 0 {
			continue
		}

		url := strings.TrimSpace(item.Link)
		if url == "" {
			continue
		}

		foldedURL := strings.ToLower(url)
		if _, ok := seenURLs[foldedURL]; ok {
			continue
		}

		seenURLs[foldedURL] = struct{}{}

		siteMatches = append(siteMatches, visualSearchSiteMatch{
			Title:   truncateRunes(strings.TrimSpace(item.Title), maxVisualSearchTitleRunes),
			Domain:  serpAPIGoogleLensMatchSource(item),
			Snippet: serpAPIGoogleLensMatchDetails(item),
			URL:     url,
		})
		if len(siteMatches) == maxVisualSearchSiteMatches {
			break
		}
	}

	return siteMatches
}

func serpAPIGoogleLensMatchSource(match serpAPIGoogleLensVisualMatch) string {
	if source := strings.TrimSpace(match.Source); source != "" {
		return truncateRunes(source, maxVisualSearchTitleRunes)
	}

	parsedURL, err := neturl.Parse(strings.TrimSpace(match.Link))
	if err != nil {
		return ""
	}

	return truncateRunes(strings.TrimSpace(parsedURL.Hostname()), maxVisualSearchTitleRunes)
}

func serpAPIGoogleLensMatchDetails(match serpAPIGoogleLensVisualMatch) string {
	details := make([]string, 0, serpAPIGoogleLensMatchDetailCap)

	if price := strings.TrimSpace(match.Price.Value); price != "" {
		details = append(details, "Price: "+price)
	}

	if condition := strings.TrimSpace(match.Condition); condition != "" {
		details = append(details, "Condition: "+condition)
	}

	if match.InStock != nil {
		if *match.InStock {
			details = append(details, "In stock")
		} else {
			details = append(details, "Out of stock")
		}
	}

	if match.Rating > 0 {
		ratingText := fmt.Sprintf("Rating: %.1f", match.Rating)
		if match.Reviews > 0 {
			ratingText += fmt.Sprintf(" (%d reviews)", match.Reviews)
		}

		details = append(details, ratingText)
	}

	if match.ExactMatches {
		details = append(details, "Exact matches available")
	}

	return truncateRunes(strings.Join(details, "; "), maxVisualSearchDescriptionRunes)
}

func (client yandexVisualSearchClient) requestURL(imageURL string) (string, error) {
	parsedURL, err := neturl.Parse(client.endpoint)
	if err != nil {
		return "", fmt.Errorf("parse visual search endpoint %q: %w", client.endpoint, err)
	}

	queryValues := parsedURL.Query()
	queryValues.Set("rpt", "imageview")
	queryValues.Set("url", strings.TrimSpace(imageURL))
	parsedURL.RawQuery = queryValues.Encode()

	return parsedURL.String(), nil
}

func (instance *bot) maybeAugmentConversationWithVisualSearch(
	ctx context.Context,
	loadedConfig config,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) ([]chatMessage, *searchMetadata, []string, error) {
	preparedAugmentation, err := instance.prepareVisualSearchAugmentation(
		ctx,
		loadedConfig,
		sourceMessage,
		conversation,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	augmentedConversation, err := applyPreparedConversationAugmentation(
		conversation,
		preparedAugmentation,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	return augmentedConversation, preparedAugmentation.metadata, preparedAugmentation.warnings, nil
}

func (instance *bot) prepareVisualSearchAugmentation(
	ctx context.Context,
	loadedConfig config,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) (preparedConversationAugmentation, error) {
	if instance.visualSearch == nil && instance.serpAPIVisualSearch == nil {
		return emptyPreparedConversationAugmentation(), nil
	}

	latestUserQuery, err := latestUserPromptQuery(conversation)
	if err != nil {
		return emptyPreparedConversationAugmentation(), fmt.Errorf(
			"extract latest user query: %w",
			err,
		)
	}

	rewrittenQuery, requested := rewriteVisualSearchUserQuery(latestUserQuery)
	if !requested {
		return emptyPreparedConversationAugmentation(), nil
	}

	imageURLs := instance.visualSearchImageURLs(ctx, sourceMessage)
	if len(imageURLs) == 0 {
		return newPreparedConversationAugmentation(
			[]string{visualSearchImageWarningText},
			nil,
			func(currentConversation []chatMessage) ([]chatMessage, error) {
				return rewriteUserQueryInConversation(currentConversation, rewrittenQuery)
			},
		), nil
	}

	providers := instance.visualSearchProvidersForConfig(loadedConfig)
	if len(providers) == 0 {
		return newPreparedConversationAugmentation(
			nil,
			nil,
			func(currentConversation []chatMessage) ([]chatMessage, error) {
				return rewriteUserQueryInConversation(currentConversation, rewrittenQuery)
			},
		), nil
	}

	results, warnings := instance.runVisualSearchProviders(ctx, imageURLs, providers)
	if len(results) == 0 {
		return newPreparedConversationAugmentation(
			warnings,
			nil,
			func(currentConversation []chatMessage) ([]chatMessage, error) {
				return rewriteUserQueryInConversation(currentConversation, rewrittenQuery)
			},
		), nil
	}

	formattedResults := formatVisualSearchResults(results)

	return newPreparedConversationAugmentation(
		warnings,
		newVisualSearchMetadata(results),
		func(currentConversation []chatMessage) ([]chatMessage, error) {
			rewrittenConversation, rewriteErr := rewriteUserQueryInConversation(
				currentConversation,
				rewrittenQuery,
			)
			if rewriteErr != nil {
				return nil, fmt.Errorf("rewrite latest user query: %w", rewriteErr)
			}

			augmentedConversation, appendErr := appendVisualSearchResultsToConversation(
				rewrittenConversation,
				formattedResults,
			)
			if appendErr != nil {
				return nil, fmt.Errorf(
					"append visual search results to conversation: %w",
					appendErr,
				)
			}

			return augmentedConversation, nil
		},
	), nil
}

func (instance *bot) visualSearchProvidersForConfig(loadedConfig config) []visualSearchProvider {
	providers := make([]visualSearchProvider, 0, visualSearchProviderCapacity)

	if instance.visualSearch != nil {
		providers = append(providers, visualSearchProvider{
			name: yandexVisualSearchProviderName,
			search: func(ctx context.Context, imageURL string) (visualSearchResult, error) {
				return instance.visualSearch.search(ctx, imageURL)
			},
		})
	}

	if instance.serpAPIVisualSearch != nil && len(loadedConfig.VisualSearch.SerpAPI.apiKeysForAttempts()) > 0 {
		providers = append(providers, visualSearchProvider{
			name: serpAPIVisualSearchProviderName,
			search: func(ctx context.Context, imageURL string) (visualSearchResult, error) {
				return instance.serpAPIVisualSearch.search(ctx, loadedConfig, imageURL)
			},
		})
	}

	return providers
}

func (instance *bot) runVisualSearchProviders(
	ctx context.Context,
	imageURLs []string,
	providers []visualSearchProvider,
) ([]visualSearchResult, []string) {
	if len(imageURLs) == 0 || len(providers) == 0 {
		return nil, nil
	}

	type indexedVisualSearchResult struct {
		result visualSearchResult
		ok     bool
	}

	results := make([]indexedVisualSearchResult, len(imageURLs)*len(providers))

	var (
		fetchFailed bool
		failedMu    sync.Mutex
		waitGroup   sync.WaitGroup
	)

	for imageIndex, imageURL := range imageURLs {
		for providerIndex, provider := range providers {
			waitGroup.Add(1)

			go func(
				imageIndex int,
				imageURL string,
				providerIndex int,
				provider visualSearchProvider,
			) {
				defer waitGroup.Done()

				result, err := provider.search(ctx, imageURL)
				if err != nil {
					slog.Warn("run visual search", "provider", provider.name, "url", imageURL, "error", err)
					failedMu.Lock()
					fetchFailed = true
					failedMu.Unlock()

					return
				}

				result.ImageIndex = imageIndex
				result.ImageURL = strings.TrimSpace(imageURL)

				if strings.TrimSpace(result.Provider) == "" {
					result.Provider = provider.name
				}

				results[imageIndex*len(providers)+providerIndex] = indexedVisualSearchResult{
					result: result,
					ok:     true,
				}
			}(imageIndex, imageURL, providerIndex, provider)
		}
	}

	waitGroup.Wait()

	formattedResults := make([]visualSearchResult, 0, len(results))
	for _, item := range results {
		if !item.ok {
			continue
		}

		formattedResults = append(formattedResults, item.result)
	}

	warnings := make([]string, 0, 1)

	if fetchFailed {
		warningText := visualSearchPartialWarningText
		if len(formattedResults) == 0 {
			warningText = visualSearchWarningText
		}

		warnings = append(warnings, warningText)
	}

	return formattedResults, warnings
}

func (instance *bot) visualSearchImageURLs(
	ctx context.Context,
	sourceMessage *discordgo.Message,
) []string {
	return imageAttachmentURLsForMessages(
		instance.attachmentAugmentationMessages(ctx, sourceMessage),
	)
}

func imageAttachmentURLsForMessages(messages []*discordgo.Message) []string {
	imageURLs := make([]string, 0)
	seenURLs := make(map[string]struct{})

	for _, message := range messages {
		if message == nil {
			continue
		}

		for _, attachment := range message.Attachments {
			if attachment == nil {
				continue
			}

			contentType := attachmentContentType(attachment)
			if !strings.HasPrefix(strings.TrimSpace(contentType), "image/") {
				continue
			}

			imageURL := strings.TrimSpace(attachment.URL)
			if imageURL == "" {
				continue
			}

			if _, ok := seenURLs[imageURL]; ok {
				continue
			}

			seenURLs[imageURL] = struct{}{}
			imageURLs = append(imageURLs, imageURL)
		}
	}

	return imageURLs
}

func rewriteVisualSearchUserQuery(query string) (string, bool) {
	prefix, body := splitUserMessagePrefix(query)
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(body)), "vsearch") {
		return query, false
	}

	trimmedBody := strings.TrimSpace(body)

	separatorIndex := visualSearchSeparatorIndex(trimmedBody)
	if separatorIndex == -1 {
		return query, false
	}

	rewrittenBody := strings.TrimSpace(trimmedBody[separatorIndex:])
	if rewrittenBody == "" {
		rewrittenBody = defaultVisualSearchQuery
	}

	if prefix == "" {
		return rewrittenBody, true
	}

	return strings.TrimSpace(prefix + rewrittenBody), true
}

func visualSearchSeparatorIndex(text string) int {
	if !strings.HasPrefix(strings.ToLower(text), "vsearch") {
		return -1
	}

	if len(text) == len("vsearch") {
		return len(text)
	}

	nextRune, size := utf8.DecodeRuneInString(text[len("vsearch"):])
	if nextRune == ' ' || nextRune == '\t' || nextRune == '\n' || strings.ContainsRune(",.:;!?-", nextRune) {
		return len("vsearch") + size
	}

	if nextRune == utf8.RuneError && size == 0 {
		return len(text)
	}

	if nextRune == utf8.RuneError && size == 1 {
		return -1
	}

	return -1
}

func parseYandexVisualSearchHTML(
	searchURL string,
	imageURL string,
	htmlBody []byte,
) (visualSearchResult, error) {
	document, err := html.Parse(bytes.NewReader(htmlBody))
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf("parse HTML: %w", err)
	}

	result := newVisualSearchResult(imageURL, searchURL)

	objectResponse := findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirObjectResponse-Container")
	})
	if objectResponse != nil {
		result.TopMatch = parseVisualSearchTopMatch(searchURL, objectResponse)
	}

	tagsSection := findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirTags")
	})
	if tagsSection != nil {
		result.Tags = parseVisualSearchTags(tagsSection)
	}

	ocrSection := findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirOcr")
	})
	if ocrSection != nil {
		result.TextInImage = parseVisualSearchOCRText(ocrSection)
	}

	similarSection := findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirSimilarList")
	})
	if similarSection != nil {
		result.SimilarImages = parseVisualSearchSimilarImages(searchURL, similarSection)
	}

	sitesSection := findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirSitesList")
	})
	if sitesSection != nil {
		result.SiteMatches = parseVisualSearchSiteMatches(searchURL, sitesSection)
	}

	return result, nil
}

func parseVisualSearchTopMatch(searchURL string, root *html.Node) visualSearchTopMatch {
	topMatch := visualSearchTopMatch{
		Title: findVisualSearchNodeText(
			root,
			"CbirObjectResponse-Title",
			maxVisualSearchTitleRunes,
		),
		Subtitle: findVisualSearchNodeText(
			root,
			"CbirObjectResponse-Subtitle",
			maxVisualSearchTitleRunes,
		),
		Description: findVisualSearchNodeText(
			root,
			"CbirObjectResponse-Description",
			maxVisualSearchDescriptionRunes,
		),
		Source: "",
		URL:    "",
	}

	sourceLink := findWebsiteNode(root, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirObjectResponse-SourceLink")
	})
	if sourceLink != nil {
		topMatch.Source = truncateRunes(nodeTextContent(sourceLink), maxVisualSearchTitleRunes)
		topMatch.URL = resolveVisualSearchURL(searchURL, htmlAttribute(sourceLink, "href"))
	}

	if topMatch.URL == "" {
		thumbLink := findWebsiteNode(root, func(node *html.Node) bool {
			return hasHTMLClass(node, "CbirObjectResponse-Thumb")
		})
		topMatch.URL = resolveVisualSearchURL(searchURL, htmlAttribute(thumbLink, "href"))
	}

	return topMatch
}

func parseVisualSearchTags(root *html.Node) []string {
	tagNodes := findVisualSearchNodes(root, func(node *html.Node) bool {
		return hasHTMLClass(node, "Tags-Item")
	})

	tags := make([]string, 0, len(tagNodes))
	seenTags := make(map[string]struct{})

	for _, tagNode := range tagNodes {
		if tagNode.Type != html.ElementNode || tagNode.Data != atomA {
			continue
		}

		tagText := truncateRunes(nodeTextContent(tagNode), maxVisualSearchTitleRunes)
		if tagText == "" {
			continue
		}

		foldedText := strings.ToLower(tagText)
		if _, ok := seenTags[foldedText]; ok {
			continue
		}

		seenTags[foldedText] = struct{}{}

		tags = append(tags, tagText)
		if len(tags) == maxVisualSearchTags {
			break
		}
	}

	return tags
}

func parseVisualSearchOCRText(root *html.Node) []string {
	textNodes := findVisualSearchNodes(root, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirOcr-TextBox")
	})

	texts := make([]string, 0, len(textNodes))
	seenTexts := make(map[string]struct{})

	for _, textNode := range textNodes {
		text := truncateRunes(nodeTextContent(textNode), maxVisualSearchTitleRunes)
		if text == "" {
			continue
		}

		foldedText := strings.ToLower(text)
		if _, ok := seenTexts[foldedText]; ok {
			continue
		}

		seenTexts[foldedText] = struct{}{}

		texts = append(texts, text)
	}

	return texts
}

func parseVisualSearchSimilarImages(
	searchURL string,
	root *html.Node,
) []visualSearchSimilarImage {
	imageNodes := findVisualSearchNodes(root, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirSimilarList-ThumbImage")
	})

	items := make([]visualSearchSimilarImage, 0, len(imageNodes))
	seenTitles := make(map[string]struct{})

	for _, imageNode := range imageNodes {
		title := truncateRunes(nodeTextContent(imageNode), maxVisualSearchTitleRunes)
		if title == "" {
			title = truncateRunes(strings.TrimSpace(htmlAttribute(imageNode, "aria-label")), maxVisualSearchTitleRunes)
		}

		if title == "" {
			continue
		}

		foldedTitle := strings.ToLower(title)
		if _, ok := seenTitles[foldedTitle]; ok {
			continue
		}

		seenTitles[foldedTitle] = struct{}{}

		items = append(items, visualSearchSimilarImage{
			Title: title,
			URL:   resolveVisualSearchURL(searchURL, htmlAttribute(imageNode, "href")),
		})
		if len(items) == maxVisualSearchSimilarItems {
			break
		}
	}

	return items
}

func parseVisualSearchSiteMatches(
	searchURL string,
	root *html.Node,
) []visualSearchSiteMatch {
	itemNodes := findVisualSearchNodes(root, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirSites-Item")
	})

	items := make([]visualSearchSiteMatch, 0, len(itemNodes))
	seenURLs := make(map[string]struct{})

	for _, itemNode := range itemNodes {
		titleNode := findWebsiteNode(itemNode, func(node *html.Node) bool {
			return hasHTMLClass(node, "CbirSites-ItemTitle")
		})
		domainNode := findWebsiteNode(itemNode, func(node *html.Node) bool {
			return hasHTMLClass(node, "CbirSites-ItemDomain")
		})
		descriptionNode := findWebsiteNode(itemNode, func(node *html.Node) bool {
			return hasHTMLClass(node, "CbirSites-ItemDescription")
		})

		titleLink := firstVisualSearchLink(titleNode)

		itemURL := resolveVisualSearchURL(searchURL, htmlAttribute(titleLink, "href"))
		if itemURL == "" {
			continue
		}

		if _, ok := seenURLs[itemURL]; ok {
			continue
		}

		seenURLs[itemURL] = struct{}{}

		items = append(items, visualSearchSiteMatch{
			Title:   truncateRunes(nodeTextContent(titleNode), maxVisualSearchTitleRunes),
			Domain:  truncateRunes(nodeTextContent(domainNode), maxVisualSearchTitleRunes),
			Snippet: truncateRunes(nodeTextContent(descriptionNode), maxVisualSearchSnippetRunes),
			URL:     itemURL,
		})
		if len(items) == maxVisualSearchSiteMatches {
			break
		}
	}

	return items
}

func findVisualSearchNodes(
	root *html.Node,
	predicate func(*html.Node) bool,
) []*html.Node {
	nodes := make([]*html.Node, 0)

	var walk func(*html.Node)

	walk = func(node *html.Node) {
		if node == nil {
			return
		}

		if predicate(node) {
			nodes = append(nodes, node)
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(root)

	return nodes
}

func firstVisualSearchLink(root *html.Node) *html.Node {
	return findWebsiteNode(root, func(node *html.Node) bool {
		return node != nil && node.Type == html.ElementNode && node.Data == atomA
	})
}

func findVisualSearchNodeText(
	root *html.Node,
	className string,
	maxRunes int,
) string {
	return truncateRunes(
		nodeTextContent(findWebsiteNode(root, func(node *html.Node) bool {
			return hasHTMLClass(node, className)
		})),
		maxRunes,
	)
}

func nodeTextContent(root *html.Node) string {
	if root == nil {
		return ""
	}

	var builder strings.Builder

	var walk func(*html.Node)

	walk = func(node *html.Node) {
		if node == nil {
			return
		}

		if node.Type == html.TextNode {
			appendWebsiteTextChunk(&builder, node.Data)

			return
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(root)

	return strings.TrimSpace(builder.String())
}

func resolveVisualSearchURL(baseURL string, rawURL string) string {
	trimmedURL := strings.TrimSpace(rawURL)
	if trimmedURL == "" {
		return ""
	}

	base, err := neturl.Parse(baseURL)
	if err != nil {
		return trimmedURL
	}

	relative, err := neturl.Parse(trimmedURL)
	if err != nil {
		return trimmedURL
	}

	return base.ResolveReference(relative).String()
}

func formatVisualSearchResults(results []visualSearchResult) string {
	if len(results) == 0 {
		return "No visual search results found."
	}

	formattedResults := make([]string, 0, len(results))

	for index, result := range results {
		formattedResults = append(
			formattedResults,
			formatSingleVisualSearchResult(index, result, results),
		)
	}

	return strings.Join(formattedResults, "\n\n")
}

func extractVisualSearchSources(result visualSearchResult) []searchSource {
	sources := make([]searchSource, 0, 2+len(result.SimilarImages)+len(result.SiteMatches)+len(result.RelatedContent))
	seenURLs := make(map[string]struct{})

	appendSource := func(title string, rawURL string) {
		url := strings.TrimSpace(rawURL)
		if url == "" {
			return
		}

		foldedURL := strings.ToLower(url)
		if _, ok := seenURLs[foldedURL]; ok {
			return
		}

		seenURLs[foldedURL] = struct{}{}

		sourceTitle := strings.TrimSpace(title)
		if sourceTitle == "" {
			sourceTitle = url
		}

		sources = append(sources, searchSource{Title: sourceTitle, URL: url})
	}

	appendSource(visualSearchTopMatchSourceTitle(result.TopMatch), result.TopMatch.URL)
	appendSource(visualSearchSearchPageSourceTitle(result), result.SearchURL)

	for _, item := range result.SimilarImages {
		appendSource(visualSearchSimilarImageSourceTitle(item), item.URL)
	}

	for _, item := range result.SiteMatches {
		appendSource(visualSearchSiteMatchSourceTitle(item), item.URL)
	}

	for _, item := range result.RelatedContent {
		appendSource(visualSearchRelatedContentSourceTitle(item), item.URL)
	}

	return sources
}

func visualSearchResultSectionLabel(
	result visualSearchResult,
	allResults []visualSearchResult,
) string {
	if len(allResults) <= 1 {
		return ""
	}

	hasMultipleImages := false
	hasMultipleProviders := false
	firstImageIndex := allResults[0].ImageIndex
	firstProvider := strings.TrimSpace(allResults[0].Provider)

	for _, item := range allResults[1:] {
		if item.ImageIndex != firstImageIndex {
			hasMultipleImages = true
		}

		if !strings.EqualFold(strings.TrimSpace(item.Provider), firstProvider) {
			hasMultipleProviders = true
		}
	}

	labelParts := make([]string, 0, visualSearchLabelPartCapacity)
	if hasMultipleImages {
		labelParts = append(labelParts, fmt.Sprintf("Image %d", result.ImageIndex+1))
	}

	if hasMultipleProviders {
		providerName := strings.TrimSpace(result.Provider)
		if providerName == "" {
			providerName = "Visual search"
		}

		labelParts = append(labelParts, providerName)
	}

	if len(labelParts) == 0 {
		return ""
	}

	return strings.Join(labelParts, " - ")
}

func visualSearchTopMatchSourceTitle(topMatch visualSearchTopMatch) string {
	title := strings.TrimSpace(topMatch.Title)
	source := strings.TrimSpace(topMatch.Source)

	switch {
	case title != "" && source != "" && !strings.EqualFold(title, source):
		return "Top match: " + title + " (" + source + ")"
	case title != "":
		return "Top match: " + title
	case source != "":
		return "Top match: " + source
	default:
		return ""
	}
}

func visualSearchSimilarImageSourceTitle(item visualSearchSimilarImage) string {
	title := strings.TrimSpace(item.Title)
	if title == "" {
		return ""
	}

	return "Similar image: " + title
}

func visualSearchSiteMatchSourceTitle(item visualSearchSiteMatch) string {
	title := strings.TrimSpace(item.Title)
	domain := strings.TrimSpace(item.Domain)

	switch {
	case title != "" && domain != "" && !strings.EqualFold(title, domain):
		return "Site match: " + title + " (" + domain + ")"
	case title != "":
		return "Site match: " + title
	case domain != "":
		return "Site match: " + domain
	default:
		return ""
	}
}

func visualSearchSearchPageSourceTitle(result visualSearchResult) string {
	if strings.TrimSpace(result.SearchURL) == "" {
		return ""
	}

	providerName := strings.TrimSpace(result.Provider)
	if providerName == "" {
		providerName = "Visual search"
	}

	return "Search page: " + providerName
}

func visualSearchRelatedContentSourceTitle(item visualSearchRelatedContent) string {
	query := strings.TrimSpace(item.Query)
	if query == "" {
		return "Related content"
	}

	return "Related content: " + query
}

func formatSingleVisualSearchResult(
	index int,
	result visualSearchResult,
	allResults []visualSearchResult,
) string {
	lines := make([]string, 0)

	if label := visualSearchResultSectionLabel(result, allResults); label != "" {
		lines = append(lines, label+":")
	} else if len(allResults) > 1 {
		lines = append(lines, fmt.Sprintf("Image %d:", index+1))
	}

	lines = append(lines, formatVisualSearchTopMatch(result.TopMatch)...)

	if len(result.Tags) > 0 {
		lines = append(lines, "Image appears to contain: "+strings.Join(result.Tags, ", "))
	}

	if len(result.TextInImage) > 0 {
		lines = append(lines, "Text in image: "+strings.Join(result.TextInImage, " | "))
	}

	if similarImages := formatVisualSearchSimilarImages(result.SimilarImages); similarImages != "" {
		lines = append(lines, similarImages)
	}

	if siteMatches := formatVisualSearchSiteMatches(result.SiteMatches); siteMatches != "" {
		lines = append(lines, siteMatches)
	}

	if relatedContent := formatVisualSearchRelatedContent(result.RelatedContent); relatedContent != "" {
		lines = append(lines, relatedContent)
	}

	if len(lines) == 0 {
		lines = append(lines, "No visual search results found.")
	}

	return strings.Join(lines, "\n")
}

func formatVisualSearchTopMatch(topMatch visualSearchTopMatch) []string {
	lines := make([]string, 0, visualSearchTopMatchLineCapacity)

	if title := strings.TrimSpace(topMatch.Title); title != "" {
		lines = append(lines, "Top match: "+title)
	}

	if subtitle := strings.TrimSpace(topMatch.Subtitle); subtitle != "" {
		lines = append(lines, "Top match type: "+subtitle)
	}

	if description := strings.TrimSpace(topMatch.Description); description != "" {
		lines = append(lines, "Top match description: "+description)
	}

	if source := strings.TrimSpace(topMatch.Source); source != "" {
		lines = append(lines, "Top match source: "+source)
	}

	if url := strings.TrimSpace(topMatch.URL); url != "" {
		lines = append(lines, "Top match URL: "+url)
	}

	return lines
}

func formatVisualSearchSimilarImages(items []visualSearchSimilarImage) string {
	if len(items) == 0 {
		return ""
	}

	lines := make([]string, 0, len(items)+1)
	lines = append(lines, "Similar images:")

	for itemIndex, item := range items {
		line := fmt.Sprintf("%d. %s", itemIndex+1, item.Title)
		if item.URL != "" {
			line += " <" + item.URL + ">"
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

func formatVisualSearchSiteMatches(items []visualSearchSiteMatch) string {
	if len(items) == 0 {
		return ""
	}

	lines := make([]string, 0, len(items)*3+1)
	lines = append(lines, "Sites with information about the image:")

	for itemIndex, item := range items {
		titleLine := fmt.Sprintf("%d. %s", itemIndex+1, item.Title)
		if item.Domain != "" && !strings.EqualFold(item.Title, item.Domain) {
			titleLine += " (" + item.Domain + ")"
		}

		lines = append(lines, titleLine)

		if item.Snippet != "" {
			lines = append(lines, "Snippet: "+item.Snippet)
		}

		if item.URL != "" {
			lines = append(lines, "URL: "+item.URL)
		}
	}

	return strings.Join(lines, "\n")
}

func formatVisualSearchRelatedContent(items []visualSearchRelatedContent) string {
	if len(items) == 0 {
		return ""
	}

	lines := make([]string, 0, len(items)+1)
	lines = append(lines, "Related content:")

	for itemIndex, item := range items {
		line := fmt.Sprintf("%d. %s", itemIndex+1, item.Query)
		if strings.TrimSpace(item.Query) == "" {
			line = fmt.Sprintf("%d. Related content", itemIndex+1)
		}

		if item.URL != "" {
			line += " <" + item.URL + ">"
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

const atomA = "a"
