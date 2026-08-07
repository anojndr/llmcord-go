package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

var errStubUnreachable = errors.New("stub should not be reached")

func stubCreateCacheError() error {
	return fmt.Errorf("cache backend unavailable: %w", errStubUnreachable)
}

func stubCachedContent() *genai.CachedContent {
	cachedContent := new(genai.CachedContent)
	cachedContent.Name = "cachedContents/test"

	return cachedContent
}

// stubGeminiCachesClient implements geminiCacheClient with call recording.
type stubGeminiCachesClient struct {
	create                func(context.Context, string, *genai.CreateCachedContentConfig) (*genai.CachedContent, error)
	generateContentStream func(
		context.Context,
		string,
		[]*genai.Content,
		*genai.GenerateContentConfig,
	) iter.Seq2[*genai.GenerateContentResponse, error]
}

func (client stubGeminiCachesClient) CreateCachedContent(
	ctx context.Context,
	model string,
	config *genai.CreateCachedContentConfig,
) (*genai.CachedContent, error) {
	if client.create == nil {
		panic("unexpected CreateCachedContent call")
	}

	return client.create(ctx, model, config)
}

func (client stubGeminiCachesClient) GenerateContentStream(
	ctx context.Context,
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
) iter.Seq2[*genai.GenerateContentResponse, error] {
	if client.generateContentStream == nil {
		panic("unexpected GenerateContentStream call")
	}

	return client.generateContentStream(ctx, model, contents, config)
}

func (client stubGeminiCachesClient) UploadFile(
	_ context.Context,
	_ io.Reader,
	_ *genai.UploadFileConfig,
) (*genai.File, error) {
	panic("unexpected UploadFile call")
}

func (client stubGeminiCachesClient) GetFile(
	_ context.Context,
	_ string,
	_ *genai.GetFileConfig,
) (*genai.File, error) {
	panic("unexpected GetFile call")
}

func newSimpleGeminiCacheRequest() chatCompletionRequest {
	request := newSimpleGeminiStreamRequest()
	request.Model = "gemini-3.6-flash"
	request.ConfiguredModel = "gemini/gemini-3.6-flash"
	request.Messages = []chatMessage{
		{Role: messageRoleUser, Content: strings.Repeat("a", 8_000)},
		{Role: messageRoleUser, Content: "latest question"},
	}

	return request
}

func TestGeminiCacheRequestCreatesCachedContentAndSetsConfig(t *testing.T) {
	t.Parallel()

	var (
		createdModel  string
		createdConfig *genai.CreateCachedContentConfig
	)

	apiClient := new(stubGeminiCachesClient)

	apiClient.create = func(
		_ context.Context,
		model string,
		config *genai.CreateCachedContentConfig,
	) (*genai.CachedContent, error) {
		createdModel = model
		createdConfig = config

		return stubCachedContent(), nil
	}

	request := newSimpleGeminiCacheRequest()

	contents, config, err := buildGeminiGenerateContentRequestWithCaching(
		context.Background(),
		request,
		apiClient,
		apiClient,
	)
	if err != nil {
		t.Fatalf("build gemini request with caching: %v", err)
	}

	if len(contents) != 2 {
		t.Fatalf("expected 2 contents, got %d", len(contents))
	}

	if config == nil || config.CachedContent != "cachedContents/test" {
		t.Fatalf("expected cached content name in config, got %#v", config)
	}

	if createdModel != "gemini-3.6-flash" {
		t.Fatalf("unexpected cache model: %q", createdModel)
	}

	if createdConfig == nil || createdConfig.DisplayName == "" {
		t.Fatalf("expected cache display name: %#v", createdConfig)
	}

	if createdConfig == nil || createdConfig.TTL <= 0 {
		t.Fatalf("expected cache TTL: %#v", createdConfig)
	}

	if createdConfig == nil || len(createdConfig.Contents) != 1 {
		t.Fatalf("expected one cached content: %#v", createdConfig)
	}

	if createdConfig.Contents[0].Parts[0].Text != strings.Repeat("a", 8_000) {
		t.Fatalf("unexpected cached text prefix: %#v", createdConfig.Contents[0].Parts)
	}

	if config.SystemInstruction == nil {
		t.Fatal("expected system instruction in generate config")
	}
}

func TestGeminiCacheRequestSkipsCacheBelowThreshold(t *testing.T) {
	t.Parallel()

	apiClient := new(stubGeminiCachesClient)
	apiClient.create = func(
		_ context.Context,
		_ string,
		_ *genai.CreateCachedContentConfig,
	) (*genai.CachedContent, error) {
		t.Fatal("cache should not be created below the token threshold")

		return nil, errStubUnreachable
	}

	request := newSimpleGeminiStreamRequest()
	request.ConfiguredModel = "gemini/gemini-3.6-flash"

	_, config, err := buildGeminiGenerateContentRequestWithCaching(
		context.Background(),
		request,
		apiClient,
		apiClient,
	)
	if err != nil {
		t.Fatalf("build gemini request with caching: %v", err)
	}

	if config.CachedContent != "" {
		t.Fatalf("expected no cached content, got %q", config.CachedContent)
	}
}

