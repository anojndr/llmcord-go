package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultTikTokDownloadURL = "https://snaptik.app/abc2.php"
	defaultTikTokLandingURL  = "https://snaptik.app/en2"
	defaultTikTokRenderURL   = "https://snaptik.app/render.php"
	defaultTikTokTaskURL     = "https://snaptik.app/task.php"
	tikTokDefaultFilename    = "tiktok.mp4"
	tikTokDefaultMIMEType    = "video/mp4"
	tikTokFilenamePrefix     = "tiktok_"
	tikTokLanguage           = "en2"
	tikTokWarningText        = "Warning: TikTok content unavailable"
	oneCaptureMatchCount     = 2
	packedScriptMatchCount   = 5
	twoCaptureMatchCount     = 3
)

var (
	snaptikDownloadURLRegexp = regexp.MustCompile(
		`href=\\"([^"]+)\\" class=\\"button download-file\\"`,
	)
	snaptikErrorRegexp        = regexp.MustCompile(`showAlert\("([^"]+)"`)
	snaptikPackedScriptRegexp = regexp.MustCompile(
		`eval\(function\(h,u,n,t,e,r\)\{[\s\S]*?\}\("((?:\\.|[^"])*)",\d+,"((?:\\.|[^"])*)",(\d+),(\d+),\d+\)\)`,
	)
	snaptikRenderTokenRegexp = regexp.MustCompile(
		`(?:class=\\"button btn-render[^"]*\\"[^>]*data-token=\\"([^"]+)\\"|` +
			`data-token=\\"([^"]+)\\"[^>]*class=\\"button btn-render[^"]*\\")`,
	)
	snaptikTokenRegexp = regexp.MustCompile(`name="token"\s+value="([^"]+)"`)
	tikTokURLRegexp    = regexp.MustCompile(
		`(?i)\b(?:https?://)?(?:[\w-]+\.)?(?:tiktok\.com|tnktok\.com)/[^\s<>()]+`,
	)
	tikTokVideoIDRegexp = regexp.MustCompile(`/video/([0-9]+)`)
)

type tiktokContentClient interface {
	fetch(ctx context.Context, rawURL string) (tiktokVideoContent, error)
}

type tiktokClient struct {
	httpClient  *http.Client
	landingURL  string
	downloadURL string
	renderURL   string
	taskURL     string
	userAgent   string
}

type tiktokRenderResponse struct {
	TaskID string `json:"task_id"`
}

type tiktokTaskResponse struct {
	Status      int    `json:"status"`
	Progress    int    `json:"progress"`
	DownloadURL string `json:"download_url"`
	Message     string `json:"message"`
}

type tiktokVideoContent struct {
	ResolvedURL string
	DownloadURL string
	MediaPart   contentPart
}

func newTikTokClient(httpClient *http.Client) tiktokClient {
	return tiktokClient{
		httpClient:  httpClient,
		landingURL:  defaultTikTokLandingURL,
		downloadURL: defaultTikTokDownloadURL,
		renderURL:   defaultTikTokRenderURL,
		taskURL:     defaultTikTokTaskURL,
		userAgent:   youtubeUserAgent,
	}
}

