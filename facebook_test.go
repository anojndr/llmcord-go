package main

import (
	"context"
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

type stubFacebookContentClient struct {
	mu      sync.Mutex
	calls   []string
	fetchFn func(context.Context, string) (facebookVideoContent, error)
}

func (client *stubFacebookContentClient) fetch(
	ctx context.Context,
	rawURL string,
) (facebookVideoContent, error) {
	client.mu.Lock()
	client.calls = append(client.calls, rawURL)
	client.mu.Unlock()

	return client.fetchFn(ctx, rawURL)
}

func newStubFacebookContentClient(
	fetchFn func(context.Context, string) (facebookVideoContent, error),
) *stubFacebookContentClient {
	client := new(stubFacebookContentClient)
	client.fetchFn = fetchFn

	return client
}

type stubFacebookScraper struct {
	postFn func(url string, contentType string, body io.Reader) (*http.Response, error)
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
	getMyFBProcessBody    string
	getMyFBResponseHeader http.Header
	downloads             map[string]facebookTestDownloadResponse
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
		case "/process":
			serveFacebookGetMyFBProcess(t, writer, request, server.URL, config)
		default:
			serveFacebookDownload(t, writer, request, config.downloads)
		}
	}))

	return server
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
		getMyFBProcessURL: server.URL + "/process",
	}
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

func TestFacebookClientFetchDownloadsBestGetMyFBVideo(t *testing.T) {
	t.Parallel()

	submittedURL := ""
	submittedLocale := ""

	server := newFacebookTestServer(t, facebookTestServerConfig{
		getMyFBProcessBody: facebookGetMyFBSearchFragment("SERVER_URL"),
		getMyFBResponseHeader: http.Header{
			"Hx-Trigger": []string{"resultsuccess"},
		},
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
			"/thumbnail.jpg": {
				body:               "ignored",
				contentType:        "image/jpeg",
				contentDisposition: "",
			},
		},
		assertGetMyFB: func(formValues url.Values) {
			submittedURL = formValues.Get("id")
			submittedLocale = formValues.Get("locale")
		},
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	result, err := client.fetch(context.Background(), testFacebookURL)
	if err != nil {
		t.Fatalf("fetch facebook content: %v", err)
	}

	if submittedURL != testFacebookURL {
		t.Fatalf("unexpected submitted url: %q", submittedURL)
	}

	if submittedLocale != facebookGetMyFBLocale {
		t.Fatalf("unexpected submitted locale: %q", submittedLocale)
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

func TestFacebookClientFetchUsesSourceURLWhenContentDispositionIsMissing(t *testing.T) {
	t.Parallel()

	server := newFacebookTestServer(t, facebookTestServerConfig{
		getMyFBProcessBody: facebookGetMyFBSearchFragment("SERVER_URL"),
		getMyFBResponseHeader: http.Header{
			"Hx-Trigger": []string{"resultsuccess"},
		},
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
			"/thumbnail.jpg": {
				body:               "ignored",
				contentType:        "image/jpeg",
				contentDisposition: "",
			},
		},
		assertGetMyFB: nil,
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	result, err := client.fetch(
		context.Background(),
		"https://fb.watch/vhalCYi2ib/",
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

func TestFacebookClientFetchReturnsGetMyFBErrorWithoutDownloads(t *testing.T) {
	t.Parallel()

	server := newFacebookTestServer(t, facebookTestServerConfig{
		getMyFBProcessBody: `<div class="result-error">Private video</div>`,
		getMyFBResponseHeader: http.Header{
			"Hx-Trigger": []string{"resulterror"},
		},
		downloads:     map[string]facebookTestDownloadResponse{},
		assertGetMyFB: nil,
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	_, err := client.fetch(context.Background(), testFacebookURL)
	if err == nil {
		t.Fatal("expected fetch facebook content to fail")
	}

	if !strings.Contains(err.Error(), "Private video") {
		t.Fatalf("unexpected error: %v", err)
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
		) (facebookVideoContent, error) {
			if rawURL != testFacebookURL {
				t.Fatalf("unexpected raw url: %q", rawURL)
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
		) (facebookVideoContent, error) {
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
			SearchMetadata:     nil,
			ResponseImages:     nil,
		})
	})

	instance := newFacebookTestBot(
		newStubFacebookContentClient(func(
			_ context.Context,
			_ string,
		) (facebookVideoContent, error) {
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
		) (facebookVideoContent, error) {
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
