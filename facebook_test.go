package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const testFacebookURL = "https://www.facebook.com/reel/823513456342882?mibextid=rS40aB7S9Ucbxw6v"

const (
	testFacebookSearchExp   = "1774158038"
	testFacebookSearchToken = "search-token"
)

type stubFacebookContentClient struct {
	mu      sync.Mutex
	calls   []string
	fetchFn func(context.Context, string, facebookExtractorConfig) (facebookVideoContent, error)
}

func (client *stubFacebookContentClient) fetch(
	ctx context.Context,
	rawURL string,
	extractorConfig facebookExtractorConfig,
) (facebookVideoContent, error) {
	client.mu.Lock()
	client.calls = append(client.calls, rawURL)
	client.mu.Unlock()

	return client.fetchFn(ctx, rawURL, extractorConfig)
}

func newStubFacebookContentClient(
	fetchFn func(context.Context, string, facebookExtractorConfig) (facebookVideoContent, error),
) *stubFacebookContentClient {
	client := new(stubFacebookContentClient)
	client.fetchFn = fetchFn

	return client
}

type stubFacebookScraper struct {
	getFn  func(url string) (*http.Response, error)
	postFn func(url string, contentType string, body io.Reader) (*http.Response, error)
}

func (scraper stubFacebookScraper) Get(requestURL string) (*http.Response, error) {
	return scraper.getFn(requestURL)
}

func (scraper stubFacebookScraper) Post(
	requestURL string,
	contentType string,
	body io.Reader,
) (*http.Response, error) {
	return scraper.postFn(requestURL, contentType, body)
}

func newFacebookTestBot(
	facebook facebookFetcher,
	chatCompletions chatCompletionStreamer,
) *bot {
	instance := new(bot)
	instance.facebook = facebook
	instance.chatCompletions = chatCompletions

	return instance
}

type facebookTestDownloadResponse struct {
	body               string
	contentType        string
	contentDisposition string
}

type facebookTestServerConfig struct {
	searchFragment        string
	searchResponse        facebookSearchResponse
	getMyFBProcessBody    string
	getMyFBResponseHeader http.Header
	downloads             map[string]facebookTestDownloadResponse
	assertSearch          func(url.Values)
	assertGetMyFB         func(url.Values)
}

func newFacebookTestServer(
	t *testing.T,
	config facebookTestServerConfig,
) *httptest.Server {
	t.Helper()

	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/en":
			serveFacebookSearchPage(t, writer, request)
		case "/api/ajaxSearch":
			serveFacebookSearch(t, writer, request, server.URL, config)
		case "/process":
			serveFacebookGetMyFBProcess(t, writer, request, server.URL, config)
		default:
			serveFacebookDownload(t, writer, request, config.downloads)
		}
	}))

	return server
}

func serveFacebookSearchPage(
	t *testing.T,
	writer http.ResponseWriter,
	request *http.Request,
) {
	t.Helper()

	if request.Method != http.MethodGet {
		t.Fatalf("unexpected request method: %s", request.Method)
	}

	_, _ = writer.Write([]byte(strings.Join([]string{
		`<html><body>`,
		`<script>`,
		`var k_exp="` + testFacebookSearchExp + `";`,
		`var k_token="` + testFacebookSearchToken + `";`,
		`</script>`,
		`</body></html>`,
	}, "")))
}

func serveFacebookSearch(
	t *testing.T,
	writer http.ResponseWriter,
	request *http.Request,
	serverURL string,
	config facebookTestServerConfig,
) {
	t.Helper()

	if request.Method != http.MethodPost {
		t.Fatalf("unexpected request method: %s", request.Method)
	}

	request.Body = http.MaxBytesReader(writer, request.Body, 4096)

	err := request.ParseForm()
	if err != nil {
		t.Fatalf("parse form: %v", err)
	}

	if config.assertSearch != nil {
		config.assertSearch(request.PostForm)
	}

	if !strings.HasPrefix(
		request.Header.Get("Content-Type"),
		"application/x-www-form-urlencoded",
	) {
		t.Fatalf("unexpected content type: %q", request.Header.Get("Content-Type"))
	}

	writer.Header().Set("Content-Type", "application/json; charset=utf-8")

	searchResponse := config.searchResponse
	if strings.TrimSpace(searchResponse.Status) == "" &&
		strings.TrimSpace(searchResponse.Data) == "" &&
		strings.TrimSpace(searchResponse.ErrorMessage) == "" {
		searchResponse = facebookSearchResponse{
			Status:       "ok",
			Data:         strings.ReplaceAll(config.searchFragment, "SERVER_URL", serverURL),
			ErrorMessage: "",
		}
	} else {
		searchResponse.Data = strings.ReplaceAll(searchResponse.Data, "SERVER_URL", serverURL)
	}

	err = json.NewEncoder(writer).Encode(searchResponse)
	if err != nil {
		t.Fatalf("encode search response: %v", err)
	}
}

