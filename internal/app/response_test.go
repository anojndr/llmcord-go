package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	providers "llmcord-go/internal/providers"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/genai"
)

type fakeChatCompletionClient struct {
	deltas []streamDelta
}

func (client fakeChatCompletionClient) StreamChatCompletion(
	_ context.Context,
	_ chatCompletionRequest,
	handle func(streamDelta) error,
) error {
	for _, delta := range client.deltas {
		err := handle(delta)
		if err != nil {
			return err
		}
	}

	return nil
}

func newTestGeminiAPIError(code int, message string) error {
	apiErr := new(genai.APIError)
	apiErr.Code = code
	apiErr.Message = message

	return *apiErr
}

func newTestUnavailableGeminiAPIErrorPointer(message string) error {
	apiErr := new(genai.APIError)
	apiErr.Code = http.StatusServiceUnavailable
	apiErr.Message = message

	return apiErr
}

func TestUserFacingResponseErrorReturnsRawErrorText(t *testing.T) {
	t.Parallel()

	for _, testCase := range userFacingResponseErrorRawTextCases() {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := userFacingResponseError(testCase.err)
			if got != testCase.expected {
				t.Fatalf(
					"unexpected user-facing error: got %q want %q",
					got,
					testCase.expected,
				)
			}
		})
	}
}

func userFacingResponseErrorRawTextCases() []struct {
	name     string
	err      error
	expected string
} {
	return []struct {
		name     string
		err      error
		expected string
	}{
		{
			name:     "nil error falls back to generic message",
			err:      nil,
			expected: "Couldn't generate a response right now. Try again.",
		},
		{
			name:     "gemini rate limit",
			err:      newTestGeminiAPIError(http.StatusTooManyRequests, "rate limited"),
			expected: newTestGeminiAPIError(http.StatusTooManyRequests, "rate limited").Error(),
		},
		{
			name:     "gemini access denied",
			err:      newTestGeminiAPIError(http.StatusForbidden, "permission denied"),
			expected: newTestGeminiAPIError(http.StatusForbidden, "permission denied").Error(),
		},
		{
			name: "provider status error returns raw message",
			err: providers.StatusError{
				StatusCode: http.StatusNotFound,
				Message:    "model not found",
				Err:        os.ErrInvalid,
			},
			expected: "model not found",
		},
		{
			name:     "gemini missing file",
			err:      newTestGeminiAPIError(http.StatusNotFound, "file not found"),
			expected: newTestGeminiAPIError(http.StatusNotFound, "file not found").Error(),
		},
		{
			name:     "gemini gateway timeout",
			err:      newTestGeminiAPIError(http.StatusGatewayTimeout, "deadline exceeded"),
			expected: newTestGeminiAPIError(http.StatusGatewayTimeout, "deadline exceeded").Error(),
		},
		{
			name:     "gemini service unavailable pointer error",
			err:      newTestUnavailableGeminiAPIErrorPointer("service unavailable"),
			expected: newTestUnavailableGeminiAPIErrorPointer("service unavailable").Error(),
		},
		{
			name:     "context deadline exceeded returns raw message",
			err:      context.DeadlineExceeded,
			expected: "context deadline exceeded",
		},
		{
			name:     "unknown error returns raw message",
			err:      errPartialStreamFailure,
			expected: "partial stream failure",
		},
		{
			name: "oversized opaque error falls back to bounded provider message",
			err: providers.StatusError{
				StatusCode: http.StatusBadGateway,
				Message:    strings.Repeat("A", userFacingErrorMaxRunes+200),
				Err:        os.ErrInvalid,
			},
			expected: "The provider returned an invalid or oversized error response. Try again.",
		},
		{
			name: "oversized readable error is truncated",
			err: providers.StatusError{
				StatusCode: http.StatusBadGateway,
				Message:    strings.Repeat("readable error ", 200),
				Err:        os.ErrInvalid,
			},
			expected: truncateRunes(
				strings.Repeat("readable error ", 200),
				userFacingErrorMaxRunes-runeCount(" [truncated]"),
			) + " [truncated]",
		},
	}
}

func TestSegmentAccumulatorSplitsByRunes(t *testing.T) {
	t.Parallel()

	accumulator := newSegmentAccumulator(4)
	splitOccurred := accumulator.appendText("abecd")

	if !splitOccurred {
		t.Fatal("expected content to split across segments")
	}

	segments := accumulator.renderSegments()
	if len(segments) != 2 {
		t.Fatalf("unexpected segment count: %#v", segments)
	}

	if segments[0] != "abec" || segments[1] != "d" {
		t.Fatalf("unexpected segments: %#v", segments)
	}
}

func TestBuildRenderSpecsMarksSettledAndStreamingSegments(t *testing.T) {
	t.Parallel()

	specs := buildRenderSpecs([]string{"first", "second"}, "length", false, false, false)
	if len(specs) != 2 {
		t.Fatalf("unexpected spec count: %#v", specs)
	}

	if specs[0].content != "first" || specs[0].color != embedColorComplete {
		t.Fatalf("unexpected first spec: %#v", specs[0])
	}

	if specs[1].content != "second"+streamingIndicator ||
		specs[1].color != embedColorIncomplete {
		t.Fatalf("unexpected second spec: %#v", specs[1])
	}

	finalSpecs := buildRenderSpecs([]string{"only"}, "stop", true, false, false)
	if len(finalSpecs) != 1 {
		t.Fatalf("unexpected final spec count: %#v", finalSpecs)
	}

	if finalSpecs[0].content != "only" || finalSpecs[0].color != embedColorComplete {
		t.Fatalf("unexpected final spec: %#v", finalSpecs[0])
	}
}

func TestVisibleResponseSegmentsPrefixesThinking(t *testing.T) {
	t.Parallel()

	segments := visibleResponseSegments("Plan first.", "Final answer.", embedResponseMaxLength)
	if len(segments) != 1 {
		t.Fatalf("unexpected segment count: %#v", segments)
	}

	expected := "**Thinking**\nPlan first.\n\n**Answer**\nFinal answer."
	if segments[0] != expected {
		t.Fatalf("unexpected visible response: %q", segments[0])
	}
}

func TestBuildResponseButtonsGistButtonLabel(t *testing.T) {
	t.Parallel()

	buttons := buildResponseButtons(responseActions{showSources: false, showThinking: false, showGist: true})
	if len(buttons) != 1 {
		t.Fatalf("unexpected button count: %#v", buttons)
	}

	button, ok := buttons[0].(*discordgo.Button)
	if !ok {
		t.Fatalf("unexpected component type: %#v", buttons[0])
	}

	if button.CustomID != createGistButtonCustomID {
		t.Fatalf("unexpected gist button custom ID: %q", button.CustomID)
	}

	want := "View response better on GitHub Gist"
	if button.Label != want {
		t.Fatalf("unexpected gist button label: got %q, want %q", button.Label, want)
	}
}

func TestBuildResponseButtonsShowImagesButton(t *testing.T) {
	t.Parallel()

	buttons := buildResponseButtons(responseActions{showSources: false, showImages: true, showThinking: false, showGist: false})
	if len(buttons) != 1 {
		t.Fatalf("unexpected button count: %#v", buttons)
	}

	button, ok := buttons[0].(*discordgo.Button)
	if !ok {
		t.Fatalf("unexpected component type: %#v", buttons[0])
	}

	if button.CustomID != showImagesButtonCustomID {
		t.Fatalf("unexpected image button custom ID: %q", button.CustomID)
	}

	want := "Show Images"
	if button.Label != want {
		t.Fatalf("unexpected image button label: got %q, want %q", button.Label, want)
	}
}

func TestBuildRenderSpecsAddsSourcesButtonOnlyToFinalSearchedSegment(t *testing.T) {
	t.Parallel()

	specs := buildRenderSpecs([]string{"first", "second"}, "stop", true, true, false)
	if len(specs) != 2 {
		t.Fatalf("unexpected spec count: %#v", specs)
	}

	if specs[0].actions.showSources || specs[0].actions.showThinking || specs[0].actions.showGist || specs[0].actions.showImages {
		t.Fatalf("expected no action buttons on first segment: %#v", specs[0])
	}

	if !specs[1].actions.showSources || specs[1].actions.showThinking || !specs[1].actions.showGist || !specs[1].actions.showImages {
		t.Fatalf("expected sources, images, and gist buttons on final segment: %#v", specs[1])
	}
}

func TestBuildRenderSpecsAddsGistButtonToFinalNonSearchedSegment(t *testing.T) {
	t.Parallel()

	specs := buildRenderSpecs([]string{"only"}, "stop", true, false, false)
	if len(specs) != 1 {
		t.Fatalf("unexpected spec count: %#v", specs)
	}

	if specs[0].actions.showSources || specs[0].actions.showThinking {
		t.Fatalf("expected no sources button on non-searched response: %#v", specs[0])
	}

	if !specs[0].actions.showGist || !specs[0].actions.showImages {
		t.Fatalf("expected gist and images button on final non-searched response: %#v", specs[0])
	}
}

func TestBuildRenderSpecsAddsThinkingButtonOnlyToFinalSegment(t *testing.T) {
	t.Parallel()

	specs := buildRenderSpecs([]string{"first", "second"}, "stop", true, false, true)
	if len(specs) != 2 {
		t.Fatalf("unexpected spec count: %#v", specs)
	}

	if specs[0].actions.showThinking {
		t.Fatalf("expected no thinking button on first segment: %#v", specs[0])
	}

	if !specs[1].actions.showThinking {
		t.Fatalf("expected thinking button on final segment: %#v", specs[1])
	}
}

