package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"
)

const (
	testRetryPrimaryAPIKey     = "first-key"
	testRetryBackupAPIKey      = "second-key"
	testRetryPrimaryAuthHeader = "Bearer " + testRetryPrimaryAPIKey
	testRetryBackupAuthHeader  = "Bearer " + testRetryBackupAPIKey
)

type stringCapture struct {
	mutex  sync.Mutex
	values []string
}

func (capture *stringCapture) append(value string) {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()

	capture.values = append(capture.values, value)
}

func (capture *stringCapture) snapshot() []string {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()

	return append([]string(nil), capture.values...)
}

type countCapture struct {
	mutex sync.Mutex
	count int
}

func (capture *countCapture) increment() {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()

	capture.count++
}

func (capture *countCapture) value() int {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()

	return capture.count
}

type durationCapture struct {
	mutex  sync.Mutex
	values []time.Duration
}

func (capture *durationCapture) append(value time.Duration) {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()

	capture.values = append(capture.values, value)
}

func (capture *durationCapture) snapshot() []time.Duration {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()

	return append([]time.Duration(nil), capture.values...)
}

func TestChatCompletionRouterRetriesOpenAIAPIKeys(t *testing.T) {
	t.Parallel()

	authCapture := new(stringCapture)

	server := newOpenAIRetryTestServer(t, authCapture)
	defer server.Close()

	router := chatCompletionRouter{
		openAI:       newOpenAIClient(server.Client()),
		openAICodex:  newOpenAICodexClient(nil),
		gemini:       newGeminiClient(nil),
		waitForRetry: nil,
	}

	content, err := collectStreamedContent(
		context.Background(),
		router,
		newOpenAIRetryRequest(server.URL+"/v1"),
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if !slices.Equal(authCapture.snapshot(), []string{testRetryPrimaryAuthHeader, testRetryBackupAuthHeader}) {
		t.Fatalf("unexpected authorization attempts: %#v", authCapture.snapshot())
	}

	if content != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", content)
	}
}

func TestChatCompletionRouterRetriesOpenAIAPIKeysOnInternalServerError(t *testing.T) {
	t.Parallel()

	authCapture := new(stringCapture)

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		authHeader := request.Header.Get("Authorization")
		authCapture.append(authHeader)

		if authHeader == testRetryPrimaryAuthHeader {
			http.Error(responseWriter, "internal server error", http.StatusInternalServerError)

			return
		}

		streamOpenAIHello(t, responseWriter)
	}))
	defer server.Close()

	router := chatCompletionRouter{
		openAI:       newOpenAIClient(server.Client()),
		openAICodex:  newOpenAICodexClient(nil),
		gemini:       newGeminiClient(nil),
		waitForRetry: nil,
	}

	content, err := collectStreamedContent(
		context.Background(),
		router,
		newOpenAIRetryRequest(server.URL+"/v1"),
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if !slices.Equal(authCapture.snapshot(), []string{testRetryPrimaryAuthHeader, testRetryBackupAuthHeader}) {
		t.Fatalf("unexpected authorization attempts: %#v", authCapture.snapshot())
	}

	if content != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", content)
	}
}

func TestChatCompletionRouterWaitsForOpenAIRetryDelayBeforeFallbackKey(t *testing.T) {
	t.Parallel()

	authCapture := new(stringCapture)
	delayCapture := new(durationCapture)

	var (
		attemptsMu    sync.Mutex
		attemptsByKey = make(map[string]int)
	)

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		authHeader := request.Header.Get("Authorization")
		authCapture.append(authHeader)

		attemptsMu.Lock()
		attemptsByKey[authHeader]++
		attempt := attemptsByKey[authHeader]
		attemptsMu.Unlock()

		switch {
		case authHeader == testRetryPrimaryAuthHeader && attempt == 1:
			responseWriter.Header().Set(openAIRateLimitRemainingRequests, "0")
			responseWriter.Header().Set(openAIRateLimitResetRequests, "1500ms")
			http.Error(responseWriter, "rate limited", http.StatusTooManyRequests)
		case authHeader == testRetryPrimaryAuthHeader:
			http.Error(responseWriter, "permission denied", http.StatusForbidden)
		default:
			streamOpenAIHello(t, responseWriter)
		}
	}))
	defer server.Close()

	router := chatCompletionRouter{
		openAI:      newOpenAIClient(server.Client()),
		openAICodex: newOpenAICodexClient(nil),
		gemini:      newGeminiClient(nil),
		waitForRetry: func(_ context.Context, delay time.Duration) error {
			delayCapture.append(delay)

			return nil
		},
	}

	content, err := collectStreamedContent(
		context.Background(),
		router,
		newOpenAIRetryRequest(server.URL+"/v1"),
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if !slices.Equal(
		authCapture.snapshot(),
		[]string{testRetryPrimaryAuthHeader, testRetryPrimaryAuthHeader, testRetryBackupAuthHeader},
	) {
		t.Fatalf("unexpected authorization attempts: %#v", authCapture.snapshot())
	}

	if !slices.Equal(delayCapture.snapshot(), []time.Duration{1500 * time.Millisecond}) {
		t.Fatalf("unexpected retry delays: %#v", delayCapture.snapshot())
	}

	if content != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", content)
	}
}