func (instance *bot) maybeAugmentConversationWithTikTok(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	conversation []chatMessage,
) ([]chatMessage, []string, error) {
	if instance.tiktok == nil {
		return conversation, nil, nil
	}

	latestUserQuery, err := latestUserMessageText(conversation)
	if err != nil {
		return nil, nil, fmt.Errorf("extract latest user message text: %w", err)
	}

	tikTokURLs := extractTikTokURLs(latestUserQuery)
	if len(tikTokURLs) == 0 {
		return conversation, nil, nil
	}

	videoContents, warnings := instance.fetchTikTokVideos(ctx, tikTokURLs)
	if len(videoContents) == 0 {
		return conversation, warnings, nil
	}

	replyModelAPIKind, err := configuredModelAPIKind(loadedConfig, providerSlashModel)
	if err != nil {
		return nil, nil, err
	}

	searchDeciderNeedsAnalysis, err := instance.searchDeciderNeedsTikTokAnalysis(
		loadedConfig,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("check tiktok search decider support: %w", err)
	}

	if replyModelAPIKind == providerAPIKindGemini {
		augmentedConversation, augmentedWarnings, appendErr := appendTikTokVideosToConversation(
			conversation,
			videoContents,
			warnings,
		)
		if appendErr != nil {
			return nil, nil, appendErr
		}

		if !searchDeciderNeedsAnalysis {
			return augmentedConversation, augmentedWarnings, nil
		}

		return instance.appendTikTokVideoAnalysesWithGemini(
			ctx,
			loadedConfig,
			augmentedConversation,
			videoContents,
			augmentedWarnings,
		)
	}

	return instance.preprocessTikTokVideosWithGemini(
		ctx,
		loadedConfig,
		providerSlashModel,
		conversation,
		videoContents,
		warnings,
	)
}

func appendTikTokVideosToConversation(
	conversation []chatMessage,
	videoContents []tiktokVideoContent,
	warnings []string,
) ([]chatMessage, []string, error) {
	mediaParts := make([]contentPart, 0, len(videoContents))
	for _, videoContent := range videoContents {
		mediaParts = append(mediaParts, cloneContentPart(videoContent.MediaPart))
	}

	augmentedConversation, err := appendMediaPartsToConversation(
		conversation,
		mediaParts,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("append tiktok media to conversation: %w", err)
	}

	return augmentedConversation, warnings, nil
}

func (instance *bot) preprocessTikTokVideosWithGemini(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	conversation []chatMessage,
	videoContents []tiktokVideoContent,
	warnings []string,
) ([]chatMessage, []string, error) {
	canUseMediaAnalysis, err := canUseGeminiMediaAnalysis(
		loadedConfig,
		providerSlashModel,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("check tiktok media analysis support: %w", err)
	}

	if !canUseMediaAnalysis {
		return conversation, mergeTikTokWarnings(warnings), nil
	}

	return instance.appendTikTokVideoAnalysesWithGemini(
		ctx,
		loadedConfig,
		conversation,
		videoContents,
		warnings,
	)
}

func (instance *bot) appendTikTokVideoAnalysesWithGemini(
	ctx context.Context,
	loadedConfig config,
	conversation []chatMessage,
	videoContents []tiktokVideoContent,
	warnings []string,
) ([]chatMessage, []string, error) {
	geminiModel, err := configuredGeminiMediaModel(loadedConfig)
	if err != nil {
		return nil, nil, err
	}

	analyses := make([]string, 0, len(videoContents))
	for index, videoContent := range videoContents {
		analysis, analysisErr := instance.analyzeMediaWithGemini(
			ctx,
			loadedConfig,
			geminiModel,
			cloneContentPart(videoContent.MediaPart),
		)
		if analysisErr != nil {
			return nil, nil, fmt.Errorf(
				"analyze tiktok video %d with gemini: %w",
				index+1,
				analysisErr,
			)
		}

		analyses = append(analyses, analysis)
	}

	augmentedConversation, err := appendMediaAnalysesToConversation(
		conversation,
		analyses,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("append tiktok media analyses: %w", err)
	}

	return augmentedConversation, warnings, nil
}

func (instance *bot) searchDeciderNeedsTikTokAnalysis(
	loadedConfig config,
) (bool, error) {
	searchDeciderModel := instance.currentSearchDeciderModelForConfig(loadedConfig)

	apiKind, err := configuredModelAPIKind(loadedConfig, searchDeciderModel)
	if err != nil {
		return false, err
	}

	return apiKind != providerAPIKindGemini, nil
}

