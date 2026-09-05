package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
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

func TestSelectYouTubeShortsDirectFormatSkipsResolverMedia(t *testing.T) {
	t.Parallel()

	formats := []aceThinkerYouTubeShortsItem{
		{
			URL:      "https://rr5---sn-vgqsrnsy.googlevideo.com/videoplayback?expire=1&ip=203.0.113.7&itag=18",
			Filesize: 4096,
			Quality:  "1080p",
			ACodec:   "opus",
			VCodec:   "av01.0.01M.08",
			Ext:      "mp4",
			Protocol: "https",
		},
		{
			URL:      "https://example.com/downloads/portable.mp4",
			Filesize: 2048,
			Quality:  "360p",
			ACodec:   "opus",
			VCodec:   "av01.0.01M.08",
			Ext:      "mp4",
			Protocol: "https",
		},
	}

	format, ok := selectYouTubeShortsDirectFormat(formats)
	if !ok {
		t.Fatal("expected portable direct format to be selected")
	}

	if format.URL != "https://example.com/downloads/portable.mp4" {
		t.Fatalf("unexpected format: %#v", format)
	}
}

func newResolverMediaYouTubeShortsTestServer(t *testing.T) (*httptest.Server, *youtubeShortsServerState) {
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
							URL:      "https://rr5---sn-vgqsrnsy.googlevideo.com/videoplayback?expire=1&ip=203.0.113.7&itag=18",
							Filesize: 8192,
							Quality:  "360p",
							ACodec:   "opus",
							VCodec:   "av01.0.01M.08",
							Ext:      "mp4",
							Protocol: "https",
						},
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
				ID:          "task-456",
				Message:     "queued",
				ProgressURL: server.URL + "/progress?id=task-456",
			})
		case "/progress":
			state.progressCallsMu.Lock()
			state.progressCalls++
			state.progressCallsMu.Unlock()

			if request.URL.Query().Get("id") != "task-456" {
				t.Fatalf("unexpected progress id: %q", request.URL.Query().Get("id"))
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

func TestYouTubeShortsClientFetchFallsBackToLoaderWhenDirectURLIsResolverMedia(t *testing.T) {
	t.Parallel()

	server, state := newResolverMediaYouTubeShortsTestServer(t)
	defer server.Close()

	client := newTestYouTubeShortsClient(server)

	result, err := client.fetch(
		context.Background(),
		testYouTubeShortsCanonicalURL,
	)
	if err != nil {
		t.Fatalf("fetch youtube shorts content: %v", err)
	}

	if !slices.Equal(state.loaderFormats, []string{"720"}) {
		t.Fatalf("unexpected loader formats: %#v", state.loaderFormats)
	}

	if result.DownloadURL != server.URL+"/downloads/merged.mp4" {
		t.Fatalf("unexpected download url: %q", result.DownloadURL)
	}

	if string(mediaPartBytes(t, result.MediaPart)) != "merged-video" {
		t.Fatalf("unexpected video bytes: %#v", result.MediaPart[contentFieldBytes])
	}

	if result.MediaPart[contentFieldFilename] != "merged.mp4" {
		t.Fatalf("unexpected filename: %#v", result.MediaPart[contentFieldFilename])
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
		httpClient:           server.Client(),
		infoURL:              server.URL + "/info",
		loaderURL:            server.URL + "/loader",
		compressorURL:        server.URL,
		userAgent:            youtubeUserAgent,
		infoRetryDelay:       time.Millisecond,
		loaderPollInterval:   time.Millisecond,
		compressPollInterval: time.Millisecond,
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

func TestYouTubeShortsClientFetchInfoRetriesResolverErrorBody(t *testing.T) {
	t.Parallel()

	var (
		infoCallsMu sync.Mutex
		infoCalls   int
	)

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/info" {
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}

		infoCallsMu.Lock()
		infoCalls++
		firstAttempt := infoCalls == 1
		infoCallsMu.Unlock()

		if firstAttempt {
			writeJSON(writer, aceThinkerYouTubeShortsInfoResponse{
				Error:   "All servers have been tried, but failed.",
				Message: "Video not found.",
			})

			return
		}

		writeJSON(writer, aceThinkerYouTubeShortsInfoResponse{
			ResData: aceThinkerYouTubeShortsInfo{
				Title:   "Example Short",
				Message: "success",
				Formats: []aceThinkerYouTubeShortsItem{
					{
						URL:      "https://example.com/downloads/direct.mp4",
						Filesize: 2048,
						Quality:  "360p",
						ACodec:   "opus",
						VCodec:   "av01.0.01M.08",
						Ext:      "mp4",
						Protocol: "https",
					},
				},
			},
		})
	}))
	defer server.Close()

	client := newTestYouTubeShortsClient(server)

	info, err := client.fetchInfo(context.Background(), testYouTubeShortsCanonicalURL)
	if err != nil {
		t.Fatalf("fetch youtube shorts info after resolver error: %v", err)
	}

	if len(info.Formats) != 1 || info.Formats[0].URL != "https://example.com/downloads/direct.mp4" {
		t.Fatalf("unexpected formats: %#v", info.Formats)
	}

	infoCallsMu.Lock()
	defer infoCallsMu.Unlock()

	if infoCalls != 2 {
		t.Fatalf("info calls = %d, want 2 (resolver error then retry)", infoCalls)
	}
}

