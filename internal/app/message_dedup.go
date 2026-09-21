package app

import (
	"context"
	"strings"
	"time"
)

// messageSeenWindow bounds how long a message ID stays in the dedup set.
const messageSeenWindow = 30 * time.Second

// markMessageSeen reports whether this message ID is being handled for the
// first time within the dedup window. Discord can deliver the same
// MESSAGE_CREATE event more than once (event replay, retried frames), so the
// bot must not answer the same message twice.
func (instance *bot) markMessageSeen(messageID string) bool {
	if strings.TrimSpace(messageID) == "" {
		return true
	}

	if !instance.reserveMessageSeenCluster(messageID) {
		return false
	}

	now := time.Now()

	instance.messageDedupMu.Lock()
	defer instance.messageDedupMu.Unlock()

	if instance.messageProcessedAt == nil {
		instance.messageProcessedAt = make(map[string]time.Time)
	}

	if seenAt, seen := instance.messageProcessedAt[messageID]; seen && now.Sub(seenAt) < messageSeenWindow {
		return false
	}

	instance.messageProcessedAt[messageID] = now

	if len(instance.messageProcessedAt) > messageDedupMapSizeLimit {
		instance.expireMessageSeen(now)
	}

	return true
}

// reserveMessageSeenCluster claims the message ID cluster-wide via Redis
// SET NX. False means another instance already claimed it. Redis errors fall
// back to local-only dedup so a Redis outage never drops messages.
func (instance *bot) reserveMessageSeenCluster(messageID string) bool {
	if instance == nil {
		return true
	}

	client := instance.redisDedupClient
	if client == nil {
		return true
	}

	first, err := reserveMessageDedup(context.Background(), client, instance.redisDedupPrefix, messageID)
	if err != nil {
		logWarn("reserve message dedup", err, "message_id", messageID)

		return true
	}

	return first
}

const messageDedupMapSizeLimit = 1024

// expireMessageSeen drops entries older than the dedup window so the map
// never grows without bound over the bot's lifetime.
func (instance *bot) expireMessageSeen(now time.Time) {
	for messageID, seenAt := range instance.messageProcessedAt {
		if now.Sub(seenAt) >= messageSeenWindow {
			delete(instance.messageProcessedAt, messageID)
		}
	}
}
