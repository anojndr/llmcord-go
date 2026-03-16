package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	defaultYouTubeWatchURL         = "https://www.youtube.com/watch"
	defaultYouTubeAPIBaseURL       = "https://www.youtube.com/youtubei/v1"
	defaultNoteGPTAPIBaseURL       = "https://notegpt.io/api/v2"
	youtubeWarningText             = "Warning: YouTube content unavailable"
	noteGPTSuccessCode             = 100000
	noteGPTPlatformYouTube         = "youtube"
	noteGPTMediaStatusProcessing   = "processing"
	noteGPTMediaStatusSuccess      = "success"
	noteGPTMediaStatusNotFound     = "not_found"
	noteGPTAnonymousUserCookieName = "anonymous_user_id"
	noteGPTUUIDVersionMask         = 0x0f
	noteGPTUUIDVersionValue        = 0x40
	noteGPTUUIDVariantMask         = 0x3f
	noteGPTUUIDVariantValue        = 0x80
	youtubeWebClientName           = "WEB"
	youtubeWebClientVersionHeader  = "1"
	youtubeAcceptLanguage          = "en-US,en;q=0.9"
	youtubeUserAgent               = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"
	youtubeCommentsPanelIdentifier = "engagement-panel-comments-section"
	youtubeMediaStatusPollInterval = 500 * time.Millisecond
)

var (
	youtubeURLRegexp = regexp.MustCompile(
		`(?i)\b(?:https?://)?(?:[\w-]+\.)?(?:youtube\.com|youtube-nocookie\.com|youtu\.be)/[^\s<>()]+`,
	)
	youtubeVideoIDRegexp       = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	youtubeAPIKeyRegexp        = regexp.MustCompile(`"INNERTUBE_API_KEY"\s*:\s*"([^"]+)"`)
	youtubeClientVersionRegexp = regexp.MustCompile(`"INNERTUBE_CLIENT_VERSION"\s*:\s*"([^"]+)"`)
	youtubeConsentValueRegexp  = regexp.MustCompile(`name="v"\s+value="([^"]+)"`)
)

type youtubeContentClient interface {
	fetch(ctx context.Context, rawURL string) (youtubeVideoContent, error)
}

type youtubeVideoContent struct {
	URL         string
	VideoID     string
	Title       string
	ChannelName string
	Transcript  string
	Comments    []youtubeComment
}

type youtubeComment struct {
	Author string
	Text   string
}

type youtubeClient struct {
	httpClient        *http.Client
	watchURL          string
	apiBaseURL        string
	noteGPTAPIBaseURL string
	userAgent         string
}

type youtubeWatchPage struct {
	APIKey        string
	ClientVersion string
	CommentsToken string
}

type noteGPTVideoTranscriptResponse struct {
	Code    int                        `json:"code"`
	Message string                     `json:"message"`
	Data    noteGPTVideoTranscriptData `json:"data"`
}

type noteGPTVideoTranscriptData struct {
	VideoID      string                             `json:"video_id"`
	VideoInfo    noteGPTVideoInfo                   `json:"video_info"`
	LanguageCode []noteGPTLanguageCode              `json:"language_code"`
	Transcripts  map[string]noteGPTTranscriptTracks `json:"transcripts"`
	Duration     string                             `json:"duration"`
}

type noteGPTVideoInfo struct {
	Name   string `json:"name"`
	Author string `json:"author"`
}

type noteGPTLanguageCode struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type noteGPTTranscriptTracks struct {
	Custom  []noteGPTTranscriptSegment `json:"custom"`
	Default []noteGPTTranscriptSegment `json:"default"`
	Auto    []noteGPTTranscriptSegment `json:"auto"`
}

type noteGPTTranscriptSegment struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Text  string `json:"text"`
}

type noteGPTStatusResponse struct {
	Code    int               `json:"code"`
	Message string            `json:"message"`
	Data    noteGPTStatusData `json:"data"`
}

type noteGPTStatusData struct {
	Status string `json:"status"`
}

func newYouTubeClient(httpClient *http.Client) youtubeClient {
	return youtubeClient{
		httpClient:        httpClient,
		watchURL:          defaultYouTubeWatchURL,
		apiBaseURL:        defaultYouTubeAPIBaseURL,
		noteGPTAPIBaseURL: defaultNoteGPTAPIBaseURL,
		userAgent:         youtubeUserAgent,
	}
}

