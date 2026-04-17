package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/genai"
)

const (
	testSearchDeciderModel    = "openai/decider-model"
	testAssistantNameReminder = "Understood. Your name is Jandron."
	testEmptyAIMention        = "at ai"
	testAIMentionQuery        = "at ai hello"
)

var errSendProgressMessageFailed = errors.New("send progress message failed")
var errProgressMessageEditedTooQuickly = errors.New("progress message edited too quickly")

func TestBuildMessageConversationUsesPlaceholderForStandaloneEmptyMention(t *testing.T) {
	t.Parallel()

	const (
		channelID       = "channel-1"
		userID          = "user-1"
		sourceMessageID = "source-message"
	)

	instance := newHistoryRetentionTestBot(t)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = testEmptyAIMention

	loadedConfig := testSearchConfig()
	loadedConfig.MaxText = defaultMaxText
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages

	messages, _, err := instance.buildMessageConversation(
		context.Background(),
		loadedConfig,
		sourceMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("build message conversation: %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("unexpected conversation length: %d", len(messages))
	}

	if messages[0].Role != messageRoleUser || messages[0].Content != fileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected standalone empty mention content: %#v", messages[0])
	}
}

func TestBuildMessageConversationKeepsEmptyFollowUpQueryAsPlaceholder(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "676735636656357396"
		sourceMessageID    = "source-message"
		assistantMessageID = "assistant-message"
		followUpMessageID  = "follow-up-message"
	)

	instance := newHistoryRetentionTestBot(t)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "at ai my name is Jandron"
	setCachedUserNode(instance, sourceMessage, nil, "my name is Jandron")

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantMessageID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.MessageReference = sourceMessage.Reference()
	assistantMessage.Type = discordgo.MessageTypeReply
	setCachedAssistantNode(instance, assistantMessage, sourceMessage)

	followUpMessage := new(discordgo.Message)
	followUpMessage.ID = followUpMessageID
	followUpMessage.ChannelID = channelID
	followUpMessage.Author = newDiscordUser(userID, false)
	followUpMessage.Content = testEmptyAIMention
	followUpMessage.MessageReference = assistantMessage.Reference()
	followUpMessage.ReferencedMessage = assistantMessage

	loadedConfig := testSearchConfig()
	loadedConfig.MaxText = defaultMaxText
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages

	messages, _, err := instance.buildMessageConversation(
		context.Background(),
		loadedConfig,
		followUpMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("build message conversation: %v", err)
	}

	if len(messages) != 3 {
		t.Fatalf("unexpected conversation length: %d", len(messages))
	}

	if messages[2].Role != messageRoleUser || messages[2].Content != fileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected follow-up content: %#v", messages[2])
	}
}

func TestBuildChatCompletionRequestPreservesConfiguredModelForDisplay(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = testOpenAIBaseURL

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/gpt-5.1": {
			"temperature": 0.2,
		},
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"openai/gpt-5.1",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Model != "gpt-5.1" {
		t.Fatalf("unexpected request model: %q", request.Model)
	}

	if request.ConfiguredModel != "openai/gpt-5.1" {
		t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
	}
}

func TestBuildChatCompletionRequestPreservesProviderAPIKeys(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = testOpenAIBaseURL
	provider.APIKeys = []string{"primary-key", "backup-key"}

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/gpt-5.1": nil,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"openai/gpt-5.1",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Provider.APIKey != "primary-key" {
		t.Fatalf("unexpected primary API key: %q", request.Provider.APIKey)
	}

	if !slices.Equal(request.Provider.APIKeys, []string{"primary-key", "backup-key"}) {
		t.Fatalf("unexpected provider API keys: %#v", request.Provider.APIKeys)
	}
}

func TestBuildChatCompletionRequestDefaultsOpenAIProviderVerbosityToLow(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = testOpenAIBaseURL

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/gpt-5.1": nil,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"openai/gpt-5.1",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Provider.ExtraBody["verbosity"] != defaultProviderVerbosityLow {
		t.Fatalf("unexpected provider extra body: %#v", request.Provider.ExtraBody)
	}
}

func TestBuildChatCompletionRequestUsesResponsesAPIForOpenAIProvider(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = testOpenAIBaseURL

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/gpt-5.1": nil,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"openai/gpt-5.1",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if !request.Provider.UseResponsesAPI {
		t.Fatal("expected openai provider to use the Responses API")
	}
}

func TestBuildChatCompletionRequestUsesContextWindowWithoutSendingItToProvider(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = testOpenAIBaseURL

	modelParameters := map[string]any{
		"context_window": 400_000,
		"temperature":    0.2,
	}

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/gpt-5.1": modelParameters,
	}
	loadedConfig.ModelContextWindows = map[string]int{
		"openai/gpt-5.1": 400_000,
	}
	loadedConfig.AutoCompactThresholdPercent = 75

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"openai/gpt-5.1",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.ContextWindow != 400_000 {
		t.Fatalf("unexpected context window: %d", request.ContextWindow)
	}

	if request.AutoCompactThresholdPercent != 75 {
		t.Fatalf("unexpected auto compact threshold percent: %d", request.AutoCompactThresholdPercent)
	}

	if _, ok := request.Provider.ExtraBody[modelConfigContextWindowKey]; ok {
		t.Fatalf("unexpected local-only model config in provider extra body: %#v", request.Provider.ExtraBody)
	}

	if got, ok := request.Provider.ExtraBody["temperature"].(float64); !ok || got != 0.2 {
		t.Fatalf("unexpected provider extra body: %#v", request.Provider.ExtraBody)
	}

	if modelParameters[modelConfigContextWindowKey] != 400_000 {
		t.Fatalf("unexpected mutation of model parameters: %#v", modelParameters)
	}
}

func TestBuildChatCompletionRequestDefaultsOpenRouterTransforms(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = "https://openrouter.ai/api/v1"
	provider.ExtraBody = map[string]any{
		"temperature": 0.2,
	}

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"router": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"router/anthropic/claude-3.7-sonnet": nil,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"router/anthropic/claude-3.7-sonnet",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Model != "anthropic/claude-3.7-sonnet" {
		t.Fatalf("unexpected request model: %q", request.Model)
	}

	transforms, transformsOK := request.Provider.ExtraBody[openRouterTransformsField].([]string)
	if !transformsOK {
		t.Fatalf("unexpected openrouter transforms payload: %#v", request.Provider.ExtraBody[openRouterTransformsField])
	}

	if !slices.Equal(transforms, []string{openRouterMiddleOutTransform}) {
		t.Fatalf("unexpected openrouter transforms: %#v", transforms)
	}

	if _, ok := provider.ExtraBody[openRouterTransformsField]; ok {
		t.Fatalf("unexpected mutation of provider extra body: %#v", provider.ExtraBody)
	}

	if request.Provider.ExtraBody["temperature"] != 0.2 {
		t.Fatalf("unexpected provider extra body: %#v", request.Provider.ExtraBody)
	}
}

func TestBuildChatCompletionRequestPreservesExplicitOpenRouterTransforms(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = "https://openrouter.ai/api/v1"

	modelParameters := map[string]any{
		openRouterTransformsField: []any{},
	}

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"router": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"router/anthropic/claude-3.7-sonnet": modelParameters,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"router/anthropic/claude-3.7-sonnet",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	transforms, transformsOK := request.Provider.ExtraBody[openRouterTransformsField].([]any)
	if !transformsOK {
		t.Fatalf("unexpected openrouter transforms payload: %#v", request.Provider.ExtraBody[openRouterTransformsField])
	}

	if len(transforms) != 0 {
		t.Fatalf("unexpected openrouter transforms override: %#v", transforms)
	}

	originalTransforms, originalTransformsOK := modelParameters[openRouterTransformsField].([]any)
	if !originalTransformsOK || len(originalTransforms) != 0 {
		t.Fatalf("unexpected mutation of model parameters: %#v", modelParameters)
	}
}

func TestBuildChatCompletionRequestNormalizesGeminiThinkingAlias(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.Type = string(providerAPIKindGemini)
	provider.ExtraBody = map[string]any{
		"temperature": 0.2,
	}

	modelParameters := map[string]any{
		"thinkingConfig": map[string]any{
			"includeThoughts": true,
		},
	}

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"google": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"google/gemini-3.1-flash-lite-preview-minimal": modelParameters,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"google/gemini-3.1-flash-lite-preview-minimal",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Model != "gemini-3.1-flash-lite-preview" {
		t.Fatalf("unexpected request model: %q", request.Model)
	}

	if request.ConfiguredModel != "google/gemini-3.1-flash-lite-preview-minimal" {
		t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
	}

	if got, ok := request.Provider.ExtraBody["temperature"].(float64); !ok || got != 0.2 {
		t.Fatalf("unexpected provider extra body: %#v", request.Provider.ExtraBody)
	}

	thinkingConfig, thinkingConfigOK := request.Provider.ExtraBody["thinkingConfig"].(map[string]any)
	if !thinkingConfigOK {
		t.Fatalf("unexpected thinking config: %#v", request.Provider.ExtraBody["thinkingConfig"])
	}

	if thinkingConfig["includeThoughts"] != true {
		t.Fatalf("unexpected thinking config contents: %#v", thinkingConfig)
	}

	if thinkingConfig["thinkingLevel"] != genai.ThinkingLevelMinimal {
		t.Fatalf("unexpected thinking level: %#v", thinkingConfig["thinkingLevel"])
	}

	if _, ok := modelParameters["thinkingLevel"]; ok {
		t.Fatalf("unexpected mutation of model parameters: %#v", modelParameters)
	}

	originalThinkingConfig, ok := modelParameters["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected original thinking config: %#v", modelParameters["thinkingConfig"])
	}

	if _, ok := originalThinkingConfig["thinkingLevel"]; ok {
		t.Fatalf("unexpected mutation of original thinking config: %#v", originalThinkingConfig)
	}
}