func serveFacebookGetMyFBProcess(
	t *testing.T,
	writer http.ResponseWriter,
	request *http.Request,
	serverURL string,
	config facebookTestServerConfig,
) {
	t.Helper()

	if request.Method != http.MethodPost {
		t.Fatalf("unexpected request method: %s", request.Method)
	}

	request.Body = http.MaxBytesReader(writer, request.Body, 4096)

	err := request.ParseForm()
	if err != nil {
		t.Fatalf("parse getmyfb form: %v", err)
	}

	if config.assertGetMyFB != nil {
		config.assertGetMyFB(request.PostForm)
	}

	if !strings.HasPrefix(
		request.Header.Get("Content-Type"),
		"application/x-www-form-urlencoded",
	) {
		t.Fatalf("unexpected getmyfb content type: %q", request.Header.Get("Content-Type"))
	}

	for key, values := range config.getMyFBResponseHeader {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}

	_, _ = writer.Write([]byte(strings.ReplaceAll(config.getMyFBProcessBody, "SERVER_URL", serverURL)))
}

func serveFacebookDownload(
	t *testing.T,
	writer http.ResponseWriter,
	request *http.Request,
	downloads map[string]facebookTestDownloadResponse,
) {
	t.Helper()

	downloadResponse, ok := downloads[request.URL.Path]
	if !ok {
		t.Fatalf("unexpected path: %s", request.URL.Path)
	}

	writer.Header().Set("Content-Type", downloadResponse.contentType)

	if downloadResponse.contentDisposition != "" {
		writer.Header().Set(
			"Content-Disposition",
			downloadResponse.contentDisposition,
		)
	}

	_, _ = writer.Write([]byte(downloadResponse.body))
}

func newTestFacebookClient(server *httptest.Server) facebookClient {
	return facebookClient{
		httpClient: server.Client(),
		scraper: stubFacebookScraper{
			getFn: func(requestURL string) (*http.Response, error) {
				httpRequest, err := http.NewRequestWithContext(
					context.Background(),
					http.MethodGet,
					requestURL,
					nil,
				)
				if err != nil {
					return nil, fmt.Errorf("create facebook scraper request: %w", err)
				}

				return server.Client().Do(httpRequest)
			},
			postFn: func(
				requestURL string,
				contentType string,
				body io.Reader,
			) (*http.Response, error) {
				httpRequest, err := http.NewRequestWithContext(
					context.Background(),
					http.MethodPost,
					requestURL,
					body,
				)
				if err != nil {
					return nil, fmt.Errorf("create facebook scraper request: %w", err)
				}

				httpRequest.Header.Set("Content-Type", contentType)

				return server.Client().Do(httpRequest)
			},
		},
		pageURL:           server.URL + "/en",
		searchURL:         server.URL + "/api/ajaxSearch",
		getMyFBProcessURL: server.URL + "/process",
	}
}

func facebookDirectSearchFragment(serverURL string) string {
	return strings.Join([]string{
		`<div id="fbdownloader"><table><tbody>`,
		`<tr><td class="video-quality">720p (HD)</td><td>No</td><td>`,
		`<a class="download-link-fb" href="` + serverURL + `/downloads/video-hd.mp4">Download</a>`,
		`</td></tr>`,
		`<tr><td class="video-quality">360p (SD)</td><td>No</td><td>`,
		`<a class="download-link-fb" href="` + serverURL + `/downloads/video-sd.mp4">Download</a>`,
		`</td></tr>`,
		`</tbody></table></div>`,
	}, "")
}

