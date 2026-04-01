package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	_ "github.com/lib/pq" // Register the PostgreSQL driver for database/sql.
)

const (
	messageNodeStoreSnapshotVersion     = 1
	messageNodeStoreTableName           = "message_history_snapshots"
	defaultMessageNodeStorePersistDelay = 250 * time.Millisecond
	postgresJSONTextReplacement         = "\uFFFD"
)

var errMessageNodeStorePersistenceDisabled = errors.New("message history persistence disabled")

const (
	messageNodeStoreSelectSQL = "SELECT version, snapshot FROM message_history_snapshots WHERE store_key = $1"
	messageNodeStoreUpsertSQL = "INSERT INTO message_history_snapshots (store_key, version, snapshot, updated_at) " +
		"VALUES ($1, $2, $3, NOW()) " +
		"ON CONFLICT (store_key) DO UPDATE SET version = EXCLUDED.version, snapshot = EXCLUDED.snapshot, updated_at = NOW()"
	messageNodeStoreCreateTableSQL = "CREATE TABLE IF NOT EXISTS message_history_snapshots (" +
		"store_key TEXT PRIMARY KEY," +
		"version INTEGER NOT NULL," +
		"snapshot JSONB NOT NULL," +
		"updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()" +
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
	Role                     string                  `json:"role"`
	Text                     string                  `json:"text"`
	ThinkingText             string                  `json:"thinking_text"`
	URLScanText              string                  `json:"url_scan_text"`
	RentryURL                string                  `json:"rentry_url"`
	Media                    []contentPartSnapshot   `json:"media"`
	SearchMetadata           *searchMetadata         `json:"search_metadata"`
	HasBadAttachments        bool                    `json:"has_bad_attachments"`
	AttachmentDownloadFailed bool                    `json:"attachment_download_failed"`
	FetchParentFailed        bool                    `json:"fetch_parent_failed"`
	ParentMessage            *discordMessageSnapshot `json:"parent_message"`
	Initialized              bool                    `json:"initialized"`
}

type contentPartSnapshot struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
	Data     []byte `json:"data"`
	MIMEType string `json:"mime_type"`
	Filename string `json:"filename"`
}

type discordMessageSnapshot struct {
	ID               string                           `json:"id"`
	ChannelID        string                           `json:"channel_id"`
	GuildID          string                           `json:"guild_id"`
	Type             int                              `json:"type"`
	Content          string                           `json:"content"`
	Author           *discordUserSnapshot             `json:"author"`
	MentionUserIDs   []string                         `json:"mention_user_ids"`
	Attachments      []discordAttachmentSnapshot      `json:"attachments"`
	Embeds           []discordEmbedSnapshot           `json:"embeds"`
	MessageReference *discordMessageReferenceSnapshot `json:"message_reference"`
}

type discordUserSnapshot struct {
	ID  string `json:"id"`
	Bot bool   `json:"bot"`
}

type discordAttachmentSnapshot struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
}

type discordEmbedSnapshot struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	FooterText  string `json:"footer_text"`
}

type discordMessageReferenceSnapshot struct {
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
}

type messageNodeStoreBackend interface {
	loadSnapshot(storeKey string, capacity int) (messageNodeStoreSnapshot, error)
	saveSnapshot(storeKey string, snapshot messageNodeStoreSnapshot) error
	close() error
}

type postgresMessageNodeStoreBackend struct {
	database *sql.DB
}

func newPostgresMessageNodeStoreBackend(
	ctx context.Context,
	connectionString string,
) (*postgresMessageNodeStoreBackend, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil postgres message history context: %w", os.ErrInvalid)
	}

	trimmedConnectionString := strings.TrimSpace(connectionString)
	if trimmedConnectionString == "" {
		return nil, errMessageNodeStorePersistenceDisabled
	}

	database, err := sql.Open("postgres", trimmedConnectionString)
	if err != nil {
		return nil, fmt.Errorf("open postgres message history database: %w", err)
	}

	err = database.PingContext(ctx)
	if err != nil {
		_ = database.Close()

		return nil, fmt.Errorf("ping postgres message history database: %w", err)
	}

	err = ensureMessageNodeStoreTable(ctx, database)
	if err != nil {
		_ = database.Close()

		return nil, err
	}

	backend := new(postgresMessageNodeStoreBackend)
	backend.database = database

	return backend, nil
}

func ensureMessageNodeStoreTable(ctx context.Context, database *sql.DB) error {
	_, err := database.ExecContext(ctx, messageNodeStoreCreateTableSQL)
	if err != nil {
		return fmt.Errorf("create postgres message history table %q: %w", messageNodeStoreTableName, err)
	}

	return nil
}

