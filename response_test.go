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

	if specs[0].showSourcesButton {
		t.Fatalf("expected no sources button on first segment: %#v", specs[0])
	}

	if !specs[1].showSourcesButton {
		t.Fatalf("expected sources button on final segment: %#v", specs[1])
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

func TestBuildPlainComponentsAddsShowSourcesButton(t *testing.T) {
	t.Parallel()

	components := buildPlainComponents("hello", true)
	if len(components) != 1 {
		t.Fatalf("unexpected component count: %#v", components)
	}

	section, sectionOK := components[0].(*discordgo.Section)
	if !sectionOK {
		t.Fatalf("expected section component, got %T", components[0])
	}

	button, buttonOK := section.Accessory.(*discordgo.Button)
	if !buttonOK {
		t.Fatalf("expected button accessory, got %T", section.Accessory)
	}

	if button.CustomID != showSourcesButtonCustomID {
		t.Fatalf("unexpected button custom id: %q", button.CustomID)
	}
}

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
	assistantMessage := newAssistantResponseMessage(
		assistantMessageID,
		channelID,
		botUserID,
		sourceMessage.Reference(),
	)
	session := newResponseHistoryTestSession(t, channelID, botUserID, assistantMessage)
	instance := newResponseHistoryTestBot(session, assistantReplyText)

	var request chatCompletionRequest

	err := instance.generateAndSendResponse(
		context.Background(),
		request,
		sourceMessage,
		nil,
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

func newAssistantResponseMessage(
	messageID string,
	channelID string,
	botUserID string,
	reference *discordgo.MessageReference,
) *discordgo.Message {
	message := new(discordgo.Message)
	message.ID = messageID
	message.ChannelID = channelID
	message.Author = newDiscordUser(botUserID, true)
	message.MessageReference = reference
	message.Type = discordgo.MessageTypeReply

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