func TestHandleGeneratedStreamDeltaMergesSearchMetadataFromStream(t *testing.T) {
	t.Parallel()

	const query = "latest ai news"

	instance := new(bot)
	tracker := newResponseTracker(newTestDiscordMessage("source-message"), "")
	tracker.searchMetadata = &searchMetadata{
		Queries: nil,
		Results: nil,
		MaxURLs: 0,
		VisualSearchSources: []visualSearchSourceGroup{{
			Label: "Visual search",
			Sources: []searchSource{{
				Title: "Visual Source",
				URL:   "https://example.com/visual",
			}},
		}},
	}

	finishReason := ""
	lastRenderTime := time.Time{}
	state := generatedStreamState{
		request:             chatCompletionRequest{},
		warnings:            nil,
		answerAccumulator:   &segmentAccumulator{maxLength: embedResponseMaxLength, segments: []string{""}},
		thinkingAccumulator: &segmentAccumulator{maxLength: embedResponseMaxLength, segments: []string{""}},
		finishReason:        &finishReason,
		lastRenderTime:      &lastRenderTime,
		rawAnswerText:       "",
		renderedAnswerText:  "",
	}

	err := instance.handleGeneratedStreamDelta(
		context.Background(),
		tracker,
		&state,
		streamDelta{
			Thinking:           "",
			Content:            "",
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata: &searchMetadata{
				Queries: []string{query},
				Results: []webSearchResult{{
					Query: query,
					Text:  "Title: Example Source\nURL: https://example.com/source\n",
				}},
				MaxURLs:             1,
				VisualSearchSources: nil,
			},
		},
	)
	if err != nil {
		t.Fatalf("handle generated stream delta: %v", err)
	}

	if tracker.searchMetadata == nil {
		t.Fatal("expected merged search metadata on tracker")
	}

	if len(tracker.searchMetadata.Queries) != 1 || tracker.searchMetadata.Queries[0] != query {
		t.Fatalf("unexpected merged search queries: %#v", tracker.searchMetadata.Queries)
	}

	if len(tracker.searchMetadata.Results) != 1 {
		t.Fatalf("unexpected merged search results: %#v", tracker.searchMetadata.Results)
	}

	if len(tracker.searchMetadata.VisualSearchSources) != 1 {
		t.Fatalf("expected existing visual search metadata to be preserved: %#v", tracker.searchMetadata.VisualSearchSources)
	}
}

func TestNewReplyMessageDisablesReplyAuthorMention(t *testing.T) {
	t.Parallel()

	reference := new(discordgo.Message)
	reference.ID = "source-message"
	reference.ChannelID = "source-channel"

	send := newReplyMessage(reference)
	if send.AllowedMentions == nil {
		t.Fatal("expected allowed mentions to be configured")
	}

	if send.AllowedMentions.RepliedUser {
		t.Fatal("expected replied user mentions to be disabled")
	}

	expectedParse := []discordgo.AllowedMentionType{
		discordgo.AllowedMentionTypeRoles,
		discordgo.AllowedMentionTypeUsers,
		discordgo.AllowedMentionTypeEveryone,
	}

	if len(send.AllowedMentions.Parse) != len(expectedParse) {
		t.Fatalf("unexpected allowed mention parse count: %#v", send.AllowedMentions.Parse)
	}

	for index, mentionType := range expectedParse {
		if send.AllowedMentions.Parse[index] != mentionType {
			t.Fatalf(
				"unexpected allowed mention parse at %d: got %q want %q",
				index,
				send.AllowedMentions.Parse[index],
				mentionType,
			)
		}
	}

	if send.Reference == nil {
		t.Fatal("expected message reference to be set")
	}
}

func TestBuildResponseEmbedSetsConfiguredModelAsAuthor(t *testing.T) {
	t.Parallel()

	embed := buildResponseEmbed("hello", "openai/gpt-5.1", embedColorComplete, nil, "")
	if embed.Author == nil {
		t.Fatal("expected embed author to be set")
	}

	if embed.Author.Name != "openai/gpt-5.1" {
		t.Fatalf("unexpected embed author: %#v", embed.Author)
	}
}

func TestBuildResponseEmbedSetsFooterText(t *testing.T) {
	t.Parallel()

	const footerText = "model: openai/gpt-5.1"

	embed := buildResponseEmbed("hello", "openai/gpt-5.1", embedColorComplete, nil, footerText)
	if embed.Footer == nil {
		t.Fatal("expected embed footer to be set")
	}

	if embed.Footer.Text != footerText {
		t.Fatalf("unexpected embed footer: %#v", embed.Footer)
	}
}

func TestBuildResponseEmbedLeavesGeneratedImageURLInDescription(t *testing.T) {
	t.Parallel()

	embed := buildResponseEmbed(
		"Result.\n\nGenerated image:\n"+testGeneratedImageURL,
		"openai/gpt-5",
		embedColorComplete,
		nil,
		"",
	)
	if embed.Image != nil {
		t.Fatalf("expected embed image to remain unset: %#v", embed.Image)
	}

	if embed.Description != "Result.\n\nGenerated image:\n"+testGeneratedImageURL {
		t.Fatalf("unexpected embed description: %q", embed.Description)
	}
}

func TestPixelVaultResponseURLsDeduplicatesMarkdownLinks(t *testing.T) {
	t.Parallel()

	urls := pixelVaultResponseURLs(
		"See [https://img.pixelvault.dev/proj_xyz789/img_abc123.jpg](https://img.pixelvault.dev/proj_xyz789/img_abc123.jpg), " +
			"https://img.pixelvault.dev/proj_xyz789/img_abc124.png, and https://example.com/ignore.jpg.",
	)

	expectedURLs := []string{
		"https://img.pixelvault.dev/proj_xyz789/img_abc123.jpg",
		"https://img.pixelvault.dev/proj_xyz789/img_abc124.png",
	}
	if len(urls) != len(expectedURLs) {
		t.Fatalf("unexpected pixelVault url count: %#v", urls)
	}

	for index, expectedURL := range expectedURLs {
		if urls[index] != expectedURL {
			t.Fatalf("unexpected pixelVault url at %d: got %q want %q", index, urls[index], expectedURL)
		}
	}
}

func TestRenderFinalResponseResendsPixelVaultURLsWithoutBreakingReplyHistory(t *testing.T) {
	t.Parallel()
	testRenderFinalResponseResendsPixelVaultURLsWithoutBreakingReplyHistory(t)
}

func testRenderFinalResponseResendsPixelVaultURLsWithoutBreakingReplyHistory(t *testing.T) {
	t.Helper()

	const (
		botUserID         = "bot-user"
		channelID         = "channel-1"
		userID            = "user-1"
		sourceMessageID   = "user-message-1"
		responseID        = "assistant-message-1"
		pixelVaultReplyID = "assistant-message-2"
		modelName         = "openai/gpt-5"
		followUpText      = "repeat the image link"
		pixelVaultURL     = "https://img.pixelvault.dev/proj_xyz789/img_abc123.jpg"
	)

	answerText := "Result.\n\nGenerated image:\n" +
		"[https://img.pixelvault.dev/proj_xyz789/img_abc123.jpg](https://img.pixelvault.dev/proj_xyz789/img_abc123.jpg)"

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	responseMessage := newAssistantReplyMessage(
		responseID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)
	pixelVaultReplyMessage := newAssistantReplyMessage(
		pixelVaultReplyID,
		newDiscordUser(botUserID, true),
		responseMessage,
	)

	session := newPixelVaultReplyHistoryTestSession(
		t,
		channelID,
		botUserID,
		pixelVaultURL,
		responseMessage,
		pixelVaultReplyMessage,
	)
	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)

	tracker := newResponseTracker(sourceMessage, modelName)
	tracker.providerResponseID = testProviderResponseID

	accumulator := newSegmentAccumulator(embedResponseMaxLength)
	_ = accumulator.appendText(answerText)

	err := instance.renderFinalResponse(
		context.Background(),
		tracker,
		nil,
		&accumulator,
		"",
		finishReasonStop,
	)
	if err != nil {
		t.Fatalf("render final response: %v", err)
	}

	tracker.release(instance.nodes, answerText, "")

	if len(tracker.responseMessages) != 1 {
		t.Fatalf("unexpected tracked response message count: %d", len(tracker.responseMessages))
	}

	assertCachedPixelVaultReplyNode(
		t,
		instance.nodes,
		pixelVaultReplyID,
		responseID,
		testProviderResponseID,
		modelName,
	)
	assertPixelVaultReplyConversation(
		t,
		instance,
		channelID,
		userID,
		answerText,
		followUpText,
		pixelVaultReplyMessage,
	)
}

func newPixelVaultReplyHistoryTestSession(
	t *testing.T,
	channelID string,
	botUserID string,
	pixelVaultURL string,
	responseMessage *discordgo.Message,
	pixelVaultReplyMessage *discordgo.Message,
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

	postCount := 0
	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodPost ||
			request.URL.Path != "/api/v9/channels/"+channelID+"/messages" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
		}

		bodyBytes, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}

		postCount++

		var payload map[string]any

		err = json.Unmarshal(bodyBytes, &payload)
		if err != nil {
			t.Fatalf("decode request payload: %v", err)
		}

		if content, contentOK := payload["content"].(string); contentOK && content == pixelVaultURL {
			assertPlainReplyPayload(t, payload, pixelVaultURL, responseMessage.ID)

			return newJSONResponse(t, request, pixelVaultReplyMessage), nil
		}

		return newJSONResponse(t, request, responseMessage), nil
	})
	session.Client = client

	return session
}

func assertCachedPixelVaultReplyNode(
	t *testing.T,
	store *messageNodeStore,
	messageID string,
	parentMessageID string,
	providerResponseID string,
	providerResponseModel string,
) {
	t.Helper()

	pixelVaultReplyNode, nodeFound := store.get(messageID)
	if !nodeFound {
		t.Fatalf("expected cached pixelVault reply node for %q", messageID)
	}

	pixelVaultReplyNode.mu.Lock()
	defer pixelVaultReplyNode.mu.Unlock()

	if pixelVaultReplyNode.role != messageRoleAssistant {
		t.Fatalf("unexpected pixelVault reply role: %q", pixelVaultReplyNode.role)
	}

	if pixelVaultReplyNode.text != "" {
		t.Fatalf("expected pixelVault reply text to stay out of history, got %q", pixelVaultReplyNode.text)
	}

	if pixelVaultReplyNode.providerResponseID != providerResponseID {
		t.Fatalf("unexpected pixelVault provider response id: %q", pixelVaultReplyNode.providerResponseID)
	}

	if pixelVaultReplyNode.providerResponseModel != providerResponseModel {
		t.Fatalf("unexpected pixelVault provider response model: %q", pixelVaultReplyNode.providerResponseModel)
	}

	if pixelVaultReplyNode.parentMessage == nil || pixelVaultReplyNode.parentMessage.ID != parentMessageID {
		t.Fatalf("unexpected pixelVault reply parent: %#v", pixelVaultReplyNode.parentMessage)
	}
}

