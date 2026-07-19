package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const (
	defaultTikTokLandingURL   = "https://snaptik.app"
	defaultTikTokChallengeURL = "https://snaptik.app/api/token"
	defaultTikTokExtractURL   = "https://snaptik.app/api/extract"
	tikTokDefaultFilename     = "tiktok.mp4"
	tikTokDefaultMIMEType     = "video/mp4"
	tikTokFilenamePrefix      = "tiktok_"
	tikTokWarningText         = "Warning: TikTok content unavailable"
	oneCaptureMatchCount      = 2
	challengeMask             = 255
)

var (
	tikTokURLRegexp = regexp.MustCompile(
		`(?i)\b(?:https?://)?(?:[\w-]+\.)?(?:tiktok\.com|tnktok\.com)/[^\s<>()]+`,
	)
	tikTokVideoIDRegexp = regexp.MustCompile(`/video/([0-9]+)`)
)

type tiktokFetcher interface {
	fetch(ctx context.Context, rawURL string) (tiktokVideoContent, error)
}

type tiktokClient struct {
	httpClient   *http.Client
	landingURL   string
	challengeURL string
	extractURL   string
	userAgent    string
}

type tiktokVideoContent struct {
	ResolvedURL string
	DownloadURL string
	MediaPart   contentPart
}

func (content tiktokVideoContent) resolvedURL() string {
	return strings.TrimSpace(content.ResolvedURL)
}

func (content tiktokVideoContent) mediaPart() contentPart {
	return content.MediaPart
}

type snaptikChallengeResponse struct {
	ID string `json:"id"`
	P  string `json:"p"`
}

type snaptikChallenge struct {
	T string `json:"t"`
	A int    `json:"a"`
	B int    `json:"b"`
	S int    `json:"s"`
	C int    `json:"c"`
	I int    `json:"i"`
	M int    `json:"m"`
	W string `json:"w"`
	N []int  `json:"n"`
	E int    `json:"e"`
	H string `json:"h"`
}

func lookupKey(raw map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		if val, ok := raw[key]; ok {
			return val, true
		}
	}

	for _, key := range keys {
		for rawKey, val := range raw {
			if strings.EqualFold(rawKey, key) {
				return val, true
			}
		}
	}

	return nil, false
}

func (challenge *snaptikChallenge) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage

	err := json.Unmarshal(data, &raw)
	if err != nil {
		return fmt.Errorf("unmarshal challenge raw JSON: %w", err)
	}

	if val, ok := lookupKey(raw, "t"); ok {
		_ = json.Unmarshal(val, &challenge.T)
	}

	if val, ok := lookupKey(raw, "a"); ok {
		_ = json.Unmarshal(val, &challenge.A)
	}

	if val, ok := lookupKey(raw, "b"); ok {
		_ = json.Unmarshal(val, &challenge.B)
	}

	if val, ok := lookupKey(raw, "s"); ok {
		_ = json.Unmarshal(val, &challenge.S)
	}

	if val, ok := lookupKey(raw, "c"); ok {
		_ = json.Unmarshal(val, &challenge.C)
	}

	if val, ok := lookupKey(raw, "i"); ok {
		_ = json.Unmarshal(val, &challenge.I)
	}

	if val, ok := lookupKey(raw, "m"); ok {
		_ = json.Unmarshal(val, &challenge.M)
	}

	if val, ok := lookupKey(raw, "w"); ok {
		_ = json.Unmarshal(val, &challenge.W)
	}

	if val, ok := lookupKey(raw, "n"); ok {
		_ = json.Unmarshal(val, &challenge.N)
	}

	if val, ok := lookupKey(raw, "_e", "e"); ok {
		_ = json.Unmarshal(val, &challenge.E)
	}

	if val, ok := lookupKey(raw, "_h", "h"); ok {
		_ = json.Unmarshal(val, &challenge.H)
	}

	return nil
}

type snaptikExtractResponse struct {
	Success bool               `json:"success"`
	Data    snaptikExtractData `json:"data"`
}

type snaptikExtractData struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	Title         string            `json:"title"`
	DownloadURL   string            `json:"download_url"`
	Filename      string            `json:"filename"`
	Headers       map[string]string `json:"headers"`
	HDDownloadURL string            `json:"hd_download_url"`
}

