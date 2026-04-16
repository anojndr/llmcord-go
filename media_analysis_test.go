package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	testOpenAIBaseURL       = "https://api.example.com/v1"
	testSearchDeciderModel2 = "openai/decider-model"
)

func TestAppendMediaAnalysesToConversationPreservesImages(t *testing.T) {
	t.Parallel()

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": "<@123>: summarize this"},
				{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
			},
		},
	}

	augmentedConversation, err := appendMediaAnalysesToConversation(
		conversation,
		[]string{"audio result", "video result"},
	)
	if err != nil {
		t.Fatalf("append media analyses: %v", err)
	}

	parts, ok := augmentedConversation[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	textValue, _ := parts[0]["text"].(string)

	expectedText := expectedMediaAnalysisUserText(
		"<@123>: summarize this",
		[]string{"audio result", "video result"},
	)
	if textValue != expectedText {
		t.Fatalf("unexpected text value: %q", textValue)
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected image to be preserved: %#v", parts[1])
	}
}

func TestMaybeAugmentConversationWithGeminiMediaAppendsAnalysesForNonGeminiModel(t *testing.T) {
	t.Parallel()

	expectedAnalyses := []string{
		"Audio transcription per timestamp:\n\n0s to 10s: hello there",
		"Video description per timestamp:\n\n0s to 10s: somebody waves",
	}

	chatClient, callIndex := newGeminiMediaAnalysisChatClient(t, expectedAnalyses)
	instance, sourceMessage := newMediaAnalysisTestBot(
		chatClient,
		"message-1",
		[]contentPart{
			{
				"type":               contentTypeAudioData,
				contentFieldBytes:    []byte("audio-bytes"),
				contentFieldMIMEType: "audio/mpeg",
				contentFieldFilename: "clip.mp3",
			},
			{
				"type":               contentTypeVideoData,
				contentFieldBytes:    []byte("video-bytes"),
				contentFieldMIMEType: testVideoMIMEType,
				contentFieldFilename: "clip.mp4",
			},
		},
	)

	conversation := []chatMessage{
		{Role: messageRoleAssistant, Content: "Earlier answer"},
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": "<@123>: summarize this"},
				{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
			},
		},
	}

	augmentedConversation, err := instance.maybeAugmentConversationWithGeminiMedia(
		context.Background(),
		testMediaAnalysisConfig(),
		"openai/gpt-5",
		sourceMessage,
		conversation,
	)
	if err != nil {
		t.Fatalf("augment conversation with gemini media: %v", err)
	}

	if *callIndex != 2 {
		t.Fatalf("unexpected gemini analysis call count: %d", *callIndex)
	}

	parts, ok := augmentedConversation[1].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected augmented content type: %T", augmentedConversation[1].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected augmented part count: %d", len(parts))
	}

	textValue, _ := parts[0]["text"].(string)

	expectedText := expectedMediaAnalysisUserText(
		"<@123>: summarize this",
		expectedAnalyses,
	)
	if textValue != expectedText {
		t.Fatalf("unexpected augmented text: %q", textValue)
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected image to be preserved: %#v", parts[1])
	}
}

