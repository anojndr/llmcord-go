package main

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/lib/pq"
)

const testSharedHomeBotsStoreKey = "shared-home-bots"

var errTestBackendUnavailable = errors.New("backend unavailable")

type testMessageNodeStoreBackend struct {
	mu        sync.Mutex
	snapshots map[string]messageNodeStoreSnapshot
}

type blockingSaveMessageNodeStoreBackend struct {
	mu          sync.Mutex
	saveCalls   int
	saveStarted chan struct{}
	releaseSave chan struct{}
	snapshot    messageNodeStoreSnapshot
}

type failingMessageNodeStoreBackend struct {
	loadErr error
	saveErr error
}

func newTestMessageNodeStoreBackend() *testMessageNodeStoreBackend {
	backend := new(testMessageNodeStoreBackend)
	backend.snapshots = make(map[string]messageNodeStoreSnapshot)

	return backend
}

func newBlockingSaveMessageNodeStoreBackend() *blockingSaveMessageNodeStoreBackend {
	return &blockingSaveMessageNodeStoreBackend{
		mu:          sync.Mutex{},
		saveCalls:   0,
		saveStarted: make(chan struct{}, 1),
		releaseSave: make(chan struct{}),
		snapshot: messageNodeStoreSnapshot{
			Version: 0,
			Nodes:   nil,
		},
	}
}

func newFailingMessageNodeStoreBackend() *failingMessageNodeStoreBackend {
	return &failingMessageNodeStoreBackend{
		loadErr: nil,
		saveErr: nil,
	}
}

