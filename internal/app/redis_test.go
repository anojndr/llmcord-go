package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeRedisTestConfig(t *testing.T, snippet string) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configText := "bot_token: discord-token\n" +
		"providers:\n" +
		"  openai:\n" +
		"    base_url: https://api.example.com/v1\n" +
		"models:\n" +
		"  openai/first-model:\n" +
		snippet +
		"\n"

	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	return configPath
}

type stubRedisClient struct {
	mu       sync.Mutex
	strings  map[string]stubRedisString
	hashes   map[string]map[string]string
	setNXErr error
	getErr   error
	setErr   error
	hGetErr  error
	hSetErr  error
	setNXOK  bool
	setNXSet bool
	closed   bool
}

type stubRedisString struct {
	value     string
	expiresAt time.Time
}

func newStubRedisClient() *stubRedisClient {
	return &stubRedisClient{
		strings: make(map[string]stubRedisString),
		hashes:  make(map[string]map[string]string),
	}
}

func (client *stubRedisClient) ping(_ context.Context) error {
	return nil
}

func (client *stubRedisClient) setNX(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.setNXErr != nil {
		return false, client.setNXErr
	}

	if client.setNXSet {
		return client.setNXOK, nil
	}

	if _, exists := client.strings[key]; exists {
		return false, nil
	}

	client.strings[key] = stubRedisString{value: value}

	return true, nil
}

func (client *stubRedisClient) get(_ context.Context, key string) (string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.getErr != nil {
		return "", client.getErr
	}

	entry, ok := client.strings[key]
	if !ok {
		return "", os.ErrNotExist
	}

	return entry.value, nil
}

func (client *stubRedisClient) set(_ context.Context, key, value string, ttl time.Duration) error {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.setErr != nil {
		return client.setErr
	}

	expiresAt := time.Time{}
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}

	client.strings[key] = stubRedisString{value: value, expiresAt: expiresAt}

	return nil
}

func (client *stubRedisClient) hGetAll(_ context.Context, key string) (map[string]string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.hGetErr != nil {
		return nil, client.hGetErr
	}

	fields, ok := client.hashes[key]
	if !ok {
		return map[string]string{}, nil
	}

	out := make(map[string]string, len(fields))
	for field, value := range fields {
		out[field] = value
	}

	return out, nil
}

func (client *stubRedisClient) hSet(_ context.Context, key string, fields map[string]string) error {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.hSetErr != nil {
		return client.hSetErr
	}

	existing, ok := client.hashes[key]
	if !ok {
		existing = make(map[string]string, len(fields))
		client.hashes[key] = existing
	}

	for field, value := range fields {
		existing[field] = value
	}

	return nil
}

func (client *stubRedisClient) expire(_ context.Context, key string, ttl time.Duration) error {
	client.mu.Lock()
	defer client.mu.Unlock()

	if entry, ok := client.strings[key]; ok {
		if ttl > 0 {
			entry.expiresAt = time.Now().Add(ttl)
			client.strings[key] = entry
		}

		return nil
	}

	return nil
}

func (client *stubRedisClient) del(_ context.Context, keys ...string) error {
	client.mu.Lock()
	defer client.mu.Unlock()

	for _, key := range keys {
		delete(client.strings, key)
		delete(client.hashes, key)
	}

	return nil
}

func (client *stubRedisClient) close() error {
	client.mu.Lock()
	defer client.mu.Unlock()

	client.closed = true

	return nil
}

func TestValidateRedisConfigDefaultsDisabled(t *testing.T) {
	t.Parallel()

	if err := validateRedisConfig(redisConfig{}); err != nil {
		t.Fatalf("disabled redis config: %v", err)
	}
}

func TestValidateRedisConfigRejectsURLAddress(t *testing.T) {
	t.Parallel()

	cfg := redisConfig{Address: "redis://localhost:6379", KeyPrefix: defaultRedisKeyPrefix}
	if err := validateRedisConfig(cfg); err == nil {
		t.Fatal("expected URL address to fail validation")
	}
}

