package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeMessageNodeSnapshotJSONTruncatesLargeFieldsToAvoidPostgresJSONBOverflow(t *testing.T) {
	t.Parallel()

	nodes := buildOversizedSnapshotForTest()

	payloadBytes, err := encodeMessageNodeSnapshotJSON(nodes)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}

	const safeMaxBytes = 200 * 1024 * 1024
	if len(payloadBytes) > safeMaxBytes {
		t.Fatalf("encoded snapshot still too large: %d bytes > %d safe max (root cause not fixed)", len(payloadBytes), safeMaxBytes)
	}

	decoded := decodeSnapshotForTest(t, payloadBytes)

	snap, ok := decoded["1234567890123456789"]
	if !ok {
		t.Fatalf("missing node after round-trip: %#v", decoded)
	}

	assertSnapshotNotTruncatedForTest(t, snap)
}

func buildOversizedSnapshotForTest() map[string]messageNodeSnapshot {
	largeText := strings.Repeat("A", 200*1024)
	largeThinking := strings.Repeat("B", 200*1024)
	largeURLScan := strings.Repeat("C", 200*1024)

	largeData := make([]byte, 2*1024*1024)
	for i := range largeData {
		largeData[i] = 'x'
	}

	largeImageURL := "data:image/png;base64," + strings.Repeat("Z", 2*1024*1024)
	largeSearchText := strings.Repeat("S", 100*1024)

	return map[string]messageNodeSnapshot{
		"1234567890123456789": {
			Role:         messageRoleAssistant,
			Text:         largeText,
			ThinkingText: largeThinking,
			URLScanText:  largeURLScan,
			GistURL:      "https://gist.github.com/example/" + strings.Repeat("g", 10*1024),
			Media: []contentPartSnapshot{
				{Type: contentTypeDocument, Data: largeData, MIMEType: mimeTypePDF, Filename: "large.pdf"},
				{Type: contentTypeImageURL, ImageURL: largeImageURL},
			},
			SearchMetadata: &searchMetadata{
				Queries: []string{strings.Repeat("Q", 10*1024)},
				Results: []webSearchResult{
					{Query: "q", Text: largeSearchText},
				},
			},
			ParentMessage: &discordMessageSnapshot{
				ID:        "parent1",
				ChannelID: "channel1",
				GuildID:   "guild1",
				Content:   strings.Repeat("P", 50*1024),
			},
			Initialized: true,
		},
	}
}

func decodeSnapshotForTest(t *testing.T, payloadBytes []byte) map[string]messageNodeSnapshot {
	t.Helper()

	var decoded map[string]messageNodeSnapshot
	if err := decodeMessageNodeSnapshotJSON(payloadBytes, &decoded); err == nil {
		return decoded
	}

	var payload messageNodeSnapshotPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("decode after encode: %v", err)
	}

	return payload.Nodes
}

func assertSnapshotNotTruncatedForTest(t *testing.T, snap messageNodeSnapshot) {
	t.Helper()

	if len(snap.Text) != 200*1024 {
		t.Fatalf("Text should not be truncated: got %d want %d", len(snap.Text), 200*1024)
	}

	if len(snap.ThinkingText) != 200*1024 {
		t.Fatalf("ThinkingText should not be truncated: got %d want %d", len(snap.ThinkingText), 200*1024)
	}

	if len(snap.URLScanText) != 200*1024 {
		t.Fatalf("URLScanText should not be truncated: got %d want %d", len(snap.URLScanText), 200*1024)
	}

	if len(snap.Media) != 2 {
		t.Fatalf("expected 2 media parts, got %d", len(snap.Media))
	}

	for _, part := range snap.Media {
		if part.Type == contentTypeDocument && len(part.Data) != 2*1024*1024 {
			t.Fatalf("Media Data should not be truncated: got %d want %d", len(part.Data), 2*1024*1024)
		}

		if part.Type == contentTypeImageURL && len(part.ImageURL) != len("data:image/png;base64,")+2*1024*1024 {
			t.Fatalf("ImageURL should not be truncated: got %d", len(part.ImageURL))
		}
	}

	if snap.SearchMetadata == nil || len(snap.SearchMetadata.Results) == 0 || len(snap.SearchMetadata.Results[0].Text) != 100*1024 {
		t.Fatalf("Search result text should not be truncated")
	}

	if snap.ParentMessage == nil || len(snap.ParentMessage.Content) != 50*1024 {
		t.Fatalf("ParentMessage Content should not be truncated: got %d", len(snap.ParentMessage.Content))
	}
}