func TestBuildChatCompletionRequestRejectsGeminiThinkingAliasWithInvalidThinkingConfig(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.Type = string(providerAPIKindGemini)

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"google": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"google/gemini-3.1-flash-lite-preview-minimal": {
			"thinkingConfig": "invalid",
		},
	}

	_, err := buildChatCompletionRequest(
		loadedConfig,
		"google/gemini-3.1-flash-lite-preview-minimal",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err == nil {
		t.Fatal("expected invalid thinkingConfig to fail")
	}
}

func TestMessageContentOptionsForModelRestrictsGeminiDocumentsToPDF(t *testing.T) {
	t.Parallel()

	loadedConfig := testMediaAnalysisConfig()

	options, err := messageContentOptionsForModel(loadedConfig, testMediaAnalysisModel)
	if err != nil {
		t.Fatalf("build message content options: %v", err)
	}

	if !options.allowDocuments {
		t.Fatal("expected gemini documents to be enabled")
	}

	if !options.allowFiles {
		t.Fatal("expected gemini files to be enabled")
	}

	if len(options.allowedDocumentMIMETypes) != 1 {
		t.Fatalf("unexpected gemini document MIME type count: %d", len(options.allowedDocumentMIMETypes))
	}

	if _, ok := options.allowedDocumentMIMETypes[mimeTypePDF]; !ok {
		t.Fatalf("expected PDF MIME type to be allowed: %#v", options.allowedDocumentMIMETypes)
	}

	if _, ok := options.allowedDocumentMIMETypes[mimeTypeDOCX]; ok {
		t.Fatalf("expected DOCX MIME type to be disallowed: %#v", options.allowedDocumentMIMETypes)
	}

	if _, ok := options.allowedDocumentMIMETypes[mimeTypePPTX]; ok {
		t.Fatalf("expected PPTX MIME type to be disallowed: %#v", options.allowedDocumentMIMETypes)
	}
}

func TestMessageContentOptionsForModelAllowsXAIManagedDocuments(t *testing.T) {
	t.Parallel()

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		xAIProviderName: {
			Type:         "",
			BaseURL:      "https://api.x.ai/v1",
			APIKey:       "",
			APIKeys:      nil,
			ExtraHeaders: nil,
			ExtraQuery:   nil,
			ExtraBody:    nil,
		},
	}
	loadedConfig.Models = map[string]map[string]any{
		"x-ai/grok-4": nil,
	}

	options, err := messageContentOptionsForModel(loadedConfig, "x-ai/grok-4")
	if err != nil {
		t.Fatalf("build message content options: %v", err)
	}

	if !options.allowDocuments {
		t.Fatal("expected xAI documents to be enabled")
	}

	if !options.allowFiles {
		t.Fatal("expected xAI files to be enabled")
	}

	if options.allowedDocumentMIMETypes != nil {
		t.Fatalf("expected xAI documents to allow all supported MIME types: %#v", options.allowedDocumentMIMETypes)
	}

	if options.allowAudio {
		t.Fatalf("expected xAI audio to remain disabled: %#v", options)
	}

	if options.allowVideo {
		t.Fatalf("expected xAI video to remain disabled: %#v", options)
	}
}

func TestMessageContentOptionsForModelLeavesOpenAIFilesDisabled(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = testOpenAIBaseURL

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/gpt-5.1": nil,
	}

	options, err := messageContentOptionsForModel(loadedConfig, "openai/gpt-5.1")
	if err != nil {
		t.Fatalf("build message content options: %v", err)
	}

	if options.allowFiles {
		t.Fatalf("expected OpenAI-compatible chat files to remain disabled: %#v", options)
	}

	if options.allowDocuments {
		t.Fatalf("expected OpenAI-compatible chat documents to remain disabled: %#v", options)
	}
}

type concurrentFetchGate struct {
	expected     int32
	startedCount int32
	release      chan struct{}
}

func newConcurrentFetchGate(expected int32) *concurrentFetchGate {
	return &concurrentFetchGate{
		expected:     expected,
		startedCount: 0,
		release:      make(chan struct{}),
	}
}

func (gate *concurrentFetchGate) wait(ctx context.Context) error {
	if atomic.AddInt32(&gate.startedCount, 1) == gate.expected {
		close(gate.release)
	}

	select {
	case <-gate.release:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for concurrent fetch gate: %w", ctx.Err())
	}
}

func newBlockedTikTokClient(gate *concurrentFetchGate) *stubTikTokContentClient {
	return newStubTikTokContentClient(func(
		ctx context.Context,
		rawURL string,
	) (tiktokVideoContent, error) {
		waitErr := gate.wait(ctx)
		if waitErr != nil {
			return tiktokVideoContent{}, waitErr
		}

		return tiktokVideoContent{
			ResolvedURL: rawURL,
			DownloadURL: "",
			MediaPart: contentPart{
				"type":               contentTypeVideoData,
				contentFieldBytes:    []byte("tiktok-video"),
				contentFieldMIMEType: testVideoMIMEType,
				contentFieldFilename: "tiktok.mp4",
			},
		}, nil
	})
}

func newBlockedFacebookClient(gate *concurrentFetchGate) *stubFacebookContentClient {
	return newStubFacebookContentClient(func(
		ctx context.Context,
		rawURL string,
	) (facebookVideoContent, error) {
		waitErr := gate.wait(ctx)
		if waitErr != nil {
			return facebookVideoContent{}, waitErr
		}

		return facebookVideoContent{
			ResolvedURL: rawURL,
			DownloadURL: "",
			MediaPart: contentPart{
				"type":               contentTypeVideoData,
				contentFieldBytes:    []byte("facebook-video"),
				contentFieldMIMEType: testVideoMIMEType,
				contentFieldFilename: "facebook.mp4",
			},
		}, nil
	})
}

func assertAugmentedVideoOrder(
	t *testing.T,
	augmentedConversation []chatMessage,
) {
	t.Helper()

	parts, ok := augmentedConversation[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if len(parts) != 3 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	firstFilename, _ := parts[1][contentFieldFilename].(string)
	secondFilename, _ := parts[2][contentFieldFilename].(string)

	if firstFilename != "tiktok.mp4" || secondFilename != "facebook.mp4" {
		t.Fatalf("unexpected video order: %#v", parts)
	}
}

func newBlockedVisualSearchClient(gate *concurrentFetchGate) *stubVisualSearchClient {
	client := new(stubVisualSearchClient)
	client.searchFn = func(ctx context.Context, imageURL string) (visualSearchResult, error) {
		waitErr := gate.wait(ctx)
		if waitErr != nil {
			return visualSearchResult{}, waitErr
		}

		return newStructuredVisualSearchResult(imageURL), nil
	}

	return client
}

func newBlockedWebsiteClient(gate *concurrentFetchGate) *stubWebsiteContentClient {
	return newStubWebsiteContentClient(func(
		ctx context.Context,
		_ config,
		rawURL string,
	) (websitePageContent, error) {
		waitErr := gate.wait(ctx)
		if waitErr != nil {
			return websitePageContent{}, waitErr
		}

		return websitePageContent{
			URL:         rawURL,
			Title:       "Example website",
			Description: "Example description",
			Content:     "Website body",
		}, nil
	})
}

func testXAIProviderConfig() providerConfig {
	return providerConfig{
		Type:         "",
		BaseURL:      "https://api.x.ai/v1",
		APIKey:       "",
		APIKeys:      nil,
		ExtraHeaders: nil,
		ExtraQuery:   nil,
		ExtraBody:    nil,
	}
}

func testXAIAugmentationConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.Providers[xAIProviderName] = testXAIProviderConfig()
	loadedConfig.Models["x-ai/grok-4"] = nil
	loadedConfig.ModelOrder = append([]string{"x-ai/grok-4"}, loadedConfig.ModelOrder...)

	return loadedConfig
}