func TestValidateRedisConfigRejectsBadPort(t *testing.T) {
	t.Parallel()

	for _, address := range []string{"localhost", "localhost:0", "localhost:99999", "localhost:not-a-port"} {
		cfg := redisConfig{Address: address, KeyPrefix: defaultRedisKeyPrefix}
		if err := validateRedisConfig(cfg); err == nil {
			t.Fatalf("expected address %q to fail validation", address)
		}
	}
}

func TestValidateRedisConfigRejectsBadDB(t *testing.T) {
	t.Parallel()

	cfg := redisConfig{Address: "127.0.0.1:6379", DB: redisMaxDatabase + 1, KeyPrefix: defaultRedisKeyPrefix}
	if err := validateRedisConfig(cfg); err == nil {
		t.Fatal("expected out-of-range db to fail validation")
	}
}

func TestNormalizeRedisConfigAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg := normalizeRedisConfig(rawRedisConfig{Address: " 127.0.0.1:6379 "})
	if cfg.Address != "127.0.0.1:6379" {
		t.Fatalf("unexpected address: %q", cfg.Address)
	}

	if cfg.DB != defaultRedisDatabase {
		t.Fatalf("unexpected db: %d", cfg.DB)
	}

	if cfg.KeyPrefix != defaultRedisKeyPrefix {
		t.Fatalf("unexpected prefix: %q", cfg.KeyPrefix)
	}
}

func TestNormalizeRedisPrefix(t *testing.T) {
	t.Parallel()

	if got := normalizeRedisPrefix("llmcord:"); got != "llmcord" {
		t.Fatalf("unexpected prefix: %q", got)
	}

	if got := normalizeRedisPrefix(""); got != defaultRedisKeyPrefix {
		t.Fatalf("unexpected default prefix: %q", got)
	}
}

func TestLoadConfigParsesRedis(t *testing.T) {
	t.Parallel()

	configPath := writeRedisTestConfig(t, "redis:\n  address: 127.0.0.1:6379\n  db: 2\n  key_prefix: testbot\n")

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loadedConfig.Redis.Address != "127.0.0.1:6379" {
		t.Fatalf("unexpected redis address: %q", loadedConfig.Redis.Address)
	}

	if loadedConfig.Redis.DB != 2 {
		t.Fatalf("unexpected redis db: %d", loadedConfig.Redis.DB)
	}

	if loadedConfig.Redis.KeyPrefix != "testbot" {
		t.Fatalf("unexpected redis prefix: %q", loadedConfig.Redis.KeyPrefix)
	}
}

func TestLoadConfigRejectsInvalidRedis(t *testing.T) {
	t.Parallel()

	configPath := writeRedisTestConfig(t, "redis:\n  address: redis://localhost:6379\n")

	if _, err := loadConfig(configPath); err == nil {
		t.Fatal("expected invalid redis address to fail validation")
	}
}

func TestRedisDedupReserveFirstWins(t *testing.T) {
	t.Parallel()

	client := newStubRedisClient()

	first, err := reserveMessageDedup(context.Background(), client, "llmcord", "message-1")
	if err != nil || !first {
		t.Fatalf("first reserve = %v, %v; want true, nil", first, err)
	}

	second, err := reserveMessageDedup(context.Background(), client, "llmcord", "message-1")
	if err != nil || second {
		t.Fatalf("second reserve = %v, %v; want false, nil", second, err)
	}
}

func TestReserveMessageDedupErrorSurfaces(t *testing.T) {
	t.Parallel()

	client := newStubRedisClient()
	client.setNXErr = errTestBackendUnavailable

	if _, err := reserveMessageDedup(context.Background(), client, "llmcord", "message-1"); !errors.Is(err, errTestBackendUnavailable) {
		t.Fatalf("reserve error = %v, want backend unavailable", err)
	}
}

