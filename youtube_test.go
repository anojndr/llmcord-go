package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stubYouTubeContentClient struct {
	mu      sync.Mutex
	calls   []string
	fetchFn func(context.Context, string) (youtubeVideoContent, error)
}

func (client *stubYouTubeContentClient) fetch(
	ctx context.Context,
	rawURL string,
) (youtubeVideoContent, error) {
	client.mu.Lock()
	client.calls = append(client.calls, rawURL)
	client.mu.Unlock()

	return client.fetchFn(ctx, rawURL)
}

func newStubYouTubeContentClient(
	fetchFn func(context.Context, string) (youtubeVideoContent, error),
) *stubYouTubeContentClient {
	client := new(stubYouTubeContentClient)
	client.fetchFn = fetchFn

	return client
}

func newYouTubeTestBot(youtube youtubeFetcher) *bot {
	instance := new(bot)
	instance.youtube = youtube

	return instance
}

func TestExtractYouTubeURLsNormalizesAndDeduplicates(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"check https://youtu.be/dQw4w9WgXcQ?t=43",
		"and https://www.youtube.com/watch?v=dQw4w9WgXcQ,",
	}, " ")

	urls := extractYouTubeURLs(text)

	expected := []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
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

func TestExtractYouTubeURLsIgnoresURLsInAugmentedPromptSections(t *testing.T) {
	t.Parallel()

	text := augmentedUserPrompt{
		RepliedMessage:   "",
		UserQuery:        "<@123>: summarize this video",
		YouTubeContent:   "URL: https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		RedditContent:    "Linked video: https://www.youtube.com/watch?v=abc123def45",
		WebsiteContent:   "URL: https://www.youtube.com/watch?v=5NV6Rdv1a3I",
		DocumentContent:  "Doc link: https://www.youtube.com/watch?v=aqz-KE-bpKQ",
		VisualSearch:     "Site match: https://youtu.be/3JZ_D3ELwOQ",
		WebSearchResults: "1. https://www.youtube.com/watch?v=oHg5SJYRHA0",
	}.render()

	urls := extractYouTubeURLs(text)
	if len(urls) != 0 {
		t.Fatalf("unexpected urls: %#v", urls)
	}
}

func TestAppendWebSearchResultsKeepsOriginalUserQueryAfterYouTubeAugmentation(t *testing.T) {
	t.Parallel()

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: "<@123>: summarize this video",
		},
	}

	augmentedConversation, err := appendYouTubeContentToConversation(
		conversation,
		"URL: https://www.youtube.com/watch?v=dQw4w9WgXcQ\nTitle: Example",
	)
	if err != nil {
		t.Fatalf("append youtube content: %v", err)
	}

	augmentedConversation, err = appendWebSearchResultsToConversation(
		augmentedConversation,
		"Query: example\nResults:\ncontext",
	)
	if err != nil {
		t.Fatalf("append web search results: %v", err)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	prompt := parseAugmentedUserPrompt(content)

	if prompt.UserQuery != "<@123>: summarize this video" {
		t.Fatalf("unexpected user query: %q", prompt.UserQuery)
	}

	if !containsFold(prompt.YouTubeContent, "Title: Example") {
		t.Fatalf("expected youtube content to be preserved: %q", prompt.YouTubeContent)
	}

	if !containsFold(prompt.WebSearchResults, "Query: example") {
		t.Fatalf("expected web search results to be appended: %q", prompt.WebSearchResults)
	}
}

func TestAppendYouTubeContentToConversationPreservesImages(t *testing.T) {
	t.Parallel()

	assertContextAugmentationPreservesImages(
		t,
		"<@123>: summarize this https://youtu.be/dQw4w9WgXcQ",
		"URL: https://www.youtube.com/watch?v=dQw4w9WgXcQ\nTitle: Example",
		youtubeSectionName,
		appendYouTubeContentToConversation,
	)
}

