package providers

import (
	"strings"
	"sync"
)

// APIKeyRotator round-robins through a provider's API keys so concurrent
// prompts spread across every configured key.
type APIKeyRotator struct {
	mu          sync.Mutex
	nextIndexes map[string]int
}

// NewAPIKeyRotator builds an API key rotator.
func NewAPIKeyRotator() *APIKeyRotator {
	return &APIKeyRotator{
		mu:          sync.Mutex{},
		nextIndexes: make(map[string]int),
	}
}

// Rotate returns the key set rotated deterministically; each call advances
// the rotation state for that signature so the next call picks a later key
// first.
func (rotator *APIKeyRotator) Rotate(apiKeys []string) []string {
	if len(apiKeys) <= 1 {
		return append([]string(nil), apiKeys...)
	}

	signature := strings.Join(apiKeys, "\x00")

	rotator.mu.Lock()
	nextIndex := rotator.nextIndexes[signature]
	rotator.nextIndexes[signature] = (nextIndex + 1) % len(apiKeys)
	rotator.mu.Unlock()

	offsetIndex := nextIndex
	if offsetIndex == 0 {
		return append([]string(nil), apiKeys...)
	}

	rotated := make([]string, len(apiKeys))
	copy(rotated, apiKeys[offsetIndex:])
	copy(rotated[len(apiKeys)-offsetIndex:], apiKeys[:offsetIndex])

	return rotated
}

func firstAPIKey(apiKeys []string) string {
	if len(apiKeys) == 0 {
		return ""
	}

	return apiKeys[0]
}
