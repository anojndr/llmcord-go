package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	defaultFacebookGetMyFBProcessURL  = "https://getmyfb.com/process"
	defaultAutocompressorBaseURL      = "https://autocompressor.net"
	autocompressorTargetSize          = "8"
	autocompressorDefaultPollInterval = time.Second
	autocompressorMaxPollAttempts     = 120
	facebookDefaultFilename           = "facebook.mp4"
	facebookDefaultMIMEType           = "video/mp4"
	facebookFilenamePrefix            = "facebook_"
	facebookGetMyFBLocale             = "en"
	facebookGetMyFBDownloadUserAgent  = "Mozilla/5.0"
	facebookDefaultHDQualityScore     = 720
	facebookDefaultSDQualityScore     = 360
	facebookDownloadCandidateCapacity = 4
	facebookMaxReplyAttachments       = 10
	facebookMaxUploadBytes            = 10 << 20
	facebookWarningText               = "Warning: Facebook content unavailable"
)

var (
	facebookURLRegexp = regexp.MustCompile(
		`(?i)\b(?:https?://)?(?:[\w-]+\.)?(?:facebook\.com|fb\.watch)/[^\s<>()]+`,
	)
	facebookFilenameRegexp = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
	facebookQualityRegexp  = regexp.MustCompile(`(\d{3,4})`)
)

const facebookRequestContextErrorFormat = "facebook request context: %w"

type facebookFetcher interface {
	fetch(
		ctx context.Context,
		rawURL string,
	) (facebookVideoContent, error)
}

type facebookScraper interface {
	Post(url string, contentType string, body io.Reader) (*http.Response, error)
}

type facebookClient struct {
	httpClient        *http.Client
	scraper           facebookScraper
	getMyFBProcessURL string
	compressorURL     string
	pollInterval      time.Duration
}