func (data *snaptikExtractData) UnmarshalJSON(bytesVal []byte) error {
	var raw map[string]json.RawMessage

	err := json.Unmarshal(bytesVal, &raw)
	if err != nil {
		return fmt.Errorf("unmarshal extract data raw JSON: %w", err)
	}

	if val, ok := lookupKey(raw, "id"); ok {
		_ = json.Unmarshal(val, &data.ID)
	}

	if val, ok := lookupKey(raw, "type"); ok {
		_ = json.Unmarshal(val, &data.Type)
	}

	if val, ok := lookupKey(raw, "title"); ok {
		_ = json.Unmarshal(val, &data.Title)
	}

	if val, ok := lookupKey(raw, "downloadUrl", "download_url"); ok {
		_ = json.Unmarshal(val, &data.DownloadURL)
	}

	if val, ok := lookupKey(raw, "filename"); ok {
		_ = json.Unmarshal(val, &data.Filename)
	}

	if val, ok := lookupKey(raw, "headers"); ok {
		_ = json.Unmarshal(val, &data.Headers)
	}

	if val, ok := lookupKey(raw, "hdDownloadUrl", "hd_download_url"); ok {
		_ = json.Unmarshal(val, &data.HDDownloadURL)
	}

	return nil
}

func newTikTokClient(httpClient *http.Client) tiktokClient {
	return tiktokClient{
		httpClient:   httpClient,
		landingURL:   defaultTikTokLandingURL,
		challengeURL: defaultTikTokChallengeURL,
		extractURL:   defaultTikTokExtractURL,
		userAgent:    youtubeUserAgent,
	}
}