func (instance *bot) maybeAugmentConversationWithYouTube(
	ctx context.Context,
	conversation []chatMessage,
	urlExtractionText string,
) ([]chatMessage, []string, error) {
	if instance.youtube == nil {
		return conversation, nil, nil
	}

	youtubeURLs := extractYouTubeURLs(urlExtractionText)
	if len(youtubeURLs) == 0 {
		return conversation, nil, nil
	}

	augmentedConversation, _, warnings, err := augmentConversationWithConcurrentURLContent(
		ctx,
		conversation,
		youtubeURLs,
		instance.youtube.fetch,
		"fetch youtube content",
		youtubeWarningText,
		formatYouTubeURLContent,
		appendYouTubeContentToConversation,
		"append youtube content to conversation",
	)

	return augmentedConversation, warnings, err
}

func (client youtubeClient) fetch(ctx context.Context, rawURL string) (youtubeVideoContent, error) {
	videoID, canonicalURL, err := parseYouTubeVideoURL(rawURL)
	if err != nil {
		return youtubeVideoContent{}, err
	}

	requestContext, cancel := context.WithTimeout(ctx, youtubeRequestTimeout)
	defer cancel()

	content, err := client.fetchNoteGPTContent(requestContext, videoID)
	if err != nil {
		return youtubeVideoContent{}, fmt.Errorf("fetch transcript for %q via notegpt: %w", rawURL, err)
	}

	content.URL = canonicalURL

	watchPage, err := client.fetchWatchPage(requestContext, videoID)
	if err != nil {
		slog.Warn("fetch youtube watch page", "url", rawURL, "error", err)
	} else {
		comments, commentsErr := client.fetchComments(
			requestContext,
			watchPage.APIKey,
			watchPage.ClientVersion,
			watchPage.CommentsToken,
		)
		if commentsErr != nil {
			slog.Warn("fetch youtube comments", "url", rawURL, "error", commentsErr)
		} else {
			content.Comments = comments
		}
	}

	return content, nil
}

func (client youtubeClient) fetchNoteGPTContent(
	ctx context.Context,
	videoID string,
) (youtubeVideoContent, error) {
	anonymousUserID, err := newNoteGPTAnonymousUserID()
	if err != nil {
		return youtubeVideoContent{}, err
	}

	transcriptData, err := client.fetchNoteGPTVideoTranscript(ctx, videoID, anonymousUserID)
	if err != nil {
		return youtubeVideoContent{}, err
	}

	title := strings.TrimSpace(transcriptData.VideoInfo.Name)
	if title == "" {
		return youtubeVideoContent{}, fmt.Errorf("extract title for %q: %w", videoID, os.ErrInvalid)
	}

	channelName := strings.TrimSpace(transcriptData.VideoInfo.Author)
	if channelName == "" {
		channelName = unknownText
	}

	transcript, err := formatNoteGPTTranscript(transcriptData)
	if err != nil {
		return youtubeVideoContent{}, fmt.Errorf("format transcript for %q: %w", videoID, err)
	}

	return youtubeVideoContent{
		URL:         "",
		VideoID:     videoID,
		Title:       title,
		ChannelName: channelName,
		Transcript:  transcript,
		Comments:    nil,
	}, nil
}

func (client youtubeClient) fetchNoteGPTVideoTranscript(
	ctx context.Context,
	videoID string,
	anonymousUserID string,
) (noteGPTVideoTranscriptData, error) {
	transcriptData, ready, err := client.fetchNoteGPTVideoTranscriptOnce(ctx, videoID, anonymousUserID)
	if err != nil {
		return noteGPTVideoTranscriptData{}, err
	}

	if ready {
		return transcriptData, nil
	}

	err = client.startNoteGPTTranscriptGeneration(ctx, videoID, anonymousUserID)
	if err != nil {
		return noteGPTVideoTranscriptData{}, err
	}

	err = client.waitForNoteGPTTranscriptGeneration(ctx, videoID, anonymousUserID)
	if err != nil {
		return noteGPTVideoTranscriptData{}, err
	}

	transcriptData, ready, err = client.fetchNoteGPTVideoTranscriptOnce(ctx, videoID, anonymousUserID)
	if err != nil {
		return noteGPTVideoTranscriptData{}, err
	}

	if !ready {
		return noteGPTVideoTranscriptData{}, fmt.Errorf(
			"transcript unavailable after generation for %q: %w",
			videoID,
			os.ErrNotExist,
		)
	}

	return transcriptData, nil
}