func TestChatCompletionRouterSkipsLongOpenAIRetryDelayBeforeFallbackKey(t *testing.T) {
	t.Parallel()

	authCapture := new(stringCapture)
	delayCapture := new(durationCapture)
	longRetryDelay := sameKeyRetryDelayLimit + time.Second

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		authHeader := request.Header.Get("Authorization")
		authCapture.append(authHeader)

		if authHeader == testRetryPrimaryAuthHeader {
			responseWriter.Header().Set(openAIRateLimitRemainingRequests, "0")
			responseWriter.Header().Set(openAIRateLimitResetRequests, longRetryDelay.String())
			http.Error(responseWriter, "rate limited", http.StatusTooManyRequests)

			return
		}

		streamOpenAIHello(t, responseWriter)
	}))
	defer server.Close()

	router := chatCompletionRouter{
		openAI:      newOpenAIClient(server.Client()),
		openAICodex: newOpenAICodexClient(nil),
		gemini:      newGeminiClient(nil),
		waitForRetry: func(_ context.Context, delay time.Duration) error {
			delayCapture.append(delay)

			return nil
		},
	}

	content, err := collectStreamedContent(
		context.Background(),
		router,
		newOpenAIRetryRequest(server.URL+"/v1"),
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if !slices.Equal(authCapture.snapshot(), []string{testRetryPrimaryAuthHeader, testRetryBackupAuthHeader}) {
		t.Fatalf("unexpected authorization attempts: %#v", authCapture.snapshot())
	}

	if len(delayCapture.snapshot()) != 0 {
		t.Fatalf("unexpected retry delays: %#v", delayCapture.snapshot())
	}

	if content != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", content)
	}
}

func TestChatCompletionRouterDoesNotRetryOpenAIAPIKeysAfterPartialStream(t *testing.T) {
	t.Parallel()

	authCapture := new(stringCapture)

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		authHeader := request.Header.Get("Authorization")
		authCapture.append(authHeader)

		if authHeader == testRetryPrimaryAuthHeader {
			streamOpenAIPartialHello(t, responseWriter)

			return
		}

		streamOpenAIHello(t, responseWriter)
	}))
	defer server.Close()

	router := chatCompletionRouter{
		openAI:       newOpenAIClient(server.Client()),
		openAICodex:  newOpenAICodexClient(nil),
		gemini:       newGeminiClient(nil),
		waitForRetry: nil,
	}

	var joinedContent strings.Builder

	err := router.streamChatCompletion(
		context.Background(),
		newOpenAIRetryRequest(server.URL+"/v1"),
		func(delta streamDelta) error {
			joinedContent.WriteString(delta.Content)

			return nil
		},
	)
	if err == nil {
		t.Fatal("expected partial stream failure")
	}

	if !slices.Equal(authCapture.snapshot(), []string{testRetryPrimaryAuthHeader}) {
		t.Fatalf("unexpected authorization attempts: %#v", authCapture.snapshot())
	}

	if joinedContent.String() != "Hel" {
		t.Fatalf("unexpected streamed content: %q", joinedContent.String())
	}

	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("unexpected stream error: %v", err)
	}
}

func TestChatCompletionRouterRetriesOpenAICodexAPIKeys(t *testing.T) {
	t.Parallel()

	validAPIKey := testOpenAICodexJWT(t)
	requestCounter := new(countCapture)

	server := newOpenAICodexRetryTestServer(t, requestCounter, validAPIKey)
	defer server.Close()

	router := chatCompletionRouter{
		openAI:       newOpenAIClient(nil),
		openAICodex:  newOpenAICodexClient(server.Client()),
		gemini:       newGeminiClient(nil),
		waitForRetry: nil,
	}

	content, err := collectStreamedContent(
		context.Background(),
		router,
		newOpenAICodexRetryRequest(server.URL+"/backend-api", validAPIKey),
	)
	if err != nil {
		t.Fatalf("stream codex completion: %v", err)
	}

	if requestCounter.value() != 1 {
		t.Fatalf("unexpected codex request count: %d", requestCounter.value())
	}

	if content != testOpenAICodexHelloText {
		t.Fatalf("unexpected streamed content: %q", content)
	}
}