func (backend *postgresMessageNodeStoreBackend) loadSnapshot(
	storeKey string,
	capacity int,
) (messageNodeStoreSnapshot, error) {
	var snapshot messageNodeStoreSnapshot

	var snapshotBytes []byte

	err := backend.database.QueryRowContext(
		context.Background(),
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
			"query message history from postgres table %q: %w",
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

func (backend *postgresMessageNodeStoreBackend) saveSnapshot(
	storeKey string,
	snapshot messageNodeStoreSnapshot,
) error {
	snapshotBytes, err := encodeMessageNodeSnapshotJSON(snapshot.Nodes)
	if err != nil {
		return fmt.Errorf("encode message history snapshot JSON: %w", err)
	}

	_, err = backend.database.ExecContext(
		context.Background(),
		messageNodeStoreUpsertSQL,
		storeKey,
		snapshot.Version,
		snapshotBytes,
	)
	if err != nil {
		return fmt.Errorf(
			"upsert message history into postgres table %q: %w",
			messageNodeStoreTableName,
			err,
		)
	}

	return nil
}

func newConfiguredMessageNodeStore(
	ctx context.Context,
	capacity int,
	configPath string,
	configuredStoreKey string,
	connectionString string,
) (*messageNodeStore, error) {
	backend, err := newPostgresMessageNodeStoreBackend(ctx, connectionString)
	if err != nil {
		if errors.Is(err, errMessageNodeStorePersistenceDisabled) {
			return newMessageNodeStore(capacity), nil
		}

		return nil, err
	}

	storeKey := messageNodeStoreKey(configPath, configuredStoreKey)

	store, err := newPersistentMessageNodeStore(capacity, storeKey, backend)
	if err == nil {
		return store, nil
	}

	closeErr := backend.close()
	if closeErr != nil {
		return nil, fmt.Errorf(
			"load persisted message history for store key %q: %w (close backend: %w)",
			storeKey,
			err,
			closeErr,
		)
	}

	return nil, fmt.Errorf("load persisted message history for store key %q: %w", storeKey, err)
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
	sanitizedPayload := sanitizeMessageNodeSnapshotPayload(messageNodeSnapshotPayload{
		Nodes: nodes,
	})

	payloadBytes, err := json.Marshal(sanitizedPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal message history snapshot payload: %w", err)
	}

	return payloadBytes, nil
}

func sanitizeMessageNodeSnapshotPayload(
	payload messageNodeSnapshotPayload,
) messageNodeSnapshotPayload {
	if payload.Nodes == nil {
		return messageNodeSnapshotPayload{Nodes: nil}
	}

	sanitizedNodes := make(map[string]messageNodeSnapshot, len(payload.Nodes))
	for messageID, snapshot := range payload.Nodes {
		sanitizedNodes[sanitizePostgresJSONString(messageID)] = sanitizeMessageNodeSnapshot(snapshot)
	}

	return messageNodeSnapshotPayload{Nodes: sanitizedNodes}
}

func sanitizeMessageNodeSnapshot(snapshot messageNodeSnapshot) messageNodeSnapshot {
	snapshot.Role = sanitizePostgresJSONString(snapshot.Role)
	snapshot.Text = sanitizePostgresJSONString(snapshot.Text)
	snapshot.ThinkingText = sanitizePostgresJSONString(snapshot.ThinkingText)
	snapshot.URLScanText = sanitizePostgresJSONString(snapshot.URLScanText)
	snapshot.RentryURL = sanitizePostgresJSONString(snapshot.RentryURL)
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
	snapshot.Type = sanitizePostgresJSONString(snapshot.Type)
	snapshot.Text = sanitizePostgresJSONString(snapshot.Text)
	snapshot.ImageURL = sanitizePostgresJSONString(snapshot.ImageURL)
	snapshot.MIMEType = sanitizePostgresJSONString(snapshot.MIMEType)
	snapshot.Filename = sanitizePostgresJSONString(snapshot.Filename)

	return snapshot
}

func sanitizeSearchMetadata(metadata *searchMetadata) *searchMetadata {
	if metadata == nil {
		return nil
	}

	return &searchMetadata{
		Queries:             sanitizePostgresJSONStrings(metadata.Queries),
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
			Query: sanitizePostgresJSONString(result.Query),
			Text:  sanitizePostgresJSONString(result.Text),
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
			Label:   sanitizePostgresJSONString(sourceGroup.Label),
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
			Title: sanitizePostgresJSONString(source.Title),
			URL:   sanitizePostgresJSONString(source.URL),
		})
	}

	return sanitizedSources
}

