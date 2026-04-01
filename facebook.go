package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	cloudscraper "github.com/Advik-B/cloudscraper/lib"
	useragent "github.com/Advik-B/cloudscraper/lib/user_agent"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	defaultFacebookPageURL            = "https://fdownloader.net/en"
	defaultFacebookSearchURL          = "https://v3.fdownloader.net/api/ajaxSearch"
	defaultFacebookGetMyFBProcessURL  = "https://getmyfb.com/process"
	defaultFacebookConversionName     = "FDownloader.Net"
	facebookDefaultFilename           = "facebook.mp4"
	facebookDefaultMIMEType           = "video/mp4"
	facebookFilenamePrefix            = "facebook_"
	facebookSearchLanguage            = "en"
	facebookSearchVersion             = "v2"
	facebookGetMyFBLocale             = "en"
	facebookDefaultHDQualityScore     = 720
	facebookDefaultSDQualityScore     = 360
	facebookDownloadCandidateCapacity = 4
	facebookRegexpMatchGroupCount     = 2
	facebookWarningText               = "Warning: Facebook content unavailable"
)

var (
	facebookURLRegexp = regexp.MustCompile(
		`(?i)\b(?:https?://)?(?:[\w-]+\.)?(?:facebook\.com|fb\.watch)/[^\s<>()]+`,
	)
	facebookFilenameRegexp    = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
	facebookSearchExpRegexp   = regexp.MustCompile(`(?m)\bk_exp\s*=\s*["']([^"']+)["']`)
	facebookSearchTokenRegexp = regexp.MustCompile(`(?m)\bk_token\s*=\s*["']([^"']+)["']`)
	facebookQualityRegexp     = regexp.MustCompile(`(\d{3,4})`)
)

const facebookRequestContextErrorFormat = "facebook request context: %w"

type facebookFetcher interface {
	fetch(
		ctx context.Context,
		rawURL string,
		extractorConfig facebookExtractorConfig,
	) (facebookVideoContent, error)
}

type facebookScraper interface {
	Get(url string) (*http.Response, error)
	Post(url string, contentType string, body io.Reader) (*http.Response, error)
}

type facebookClient struct {
	httpClient        *http.Client
	scraper           facebookScraper
	pageURL           string
	searchURL         string
	getMyFBProcessURL string
}

type facebookVideoContent struct {
	ResolvedURL string
	DownloadURL string
	MediaPart   contentPart
}

type facebookSearchTokens struct {
	Exp   string
	Token string
}

type facebookSearchResponse struct {
	Status       string `json:"status"`
	Data         string `json:"data"`
	ErrorMessage string `json:"mess"`
}

type facebookDownloadCandidate struct {
	QualityLabel string
	Score        int
	DirectURL    string
}

func (content facebookVideoContent) resolvedURL() string {
	return strings.TrimSpace(content.ResolvedURL)
}

func (content facebookVideoContent) mediaPart() contentPart {
	return content.MediaPart
}

func newFacebookClient(httpClient *http.Client) (facebookClient, error) {
	scraper, err := cloudscraper.New(cloudscraper.WithBrowser(useragent.Config{
		Browser:  "firefox",
		Custom:   "",
		Desktop:  true,
		Mobile:   false,
		Platform: "linux",
	}))
	if err != nil {
		return facebookClient{}, fmt.Errorf("create facebook scraper: %w", err)
	}

	return facebookClient{
		httpClient:        httpClient,
		scraper:           scraper,
		pageURL:           defaultFacebookPageURL,
		searchURL:         defaultFacebookSearchURL,
		getMyFBProcessURL: defaultFacebookGetMyFBProcessURL,
	}, nil
}

func (instance *bot) maybeAugmentConversationWithFacebook(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	conversation []chatMessage,
	urlExtractionText string,
) ([]chatMessage, []string, error) {
	preparedAugmentation, err := instance.prepareFacebookAugmentation(
		ctx,
		loadedConfig,
		providerSlashModel,
		urlExtractionText,
	)
	if err != nil {
		return nil, nil, err
	}

	augmentedConversation, err := applyPreparedConversationAugmentation(
		conversation,
		preparedAugmentation,
	)
	if err != nil {
		return nil, nil, err
	}

	return augmentedConversation, preparedAugmentation.warnings, nil
}

