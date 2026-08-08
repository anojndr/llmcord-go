package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ErrInstanceAlreadyRunning is returned when another bot instance already
// holds the config's single-instance lock.
var ErrInstanceAlreadyRunning = errors.New("another llmcord instance is already running")

// InstanceLock is an exclusive advisory lock held for the lifetime of the bot
// process. It guarantees that only one bot instance can run per config path,
// so a stale binary and `go run` launched side by side can never both connect
// to Discord and each answer every message.
type InstanceLock struct {
	file *os.File
	// Path is the lock file location, exposed for tests.
	Path string
}

// AcquireInstanceLock takes an exclusive advisory lock on the config file
// itself. It fails fast when another instance already holds it, so a stale
// binary and `go run` launched side by side can never both connect to
// Discord and each answer every message.
func AcquireInstanceLock(configPath string) (*InstanceLock, error) {
	lockFile, err := os.OpenFile(filepath.Clean(configPath), os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open config file for instance lock: %w", err)
	}

	err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		_ = lockFile.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf(
				"%w for config %q",
				ErrInstanceAlreadyRunning,
				configPath,
			)
		}

		return nil, fmt.Errorf("flock config file: %w", err)
	}

	return &InstanceLock{file: lockFile, Path: filepath.Clean(configPath)}, nil
}

// Release drops the lock so a later instance can start.
func (lock *InstanceLock) Release() {
	if lock == nil || lock.file == nil {
		return
	}

	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	_ = lock.file.Close()
	lock.file = nil
}

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