func mergeTikTokWarnings(warnings []string) []string {
	warningSet := make(map[string]struct{}, len(warnings)+1)
	for _, warning := range warnings {
		appendUniqueWarning(warningSet, warning)
	}

	appendUniqueWarning(warningSet, tikTokWarningText)

	return sortedWarnings(warningSet)
}

func (instance *bot) fetchTikTokVideos(
	ctx context.Context,
	urls []string,
) ([]tiktokVideoContent, []string) {
	results := make([]tiktokVideoContent, len(urls))
	successful := make([]bool, len(urls))

	var (
		failed    bool
		failedMu  sync.Mutex
		waitGroup sync.WaitGroup
	)

	for index, rawURL := range urls {
		waitGroup.Add(1)

		go func(index int, rawURL string) {
			defer waitGroup.Done()

			videoContent, err := instance.tiktok.fetch(ctx, rawURL)
			if err != nil {
				slog.Warn("fetch tiktok content", "url", rawURL, "error", err)
				failedMu.Lock()
				failed = true
				failedMu.Unlock()

				return
			}

			results[index] = videoContent
			successful[index] = true
		}(index, rawURL)
	}

	waitGroup.Wait()

	videoContents := make([]tiktokVideoContent, 0, len(results))
	seenResolvedURLs := make(map[string]struct{}, len(results))

	for index, result := range results {
		if !successful[index] {
			continue
		}

		resolvedURL := strings.TrimSpace(result.ResolvedURL)
		if resolvedURL == "" {
			resolvedURL = urls[index]
		}

		if _, seen := seenResolvedURLs[resolvedURL]; seen {
			continue
		}

		seenResolvedURLs[resolvedURL] = struct{}{}

		videoContents = append(videoContents, result)
	}

	warnings := make([]string, 0, 1)
	if failed {
		warnings = append(warnings, tikTokWarningText)
	}

	return videoContents, warnings
}

