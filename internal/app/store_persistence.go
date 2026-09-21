package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	// Register the SQLite driver for database/sql ("sqlite" driver name).
	_ "modernc.org/sqlite"
)

const (
	messageNodeStoreSnapshotVersion     = 1
	messageNodeStoreTableName           = "message_history_snapshots"
	defaultMessageNodeStorePersistDelay = 250 * time.Millisecond
	snapshotJSONTextReplacement         = "\uFFFD"
	sqliteCorruptErrorSubstring         = "database disk image is malformed"
	sqliteNotDatabaseErrorSubstring     = "file is not a database"
	sqliteDatabaseDirPerm               = 0o750
)

const (
	messageNodeStorePersistSafeMaxBytes = 200 * 1024 * 1024
	trimSnapshotBinarySearchDivisor     = 2
)

const (
	messageNodeStoreMaxOpenConns     = 1
	messageNodeStoreConnMaxLifetime  = 5 * time.Minute
	messageNodeStoreConnMaxIdleTime  = 1 * time.Minute
	messageNodeStoreStatementTimeout = 30 * time.Second
)

var errMessageNodeStorePersistenceDisabled = errors.New("message history persistence disabled")

const (
	messageNodeStoreSelectSQL = "SELECT version, snapshot FROM message_history_snapshots WHERE store_key = ?"
	messageNodeStoreUpsertSQL = "INSERT INTO message_history_snapshots (store_key, version, snapshot, updated_at) " +
		"VALUES (?, ?, ?, CURRENT_TIMESTAMP) " +
		"ON CONFLICT (store_key) DO UPDATE SET version = excluded.version, " +
		"snapshot = excluded.snapshot, updated_at = CURRENT_TIMESTAMP"
	messageNodeStoreCreateTableSQL = "CREATE TABLE IF NOT EXISTS message_history_snapshots (" +
		"store_key TEXT PRIMARY KEY," +
		"version INTEGER NOT NULL," +
		"snapshot TEXT NOT NULL," +
		"updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP" +
		")"
)

type messageNodeStoreSnapshot struct {
	Version int                            `json:"version"`
	Nodes   map[string]messageNodeSnapshot `json:"nodes"`
}

type messageNodeSnapshotPayload struct {
	Nodes map[string]messageNodeSnapshot `json:"nodes"`
}

type messageNodeSnapshot struct {
	Role                     string                  `json:"role,omitempty"`
	Text                     string                  `json:"text,omitempty"`
	ThinkingText             string                  `json:"thinking_text,omitempty"`
	URLScanText              string                  `json:"url_scan_text,omitempty"`
	GistURL                  string                  `json:"gist_url,omitempty"`
	ProviderResponseID       string                  `json:"provider_response_id,omitempty"`
	ProviderResponseModel    string                  `json:"provider_response_model,omitempty"`
	Media                    []contentPartSnapshot   `json:"media,omitempty"`
	SearchMetadata           *searchMetadata         `json:"search_metadata,omitempty"`
	HasBadAttachments        bool                    `json:"has_bad_attachments,omitempty"`
	AttachmentDownloadFailed bool                    `json:"attachment_download_failed,omitempty"`
	FetchParentFailed        bool                    `json:"fetch_parent_failed,omitempty"`
	ParentMessage            *discordMessageSnapshot `json:"parent_message,omitempty"`
	Initialized              bool                    `json:"initialized,omitempty"`
}

type contentPartSnapshot struct {
	Type     string `json:"type,omitempty"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Data     []byte `json:"data,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Filename string `json:"filename,omitempty"`
}

type discordMessageSnapshot struct {
	ID               string                           `json:"id,omitempty"`
	ChannelID        string                           `json:"channel_id,omitempty"`
	GuildID          string                           `json:"guild_id,omitempty"`
	Type             int                              `json:"type,omitempty"`
	Content          string                           `json:"content,omitempty"`
	Author           *discordUserSnapshot             `json:"author,omitempty"`
	MentionUserIDs   []string                         `json:"mention_user_ids,omitempty"`
	Attachments      []discordAttachmentSnapshot      `json:"attachments,omitempty"`
	Embeds           []discordEmbedSnapshot           `json:"embeds,omitempty"`
	MessageReference *discordMessageReferenceSnapshot `json:"message_reference,omitempty"`
}

type discordUserSnapshot struct {
	ID  string `json:"id,omitempty"`
	Bot bool   `json:"bot,omitempty"`
}

type discordAttachmentSnapshot struct {
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	URL         string `json:"url,omitempty"`
}

type discordEmbedSnapshot struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	FooterText  string `json:"footer_text,omitempty"`
}