func TestYouTubeShortsClientFetchInfoExhaustsRetriesAndSurfacesUpstreamError(t *testing.T) {
	t.Parallel()

	var (
		infoCallsMu sync.Mutex
		infoCalls   int
	)

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/info" {
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}

		infoCallsMu.Lock()
		infoCalls++
		infoCallsMu.Unlock()

		writeJSON(writer, aceThinkerYouTubeShortsInfoResponse{
			Error:   "All servers have been tried, but failed.",
			Message: "Video not found.",
		})
	}))
	defer server.Close()

	client := newTestYouTubeShortsClient(server)

	_, err := client.fetchInfo(context.Background(), testYouTubeShortsCanonicalURL)
	if err == nil {
		t.Fatal("fetch youtube shorts info: want error after exhausted retries")
	}

	if !strings.Contains(err.Error(), "All servers have been tried, but failed.") {
		t.Fatalf("error missing upstream resolver message: %v", err)
	}

	infoCallsMu.Lock()
	defer infoCallsMu.Unlock()

	if infoCalls != youtubeShortsInfoRetryMaxAttempts {
		t.Fatalf(
			"info calls = %d, want %d (retry budget exhausted)",
			infoCalls,
			youtubeShortsInfoRetryMaxAttempts,
		)
	}
}

func TestYouTubeShortsClientFetchInfoDoesNotRetryTransportFailures(t *testing.T) {
	t.Parallel()

	var (
		infoCallsMu sync.Mutex
		infoCalls   int
	)

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		infoCallsMu.Lock()
		infoCalls++
		infoCallsMu.Unlock()

		http.Error(writer, "upstream exploded", http.StatusBadGateway)
	}))
	defer server.Close()

	client := newTestYouTubeShortsClient(server)

	_, err := client.fetchInfo(context.Background(), testYouTubeShortsCanonicalURL)
	if err == nil {
		t.Fatal("fetch youtube shorts info: want error for failed transport")
	}

	infoCallsMu.Lock()
	defer infoCallsMu.Unlock()

	if infoCalls != 1 {
		t.Fatalf("info calls = %d, want 1 (transport failures are not retried)", infoCalls)
	}
}

func TestYouTubeShortsClientFetchInfoFallsBackToResDataMessage(t *testing.T) {
	t.Parallel()

	var (
		infoCallsMu sync.Mutex
		infoCalls   int
	)

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		infoCallsMu.Lock()
		infoCalls++
		infoCallsMu.Unlock()

		writeJSON(writer, aceThinkerYouTubeShortsInfoResponse{
			ResData: aceThinkerYouTubeShortsInfo{
				Message: "resolver is warming up",
			},
		})
	}))
	defer server.Close()

	client := newTestYouTubeShortsClient(server)

	_, err := client.fetchInfo(context.Background(), testYouTubeShortsCanonicalURL)
	if err == nil {
		t.Fatal("fetch youtube shorts info: want error for body without formats")
	}

	if !strings.Contains(err.Error(), "resolver is warming up") {
		t.Fatalf("error missing res_data message: %v", err)
	}

	infoCallsMu.Lock()
	defer infoCallsMu.Unlock()

	if infoCalls != youtubeShortsInfoRetryMaxAttempts {
		t.Fatalf(
			"info calls = %d, want %d (retry budget exhausted)",
			infoCalls,
			youtubeShortsInfoRetryMaxAttempts,
		)
	}
}

func TestYouTubeShortsClientFetchInfoDefaultsMessageForEmptyBody(t *testing.T) {
	t.Parallel()

	var (
		infoCallsMu sync.Mutex
		infoCalls   int
	)

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		infoCallsMu.Lock()
		infoCalls++
		infoCallsMu.Unlock()

		writeJSON(writer, aceThinkerYouTubeShortsInfoResponse{})
	}))
	defer server.Close()

	client := newTestYouTubeShortsClient(server)

	_, err := client.fetchInfo(context.Background(), testYouTubeShortsCanonicalURL)
	if err == nil {
		t.Fatal("fetch youtube shorts info: want error for empty body")
	}

	if !strings.Contains(err.Error(), "no downloadable formats") {
		t.Fatalf("error missing default message: %v", err)
	}

	infoCallsMu.Lock()
	defer infoCallsMu.Unlock()

	if infoCalls != youtubeShortsInfoRetryMaxAttempts {
		t.Fatalf(
			"info calls = %d, want %d (retry budget exhausted)",
			infoCalls,
			youtubeShortsInfoRetryMaxAttempts,
		)
	}
}