func (client tiktokClient) fetch(
	ctx context.Context,
	rawURL string,
) (tiktokVideoContent, error) {
	requestContext, cancel := context.WithTimeout(ctx, tikTokRequestTimeout)
	defer cancel()

	resolvedURL, err := client.resolveURL(requestContext, rawURL)
	if err != nil {
		return tiktokVideoContent{}, fmt.Errorf("resolve tiktok url %q: %w", rawURL, err)
	}

	token, err := client.fetchToken(requestContext)
	if err != nil {
		return tiktokVideoContent{}, fmt.Errorf("fetch snaptik token: %w", err)
	}

	responseScript, err := client.fetchDownloadScript(requestContext, resolvedURL, token)
	if err != nil {
		return tiktokVideoContent{}, fmt.Errorf("fetch snaptik download script: %w", err)
	}

	decodedScript, err := decodeSnaptikPackedScript(responseScript)
	if err != nil {
		return tiktokVideoContent{}, fmt.Errorf("decode snaptik response: %w", err)
	}

	downloadURL, err := client.extractDownloadURL(requestContext, decodedScript)
	if err != nil {
		return tiktokVideoContent{}, fmt.Errorf("extract snaptik download url: %w", err)
	}

	videoBytes, mimeType, filename, err := client.downloadVideo(requestContext, downloadURL, resolvedURL)
	if err != nil {
		return tiktokVideoContent{}, fmt.Errorf("download tiktok video: %w", err)
	}

	return tiktokVideoContent{
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

func extractTikTokURLs(text string) []string {
	matches := tikTokURLRegexp.FindAllString(text, -1)
	normalizedURLs := make([]string, 0, len(matches))
	seenURLs := make(map[string]struct{}, len(matches))

	for _, match := range matches {
		normalizedURL, err := normalizeTikTokURL(match)
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

func normalizeTikTokURL(rawURL string) (string, error) {
	cleanedURL := strings.TrimSpace(rawURL)
	cleanedURL = strings.Trim(cleanedURL, `"'<>[]()`)
	cleanedURL = strings.TrimRight(cleanedURL, ".,!?;:")

	if cleanedURL == "" {
		return "", fmt.Errorf("empty tiktok url: %w", os.ErrInvalid)
	}

	if !strings.Contains(cleanedURL, "://") {
		cleanedURL = "https://" + cleanedURL
	}

	parsedURL, err := url.Parse(cleanedURL)
	if err != nil {
		return "", fmt.Errorf("parse tiktok url %q: %w", rawURL, err)
	}

	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return "", fmt.Errorf("parse tiktok url %q: %w", rawURL, os.ErrInvalid)
	}

	return parsedURL.String(), nil
}

func (client tiktokClient) resolveURL(
	ctx context.Context,
	rawURL string,
) (string, error) {
	normalizedURL, err := normalizeTikTokURL(rawURL)
	if err != nil {
		return "", err
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		normalizedURL,
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("create tiktok resolve request: %w", err)
	}

	httpRequest.Header.Set("User-Agent", client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("send tiktok resolve request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf(
			"tiktok resolve request failed with status %d: %w",
			httpResponse.StatusCode,
			os.ErrInvalid,
		)
	}

	return httpResponse.Request.URL.String(), nil
}

func (client tiktokClient) fetchToken(ctx context.Context) (string, error) {
	htmlText, err := client.fetchText(ctx, client.landingURL)
	if err != nil {
		return "", err
	}

	match := snaptikTokenRegexp.FindStringSubmatch(htmlText)
	if len(match) != oneCaptureMatchCount {
		return "", fmt.Errorf("extract snaptik token: %w", os.ErrInvalid)
	}

	return strings.TrimSpace(match[1]), nil
}

func (client tiktokClient) fetchDownloadScript(
	ctx context.Context,
	resolvedURL string,
	token string,
) (string, error) {
	formValues := url.Values{
		"url":   {resolvedURL},
		"lang":  {tikTokLanguage},
		"token": {token},
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.downloadURL,
		strings.NewReader(formValues.Encode()),
	)
	if err != nil {
		return "", fmt.Errorf("create snaptik download request: %w", err)
	}

	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpRequest.Header.Set("User-Agent", client.userAgent)
	httpRequest.Header.Set("X-Requested-With", "XMLHttpRequest")

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("send snaptik download request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return "", fmt.Errorf("read snaptik download response: %w", err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf(
			"snaptik download request failed with status %d: %s: %w",
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	return string(responseBody), nil
}

func decodeSnaptikPackedScript(responseScript string) (string, error) {
	matches := snaptikPackedScriptRegexp.FindStringSubmatch(responseScript)
	if len(matches) != packedScriptMatchCount {
		return "", fmt.Errorf("parse snaptik packed script: %w", os.ErrInvalid)
	}

	payload, err := strconv.Unquote(`"` + matches[1] + `"`)
	if err != nil {
		return "", fmt.Errorf("unquote snaptik payload: %w", err)
	}

	alphabet, err := strconv.Unquote(`"` + matches[2] + `"`)
	if err != nil {
		return "", fmt.Errorf("unquote snaptik alphabet: %w", err)
	}

	offset, err := strconv.Atoi(matches[3])
	if err != nil {
		return "", fmt.Errorf("parse snaptik offset %q: %w", matches[3], err)
	}

	base, err := strconv.Atoi(matches[4])
	if err != nil {
		return "", fmt.Errorf("parse snaptik base %q: %w", matches[4], err)
	}

	if base <= 0 || base >= len(alphabet) {
		return "", fmt.Errorf("invalid snaptik base %d: %w", base, os.ErrInvalid)
	}

	separator := alphabet[base]
	chunks := strings.Split(payload, string(separator))

	var builder strings.Builder

	for _, chunk := range chunks {
		if chunk == "" {
			continue
		}

		value, err := decodeSnaptikChunk(chunk, alphabet[:base], base)
		if err != nil {
			return "", fmt.Errorf("decode snaptik chunk %q: %w", chunk, err)
		}

		decodedValue := value - offset
		if decodedValue < 0 || decodedValue > utf8.MaxRune {
			return "", fmt.Errorf("decode snaptik rune %d: %w", decodedValue, os.ErrInvalid)
		}

		builder.WriteRune(rune(decodedValue))
	}

	return builder.String(), nil
}

func decodeSnaptikChunk(
	chunk string,
	alphabet string,
	base int,
) (int, error) {
	value := 0
	multiplier := 1

	for index := len(chunk) - 1; index >= 0; index-- {
		digit := strings.IndexByte(alphabet, chunk[index])
		if digit == -1 {
			return 0, fmt.Errorf("invalid digit %q: %w", string(chunk[index]), os.ErrInvalid)
		}

		value += digit * multiplier
		multiplier *= base
	}

	return value, nil
}

func (client tiktokClient) extractDownloadURL(
	ctx context.Context,
	decodedScript string,
) (string, error) {
	match := snaptikDownloadURLRegexp.FindStringSubmatch(decodedScript)
	if len(match) == oneCaptureMatchCount {
		return strings.TrimSpace(match[1]), nil
	}

	renderToken := snaptikRenderToken(decodedScript)
	if renderToken != "" {
		return client.renderDownloadURL(ctx, renderToken)
	}

	errorMatch := snaptikErrorRegexp.FindStringSubmatch(decodedScript)
	if len(errorMatch) == oneCaptureMatchCount {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(errorMatch[1]), os.ErrInvalid)
	}

	return "", fmt.Errorf("find snaptik download url: %w", os.ErrInvalid)
}

func snaptikRenderToken(decodedScript string) string {
	match := snaptikRenderTokenRegexp.FindStringSubmatch(decodedScript)
	if len(match) != twoCaptureMatchCount {
		return ""
	}

	if strings.TrimSpace(match[1]) != "" {
		return strings.TrimSpace(match[1])
	}

	return strings.TrimSpace(match[2])
}

func (client tiktokClient) renderDownloadURL(
	ctx context.Context,
	renderToken string,
) (string, error) {
	renderRequestURL, err := urlWithToken(client.renderURL, renderToken)
	if err != nil {
		return "", fmt.Errorf("build snaptik render url: %w", err)
	}

	responseBody, err := client.fetchText(ctx, renderRequestURL)
	if err != nil {
		return "", fmt.Errorf("fetch snaptik render task: %w", err)
	}

	var renderResponse tiktokRenderResponse

	err = json.Unmarshal([]byte(responseBody), &renderResponse)
	if err != nil {
		return "", fmt.Errorf("parse snaptik render response: %w", err)
	}

	if strings.TrimSpace(renderResponse.TaskID) == "" {
		return "", fmt.Errorf("missing snaptik render task id: %w", os.ErrInvalid)
	}

	waitContext, cancel := context.WithTimeout(ctx, tikTokRenderTimeout)
	defer cancel()

	ticker := time.NewTicker(tikTokRenderPollInterval)
	defer ticker.Stop()

	for {
		downloadURL, done, err := client.pollRenderTask(waitContext, renderResponse.TaskID)
		if err != nil {
			return "", err
		}

		if done {
			return downloadURL, nil
		}

		select {
		case <-waitContext.Done():
			return "", fmt.Errorf("wait for snaptik render task: %w", waitContext.Err())
		case <-ticker.C:
		}
	}
}

func (client tiktokClient) pollRenderTask(
	ctx context.Context,
	taskID string,
) (string, bool, error) {
	taskRequestURL, err := urlWithToken(client.taskURL, taskID)
	if err != nil {
		return "", false, fmt.Errorf("build snaptik task url: %w", err)
	}

	responseBody, err := client.fetchText(ctx, taskRequestURL)
	if err != nil {
		return "", false, fmt.Errorf("fetch snaptik task status: %w", err)
	}

	var taskResponse tiktokTaskResponse

	err = json.Unmarshal([]byte(responseBody), &taskResponse)
	if err != nil {
		return "", false, fmt.Errorf("parse snaptik task response: %w", err)
	}

	if taskResponse.Status != 0 {
		message := strings.TrimSpace(taskResponse.Message)
		if message == "" {
			message = "render failed"
		}

		return "", false, fmt.Errorf("snaptik render failed: %s: %w", message, os.ErrInvalid)
	}

	if taskResponse.Progress < 100 || strings.TrimSpace(taskResponse.DownloadURL) == "" {
		return "", false, nil
	}

	return strings.TrimSpace(taskResponse.DownloadURL), true, nil
}

func urlWithToken(baseURL string, token string) (string, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", baseURL, err)
	}

	queryValues := parsedURL.Query()
	queryValues.Set("token", token)
	parsedURL.RawQuery = queryValues.Encode()

	return parsedURL.String(), nil
}

func (client tiktokClient) downloadVideo(
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
		return nil, "", "", fmt.Errorf("create tiktok video request: %w", err)
	}

	httpRequest.Header.Set("User-Agent", client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return nil, "", "", fmt.Errorf("send tiktok video request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	videoBytes, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read tiktok video response: %w", err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, "", "", fmt.Errorf(
			"tiktok video request failed with status %d: %s: %w",
			httpResponse.StatusCode,
			strings.TrimSpace(string(videoBytes)),
			os.ErrInvalid,
		)
	}

	mimeType := normalizedTikTokMIMEType(httpResponse.Header.Get("Content-Type"))
	filename := tikTokFilename(
		resolvedURL,
		httpResponse.Header.Get("Content-Disposition"),
	)

	return videoBytes, mimeType, filename, nil
}

func normalizedTikTokMIMEType(contentType string) string {
	trimmedContentType := strings.TrimSpace(contentType)
	if trimmedContentType == "" {
		return tikTokDefaultMIMEType
	}

	mediaType, _, err := mime.ParseMediaType(trimmedContentType)
	if err != nil {
		return tikTokDefaultMIMEType
	}

	if strings.TrimSpace(mediaType) == "" {
		return tikTokDefaultMIMEType
	}

	if strings.EqualFold(mediaType, "application/octet-stream") {
		return tikTokDefaultMIMEType
	}

	return mediaType
}

func tikTokFilename(resolvedURL string, contentDisposition string) string {
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

	match := tikTokVideoIDRegexp.FindStringSubmatch(resolvedURL)
	if len(match) == oneCaptureMatchCount {
		return tikTokFilenamePrefix + match[1] + ".mp4"
	}

	return tikTokDefaultFilename
}

func (client tiktokClient) fetchText(
	ctx context.Context,
	requestURL string,
) (string, error) {
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		requestURL,
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("create request for %q: %w", requestURL, err)
	}

	httpRequest.Header.Set("User-Agent", client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("send request for %q: %w", requestURL, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return "", fmt.Errorf("read response for %q: %w", requestURL, err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf(
			"request for %q failed with status %d: %s: %w",
			requestURL,
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	return string(responseBody), nil
}

func encodeSnaptikPackedScriptForTest(decodedScript string) string {
	const (
		offset   = 11
		base     = 6
		alphabet = "abcdefghi"
	)

	separator := alphabet[base]

	var payload bytes.Buffer

	for index, currentRune := range decodedScript {
		if index > 0 {
			payload.WriteByte(separator)
		}

		encodedValue := strconv.FormatInt(int64(currentRune+offset), base)
		for _, digit := range encodedValue {
			payload.WriteByte(alphabet[digit-'0'])
		}
	}

	return fmt.Sprintf(
		`var packed = true;eval(function(h,u,n,t,e,r){return h}("%s",76,"%s",%d,%d,60))`,
		payload.String(),
		alphabet,
		offset,
		base,
	)
}
