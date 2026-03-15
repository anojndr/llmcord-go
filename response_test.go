package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"
)

type fakeChatCompletionClient struct {
	deltas []streamDelta
}

func (client fakeChatCompletionClient) streamChatCompletion(
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

func TestSegmentAccumulatorSplitsByRunes(t *testing.T) {
	t.Parallel()

	accumulator := newSegmentAccumulator(4)
	splitOccurred := accumulator.appendText("abecd")

	if !splitOccurred {
		t.Fatal("expected content to split across segments")
	}

	segments := accumulator.renderSegments(true)
	if len(segments) != 2 {
		t.Fatalf("unexpected segment count: %#v", segments)
	}

	if segments[0] != "abec" || segments[1] != "d" {
		t.Fatalf("unexpected segments: %#v", segments)
	}
}

func TestBuildRenderSpecsMarksSettledAndStreamingSegments(t *testing.T) {
	t.Parallel()

	specs := buildRenderSpecs([]string{"first", "second"}, "length", false, false)
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

	finalSpecs := buildRenderSpecs([]string{"only"}, "stop", true, false)
	if len(finalSpecs) != 1 {
		t.Fatalf("unexpected final spec count: %#v", finalSpecs)
	}

	if finalSpecs[0].content != "only" || finalSpecs[0].color != embedColorComplete {
		t.Fatalf("unexpected final spec: %#v", finalSpecs[0])
	}
}

func TestBuildRenderSpecsAddsSourcesButtonOnlyToFinalSearchedSegment(t *testing.T) {
	t.Parallel()

	specs := buildRenderSpecs([]string{"first", "second"}, "stop", true, true)
	if len(specs) != 2 {
		t.Fatalf("unexpected spec count: %#v", specs)
	}

	if specs[0].actions.showSources || specs[0].actions.showRentry {
		t.Fatalf("expected no action buttons on first segment: %#v", specs[0])
	}

	if !specs[1].actions.showSources || !specs[1].actions.showRentry {
		t.Fatalf("expected sources and Rentry buttons on final segment: %#v", specs[1])
	}
}

func TestBuildRenderSpecsAddsRentryButtonToFinalNonSearchedSegment(t *testing.T) {
	t.Parallel()

	specs := buildRenderSpecs([]string{"only"}, "stop", true, false)
	if len(specs) != 1 {
		t.Fatalf("unexpected spec count: %#v", specs)
	}

	if specs[0].actions.showSources {
		t.Fatalf("expected no sources button on non-searched response: %#v", specs[0])
	}

	if !specs[0].actions.showRentry {
		t.Fatalf("expected Rentry button on final non-searched response: %#v", specs[0])
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

func TestBuildPlainComponentsAddsActionButtons(t *testing.T) {
	t.Parallel()

	components := buildPlainComponents("hello", responseActions{
		showSources: true,
		showRentry:  true,
	})
	if len(components) != 2 {
		t.Fatalf("unexpected component count: %#v", components)
	}

	textDisplay, textDisplayOK := components[0].(*discordgo.TextDisplay)
	if !textDisplayOK {
		t.Fatalf("expected text display component, got %T", components[0])
	}

	if textDisplay.Content != "hello" {
		t.Fatalf("unexpected text display content: %#v", textDisplay)
	}

	row, rowOK := components[1].(*discordgo.ActionsRow)
	if !rowOK {
		t.Fatalf("expected action row component, got %T", components[1])
	}

	if len(row.Components) != 2 {
		t.Fatalf("unexpected action row button count: %#v", row.Components)
	}

	firstButton, firstButtonOK := row.Components[0].(*discordgo.Button)
	if !firstButtonOK {
		t.Fatalf("expected first row button, got %T", row.Components[0])
	}

	if firstButton.CustomID != showSourcesButtonCustomID {
		t.Fatalf("unexpected first button custom id: %q", firstButton.CustomID)
	}

	secondButton, secondButtonOK := row.Components[1].(*discordgo.Button)
	if !secondButtonOK {
		t.Fatalf("expected second row button, got %T", row.Components[1])
	}

	if secondButton.CustomID != viewOnRentryButtonCustomID {
		t.Fatalf("unexpected second button custom id: %q", secondButton.CustomID)
	}
}

func TestBuildResponseEmbedSetsConfiguredModelAsAuthor(t *testing.T) {
	t.Parallel()

	embed := buildResponseEmbed("hello", "openai/gpt-5.1", embedColorComplete, nil)
	if embed.Author == nil {
		t.Fatal("expected embed author to be set")
	}

	if embed.Author.Name != "openai/gpt-5.1" {
		t.Fatalf("unexpected embed author: %#v", embed.Author)
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

		assertRequestEmbedAuthor(t, request, modelName, responseBody)

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
	)
	if err != nil {
		t.Fatalf("render embed response: %v", err)
	}
}

func TestSendPlainResponseEditsExistingProgressMessage(t *testing.T) {
	t.Parallel()

	const (
		channelID        = "channel-1"
		sourceID         = "source-message"
		progressID       = "progress-message"
		plainContent     = "hello from plain response"
		expectedPatchURL = "/api/v9/channels/" + channelID + "/messages/" + progressID
	)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceID
	sourceMessage.ChannelID = channelID

	progressMessage := new(discordgo.Message)
	progressMessage.ID = progressID
	progressMessage.ChannelID = channelID

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages" {
			t.Fatalf("unexpected additional message send: %s %s", request.Method, request.URL.Path)
		}

		if request.Method != http.MethodPatch || request.URL.Path != expectedPatchURL {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
		}

		assertPlainEditRequest(t, request, plainContent)

		return newJSONResponse(t, request, progressMessage), nil
	})
	session.Client = client

	instance := new(bot)
	instance.session = session

	tracker := newResponseTracker(sourceMessage, "")
	tracker.responseMessages = []*discordgo.Message{progressMessage}
	tracker.progressActive = true

	err = instance.sendPlainResponse(
		context.Background(),
		tracker,
		[]string{plainContent},
	)
	if err != nil {
		t.Fatalf("send plain response: %v", err)
	}

	if tracker.progressActive {
		t.Fatal("expected progress placeholder to be cleared after plain response edit")
	}
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
		expectedError      = "Couldn't generate a response right now. Try again."
	)

	sourceMessage := newPromptMessage(sourceMessageID, channelID, userID, botUserID)
	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantMessageID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.MessageReference = sourceMessage.Reference()
	assistantMessage.Type = discordgo.MessageTypeReply

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
		emptyChatCompletionRequest(),
		newResponseTracker(sourceMessage, ""),
		nil,
		false,
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

