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

func TestChatCompletionRouterRetriesOpenAIAPIKeysAfterStreamFailure(t *testing.T) {
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
						if !yield(newGeminiGenerateContentResponse("No", genai.FinishReasonUnspecified), nil) {
							return
						}

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