func TestChatCompletionRouterWaitsForOpenAICodexRetryDelayBeforeFallbackKey(t *testing.T) {
	t.Parallel()

	primaryAPIKey := testOpenAICodexJWTWithSignature(t, "primary")
	backupAPIKey := testOpenAICodexJWTWithSignature(t, "backup")
	delayCapture := new(durationCapture)
	authCapture := new(stringCapture)

	var (
		attemptsMu    sync.Mutex
		attemptsByKey = make(map[string]int)
	)

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		authHeader := request.Header.Get("Authorization")
		authCapture.append(authHeader)

		attemptsMu.Lock()
		attemptsByKey[authHeader]++
		attempt := attemptsByKey[authHeader]
		attemptsMu.Unlock()

		switch {
		case authHeader == "Bearer "+primaryAPIKey && attempt == 1:
			responseWriter.Header().Set("Content-Type", "application/json")
			responseWriter.Header().Set(openAIRateLimitRemainingRequests, "0")
			responseWriter.Header().Set(openAIRateLimitResetRequests, "2s")
			responseWriter.WriteHeader(http.StatusTooManyRequests)
			writeStreamChunk(
				t,
				responseWriter,
				`{"error":{"message":"Rate limited","type":"server_error","code":"rate_limit_exceeded"}}`,
			)
		case authHeader == "Bearer "+primaryAPIKey:
			http.Error(responseWriter, "permission denied", http.StatusForbidden)
		default:
			streamOpenAICodexHello(t, responseWriter)
		}
	}))
	defer server.Close()

	router := chatCompletionRouter{
		openAI:      newOpenAIClient(nil),
		openAICodex: newOpenAICodexClient(server.Client()),
		gemini:      newGeminiClient(nil),
		waitForRetry: func(_ context.Context, delay time.Duration) error {
			delayCapture.append(delay)

			return nil
		},
	}

	request := newOpenAICodexRetryRequest(server.URL+"/backend-api", primaryAPIKey)
	request.Provider.APIKeys = []string{primaryAPIKey, backupAPIKey}

	content, err := collectStreamedContent(
		context.Background(),
		router,
		request,
	)
	if err != nil {
		t.Fatalf("stream codex completion: %v", err)
	}

	if !slices.Equal(
		authCapture.snapshot(),
		[]string{"Bearer " + primaryAPIKey, "Bearer " + primaryAPIKey, "Bearer " + backupAPIKey},
	) {
		t.Fatalf("unexpected codex authorization attempts: %#v", authCapture.snapshot())
	}

	if !slices.Equal(delayCapture.snapshot(), []time.Duration{2 * time.Second}) {
		t.Fatalf("unexpected retry delays: %#v", delayCapture.snapshot())
	}

	if content != testOpenAICodexHelloText {
		t.Fatalf("unexpected streamed content: %q", content)
	}
}