func testXAIWithMediaAnalysisConfig() config {
	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.Providers[xAIProviderName] = testXAIProviderConfig()
	loadedConfig.Models["x-ai/grok-4"] = nil
	loadedConfig.ModelOrder = append([]string{"x-ai/grok-4"}, loadedConfig.ModelOrder...)

	return loadedConfig
}

func failUnexpectedURLFetch(t *testing.T, label string, rawURL string) {
	t.Helper()
	t.Fatalf("unexpected %s fetch for %q", label, rawURL)
}

func newCountingGeminiVideoAnalysisChatClient(
	t *testing.T,
	expectedAnalysis string,
) (*stubChatCompletionClient, *atomic.Int32) {
	t.Helper()

	callCount := new(atomic.Int32)

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

		callCount.Add(1)

		return handle(streamDelta{
			Thinking:           "",
			Content:            expectedAnalysis,
			FinishReason:       finishReasonStop,
			Usage:              nil,
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
	})

	return chatClient, callCount
}

func newExpectedFacebookContentClient(t *testing.T) *stubFacebookContentClient {
	t.Helper()

	return newStubFacebookContentClient(func(
		_ context.Context,
		rawURL string,
	) (facebookVideoContent, error) {
		if rawURL != testFacebookURL {
			t.Fatalf("unexpected facebook raw url: %q", rawURL)
		}

		return testFacebookVideoContent(), nil
	})
}

func newExpectedYouTubeShortsContentClient(t *testing.T) *stubYouTubeShortsContentClient {
	t.Helper()

	return newStubYouTubeShortsContentClient(func(
		_ context.Context,
		rawURL string,
	) (youtubeShortsVideoContent, error) {
		if rawURL != testYouTubeShortsCanonicalURL {
			t.Fatalf("unexpected youtube shorts raw url: %q", rawURL)
		}

		return testYouTubeShortsVideoContent(), nil
	})
}

func newXAIURLBypassTestBot(
	t *testing.T,
	facebook facebookFetcher,
	youtubeShorts youtubeShortsFetcher,
) *bot {
	t.Helper()

	instance := new(bot)

	instance.tiktok = newStubTikTokContentClient(func(
		_ context.Context,
		rawURL string,
	) (tiktokVideoContent, error) {
		failUnexpectedURLFetch(t, "tiktok", rawURL)
		panic("unreachable")
	})
	if facebook != nil {
		instance.facebook = facebook
	} else {
		instance.facebook = newStubFacebookContentClient(func(
			_ context.Context,
			rawURL string,
		) (facebookVideoContent, error) {
			failUnexpectedURLFetch(t, "facebook", rawURL)
			panic("unreachable")
		})
	}

	if youtubeShorts != nil {
		instance.youtubeShorts = youtubeShorts
	} else {
		instance.youtubeShorts = newStubYouTubeShortsContentClient(func(
			_ context.Context,
			rawURL string,
		) (youtubeShortsVideoContent, error) {
			failUnexpectedURLFetch(t, "youtube shorts", rawURL)
			panic("unreachable")
		})
	}

	instance.website = newStubWebsiteContentClient(func(
		_ context.Context,
		_ config,
		rawURL string,
	) (websitePageContent, error) {
		failUnexpectedURLFetch(t, "website", rawURL)
		panic("unreachable")
	})
	instance.youtube = newStubYouTubeContentClient(func(
		_ context.Context,
		rawURL string,
	) (youtubeVideoContent, error) {
		failUnexpectedURLFetch(t, "youtube", rawURL)
		panic("unreachable")
	})
	instance.reddit = newStubRedditContentClient(func(
		_ context.Context,
		rawURL string,
	) (redditThreadContent, error) {
		failUnexpectedURLFetch(t, "reddit", rawURL)
		panic("unreachable")
	})

	return instance
}

func newBlockedYouTubeClient(gate *concurrentFetchGate) *stubYouTubeContentClient {
	return newStubYouTubeContentClient(func(
		ctx context.Context,
		rawURL string,
	) (youtubeVideoContent, error) {
		waitErr := gate.wait(ctx)
		if waitErr != nil {
			return youtubeVideoContent{}, waitErr
		}

		videoID, canonicalURL, err := parseYouTubeVideoURL(rawURL)
		if err != nil {
			return youtubeVideoContent{}, err
		}

		return youtubeVideoContent{
			URL:         canonicalURL,
			VideoID:     videoID,
			Title:       "Example YouTube video",
			ChannelName: "Example channel",
			Transcript:  "Example transcript",
			Comments:    nil,
		}, nil
	})
}

func newBlockedRedditClient(gate *concurrentFetchGate) *stubRedditContentClient {
	return newStubRedditContentClient(func(
		ctx context.Context,
		rawURL string,
	) (redditThreadContent, error) {
		waitErr := gate.wait(ctx)
		if waitErr != nil {
			return redditThreadContent{}, waitErr
		}

		return redditThreadContent{
			URL:         rawURL,
			JSONURL:     rawURL + ".json",
			Subreddit:   "r/testing",
			Title:       "Example Reddit thread",
			Author:      "tester",
			Body:        "Reddit body",
			LinkedURL:   "",
			Comments:    nil,
			Score:       10,
			UpvoteRatio: 0.9,
			NumComments: 1,
			CreatedUTC:  1,
		}, nil
	})
}

func newNoSearchDecisionChatClient(
	t *testing.T,
	requiredFragments []string,
) *stubChatCompletionClient {
	t.Helper()

	return newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		renderedMessages := make([]string, 0, len(request.Messages))
		for _, message := range request.Messages {
			renderedMessages = append(renderedMessages, messageContentText(message.Content))
		}

		renderedConversation := strings.Join(renderedMessages, "\n\n")

		for _, fragment := range requiredFragments {
			if !strings.Contains(renderedConversation, fragment) {
				t.Fatalf("expected fragment %q in search decider request: %q", fragment, renderedConversation)
			}
		}

		return handle(streamDelta{
			Thinking:           "",
			Content:            `{"needs_search":false,"queries":[]}`,
			FinishReason:       finishReasonStop,
			Usage:              nil,
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
	})
}

