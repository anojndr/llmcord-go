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

func newYouTubeTestBot(youtube youtubeContentClient) *bot {
	instance := new(bot)
	instance.youtube = youtube

	return instance
}

func TestExtractYouTubeURLsNormalizesAndDeduplicates(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"check https://youtu.be/dQw4w9WgXcQ?t=43",
		"and https://www.youtube.com/watch?v=dQw4w9WgXcQ,",
		"plus https://youtube.com/shorts/abc123def45!",
	}, " ")

	urls := extractYouTubeURLs(text)

	expected := []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://www.youtube.com/watch?v=abc123def45",
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

func TestMaybeAugmentConversationWithYouTubeIgnoresURLsOnlyPresentInPDFContent(t *testing.T) {
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

	assertURLAugmentationIgnoresPDFOnlyURLs(
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

func TestYouTubeClientFetchExtractsTranscriptAndLimitsCommentsToFifty(t *testing.T) {
	t.Parallel()

	const (
		videoID       = "abc123def45"
		youtubeAPIKey = "test-api-key"
		clientVersion = "2.20260309.01.00"
		initialToken  = "page-1"
	)

	server, nextCalls := newMockYouTubeServer(
		t,
		videoID,
		youtubeAPIKey,
		clientVersion,
		initialToken,
	)
	defer server.Close()

	client := youtubeClient{
		httpClient: server.Client(),
		watchURL:   server.URL + "/watch",
		apiBaseURL: server.URL + "/youtubei/v1",
		userAgent:  youtubeUserAgent,
	}

	result, err := client.fetch(context.Background(), "https://youtu.be/"+videoID+"?si=test")
	if err != nil {
		t.Fatalf("fetch youtube content: %v", err)
	}

	assertMockYouTubeResult(t, result, videoID)

	if nextCallCount := atomic.LoadInt32(nextCalls); nextCallCount != 3 {
		t.Fatalf("unexpected next call count: %d", nextCallCount)
	}
}

func newMockYouTubeServer(
	t *testing.T,
	videoID string,
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
		handleMockYouTubeRequest(
			t,
			writer,
			request,
			server.URL,
			videoID,
			youtubeAPIKey,
			clientVersion,
			initialToken,
			&nextCalls,
		)
	}))

	return server, &nextCalls
}

func handleMockYouTubeRequest(
	t *testing.T,
	writer http.ResponseWriter,
	request *http.Request,
	serverURL string,
	videoID string,
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
	case "/youtubei/v1/player":
		payload := decodeRequestBody(t, request)

		clientName, _ := stringAt(payload, "context", "client", "clientName")
		if clientName != youtubeAndroidClientName {
			t.Fatalf("unexpected player client name: %q", clientName)
		}

		writeJSON(writer, mockYouTubePlayerResponse(serverURL, videoID))
	case "/api/timedtext":
		writer.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = writer.Write([]byte(
			`<?xml version="1.0" encoding="utf-8"?><timedtext format="3"><body>` +
				`<p>Hello <s>world</s></p><p>Second line</p></body></timedtext>`,
		))
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

func mockYouTubePlayerResponse(serverURL string, videoID string) map[string]any {
	return map[string]any{
		"playabilityStatus": map[string]any{"status": "OK"},
		"videoDetails": map[string]any{
			"title":  "Example Video",
			"author": "Example Channel",
		},
		"captions": map[string]any{
			"playerCaptionsTracklistRenderer": map[string]any{
				"captionTracks": []any{
					map[string]any{
						"baseUrl":        serverURL + "/api/timedtext?v=" + videoID + "&lang=en",
						"languageCode":   "en",
						"isTranslatable": true,
					},
				},
				"translationLanguages": []any{
					map[string]any{"languageCode": "en"},
				},
			},
		},
	}
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

func assertMockYouTubeResult(t *testing.T, result youtubeVideoContent, videoID string) {
	t.Helper()

	if result.URL != "https://www.youtube.com/watch?v="+videoID {
		t.Fatalf("unexpected canonical url: %q", result.URL)
	}

	if result.Title != "Example Video" {
		t.Fatalf("unexpected title: %q", result.Title)
	}

	if result.ChannelName != "Example Channel" {
		t.Fatalf("unexpected channel name: %q", result.ChannelName)
	}

	if result.Transcript != "Hello world\nSecond line" {
		t.Fatalf("unexpected transcript: %q", result.Transcript)
	}

	if len(result.Comments) != 50 {
		t.Fatalf("unexpected comment count: %d", len(result.Comments))
	}

	if result.Comments[0].Author != "Author 1" || result.Comments[0].Text != "Comment 1" {
		t.Fatalf("unexpected first comment: %#v", result.Comments[0])
	}

	if result.Comments[49].Author != "Author 50" || result.Comments[49].Text != "Comment 50" {
		t.Fatalf("unexpected fiftieth comment: %#v", result.Comments[49])
	}
}

func mockYouTubeWatchHTML(apiKey string, clientVersion string, continuationToken string) string {
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