func emptyChatCompletionRequest() chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:      "",
			BaseURL:      "",
			APIKey:       "",
			APIKeys:      nil,
			ExtraHeaders: nil,
			ExtraQuery:   nil,
			ExtraBody:    nil,
		},
		Model:           "",
		ConfiguredModel: "",
		Messages:        nil,
	}
}

var errPartialStreamFailure = errors.New("partial stream failure")

var errUnexpectedTestRequest = errors.New("unexpected test request")

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
		request,
		tracker,
		nil,
		true,
	)
	if err != nil {
		t.Fatalf("generate and send response: %v", err)
	}

	followUpMessage := newFollowUpReplyMessage("user-message-2", channelID, userID, assistantMessage)

	var contentOptions messageContentOptions

	conversation, warnings := instance.buildConversation(
		context.Background(),
		followUpMessage,
		defaultMaxText,
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
		userID,
		assistantReplyText,
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
	response.Status = "200 OK"
	response.StatusCode = http.StatusOK
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header = make(http.Header)
	response.Request = request

	return response
}

func assertRequestEmbedAuthor(
	t *testing.T,
	request *http.Request,
	expectedModelName string,
	expectedDescription string,
) {
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
}

func assertPlainEditRequest(
	t *testing.T,
	request *http.Request,
	expectedContent string,
) {
	t.Helper()

	var payload map[string]any

	err := json.NewDecoder(request.Body).Decode(&payload)
	if err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	if flags, ok := payload["flags"].(float64); !ok ||
		discordgo.MessageFlags(int(flags)) !=
			(discordgo.MessageFlagsIsComponentsV2|
				discordgo.MessageFlagsSuppressNotifications) {
		t.Fatalf("unexpected flags payload: %#v", payload["flags"])
	}

	embeds, embedsOK := payload["embeds"].([]any)
	if !embedsOK || len(embeds) != 0 {
		t.Fatalf("unexpected embeds payload: %#v", payload["embeds"])
	}

	components, componentsOK := payload["components"].([]any)
	if !componentsOK || len(components) != 2 {
		t.Fatalf("unexpected components payload: %#v", payload["components"])
	}

	textDisplay, textDisplayOK := components[0].(map[string]any)
	if !textDisplayOK {
		t.Fatalf("unexpected text display payload: %#v", components[0])
	}

	if textDisplay["content"] != expectedContent {
		t.Fatalf("unexpected text display content: %#v", textDisplay["content"])
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
	messageID string,
	channelID string,
	userID string,
	botUserID string,
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
	messageID string,
	channelID string,
	userID string,
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

func newStreamDelta(content string, finishReason string) streamDelta {
	var delta streamDelta

	delta.Content = content
	delta.FinishReason = finishReason

	return delta
}

func assertConversationHistory(
	t *testing.T,
	conversation []chatMessage,
	userID string,
	assistantReplyText string,
) {
	t.Helper()

	if len(conversation) != 3 {
		t.Fatalf("unexpected conversation length: %#v", conversation)
	}

	if conversation[0].Role != messageRoleUser ||
		conversation[0].Content != "<@"+userID+">: generate a random 10-digit number" {
		t.Fatalf("unexpected source message: %#v", conversation[0])
	}

	if conversation[1].Role != messageRoleAssistant ||
		conversation[1].Content != assistantReplyText {
		t.Fatalf("unexpected assistant message: %#v", conversation[1])
	}

	if conversation[2].Role != messageRoleUser ||
		conversation[2].Content != "<@"+userID+">: repeat the 10-digit number that you just generated" {
		t.Fatalf("unexpected follow-up message: %#v", conversation[2])
	}
}