func TestAugmentConversationWithVideoURLsFetchesProvidersConcurrentlyAndKeepsOrder(t *testing.T) {
	t.Parallel()

	gate := newConcurrentFetchGate(2)

	instance := new(bot)
	instance.tiktok = newBlockedTikTokClient(gate)
	instance.facebook = newBlockedFacebookClient(gate)

	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.SearchDeciderModel = testMediaAnalysisModel

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: strings.Join([]string{
				"<@123>: summarize these videos",
				"https://www.tiktok.com/@mikemhan/video/7614735539660442893",
				testFacebookURL,
			}, " "),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	augmentedConversation, warnings, err := instance.augmentConversationWithVideoURLs(
		ctx,
		loadedConfig,
		testMediaAnalysisModel,
		conversation,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with video URLs: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	assertAugmentedVideoOrder(t, augmentedConversation)
}

func TestAugmentConversationWithVideoURLsProcessesFacebookAndYouTubeShortsForXAIProviders(t *testing.T) {
	t.Parallel()

	expectedAnalysis := "Video description per timestamp:\n\n0s to 10s: somebody waves"
	chatClient, analysisCallCount := newCountingGeminiVideoAnalysisChatClient(t, expectedAnalysis)
	facebook := newExpectedFacebookContentClient(t)
	youtubeShorts := newExpectedYouTubeShortsContentClient(t)

	instance := newXAIURLBypassTestBot(t, facebook, youtubeShorts)
	instance.chatCompletions = chatClient

	loadedConfig := testXAIWithMediaAnalysisConfig()
	query := strings.Join([]string{
		"<@123>: summarize these videos",
		"https://www.tiktok.com/@mikemhan/video/7614735539660442893",
		testFacebookURL,
		testYouTubeShortsCanonicalURL,
	}, " ")

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: query,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	augmentedConversation, warnings, err := instance.augmentConversationWithVideoURLs(
		ctx,
		loadedConfig,
		"x-ai/grok-4",
		conversation,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with video urls: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if analysisCallCount.Load() != 2 {
		t.Fatalf("unexpected gemini analysis call count: %d", analysisCallCount.Load())
	}

	if len(facebook.calls) != 1 || facebook.calls[0] != testFacebookURL {
		t.Fatalf("unexpected facebook calls: %#v", facebook.calls)
	}

	if len(youtubeShorts.calls) != 1 || youtubeShorts.calls[0] != testYouTubeShortsCanonicalURL {
		t.Fatalf("unexpected youtube shorts calls: %#v", youtubeShorts.calls)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	prompt := parseAugmentedUserPrompt(content)

	expectedUserQuery := expectedMediaAnalysisUserText(
		query,
		[]string{expectedAnalysis, expectedAnalysis},
	)
	if prompt.UserQuery != expectedUserQuery {
		t.Fatalf("unexpected augmented user query: got %q want %q", prompt.UserQuery, expectedUserQuery)
	}
}

func TestAugmentConversationFetchesIndependentContextBeforeWebSearchDecision(t *testing.T) {
	t.Parallel()

	gate := newConcurrentFetchGate(4)

	instance := new(bot)
	instance.visualSearch = newBlockedVisualSearchClient(gate)
	instance.website = newBlockedWebsiteClient(gate)
	instance.youtube = newBlockedYouTubeClient(gate)
	instance.reddit = newBlockedRedditClient(gate)
	instance.chatCompletions = newNoSearchDecisionChatClient(
		t,
		[]string{
			visualSearchSectionName + ":",
			websiteSectionName + ":",
			youtubeSectionName + ":",
			redditSectionName + ":",
		},
	)
	instance.nodes = newMessageNodeStore(10)

	sourceMessage := newVisualSearchSourceMessage("source-message", "123")

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: strings.Join([]string{
				"<@123>: vsearch identify this",
				"https://example.com/article",
				"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
				"https://www.reddit.com/r/testing/comments/abc123/thread-title/",
			}, " "),
		},
	}

	loadedConfig := testMediaAnalysisConfig()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	augmentedConversation, metadata, warnings, err := instance.augmentConversation(
		ctx,
		loadedConfig,
		"openai/gpt-5",
		sourceMessage,
		conversation,
		nil,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if metadata == nil || len(metadata.VisualSearchSources) == 0 {
		t.Fatalf("expected visual search metadata: %#v", metadata)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	prompt := parseAugmentedUserPrompt(content)
	for field, value := range map[string]string{
		visualSearchSectionName: prompt.VisualSearch,
		websiteSectionName:      prompt.WebsiteContent,
		youtubeSectionName:      prompt.YouTubeContent,
		redditSectionName:       prompt.RedditContent,
	} {
		if strings.TrimSpace(value) == "" {
			t.Fatalf("expected non-empty %s content in prompt: %#v", field, prompt)
		}
	}
}

func TestAugmentConversationForXAILeavesNonFacebookNonShortsURLsForProviderHandling(t *testing.T) {
	t.Parallel()

	instance := newXAIURLBypassTestBot(t, nil, nil)
	loadedConfig := testXAIAugmentationConfig()

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: strings.Join([]string{
				"<@123>: summarize these links",
				"https://x.com/example/status/123",
				"https://example.com/article",
				"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
				"https://www.reddit.com/r/testing/comments/abc123/thread-title/",
			}, " "),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	augmentedConversation, metadata, warnings, err := instance.augmentConversation(
		ctx,
		loadedConfig,
		"x-ai/grok-4",
		nil,
		conversation,
		nil,
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if metadata != nil {
		t.Fatalf("expected no search metadata: %#v", metadata)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	prompt := parseAugmentedUserPrompt(content)
	for _, rawURL := range []string{
		"https://x.com/example/status/123",
		"https://example.com/article",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://www.reddit.com/r/testing/comments/abc123/thread-title/",
	} {
		if !strings.Contains(prompt.UserQuery, rawURL) {
			t.Fatalf("expected url to remain in user query: %q in %#v", rawURL, prompt)
		}
	}

	if strings.TrimSpace(prompt.WebsiteContent) != "" {
		t.Fatalf("expected website augmentation to be skipped: %#v", prompt)
	}

	if strings.TrimSpace(prompt.YouTubeContent) != "" {
		t.Fatalf("expected youtube augmentation to be skipped: %#v", prompt)
	}

	if strings.TrimSpace(prompt.RedditContent) != "" {
		t.Fatalf("expected reddit augmentation to be skipped: %#v", prompt)
	}
}

func TestBuildChatCompletionRequestNormalizesOpenAICodexReasoningAlias(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.Type = string(providerAPIKindOpenAICodex)
	provider.ExtraBody = map[string]any{
		"verbosity":        "medium",
		"reasoning_effort": "medium",
	}

	modelParameters := map[string]any{
		"reasoning": map[string]any{
			"summary": "concise",
			"effort":  "high",
		},
	}

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"codex": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"codex/gpt-5.4-none": modelParameters,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"codex/gpt-5.4-none",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Model != "gpt-5.4" {
		t.Fatalf("unexpected request model: %q", request.Model)
	}

	if request.ConfiguredModel != "codex/gpt-5.4-none" {
		t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
	}

	if request.Provider.ExtraBody["verbosity"] != "medium" {
		t.Fatalf("unexpected provider extra body: %#v", request.Provider.ExtraBody)
	}

	if _, ok := request.Provider.ExtraBody["reasoning_effort"]; ok {
		t.Fatalf("unexpected top-level reasoning_effort: %#v", request.Provider.ExtraBody)
	}

	reasoningConfig, reasoningConfigOK := request.Provider.ExtraBody["reasoning"].(map[string]any)
	if !reasoningConfigOK {
		t.Fatalf("unexpected reasoning config: %#v", request.Provider.ExtraBody["reasoning"])
	}

	if reasoningConfig["summary"] != "concise" {
		t.Fatalf("unexpected reasoning summary: %#v", reasoningConfig["summary"])
	}

	if reasoningConfig["effort"] != "none" {
		t.Fatalf("unexpected reasoning effort: %#v", reasoningConfig["effort"])
	}

	originalReasoningConfig, ok := modelParameters["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected original reasoning config: %#v", modelParameters["reasoning"])
	}

	if originalReasoningConfig["effort"] != openAICodexReasoningHigh {
		t.Fatalf("unexpected mutation of original reasoning config: %#v", originalReasoningConfig)
	}
}

func TestBuildChatCompletionRequestNormalizesOpenAIResponsesReasoningAlias(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = testOpenAIBaseURL
	provider.ExtraBody = map[string]any{
		"verbosity":        openAIReasoningEffortMedium,
		"reasoning_effort": openAIReasoningEffortMedium,
	}

	modelParameters := map[string]any{
		"reasoning": map[string]any{
			"summary": openAIReasoningSummaryConcise,
			"effort":  "high",
		},
	}

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/gpt-5.4-none": modelParameters,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"openai/gpt-5.4-none",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Model != openAIReasoningModelGPT54 {
		t.Fatalf("unexpected request model: %q", request.Model)
	}

	if request.ConfiguredModel != "openai/gpt-5.4-none" {
		t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
	}

	if !request.Provider.UseResponsesAPI {
		t.Fatal("expected openai provider to use the Responses API")
	}

	if request.Provider.ExtraBody["verbosity"] != openAIReasoningEffortMedium {
		t.Fatalf("unexpected provider extra body: %#v", request.Provider.ExtraBody)
	}

	if _, ok := request.Provider.ExtraBody["reasoning_effort"]; ok {
		t.Fatalf("unexpected top-level reasoning_effort: %#v", request.Provider.ExtraBody)
	}

	reasoningConfig, reasoningConfigOK := request.Provider.ExtraBody["reasoning"].(map[string]any)
	if !reasoningConfigOK {
		t.Fatalf("unexpected reasoning config: %#v", request.Provider.ExtraBody["reasoning"])
	}

	if reasoningConfig["summary"] != openAIReasoningSummaryConcise {
		t.Fatalf("unexpected reasoning summary: %#v", reasoningConfig["summary"])
	}

	if reasoningConfig["effort"] != openAIReasoningEffortNone {
		t.Fatalf("unexpected reasoning effort: %#v", reasoningConfig["effort"])
	}

	originalReasoningConfig, ok := modelParameters["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected original reasoning config: %#v", modelParameters["reasoning"])
	}

	if originalReasoningConfig["effort"] != openAICodexReasoningHigh {
		t.Fatalf("unexpected mutation of original reasoning config: %#v", originalReasoningConfig)
	}
}

func TestBuildChatCompletionRequestNormalizesOpenAIResponsesReasoningEffort(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = testOpenAIBaseURL

	modelParameters := map[string]any{
		"reasoning_effort": openAIReasoningEffortMinimal,
	}

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/gpt-5.4": modelParameters,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"openai/gpt-5.4",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if _, ok := request.Provider.ExtraBody["reasoning_effort"]; ok {
		t.Fatalf("unexpected top-level reasoning_effort: %#v", request.Provider.ExtraBody)
	}

	reasoningConfig, reasoningConfigOK := request.Provider.ExtraBody["reasoning"].(map[string]any)
	if !reasoningConfigOK {
		t.Fatalf("unexpected reasoning config: %#v", request.Provider.ExtraBody["reasoning"])
	}

	if reasoningConfig["effort"] != openAIReasoningEffortLow {
		t.Fatalf("unexpected reasoning effort: %#v", reasoningConfig["effort"])
	}

	if modelParameters["reasoning_effort"] != openAIReasoningEffortMinimal {
		t.Fatalf("unexpected mutation of original reasoning config: %#v", modelParameters)
	}
}

func TestBuildChatCompletionRequestNormalizesOpenAIResponsesLowAliases(t *testing.T) {
	t.Parallel()

	for _, configuredModel := range []string{
		"openai/gpt-5.4-low",
		"openai/gpt-5.4-low:vision",
	} {
		t.Run(configuredModel, func(t *testing.T) {
			t.Parallel()

			provider := new(providerConfig)
			provider.BaseURL = testOpenAIBaseURL

			var loadedConfig config

			loadedConfig.Providers = map[string]providerConfig{
				"openai": *provider,
			}
			loadedConfig.Models = map[string]map[string]any{
				configuredModel: nil,
			}

			request, err := buildChatCompletionRequest(
				loadedConfig,
				configuredModel,
				[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
			)
			if err != nil {
				t.Fatalf("build chat completion request: %v", err)
			}

			if request.Model != openAIReasoningModelGPT54 {
				t.Fatalf("unexpected request model: %q", request.Model)
			}

			if request.ConfiguredModel != configuredModel {
				t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
			}

			reasoningConfig, reasoningConfigOK := request.Provider.ExtraBody["reasoning"].(map[string]any)
			if !reasoningConfigOK {
				t.Fatalf("unexpected reasoning config: %#v", request.Provider.ExtraBody["reasoning"])
			}

			if reasoningConfig["effort"] != openAIReasoningEffortLow {
				t.Fatalf("unexpected reasoning effort: %#v", reasoningConfig["effort"])
			}
		})
	}
}

func TestBuildChatCompletionRequestNormalizesOpenAIChatCompletionsLowAliases(t *testing.T) {
	t.Parallel()

	for _, configuredModel := range []string{
		"compat/gpt-5.4-low",
		"compat/gpt-5.4-low:vision",
	} {
		t.Run(configuredModel, func(t *testing.T) {
			t.Parallel()

			provider := new(providerConfig)
			provider.BaseURL = testOfficialOpenAIBaseURL

			var loadedConfig config

			loadedConfig.Providers = map[string]providerConfig{
				"compat": *provider,
			}
			loadedConfig.Models = map[string]map[string]any{
				configuredModel: nil,
			}

			request, err := buildChatCompletionRequest(
				loadedConfig,
				configuredModel,
				[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
			)
			if err != nil {
				t.Fatalf("build chat completion request: %v", err)
			}

			if request.Provider.UseResponsesAPI {
				t.Fatal("expected non-openai provider to stay on Chat Completions")
			}

			if request.Model != openAIReasoningModelGPT54 {
				t.Fatalf("unexpected request model: %q", request.Model)
			}

			if request.ConfiguredModel != configuredModel {
				t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
			}

			if request.Provider.ExtraBody["reasoning_effort"] != openAIReasoningEffortLow {
				t.Fatalf("unexpected reasoning effort: %#v", request.Provider.ExtraBody["reasoning_effort"])
			}
		})
	}
}

func TestRespondToMessageSendsProgressEmbedBeforeTypingAndAttachmentProcessing(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		attachmentURL      = "https://cdn.discordapp.com/attachments/test/context.txt"
	)

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantMessageID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.Type = discordgo.MessageTypeReply
	fixture := newRespondToMessageTypingFixture(
		t,
		channelID,
		botUserID,
		userID,
		sourceMessageID,
		attachmentURL,
		assistantMessage,
	)

	err := fixture.instance.respondToMessage(
		context.Background(),
		fixture.loadedConfig,
		fixture.sourceMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("respond to message: %v", err)
	}

	if !fixture.typingSent.Load() {
		t.Fatal("expected typing indicator to be sent")
	}

	if !fixture.progressSent.Load() {
		t.Fatal("expected progress embed to be sent")
	}

	if !fixture.attachmentFetched.Load() {
		t.Fatal("expected attachment download during conversation preprocessing")
	}
}

func TestRespondToMessageEditsProgressMessageWithRateLimitError(t *testing.T) {
	t.Parallel()

	const (
		botUserID       = "bot-user"
		channelID       = "channel-1"
		userID          = "user-1"
		sourceMessageID = "user-message-1"
		progressID      = "progress-message"
		expectedError   = "stream response: rate limited"
	)

	progressMessage := new(discordgo.Message)
	progressMessage.ID = progressID
	progressMessage.ChannelID = channelID
	progressMessage.Author = newDiscordUser(botUserID, true)

	patchDescriptions := make([]string, 0, 3)
	session := newProgressEditCaptureSession(
		t,
		channelID,
		botUserID,
		progressMessage,
		&patchDescriptions,
	)

	instance := newRateLimitedRespondToMessageBot(session)

	err := instance.respondToMessage(
		context.Background(),
		newRateLimitedRespondToMessageConfig(),
		newRateLimitedRespondToMessageSourceMessage(channelID, sourceMessageID, userID),
		"openai/main-model",
	)
	if err == nil {
		t.Fatal("expected respond to message error")
	}

	if len(patchDescriptions) == 0 {
		t.Fatal("expected progress message edits")
	}

	if patchDescriptions[len(patchDescriptions)-1] != expectedError {
		t.Fatalf("unexpected final progress error: %#v", patchDescriptions)
	}
}

func TestPrepareMessageResponseUsesPlaceholderForEmptyStandaloneMention(t *testing.T) {
	t.Parallel()

	const (
		channelID       = "channel-1"
		userID          = "user-1"
		sourceMessageID = "user-message-1"
	)

	instance := newHistoryRetentionTestBot(t)
	instance.chatCompletions = newRateLimitedRespondToMessageChatClient()
	sourceMessage := newRateLimitedRespondToMessageSourceMessage(channelID, sourceMessageID, userID)
	sourceMessage.Content = testEmptyAIMention

	request, _, _, err := instance.prepareMessageResponse(
		context.Background(),
		newRateLimitedRespondToMessageConfig(),
		sourceMessage,
		"openai/main-model",
		nil,
	)
	if err != nil {
		t.Fatalf("prepare message response: %v", err)
	}

	index, err := latestUserMessageIndex(request.Messages)
	if err != nil {
		t.Fatalf("find latest user message: %v", err)
	}

	if request.Messages[index].Content != fileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected latest user content: %#v", request.Messages[index])
	}
}

func TestRespondToMessageEditsProgressMessageWhenGeminiMediaAnalysisFailsDuringPreparation(t *testing.T) {
	t.Parallel()

	const (
		botUserID       = "bot-user"
		channelID       = "channel-1"
		userID          = "user-1"
		sourceMessageID = "user-message-1"
		progressID      = "progress-message"
	)

	expectedError := newUnavailableGeminiMediaPreparationProgressError().Error()

	fixture := newGeminiMediaPreparationFailureFixture(
		t,
		botUserID,
		channelID,
		userID,
		sourceMessageID,
		progressID,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := fixture.instance.respondToMessage(
		ctx,
		fixture.loadedConfig,
		fixture.sourceMessage,
		"openai/gpt-5",
	)
	if err == nil {
		t.Fatal("expected respond to message error")
	}

	patchDescriptions := *fixture.patchDescriptions
	if len(patchDescriptions) < 2 {
		t.Fatalf("expected progress and failure edits: %#v", patchDescriptions)
	}

	if !containsFold(patchDescriptions[0], "Gathering context") {
		t.Fatalf("expected first progress edit to gather context: %#v", patchDescriptions)
	}

	if patchDescriptions[len(patchDescriptions)-1] != expectedError {
		t.Fatalf("unexpected final progress error: %#v", patchDescriptions)
	}
}

type geminiMediaPreparationFailureFixture struct {
	instance          *bot
	loadedConfig      config
	sourceMessage     *discordgo.Message
	patchDescriptions *[]string
}

func newGeminiMediaPreparationFailureFixture(
	t *testing.T,
	botUserID string,
	channelID string,
	userID string,
	sourceMessageID string,
	progressID string,
) geminiMediaPreparationFailureFixture {
	t.Helper()

	progressMessage := new(discordgo.Message)
	progressMessage.ID = progressID
	progressMessage.ChannelID = channelID
	progressMessage.Author = newDiscordUser(botUserID, true)

	stageEdited := make(chan struct{})

	var stageEditedOnce sync.Once

	patchDescriptions := make([]string, 0, 3)
	patchTimes := make([]time.Time, 0, 2)
	messageSendCount := 0

	session := newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/typing":
			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages":
			messageSendCount++
			if messageSendCount > 1 {
				t.Fatalf("unexpected additional message send: %s %s", request.Method, request.URL.Path)
			}

			return newJSONResponse(t, request, progressMessage), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/"+progressID:
			patchDescriptions = append(patchDescriptions, requestEmbedDescription(t, request))
			patchTimes = append(patchTimes, time.Now())

			if len(patchDescriptions) == 1 {
				stageEditedOnce.Do(func() {
					close(stageEdited)
				})

				return newJSONResponse(t, request, progressMessage), nil
			}

			if time.Since(patchTimes[0]) < editDelay/2 {
				return nil, errProgressMessageEditedTooQuickly
			}

			return newJSONResponse(t, request, progressMessage), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}))

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newUnavailableGeminiMediaPreparationChatClient(t, stageEdited)

	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.MaxText = defaultMaxText
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	seedGeminiMediaPreparationFailureSource(instance, sourceMessage)

	return geminiMediaPreparationFailureFixture{
		instance:          instance,
		loadedConfig:      loadedConfig,
		sourceMessage:     sourceMessage,
		patchDescriptions: &patchDescriptions,
	}
}