type facebookVideoContent struct {
	ResolvedURL string
	DownloadURL string
	MediaPart   contentPart
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

func newFacebookClient(httpClient *http.Client) facebookClient {
	return facebookClient{
		httpClient:        httpClient,
		scraper:           httpClient,
		getMyFBProcessURL: defaultFacebookGetMyFBProcessURL,
		compressorURL:     defaultAutocompressorBaseURL,
		pollInterval:      autocompressorDefaultPollInterval,
	}
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
			return instance.facebook.fetch(fetchCtx, rawURL)
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
) (facebookVideoContent, error) {
	normalizedURL, err := normalizeFacebookURL(rawURL)
	if err != nil {
		return facebookVideoContent{}, err
	}

	videoContent, err := client.fetchWithGetMyFB(ctx, normalizedURL)
	if err != nil {
		return facebookVideoContent{}, fmt.Errorf("fetch facebook content: %w", err)
	}

	return videoContent, nil
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

		if int64(len(videoBytes)) > facebookMaxUploadBytes {
			compressedBytes, compressedMIME, compressedFilename, compressErr := client.compressVideo(
				ctx,
				videoBytes,
				filename,
			)
			if compressErr == nil && len(compressedBytes) > 0 {
				videoBytes = compressedBytes
				if compressedMIME != "" {
					mimeType = compressedMIME
				}
				if compressedFilename != "" {
					filename = compressedFilename
				}
			} else if compressErr != nil {
				logWarn(
					"compress facebook video with autocompressor",
					compressErr,
					"url",
					normalizedURL,
				)
			}
		}

		return facebookVideoContent{
			ResolvedURL: normalizedURL,
			DownloadURL: downloadURL,
			MediaPart: contentPart{
				messageTypeKey:       contentTypeVideoData,
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
	if len(qualityMatch) > 1 {
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

func facebookGetMyFBDownloadHeaders(processURL string) (string, string) {
	parsedURL, err := url.Parse(strings.TrimSpace(processURL))
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return "", ""
	}

	originURL := parsedURL.Scheme + "://" + parsedURL.Host

	return originURL + "/", originURL
}

type autocompressorRQJobRequest struct {
	SourceType       string                 `json:"source_type"`
	CompressionLevel string                 `json:"compression_level"`
	TargetSize       string                 `json:"target_size"`
	OutputFormat     string                 `json:"output_format"`
	MoreOptions      map[string]interface{} `json:"moreoptions"`
}

type autocompressorRQJobResponse struct {
	Allowed     bool   `json:"allowed"`
	Server      string `json:"server"`
	Message     string `json:"message"`
	UploadLimit int64  `json:"upload_limit"`
}

type autocompressorUploadResponse struct {
	Error interface{} `json:"error"`
}

type autocompressorStatusResponse struct {
	Error  interface{} `json:"error"`
	Status struct {
		Thumbnail bool        `json:"thumbnail"`
		Ended     bool        `json:"ended"`
		Error     interface{} `json:"error"`
	} `json:"status"`
	Progress struct {
		Action     string  `json:"action"`
		Quantified bool    `json:"quantified"`
		Progress   float64 `json:"progress"`
	} `json:"progress"`
}

func (client facebookClient) compressVideo(
	ctx context.Context,
	videoBytes []byte,
	filename string,
) ([]byte, string, string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(client.compressorURL), "/")
	if baseURL == "" {
		baseURL = defaultAutocompressorBaseURL
	}

	rqReqBody, err := json.Marshal(autocompressorRQJobRequest{
		SourceType:       "file",
		CompressionLevel: "normal",
		TargetSize:       autocompressorTargetSize,
		OutputFormat:     "mp4",
		MoreOptions: map[string]interface{}{
			"av1webm": false,
			"dlaudio": false,
		},
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("marshal autocompressor rqjob request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		baseURL+"/rqjob",
		bytes.NewReader(rqReqBody),
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("create autocompressor rqjob request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", facebookGetMyFBDownloadUserAgent)

	httpResp, err := client.httpClient.Do(httpReq)
	if err != nil {
		return nil, "", "", fmt.Errorf("send autocompressor rqjob request: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read autocompressor rqjob response: %w", err)
	}

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return nil, "", "", fmt.Errorf(
			"autocompressor rqjob failed with status %d: %s: %w",
			httpResp.StatusCode,
			strings.TrimSpace(string(respBytes)),
			os.ErrInvalid,
		)
	}

	var rqResp autocompressorRQJobResponse
	err = json.Unmarshal(respBytes, &rqResp)
	if err != nil {
		return nil, "", "", fmt.Errorf("decode autocompressor rqjob response: %w", err)
	}

	if !rqResp.Allowed || strings.TrimSpace(rqResp.Message) == "" || strings.TrimSpace(rqResp.Server) == "" {
		return nil, "", "", fmt.Errorf("autocompressor rqjob not allowed: %s: %w", rqResp.Message, os.ErrInvalid)
	}

	jobID := strings.TrimSpace(rqResp.Message)
	server := strings.TrimSpace(rqResp.Server)

	var serverURL string
	if strings.Contains(baseURL, "://autocompressor.net") {
		serverURL = fmt.Sprintf("https://auto-rez-%s.autocompressor.net", server)
	} else {
		serverURL = baseURL
	}

	var bodyBuf bytes.Buffer
	mpWriter := multipart.NewWriter(&bodyBuf)

	_ = mpWriter.WriteField("source_url", "null")

	uploadFilename := strings.TrimSpace(filename)
	if uploadFilename == "" {
		uploadFilename = facebookDefaultFilename
	}

	part, err := mpWriter.CreateFormFile("filetoupload", uploadFilename)
	if err != nil {
		return nil, "", "", fmt.Errorf("create autocompressor form file: %w", err)
	}

	_, err = part.Write(videoBytes)
	if err != nil {
		return nil, "", "", fmt.Errorf("write autocompressor form file bytes: %w", err)
	}

	err = mpWriter.Close()
	if err != nil {
		return nil, "", "", fmt.Errorf("close autocompressor multipart writer: %w", err)
	}

	uploadReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/job/%s/upload", serverURL, jobID),
		&bodyBuf,
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("create autocompressor upload request: %w", err)
	}

	uploadReq.Header.Set("Content-Type", mpWriter.FormDataContentType())
	uploadReq.Header.Set("User-Agent", facebookGetMyFBDownloadUserAgent)

	uploadResp, err := client.httpClient.Do(uploadReq)
	if err != nil {
		return nil, "", "", fmt.Errorf("send autocompressor upload request: %w", err)
	}
	defer func() {
		_ = uploadResp.Body.Close()
	}()

	uploadRespBytes, err := io.ReadAll(uploadResp.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read autocompressor upload response: %w", err)
	}

	if uploadResp.StatusCode < http.StatusOK || uploadResp.StatusCode >= http.StatusMultipleChoices {
		return nil, "", "", fmt.Errorf(
			"autocompressor upload failed with status %d: %s: %w",
			uploadResp.StatusCode,
			strings.TrimSpace(string(uploadRespBytes)),
			os.ErrInvalid,
		)
	}

	var uploadStatus autocompressorUploadResponse
	if err := json.Unmarshal(uploadRespBytes, &uploadStatus); err == nil {
		if uploadStatus.Error != nil && uploadStatus.Error != false {
			return nil, "", "", fmt.Errorf("autocompressor upload returned error: %v: %w", uploadStatus.Error, os.ErrInvalid)
		}
	}

	downloadURL := fmt.Sprintf("%s/job/%s/download", serverURL, jobID)
	statusURL := fmt.Sprintf("%s/job/%s/status", serverURL, jobID)

	for range autocompressorMaxPollAttempts {
		err := ctx.Err()
		if err != nil {
			return nil, "", "", fmt.Errorf(facebookRequestContextErrorFormat, err)
		}

		pollInterval := client.pollInterval
		if pollInterval <= 0 {
			pollInterval = autocompressorDefaultPollInterval
		}

		select {
		case <-ctx.Done():
			return nil, "", "", fmt.Errorf(facebookRequestContextErrorFormat, ctx.Err())
		case <-time.After(pollInterval):
		}

		statusReq, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			statusURL,
			nil,
		)
		if err != nil {
			return nil, "", "", fmt.Errorf("create autocompressor status request: %w", err)
		}

		statusReq.Header.Set("User-Agent", facebookGetMyFBDownloadUserAgent)

		statusResp, err := client.httpClient.Do(statusReq)
		if err != nil {
			return nil, "", "", fmt.Errorf("send autocompressor status request: %w", err)
		}

		statusBytes, err := io.ReadAll(statusResp.Body)
		_ = statusResp.Body.Close()
		if err != nil {
			return nil, "", "", fmt.Errorf("read autocompressor status response: %w", err)
		}

		if statusResp.StatusCode < http.StatusOK || statusResp.StatusCode >= http.StatusMultipleChoices {
			return nil, "", "", fmt.Errorf(
				"autocompressor status failed with status %d: %s: %w",
				statusResp.StatusCode,
				strings.TrimSpace(string(statusBytes)),
				os.ErrInvalid,
			)
		}

		var statusData autocompressorStatusResponse
		err = json.Unmarshal(statusBytes, &statusData)
		if err != nil {
			return nil, "", "", fmt.Errorf("decode autocompressor status response: %w", err)
		}

		if statusData.Status.Ended {
			if statusData.Status.Error != nil && statusData.Status.Error != false {
				return nil, "", "", fmt.Errorf("autocompressor job failed: %v: %w", statusData.Status.Error, os.ErrInvalid)
			}

			return client.downloadCompressedVideo(ctx, downloadURL, filename)
		}
	}

	return nil, "", "", fmt.Errorf("autocompressor job timed out: %w", os.ErrDeadlineExceeded)
}

func (client facebookClient) downloadCompressedVideo(
	ctx context.Context,
	downloadURL string,
	originalFilename string,
) ([]byte, string, string, error) {
	dlReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		downloadURL,
		nil,
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("create autocompressor download request: %w", err)
	}

	dlReq.Header.Set("User-Agent", facebookGetMyFBDownloadUserAgent)

	dlResp, err := client.httpClient.Do(dlReq)
	if err != nil {
		return nil, "", "", fmt.Errorf("send autocompressor download request: %w", err)
	}
	defer func() {
		_ = dlResp.Body.Close()
	}()

	dlBytes, err := io.ReadAll(dlResp.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read autocompressor download response: %w", err)
	}

	if dlResp.StatusCode < http.StatusOK || dlResp.StatusCode >= http.StatusMultipleChoices {
		return nil, "", "", fmt.Errorf(
			"autocompressor download failed with status %d: %s: %w",
			dlResp.StatusCode,
			strings.TrimSpace(string(dlBytes)),
			os.ErrInvalid,
		)
	}

	if len(dlBytes) == 0 {
		return nil, "", "", fmt.Errorf("empty autocompressor download response: %w", os.ErrInvalid)
	}

	mimeType := normalizedFacebookMIMEType(dlResp.Header.Get("Content-Type"))
	filename := originalFilename
	contentDisposition := dlResp.Header.Get("Content-Disposition")
	if strings.TrimSpace(contentDisposition) != "" {
		_, params, err := mime.ParseMediaType(contentDisposition)
		if err == nil {
			dispositionFilename := strings.TrimSpace(params["filename"])
			if dispositionFilename != "" {
				filename = dispositionFilename
			}
		}
	}
	if strings.TrimSpace(filename) == "" {
		filename = facebookDefaultFilename
	}

	return dlBytes, mimeType, filename, nil
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

	httpRequest.Header.Set("User-Agent", facebookGetMyFBDownloadUserAgent)

	referer, origin := facebookGetMyFBDownloadHeaders(client.getMyFBProcessURL)
	if referer != "" {
		httpRequest.Header.Set("Referer", referer)
	}

	if origin != "" {
		httpRequest.Header.Set("Origin", origin)
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

	if len(videoBytes) == 0 {
		return nil, "", "", fmt.Errorf("empty facebook video response: %w", os.ErrInvalid)
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
	for _, segment := range slices.Backward(segments) {
		segment = strings.TrimSpace(segment)
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

func (instance *bot) replyWithFacebookVideos(
	ctx context.Context,
	message *discordgo.Message,
) {
	if instance == nil || instance.session == nil || instance.facebook == nil || message == nil {
		return
	}

	facebookURLs := extractFacebookURLs(message.Content)
	if len(facebookURLs) == 0 {
		return
	}

	stopTyping := instance.startTyping(ctx, message.ChannelID)
	defer stopTyping()

	videoContents, _ := fetchDownloadedVideos(
		ctx,
		facebookURLs,
		func(fetchCtx context.Context, rawURL string) (facebookVideoContent, error) {
			return instance.facebook.fetch(fetchCtx, rawURL)
		},
		"fetch facebook video reply",
		facebookWarningText,
	)
	if len(videoContents) == 0 {
		return
	}

	files, fallbackLinks, deliverableContents := facebookVideoReplyAttachments(videoContents)
	if len(files) == 0 && len(fallbackLinks) == 0 {
		return
	}

	send := newReplyMessage(message)
	send.Files = files
	send.Content = strings.Join(fallbackLinks, "\n")

	sentMessage, err := instance.session.ChannelMessageSendComplex(message.ChannelID, send)
	if err != nil {
		logWarn(
			"send facebook video reply",
			err,
			"channel_id",
			message.ChannelID,
			"message_id",
			message.ID,
		)

		return
	}

	instance.cacheDownloadedVideoReply(
		sentMessage,
		message,
		downloadedVideoMediaParts(deliverableContents),
	)
}

func facebookVideoReplyAttachments(
	videoContents []facebookVideoContent,
) ([]*discordgo.File, []string, []facebookVideoContent) {
	files := make([]*discordgo.File, 0, min(len(videoContents), facebookMaxReplyAttachments))
	fallbackLinks := make([]string, 0)
	deliverableContents := make([]facebookVideoContent, 0, len(videoContents))

	for _, videoContent := range videoContents {
		if len(files) >= facebookMaxReplyAttachments {
			fallbackLink := videoContent.downloadURL()
			if fallbackLink != "" {
				fallbackLinks = append(fallbackLinks, fallbackLink)
				deliverableContents = append(deliverableContents, videoContent)
			} else {
				logWarn(
					"facebook video exceeds attachment limit without download url",
					os.ErrInvalid,
					"url",
					videoContent.resolvedURL(),
				)
			}

			continue
		}

		file, fallbackLink := facebookVideoMediaFile(videoContent)
		if file != nil {
			files = append(files, file)
			deliverableContents = append(deliverableContents, videoContent)
		}

		if fallbackLink != "" {
			fallbackLinks = append(fallbackLinks, fallbackLink)
			deliverableContents = append(deliverableContents, videoContent)
		}

		if file == nil && fallbackLink == "" {
			logWarn(
				"facebook video has no uploadable bytes or download url",
				os.ErrInvalid,
				"url",
				videoContent.resolvedURL(),
			)
		}
	}

	return files, fallbackLinks, deliverableContents
}

func facebookVideoMediaFile(videoContent facebookVideoContent) (*discordgo.File, string) {
	part := videoContent.mediaPart()

	videoBytes, ok := part[contentFieldBytes].([]byte)
	if !ok || len(videoBytes) == 0 {
		return nil, videoContent.downloadURL()
	}

	if int64(len(videoBytes)) > facebookMaxUploadBytes {
		return nil, videoContent.downloadURL()
	}

	filename, _ := part[contentFieldFilename].(string)
	if strings.TrimSpace(filename) == "" {
		filename = facebookDefaultFilename
	}

	mimeType, _ := part[contentFieldMIMEType].(string)
	if strings.TrimSpace(mimeType) == "" {
		mimeType = facebookDefaultMIMEType
	}

	return &discordgo.File{
		Name:        filename,
		ContentType: mimeType,
		Reader:      bytes.NewReader(videoBytes),
	}, ""
}

func (content facebookVideoContent) downloadURL() string {
	return strings.TrimSpace(content.DownloadURL)
}
