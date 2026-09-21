package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// isRedisNilError reports a missing-key response from any redisClient
// implementation. Production returns redis.Nil; stubs surface os.ErrNotExist.
func isRedisNilError(err error) bool {
	return errors.Is(err, redis.Nil) || errors.Is(err, os.ErrNotExist)
}

func jsonMarshalBotState(snapshot botStateSnapshot) ([]byte, error) {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode bot state snapshot JSON: %w", err)
	}

	return payload, nil
}

func jsonUnmarshalBotState(payload []byte, snapshot *botStateSnapshot) error {
	if err := json.Unmarshal(payload, snapshot); err != nil {
		return fmt.Errorf("decode bot state snapshot JSON: %w", err)
	}

	snapshot.Version = botStateSnapshotVersion

	return nil
}

type rawRedisConfig struct {
	Address   scalarString `yaml:"address"`
	Password  scalarString `yaml:"password"`
	DB        *int         `yaml:"db"`
	KeyPrefix scalarString `yaml:"key_prefix"`
}

type redisConfig struct {
	Address   string
	Password  string
	DB        int
	KeyPrefix string
}

// enabled reports whether Redis is configured. A blank address disables every
// Redis path; the bot keeps its in-memory and SQLite behavior unchanged.
func (cfg redisConfig) enabled() bool {
	return strings.TrimSpace(cfg.Address) != ""
}

func normalizeRedisConfig(raw rawRedisConfig) redisConfig {
	return redisConfig{
		Address:   strings.TrimSpace(string(raw.Address)),
		Password:  strings.TrimSpace(string(raw.Password)),
		DB:        intValueOrDefault(raw.DB, defaultRedisDatabase),
		KeyPrefix: normalizeRedisPrefix(string(raw.KeyPrefix)),
	}
}

// normalizeRedisPrefix trims namespace separators so configured prefixes like
// "llmcord:" and "llmcord" produce identical keys. Blank falls back to the
// default prefix.
func normalizeRedisPrefix(prefix string) string {
	trimmed := strings.Trim(strings.TrimSpace(prefix), ":")
	if trimmed == "" {
		return defaultRedisKeyPrefix
	}

	return trimmed
}

func validateRedisConfig(cfg redisConfig) error {
	if !cfg.enabled() {
		return nil
	}

	if strings.Contains(strings.ToLower(cfg.Address), "://") {
		return fmt.Errorf("redis.address must be host:port, not a URL: %w", os.ErrInvalid)
	}

	host, portStr, err := net.SplitHostPort(cfg.Address)
	if err != nil || strings.TrimSpace(host) == "" {
		return fmt.Errorf("redis.address %q must be host:port like 127.0.0.1:6379: %w", cfg.Address, os.ErrInvalid)
	}

	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("redis.address %q must use a port between 1 and 65535: %w", cfg.Address, os.ErrInvalid)
	}

	if cfg.DB < 0 || cfg.DB > redisMaxDatabase {
		return fmt.Errorf("redis.db must be between 0 and %d: %w", redisMaxDatabase, os.ErrInvalid)
	}

	if strings.TrimSpace(cfg.KeyPrefix) == "" {
		return fmt.Errorf("redis.key_prefix must not be only whitespace: %w", os.ErrInvalid)
	}

	return nil
}

// redisClient is the narrow command surface the bot needs. goRedisClient is
// the production adapter; tests inject a hand-rolled stub.
type redisClient interface {
	ping(ctx context.Context) error
	setNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	get(ctx context.Context, key string) (string, error)
	set(ctx context.Context, key, value string, ttl time.Duration) error
	hGetAll(ctx context.Context, key string) (map[string]string, error)
	hSet(ctx context.Context, key string, fields map[string]string) error
	expire(ctx context.Context, key string, ttl time.Duration) error
	del(ctx context.Context, keys ...string) error
	close() error
}

