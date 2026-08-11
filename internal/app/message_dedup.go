package app

import (
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

// messageDedupMapSizeLimit bounds the dedup map before a prune sweep.
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