func assertPixelVaultReplyConversation(
	t *testing.T,
	instance *bot,
	channelID string,
	userID string,
	answerText string,
	followUpText string,
	pixelVaultReplyMessage *discordgo.Message,
) {
	t.Helper()

	followUpMessage := new(discordgo.Message)
	followUpMessage.ID = "user-message-2"
	followUpMessage.ChannelID = channelID
	followUpMessage.Author = newDiscordUser(userID, false)
	followUpMessage.Content = followUpText
	followUpMessage.MessageReference = pixelVaultReplyMessage.Reference()
	followUpMessage.ReferencedMessage = pixelVaultReplyMessage

	var contentOptions messageContentOptions

	conversation, warnings := instance.buildConversation(
		context.Background(),
		followUpMessage,
		contentOptions,
		defaultMaxMessages,
		false,
		false,
	)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if len(conversation) != 3 {
		t.Fatalf("unexpected conversation length: %#v", conversation)
	}

	if conversation[0].Role != messageRoleUser ||
		conversation[0].Content != "generate a random 10-digit number" {
		t.Fatalf("unexpected source message: %#v", conversation[0])
	}

	if conversation[1].Role != messageRoleAssistant ||
		conversation[1].Content != answerText {
		t.Fatalf("unexpected assistant message: %#v", conversation[1])
	}

	if conversation[2].Role != messageRoleUser ||
		conversation[2].Content != followUpText {
		t.Fatalf("unexpected follow-up message: %#v", conversation[2])
	}
}

func TestEditEmbedMessageLeavesGeneratedImageURLInDescription(t *testing.T) {
	t.Parallel()

	const (
		channelID = "channel-1"
		messageID = "message-1"
	)

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	sessionClient := new(http.Client)
	sessionClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodPatch ||
			request.URL.Path != "/api/v9/channels/"+channelID+"/messages/"+messageID {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
		}

		assertRequestEmbed(
			t,
			request,
			"openai/gpt-5",
			"Result.\n\nGenerated image:\n"+testGeneratedImageURL,
			"",
		)

		editedMessage := new(discordgo.Message)
		editedMessage.ID = messageID
		editedMessage.ChannelID = channelID

		return newJSONResponse(t, request, editedMessage), nil
	})
	session.Client = sessionClient

	instance := new(bot)
	instance.session = session

	message := new(discordgo.Message)
	message.ID = messageID
	message.ChannelID = channelID

	embed := buildResponseEmbed(
		"Result.\n\nGenerated image:\n"+testGeneratedImageURL,
		"openai/gpt-5",
		embedColorComplete,
		nil,
		"",
	)

	err = instance.editEmbedMessage(
		message,
		embed,
		nil,
	)
	if err != nil {
		t.Fatalf("edit embed message: %v", err)
	}
}

func TestRenderEmbedResponseIncludesConfiguredModelAsAuthor(t *testing.T) {
	t.Parallel()

	const (
		channelID    = "channel-1"
		sourceID     = "source-message"
		modelName    = "openai/gpt-5.1"
		responseID   = "assistant-message"
		responseBody = "hello"
	)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceID
	sourceMessage.ChannelID = channelID

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodPost ||
			request.URL.Path != "/api/v9/channels/"+channelID+"/messages" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
		}

		assertRequestEmbed(t, request, modelName, responseBody, "")

		sentMessage := new(discordgo.Message)
		sentMessage.ID = responseID
		sentMessage.ChannelID = channelID

		return newJSONResponse(t, request, sentMessage), nil
	})
	session.Client = client

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)

	tracker := newResponseTracker(sourceMessage, modelName)

	err = instance.renderEmbedResponse(
		context.Background(),
		tracker,
		nil,
		[]string{responseBody},
		finishReasonStop,
		true,
		false,
	)
	if err != nil {
		t.Fatalf("render embed response: %v", err)
	}
}

func TestRenderEmbedResponseDeletesExtraMessagesWhenSegmentCountShrinks(t *testing.T) {
	t.Parallel()

	const (
		channelID     = "channel-1"
		sourceID      = "source-message"
		firstReplyID  = "assistant-message-1"
		secondReplyID = "assistant-message-2"
	)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceID
	sourceMessage.ChannelID = channelID

	postCount := 0
	deleteCount := 0
	session := newEmbedShrinkTestSession(
		t,
		channelID,
		firstReplyID,
		secondReplyID,
		&postCount,
		&deleteCount,
	)

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)

	tracker := newResponseTracker(sourceMessage, "openai/gpt-5.1")

	err := instance.renderEmbedResponse(
		context.Background(),
		tracker,
		nil,
		[]string{"first", "second"},
		finishReasonStop,
		false,
		false,
	)
	if err != nil {
		t.Fatalf("render initial embed response: %v", err)
	}

	err = instance.renderEmbedResponse(
		context.Background(),
		tracker,
		nil,
		[]string{"first"},
		finishReasonStop,
		true,
		false,
	)
	if err != nil {
		t.Fatalf("render collapsed embed response: %v", err)
	}

	if deleteCount != 1 {
		t.Fatalf("expected one deleted extra message, got %d", deleteCount)
	}

	if len(tracker.responseMessages) != 1 {
		t.Fatalf("unexpected response message count: %d", len(tracker.responseMessages))
	}

	if len(tracker.pendingResponses) != 1 {
		t.Fatalf("unexpected pending response count: %d", len(tracker.pendingResponses))
	}

	if len(tracker.renderedSpecs) != 1 {
		t.Fatalf("unexpected rendered spec count: %d", len(tracker.renderedSpecs))
	}

	if _, ok := instance.nodes.get(secondReplyID); ok {
		t.Fatal("expected deleted extra response node to be removed from the store")
	}
}

func newEmbedShrinkTestSession(
	t *testing.T,
	channelID string,
	firstReplyID string,
	secondReplyID string,
	postCount *int,
	deleteCount *int,
) *discordgo.Session {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages":
			(*postCount)++

			sentMessage := new(discordgo.Message)
			sentMessage.ChannelID = channelID

			switch *postCount {
			case 1:
				sentMessage.ID = firstReplyID
			case 2:
				sentMessage.ID = secondReplyID
			default:
				t.Fatalf("unexpected post count: %d", *postCount)
			}

			return newJSONResponse(t, request, sentMessage), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/"+firstReplyID:
			firstMessage := new(discordgo.Message)
			firstMessage.ID = firstReplyID
			firstMessage.ChannelID = channelID

			return newJSONResponse(t, request, firstMessage), nil
		case request.Method == http.MethodDelete &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/"+secondReplyID:
			(*deleteCount)++

			return newNoContentResponse(request), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	})
	session.Client = client

	return session
}

func TestGenerateAndSendResponseAppendsErrorWhenStreamFailsAfterPartialOutput(t *testing.T) {
	t.Parallel()
	testGenerateAndSendResponseAppendsErrorWhenStreamFailsAfterPartialOutput(t)
}

func testGenerateAndSendResponseAppendsErrorWhenStreamFailsAfterPartialOutput(t *testing.T) {
	t.Helper()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		partialText        = "partial reply"
		expectedError      = "stream response: partial stream failure"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	messageDescriptions := make([]string, 0, 3)
	patchDescriptions := make([]string, 0, 2)
	messageSendCount := 0
	session := newPartialFailureResponseSession(
		t,
		channelID,
		botUserID,
		assistantMessage,
		&messageDescriptions,
		&patchDescriptions,
		&messageSendCount,
	)
	instance := newPartialFailureResponseBot(session, partialText)

	err := instance.generateAndSendResponse(
		context.Background(),
		config{},
		chatCompletionRequest{},
		newResponseTracker(sourceMessage, ""),
		nil,
	)
	if err == nil {
		t.Fatal("expected generate and send response error")
	}

	if messageSendCount != 2 {
		t.Fatalf("unexpected message send count: %d", messageSendCount)
	}

	if len(patchDescriptions) == 0 {
		t.Fatal("expected partial response edit")
	}

	if !containsFold(patchDescriptions[len(patchDescriptions)-1], partialText) {
		t.Fatalf("unexpected partial response patch: %q", patchDescriptions[len(patchDescriptions)-1])
	}

	if !containsFold(messageDescriptions[len(messageDescriptions)-1], expectedError) {
		t.Fatalf("unexpected final error response: %q", messageDescriptions[len(messageDescriptions)-1])
	}
}

func newPartialFailureResponseSession(
	t *testing.T,
	channelID string,
	botUserID string,
	assistantMessage *discordgo.Message,
	messageDescriptions *[]string,
	patchDescriptions *[]string,
	messageSendCount *int,
) *discordgo.Session {
	t.Helper()

	return newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages":
			*messageDescriptions = append(*messageDescriptions, requestEmbedDescription(t, request))
			*messageSendCount++

			return newJSONResponse(t, request, assistantMessage), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/"+assistantMessage.ID:
			*patchDescriptions = append(*patchDescriptions, requestEmbedDescription(t, request))

			return newJSONResponse(t, request, assistantMessage), nil
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/typing":
			return newNoContentResponse(request), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}))
}

func newPartialFailureResponseBot(session *discordgo.Session, partialText string) *bot {
	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		err := handle(newStreamDelta(partialText, ""))
		if err != nil {
			return err
		}

		return errPartialStreamFailure
	})

	return instance
}

var errPartialStreamFailure = errors.New("partial stream failure")

var errUnexpectedTestRequest = errors.New("unexpected test request")

