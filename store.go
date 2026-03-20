package main

import (
	"cmp"
	"slices"
	"strconv"
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
	rentryURL                string
	media                    []contentPart
	searchMetadata           *searchMetadata
	hasBadAttachments        bool
	attachmentDownloadFailed bool
	fetchParentFailed        bool
	parentMessage            *discordgo.Message
	initialized              bool
	mu                       sync.Mutex
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

func compareMessageIDs(left string, right string) int {
	leftValue, leftErr := strconv.ParseUint(left, 10, 64)

	rightValue, rightErr := strconv.ParseUint(right, 10, 64)

	if leftErr == nil && rightErr == nil {
		return cmp.Compare(leftValue, rightValue)
	}

	return cmp.Compare(left, right)
}
