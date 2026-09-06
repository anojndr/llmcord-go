package app

import (
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

type testBotStateBackend struct {
	mu        sync.Mutex
	snapshots map[string]botStateSnapshot
	loadErr   error
	saveErr   error
	saveCalls int
}

func newTestBotStateBackend() *testBotStateBackend {
	backend := new(testBotStateBackend)
	backend.snapshots = make(map[string]botStateSnapshot)

	return backend
}

func (backend *testBotStateBackend) loadBotState(storeKey string) (botStateSnapshot, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()

	if backend.loadErr != nil {
		return botStateSnapshot{}, backend.loadErr
	}

	snapshot, ok := backend.snapshots[storeKey]
	if !ok {
		return botStateSnapshot{}, os.ErrNotExist
	}

	return snapshot, nil
}

func (backend *testBotStateBackend) saveBotState(storeKey string, snapshot botStateSnapshot) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()

	backend.saveCalls++

	if backend.saveErr != nil {
		return backend.saveErr
	}

	backend.snapshots[storeKey] = snapshot

	return nil
}

func (backend *testBotStateBackend) savedSnapshot() (botStateSnapshot, bool) {
	backend.mu.Lock()
	defer backend.mu.Unlock()

	snapshot, ok := backend.snapshots[testSharedHomeBotsStoreKey]

	return snapshot, ok
}

func testBotStateConfig() config {
	return config{
		Models: map[string]map[string]any{
			"openai/gpt-5.4":          {},
			"google/gemini-3.6-flash": {},
		},
		ModelOrder: []string{"openai/gpt-5.4", "google/gemini-3.6-flash"},
	}
}

func newTestBotWithStateBackend(backend botStateBackend, loadedConfig config) *bot {
	instance := new(bot)
	instance.currentModel = loadedConfig.firstModel()
	instance.currentExaSearchTypeValue = defaultExaSearchType
	instance.maintenanceChannels = make(map[string]struct{})
	instance.botStateKey = testSharedHomeBotsStoreKey
	instance.botStateBackend = backend

	return instance
}

func TestBotStateSettersPersistAcrossRestart(t *testing.T) {
	t.Parallel()

	loadedConfig := testBotStateConfig()
	backend := newTestBotStateBackend()
	instance := newTestBotWithStateBackend(backend, loadedConfig)

	instance.setCurrentModel("google/gemini-3.6-flash")
	instance.setCurrentExaSearchType("deep")

	grounding := true
	instance.setCurrentGroundingEnabled(&grounding)
	instance.setMaintenanceChannel("channel-123")

	saved, ok := backend.savedSnapshot()
	if !ok {
		t.Fatal("expected bot state snapshot after setters")
	}

	if saved.CurrentModel != "google/gemini-3.6-flash" {
		t.Fatalf("unexpected persisted model: %q", saved.CurrentModel)
	}

	if saved.ExaSearchType != "deep" {
		t.Fatalf("unexpected persisted search type: %q", saved.ExaSearchType)
	}

	if saved.GroundingEnabled == nil || !*saved.GroundingEnabled {
		t.Fatalf("unexpected persisted grounding: %#v", saved.GroundingEnabled)
	}

	if !slices.Contains(saved.MaintenanceChannels, "channel-123") {
		t.Fatalf("unexpected persisted maintenance channels: %q", saved.MaintenanceChannels)
	}

	restarted := newTestBotWithStateBackend(backend, loadedConfig)
	restarted.loadPersistedBotState(loadedConfig)

	if restarted.currentModel != "google/gemini-3.6-flash" {
		t.Fatalf("restart lost model: %q", restarted.currentModel)
	}

	if restarted.currentExaSearchType() != "deep" {
		t.Fatalf("restart lost search type: %q", restarted.currentExaSearchType())
	}

	provider := providerConfig{EnableGrounding: false}
	if !restarted.currentGroundingEnabled(provider) {
		t.Fatal("restart lost grounding toggle")
	}

	if !restarted.isMaintenanceChannel("channel-123") {
		t.Fatal("restart lost maintenance channel")
	}
}

func TestBotStateClearMaintenancePersists(t *testing.T) {
	t.Parallel()

	loadedConfig := testBotStateConfig()
	backend := newTestBotStateBackend()
	instance := newTestBotWithStateBackend(backend, loadedConfig)

	instance.setMaintenanceChannel("channel-123")
	instance.clearMaintenanceChannel("channel-123")

	saved, ok := backend.savedSnapshot()
	if !ok {
		t.Fatal("expected bot state snapshot after clear")
	}

	if slices.Contains(saved.MaintenanceChannels, "channel-123") {
		t.Fatalf("cleared channel still persisted: %q", saved.MaintenanceChannels)
	}

	restarted := newTestBotWithStateBackend(backend, loadedConfig)
	restarted.loadPersistedBotState(loadedConfig)

	if restarted.isMaintenanceChannel("channel-123") {
		t.Fatal("restart resurrected cleared maintenance channel")
	}
}