func TestYouTubeShortsClientFetchInfoMakesSingleCallOnSuccess(t *testing.T) {
	t.Parallel()

	var (
		infoCallsMu sync.Mutex
		infoCalls   int
	)

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		infoCallsMu.Lock()
		infoCalls++
		infoCallsMu.Unlock()

		writeJSON(writer, aceThinkerYouTubeShortsInfoResponse{
			ResData: aceThinkerYouTubeShortsInfo{
				Title:   "Example Short",
				Message: "success",
				Formats: []aceThinkerYouTubeShortsItem{
					{
						URL:      "https://example.com/downloads/direct.mp4",
						Filesize: 2048,
						Quality:  "360p",
						ACodec:   "opus",
						VCodec:   "av01.0.01M.08",
						Ext:      "mp4",
						Protocol: "https",
					},
				},
			},
		})
	}))
	defer server.Close()

	client := newTestYouTubeShortsClient(server)

	info, err := client.fetchInfo(context.Background(), testYouTubeShortsCanonicalURL)
	if err != nil {
		t.Fatalf("fetch youtube shorts info: %v", err)
	}

	if len(info.Formats) != 1 {
		t.Fatalf("unexpected formats: %#v", info.Formats)
	}

	infoCallsMu.Lock()
	defer infoCallsMu.Unlock()

	if infoCalls != 1 {
		t.Fatalf("info calls = %d, want 1 (success on first attempt)", infoCalls)
	}
}

func TestSleepYouTubeShortsInfoRetryContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sleepYouTubeShortsInfoRetry(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sleep retry error = %v, want context.Canceled", err)
	}
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
			prepared, err := instance.prepareYouTubeShortsAugmentation(
				ctx,
				testSearchConfig(),
				"openai/main-model",
				urlExtractionText,
			)
			if err != nil {
				return nil, nil, err
			}

			augmentedConversation, err := applyPreparedConversationAugmentation(
				conversation,
				prepared,
			)
			if err != nil {
				return nil, nil, err
			}

			return augmentedConversation, prepared.warnings, nil
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

			ProviderResponseID: "",
			SearchMetadata:     nil,
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

	prepared, err := instance.prepareYouTubeShortsAugmentation(
		context.Background(),
		testMediaAnalysisConfig(),
		providerSlashModel,
		query,
	)
	if err != nil {
		t.Fatalf("augment conversation with youtube shorts: %v", err)
	}

	augmentedConversation, err := applyPreparedConversationAugmentation(
		conversation,
		prepared,
	)
	if err != nil {
		t.Fatalf("augment conversation with youtube shorts: %v", err)
	}

	warnings := prepared.warnings

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	assertResult(t, augmentedConversation, expectedText, callCount)
}

func oversizedYouTubeShortsTestVideo() []byte {
	video := make([]byte, youtubeShortsMaxUploadBytes+1024)
	for index := range video {
		video[index] = byte(index)
	}

	return video
}

type youtubeShortsCompressServerConfig struct {
	oversizedVideo  []byte
	compressedVideo []byte
	allowJob        bool
	uploaded        *[]byte
	rqjobCalls      *int
}

