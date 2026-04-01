package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testYouTubeShortsVideoID      = "abc123def45"
	testYouTubeShortsCanonicalURL = "https://www.youtube.com/shorts/" + testYouTubeShortsVideoID
)

type stubYouTubeShortsContentClient struct {
	mu      sync.Mutex
	calls   []string
	fetchFn func(context.Context, string) (youtubeShortsVideoContent, error)
}

func (client *stubYouTubeShortsContentClient) fetch(
	ctx context.Context,
	rawURL string,
) (youtubeShortsVideoContent, error) {
	client.mu.Lock()
	client.calls = append(client.calls, rawURL)
	client.mu.Unlock()

	return client.fetchFn(ctx, rawURL)
}

func newStubYouTubeShortsContentClient(
	fetchFn func(context.Context, string) (youtubeShortsVideoContent, error),
) *stubYouTubeShortsContentClient {
	client := new(stubYouTubeShortsContentClient)
	client.fetchFn = fetchFn

	return client
}

func newYouTubeShortsTestBot(
	youtubeShorts youtubeShortsFetcher,
	chatCompletions chatCompletionStreamer,
) *bot {
	instance := new(bot)
	instance.youtubeShorts = youtubeShorts
	instance.chatCompletions = chatCompletions

	return instance
}

func testYouTubeShortsVideoContent() youtubeShortsVideoContent {
	return youtubeShortsVideoContent{
		ResolvedURL: testYouTubeShortsCanonicalURL,
		DownloadURL: "https://example.com/shorts.mp4",
		MediaPart: contentPart{
			"type":               contentTypeVideoData,
			contentFieldBytes:    []byte(testVideoBody),
			contentFieldMIMEType: testVideoMIMEType,
			contentFieldFilename: "clip.mp4",
		},
	}
}

func testYouTubeShortsConversationWithImage() []chatMessage {
	return []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": "<@123>: summarize " + testYouTubeShortsCanonicalURL},
				{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
			},
		},
	}
}

func TestExtractYouTubeShortsURLsNormalizesAndDeduplicates(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"check https://www.youtube.com/shorts/abc123def45?feature=share",
		"and https://m.youtube.com/shorts/abc123def45/,",
		"plus youtube.com/shorts/ZYX987wvu65!",
	}, " ")

	urls := extractYouTubeShortsURLs(text)

	expected := []string{
		testYouTubeShortsCanonicalURL,
		"https://www.youtube.com/shorts/ZYX987wvu65",
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

func TestExtractYouTubeShortsURLsIgnoresURLsInAugmentedPromptSections(t *testing.T) {
	t.Parallel()

	text := augmentedUserPrompt{
		RepliedMessage:   "",
		UserQuery:        "<@123>: summarize this short",
		YouTubeContent:   "URL: " + testYouTubeShortsCanonicalURL,
		RedditContent:    "Linked short: https://www.youtube.com/shorts/ZYX987wvu65",
		WebsiteContent:   "URL: https://www.youtube.com/shorts/LMN456rst98",
		DocumentContent:  "Doc link: https://www.youtube.com/shorts/QWE123asd45",
		VisualSearch:     "Site match: https://www.youtube.com/shorts/RTY456fgh78",
		WebSearchResults: "1. https://www.youtube.com/shorts/UIO789jkl01",
	}.render()

	urls := extractYouTubeShortsURLs(text)
	if len(urls) != 0 {
		t.Fatalf("unexpected urls: %#v", urls)
	}
}

type youtubeShortsServerState struct {
	submittedURL    string
	loaderFormats   []string
	progressCalls   int
	progressCallsMu sync.Mutex
}

func newDirectYouTubeShortsTestServer(
	t *testing.T,
	videoBody string,
	videoContentType string,
	contentDisposition string,
) (*httptest.Server, *youtubeShortsServerState) {
	t.Helper()

	state := new(youtubeShortsServerState)

	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/info":
			state.submittedURL = request.URL.Query().Get("url")
			writeJSON(writer, aceThinkerYouTubeShortsInfoResponse{
				ResData: aceThinkerYouTubeShortsInfo{
					Title:   "Example Short",
					Message: "success",
					Formats: []aceThinkerYouTubeShortsItem{
						{
							URL:      server.URL + "/downloads/direct.mp4",
							Filesize: int64(len(videoBody)),
							Quality:  "360p",
							ACodec:   "opus",
							VCodec:   "av01.0.01M.08",
							Ext:      "mp4",
							Protocol: "https",
						},
					},
				},
			})
		case "/downloads/direct.mp4":
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

func newLoaderYouTubeShortsTestServer(t *testing.T) (*httptest.Server, *youtubeShortsServerState) {
	t.Helper()

	state := new(youtubeShortsServerState)

	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/info":
			state.submittedURL = request.URL.Query().Get("url")
			writeJSON(writer, aceThinkerYouTubeShortsInfoResponse{
				ResData: aceThinkerYouTubeShortsInfo{
					Title:   "Example Short",
					Message: "success",
					Formats: []aceThinkerYouTubeShortsItem{
						{
							URL:      server.URL + "/downloads/video-only.mp4",
							Filesize: 4096,
							Quality:  "720p",
							ACodec:   "none",
							VCodec:   "av01.0.01M.08",
							Ext:      "mp4",
							Protocol: "https",
						},
						{
							URL:      server.URL + "/downloads/audio-only.m4a",
							Filesize: 1024,
							Quality:  "high",
							ACodec:   "mp4a.40.2",
							VCodec:   "none",
							Ext:      "m4a",
							Protocol: "https",
						},
					},
				},
			})
		case "/loader":
			state.loaderFormats = append(state.loaderFormats, request.URL.Query().Get("f"))
			writeJSON(writer, aceThinkerYouTubeShortsLoaderResponse{
				Success:     true,
				ID:          "task-123",
				Message:     "queued",
				ProgressURL: server.URL + "/progress?id=task-123",
			})
		case "/progress":
			state.progressCallsMu.Lock()
			state.progressCalls++
			progressCalls := state.progressCalls
			state.progressCallsMu.Unlock()

			if request.URL.Query().Get("id") != "task-123" {
				t.Fatalf("unexpected progress id: %q", request.URL.Query().Get("id"))
			}

			if progressCalls == 1 {
				writeJSON(writer, aceThinkerYouTubeShortsProgressResponse{
					Success:     0,
					Progress:    500,
					DownloadURL: "",
					Message:     "processing",
					Text:        "Processing",
				})

				return
			}

			writeJSON(writer, aceThinkerYouTubeShortsProgressResponse{
				Success:     1,
				Progress:    1000,
				DownloadURL: server.URL + "/downloads/merged.mp4",
				Message:     "finished",
				Text:        "Finished",
			})
		case "/downloads/merged.mp4":
			writer.Header().Set("Content-Type", "video/mp4")
			writer.Header().Set("Content-Disposition", `attachment; filename="merged.mp4"`)
			_, _ = writer.Write([]byte("merged-video"))
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}
	}))

	return server, state
}