func facebookRenderedSearchFragment(serverURL string) string {
	return strings.Join([]string{
		`<div id="fbdownloader"><table><tbody>`,
		`<tr><td class="video-quality">720p (HD)</td><td>No</td><td>`,
		`<a class="download-link-fb" href="` + serverURL + `/downloads/video-hd.mp4">Download</a>`,
		`</td></tr>`,
		`<tr><td class="video-quality">1080p</td><td>Yes</td><td>`,
		`<button data-videourl="https://example.com/video-only.mp4" `,
		`data-videotype="video/mp4" data-videocodec="av01.0.08M.08.0.111" `,
		`data-fquality="1080p">Render</button>`,
		`</td></tr>`,
		`</tbody></table></div>`,
	}, "")
}

func facebookGetMyFBSearchFragment(serverURL string) string {
	return strings.Join([]string{
		`<section class="results"><div class="container">`,
		`<figure class="results-item"><div class="results-item-image-wrapper">`,
		`<img class="results-item-image" src="` + serverURL + `/thumbnail.jpg" alt="Video thumbnail">`,
		`</div><figcaption class="results-item-text">Preview</figcaption></figure>`,
		`<div class="results-download"><ul class="results-list">`,
		`<li class="results-list-item">720p(HD)`,
		`<a href="/downloads/video-hd.mp4" class="bxmfunk-button ripple-btn hd-button">Download</a></li>`,
		`<li class="results-list-item">360p(SD)`,
		`<a href="/downloads/video-sd.mp4" class="bxmfunk-button ripple-btn sd-button">Download</a></li>`,
		`<li class="results-list-item">Mp3`,
		`<a href="/downloads/video-hd.mp4" data-id="123" class="mp3 bxmfunk-button ripple-btn sd-button">Download</a></li>`,
		`</ul></div></div></section>`,
	}, "")
}

func testFacebookVideoContent() facebookVideoContent {
	return facebookVideoContent{
		ResolvedURL: testFacebookURL,
		DownloadURL: "https://example.com/video.mp4",
		MediaPart: contentPart{
			"type":               contentTypeVideoData,
			contentFieldBytes:    []byte(testVideoBody),
			contentFieldMIMEType: testVideoMIMEType,
			contentFieldFilename: "clip.mp4",
		},
	}
}

func testFacebookConversationWithImage() []chatMessage {
	return []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": "<@123>: summarize " + testFacebookURL},
				{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
			},
		},
	}
}

func testFacebookExtractorConfig() facebookExtractorConfig {
	return facebookExtractorConfig{
		PrimaryProvider:  facebookExtractorProviderKindFDownloader,
		FallbackProvider: facebookExtractorProviderKindGetMyFB,
	}
}

func TestExtractFacebookURLsNormalizesAndDeduplicates(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"watch https://fb.watch/vhalCYi2ib/",
		"and https://fb.watch/vhalCYi2ib/,",
		"plus www.facebook.com/reel/823513456342882?mibextid=rS40aB7S9Ucbxw6v#watch",
	}, " ")

	urls := extractFacebookURLs(text)

	expected := []string{
		"https://fb.watch/vhalCYi2ib/",
		testFacebookURL,
	}

	if len(urls) != len(expected) {
		t.Fatalf("unexpected url count: got %d want %d (%#v)", len(urls), len(expected), urls)
	}

	for index, expectedURL := range expected {
		if urls[index] != expectedURL {
			t.Fatalf("unexpected url at index %d: got %q want %q", index, urls[index], expectedURL)
		}
	}
}

func TestExtractFacebookURLsIgnoresURLsInAugmentedPromptSections(t *testing.T) {
	t.Parallel()

	text := augmentedUserPrompt{
		RepliedMessage:   "",
		UserQuery:        "<@123>: summarize this video",
		YouTubeContent:   "Mirror: " + testFacebookURL,
		RedditContent:    "Mirror: https://fb.watch/vhalCYi2ib/",
		WebsiteContent:   "Source: https://www.facebook.com/watch/?v=823513456342882",
		DocumentContent:  "Doc URL: https://www.facebook.com/reel/1111111111111111",
		VisualSearch:     "Site match: https://www.facebook.com/share/v/19akxExample/",
		WebSearchResults: "1. https://www.facebook.com/reel/923513456342883",
	}.render()

	urls := extractFacebookURLs(text)
	if len(urls) != 0 {
		t.Fatalf("unexpected urls: %#v", urls)
	}
}