func (client youtubeClient) fetchNoteGPTVideoTranscriptOnce(
	ctx context.Context,
	videoID string,
	anonymousUserID string,
) (noteGPTVideoTranscriptData, bool, error) {
	requestURL, err := client.buildNoteGPTURL(
		"video-transcript",
		map[string]string{
			"platform": noteGPTPlatformYouTube,
			"video_id": videoID,
		},
	)
	if err != nil {
		return noteGPTVideoTranscriptData{}, false, err
	}

	responseBody, err := client.doRequest(
		ctx,
		http.MethodGet,
		requestURL,
		nil,
		map[string]string{
			"Cookie": client.noteGPTAnonymousUserCookie(anonymousUserID),
		},
	)
	if err != nil {
		return noteGPTVideoTranscriptData{}, false, err
	}

	var payload noteGPTVideoTranscriptResponse

	err = json.Unmarshal(responseBody, &payload)
	if err != nil {
		return noteGPTVideoTranscriptData{}, false, fmt.Errorf("decode notegpt video transcript response: %w", err)
	}

	if payload.Code != noteGPTSuccessCode {
		return noteGPTVideoTranscriptData{}, false, fmt.Errorf(
			"notegpt video transcript code %d: %s: %w",
			payload.Code,
			strings.TrimSpace(payload.Message),
			os.ErrInvalid,
		)
	}

	return payload.Data, len(selectNoteGPTTranscriptSegments(payload.Data)) > 0, nil
}

func (client youtubeClient) startNoteGPTTranscriptGeneration(
	ctx context.Context,
	videoID string,
	anonymousUserID string,
) error {
	requestURL, err := client.buildNoteGPTURL(
		"transcript-generate",
		map[string]string{
			"platform": noteGPTPlatformYouTube,
			"video_id": videoID,
		},
	)
	if err != nil {
		return err
	}

	responseBody, err := client.doRequest(
		ctx,
		http.MethodGet,
		requestURL,
		nil,
		map[string]string{
			"Cookie": client.noteGPTAnonymousUserCookie(anonymousUserID),
		},
	)
	if err != nil {
		return err
	}

	var payload noteGPTStatusResponse

	err = json.Unmarshal(responseBody, &payload)
	if err != nil {
		return fmt.Errorf("decode notegpt transcript generate response: %w", err)
	}

	if payload.Code != noteGPTSuccessCode {
		return fmt.Errorf(
			"notegpt transcript generate code %d: %s: %w",
			payload.Code,
			strings.TrimSpace(payload.Message),
			os.ErrInvalid,
		)
	}

	return nil
}