func newTestYouTubeShortsClient(server *httptest.Server) youtubeShortsClient {
	return youtubeShortsClient{
		httpClient:         server.Client(),
		infoURL:            server.URL + "/info",
		loaderURL:          server.URL + "/loader",
		userAgent:          youtubeUserAgent,
		requestTimeout:     time.Second,
		loaderPollInterval: time.Millisecond,
	}
}

func TestYouTubeShortsClientFetchDownloadsDirectMP4(t *testing.T) {
	t.Parallel()

	server, state := newDirectYouTubeShortsTestServer(
		t,
		testVideoBody,
		"application/octet-stream",
		"",
	)
	defer server.Close()

	client := newTestYouTubeShortsClient(server)

	result, err := client.fetch(
		context.Background(),
		testYouTubeShortsCanonicalURL+"?feature=share",
	)
	if err != nil {
		t.Fatalf("fetch youtube shorts content: %v", err)
	}

	if state.submittedURL != testYouTubeShortsCanonicalURL {
		t.Fatalf("unexpected submitted url: %q", state.submittedURL)
	}

	if result.ResolvedURL != testYouTubeShortsCanonicalURL {
		t.Fatalf("unexpected resolved url: %q", result.ResolvedURL)
	}

	if result.DownloadURL != server.URL+"/downloads/direct.mp4" {
		t.Fatalf("unexpected download url: %q", result.DownloadURL)
	}

	if string(mediaPartBytes(t, result.MediaPart)) != testVideoBody {
		t.Fatalf("unexpected video bytes: %#v", result.MediaPart[contentFieldBytes])
	}

	if result.MediaPart[contentFieldMIMEType] != youtubeShortsDefaultMIMEType {
		t.Fatalf("unexpected MIME type: %#v", result.MediaPart[contentFieldMIMEType])
	}

	if result.MediaPart[contentFieldFilename] != youtubeShortsFilenamePrefix+testYouTubeShortsVideoID+".mp4" {
		t.Fatalf("unexpected filename: %#v", result.MediaPart[contentFieldFilename])
	}
}

func TestYouTubeShortsClientFetchFallsBackToLoaderWhenDirectProgressiveMP4Unavailable(t *testing.T) {
	t.Parallel()

	server, state := newLoaderYouTubeShortsTestServer(t)
	defer server.Close()

	client := newTestYouTubeShortsClient(server)

	result, err := client.fetch(context.Background(), testYouTubeShortsCanonicalURL)
	if err != nil {
		t.Fatalf("fetch youtube shorts content: %v", err)
	}

	if state.submittedURL != testYouTubeShortsCanonicalURL {
		t.Fatalf("unexpected submitted url: %q", state.submittedURL)
	}

	if len(state.loaderFormats) != 1 || state.loaderFormats[0] != "720" {
		t.Fatalf("unexpected loader formats: %#v", state.loaderFormats)
	}

	if state.progressCalls != 2 {
		t.Fatalf("unexpected progress call count: %d", state.progressCalls)
	}

	if result.DownloadURL != server.URL+"/downloads/merged.mp4" {
		t.Fatalf("unexpected download url: %q", result.DownloadURL)
	}

	if string(mediaPartBytes(t, result.MediaPart)) != "merged-video" {
		t.Fatalf("unexpected merged video bytes: %#v", result.MediaPart[contentFieldBytes])
	}

	if result.MediaPart[contentFieldFilename] != "merged.mp4" {
		t.Fatalf("unexpected filename: %#v", result.MediaPart[contentFieldFilename])
	}
}