func TestBotStateApplyRejectsUnknownValues(t *testing.T) {
	t.Parallel()

	loadedConfig := testBotStateConfig()
	backend := newTestBotStateBackend()
	instance := newTestBotWithStateBackend(backend, loadedConfig)

	grounding := true
	backend.snapshots[testSharedHomeBotsStoreKey] = botStateSnapshot{
		Version:             botStateSnapshotVersion,
		CurrentModel:        "unknown/model",
		ExaSearchType:       "not-a-search-type",
		GroundingEnabled:    &grounding,
		MaintenanceChannels: []string{"", "  ", "channel-9"},
	}

	instance.loadPersistedBotState(loadedConfig)

	if instance.currentModel != loadedConfig.firstModel() {
		t.Fatalf("unknown persisted model overwrote default: %q", instance.currentModel)
	}

	if instance.currentExaSearchType() != defaultExaSearchType {
		t.Fatalf("unknown persisted search type overwrote default: %q", instance.currentExaSearchType())
	}

	provider := providerConfig{EnableGrounding: false}
	if !instance.currentGroundingEnabled(provider) {
		t.Fatal("persisted grounding toggle was not applied")
	}

	if !instance.isMaintenanceChannel("channel-9") {
		t.Fatal("valid maintenance channel was not restored")
	}
}

func TestBotStateLoadMissingKeepsDefaults(t *testing.T) {
	t.Parallel()

	loadedConfig := testBotStateConfig()
	backend := newTestBotStateBackend()
	instance := newTestBotWithStateBackend(backend, loadedConfig)

	instance.loadPersistedBotState(loadedConfig)

	if instance.currentModel != loadedConfig.firstModel() {
		t.Fatalf("missing snapshot changed model: %q", instance.currentModel)
	}

	if instance.isMaintenanceChannel("channel-123") {
		t.Fatal("missing snapshot created maintenance channel")
	}
}

func TestBotStateLoadErrorKeepsDefaults(t *testing.T) {
	t.Parallel()

	loadedConfig := testBotStateConfig()
	backend := newTestBotStateBackend()
	backend.loadErr = errTestBackendUnavailable
	instance := newTestBotWithStateBackend(backend, loadedConfig)

	instance.loadPersistedBotState(loadedConfig)

	if instance.currentModel != loadedConfig.firstModel() {
		t.Fatalf("failed load changed model: %q", instance.currentModel)
	}
}

func TestBotStatePersistSyncDisabledIsNoOp(t *testing.T) {
	t.Parallel()

	instance := new(bot)

	if err := instance.persistBotStateSync(); err != nil {
		t.Fatalf("disabled bot state persist: %v", err)
	}

	instance.persistBotStateBestEffort()
}

func TestBotStatePersistSyncSurfacesSaveError(t *testing.T) {
	t.Parallel()

	loadedConfig := testBotStateConfig()
	backend := newTestBotStateBackend()
	backend.saveErr = errTestBackendUnavailable
	instance := newTestBotWithStateBackend(backend, loadedConfig)

	if err := instance.persistBotStateSync(); err == nil {
		t.Fatal("expected persist error")
	}
}

func TestBotCloseFlushesBotState(t *testing.T) {
	t.Parallel()

	loadedConfig := testBotStateConfig()
	backend := newTestBotStateBackend()
	instance := newTestBotWithStateBackend(backend, loadedConfig)
	instance.nodes = newMessageNodeStore(10)
	instance.setCurrentModel("google/gemini-3.6-flash")

	session, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	instance.session = session

	if err := instance.close(); err != nil {
		t.Fatalf("bot close: %v", err)
	}

	saved, ok := backend.savedSnapshot()
	if !ok {
		t.Fatal("expected bot state snapshot saved during close")
	}

	if saved.CurrentModel != "google/gemini-3.6-flash" {
		t.Fatalf("unexpected saved model on close: %q", saved.CurrentModel)
	}
}

func TestBotCloseFlushesBotStateEvenWhenSessionCloseFails(t *testing.T) {
	t.Parallel()

	loadedConfig := testBotStateConfig()
	backend := newTestBotStateBackend()
	instance := newTestBotWithStateBackend(backend, loadedConfig)
	instance.nodes = newMessageNodeStore(10)
	instance.setCurrentModel("google/gemini-3.6-flash")
	instance.sessionClose = func(_ *discordgo.Session) error {
		return errTestBackendUnavailable
	}

	err := instance.close()
	if err == nil {
		t.Fatal("expected close error from session close failure")
	}

	saved, ok := backend.savedSnapshot()
	if !ok {
		t.Fatal("expected bot state snapshot saved during close even after session error")
	}

	if saved.CurrentModel != "google/gemini-3.6-flash" {
		t.Fatalf("unexpected saved model on close: %q", saved.CurrentModel)
	}
}