func TestGenerateAndSendResponseRetriesPrematureStreamUntilFinishReason(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		prematureText      = "truncated repl"
		finalText          = "full completed reply"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	messageDescriptions := make([]string, 0, 1)
	patchDescriptions := make([]string, 0, 6)
	messageSendCount := 0
	session := newPartialFailureResponseSession(
		t,
		channelID,
		botUserID,
		assistantMessage,
		&messageDescriptions,
		&patchDescriptions,
		&messageSendCount,
	)

	streamCalls := 0
	instance := newPrematureStreamResponseBot(session, func(_ chatCompletionRequest, handle func(streamDelta) error) error {
		streamCalls++

		if streamCalls <= 2 {
			return handle(newStreamDelta(prematureText, ""))
		}

		return handle(newStreamDelta(finalText, finishReasonStop))
	})

	err := instance.generateAndSendResponse(
		context.Background(),
		config{},
		chatCompletionRequest{},
		newResponseTracker(sourceMessage, ""),
		nil,
	)
	if err != nil {
		t.Fatalf("generate and send response: %v", err)
	}

	if streamCalls != 3 {
		t.Fatalf("unexpected stream call count: %d", streamCalls)
	}

	if messageSendCount != 1 {
		t.Fatalf("unexpected message send count: %d", messageSendCount)
	}

	if len(patchDescriptions) == 0 {
		t.Fatal("expected final response patch")
	}

	lastPatch := patchDescriptions[len(patchDescriptions)-1]
	if containsFold(lastPatch, prematureText) {
		t.Fatalf("retry duplicated truncated text in final patch: %q", lastPatch)
	}

	if !containsFold(lastPatch, finalText) {
		t.Fatalf("unexpected final response patch: %q", lastPatch)
	}
}

func TestGenerateAndSendResponseExhaustsPrematureStreamRetries(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		prematureText      = "truncated reply"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	messageDescriptions := make([]string, 0, 1)
	patchDescriptions := make([]string, 0, 8)
	messageSendCount := 0
	session := newPartialFailureResponseSession(
		t,
		channelID,
		botUserID,
		assistantMessage,
		&messageDescriptions,
		&patchDescriptions,
		&messageSendCount,
	)

	streamCalls := 0
	instance := newPrematureStreamResponseBot(session, func(_ chatCompletionRequest, handle func(streamDelta) error) error {
		streamCalls++

		return handle(newStreamDelta(prematureText, ""))
	})

	err := instance.generateAndSendResponse(
		context.Background(),
		config{},
		chatCompletionRequest{},
		newResponseTracker(sourceMessage, ""),
		nil,
	)
	if err != nil {
		t.Fatalf("generate and send response: %v", err)
	}

	if streamCalls != prematureStreamRetryMaxAttempts {
		t.Fatalf("unexpected stream call count: %d", streamCalls)
	}

	if messageSendCount != 1 {
		t.Fatalf("unexpected message send count: %d", messageSendCount)
	}

	if len(patchDescriptions) == 0 {
		t.Fatal("expected truncated response patch")
	}

	lastPatch := patchDescriptions[len(patchDescriptions)-1]
	if !containsFold(lastPatch, prematureText) {
		t.Fatalf("unexpected final response patch: %q", lastPatch)
	}
}

func TestGenerateAndSendResponseRetriesPrematureStreamOnFallbackModel(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		primaryModel       = "gemini-search/gemini-3.7-flash-medium:vision"
		fallbackModel      = "9router/stable_model:vision"
		prematureText      = "fallback truncated reply"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	messageDescriptions := make([]string, 0, 1)
	patchDescriptions := make([]string, 0, 8)
	messageSendCount := 0
	session := newPartialFailureResponseSession(
		t,
		channelID,
		botUserID,
		assistantMessage,
		&messageDescriptions,
		&patchDescriptions,
		&messageSendCount,
	)

	attemptedModels := make([]string, 0, prematureStreamRetryMaxAttempts+1)
	instance := newPrematureStreamResponseBot(session, func(request chatCompletionRequest, handle func(streamDelta) error) error {
		attemptedModels = append(attemptedModels, request.ConfiguredModel)

		if request.ConfiguredModel != fallbackModel {
			return errors.New("upstream primary model overloaded 503")
		}

		return handle(newStreamDelta(prematureText, ""))
	})

	loadedConfig := config{
		Providers: map[string]providerConfig{
			"gemini-search": {
				Name: "gemini-search",
			},
			"9router": {
				Name:    "9router",
				BaseURL: "http://localhost:20128/v1",
			},
		},
		Models: map[string]map[string]any{
			primaryModel:  nil,
			fallbackModel: nil,
		},
		FallbackModel: fallbackModel,
	}

	request := chatCompletionRequest{
		ConfiguredModel: primaryModel,
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: "Hello"},
		},
	}
	tracker := newResponseTracker(sourceMessage, request.ConfiguredModel)
	// The fallback path would otherwise consult an unconfigured decider.
	tracker.webSearchDecided = true

	err := instance.generateAndSendResponse(
		context.Background(),
		loadedConfig,
		request,
		tracker,
		nil,
	)
	if err != nil {
		t.Fatalf("generate and send response: %v", err)
	}

	if len(attemptedModels) != prematureStreamRetryMaxAttempts+1 {
		t.Fatalf("unexpected attempted model count: %v", attemptedModels)
	}

	if attemptedModels[0] != primaryModel {
		t.Fatalf("unexpected first attempted model: %q", attemptedModels[0])
	}

	for _, attemptedModel := range attemptedModels[1:] {
		if attemptedModel != fallbackModel {
			t.Fatalf("unexpected retried model: %v", attemptedModels)
		}
	}

	if len(patchDescriptions) == 0 {
		t.Fatal("expected truncated fallback response patch")
	}

	lastPatch := patchDescriptions[len(patchDescriptions)-1]
	if !containsFold(lastPatch, prematureText) {
		t.Fatalf("unexpected final response patch: %q", lastPatch)
	}
}

func TestRenderFinalResponseSkipsDuplicatePixelVaultURLRepliesAcrossAttempts(t *testing.T) {
	t.Parallel()

	const (
		botUserID         = "bot-user"
		channelID         = "channel-1"
		userID            = "user-1"
		sourceMessageID   = "user-message-1"
		responseID        = "assistant-message-1"
		pixelVaultReplyID = "assistant-message-2"
		modelName         = "openai/gpt-5"
		pixelVaultURL     = "https://img.pixelvault.dev/proj_xyz789/img_abc123.jpg"
	)

	answerText := "Result.\n\nGenerated image:\n" +
		"[https://img.pixelvault.dev/proj_xyz789/img_abc123.jpg](https://img.pixelvault.dev/proj_xyz789/img_abc123.jpg)"

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	responseMessage := newAssistantReplyMessage(
		responseID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)
	pixelVaultReplyMessage := newAssistantReplyMessage(
		pixelVaultReplyID,
		newDiscordUser(botUserID, true),
		responseMessage,
	)

	urlReplyCount := 0

	session, sessionErr := discordgo.New("Bot discord-token")
	if sessionErr != nil {
		t.Fatalf("create discord session: %v", sessionErr)
	}

	session.State.User = newDiscordUser(botUserID, true)

	channel := new(discordgo.Channel)
	channel.ID = channelID
	channel.Type = discordgo.ChannelTypeDM

	if err := session.State.ChannelAdd(channel); err != nil {
		t.Fatalf("add channel to state: %v", err)
	}

	session.Client = &http.Client{Transport: roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		bodyBytes, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}

		var payload map[string]any
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			t.Fatalf("decode request payload: %v", err)
		}

		if content, contentOK := payload["content"].(string); contentOK && containsFold(content, pixelVaultURL) {
			urlReplyCount++

			return newJSONResponse(t, request, pixelVaultReplyMessage), nil
		}

		return newJSONResponse(t, request, responseMessage), nil
	})}

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)

	tracker := newResponseTracker(sourceMessage, modelName)

	accumulator := newSegmentAccumulator(embedResponseMaxLength)
	_ = accumulator.appendText(answerText)

	// Two attempts finalize over the same tracker, mirroring a premature
	// stream close followed by a retry that completes with the same URL.
	for attempt := 0; attempt < 2; attempt++ {
		err := instance.renderFinalResponse(
			context.Background(),
			tracker,
			nil,
			&accumulator,
			"",
			finishReasonStop,
		)
		if err != nil {
			t.Fatalf("render final response attempt %d: %v", attempt+1, err)
		}
	}

	if urlReplyCount != 1 {
		t.Fatalf("unexpected pixelVault url reply count: %d", urlReplyCount)
	}
}

// newPrematureStreamResponseBot builds a bot whose chat completion streamer
// invokes stream for every attempt, so tests can count attempts, inspect the
// requested model, and decide per attempt whether the stream closes
// prematurely or completes.
func newPrematureStreamResponseBot(
	session *discordgo.Session,
	stream func(request chatCompletionRequest, handle func(streamDelta) error) error,
) *bot {
	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		return stream(request, handle)
	})

	return instance
}

func TestGenerateAndSendResponseKeepsAssistantReplyInConversationHistory(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		assistantReplyText = "@sweet_potet, your random 10-digit number is: 8294051736"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantMessageID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.MessageReference = sourceMessage.Reference()
	assistantMessage.Type = discordgo.MessageTypeReply
	session := newResponseHistoryTestSession(t, channelID, botUserID, assistantMessage)
	instance := newResponseHistoryTestBot(session, assistantReplyText)

	var request chatCompletionRequest

	tracker := newResponseTracker(sourceMessage, "")

	err := instance.generateAndSendResponse(
		context.Background(),
		config{},
		request,
		tracker,
		nil,
	)
	if err != nil {
		t.Fatalf("generate and send response: %v", err)
	}

	followUpMessage := newFollowUpReplyMessage("user-message-2", channelID, userID, assistantMessage)

	var contentOptions messageContentOptions

	conversation, warnings := instance.buildConversation(
		context.Background(),
		followUpMessage,
		contentOptions,
		defaultMaxMessages,
		false,
		false,
	)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	assertConversationHistory(
		t,
		conversation,
		assistantReplyText,
	)
}

