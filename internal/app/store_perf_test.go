package app

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEncodeMessageNodeSnapshotJSONOmitsEmptyFields(t *testing.T) {
	t.Parallel()

	nodes := map[string]messageNodeSnapshot{
		"1234567890123456789": {
			Role:        messageRoleUser,
			Text:        "hello",
			Initialized: true,
		},
	}

	payloadBytes, err := encodeMessageNodeSnapshotJSON(nodes)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}

	payload := string(payloadBytes)

	for _, key := range []string{`"thinking_text"`, `"url_scan_text"`, `"gist_url"`, `"provider_response_id"`, `"provider_response_model"`, `"media"`, `"search_metadata"`, `"parent_message"`, `"has_bad_attachments"`, `"attachment_download_failed"`, `"fetch_parent_failed"`} {
		if strings.Contains(payload, key) {
			t.Fatalf("expected empty field %s to be omitted from encoded snapshot, got %s", key, payload)
		}
	}

	decoded := decodeSnapshotForTest(t, payloadBytes)

	snap, ok := decoded["1234567890123456789"]
	if !ok {
		t.Fatalf("missing node after round-trip")
	}

	if snap.Role != messageRoleUser || snap.Text != "hello" || !snap.Initialized {
		t.Fatalf("round-trip lost data: %#v", snap)
	}
}

func TestPersistSkipsUnchangedSnapshotWithoutBackendWrite(t *testing.T) {
	t.Parallel()

	backend := newTestMessageNodeStoreBackend()

	store, err := newPersistentMessageNodeStore(10, testSharedHomeBotsStoreKey, backend)
	if err != nil {
		t.Fatalf("new persistent store: %v", err)
	}

	defer func() {
		_ = store.close()
	}()

	cacheInitializedStoreNode(store, "1234567890123456789", "hello")

	if err := store.persist(); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	backend.mu.Lock()
	first, ok := backend.snapshots[testSharedHomeBotsStoreKey]
	backend.mu.Unlock()

	if !ok {
		t.Fatalf("expected snapshot after first persist")
	}

	if first.Nodes["1234567890123456789"].Text != "hello" {
		t.Fatalf("first persist lost data: %#v", first.Nodes)
	}

	// Second persist without any mutation must not rewrite the backend.
	// Replace the stored copy with a sentinel to detect a rewrite.
	backend.mu.Lock()
	backend.snapshots[testSharedHomeBotsStoreKey] = messageNodeStoreSnapshot{
		Version: first.Version,
		Nodes:   map[string]messageNodeSnapshot{},
	}
	backend.mu.Unlock()

	if err := store.persist(); err != nil {
		t.Fatalf("second persist: %v", err)
	}

	backend.mu.Lock()
	second, ok := backend.snapshots[testSharedHomeBotsStoreKey]
	backend.mu.Unlock()

	if !ok {
		t.Fatalf("expected snapshot entry to remain")
	}

	if len(second.Nodes) != 0 {
		t.Fatalf("expected unchanged persist to skip backend write, backend was rewritten with %d nodes", len(second.Nodes))
	}
}

func TestConfigurePostgresMessageNodeStorePoolSetsLimits(t *testing.T) {
	t.Parallel()

	database, err := sql.Open("postgres", "postgres://localhost:5432/llmcordgo?sslmode=disable")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	defer func() {
		_ = database.Close()
	}()

	configurePostgresMessageNodeStorePool(database)

	stats := database.Stats()

	if stats.MaxOpenConnections <= 0 {
		t.Fatalf("expected MaxOpenConnections to be set, got %d", stats.MaxOpenConnections)
	}

	if stats.MaxOpenConnections > 16 {
		t.Fatalf("expected bounded MaxOpenConnections, got %d", stats.MaxOpenConnections)
	}

	if stats.MaxIdleClosed < 0 {
		t.Fatalf("unexpected stats: %#v", stats)
	}
}

func TestSanitizePostgresJSONStringAvoidsAllocationForCleanStrings(t *testing.T) {
	clean := "hello world, plain ascii message with no weird bytes"

	allocs := testing.AllocsPerRun(100, func() {
		_ = sanitizePostgresJSONString(clean)
	})

	if allocs != 0 {
		t.Fatalf("expected zero allocations sanitizing clean string, got %v", allocs)
	}

	if got := sanitizePostgresJSONString(clean); got != clean {
		t.Fatalf("clean string changed by sanitize: %q", got)
	}

	if got := sanitizePostgresJSONString("a\x00b"); strings.ContainsRune(got, 0) {
		t.Fatalf("NUL byte not sanitized: %q", got)
	}

	if got := sanitizePostgresJSONString("a\xffb"); got == "a\xffb" {
		t.Fatalf("invalid UTF-8 not sanitized: %q", got)
	}
}

func TestTrimSnapshotToFitKeepsNewestAndFits(t *testing.T) {
	t.Parallel()

	const (
		nodeCount    = 20
		safeMaxBytes = 1 * 1024 * 1024
	)

	nodes := make(map[string]messageNodeSnapshot, nodeCount)

	for i := 1; i <= nodeCount; i++ {
		id := strings.Repeat("0", 19-len(itoaForTest(i))) + itoaForTest(i)
		nodes[id] = messageNodeSnapshot{
			Role:        messageRoleAssistant,
			Text:        strings.Repeat("X", 100*1024),
			Initialized: true,
		}
	}

	fitted := trimSnapshotToFit(nodes, safeMaxBytes)

	payloadBytes, err := json.Marshal(sanitizeMessageNodeSnapshotPayload(messageNodeSnapshotPayload{Nodes: fitted}))
	if err != nil {
		t.Fatalf("marshal fitted: %v", err)
	}

	if len(payloadBytes) > safeMaxBytes {
		t.Fatalf("fitted snapshot still too large: %d > %d", len(payloadBytes), safeMaxBytes)
	}

	if len(fitted) >= nodeCount {
		t.Fatalf("expected eviction, got %d nodes", len(fitted))
	}

	if _, ok := fitted["0000000000000000001"]; ok {
		t.Fatalf("expected oldest node to be evicted")
	}

	if _, ok := fitted["0000000000000000020"]; !ok {
		t.Fatalf("expected newest node to be kept")
	}
}

func TestPersistKeepsDirtyWhenMutatedDuringWrite(t *testing.T) {
	t.Parallel()

	backend := newBlockingSaveMessageNodeStoreBackend()
	store := newMessageNodeStore(10)
	store.storeKey = "dirty-race-store"
	store.backend = backend

	cacheInitializedStoreNode(store, "message-1", "v1")

	persistDone := make(chan error, 1)

	go func() {
		persistDone <- store.persist()
	}()

	select {
	case <-backend.saveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for persist to reach the backend")
	}

	cacheInitializedStoreNode(store, "message-1", "v2")
	store.persistBestEffort()
	close(backend.releaseSave)

	select {
	case err := <-persistDone:
		if err != nil {
			t.Fatalf("persist during mutation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for persist to finish")
	}

	if !store.dirty.Load() {
		t.Fatal("mutation during backend write was cleared without being persisted")
	}

	store.snapshotMu.Lock()
	cached, found := store.snapshotCache["message-1"]
	store.snapshotMu.Unlock()

	if !found || cached.Text != "v2" {
		t.Fatalf("stale snapshot overwrote fresh cache entry: %#v", cached)
	}
}

func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}

	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}

	return digits
}
