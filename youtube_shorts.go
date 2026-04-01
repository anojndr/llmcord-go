package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultYouTubeShortsInfoURL   = "https://www.acethinker.ai/downloader/api/dlapinewv2.php"
	defaultYouTubeShortsLoaderURL = "https://www.acethinker.ai/downloader/api/iframe_stuff/iframe_api_loader.php"
	youtubeShortsDefaultFilename  = "youtube_shorts.mp4"
	youtubeShortsDefaultMIMEType  = "video/mp4"
	youtubeShortsFilenamePrefix   = "youtube_shorts_"
	youtubeShortsNoCodec          = "none"
	youtubeShortsWarningText      = "Warning: YouTube Shorts content unavailable"
	youtubeQuality4K              = 2160
	youtubeQuality8K              = 4320
)

type youtubeShortsFetcher interface {
	fetch(ctx context.Context, rawURL string) (youtubeShortsVideoContent, error)
}

type youtubeShortsClient struct {
	httpClient         *http.Client
	infoURL            string
	loaderURL          string
	userAgent          string
	requestTimeout     time.Duration
	loaderPollInterval time.Duration
}

type youtubeShortsVideoContent struct {
	ResolvedURL string
	DownloadURL string
	MediaPart   contentPart
}

type aceThinkerYouTubeShortsInfoResponse struct {
	ResData aceThinkerYouTubeShortsInfo `json:"res_data"`
}

type aceThinkerYouTubeShortsInfo struct {
	Title   string                        `json:"title"`
	Message string                        `json:"message"`
	Formats []aceThinkerYouTubeShortsItem `json:"formats"`
}

type aceThinkerYouTubeShortsItem struct {
	URL      string `json:"url"`
	Filesize int64  `json:"filesize"`
	Quality  string `json:"quality"`
	ACodec   string `json:"acodec"`
	VCodec   string `json:"vcodec"`
	Ext      string `json:"ext"`
	Protocol string `json:"protocol"`
}

type aceThinkerYouTubeShortsLoaderResponse struct {
	Success     bool   `json:"success"`
	ID          string `json:"id"`
	Message     string `json:"message"`
	ProgressURL string `json:"progress_url"`
}

type aceThinkerYouTubeShortsProgressResponse struct {
	Success     int    `json:"success"`
	Progress    int    `json:"progress"`
	DownloadURL string `json:"download_url"`
	Message     string `json:"message"`
	Text        string `json:"text"`
}

func (content youtubeShortsVideoContent) resolvedURL() string {
	return strings.TrimSpace(content.ResolvedURL)
}

func (content youtubeShortsVideoContent) mediaPart() contentPart {
	return content.MediaPart
}

func newYouTubeShortsClient(httpClient *http.Client) youtubeShortsClient {
	return youtubeShortsClient{
		httpClient:         httpClient,
		infoURL:            defaultYouTubeShortsInfoURL,
		loaderURL:          defaultYouTubeShortsLoaderURL,
		userAgent:          youtubeUserAgent,
		requestTimeout:     youtubeShortsRequestTimeout,
		loaderPollInterval: youtubeShortsLoaderPollInterval,
	}
}