func TestMaybeAugmentConversationWithYouTubeFetchesMultipleURLsConcurrentlyAndKeepsOrder(t *testing.T) {
	t.Parallel()

	var (
		startedCount int
		startedMu    sync.Mutex
		release      = make(chan struct{})
	)

	youtube := newStubYouTubeContentClient(func(
		_ context.Context,
		rawURL string,
	) (youtubeVideoContent, error) {
		startedMu.Lock()

		startedCount++
		if startedCount == 2 {
			close(release)
		}
		startedMu.Unlock()

		<-release

		videoID, canonicalURL, err := parseYouTubeVideoURL(rawURL)
		if err != nil {
			return youtubeVideoContent{}, err
		}

		return youtubeVideoContent{
			URL:         canonicalURL,
			VideoID:     videoID,
			Title:       "Title for " + videoID,
			ChannelName: "Channel for " + videoID,
			Transcript:  "Transcript for " + videoID,
			Comments:    nil,
		}, nil
	})

	instance := newYouTubeTestBot(youtube)

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: strings.Join([]string{
				"<@123>: summarize these",
				"https://youtu.be/dQw4w9WgXcQ",
				"and https://www.youtube.com/watch?v=abc123def45",
			}, " "),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	augmentedConversation, warnings, err := instance.maybeAugmentConversationWithYouTube(
		ctx,
		conversation,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with youtube: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	firstIndex := strings.Index(content, "Title: Title for dQw4w9WgXcQ")

	secondIndex := strings.Index(content, "Title: Title for abc123def45")
	if firstIndex == -1 || secondIndex == -1 || firstIndex >= secondIndex {
		t.Fatalf("expected youtube results to preserve url order: %q", content)
	}

	if len(youtube.calls) != 2 {
		t.Fatalf("unexpected fetch call count: %d", len(youtube.calls))
	}
}

func TestYouTubeFetchStartsWatchPageBeforeTranscriptCompletes(t *testing.T) {
	t.Parallel()

	const (
		videoID       = "dQw4w9WgXcQ"
		youtubeAPIKey = "youtube-api-key"
		clientVersion = "1.20250301.00.00"
		commentsToken = "page-1"
		videoURL      = "https://www.youtube.com/watch?v=" + videoID
	)

	watchStarted := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/notegpt/video-transcript":
			select {
			case <-watchStarted:
			case <-request.Context().Done():
				http.Error(writer, request.Context().Err().Error(), http.StatusGatewayTimeout)

				return
			}

			writeJSON(writer, newSuccessNoteGPTVideoTranscriptResponse(mockNoteGPTTranscriptData(videoID)))
		case "/watch":
			close(watchStarted)
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = writer.Write([]byte(mockYouTubeWatchHTML(youtubeAPIKey, clientVersion, commentsToken)))
		case "/youtubei/v1/next":
			writeJSON(writer, mockYouTubeCommentsPageResponse("reloadContinuationItemsCommand", 1, 1, ""))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := youtubeClient{
		httpClient:        server.Client(),
		watchURL:          server.URL + "/watch",
		apiBaseURL:        server.URL + "/youtubei/v1",
		noteGPTAPIBaseURL: server.URL + "/notegpt",
		userAgent:         youtubeUserAgent,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	content, err := client.fetch(ctx, videoURL)
	if err != nil {
		t.Fatalf("fetch youtube content: %v", err)
	}

	if content.URL != videoURL {
		t.Fatalf("unexpected canonical URL: %q", content.URL)
	}

	if strings.TrimSpace(content.Transcript) == "" {
		t.Fatal("expected transcript to be populated")
	}

	if len(content.Comments) != 1 {
		t.Fatalf("unexpected comments: %#v", content.Comments)
	}
}

func TestMaybeAugmentConversationWithYouTubeIgnoresURLsOnlyPresentInDocumentContent(t *testing.T) {
	t.Parallel()

	youtube := newStubYouTubeContentClient(func(
		_ context.Context,
		rawURL string,
	) (youtubeVideoContent, error) {
		t.Fatalf("unexpected youtube fetch for %q", rawURL)

		return youtubeVideoContent{
			URL:         "",
			VideoID:     "",
			Title:       "",
			ChannelName: "",
			Transcript:  "",
			Comments:    nil,
		}, nil
	})

	instance := newYouTubeTestBot(youtube)

	assertURLAugmentationIgnoresDocumentOnlyURLs(
		t,
		"https://youtu.be/dQw4w9WgXcQ",
		func(
			ctx context.Context,
			conversation []chatMessage,
			urlExtractionText string,
		) ([]chatMessage, []string, error) {
			return instance.maybeAugmentConversationWithYouTube(
				ctx,
				conversation,
				urlExtractionText,
			)
		},
	)

	if len(youtube.calls) != 0 {
		t.Fatalf("unexpected fetch call count: %d", len(youtube.calls))
	}
}

func TestFormatNoteGPTTranscriptPrefersEnglishDefaultSegments(t *testing.T) {
	t.Parallel()

	transcript, err := formatNoteGPTTranscript(
		newNoteGPTTranscriptData(
			"",
			"",
			"",
			[]noteGPTLanguageCode{
				newNoteGPTLanguageCode("es_auto_auto", "Spanish"),
				newNoteGPTLanguageCode("en_auto_auto", "English"),
			},
			map[string]noteGPTTranscriptTracks{
				"es_auto_auto": newNoteGPTTranscriptTracks(
					[]string{"hola", "mundo"},
					nil,
				),
				"en_auto_auto": newNoteGPTTranscriptTracks(
					[]string{"Hello", "world", "world"},
					[]string{"Ignored custom transcript"},
				),
			},
			"",
		),
	)
	if err != nil {
		t.Fatalf("format notegpt transcript: %v", err)
	}

	if transcript != "Hello\nworld" {
		t.Fatalf("unexpected transcript: %q", transcript)
	}
}

func TestYouTubeClientFetchUsesNoteGPTTranscriptAndLimitsCommentsToFifty(t *testing.T) {
	t.Parallel()

	const (
		videoID       = "abc123def45"
		youtubeAPIKey = "test-api-key"
		clientVersion = "2.20260309.01.00"
		initialToken  = "page-1"
	)

	noteGPTServer, noteGPTCalls := newMockNoteGPTServer(
		t,
		videoID,
		newDirectNoteGPTConfig(videoID),
	)
	defer noteGPTServer.Close()

	commentsServer, nextCalls := newMockYouTubeCommentsServer(
		t,
		youtubeAPIKey,
		clientVersion,
		initialToken,
	)
	defer commentsServer.Close()

	client := newTestYouTubeClient(
		noteGPTServer.Client(),
		noteGPTServer.URL+"/api/v2",
		commentsServer.URL+"/watch",
		commentsServer.URL+"/youtubei/v1",
	)

	result, err := client.fetch(context.Background(), "https://youtu.be/"+videoID+"?si=test")
	if err != nil {
		t.Fatalf("fetch youtube content: %v", err)
	}

	assertMockYouTubeResult(t, result, videoID, "Example Video", "Example Channel", "Hello\nworld")

	if len(result.Comments) != 50 {
		t.Fatalf("unexpected comment count: %d", len(result.Comments))
	}

	if result.Comments[0].Author != "Author 1" || result.Comments[0].Text != "Comment 1" {
		t.Fatalf("unexpected first comment: %#v", result.Comments[0])
	}

	if result.Comments[49].Author != "Author 50" || result.Comments[49].Text != "Comment 50" {
		t.Fatalf("unexpected fiftieth comment: %#v", result.Comments[49])
	}

	if nextCallCount := atomic.LoadInt32(nextCalls); nextCallCount != 3 {
		t.Fatalf("unexpected next call count: %d", nextCallCount)
	}

	assertNoteGPTCallCounts(t, noteGPTCalls, 1, 0, 0)
}

func TestYouTubeClientFetchGeneratesTranscriptWhenNoteGPTCacheMisses(t *testing.T) {
	t.Parallel()

	const (
		videoID       = "abc123def45"
		youtubeAPIKey = "test-api-key"
		clientVersion = "2.20260309.01.00"
	)

	noteGPTServer, noteGPTCalls := newMockNoteGPTServer(
		t,
		videoID,
		newGeneratingNoteGPTConfig(videoID),
	)
	defer noteGPTServer.Close()

	commentsServer, nextCalls := newMockYouTubeCommentsServer(
		t,
		youtubeAPIKey,
		clientVersion,
		"",
	)
	defer commentsServer.Close()

	client := newTestYouTubeClient(
		noteGPTServer.Client(),
		noteGPTServer.URL+"/api/v2",
		commentsServer.URL+"/watch",
		commentsServer.URL+"/youtubei/v1",
	)

	result, err := client.fetch(context.Background(), "https://youtu.be/"+videoID)
	if err != nil {
		t.Fatalf("fetch youtube content after notegpt generation: %v", err)
	}

	assertMockYouTubeResult(t, result, videoID, "Example Video", "Example Channel", "Hello\nworld")

	if len(result.Comments) != 0 {
		t.Fatalf("unexpected comments: %#v", result.Comments)
	}

	assertNoteGPTCallCounts(t, noteGPTCalls, 2, 1, 2)

	if got := atomic.LoadInt32(nextCalls); got != 0 {
		t.Fatalf("unexpected youtube comments call count: %d", got)
	}
}

func TestYouTubeClientFetchReturnsErrorWhenNoteGPTGenerationFails(t *testing.T) {
	t.Parallel()

	const videoID = "abc123def45"

	noteGPTServer, noteGPTCalls := newMockNoteGPTServer(
		t,
		videoID,
		newFailingNoteGPTConfig(),
	)
	defer noteGPTServer.Close()

	client := newTestYouTubeClient(
		noteGPTServer.Client(),
		noteGPTServer.URL+"/api/v2",
		"http://127.0.0.1/unused-watch",
		"http://127.0.0.1/unused-api",
	)

	_, err := client.fetch(context.Background(), "https://youtu.be/"+videoID)
	if err == nil {
		t.Fatal("expected fetch youtube content error")
	}

	if !strings.Contains(err.Error(), "notegpt transcript generate code 164003") {
		t.Fatalf("unexpected error: %v", err)
	}

	assertNoteGPTCallCounts(t, noteGPTCalls, 1, 1, 0)
}

type mockNoteGPTConfig struct {
	videoTranscriptResponses   []noteGPTVideoTranscriptResponse
	transcriptGenerateResponse noteGPTStatusResponse
	mediaStatusResponses       []noteGPTStatusResponse
}

type mockNoteGPTCalls struct {
	videoTranscriptCalls    atomic.Int32
	transcriptGenerateCalls atomic.Int32
	mediaStatusCalls        atomic.Int32
}

func newTestYouTubeClient(
	httpClient *http.Client,
	noteGPTAPIBaseURL string,
	watchURL string,
	apiBaseURL string,
) youtubeClient {
	return youtubeClient{
		httpClient:        httpClient,
		watchURL:          watchURL,
		apiBaseURL:        apiBaseURL,
		noteGPTAPIBaseURL: noteGPTAPIBaseURL,
		userAgent:         youtubeUserAgent,
	}
}

func newDirectNoteGPTConfig(videoID string) mockNoteGPTConfig {
	return mockNoteGPTConfig{
		videoTranscriptResponses: []noteGPTVideoTranscriptResponse{
			newSuccessNoteGPTVideoTranscriptResponse(mockNoteGPTTranscriptData(videoID)),
		},
		transcriptGenerateResponse: noteGPTStatusResponse{
			Code:    0,
			Message: "",
			Data: noteGPTStatusData{
				Status: "",
			},
		},
		mediaStatusResponses: nil,
	}
}

func newGeneratingNoteGPTConfig(videoID string) mockNoteGPTConfig {
	return mockNoteGPTConfig{
		videoTranscriptResponses: []noteGPTVideoTranscriptResponse{
			newNoTranscriptNoteGPTVideoTranscriptResponse("523"),
			newSuccessNoteGPTVideoTranscriptResponse(mockNoteGPTTranscriptData(videoID)),
		},
		transcriptGenerateResponse: newSuccessNoteGPTStatusResponse(noteGPTMediaStatusProcessing),
		mediaStatusResponses: []noteGPTStatusResponse{
			newSuccessNoteGPTStatusResponse(noteGPTMediaStatusProcessing),
			newSuccessNoteGPTStatusResponse(noteGPTMediaStatusSuccess),
		},
	}
}

func newFailingNoteGPTConfig() mockNoteGPTConfig {
	return mockNoteGPTConfig{
		videoTranscriptResponses: []noteGPTVideoTranscriptResponse{
			newNoTranscriptNoteGPTVideoTranscriptResponse("523"),
		},
		transcriptGenerateResponse: noteGPTStatusResponse{
			Code:    164003,
			Message: "quota exceeded",
			Data: noteGPTStatusData{
				Status: "",
			},
		},
		mediaStatusResponses: nil,
	}
}

func newSuccessNoteGPTVideoTranscriptResponse(
	transcriptData noteGPTVideoTranscriptData,
) noteGPTVideoTranscriptResponse {
	return noteGPTVideoTranscriptResponse{
		Code:    noteGPTSuccessCode,
		Message: "success",
		Data:    transcriptData,
	}
}

func newNoTranscriptNoteGPTVideoTranscriptResponse(
	duration string,
) noteGPTVideoTranscriptResponse {
	return noteGPTVideoTranscriptResponse{
		Code:    noteGPTSuccessCode,
		Message: "no transcript",
		Data: newNoteGPTTranscriptData(
			"",
			"",
			"",
			nil,
			nil,
			duration,
		),
	}
}

func newSuccessNoteGPTStatusResponse(status string) noteGPTStatusResponse {
	return noteGPTStatusResponse{
		Code:    noteGPTSuccessCode,
		Message: "success",
		Data: noteGPTStatusData{
			Status: status,
		},
	}
}

func assertNoteGPTCallCounts(
	t *testing.T,
	calls *mockNoteGPTCalls,
	videoTranscriptCalls int32,
	transcriptGenerateCalls int32,
	mediaStatusCalls int32,
) {
	t.Helper()

	if got := calls.videoTranscriptCalls.Load(); got != videoTranscriptCalls {
		t.Fatalf("unexpected notegpt video transcript call count: %d", got)
	}

	if got := calls.transcriptGenerateCalls.Load(); got != transcriptGenerateCalls {
		t.Fatalf("unexpected notegpt transcript generate call count: %d", got)
	}

	if got := calls.mediaStatusCalls.Load(); got != mediaStatusCalls {
		t.Fatalf("unexpected notegpt media status call count: %d", got)
	}
}

func newMockNoteGPTServer(
	t *testing.T,
	videoID string,
	config mockNoteGPTConfig,
) (*httptest.Server, *mockNoteGPTCalls) {
	t.Helper()

	calls := new(mockNoteGPTCalls)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Helper()

		cookieHeader := request.Header.Get("Cookie")
		if !strings.Contains(cookieHeader, noteGPTAnonymousUserCookieName+"=") {
			t.Fatalf("missing anonymous user cookie: %q", cookieHeader)
		}

		queryValues := request.URL.Query()
		if platform := queryValues.Get("platform"); platform != noteGPTPlatformYouTube {
			t.Fatalf("unexpected notegpt platform: %q", platform)
		}

		if gotVideoID := queryValues.Get("video_id"); gotVideoID != videoID {
			t.Fatalf("unexpected notegpt video id: got %q want %q", gotVideoID, videoID)
		}

		switch request.URL.Path {
		case "/api/v2/video-transcript":
			responseIndex := int(calls.videoTranscriptCalls.Add(1)) - 1
			writeJSON(writer, noteGPTVideoTranscriptResponseAt(config.videoTranscriptResponses, responseIndex))
		case "/api/v2/transcript-generate":
			calls.transcriptGenerateCalls.Add(1)
			writeJSON(writer, defaultNoteGPTStatusResponse(config.transcriptGenerateResponse, noteGPTMediaStatusProcessing))
		case "/api/v2/media-status":
			responseIndex := int(calls.mediaStatusCalls.Add(1)) - 1
			writeJSON(writer, noteGPTStatusResponseAt(config.mediaStatusResponses, responseIndex))
		default:
			http.NotFound(writer, request)
		}
	}))

	return server, calls
}