func (instance *bot) maybeAugmentConversationWithTikTok(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	conversation []chatMessage,
	urlExtractionText string,
) ([]chatMessage, []string, error) {
	preparedAugmentation, err := instance.prepareTikTokAugmentation(
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

func (instance *bot) prepareTikTokAugmentation(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	urlExtractionText string,
) (preparedConversationAugmentation, error) {
	if instance.tiktok == nil {
		return emptyPreparedConversationAugmentation(), nil
	}

	tikTokURLs := extractTikTokURLs(urlExtractionText)
	if len(tikTokURLs) == 0 {
		return emptyPreparedConversationAugmentation(), nil
	}

	videoContents, warnings := fetchDownloadedVideos(
		ctx,
		tikTokURLs,
		instance.tiktok.fetch,
		"fetch tiktok content",
		tikTokWarningText,
	)
	if len(videoContents) == 0 {
		return warningPreparedConversationAugmentation(warnings), nil
	}

	return prepareDownloadedVideoAugmentation(
		ctx,
		downloadedVideoAugmentationRequest[tiktokVideoContent]{
			instance:           instance,
			loadedConfig:       loadedConfig,
			providerSlashModel: providerSlashModel,
			videoContents:      videoContents,
			warnings:           warnings,
			warningText:        tikTokWarningText,
			label:              "tiktok",
		},
	)
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

	verifyCode, err := client.fetchToken(requestContext)
	if err != nil {
		return tiktokVideoContent{}, fmt.Errorf("fetch snaptik token: %w", err)
	}

	extractData, err := client.extractMedia(requestContext, resolvedURL, verifyCode)
	if err != nil {
		return tiktokVideoContent{}, fmt.Errorf("extract snaptik media: %w", err)
	}

	videoBytes, mimeType, filename, err := client.downloadVideo(
		requestContext,
		extractData.DownloadURL,
		resolvedURL,
		extractData.Headers,
	)
	if err != nil {
		return tiktokVideoContent{}, fmt.Errorf("download tiktok video: %w", err)
	}

	return tiktokVideoContent{
		ResolvedURL: resolvedURL,
		DownloadURL: extractData.DownloadURL,
		MediaPart: contentPart{
			messageTypeKey:       contentTypeVideoData,
			contentFieldBytes:    videoBytes,
			contentFieldMIMEType: mimeType,
			contentFieldFilename: filename,
		},
	}, nil
}

func extractTikTokURLs(text string) []string {
	text = normalizedURLExtractionText(text)

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

	httpRequest.Header.Set(userAgentHeader, client.userAgent)

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

func solveSnaptikChallenge(challenge snaptikChallenge) (int, error) {
	switch challenge.T {
	case "b":
		return (challenge.A ^ challenge.B) >> challenge.S & challengeMask, nil

	case "r":
		sum := 0
		for _, value := range challenge.N {
			sum += value
		}

		return sum*2 + 1, nil

	case "c":
		runes := []rune(challenge.W)
		if challenge.I < 0 || challenge.I >= len(runes) {
			return 0, fmt.Errorf("index out of bounds in string challenge: %w", os.ErrInvalid)
		}

		return int(runes[challenge.I]) * challenge.M, nil

	case "m":
		return (challenge.A + challenge.B) % 100 * challenge.C, nil

	case "n":
		return challenge.A*challenge.B + challenge.B*challenge.C + challenge.C*challenge.A - challenge.A, nil

	default:
		return 0, fmt.Errorf("unknown challenge type %q: %w", challenge.T, os.ErrInvalid)
	}
}

func decryptAES(key []byte, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < aes.BlockSize {
		return nil, fmt.Errorf("ciphertext too short: %w", os.ErrInvalid)
	}

	initializationVector := ciphertext[:aes.BlockSize]
	data := ciphertext[aes.BlockSize:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create aes cipher: %w", err)
	}

	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext not a multiple of block size: %w", os.ErrInvalid)
	}

	mode := cipher.NewCBCDecrypter(block, initializationVector)
	decrypted := make([]byte, len(data))
	mode.CryptBlocks(decrypted, data)

	if len(decrypted) == 0 {
		return nil, fmt.Errorf("decrypted data is empty: %w", os.ErrInvalid)
	}

	padding := int(decrypted[len(decrypted)-1])

	if padding < 1 || padding > aes.BlockSize {
		return decrypted, nil
	}

	for i := len(decrypted) - padding; i < len(decrypted); i++ {
		if int(decrypted[i]) != padding {
			return decrypted, nil
		}
	}

	return decrypted[:len(decrypted)-padding], nil
}

func (client tiktokClient) fetchToken(ctx context.Context) (string, error) {
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.challengeURL,
		bytes.NewBufferString("{}"),
	)
	if err != nil {
		return "", fmt.Errorf("create snaptik token request: %w", err)
	}

	httpRequest.Header.Set("X-Requested-With", "XMLHttpRequest")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set(userAgentHeader, client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("send snaptik token request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return "", fmt.Errorf("read snaptik token response: %w", err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf(
			"snaptik token request failed with status %d: %s: %w",
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	var response snaptikChallengeResponse

	err = json.Unmarshal(responseBody, &response)
	if err != nil {
		return "", fmt.Errorf("parse snaptik token response: %w", err)
	}

	decodedBytes, err := base64.StdEncoding.DecodeString(response.P)
	if err != nil {
		return "", fmt.Errorf("decode snaptik encrypted token payload: %w", err)
	}

	h := sha256.New()
	h.Write([]byte("sn4pt1k_v3r1fy2026:" + response.ID))
	key := h.Sum(nil)

	decryptedBytes, err := decryptAES(key, decodedBytes)
	if err != nil {
		return "", fmt.Errorf("decrypt snaptik token payload: %w", err)
	}

	var challenge snaptikChallenge

	err = json.Unmarshal(decryptedBytes, &challenge)
	if err != nil {
		return "", fmt.Errorf("parse snaptik challenge: %w", err)
	}

	ans, err := solveSnaptikChallenge(challenge)
	if err != nil {
		return "", fmt.Errorf("solve snaptik challenge: %w", err)
	}

	verifyCode := fmt.Sprintf("%s:%d:%d:%s", response.ID, ans, challenge.E, challenge.H)

	return verifyCode, nil
}

func (client tiktokClient) extractMedia(
	ctx context.Context,
	resolvedURL string,
	verifyCode string,
) (snaptikExtractData, error) {
	extractURL, err := url.Parse(client.extractURL)
	if err != nil {
		return snaptikExtractData{}, fmt.Errorf("parse snaptik extract url: %w", err)
	}

	queryValues := extractURL.Query()
	queryValues.Set("url", resolvedURL)
	extractURL.RawQuery = queryValues.Encode()

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		extractURL.String(),
		nil,
	)
	if err != nil {
		return snaptikExtractData{}, fmt.Errorf("create snaptik extract request: %w", err)
	}

	httpRequest.Header.Set("X-Requested-With", "XMLHttpRequest")
	httpRequest.Header.Set("X-Verify", verifyCode)
	httpRequest.Header.Set(userAgentHeader, client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return snaptikExtractData{}, fmt.Errorf("send snaptik extract request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return snaptikExtractData{}, fmt.Errorf("read snaptik extract response: %w", err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return snaptikExtractData{}, fmt.Errorf(
			"snaptik extract request failed with status %d: %s: %w",
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	var response snaptikExtractResponse

	err = json.Unmarshal(responseBody, &response)
	if err != nil {
		return snaptikExtractData{}, fmt.Errorf("parse snaptik extract response: %w", err)
	}

	if !response.Success {
		return snaptikExtractData{}, fmt.Errorf("snaptik extract failed: success=false: %w", os.ErrInvalid)
	}

	if strings.TrimSpace(response.Data.DownloadURL) == "" {
		return snaptikExtractData{}, fmt.Errorf("missing download url in snaptik extract response: %w", os.ErrInvalid)
	}

	return response.Data, nil
}

func (client tiktokClient) downloadVideo(
	ctx context.Context,
	downloadURL string,
	resolvedURL string,
	headers map[string]string,
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

	for key, value := range headers {
		httpRequest.Header.Set(key, value)
	}

	if httpRequest.Header.Get(userAgentHeader) == "" {
		httpRequest.Header.Set(userAgentHeader, client.userAgent)
	}

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

func tikTokFilename(resolvedURL, contentDisposition string) string {
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