func (instance *bot) maybeAugmentConversationWithYouTubeShorts(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	conversation []chatMessage,
	urlExtractionText string,
) ([]chatMessage, []string, error) {
	preparedAugmentation, err := instance.prepareYouTubeShortsAugmentation(
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

func (instance *bot) prepareYouTubeShortsAugmentation(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	urlExtractionText string,
) (preparedConversationAugmentation, error) {
	if instance.youtubeShorts == nil {
		return emptyPreparedConversationAugmentation(), nil
	}

	shortsURLs := extractYouTubeShortsURLs(urlExtractionText)
	if len(shortsURLs) == 0 {
		return emptyPreparedConversationAugmentation(), nil
	}

	videoContents, warnings := fetchDownloadedVideos(
		ctx,
		shortsURLs,
		instance.youtubeShorts.fetch,
		"fetch youtube shorts content",
		youtubeShortsWarningText,
	)
	if len(videoContents) == 0 {
		return warningPreparedConversationAugmentation(warnings), nil
	}

	return prepareDownloadedVideoAugmentation(
		ctx,
		downloadedVideoAugmentationRequest[youtubeShortsVideoContent]{
			instance:           instance,
			loadedConfig:       loadedConfig,
			providerSlashModel: providerSlashModel,
			videoContents:      videoContents,
			warnings:           warnings,
			warningText:        youtubeShortsWarningText,
			label:              "youtube shorts",
		},
	)
}

func extractYouTubeShortsURLs(text string) []string {
	text = normalizedURLExtractionText(text)

	matches := youtubeURLRegexp.FindAllString(text, -1)
	normalizedURLs := make([]string, 0, len(matches))
	seenVideoIDs := make(map[string]struct{}, len(matches))

	for _, match := range matches {
		videoID, canonicalURL, err := parseYouTubeShortsURL(match)
		if err != nil {
			continue
		}

		if _, seen := seenVideoIDs[videoID]; seen {
			continue
		}

		seenVideoIDs[videoID] = struct{}{}

		normalizedURLs = append(normalizedURLs, canonicalURL)
	}

	return normalizedURLs
}

func isYouTubeShortsURL(rawURL string) bool {
	_, _, err := parseYouTubeShortsURL(rawURL)

	return err == nil
}

func parseYouTubeShortsURL(rawURL string) (string, string, error) {
	parsedURL, err := parseRawYouTubeURL(rawURL)
	if err != nil {
		return "", "", err
	}

	host := strings.ToLower(strings.TrimSpace(parsedURL.Hostname()))
	if !isYouTubeHost(host) {
		return "", "", fmt.Errorf("unsupported youtube shorts url %q: %w", rawURL, os.ErrInvalid)
	}

	if !strings.HasPrefix(parsedURL.Path, "/shorts/") {
		return "", "", fmt.Errorf("extract youtube shorts id from %q: %w", rawURL, os.ErrInvalid)
	}

	videoID := firstPathSegment(strings.TrimPrefix(parsedURL.Path, "/shorts/"))
	if !youtubeVideoIDRegexp.MatchString(videoID) {
		return "", "", fmt.Errorf("extract youtube shorts id from %q: %w", rawURL, os.ErrInvalid)
	}

	return videoID, canonicalYouTubeShortsURL(videoID), nil
}

func (client youtubeShortsClient) fetch(
	ctx context.Context,
	rawURL string,
) (youtubeShortsVideoContent, error) {
	requestTimeout := client.requestTimeout
	if requestTimeout <= 0 {
		requestTimeout = youtubeShortsRequestTimeout
	}

	requestContext, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resolvedURL, err := normalizeYouTubeShortsURL(rawURL)
	if err != nil {
		return youtubeShortsVideoContent{}, err
	}

	info, err := client.fetchInfo(requestContext, resolvedURL)
	if err != nil {
		return youtubeShortsVideoContent{}, fmt.Errorf("fetch youtube shorts info: %w", err)
	}

	downloadURL, err := client.resolveDownloadURL(requestContext, resolvedURL, info.Formats)
	if err != nil {
		return youtubeShortsVideoContent{}, err
	}

	videoBytes, mimeType, filename, err := client.downloadVideo(
		requestContext,
		downloadURL,
		resolvedURL,
	)
	if err != nil {
		return youtubeShortsVideoContent{}, fmt.Errorf("download youtube shorts video: %w", err)
	}

	return youtubeShortsVideoContent{
		ResolvedURL: resolvedURL,
		DownloadURL: downloadURL,
		MediaPart: contentPart{
			"type":               contentTypeVideoData,
			contentFieldBytes:    videoBytes,
			contentFieldMIMEType: mimeType,
			contentFieldFilename: filename,
		},
	}, nil
}

func normalizeYouTubeShortsURL(rawURL string) (string, error) {
	_, canonicalURL, err := parseYouTubeShortsURL(rawURL)
	if err != nil {
		return "", err
	}

	return canonicalURL, nil
}

func (client youtubeShortsClient) fetchInfo(
	ctx context.Context,
	resolvedURL string,
) (aceThinkerYouTubeShortsInfo, error) {
	requestURL, err := buildYouTubeShortsRequestURL(client.infoURL, resolvedURL)
	if err != nil {
		return aceThinkerYouTubeShortsInfo{}, err
	}

	var response aceThinkerYouTubeShortsInfoResponse

	err = client.fetchJSON(ctx, requestURL, &response)
	if err != nil {
		return aceThinkerYouTubeShortsInfo{}, err
	}

	if len(response.ResData.Formats) == 0 {
		message := strings.TrimSpace(response.ResData.Message)
		if message == "" {
			message = "no downloadable formats"
		}

		return aceThinkerYouTubeShortsInfo{}, fmt.Errorf(
			"resolve youtube shorts formats: %s: %w",
			message,
			os.ErrInvalid,
		)
	}

	return response.ResData, nil
}

func buildYouTubeShortsRequestURL(baseURL, resolvedURL string) (string, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse youtube shorts request url %q: %w", baseURL, err)
	}

	queryValues := parsedURL.Query()
	queryValues.Set("url", resolvedURL)
	parsedURL.RawQuery = queryValues.Encode()

	return parsedURL.String(), nil
}

func (client youtubeShortsClient) resolveDownloadURL(
	ctx context.Context,
	resolvedURL string,
	formats []aceThinkerYouTubeShortsItem,
) (string, error) {
	if directFormat, ok := selectYouTubeShortsDirectFormat(formats); ok {
		return strings.TrimSpace(directFormat.URL), nil
	}

	loaderFormat, ok := selectYouTubeShortsLoaderFormat(formats)
	if !ok {
		return "", fmt.Errorf("select youtube shorts mp4 format: %w", os.ErrInvalid)
	}

	return client.fetchLoaderDownloadURL(ctx, resolvedURL, loaderFormat)
}

func selectYouTubeShortsDirectFormat(
	formats []aceThinkerYouTubeShortsItem,
) (aceThinkerYouTubeShortsItem, bool) {
	bestRank := -1

	var bestFormat aceThinkerYouTubeShortsItem

	for _, format := range formats {
		if !youtubeShortsFormatHasAudio(format) ||
			!youtubeShortsFormatHasVideo(format) ||
			!strings.EqualFold(strings.TrimSpace(format.Ext), "mp4") ||
			!youtubeShortsFormatUsesHTTP(format) {
			continue
		}

		rank := youtubeShortsQualityRank(format.Quality)
		if rank <= bestRank {
			continue
		}

		bestRank = rank
		bestFormat = format
	}

	return bestFormat, bestRank >= 0
}

func selectYouTubeShortsLoaderFormat(formats []aceThinkerYouTubeShortsItem) (string, bool) {
	bestRank := -1
	bestFormat := ""

	for _, format := range formats {
		if !youtubeShortsFormatHasVideo(format) || !youtubeShortsFormatUsesHTTP(format) {
			continue
		}

		loaderFormat, ok := youtubeShortsLoaderFormat(format.Quality)
		if !ok {
			continue
		}

		rank := youtubeShortsQualityRank(format.Quality)
		if rank <= bestRank {
			continue
		}

		bestRank = rank
		bestFormat = loaderFormat
	}

	return bestFormat, bestRank >= 0
}

func youtubeShortsLoaderFormat(quality string) (string, bool) {
	trimmedQuality := strings.ToLower(strings.TrimSpace(quality))
	switch trimmedQuality {
	case "":
		return "", false
	case "4320p", "8k":
		return "8k", true
	case "2160p", "4k":
		return "4k", true
	}

	trimmedQuality = strings.TrimSuffix(trimmedQuality, "p")
	if trimmedQuality == "" {
		return "", false
	}

	_, err := strconv.Atoi(trimmedQuality)
	if err != nil {
		return "", false
	}

	return trimmedQuality, true
}

func youtubeShortsQualityRank(quality string) int {
	loaderFormat, ok := youtubeShortsLoaderFormat(quality)
	if !ok {
		return -1
	}

	switch loaderFormat {
	case "4k":
		return youtubeQuality4K
	case "8k":
		return youtubeQuality8K
	default:
		rank, err := strconv.Atoi(loaderFormat)
		if err != nil {
			return -1
		}

		return rank
	}
}

func youtubeShortsFormatHasAudio(format aceThinkerYouTubeShortsItem) bool {
	audioCodec := strings.ToLower(strings.TrimSpace(format.ACodec))

	return audioCodec != "" && audioCodec != youtubeShortsNoCodec
}

func youtubeShortsFormatHasVideo(format aceThinkerYouTubeShortsItem) bool {
	videoCodec := strings.ToLower(strings.TrimSpace(format.VCodec))

	return videoCodec != "" && videoCodec != youtubeShortsNoCodec
}

func youtubeShortsFormatUsesHTTP(format aceThinkerYouTubeShortsItem) bool {
	normalizedProtocol := strings.ToLower(strings.TrimSpace(format.Protocol))
	if isWebsiteScheme(normalizedProtocol) {
		return true
	}

	parsedURL, err := url.Parse(strings.TrimSpace(format.URL))
	if err != nil {
		return false
	}

	return isWebsiteScheme(parsedURL.Scheme)
}

func (client youtubeShortsClient) fetchLoaderDownloadURL(
	ctx context.Context,
	resolvedURL string,
	loaderFormat string,
) (string, error) {
	requestURL, err := buildYouTubeShortsRequestURL(client.loaderURL, resolvedURL)
	if err != nil {
		return "", err
	}

	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("parse youtube shorts loader url %q: %w", requestURL, err)
	}

	queryValues := parsedURL.Query()
	queryValues.Set("f", loaderFormat)
	parsedURL.RawQuery = queryValues.Encode()

	var response aceThinkerYouTubeShortsLoaderResponse

	err = client.fetchJSON(ctx, parsedURL.String(), &response)
	if err != nil {
		return "", fmt.Errorf("fetch youtube shorts loader response: %w", err)
	}

	if !response.Success {
		message := strings.TrimSpace(response.Message)
		if message == "" {
			message = "youtube shorts loader rejected request"
		}

		return "", fmt.Errorf("%s: %w", message, os.ErrInvalid)
	}

	progressURL := strings.TrimSpace(response.ProgressURL)
	if progressURL == "" {
		return "", fmt.Errorf("youtube shorts loader missing progress url: %w", os.ErrInvalid)
	}

	return client.pollLoaderDownloadURL(ctx, progressURL)
}