func newYouTubeShortsCompressTestServer(
	t *testing.T,
	config youtubeShortsCompressServerConfig,
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/info":
			writeJSON(writer, aceThinkerYouTubeShortsInfoResponse{
				ResData: aceThinkerYouTubeShortsInfo{
					Title:   "Example Short",
					Message: "success",
					Formats: []aceThinkerYouTubeShortsItem{
						{
							URL:      "http://" + request.Host + "/downloads/direct.mp4",
							Filesize: int64(len(config.oversizedVideo)),
							Quality:  "720p",
							ACodec:   "opus",
							VCodec:   "av01.0.01M.08",
							Ext:      "mp4",
							Protocol: "https",
						},
					},
				},
			})
		case "/downloads/direct.mp4":
			writer.Header().Set("Content-Type", "video/mp4")
			writer.Header().Set("Content-Disposition", `attachment; filename="original.mp4"`)
			_, _ = writer.Write(config.oversizedVideo)
		case "/rqjob":
			if config.rqjobCalls != nil {
				*config.rqjobCalls++
			}

			if !config.allowJob {
				writeJSON(writer, autocompressorRQJobResponse{
					Allowed: false,
					Message: "Server full",
				})

				return
			}

			var req autocompressorRQJobRequest

			_ = json.NewDecoder(request.Body).Decode(&req)
			if req.TargetSize != "8" || req.OutputFormat != "mp4" {
				t.Errorf("unexpected rqjob request: %#v", req)
			}

			writeJSON(writer, autocompressorRQJobResponse{
				Allowed:     true,
				Server:      "01",
				Message:     "job-12345",
				UploadLimit: 2147483648,
			})
		case "/job/job-12345/upload":
			serveYouTubeShortsCompressUpload(t, writer, request, config.uploaded)
		case "/job/job-12345/status":
			writeJSON(writer, finishedAutocompressorStatusResponse())
		case "/job/job-12345/download":
			writer.Header().Set("Content-Type", "video/mp4")
			writer.Header().Set("Content-Disposition", `attachment; filename="original-8.mp4"`)
			_, _ = writer.Write(config.compressedVideo)
		default:
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
	}))
}

func serveYouTubeShortsCompressUpload(
	t *testing.T,
	writer http.ResponseWriter,
	request *http.Request,
	uploaded *[]byte,
) {
	t.Helper()

	err := request.ParseMultipartForm(32 << 20)
	if err != nil {
		t.Errorf("parse multipart upload form: %v", err)
	}

	file, _, err := request.FormFile("filetoupload")
	if err != nil {
		t.Errorf("get filetoupload: %v", err)
	} else {
		body, _ := io.ReadAll(file)
		_ = file.Close()

		if uploaded != nil {
			*uploaded = body
		}
	}

	writeJSON(writer, autocompressorUploadResponse{Error: false})
}

func finishedAutocompressorStatusResponse() autocompressorStatusResponse {
	var resp autocompressorStatusResponse

	resp.Status.Ended = true
	resp.Status.Error = false
	resp.Progress.Action = "Running final encoding pass"
	resp.Progress.Quantified = true
	resp.Progress.Progress = 1.0

	return resp
}

func TestYouTubeShortsClientFetchCompressesOversizedVideo(t *testing.T) {
	t.Parallel()

	oversizedVideo := oversizedYouTubeShortsTestVideo()
	compressedVideo := []byte("compressed-8mb-shorts-video")

	var uploadFileBytes []byte

	var rqjobCalls int

	server := newYouTubeShortsCompressTestServer(t, youtubeShortsCompressServerConfig{
		oversizedVideo:  oversizedVideo,
		compressedVideo: compressedVideo,
		allowJob:        true,
		uploaded:        &uploadFileBytes,
		rqjobCalls:      &rqjobCalls,
	})
	defer server.Close()

	client := newTestYouTubeShortsClient(server)

	result, err := client.fetch(context.Background(), testYouTubeShortsCanonicalURL)
	if err != nil {
		t.Fatalf("fetch youtube shorts content: %v", err)
	}

	if rqjobCalls != 1 {
		t.Fatalf("compressor job requests = %d, want 1", rqjobCalls)
	}

	if len(uploadFileBytes) != len(oversizedVideo) {
		t.Fatalf("uploaded file size = %d, want %d", len(uploadFileBytes), len(oversizedVideo))
	}

	if string(mediaPartBytes(t, result.MediaPart)) != string(compressedVideo) {
		t.Fatal("expected compressed video bytes in result media part")
	}

	if result.MediaPart[contentFieldFilename] != "original-8.mp4" {
		t.Fatalf("got filename %q, want %q", result.MediaPart[contentFieldFilename], "original-8.mp4")
	}
}

func TestYouTubeShortsClientFetchCompressFallbackOnFailure(t *testing.T) {
	t.Parallel()

	oversizedVideo := oversizedYouTubeShortsTestVideo()

	var rqjobCalls int

	server := newYouTubeShortsCompressTestServer(t, youtubeShortsCompressServerConfig{
		oversizedVideo: oversizedVideo,
		rqjobCalls:     &rqjobCalls,
	})
	defer server.Close()

	client := newTestYouTubeShortsClient(server)

	result, err := client.fetch(context.Background(), testYouTubeShortsCanonicalURL)
	if err != nil {
		t.Fatalf("fetch youtube shorts content: %v", err)
	}

	if rqjobCalls != 1 {
		t.Fatalf("compressor job requests = %d, want 1", rqjobCalls)
	}

	if len(mediaPartBytes(t, result.MediaPart)) != len(oversizedVideo) {
		t.Fatalf("got result bytes len %d, want %d", len(mediaPartBytes(t, result.MediaPart)), len(oversizedVideo))
	}
}
