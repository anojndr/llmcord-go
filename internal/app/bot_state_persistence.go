package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

const (
	botStateSnapshotVersion = 1
	botStateTableName       = "bot_state_snapshots"
)

const (
	botStateSelectSQL = "SELECT version, snapshot FROM bot_state_snapshots WHERE store_key = $1"
	botStateUpsertSQL = "INSERT INTO bot_state_snapshots (store_key, version, snapshot, updated_at) " +
		"VALUES ($1, $2, $3, NOW()) " +
		"ON CONFLICT (store_key) DO UPDATE SET version = EXCLUDED.version, snapshot = EXCLUDED.snapshot, updated_at = NOW()"
	botStateCreateTableSQL = "CREATE TABLE IF NOT EXISTS bot_state_snapshots (" +
		"store_key TEXT PRIMARY KEY," +
		"version INTEGER NOT NULL," +
		"snapshot JSONB NOT NULL," +
		"updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()" +
		")"
)

// botStateSnapshot is the JSON payload persisted per store key. It captures
// every piece of bot runtime state that would otherwise be lost on restart:
// the operators-selected model, search type, and grounding toggle, plus the
// set of maintenance-locked channels.
type botStateSnapshot struct {
	Version             int      `json:"version"`
	CurrentModel        string   `json:"current_model,omitempty"`
	ExaSearchType       string   `json:"exa_search_type,omitempty"`
	GroundingEnabled    *bool    `json:"grounding_enabled,omitempty"`
	MaintenanceChannels []string `json:"maintenance_channels,omitempty"`
}

// botStateBackend persists bot runtime state. The postgres implementation
// shares the message-history *sql.DB connection owned by the message store,
// so it never closes the database itself.
type botStateBackend interface {
	loadBotState(storeKey string) (botStateSnapshot, error)
	saveBotState(storeKey string, snapshot botStateSnapshot) error
}

type postgresBotStateBackend struct {
	database *sql.DB
}

func newPostgresBotStateBackend(ctx context.Context, database *sql.DB) (*postgresBotStateBackend, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil bot state context: %w", os.ErrInvalid)
	}

	if database == nil {
		return nil, fmt.Errorf("nil bot state database: %w", os.ErrInvalid)
	}

	err := ensureBotStateTable(ctx, database)
	if err != nil {
		return nil, err
	}

	backend := new(postgresBotStateBackend)
	backend.database = database

	return backend, nil
}

func ensureBotStateTable(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return fmt.Errorf("nil bot state database: %w", os.ErrInvalid)
	}

	_, err := database.ExecContext(ctx, botStateCreateTableSQL)
	if err != nil {
		return fmt.Errorf("create postgres bot state table %q: %w", botStateTableName, err)
	}

	return nil
}

func (backend *postgresBotStateBackend) loadBotState(storeKey string) (botStateSnapshot, error) {
	var snapshot botStateSnapshot

	var snapshotBytes []byte

	loadCtx, cancelLoad := context.WithTimeout(context.Background(), postgresMessageNodeStoreStatementTimeout)
	defer cancelLoad()

	err := backend.database.QueryRowContext(
		loadCtx,
		botStateSelectSQL,
		storeKey,
	).Scan(
		&snapshot.Version,
		&snapshotBytes,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return botStateSnapshot{}, os.ErrNotExist
		}

		return botStateSnapshot{}, fmt.Errorf(
			"query bot state from postgres table %q: %w",
			botStateTableName,
			err,
		)
	}

	if snapshot.Version != botStateSnapshotVersion {
		return botStateSnapshot{}, fmt.Errorf(
			"unsupported bot state version %d: %w",
			snapshot.Version,
			os.ErrInvalid,
		)
	}

	err = json.Unmarshal(snapshotBytes, &snapshot)
	if err != nil {
		return botStateSnapshot{}, fmt.Errorf("decode bot state snapshot JSON: %w", err)
	}

	snapshot.Version = botStateSnapshotVersion

	return snapshot, nil
}

