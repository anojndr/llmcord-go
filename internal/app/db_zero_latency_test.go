package app

import (
	"context"
	"sync"
	"testing"
	"time"
)

type blockingBotStateBackend struct {
	mu          sync.Mutex
	saveCalls   int
	saveStarted chan struct{}
	releaseSave chan struct{}
	snapshot    botStateSnapshot
	storeKey    string
}

func newBlockingBotStateBackend() *blockingBotStateBackend {
	return &blockingBotStateBackend{
		saveStarted: make(chan struct{}, 1),
		releaseSave: make(chan struct{}),
	}
}

func (backend *blockingBotStateBackend) loadBotState(storeKey string) (botStateSnapshot, error) {
	return botStateSnapshot{}, errTestBackendUnavailable
}

func (backend *blockingBotStateBackend) saveBotState(storeKey string, snapshot botStateSnapshot) error {
	backend.mu.Lock()
	backend.saveCalls++
	backend.snapshot = snapshot
	backend.storeKey = storeKey
	backend.mu.Unlock()

	select {
	case backend.saveStarted <- struct{}{}:
	default:
	}

	<-backend.releaseSave

	return nil
}

func TestPersistBotStateBestEffortDoesNotBlockCaller(t *testing.T) {
	t.Parallel()

	loadedConfig := testBotStateConfig()
	backend := newBlockingBotStateBackend()
	instance := newTestBotWithStateBackend(backend, loadedConfig)

	done := make(chan struct{})

	go func() {
		instance.setCurrentModel("google/gemini-3.6-flash")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("persistBotStateBestEffort blocked the caller on slow database")
	}

	select {
	case <-backend.saveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for background bot state save")
	}

	close(backend.releaseSave)
}

func TestNewBotDoesNotBlockStartupOnUnreachableDatabase(t *testing.T) {
	t.Parallel()

	loadedConfig := testBotStateConfig()
	loadedConfig.BotToken = "test-token"
	loadedConfig.Database.ConnectionString = "postgres://127.0.0.1:1/llmcord?sslmode=disable"
	loadedConfig.Database.StoreKey = "startup-latency-probe"

	done := make(chan *bot, 1)

	go func() {
		instance, err := newBot(context.Background(), "config.yaml", loadedConfig)
		if err != nil {
			t.Errorf("newBot with unreachable database: %v", err)

			done <- nil

			return
		}

		done <- instance
	}()

	var instance *bot

	select {
	case instance = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("newBot blocked startup on database")
	}

	if instance == nil {
		t.Fatal("expected bot instance despite unreachable database")
	}

	node := instance.nodes.getOrCreate("live-message")
	if node == nil {
		t.Fatal("expected in-memory store usable before hydration")
	}

	instance.botClosed.Store(true)

	if instance.nodes != nil {
		instance.nodes.closed.Store(true)
	}
}

func TestAttachPersistentBackendMergesWithoutLosingLiveWrites(t *testing.T) {
	t.Parallel()

	store := newMessageNodeStore(10)
	cacheInitializedStoreNode(store, "live-1", "live text")

	loaded := map[string]messageNodeSnapshot{
		"live-1":   {Role: messageRoleUser, Text: "stale text", Initialized: true},
		"loaded-1": {Role: messageRoleUser, Text: "loaded text", Initialized: true},
	}
	backend := newTestMessageNodeStoreBackend()

	if !store.attachPersistentBackend("merge-store", backend, loaded) {
		t.Fatal("expected attach to install backend")
	}

	defer func() {
		_ = store.close()
	}()

	live, ok := store.get("live-1")
	if !ok || live == nil {
		t.Fatal("expected live node to survive attach")
	}

	live.mu.Lock()
	liveText := live.text
	live.mu.Unlock()

	if liveText != "live text" {
		t.Fatalf("live write lost during merge: %q", liveText)
	}

	if _, ok := store.get("loaded-1"); !ok {
		t.Fatal("expected loaded history to merge")
	}

	_, storeKey := store.backendAndKey()
	if storeKey != "merge-store" {
		t.Fatalf("unexpected store key after attach: %q", storeKey)
	}
}