func seedGeminiMediaPreparationFailureSource(
	instance *bot,
	sourceMessage *discordgo.Message,
) {
	sourceNode := instance.nodes.getOrCreate(sourceMessage.ID)
	sourceNode.initialized = true
	sourceNode.role = messageRoleUser
	sourceNode.text = "summarize this clip"
	sourceNode.urlScanText = sourceNode.text
	sourceNode.media = []contentPart{
		{
			"type":               contentTypeAudioData,
			contentFieldBytes:    []byte("audio-bytes"),
			contentFieldMIMEType: "audio/mpeg",
			contentFieldFilename: "clip.mp3",
		},
	}
}

func TestRespondToMessageSendsRateLimitErrorWhenProgressMessageSendFails(t *testing.T) {
	t.Parallel()

	const (
		botUserID       = "bot-user"
		channelID       = "channel-1"
		userID          = "user-1"
		sourceMessageID = "user-message-1"
		failureID       = "failure-message"
		expectedError   = "stream response: rate limited"
	)

	failureMessage := new(discordgo.Message)
	failureMessage.ID = failureID
	failureMessage.ChannelID = channelID
	failureMessage.Author = newDiscordUser(botUserID, true)

	messageDescriptions := make([]string, 0, 2)
	messageSendCount := 0
	session := newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/typing":
			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages":
			messageDescriptions = append(messageDescriptions, requestEmbedDescription(t, request))
			messageSendCount++

			if messageSendCount == 1 {
				return nil, errSendProgressMessageFailed
			}

			return newJSONResponse(t, request, failureMessage), nil
		case request.Method == http.MethodPatch:
			t.Fatalf("unexpected progress message edit: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}))

	instance := newRateLimitedRespondToMessageBot(session)

	err := instance.respondToMessage(
		context.Background(),
		newRateLimitedRespondToMessageConfig(),
		newRateLimitedRespondToMessageSourceMessage(channelID, sourceMessageID, userID),
		"openai/main-model",
	)
	if err == nil {
		t.Fatal("expected respond to message error")
	}

	if messageSendCount != 2 {
		t.Fatalf("unexpected message send count: %d", messageSendCount)
	}

	if messageDescriptions[1] != expectedError {
		t.Fatalf("unexpected fallback error message: %#v", messageDescriptions)
	}
}

func TestRespondToMessageContinuesWhenAttachmentDownloadAlwaysFails(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		attachmentURL      = "https://cdn.discordapp.com/attachments/test/context.txt"
	)

	fixture := newRespondToMessageAttachmentFailureFixture(
		t,
		botUserID,
		channelID,
		userID,
		sourceMessageID,
		assistantMessageID,
		attachmentURL,
	)

	err := fixture.instance.respondToMessage(
		context.Background(),
		fixture.loadedConfig,
		fixture.sourceMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("respond to message: %v", err)
	}

	if !fixture.typingSent.Load() {
		t.Fatal("expected typing indicator to be sent")
	}

	if !fixture.progressSent.Load() {
		t.Fatal("expected progress embed to be sent")
	}
}