func TestChatCompletionRouterSkipsLongOpenAICodexRetryDelayBeforeFallbackKey(t *testing.T) {
	t.Parallel()

	primaryAPIKey := testOpenAICodexJWTWithSignature(t, "primary")
	backupAPIKey := testOpenAICodexJWTWithSignature(t, "backup")
	delayCapture := new(durationCapture)
	authCapture := new(stringCapture)
	longRetryDelay := sameKeyRetryDelayLimit + time.Second

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		authHeader := request.Header.Get("Authorization")
		authCapture.append(authHeader)

		if authHeader == "Bearer "+primaryAPIKey {
			responseWriter.Header().Set("Content-Type", "application/json")
			responseWriter.Header().Set(openAIRateLimitRemainingRequests, "0")
			responseWriter.Header().Set(openAIRateLimitResetRequests, longRetryDelay.String())
			responseWriter.WriteHeader(http.StatusTooManyRequests)
			writeStreamChunk(
				t,
				responseWriter,
				`{"error":{"message":"Rate limited","type":"server_error","code":"rate_limit_exceeded"}}`,
			)

			return
		}

		streamOpenAICodexHello(t, responseWriter)
	}))
	defer server.Close()

	router := chatCompletionRouter{
		openAI:      newOpenAIClient(nil),
		openAICodex: newOpenAICodexClient(server.Client()),
		gemini:      newGeminiClient(nil),
		waitForRetry: func(_ context.Context, delay time.Duration) error {
			delayCapture.append(delay)

			return nil
		},
	}

	request := newOpenAICodexRetryRequest(server.URL+"/backend-api", primaryAPIKey)
	request.Provider.APIKeys = []string{primaryAPIKey, backupAPIKey}

	content, err := collectStreamedContent(
		context.Background(),
		router,
		request,
	)
	if err != nil {
		t.Fatalf("stream codex completion: %v", err)
	}

	if !slices.Equal(authCapture.snapshot(), []string{"Bearer " + primaryAPIKey, "Bearer " + backupAPIKey}) {
		t.Fatalf("unexpected codex authorization attempts: %#v", authCapture.snapshot())
	}

	if len(delayCapture.snapshot()) != 0 {
		t.Fatalf("unexpected retry delays: %#v", delayCapture.snapshot())
	}

	if content != testOpenAICodexHelloText {
		t.Fatalf("unexpected streamed content: %q", content)
	}
}

func TestChatCompletionRouterStreamsOpenAICodexImmediately(t *testing.T) {
	t.Parallel()

	validAPIKey := testOpenAICodexJWT(t)
	releaseStream, release := newReleaseSignal()
	firstDeltaSeen := make(chan struct{})
	chunkCapture := new(stringCapture)

	server := newBlockingOpenAICodexStreamServer(t, validAPIKey, releaseStream)
	defer server.Close()
	defer release()

	router := chatCompletionRouter{
		openAI:       newOpenAIClient(nil),
		openAICodex:  newOpenAICodexClient(server.Client()),
		gemini:       newGeminiClient(nil),
		waitForRetry: nil,
	}

	request := newOpenAICodexRetryRequest(server.URL+"/backend-api", validAPIKey)
	request.Provider.APIKeys = []string{validAPIKey}

	streamErr := startStreamingContentCapture(
		context.Background(),
		router,
		request,
		chunkCapture,
		firstDeltaSeen,
	)

	waitForFirstStreamedDelta(t, "codex", firstDeltaSeen)

	if !slices.Equal(chunkCapture.snapshot(), []string{"Hel"}) {
		t.Fatalf("unexpected codex chunks before release: %#v", chunkCapture.snapshot())
	}

	release()

	err := <-streamErr
	if err != nil {
		t.Fatalf("stream codex completion: %v", err)
	}

	if strings.Join(chunkCapture.snapshot(), "") != testOpenAICodexHelloText {
		t.Fatalf("unexpected streamed codex content: %q", strings.Join(chunkCapture.snapshot(), ""))
	}
}

func TestChatCompletionRouterRetriesSingleGeminiAPIKeyAfterRetryDelay(t *testing.T) {
	t.Parallel()

	attemptCapture := new(stringCapture)
	delayCapture := new(durationCapture)

	router := newGeminiRetryRouterWithFactory(
		attemptCapture,
		delayCapture,
		func(_ string, attempt int) func(
			context.Context,
			string,
			[]*genai.Content,
			*genai.GenerateContentConfig,
		) iter.Seq2[*genai.GenerateContentResponse, error] {
			return func(
				_ context.Context,
				_ string,
				_ []*genai.Content,
				_ *genai.GenerateContentConfig,
			) iter.Seq2[*genai.GenerateContentResponse, error] {
				return func(yield func(*genai.GenerateContentResponse, error) bool) {
					if attempt == 0 {
						_ = yield(
							nil,
							newTestGeminiRetryDelayError("Please retry in 47.453198619s.", "47s"),
						)

						return
					}

					if !yield(newGeminiGenerateContentResponse("Hel", genai.FinishReasonUnspecified), nil) {
						return
					}

					_ = yield(newGeminiGenerateContentResponse("lo", genai.FinishReasonStop), nil)
				}
			}
		},
	)

	content, err := collectStreamedContent(
		context.Background(),
		router,
		newSimpleGeminiStreamRequest(),
	)
	if err != nil {
		t.Fatalf("stream gemini completion: %v", err)
	}

	if !slices.Equal(attemptCapture.snapshot(), []string{"gemini-key", "gemini-key"}) {
		t.Fatalf("unexpected gemini API key attempts: %#v", attemptCapture.snapshot())
	}

	if !slices.Equal(delayCapture.snapshot(), []time.Duration{47*time.Second + 453198619*time.Nanosecond}) {
		t.Fatalf("unexpected retry delays: %#v", delayCapture.snapshot())
	}

	if content != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", content)
	}
}