func TestGeminiCacheRequestSurfacesCacheCreateErrors(t *testing.T) {
	t.Parallel()

	apiClient := new(stubGeminiCachesClient)
	apiClient.create = func(
		_ context.Context,
		_ string,
		_ *genai.CreateCachedContentConfig,
	) (*genai.CachedContent, error) {
		return nil, stubCreateCacheError()
	}

	_, _, err := buildGeminiGenerateContentRequestWithCaching(
		context.Background(),
		newSimpleGeminiCacheRequest(),
		apiClient,
		apiClient,
	)
	if err == nil {
		t.Fatal("expected cache create error")
	}

	if !strings.Contains(err.Error(), "cache backend unavailable") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestGeminiCachePrefixStopsBeforeLatestUserTurn(t *testing.T) {
	t.Parallel()

	var createdConfig *genai.CreateCachedContentConfig

	apiClient := new(stubGeminiCachesClient)
	apiClient.create = func(
		_ context.Context,
		_ string,
		config *genai.CreateCachedContentConfig,
	) (*genai.CachedContent, error) {
		createdConfig = config

		return stubCachedContent(), nil
	}

	request := newSimpleGeminiCacheRequest()
	request.Messages = []chatMessage{
		{Role: messageRoleUser, Content: strings.Repeat("old context\n", 120)},
		{Role: messageRoleAssistant, Content: strings.Repeat("old answer\n", 120)},
		{Role: messageRoleUser, Content: "latest question"},
	}

	contents, _, err := buildGeminiGenerateContentRequestWithCaching(
		context.Background(),
		request,
		apiClient,
		apiClient,
	)
	if err != nil {
		t.Fatalf("build gemini request with caching: %v", err)
	}

	if createdConfig == nil || len(createdConfig.Contents) != 2 {
		t.Fatalf("expected the two leading turns cached: %#v", createdConfig)
	}

	if contents[len(contents)-1].Parts[0].Text != "latest question" {
		t.Fatalf("expected latest turn outside cache: %#v", contents[len(contents)-1])
	}
}

func TestGeminiCachePrefixHonorsMinTokenLimit(t *testing.T) {
	t.Parallel()

	createCalls := 0

	apiClient := new(stubGeminiCachesClient)
	apiClient.create = func(
		_ context.Context,
		_ string,
		_ *genai.CreateCachedContentConfig,
	) (*genai.CachedContent, error) {
		createCalls++

		return stubCachedContent(), nil
	}

	request := newSimpleGeminiStreamRequest()
	request.ConfiguredModel = "gemini/gemini-2.5-flash"
	request.Messages = []chatMessage{
		{Role: messageRoleUser, Content: strings.Repeat("b", 400)},
		{Role: messageRoleUser, Content: "q"},
	}

	_, _, err := buildGeminiGenerateContentRequestWithCaching(
		context.Background(),
		request,
		apiClient,
		apiClient,
	)
	if err != nil {
		t.Fatalf("build gemini request with caching: %v", err)
	}

	if createCalls != 0 {
		t.Fatalf("expected no cache create below min token limit, got %d calls", createCalls)
	}
}

func TestGeminiCacheUsesFilesURIPartForUploadedMedia(t *testing.T) {
	t.Parallel()

	var createdConfig *genai.CreateCachedContentConfig

	apiClient := new(stubGeminiCachesClient)
	apiClient.create = func(
		_ context.Context,
		_ string,
		config *genai.CreateCachedContentConfig,
	) (*genai.CachedContent, error) {
		createdConfig = config

		return stubCachedContent(), nil
	}

	request := newSimpleGeminiCacheRequest()
	request.Messages = []chatMessage{
		{Role: messageRoleUser, Content: strings.Repeat("a", 8_000)},
		{Role: messageRoleUser, Content: "latest question"},
	}

	_, _, err := buildGeminiGenerateContentRequestWithCaching(
		context.Background(),
		request,
		apiClient,
		apiClient,
	)
	if err != nil {
		t.Fatalf("build gemini request with caching: %v", err)
	}

	if createdConfig == nil {
		t.Fatal("expected cache config")
	}

	if createdConfig.Contents[0].Parts[0].Text != strings.Repeat("a", 8_000) {
		t.Fatalf("unexpected cached text: %#v", createdConfig.Contents[0].Parts)
	}
}

func TestGeminiCacheTTLDefaultsToOneHour(t *testing.T) {
	t.Parallel()

	if ttl := geminiCacheTTL(map[string]any{}); ttl != time.Hour {
		t.Fatalf("expected default one-hour TTL, got %v", ttl)
	}

	if ttl := geminiCacheTTL(map[string]any{
		geminiCacheOptionKey: map[string]any{"ttl": "not-a-duration"},
	}); ttl != time.Hour {
		t.Fatalf("expected fallback TTL for invalid duration, got %v", ttl)
	}
}

func TestGeminiCacheTTLReadsExtraBody(t *testing.T) {
	t.Parallel()

	if ttl := geminiCacheTTL(map[string]any{
		geminiCacheOptionKey: map[string]any{"ttl": "30m"},
	}); ttl != 30*time.Minute {
		t.Fatalf("expected 30m TTL, got %v", ttl)
	}
}

func TestGeminiCacheDisplayNameIncludesModelAndRequestID(t *testing.T) {
	t.Parallel()

	request := newSimpleGeminiCacheRequest()
	request.RequestID = "12345"

	displayName := geminiCacheDisplayName(request)
	if !strings.Contains(displayName, "gemini/gemini-3.6-flash") {
		t.Fatalf("expected model in display name, got %q", displayName)
	}

	if !strings.Contains(displayName, "12345") {
		t.Fatalf("expected request ID in display name, got %q", displayName)
	}
}

func TestGeminiCacheDisplayNameFallsBack(t *testing.T) {
	t.Parallel()

	if displayName := geminiCacheDisplayName(emptyChatCompletionRequest()); displayName != "llmcord-go" {
		t.Fatalf("unexpected fallback display name: %q", displayName)
	}
}

func TestGeminiCacheVersionedMinThresholds(t *testing.T) {
	t.Parallel()

	if tokenCount := geminiCacheMinTokensForModel("gemini-2.5-flash"); tokenCount != 2048 {
		t.Fatalf("unexpected 2.5 flash threshold: %d", tokenCount)
	}

	if tokenCount := geminiCacheMinTokensForModel("gemini-3.5-flash"); tokenCount != 4096 {
		t.Fatalf("unexpected 3.5 flash threshold: %d", tokenCount)
	}

	if tokenCount := geminiCacheMinTokensForModel("gemini-2.0-flash"); tokenCount != 0 {
		t.Fatalf("unexpected legacy model threshold: %d", tokenCount)
	}
}

func TestGeminiCacheRequestSupportsDisableExtraBody(t *testing.T) {
	t.Parallel()

	apiClient := new(stubGeminiCachesClient)
	apiClient.create = func(
		_ context.Context,
		_ string,
		_ *genai.CreateCachedContentConfig,
	) (*genai.CachedContent, error) {
		t.Fatal("cache must not be created when disabled via extra_body")

		return nil, errStubUnreachable
	}

	request := newSimpleGeminiCacheRequest()
	request.Provider.ExtraBody = map[string]any{"context_caching": "off"}

	_, config, err := buildGeminiGenerateContentRequestWithCaching(
		context.Background(),
		request,
		apiClient,
		apiClient,
	)
	if err != nil {
		t.Fatalf("build gemini request with caching: %v", err)
	}

	if config.CachedContent != "" {
		t.Fatalf("expected no cached content when disabled, got %q", config.CachedContent)
	}
}

func TestGeminiCacheRequestStreamClientWithoutCacheSupportSkips(t *testing.T) {
	t.Parallel()

	apiClient := new(stubGeminiAPIClient)

	_, config, err := buildGeminiGenerateContentRequestWithCaching(
		context.Background(),
		newSimpleGeminiCacheRequest(),
		apiClient,
		apiClient,
	)
	if err != nil {
		t.Fatalf("build gemini request with caching: %v", err)
	}

	if config.CachedContent != "" {
		t.Fatalf("expected no cached content without cache support, got %q", config.CachedContent)
	}
}

func TestGeminiCacheStreamChatCompletionWiringSetsCachedContent(t *testing.T) {
	t.Parallel()

	var streamedConfig *genai.GenerateContentConfig

	cacheClient := new(stubGeminiCachesClient)
	cacheClient.create = func(
		_ context.Context,
		_ string,
		_ *genai.CreateCachedContentConfig,
	) (*genai.CachedContent, error) {
		return stubCachedContent(), nil
	}
	cacheClient.generateContentStream = func(
		_ context.Context,
		_ string,
		_ []*genai.Content,
		config *genai.GenerateContentConfig,
	) iter.Seq2[*genai.GenerateContentResponse, error] {
		streamedConfig = config

		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			_ = yield(newGeminiGenerateContentResponse("ok", genai.FinishReasonStop), nil)
		}
	}

	client := geminiClient{
		httpClient: new(http.Client),
		newClient: func(
			_ context.Context,
			_ *genai.ClientConfig,
		) (geminiAPIClient, error) {
			return cacheClient, nil
		},
	}

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiCacheRequest(),
		func(streamDelta) error {
			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if streamedConfig == nil || streamedConfig.CachedContent != "cachedContents/test" {
		t.Fatalf("expected cached content name in stream config, got %#v", streamedConfig)
	}
}