func (client youtubeClient) waitForNoteGPTTranscriptGeneration(
	ctx context.Context,
	videoID string,
	anonymousUserID string,
) error {
	ticker := time.NewTicker(youtubeMediaStatusPollInterval)
	defer ticker.Stop()

	for {
		status, err := client.fetchNoteGPTMediaStatus(ctx, videoID, anonymousUserID)
		if err != nil {
			return err
		}

		switch strings.TrimSpace(strings.ToLower(status)) {
		case noteGPTMediaStatusSuccess:
			return nil
		case "", noteGPTMediaStatusNotFound, noteGPTMediaStatusProcessing:
		default:
			return fmt.Errorf("notegpt media status for %q: %s: %w", videoID, status, os.ErrInvalid)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for notegpt transcript generation for %q: %w", videoID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (client youtubeClient) fetchNoteGPTMediaStatus(
	ctx context.Context,
	videoID string,
	anonymousUserID string,
) (string, error) {
	requestURL, err := client.buildNoteGPTURL(
		"media-status",
		map[string]string{
			"platform": noteGPTPlatformYouTube,
			"video_id": videoID,
		},
	)
	if err != nil {
		return "", err
	}

	responseBody, err := client.doRequest(
		ctx,
		http.MethodGet,
		requestURL,
		nil,
		map[string]string{
			"Cookie": client.noteGPTAnonymousUserCookie(anonymousUserID),
		},
	)
	if err != nil {
		return "", err
	}

	var payload noteGPTStatusResponse

	err = json.Unmarshal(responseBody, &payload)
	if err != nil {
		return "", fmt.Errorf("decode notegpt media status response: %w", err)
	}

	if payload.Code != noteGPTSuccessCode {
		return "", fmt.Errorf(
			"notegpt media status code %d: %s: %w",
			payload.Code,
			strings.TrimSpace(payload.Message),
			os.ErrInvalid,
		)
	}

	return payload.Data.Status, nil
}

func (client youtubeClient) buildNoteGPTURL(
	endpoint string,
	queryParameters map[string]string,
) (string, error) {
	requestURL, err := url.Parse(client.noteGPTAPIBaseURL)
	if err != nil {
		return "", fmt.Errorf("parse notegpt api base url %q: %w", client.noteGPTAPIBaseURL, err)
	}

	requestURL.Path = strings.TrimRight(requestURL.Path, "/") + "/" + strings.TrimLeft(endpoint, "/")

	queryValues := requestURL.Query()
	for key, value := range queryParameters {
		queryValues.Set(key, value)
	}

	requestURL.RawQuery = queryValues.Encode()

	return requestURL.String(), nil
}

func (client youtubeClient) noteGPTAnonymousUserCookie(anonymousUserID string) string {
	return noteGPTAnonymousUserCookieName + "=" + anonymousUserID
}

func newNoteGPTAnonymousUserID() (string, error) {
	var randomBytes [16]byte

	_, err := rand.Read(randomBytes[:])
	if err != nil {
		return "", fmt.Errorf("generate notegpt anonymous user id: %w", err)
	}

	randomBytes[6] = (randomBytes[6] & noteGPTUUIDVersionMask) | noteGPTUUIDVersionValue
	randomBytes[8] = (randomBytes[8] & noteGPTUUIDVariantMask) | noteGPTUUIDVariantValue

	return fmt.Sprintf(
		"%x-%x-%x-%x-%x",
		randomBytes[0:4],
		randomBytes[4:6],
		randomBytes[6:8],
		randomBytes[8:10],
		randomBytes[10:16],
	), nil
}

func (transcriptData *noteGPTVideoTranscriptData) UnmarshalJSON(rawValue []byte) error {
	type noteGPTVideoTranscriptDataAlias noteGPTVideoTranscriptData

	var alias noteGPTVideoTranscriptDataAlias

	err := json.Unmarshal(rawValue, &alias)
	if err != nil {
		return fmt.Errorf("decode notegpt transcript data: %w", err)
	}

	*transcriptData = noteGPTVideoTranscriptData(alias)

	var payload map[string]json.RawMessage

	err = json.Unmarshal(rawValue, &payload)
	if err != nil {
		return fmt.Errorf("decode notegpt transcript payload: %w", err)
	}

	if videoID, ok := payload["videoId"]; ok {
		err = json.Unmarshal(videoID, &transcriptData.VideoID)
		if err != nil {
			return fmt.Errorf("decode notegpt video id: %w", err)
		}
	}

	if videoInfo, ok := payload["videoInfo"]; ok {
		err = json.Unmarshal(videoInfo, &transcriptData.VideoInfo)
		if err != nil {
			return fmt.Errorf("decode notegpt video info: %w", err)
		}
	}

	return nil
}

func (client youtubeClient) fetchWatchPage(
	ctx context.Context,
	videoID string,
) (youtubeWatchPage, error) {
	htmlText, err := client.fetchWatchHTML(ctx, videoID, "")
	if err != nil {
		return youtubeWatchPage{}, err
	}

	if strings.Contains(htmlText, `action="https://consent.youtube.com/s"`) {
		consentValue, consentErr := extractYouTubeConsentValue(htmlText)
		if consentErr != nil {
			return youtubeWatchPage{}, consentErr
		}

		htmlText, err = client.fetchWatchHTML(ctx, videoID, "CONSENT=YES+"+consentValue)
		if err != nil {
			return youtubeWatchPage{}, err
		}
	}

	apiKey, err := extractYouTubeAPIKey(htmlText)
	if err != nil {
		return youtubeWatchPage{}, err
	}

	clientVersion, err := extractYouTubeClientVersion(htmlText)
	if err != nil {
		return youtubeWatchPage{}, err
	}

	initialData, err := extractYouTubeInitialData(htmlText)
	if err != nil {
		return youtubeWatchPage{}, err
	}

	return youtubeWatchPage{
		APIKey:        apiKey,
		ClientVersion: clientVersion,
		CommentsToken: extractYouTubeCommentsContinuationToken(initialData),
	}, nil
}

func (client youtubeClient) fetchWatchHTML(
	ctx context.Context,
	videoID string,
	cookie string,
) (string, error) {
	requestURL, err := url.Parse(client.watchURL)
	if err != nil {
		return "", fmt.Errorf("parse youtube watch url %q: %w", client.watchURL, err)
	}

	queryValues := requestURL.Query()
	queryValues.Set("v", videoID)
	queryValues.Set("hl", "en")
	requestURL.RawQuery = queryValues.Encode()

	headers := map[string]string{}
	if cookie != "" {
		headers["Cookie"] = cookie
	}

	responseBody, err := client.doRequest(ctx, http.MethodGet, requestURL.String(), nil, headers)
	if err != nil {
		return "", err
	}

	return html.UnescapeString(string(responseBody)), nil
}

func (client youtubeClient) fetchComments(
	ctx context.Context,
	apiKey string,
	clientVersion string,
	continuationToken string,
) ([]youtubeComment, error) {
	if continuationToken == "" || apiKey == "" || clientVersion == "" {
		return nil, nil
	}

	comments := make([]youtubeComment, 0, maxYouTubeComments)
	nextToken := continuationToken

	for len(comments) < maxYouTubeComments && nextToken != "" {
		pageComments, pageToken, err := client.fetchCommentsPage(
			ctx,
			apiKey,
			clientVersion,
			nextToken,
		)
		if err != nil {
			return comments, err
		}

		comments = append(comments, pageComments...)
		nextToken = pageToken
	}

	if len(comments) > maxYouTubeComments {
		comments = comments[:maxYouTubeComments]
	}

	return comments, nil
}

func (client youtubeClient) fetchCommentsPage(
	ctx context.Context,
	apiKey string,
	clientVersion string,
	continuationToken string,
) ([]youtubeComment, string, error) {
	requestURL := client.buildAPIURL("next", apiKey) + "&prettyPrint=false"
	requestBody := map[string]any{
		"context": map[string]any{
			"client": map[string]string{
				"clientName":    youtubeWebClientName,
				"clientVersion": clientVersion,
				"hl":            "en",
				"gl":            "US",
			},
		},
		"continuation": continuationToken,
	}

	headers := map[string]string{
		"X-YouTube-Client-Name":    youtubeWebClientVersionHeader,
		"X-YouTube-Client-Version": clientVersion,
	}

	responseBody, err := client.doJSONRequest(ctx, requestURL, requestBody, headers)
	if err != nil {
		return nil, "", err
	}

	comments, nextToken, err := parseYouTubeCommentsResponse(responseBody)
	if err != nil {
		return nil, "", err
	}

	return comments, nextToken, nil
}

func (client youtubeClient) buildAPIURL(endpoint string, apiKey string) string {
	return fmt.Sprintf("%s/%s?key=%s", client.apiBaseURL, endpoint, url.QueryEscape(apiKey))
}

func (client youtubeClient) doJSONRequest(
	ctx context.Context,
	requestURL string,
	requestBody map[string]any,
	headers map[string]string,
) ([]byte, error) {
	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("marshal youtube request body: %w", err)
	}

	requestHeaders := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
	}

	maps.Copy(requestHeaders, headers)

	return client.doRequest(ctx, http.MethodPost, requestURL, requestBytes, requestHeaders)
}