func TestEncodeMessageNodeSnapshotJSONEvictsOldestNodesWhenTotalSizeExceedsSafeLimit(t *testing.T) {
	t.Parallel()

	const (
		nodeCount    = 20
		safeMaxBytes = 1 * 1024 * 1024
	)

	nodes := make(map[string]messageNodeSnapshot, nodeCount)

	for i := 1; i <= nodeCount; i++ {
		digits := ""
		n := i

		for n > 0 {
			digits = string(rune('0'+n%10)) + digits
			n /= 10
		}

		if digits == "" {
			digits = "0"
		}

		padded := strings.Repeat("0", 19-len(digits)) + digits
		nodes[padded] = messageNodeSnapshot{
			Role:        messageRoleAssistant,
			Text:        strings.Repeat("X", 100*1024),
			Initialized: true,
		}
	}

	fitted := trimSnapshotToFit(nodes, safeMaxBytes)

	sanitizedPayload := sanitizeMessageNodeSnapshotPayload(messageNodeSnapshotPayload{Nodes: fitted})

	payloadBytes, err := json.Marshal(sanitizedPayload)
	if err != nil {
		t.Fatalf("marshal fitted: %v", err)
	}

	if len(payloadBytes) > safeMaxBytes {
		t.Fatalf("fitted snapshot still too large after eviction: %d > %d", len(payloadBytes), safeMaxBytes)
	}

	if len(fitted) >= nodeCount {
		t.Fatalf("expected oldest nodes to be evicted to fit size limit, got %d nodes (expected <%d)", len(fitted), nodeCount)
	}

	if _, ok := fitted["0000000000000000001"]; ok {
		t.Fatalf("expected oldest node 0000000000000000001 to be evicted")
	}

	lastID := ""
	n := nodeCount

	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}

	if digits == "" {
		digits = "0"
	}

	lastID = strings.Repeat("0", 19-len(digits)) + digits
	if _, ok := fitted[lastID]; !ok {
		t.Fatalf("expected newest node %s to be kept", lastID)
	}
}

func TestSaveSnapshotHandlesPostgresJSONBSizeErrorByTrimming(t *testing.T) {
	t.Parallel()

	largeData := make([]byte, 200*1024)
	for i := range largeData {
		largeData[i] = 'a'
	}

	nodes := map[string]messageNodeSnapshot{}

	for i := 1; i <= 20; i++ {
		digits := ""
		n := i

		for n > 0 {
			digits = string(rune('0'+n%10)) + digits
			n /= 10
		}

		if digits == "" {
			digits = "0"
		}

		padded := strings.Repeat("0", 19-len(digits)) + digits
		nodes[padded] = messageNodeSnapshot{
			Role:        messageRoleAssistant,
			Text:        strings.Repeat("T", 20*1024),
			Initialized: true,
			Media: []contentPartSnapshot{
				{Type: contentTypeDocument, Data: largeData, MIMEType: mimeTypePDF},
			},
		}
	}

	snapshot := messageNodeStoreSnapshot{Version: messageNodeStoreSnapshotVersion, Nodes: nodes}
	backend := &sizeFailingTestBackend{safeMax: 1 * 1024 * 1024}

	err := backend.saveSnapshot("shared-home-bots", snapshot)
	if err != nil {
		t.Fatalf("saveSnapshot should have trimmed and succeeded, got error: %v", err)
	}

	if backend.storedBytes > 1*1024*1024 {
		t.Fatalf("stored bytes still exceed safe limit: %d", backend.storedBytes)
	}
}

type sizeFailingTestBackend struct {
	safeMax     int
	storedBytes int
}

func (b *sizeFailingTestBackend) loadSnapshot(_ string, _ int) (messageNodeStoreSnapshot, error) {
	return messageNodeStoreSnapshot{}, nil
}

func (b *sizeFailingTestBackend) saveSnapshot(_ string, snapshot messageNodeStoreSnapshot) error {
	payloadBytes, err := encodeMessageNodeSnapshotJSON(snapshot.Nodes)
	if err != nil {
		return err
	}

	if len(payloadBytes) > b.safeMax {
		fitted := trimSnapshotToFit(snapshot.Nodes, b.safeMax)

		payloadBytes, err = encodeMessageNodeSnapshotJSON(fitted)
		if err != nil {
			return err
		}

		if len(payloadBytes) > b.safeMax {
			return &stubPQError{msg: "pq: total size of jsonb object elements exceeds the maximum of 268435455 bytes"}
		}
	}

	b.storedBytes = len(payloadBytes)

	return nil
}

func (b *sizeFailingTestBackend) close() error { return nil }

type stubPQError struct{ msg string }

func (e *stubPQError) Error() string { return e.msg }