func (instance *bot) prepareFacebookAugmentation(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	urlExtractionText string,
) (preparedConversationAugmentation, error) {
	if instance.facebook == nil {
		return emptyPreparedConversationAugmentation(), nil
	}

	facebookURLs := extractFacebookURLs(urlExtractionText)
	if len(facebookURLs) == 0 {
		return emptyPreparedConversationAugmentation(), nil
	}

	videoContents, warnings := fetchDownloadedVideos(
		ctx,
		facebookURLs,
		func(fetchCtx context.Context, rawURL string) (facebookVideoContent, error) {
			return instance.facebook.fetch(fetchCtx, rawURL, loadedConfig.Facebook)
		},
		"fetch facebook content",
		facebookWarningText,
	)
	if len(videoContents) == 0 {
		return warningPreparedConversationAugmentation(warnings), nil
	}

	return prepareDownloadedVideoAugmentation(
		ctx,
		downloadedVideoAugmentationRequest[facebookVideoContent]{
			instance:           instance,
			loadedConfig:       loadedConfig,
			providerSlashModel: providerSlashModel,
			videoContents:      videoContents,
			warnings:           warnings,
			warningText:        facebookWarningText,
			label:              "facebook",
		},
	)
}

func extractFacebookURLs(text string) []string {
	text = normalizedURLExtractionText(text)

	matches := facebookURLRegexp.FindAllString(text, -1)
	normalizedURLs := make([]string, 0, len(matches))
	seenURLs := make(map[string]struct{}, len(matches))

	for _, match := range matches {
		normalizedURL, err := normalizeFacebookURL(match)
		if err != nil {
			continue
		}

		if _, seen := seenURLs[normalizedURL]; seen {
			continue
		}

		seenURLs[normalizedURL] = struct{}{}
		normalizedURLs = append(normalizedURLs, normalizedURL)
	}

	return normalizedURLs
}

func normalizeFacebookURL(rawURL string) (string, error) {
	cleanedURL := cleanWebsiteURL(rawURL)
	if cleanedURL == "" {
		return "", fmt.Errorf("empty facebook url: %w", os.ErrInvalid)
	}

	if !strings.Contains(cleanedURL, "://") {
		cleanedURL = "https://" + cleanedURL
	}

	parsedURL, err := url.Parse(cleanedURL)
	if err != nil {
		return "", fmt.Errorf("parse facebook url %q: %w", rawURL, err)
	}

	if !isWebsiteScheme(parsedURL.Scheme) || strings.TrimSpace(parsedURL.Hostname()) == "" {
		return "", fmt.Errorf("unsupported facebook url %q: %w", rawURL, os.ErrInvalid)
	}

	if !isFacebookHost(parsedURL.Hostname()) {
		return "", fmt.Errorf("unsupported facebook host in %q: %w", rawURL, os.ErrInvalid)
	}

	parsedURL.Scheme = strings.ToLower(parsedURL.Scheme)
	parsedURL.Host = strings.ToLower(parsedURL.Host)
	parsedURL.Fragment = ""

	return parsedURL.String(), nil
}

func (client facebookClient) fetch(
	ctx context.Context,
	rawURL string,
	extractorConfig facebookExtractorConfig,
) (facebookVideoContent, error) {
	requestContext, cancel := context.WithTimeout(ctx, facebookRequestTimeout)
	defer cancel()

	normalizedURL, err := normalizeFacebookURL(rawURL)
	if err != nil {
		return facebookVideoContent{}, err
	}

	extractorConfig = normalizeFacebookConfig(rawFacebookConfig{
		PrimaryProvider:  scalarString(extractorConfig.PrimaryProvider),
		FallbackProvider: scalarString(extractorConfig.FallbackProvider),
	})

	providers := extractorConfig.providersInOrder()
	errs := make([]error, 0, len(providers))

	for _, provider := range providers {
		videoContent, fetchErr := client.fetchWithProvider(
			requestContext,
			normalizedURL,
			provider,
		)
		if fetchErr == nil {
			return videoContent, nil
		}

		errs = append(errs, fmt.Errorf("%s: %w", provider, fetchErr))
	}

	return facebookVideoContent{}, fmt.Errorf(
		"fetch facebook content: %w",
		errors.Join(errs...),
	)
}