func (client youtubeShortsClient) pollLoaderDownloadURL(
	ctx context.Context,
	progressURL string,
) (string, error) {
	pollInterval := client.loaderPollInterval
	if pollInterval <= 0 {
		pollInterval = youtubeShortsLoaderPollInterval
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		var response aceThinkerYouTubeShortsProgressResponse

		err := client.fetchJSON(ctx, progressURL, &response)
		if err != nil {
			return "", fmt.Errorf("fetch youtube shorts loader progress: %w", err)
		}

		downloadURL := strings.TrimSpace(response.DownloadURL)
		if response.Success == 1 && downloadURL != "" {
			return downloadURL, nil
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for youtube shorts mp4: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (client youtubeShortsClient) fetchJSON(
	ctx context.Context,
	requestURL string,
	target any,
) error {
	responseBody, err := client.doRequest(ctx, requestURL)
	if err != nil {
		return err
	}

	err = json.Unmarshal(responseBody, target)
	if err != nil {
		return fmt.Errorf("decode response for %q: %w", requestURL, err)
	}

	return nil
}

func (client youtubeShortsClient) doRequest(
	ctx context.Context,
	requestURL string,
) ([]byte, error) {
	return doConfiguredGetRequest(
		ctx,
		client.httpClient,
		requestURL,
		client.userAgent,
		"application/json, text/plain, */*",
		"youtube shorts request",
	)
}

func (client youtubeShortsClient) downloadVideo(
	ctx context.Context,
	downloadURL string,
	resolvedURL string,
) ([]byte, string, string, error) {
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		downloadURL,
		nil,
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("create youtube shorts video request: %w", err)
	}

	httpRequest.Header.Set("Accept-Language", youtubeAcceptLanguage)
	httpRequest.Header.Set("User-Agent", client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return nil, "", "", fmt.Errorf("send youtube shorts video request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	videoBytes, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read youtube shorts video response: %w", err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, "", "", fmt.Errorf(
			"youtube shorts video request failed with status %d: %s: %w",
			httpResponse.StatusCode,
			strings.TrimSpace(string(videoBytes)),
			os.ErrInvalid,
		)
	}

	mimeType := normalizedYouTubeShortsMIMEType(httpResponse.Header.Get("Content-Type"))
	filename := youtubeShortsFilename(
		resolvedURL,
		httpResponse.Header.Get("Content-Disposition"),
	)

	return videoBytes, mimeType, filename, nil
}

func normalizedYouTubeShortsMIMEType(contentType string) string {
	trimmedContentType := strings.TrimSpace(contentType)
	if trimmedContentType == "" {
		return youtubeShortsDefaultMIMEType
	}

	mediaType, _, err := mime.ParseMediaType(trimmedContentType)
	if err != nil {
		return youtubeShortsDefaultMIMEType
	}

	if strings.TrimSpace(mediaType) == "" {
		return youtubeShortsDefaultMIMEType
	}

	if strings.EqualFold(mediaType, "application/octet-stream") {
		return youtubeShortsDefaultMIMEType
	}

	return mediaType
}

func youtubeShortsFilename(resolvedURL, contentDisposition string) string {
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

	videoID, _, err := parseYouTubeShortsURL(resolvedURL)
	if err == nil && strings.TrimSpace(videoID) != "" {
		return youtubeShortsFilenamePrefix + videoID + ".mp4"
	}

	return youtubeShortsDefaultFilename
}

func doConfiguredGetRequest(
	ctx context.Context,
	httpClient *http.Client,
	requestURL string,
	userAgent string,
	accept string,
	requestLabel string,
) ([]byte, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create %s %q: %w", requestLabel, requestURL, err)
	}

	httpRequest.Header.Set("Accept", accept)
	httpRequest.Header.Set("Accept-Language", youtubeAcceptLanguage)
	httpRequest.Header.Set("User-Agent", userAgent)

	httpResponse, err := httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("send %s %q: %w", requestLabel, requestURL, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s %q: %w", requestLabel, requestURL, err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf(
			"%s %q failed with status %d: %s: %w",
			requestLabel,
			requestURL,
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	return responseBody, nil
}