func (client youtubeClient) doRequest(
	ctx context.Context,
	method string,
	requestURL string,
	requestBody []byte,
	headers map[string]string,
) ([]byte, error) {
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		method,
		requestURL,
		bytes.NewReader(requestBody),
	)
	if err != nil {
		return nil, fmt.Errorf("create youtube request %s %q: %w", method, requestURL, err)
	}

	httpRequest.Header.Set("Accept-Language", youtubeAcceptLanguage)
	httpRequest.Header.Set("User-Agent", client.userAgent)

	for key, value := range headers {
		httpRequest.Header.Set(key, value)
	}

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("send youtube request %s %q: %w", method, requestURL, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, fmt.Errorf("read youtube response %s %q: %w", method, requestURL, err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf(
			"youtube request %s %q failed with status %d: %s: %w",
			method,
			requestURL,
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	return responseBody, nil
}

func latestUserMessageText(conversation []chatMessage) (string, error) {
	for index := len(conversation) - 1; index >= 0; index-- {
		if conversation[index].Role != messageRoleUser {
			continue
		}

		return messageContentText(conversation[index].Content), nil
	}

	return "", fmt.Errorf("find latest user message: %w", os.ErrNotExist)
}

func messageContentText(content any) string {
	switch typedContent := content.(type) {
	case nil:
		return ""
	case string:
		return typedContent
	case []contentPart:
		return contentPartsText(typedContent)
	default:
		return ""
	}
}

func extractYouTubeURLs(text string) []string {
	text = normalizedURLExtractionText(text)

	matches := youtubeURLRegexp.FindAllString(text, -1)
	normalizedURLs := make([]string, 0, len(matches))
	seenVideoIDs := make(map[string]struct{}, len(matches))

	for _, match := range matches {
		videoID, canonicalURL, err := parseYouTubeVideoURL(match)
		if err != nil {
			continue
		}

		if _, ok := seenVideoIDs[videoID]; ok {
			continue
		}

		seenVideoIDs[videoID] = struct{}{}

		normalizedURLs = append(normalizedURLs, canonicalURL)
	}

	return normalizedURLs
}

func parseYouTubeVideoURL(rawURL string) (string, string, error) {
	cleanedURL := strings.TrimSpace(rawURL)
	cleanedURL = strings.Trim(cleanedURL, `"'<>[]()`)

	cleanedURL = strings.TrimRight(cleanedURL, ".,!?;:")
	if cleanedURL == "" {
		return "", "", fmt.Errorf("empty youtube url: %w", os.ErrInvalid)
	}

	if !strings.Contains(cleanedURL, "://") {
		cleanedURL = "https://" + cleanedURL
	}

	parsedURL, err := url.Parse(cleanedURL)
	if err != nil {
		return "", "", fmt.Errorf("parse youtube url %q: %w", rawURL, err)
	}

	host := strings.ToLower(parsedURL.Hostname())
	host = strings.TrimPrefix(host, "www.")

	videoID := ""

	switch {
	case host == "youtu.be":
		videoID = firstPathSegment(parsedURL.Path)
	case strings.HasSuffix(host, "youtube.com") || strings.HasSuffix(host, "youtube-nocookie.com"):
		switch {
		case parsedURL.Path == "/watch":
			videoID = parsedURL.Query().Get("v")
		case strings.HasPrefix(parsedURL.Path, "/shorts/"):
			videoID = firstPathSegment(strings.TrimPrefix(parsedURL.Path, "/shorts/"))
		case strings.HasPrefix(parsedURL.Path, "/embed/"):
			videoID = firstPathSegment(strings.TrimPrefix(parsedURL.Path, "/embed/"))
		case strings.HasPrefix(parsedURL.Path, "/live/"):
			videoID = firstPathSegment(strings.TrimPrefix(parsedURL.Path, "/live/"))
		}
	}

	if !youtubeVideoIDRegexp.MatchString(videoID) {
		return "", "", fmt.Errorf("extract youtube video id from %q: %w", rawURL, os.ErrInvalid)
	}

	return videoID, canonicalYouTubeURL(videoID), nil
}

func firstPathSegment(path string) string {
	trimmedPath := strings.Trim(path, "/")
	if trimmedPath == "" {
		return ""
	}

	segment, _, _ := strings.Cut(trimmedPath, "/")

	return segment
}

func canonicalYouTubeURL(videoID string) string {
	return defaultYouTubeWatchURL + "?v=" + url.QueryEscape(videoID)
}

func extractYouTubeConsentValue(htmlText string) (string, error) {
	match := youtubeConsentValueRegexp.FindStringSubmatch(htmlText)
	if len(match) != configuredModelParts {
		return "", fmt.Errorf("extract youtube consent value: %w", os.ErrInvalid)
	}

	return match[1], nil
}

func extractYouTubeAPIKey(htmlText string) (string, error) {
	match := youtubeAPIKeyRegexp.FindStringSubmatch(htmlText)
	if len(match) != configuredModelParts {
		return "", fmt.Errorf("extract youtube api key: %w", os.ErrInvalid)
	}

	return match[1], nil
}

func extractYouTubeClientVersion(htmlText string) (string, error) {
	match := youtubeClientVersionRegexp.FindStringSubmatch(htmlText)
	if len(match) != configuredModelParts {
		return "", fmt.Errorf("extract youtube client version: %w", os.ErrInvalid)
	}

	return match[1], nil
}

func extractYouTubeInitialData(htmlText string) (map[string]any, error) {
	for _, marker := range []string{`var ytInitialData = `, `window["ytInitialData"] = `} {
		jsonText, ok := extractJavaScriptObject(htmlText, marker)
		if !ok {
			continue
		}

		var initialData map[string]any

		err := json.Unmarshal([]byte(jsonText), &initialData)
		if err != nil {
			return nil, fmt.Errorf("decode youtube initial data: %w", err)
		}

		return initialData, nil
	}

	return nil, fmt.Errorf("extract youtube initial data: %w", os.ErrInvalid)
}

func extractJavaScriptObject(text string, marker string) (string, bool) {
	markerIndex := strings.Index(text, marker)
	if markerIndex == -1 {
		return "", false
	}

	startIndex := strings.Index(text[markerIndex+len(marker):], "{")
	if startIndex == -1 {
		return "", false
	}

	startIndex += markerIndex + len(marker)

	depth := 0
	inString := false
	escaped := false

	for index := startIndex; index < len(text); index++ {
		character := text[index]

		if inString {
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}

			continue
		}

		switch character {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[startIndex : index+1], true
			}
		}
	}

	return "", false
}