func TestChatCompletionRouterWaitsForGeminiRetryDelayBeforeFallbackKey(t *testing.T) {
	t.Parallel()

	attemptCapture := new(stringCapture)
	delayCapture := new(durationCapture)

	router := newGeminiRetryRouterWithFactory(
		attemptCapture,
		delayCapture,
		func(apiKey string, attempt int) func(
			context.Context,
			string,
			[]*genai.Content,
			*genai.GenerateContentConfig,
		) iter.Seq2[*genai.GenerateContentResponse, error] {
			return func(
				_ context.Context,
				_ string,
				_ []*genai.Content,
				_ *genai.GenerateContentConfig,
			) iter.Seq2[*genai.GenerateContentResponse, error] {
				return func(yield func(*genai.GenerateContentResponse, error) bool) {
					switch {
					case apiKey == testRetryPrimaryAPIKey && attempt == 0:
						_ = yield(nil, newTestGeminiRetryDelayError("Please retry in 47.453198619s.", "47s"))
					case apiKey == testRetryPrimaryAPIKey:
						_ = yield(nil, newTestGeminiAPIError(http.StatusForbidden, "permission denied"))
					default:
						if !yield(newGeminiGenerateContentResponse("Hel", genai.FinishReasonUnspecified), nil) {
							return
						}

						_ = yield(newGeminiGenerateContentResponse("lo", genai.FinishReasonStop), nil)
					}
				}
			}
		},
	)

	content, err := collectStreamedContent(
		context.Background(),
		router,
		newGeminiRetryRequest(),
	)
	if err != nil {
		t.Fatalf("stream gemini completion: %v", err)
	}

	if !slices.Equal(
		attemptCapture.snapshot(),
		[]string{testRetryPrimaryAPIKey, testRetryPrimaryAPIKey, testRetryBackupAPIKey},
	) {
		t.Fatalf("unexpected gemini API key attempts: %#v", attemptCapture.snapshot())
	}

	if !slices.Equal(delayCapture.snapshot(), []time.Duration{47*time.Second + 453198619*time.Nanosecond}) {
		t.Fatalf("unexpected retry delays: %#v", delayCapture.snapshot())
	}

	if content != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", content)
	}
}

func TestChatCompletionRouterSkipsLongGeminiRetryDelayBeforeFallbackKey(t *testing.T) {
	t.Parallel()

	attemptCapture := new(stringCapture)
	delayCapture := new(durationCapture)
	longRetryDelay := sameKeyRetryDelayLimit + time.Second

	router := newGeminiRetryRouterWithFactory(
		attemptCapture,
		delayCapture,
		func(apiKey string, _ int) func(
			context.Context,
			string,
			[]*genai.Content,
			*genai.GenerateContentConfig,
		) iter.Seq2[*genai.GenerateContentResponse, error] {
			return func(
				_ context.Context,
				_ string,
				_ []*genai.Content,
				_ *genai.GenerateContentConfig,
			) iter.Seq2[*genai.GenerateContentResponse, error] {
				return func(yield func(*genai.GenerateContentResponse, error) bool) {
					if apiKey == testRetryPrimaryAPIKey {
						_ = yield(
							nil,
							newTestGeminiRetryDelayError(
								fmt.Sprintf("Please retry in %ds.", int(longRetryDelay/time.Second)),
								longRetryDelay.String(),
							),
						)

						return
					}

					if !yield(newGeminiGenerateContentResponse("Hel", genai.FinishReasonUnspecified), nil) {
						return
					}

					_ = yield(newGeminiGenerateContentResponse("lo", genai.FinishReasonStop), nil)
				}
			}
		},
	)

	content, err := collectStreamedContent(
		context.Background(),
		router,
		newGeminiRetryRequest(),
	)
	if err != nil {
		t.Fatalf("stream gemini completion: %v", err)
	}

	if !slices.Equal(
		attemptCapture.snapshot(),
		[]string{testRetryPrimaryAPIKey, testRetryBackupAPIKey},
	) {
		t.Fatalf("unexpected gemini API key attempts: %#v", attemptCapture.snapshot())
	}

	if len(delayCapture.snapshot()) != 0 {
		t.Fatalf("unexpected retry delays: %#v", delayCapture.snapshot())
	}

	if content != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", content)
	}
}