func newTestPQError(code string, message string) *pq.Error {
	pqErr := new(pq.Error)
	pqErr.Code = pq.ErrorCode(code)
	pqErr.Message = message

	return pqErr
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

func (backend *blockingSaveMessageNodeStoreBackend) loadSnapshot(
	_ string,
	_ int,
) (messageNodeStoreSnapshot, error) {
	return messageNodeStoreSnapshot{}, os.ErrNotExist
}

func (backend *blockingSaveMessageNodeStoreBackend) saveSnapshot(
	_ string,
	snapshot messageNodeStoreSnapshot,
) error {
	backend.mu.Lock()
	backend.saveCalls++
	backend.snapshot = messageNodeStoreSnapshot{
		Version: snapshot.Version,
		Nodes:   maps.Clone(snapshot.Nodes),
	}
	backend.mu.Unlock()

	select {
	case backend.saveStarted <- struct{}{}:
	default:
	}

	<-backend.releaseSave

	return nil
}

func (backend *blockingSaveMessageNodeStoreBackend) close() error {
	return nil
}

func (backend *failingMessageNodeStoreBackend) loadSnapshot(
	_ string,
	_ int,
) (messageNodeStoreSnapshot, error) {
	return messageNodeStoreSnapshot{}, backend.loadErr
}

func (backend *failingMessageNodeStoreBackend) saveSnapshot(
	_ string,
	_ messageNodeStoreSnapshot,
) error {
	return backend.saveErr
}

func (backend *failingMessageNodeStoreBackend) close() error {
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

func cacheInitializedStoreNode(
	store *messageNodeStore,
	messageID string,
	text string,
) {
	node := store.getOrCreate(messageID)
	node.mu.Lock()
	node.role = messageRoleUser
	node.text = text
	node.initialized = true
	store.cacheLockedNode(messageID, node)
	node.mu.Unlock()
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

	storeKey := messageNodeStoreKey("config.yaml", testSharedHomeBotsStoreKey)

	if storeKey != testSharedHomeBotsStoreKey {
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

func TestEncodeMessageNodeSnapshotJSONSanitizesPostgresUnsupportedUnicodeEscapes(t *testing.T) {
	t.Parallel()

	snapshotBytes, err := encodeMessageNodeSnapshotJSON(testMessageNodeSnapshotsWithNULs())
	if err != nil {
		t.Fatalf("encode snapshot JSON: %v", err)
	}

	if bytes.Contains(snapshotBytes, []byte("\\u0000")) {
		t.Fatalf("expected encoded snapshot JSON to omit NUL escapes: %s", snapshotBytes)
	}

	var decodedNodes map[string]messageNodeSnapshot

	err = decodeMessageNodeSnapshotJSON(snapshotBytes, &decodedNodes)
	if err != nil {
		t.Fatalf("decode sanitized snapshot JSON: %v", err)
	}

	decodedSnapshot, ok := decodedNodes["message-"+postgresJSONTextReplacement+"id"]
	if !ok {
		t.Fatalf("expected sanitized message id key, got %#v", decodedNodes)
	}

	if decodedSnapshot.Text != "reply"+postgresJSONTextReplacement+"text" {
		t.Fatalf("unexpected sanitized message text: %q", decodedSnapshot.Text)
	}

	if decodedSnapshot.SearchMetadata == nil {
		t.Fatal("expected sanitized search metadata")
	}

	if decodedSnapshot.SearchMetadata.Queries[0] != "query"+postgresJSONTextReplacement+"value" {
		t.Fatalf("unexpected sanitized query: %#v", decodedSnapshot.SearchMetadata.Queries)
	}

	if decodedSnapshot.ParentMessage == nil {
		t.Fatal("expected sanitized parent message")
	}

	if decodedSnapshot.ParentMessage.Content != "parent"+postgresJSONTextReplacement+"content" {
		t.Fatalf("unexpected sanitized parent message content: %q", decodedSnapshot.ParentMessage.Content)
	}
}

func TestPersistentMessageNodeStoreAnnotatesPostgresCorruptionOnLoad(t *testing.T) {
	t.Parallel()

	backend := newFailingMessageNodeStoreBackend()
	backend.loadErr = newTestPQError(
		postgresDataCorruptedSQLState,
		"invalid page in block 31535 of relation \"base/16387/16396\"",
	)

	_, err := newPersistentMessageNodeStore(10, testSharedHomeBotsStoreKey, backend)
	if err == nil {
		t.Fatal("expected load error")
	}

	errorText := err.Error()
	if !strings.Contains(errorText, "postgres message history storage appears corrupted") {
		t.Fatalf("expected corruption annotation, got %q", errorText)
	}

	if !strings.Contains(errorText, postgresDataCorruptedSQLState) {
		t.Fatalf("expected SQLSTATE in error, got %q", errorText)
	}

	if !strings.Contains(errorText, messageNodeStoreTableName) {
		t.Fatalf("expected table name in error, got %q", errorText)
	}
}

func TestMessageNodeStorePersistAnnotatesPostgresCorruption(t *testing.T) {
	t.Parallel()

	store := newMessageNodeStore(10)
	store.storeKey = testSharedHomeBotsStoreKey
	backend := newFailingMessageNodeStoreBackend()
	backend.saveErr = newTestPQError(postgresIndexCorruptedSQLState, "index is corrupted")
	store.backend = backend

	cacheInitializedStoreNode(store, "message-1", "cached text")

	err := store.persist()
	if err == nil {
		t.Fatal("expected persist error")
	}

	errorText := err.Error()
	if !strings.Contains(errorText, "postgres message history storage appears corrupted") {
		t.Fatalf("expected corruption annotation, got %q", errorText)
	}

	if !strings.Contains(errorText, postgresIndexCorruptedSQLState) {
		t.Fatalf("expected SQLSTATE in error, got %q", errorText)
	}

	if !strings.Contains(errorText, testSharedHomeBotsStoreKey) {
		t.Fatalf("expected store key in error, got %q", errorText)
	}
}

func TestAnnotateMessageHistoryPersistenceErrorWithGenericErrorAndStoreKey(t *testing.T) {
	t.Parallel()

	err := annotateMessageHistoryPersistenceError(
		"load persisted message history",
		testSharedHomeBotsStoreKey,
		errTestBackendUnavailable,
	)
	if err == nil {
		t.Fatal("expected wrapped error")
	}

	if !strings.Contains(err.Error(), testSharedHomeBotsStoreKey) {
		t.Fatalf("expected store key in wrapped error, got %q", err)
	}

	if !errors.Is(err, errTestBackendUnavailable) {
		t.Fatalf("expected wrapped source error, got %v", err)
	}
}

func TestAnnotateMessageHistoryPersistenceErrorWithGenericErrorAndEmptyStoreKey(t *testing.T) {
	t.Parallel()

	err := annotateMessageHistoryPersistenceError(
		"persist message history",
		"   ",
		errTestBackendUnavailable,
	)
	if err == nil {
		t.Fatal("expected wrapped error")
	}

	errorText := err.Error()
	if strings.Contains(errorText, "store key") {
		t.Fatalf("did not expect store key text in wrapped error, got %q", errorText)
	}

	if !strings.Contains(errorText, "persist message history") {
		t.Fatalf("expected operation in wrapped error, got %q", errorText)
	}
}

func TestAnnotateMessageHistoryPersistenceErrorWithCorruptionAndEmptyStoreKey(t *testing.T) {
	t.Parallel()

	sourceErr := newTestPQError(
		postgresDataCorruptedSQLState,
		"invalid page in block 31535 of relation \"base/16387/16396\"",
	)

	err := annotateMessageHistoryPersistenceError(
		"load persisted message history",
		"",
		sourceErr,
	)
	if err == nil {
		t.Fatal("expected wrapped error")
	}

	errorText := err.Error()
	if strings.Contains(errorText, "store key") {
		t.Fatalf("did not expect store key text in wrapped error, got %q", errorText)
	}

	if !strings.Contains(errorText, messageNodeStoreTableName) {
		t.Fatalf("expected table name in wrapped error, got %q", errorText)
	}
}

func TestAnnotateMessageHistoryPersistenceErrorReturnsOriginalForBlankOperation(t *testing.T) {
	t.Parallel()

	err := annotateMessageHistoryPersistenceError(
		"   ",
		testSharedHomeBotsStoreKey,
		errTestBackendUnavailable,
	)
	if !errors.Is(err, errTestBackendUnavailable) {
		t.Fatalf("expected original error for blank operation, got %v", err)
	}
}

func TestAnnotateMessageHistoryPersistenceErrorReturnsNilForNilError(t *testing.T) {
	t.Parallel()

	err := annotateMessageHistoryPersistenceError("persist message history", testSharedHomeBotsStoreKey, nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestPostgresMessageHistoryCorruptionSQLStateReturnsFalseForGenericError(t *testing.T) {
	t.Parallel()

	sqlState, corrupted := postgresMessageHistoryCorruptionSQLState(errTestBackendUnavailable)
	if corrupted {
		t.Fatalf("expected generic error to not be treated as corruption, got SQLSTATE %q", sqlState)
	}

	if sqlState != "" {
		t.Fatalf("expected empty SQLSTATE for generic error, got %q", sqlState)
	}
}

func testMessageNodeSnapshotsWithNULs() map[string]messageNodeSnapshot {
	return map[string]messageNodeSnapshot{
		"message-\x00id": {
			Role:                     "assist\x00ant",
			Text:                     "reply\x00text",
			ThinkingText:             "thinking\x00text",
			URLScanText:              "scan\x00text",
			RentryURL:                "https://example.com/\x00notes",
			Media:                    testContentPartSnapshotsWithNULs(),
			SearchMetadata:           testSearchMetadataWithNULs(),
			HasBadAttachments:        false,
			AttachmentDownloadFailed: false,
			FetchParentFailed:        false,
			ParentMessage:            testDiscordMessageSnapshotWithNULs(),
			Initialized:              true,
		},
	}
}

func testContentPartSnapshotsWithNULs() []contentPartSnapshot {
	return []contentPartSnapshot{
		{
			Type:     contentTypeText,
			Text:     "part\x00text",
			ImageURL: "",
			Data:     nil,
			MIMEType: "",
			Filename: "",
		},
		{
			Type:     contentTypeDocument,
			Text:     "",
			ImageURL: "",
			Data:     []byte("doc-bytes"),
			MIMEType: "application/\x00pdf",
			Filename: "report\x00.pdf",
		},
	}
}

func testSearchMetadataWithNULs() *searchMetadata {
	return &searchMetadata{
		Queries: []string{"query\x00value"},
		Results: []webSearchResult{{
			Query: "search\x00query",
			Text:  "result\x00text",
		}},
		MaxURLs: 2,
		VisualSearchSources: []visualSearchSourceGroup{{
			Label: "group\x00label",
			Sources: []searchSource{{
				Title: "source\x00title",
				URL:   "https://example.com/\x00source",
			}},
		}},
	}
}

func testDiscordMessageSnapshotWithNULs() *discordMessageSnapshot {
	return &discordMessageSnapshot{
		ID:        "parent\x00id",
		ChannelID: "channel\x00id",
		GuildID:   "guild\x00id",
		Type:      0,
		Content:   "parent\x00content",
		Author: &discordUserSnapshot{
			ID:  "user\x00id",
			Bot: true,
		},
		MentionUserIDs: []string{"mention\x00id"},
		Attachments: []discordAttachmentSnapshot{{
			Filename:    "attachment\x00.txt",
			ContentType: "text/\x00plain",
			URL:         "https://example.com/\x00attachment",
		}},
		Embeds: []discordEmbedSnapshot{{
			Title:       "embed\x00title",
			Description: "embed\x00description",
			FooterText:  "embed\x00footer",
		}},
		MessageReference: &discordMessageReferenceSnapshot{
			MessageID: "ref\x00message",
			ChannelID: "ref\x00channel",
			GuildID:   "ref\x00guild",
		},
	}
}

func TestMessageNodeStorePersistBestEffortIsAsyncAndDebounced(t *testing.T) {
	t.Parallel()

	backend := newBlockingSaveMessageNodeStoreBackend()
	store := newMessageNodeStore(10)
	store.storeKey = "async-store"
	store.backend = backend
	store.persistDelay = 20 * time.Millisecond
	store.startSaveWorker()

	cacheInitializedStoreNode(store, "message-1", "cached text")

	done := make(chan struct{})

	go func() {
		store.persistBestEffort()
		store.persistBestEffort()
		store.persistBestEffort()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("persistBestEffort blocked the caller")
	}

	select {
	case <-backend.saveStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for background save to start")
	}

	close(backend.releaseSave)
	store.stopSaveWorker()

	time.Sleep(100 * time.Millisecond)

	backend.mu.Lock()
	saveCalls := backend.saveCalls
	backend.mu.Unlock()

	if saveCalls != 1 {
		t.Fatalf("expected a single debounced save, got %d", saveCalls)
	}
}

func TestMessageNodeStoreCloseFlushesPendingPersist(t *testing.T) {
	t.Parallel()

	backend := newTestMessageNodeStoreBackend()
	store := newMessageNodeStore(10)
	store.storeKey = "close-store"
	store.backend = backend
	store.persistDelay = time.Hour
	store.startSaveWorker()

	cacheInitializedStoreNode(store, "message-1", "cached text")
	store.persistBestEffort()

	err := store.close()
	if err != nil {
		t.Fatalf("close store: %v", err)
	}

	snapshot, ok := backend.snapshot("close-store")
	if !ok {
		t.Fatal("expected close to flush a snapshot")
	}

	node, found := snapshot.Nodes["message-1"]
	if !found {
		t.Fatalf("expected flushed node in snapshot: %#v", snapshot.Nodes)
	}

	if node.Text != "cached text" {
		t.Fatalf("unexpected flushed node text: %#v", node)
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
		node.thinkingText != testThinkingReply ||
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
		persistedNode.ThinkingText != testThinkingReply ||
		!persistedNode.Initialized {
		t.Fatalf("unexpected persisted node: %#v", persistedNode)
	}
}

func testAssistantMessageNodeSnapshot() messageNodeSnapshot {
	return messageNodeSnapshot{
		Role:                     messageRoleAssistant,
		Text:                     testAssistantReply,
		ThinkingText:             testThinkingReply,
		URLScanText:              "",
		RentryURL:                "",
		Media:                    nil,
		SearchMetadata:           nil,
		HasBadAttachments:        false,
		AttachmentDownloadFailed: false,
		FetchParentFailed:        false,
		ParentMessage:            nil,
		Initialized:              true,
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
			maxImages:                defaultMaxImages,
			allowAudio:               false,
			allowDocuments:           false,
			allowedDocumentMIMETypes: nil,
			allowVideo:               false,
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

	gotThinkingText := restartedInstance.thinkingTextForMessage(assistantMessageID)
	if gotThinkingText != testThinkingReply {
		t.Fatalf("unexpected restored thinking text: got %q want %q", gotThinkingText, testThinkingReply)
	}
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
			maxImages:                0,
			allowAudio:               false,
			allowDocuments:           false,
			allowedDocumentMIMETypes: nil,
			allowVideo:               true,
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

	instance := newHistoryRetentionTestBot(t)

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
		func(left, right visualSearchSourceGroup) bool {
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