func noteGPTVideoTranscriptResponseAt(
	responses []noteGPTVideoTranscriptResponse,
	index int,
) noteGPTVideoTranscriptResponse {
	if len(responses) == 0 {
		return noteGPTVideoTranscriptResponse{
			Code:    noteGPTSuccessCode,
			Message: "no transcript",
			Data:    newNoteGPTTranscriptData("", "", "", nil, nil, ""),
		}
	}

	if index >= len(responses) {
		index = len(responses) - 1
	}

	return responses[index]
}

func noteGPTStatusResponseAt(
	responses []noteGPTStatusResponse,
	index int,
) noteGPTStatusResponse {
	if len(responses) == 0 {
		return noteGPTStatusResponse{
			Code:    noteGPTSuccessCode,
			Message: "success",
			Data: noteGPTStatusData{
				Status: noteGPTMediaStatusSuccess,
			},
		}
	}

	if index >= len(responses) {
		index = len(responses) - 1
	}

	return responses[index]
}

func defaultNoteGPTStatusResponse(
	response noteGPTStatusResponse,
	defaultStatus string,
) noteGPTStatusResponse {
	if response.Code != 0 {
		return response
	}

	return noteGPTStatusResponse{
		Code:    noteGPTSuccessCode,
		Message: "success",
		Data: noteGPTStatusData{
			Status: defaultStatus,
		},
	}
}