func TestChatCompletionRouterStreamsGeminiImmediately(t *testing.T) {
	t.Parallel()

	releaseStream, release := newReleaseSignal()
	firstDeltaSeen := make(chan struct{})
	chunkCapture := new(stringCapture)

	router := newBlockingGeminiStreamRouter(releaseStream)

	defer release()

	streamErr := startStreamingContentCapture(
		context.Background(),
		router,
		newSimpleGeminiStreamRequest(),
		chunkCapture,
		firstDeltaSeen,
	)

	waitForFirstStreamedDelta(t, "gemini", firstDeltaSeen)

	if !slices.Equal(chunkCapture.snapshot(), []string{"Hel"}) {
		t.Fatalf("unexpected gemini chunks before release: %#v", chunkCapture.snapshot())
	}

	release()

	err := <-streamErr
	if err != nil {
		t.Fatalf("stream gemini completion: %v", err)
	}

	if strings.Join(chunkCapture.snapshot(), "") != testStreamedHelloText {
		t.Fatalf("unexpected streamed gemini content: %q", strings.Join(chunkCapture.snapshot(), ""))
	}
}

func newReleaseSignal() (chan struct{}, func()) {
	releaseStream := make(chan struct{})

	var releaseOnce sync.Once

	release := func() {
		releaseOnce.Do(func() {
			close(releaseStream)
		})
	}

	return releaseStream, release
}

func newBlockingOpenAICodexStreamServer(
	t *testing.T,
	apiKey string,
	releaseStream <-chan struct{},
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		assertOpenAICodexRequest(t, request, apiKey, "")
		responseWriter.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := responseWriter.(http.Flusher)
		if !ok {
			t.Fatal("expected response writer to support flushing")
		}

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n",
		)
		flusher.Flush()

		<-releaseStream

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n",
		)
		flusher.Flush()
		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
		)
		flusher.Flush()
		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func newBlockingGeminiStreamRouter(releaseStream <-chan struct{}) chatCompletionRouter {
	return chatCompletionRouter{
		openAI:      newOpenAIClient(nil),
		openAICodex: newOpenAICodexClient(nil),
		waitForRetry: func(context.Context, time.Duration) error {
			return nil
		},
		gemini: geminiClient{
			httpClient: new(http.Client),
			newClient: func(
				_ context.Context,
				_ *genai.ClientConfig,
			) (geminiAPIClient, error) {
				return stubGeminiAPIClient{
					generateContentStream: func(
						_ context.Context,
						_ string,
						_ []*genai.Content,
						_ *genai.GenerateContentConfig,
					) iter.Seq2[*genai.GenerateContentResponse, error] {
						return func(yield func(*genai.GenerateContentResponse, error) bool) {
							if !yield(newGeminiGenerateContentResponse("Hel", genai.FinishReasonUnspecified), nil) {
								return
							}

							<-releaseStream

							_ = yield(newGeminiGenerateContentResponse("lo", genai.FinishReasonStop), nil)
						}
					},
					uploadFile: nil,
					getFile:    nil,
				}, nil
			},
		},
	}
}

func startStreamingContentCapture(
	ctx context.Context,
	router chatCompletionRouter,
	request chatCompletionRequest,
	chunkCapture *stringCapture,
	firstDeltaSeen chan struct{},
) <-chan error {
	streamErr := make(chan error, 1)

	var firstDeltaOnce sync.Once

	go func() {
		streamErr <- router.streamChatCompletion(ctx, request, func(delta streamDelta) error {
			if delta.Content == "" {
				return nil
			}

			chunkCapture.append(delta.Content)
			firstDeltaOnce.Do(func() {
				close(firstDeltaSeen)
			})

			return nil
		})
	}()

	return streamErr
}

func waitForFirstStreamedDelta(t *testing.T, provider string, firstDeltaSeen <-chan struct{}) {
	t.Helper()

	select {
	case <-firstDeltaSeen:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected %s delta before stream completion", provider)
	}
}

func TestChatCompletionRouterRetriesGeminiAPIKeys(t *testing.T) {
	t.Parallel()

	attemptCapture := new(stringCapture)
	router := newGeminiRetryRouter(attemptCapture)

	content, err := collectStreamedContent(
		context.Background(),
		router,
		newGeminiRetryRequest(),
	)
	if err != nil {
		t.Fatalf("stream gemini completion: %v", err)
	}

	if !slices.Equal(attemptCapture.snapshot(), []string{testRetryPrimaryAPIKey, testRetryBackupAPIKey}) {
		t.Fatalf("unexpected gemini API key attempts: %#v", attemptCapture.snapshot())
	}

	if content != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", content)
	}
}