func sanitizeDiscordMessageSnapshot(message *discordMessageSnapshot) *discordMessageSnapshot {
	if message == nil {
		return nil
	}

	return &discordMessageSnapshot{
		ID:               sanitizePostgresJSONString(message.ID),
		ChannelID:        sanitizePostgresJSONString(message.ChannelID),
		GuildID:          sanitizePostgresJSONString(message.GuildID),
		Type:             message.Type,
		Content:          sanitizePostgresJSONString(message.Content),
		Author:           sanitizeDiscordUserSnapshot(message.Author),
		MentionUserIDs:   sanitizePostgresJSONStrings(message.MentionUserIDs),
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
		ID:  sanitizePostgresJSONString(user.ID),
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
			Filename:    sanitizePostgresJSONString(attachment.Filename),
			ContentType: sanitizePostgresJSONString(attachment.ContentType),
			URL:         sanitizePostgresJSONString(attachment.URL),
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
			Title:       sanitizePostgresJSONString(embed.Title),
			Description: sanitizePostgresJSONString(embed.Description),
			FooterText:  sanitizePostgresJSONString(embed.FooterText),
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
		MessageID: sanitizePostgresJSONString(reference.MessageID),
		ChannelID: sanitizePostgresJSONString(reference.ChannelID),
		GuildID:   sanitizePostgresJSONString(reference.GuildID),
	}
}

func sanitizePostgresJSONStrings(values []string) []string {
	if values == nil {
		return nil
	}

	sanitizedValues := make([]string, 0, len(values))
	for _, value := range values {
		sanitizedValues = append(sanitizedValues, sanitizePostgresJSONString(value))
	}

	return sanitizedValues
}

func sanitizePostgresJSONString(value string) string {
	sanitizedValue := strings.ToValidUTF8(value, postgresJSONTextReplacement)

	return strings.ReplaceAll(
		sanitizedValue,
		"\x00",
		postgresJSONTextReplacement,
	)
}

func (backend *postgresMessageNodeStoreBackend) close() error {
	if backend == nil || backend.database == nil {
		return nil
	}

	err := backend.database.Close()
	if err != nil {
		return fmt.Errorf("close postgres message history database: %w", err)
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

	store.storeKey = trimmedStoreKey
	store.backend = backend

	snapshot, err := backend.loadSnapshot(trimmedStoreKey, capacity)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			store.startSaveWorker()

			return store, nil
		}

		return nil, err
	}

	store.nodes = snapshotNodesToStoreNodes(snapshot.Nodes)
	store.snapshotCache = maps.Clone(snapshot.Nodes)
	store.startSaveWorker()

	return store, nil
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

func (store *messageNodeStore) persistBestEffort() {
	if store == nil || strings.TrimSpace(store.storeKey) == "" || store.backend == nil {
		return
	}

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
	if strings.TrimSpace(store.storeKey) == "" {
		return nil
	}

	store.saveMu.Lock()
	defer store.saveMu.Unlock()

	snapshot := store.snapshot()

	err := store.backend.saveSnapshot(store.storeKey, snapshot)
	if err != nil {
		return err
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
	if node == nil || strings.TrimSpace(store.storeKey) == "" {
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
}

func (store *messageNodeStore) deleteCachedSnapshot(messageID string) {
	if strings.TrimSpace(store.storeKey) == "" {
		return
	}

	trimmedMessageID := strings.TrimSpace(messageID)
	if trimmedMessageID == "" {
		return
	}

	store.snapshotMu.Lock()
	delete(store.snapshotCache, trimmedMessageID)
	store.snapshotMu.Unlock()
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
	if store == nil || store.backend == nil {
		return nil
	}

	store.stopSaveWorker()

	persistErr := store.persist()

	closeErr := store.backend.close()
	if closeErr == nil {
		store.backend = nil
	}

	switch {
	case persistErr != nil && closeErr != nil:
		return errors.Join(persistErr, closeErr)
	case persistErr != nil:
		return persistErr
	case closeErr != nil:
		return closeErr
	}

	store.backend = nil

	return nil
}

func (store *messageNodeStore) startSaveWorker() {
	if store == nil || strings.TrimSpace(store.storeKey) == "" || store.backend == nil {
		return
	}

	store.saveWorkerMu.Lock()
	defer store.saveWorkerMu.Unlock()

	if store.saveRequests != nil {
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
				slog.Warn(
					"persist message history",
					"store_key",
					store.storeKey,
					"error",
					err,
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
		RentryURL:                node.rentryURL,
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
	node.rentryURL = snapshot.RentryURL
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
	case contentTypeAudioData, contentTypeDocument, contentTypeVideoData:
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

	part := contentPart{"type": snapshot.Type}

	switch snapshot.Type {
	case contentTypeText:
		part["text"] = snapshot.Text
	case contentTypeImageURL:
		part["image_url"] = map[string]string{"url": snapshot.ImageURL}
	case contentTypeAudioData, contentTypeDocument, contentTypeVideoData:
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
