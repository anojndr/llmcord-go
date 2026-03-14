package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestDefaultMessageNodeStorePathUsesProjectRoot(t *testing.T) {
	t.Parallel()

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}

	historyPath := defaultMessageNodeStorePath(defaultConfigPath)
	expectedDirectory := filepath.Join(workingDirectory, messageNodeStoreDirectoryName)

	if filepath.Dir(historyPath) != expectedDirectory {
		t.Fatalf("unexpected history directory: got %q want %q", filepath.Dir(historyPath), expectedDirectory)
	}
}

func TestPersistentMessageNodeStoreRestoresRetainedSearchHistoryAfterRestart(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "source-message"
		assistantMessageID = "assistant-message"
		followUpMessageID  = "follow-up-message"
	)

	historyPath := filepath.Join(t.TempDir(), "message-history.gob")
	initialInstance := newPersistentHistoryTestBot(t, historyPath)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "at ai what is in this image and these links"

	conversation := newRetainedSearchContextConversation(t, userID)
	persistAugmentedSourceConversation(t, initialInstance, sourceMessage, conversation)

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantMessageID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.MessageReference = sourceMessage.Reference()
	assistantMessage.Type = discordgo.MessageTypeReply
	setCachedAssistantNode(initialInstance, assistantMessage, sourceMessage)
	setAssistantNodeSearchMetadataAndRentry(
		t,
		initialInstance,
		assistantMessage.ID,
		newSearchMetadata(
			[]string{"example"},
			[]webSearchResult{{Query: "example", Text: "Title: Example\nURL: https://example.com"}},
			defaultWebSearchMaxURLs,
		),
		"https://rentry.co/example",
	)

	err := initialInstance.nodes.persist()
	if err != nil {
		t.Fatalf("persist message history: %v", err)
	}

	restartedInstance := newPersistentHistoryTestBot(t, historyPath)
	followUpMessage := newRestartFollowUpMessage(
		followUpMessageID,
		channelID,
		userID,
		assistantMessage,
		"follow-up question",
	)

	history := retainedRestartHistoryForFollowUp(
		t,
		restartedInstance,
		followUpMessage,
		messageContentOptions{
			maxImages:      defaultMaxImages,
			allowAudio:     false,
			allowDocuments: false,
			allowVideo:     false,
		},
	)

	assertRetainedSearchHistory(t, history)
	assertRestartedConversationTail(t, history, userID, "assistant reply", "follow-up question")
	assertPersistedAssistantMetadata(
		t,
		restartedInstance,
		assistantMessageID,
		[]string{"example"},
		[]webSearchResult{{Query: "example", Text: "Title: Example\nURL: https://example.com"}},
		"https://rentry.co/example",
	)
}

func TestPersistentMessageNodeStoreRestoresRetainedVideoHistoryAfterRestart(t *testing.T) {
	t.Parallel()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		sourceMessageID    = "source-message"
		assistantMessageID = "assistant-message"
		followUpMessageID  = "follow-up-message"
	)

	historyPath := filepath.Join(t.TempDir(), "message-history.gob")
	initialInstance := newPersistentHistoryTestBot(t, historyPath)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = sourceMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "at ai summarize these videos"

	conversation := newRetainedVideoContextConversation(t, userID)
	persistAugmentedSourceConversation(t, initialInstance, sourceMessage, conversation)

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantMessageID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.MessageReference = sourceMessage.Reference()
	assistantMessage.Type = discordgo.MessageTypeReply
	setCachedAssistantNode(initialInstance, assistantMessage, sourceMessage)

	err := initialInstance.nodes.persist()
	if err != nil {
		t.Fatalf("persist message history: %v", err)
	}

	restartedInstance := newPersistentHistoryTestBot(t, historyPath)
	followUpMessage := newRestartFollowUpMessage(
		followUpMessageID,
		channelID,
		userID,
		assistantMessage,
		"follow-up question",
	)

	history := retainedRestartHistoryForFollowUp(
		t,
		restartedInstance,
		followUpMessage,
		messageContentOptions{
			maxImages:      0,
			allowAudio:     false,
			allowDocuments: false,
			allowVideo:     true,
		},
	)

	assertRetainedVideoHistory(t, history)
	assertRestartedConversationTail(t, history, userID, "assistant reply", "follow-up question")
	assertRetainedVideoBytes(t, history)
}