type respondToMessageAttachmentFailureFixture struct {
	instance      *bot
	loadedConfig  config
	sourceMessage *discordgo.Message
	typingSent    *atomic.Bool
	progressSent  *atomic.Bool
}

func newRespondToMessageAttachmentFailureFixture(
	t *testing.T,
	botUserID string,
	channelID string,
	userID string,
	sourceMessageID string,
	assistantMessageID string,
	attachmentURL string,
) respondToMessageAttachmentFailureFixture {
	t.Helper()

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantMessageID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.Type = discordgo.MessageTypeReply

	typingSent := new(atomic.Bool)
	progressSent := new(atomic.Bool)
	probe := typingPreprocessingProbe{
		typingSent:        typingSent,
		progressSent:      progressSent,
		attachmentFetched: new(atomic.Bool),
	}

	session := newRespondToMessageTypingSession(
		t,
		channelID,
		botUserID,
		assistantMessage,
		probe,
	)

	chatClient := newRespondToMessageAttachmentFailureChatClient(t)

	instance := new(bot)
	instance.session = session
	instance.chatCompletions = chatClient
	instance.nodes = newMessageNodeStore(10)
	instance.httpClient = new(http.Client)
	instance.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodGet || request.URL.String() != attachmentURL {
			t.Fatalf("unexpected attachment request: %s %s", request.Method, request.URL.String())
		}

		return nil, temporaryAttachmentNetError{}
	})

	loadedConfig := testSearchConfig()
	loadedConfig.MaxText = defaultMaxText
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages
	loadedConfig.UsePlainResponses = true

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	sourceMessage.Content = "<@" + botUserID + ">"
	sourceMessage.Mentions = []*discordgo.User{newDiscordUser(botUserID, false)}
	sourceMessage.Attachments = []*discordgo.MessageAttachment{{
		ContentType: "text/plain",
		Filename:    "context.txt",
		URL:         attachmentURL,
	}}

	return respondToMessageAttachmentFailureFixture{
		instance:      instance,
		loadedConfig:  loadedConfig,
		sourceMessage: sourceMessage,
		typingSent:    typingSent,
		progressSent:  progressSent,
	}
}

func newRespondToMessageAttachmentFailureChatClient(t *testing.T) *stubChatCompletionClient {
	t.Helper()

	return newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if request.ConfiguredModel == testSearchDeciderModel {
			return handle(newStreamDelta(`{"needs_search":false}`, ""))
		}

		if request.ConfiguredModel != "openai/main-model" {
			t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
		}

		if len(request.Messages) == 0 {
			t.Fatalf("unexpected request message count: %d", len(request.Messages))
		}

		latest := request.Messages[len(request.Messages)-1]

		latestText := messageContentText(latest.Content)
		if !containsFold(latestText, attachmentDownloadFallbackText) {
			t.Fatalf("expected attachment fallback in latest user content: %q", latestText)
		}

		err := handle(newStreamDelta("assistant reply", ""))
		if err != nil {
			return err
		}

		return handle(newStreamDelta("", finishReasonStop))
	})
}

func TestBuildMessageConversationKeepsFollowUpQueryPlainForRepliedTextAttachment(t *testing.T) {
	t.Parallel()

	const (
		botUserID       = "bot-user"
		channelID       = "channel-1"
		userID          = "user-1"
		parentMessageID = "user-message-1"
		sourceMessageID = "user-message-2"
		attachmentURL   = "https://cdn.discordapp.com/attachments/test/context.txt"
	)

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.State.User = newDiscordUser(botUserID, true)

	channel := new(discordgo.Channel)
	channel.ID = channelID
	channel.Type = discordgo.ChannelTypeDM

	err = session.State.ChannelAdd(channel)
	if err != nil {
		t.Fatalf("add channel to state: %v", err)
	}

	parentMessage := new(discordgo.Message)
	parentMessage.ID = parentMessageID
	parentMessage.ChannelID = channelID
	parentMessage.Author = newDiscordUser(userID, false)
	parentMessage.Attachments = []*discordgo.MessageAttachment{
		{
			Filename: "context.txt",
			URL:      attachmentURL,
		},
	}

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "at ai what is the text inside this file"
	sourceMessage.MessageReference = parentMessage.Reference()
	sourceMessage.ReferencedMessage = parentMessage

	instance := new(bot)
	instance.session = session
	instance.httpClient = newTextAttachmentDownloadClient(t, attachmentURL, "jandron")
	instance.nodes = newMessageNodeStore(10)

	loadedConfig := testSearchConfig()
	loadedConfig.MaxText = defaultMaxText
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages

	messages, _, err := instance.buildMessageConversation(
		context.Background(),
		loadedConfig,
		sourceMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("build message conversation: %v", err)
	}

	if len(messages) != 2 {
		t.Fatalf("unexpected conversation length: %d", len(messages))
	}

	if messages[0].Content != "jandron" {
		t.Fatalf("unexpected replied message content: %#v", messages[0].Content)
	}

	latestContent, ok := messages[1].Content.(string)
	if !ok {
		t.Fatalf("unexpected latest message content type: %T", messages[1].Content)
	}

	expectedLatestContent := "what is the text inside this file"
	if latestContent != expectedLatestContent {
		t.Fatalf("unexpected latest message content: got %q want %q", latestContent, expectedLatestContent)
	}

	if containsFold(latestContent, replyTargetSectionName+":") {
		t.Fatalf("unexpected replied message section in latest content: %q", latestContent)
	}
}

