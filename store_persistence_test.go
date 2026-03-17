package main

import (
	"bytes"
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

type testMessageNodeStoreBackend struct {
	mu        sync.Mutex
	snapshots map[string]messageNodeStoreSnapshot
}

func newTestMessageNodeStoreBackend() *testMessageNodeStoreBackend {
	backend := new(testMessageNodeStoreBackend)
	backend.snapshots = make(map[string]messageNodeStoreSnapshot)

	return backend
}

func (backend *testMessageNodeStoreBackend) loadSnapshot(
	storeKey string,
	_ int,
) (messageNodeStoreSnapshot, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()

	snapshot, ok := backend.snapshots[storeKey]
	if !ok {
		return messageNodeStoreSnapshot{}, os.ErrNotExist
	}

	return messageNodeStoreSnapshot{
		Version: snapshot.Version,
		Nodes:   maps.Clone(snapshot.Nodes),
	}, nil
}

func (backend *testMessageNodeStoreBackend) saveSnapshot(
	storeKey string,
	snapshot messageNodeStoreSnapshot,
) error {
	backend.mu.Lock()
	backend.snapshots[storeKey] = messageNodeStoreSnapshot{
		Version: snapshot.Version,
		Nodes:   maps.Clone(snapshot.Nodes),
	}
	backend.mu.Unlock()

	return nil
}

func (backend *testMessageNodeStoreBackend) close() error {
	return nil
}

func (backend *testMessageNodeStoreBackend) setSnapshot(
	storeKey string,
	snapshot messageNodeStoreSnapshot,
) {
	backend.mu.Lock()
	backend.snapshots[storeKey] = messageNodeStoreSnapshot{
		Version: snapshot.Version,
		Nodes:   maps.Clone(snapshot.Nodes),
	}
	backend.mu.Unlock()
}

func (backend *testMessageNodeStoreBackend) snapshot(
	storeKey string,
) (messageNodeStoreSnapshot, bool) {
	backend.mu.Lock()
	defer backend.mu.Unlock()

	snapshot, ok := backend.snapshots[storeKey]
	if !ok {
		return messageNodeStoreSnapshot{
			Version: 0,
			Nodes:   nil,
		}, false
	}

	return messageNodeStoreSnapshot{
		Version: snapshot.Version,
		Nodes:   maps.Clone(snapshot.Nodes),
	}, true
}

func TestDefaultMessageNodeStoreKeyUsesExpectedPrefix(t *testing.T) {
	t.Parallel()

	storeKey := defaultMessageNodeStoreKey(defaultConfigPath)

	if !strings.HasPrefix(storeKey, "message-history-") {
		t.Fatalf("unexpected message store key prefix: %q", storeKey)
	}
}

func TestMessageNodeStoreKeyUsesConfiguredValue(t *testing.T) {
	t.Parallel()

	storeKey := messageNodeStoreKey("config.yaml", "shared-home-bots")

	if storeKey != "shared-home-bots" {
		t.Fatalf("unexpected configured store key: %q", storeKey)
	}
}

func TestMessageNodeStoreKeyFallsBackToDefaultWhenEmpty(t *testing.T) {
	t.Parallel()

	defaultStoreKey := defaultMessageNodeStoreKey("config.yaml")
	storeKey := messageNodeStoreKey("config.yaml", "")

	if storeKey != defaultStoreKey {
		t.Fatalf("unexpected fallback store key: got %q want %q", storeKey, defaultStoreKey)
	}
}

func TestDefaultMessageNodeStoreKeyIsStableForEquivalentPaths(t *testing.T) {
	t.Parallel()

	leftStoreKey := defaultMessageNodeStoreKey("./config.yaml")
	rightStoreKey := defaultMessageNodeStoreKey("config.yaml")

	if leftStoreKey != rightStoreKey {
		t.Fatalf("unexpected store key mismatch: left=%q right=%q", leftStoreKey, rightStoreKey)
	}
}

func TestPersistentMessageNodeStoreLoadsAndPersistsSnapshot(t *testing.T) {
	t.Parallel()

	const storeKey = "message-history-test"

	backend := newTestMessageNodeStoreBackend()

	seedSnapshot := messageNodeStoreSnapshot{
		Version: messageNodeStoreSnapshotVersion,
		Nodes: map[string]messageNodeSnapshot{
			"assistant-message": testAssistantMessageNodeSnapshot(),
		},
	}
	backend.setSnapshot(storeKey, seedSnapshot)

	store, err := newPersistentMessageNodeStore(10, storeKey, backend)
	if err != nil {
		t.Fatalf("load persistent message store: %v", err)
	}

	if store.storeKey != storeKey {
		t.Fatalf("unexpected store key: got %q want %q", store.storeKey, storeKey)
	}

	node, found := store.get("assistant-message")
	if !found {
		t.Fatal("expected assistant node from persisted snapshot")
	}

	node.mu.Lock()
	if node.role != messageRoleAssistant ||
		node.text != testAssistantReply ||
		!node.initialized {
		node.mu.Unlock()
		t.Fatalf("unexpected restored node: %#v", node)
	}
	node.mu.Unlock()

	err = store.persist()
	if err != nil {
		t.Fatalf("persist message history: %v", err)
	}

	persistedSnapshot, ok := backend.snapshot(storeKey)
	if !ok {
		t.Fatal("expected persisted snapshot in backend")
	}

	persistedNode, found := persistedSnapshot.Nodes["assistant-message"]
	if !found {
		t.Fatal("expected assistant node in persisted snapshot")
	}

	if persistedNode.Role != messageRoleAssistant ||
		persistedNode.Text != testAssistantReply ||
		!persistedNode.Initialized {
		t.Fatalf("unexpected persisted node: %#v", persistedNode)
	}
}

func testAssistantMessageNodeSnapshot() messageNodeSnapshot {
	return messageNodeSnapshot{
		Role:              messageRoleAssistant,
		Text:              testAssistantReply,
		URLScanText:       "",
		RentryURL:         "",
		Media:             nil,
		SearchMetadata:    nil,
		HasBadAttachments: false,
		FetchParentFailed: false,
		ParentMessage:     nil,
		Initialized:       true,
	}
}

func testRestartSearchMetadata() *searchMetadata {
	metadata := newSearchMetadata(
		[]string{"example"},
		[]webSearchResult{{Query: "example", Text: "Title: Example\nURL: https://example.com"}},
		defaultWebSearchMaxURLs,
	)
	metadata.VisualSearchSources = []visualSearchSourceGroup{{
		Label: "",
		Sources: []searchSource{{
			Title: "Top match: Example image",
			URL:   "https://images.example.com/example",
		}},
	}}

	return metadata
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

	expectedMetadata := testRestartSearchMetadata()
	backend := newTestMessageNodeStoreBackend()

	const storeKey = "message-history-search-restart"

	initialInstance := newPersistentHistoryTestBot(t, backend, storeKey)

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
		expectedMetadata,
		"https://rentry.co/example",
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
	assertRestartedConversationTail(t, history, userID, testAssistantReply, "follow-up question")
	assertPersistedAssistantMetadata(
		t,
		restartedInstance,
		assistantMessageID,
		expectedMetadata.Queries,
		expectedMetadata.Results,
		expectedMetadata.VisualSearchSources,
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

	backend := newTestMessageNodeStoreBackend()

	const storeKey = "message-history-video-restart"

	initialInstance := newPersistentHistoryTestBot(t, backend, storeKey)

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

	restartedInstance := newPersistentHistoryTestBot(t, backend, storeKey)
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
	assertRestartedConversationTail(t, history, userID, testAssistantReply, "follow-up question")
	assertRetainedVideoBytes(t, history)
}

func newPersistentHistoryTestBot(
	t *testing.T,
	backend messageNodeStoreBackend,
	storeKey string,
) *bot {
	t.Helper()

	instance := newHistoryRetentionTestBot(t, "bot-user", "channel-1")

	store, err := newPersistentMessageNodeStore(10, storeKey, backend)
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
	expectedVisualSearchSources []visualSearchSourceGroup,
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

	if !slices.EqualFunc(
		metadata.VisualSearchSources,
		expectedVisualSearchSources,
		func(left visualSearchSourceGroup, right visualSearchSourceGroup) bool {
			return slices.Equal(left.Sources, right.Sources)
		},
	) {
		t.Fatalf("unexpected persisted visual search sources: %#v", metadata.VisualSearchSources)
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