func extractYouTubeCommentsContinuationToken(initialData map[string]any) string {
	panels, ok := anySliceAt(initialData, "engagementPanels")
	if !ok {
		return ""
	}

	for _, panel := range panels {
		panelIdentifier, _ := stringAt(
			panel,
			"engagementPanelSectionListRenderer",
			"panelIdentifier",
		)
		if panelIdentifier != youtubeCommentsPanelIdentifier {
			continue
		}

		token, _ := stringAt(
			panel,
			"engagementPanelSectionListRenderer",
			"content",
			"sectionListRenderer",
			"contents",
			0,
			"itemSectionRenderer",
			"contents",
			0,
			"continuationItemRenderer",
			"continuationEndpoint",
			"continuationCommand",
			"token",
		)

		return token
	}

	return ""
}

func isEnglishLanguageCode(languageCode string) bool {
	return languageCode == "en" ||
		strings.HasPrefix(languageCode, "en-") ||
		strings.HasPrefix(languageCode, "en_")
}

func formatNoteGPTTranscript(transcriptData noteGPTVideoTranscriptData) (string, error) {
	segments := selectNoteGPTTranscriptSegments(transcriptData)
	if len(segments) == 0 {
		return "", fmt.Errorf("select notegpt transcript segments: %w", os.ErrNotExist)
	}

	lines := make([]string, 0, len(segments))
	previousLine := ""

	for _, segment := range segments {
		text := strings.Join(strings.Fields(strings.TrimSpace(segment.Text)), " ")
		if text == "" || text == previousLine {
			continue
		}

		lines = append(lines, text)
		previousLine = text
	}

	transcript := strings.Join(lines, "\n")
	if strings.TrimSpace(transcript) == "" {
		return "", fmt.Errorf("empty notegpt transcript: %w", os.ErrInvalid)
	}

	return transcript, nil
}