func collectStreamedContent(
	ctx context.Context,
	router chatCompletionRouter,
	request chatCompletionRequest,
) (string, error) {
	var joinedContent strings.Builder

	err := router.streamChatCompletion(ctx, request, func(delta streamDelta) error {
		joinedContent.WriteString(delta.Content)

		return nil
	})
	if err != nil {
		return "", err
	}

	return joinedContent.String(), nil
}

func newOpenAIRetryTestServer(
	t *testing.T,
	authCapture *stringCapture,
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		authHeader := request.Header.Get("Authorization")
		authCapture.append(authHeader)

		if authHeader == testRetryPrimaryAuthHeader {
			http.Error(responseWriter, "rate limited", http.StatusTooManyRequests)

			return
		}

		streamOpenAIHello(t, responseWriter)
	}))
}

func newOpenAIRetryRequest(baseURL string) chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:      providerAPIKindOpenAI,
			BaseURL:      baseURL,
			APIKey:       "",
			APIKeys:      []string{testRetryPrimaryAPIKey, testRetryBackupAPIKey},
			ExtraHeaders: nil,
			ExtraQuery:   nil,
			ExtraBody:    nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		ContextWindow:   0,
		SessionID:       "",
		Messages:        []chatMessage{{Role: messageRoleUser, Content: "hello"}},
	}
}

func streamOpenAIHello(t *testing.T, responseWriter http.ResponseWriter) {
	t.Helper()

	responseWriter.Header().Set("Content-Type", "text/event-stream")

	flusher, ok := responseWriter.(http.Flusher)
	if !ok {
		t.Fatal("expected response writer to support flushing")
	}

	writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
	flusher.Flush()
	writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
	flusher.Flush()
	writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	flusher.Flush()
	writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
	flusher.Flush()
}

func streamOpenAIPartialHello(t *testing.T, responseWriter http.ResponseWriter) {
	t.Helper()

	responseWriter.Header().Set("Content-Type", "text/event-stream")

	flusher, ok := responseWriter.(http.Flusher)
	if !ok {
		t.Fatal("expected response writer to support flushing")
	}

	writeStreamChunk(t, responseWriter, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
	flusher.Flush()
}

func newOpenAICodexRetryTestServer(
	t *testing.T,
	requestCounter *countCapture,
	validAPIKey string,
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		requestCounter.increment()
		assertOpenAICodexRequest(t, request, validAPIKey, "")
		streamOpenAICodexHello(t, responseWriter)
	}))
}

func newOpenAICodexRetryRequest(baseURL string, validAPIKey string) chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:      providerAPIKindOpenAICodex,
			BaseURL:      baseURL,
			APIKey:       "",
			APIKeys:      []string{"not-a-jwt", validAPIKey},
			ExtraHeaders: map[string]any{"X-Test": testOpenAICodexHeaderValue},
			ExtraQuery:   map[string]any{"feature": "enabled"},
			ExtraBody:    map[string]any{"verbosity": "high", "reasoning_effort": "medium"},
		},
		Model:           testOpenAICodexModel,
		ConfiguredModel: "",
		ContextWindow:   0,
		SessionID:       "",
		Messages: []chatMessage{
			{Role: openAICodexRoleSystem, Content: "Be brief."},
			{
				Role: messageRoleUser,
				Content: []contentPart{
					{"type": contentTypeText, "text": testOpenAICodexHelloText},
					{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
				},
			},
			{Role: messageRoleAssistant, Content: "Previous answer"},
		},
	}
}

func streamOpenAICodexHello(t *testing.T, responseWriter http.ResponseWriter) {
	t.Helper()

	responseWriter.Header().Set("Content-Type", "text/event-stream")

	flusher, ok := responseWriter.(http.Flusher)
	if !ok {
		t.Fatal("expected response writer to support flushing")
	}

	writeStreamChunk(
		t,
		responseWriter,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n",
	)
	flusher.Flush()
	writeStreamChunk(
		t,
		responseWriter,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n",
	)
	flusher.Flush()
	writeStreamChunk(
		t,
		responseWriter,
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
	)
	flusher.Flush()
	writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
	flusher.Flush()
}