func TestGenerateAndSendResponseShowsThinkingDuringStreamButNotFinalResponse(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		thoughtText        = "Plan first."
		answerText         = "Final answer."
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)
	messageDescriptions := make([]string, 0, 2)
	patchDescriptions := make([]string, 0, 2)
	messageSendCount := 0
	session := newPartialFailureResponseSession(
		t,
		channelID,
		botUserID,
		assistantMessage,
		&messageDescriptions,
		&patchDescriptions,
		&messageSendCount,
	)

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = fakeChatCompletionClient{
		deltas: thinkingAnswerResponseDeltas(thoughtText, answerText),
	}

	tracker := newResponseTracker(sourceMessage, "")

	err := instance.generateAndSendResponse(
		context.Background(),
		config{},
		chatCompletionRequest{},
		tracker,
		nil,
	)
	if err != nil {
		t.Fatalf("generate and send response: %v", err)
	}

	if len(messageDescriptions) == 0 {
		t.Fatal("expected rendered response message")
	}

	if len(messageDescriptions) != 1 {
		t.Fatalf("expected one streamed response send, got %d", len(messageDescriptions))
	}

	if !containsFold(messageDescriptions[0], thoughtText) {
		t.Fatalf("expected streaming response to include thinking: %q", messageDescriptions[0])
	}

	if len(patchDescriptions) != 1 {
		t.Fatalf("expected one final answer edit, got %d", len(patchDescriptions))
	}

	if containsFold(patchDescriptions[0], thoughtText) {
		t.Fatalf("expected final response to remove thinking: %q", patchDescriptions[len(patchDescriptions)-1])
	}

	if !containsFold(patchDescriptions[0], answerText) {
		t.Fatalf("expected final response to include answer: %q", patchDescriptions[0])
	}

	if len(tracker.pendingResponses) != 1 {
		t.Fatalf("unexpected pending response count: %d", len(tracker.pendingResponses))
	}

	expectedStoredText := visibleResponseText(thoughtText, answerText)
	if tracker.pendingResponses[0].node.text != expectedStoredText {
		t.Fatalf("unexpected stored assistant text: %q", tracker.pendingResponses[0].node.text)
	}
}

func TestGenerateAndSendResponseDoesNotStreamBridgeSourceAppendix(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		answerText         = "Answer paragraph."
		sourceURL          = "https://example.com/source"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	messageDescriptions := make([]string, 0, 2)
	patchDescriptions := make([]string, 0, 2)
	messageSendCount := 0
	instance := newBridgeSourceAppendixStreamingTestBot(
		t,
		channelID,
		botUserID,
		assistantMessage,
		&messageDescriptions,
		&patchDescriptions,
		&messageSendCount,
		answerText,
		sourceURL,
	)

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providers.ProviderAPIKindOpenAI,
			BaseURL:         "http://127.0.0.1:8787/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "deepseek-chat",
		ConfiguredModel: "openai/deepseek-chat",
		SessionID:       "",
		RequestID:       "",
		Messages:        nil,
	}
	tracker := newResponseTracker(sourceMessage, "")

	err := instance.generateAndSendResponse(
		context.Background(),
		config{},
		request,
		tracker,
		nil,
	)
	if err != nil {
		t.Fatalf("generate and send response: %v", err)
	}

	if messageSendCount != 1 {
		t.Fatalf("unexpected streamed message send count: %d", messageSendCount)
	}

	if len(messageDescriptions) != 1 {
		t.Fatalf("unexpected streamed message descriptions: %#v", messageDescriptions)
	}

	if !containsFold(messageDescriptions[0], answerText) {
		t.Fatalf("unexpected streamed response body: %q", messageDescriptions[0])
	}

	if containsFold(messageDescriptions[0], sourceURL) {
		t.Fatalf("expected streamed response to omit source appendix: %q", messageDescriptions[0])
	}

	if len(patchDescriptions) != 1 {
		t.Fatalf("unexpected final patch descriptions: %#v", patchDescriptions)
	}

	if patchDescriptions[0] != answerText {
		t.Fatalf("unexpected final response body: %q", patchDescriptions[0])
	}

	if containsFold(patchDescriptions[0], sourceURL) {
		t.Fatalf("expected final response to omit source appendix: %q", patchDescriptions[0])
	}

	assertRenderedDescriptionsHideSources(
		t,
		sourceURL,
		append(messageDescriptions, patchDescriptions...),
	)
	assertStoredBridgeSourceAppendixResponse(t, tracker, answerText, sourceURL)
}

func TestGenerateAndSendResponseShowsSourcesButtonForBridgeSources(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		answerText         = "Answer text."
		sourceURL          = "https://example.com/source"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	messageDescriptions := make([]string, 0, 2)
	patchDescriptions := make([]string, 0, 2)
	messageSendCount := 0
	instance := newBridgeSourceAppendixStreamingTestBot(
		t,
		channelID,
		botUserID,
		assistantMessage,
		&messageDescriptions,
		&patchDescriptions,
		&messageSendCount,
		answerText,
		sourceURL,
	)

	request := chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providers.ProviderAPIKindOpenAI,
			BaseURL:         "http://127.0.0.1:8787/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "deepseek-chat",
		ConfiguredModel: "openai/deepseek-chat",
		SessionID:       "",
		RequestID:       "",
		Messages:        nil,
	}
	tracker := newResponseTracker(sourceMessage, "")

	err := instance.generateAndSendResponse(
		context.Background(),
		config{},
		request,
		tracker,
		nil,
	)
	if err != nil {
		t.Fatalf("generate and send response: %v", err)
	}

	if tracker.searchMetadata == nil {
		t.Fatal("expected search metadata to be set on tracker")
	}

	totalSources := countSearchSources(tracker.searchMetadata)
	if totalSources != 1 {
		t.Fatalf("expected 1 parsed source, got %d", totalSources)
	}

	if len(tracker.renderedSpecs) == 0 ||
		!tracker.renderedSpecs[len(tracker.renderedSpecs)-1].actions.showSources {
		t.Fatalf("expected Show Sources action on final rendered spec: %#v", tracker.renderedSpecs)
	}
}

func TestGenerateAndSendResponseTreatsAppendixOnlyStreamAsEmptyResponse(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		sourceURL          = "https://example.com/source"
		expectedError      = "The model returned an empty response. Try again."
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	messageDescriptions := make([]string, 0, 1)
	patchDescriptions := make([]string, 0, 1)
	messageSendCount := 0
	session := newPartialFailureResponseSession(
		t,
		channelID,
		botUserID,
		assistantMessage,
		&messageDescriptions,
		&patchDescriptions,
		&messageSendCount,
	)

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		appendixDelta := "\n\nSources\n1. [Example Source](" + sourceURL +
			") (example.com/source) via `latest ai news`\n\n" +
			"Search Queries\n1. `latest ai news`\n"
		if err := handle(newStreamDelta(appendixDelta, "")); err != nil {
			return err
		}

		return handle(newStreamDelta("", finishReasonStop))
	})

	tracker := newResponseTracker(sourceMessage, "test-model")
	tracker.responseMessages = append(tracker.responseMessages, assistantMessage)
	tracker.progressActive = true

	err := instance.generateAndSendResponse(
		context.Background(),
		config{},
		chatCompletionRequest{},
		tracker,
		nil,
	)
	if err == nil {
		t.Fatal("expected error for appendix-only model response")
	}

	if !errors.Is(err, errEmptyModelResponse) {
		t.Fatalf("unexpected error: %v, expected errEmptyModelResponse", err)
	}

	if messageSendCount != 0 {
		t.Fatalf("expected no streamed message send for appendix-only response, got %d", messageSendCount)
	}

	if len(patchDescriptions) == 0 {
		t.Fatal("expected failure embed edit on progress message")
	}

	if !containsFold(patchDescriptions[len(patchDescriptions)-1], expectedError) {
		t.Fatalf("unexpected patch description: %q", patchDescriptions[len(patchDescriptions)-1])
	}

	if containsFold(patchDescriptions[len(patchDescriptions)-1], sourceURL) {
		t.Fatalf("expected failure embed to omit source appendix: %q", patchDescriptions[len(patchDescriptions)-1])
	}
}

func newBridgeSourceAppendixStreamingTestBot(
	t *testing.T,
	channelID string,
	botUserID string,
	assistantMessage *discordgo.Message,
	messageDescriptions *[]string,
	patchDescriptions *[]string,
	messageSendCount *int,
	answerText string,
	sourceURL string,
) *bot {
	t.Helper()

	session := newPartialFailureResponseSession(
		t,
		channelID,
		botUserID,
		assistantMessage,
		messageDescriptions,
		patchDescriptions,
		messageSendCount,
	)

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = fakeChatCompletionClient{
		deltas: []streamDelta{
			newStreamDelta(answerText, ""),
			newStreamDelta("\n", ""),
			newStreamDelta("\n### ", ""),
			newStreamDelta("Sources:\n1. [Example Source]("+sourceURL+")", ""),
			newStreamDelta(
				" (example.com/source) via `latest ai news`\n\nSearch Queries\n1. `latest ai news`\n",
				"",
			),
			newStreamDelta("", finishReasonStop),
		},
	}

	return instance
}

func assertRenderedDescriptionsHideSources(
	t *testing.T,
	sourceURL string,
	descriptions []string,
) {
	t.Helper()

	for _, description := range descriptions {
		if containsFold(description, "Sources") || containsFold(description, sourceURL) {
			t.Fatalf("expected sources to stay hidden during response rendering: %q", description)
		}
	}
}

func assertStoredBridgeSourceAppendixResponse(
	t *testing.T,
	tracker *responseTracker,
	answerText string,
	sourceURL string,
) {
	t.Helper()

	if len(tracker.pendingResponses) != 1 {
		t.Fatalf("unexpected pending response count: %d", len(tracker.pendingResponses))
	}

	storedNode := tracker.pendingResponses[0].node
	if storedNode.text != answerText {
		t.Fatalf("unexpected stored assistant text: %q", storedNode.text)
	}

	if storedNode.searchMetadata == nil || len(storedNode.searchMetadata.Results) != 1 {
		t.Fatalf("expected parsed source metadata on stored node: %#v", storedNode.searchMetadata)
	}

	storedSources := extractSearchSources(storedNode.searchMetadata.Results[0].Text)
	if len(storedSources) != 1 || storedSources[0].URL != sourceURL {
		t.Fatalf("unexpected stored source metadata: %#v", storedSources)
	}
}

