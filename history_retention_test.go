package main

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestPersistAugmentedSourceMessageRetainsURLAndSearchSectionsInFollowUpHistory(t *testing.T) {
	t.Parallel()

	fixture := newHistoryRetentionFixture(t, "at ai what is in this image and these links")
	conversation := newRetainedSearchContextConversation(t)

	persistAugmentedSourceConversation(t, fixture.instance, fixture.sourceMessage, conversation)

	history := retainedHistoryForFollowUp(
		t,
		fixture.instance,
		fixture.followUpMessage,
		messageContentOptions{
			maxImages:                defaultMaxImages,
			allowAudio:               false,
			allowDocuments:           false,
			allowFiles:               false,
			allowedDocumentMIMETypes: nil,
			allowVideo:               false,
		},
	)

	assertRetainedSearchHistory(t, history)
}

func TestPersistAugmentedSourceMessageRetainsTikTokAndFacebookVideoContextInFollowUpHistory(t *testing.T) {
	t.Parallel()

	fixture := newHistoryRetentionFixture(t, "at ai summarize these videos")
	conversation := newRetainedVideoContextConversation(t)

	persistAugmentedSourceConversation(t, fixture.instance, fixture.sourceMessage, conversation)

	history := retainedHistoryForFollowUp(
		t,
		fixture.instance,
		fixture.followUpMessage,
		messageContentOptions{
			maxImages:                0,
			allowAudio:               false,
			allowDocuments:           false,
			allowFiles:               false,
			allowedDocumentMIMETypes: nil,
			allowVideo:               true,
		},
	)

	assertRetainedVideoHistory(t, history)
}

type historyRetentionFixture struct {
	instance        *bot
	userID          string
	sourceMessage   *discordgo.Message
	followUpMessage *discordgo.Message
}

func newHistoryRetentionFixture(t *testing.T, sourceContent string) historyRetentionFixture {
	t.Helper()

	const (
		botUserID          = "bot-user"
		userID             = "user-1"
		sourceMessageID    = "source-message"
		assistantMessageID = "assistant-message"
		followUpMessageID  = "follow-up-message"
	)

	instance := newHistoryRetentionTestBot(t)

	const channelID = "channel-1"

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = sourceContent

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
	setCachedUserNode(
		instance,
		followUpMessage,
		assistantMessage,
		"follow-up question",
	)

	return historyRetentionFixture{
		instance:        instance,
		userID:          userID,
		sourceMessage:   sourceMessage,
		followUpMessage: followUpMessage,
	}
}

func newRetainedSearchContextConversation(t *testing.T) []chatMessage {
	t.Helper()

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": "what is in this image and these links"},
				{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
			},
		},
	}

	var err error

	conversation, err = appendContextToConversation(conversation, func(prompt *augmentedUserPrompt) {
		prompt.RepliedMessage = "quoted context that should not be retained"
	})
	if err != nil {
		t.Fatalf("append reply target section: %v", err)
	}

	for _, step := range []struct {
		label string
		run   func([]chatMessage) ([]chatMessage, error)
	}{
		{
			label: "append visual search results",
			run: func(messages []chatMessage) ([]chatMessage, error) {
				return appendVisualSearchResultsToConversation(
					messages,
					"Image 1\nTop match: Sword Art Online",
				)
			},
		},
		{
			label: "append website content",
			run: func(messages []chatMessage) ([]chatMessage, error) {
				return appendWebsiteContentToConversation(
					messages,
					"URL: https://example.com\nTitle: Example",
				)
			},
		},
		{
			label: "append youtube content",
			run: func(messages []chatMessage) ([]chatMessage, error) {
				return appendYouTubeContentToConversation(
					messages,
					"URL: https://www.youtube.com/watch?v=dQw4w9WgXcQ\nTitle: Example",
				)
			},
		},
		{
			label: "append reddit content",
			run: func(messages []chatMessage) ([]chatMessage, error) {
				return appendRedditContentToConversation(
					messages,
					"Thread URL: https://www.reddit.com/r/testing/comments/abc123/thread-title/\nTitle: Example",
				)
			},
		},
		{
			label: "append web search results",
			run: func(messages []chatMessage) ([]chatMessage, error) {
				return appendWebSearchResultsToConversation(
					messages,
					"Query: example\nResults:\nExample result",
				)
			},
		},
	} {
		conversation, err = step.run(conversation)
		if err != nil {
			t.Fatalf("%s: %v", step.label, err)
		}
	}

	return conversation
}

