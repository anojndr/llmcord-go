package main

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	cloudscraper "github.com/Advik-B/cloudscraper/lib"
	useragent "github.com/Advik-B/cloudscraper/lib/user_agent"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	defaultFacebookDownloadURL = "https://fdown.net/download.php"
	facebookDefaultFilename    = "facebook.mp4"
	facebookDefaultMIMEType    = "video/mp4"
	facebookDownloadFieldName  = "URLz"
	facebookFilenamePrefix     = "facebook_"
	facebookWarningText        = "Warning: Facebook content unavailable"
)

var (
	facebookURLRegexp = regexp.MustCompile(
		`(?i)\b(?:https?://)?(?:[\w-]+\.)?(?:facebook\.com|fb\.watch)/[^\s<>()]+`,
	)
	facebookFilenameRegexp = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
)

type facebookContentClient interface {
	fetch(ctx context.Context, rawURL string) (facebookVideoContent, error)
}

type facebookScraper interface {
	Post(url string, contentType string, body io.Reader) (*http.Response, error)
}

type facebookClient struct {
	httpClient  *http.Client
	scraper     facebookScraper
	downloadURL string
}

type facebookVideoContent struct {
	ResolvedURL string
	DownloadURL string
	MediaPart   contentPart
}

type facebookDownloadLinks struct {
	SDLink string
	HDLink string
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
		httpClient:  httpClient,
		scraper:     scraper,
		downloadURL: defaultFacebookDownloadURL,
	}, nil
}

func (instance *bot) maybeAugmentConversationWithFacebook(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	conversation []chatMessage,
	urlExtractionText string,
) ([]chatMessage, []string, error) {
	if instance.facebook == nil {
		return conversation, nil, nil
	}

	facebookURLs := extractFacebookURLs(urlExtractionText)
	if len(facebookURLs) == 0 {
		return conversation, nil, nil
	}

	videoContents, warnings := fetchDownloadedVideos(
		ctx,
		facebookURLs,
		instance.facebook.fetch,
		"fetch facebook content",
		facebookWarningText,
	)
	if len(videoContents) == 0 {
		return conversation, warnings, nil
	}

	return augmentConversationWithDownloadedVideos(
		ctx,
		instance,
		loadedConfig,
		providerSlashModel,
		conversation,
		videoContents,
		warnings,
		facebookWarningText,
		"facebook",
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
) (facebookVideoContent, error) {
	requestContext, cancel := context.WithTimeout(ctx, facebookRequestTimeout)
	defer cancel()

	normalizedURL, err := normalizeFacebookURL(rawURL)
	if err != nil {
		return facebookVideoContent{}, err
	}

	responseBody, responseURL, err := client.fetchDownloadPage(requestContext, normalizedURL)
	if err != nil {
		return facebookVideoContent{}, fmt.Errorf("fetch facebook download page: %w", err)
	}

	downloadURL, err := parseFacebookDownloadURL(responseURL, responseBody)
	if err != nil {
		return facebookVideoContent{}, fmt.Errorf("parse facebook download page: %w", err)
	}

	videoBytes, mimeType, filename, err := client.downloadVideo(
		requestContext,
		downloadURL,
		normalizedURL,
	)
	if err != nil {
		return facebookVideoContent{}, fmt.Errorf("download facebook video: %w", err)
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

func (client facebookClient) fetchDownloadPage(
	ctx context.Context,
	facebookURL string,
) ([]byte, string, error) {
	err := ctx.Err()
	if err != nil {
		return nil, "", fmt.Errorf("facebook request context: %w", err)
	}

	formValues := url.Values{
		facebookDownloadFieldName: {facebookURL},
	}

	httpResponse, err := client.scraper.Post(
		client.downloadURL,
		"application/x-www-form-urlencoded",
		strings.NewReader(formValues.Encode()),
	)
	if err != nil {
		return nil, "", fmt.Errorf("send facebook download request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read facebook download response: %w", err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, "", fmt.Errorf(
			"facebook download request failed with status %d: %s: %w",
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	err = ctx.Err()
	if err != nil {
		return nil, "", fmt.Errorf("facebook request context: %w", err)
	}

	responseURL := client.downloadURL
	if httpResponse.Request != nil && httpResponse.Request.URL != nil {
		responseURL = httpResponse.Request.URL.String()
	}

	return responseBody, responseURL, nil
}

func parseFacebookDownloadURL(pageURL string, responseBody []byte) (string, error) {
	document, err := html.Parse(strings.NewReader(string(responseBody)))
	if err != nil {
		return "", fmt.Errorf("parse facebook html document: %w", err)
	}

	downloadLinks := extractFacebookDownloadLinks(document, pageURL)
	bestDownloadURL := strings.TrimSpace(downloadLinks.HDLink)

	if bestDownloadURL != "" {
		return bestDownloadURL, nil
	}

	bestDownloadURL = strings.TrimSpace(downloadLinks.SDLink)
	if bestDownloadURL != "" {
		return bestDownloadURL, nil
	}

	return "", fmt.Errorf("find facebook download url: %w", os.ErrInvalid)
}

func extractFacebookDownloadLinks(
	document *html.Node,
	pageURL string,
) facebookDownloadLinks {
	downloadLinks := facebookDownloadLinks{
		HDLink: "",
		SDLink: "",
	}

	var walk func(*html.Node)

	walk = func(node *html.Node) {
		if node == nil {
			return
		}

		if node.Type == html.ElementNode && node.DataAtom == atom.A {
			updateFacebookDownloadLinks(&downloadLinks, pageURL, node)
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(document)

	return downloadLinks
}

func updateFacebookDownloadLinks(
	downloadLinks *facebookDownloadLinks,
	pageURL string,
	node *html.Node,
) {
	href := strings.TrimSpace(htmlAttribute(node, "href"))
	if href == "" {
		return
	}

	switch strings.ToLower(strings.TrimSpace(htmlAttribute(node, "id"))) {
	case "sdlink":
		if downloadLinks.SDLink == "" {
			downloadLinks.SDLink = resolveFacebookDownloadURL(pageURL, href)
		}
	case "hdlink":
		if downloadLinks.HDLink == "" {
			downloadLinks.HDLink = resolveFacebookDownloadURL(pageURL, href)
		}
	}
}

func resolveFacebookDownloadURL(pageURL string, rawURL string) string {
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

func facebookFilename(sourceURL string, contentDisposition string) string {
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