func TestBuildMessageConversationKeepsFollowUpQueryPlainWhenReplyingToAssistant(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "676735636656357396"
		sourceMessageID    = "source-message"
		assistantMessageID = "assistant-message"
		followUpMessageID  = "follow-up-message"
	)

	instance := newHistoryRetentionTestBot(t)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "at ai my name is Jandron"
	setCachedUserNode(instance, sourceMessage, nil, "my name is Jandron")

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantMessageID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.MessageReference = sourceMessage.Reference()
	assistantMessage.Type = discordgo.MessageTypeReply

	assistantNode := instance.nodes.getOrCreate(assistantMessage.ID)
	assistantNode.mu.Lock()
	assistantNode.role = messageRoleAssistant
	assistantNode.text = testAssistantNameReminder
	assistantNode.thinkingText = ""
	assistantNode.urlScanText = testAssistantNameReminder
	assistantNode.parentMessage = sourceMessage
	assistantNode.initialized = true
	instance.nodes.cacheLockedNode(assistantMessage.ID, assistantNode)
	assistantNode.mu.Unlock()

	followUpMessage := new(discordgo.Message)
	followUpMessage.ID = followUpMessageID
	followUpMessage.ChannelID = channelID
	followUpMessage.Author = newDiscordUser(userID, false)
	followUpMessage.Content = "at ai what is my name again"
	followUpMessage.MessageReference = assistantMessage.Reference()
	followUpMessage.ReferencedMessage = assistantMessage
	setCachedUserNode(instance, followUpMessage, assistantMessage, "what is my name again")

	loadedConfig := testSearchConfig()
	loadedConfig.MaxText = defaultMaxText
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages

	messages, _, err := instance.buildMessageConversation(
		context.Background(),
		loadedConfig,
		followUpMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("build message conversation: %v", err)
	}

	if len(messages) != 3 {
		t.Fatalf("unexpected conversation length: %d", len(messages))
	}

	if messages[1].Role != messageRoleAssistant ||
		messages[1].Content != testAssistantNameReminder {
		t.Fatalf("unexpected assistant reply content: %#v", messages[1])
	}

	latestContent, ok := messages[2].Content.(string)
	if !ok {
		t.Fatalf("unexpected latest message content type: %T", messages[2].Content)
	}

	expectedLatestContent := "what is my name again"
	if latestContent != expectedLatestContent {
		t.Fatalf("unexpected latest message content: got %q want %q", latestContent, expectedLatestContent)
	}

	if containsFold(latestContent, replyTargetSectionName+":") {
		t.Fatalf("unexpected replied message section in latest content: %q", latestContent)
	}
}

func TestSourceMessageURLExtractionTextSkipsTextAttachmentURLs(t *testing.T) {
	t.Parallel()

	const (
		botUserID            = "bot-user"
		channelID            = "channel-1"
		userID               = "user-1"
		parentMessageID      = "user-message-1"
		sourceMessageID      = "user-message-2"
		parentAttachmentURL  = "https://cdn.discordapp.com/attachments/test/replied-context.txt"
		sourceAttachmentURL  = "https://cdn.discordapp.com/attachments/test/source-context.txt"
		parentTypedURL       = "https://example.com/manual"
		sourceTypedURL       = "https://youtu.be/dQw4w9WgXcQ"
		parentAttachmentText = "https://www.reddit.com/r/testing/comments/abc123/thread-title/"
		sourceAttachmentText = "https://example.com/from-file"
	)

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.State.User = newDiscordUser(botUserID, true)

	channel := new(discordgo.Channel)
	channel.ID = channelID
	channel.Type = discordgo.ChannelTypeDM

	err = session.State.ChannelAdd(channel)
	if err != nil {
		t.Fatalf("add channel to state: %v", err)
	}

	parentMessage := new(discordgo.Message)
	parentMessage.ID = parentMessageID
	parentMessage.ChannelID = channelID
	parentMessage.Author = newDiscordUser(userID, false)
	parentMessage.Content = parentTypedURL
	parentMessage.Attachments = []*discordgo.MessageAttachment{
		{
			ContentType: "text/plain",
			Filename:    "replied-context.txt",
			URL:         parentAttachmentURL,
		},
	}

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "at ai summarize " + sourceTypedURL
	sourceMessage.Attachments = []*discordgo.MessageAttachment{
		{
			ContentType: "text/plain",
			Filename:    "source-context.txt",
			URL:         sourceAttachmentURL,
		},
	}
	sourceMessage.MessageReference = parentMessage.Reference()
	sourceMessage.ReferencedMessage = parentMessage

	instance := new(bot)
	instance.session = session
	instance.httpClient = newTextAttachmentDownloadClientMap(t, map[string]string{
		parentAttachmentURL: parentAttachmentText,
		sourceAttachmentURL: sourceAttachmentText,
	})
	instance.nodes = newMessageNodeStore(10)

	urlExtractionText := instance.sourceMessageURLExtractionText(
		context.Background(),
		sourceMessage,
	)

	if !containsFold(urlExtractionText, parentTypedURL) {
		t.Fatalf("expected replied message URL in extraction text: %q", urlExtractionText)
	}

	if !containsFold(urlExtractionText, sourceTypedURL) {
		t.Fatalf("expected source message URL in extraction text: %q", urlExtractionText)
	}

	if containsFold(urlExtractionText, parentAttachmentText) {
		t.Fatalf("unexpected replied attachment URL in extraction text: %q", urlExtractionText)
	}

	if containsFold(urlExtractionText, sourceAttachmentText) {
		t.Fatalf("unexpected source attachment URL in extraction text: %q", urlExtractionText)
	}
}

func TestSourceMessageURLExtractionTextIgnoresAssistantReplyURLs(t *testing.T) {
	t.Parallel()

	const (
		botUserID        = "bot-user"
		channelID        = "channel-1"
		userID           = "user-1"
		assistantMessage = "assistant-message-1"
		sourceMessageID  = "user-message-1"
		sourceTypedURL   = "https://example.com/manual"
	)

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.State.User = newDiscordUser(botUserID, true)

	channel := new(discordgo.Channel)
	channel.ID = channelID
	channel.Type = discordgo.ChannelTypeDM

	err = session.State.ChannelAdd(channel)
	if err != nil {
		t.Fatalf("add channel to state: %v", err)
	}

	replyTarget := new(discordgo.Message)
	replyTarget.ID = assistantMessage
	replyTarget.ChannelID = channelID
	replyTarget.Author = newDiscordUser(botUserID, true)
	replyTarget.Content = strings.Join([]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://www.reddit.com/r/testing/comments/abc123/thread-title/",
		"https://www.tiktok.com/@mikemhan/video/7614735539660442893",
		testFacebookURL,
		"https://example.com/from-assistant",
	}, " ")

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "at ai summarize " + sourceTypedURL
	sourceMessage.MessageReference = replyTarget.Reference()
	sourceMessage.ReferencedMessage = replyTarget

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)

	urlExtractionText := instance.sourceMessageURLExtractionText(
		context.Background(),
		sourceMessage,
	)

	if !containsFold(urlExtractionText, sourceTypedURL) {
		t.Fatalf("expected source message URL in extraction text: %q", urlExtractionText)
	}

	for _, unexpectedURL := range []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://www.reddit.com/r/testing/comments/abc123/thread-title/",
		"https://www.tiktok.com/@mikemhan/video/7614735539660442893",
		testFacebookURL,
		"https://example.com/from-assistant",
	} {
		if containsFold(urlExtractionText, unexpectedURL) {
			t.Fatalf("unexpected assistant reply URL in extraction text: %q", urlExtractionText)
		}
	}
}

type respondToMessageTypingFixture struct {
	instance          *bot
	loadedConfig      config
	sourceMessage     *discordgo.Message
	typingSent        *atomic.Bool
	progressSent      *atomic.Bool
	attachmentFetched *atomic.Bool
}

func newRespondToMessageTypingFixture(
	t *testing.T,
	channelID string,
	botUserID string,
	userID string,
	sourceMessageID string,
	attachmentURL string,
	assistantMessage *discordgo.Message,
) respondToMessageTypingFixture {
	t.Helper()

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	sourceMessage.Attachments = []*discordgo.MessageAttachment{
		{
			ContentType: "text/plain",
			Filename:    "context.txt",
			URL:         attachmentURL,
		},
	}
	assistantMessage.MessageReference = sourceMessage.Reference()

	typingSent := new(atomic.Bool)
	progressSent := new(atomic.Bool)
	attachmentFetched := new(atomic.Bool)
	probe := typingPreprocessingProbe{
		typingSent:        typingSent,
		progressSent:      progressSent,
		attachmentFetched: attachmentFetched,
	}

	session := newRespondToMessageTypingSession(
		t,
		channelID,
		botUserID,
		assistantMessage,
		probe,
	)
	chatClient := newRespondToMessageTypingChatClient(t, probe)
	attachmentClient := newRespondToMessageAttachmentClient(t, attachmentURL, probe)

	instance := new(bot)
	instance.session = session
	instance.httpClient = attachmentClient
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = chatClient

	loadedConfig := testSearchConfig()
	loadedConfig.MaxText = defaultMaxText
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages
	loadedConfig.UsePlainResponses = true

	return respondToMessageTypingFixture{
		instance:          instance,
		loadedConfig:      loadedConfig,
		sourceMessage:     sourceMessage,
		typingSent:        typingSent,
		progressSent:      progressSent,
		attachmentFetched: attachmentFetched,
	}
}

