package main

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

type contentPart map[string]any

type messageNode struct {
	role                     string
	text                     string
	thinkingText             string
	urlScanText              string
	pastebinURL              string
	providerResponseID       string
	providerResponseModel    string
	media                    []contentPart
	searchMetadata           *searchMetadata
	compactionSummary        *messageNodeCompactionSummary
	hasBadAttachments        bool
	attachmentDownloadFailed bool
	fetchParentFailed        bool
	parentMessage            *discordgo.Message
	initialized              bool
	mu                       sync.Mutex
}

// messageNodeCompactionSummary records the auto-compaction boundary for a
// user message: the handoff summary text produced by compacting the older
// history. A later request that re-walks the reply chain stops at the message
// carrying the boundary, renders the recorded summary for everything before
// it, and keeps the tail — instead of re-summarizing the entire history again.
// `anchor` is the message ID the boundary belongs to (the message whose older
// history the summary replaces).
type messageNodeCompactionSummary struct {
	text    string
	anchor  string
	applied bool
}

type messageNodeStore struct {
	mu            sync.Mutex
	nodes         map[string]*messageNode
	capacity      int
	storeKey      string
	backend       messageNodeStoreBackend
	saveMu        sync.Mutex
	saveWorkerMu  sync.Mutex
	saveRequests  chan struct{}
	saveStop      chan struct{}
	saveDone      chan struct{}
	persistDelay  time.Duration
	snapshotMu    sync.Mutex
	snapshotCache map[string]messageNodeSnapshot
}

func newMessageNodeStore(capacity int) *messageNodeStore {
	store := new(messageNodeStore)
	store.nodes = make(map[string]*messageNode)
	store.capacity = capacity
	store.persistDelay = defaultMessageNodeStorePersistDelay
	store.snapshotCache = make(map[string]messageNodeSnapshot)

	return store
}

func (store *messageNodeStore) getOrCreate(messageID string) *messageNode {
	store.mu.Lock()
	defer store.mu.Unlock()

	if node, ok := store.nodes[messageID]; ok {
		return node
	}

	node := new(messageNode)
	store.nodes[messageID] = node

	return node
}

func (store *messageNodeStore) get(messageID string) (*messageNode, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()

	node, ok := store.nodes[messageID]

	return node, ok
}

func (store *messageNodeStore) addPending(messageID string, parentMessage *discordgo.Message) *messageNode {
	node := new(messageNode)
	node.parentMessage = parentMessage
	node.mu.Lock()

	store.mu.Lock()
	store.nodes[messageID] = node
	store.mu.Unlock()

	return node
}

// recordCompactionSummary publishes a compaction boundary for a user message
// under a fresh node lock so worker goroutines never mutate a node that
// another goroutine holds.
func (store *messageNodeStore) recordCompactionSummary(
	messageID string,
	summary *messageNodeCompactionSummary,
) {
	if store == nil || strings.TrimSpace(messageID) == "" || summary == nil {
		return
	}

	node, ok := store.get(messageID)
	if !ok || node == nil {
		return
	}

	node.mu.Lock()

	stored := summary
	if stored.applied {
		stored = &messageNodeCompactionSummary{
			text:    stored.text,
			anchor:  strings.TrimSpace(stored.anchor),
			applied: true,
		}
	}

	node.compactionSummary = stored
	store.cacheLockedNodeLocked(messageID, node)
	node.mu.Unlock()
}

func (store *messageNodeStore) evictExcess() {
	store.mu.Lock()

	excessCount := len(store.nodes) - store.capacity
	if excessCount <= 0 {
		store.mu.Unlock()

		return
	}

	messageIDs := make([]string, 0, len(store.nodes))
	for messageID := range store.nodes {
		messageIDs = append(messageIDs, messageID)
	}
	store.mu.Unlock()

	sortMessageIDs(messageIDs)

	deletedAny := false

	for _, messageID := range messageIDs[:excessCount] {
		if store.deleteWhenUnlocked(messageID) {
			deletedAny = true
		}
	}

	if deletedAny {
		store.persistBestEffort()
	}
}

func (store *messageNodeStore) deleteWhenUnlocked(messageID string) bool {
	store.mu.Lock()
	node, ok := store.nodes[messageID]
	store.mu.Unlock()

	if !ok {
		return false
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	deleted := false

	store.mu.Lock()
	if currentNode, currentNodeExists := store.nodes[messageID]; currentNodeExists && currentNode == node {
		delete(store.nodes, messageID)

		deleted = true
	}
	store.mu.Unlock()

	if deleted {
		store.deleteCachedSnapshot(messageID)
	}

	return deleted
}

func sortMessageIDs(messageIDs []string) {
	slices.SortFunc(messageIDs, compareMessageIDs)
}

func compareMessageIDs(left, right string) int {
	if len(left) == len(right) {
		return cmp.Compare(left, right)
	}

	leftValue, leftErr := strconv.ParseUint(left, 10, 64)
	rightValue, rightErr := strconv.ParseUint(right, 10, 64)

	if leftErr == nil && rightErr == nil {
		return cmp.Compare(leftValue, rightValue)
	}

	return cmp.Compare(left, right)
}