func TestGenerateAndSendResponsePersistsThinkingInConversationHistory(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		thoughtText        = "Plan first."
		answerText         = "Final answer."
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantMessageID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.MessageReference = sourceMessage.Reference()
	assistantMessage.Type = discordgo.MessageTypeReply

	session := newResponseHistoryTestSession(t, channelID, botUserID, assistantMessage)
	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = fakeChatCompletionClient{
		deltas: thinkingAnswerResponseDeltas(thoughtText, answerText),
	}

	tracker := newResponseTracker(sourceMessage, "")

	err := instance.generateAndSendResponse(
		context.Background(),
		config{},
		chatCompletionRequest{},
		tracker,
		nil,
	)
	if err != nil {
		t.Fatalf("generate and send response: %v", err)
	}

	followUpMessage := newFollowUpReplyMessage("user-message-2", channelID, userID, assistantMessage)

	var contentOptions messageContentOptions

	conversation, warnings := instance.buildConversation(
		context.Background(),
		followUpMessage,
		contentOptions,
		defaultMaxMessages,
		false,
		false,
	)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	assertConversationHistory(
		t,
		conversation,
		visibleResponseText(thoughtText, answerText),
	)
}

func newResponseHistoryTestSession(
	t *testing.T,
	channelID string,
	botUserID string,
	sentMessage *discordgo.Message,
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

		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/typing":
			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages":
			return newJSONResponse(t, request, sentMessage), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/"+sentMessage.ID:
			return newJSONResponse(t, request, sentMessage), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	})
	session.Client = client

	return session
}

func newJSONResponse(t *testing.T, request *http.Request, payload any) *http.Response {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal response payload: %v", err)
	}

	response := new(http.Response)
	response.Status = httpStatusOKText
	response.StatusCode = http.StatusOK
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header = make(http.Header)
	response.Request = request

	return response
}

func assertRequestEmbed(
	t *testing.T,
	request *http.Request,
	expectedModelName string,
	expectedDescription string,
	expectedFooter string,
) {
	t.Helper()

	var payload map[string]any

	err := json.NewDecoder(request.Body).Decode(&payload)
	if err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	assertEmbedPayload(t, payload, expectedModelName, expectedDescription, expectedFooter)
}

func assertEmbedPayload(
	t *testing.T,
	payload map[string]any,
	expectedModelName string,
	expectedDescription string,
	expectedFooter string,
) {
	t.Helper()

	embeds, embedsOK := payload["embeds"].([]any)
	if !embedsOK || len(embeds) != 1 {
		t.Fatalf("unexpected embeds payload: %#v", payload["embeds"])
	}

	embed, embedOK := embeds[0].(map[string]any)
	if !embedOK {
		t.Fatalf("unexpected embed payload: %#v", embeds[0])
	}

	author, authorOK := embed["author"].(map[string]any)
	if !authorOK {
		t.Fatalf("unexpected embed author payload: %#v", embed["author"])
	}

	if author["name"] != expectedModelName {
		t.Fatalf("unexpected embed author name: %#v", author["name"])
	}

	if embed["description"] != expectedDescription {
		t.Fatalf("unexpected embed description: %#v", embed["description"])
	}

	footerValue, footerSet := embed["footer"]
	if expectedFooter == "" {
		if footerSet {
			t.Fatalf("unexpected embed footer payload: %#v", footerValue)
		}

		return
	}

	footer, footerOK := footerValue.(map[string]any)
	if !footerOK {
		t.Fatalf("unexpected embed footer payload: %#v", footerValue)
	}

	if footer["text"] != expectedFooter {
		t.Fatalf("unexpected embed footer text: %#v", footer["text"])
	}
}

func assertPlainReplyPayload(
	t *testing.T,
	payload map[string]any,
	expectedContent string,
	expectedReferenceMessageID string,
) {
	t.Helper()

	if payload["content"] != expectedContent {
		t.Fatalf("unexpected message content: %#v", payload["content"])
	}

	if flags, ok := payload["flags"].(float64); !ok ||
		discordgo.MessageFlags(int(flags)) != discordgo.MessageFlagsSuppressNotifications {
		t.Fatalf("unexpected flags payload: %#v", payload["flags"])
	}

	if embedsValue, embedsSet := payload["embeds"]; embedsSet && embedsValue != nil {
		embeds, embedsOK := embedsValue.([]any)
		if !embedsOK || len(embeds) != 0 {
			t.Fatalf("unexpected embeds payload: %#v", payload["embeds"])
		}
	}

	if componentsValue, componentsSet := payload["components"]; componentsSet && componentsValue != nil {
		components, componentsOK := componentsValue.([]any)
		if !componentsOK || len(components) != 0 {
			t.Fatalf("unexpected components payload: %#v", payload["components"])
		}
	}

	reference, referenceOK := payload["message_reference"].(map[string]any)
	if !referenceOK {
		t.Fatalf("unexpected message reference payload: %#v", payload["message_reference"])
	}

	if reference["message_id"] != expectedReferenceMessageID {
		t.Fatalf("unexpected reply target: %#v", reference["message_id"])
	}

	allowedMentions, allowedMentionsOK := payload["allowed_mentions"].(map[string]any)
	if !allowedMentionsOK {
		t.Fatalf("unexpected allowed mentions payload: %#v", payload["allowed_mentions"])
	}

	if repliedUser, ok := allowedMentions["replied_user"].(bool); !ok || repliedUser {
		t.Fatalf("unexpected replied_user value: %#v", allowedMentions["replied_user"])
	}
}

func newResponseHistoryTestBot(session *discordgo.Session, assistantReplyText string) *bot {
	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = fakeChatCompletionClient{
		deltas: []streamDelta{
			newStreamDelta(assistantReplyText, ""),
			newStreamDelta("", finishReasonStop),
		},
	}

	return instance
}

func newPromptMessage(
	messageID, channelID, userID, botUserID string,
) *discordgo.Message {
	message := new(discordgo.Message)
	message.ID = messageID
	message.ChannelID = channelID
	message.Author = newDiscordUser(userID, false)
	message.Content = "<@" + botUserID + "> generate a random 10-digit number"
	message.Mentions = []*discordgo.User{newDiscordUser(botUserID, false)}

	return message
}

func newFollowUpReplyMessage(
	messageID, channelID, userID string,
	assistantMessage *discordgo.Message,
) *discordgo.Message {
	message := new(discordgo.Message)
	message.ID = messageID
	message.ChannelID = channelID
	message.Author = newDiscordUser(userID, false)
	message.Content = "repeat the 10-digit number that you just generated"
	message.MessageReference = assistantMessage.Reference()
	message.ReferencedMessage = assistantMessage

	return message
}

func newDiscordUser(userID string, bot bool) *discordgo.User {
	user := new(discordgo.User)
	user.ID = userID
	user.Bot = bot

	return user
}

func newStreamDelta(content, finishReason string) streamDelta {
	var delta streamDelta

	delta.Thinking = ""
	delta.Content = content
	delta.FinishReason = finishReason

	return delta
}

func thinkingAnswerResponseDeltas(thinkingText, answerText string) []streamDelta {
	return []streamDelta{
		{
			Thinking:           thinkingText,
			Content:            "",
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata:     nil,
		},
		{
			Thinking:           "",
			Content:            answerText,
			FinishReason:       "",
			ProviderResponseID: "",
			SearchMetadata:     nil,
		},
		{
			Thinking:           "",
			Content:            "",
			FinishReason:       finishReasonStop,
			ProviderResponseID: "",
			SearchMetadata:     nil,
		},
	}
}

func assertConversationHistory(
	t *testing.T,
	conversation []chatMessage,
	assistantReplyText string,
) {
	t.Helper()

	if len(conversation) != 3 {
		t.Fatalf("unexpected conversation length: %#v", conversation)
	}

	if conversation[0].Role != messageRoleUser ||
		conversation[0].Content != "generate a random 10-digit number" {
		t.Fatalf("unexpected source message: %#v", conversation[0])
	}

	if conversation[1].Role != messageRoleAssistant ||
		conversation[1].Content != assistantReplyText {
		t.Fatalf("unexpected assistant message: %#v", conversation[1])
	}

	if conversation[2].Role != messageRoleUser ||
		conversation[2].Content != "repeat the 10-digit number that you just generated" {
		t.Fatalf("unexpected follow-up message: %#v", conversation[2])
	}
}

func TestGenerateAndSendResponseRendersFailureOnEmptyModelResponse(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		expectedError      = "The model returned an empty response. Try again."
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	messageDescriptions := make([]string, 0, 1)
	patchDescriptions := make([]string, 0, 1)
	messageSendCount := 0
	session := newPartialFailureResponseSession(
		t,
		channelID,
		botUserID,
		assistantMessage,
		&messageDescriptions,
		&patchDescriptions,
		&messageSendCount,
	)

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		return nil
	})

	tracker := newResponseTracker(sourceMessage, "test-model")
	tracker.responseMessages = append(tracker.responseMessages, assistantMessage)
	tracker.progressActive = true

	err := instance.generateAndSendResponse(
		context.Background(),
		config{},
		chatCompletionRequest{},
		tracker,
		nil,
	)
	if err == nil {
		t.Fatal("expected error for empty model response")
	}

	if !errors.Is(err, errEmptyModelResponse) {
		t.Fatalf("unexpected error: %v, expected errEmptyModelResponse", err)
	}

	if len(patchDescriptions) == 0 {
		t.Fatal("expected failure embed edit on progress message")
	}

	if !containsFold(patchDescriptions[len(patchDescriptions)-1], expectedError) {
		t.Fatalf("unexpected patch description: %q", patchDescriptions[len(patchDescriptions)-1])
	}
}

func TestUserFacingResponseError_EmptyModelResponse(t *testing.T) {
	t.Parallel()

	expected := "The model returned an empty response. Try again."
	errText := userFacingResponseError(errEmptyModelResponse)

	if errText != expected {
		t.Fatalf("userFacingResponseError(errEmptyModelResponse) = %q, expected %q", errText, expected)
	}
}

