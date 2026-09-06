package app

import (
	"strings"
	"sync"
	"time"
)

type tinyFishFetchCacheEntry struct {
	text        string
	title       string
	description string
	url         string
	finalURL    string
	expiresAt   time.Time
}

// tinyFishFetchCache is a bounded in-memory cache of TinyFish Fetch results
// keyed by lower-cased URL. The Fetch API already serves its own server-side
// cache when ttl is omitted, but a server cache hit still costs a full HTTPS
// round trip; this client cache turns repeat URLs (overlapping queries, show
// sources, website re-fetches) into zero-round-trip lookups. Full fetched
// text is stored so later callers can truncate to their own limit. The
// resolved request and final URLs are stored alongside so cache hits rebuild
// the exact same page URL and title fallback a live fetch would produce.
type tinyFishFetchCache struct {
	mu      sync.Mutex
	entries map[string]tinyFishFetchCacheEntry
}

func newTinyFishFetchCache() *tinyFishFetchCache {
	return &tinyFishFetchCache{
		entries: make(map[string]tinyFishFetchCacheEntry),
	}
}

func normalizeTinyFishFetchCacheKey(rawURL string) string {
	return strings.ToLower(strings.TrimSpace(rawURL))
}

func (cache *tinyFishFetchCache) lookup(rawURL string) (text, title, description, resolvedURL, finalURL string, ok bool) {
	if cache == nil {
		return "", "", "", "", "", false
	}

	key := normalizeTinyFishFetchCacheKey(rawURL)
	if key == "" {
		return "", "", "", "", "", false
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	entry, found := cache.entries[key]
	if !found {
		return "", "", "", "", "", false
	}

	if !time.Now().Before(entry.expiresAt) {
		delete(cache.entries, key)

		return "", "", "", "", "", false
	}

	return entry.text, entry.title, entry.description, entry.url, entry.finalURL, true
}

func (cache *tinyFishFetchCache) store(rawURL, finalURL, text, title, description string) {
	if cache == nil {
		return
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	trimmedRawURL := strings.TrimSpace(rawURL)
	trimmedFinalURL := strings.TrimSpace(finalURL)

	keys := make([]string, 0, 2)
	seenKeys := make(map[string]struct{}, 2)

	for _, candidate := range []string{trimmedRawURL, trimmedFinalURL} {
		key := normalizeTinyFishFetchCacheKey(candidate)
		if key == "" {
			continue
		}

		if _, seen := seenKeys[key]; seen {
			continue
		}

		seenKeys[key] = struct{}{}
		keys = append(keys, key)
	}

	if len(keys) == 0 {
		return
	}

	expiresAt := time.Now().Add(tinyFishFetchCacheTTL)

	cache.mu.Lock()
	defer cache.mu.Unlock()

	cache.evictExpiredLocked()

	newKeysCount := 0

	for _, key := range keys {
		if _, exists := cache.entries[key]; !exists {
			newKeysCount++
		}
	}

	// Evict enough entries so that adding all new keys cannot exceed MaxEntries.
	for len(cache.entries)+newKeysCount > tinyFishFetchCacheMaxEntries && len(cache.entries) > 0 {
		cache.evictOldestLocked()
	}

	entry := tinyFishFetchCacheEntry{
		text:        text,
		title:       strings.TrimSpace(title),
		description: strings.TrimSpace(description),
		url:         trimmedRawURL,
		finalURL:    trimmedFinalURL,
		expiresAt:   expiresAt,
	}

	for _, key := range keys {
		cache.entries[key] = entry
	}
}

func (cache *tinyFishFetchCache) len() int {
	if cache == nil {
		return 0
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	return len(cache.entries)
}

func (cache *tinyFishFetchCache) evictExpiredLocked() {
	now := time.Now()

	for key, entry := range cache.entries {
		if !now.Before(entry.expiresAt) {
			delete(cache.entries, key)
		}
	}
}

func (cache *tinyFishFetchCache) evictOldestLocked() {
	var (
		oldestKey string
		oldestAt  time.Time
		found     bool
	)

	for key, entry := range cache.entries {
		if !found || entry.expiresAt.Before(oldestAt) {
			oldestKey = key
			oldestAt = entry.expiresAt
			found = true
		}
	}

	if found {
		delete(cache.entries, oldestKey)
	}
}