func newGeminiRetryRouter(attemptCapture *stringCapture) chatCompletionRouter {
	return newGeminiRetryRouterWithFactory(
		attemptCapture,
		nil,
		func(apiKey string, _ int) func(
			context.Context,
			string,
			[]*genai.Content,
			*genai.GenerateContentConfig,
		) iter.Seq2[*genai.GenerateContentResponse, error] {
			return newGeminiRetryStream(apiKey)
		},
	)
}

type geminiRetryStreamFactory func(
	string,
	int,
) func(
	context.Context,
	string,
	[]*genai.Content,
	*genai.GenerateContentConfig,
) iter.Seq2[*genai.GenerateContentResponse, error]

func newGeminiRetryRouterWithFactory(
	attemptCapture *stringCapture,
	delayCapture *durationCapture,
	streamFactory geminiRetryStreamFactory,
) chatCompletionRouter {
	var (
		attemptsMu    sync.Mutex
		attemptsByKey = make(map[string]int)
	)

	return chatCompletionRouter{
		openAI:      newOpenAIClient(nil),
		openAICodex: newOpenAICodexClient(nil),
		waitForRetry: func(_ context.Context, delay time.Duration) error {
			if delayCapture != nil {
				delayCapture.append(delay)
			}

			return nil
		},
		gemini: geminiClient{
			httpClient: new(http.Client),
			newClient: func(
				_ context.Context,
				config *genai.ClientConfig,
			) (geminiAPIClient, error) {
				attemptCapture.append(config.APIKey)

				attemptsMu.Lock()
				attempt := attemptsByKey[config.APIKey]
				attemptsByKey[config.APIKey] = attempt + 1
				attemptsMu.Unlock()

				var stubClient stubGeminiAPIClient

				stubClient.generateContentStream = streamFactory(config.APIKey, attempt)

				return stubClient, nil
			},
		},
	}
}

func newGeminiRetryRequest() chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:      providerAPIKindGemini,
			BaseURL:      "",
			APIKey:       "",
			APIKeys:      []string{testRetryPrimaryAPIKey, testRetryBackupAPIKey},
			ExtraHeaders: nil,
			ExtraQuery:   nil,
			ExtraBody:    nil,
		},
		Model:           "gemini-3-flash-preview",
		ConfiguredModel: "",
		ContextWindow:   0,
		SessionID:       "",
		Messages:        []chatMessage{{Role: messageRoleUser, Content: "hello"}},
	}
}

func newGeminiRetryStream(apiKey string) func(
	context.Context,
	string,
	[]*genai.Content,
	*genai.GenerateContentConfig,
) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(
		_ context.Context,
		_ string,
		_ []*genai.Content,
		_ *genai.GenerateContentConfig,
	) iter.Seq2[*genai.GenerateContentResponse, error] {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			if apiKey == testRetryPrimaryAPIKey {
				apiErr := new(genai.APIError)
				apiErr.Code = http.StatusForbidden
				apiErr.Message = "permission denied"

				_ = yield(nil, *apiErr)

				return
			}

			if !yield(newGeminiGenerateContentResponse("Hel", genai.FinishReasonUnspecified), nil) {
				return
			}

			_ = yield(newGeminiGenerateContentResponse("lo", genai.FinishReasonStop), nil)
		}
	}
}

func newTestGeminiRetryDelayError(message string, retryDelay string) error {
	apiErr := new(genai.APIError)
	apiErr.Code = http.StatusTooManyRequests
	apiErr.Message = message
	apiErr.Status = "RESOURCE_EXHAUSTED"
	apiErr.Details = []map[string]any{{
		"@type":      geminiRetryInfoType,
		"retryDelay": retryDelay,
	}}

	return *apiErr
}

func testOpenAICodexJWTWithSignature(t *testing.T, signature string) string {
	t.Helper()

	headerBytes, err := json.Marshal(map[string]any{
		"alg": "HS256",
		"typ": "JWT",
	})
	if err != nil {
		t.Fatalf("marshal jwt header: %v", err)
	}

	payloadBytes, err := json.Marshal(map[string]any{
		openAICodexJWTClaimPath: map[string]any{
			"chatgpt_account_id": testOpenAICodexAccountID,
		},
	})
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}

	encode := func(data []byte) string {
		return base64.RawURLEncoding.EncodeToString(data)
	}

	return fmt.Sprintf("%s.%s.%s", encode(headerBytes), encode(payloadBytes), signature)
}