func mockNoteGPTTranscriptData(videoID string) noteGPTVideoTranscriptData {
	return newNoteGPTTranscriptData(
		videoID,
		"Example Video",
		"Example Channel",
		[]noteGPTLanguageCode{
			newNoteGPTLanguageCode("es_auto_auto", "Spanish"),
			newNoteGPTLanguageCode("en_auto_auto", "English"),
		},
		map[string]noteGPTTranscriptTracks{
			"es_auto_auto": newNoteGPTTranscriptTracks(
				[]string{"hola", "mundo"},
				nil,
			),
			"en_auto_auto": newNoteGPTTranscriptTracks(
				[]string{"Hello", "world"},
				[]string{"Ignored custom transcript"},
			),
		},
		"",
	)
}

func newNoteGPTTranscriptData(
	videoID string,
	title string,
	author string,
	languageCodes []noteGPTLanguageCode,
	transcripts map[string]noteGPTTranscriptTracks,
	duration string,
) noteGPTVideoTranscriptData {
	return noteGPTVideoTranscriptData{
		VideoID: videoID,
		VideoInfo: noteGPTVideoInfo{
			Name:   title,
			Author: author,
		},
		LanguageCode: languageCodes,
		Transcripts:  transcripts,
		Duration:     duration,
	}
}