func TestMarkMessageSeenFallsBackWhenRedisFails(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.redisDedupClient = newStubRedisClient()
	instance.redisDedupClient.(*stubRedisClient).setNXErr = errTestBackendUnavailable
	instance.redisDedupPrefix = "llmcord"

	if !instance.markMessageSeen("message-1") {
		t.Fatal("expected redis error to fall back to local dedup")
	}

	if instance.markMessageSeen("message-1") {
		t.Fatal("expected local dedup to reject the repeat after fallback")
	}
}

func TestMarkMessageSeenRejectsClusterDuplicate(t *testing.T) {
	t.Parallel()

	client := newStubRedisClient()
	first := new(bot)
	first.redisDedupClient = client
	first.redisDedupPrefix = "llmcord"
	second := new(bot)
	second.redisDedupClient = client
	second.redisDedupPrefix = "llmcord"

	if !first.markMessageSeen("shared-message") {
		t.Fatal("expected first instance to claim the message")
	}

	if second.markMessageSeen("shared-message") {
		t.Fatal("expected second instance to lose the cluster claim")
	}
}

func TestTinyFishFetchCacheSharesThroughRedis(t *testing.T) {
	t.Parallel()

	client := newStubRedisClient()
	writer := newTinyFishFetchCacheWithRedis(client, "llmcord")
	writer.store("https://example.com/a", "https://example.com/a-final", "full body", "Example", "desc")

	reader := newTinyFishFetchCacheWithRedis(client, "llmcord")

	text, title, _, _, _, ok := reader.lookup("https://example.com/a")
	if !ok || text != "full body" || title != "Example" {
		t.Fatalf("redis lookup = %q %q %v, want shared entry", text, title, ok)
	}
}

func TestTinyFishFetchCacheFallsBackWhenRedisFails(t *testing.T) {
	t.Parallel()

	client := newStubRedisClient()
	client.hGetErr = errTestBackendUnavailable
	cache := newTinyFishFetchCacheWithRedis(client, "llmcord")

	if _, _, _, _, _, ok := cache.lookup("https://example.com/a"); ok {
		t.Fatal("expected redis error to be a cache miss")
	}

	client.hSetErr = errTestBackendUnavailable

	cache.store("https://example.com/a", "", "body", "", "")
	_, _, _, _, _, memOK := cache.lookupMemory("https://example.com/a")

	if !memOK {
		t.Fatal("expected memory store to survive a redis failure")
	}
}

func TestRedisHistoryBackendRoundTrip(t *testing.T) {
	t.Parallel()

	client := newStubRedisClient()
	backend := newRedisMessageNodeStoreBackend(client, "llmcord")

	snapshot := messageNodeStoreSnapshot{
		Version: messageNodeStoreSnapshotVersion,
		Nodes: map[string]messageNodeSnapshot{
			"message-1": {Role: messageRoleUser, Text: "hello", Initialized: true},
		},
	}

	if err := backend.saveSnapshot(testSharedHomeBotsStoreKey, snapshot); err != nil {
		t.Fatalf("save redis snapshot: %v", err)
	}

	loaded, err := backend.loadSnapshot(testSharedHomeBotsStoreKey, 10)
	if err != nil {
		t.Fatalf("load redis snapshot: %v", err)
	}

	node, ok := loaded.Nodes["message-1"]
	if !ok || node.Text != "hello" {
		t.Fatalf("unexpected loaded nodes: %#v", loaded.Nodes)
	}
}

func TestRedisHistoryBackendMissingIsNotExist(t *testing.T) {
	t.Parallel()

	backend := newRedisMessageNodeStoreBackend(newStubRedisClient(), "llmcord")

	if _, err := backend.loadSnapshot("missing", 10); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("load missing = %v, want not exist", err)
	}
}