func TestFacebookClientFetchDownloadsBestDirectVideo(t *testing.T) {
	t.Parallel()

	submittedURL := ""

	server := newFacebookTestServer(t, facebookTestServerConfig{
		searchFragment: facebookDirectSearchFragment("SERVER_URL"),
		searchResponse: facebookSearchResponse{
			Status:       "",
			Data:         "",
			ErrorMessage: "",
		},
		getMyFBProcessBody:    "",
		getMyFBResponseHeader: nil,
		downloads: map[string]facebookTestDownloadResponse{
			"/downloads/video-hd.mp4": {
				body:               testVideoBody,
				contentType:        "video/mp4; charset=utf-8",
				contentDisposition: `attachment; filename="resolved.mp4"`,
			},
			"/downloads/video-sd.mp4": {
				body:               "sd-video",
				contentType:        "video/mp4",
				contentDisposition: "",
			},
		},
		assertSearch: func(formValues url.Values) {
			submittedURL = formValues.Get("q")

			if formValues.Get("k_exp") != testFacebookSearchExp {
				t.Fatalf("unexpected k_exp: %q", formValues.Get("k_exp"))
			}

			if formValues.Get("k_token") != testFacebookSearchToken {
				t.Fatalf("unexpected k_token: %q", formValues.Get("k_token"))
			}

			if formValues.Get("lang") != facebookSearchLanguage {
				t.Fatalf("unexpected lang: %q", formValues.Get("lang"))
			}

			if formValues.Get("web") != "127.0.0.1" {
				t.Fatalf("unexpected web host: %q", formValues.Get("web"))
			}
		},
		assertGetMyFB: nil,
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	result, err := client.fetch(context.Background(), testFacebookURL, testFacebookExtractorConfig())
	if err != nil {
		t.Fatalf("fetch facebook content: %v", err)
	}

	if submittedURL != testFacebookURL {
		t.Fatalf("unexpected submitted url: %q", submittedURL)
	}

	if result.ResolvedURL != testFacebookURL {
		t.Fatalf("unexpected resolved url: %q", result.ResolvedURL)
	}

	if result.DownloadURL != server.URL+"/downloads/video-hd.mp4" {
		t.Fatalf("unexpected download url: %q", result.DownloadURL)
	}

	if result.MediaPart["type"] != contentTypeVideoData {
		t.Fatalf("unexpected media part type: %#v", result.MediaPart)
	}

	if string(mediaPartBytes(t, result.MediaPart)) != testVideoBody {
		t.Fatalf("unexpected video bytes: %#v", result.MediaPart[contentFieldBytes])
	}

	if result.MediaPart[contentFieldMIMEType] != facebookDefaultMIMEType {
		t.Fatalf("unexpected mime type: %#v", result.MediaPart)
	}

	if result.MediaPart[contentFieldFilename] != "resolved.mp4" {
		t.Fatalf("unexpected filename: %#v", result.MediaPart)
	}
}

func TestFacebookClientFetchUsesDirectVideoWhenHigherQualityRequiresProcessing(t *testing.T) {
	t.Parallel()

	server := newFacebookTestServer(t, facebookTestServerConfig{
		searchFragment: facebookRenderedSearchFragment("SERVER_URL"),
		searchResponse: facebookSearchResponse{
			Status:       "",
			Data:         "",
			ErrorMessage: "",
		},
		getMyFBProcessBody:    "",
		getMyFBResponseHeader: nil,
		downloads: map[string]facebookTestDownloadResponse{
			"/downloads/video-hd.mp4": {
				body:               testVideoBody,
				contentType:        "video/mp4",
				contentDisposition: `attachment; filename="direct.mp4"`,
			},
		},
		assertSearch:  nil,
		assertGetMyFB: nil,
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	result, err := client.fetch(context.Background(), testFacebookURL, testFacebookExtractorConfig())
	if err != nil {
		t.Fatalf("fetch facebook content: %v", err)
	}

	if result.DownloadURL != server.URL+"/downloads/video-hd.mp4" {
		t.Fatalf("unexpected download url: %q", result.DownloadURL)
	}

	if string(mediaPartBytes(t, result.MediaPart)) != testVideoBody {
		t.Fatalf("unexpected video bytes: %#v", result.MediaPart[contentFieldBytes])
	}

	if result.MediaPart[contentFieldFilename] != "direct.mp4" {
		t.Fatalf("unexpected filename: %#v", result.MediaPart)
	}
}

func TestFacebookClientFetchUsesSourceURLWhenContentDispositionIsMissing(t *testing.T) {
	t.Parallel()

	server := newFacebookTestServer(t, facebookTestServerConfig{
		searchFragment: facebookDirectSearchFragment("SERVER_URL"),
		searchResponse: facebookSearchResponse{
			Status:       "",
			Data:         "",
			ErrorMessage: "",
		},
		getMyFBProcessBody:    "",
		getMyFBResponseHeader: nil,
		downloads: map[string]facebookTestDownloadResponse{
			"/downloads/video-hd.mp4": {
				body:               testVideoBody,
				contentType:        "application/octet-stream",
				contentDisposition: "",
			},
			"/downloads/video-sd.mp4": {
				body:               "sd-video",
				contentType:        "video/mp4",
				contentDisposition: "",
			},
		},
		assertSearch:  nil,
		assertGetMyFB: nil,
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	result, err := client.fetch(
		context.Background(),
		"https://fb.watch/vhalCYi2ib/",
		testFacebookExtractorConfig(),
	)
	if err != nil {
		t.Fatalf("fetch facebook content: %v", err)
	}

	if result.MediaPart[contentFieldMIMEType] != facebookDefaultMIMEType {
		t.Fatalf("unexpected mime type: %#v", result.MediaPart)
	}

	if result.MediaPart[contentFieldFilename] != "facebook_vhalCYi2ib.mp4" {
		t.Fatalf("unexpected filename: %#v", result.MediaPart)
	}
}

func TestFacebookClientFetchFallsBackToGetMyFBWhenFDownloaderFails(t *testing.T) {
	t.Parallel()

	server := newFacebookTestServer(t, facebookTestServerConfig{
		searchFragment: "",
		searchResponse: facebookSearchResponse{
			Status:       "error",
			Data:         "",
			ErrorMessage: "primary failed",
		},
		getMyFBProcessBody: facebookGetMyFBSearchFragment("SERVER_URL"),
		getMyFBResponseHeader: http.Header{
			"Hx-Trigger": []string{"resultsuccess"},
		},
		downloads: map[string]facebookTestDownloadResponse{
			"/downloads/video-hd.mp4": {
				body:               testVideoBody,
				contentType:        "video/mp4",
				contentDisposition: `attachment; filename="fallback.mp4"`,
			},
			"/downloads/video-sd.mp4": {
				body:               "sd-video",
				contentType:        "video/mp4",
				contentDisposition: `attachment; filename="fallback-sd.mp4"`,
			},
			"/thumbnail.jpg": {
				body:               "ignored",
				contentType:        "image/jpeg",
				contentDisposition: "",
			},
		},
		assertSearch: nil,
		assertGetMyFB: func(formValues url.Values) {
			if formValues.Get("id") != testFacebookURL {
				t.Fatalf("unexpected getmyfb id: %q", formValues.Get("id"))
			}

			if formValues.Get("locale") != facebookGetMyFBLocale {
				t.Fatalf("unexpected getmyfb locale: %q", formValues.Get("locale"))
			}
		},
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	result, err := client.fetch(context.Background(), testFacebookURL, testFacebookExtractorConfig())
	if err != nil {
		t.Fatalf("fetch facebook content with fallback: %v", err)
	}

	if result.DownloadURL != server.URL+"/downloads/video-hd.mp4" {
		t.Fatalf("unexpected fallback download url: %q", result.DownloadURL)
	}

	if string(mediaPartBytes(t, result.MediaPart)) != testVideoBody {
		t.Fatalf("unexpected fallback video bytes: %#v", result.MediaPart[contentFieldBytes])
	}

	if result.MediaPart[contentFieldFilename] != "fallback.mp4" {
		t.Fatalf("unexpected fallback filename: %#v", result.MediaPart)
	}
}

func TestFacebookClientFetchUsesConfiguredGetMyFBPrimaryProvider(t *testing.T) {
	t.Parallel()

	server := newFacebookTestServer(t, facebookTestServerConfig{
		searchFragment: "",
		searchResponse: facebookSearchResponse{
			Status:       "ok",
			Data:         "",
			ErrorMessage: "",
		},
		getMyFBProcessBody: facebookGetMyFBSearchFragment("SERVER_URL"),
		getMyFBResponseHeader: http.Header{
			"Hx-Trigger": []string{"resultsuccess"},
		},
		downloads: map[string]facebookTestDownloadResponse{
			"/downloads/video-hd.mp4": {
				body:               "getmyfb-video",
				contentType:        "video/mp4",
				contentDisposition: `attachment; filename="getmyfb.mp4"`,
			},
			"/downloads/video-sd.mp4": {
				body:               "getmyfb-sd-video",
				contentType:        "video/mp4",
				contentDisposition: `attachment; filename="getmyfb-sd.mp4"`,
			},
			"/thumbnail.jpg": {
				body:               "ignored",
				contentType:        "image/jpeg",
				contentDisposition: "",
			},
		},
		assertSearch: func(url.Values) {
			t.Fatal("unexpected fdownloader request")
		},
		assertGetMyFB: func(formValues url.Values) {
			if formValues.Get("id") != testFacebookURL {
				t.Fatalf("unexpected getmyfb id: %q", formValues.Get("id"))
			}
		},
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	result, err := client.fetch(context.Background(), testFacebookURL, facebookExtractorConfig{
		PrimaryProvider:  facebookExtractorProviderKindGetMyFB,
		FallbackProvider: facebookExtractorProviderKindFDownloader,
	})
	if err != nil {
		t.Fatalf("fetch facebook content with getmyfb primary: %v", err)
	}

	if result.DownloadURL != server.URL+"/downloads/video-hd.mp4" {
		t.Fatalf("unexpected getmyfb primary download url: %q", result.DownloadURL)
	}

	if string(mediaPartBytes(t, result.MediaPart)) != "getmyfb-video" {
		t.Fatalf("unexpected getmyfb primary video bytes: %#v", result.MediaPart[contentFieldBytes])
	}
}

func TestMaybeAugmentConversationWithFacebookAppendsVideoPartsAndAnalysesForNonGeminiSearchDecider(t *testing.T) {
	t.Parallel()

	expectedAnalysis := []string{
		"Video description per timestamp:\n\n0s to 10s: somebody waves",
	}
	chatClient, analysisCallCount := newGeminiVideoAnalysisChatClient(t, expectedAnalysis[0])

	instance := newFacebookTestBot(
		newStubFacebookContentClient(func(
			_ context.Context,
			rawURL string,
			extractorConfig facebookExtractorConfig,
		) (facebookVideoContent, error) {
			if rawURL != testFacebookURL {
				t.Fatalf("unexpected raw url: %q", rawURL)
			}

			if extractorConfig != testFacebookExtractorConfig() {
				t.Fatalf("unexpected facebook extractor config: %#v", extractorConfig)
			}

			return testFacebookVideoContent(), nil
		}),
		chatClient,
	)

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithFacebook(
		context.Background(),
		testMediaAnalysisConfig(),
		testMediaAnalysisModel,
		testFacebookConversationWithImage(),
		"<@123>: summarize "+testFacebookURL,
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if *analysisCallCount != 1 {
		t.Fatalf("unexpected gemini analysis call count: %d", *analysisCallCount)
	}

	expectedText := expectedMediaAnalysisUserText(
		"<@123>: summarize "+testFacebookURL,
		expectedAnalysis,
	)

	assertAugmentedVideoParts(t, augmentedConversation, expectedText)
	assertSearchDeciderTextContent(
		t,
		augmentedConversation,
		testMediaAnalysisConfig(),
		"openai/decider-model",
		expectedText,
	)
}

func TestMaybeAugmentConversationWithFacebookSkipsAnalysesForGeminiSearchDecider(t *testing.T) {
	t.Parallel()

	chatClient := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		t.Fatal("unexpected gemini analysis request")

		return nil
	})

	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.SearchDeciderModel = testMediaAnalysisModel

	instance := newFacebookTestBot(
		newStubFacebookContentClient(func(
			_ context.Context,
			_ string,
			extractorConfig facebookExtractorConfig,
		) (facebookVideoContent, error) {
			if extractorConfig != testFacebookExtractorConfig() {
				t.Fatalf("unexpected facebook extractor config: %#v", extractorConfig)
			}

			return testFacebookVideoContent(), nil
		}),
		chatClient,
	)
	instance.currentSearchDeciderModel = testMediaAnalysisModel

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithFacebook(
		context.Background(),
		loadedConfig,
		testMediaAnalysisModel,
		testFacebookConversationWithImage(),
		"<@123>: summarize "+testFacebookURL,
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	assertAugmentedVideoParts(
		t,
		augmentedConversation,
		"<@123>: summarize "+testFacebookURL,
	)
}

func TestMaybeAugmentConversationWithFacebookPreprocessesForNonGeminiModels(t *testing.T) {
	t.Parallel()

	expectedAnalysis := []string{
		"Video description per timestamp:\n\n0s to 10s: somebody waves",
	}

	callCount := 0
	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		assertGeminiMediaAnalysisRequest(
			t,
			request,
			geminiVideoAnalysisPrompt,
			contentTypeVideoData,
		)

		callCount++

		return handle(streamDelta{
			Thinking:           "",
			Content:            expectedAnalysis[0],
			FinishReason:       finishReasonStop,
			Usage:              nil,
			ProviderResponseID: "",
		})
	})

	instance := newFacebookTestBot(
		newStubFacebookContentClient(func(
			_ context.Context,
			_ string,
			extractorConfig facebookExtractorConfig,
		) (facebookVideoContent, error) {
			if extractorConfig != testFacebookExtractorConfig() {
				t.Fatalf("unexpected facebook extractor config: %#v", extractorConfig)
			}

			return testFacebookVideoContent(), nil
		}),
		chatClient,
	)

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: "<@123>: summarize " + testFacebookURL,
		},
	}

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithFacebook(
		context.Background(),
		testMediaAnalysisConfig(),
		"openai/gpt-5",
		conversation,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if callCount != 1 {
		t.Fatalf("unexpected gemini analysis call count: %d", callCount)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	expectedText := expectedMediaAnalysisUserText(
		"<@123>: summarize "+testFacebookURL,
		expectedAnalysis,
	)
	if content != expectedText {
		t.Fatalf("unexpected augmented content: %q", content)
	}
}

func TestMaybeAugmentConversationWithFacebookWarnsWithoutGeminiPreprocessor(t *testing.T) {
	t.Parallel()

	instance := newFacebookTestBot(
		newStubFacebookContentClient(func(
			_ context.Context,
			_ string,
			extractorConfig facebookExtractorConfig,
		) (facebookVideoContent, error) {
			if extractorConfig != testFacebookExtractorConfig() {
				t.Fatalf("unexpected facebook extractor config: %#v", extractorConfig)
			}

			return testFacebookVideoContent(), nil
		}),
		nil,
	)

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: "<@123>: summarize " + testFacebookURL,
		},
	}

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithFacebook(
		context.Background(),
		testSearchConfig(),
		"openai/main-model",
		conversation,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	if len(warnings) != 1 || warnings[0] != facebookWarningText {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if content != "<@123>: summarize "+testFacebookURL {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestMaybeAugmentConversationWithFacebookIgnoresURLsOnlyPresentInDocumentContent(t *testing.T) {
	t.Parallel()

	instance := newFacebookTestBot(
		newStubFacebookContentClient(func(
			_ context.Context,
			rawURL string,
			_ facebookExtractorConfig,
		) (facebookVideoContent, error) {
			t.Fatalf("unexpected facebook fetch for %q", rawURL)

			return facebookVideoContent{
				ResolvedURL: "",
				DownloadURL: "",
				MediaPart:   nil,
			}, nil
		}),
		nil,
	)

	assertURLAugmentationIgnoresDocumentOnlyURLs(
		t,
		testFacebookURL,
		func(
			ctx context.Context,
			conversation []chatMessage,
			urlExtractionText string,
		) ([]chatMessage, []string, error) {
			return instance.maybeAugmentConversationWithFacebook(
				ctx,
				testSearchConfig(),
				"openai/main-model",
				conversation,
				urlExtractionText,
			)
		},
	)
}
