package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const (
	defaultYouTubeWatchURL        = "https://www.youtube.com/watch"
	defaultYouTubeAPIBaseURL      = "https://www.youtube.com/youtubei/v1"
	youtubeWarningText            = "Warning: YouTube content unavailable"
	youtubeAndroidClientName      = "ANDROID"
	youtubeAndroidClientVersion   = "20.10.38"
	youtubeWebClientName          = "WEB"
	youtubeWebClientVersionHeader = "1"
	youtubeAcceptLanguage         = "en-US,en;q=0.9"
	youtubeUserAgent              = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"
	youtubeCommentsPanelIdentifier = "engagement-panel-comments-section"
)

var (
	youtubeURLRegexp = regexp.MustCompile(
		`(?i)\b(?:https?://)?(?:[\w-]+\.)?(?:youtube\.com|youtube-nocookie\.com|youtu\.be)/[^\s<>()]+`,
	)
	youtubeVideoIDRegexp       = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	youtubeAPIKeyRegexp        = regexp.MustCompile(`"INNERTUBE_API_KEY"\s*:\s*"([^"]+)"`)
	youtubeClientVersionRegexp = regexp.MustCompile(`"INNERTUBE_CLIENT_VERSION"\s*:\s*"([^"]+)"`)
	youtubeConsentValueRegexp  = regexp.MustCompile(`name="v"\s+value="([^"]+)"`)
	youtubeHTMLTagRegexp       = regexp.MustCompile(`<[^>]+>`)
	youtubeLineBreakTagRegexp  = regexp.MustCompile(`(?i)<br\s*/?>`)
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
	httpClient *http.Client
	watchURL   string
	apiBaseURL string
	userAgent  string
}

type youtubeWatchPage struct {
	APIKey        string
	ClientVersion string
	CommentsToken string
}

type youtubePlayerResponse struct {
	PlayabilityStatus youtubePlayabilityStatus
	VideoDetails      youtubeVideoDetails
	Captions          youtubeTracklistRenderer
}

type youtubePlayabilityStatus struct {
	Status string
	Reason string
}

type youtubeVideoDetails struct {
	Title  string
	Author string
}

type youtubeTracklistRenderer struct {
	CaptionTracks        []youtubeCaptionTrack
	TranslationLanguages []youtubeTranslationLanguage
}

type youtubeCaptionTrack struct {
	BaseURL        string
	LanguageCode   string
	Kind           string
	IsTranslatable bool
}

type youtubeTranslationLanguage struct {
	LanguageCode string
}

type youtubeTranscriptDocument struct {
	Body youtubeTranscriptBody `xml:"body"`
}

type youtubeTranscriptBody struct {
	Paragraphs []youtubeTranscriptParagraph `xml:"p"`
}

type youtubeTranscriptParagraph struct {
	InnerXML string `xml:",innerxml"`
}