func (client facebookClient) fetchWithProvider(
	ctx context.Context,
	normalizedURL string,
	provider facebookExtractorProviderKind,
) (facebookVideoContent, error) {
	switch provider {
	case facebookExtractorProviderKindFDownloader:
		return client.fetchWithFDownloader(ctx, normalizedURL)
	case facebookExtractorProviderKindGetMyFB:
		return client.fetchWithGetMyFB(ctx, normalizedURL)
	default:
		return facebookVideoContent{}, fmt.Errorf(
			"unsupported facebook extractor provider %q: %w",
			provider,
			os.ErrInvalid,
		)
	}
}

func (client facebookClient) fetchWithFDownloader(
	ctx context.Context,
	normalizedURL string,
) (facebookVideoContent, error) {
	searchTokens, err := client.fetchSearchTokens(ctx)
	if err != nil {
		return facebookVideoContent{}, fmt.Errorf("fetch facebook search page: %w", err)
	}

	downloadCandidates, err := client.fetchDownloadCandidates(
		ctx,
		normalizedURL,
		searchTokens,
	)
	if err != nil {
		return facebookVideoContent{}, fmt.Errorf("parse facebook search results: %w", err)
	}

	return client.downloadFacebookVideo(ctx, normalizedURL, downloadCandidates)
}

func (client facebookClient) fetchWithGetMyFB(
	ctx context.Context,
	normalizedURL string,
) (facebookVideoContent, error) {
	downloadCandidates, err := client.fetchGetMyFBDownloadCandidates(ctx, normalizedURL)
	if err != nil {
		return facebookVideoContent{}, fmt.Errorf("parse getmyfb search results: %w", err)
	}

	return client.downloadFacebookVideo(ctx, normalizedURL, downloadCandidates)
}

func (client facebookClient) downloadFacebookVideo(
	ctx context.Context,
	normalizedURL string,
	downloadCandidates []facebookDownloadCandidate,
) (facebookVideoContent, error) {
	var lastErr error

	for _, candidate := range downloadCandidates {
		downloadURL := strings.TrimSpace(candidate.DirectURL)

		videoBytes, mimeType, filename, err := client.downloadVideo(
			ctx,
			downloadURL,
			normalizedURL,
		)
		if err != nil {
			lastErr = fmt.Errorf("download facebook %s video: %w", candidate.QualityLabel, err)

			continue
		}

		return facebookVideoContent{
			ResolvedURL: normalizedURL,
			DownloadURL: downloadURL,
			MediaPart: contentPart{
				"type":               contentTypeVideoData,
				contentFieldBytes:    videoBytes,
				contentFieldMIMEType: mimeType,
				contentFieldFilename: filename,
			},
		}, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("find facebook download url: %w", os.ErrInvalid)
	}

	return facebookVideoContent{}, lastErr
}

