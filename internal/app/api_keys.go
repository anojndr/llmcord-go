package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const yamlNullTag = "!!null"

type scalarStringList []string

func (value *scalarStringList) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == yamlNullTag {
		*value = nil

		return nil
	}

	switch node.Kind {
	case yaml.ScalarNode:
		*value = []string{node.Value}

		return nil
	case yaml.SequenceNode:
		items := make([]string, 0, len(node.Content))

		for _, childNode := range node.Content {
			if childNode.Kind != yaml.ScalarNode {
				return fmt.Errorf("decode scalar string list item: %w", os.ErrInvalid)
			}

			items = append(items, childNode.Value)
		}

		*value = items

		return nil
	case yaml.DocumentNode, yaml.MappingNode, yaml.AliasNode:
		return fmt.Errorf("decode scalar string list: %w", os.ErrInvalid)
	default:
		return fmt.Errorf("decode scalar string list: %w", os.ErrInvalid)
	}
}

func normalizeAPIKeys(candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(candidates))
	keys := make([]string, 0, len(candidates))

	for _, candidate := range candidates {
		trimmedCandidate := strings.TrimSpace(candidate)
		if trimmedCandidate == "" {
			continue
		}

		if _, ok := seen[trimmedCandidate]; ok {
			continue
		}

		seen[trimmedCandidate] = struct{}{}
		keys = append(keys, trimmedCandidate)
	}

	if len(keys) == 0 {
		return nil
	}

	return keys
}

func firstAPIKey(apiKeys []string) string {
	if len(apiKeys) == 0 {
		return ""
	}

	return apiKeys[0]
}

// apiKeyRotator round-robins configured API key lists per key-set, so
// concurrent prompts spread across every key while each request streams
// with exactly one.
type apiKeyRotator struct {
	mu          sync.Mutex
	nextIndexes map[string]int
}

func newAPIKeyRotator() *apiKeyRotator {
	return &apiKeyRotator{
		mu:          sync.Mutex{},
		nextIndexes: make(map[string]int),
	}
}

// rotate returns the API keys rotated by the number of previous calls for
// the same key set, wrapping around. The first call returns the keys in
// configured order, so the primary key handles the first request. Results
// are fresh slices; the input is never mutated.
func (rotator *apiKeyRotator) rotate(apiKeys []string) []string {
	if len(apiKeys) <= 1 {
		return append([]string(nil), apiKeys...)
	}

	signature := keySetSignature(apiKeys)

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

func providerAPIKeys(primaryKey string, fallbackKeys []string) []string {
	candidates := make([]string, 0, len(fallbackKeys)+1)
	candidates = append(candidates, primaryKey)
	candidates = append(candidates, fallbackKeys...)

	return normalizeAPIKeys(candidates)
}

func (provider providerConfig) apiKeys() []string {
	return providerAPIKeys(provider.APIKey, provider.APIKeys)
}

func (provider providerConfig) primaryAPIKey() string {
	return firstAPIKey(provider.apiKeys())
}

func keySetSignature(apiKeys []string) string {
	if len(apiKeys) == 0 {
		return ""
	}

	return strings.Join(apiKeys, "\x00")
}

func (settings tavilySearchConfig) apiKeys() []string {
	return providerAPIKeys(settings.APIKey, settings.APIKeys)
}

func (settings exaSearchConfig) apiKeys() []string {
	return providerAPIKeys(settings.APIKey, settings.APIKeys)
}

func (settings firecrawlSearchConfig) apiKeys() []string {
	return providerAPIKeys(settings.APIKey, settings.APIKeys)
}

func (settings firecrawlSearchConfig) maxMarkdownCharacters() int {
	if settings.MaxMarkdownCharacters <= 0 {
		return defaultFirecrawlMaxMarkdownCharacters
	}

	return settings.MaxMarkdownCharacters
}

func (settings tinyFishSearchConfig) apiKeys() []string {
	return providerAPIKeys(settings.APIKey, settings.APIKeys)
}

func (settings parallelSearchConfig) apiKeys() []string {
	return providerAPIKeys(settings.APIKey, settings.APIKeys)
}

func (settings serpAPIVisualSearchConfig) apiKeys() []string {
	return providerAPIKeys(settings.APIKey, settings.APIKeys)
}

// tryAllAPIKeys runs attempt with each rotated key in order, trying every key
// before returning an error. The key set is rotated once for load spreading,
// then each key is tried until one succeeds. Context cancellation aborts the
// remaining keys and returns the last attempt error.
func tryAllAPIKeys[T any](ctx context.Context, rotator *apiKeyRotator, apiKeys []string, attempt func(string) (T, error)) (T, error) {
	var rotated []string
	if rotator != nil {
		rotated = rotator.rotate(apiKeys)
	} else {
		rotated = append([]string(nil), apiKeys...)
	}

	var zero T
	if len(rotated) == 0 {
		return zero, fmt.Errorf("no API keys configured: %w", os.ErrNotExist)
	}

	var lastErr error

	for index, apiKey := range rotated {
		if index > 0 && ctx != nil && ctx.Err() != nil {
			break
		}

		result, err := attempt(apiKey)
		if err == nil {
			return result, nil
		}

		lastErr = err
	}

	return zero, lastErr
}