// connectRedis dials Redis when configured, returning the live client and the
// normalized key prefix. A blank address is a clean no-op; a dial failure
// logs and returns nil so the bot keeps its in-memory and SQLite behavior.
func connectRedis(ctx context.Context, cfg redisConfig) (redisClient, string) {
	if !cfg.enabled() {
		return nil, ""
	}

	prefix := normalizeRedisPrefix(cfg.KeyPrefix)

	client, err := newRedisClient(ctx, cfg)
	if err != nil {
		logWarn("connect redis", err, "address", cfg.Address)

		return nil, prefix
	}

	return client, prefix
}

type goRedisClient struct {
	client *redis.Client
}

func newRedisClient(ctx context.Context, cfg redisConfig) (redisClient, error) {
	if !cfg.enabled() {
		return nil, nil
	}

	if ctx == nil {
		return nil, fmt.Errorf("nil redis context: %w", os.ErrInvalid)
	}

	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Address,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  redisConnectTimeout,
		ReadTimeout:  redisReadTimeout,
		WriteTimeout: redisWriteTimeout,
	})

	pingCtx, cancelPing := context.WithTimeout(ctx, redisConnectTimeout+redisReadTimeout)
	defer cancelPing()

	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()

		return nil, fmt.Errorf("ping redis at %q: %w", cfg.Address, err)
	}

	return &goRedisClient{client: client}, nil
}

func (adapter *goRedisClient) ping(ctx context.Context) error {
	if err := adapter.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}

	return nil
}

func (adapter *goRedisClient) setNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	claimed, err := adapter.client.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("claim redis key %q: %w", key, err)
	}

	return claimed, nil
}

func (adapter *goRedisClient) get(ctx context.Context, key string) (string, error) {
	value, err := adapter.client.Get(ctx, key).Result()
	if err != nil {
		return "", fmt.Errorf("get redis key %q: %w", key, err)
	}

	return value, nil
}

func (adapter *goRedisClient) set(ctx context.Context, key, value string, ttl time.Duration) error {
	if err := adapter.client.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("set redis key %q: %w", key, err)
	}

	return nil
}

func (adapter *goRedisClient) hGetAll(ctx context.Context, key string) (map[string]string, error) {
	fields, err := adapter.client.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("read redis hash %q: %w", key, err)
	}

	return fields, nil
}

func (adapter *goRedisClient) hSet(ctx context.Context, key string, fields map[string]string) error {
	if err := adapter.client.HSet(ctx, key, fields).Err(); err != nil {
		return fmt.Errorf("write redis hash %q: %w", key, err)
	}

	return nil
}

func (adapter *goRedisClient) expire(ctx context.Context, key string, ttl time.Duration) error {
	if err := adapter.client.Expire(ctx, key, ttl).Err(); err != nil {
		return fmt.Errorf("expire redis key %q: %w", key, err)
	}

	return nil
}

func (adapter *goRedisClient) del(ctx context.Context, keys ...string) error {
	if err := adapter.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("delete redis keys: %w", err)
	}

	return nil
}

func (adapter *goRedisClient) close() error {
	if err := adapter.client.Close(); err != nil {
		return fmt.Errorf("close redis client: %w", err)
	}

	return nil
}

// redisKey joins colon-separated segments under the configured prefix. Empty
// segments are dropped so callers never produce doubled separators.
func redisKey(prefix string, parts ...string) string {
	segments := make([]string, 0, len(parts)+1)

	if trimmed := strings.Trim(strings.TrimSpace(prefix), ":"); trimmed != "" {
		segments = append(segments, trimmed)
	}

	for _, part := range parts {
		if trimmed := strings.Trim(strings.TrimSpace(part), ":"); trimmed != "" {
			segments = append(segments, trimmed)
		}
	}

	return strings.Join(segments, ":")
}

func redisDedupKey(prefix, messageID string) string {
	return redisKey(prefix, "dedup", messageID)
}

// redisFetchKey hashes the normalized URL instead of embedding it. Full URLs
// are long, leak query strings into keyspace listings, and vary only in case;
// the digest keeps keys short while the hash fields retain the real URLs.
func redisFetchKey(prefix, normalizedURL string) string {
	sum := sha256.Sum256([]byte(normalizedURL))

	return redisKey(prefix, "fetch", hex.EncodeToString(sum[:]))
}

