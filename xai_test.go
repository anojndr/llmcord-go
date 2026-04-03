package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	testXAIProviderResponseID = "resp_123"
	testXAIImageOutputID      = "ig_123"
	testXAIImageURL           = "https://assets.grok.com/generated/image.jpg"
	testXAIImageResultBase64  = "aW1hZ2UtYnl0ZXM="
)

func TestBuildChatCompletionRequestEnablesResponsesAPIForXAIProvider(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.BaseURL = "https://api.x.ai/v1"

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		xAIProviderName: *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"x-ai/grok-4": nil,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"x-ai/grok-4",
		[]chatMessage{{Role: messageRoleUser, Content: "hello"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if !request.Provider.UseResponsesAPI {
		t.Fatal("expected xAI request to use the Responses API")
	}
}

func TestBuildXAIResponsesRequestBodyDefaultsBridgeSourceAttribution(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("http://127.0.0.1:8787/v1")

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	sourceAttribution, ok := requestBody["source_attribution"].(map[string]any)
	if !ok {
		t.Fatalf("expected source attribution request body, got %#v", requestBody["source_attribution"])
	}

	if sourceAttribution["include_sources"] != true {
		t.Fatalf("expected include_sources=true, got %#v", sourceAttribution)
	}

	if sourceAttribution["include_search_queries"] != true {
		t.Fatalf("expected include_search_queries=true, got %#v", sourceAttribution)
	}
}

func TestBuildXAIResponsesRequestBodySkipsBridgeSourceAttributionForOfficialAPI(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("https://api.x.ai/v1")

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	if _, ok := requestBody["source_attribution"]; ok {
		t.Fatalf("expected official xAI API request to omit source_attribution: %#v", requestBody)
	}
}

func TestOpenAIClientStreamChatCompletionUsesXAIResponsesAPI(t *testing.T) {
	t.Parallel()

	server := newXAIResponsesStreamingTestServer(t)
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := newXAIResponsesStreamingRequest(server.URL + "/v1")

	var (
		joinedContent  strings.Builder
		finishReason   string
		usage          *tokenUsage
		providerRespID string
		searchMetadata *searchMetadata
	)

	err := client.streamChatCompletion(context.Background(), request, func(delta streamDelta) error {
		joinedContent.WriteString(delta.Content)

		if delta.Usage != nil {
			usage = cloneTokenUsage(delta.Usage)
		}

		if delta.FinishReason != "" {
			finishReason = delta.FinishReason
		}

		if delta.ProviderResponseID != "" {
			providerRespID = delta.ProviderResponseID
		}

		if delta.SearchMetadata != nil {
			searchMetadata = cloneSearchMetadata(delta.SearchMetadata)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("stream xAI responses chat completion: %v", err)
	}

	if joinedContent.String() != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", joinedContent.String())
	}

	if finishReason != finishReasonStop {
		t.Fatalf("unexpected finish reason: %q", finishReason)
	}

	if usage == nil || usage.Input != 12 || usage.Output != 34 {
		t.Fatalf("unexpected usage: %#v", usage)
	}

	if providerRespID != testXAIProviderResponseID {
		t.Fatalf("unexpected provider response id: %q", providerRespID)
	}

	if searchMetadata == nil {
		t.Fatal("expected xAI source attribution metadata from completed response")
	}

	if len(searchMetadata.Queries) != 1 || searchMetadata.Queries[0] != "latest ai news" {
		t.Fatalf("unexpected search queries: %#v", searchMetadata.Queries)
	}

	if len(searchMetadata.Results) != 1 {
		t.Fatalf("unexpected source result count: %#v", searchMetadata.Results)
	}

	sources := extractSearchSources(searchMetadata.Results[0].Text)
	if len(sources) != 1 {
		t.Fatalf("unexpected parsed source count: %#v", sources)
	}

	if sources[0].Title != "Example Source" || sources[0].URL != "https://example.com/source" {
		t.Fatalf("unexpected parsed source: %#v", sources[0])
	}
}

func TestOpenAIClientStreamChatCompletionStreamsXAIImageOutputOnce(t *testing.T) {
	t.Parallel()

	server := newXAIResponsesStreamingImageTestServer(t, true)
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := newXAIResponsesStreamingRequest(server.URL + "/v1")

	var joinedContent strings.Builder

	err := client.streamChatCompletion(context.Background(), request, func(delta streamDelta) error {
		joinedContent.WriteString(delta.Content)

		return nil
	})
	if err != nil {
		t.Fatalf("stream xAI responses image output: %v", err)
	}

	expected := testStreamedHelloText + "\n\nGenerated image:\n" + testXAIImageURL
	if joinedContent.String() != expected {
		t.Fatalf("unexpected streamed image content: got %q want %q", joinedContent.String(), expected)
	}
}

func TestOpenAIClientStreamChatCompletionFallsBackToCompletedXAIImageOutput(t *testing.T) {
	t.Parallel()

	server := newXAIResponsesStreamingImageTestServer(t, false)
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := newXAIResponsesStreamingRequest(server.URL + "/v1")

	var joinedContent strings.Builder

	err := client.streamChatCompletion(context.Background(), request, func(delta streamDelta) error {
		joinedContent.WriteString(delta.Content)

		return nil
	})
	if err != nil {
		t.Fatalf("stream xAI completed image output: %v", err)
	}

	expected := testStreamedHelloText + "\n\nGenerated image:\n" + testXAIImageURL
	if joinedContent.String() != expected {
		t.Fatalf("unexpected completed image content: got %q want %q", joinedContent.String(), expected)
	}
}

func TestOpenAIClientStreamChatCompletionCarriesCompletedXAIImageBytes(t *testing.T) {
	t.Parallel()

	server := newXAIResponsesStreamingImageTestServer(t, true)
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := newXAIResponsesStreamingRequest(server.URL + "/v1")

	var responseImages []responseImageAsset

	err := client.streamChatCompletion(context.Background(), request, func(delta streamDelta) error {
		if len(delta.ResponseImages) > 0 {
			responseImages = mergeResponseImageAssets(responseImages, delta.ResponseImages)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("stream xAI image bytes: %v", err)
	}

	if len(responseImages) != 1 {
		t.Fatalf("unexpected response images: %#v", responseImages)
	}

	if string(responseImages[0].Data) != "image-bytes" {
		t.Fatalf("unexpected response image bytes: %#v", responseImages[0])
	}
}

func TestAssignXAIPreviousResponseIDUsesAssistantAnchorAndTrimsHistory(t *testing.T) {
	t.Parallel()

	store := newMessageNodeStore(8)

	rootUser := newTestDiscordMessage("100")
	assistant := newTestDiscordMessage("200")
	firstFollowUp := newTestDiscordMessage("300")
	secondFollowUp := newTestDiscordMessage("400")

	setConversationNode(store, rootUser.ID, messageRoleUser, "", "", nil)
	setConversationNode(
		store,
		assistant.ID,
		messageRoleAssistant,
		testXAIProviderResponseID,
		"x-ai/grok-4",
		rootUser,
	)
	setConversationNode(store, firstFollowUp.ID, messageRoleUser, "", "", assistant)
	setConversationNode(store, secondFollowUp.ID, messageRoleUser, "", "", firstFollowUp)

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: true,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                       "",
		ConfiguredModel:             "x-ai/grok-4",
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		SessionID:                   "",
		PreviousResponseID:          "",
		Messages: []chatMessage{
			{Role: openAICodexRoleSystem, Content: "You are concise."},
			{Role: messageRoleUser, Content: "first question"},
			{Role: messageRoleAssistant, Content: "first answer"},
			{Role: messageRoleUser, Content: "follow-up one"},
			{Role: messageRoleUser, Content: "follow-up two"},
		},
	}

	assignXAIPreviousResponseID(&request, secondFollowUp, store, defaultMaxMessages)

	if request.PreviousResponseID != testXAIProviderResponseID {
		t.Fatalf("unexpected previous response id: %q", request.PreviousResponseID)
	}

	if len(request.Messages) != 2 {
		t.Fatalf("unexpected continuation message count: %d", len(request.Messages))
	}

	if request.Messages[0].Content != "follow-up one" || request.Messages[1].Content != "follow-up two" {
		t.Fatalf("unexpected continuation messages: %#v", request.Messages)
	}
}

func TestGenerateAndSendResponseStoresProviderResponseIDForXAIContinuation(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		assistantReplyText = "The number is 8294051736."
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	session := newResponseHistoryTestSession(t, channelID, botUserID, assistantMessage)
	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = fakeChatCompletionClient{
		deltas: []streamDelta{
			newStreamDelta(assistantReplyText, ""),
			{
				Thinking:           "",
				Content:            "",
				FinishReason:       finishReasonStop,
				Usage:              nil,
				ProviderResponseID: testXAIProviderResponseID,
				SearchMetadata:     nil,
				ResponseImages:     nil,
			},
		},
	}

	request := emptyChatCompletionRequest()
	request.ConfiguredModel = "x-ai/grok-4"

	tracker := newResponseTracker(sourceMessage, "")

	err := instance.generateAndSendResponse(
		context.Background(),
		request,
		tracker,
		nil,
		true,
	)
	if err != nil {
		t.Fatalf("generate and send response: %v", err)
	}

	followUpMessage := newFollowUpReplyMessage("user-message-2", channelID, userID, assistantMessage)
	setCachedUserNode(instance, followUpMessage, assistantMessage, followUpMessage.Content)

	gotResponseID := xAIConversationPreviousResponseID(
		request.ConfiguredModel,
		followUpMessage,
		instance.nodes,
		defaultMaxMessages,
	)
	if gotResponseID != testXAIProviderResponseID {
		t.Fatalf("unexpected stored provider response id: %q", gotResponseID)
	}

	assistantNode, found := instance.nodes.get(assistantMessageID)
	if !found {
		t.Fatalf("assistant node %q not found", assistantMessageID)
	}

	assistantNode.mu.Lock()
	defer assistantNode.mu.Unlock()

	if assistantNode.providerResponseID != testXAIProviderResponseID {
		t.Fatalf("unexpected assistant provider response id: %q", assistantNode.providerResponseID)
	}

	if assistantNode.providerResponseModel != request.ConfiguredModel {
		t.Fatalf("unexpected assistant provider response model: %q", assistantNode.providerResponseModel)
	}
}

func TestFinalizeXAIResponseAnswerParsesBridgeSourcesAndStripsAppendix(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("http://127.0.0.1:8787/v1")
	answerText := "Answer paragraph.\n\nSources\n" +
		"1. [Example Source](https://example.com/source) (example.com/source) via `latest ai news`\n" +
		"2. [Another Source](https://example.com/other) (example.com/other)\n\n" +
		"Search Queries\n" +
		"1. `latest ai news`\n"

	cleanedText, metadata := finalizeXAIResponseAnswer(request, answerText, nil)

	if cleanedText != "Answer paragraph." {
		t.Fatalf("unexpected cleaned answer text: %q", cleanedText)
	}

	if metadata == nil {
		t.Fatal("expected parsed xAI bridge search metadata")
	}

	if len(metadata.Queries) != 1 || metadata.Queries[0] != "latest ai news" {
		t.Fatalf("unexpected parsed queries: %#v", metadata.Queries)
	}

	if len(metadata.Results) != 2 {
		t.Fatalf("unexpected parsed result groups: %#v", metadata.Results)
	}

	firstResultSources := extractSearchSources(metadata.Results[0].Text)
	if len(firstResultSources) != 1 || firstResultSources[0].URL != "https://example.com/source" {
		t.Fatalf("unexpected scoped source parsing: %#v", firstResultSources)
	}

	secondResultSources := extractSearchSources(metadata.Results[1].Text)
	if len(secondResultSources) != 1 || secondResultSources[0].URL != "https://example.com/other" {
		t.Fatalf("unexpected unscoped source parsing: %#v", secondResultSources)
	}
}

func TestXAIStreamingVisibleAnswerTextHidesBridgeSourceAppendix(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("http://127.0.0.1:8787/v1")

	tests := []struct {
		name     string
		answer   string
		expected string
	}{
		{
			name:     "complete appendix heading is hidden",
			answer:   "Answer paragraph.\n\nSources\n1. [Example Source](https://example.com/source)",
			expected: "Answer paragraph.",
		},
		{
			name:     "partial appendix heading is hidden",
			answer:   "Answer paragraph.\n\nSo",
			expected: "Answer paragraph.",
		},
		{
			name:     "non appendix text stays visible",
			answer:   "Answer paragraph.\n\nSummary",
			expected: "Answer paragraph.\n\nSummary",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := xAIStreamingVisibleAnswerText(request, testCase.answer)
			if got != testCase.expected {
				t.Fatalf("unexpected visible answer text: got %q want %q", got, testCase.expected)
			}
		})
	}
}

func TestXAIStreamingVisibleAnswerTextLeavesOfficialAPIUntouched(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("https://api.x.ai/v1")
	answerText := "Answer paragraph.\n\nSources\n1. [Example Source](https://example.com/source)"

	if got := xAIStreamingVisibleAnswerText(request, answerText); got != answerText {
		t.Fatalf("unexpected official xAI streaming answer text: %q", got)
	}
}

func TestPersistentMessageNodeStoreRestoresXAIResponseIDAfterRestart(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "source-message"
		assistantMessageID = "assistant-message"
		followUpMessageID  = "follow-up-message"
	)

	backend := newTestMessageNodeStoreBackend()

	const storeKey = "message-history-xai-response-id-restart"

	initialInstance := newPersistentHistoryTestBot(t, backend, storeKey)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "at ai hello"

	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	setCachedAssistantNode(initialInstance, assistantMessage, sourceMessage)
	setProviderResponseOnNode(
		t,
		initialInstance,
		assistantMessage.ID,
		testXAIProviderResponseID,
		"x-ai/grok-4",
	)

	err := initialInstance.nodes.persist()
	if err != nil {
		t.Fatalf("persist message history: %v", err)
	}

	restartedInstance := newPersistentHistoryTestBot(t, backend, storeKey)
	followUpMessage := newRestartFollowUpMessage(
		followUpMessageID,
		channelID,
		userID,
		assistantMessage,
		"follow-up question",
	)
	setCachedUserNode(restartedInstance, followUpMessage, assistantMessage, followUpMessage.Content)

	gotResponseID := xAIConversationPreviousResponseID(
		"x-ai/grok-4",
		followUpMessage,
		restartedInstance.nodes,
		defaultMaxMessages,
	)
	if gotResponseID != testXAIProviderResponseID {
		t.Fatalf("unexpected restored provider response id: %q", gotResponseID)
	}
}

func newXAIResponsesStreamingTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		assertXAIResponsesRequest(t, request)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		flusher, responseOK := responseWriter.(http.Flusher)
		if !responseOK {
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

		writeStreamChunk(t, responseWriter, xAIResponseCompletedChunk())
		flusher.Flush()

		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func newXAIResponsesStreamingImageTestServer(
	t *testing.T,
	includeOutputItemDone bool,
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		assertXAIResponsesRequest(t, request)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		flusher, responseOK := responseWriter.(http.Flusher)
		if !responseOK {
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

		if includeOutputItemDone {
			writeStreamChunk(t, responseWriter, xAIResponseOutputItemDoneChunk())
			flusher.Flush()
		}

		writeStreamChunk(t, responseWriter, xAIResponseCompletedChunkWithImageOutput())
		flusher.Flush()

		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func assertXAIResponsesRequest(t *testing.T, request *http.Request) {
	t.Helper()

	if request.URL.Path != "/v1/responses" {
		t.Fatalf("unexpected path: %s", request.URL.Path)
	}

	if request.URL.Query().Get("api-version") != "2024-12-01-preview" {
		t.Fatalf("unexpected query string: %s", request.URL.RawQuery)
	}

	if request.Header.Get("Authorization") != "Bearer test-key" {
		t.Fatalf("unexpected authorization header: %q", request.Header.Get("Authorization"))
	}

	if request.Header.Get("X-Test") != "present" {
		t.Fatalf("unexpected extra header: %q", request.Header.Get("X-Test"))
	}

	var payload map[string]any

	err := json.NewDecoder(request.Body).Decode(&payload)
	if err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	if payload["model"] != "grok-4" {
		t.Fatalf("unexpected model: %#v", payload["model"])
	}

	if payload["stream"] != true {
		t.Fatalf("unexpected stream flag: %#v", payload["stream"])
	}

	sourceAttribution, sourceAttributionOK := payload["source_attribution"].(map[string]any)
	if !sourceAttributionOK {
		t.Fatalf("unexpected source_attribution payload: %#v", payload["source_attribution"])
	}

	if sourceAttribution["include_sources"] != true || sourceAttribution["include_search_queries"] != true {
		t.Fatalf("unexpected source_attribution settings: %#v", sourceAttribution)
	}

	inputPayload, inputOK := payload["input"].([]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input payload: %#v", payload["input"])
	}

	assertXAIResponsesSystemMessage(t, inputPayload[0])
	assertXAIResponsesUserMessage(t, inputPayload[1])
}

func assertXAIResponsesSystemMessage(t *testing.T, rawMessage any) {
	t.Helper()

	systemMessage, messageOK := rawMessage.(map[string]any)
	if !messageOK {
		t.Fatalf("unexpected system message payload: %#v", rawMessage)
	}

	if systemMessage["role"] != openAICodexRoleSystem || systemMessage["content"] != "You are concise." {
		t.Fatalf("unexpected system message: %#v", systemMessage)
	}
}

func assertXAIResponsesUserMessage(t *testing.T, rawMessage any) {
	t.Helper()

	userMessage, messageOK := rawMessage.(map[string]any)
	if !messageOK {
		t.Fatalf("unexpected user message payload: %#v", rawMessage)
	}

	userContent, contentOK := userMessage["content"].([]any)
	if !contentOK || len(userContent) != 2 {
		t.Fatalf("unexpected user content payload: %#v", userMessage["content"])
	}

	firstPart, firstPartOK := userContent[0].(map[string]any)
	if !firstPartOK {
		t.Fatalf("unexpected first user content part: %#v", userContent[0])
	}

	if firstPart["type"] != xAIResponsesInputTextType || firstPart["text"] != "What is in this image?" {
		t.Fatalf("unexpected first user content part: %#v", firstPart)
	}

	secondPart, secondPartOK := userContent[1].(map[string]any)
	if !secondPartOK {
		t.Fatalf("unexpected second user content part: %#v", userContent[1])
	}

	if secondPart["type"] != xAIResponsesInputImageType {
		t.Fatalf("unexpected second user content part: %#v", secondPart)
	}

	if secondPart["image_url"] != "data:image/png;base64,abc" {
		t.Fatalf("unexpected image_url: %#v", secondPart["image_url"])
	}
}

func xAIResponseCompletedChunk() string {
	return "data: {\"type\":\"response.completed\",\"response\":{" +
		"\"id\":\"" + testXAIProviderResponseID + "\"," +
		"\"status\":\"completed\"," +
		"\"usage\":{\"input_tokens\":12,\"output_tokens\":34}," +
		"\"source_attribution\":{" +
		"\"search_queries\":[\"latest ai news\"]," +
		"\"sources\":[{" +
		"\"title\":\"Example Source\"," +
		"\"url\":\"https://example.com/source\"," +
		"\"search_queries\":[\"latest ai news\"]" +
		"}]}}}\n\n"
}

func xAIResponseOutputItemDoneChunk() string {
	return "data: {\"type\":\"response.output_item.done\",\"item\":{" +
		"\"id\":\"" + testXAIImageOutputID + "\"," +
		"\"type\":\"image_generation_call\"," +
		"\"status\":\"completed\"," +
		"\"result_url\":\"" + testXAIImageURL + "\"," +
		"\"mime_type\":\"image/jpeg\"," +
		"\"action\":\"generate\"," +
		"\"prompt\":\"Generate an image of a cat.\"}}\n\n"
}

func xAIResponseCompletedChunkWithImageOutput() string {
	return "data: {\"type\":\"response.completed\",\"response\":{" +
		"\"id\":\"" + testXAIProviderResponseID + "\"," +
		"\"status\":\"completed\"," +
		"\"usage\":{\"input_tokens\":12,\"output_tokens\":34}," +
		"\"output\":[{" +
		"\"id\":\"" + testXAIImageOutputID + "\"," +
		"\"type\":\"image_generation_call\"," +
		"\"status\":\"completed\"," +
		"\"result\":\"" + testXAIImageResultBase64 + "\"," +
		"\"result_url\":\"" + testXAIImageURL + "\"," +
		"\"mime_type\":\"image/jpeg\"," +
		"\"action\":\"generate\"," +
		"\"prompt\":\"Generate an image of a cat.\"}]," +
		"\"source_attribution\":{" +
		"\"search_queries\":[\"latest ai news\"]," +
		"\"sources\":[{" +
		"\"title\":\"Example Source\"," +
		"\"url\":\"https://example.com/source\"," +
		"\"search_queries\":[\"latest ai news\"]" +
		"}]}}}\n\n"
}

func newXAIResponsesStreamingRequest(baseURL string) chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         baseURL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			ExtraHeaders: map[string]any{
				"X-Test": "present",
			},
			ExtraQuery: map[string]any{
				"api-version": "2024-12-01-preview",
			},
			ExtraBody: nil,
		},
		Model:                       "grok-4",
		ConfiguredModel:             "x-ai/grok-4",
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		SessionID:                   "",
		PreviousResponseID:          "",
		Messages: []chatMessage{
			{Role: openAICodexRoleSystem, Content: "You are concise."},
			{
				Role: messageRoleUser,
				Content: []contentPart{
					{"type": contentTypeText, "text": "What is in this image?"},
					{
						"type":      contentTypeImageURL,
						"image_url": map[string]string{"url": "data:image/png;base64,abc"},
					},
				},
			},
		},
	}
}

func newTestDiscordMessage(messageID string) *discordgo.Message {
	message := new(discordgo.Message)
	message.ID = messageID
	message.ChannelID = "channel-1"

	return message
}

func newAssistantReplyMessage(
	messageID string,
	author *discordgo.User,
	sourceMessage *discordgo.Message,
) *discordgo.Message {
	message := new(discordgo.Message)
	message.ID = messageID
	message.ChannelID = sourceMessage.ChannelID
	message.Author = author
	message.MessageReference = sourceMessage.Reference()
	message.Type = discordgo.MessageTypeReply

	return message
}

func setConversationNode(
	store *messageNodeStore,
	messageID string,
	role string,
	providerResponseID string,
	providerResponseModel string,
	parentMessage *discordgo.Message,
) {
	node := store.getOrCreate(messageID)

	node.mu.Lock()
	node.role = role
	node.providerResponseID = providerResponseID
	node.providerResponseModel = providerResponseModel
	node.parentMessage = parentMessage
	node.initialized = true
	node.mu.Unlock()
}

func setProviderResponseOnNode(
	t *testing.T,
	instance *bot,
	messageID string,
	providerResponseID string,
	providerResponseModel string,
) {
	t.Helper()

	node, found := instance.nodes.get(messageID)
	if !found {
		t.Fatalf("message node %q not found", messageID)
	}

	node.mu.Lock()
	node.providerResponseID = providerResponseID
	node.providerResponseModel = providerResponseModel
	instance.nodes.cacheLockedNode(messageID, node)
	node.mu.Unlock()
}