func TestRedisChainedBackendWritesBothTiers(t *testing.T) {
	t.Parallel()

	redisBackend := newRedisMessageNodeStoreBackend(newStubRedisClient(), "llmcord")
	sqliteBackend := newTestMessageNodeStoreBackend()
	chained := newRedisChainedHistoryBackend(redisBackend, sqliteBackend)

	snapshot := messageNodeStoreSnapshot{
		Version: messageNodeStoreSnapshotVersion,
		Nodes: map[string]messageNodeSnapshot{
			"message-1": {Role: messageRoleUser, Text: "hello", Initialized: true},
		},
	}

	if err := chained.saveSnapshot(testSharedHomeBotsStoreKey, snapshot); err != nil {
		t.Fatalf("save chained snapshot: %v", err)
	}

	if _, err := sqliteBackend.loadSnapshot(testSharedHomeBotsStoreKey, 10); err != nil {
		t.Fatalf("sqlite tier missing chained write: %v", err)
	}

	loaded, err := redisBackend.loadSnapshot(testSharedHomeBotsStoreKey, 10)
	if err != nil {
		t.Fatalf("redis tier missing chained write: %v", err)
	}

	if loaded.Nodes["message-1"].Text != "hello" {
		t.Fatalf("unexpected chained redis nodes: %#v", loaded.Nodes)
	}
}

func TestRedisBotStateBackendRoundTrip(t *testing.T) {
	t.Parallel()

	backend := newRedisBotStateBackend(newStubRedisClient(), "llmcord")
	snapshot := botStateSnapshot{
		Version:      botStateSnapshotVersion,
		CurrentModel: "openai/gpt-5.4",
	}

	if err := backend.saveBotState(testSharedHomeBotsStoreKey, snapshot); err != nil {
		t.Fatalf("save redis bot state: %v", err)
	}

	loaded, err := backend.loadBotState(testSharedHomeBotsStoreKey)
	if err != nil {
		t.Fatalf("load redis bot state: %v", err)
	}

	if loaded.CurrentModel != "openai/gpt-5.4" {
		t.Fatalf("unexpected bot state: %#v", loaded)
	}
}

func TestWireRedisBotStateRestoresSnapshot(t *testing.T) {
	t.Parallel()

	loadedConfig := testBotStateConfig()
	instance := new(bot)
	instance.currentModel = loadedConfig.firstModel()
	instance.currentExaSearchTypeValue = defaultExaSearchType
	instance.maintenanceChannels = make(map[string]struct{})
	instance.configPath = writeRedisTestConfig(t, "bot_token: discord-token\nproviders:\n  openai:\n    base_url: https://api.example.com/v1\nmodels:\n  openai/gpt-5.4:\n  google/gemini-3.6-flash:\n")
	instance.seedConfigCache(loadedConfig)
	instance.nodes = newMessageNodeStore(10)
	instance.redisClient = newStubRedisClient()
	instance.redisPrefix = "llmcord"

	redisBackend := newRedisBotStateBackend(instance.redisClient, instance.redisPrefix)
	if err := redisBackend.saveBotState(testSharedHomeBotsStoreKey, botStateSnapshot{CurrentModel: "google/gemini-3.6-flash"}); err != nil {
		t.Fatalf("seed redis bot state: %v", err)
	}

	instance.wireRedisBotState(testSharedHomeBotsStoreKey)

	if instance.currentModel != "google/gemini-3.6-flash" {
		t.Fatalf("unexpected restored model: %q", instance.currentModel)
	}
}

func TestServiceHealthReportsRedis(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.redisConfigured.Store(true)
	instance.redisClient = newStubRedisClient()
	instance.redisReachable.Store(true)

	health := instance.serviceHealth()
	if !health.RedisConfigured || !health.RedisReachable {
		t.Fatalf("unexpected redis health: %#v", health)
	}
}