type discordMessageReferenceSnapshot struct {
	MessageID string `json:"message_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	GuildID   string `json:"guild_id,omitempty"`
}

type messageNodeStoreBackend interface {
	loadSnapshot(storeKey string, capacity int) (messageNodeStoreSnapshot, error)
	saveSnapshot(storeKey string, snapshot messageNodeStoreSnapshot) error
	close() error
}

type sqliteMessageNodeStoreBackend struct {
	database *sql.DB
}

func newSQLiteMessageNodeStoreBackend(
	ctx context.Context,
	connectionString string,
) (*sqliteMessageNodeStoreBackend, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil sqlite message history context: %w", os.ErrInvalid)
	}

	databasePath, err := resolveSQLiteDatabasePath(connectionString)
	if err != nil {
		return nil, err
	}

	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite message history database: %w", err)
	}

	configureSQLiteMessageNodeStorePool(database)

	err = database.PingContext(ctx)
	if err != nil {
		_ = database.Close()

		return nil, fmt.Errorf("ping sqlite message history database: %w", err)
	}

	_, err = database.ExecContext(ctx, "PRAGMA journal_mode=WAL")
	if err != nil {
		_ = database.Close()

		return nil, fmt.Errorf("enable sqlite WAL mode: %w", err)
	}

	_, err = database.ExecContext(ctx, "PRAGMA busy_timeout = 5000")
	if err != nil {
		_ = database.Close()

		return nil, fmt.Errorf("set sqlite busy timeout: %w", err)
	}

	err = ensureMessageNodeStoreTable(ctx, database)
	if err != nil {
		_ = database.Close()

		return nil, err
	}

	backend := new(sqliteMessageNodeStoreBackend)
	backend.database = database

	return backend, nil
}

func resolveSQLiteDatabasePath(connectionString string) (string, error) {
	trimmedConnectionString := strings.TrimSpace(connectionString)
	if trimmedConnectionString == "" {
		return "", errMessageNodeStorePersistenceDisabled
	}

	cleanedPath := filepath.Clean(trimmedConnectionString)
	parentDir := filepath.Dir(cleanedPath)

	if parentDir != "." && parentDir != "" {
		if err := os.MkdirAll(parentDir, sqliteDatabaseDirPerm); err != nil {
			return "", fmt.Errorf("create sqlite database directory %q: %w", parentDir, err)
		}
	}

	return cleanedPath, nil
}

func configureSQLiteMessageNodeStorePool(database *sql.DB) {
	if database == nil {
		return
	}

	database.SetMaxOpenConns(messageNodeStoreMaxOpenConns)
	database.SetMaxIdleConns(messageNodeStoreMaxOpenConns)
	database.SetConnMaxLifetime(messageNodeStoreConnMaxLifetime)
	database.SetConnMaxIdleTime(messageNodeStoreConnMaxIdleTime)
}

func ensureMessageNodeStoreTable(ctx context.Context, database *sql.DB) error {
	_, err := database.ExecContext(ctx, messageNodeStoreCreateTableSQL)
	if err != nil {
		return fmt.Errorf("create sqlite message history table %q: %w", messageNodeStoreTableName, err)
	}

	return nil
}

func (backend *sqliteMessageNodeStoreBackend) loadSnapshot(
	storeKey string,
	capacity int,
) (messageNodeStoreSnapshot, error) {
	var snapshot messageNodeStoreSnapshot

	var snapshotBytes []byte

	loadCtx, cancelLoad := context.WithTimeout(context.Background(), messageNodeStoreStatementTimeout)
	defer cancelLoad()

	err := backend.database.QueryRowContext(
		loadCtx,
		messageNodeStoreSelectSQL,
		storeKey,
	).Scan(
		&snapshot.Version,
		&snapshotBytes,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return messageNodeStoreSnapshot{}, os.ErrNotExist
		}

		return messageNodeStoreSnapshot{}, fmt.Errorf(
			"query message history from sqlite table %q: %w",
			messageNodeStoreTableName,
			err,
		)
	}

	if snapshot.Version != messageNodeStoreSnapshotVersion {
		return messageNodeStoreSnapshot{}, fmt.Errorf(
			"unsupported message history version %d: %w",
			snapshot.Version,
			os.ErrInvalid,
		)
	}

	err = decodeMessageNodeSnapshotJSON(snapshotBytes, &snapshot.Nodes)
	if err != nil {
		return messageNodeStoreSnapshot{}, fmt.Errorf("decode message history snapshot JSON: %w", err)
	}

	if snapshot.Nodes == nil {
		snapshot.Nodes = make(map[string]messageNodeSnapshot)
	}

	snapshot.Nodes = trimSnapshotNodes(snapshot.Nodes, capacity)

	return snapshot, nil
}

func (backend *sqliteMessageNodeStoreBackend) saveSnapshot(
	storeKey string,
	snapshot messageNodeStoreSnapshot,
) error {
	snapshotBytes, err := encodeMessageNodeSnapshotJSON(snapshot.Nodes)
	if err != nil {
		return fmt.Errorf("encode message history snapshot JSON: %w", err)
	}

	saveCtx, cancelSave := context.WithTimeout(context.Background(), messageNodeStoreStatementTimeout)
	defer cancelSave()

	_, err = backend.database.ExecContext(
		saveCtx,
		messageNodeStoreUpsertSQL,
		storeKey,
		snapshot.Version,
		snapshotBytes,
	)
	if err != nil {
		return fmt.Errorf(
			"upsert message history into sqlite table %q: %w",
			messageNodeStoreTableName,
			err,
		)
	}

	return nil
}

func decodeMessageNodeSnapshotJSON(
	snapshotBytes []byte,
	nodes *map[string]messageNodeSnapshot,
) error {
	payload := new(messageNodeSnapshotPayload)

	err := json.Unmarshal(snapshotBytes, payload)
	if err != nil {
		return fmt.Errorf("unmarshal message history snapshot payload: %w", err)
	}

	*nodes = payload.Nodes

	return nil
}

func encodeMessageNodeSnapshotJSON(
	nodes map[string]messageNodeSnapshot,
) ([]byte, error) {
	if nodes == nil {
		nodes = make(map[string]messageNodeSnapshot)
	}

	sanitizedPayload := sanitizeMessageNodeSnapshotPayload(messageNodeSnapshotPayload{Nodes: nodes})

	payloadBytes, err := json.Marshal(sanitizedPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal message history snapshot payload: %w", err)
	}

	if len(payloadBytes) <= messageNodeStorePersistSafeMaxBytes {
		return payloadBytes, nil
	}

	fittedNodes := trimSnapshotToFit(nodes, messageNodeStorePersistSafeMaxBytes)

	sanitizedPayload = sanitizeMessageNodeSnapshotPayload(messageNodeSnapshotPayload{Nodes: fittedNodes})

	payloadBytes, err = json.Marshal(sanitizedPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal message history snapshot payload: %w", err)
	}

	if len(payloadBytes) > messageNodeStorePersistSafeMaxBytes {
		return nil, fmt.Errorf("message history snapshot still exceeds %d bytes after trimming", messageNodeStorePersistSafeMaxBytes)
	}

	return payloadBytes, nil
}

func trimSnapshotToFit(nodes map[string]messageNodeSnapshot, maxBytes int) map[string]messageNodeSnapshot {
	if nodes == nil {
		return nil
	}

	if maxBytes <= 0 {
		return maps.Clone(nodes)
	}

	if len(nodes) == 0 {
		return maps.Clone(nodes)
	}

	if marshaledSizeFits(nodes, maxBytes) {
		return maps.Clone(nodes)
	}

	if len(nodes) == 1 {
		return truncateSingleHugeNodeToFit(maps.Clone(nodes), maxBytes)
	}

	messageIDs := make([]string, 0, len(nodes))
	for messageID := range nodes {
		messageIDs = append(messageIDs, messageID)
	}

	slices.SortFunc(messageIDs, compareMessageIDs)

	// Dropping an oldest-prefix only shrinks the JSON payload, so the fit is
	// monotonic in the drop count. Binary-search the smallest prefix drop that
	// fits instead of re-marshaling after every single eviction.
	low, high := 1, len(messageIDs)
	best := len(messageIDs)

	for low <= high {
		mid := (low + high) / trimSnapshotBinarySearchDivisor
		kept := make(map[string]messageNodeSnapshot, len(nodes)-mid)

		for _, messageID := range messageIDs[mid:] {
			kept[messageID] = nodes[messageID]
		}

		if len(kept) == 1 {
			truncated := truncateSingleHugeNodeToFit(kept, maxBytes)
			if marshaledSizeFits(truncated, maxBytes) {
				return truncated
			}

			low = mid + 1

			continue
		}

		if marshaledSizeFits(kept, maxBytes) {
			best = mid
			high = mid - 1
		} else {
			low = mid + 1
		}
	}

	if best >= len(messageIDs) {
		// Only the newest node could fit; truncate it to the byte budget.
		kept := map[string]messageNodeSnapshot{messageIDs[len(messageIDs)-1]: nodes[messageIDs[len(messageIDs)-1]]}

		return truncateSingleHugeNodeToFit(kept, maxBytes)
	}

	kept := make(map[string]messageNodeSnapshot, len(nodes)-best)
	for _, messageID := range messageIDs[best:] {
		kept[messageID] = nodes[messageID]
	}

	return kept
}

func marshaledSizeFits(nodes map[string]messageNodeSnapshot, maxBytes int) bool {
	sanitizedPayload := sanitizeMessageNodeSnapshotPayload(messageNodeSnapshotPayload{Nodes: nodes})

	payloadBytes, err := json.Marshal(sanitizedPayload)
	if err != nil {
		return true
	}

	return len(payloadBytes) <= maxBytes
}

func truncateStringToBytes(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}

	truncated := value[:maxBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}

	return truncated
}

func truncateSingleHugeNodeToFit(nodes map[string]messageNodeSnapshot, maxBytes int) map[string]messageNodeSnapshot {
	if len(nodes) != 1 {
		return nodes
	}

	for messageID, snapshot := range nodes {
		snapshot.Text = truncateStringToBytes(snapshot.Text, 64*1024)
		snapshot.ThinkingText = truncateStringToBytes(snapshot.ThinkingText, 64*1024)
		snapshot.URLScanText = truncateStringToBytes(snapshot.URLScanText, 32*1024)
		snapshot.GistURL = truncateStringToBytes(snapshot.GistURL, 4*1024)

		truncatedMedia := make([]contentPartSnapshot, 0, len(snapshot.Media))
		for _, part := range snapshot.Media {
			part.Text = truncateStringToBytes(part.Text, 64*1024)

			part.ImageURL = truncateStringToBytes(part.ImageURL, 512*1024)
			if len(part.Data) > 256*1024 {
				part.Data = part.Data[:256*1024]
			}

			truncatedMedia = append(truncatedMedia, part)
		}

		snapshot.Media = truncatedMedia

		if snapshot.SearchMetadata != nil {
			metadataCopy := *snapshot.SearchMetadata
			for index := range metadataCopy.Results {
				metadataCopy.Results[index].Text = truncateStringToBytes(metadataCopy.Results[index].Text, 32*1024)
			}

			snapshot.SearchMetadata = &metadataCopy
		}

		if snapshot.ParentMessage != nil {
			parentCopy := *snapshot.ParentMessage
			parentCopy.Content = truncateStringToBytes(parentCopy.Content, 16*1024)
			snapshot.ParentMessage = &parentCopy
		}

		nodes[messageID] = snapshot

		sanitizedPayload := sanitizeMessageNodeSnapshotPayload(messageNodeSnapshotPayload{Nodes: nodes})

		payloadBytes, err := json.Marshal(sanitizedPayload)
		if err == nil && len(payloadBytes) <= maxBytes {
			return nodes
		}

		if len(payloadBytes) > maxBytes {
			return make(map[string]messageNodeSnapshot)
		}

		return nodes
	}

	return nodes
}

func sanitizeMessageNodeSnapshotPayload(
	payload messageNodeSnapshotPayload,
) messageNodeSnapshotPayload {
	if payload.Nodes == nil {
		return messageNodeSnapshotPayload{Nodes: nil}
	}

	sanitizedNodes := make(map[string]messageNodeSnapshot, len(payload.Nodes))
	for messageID, snapshot := range payload.Nodes {
		sanitizedNodes[sanitizeSnapshotJSONString(messageID)] = sanitizeMessageNodeSnapshot(snapshot)
	}

	return messageNodeSnapshotPayload{Nodes: sanitizedNodes}
}

func sanitizeMessageNodeSnapshot(snapshot messageNodeSnapshot) messageNodeSnapshot {
	snapshot.Role = sanitizeSnapshotJSONString(snapshot.Role)
	snapshot.Text = sanitizeSnapshotJSONString(snapshot.Text)
	snapshot.ThinkingText = sanitizeSnapshotJSONString(snapshot.ThinkingText)
	snapshot.URLScanText = sanitizeSnapshotJSONString(snapshot.URLScanText)
	snapshot.GistURL = sanitizeSnapshotJSONString(snapshot.GistURL)
	snapshot.ProviderResponseID = sanitizeSnapshotJSONString(snapshot.ProviderResponseID)
	snapshot.ProviderResponseModel = sanitizeSnapshotJSONString(snapshot.ProviderResponseModel)
	snapshot.Media = sanitizeContentPartSnapshots(snapshot.Media)
	snapshot.SearchMetadata = sanitizeSearchMetadata(snapshot.SearchMetadata)
	snapshot.ParentMessage = sanitizeDiscordMessageSnapshot(snapshot.ParentMessage)

	return snapshot
}

func sanitizeContentPartSnapshots(snapshots []contentPartSnapshot) []contentPartSnapshot {
	if snapshots == nil {
		return nil
	}

	sanitizedSnapshots := make([]contentPartSnapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		sanitizedSnapshots = append(sanitizedSnapshots, sanitizeContentPartSnapshot(snapshot))
	}

	return sanitizedSnapshots
}

func sanitizeContentPartSnapshot(snapshot contentPartSnapshot) contentPartSnapshot {
	snapshot.Type = sanitizeSnapshotJSONString(snapshot.Type)
	snapshot.Text = sanitizeSnapshotJSONString(snapshot.Text)
	snapshot.ImageURL = sanitizeSnapshotJSONString(snapshot.ImageURL)
	snapshot.MIMEType = sanitizeSnapshotJSONString(snapshot.MIMEType)
	snapshot.Filename = sanitizeSnapshotJSONString(snapshot.Filename)

	return snapshot
}

func sanitizeSearchMetadata(metadata *searchMetadata) *searchMetadata {
	if metadata == nil {
		return nil
	}

	return &searchMetadata{
		Queries:             sanitizeSnapshotJSONStrings(metadata.Queries),
		Results:             sanitizeWebSearchResults(metadata.Results),
		MaxURLs:             metadata.MaxURLs,
		VisualSearchSources: sanitizeVisualSearchSourceGroups(metadata.VisualSearchSources),
	}
}

func sanitizeWebSearchResults(results []webSearchResult) []webSearchResult {
	if results == nil {
		return nil
	}

	sanitizedResults := make([]webSearchResult, 0, len(results))
	for _, result := range results {
		sanitizedResults = append(sanitizedResults, webSearchResult{
			Query: sanitizeSnapshotJSONString(result.Query),
			Text:  sanitizeSnapshotJSONString(result.Text),
		})
	}

	return sanitizedResults
}

func sanitizeVisualSearchSourceGroups(
	sourceGroups []visualSearchSourceGroup,
) []visualSearchSourceGroup {
	if sourceGroups == nil {
		return nil
	}

	sanitizedGroups := make([]visualSearchSourceGroup, 0, len(sourceGroups))
	for _, sourceGroup := range sourceGroups {
		sanitizedGroups = append(sanitizedGroups, visualSearchSourceGroup{
			Label:   sanitizeSnapshotJSONString(sourceGroup.Label),
			Sources: sanitizeSearchSources(sourceGroup.Sources),
		})
	}

	return sanitizedGroups
}

func sanitizeSearchSources(sources []searchSource) []searchSource {
	if sources == nil {
		return nil
	}

	sanitizedSources := make([]searchSource, 0, len(sources))
	for _, source := range sources {
		sanitizedSources = append(sanitizedSources, searchSource{
			Title: sanitizeSnapshotJSONString(source.Title),
			URL:   sanitizeSnapshotJSONString(source.URL),
		})
	}

	return sanitizedSources
}

func sanitizeDiscordMessageSnapshot(message *discordMessageSnapshot) *discordMessageSnapshot {
	if message == nil {
		return nil
	}

	return &discordMessageSnapshot{
		ID:               sanitizeSnapshotJSONString(message.ID),
		ChannelID:        sanitizeSnapshotJSONString(message.ChannelID),
		GuildID:          sanitizeSnapshotJSONString(message.GuildID),
		Type:             message.Type,
		Content:          sanitizeSnapshotJSONString(message.Content),
		Author:           sanitizeDiscordUserSnapshot(message.Author),
		MentionUserIDs:   sanitizeSnapshotJSONStrings(message.MentionUserIDs),
		Attachments:      sanitizeDiscordAttachmentSnapshots(message.Attachments),
		Embeds:           sanitizeDiscordEmbedSnapshots(message.Embeds),
		MessageReference: sanitizeDiscordMessageReferenceSnapshot(message.MessageReference),
	}
}

func sanitizeDiscordUserSnapshot(user *discordUserSnapshot) *discordUserSnapshot {
	if user == nil {
		return nil
	}

	return &discordUserSnapshot{
		ID:  sanitizeSnapshotJSONString(user.ID),
		Bot: user.Bot,
	}
}

func sanitizeDiscordAttachmentSnapshots(
	attachments []discordAttachmentSnapshot,
) []discordAttachmentSnapshot {
	if attachments == nil {
		return nil
	}

	sanitizedAttachments := make([]discordAttachmentSnapshot, 0, len(attachments))
	for _, attachment := range attachments {
		sanitizedAttachments = append(sanitizedAttachments, discordAttachmentSnapshot{
			Filename:    sanitizeSnapshotJSONString(attachment.Filename),
			ContentType: sanitizeSnapshotJSONString(attachment.ContentType),
			URL:         sanitizeSnapshotJSONString(attachment.URL),
		})
	}

	return sanitizedAttachments
}

func sanitizeDiscordEmbedSnapshots(embeds []discordEmbedSnapshot) []discordEmbedSnapshot {
	if embeds == nil {
		return nil
	}

	sanitizedEmbeds := make([]discordEmbedSnapshot, 0, len(embeds))
	for _, embed := range embeds {
		sanitizedEmbeds = append(sanitizedEmbeds, discordEmbedSnapshot{
			Title:       sanitizeSnapshotJSONString(embed.Title),
			Description: sanitizeSnapshotJSONString(embed.Description),
			FooterText:  sanitizeSnapshotJSONString(embed.FooterText),
		})
	}

	return sanitizedEmbeds
}

func sanitizeDiscordMessageReferenceSnapshot(
	reference *discordMessageReferenceSnapshot,
) *discordMessageReferenceSnapshot {
	if reference == nil {
		return nil
	}

	return &discordMessageReferenceSnapshot{
		MessageID: sanitizeSnapshotJSONString(reference.MessageID),
		ChannelID: sanitizeSnapshotJSONString(reference.ChannelID),
		GuildID:   sanitizeSnapshotJSONString(reference.GuildID),
	}
}

func sanitizeSnapshotJSONStrings(values []string) []string {
	if values == nil {
		return nil
	}

	sanitizedValues := make([]string, 0, len(values))
	for _, value := range values {
		sanitizedValues = append(sanitizedValues, sanitizeSnapshotJSONString(value))
	}

	return sanitizedValues
}

func sanitizeSnapshotJSONString(value string) string {
	if strings.IndexByte(value, 0) == -1 && utf8.ValidString(value) {
		return value
	}

	sanitizedValue := strings.ToValidUTF8(value, snapshotJSONTextReplacement)

	return strings.ReplaceAll(
		sanitizedValue,
		"\x00",
		snapshotJSONTextReplacement,
	)
}

func (backend *sqliteMessageNodeStoreBackend) close() error {
	if backend == nil || backend.database == nil {
		return nil
	}

	err := backend.database.Close()
	if err != nil {
		return fmt.Errorf("close sqlite message history database: %w", err)
	}

	return nil
}

func newPersistentMessageNodeStore(
	capacity int,
	storeKey string,
	backend messageNodeStoreBackend,
) (*messageNodeStore, error) {
	store := newMessageNodeStore(capacity)

	trimmedStoreKey := strings.TrimSpace(storeKey)
	if trimmedStoreKey == "" || backend == nil {
		return store, nil
	}

	store.backendMu.Lock()
	store.storeKey = trimmedStoreKey
	store.backend = backend
	store.backendMu.Unlock()

	snapshot, err := backend.loadSnapshot(trimmedStoreKey, capacity)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			store.startSaveWorker()

			return store, nil
		}

		_ = backend.close()

		return nil, annotateMessageHistoryPersistenceError(
			"load persisted message history",
			trimmedStoreKey,
			err,
		)
	}

	store.nodes = snapshotNodesToStoreNodes(snapshot.Nodes)
	store.snapshotCache = maps.Clone(snapshot.Nodes)
	store.startSaveWorker()

	return store, nil
}
func (store *messageNodeStore) attachPersistentBackend(
	storeKey string,
	backend messageNodeStoreBackend,
	loaded map[string]messageNodeSnapshot,
) bool {
	if store == nil {
		if backend != nil {
			_ = backend.close()
		}

		return false
	}

	trimmedStoreKey := strings.TrimSpace(storeKey)
	if trimmedStoreKey == "" || backend == nil {
		if backend != nil {
			_ = backend.close()
		}

		return false
	}

	if store.closed.Load() {
		_ = backend.close()

		return false
	}

	store.backendMu.Lock()
	if store.closed.Load() || store.backend != nil {
		store.backendMu.Unlock()

		_ = backend.close()

		return false
	}

	store.storeKey = trimmedStoreKey
	store.backend = backend
	store.backendMu.Unlock()

	liveCount := store.mergeLoadedHistory(loaded)
	store.startSaveWorker()
	store.evictExcess()

	if liveCount > 0 || store.dirty.Load() {
		store.persistBestEffort()
	}

	return true
}

func (store *messageNodeStore) mergeLoadedHistory(loaded map[string]messageNodeSnapshot) int {
	liveNodes := make(map[string]*messageNode)

	store.mu.Lock()
	if store.nodes == nil {
		store.nodes = make(map[string]*messageNode, len(loaded))
	}

	for messageID, node := range store.nodes {
		liveNodes[messageID] = node
	}

	for messageID, snapshot := range loaded {
		if _, ok := store.nodes[messageID]; !ok {
			store.nodes[messageID] = snapshot.messageNode()
		}
	}

	store.mu.Unlock()

	store.snapshotMu.Lock()
	if store.snapshotCache == nil {
		store.snapshotCache = make(map[string]messageNodeSnapshot, len(loaded))
	}

	for messageID, snapshot := range loaded {
		if _, isLive := liveNodes[messageID]; isLive {
			continue
		}

		if _, ok := store.snapshotCache[messageID]; !ok {
			store.snapshotCache[messageID] = snapshot
		}
	}

	for messageID, node := range liveNodes {
		if node == nil {
			continue
		}

		if node.mu.TryLock() {
			if snapshot, ok := messageNodeSnapshotFromLockedNode(node); ok {
				store.snapshotCache[messageID] = snapshot
			}

			node.mu.Unlock()
		}
	}

	store.snapshotMu.Unlock()

	return len(liveNodes)
}

func hydrateMessageHistoryInBackground(
	store *messageNodeStore,
	capacity int,
	configPath, configuredStoreKey, connectionString string,
) {
	if store == nil || strings.TrimSpace(connectionString) == "" {
		return
	}

	if store.closed.Load() {
		return
	}

	storeKey := messageNodeStoreKey(configPath, configuredStoreKey)

	loadCtx, cancelLoad := context.WithTimeout(context.Background(), messageNodeStoreStatementTimeout)
	defer cancelLoad()

	backend, err := newSQLiteMessageNodeStoreBackend(loadCtx, connectionString)
	if err != nil {
		if errors.Is(err, errMessageNodeStorePersistenceDisabled) {
			return
		}

		logWarn("configure persisted message history", err, "store_key", storeKey)

		return
	}

	snapshot, err := backend.loadSnapshot(storeKey, capacity)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			store.attachPersistentBackend(storeKey, backend, nil)

			return
		}

		logWarn("load persisted message history", err, "store_key", storeKey)

		_ = backend.close()

		return
	}

	store.attachPersistentBackend(storeKey, backend, snapshot.Nodes)
}

func defaultMessageNodeStoreKey(configPath string) string {
	resolvedConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		resolvedConfigPath = configPath
	}

	hash := sha256.Sum256([]byte(filepath.Clean(resolvedConfigPath)))

	return fmt.Sprintf("message-history-%x", hash[:8])
}

func messageNodeStoreKey(configPath, configuredStoreKey string) string {
	trimmedStoreKey := strings.TrimSpace(configuredStoreKey)
	if trimmedStoreKey != "" {
		return trimmedStoreKey
	}

	return defaultMessageNodeStoreKey(configPath)
}

func (store *messageNodeStore) backendAndKey() (messageNodeStoreBackend, string) {
	if store == nil {
		return nil, ""
	}

	store.backendMu.RLock()
	defer store.backendMu.RUnlock()

	return store.backend, store.storeKey
}

// wrapHistoryBackend swaps the attached backend in place, keeping the store
// key and save worker. Used when Redis chains after SQLite already attached:
// the SQLite handle moves inside the chained backend instead of being refused
// and closed.
func (store *messageNodeStore) wrapHistoryBackend(storeKey string, backend messageNodeStoreBackend) bool {
	if store == nil || backend == nil {
		if backend != nil {
			_ = backend.close()
		}

		return false
	}

	trimmedStoreKey := strings.TrimSpace(storeKey)
	if trimmedStoreKey == "" {
		_ = backend.close()

		return false
	}

	store.backendMu.Lock()
	defer store.backendMu.Unlock()

	if store.closed.Load() || store.backend == nil || strings.TrimSpace(store.storeKey) == "" {
		_ = backend.close()

		return false
	}

	store.storeKey = trimmedStoreKey
	store.backend = backend

	return true
}

func (store *messageNodeStore) persistBestEffort() {
	backend, storeKey := store.backendAndKey()
	if store == nil || strings.TrimSpace(storeKey) == "" || backend == nil {
		return
	}

	store.dirty.Store(true)

	store.saveWorkerMu.Lock()
	saveRequests := store.saveRequests
	store.saveWorkerMu.Unlock()

	if saveRequests == nil {
		return
	}

	select {
	case saveRequests <- struct{}{}:
	default:
	}
}

func (store *messageNodeStore) persist() error {
	backend, storeKey := store.backendAndKey()
	if strings.TrimSpace(storeKey) == "" || backend == nil {
		return nil
	}

	store.saveMu.Lock()
	defer store.saveMu.Unlock()

	if !store.dirty.Swap(false) {
		return nil
	}

	snapshot := store.snapshot()

	err := backend.saveSnapshot(storeKey, snapshot)
	if err != nil {
		store.dirty.Store(true)

		return annotateMessageHistoryPersistenceError(
			"persist message history",
			storeKey,
			err,
		)
	}

	if store.dirty.Load() {
		// Mutated during the write: the cache already holds fresher entries
		// than this snapshot, so leave them and stay dirty for the next flush.
		return nil
	}

	store.setSnapshotCache(snapshot.Nodes)

	return nil
}

func (store *messageNodeStore) snapshot() messageNodeStoreSnapshot {
	cachedSnapshots := store.snapshotCacheCopy()
	nodeEntries := store.nodeEntries()
	nodes := make(map[string]messageNodeSnapshot, len(nodeEntries))

	for messageID, node := range nodeEntries {
		if node == nil {
			continue
		}

		if node.mu.TryLock() {
			snapshot, ok := messageNodeSnapshotFromLockedNode(node)
			node.mu.Unlock()

			if ok {
				nodes[messageID] = snapshot
			}

			continue
		}

		cachedSnapshot, ok := cachedSnapshots[messageID]
		if ok {
			nodes[messageID] = cachedSnapshot
		}
	}

	return messageNodeStoreSnapshot{
		Version: messageNodeStoreSnapshotVersion,
		Nodes:   nodes,
	}
}

func (store *messageNodeStore) cacheLockedNode(messageID string, node *messageNode) {
	store.cacheLockedNodeLocked(messageID, node)
}

func (store *messageNodeStore) hasStoreKey() bool {
	_, storeKey := store.backendAndKey()

	return strings.TrimSpace(storeKey) != ""
}

func (store *messageNodeStore) cacheLockedNodeLocked(messageID string, node *messageNode) {
	if node == nil || !store.hasStoreKey() {
		return
	}

	snapshot, ok := messageNodeSnapshotFromLockedNode(node)
	if !ok {
		return
	}

	trimmedMessageID := strings.TrimSpace(messageID)
	if trimmedMessageID == "" {
		return
	}

	store.snapshotMu.Lock()
	store.snapshotCache[trimmedMessageID] = snapshot
	store.snapshotMu.Unlock()
	store.dirty.Store(true)
}

func (store *messageNodeStore) deleteCachedSnapshot(messageID string) {
	if !store.hasStoreKey() {
		return
	}

	trimmedMessageID := strings.TrimSpace(messageID)
	if trimmedMessageID == "" {
		return
	}

	store.snapshotMu.Lock()
	delete(store.snapshotCache, trimmedMessageID)
	store.snapshotMu.Unlock()
	store.dirty.Store(true)
}

func (store *messageNodeStore) snapshotCacheCopy() map[string]messageNodeSnapshot {
	store.snapshotMu.Lock()
	defer store.snapshotMu.Unlock()

	return maps.Clone(store.snapshotCache)
}

func (store *messageNodeStore) setSnapshotCache(snapshotNodes map[string]messageNodeSnapshot) {
	store.snapshotMu.Lock()
	store.snapshotCache = maps.Clone(snapshotNodes)
	store.snapshotMu.Unlock()
}

func (store *messageNodeStore) nodeEntries() map[string]*messageNode {
	store.mu.Lock()
	defer store.mu.Unlock()

	return maps.Clone(store.nodes)
}
func (store *messageNodeStore) close() error {
	if store == nil {
		return nil
	}

	if store.closed.Swap(true) {
		return nil
	}

	store.stopSaveWorker()

	persistErr := store.persist()

	for range 2 {
		if persistErr != nil || !store.dirty.Load() {
			break
		}

		persistErr = store.persist()
	}

	backend, _ := store.backendAndKey()

	var closeErr error
	if backend != nil {
		closeErr = backend.close()
	}

	store.backendMu.Lock()
	store.backend = nil
	store.backendMu.Unlock()

	switch {
	case persistErr != nil && closeErr != nil:
		return errors.Join(persistErr, closeErr)
	case persistErr != nil:
		return persistErr
	case closeErr != nil:
		return closeErr
	}

	return nil
}

func (store *messageNodeStore) startSaveWorker() {
	backend, storeKey := store.backendAndKey()
	if store == nil || strings.TrimSpace(storeKey) == "" || backend == nil {
		return
	}

	store.saveWorkerMu.Lock()
	defer store.saveWorkerMu.Unlock()

	if store.closed.Load() || store.saveRequests != nil {
		return
	}

	saveRequests := make(chan struct{}, 1)
	saveStop := make(chan struct{})
	saveDone := make(chan struct{})

	store.saveRequests = saveRequests
	store.saveStop = saveStop
	store.saveDone = saveDone

	go store.runSaveWorker(saveRequests, saveStop, saveDone)
}

func (store *messageNodeStore) stopSaveWorker() {
	if store == nil {
		return
	}

	store.saveWorkerMu.Lock()
	saveStop := store.saveStop
	saveDone := store.saveDone
	store.saveRequests = nil
	store.saveStop = nil
	store.saveDone = nil
	store.saveWorkerMu.Unlock()

	if saveStop == nil || saveDone == nil {
		return
	}

	close(saveStop)
	<-saveDone
}

func (store *messageNodeStore) runSaveWorker(
	saveRequests <-chan struct{},
	saveStop <-chan struct{},
	saveDone chan<- struct{},
) {
	defer close(saveDone)

	persistDelay := max(store.persistDelay, time.Duration(0))

	timer := time.NewTimer(persistDelay)
	stopTimer(timer)

	persistPending := false

	for {
		var timerChannel <-chan time.Time
		if persistPending {
			timerChannel = timer.C
		}

		select {
		case <-saveRequests:
			persistPending = true

			resetTimer(timer, persistDelay)
		case <-timerChannel:
			persistPending = false

			err := store.persist()
			if err != nil {
				_, logStoreKey := store.backendAndKey()
				logWarn(
					"persist message history",
					err,
					"store_key",
					logStoreKey,
				)
			}
		case <-saveStop:
			stopTimer(timer)

			return
		}
	}
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}

	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	stopTimer(timer)
	timer.Reset(delay)
}

func annotateMessageHistoryPersistenceError(
	operation string,
	storeKey string,
	err error,
) error {
	trimmedOperation := strings.TrimSpace(operation)
	if trimmedOperation == "" || err == nil {
		return err
	}

	trimmedStoreKey := strings.TrimSpace(storeKey)

	if !isSQLiteCorruptionError(err) {
		if trimmedStoreKey == "" {
			return fmt.Errorf("%s: %w", trimmedOperation, err)
		}

		return fmt.Errorf("%s for store key %q: %w", trimmedOperation, trimmedStoreKey, err)
	}

	if trimmedStoreKey == "" {
		return fmt.Errorf(
			"%s: sqlite message history storage appears corrupted; "+
				"delete the database file before re-enabling persistence: %w",
			trimmedOperation,
			err,
		)
	}

	return fmt.Errorf(
		"%s for store key %q: sqlite message history storage appears corrupted; "+
			"delete the database file before re-enabling persistence: %w",
		trimmedOperation,
		trimmedStoreKey,
		err,
	)
}

func isSQLiteCorruptionError(err error) bool {
	if err == nil {
		return false
	}

	lowered := strings.ToLower(err.Error())

	return strings.Contains(lowered, sqliteCorruptErrorSubstring) ||
		strings.Contains(lowered, sqliteNotDatabaseErrorSubstring)
}

func trimSnapshotNodes(
	snapshotNodes map[string]messageNodeSnapshot,
	capacity int,
) map[string]messageNodeSnapshot {
	if len(snapshotNodes) <= capacity || capacity <= 0 {
		return maps.Clone(snapshotNodes)
	}

	messageIDs := make([]string, 0, len(snapshotNodes))
	for messageID := range snapshotNodes {
		messageIDs = append(messageIDs, messageID)
	}

	sortMessageIDs(messageIDs)
	keepMessageIDs := messageIDs[len(messageIDs)-capacity:]
	trimmedNodes := make(map[string]messageNodeSnapshot, len(keepMessageIDs))

	for _, messageID := range keepMessageIDs {
		trimmedNodes[messageID] = snapshotNodes[messageID]
	}

	return trimmedNodes
}

func snapshotNodesToStoreNodes(
	snapshotNodes map[string]messageNodeSnapshot,
) map[string]*messageNode {
	storeNodes := make(map[string]*messageNode, len(snapshotNodes))

	for messageID, snapshotNode := range snapshotNodes {
		storeNodes[messageID] = snapshotNode.messageNode()
	}

	return storeNodes
}

func messageNodeSnapshotFromLockedNode(node *messageNode) (messageNodeSnapshot, bool) {
	var emptySnapshot messageNodeSnapshot

	if node == nil || !node.initialized {
		return emptySnapshot, false
	}

	mediaSnapshots := make([]contentPartSnapshot, 0, len(node.media))
	for _, part := range node.media {
		partSnapshot, ok := contentPartSnapshotFromPart(part)
		if !ok {
			continue
		}

		mediaSnapshots = append(mediaSnapshots, partSnapshot)
	}

	snapshot := messageNodeSnapshot{
		Role:                     node.role,
		Text:                     node.text,
		ThinkingText:             node.thinkingText,
		URLScanText:              node.urlScanText,
		GistURL:                  node.gistURL,
		ProviderResponseID:       node.providerResponseID,
		ProviderResponseModel:    node.providerResponseModel,
		Media:                    mediaSnapshots,
		SearchMetadata:           cloneSearchMetadata(node.searchMetadata),
		HasBadAttachments:        node.hasBadAttachments,
		AttachmentDownloadFailed: node.attachmentDownloadFailed,
		FetchParentFailed:        node.fetchParentFailed,
		ParentMessage:            newDiscordMessageSnapshot(node.parentMessage),
		Initialized:              node.initialized,
	}

	return snapshot, true
}

func (snapshot messageNodeSnapshot) messageNode() *messageNode {
	node := new(messageNode)
	node.role = snapshot.Role
	node.text = snapshot.Text
	node.thinkingText = snapshot.ThinkingText
	node.urlScanText = snapshot.URLScanText
	node.gistURL = snapshot.GistURL
	node.providerResponseID = snapshot.ProviderResponseID
	node.providerResponseModel = snapshot.ProviderResponseModel
	node.media = make([]contentPart, 0, len(snapshot.Media))

	for _, mediaSnapshot := range snapshot.Media {
		part, ok := mediaSnapshot.contentPart()
		if !ok {
			continue
		}

		node.media = append(node.media, part)
	}

	node.searchMetadata = cloneSearchMetadata(snapshot.SearchMetadata)
	node.hasBadAttachments = snapshot.HasBadAttachments
	node.attachmentDownloadFailed = snapshot.AttachmentDownloadFailed
	node.fetchParentFailed = snapshot.FetchParentFailed
	node.parentMessage = snapshot.ParentMessage.discordMessage()
	node.initialized = snapshot.Initialized

	return node
}

func contentPartSnapshotFromPart(part contentPart) (contentPartSnapshot, bool) {
	var emptySnapshot contentPartSnapshot

	partType, _ := part["type"].(string)
	if strings.TrimSpace(partType) == "" {
		return emptySnapshot, false
	}

	var snapshot contentPartSnapshot

	snapshot.Type = partType

	switch partType {
	case contentTypeText:
		snapshot.Text, _ = part["text"].(string)

		return snapshot, true
	case contentTypeImageURL:
		imageURL, err := contentPartImageURL(part)
		if err != nil {
			return emptySnapshot, false
		}

		snapshot.ImageURL = imageURL

		return snapshot, true
	case contentTypeAudioData, contentTypeDocument, contentTypeFileData, contentTypeVideoData:
		attachmentBytes, _ := part[contentFieldBytes].([]byte)
		snapshot.Data = make([]byte, len(attachmentBytes))
		copy(snapshot.Data, attachmentBytes)
		snapshot.MIMEType, _ = part[contentFieldMIMEType].(string)
		snapshot.Filename, _ = part[contentFieldFilename].(string)

		return snapshot, true
	default:
		return emptySnapshot, false
	}
}

func (snapshot contentPartSnapshot) contentPart() (contentPart, bool) {
	if strings.TrimSpace(snapshot.Type) == "" {
		return nil, false
	}

	part := contentPart{messageTypeKey: snapshot.Type}

	switch snapshot.Type {
	case contentTypeText:
		part[messageTextKey] = snapshot.Text
	case contentTypeImageURL:
		part["image_url"] = map[string]string{messageURLKey: snapshot.ImageURL}
	case contentTypeAudioData, contentTypeDocument, contentTypeFileData, contentTypeVideoData:
		attachmentBytes := make([]byte, len(snapshot.Data))
		copy(attachmentBytes, snapshot.Data)
		part[contentFieldBytes] = attachmentBytes

		part[contentFieldMIMEType] = snapshot.MIMEType
		if strings.TrimSpace(snapshot.Filename) != "" {
			part[contentFieldFilename] = snapshot.Filename
		}
	default:
		return nil, false
	}

	return part, true
}

func newDiscordMessageSnapshot(message *discordgo.Message) *discordMessageSnapshot {
	if message == nil {
		return nil
	}

	snapshot := new(discordMessageSnapshot)
	snapshot.ID = message.ID
	snapshot.ChannelID = message.ChannelID
	snapshot.GuildID = message.GuildID
	snapshot.Type = int(message.Type)
	snapshot.Content = message.Content

	if message.Author != nil {
		authorSnapshot := new(discordUserSnapshot)
		authorSnapshot.ID = message.Author.ID
		authorSnapshot.Bot = message.Author.Bot
		snapshot.Author = authorSnapshot
	}

	snapshot.MentionUserIDs = mentionedUserIDs(message.Mentions)
	snapshot.Attachments = attachmentSnapshots(message.Attachments)

	snapshot.Embeds = embedSnapshots(message.Embeds)
	if message.MessageReference != nil {
		referenceSnapshot := new(discordMessageReferenceSnapshot)
		referenceSnapshot.MessageID = message.MessageReference.MessageID
		referenceSnapshot.ChannelID = message.MessageReference.ChannelID
		referenceSnapshot.GuildID = message.MessageReference.GuildID
		snapshot.MessageReference = referenceSnapshot
	}

	return snapshot
}

func (snapshot *discordMessageSnapshot) discordMessage() *discordgo.Message {
	if snapshot == nil {
		return nil
	}

	message := new(discordgo.Message)
	message.ID = snapshot.ID
	message.ChannelID = snapshot.ChannelID
	message.GuildID = snapshot.GuildID
	message.Type = discordgo.MessageType(snapshot.Type)
	message.Content = snapshot.Content

	if snapshot.Author != nil {
		author := new(discordgo.User)
		author.ID = snapshot.Author.ID
		author.Bot = snapshot.Author.Bot
		message.Author = author
	}

	message.Mentions = mentionedUsers(snapshot.MentionUserIDs)
	message.Attachments = attachmentsFromSnapshots(snapshot.Attachments)

	message.Embeds = embedsFromSnapshots(snapshot.Embeds)
	if snapshot.MessageReference != nil {
		reference := new(discordgo.MessageReference)
		reference.MessageID = snapshot.MessageReference.MessageID
		reference.ChannelID = snapshot.MessageReference.ChannelID
		reference.GuildID = snapshot.MessageReference.GuildID
		message.MessageReference = reference
	}

	return message
}

func mentionedUserIDs(users []*discordgo.User) []string {
	userIDs := make([]string, 0, len(users))

	for _, user := range users {
		if user == nil || strings.TrimSpace(user.ID) == "" {
			continue
		}

		userIDs = append(userIDs, user.ID)
	}

	return userIDs
}

func mentionedUsers(userIDs []string) []*discordgo.User {
	users := make([]*discordgo.User, 0, len(userIDs))

	for _, userID := range userIDs {
		trimmedUserID := strings.TrimSpace(userID)
		if trimmedUserID == "" {
			continue
		}

		user := new(discordgo.User)
		user.ID = trimmedUserID
		users = append(users, user)
	}

	return users
}

func attachmentSnapshots(
	attachments []*discordgo.MessageAttachment,
) []discordAttachmentSnapshot {
	snapshots := make([]discordAttachmentSnapshot, 0, len(attachments))

	for _, attachment := range attachments {
		if attachment == nil {
			continue
		}

		snapshots = append(snapshots, discordAttachmentSnapshot{
			Filename:    attachment.Filename,
			ContentType: attachment.ContentType,
			URL:         attachment.URL,
		})
	}

	return snapshots
}

func attachmentsFromSnapshots(
	snapshots []discordAttachmentSnapshot,
) []*discordgo.MessageAttachment {
	attachments := make([]*discordgo.MessageAttachment, 0, len(snapshots))

	for _, snapshot := range snapshots {
		attachment := new(discordgo.MessageAttachment)
		attachment.Filename = snapshot.Filename
		attachment.ContentType = snapshot.ContentType
		attachment.URL = snapshot.URL
		attachments = append(attachments, attachment)
	}

	return attachments
}

func embedSnapshots(embeds []*discordgo.MessageEmbed) []discordEmbedSnapshot {
	snapshots := make([]discordEmbedSnapshot, 0, len(embeds))

	for _, embed := range embeds {
		if embed == nil {
			continue
		}

		footerText := ""
		if embed.Footer != nil {
			footerText = embed.Footer.Text
		}

		snapshots = append(snapshots, discordEmbedSnapshot{
			Title:       embed.Title,
			Description: embed.Description,
			FooterText:  footerText,
		})
	}

	return snapshots
}

func embedsFromSnapshots(snapshots []discordEmbedSnapshot) []*discordgo.MessageEmbed {
	embeds := make([]*discordgo.MessageEmbed, 0, len(snapshots))

	for _, snapshot := range snapshots {
		embed := new(discordgo.MessageEmbed)
		embed.Title = snapshot.Title

		embed.Description = snapshot.Description
		if strings.TrimSpace(snapshot.FooterText) != "" {
			footer := new(discordgo.MessageEmbedFooter)
			footer.Text = snapshot.FooterText
			embed.Footer = footer
		}

		embeds = append(embeds, embed)
	}

	return embeds
}