func newNoteGPTLanguageCode(code, name string) noteGPTLanguageCode {
	return noteGPTLanguageCode{
		Code: code,
		Name: name,
	}
}

func newNoteGPTTranscriptTracks(
	defaultTexts, customTexts []string,
) noteGPTTranscriptTracks {
	return noteGPTTranscriptTracks{
		Custom:  newNoteGPTTranscriptSegments(customTexts...),
		Default: newNoteGPTTranscriptSegments(defaultTexts...),
		Auto:    nil,
	}
}

func newNoteGPTTranscriptSegments(texts ...string) []noteGPTTranscriptSegment {
	segments := make([]noteGPTTranscriptSegment, 0, len(texts))
	for _, text := range texts {
		segments = append(segments, noteGPTTranscriptSegment{
			Start: "00:00:00",
			End:   "00:00:00",
			Text:  text,
		})
	}

	return segments
}

func newMockYouTubeCommentsServer(
	t *testing.T,
	youtubeAPIKey string,
	clientVersion string,
	initialToken string,
) (*httptest.Server, *int32) {
	t.Helper()

	var (
		nextCalls int32
		server    *httptest.Server
	)

	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleMockYouTubeCommentsRequest(
			t,
			writer,
			request,
			youtubeAPIKey,
			clientVersion,
			initialToken,
			&nextCalls,
		)
	}))

	return server, &nextCalls
}