func newTestDiscordMessage(messageID string) *discordgo.Message {
	message := new(discordgo.Message)
	message.ID = messageID
	message.ChannelID = "channel-1"

	return message
}

const testGeneratedImageURL = "https://assets.example.com/generated/image.jpg"

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

const testProviderResponseID = "resp_123"

func TestGenerateAndSendResponseFallsBackToStableModelOnFailure(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		fallbackReplyText  = "Fallback reply from 9router stable model"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	messageDescriptions := make([]string, 0, 2)
	messageAuthors := make([]string, 0, 2)
	messageWarnings := make([][]string, 0, 2)
	session := newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		if (request.Method == http.MethodPost && request.URL.Path == "/api/v9/channels/"+channelID+"/messages") ||
			(request.Method == http.MethodPatch && strings.HasPrefix(request.URL.Path, "/api/v9/channels/"+channelID+"/messages/")) {
			body, _ := io.ReadAll(request.Body)

			var payload struct {
				Embeds []struct {
					Description string `json:"description"`
					Author      struct {
						Name string `json:"name"`
					} `json:"author"`
					Fields []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"fields"`
				} `json:"embeds"`
			}

			_ = json.Unmarshal(body, &payload)
			if len(payload.Embeds) > 0 {
				messageDescriptions = append(messageDescriptions, payload.Embeds[0].Description)
				messageAuthors = append(messageAuthors, payload.Embeds[0].Author.Name)

				fieldNames := make([]string, 0, len(payload.Embeds[0].Fields))
				for _, field := range payload.Embeds[0].Fields {
					fieldNames = append(fieldNames, field.Name)
				}

				messageWarnings = append(messageWarnings, fieldNames)
			}

			return newJSONResponse(t, request, assistantMessage), nil
		}

		return newNoContentResponse(request), nil
	}))

	var attemptedModels []string

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		req chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		attemptedModels = append(attemptedModels, req.ConfiguredModel)
		if req.ConfiguredModel == "gemini-search/gemini-3.7-flash-medium:vision" {
			return errors.New("upstream primary model overloaded 503")
		}

		if req.ConfiguredModel == "9router/stable_model:vision" {
			_ = handle(newStreamDelta(fallbackReplyText, finishReasonStop))

			return nil
		}

		return errors.New("unknown model")
	})

	loadedConfig := config{
		Providers: map[string]providerConfig{
			"gemini-search": {
				Name: "gemini-search",
			},
			"9router": {
				Name:    "9router",
				BaseURL: "http://localhost:20128/v1",
			},
		},
		Models: map[string]map[string]any{
			"gemini-search/gemini-3.7-flash-medium:vision": nil,
			"9router/stable_model:vision":                  nil,
		},
		ModelOrder:    []string{"gemini-search/gemini-3.7-flash-medium:vision", "9router/stable_model:vision"},
		FallbackModel: "9router/stable_model:vision",
	}

	request := chatCompletionRequest{
		ConfiguredModel: "gemini-search/gemini-3.7-flash-medium:vision",
		Model:           "gemini-3.7-flash-medium",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: "Hello"},
		},
	}
	tracker := newResponseTracker(sourceMessage, request.ConfiguredModel)
	// The primary pipeline would have run the search-decider stage for these
	// non-disabled providers; mark it decided so the fallback path stays
	// hermetic instead of consulting an unconfigured decider.
	tracker.webSearchDecided = true

	err := instance.generateAndSendResponse(
		context.Background(),
		loadedConfig,
		request,
		tracker,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected generateAndSendResponse error: %v", err)
	}

	if len(attemptedModels) != 2 {
		t.Fatalf("expected 2 attempted models, got: %v", attemptedModels)
	}

	if attemptedModels[0] != "gemini-search/gemini-3.7-flash-medium:vision" ||
		attemptedModels[1] != "9router/stable_model:vision" {
		t.Fatalf("unexpected attempted models order: %v", attemptedModels)
	}

	if len(messageDescriptions) == 0 ||
		!strings.Contains(messageDescriptions[len(messageDescriptions)-1], fallbackReplyText) {
		t.Fatalf("unexpected message description: %v", messageDescriptions)
	}

	if len(messageAuthors) == 0 || messageAuthors[len(messageAuthors)-1] != "9router/stable_model:vision" {
		t.Fatalf("unexpected message author: %v", messageAuthors)
	}

	expectedWarning := "Warning: fallback to 9router/stable_model:vision"
	if len(messageWarnings) == 0 || !slicesContainsString(messageWarnings[len(messageWarnings)-1], expectedWarning) {
		t.Fatalf("expected fallback warning %q in final embed fields, got: %v", expectedWarning, messageWarnings)
	}
}

func TestGenerateAndSendResponseRendersFailureWhenFallbackModelAlsoFails(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	messageDescriptions := make([]string, 0, 2)
	messageAuthors := make([]string, 0, 2)
	session := newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		if (request.Method == http.MethodPost && request.URL.Path == "/api/v9/channels/"+channelID+"/messages") ||
			(request.Method == http.MethodPatch && strings.HasPrefix(request.URL.Path, "/api/v9/channels/"+channelID+"/messages/")) {
			body, _ := io.ReadAll(request.Body)

			var payload struct {
				Embeds []struct {
					Description string `json:"description"`
					Author      struct {
						Name string `json:"name"`
					} `json:"author"`
				} `json:"embeds"`
			}

			_ = json.Unmarshal(body, &payload)
			if len(payload.Embeds) > 0 {
				messageDescriptions = append(messageDescriptions, payload.Embeds[0].Description)
				messageAuthors = append(messageAuthors, payload.Embeds[0].Author.Name)
			}

			return newJSONResponse(t, request, assistantMessage), nil
		}

		return newNoContentResponse(request), nil
	}))

	var attemptedModels []string

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		req chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		attemptedModels = append(attemptedModels, req.ConfiguredModel)

		return errors.New("model failure")
	})

	loadedConfig := config{
		Providers: map[string]providerConfig{
			"gemini-search": {Name: "gemini-search"},
			"9router":       {Name: "9router", BaseURL: "http://localhost:20128/v1"},
		},
		Models: map[string]map[string]any{
			"gemini-search/gemini-3.7-flash-medium:vision": nil,
			"9router/stable_model:vision":                  nil,
		},
		ModelOrder:    []string{"gemini-search/gemini-3.7-flash-medium:vision", "9router/stable_model:vision"},
		FallbackModel: "9router/stable_model:vision",
	}

	request := chatCompletionRequest{
		ConfiguredModel: "gemini-search/gemini-3.7-flash-medium:vision",
		Model:           "gemini-3.7-flash-medium",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: "Hello"},
		},
	}
	tracker := newResponseTracker(sourceMessage, request.ConfiguredModel)
	// The primary pipeline would have run the search-decider stage for these
	// non-disabled providers; mark it decided so the fallback path stays
	// hermetic instead of consulting an unconfigured decider.
	tracker.webSearchDecided = true

	err := instance.generateAndSendResponse(
		context.Background(),
		loadedConfig,
		request,
		tracker,
		nil,
	)
	if err == nil {
		t.Fatal("expected error when both primary and fallback fail")
	}

	if len(attemptedModels) != 2 {
		t.Fatalf("expected 2 attempted models, got: %v", attemptedModels)
	}

	if len(messageDescriptions) == 0 ||
		!strings.Contains(messageDescriptions[len(messageDescriptions)-1], "model failure") {
		t.Fatalf("unexpected failure message description: %v", messageDescriptions)
	}
}

func TestGenerateAndSendResponseDoesNotFallbackWhenPrimaryIsAlreadyFallbackModel(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := newAssistantReplyMessage(
		assistantMessageID,
		newDiscordUser(botUserID, true),
		sourceMessage,
	)

	session := newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		if (request.Method == http.MethodPost && request.URL.Path == "/api/v9/channels/"+channelID+"/messages") ||
			(request.Method == http.MethodPatch && strings.HasPrefix(request.URL.Path, "/api/v9/channels/"+channelID+"/messages/")) {
			return newJSONResponse(t, request, assistantMessage), nil
		}

		return newNoContentResponse(request), nil
	}))

	var attemptedCount int

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		attemptedCount++

		return errors.New("stable model failed")
	})

	loadedConfig := config{
		Providers: map[string]providerConfig{
			"9router": {Name: "9router", BaseURL: "http://localhost:20128/v1"},
		},
		Models: map[string]map[string]any{
			"9router/stable_model:vision": nil,
		},
		ModelOrder:    []string{"9router/stable_model:vision"},
		FallbackModel: "9router/stable_model:vision",
	}

	request := chatCompletionRequest{
		ConfiguredModel: "9router/stable_model:vision",
		Model:           "stable_model:vision",
		Messages: []chatMessage{
			{Role: messageRoleUser, Content: "Hello"},
		},
	}
	tracker := newResponseTracker(sourceMessage, request.ConfiguredModel)
	// The primary pipeline would have run the search-decider stage for these
	// non-disabled providers; mark it decided so the fallback path stays
	// hermetic instead of consulting an unconfigured decider.
	tracker.webSearchDecided = true

	err := instance.generateAndSendResponse(
		context.Background(),
		loadedConfig,
		request,
		tracker,
		nil,
	)
	if err == nil {
		t.Fatal("expected error when stable model fails")
	}

	if attemptedCount != 1 {
		t.Fatalf("expected exactly 1 attempt without fallback recursion, got %d", attemptedCount)
	}
}

func TestFallbackModelWarning(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		fallbackModel string
		want          string
	}{
		{
			name:          "empty fallback model",
			fallbackModel: "",
			want:          "",
		},
		{
			name:          "whitespace only fallback model",
			fallbackModel: "   ",
			want:          "",
		},
		{
			name:          "stable model fallback",
			fallbackModel: "9router/stable_model:vision",
			want:          "Warning: fallback to 9router/stable_model:vision",
		},
		{
			name:          "trimmed model fallback",
			fallbackModel: "  custom/model:vision  ",
			want:          "Warning: fallback to custom/model:vision",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := fallbackModelWarning(tc.fallbackModel)
			if got != tc.want {
				t.Fatalf("fallbackModelWarning(%q) = %q, want %q", tc.fallbackModel, got, tc.want)
			}
		})
	}
}

func TestAppendFallbackWarning(t *testing.T) {
	t.Parallel()

	existing := []string{"Warning: web search unavailable", "Warning: unsupported attachments"}
	result := appendFallbackWarning(existing, "9router/stable_model:vision")

	expected := []string{
		"Warning: fallback to 9router/stable_model:vision",
		"Warning: unsupported attachments",
		"Warning: web search unavailable",
	}

	if len(result) != len(expected) {
		t.Fatalf("unexpected warning count: got %d want %d (%v)", len(result), len(expected), result)
	}

	for index, exp := range expected {
		if result[index] != exp {
			t.Fatalf("result[%d] = %q, want %q", index, result[index], exp)
		}
	}

	// Verify idempotency / deduplication
	dedupResult := appendFallbackWarning(result, "9router/stable_model:vision")
	if len(dedupResult) != len(expected) {
		t.Fatalf("expected duplicate warning to be deduplicated: got %v", dedupResult)
	}
}

func newFallbackSearchDeciderTestConfig() config {
	loadedConfig := new(config)
	loadedConfig.Providers = map[string]providerConfig{
		"openai":  {Name: "openai", BaseURL: "https://api.example.com/v1"},
		"9router": {Name: "9router", BaseURL: "http://localhost:20128/v1"},
	}
	loadedConfig.Models = map[string]map[string]any{
		"openai/main-model":      nil,
		"9router/fallback-model": nil,
		"openai/decider-model":   nil,
	}
	loadedConfig.ModelOrder = []string{
		"openai/main-model",
		"9router/fallback-model",
		"openai/decider-model",
	}
	loadedConfig.FallbackModel = "9router/fallback-model"
	loadedConfig.SearchDeciderModel = "openai/decider-model"
	loadedConfig.WebSearch.MaxURLs = defaultWebSearchMaxURLs
	loadedConfig.MaxMessages = defaultMaxMessages

	return *loadedConfig
}

// TestRespondToMessageRunsSearchDeciderForFallbackWhenPrimaryDisabledIt
// asserts the fallback attempt gets its own web-search decision: when the
// primary provider skips the search-decider stage (disable_search_decider),
// a failed primary request must retry through the search decider with the
// fallback model and append the search results to the fallback conversation.
func TestRespondToMessageRunsSearchDeciderForFallbackWhenPrimaryDisabledIt(t *testing.T) {
	t.Parallel()

	const (
		botUserID = "bot-user"
		channelID = "channel-1"
		userID    = "user-1"
	)

	sourceMessage := newPromptMessage("user-message-1", channelID, userID, botUserID)

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
			response := new(discordgo.Message)
			response.ID = "response-message"
			response.ChannelID = channelID

			return newJSONResponse(t, request, response), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/response-message":
			return newJSONResponse(t, request, new(discordgo.Message)), nil
		default:
			return newNoContentResponse(request), nil
		}
	}))

	var deciderRequests int
	var fallbackRequestMessages []chatMessage

	openAI := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		switch request.ConfiguredModel {
		case "openai/decider-model":
			deciderRequests++

			return handle(newStreamDelta(`{"needs_search":true,"queries":["fallback query"]}`, ""))
		case "9router/fallback-model":
			fallbackRequestMessages = request.Messages

			return handle(newStreamDelta("fallback answer", finishReasonStop))
		default:
			return errors.New("primary model failure")
		}
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{{Query: queries[0], Text: "Fallback search context"}}, nil
	})

	instance := newSearchTestBot(openAI, webSearch)
	instance.session = session
	instance.currentSearchDeciderModel = "openai/decider-model"

	loadedConfig := newFallbackSearchDeciderTestConfig()
	primaryProvider := loadedConfig.Providers["openai"]
	primaryProvider.DisableSearchDecider = true
	loadedConfig.Providers["openai"] = primaryProvider

	err := instance.respondToMessage(
		context.Background(),
		loadedConfig,
		sourceMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("respond to message: %v", err)
	}

	if deciderRequests != 1 {
		t.Fatalf("expected exactly 1 search decider request for the fallback attempt, got %d", deciderRequests)
	}

	if len(webSearch.calls) != 1 {
		t.Fatalf("expected exactly 1 web search call for the fallback attempt, got %d", len(webSearch.calls))
	}

	var latestContent string

	for index := len(fallbackRequestMessages) - 1; index >= 0; index-- {
		if text, ok := fallbackRequestMessages[index].Content.(string); ok {
			latestContent = text

			break
		}
	}

	if !strings.Contains(latestContent, "Fallback search context") {
		t.Fatalf("expected search results in the fallback request, got: %q", latestContent)
	}
}

// TestRespondToMessageDoesNotReRunSearchDeciderForFallbackWhenPrimaryDecided
// asserts the fallback attempt does not duplicate the primary preparation's
// search-decider work: once the primary pipeline made the decision (here:
// needs_search=false), the fallback retries reuse that conversation without
// another decider call or web search.
func TestRespondToMessageDoesNotReRunSearchDeciderForFallbackWhenPrimaryDecided(t *testing.T) {
	t.Parallel()

	const (
		botUserID = "bot-user"
		channelID = "channel-1"
		userID    = "user-1"
	)

	sourceMessage := newPromptMessage("user-message-1", channelID, userID, botUserID)

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
			response := new(discordgo.Message)
			response.ID = "response-message"
			response.ChannelID = channelID

			return newJSONResponse(t, request, response), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/response-message":
			return newJSONResponse(t, request, new(discordgo.Message)), nil
		default:
			return newNoContentResponse(request), nil
		}
	}))

	var deciderRequests int
	var fallbackRequested bool

	openAI := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		switch request.ConfiguredModel {
		case "openai/decider-model":
			deciderRequests++

			return handle(newStreamDelta(`{"needs_search":false}`, ""))
		case "9router/fallback-model":
			fallbackRequested = true

			return handle(newStreamDelta("fallback answer", finishReasonStop))
		default:
			return errors.New("primary model failure")
		}
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		t.Error("unexpected web search call during fallback")

		return nil, nil
	})

	instance := newSearchTestBot(openAI, webSearch)
	instance.session = session
	instance.currentSearchDeciderModel = "openai/decider-model"

	err := instance.respondToMessage(
		context.Background(),
		newFallbackSearchDeciderTestConfig(),
		sourceMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("respond to message: %v", err)
	}

	if !fallbackRequested {
		t.Fatal("expected the fallback model to be attempted")
	}

	if deciderRequests != 1 {
		t.Fatalf(
			"expected only the primary preparation to consult the search decider, got %d requests",
			deciderRequests,
		)
	}
}

// TestBuildFallbackRequestSkipsWebSearchWhenFallbackProviderDisablesDecider
// asserts a per-provider disable_search_decider on the fallback provider also
// suppresses the fallback attempt's web-search decision.
func TestBuildFallbackRequestSkipsWebSearchWhenFallbackProviderDisablesDecider(t *testing.T) {
	t.Parallel()

	openAI := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		t.Fatal("unexpected search decider request for a disabled fallback provider")

		return nil
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		t.Fatal("unexpected web search call for a disabled fallback provider")

		return nil, nil
	})

	instance := newSearchTestBot(openAI, webSearch)

	sourceMessage := newPromptMessage("user-message-1", "channel-1", "user-1", "bot-user")
	sourceMessage.Author = newDiscordUser("user-1", false)

	tracker := newResponseTracker(sourceMessage, "openai/main-model")
	tracker.originalMessages = []chatMessage{
		{Role: messageRoleUser, Content: "<@bot-user>: latest ai news"},
	}

	loadedConfig := newFallbackSearchDeciderTestConfig()
	fallbackProvider := loadedConfig.Providers["9router"]
	fallbackProvider.DisableSearchDecider = true
	loadedConfig.Providers["9router"] = fallbackProvider

	fallbackRequest, warnings, err := instance.buildFallbackRequest(
		context.Background(),
		loadedConfig,
		"9router/fallback-model",
		chatCompletionRequest{ConfiguredModel: "openai/main-model"},
		tracker,
	)
	if err != nil {
		t.Fatalf("build fallback request: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if fallbackRequest.ConfiguredModel != "9router/fallback-model" {
		t.Fatalf("unexpected fallback model: %q", fallbackRequest.ConfiguredModel)
	}
}

// TestBuildFallbackRequestPropagatesSearchDeciderFailureWarning asserts that
// a failed web-search decision during the fallback attempt surfaces as a
// warning instead of being silently dropped.
func TestBuildFallbackRequestPropagatesSearchDeciderFailureWarning(t *testing.T) {
	t.Parallel()

	openAI := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		if request.ConfiguredModel == "openai/decider-model" {
			return errors.New("decider unavailable")
		}

		return nil
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		t.Fatal("unexpected web search call after decider failure")

		return nil, nil
	})

	instance := newSearchTestBot(openAI, webSearch)

	sourceMessage := newPromptMessage("user-message-1", "channel-1", "user-1", "bot-user")
	setCachedUserNode(instance, sourceMessage, nil, "<@bot-user>: latest ai news")

	tracker := newResponseTracker(sourceMessage, "openai/main-model")
	tracker.originalMessages = []chatMessage{
		{Role: messageRoleUser, Content: "<@bot-user>: latest ai news"},
	}

	fallbackRequest, warnings, err := instance.buildFallbackRequest(
		context.Background(),
		newFallbackSearchDeciderTestConfig(),
		"9router/fallback-model",
		chatCompletionRequest{ConfiguredModel: "openai/main-model"},
		tracker,
	)
	if err != nil {
		t.Fatalf("build fallback request: %v", err)
	}

	if len(warnings) != 1 || warnings[0] != searchWarningText {
		t.Fatalf("expected %q warning, got %#v", searchWarningText, warnings)
	}

	if fallbackRequest.ConfiguredModel != "9router/fallback-model" {
		t.Fatalf("unexpected fallback model: %q", fallbackRequest.ConfiguredModel)
	}
}