func newPersistentHistoryTestBot(t *testing.T, historyPath string) *bot {
	t.Helper()

	instance := newHistoryRetentionTestBot(t, "bot-user", "channel-1")

	store, err := newPersistentMessageNodeStore(10, historyPath)
	if err != nil {
		t.Fatalf("create persistent message store: %v", err)
	}

	instance.nodes = store

	return instance
}

func setAssistantNodeSearchMetadataAndRentry(
	t *testing.T,
	instance *bot,
	messageID string,
	metadata *searchMetadata,
	rentryURL string,
) {
	t.Helper()

	node, ok := instance.nodes.get(messageID)
	if !ok {
		t.Fatalf("assistant node %q not found", messageID)
	}

	node.mu.Lock()
	node.searchMetadata = cloneSearchMetadata(metadata)
	node.rentryURL = rentryURL
	instance.nodes.cacheLockedNode(messageID, node)
	node.mu.Unlock()
}

func newRestartFollowUpMessage(
	messageID string,
	channelID string,
	userID string,
	assistantMessage *discordgo.Message,
	content string,
) *discordgo.Message {
	message := new(discordgo.Message)
	message.ID = messageID
	message.ChannelID = channelID
	message.Author = newDiscordUser(userID, false)
	message.Content = content
	message.MessageReference = assistantMessage.Reference()

	referencedMessage := new(discordgo.Message)
	referencedMessage.ID = assistantMessage.ID
	referencedMessage.ChannelID = assistantMessage.ChannelID
	referencedMessage.Author = assistantMessage.Author
	message.ReferencedMessage = referencedMessage

	return message
}

func retainedRestartHistoryForFollowUp(
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

func assertRestartedConversationTail(
	t *testing.T,
	history []chatMessage,
	userID string,
	assistantReply string,
	followUpText string,
) {
	t.Helper()

	if history[1].Role != messageRoleAssistant || history[1].Content != assistantReply {
		t.Fatalf("unexpected assistant history: %#v", history[1])
	}

	expectedFollowUp := "<@" + userID + ">: " + followUpText
	if history[2].Role != messageRoleUser || history[2].Content != expectedFollowUp {
		t.Fatalf("unexpected follow-up history: %#v", history[2])
	}
}

func assertPersistedAssistantMetadata(
	t *testing.T,
	instance *bot,
	messageID string,
	expectedQueries []string,
	expectedResults []webSearchResult,
	expectedRentryURL string,
) {
	t.Helper()

	metadata := instance.searchMetadataForMessage(messageID)
	if metadata == nil {
		t.Fatal("expected persisted search metadata")
	}

	if !slices.Equal(metadata.Queries, expectedQueries) {
		t.Fatalf("unexpected persisted queries: %#v", metadata.Queries)
	}

	if !slices.Equal(metadata.Results, expectedResults) {
		t.Fatalf("unexpected persisted results: %#v", metadata.Results)
	}

	if metadata.MaxURLs != defaultWebSearchMaxURLs {
		t.Fatalf("unexpected persisted max urls: %d", metadata.MaxURLs)
	}

	node, ok := instance.nodes.get(messageID)
	if !ok {
		t.Fatalf("persisted assistant node %q not found", messageID)
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	if node.rentryURL != expectedRentryURL {
		t.Fatalf("unexpected persisted rentry url: %q", node.rentryURL)
	}
}

func assertRetainedVideoBytes(t *testing.T, history []chatMessage) {
	t.Helper()

	sourceParts, ok := history[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected source history content type: %T", history[0].Content)
	}

	firstBytes, firstOK := sourceParts[1][contentFieldBytes].([]byte)
	if !firstOK {
		t.Fatalf("unexpected first retained video bytes: %#v", sourceParts[1][contentFieldBytes])
	}

	secondBytes, secondOK := sourceParts[2][contentFieldBytes].([]byte)
	if !secondOK {
		t.Fatalf("unexpected second retained video bytes: %#v", sourceParts[2][contentFieldBytes])
	}

	if !bytes.Equal(firstBytes, []byte("tiktok-video")) {
		t.Fatalf("unexpected first retained video bytes: %#v", firstBytes)
	}

	if !bytes.Equal(secondBytes, []byte("facebook-video")) {
		t.Fatalf("unexpected second retained video bytes: %#v", secondBytes)
	}
}