func redisHistoryKey(prefix, storeKey string) string {
	return redisKey(prefix, "history", storeKey)
}

func redisBotStateKey(prefix, storeKey string) string {
	return redisKey(prefix, "botstate", storeKey)
}

// redisMessageNodeStoreBackend persists the reply-chain snapshot as a single
// JSON string per store key, using the same payload encoding as SQLite. The
// snapshot TTL refreshes on every write so idle keys expire instead of
// accumulating.
type redisMessageNodeStoreBackend struct {
	client redisClient
	prefix string
}

func newRedisMessageNodeStoreBackend(client redisClient, keyPrefix string) *redisMessageNodeStoreBackend {
	if client == nil {
		return nil
	}

	return &redisMessageNodeStoreBackend{client: client, prefix: normalizeRedisPrefix(keyPrefix)}
}

func (backend *redisMessageNodeStoreBackend) loadSnapshot(storeKey string, capacity int) (messageNodeStoreSnapshot, error) {
	if backend == nil || backend.client == nil {
		return messageNodeStoreSnapshot{}, os.ErrNotExist
	}

	opCtx, cancel := context.WithTimeout(context.Background(), redisPersistenceOpTimeout)
	defer cancel()

	payload, err := backend.client.get(opCtx, redisHistoryKey(backend.prefix, storeKey))
	if err != nil {
		if isRedisNilError(err) {
			return messageNodeStoreSnapshot{}, os.ErrNotExist
		}

		return messageNodeStoreSnapshot{}, fmt.Errorf("load message history from redis: %w", err)
	}

	var snapshot messageNodeStoreSnapshot
	if err := decodeMessageNodeSnapshotJSON([]byte(payload), &snapshot.Nodes); err != nil {
		return messageNodeStoreSnapshot{}, fmt.Errorf("decode message history snapshot JSON: %w", err)
	}

	snapshot.Version = messageNodeStoreSnapshotVersion
	if snapshot.Nodes == nil {
		snapshot.Nodes = make(map[string]messageNodeSnapshot)
	}

	snapshot.Nodes = trimSnapshotNodes(snapshot.Nodes, capacity)

	return snapshot, nil
}

func (backend *redisMessageNodeStoreBackend) saveSnapshot(storeKey string, snapshot messageNodeStoreSnapshot) error {
	if backend == nil || backend.client == nil {
		return fmt.Errorf("nil redis message history backend: %w", os.ErrInvalid)
	}

	payload, err := encodeMessageNodeSnapshotJSON(snapshot.Nodes)
	if err != nil {
		return err
	}

	opCtx, cancel := context.WithTimeout(context.Background(), redisPersistenceOpTimeout)
	defer cancel()

	if err := backend.client.set(opCtx, redisHistoryKey(backend.prefix, storeKey), string(payload), redisHistoryTTL); err != nil {
		return fmt.Errorf("save message history to redis: %w", err)
	}

	return nil
}

func (backend *redisMessageNodeStoreBackend) close() error {
	return nil
}

// redisBotStateBackend persists the operator runtime snapshot as JSON per
// store key with a refreshed TTL on every write.
type redisBotStateBackend struct {
	client redisClient
	prefix string
}

func newRedisBotStateBackend(client redisClient, keyPrefix string) *redisBotStateBackend {
	if client == nil {
		return nil
	}

	return &redisBotStateBackend{client: client, prefix: normalizeRedisPrefix(keyPrefix)}
}

func (backend *redisBotStateBackend) loadBotState(storeKey string) (botStateSnapshot, error) {
	if backend == nil || backend.client == nil {
		return botStateSnapshot{}, os.ErrNotExist
	}

	opCtx, cancel := context.WithTimeout(context.Background(), redisPersistenceOpTimeout)
	defer cancel()

	payload, err := backend.client.get(opCtx, redisBotStateKey(backend.prefix, storeKey))
	if err != nil {
		if isRedisNilError(err) {
			return botStateSnapshot{}, os.ErrNotExist
		}

		return botStateSnapshot{}, fmt.Errorf("load bot state from redis: %w", err)
	}

	var snapshot botStateSnapshot
	if err := jsonUnmarshalBotState([]byte(payload), &snapshot); err != nil {
		return botStateSnapshot{}, err
	}

	return snapshot, nil
}