func TestServiceHealthMarksRedisDownOnPingFailure(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.redisConfigured.Store(true)
	instance.redisClient = &failingPingRedisClient{err: errTestBackendUnavailable}
	instance.redisReachable.Store(true)

	health := instance.serviceHealth()
	if !health.RedisConfigured || health.RedisReachable {
		t.Fatalf("unexpected redis health: %#v", health)
	}

	if instance.redisReachable.Load() {
		t.Fatal("expected failed ping to clear reachability")
	}
}

type failingPingRedisClient struct {
	stubRedisClient
	err error
}

func (client *failingPingRedisClient) ping(_ context.Context) error {
	return client.err
}

func TestRedisKeysUseColonSeparation(t *testing.T) {
	t.Parallel()

	if got := redisKey("llmcord:", ":dedup:", "message-1"); got != "llmcord:dedup:message-1" {
		t.Fatalf("unexpected key: %q", got)
	}

	history := redisHistoryKey("llmcord", testSharedHomeBotsStoreKey)
	if !strings.HasPrefix(history, "llmcord:history:") {
		t.Fatalf("unexpected history key: %q", history)
	}

	botState := redisBotStateKey("llmcord", testSharedHomeBotsStoreKey)
	if !strings.HasPrefix(botState, "llmcord:botstate:") {
		t.Fatalf("unexpected bot state key: %q", botState)
	}

	first := redisFetchKey("llmcord", "https://example.com/a")

	second := redisFetchKey("llmcord", "https://example.com/a")
	if first != second || !strings.HasPrefix(first, "llmcord:fetch:") {
		t.Fatalf("unexpected fetch keys: %q %q", first, second)
	}
}

func TestReserveMessageSeenClusterNilBotIsSafe(t *testing.T) {
	t.Parallel()

	var instance *bot

	if !instance.reserveMessageSeenCluster("message-1") {
		t.Fatal("expected nil bot to fall back to local dedup")
	}
}

func TestWireRedisPersistenceAfterSQLiteAttachKeepsBothTiers(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.redisClient = newStubRedisClient()
	instance.redisPrefix = "llmcord"
	instance.configPath = writeRedisTestConfig(t, "bot_token: discord-token\nproviders:\n  openai:\n    base_url: https://api.example.com/v1\nmodels:\n  openai/gpt-5.4:\n  google/gemini-3.6-flash:\n")
	instance.seedConfigCache(testBotStateConfig())

	sqliteBackend := newTestMessageNodeStoreBackend()
	if !instance.nodes.attachPersistentBackend(testSharedHomeBotsStoreKey, sqliteBackend, map[string]messageNodeSnapshot{
		"sqlite-1": {Role: messageRoleUser, Text: "sqlite row", Initialized: true},
	}) {
		t.Fatal("expected sqlite attach to install backend")
	}

	redisBackend := instance.redisHistoryBackend()
	if err := redisBackend.saveSnapshot(testSharedHomeBotsStoreKey, messageNodeStoreSnapshot{
		Version: messageNodeStoreSnapshotVersion,
		Nodes: map[string]messageNodeSnapshot{
			"sqlite-1": {Role: messageRoleUser, Text: "sqlite row", Initialized: true},
			"redis-1":  {Role: messageRoleUser, Text: "redis row", Initialized: true},
		},
	}); err != nil {
		t.Fatalf("seed redis snapshot: %v", err)
	}

	instance.wireRedisPersistence(testSharedHomeBotsStoreKey)
	backend, _ := instance.nodes.backendAndKey()

	chained, ok := backend.(*redisChainedHistoryBackend)
	if !ok || chained == nil {
		t.Fatalf("expected chained backend, got %T", backend)
	}

	if chained.other != sqliteBackend {
		t.Fatal("expected sqlite backend preserved as chained sibling")
	}

	loaded, err := chained.loadSnapshot(testSharedHomeBotsStoreKey, 10)
	if err != nil {
		t.Fatalf("load chained snapshot: %v", err)
	}

	if loaded.Nodes["sqlite-1"].Text != "sqlite row" {
		t.Fatalf("chained load lost sqlite tier: %#v", loaded.Nodes)
	}
}
