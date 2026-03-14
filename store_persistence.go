package main

import (
	"crypto/sha256"
	"encoding/gob"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/bwmarrin/discordgo"
)

const (
	messageNodeStoreSnapshotVersion = 1
	messageNodeStoreDirectoryName   = ".llmcord-go"
	messageNodeStoreDirectoryMode   = 0o750
)

type messageNodeStoreSnapshot struct {
	Version int
	Nodes   map[string]messageNodeSnapshot
}

type messageNodeSnapshot struct {
	Role              string
	Text              string
	URLScanText       string
	RentryURL         string
	Media             []contentPartSnapshot
	SearchMetadata    *searchMetadata
	HasBadAttachments bool
	FetchParentFailed bool
	ParentMessage     *discordMessageSnapshot
	Initialized       bool
}

type contentPartSnapshot struct {
	Type     string
	Text     string
	ImageURL string
	Data     []byte
	MIMEType string
	Filename string
}

type discordMessageSnapshot struct {
	ID               string
	ChannelID        string
	GuildID          string
	Type             int
	Content          string
	Author           *discordUserSnapshot
	MentionUserIDs   []string
	Attachments      []discordAttachmentSnapshot
	Embeds           []discordEmbedSnapshot
	MessageReference *discordMessageReferenceSnapshot
}

type discordUserSnapshot struct {
	ID  string
	Bot bool
}

type discordAttachmentSnapshot struct {
	Filename    string
	ContentType string
	URL         string
}

type discordEmbedSnapshot struct {
	Title       string
	Description string
	FooterText  string
}

type discordMessageReferenceSnapshot struct {
	MessageID string
	ChannelID string
	GuildID   string
}

func newPersistentMessageNodeStore(capacity int, path string) (*messageNodeStore, error) {
	store := newMessageNodeStore(capacity)

	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return store, nil
	}

	store.path = filepath.Clean(trimmedPath)

	snapshot, err := readMessageNodeStoreSnapshot(store.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}

		return nil, err
	}

	snapshot.Nodes = trimSnapshotNodes(snapshot.Nodes, capacity)
	store.nodes = snapshotNodesToStoreNodes(snapshot.Nodes)
	store.snapshotCache = maps.Clone(snapshot.Nodes)

	return store, nil
}

func defaultMessageNodeStorePath(configPath string) string {
	projectRoot, err := os.Getwd()
	if err != nil || strings.TrimSpace(projectRoot) == "" {
		projectRoot = "."
	}

	resolvedConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		resolvedConfigPath = configPath
	}

	hash := sha256.Sum256([]byte(filepath.Clean(resolvedConfigPath)))

	return filepath.Join(
		projectRoot,
		messageNodeStoreDirectoryName,
		fmt.Sprintf("message-history-%x.gob", hash[:8]),
	)
}

func (store *messageNodeStore) persistBestEffort() {
	err := store.persist()
	if err == nil {
		return
	}

	slog.Warn("persist message history", "path", store.path, "error", err)
}

func (store *messageNodeStore) persist() error {
	if strings.TrimSpace(store.path) == "" {
		return nil
	}

	store.saveMu.Lock()
	defer store.saveMu.Unlock()

	snapshot := store.snapshot()

	err := writeMessageNodeStoreSnapshot(store.path, snapshot)
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
	if node == nil || strings.TrimSpace(store.path) == "" {
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
	if strings.TrimSpace(store.path) == "" {
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

func readMessageNodeStoreSnapshot(path string) (messageNodeStoreSnapshot, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return messageNodeStoreSnapshot{}, fmt.Errorf("open message history %q: %w", path, err)
	}

	defer func() {
		_ = file.Close()
	}()

	var snapshot messageNodeStoreSnapshot

	err = gob.NewDecoder(file).Decode(&snapshot)
	if err != nil {
		return messageNodeStoreSnapshot{}, fmt.Errorf("decode message history %q: %w", path, err)
	}

	if snapshot.Version != messageNodeStoreSnapshotVersion {
		return messageNodeStoreSnapshot{}, fmt.Errorf(
			"unsupported message history version %d: %w",
			snapshot.Version,
			os.ErrInvalid,
		)
	}

	if snapshot.Nodes == nil {
		snapshot.Nodes = make(map[string]messageNodeSnapshot)
	}

	return snapshot, nil
}

func writeMessageNodeStoreSnapshot(path string, snapshot messageNodeStoreSnapshot) error {
	directory := filepath.Dir(path)

	err := os.MkdirAll(directory, messageNodeStoreDirectoryMode)
	if err != nil {
		return fmt.Errorf("create message history directory %q: %w", directory, err)
	}

	tempFile, err := os.CreateTemp(directory, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp message history file: %w", err)
	}

	tempPath := tempFile.Name()
	removeTempFile := true

	defer func() {
		if removeTempFile {
			_ = os.Remove(tempPath)
		}
	}()

	encoder := gob.NewEncoder(tempFile)

	err = encoder.Encode(snapshot)
	if err != nil {
		_ = tempFile.Close()

		return fmt.Errorf("encode message history %q: %w", path, err)
	}

	err = tempFile.Sync()
	if err != nil {
		_ = tempFile.Close()

		return fmt.Errorf("sync message history %q: %w", path, err)
	}

	err = tempFile.Close()
	if err != nil {
		return fmt.Errorf("close message history %q: %w", path, err)
	}

	err = os.Rename(tempPath, filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("replace message history %q: %w", path, err)
	}

	removeTempFile = false

	return nil
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
		Role:              node.role,
		Text:              node.text,
		URLScanText:       node.urlScanText,
		RentryURL:         node.rentryURL,
		Media:             mediaSnapshots,
		SearchMetadata:    cloneSearchMetadata(node.searchMetadata),
		HasBadAttachments: node.hasBadAttachments,
		FetchParentFailed: node.fetchParentFailed,
		ParentMessage:     newDiscordMessageSnapshot(node.parentMessage),
		Initialized:       node.initialized,
	}

	return snapshot, true
}

func (snapshot messageNodeSnapshot) messageNode() *messageNode {
	node := new(messageNode)
	node.role = snapshot.Role
	node.text = snapshot.Text
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