// redisChainedHistoryBackend writes every snapshot to Redis and to the
// previously attached backend (typically SQLite) so either tier can restore a
// cold start. Reads prefer the previously attached backend; a miss falls back
// to Redis, which also covers Redis-only deployments with no SQLite path. The
// sibling backend is captured at attach time so persist/close never re-enter
// the store lock.
type redisChainedHistoryBackend struct {
	redis *redisMessageNodeStoreBackend
	other messageNodeStoreBackend
}

func newRedisChainedHistoryBackend(redisBackend *redisMessageNodeStoreBackend, other messageNodeStoreBackend) *redisChainedHistoryBackend {
	return &redisChainedHistoryBackend{redis: redisBackend, other: other}
}

func (backend *redisChainedHistoryBackend) loadSnapshot(storeKey string, capacity int) (messageNodeStoreSnapshot, error) {
	if backend != nil && backend.other != nil {
		if snapshot, err := backend.other.loadSnapshot(storeKey, capacity); err == nil {
			return snapshot, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			logWarn("load chained message history", err, "store_key", storeKey)
		}
	}

	if backend != nil && backend.redis != nil {
		return backend.redis.loadSnapshot(storeKey, capacity)
	}

	return messageNodeStoreSnapshot{}, os.ErrNotExist
}

func (backend *redisChainedHistoryBackend) saveSnapshot(storeKey string, snapshot messageNodeStoreSnapshot) error {
	if backend == nil {
		return os.ErrNotExist
	}

	var redisErr, otherErr error
	if backend.redis != nil {
		redisErr = backend.redis.saveSnapshot(storeKey, snapshot)
	}

	if backend.other != nil {
		otherErr = backend.other.saveSnapshot(storeKey, snapshot)
	}

	if redisErr != nil && otherErr != nil {
		return errors.Join(redisErr, otherErr)
	}

	if otherErr != nil {
		return otherErr
	}

	return redisErr
}

func (backend *redisChainedHistoryBackend) close() error {
	if backend != nil && backend.other != nil {
		return backend.other.close()
	}

	return nil
}

func (backend *redisBotStateBackend) saveBotState(storeKey string, snapshot botStateSnapshot) error {
	if backend == nil || backend.client == nil {
		return fmt.Errorf("nil redis bot state backend: %w", os.ErrInvalid)
	}

	snapshot.Version = botStateSnapshotVersion

	payload, err := jsonMarshalBotState(snapshot)
	if err != nil {
		return err
	}

	opCtx, cancel := context.WithTimeout(context.Background(), redisPersistenceOpTimeout)
	defer cancel()

	if err := backend.client.set(opCtx, redisBotStateKey(backend.prefix, storeKey), string(payload), redisHistoryTTL); err != nil {
		return fmt.Errorf("save bot state to redis: %w", err)
	}

	return nil
}

// closeRedisClient releases the shared Redis connection. History and bot
// state are already flushed through the chained backends before this runs.
func (instance *bot) closeRedisClient() {
	if instance == nil || instance.redisClient == nil {
		return
	}

	if err := instance.redisClient.close(); err != nil {
		logWarn("close redis client", err)
	}

	instance.redisClient = nil
	instance.redisDedupClient = nil
	instance.redisReachable.Store(false)
}

// probeRedisRefreshes the reachable flag without failing startup. Handlers
// treat an unreachable client as absent and use local state.
func (instance *bot) probeRedisReachable(ctx context.Context) bool {
	if instance == nil || instance.redisClient == nil {
		return false
	}

	if ctx == nil {
		ctx = context.Background()
	}

	opCtx, cancel := context.WithTimeout(ctx, redisFastOpTimeout)
	defer cancel()

	if err := instance.redisClient.ping(opCtx); err != nil {
		instance.redisReachable.Store(false)

		return false
	}

	instance.redisReachable.Store(true)

	return true
}