func TestMaybeAugmentConversationWithYouTubeShortsAppendsVideoPartsAndAnalysesForNonGeminiSearchDecider(t *testing.T) {
	t.Parallel()

	assertYouTubeShortsAugmentationForProvider(
		t,
		testMediaAnalysisModel,
		testYouTubeShortsConversationWithImage(),
		func(
			t *testing.T,
			augmentedConversation []chatMessage,
			expectedText string,
			_ int,
		) {
			t.Helper()

			assertAugmentedVideoParts(t, augmentedConversation, expectedText)
			assertSearchDeciderTextContent(
				t,
				augmentedConversation,
				testMediaAnalysisConfig(),
				"openai/decider-model",
				expectedText,
			)
		},
	)
}

func TestMaybeAugmentConversationWithYouTubeShortsPreprocessesForNonGeminiModels(t *testing.T) {
	t.Parallel()

	assertYouTubeShortsAugmentationForProvider(
		t,
		"openai/gpt-5",
		[]chatMessage{
			{
				Role:    messageRoleUser,
				Content: "<@123>: summarize " + testYouTubeShortsCanonicalURL,
			},
		},
		func(
			t *testing.T,
			augmentedConversation []chatMessage,
			expectedText string,
			callCount int,
		) {
			t.Helper()

			if callCount != 1 {
				t.Fatalf("unexpected gemini analysis call count: %d", callCount)
			}

			content, ok := augmentedConversation[0].Content.(string)
			if !ok {
				t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
			}

			if content != expectedText {
				t.Fatalf("unexpected augmented content: %q", content)
			}
		},
	)
}

func TestMaybeAugmentConversationWithYouTubeShortsIgnoresURLsOnlyPresentInDocumentContent(t *testing.T) {
	t.Parallel()

	instance := newYouTubeShortsTestBot(
		newStubYouTubeShortsContentClient(func(
			_ context.Context,
			rawURL string,
		) (youtubeShortsVideoContent, error) {
			t.Errorf("unexpected youtube shorts fetch for %q", rawURL)

			return testYouTubeShortsVideoContent(), os.ErrInvalid
		}),
		nil,
	)

	assertURLAugmentationIgnoresDocumentOnlyURLs(
		t,
		testYouTubeShortsCanonicalURL,
		func(
			ctx context.Context,
			conversation []chatMessage,
			urlExtractionText string,
		) ([]chatMessage, []string, error) {
			return instance.maybeAugmentConversationWithYouTubeShorts(
				ctx,
				testSearchConfig(),
				"openai/main-model",
				conversation,
				urlExtractionText,
			)
		},
	)

	client, ok := instance.youtubeShorts.(*stubYouTubeShortsContentClient)
	if !ok {
		t.Fatalf("unexpected youtube shorts client type: %T", instance.youtubeShorts)
	}

	if len(client.calls) != 0 {
		t.Fatalf(
			"unexpected fetch call count: %d",
			len(client.calls),
		)
	}
}

func assertYouTubeShortsAugmentationForProvider(
	t *testing.T,
	providerSlashModel string,
	conversation []chatMessage,
	assertResult func(*testing.T, []chatMessage, string, int),
) {
	t.Helper()

	expectedAnalysis := "Video description per timestamp:\n\n0s to 10s: somebody waves"
	query := "<@123>: summarize " + testYouTubeShortsCanonicalURL
	expectedText := expectedMediaAnalysisUserText(query, []string{expectedAnalysis})

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
			Thinking:     "",
			Content:      expectedAnalysis,
			FinishReason: finishReasonStop,
			Usage:        nil,
		})
	})

	instance := newYouTubeShortsTestBot(
		newStubYouTubeShortsContentClient(func(
			_ context.Context,
			rawURL string,
		) (youtubeShortsVideoContent, error) {
			if rawURL != testYouTubeShortsCanonicalURL {
				t.Fatalf("unexpected raw url: %q", rawURL)
			}

			return testYouTubeShortsVideoContent(), nil
		}),
		chatClient,
	)

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithYouTubeShorts(
		context.Background(),
		testMediaAnalysisConfig(),
		providerSlashModel,
		conversation,
		query,
	)
	if err != nil {
		t.Fatalf("augment conversation with youtube shorts: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	assertResult(t, augmentedConversation, expectedText, callCount)
}