func handleMockYouTubeCommentsRequest(
	t *testing.T,
	writer http.ResponseWriter,
	request *http.Request,
	youtubeAPIKey string,
	clientVersion string,
	initialToken string,
	nextCalls *int32,
) {
	t.Helper()

	switch request.URL.Path {
	case "/watch":
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte(mockYouTubeWatchHTML(youtubeAPIKey, clientVersion, initialToken)))
	case "/youtubei/v1/next":
		atomic.AddInt32(nextCalls, 1)

		payload := decodeRequestBody(t, request)
		continuation, _ := stringAt(payload, "continuation")

		clientName, _ := stringAt(payload, "context", "client", "clientName")
		if clientName != youtubeWebClientName {
			t.Fatalf("unexpected next client name: %q", clientName)
		}

		writeJSON(writer, mockYouTubeContinuationResponse(t, continuation))
	default:
		http.NotFound(writer, request)
	}
}

func decodeRequestBody(t *testing.T, request *http.Request) map[string]any {
	t.Helper()

	var payload map[string]any

	err := json.NewDecoder(request.Body).Decode(&payload)
	if err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	return payload
}

func mockYouTubeContinuationResponse(t *testing.T, continuation string) map[string]any {
	t.Helper()

	switch continuation {
	case "page-1":
		return mockYouTubeCommentsPageResponse("reloadContinuationItemsCommand", 1, 20, "page-2")
	case "page-2":
		return mockYouTubeCommentsPageResponse("appendContinuationItemsAction", 21, 40, "page-3")
	case "page-3":
		return mockYouTubeCommentsPageResponse("appendContinuationItemsAction", 41, 60, "")
	default:
		t.Fatalf("unexpected continuation token: %q", continuation)

		return nil
	}
}