func (backend *postgresBotStateBackend) saveBotState(storeKey string, snapshot botStateSnapshot) error {
	snapshot.Version = botStateSnapshotVersion

	snapshotBytes, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode bot state snapshot JSON: %w", err)
	}

	saveCtx, cancelSave := context.WithTimeout(context.Background(), postgresMessageNodeStoreStatementTimeout)
	defer cancelSave()

	_, err = backend.database.ExecContext(
		saveCtx,
		botStateUpsertSQL,
		storeKey,
		snapshot.Version,
		snapshotBytes,
	)
	if err != nil {
		return fmt.Errorf(
			"upsert bot state into postgres table %q: %w",
			botStateTableName,
			err,
		)
	}

	return nil
}

// postgresDatabase returns the shared postgres handle when the message store
// is persistent, or nil when history persistence is disabled.
func (store *messageNodeStore) postgresDatabase() *sql.DB {
	backend, _ := store.backendAndKey()
	if backend == nil {
		return nil
	}

	postgresBackend, ok := backend.(*postgresMessageNodeStoreBackend)
	if !ok || postgresBackend == nil {
		return nil
	}

	return postgresBackend.database
}

// snapshotBotState copies the restart-relevant runtime state under read
// locks. Callers must not hold modelMu or maintenanceMu.
func (instance *bot) snapshotBotState() botStateSnapshot {
	snapshot := new(botStateSnapshot)
	snapshot.Version = botStateSnapshotVersion

	if instance == nil {
		return *snapshot
	}

	instance.modelMu.RLock()
	snapshot.CurrentModel = instance.currentModel
	snapshot.ExaSearchType = instance.currentExaSearchTypeValue

	if instance.currentGroundingEnabledValue != nil {
		enabled := *instance.currentGroundingEnabledValue
		snapshot.GroundingEnabled = &enabled
	}

	instance.modelMu.RUnlock()

	instance.maintenanceMu.RLock()

	if len(instance.maintenanceChannels) > 0 {
		channels := make([]string, 0, len(instance.maintenanceChannels))

		for channelID := range instance.maintenanceChannels {
			if strings.TrimSpace(channelID) == "" {
				continue
			}

			channels = append(channels, channelID)
		}

		slices.Sort(channels)
		snapshot.MaintenanceChannels = channels
	}

	instance.maintenanceMu.RUnlock()

	return *snapshot
}

// applyBotStateSnapshot restores persisted runtime state, validating each
// field so a stale snapshot can never select an unknown model or search
// type. Unknown values keep the startup defaults.
func (instance *bot) applyBotStateSnapshot(snapshot botStateSnapshot, loadedConfig config) {
	if instance == nil {
		return
	}

	if strings.TrimSpace(snapshot.CurrentModel) != "" && loadedConfig.hasModel(snapshot.CurrentModel) {
		instance.modelMu.Lock()
		instance.currentModel = snapshot.CurrentModel
		instance.modelMu.Unlock()
	}

	if normalizedSearchType, ok := normalizeExaSearchType(snapshot.ExaSearchType); ok {
		instance.modelMu.Lock()
		instance.currentExaSearchTypeValue = normalizedSearchType
		instance.modelMu.Unlock()
	}

	if snapshot.GroundingEnabled != nil {
		enabled := *snapshot.GroundingEnabled

		instance.modelMu.Lock()
		instance.currentGroundingEnabledValue = &enabled
		instance.modelMu.Unlock()
	}

	if len(snapshot.MaintenanceChannels) > 0 {
		restored := make(map[string]struct{}, len(snapshot.MaintenanceChannels))

		for _, channelID := range snapshot.MaintenanceChannels {
			if strings.TrimSpace(channelID) == "" {
				continue
			}

			restored[channelID] = struct{}{}
		}

		if len(restored) > 0 {
			instance.maintenanceMu.Lock()
			instance.maintenanceChannels = restored
			instance.maintenanceMu.Unlock()
		}
	}
}

