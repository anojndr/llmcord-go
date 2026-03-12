package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testSnaptikDownloadPath = "/abc2.php"
	testSnaptikLandingPath  = "/en2"
	testTikTokResolvedPath  = "/@mikemhan/video/7614735539660442893"
)

type stubTikTokContentClient struct {
	mu      sync.Mutex
	calls   []string
	fetchFn func(context.Context, string) (tiktokVideoContent, error)
}

func (client *stubTikTokContentClient) fetch(
	ctx context.Context,
	rawURL string,
) (tiktokVideoContent, error) {
	client.mu.Lock()
	client.calls = append(client.calls, rawURL)
	client.mu.Unlock()

	return client.fetchFn(ctx, rawURL)
}

func newStubTikTokContentClient(
	fetchFn func(context.Context, string) (tiktokVideoContent, error),
) *stubTikTokContentClient {
	client := new(stubTikTokContentClient)
	client.fetchFn = fetchFn

	return client
}

func newTikTokTestBot(
	tiktok tiktokContentClient,
	chatCompletions chatCompletionClient,
) *bot {
	instance := new(bot)
	instance.tiktok = tiktok
	instance.chatCompletions = chatCompletions

	return instance
}

func TestExtractTikTokURLsNormalizesAndDeduplicates(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"watch https://vt.tnktok.com/ZSuhvMpsr/",
		"and https://vt.tnktok.com/ZSuhvMpsr/,",
		"plus www.tiktok.com/@mikemhan/video/7614735539660442893!",
	}, " ")

	urls := extractTikTokURLs(text)

	expected := []string{
		"https://vt.tnktok.com/ZSuhvMpsr/",
		"https://www.tiktok.com/@mikemhan/video/7614735539660442893",
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

func TestDecodeSnaptikPackedScript(t *testing.T) {
	t.Parallel()

	expectedScript := `$("#download").innerHTML = "<a href=\"https://example.com/video.mp4\" ` +
		`class=\"button download-file\">Download Video</a>";`

	decodedScript, err := decodeSnaptikPackedScript(
		encodeSnaptikPackedScriptForTest(expectedScript),
	)
	if err != nil {
		t.Fatalf("decode snaptik packed script: %v", err)
	}

	if decodedScript != expectedScript {
		t.Fatalf("unexpected decoded script: %q", decodedScript)
	}
}

type directTikTokServerState struct {
	landingRequests int
	submittedURLs   []string
	submittedURLsMu sync.Mutex
}

func newDirectTikTokTestServer(
	t *testing.T,
	rawPath string,
	videoBody string,
	videoContentType string,
	contentDisposition string,
) (*httptest.Server, *directTikTokServerState) {
	t.Helper()

	state := new(directTikTokServerState)

	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case rawPath:
			http.Redirect(writer, request, testTikTokResolvedPath, http.StatusFound)
		case testTikTokResolvedPath:
			writer.WriteHeader(http.StatusOK)
		case testSnaptikLandingPath:
			state.landingRequests++
			_, _ = writer.Write([]byte(`<input name="token" value="test-token" type="hidden">`))
		case testSnaptikDownloadPath:
			request.Body = http.MaxBytesReader(writer, request.Body, 1024)

			err := request.ParseForm()
			if err != nil {
				t.Fatalf("parse form: %v", err)
			}

			state.submittedURLsMu.Lock()
			state.submittedURLs = append(state.submittedURLs, request.PostForm.Get("url"))
			state.submittedURLsMu.Unlock()

			if request.PostForm.Get("lang") != tikTokLanguage {
				t.Fatalf("unexpected lang: %q", request.PostForm.Get("lang"))
			}

			if request.PostForm.Get("token") != "test-token" {
				t.Fatalf("unexpected token: %q", request.PostForm.Get("token"))
			}

			downloadURL := server.URL + "/downloads/video.mp4"
			script := fmt.Sprintf(
				`$("#download").innerHTML = "<a href=\"%s\" class=\"button download-file\">Download Video</a>";`,
				downloadURL,
			)
			_, _ = writer.Write([]byte(encodeSnaptikPackedScriptForTest(script)))
		case "/downloads/video.mp4":
			writer.Header().Set("Content-Type", videoContentType)

			if contentDisposition != "" {
				writer.Header().Set("Content-Disposition", contentDisposition)
			}

			_, _ = writer.Write([]byte(videoBody))
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}
	}))

	return server, state
}

func newRenderedTikTokTestServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()

	taskCalls := 0

	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/slides":
			http.Redirect(writer, request, testTikTokResolvedPath, http.StatusFound)
		case testTikTokResolvedPath:
			writer.WriteHeader(http.StatusOK)
		case testSnaptikLandingPath:
			_, _ = writer.Write([]byte(`<input name="token" value="test-token" type="hidden">`))
		case testSnaptikDownloadPath:
			script := `$("#download").innerHTML = "` +
				`<button class=\"button btn-render\" data-token=\"render-token\">Render</button>";`
			_, _ = writer.Write([]byte(encodeSnaptikPackedScriptForTest(script)))
		case "/render.php":
			if request.URL.Query().Get("token") != "render-token" {
				t.Fatalf("unexpected render token: %q", request.URL.Query().Get("token"))
			}

			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"task_id":"task-123"}`))
		case "/task.php":
			if request.URL.Query().Get("token") != "task-123" {
				t.Fatalf("unexpected task token: %q", request.URL.Query().Get("token"))
			}

			taskCalls++

			writer.Header().Set("Content-Type", "application/json")

			if taskCalls == 1 {
				_, _ = writer.Write([]byte(`{"status":0,"progress":50}`))

				return
			}

			_, _ = writer.Write([]byte(
				`{"status":0,"progress":100,"download_url":"` + server.URL + `/downloads/rendered.mp4"}`,
			))
		case "/downloads/rendered.mp4":
			writer.Header().Set("Content-Type", "video/mp4")
			_, _ = writer.Write([]byte("rendered-video"))
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}
	}))

	return server, &taskCalls
}

func newTestTikTokClient(server *httptest.Server) tiktokClient {
	return tiktokClient{
		httpClient:  server.Client(),
		landingURL:  server.URL + testSnaptikLandingPath,
		downloadURL: server.URL + testSnaptikDownloadPath,
		renderURL:   server.URL + "/render.php",
		taskURL:     server.URL + "/task.php",
		userAgent:   youtubeUserAgent,
	}
}

func mediaPartBytes(t *testing.T, part contentPart) []byte {
	t.Helper()

	partBytes, ok := part[contentFieldBytes].([]byte)
	if !ok {
		t.Fatalf("unexpected media part bytes: %#v", part[contentFieldBytes])
	}

	return partBytes
}

func testTikTokVideoContent() tiktokVideoContent {
	return tiktokVideoContent{
		ResolvedURL: "https://www.tiktok.com/@mikemhan/video/7614735539660442893",
		DownloadURL: "https://example.com/video.mp4",
		MediaPart: contentPart{
			"type":               contentTypeVideoData,
			contentFieldBytes:    []byte("video-bytes"),
			contentFieldMIMEType: testVideoMIMEType,
			contentFieldFilename: "clip.mp4",
		},
	}
}

func testTikTokConversationWithImage() []chatMessage {
	return []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": "<@123>: summarize https://vt.tnktok.com/ZSuhvMpsr/"},
				{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
			},
		},
	}
}

func assertTikTokAugmentedParts(
	t *testing.T,
	conversation []chatMessage,
	expectedText string,
) []contentPart {
	t.Helper()

	parts, partsOK := conversation[0].Content.([]contentPart)
	if !partsOK {
		t.Fatalf("unexpected content type: %T", conversation[0].Content)
	}

	if len(parts) != 3 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	textValue, _ := parts[0]["text"].(string)
	if textValue != expectedText {
		t.Fatalf("unexpected text value: %q", textValue)
	}

	if parts[2]["type"] != contentTypeVideoData {
		t.Fatalf("expected appended video part: %#v", parts[2])
	}

	return parts
}

func assertSearchDeciderTextContent(
	t *testing.T,
	conversation []chatMessage,
	loadedConfig config,
	configuredModel string,
	expectedText string,
) {
	t.Helper()

	searchDeciderMessages, err := searchDeciderConversation(
		conversation,
		loadedConfig,
		configuredModel,
	)
	if err != nil {
		t.Fatalf("build search decider conversation: %v", err)
	}

	searchDeciderContent, contentOK := searchDeciderMessages[0].Content.(string)
	if !contentOK {
		t.Fatalf("unexpected search decider content type: %T", searchDeciderMessages[0].Content)
	}

	if searchDeciderContent != expectedText {
		t.Fatalf("unexpected search decider content: %q", searchDeciderContent)
	}
}

func newTikTokGeminiAnalysisChatClient(
	t *testing.T,
	expectedAnalysis string,
) (*stubChatCompletionClient, *int) {
	t.Helper()

	callCount := 0
	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		assertGeminiMediaAnalysisRequest(
			t,
			request,
			geminiVideoAnalysisPrompt,
			contentTypeVideoData,
		)

		callCount++

		return handle(streamDelta{
			Content:      expectedAnalysis,
			FinishReason: finishReasonStop,
		})
	})

	return chatClient, &callCount
}

func TestTikTokClientFetchDownloadsDirectVideo(t *testing.T) {
	t.Parallel()

	const (
		rawPath   = "/short"
		videoBody = "video-bytes"
	)

	server, state := newDirectTikTokTestServer(
		t,
		rawPath,
		videoBody,
		"video/mp4; charset=utf-8",
		`attachment; filename="resolved.mp4"`,
	)
	defer server.Close()

	client := newTestTikTokClient(server)

	result, err := client.fetch(context.Background(), server.URL+rawPath)
	if err != nil {
		t.Fatalf("fetch tiktok content: %v", err)
	}

	if result.ResolvedURL != server.URL+testTikTokResolvedPath {
		t.Fatalf("unexpected resolved url: %q", result.ResolvedURL)
	}

	if result.DownloadURL != server.URL+"/downloads/video.mp4" {
		t.Fatalf("unexpected download url: %q", result.DownloadURL)
	}

	if state.landingRequests != 1 {
		t.Fatalf("unexpected landing request count: %d", state.landingRequests)
	}

	if len(state.submittedURLs) != 1 || state.submittedURLs[0] != server.URL+testTikTokResolvedPath {
		t.Fatalf("unexpected submitted urls: %#v", state.submittedURLs)
	}

	if result.MediaPart["type"] != contentTypeVideoData {
		t.Fatalf("unexpected media part type: %#v", result.MediaPart)
	}

	if string(mediaPartBytes(t, result.MediaPart)) != videoBody {
		t.Fatalf("unexpected video bytes: %#v", result.MediaPart[contentFieldBytes])
	}

	if result.MediaPart[contentFieldMIMEType] != tikTokDefaultMIMEType {
		t.Fatalf("unexpected mime type: %#v", result.MediaPart)
	}

	if result.MediaPart[contentFieldFilename] != "resolved.mp4" {
		t.Fatalf("unexpected filename: %#v", result.MediaPart)
	}
}

func TestTikTokClientFetchRendersSlideshowsWhenNeeded(t *testing.T) {
	t.Parallel()

	server, taskCalls := newRenderedTikTokTestServer(t)
	defer server.Close()

	client := newTestTikTokClient(server)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := client.fetch(ctx, server.URL+"/slides")
	if err != nil {
		t.Fatalf("fetch tiktok slideshow: %v", err)
	}

	if string(mediaPartBytes(t, result.MediaPart)) != "rendered-video" {
		t.Fatalf("unexpected rendered bytes: %#v", result.MediaPart[contentFieldBytes])
	}

	if *taskCalls != 2 {
		t.Fatalf("unexpected task poll count: %d", *taskCalls)
	}
}

func TestMaybeAugmentConversationWithTikTokAppendsVideoPartsAndAnalysesForNonGeminiSearchDecider(t *testing.T) {
	t.Parallel()

	expectedAnalysis := []string{
		"Video description per timestamp:\n\n0s to 10s: somebody waves",
	}
	chatClient, analysisCallCount := newTikTokGeminiAnalysisChatClient(t, expectedAnalysis[0])

	instance := newTikTokTestBot(
		newStubTikTokContentClient(func(
			_ context.Context,
			rawURL string,
		) (tiktokVideoContent, error) {
			if rawURL != "https://vt.tnktok.com/ZSuhvMpsr/" {
				t.Fatalf("unexpected raw url: %q", rawURL)
			}

			return testTikTokVideoContent(), nil
		}),
		chatClient,
	)

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithTikTok(
		context.Background(),
		testMediaAnalysisConfig(),
		testMediaAnalysisModel,
		testTikTokConversationWithImage(),
		"<@123>: summarize https://vt.tnktok.com/ZSuhvMpsr/",
	)
	if err != nil {
		t.Fatalf("augment conversation with tiktok: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if *analysisCallCount != 1 {
		t.Fatalf("unexpected gemini analysis call count: %d", *analysisCallCount)
	}

	expectedText := expectedMediaAnalysisUserText(
		"<@123>: summarize https://vt.tnktok.com/ZSuhvMpsr/",
		expectedAnalysis,
	)

	assertTikTokAugmentedParts(t, augmentedConversation, expectedText)
	assertSearchDeciderTextContent(
		t,
		augmentedConversation,
		testMediaAnalysisConfig(),
		"openai/decider-model",
		expectedText,
	)
}

func TestMaybeAugmentConversationWithTikTokSkipsAnalysesForGeminiSearchDecider(t *testing.T) {
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

	instance := newTikTokTestBot(
		newStubTikTokContentClient(func(
			_ context.Context,
			_ string,
		) (tiktokVideoContent, error) {
			return testTikTokVideoContent(), nil
		}),
		chatClient,
	)
	instance.currentSearchDeciderModel = testMediaAnalysisModel

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithTikTok(
		context.Background(),
		loadedConfig,
		testMediaAnalysisModel,
		testTikTokConversationWithImage(),
		"<@123>: summarize https://vt.tnktok.com/ZSuhvMpsr/",
	)
	if err != nil {
		t.Fatalf("augment conversation with tiktok: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	assertTikTokAugmentedParts(
		t,
		augmentedConversation,
		"<@123>: summarize https://vt.tnktok.com/ZSuhvMpsr/",
	)
}

func TestMaybeAugmentConversationWithTikTokPreprocessesForNonGeminiModels(t *testing.T) {
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
			Content:      expectedAnalysis[0],
			FinishReason: finishReasonStop,
		})
	})

	instance := newTikTokTestBot(
		newStubTikTokContentClient(func(
			_ context.Context,
			_ string,
		) (tiktokVideoContent, error) {
			return tiktokVideoContent{
				ResolvedURL: "https://www.tiktok.com/@mikemhan/video/7614735539660442893",
				DownloadURL: "https://example.com/video.mp4",
				MediaPart: contentPart{
					"type":               contentTypeVideoData,
					contentFieldBytes:    []byte("video-bytes"),
					contentFieldMIMEType: testVideoMIMEType,
					contentFieldFilename: "clip.mp4",
				},
			}, nil
		}),
		chatClient,
	)

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: "<@123>: summarize https://vt.tnktok.com/ZSuhvMpsr/",
		},
	}

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithTikTok(
		context.Background(),
		testMediaAnalysisConfig(),
		"openai/gpt-5",
		conversation,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with tiktok: %v", err)
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
		"<@123>: summarize https://vt.tnktok.com/ZSuhvMpsr/",
		expectedAnalysis,
	)
	if content != expectedText {
		t.Fatalf("unexpected augmented content: %q", content)
	}
}

func TestMaybeAugmentConversationWithTikTokWarnsWithoutGeminiPreprocessor(t *testing.T) {
	t.Parallel()

	instance := newTikTokTestBot(
		newStubTikTokContentClient(func(
			_ context.Context,
			_ string,
		) (tiktokVideoContent, error) {
			return tiktokVideoContent{
				ResolvedURL: "https://www.tiktok.com/@mikemhan/video/7614735539660442893",
				DownloadURL: "https://example.com/video.mp4",
				MediaPart: contentPart{
					"type":               contentTypeVideoData,
					contentFieldBytes:    []byte("video-bytes"),
					contentFieldMIMEType: testVideoMIMEType,
					contentFieldFilename: "clip.mp4",
				},
			}, nil
		}),
		nil,
	)

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: "<@123>: summarize https://vt.tnktok.com/ZSuhvMpsr/",
		},
	}

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithTikTok(
		context.Background(),
		testSearchConfig(),
		"openai/main-model",
		conversation,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with tiktok: %v", err)
	}

	if len(warnings) != 1 || warnings[0] != tikTokWarningText {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if content != "<@123>: summarize https://vt.tnktok.com/ZSuhvMpsr/" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestMaybeAugmentConversationWithTikTokIgnoresURLsOnlyPresentInPDFContent(t *testing.T) {
	t.Parallel()

	instance := newTikTokTestBot(
		newStubTikTokContentClient(func(
			_ context.Context,
			rawURL string,
		) (tiktokVideoContent, error) {
			t.Fatalf("unexpected tiktok fetch for %q", rawURL)

			return tiktokVideoContent{
				ResolvedURL: "",
				DownloadURL: "",
				MediaPart:   nil,
			}, nil
		}),
		nil,
	)

	assertURLAugmentationIgnoresPDFOnlyURLs(
		t,
		"https://vt.tnktok.com/ZSuhvMpsr/",
		func(
			ctx context.Context,
			conversation []chatMessage,
			urlExtractionText string,
		) ([]chatMessage, []string, error) {
			return instance.maybeAugmentConversationWithTikTok(
				ctx,
				testSearchConfig(),
				"openai/main-model",
				conversation,
				urlExtractionText,
			)
		},
	)
}

func TestTikTokClientFetchUsesResolvedVideoIDWhenContentDispositionIsMissing(t *testing.T) {
	t.Parallel()

	server, _ := newDirectTikTokTestServer(
		t,
		"/short",
		"video-bytes",
		"video/mp4",
		"",
	)
	defer server.Close()

	client := newTestTikTokClient(server)

	result, err := client.fetch(context.Background(), server.URL+"/short")
	if err != nil {
		t.Fatalf("fetch tiktok content: %v", err)
	}

	if result.MediaPart[contentFieldFilename] != "tiktok_7614735539660442893.mp4" {
		t.Fatalf("unexpected filename: %#v", result.MediaPart)
	}
}

func TestTikTokClientFetchNormalizesOctetStreamToMP4(t *testing.T) {
	t.Parallel()

	server, _ := newDirectTikTokTestServer(
		t,
		"/short",
		"video-bytes",
		"application/octet-stream",
		`attachment; filename="resolved.mp4"`,
	)
	defer server.Close()

	client := newTestTikTokClient(server)

	result, err := client.fetch(context.Background(), server.URL+"/short")
	if err != nil {
		t.Fatalf("fetch tiktok content: %v", err)
	}

	if result.MediaPart[contentFieldMIMEType] != tikTokDefaultMIMEType {
		t.Fatalf("unexpected mime type: %#v", result.MediaPart)
	}
}

func TestMaybeAugmentConversationWithTikTokKeepsFirstSuccessfulVideoPerResolvedURL(t *testing.T) {
	t.Parallel()

	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.SearchDeciderModel = testMediaAnalysisModel

	instance := newTikTokTestBot(
		newStubTikTokContentClient(func(
			_ context.Context,
			rawURL string,
		) (tiktokVideoContent, error) {
			return tiktokVideoContent{
				ResolvedURL: "https://www.tiktok.com/@mikemhan/video/7614735539660442893",
				DownloadURL: "https://example.com/video.mp4?source=" + url.QueryEscape(rawURL),
				MediaPart: contentPart{
					"type":               contentTypeVideoData,
					contentFieldBytes:    []byte(rawURL),
					contentFieldMIMEType: testVideoMIMEType,
					contentFieldFilename: "clip.mp4",
				},
			}, nil
		}),
		nil,
	)
	instance.currentSearchDeciderModel = testMediaAnalysisModel

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: strings.Join([]string{
				"<@123>: summarize https://vt.tnktok.com/ZSuhvMpsr/",
				"and https://www.tiktok.com/@mikemhan/video/7614735539660442893",
			}, " "),
		},
	}

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithTikTok(
		context.Background(),
		loadedConfig,
		testMediaAnalysisModel,
		conversation,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with tiktok: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	parts, ok := augmentedConversation[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}
}