func selectNoteGPTTranscriptSegments(
	transcriptData noteGPTVideoTranscriptData,
) []noteGPTTranscriptSegment {
	languageCodes := orderedNoteGPTTranscriptLanguageCodes(transcriptData)

	for _, languageCode := range languageCodes {
		if !isEnglishLanguageCode(languageCode) {
			continue
		}

		if segments := preferredNoteGPTTranscriptSegments(transcriptData.Transcripts[languageCode]); len(segments) > 0 {
			return segments
		}
	}

	for _, languageCode := range languageCodes {
		if segments := preferredNoteGPTTranscriptSegments(transcriptData.Transcripts[languageCode]); len(segments) > 0 {
			return segments
		}
	}

	return nil
}

func orderedNoteGPTTranscriptLanguageCodes(
	transcriptData noteGPTVideoTranscriptData,
) []string {
	orderedCodes := make([]string, 0, len(transcriptData.Transcripts))
	seenCodes := make(map[string]struct{}, len(transcriptData.Transcripts))

	for _, languageCode := range transcriptData.LanguageCode {
		code := strings.TrimSpace(languageCode.Code)
		if code == "" {
			continue
		}

		if _, exists := transcriptData.Transcripts[code]; !exists {
			continue
		}

		if _, seen := seenCodes[code]; seen {
			continue
		}

		seenCodes[code] = struct{}{}
		orderedCodes = append(orderedCodes, code)
	}

	additionalCodes := make([]string, 0, len(transcriptData.Transcripts))
	for code := range transcriptData.Transcripts {
		code = strings.TrimSpace(code)
		if code == "" {
			continue
		}

		if _, seen := seenCodes[code]; seen {
			continue
		}

		additionalCodes = append(additionalCodes, code)
	}

	sort.Strings(additionalCodes)

	return append(orderedCodes, additionalCodes...)
}

func preferredNoteGPTTranscriptSegments(
	transcriptTracks noteGPTTranscriptTracks,
) []noteGPTTranscriptSegment {
	if len(transcriptTracks.Default) > 0 {
		return transcriptTracks.Default
	}

	if len(transcriptTracks.Auto) > 0 {
		return transcriptTracks.Auto
	}

	if len(transcriptTracks.Custom) > 0 {
		return transcriptTracks.Custom
	}

	return nil
}

func parseYouTubeCommentsResponse(
	responseBody []byte,
) ([]youtubeComment, string, error) {
	var response map[string]any

	err := json.Unmarshal(responseBody, &response)
	if err != nil {
		return nil, "", fmt.Errorf("decode youtube comments response: %w", err)
	}

	continuationItems := extractYouTubeContinuationItems(response)
	commentEntities := extractYouTubeCommentEntities(response)

	comments := make([]youtubeComment, 0, len(continuationItems))
	nextToken := ""

	for _, item := range continuationItems {
		commentKey, commentKeyFound := stringAt(
			item,
			"commentThreadRenderer",
			"commentViewModel",
			"commentViewModel",
			"commentKey",
		)
		if commentKeyFound {
			comment, exists := commentEntities[commentKey]
			if exists {
				comments = append(comments, comment)
			}

			continue
		}

		token, tokenFound := stringAt(
			item,
			"continuationItemRenderer",
			"continuationEndpoint",
			"continuationCommand",
			"token",
		)
		if tokenFound {
			nextToken = token
		}
	}

	return comments, nextToken, nil
}