func newRetainedVideoContextConversation(t *testing.T) []chatMessage {
	t.Helper()

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: "summarize these videos",
		},
	}

	var err error

	conversation, err = appendMediaAnalysesToConversation(
		conversation,
		[]string{"TikTok clip analysis", "Facebook clip analysis"},
	)
	if err != nil {
		t.Fatalf("append media analyses: %v", err)
	}

	conversation, err = appendMediaPartsToConversation(
		conversation,
		[]contentPart{
			{
				"type":               contentTypeVideoData,
				contentFieldBytes:    []byte("tiktok-video"),
				contentFieldMIMEType: "video/mp4",
				contentFieldFilename: "tiktok_123.mp4",
			},
			{
				"type":               contentTypeVideoData,
				contentFieldBytes:    []byte("facebook-video"),
				contentFieldMIMEType: "video/mp4",
				contentFieldFilename: "facebook_456.mp4",
			},
		},
	)
	if err != nil {
		t.Fatalf("append retained video parts: %v", err)
	}

	return conversation
}

func persistAugmentedSourceConversation(
	t *testing.T,
	instance *bot,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) {
	t.Helper()

	err := instance.persistAugmentedSourceMessage(
		context.Background(),
		sourceMessage,
		conversation,
	)
	if err != nil {
		t.Fatalf("persist augmented source message: %v", err)
	}
}

func retainedHistoryForFollowUp(
	t *testing.T,
	instance *bot,
	followUpMessage *discordgo.Message,
	options messageContentOptions,
) []chatMessage {
	t.Helper()

	history, _ := instance.buildConversation(
		context.Background(),
		followUpMessage,
		defaultMaxText,
		options,
		defaultMaxMessages,
		false,
		false,
	)

	if len(history) != 3 {
		t.Fatalf("unexpected conversation length: %d", len(history))
	}

	return history
}

func assertRetainedSearchHistory(t *testing.T, history []chatMessage) {
	t.Helper()

	sourceParts, ok := history[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected source message content type: %T", history[0].Content)
	}

	if len(sourceParts) != 2 {
		t.Fatalf("unexpected retained source part count: %d", len(sourceParts))
	}

	if sourceParts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected retained image part: %#v", sourceParts[1])
	}

	sourceText := messageContentText(history[0].Content)
	if containsFold(sourceText, replyTargetSectionName) {
		t.Fatalf("unexpected retained reply-target section: %q", sourceText)
	}

	for _, expectedFragment := range []string{
		visualSearchSectionName,
		websiteSectionName,
		youtubeSectionName,
		redditSectionName,
		webSearchSectionName,
		"Sword Art Online",
		"https://example.com",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://www.reddit.com/r/testing/comments/abc123/thread-title/",
		"Query: example",
	} {
		if !containsFold(sourceText, expectedFragment) {
			t.Fatalf("expected retained source text fragment %q in %q", expectedFragment, sourceText)
		}
	}
}

func assertRetainedVideoHistory(t *testing.T, history []chatMessage) {
	t.Helper()

	sourceParts, ok := history[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected source message content type: %T", history[0].Content)
	}

	if len(sourceParts) != 3 {
		t.Fatalf("unexpected retained source part count: %d", len(sourceParts))
	}

	sourceText := messageContentText(history[0].Content)
	for _, expectedFragment := range []string{
		"TikTok clip analysis",
		"Facebook clip analysis",
		mediaAnalysisOpenTag,
		mediaAnalysisCloseTag,
	} {
		if !containsFold(sourceText, expectedFragment) {
			t.Fatalf("expected retained source text fragment %q in %q", expectedFragment, sourceText)
		}
	}

	if sourceParts[1]["type"] != contentTypeVideoData || sourceParts[2]["type"] != contentTypeVideoData {
		t.Fatalf("expected retained video parts: %#v", sourceParts)
	}

	if sourceParts[1][contentFieldFilename] != "tiktok_123.mp4" {
		t.Fatalf("unexpected tiktok filename: %#v", sourceParts[1][contentFieldFilename])
	}

	if sourceParts[2][contentFieldFilename] != "facebook_456.mp4" {
		t.Fatalf("unexpected facebook filename: %#v", sourceParts[2][contentFieldFilename])
	}
}

func newHistoryRetentionTestBot(t *testing.T) *bot {
	t.Helper()

	const (
		botUserID = "bot-user"
		channelID = "channel-1"
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

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(10)

	return instance
}

func setCachedAssistantNode(
	instance *bot,
	assistantMessage *discordgo.Message,
	parentMessage *discordgo.Message,
) {
	node := instance.nodes.getOrCreate(assistantMessage.ID)

	node.mu.Lock()
	defer node.mu.Unlock()

	node.role = messageRoleAssistant
	node.text = testAssistantReply
	node.thinkingText = testThinkingReply
	node.urlScanText = testAssistantReply
	node.parentMessage = parentMessage
	node.initialized = true
	instance.nodes.cacheLockedNode(assistantMessage.ID, node)
}

func setCachedUserNode(
	instance *bot,
	userMessage *discordgo.Message,
	parentMessage *discordgo.Message,
	text string,
) {
	node := instance.nodes.getOrCreate(userMessage.ID)

	node.mu.Lock()
	defer node.mu.Unlock()

	node.role = messageRoleUser
	node.text = text
	node.urlScanText = text
	node.parentMessage = parentMessage
	node.initialized = true
	instance.nodes.cacheLockedNode(userMessage.ID, node)
}