// loadPersistedBotState hydrates runtime state from postgres. Missing rows
// (first run) are a no-op; other failures are logged and keep defaults.
func (instance *bot) loadPersistedBotState(loadedConfig config) {
	backend, storeKey := instance.botStateBackendAndKey()
	if instance == nil || backend == nil || strings.TrimSpace(storeKey) == "" {
		return
	}

	generationBeforeLoad := instance.botStateGeneration.Load()

	snapshot, err := backend.loadBotState(storeKey)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}

		logWarn("load persisted bot state", err, "store_key", storeKey)

		return
	}

	if instance.botStateGeneration.Load() != generationBeforeLoad {
		instance.persistBotStateBestEffort()

		return
	}

	instance.applyBotStateSnapshot(snapshot, loadedConfig)
}

func (instance *bot) botStateBackendAndKey() (botStateBackend, string) {
	if instance == nil {
		return nil, ""
	}

	instance.botStateMu.RLock()
	defer instance.botStateMu.RUnlock()

	return instance.botStateBackend, instance.botStateKey
}

// persistBotStateBestEffort saves runtime state without blocking the caller.
// It flushes on a background goroutine, so Discord handlers never wait on
// postgres latency or outages. The snapshot is taken inside the serialized
// save, so concurrent mutations converge on the latest state.
func (instance *bot) persistBotStateBestEffort() {
	backend, storeKey := instance.botStateBackendAndKey()
	if instance == nil || backend == nil || strings.TrimSpace(storeKey) == "" {
		return
	}

	safeGo(func() {
		instance.botStateSaveMu.Lock()
		defer instance.botStateSaveMu.Unlock()

		if instance.botClosed.Load() {
			return
		}

		snapshot := instance.snapshotBotState()

		if err := backend.saveBotState(storeKey, snapshot); err != nil {
			logWarn("persist bot state", err, "store_key", storeKey)
		}
	})
}

// persistBotStateSync saves runtime state and reports the error for shutdown
// flushing. A nil backend (persistence disabled) is a successful no-op.
func (instance *bot) persistBotStateSync() error {
	backend, storeKey := instance.botStateBackendAndKey()
	if instance == nil || backend == nil || strings.TrimSpace(storeKey) == "" {
		return nil
	}

	instance.botStateSaveMu.Lock()
	defer instance.botStateSaveMu.Unlock()

	snapshot := instance.snapshotBotState()

	err := backend.saveBotState(storeKey, snapshot)
	if err != nil {
		return fmt.Errorf("persist bot state for store key %q: %w", storeKey, err)
	}

	return nil
}

// wireBotStatePersistence shares the message-history postgres connection for
// runtime-state persistence and hydrates the current bot settings. Without a
// persistent message store, bot state stays in memory.
func (instance *bot) wireBotStatePersistence(ctx context.Context, loadedConfig config) {
	if instance == nil || instance.nodes == nil {
		return
	}

	_, rawStoreKey := instance.nodes.backendAndKey()
	storeKey := strings.TrimSpace(rawStoreKey)

	if storeKey == "" {
		return
	}

	database := instance.nodes.postgresDatabase()
	if database == nil {
		return
	}

	generationBeforeLoad := instance.botStateGeneration.Load()

	backend, err := newPostgresBotStateBackend(ctx, database)
	if err != nil {
		logWarn("configure persisted bot state", err, "store_key", storeKey)

		return
	}

	if instance.botClosed.Load() {
		if instance.botStateGeneration.Load() != generationBeforeLoad {
			instance.botStateSaveMu.Lock()
			snapshot := instance.snapshotBotState()

			if err := backend.saveBotState(storeKey, snapshot); err != nil {
				logWarn("persist bot state at shutdown", err, "store_key", storeKey)
			}

			instance.botStateSaveMu.Unlock()
		}

		return
	}

	instance.botStateMu.Lock()
	instance.botStateKey = storeKey
	instance.botStateBackend = backend
	instance.botStateMu.Unlock()

	if instance.botStateGeneration.Load() != generationBeforeLoad {
		instance.persistBotStateBestEffort()

		return
	}

	instance.loadPersistedBotState(loadedConfig)
}