func extractYouTubeContinuationItems(response map[string]any) []any {
	onResponseReceivedEndpoints, found := anySliceAt(response, "onResponseReceivedEndpoints")
	if !found {
		return nil
	}

	for _, endpoint := range onResponseReceivedEndpoints {
		continuationItems, continuationItemsFound := anySliceAt(
			endpoint,
			"reloadContinuationItemsCommand",
			"continuationItems",
		)
		if continuationItemsFound {
			return continuationItems
		}

		continuationItems, continuationItemsFound = anySliceAt(
			endpoint,
			"appendContinuationItemsAction",
			"continuationItems",
		)
		if continuationItemsFound {
			return continuationItems
		}
	}

	return nil
}

func extractYouTubeCommentEntities(response map[string]any) map[string]youtubeComment {
	mutations, ok := anySliceAt(response, "frameworkUpdates", "entityBatchUpdate", "mutations")
	if !ok {
		return nil
	}

	comments := make(map[string]youtubeComment, len(mutations))

	for _, mutation := range mutations {
		replyLevel, replyLevelFound := numberAt(
			mutation,
			"payload",
			"commentEntityPayload",
			"properties",
			"replyLevel",
		)
		if !replyLevelFound || replyLevel != 0 {
			continue
		}

		entityKey, entityKeyFound := stringAt(mutation, "entityKey")
		if !entityKeyFound {
			continue
		}

		text, textFound := stringAt(
			mutation,
			"payload",
			"commentEntityPayload",
			"properties",
			"content",
			"content",
		)
		if !textFound || strings.TrimSpace(text) == "" {
			continue
		}

		author, _ := stringAt(
			mutation,
			"payload",
			"commentEntityPayload",
			"author",
			"displayName",
		)
		if author == "" {
			author = unknownText
		}

		comments[entityKey] = youtubeComment{
			Author: author,
			Text:   strings.Join(strings.Fields(strings.TrimSpace(text)), " "),
		}
	}

	return comments
}

func formatYouTubeURLContent(contents []youtubeVideoContent) string {
	formattedContents := make([]string, 0, len(contents))

	for _, content := range contents {
		commentLines := make([]string, 0, len(content.Comments))
		for index, comment := range content.Comments[:minInt(len(content.Comments), maxYouTubeComments)] {
			commentLines = append(
				commentLines,
				fmt.Sprintf("%d. %s: %s", index+1, comment.Author, comment.Text),
			)
		}

		topComments := "No comments found."
		if len(commentLines) > 0 {
			topComments = strings.Join(commentLines, "\n")
		}

		formattedContents = append(
			formattedContents,
			strings.TrimSpace(fmt.Sprintf(
				"URL: %s\nTitle: %s\nChannel: %s\nTranscript:\n%s\nTop comments:\n%s",
				content.URL,
				content.Title,
				content.ChannelName,
				content.Transcript,
				topComments,
			)),
		)
	}

	return strings.Join(formattedContents, "\n\n")
}

func anySliceAt(value any, path ...any) ([]any, bool) {
	resolvedValue, valueFound := valueAt(value, path...)
	if !valueFound {
		return nil, false
	}

	typedValue, ok := resolvedValue.([]any)

	return typedValue, ok
}

func stringAt(value any, path ...any) (string, bool) {
	resolvedValue, valueFound := valueAt(value, path...)
	if !valueFound {
		return "", false
	}

	typedValue, ok := resolvedValue.(string)

	return typedValue, ok
}

func numberAt(value any, path ...any) (float64, bool) {
	resolvedValue, valueFound := valueAt(value, path...)
	if !valueFound {
		return 0, false
	}

	typedValue, ok := resolvedValue.(float64)

	return typedValue, ok
}

func valueAt(value any, path ...any) (any, bool) {
	currentValue := value

	for _, step := range path {
		switch typedStep := step.(type) {
		case string:
			currentMap, ok := currentValue.(map[string]any)
			if !ok {
				return nil, false
			}

			nextValue, exists := currentMap[typedStep]
			if !exists {
				return nil, false
			}

			currentValue = nextValue
		case int:
			currentSlice, ok := currentValue.([]any)
			if !ok || typedStep < 0 || typedStep >= len(currentSlice) {
				return nil, false
			}

			currentValue = currentSlice[typedStep]
		default:
			return nil, false
		}
	}

	return currentValue, true
}