func TestMaybeAugmentConversationWithGeminiMediaRunsAnalysesConcurrentlyAndKeepsOrder(t *testing.T) {
	t.Parallel()

	const (
		audioAnalysis = "Audio transcription per timestamp:\n\n0s to 10s: hello from audio"
		videoAnalysis = "Video description per timestamp:\n\n0s to 10s: hello from video"
	)

	chatClient, callCount := newConcurrentGeminiMediaAnalysisChatClient(
		t,
		audioAnalysis,
		videoAnalysis,
	)

	instance, sourceMessage := newMediaAnalysisTestBot(
		chatClient,
		"message-concurrent-media",
		[]contentPart{
			{
				"type":               contentTypeAudioData,
				contentFieldBytes:    []byte("audio-bytes"),
				contentFieldMIMEType: "audio/mpeg",
				contentFieldFilename: "clip.mp3",
			},
			{
				"type":               contentTypeVideoData,
				contentFieldBytes:    []byte("video-bytes"),
				contentFieldMIMEType: testVideoMIMEType,
				contentFieldFilename: "clip.mp4",
			},
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	augmentedConversation, err := instance.maybeAugmentConversationWithGeminiMedia(
		ctx,
		testMediaAnalysisConfig(),
		"openai/gpt-5",
		sourceMessage,
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: summarize these files"}},
	)
	if err != nil {
		t.Fatalf("augment conversation with gemini media: %v", err)
	}

	if *callCount != 2 {
		t.Fatalf("unexpected gemini analysis call count: %d", *callCount)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected augmented content type: %T", augmentedConversation[0].Content)
	}

	expectedText := expectedMediaAnalysisUserText(
		"<@123>: summarize these files",
		[]string{audioAnalysis, videoAnalysis},
	)
	if content != expectedText {
		t.Fatalf("unexpected augmented text: %q", content)
	}
}

func TestMaybeAugmentConversationWithGeminiMediaAppendsReplyTargetAnalysesForNonGeminiModel(t *testing.T) {
	t.Parallel()

	expectedAnalyses := []string{
		"Audio transcription per timestamp:\n\n0s to 10s: reply target hello there",
		"Video description per timestamp:\n\n0s to 10s: reply target somebody waves",
	}

	chatClient, callIndex := newGeminiMediaAnalysisChatClient(t, expectedAnalyses)
	instance, sourceMessage := newMediaAnalysisTestBot(
		chatClient,
		"message-1-reply",
		nil,
	)

	replyTargetMessage := new(discordgo.Message)
	replyTargetMessage.ID = "message-1-parent"

	sourceMessage.MessageReference = replyTargetMessage.Reference()

	sourceNode := instance.nodes.getOrCreate(sourceMessage.ID)
	sourceNode.parentMessage = replyTargetMessage
	sourceNode.role = messageRoleUser
	sourceNode.text = "<@123>: summarize the replied media"

	replyTargetNode := instance.nodes.getOrCreate(replyTargetMessage.ID)
	replyTargetNode.initialized = true
	replyTargetNode.role = messageRoleUser
	replyTargetNode.media = []contentPart{
		{
			"type":               contentTypeAudioData,
			contentFieldBytes:    []byte("audio-bytes"),
			contentFieldMIMEType: "audio/mpeg",
			contentFieldFilename: "reply.mp3",
		},
		{
			"type":               contentTypeVideoData,
			contentFieldBytes:    []byte("video-bytes"),
			contentFieldMIMEType: testVideoMIMEType,
			contentFieldFilename: "reply.mp4",
		},
	}

	augmentedConversation, err := instance.maybeAugmentConversationWithGeminiMedia(
		context.Background(),
		testMediaAnalysisConfig(),
		"openai/gpt-5",
		sourceMessage,
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: summarize the replied media"}},
	)
	if err != nil {
		t.Fatalf("augment conversation with replied gemini media: %v", err)
	}

	if *callIndex != 2 {
		t.Fatalf("unexpected gemini analysis call count: %d", *callIndex)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected augmented content type: %T", augmentedConversation[0].Content)
	}

	expectedText := expectedMediaAnalysisUserText(
		"<@123>: summarize the replied media",
		expectedAnalyses,
	)
	if content != expectedText {
		t.Fatalf("unexpected augmented text: %q", content)
	}
}

func TestMaybeAugmentConversationWithGeminiMediaUsesAssistantReplyTargetSource(t *testing.T) {
	t.Parallel()

	expectedAnalyses := []string{
		"Audio transcription per timestamp:\n\n0s to 10s: original source hello there",
	}

	chatClient, callIndex := newGeminiMediaAnalysisChatClient(t, expectedAnalyses)
	instance, sourceMessage := newMediaAnalysisTestBot(
		chatClient,
		"message-1-assistant-reply",
		nil,
	)

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = "message-1-assistant"

	originalSourceMessage := new(discordgo.Message)
	originalSourceMessage.ID = "message-1-original"

	sourceMessage.MessageReference = assistantMessage.Reference()

	sourceNode := instance.nodes.getOrCreate(sourceMessage.ID)
	sourceNode.parentMessage = assistantMessage
	sourceNode.role = messageRoleUser
	sourceNode.text = "<@123>: summarize that original audio"

	assistantNode := instance.nodes.getOrCreate(assistantMessage.ID)
	assistantNode.initialized = true
	assistantNode.role = messageRoleAssistant
	assistantNode.text = "Earlier answer"
	assistantNode.parentMessage = originalSourceMessage

	originalSourceNode := instance.nodes.getOrCreate(originalSourceMessage.ID)
	originalSourceNode.initialized = true
	originalSourceNode.role = messageRoleUser
	originalSourceNode.media = []contentPart{
		{
			"type":               contentTypeAudioData,
			contentFieldBytes:    []byte("audio-bytes"),
			contentFieldMIMEType: "audio/mpeg",
			contentFieldFilename: "original.mp3",
		},
	}

	augmentedConversation, err := instance.maybeAugmentConversationWithGeminiMedia(
		context.Background(),
		testMediaAnalysisConfig(),
		"openai/gpt-5",
		sourceMessage,
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: summarize that original audio"}},
	)
	if err != nil {
		t.Fatalf("augment conversation with assistant reply target media: %v", err)
	}

	if *callIndex != 1 {
		t.Fatalf("unexpected gemini analysis call count: %d", *callIndex)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected augmented content type: %T", augmentedConversation[0].Content)
	}

	expectedText := expectedMediaAnalysisUserText(
		"<@123>: summarize that original audio",
		expectedAnalyses,
	)
	if content != expectedText {
		t.Fatalf("unexpected augmented text: %q", content)
	}
}

func TestMaybeAugmentConversationWithGeminiMediaSkipsGeminiModel(t *testing.T) {
	t.Parallel()

	chatClient := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		t.Fatal("unexpected gemini analysis request")

		return nil
	})

	instance, sourceMessage := newMediaAnalysisTestBot(
		chatClient,
		"message-2",
		[]contentPart{
			{
				"type":               contentTypeAudioData,
				contentFieldBytes:    []byte("audio-bytes"),
				contentFieldMIMEType: "audio/mpeg",
			},
		},
	)

	conversation := []chatMessage{
		{Role: messageRoleUser, Content: "<@123>: summarize this"},
	}

	augmentedConversation, err := instance.maybeAugmentConversationWithGeminiMedia(
		context.Background(),
		testMediaAnalysisConfig(),
		testMediaAnalysisModel,
		sourceMessage,
		conversation,
	)
	if err != nil {
		t.Fatalf("augment conversation with gemini media: %v", err)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if content != "<@123>: summarize this" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestMaybeAugmentConversationWithGeminiMediaRequiresGeminiModel(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newMediaAnalysisTestBot(
		newStubChatClient(func(
			_ context.Context,
			_ chatCompletionRequest,
			_ func(streamDelta) error,
		) error {
			t.Fatal("unexpected chat completion request")

			return nil
		}),
		"message-3",
		[]contentPart{
			{
				"type":               contentTypeAudioData,
				contentFieldBytes:    []byte("audio-bytes"),
				contentFieldMIMEType: "audio/mpeg",
			},
		},
	)

	_, err := instance.maybeAugmentConversationWithGeminiMedia(
		context.Background(),
		testSearchConfig(),
		"openai/main-model",
		sourceMessage,
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: summarize this"}},
	)
	if err == nil {
		t.Fatal("expected missing gemini model error")
	}

	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildConversationSuppressesUnsupportedWarningForReplyTargetGeminiMedia(t *testing.T) {
	t.Parallel()

	instance, sourceMessage := newMediaAnalysisTestBot(
		newStubChatClient(func(
			_ context.Context,
			_ chatCompletionRequest,
			_ func(streamDelta) error,
		) error {
			t.Fatal("unexpected chat completion request")

			return nil
		}),
		"message-4",
		nil,
	)

	replyTargetMessage := new(discordgo.Message)
	replyTargetMessage.ID = "message-4-parent"

	sourceMessage.MessageReference = replyTargetMessage.Reference()

	sourceNode := instance.nodes.getOrCreate(sourceMessage.ID)
	sourceNode.parentMessage = replyTargetMessage
	sourceNode.role = messageRoleUser
	sourceNode.text = "<@123>: summarize the replied media"

	replyTargetNode := instance.nodes.getOrCreate(replyTargetMessage.ID)
	replyTargetNode.initialized = true
	replyTargetNode.role = messageRoleUser
	replyTargetNode.media = []contentPart{
		{
			"type":               contentTypeAudioData,
			contentFieldBytes:    []byte("audio-bytes"),
			contentFieldMIMEType: "audio/mpeg",
		},
	}

	conversation, warnings := instance.buildConversation(
		context.Background(),
		sourceMessage,
		defaultMaxText,
		messageContentOptions{
			maxImages:                0,
			allowAudio:               false,
			allowDocuments:           false,
			allowFiles:               false,
			allowedDocumentMIMETypes: nil,
			allowVideo:               false,
		},
		defaultMaxMessages,
		true,
		false,
	)

	if len(conversation) != 1 {
		t.Fatalf("unexpected conversation length: %d", len(conversation))
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
}

func TestConfiguredGeminiMediaModelPrefersSearchDeciderModel(t *testing.T) {
	t.Parallel()

	loadedConfig := testMediaAnalysisFallbackConfig()
	loadedConfig.Models["google/gemini-3-pro-preview"] = nil
	loadedConfig.ModelOrder = append(loadedConfig.ModelOrder, "google/gemini-3-pro-preview")
	loadedConfig.SearchDeciderModel = "google/gemini-3-pro-preview"

	modelName, err := configuredGeminiMediaModel(loadedConfig)
	if err != nil {
		t.Fatalf("find configured gemini media model: %v", err)
	}

	if modelName != "google/gemini-3-pro-preview" {
		t.Fatalf("unexpected gemini media model: %q", modelName)
	}
}

func TestConfiguredGeminiMediaModelUsesConfiguredModel(t *testing.T) {
	t.Parallel()

	modelName, err := configuredGeminiMediaModel(testMediaAnalysisConfig())
	if err != nil {
		t.Fatalf("find configured gemini media model: %v", err)
	}

	if modelName != testMediaAnalysisModel {
		t.Fatalf("unexpected gemini media model: %q", modelName)
	}
}

func testMediaAnalysisConfig() config {
	loadedConfig := new(config)

	openAIProvider := new(providerConfig)
	openAIProvider.BaseURL = testOpenAIBaseURL

	geminiProvider := new(providerConfig)
	geminiProvider.Type = string(providerAPIKindGemini)

	loadedConfig.Providers = map[string]providerConfig{
		"openai": *openAIProvider,
		"google": *geminiProvider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/gpt-5":          nil,
		testSearchDeciderModel2: nil,
		testMediaAnalysisModel:  nil,
	}
	loadedConfig.ModelOrder = []string{
		"openai/gpt-5",
		testMediaAnalysisModel,
		testSearchDeciderModel2,
	}
	loadedConfig.SearchDeciderModel = testSearchDeciderModel2
	loadedConfig.MediaAnalysisModel = testMediaAnalysisModel

	return *loadedConfig
}

func testMediaAnalysisFallbackConfig() config {
	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.MediaAnalysisModel = ""

	return loadedConfig
}

func expectedMediaAnalysisUserText(userQuery string, analyses []string) string {
	return userQuery + "\n\n" + renderMediaAnalyses(analyses)
}

func newGeminiMediaAnalysisChatClient(
	t *testing.T,
	expectedAnalyses []string,
) (*stubChatCompletionClient, *int) {
	t.Helper()

	analysisByPartType := make(map[string][]string, 2)

	for _, analysis := range expectedAnalyses {
		switch {
		case strings.HasPrefix(analysis, "Audio "):
			analysisByPartType[contentTypeAudioData] = append(
				analysisByPartType[contentTypeAudioData],
				analysis,
			)
		case strings.HasPrefix(analysis, "Video "):
			analysisByPartType[contentTypeVideoData] = append(
				analysisByPartType[contentTypeVideoData],
				analysis,
			)
		default:
			t.Fatalf("unsupported media analysis fixture: %q", analysis)
		}
	}

	callIndex := 0
	partCallIndex := make(map[string]int, 2)

	var callMu sync.Mutex

	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		partType := geminiMediaRequestPartType(t, request)

		assertGeminiMediaAnalysisRequest(
			t,
			request,
			geminiMediaAnalysisPromptForPartType(t, partType),
			partType,
		)

		callMu.Lock()
		analysisIndex := partCallIndex[partType]
		partCallIndex[partType] = analysisIndex + 1
		callIndex++
		callMu.Unlock()

		partAnalyses := analysisByPartType[partType]
		if analysisIndex >= len(partAnalyses) {
			t.Fatalf("unexpected extra %s analysis request", partType)
		}

		err := handle(streamDelta{
			Thinking:           "",
			Content:            partAnalyses[analysisIndex],
			FinishReason:       finishReasonStop,
			Usage:              nil,
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
		if err != nil {
			return err
		}

		return nil
	})

	return chatClient, &callIndex
}

func newConcurrentGeminiMediaAnalysisChatClient(
	t *testing.T,
	audioAnalysis string,
	videoAnalysis string,
) (*stubChatCompletionClient, *int) {
	t.Helper()

	var (
		startedCount int
		startedMu    sync.Mutex
		release      = make(chan struct{})
		callCount    int
		callMu       sync.Mutex
	)

	chatClient := newStubChatClient(func(
		ctx context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		partType := geminiMediaRequestPartType(t, request)
		assertGeminiMediaAnalysisRequest(
			t,
			request,
			geminiMediaAnalysisPromptForPartType(t, partType),
			partType,
		)

		startedMu.Lock()
		startedCount++

		if startedCount == 2 {
			close(release)
		}
		startedMu.Unlock()

		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}

		callMu.Lock()
		callCount++
		callMu.Unlock()

		analysis := audioAnalysis
		if partType == contentTypeVideoData {
			analysis = videoAnalysis
		}

		return handle(streamDelta{
			Thinking:           "",
			Content:            analysis,
			FinishReason:       finishReasonStop,
			Usage:              nil,
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
	})

	return chatClient, &callCount
}

func geminiMediaAnalysisPromptForPartType(t *testing.T, partType string) string {
	t.Helper()

	switch partType {
	case contentTypeAudioData:
		return geminiAudioAnalysisPrompt
	case contentTypeVideoData:
		return geminiVideoAnalysisPrompt
	default:
		t.Fatalf("unexpected media part type: %q", partType)

		return ""
	}
}

func geminiMediaRequestPartType(
	t *testing.T,
	request chatCompletionRequest,
) string {
	t.Helper()

	contentParts, ok := request.Messages[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected request content type: %T", request.Messages[0].Content)
	}

	if len(contentParts) != 2 {
		t.Fatalf("unexpected request part count: %d", len(contentParts))
	}

	partType, _ := contentParts[1]["type"].(string)
	if partType == "" {
		t.Fatalf("missing media part type in request: %#v", contentParts[1])
	}

	return partType
}

func assertGeminiMediaAnalysisRequest(
	t *testing.T,
	request chatCompletionRequest,
	expectedPrompt string,
	expectedPartType string,
) {
	t.Helper()

	_, expectedModelName, err := splitConfiguredModel(testMediaAnalysisModel)
	if err != nil {
		t.Fatalf("split configured media analysis model: %v", err)
	}

	if request.Provider.APIKind != providerAPIKindGemini {
		t.Fatalf("unexpected provider API kind: %q", request.Provider.APIKind)
	}

	if request.Model != expectedModelName {
		t.Fatalf("unexpected model: %q", request.Model)
	}

	contentParts, ok := request.Messages[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected request content type: %T", request.Messages[0].Content)
	}

	if len(contentParts) != 2 {
		t.Fatalf("unexpected request part count: %d", len(contentParts))
	}

	prompt, _ := contentParts[0]["text"].(string)
	if prompt != expectedPrompt {
		t.Fatalf("unexpected gemini prompt: %q", prompt)
	}

	if contentParts[1]["type"] != expectedPartType {
		t.Fatalf("unexpected media part: %#v", contentParts[1])
	}
}

func newMediaAnalysisTestBot(
	chatClient chatCompletionStreamer,
	messageID string,
	media []contentPart,
) (*bot, *discordgo.Message) {
	instance := new(bot)
	instance.chatCompletions = chatClient
	instance.nodes = newMessageNodeStore(maxMessageNodes)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = messageID

	sourceNode := instance.nodes.getOrCreate(sourceMessage.ID)
	sourceNode.initialized = true
	sourceNode.media = media

	return instance, sourceMessage
}
