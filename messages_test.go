package main

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/genai"
)

const testSearchDeciderModel = "openai/decider-model"

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

	if originalReasoningConfig["effort"] != "high" {
		t.Fatalf("unexpected mutation of original reasoning config: %#v", originalReasoningConfig)
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
		attachmentURL      = "https://attachments.example.com/context.txt"
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

func TestBuildMessageConversationAddsReplyTargetForRepliedTextAttachment(t *testing.T) {
	t.Parallel()

	const (
		botUserID       = "bot-user"
		channelID       = "channel-1"
		userID          = "user-1"
		parentMessageID = "user-message-1"
		sourceMessageID = "user-message-2"
		attachmentURL   = "https://attachments.example.com/context.txt"
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

	if messages[0].Content != "<@"+userID+">: jandron" {
		t.Fatalf("unexpected replied message content: %#v", messages[0].Content)
	}

	latestContent, ok := messages[1].Content.(string)
	if !ok {
		t.Fatalf("unexpected latest message content type: %T", messages[1].Content)
	}

	if !containsFold(latestContent, replyTargetSectionName+":") {
		t.Fatalf("expected replied message section in latest content: %q", latestContent)
	}

	if !containsFold(latestContent, "jandron") {
		t.Fatalf("expected replied attachment text in latest content: %q", latestContent)
	}

	if !containsFold(latestContent, "what is the text inside this file") {
		t.Fatalf("expected user question in latest content: %q", latestContent)
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
		parentAttachmentURL  = "https://attachments.example.com/replied-context.txt"
		sourceAttachmentURL  = "https://attachments.example.com/source-context.txt"
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
	response.Status = "200 OK"
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