// reserveMessageDedup claims a message ID cluster-wide. The second claimant
// loses: SetNX returns false when another instance (or a retry) already holds
// the key. Redis errors are returned so the caller can fall back to the local
// dedup map instead of dropping the message.
func reserveMessageDedup(ctx context.Context, client redisClient, prefix, messageID string) (bool, error) {
	if client == nil || strings.TrimSpace(messageID) == "" {
		return true, nil
	}

	if ctx == nil {
		ctx = context.Background()
	}

	opCtx, cancel := context.WithTimeout(ctx, redisFastOpTimeout)
	defer cancel()

	return client.setNX(opCtx, redisDedupKey(prefix, messageID), "1", messageSeenWindow)
}

func newTinyFishFetchCacheWithRedis(client redisClient, keyPrefix string) *tinyFishFetchCache {
	cache := newTinyFishFetchCache()
	cache.redisClient = client
	cache.redisPrefix = normalizeRedisPrefix(keyPrefix)

	return cache
}

func (cache *tinyFishFetchCache) redisFetchClient() redisClient {
	if cache == nil {
		return nil
	}

	return cache.redisClient
}

// lookupRedis serves a memory miss from the shared Redis fetch cache and
// backfills memory so repeat URLs stay zero-round-trip. Any failure is a
// plain miss; the caller falls through to a live fetch.
func (cache *tinyFishFetchCache) lookupRedis(rawURL string) (text, title, description, resolvedURL, finalURL string, ok bool) {
	client := cache.redisFetchClient()
	if client == nil {
		return "", "", "", "", "", false
	}

	normalized := normalizeTinyFishFetchCacheKey(rawURL)
	if normalized == "" {
		return "", "", "", "", "", false
	}

	opCtx, cancel := context.WithTimeout(context.Background(), redisFastOpTimeout)
	defer cancel()

	fields, err := client.hGetAll(opCtx, redisFetchKey(cache.redisPrefix, normalized))
	if err != nil {
		slog.Debug("redis fetch cache lookup failed", "error", err, "url", rawURL)

		return "", "", "", "", "", false
	}

	text = strings.TrimSpace(fields["text"])
	if text == "" {
		return "", "", "", "", "", false
	}

	title = strings.TrimSpace(fields["title"])
	description = strings.TrimSpace(fields["description"])
	resolvedURL = strings.TrimSpace(fields["url"])
	finalURL = strings.TrimSpace(fields["final_url"])

	cache.storeMemory([]string{normalized}, tinyFishFetchCacheEntry{
		text:        text,
		title:       title,
		description: description,
		url:         resolvedURL,
		finalURL:    finalURL,
		expiresAt:   time.Now().Add(tinyFishFetchCacheTTL),
	})

	return text, title, description, resolvedURL, finalURL, true
}

// storeRedis mirrors a fetched page into the shared cache. Keys are derived
// per URL variant so lookups by either the request or the final URL hit.
func (cache *tinyFishFetchCache) storeRedis(keys []string, entry tinyFishFetchCacheEntry) {
	client := cache.redisFetchClient()
	if client == nil || len(keys) == 0 {
		return
	}

	opCtx, cancel := context.WithTimeout(context.Background(), redisFastOpTimeout)
	defer cancel()

	fields := map[string]string{
		"text":        entry.text,
		"title":       entry.title,
		"description": entry.description,
		"url":         entry.url,
		"final_url":   entry.finalURL,
	}

	seen := make(map[string]struct{}, len(keys))

	for _, normalized := range keys {
		key := redisFetchKey(cache.redisPrefix, normalized)
		if _, duplicate := seen[key]; duplicate {
			continue
		}

		seen[key] = struct{}{}

		if err := client.hSet(opCtx, key, fields); err != nil {
			slog.Debug("redis fetch cache store failed", "error", err, "key", key)

			continue
		}

		if err := client.expire(opCtx, key, tinyFishFetchCacheTTL); err != nil {
			slog.Debug("redis fetch cache expire failed", "error", err, "key", key)
		}
	}
}