func newDirectMessageTestSession(
	t *testing.T,
	channelID string,
	botUserID string,
	transport roundTripFunc,
) *discordgo.Session {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.State.User = newDiscordUser(botUserID, true)

	channel := new(discordgo.Channel)
	channel.ID = channelID
	channel.Type = discordgo.ChannelTypeDM

	err = session.State.ChannelAdd(channel)
	if err != nil {
		t.Fatalf("add channel to state: %v", err)
	}

	client := new(http.Client)
	client.Transport = transport
	session.Client = client

	return session
}

func newProgressEditCaptureSession(
	t *testing.T,
	channelID string,
	botUserID string,
	progressMessage *discordgo.Message,
	patchDescriptions *[]string,
) *discordgo.Session {
	t.Helper()

	progressSent := false

	return newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/typing":
			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages":
			if progressSent {
				t.Fatalf("unexpected additional message send: %s %s", request.Method, request.URL.Path)
			}

			progressSent = true

			return newJSONResponse(t, request, progressMessage), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/"+progressMessage.ID:
			*patchDescriptions = append(*patchDescriptions, requestEmbedDescription(t, request))

			return newJSONResponse(t, request, progressMessage), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}))
}

func newRateLimitedRespondToMessageBot(session *discordgo.Session) *bot {
	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newRateLimitedRespondToMessageChatClient()

	return instance
}

func newRateLimitedRespondToMessageConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.MaxText = defaultMaxText
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages

	return loadedConfig
}

func newRateLimitedRespondToMessageSourceMessage(
	channelID, sourceMessageID, userID string,
) *discordgo.Message {
	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "at ai hello"

	return sourceMessage
}

func newUnavailableGeminiMediaPreparationChatClient(
	t *testing.T,
	stageEdited <-chan struct{},
) *stubChatCompletionClient {
	t.Helper()

	return newStubChatClient(func(
		ctx context.Context,
		request chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		t.Helper()

		if request.ConfiguredModel != testMediaAnalysisModel {
			t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
		}

		select {
		case <-stageEdited:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for gathering context progress edit")
		}

		return newUnavailableGeminiMediaPreparationChatError()
	})
}

func newUnavailableGeminiMediaPreparationChatError() error {
	return fmt.Errorf(
		"all configured API keys failed: %w",
		errors.Join(
			fmt.Errorf(
				"stream gemini content: %w",
				newTestUnavailableGeminiAPIErrorPointer(
					"This model is currently experiencing high demand.",
				),
			),
			fmt.Errorf(
				"stream gemini content: %w",
				newTestUnavailableGeminiAPIErrorPointer(
					"This model is currently experiencing high demand.",
				),
			),
		),
	)
}

func newUnavailableGeminiMediaPreparationProgressError() error {
	return fmt.Errorf(
		"augment prepared message response: %w",
		fmt.Errorf(
			"augment conversation with gemini media: %w",
			fmt.Errorf(
				"analyze media file 1 with gemini: %w",
				fmt.Errorf(
					"collect gemini media analysis: %w",
					newUnavailableGeminiMediaPreparationChatError(),
				),
			),
		),
	)
}

func newRateLimitedRespondToMessageChatClient() *stubChatCompletionClient {
	return newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		if request.ConfiguredModel == testSearchDeciderModel {
			err := handle(newStreamDelta(`{"needs_search":false}`, ""))
			if err != nil {
				return err
			}

			return handle(newStreamDelta("", finishReasonStop))
		}

		return providerStatusError{
			StatusCode: http.StatusTooManyRequests,
			Message:    "rate limited",
			RetryDelay: 0,
			Err:        os.ErrInvalid,
		}
	})
}

func requestEmbedDescription(t *testing.T, request *http.Request) string {
	t.Helper()

	var payload map[string]any

	err := json.NewDecoder(request.Body).Decode(&payload)
	if err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	embeds, embedsOK := payload["embeds"].([]any)
	if !embedsOK || len(embeds) != 1 {
		t.Fatalf("unexpected embeds payload: %#v", payload["embeds"])
	}

	embed, embedOK := embeds[0].(map[string]any)
	if !embedOK {
		t.Fatalf("unexpected embed payload: %#v", embeds[0])
	}

	description, descriptionOK := embed["description"].(string)
	if !descriptionOK {
		t.Fatalf("unexpected embed description: %#v", embed["description"])
	}

	return description
}

type typingPreprocessingProbe struct {
	typingSent        *atomic.Bool
	progressSent      *atomic.Bool
	attachmentFetched *atomic.Bool
}

func newRespondToMessageTypingSession(
	t *testing.T,
	channelID string,
	botUserID string,
	assistantMessage *discordgo.Message,
	probe typingPreprocessingProbe,
) *discordgo.Session {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.State.User = newDiscordUser(botUserID, true)

	channel := new(discordgo.Channel)
	channel.ID = channelID
	channel.Type = discordgo.ChannelTypeDM

	err = session.State.ChannelAdd(channel)
	if err != nil {
		t.Fatalf("add channel to state: %v", err)
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/typing" {
			if !probe.progressSent.Load() {
				t.Fatal("expected progress embed before typing indicator")
			}

			probe.typingSent.Store(true)

			return newNoContentResponse(request), nil
		}

		if request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages" {
			if probe.typingSent.Load() {
				t.Fatal("expected progress embed to be sent before the typing indicator")
			}

			probe.progressSent.Store(true)

			return newJSONResponse(t, request, assistantMessage), nil
		}

		if request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/"+assistantMessage.ID {
			if !probe.progressSent.Load() {
				t.Fatal("expected progress message before editing the final response")
			}

			return newJSONResponse(t, request, assistantMessage), nil
		}

		t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

		return nil, errUnexpectedTestRequest
	})
	session.Client = client

	return session
}

func newRespondToMessageTypingChatClient(
	t *testing.T,
	probe typingPreprocessingProbe,
) *stubChatCompletionClient {
	t.Helper()

	return newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if !probe.typingSent.Load() {
			t.Fatal("expected typing indicator before chat completion")
		}

		if request.ConfiguredModel == testSearchDeciderModel {
			return handle(newStreamDelta(`{"needs_search":false}`, ""))
		}

		if request.ConfiguredModel != "openai/main-model" {
			t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
		}

		if !probe.progressSent.Load() {
			t.Fatal("expected progress embed before chat completion")
		}

		if !probe.attachmentFetched.Load() {
			t.Fatal("expected attachment preprocessing before the main completion request")
		}

		err := handle(newStreamDelta("assistant reply", ""))
		if err != nil {
			return err
		}

		return handle(newStreamDelta("", finishReasonStop))
	})
}

func newRespondToMessageAttachmentClient(
	t *testing.T,
	attachmentURL string,
	probe typingPreprocessingProbe,
) *http.Client {
	t.Helper()

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodGet || request.URL.String() != attachmentURL {
			t.Fatalf("unexpected attachment request: %s %s", request.Method, request.URL.String())
		}

		probe.attachmentFetched.Store(true)

		if !probe.typingSent.Load() {
			t.Fatal("expected typing indicator before attachment download")
		}

		if !probe.progressSent.Load() {
			t.Fatal("expected progress embed before attachment download")
		}

		return newTextResponse(request, "attachment context"), nil
	})

	return client
}

func newTextResponse(request *http.Request, body string) *http.Response {
	response := new(http.Response)
	response.Status = httpStatusOKText
	response.StatusCode = http.StatusOK
	response.Body = io.NopCloser(strings.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header = make(http.Header)
	response.Request = request

	return response
}

func newTextAttachmentDownloadClient(t *testing.T, attachmentURL string, body string) *http.Client {
	t.Helper()

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodGet || request.URL.String() != attachmentURL {
			t.Fatalf("unexpected attachment request: %s %s", request.Method, request.URL.String())
		}

		return newTextResponse(request, body), nil
	})

	return client
}

func newTextAttachmentDownloadClientMap(
	t *testing.T,
	bodies map[string]string,
) *http.Client {
	t.Helper()

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodGet {
			t.Fatalf("unexpected attachment request method: %s", request.Method)
		}

		body, ok := bodies[request.URL.String()]
		if !ok {
			t.Fatalf("unexpected attachment request: %s %s", request.Method, request.URL.String())
		}

		return newTextResponse(request, body), nil
	})

	return client
}