func newYouTubeClient(httpClient *http.Client) youtubeClient {
	return youtubeClient{
		httpClient: httpClient,
		watchURL:   defaultYouTubeWatchURL,
		apiBaseURL: defaultYouTubeAPIBaseURL,
		userAgent:  youtubeUserAgent,
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

	augmentedConversation, warnings, err := augmentConversationWithConcurrentURLContent(
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

	watchPage, err := client.fetchWatchPage(requestContext, videoID)
	if err != nil {
		return youtubeVideoContent{}, fmt.Errorf("fetch watch page for %q: %w", rawURL, err)
	}

	playerResponse, err := client.fetchPlayerResponse(requestContext, watchPage.APIKey, videoID)
	if err != nil {
		return youtubeVideoContent{}, fmt.Errorf("fetch player response for %q: %w", rawURL, err)
	}

	if playabilityStatus := strings.TrimSpace(playerResponse.PlayabilityStatus.Status); playabilityStatus != "" &&
		!strings.EqualFold(playabilityStatus, "OK") {
		reason := strings.TrimSpace(playerResponse.PlayabilityStatus.Reason)
		if reason == "" {
			reason = playabilityStatus
		}

		return youtubeVideoContent{}, fmt.Errorf("video %q unavailable: %s: %w", rawURL, reason, os.ErrInvalid)
	}

	title := strings.TrimSpace(playerResponse.VideoDetails.Title)

	channelName := strings.TrimSpace(playerResponse.VideoDetails.Author)
	if title == "" || channelName == "" {
		return youtubeVideoContent{}, fmt.Errorf(
			"extract title/channel for %q: %w",
			rawURL,
			os.ErrInvalid,
		)
	}

	transcript, err := client.fetchTranscript(
		requestContext,
		videoID,
		playerResponse.Captions,
	)
	if err != nil {
		return youtubeVideoContent{}, fmt.Errorf("fetch transcript for %q: %w", rawURL, err)
	}

	comments, err := client.fetchComments(
		requestContext,
		watchPage.APIKey,
		watchPage.ClientVersion,
		watchPage.CommentsToken,
	)
	if err != nil {
		slog.Warn("fetch youtube comments", "url", rawURL, "error", err)
	}

	return youtubeVideoContent{
		URL:         canonicalURL,
		VideoID:     videoID,
		Title:       title,
		ChannelName: channelName,
		Transcript:  transcript,
		Comments:    comments,
	}, nil
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

func (client youtubeClient) fetchPlayerResponse(
	ctx context.Context,
	apiKey string,
	videoID string,
) (youtubePlayerResponse, error) {
	requestURL := client.buildAPIURL("player", apiKey)

	requestBody := map[string]any{
		"context": map[string]any{
			"client": map[string]string{
				"clientName":    youtubeAndroidClientName,
				"clientVersion": youtubeAndroidClientVersion,
				"hl":            "en",
				"gl":            "US",
			},
		},
		"videoId": videoID,
	}

	responseBody, err := client.doJSONRequest(ctx, requestURL, requestBody, nil)
	if err != nil {
		return youtubePlayerResponse{}, err
	}

	var payload map[string]any

	err = json.Unmarshal(responseBody, &payload)
	if err != nil {
		return youtubePlayerResponse{}, fmt.Errorf("decode youtube player response: %w", err)
	}

	return parseYouTubePlayerResponse(payload), nil
}

func (client youtubeClient) fetchTranscript(
	ctx context.Context,
	videoID string,
	tracklist youtubeTracklistRenderer,
) (string, error) {
	track, err := selectYouTubeCaptionTrack(tracklist)
	if err != nil {
		return "", fmt.Errorf("select caption track for %q: %w", videoID, err)
	}

	transcriptURL, err := buildYouTubeTranscriptURL(tracklist, track)
	if err != nil {
		return "", fmt.Errorf("build transcript url for %q: %w", videoID, err)
	}

	responseBody, err := client.doRequest(ctx, http.MethodGet, transcriptURL, nil, nil)
	if err != nil {
		return "", err
	}

	transcript, err := parseYouTubeTranscript(responseBody)
	if err != nil {
		return "", fmt.Errorf("parse transcript for %q: %w", videoID, err)
	}

	if strings.TrimSpace(transcript) == "" {
		return "", fmt.Errorf("empty transcript for %q: %w", videoID, os.ErrInvalid)
	}

	return transcript, nil
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

func selectYouTubeCaptionTrack(tracklist youtubeTracklistRenderer) (youtubeCaptionTrack, error) {
	if len(tracklist.CaptionTracks) == 0 {
		return youtubeCaptionTrack{}, fmt.Errorf("no caption tracks available: %w", os.ErrNotExist)
	}

	priorityGroups := [][]youtubeCaptionTrack{
		filterCaptionTracks(tracklist.CaptionTracks, true, true),
		filterCaptionTracks(tracklist.CaptionTracks, true, false),
		filterCaptionTracks(tracklist.CaptionTracks, false, true),
		filterCaptionTracks(tracklist.CaptionTracks, false, false),
	}

	for _, tracks := range priorityGroups {
		if len(tracks) == 0 {
			continue
		}

		return tracks[0], nil
	}

	return youtubeCaptionTrack{}, fmt.Errorf("select caption track: %w", os.ErrNotExist)
}

func filterCaptionTracks(
	tracks []youtubeCaptionTrack,
	preferEnglish bool,
	manualOnly bool,
) []youtubeCaptionTrack {
	filteredTracks := make([]youtubeCaptionTrack, 0, len(tracks))

	for _, track := range tracks {
		if manualOnly && strings.EqualFold(track.Kind, "asr") {
			continue
		}

		if preferEnglish && !isEnglishLanguageCode(track.LanguageCode) {
			continue
		}

		filteredTracks = append(filteredTracks, track)
	}

	return filteredTracks
}

func isEnglishLanguageCode(languageCode string) bool {
	return languageCode == "en" || strings.HasPrefix(languageCode, "en-")
}

func buildYouTubeTranscriptURL(
	tracklist youtubeTracklistRenderer,
	track youtubeCaptionTrack,
) (string, error) {
	transcriptURL, err := url.Parse(track.BaseURL)
	if err != nil {
		return "", fmt.Errorf("parse transcript url %q: %w", track.BaseURL, err)
	}

	queryValues := transcriptURL.Query()
	queryValues.Set("fmt", "srv3")

	if !isEnglishLanguageCode(track.LanguageCode) &&
		track.IsTranslatable &&
		tracklistHasEnglishTranslation(tracklist) {
		queryValues.Set("tlang", "en")
	}

	transcriptURL.RawQuery = queryValues.Encode()

	return transcriptURL.String(), nil
}

func tracklistHasEnglishTranslation(tracklist youtubeTracklistRenderer) bool {
	for _, language := range tracklist.TranslationLanguages {
		if isEnglishLanguageCode(language.LanguageCode) {
			return true
		}
	}

	return false
}

func parseYouTubeTranscript(responseBody []byte) (string, error) {
	var document youtubeTranscriptDocument

	err := xml.Unmarshal(responseBody, &document)
	if err != nil {
		return "", fmt.Errorf("decode youtube transcript xml: %w", err)
	}

	textParts := make([]string, 0, len(document.Body.Paragraphs))
	for _, paragraph := range document.Body.Paragraphs {
		text := strings.TrimSpace(paragraph.InnerXML)
		if text == "" {
			continue
		}

		text = youtubeLineBreakTagRegexp.ReplaceAllString(text, "\n")
		text = youtubeHTMLTagRegexp.ReplaceAllString(text, "")
		text = html.UnescapeString(text)

		text = strings.Join(strings.Fields(text), " ")
		if text == "" {
			continue
		}

		textParts = append(textParts, text)
	}

	return strings.Join(textParts, "\n"), nil
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
			author = "Unknown"
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

func boolAt(value any, path ...any) (bool, bool) {
	resolvedValue, valueFound := valueAt(value, path...)
	if !valueFound {
		return false, false
	}

	typedValue, ok := resolvedValue.(bool)

	return typedValue, ok
}

func parseYouTubePlayerResponse(payload map[string]any) youtubePlayerResponse {
	response := youtubePlayerResponse{
		PlayabilityStatus: youtubePlayabilityStatus{
			Status: valueOrEmptyString(payload, "playabilityStatus", "status"),
			Reason: valueOrEmptyString(payload, "playabilityStatus", "reason"),
		},
		VideoDetails: youtubeVideoDetails{
			Title:  valueOrEmptyString(payload, "videoDetails", "title"),
			Author: valueOrEmptyString(payload, "videoDetails", "author"),
		},
		Captions: youtubeTracklistRenderer{
			CaptionTracks:        nil,
			TranslationLanguages: nil,
		},
	}

	captionTracks, _ := anySliceAt(
		payload,
		"captions",
		"playerCaptionsTracklistRenderer",
		"captionTracks",
	)
	for _, captionTrack := range captionTracks {
		baseURL, hasBaseURL := stringAt(captionTrack, "baseUrl")
		if !hasBaseURL || strings.TrimSpace(baseURL) == "" {
			continue
		}

		response.Captions.CaptionTracks = append(response.Captions.CaptionTracks, youtubeCaptionTrack{
			BaseURL:        baseURL,
			LanguageCode:   valueOrEmptyString(captionTrack, "languageCode"),
			Kind:           valueOrEmptyString(captionTrack, "kind"),
			IsTranslatable: valueOrFalse(captionTrack, "isTranslatable"),
		})
	}

	translationLanguages, _ := anySliceAt(
		payload,
		"captions",
		"playerCaptionsTracklistRenderer",
		"translationLanguages",
	)
	for _, translationLanguage := range translationLanguages {
		languageCode := valueOrEmptyString(translationLanguage, "languageCode")
		if languageCode == "" {
			continue
		}

		response.Captions.TranslationLanguages = append(
			response.Captions.TranslationLanguages,
			youtubeTranslationLanguage{LanguageCode: languageCode},
		)
	}

	return response
}

func valueOrEmptyString(value any, path ...any) string {
	resolvedValue, _ := stringAt(value, path...)

	return resolvedValue
}

func valueOrFalse(value any, path ...any) bool {
	resolvedValue, _ := boolAt(value, path...)

	return resolvedValue
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