func assertMockYouTubeResult(
	t *testing.T,
	result youtubeVideoContent,
	videoID string,
	expectedTitle string,
	expectedChannel string,
	expectedTranscript string,
) {
	t.Helper()

	if result.URL != "https://www.youtube.com/watch?v="+videoID {
		t.Fatalf("unexpected canonical url: %q", result.URL)
	}

	if result.Title != expectedTitle {
		t.Fatalf("unexpected title: %q", result.Title)
	}

	if result.ChannelName != expectedChannel {
		t.Fatalf("unexpected channel name: %q", result.ChannelName)
	}

	if result.Transcript != expectedTranscript {
		t.Fatalf("unexpected transcript: %q", result.Transcript)
	}
}

func mockYouTubeWatchHTML(apiKey, clientVersion, continuationToken string) string {
	initialData := map[string]any{
		"engagementPanels": []any{
			map[string]any{
				"engagementPanelSectionListRenderer": map[string]any{
					"panelIdentifier": youtubeCommentsPanelIdentifier,
					"content": map[string]any{
						"sectionListRenderer": map[string]any{
							"contents": []any{
								map[string]any{
									"itemSectionRenderer": map[string]any{
										"contents": []any{
											map[string]any{
												"continuationItemRenderer": map[string]any{
													"continuationEndpoint": map[string]any{
														"continuationCommand": map[string]any{
															"token": continuationToken,
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	initialDataBytes, err := json.Marshal(initialData)
	if err != nil {
		panic(err)
	}

	return `<!doctype html><html><head><script>` +
		`var ytInitialData = ` + string(initialDataBytes) + `;` +
		`</script></head><body>` +
		`"INNERTUBE_API_KEY":"` + apiKey + `"` +
		`"INNERTUBE_CLIENT_VERSION":"` + clientVersion + `"` +
		`</body></html>`
}

func mockYouTubeCommentsPageResponse(
	actionKey string,
	start int,
	end int,
	nextToken string,
) map[string]any {
	continuationItems := make([]any, 0, end-start+2)
	mutations := make([]any, 0, end-start+2)

	for commentNumber := start; commentNumber <= end; commentNumber++ {
		commentKey := "comment-" + strconv.Itoa(commentNumber)

		continuationItems = append(continuationItems, map[string]any{
			"commentThreadRenderer": map[string]any{
				"commentViewModel": map[string]any{
					"commentViewModel": map[string]any{
						"commentKey": commentKey,
					},
				},
			},
		})

		mutations = append(mutations, map[string]any{
			"entityKey": commentKey,
			"payload": map[string]any{
				"commentEntityPayload": map[string]any{
					"properties": map[string]any{
						"replyLevel": float64(0),
						"content": map[string]any{
							"content": "Comment " + strconv.Itoa(commentNumber),
						},
					},
					"author": map[string]any{
						"displayName": "Author " + strconv.Itoa(commentNumber),
					},
				},
			},
		})
	}

	mutations = append(mutations, map[string]any{
		"entityKey": "reply-comment",
		"payload": map[string]any{
			"commentEntityPayload": map[string]any{
				"properties": map[string]any{
					"replyLevel": float64(1),
					"content": map[string]any{
						"content": "Reply comment",
					},
				},
				"author": map[string]any{
					"displayName": "Reply author",
				},
			},
		},
	})

	if nextToken != "" {
		continuationItems = append(continuationItems, map[string]any{
			"continuationItemRenderer": map[string]any{
				"continuationEndpoint": map[string]any{
					"continuationCommand": map[string]any{
						"token": nextToken,
					},
				},
			},
		})
	}

	return map[string]any{
		"onResponseReceivedEndpoints": []any{
			map[string]any{
				actionKey: map[string]any{
					"continuationItems": continuationItems,
				},
			},
		},
		"frameworkUpdates": map[string]any{
			"entityBatchUpdate": map[string]any{
				"mutations": mutations,
			},
		},
	}
}

func writeJSON(writer http.ResponseWriter, payload any) {
	writer.Header().Set("Content-Type", "application/json")

	err := json.NewEncoder(writer).Encode(payload)
	if err != nil {
		panic(err)
	}
}