func (client facebookClient) fetchGetMyFBDownloadCandidates(
	ctx context.Context,
	facebookURL string,
) ([]facebookDownloadCandidate, error) {
	err := ctx.Err()
	if err != nil {
		return nil, fmt.Errorf(facebookRequestContextErrorFormat, err)
	}

	formValues := url.Values{
		"id":     {facebookURL},
		"locale": {facebookGetMyFBLocale},
	}

	httpResponse, err := client.scraper.Post(
		client.getMyFBProcessURL,
		"application/x-www-form-urlencoded; charset=UTF-8",
		strings.NewReader(formValues.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("send getmyfb request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, fmt.Errorf("read getmyfb response: %w", err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf(
			"getmyfb request failed with status %d: %s: %w",
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	err = ctx.Err()
	if err != nil {
		return nil, fmt.Errorf(facebookRequestContextErrorFormat, err)
	}

	return parseGetMyFBDownloadCandidates(
		client.getMyFBProcessURL,
		httpResponse.Header.Get("Hx-Trigger"),
		responseBody,
	)
}

func (client facebookClient) fetchSearchTokens(
	ctx context.Context,
) (facebookSearchTokens, error) {
	err := ctx.Err()
	if err != nil {
		return facebookSearchTokens{}, fmt.Errorf(facebookRequestContextErrorFormat, err)
	}

	httpResponse, err := client.scraper.Get(client.pageURL)
	if err != nil {
		return facebookSearchTokens{}, fmt.Errorf("send facebook search page request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return facebookSearchTokens{}, fmt.Errorf("read facebook search page response: %w", err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return facebookSearchTokens{}, fmt.Errorf(
			"facebook search page request failed with status %d: %s: %w",
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	err = ctx.Err()
	if err != nil {
		return facebookSearchTokens{}, fmt.Errorf(facebookRequestContextErrorFormat, err)
	}

	return parseFacebookSearchTokens(responseBody)
}

func (client facebookClient) fetchDownloadCandidates(
	ctx context.Context,
	facebookURL string,
	searchTokens facebookSearchTokens,
) ([]facebookDownloadCandidate, error) {
	err := ctx.Err()
	if err != nil {
		return nil, fmt.Errorf(facebookRequestContextErrorFormat, err)
	}

	formValues := url.Values{
		"k_exp":   {searchTokens.Exp},
		"k_token": {searchTokens.Token},
		"q":       {facebookURL},
		"lang":    {facebookSearchLanguage},
		"web":     {facebookSearchWebHost(client.pageURL)},
		"v":       {facebookSearchVersion},
		"w":       {""},
		"cftoken": {""},
	}

	httpResponse, err := client.scraper.Post(
		client.searchURL,
		"application/x-www-form-urlencoded; charset=UTF-8",
		strings.NewReader(formValues.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("send facebook search request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, fmt.Errorf("read facebook search response: %w", err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf(
			"facebook search request failed with status %d: %s: %w",
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	err = ctx.Err()
	if err != nil {
		return nil, fmt.Errorf(facebookRequestContextErrorFormat, err)
	}

	return parseFacebookDownloadCandidates(client.searchURL, responseBody)
}

func parseFacebookSearchTokens(responseBody []byte) (facebookSearchTokens, error) {
	responseText := string(responseBody)
	searchTokens := facebookSearchTokens{
		Exp:   strings.TrimSpace(firstRegexpGroup(responseText, facebookSearchExpRegexp)),
		Token: strings.TrimSpace(firstRegexpGroup(responseText, facebookSearchTokenRegexp)),
	}

	if searchTokens.Exp == "" || searchTokens.Token == "" {
		return facebookSearchTokens{}, fmt.Errorf("find facebook search tokens: %w", os.ErrInvalid)
	}

	return searchTokens, nil
}

func parseFacebookDownloadCandidates(
	baseURL string,
	responseBody []byte,
) ([]facebookDownloadCandidate, error) {
	responseFragment, err := facebookSearchResponseFragment(responseBody)
	if err != nil {
		return nil, err
	}

	document, err := parseFacebookDownloadDocument(responseFragment, "parse facebook result html")
	if err != nil {
		return nil, err
	}

	downloadCandidates := collectFacebookDownloadCandidates(
		document,
		func(node *html.Node) (facebookDownloadCandidate, bool) {
			if node.Type != html.ElementNode || node.DataAtom != atom.Tr {
				return emptyFacebookDownloadCandidate(), false
			}

			return extractFacebookDownloadCandidate(baseURL, node)
		},
	)
	if len(downloadCandidates) == 0 {
		return nil, fmt.Errorf("find facebook download url: %w", os.ErrInvalid)
	}

	sortFacebookDownloadCandidates(downloadCandidates)

	return downloadCandidates, nil
}

func parseGetMyFBDownloadCandidates(
	baseURL, responseTrigger string,
	responseBody []byte,
) ([]facebookDownloadCandidate, error) {
	trimmedResponseBody, err := normalizedGetMyFBResponseBody(responseBody)
	if err != nil {
		return nil, err
	}

	document, err := parseFacebookDownloadDocument(trimmedResponseBody, "parse getmyfb result html")
	if err != nil {
		return nil, err
	}

	downloadCandidates := collectFacebookDownloadCandidates(
		document,
		func(node *html.Node) (facebookDownloadCandidate, bool) {
			if node.Type != html.ElementNode || node.DataAtom != atom.Li || !hasHTMLClass(node, "results-list-item") {
				return emptyFacebookDownloadCandidate(), false
			}

			return extractGetMyFBDownloadCandidate(baseURL, node)
		},
	)
	if len(downloadCandidates) == 0 {
		message := getMyFBDownloadErrorMessage(document, responseTrigger, trimmedResponseBody)

		return nil, fmt.Errorf("getmyfb response did not include video downloads: %s: %w", message, os.ErrInvalid)
	}

	sortFacebookDownloadCandidates(downloadCandidates)

	return downloadCandidates, nil
}

func extractFacebookDownloadCandidate(
	baseURL string,
	row *html.Node,
) (facebookDownloadCandidate, bool) {
	qualityLabel := ""
	directURL := ""

	var walk func(*html.Node)

	walk = func(node *html.Node) {
		if node == nil {
			return
		}

		if node.Type == html.ElementNode && node.DataAtom == atom.Td &&
			strings.EqualFold(strings.TrimSpace(htmlAttribute(node, "class")), "video-quality") {
			qualityLabel = strings.TrimSpace(nodeTextContent(node))
		}

		if node.Type == html.ElementNode && node.DataAtom == atom.A {
			href := strings.TrimSpace(htmlAttribute(node, "href"))
			if href != "" && strings.EqualFold(strings.TrimSpace(nodeTextContent(node)), "Download") && directURL == "" {
				directURL = resolveFacebookDownloadURL(baseURL, html.UnescapeString(href))
			}
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(row)

	qualityLabel = strings.TrimSpace(qualityLabel)
	if strings.TrimSpace(directURL) == "" {
		return facebookDownloadCandidate{
			QualityLabel: "",
			Score:        0,
			DirectURL:    "",
		}, false
	}

	return facebookDownloadCandidate{
		QualityLabel: qualityLabel,
		Score:        facebookDownloadQualityScore(qualityLabel),
		DirectURL:    strings.TrimSpace(directURL),
	}, true
}

func extractGetMyFBDownloadCandidate(
	baseURL string,
	row *html.Node,
) (facebookDownloadCandidate, bool) {
	qualityLabel := facebookGetMyFBQualityLabel(row)
	directURL, skipCandidate := getMyFBDownloadCandidateDetails(baseURL, row, qualityLabel)

	if skipCandidate || strings.TrimSpace(directURL) == "" {
		return facebookDownloadCandidate{
			QualityLabel: "",
			Score:        0,
			DirectURL:    "",
		}, false
	}

	return facebookDownloadCandidate{
		QualityLabel: qualityLabel,
		Score:        facebookDownloadQualityScore(qualityLabel),
		DirectURL:    strings.TrimSpace(directURL),
	}, true
}

func facebookSearchResponseFragment(responseBody []byte) (string, error) {
	var response facebookSearchResponse

	err := json.Unmarshal(responseBody, &response)
	if err != nil {
		return "", fmt.Errorf("decode facebook search response: %w", err)
	}

	if !strings.EqualFold(strings.TrimSpace(response.Status), "ok") {
		message := strings.TrimSpace(response.ErrorMessage)
		if message == "" {
			message = strings.TrimSpace(string(responseBody))
		}

		return "", fmt.Errorf("facebook search response not ok: %s: %w", message, os.ErrInvalid)
	}

	responseFragment := strings.TrimSpace(response.Data)
	if responseFragment == "" {
		return "", fmt.Errorf("empty facebook search response: %w", os.ErrInvalid)
	}

	return responseFragment, nil
}

func normalizedGetMyFBResponseBody(responseBody []byte) (string, error) {
	trimmedResponseBody := strings.TrimSpace(string(responseBody))
	if trimmedResponseBody == "" {
		return "", fmt.Errorf("empty getmyfb response: %w", os.ErrInvalid)
	}

	if strings.HasPrefix(trimmedResponseBody, "{") {
		var response struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}

		err := json.Unmarshal(responseBody, &response)
		if err != nil {
			return "", fmt.Errorf("decode getmyfb response: %w", err)
		}

		message := strings.TrimSpace(response.Message)
		if message == "" {
			message = strings.TrimSpace(response.Error)
		}

		if message == "" {
			message = trimmedResponseBody
		}

		return "", fmt.Errorf("getmyfb response failed: %s: %w", message, os.ErrInvalid)
	}

	return trimmedResponseBody, nil
}

func parseFacebookDownloadDocument(responseText, context string) (*html.Node, error) {
	document, err := html.Parse(strings.NewReader("<html><body>" + responseText + "</body></html>"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", context, err)
	}

	return document, nil
}

func collectFacebookDownloadCandidates(
	document *html.Node,
	extractCandidate func(*html.Node) (facebookDownloadCandidate, bool),
) []facebookDownloadCandidate {
	downloadCandidates := make([]facebookDownloadCandidate, 0, facebookDownloadCandidateCapacity)

	var walk func(*html.Node)

	walk = func(node *html.Node) {
		if node == nil {
			return
		}

		candidate, ok := extractCandidate(node)
		if ok {
			downloadCandidates = append(downloadCandidates, candidate)
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(document)

	return downloadCandidates
}

func sortFacebookDownloadCandidates(downloadCandidates []facebookDownloadCandidate) {
	sort.SliceStable(downloadCandidates, func(leftIndex, rightIndex int) bool {
		leftCandidate := downloadCandidates[leftIndex]
		rightCandidate := downloadCandidates[rightIndex]

		if leftCandidate.Score != rightCandidate.Score {
			return leftCandidate.Score > rightCandidate.Score
		}

		return strings.Compare(leftCandidate.QualityLabel, rightCandidate.QualityLabel) < 0
	})
}

func getMyFBDownloadErrorMessage(document *html.Node, responseTrigger string, responseBody string) string {
	message := strings.TrimSpace(findGetMyFBErrorMessage(document))
	if message == "" {
		message = strings.TrimSpace(responseTrigger)
	}

	if message == "" {
		message = strings.TrimSpace(responseBody)
	}

	return message
}

func getMyFBDownloadCandidateDetails(
	baseURL string,
	row *html.Node,
	qualityLabel string,
) (string, bool) {
	directURL := ""
	skipCandidate := false

	var walk func(*html.Node)

	walk = func(node *html.Node) {
		if node == nil {
			return
		}

		if node.Type == html.ElementNode && node.DataAtom == atom.A {
			if hasHTMLClass(node, "mp3") || strings.EqualFold(qualityLabel, "mp3") {
				skipCandidate = true

				return
			}

			href := strings.TrimSpace(htmlAttribute(node, "href"))
			if href != "" && strings.EqualFold(strings.TrimSpace(nodeTextContent(node)), "Download") && directURL == "" {
				directURL = resolveFacebookDownloadURL(baseURL, html.UnescapeString(href))
			}
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(row)

	return directURL, skipCandidate
}

func emptyFacebookDownloadCandidate() facebookDownloadCandidate {
	return facebookDownloadCandidate{
		QualityLabel: "",
		Score:        0,
		DirectURL:    "",
	}
}

func facebookGetMyFBQualityLabel(row *html.Node) string {
	qualityLabel := strings.TrimSpace(nodeTextContent(row))
	if qualityLabel == "" {
		return ""
	}

	qualityLabel = strings.TrimSpace(strings.TrimSuffix(qualityLabel, "Download"))

	return strings.TrimSpace(qualityLabel)
}

func findGetMyFBErrorMessage(document *html.Node) string {
	if document == nil {
		return ""
	}

	var walk func(*html.Node) string

	walk = func(node *html.Node) string {
		if node == nil {
			return ""
		}

		if node.Type == html.ElementNode && hasHTMLClass(node, "result-error") {
			return strings.TrimSpace(nodeTextContent(node))
		}

		if node.Type == html.ElementNode && hasHTMLClass(node, "result-login") {
			return strings.TrimSpace(nodeTextContent(node))
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			text := walk(child)
			if text != "" {
				return text
			}
		}

		return ""
	}

	return walk(document)
}

func facebookDownloadQualityScore(qualityLabel string) int {
	normalizedQuality := strings.TrimSpace(qualityLabel)
	if normalizedQuality == "" {
		return 0
	}

	qualityMatch := facebookQualityRegexp.FindStringSubmatch(normalizedQuality)
	if len(qualityMatch) >= facebookRegexpMatchGroupCount {
		qualityScore, err := strconv.Atoi(strings.TrimSpace(qualityMatch[1]))
		if err == nil {
			return qualityScore
		}
	}

	switch {
	case strings.Contains(strings.ToLower(normalizedQuality), "hd"):
		return facebookDefaultHDQualityScore
	case strings.Contains(strings.ToLower(normalizedQuality), "sd"):
		return facebookDefaultSDQualityScore
	default:
		return 0
	}
}

func facebookSearchWebHost(pageURL string) string {
	parsedURL, err := url.Parse(pageURL)
	if err != nil {
		return "fdownloader.net"
	}

	host := strings.TrimSpace(parsedURL.Hostname())
	if host == "" {
		return "fdownloader.net"
	}

	return host
}

func resolveFacebookDownloadURL(pageURL, rawURL string) string {
	trimmedURL := strings.TrimSpace(rawURL)
	if trimmedURL == "" {
		return ""
	}

	baseURL, err := url.Parse(pageURL)
	if err != nil {
		return trimmedURL
	}

	relativeURL, err := url.Parse(trimmedURL)
	if err != nil {
		return trimmedURL
	}

	return baseURL.ResolveReference(relativeURL).String()
}

func firstRegexpGroup(text string, pattern *regexp.Regexp) string {
	matches := pattern.FindStringSubmatch(text)
	if len(matches) < facebookRegexpMatchGroupCount {
		return ""
	}

	return strings.TrimSpace(matches[1])
}

func (client facebookClient) downloadVideo(
	ctx context.Context,
	downloadURL string,
	sourceURL string,
) ([]byte, string, string, error) {
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		downloadURL,
		nil,
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("create facebook video request: %w", err)
	}

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return nil, "", "", fmt.Errorf("send facebook video request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	videoBytes, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read facebook video response: %w", err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, "", "", fmt.Errorf(
			"facebook video request failed with status %d: %s: %w",
			httpResponse.StatusCode,
			strings.TrimSpace(string(videoBytes)),
			os.ErrInvalid,
		)
	}

	mimeType := normalizedFacebookMIMEType(httpResponse.Header.Get("Content-Type"))
	filename := facebookFilename(
		sourceURL,
		httpResponse.Header.Get("Content-Disposition"),
	)

	return videoBytes, mimeType, filename, nil
}

func normalizedFacebookMIMEType(contentType string) string {
	trimmedContentType := strings.TrimSpace(contentType)
	if trimmedContentType == "" {
		return facebookDefaultMIMEType
	}

	mediaType, _, err := mime.ParseMediaType(trimmedContentType)
	if err != nil {
		return facebookDefaultMIMEType
	}

	if strings.TrimSpace(mediaType) == "" {
		return facebookDefaultMIMEType
	}

	if strings.EqualFold(mediaType, "application/octet-stream") {
		return facebookDefaultMIMEType
	}

	return mediaType
}

func facebookFilename(sourceURL, contentDisposition string) string {
	trimmedContentDisposition := strings.TrimSpace(contentDisposition)
	if trimmedContentDisposition != "" {
		_, params, err := mime.ParseMediaType(trimmedContentDisposition)
		if err == nil {
			filename := strings.TrimSpace(params["filename"])
			if filename != "" {
				return filename
			}
		}
	}

	videoID := facebookVideoIdentifier(sourceURL)
	if videoID != "" {
		return facebookFilenamePrefix + videoID + ".mp4"
	}

	return facebookDefaultFilename
}

func facebookVideoIdentifier(rawURL string) string {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	queryID := facebookFileStem(parsedURL.Query().Get("v"))
	if queryID != "" {
		return queryID
	}

	trimmedPath := strings.Trim(parsedURL.Path, "/")
	if trimmedPath == "" {
		return ""
	}

	segments := strings.Split(trimmedPath, "/")
	for index := len(segments) - 1; index >= 0; index-- {
		segment := strings.TrimSpace(segments[index])
		if segment == "" {
			continue
		}

		switch strings.ToLower(segment) {
		case "reel", "watch", "videos", "posts", "share", "v", "story", "stories":
			continue
		}

		return facebookFileStem(segment)
	}

	return ""
}

func facebookFileStem(value string) string {
	sanitizedValue := facebookFilenameRegexp.ReplaceAllString(strings.TrimSpace(value), "_")
	sanitizedValue = strings.Trim(sanitizedValue, "_")

	return sanitizedValue
}
