package config

import (
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

// APIKeyRotator round-robins configured API key lists per key-set, so
// concurrent prompts spread across every key while each request streams
// with exactly one.
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

// Rotate returns the API keys rotated by the number of previous calls for
// the same key set, wrapping around. The first call returns the keys in
// configured order, so the primary key handles the first request. Results
// are fresh slices; the input is never mutated.
func (rotator *APIKeyRotator) Rotate(apiKeys []string) []string {
	if len(apiKeys) <= 1 {
		return append([]string(nil), apiKeys...)
	}

	signature := KeySetSignature(apiKeys)

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

// ProviderAPIKeys joins a primary key with fallback keys and normalizes.
func ProviderAPIKeys(primaryKey string, fallbackKeys []string) []string {
	candidates := make([]string, 0, len(fallbackKeys)+1)
	candidates = append(candidates, primaryKey)
	candidates = append(candidates, fallbackKeys...)

	return normalizeAPIKeys(candidates)
}

// AllKeys returns the normalized provider key set.
func (provider ProviderConfig) AllKeys() []string {
	return ProviderAPIKeys(provider.APIKey, provider.APIKeys)
}

// PrimaryAPIKey returns the first provider key.
func (provider ProviderConfig) PrimaryAPIKey() string {
	return firstAPIKey(provider.AllKeys())
}

// KeySetSignature fingerprints a key set for rotation bookkeeping.
func KeySetSignature(apiKeys []string) string {
	if len(apiKeys) == 0 {
		return ""
	}

	return strings.Join(apiKeys, "\x00")
}

// AllKeys returns the normalized Tavily key set.
func (settings TavilySearchConfig) AllKeys() []string {
	return ProviderAPIKeys(settings.APIKey, settings.APIKeys)
}

// PrimaryAPIKey returns the first Tavily key.
func (settings TavilySearchConfig) PrimaryAPIKey() string {
	return firstAPIKey(settings.AllKeys())
}

// AllKeys returns the normalized Exa key set.
func (settings ExaSearchConfig) AllKeys() []string {
	return ProviderAPIKeys(settings.APIKey, settings.APIKeys)
}

// PrimaryAPIKey returns the first Exa key.
func (settings ExaSearchConfig) PrimaryAPIKey() string {
	return firstAPIKey(settings.AllKeys())
}

// AllKeys returns the normalized Firecrawl key set.
func (settings FirecrawlSearchConfig) AllKeys() []string {
	return ProviderAPIKeys(settings.APIKey, settings.APIKeys)
}

// PrimaryAPIKey returns the first Firecrawl key.
func (settings FirecrawlSearchConfig) PrimaryAPIKey() string {
	return firstAPIKey(settings.AllKeys())
}

// MaxMarkdownCharactersOrDefault returns the markdown cap with its default.
func (settings FirecrawlSearchConfig) MaxMarkdownCharactersOrDefault() int {
	if settings.MaxMarkdownCharacters <= 0 {
		return defaultFirecrawlMaxMarkdownCharacters
	}

	return settings.MaxMarkdownCharacters
}

// AllKeys returns the normalized SerpAPI key set.
func (settings SerpAPIVisualSearchConfig) AllKeys() []string {
	return ProviderAPIKeys(settings.APIKey, settings.APIKeys)
}

// PrimaryAPIKey returns the first SerpAPI key.
func (settings SerpAPIVisualSearchConfig) PrimaryAPIKey() string {
	return firstAPIKey(settings.AllKeys())
}
