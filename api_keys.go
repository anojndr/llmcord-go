package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

const yamlNullTag = "!!null"

type apiKeyCounterError struct {
	sync.Map
}

func (counters *apiKeyCounterError) Error() string {
	return "apiKeyCounterError"
}

var errAPIKeyCounterError error = new(apiKeyCounterError)

func roundRobinAPIKeys(apiKeys []string) []string {
	if len(apiKeys) <= 1 {
		return apiKeys
	}

	var countersMap *apiKeyCounterError
	if !errors.As(errAPIKeyCounterError, &countersMap) {
		return apiKeys
	}

	sig := strings.Join(apiKeys, "\x00")
	val, _ := countersMap.LoadOrStore(sig, new(atomic.Uint64))

	counter, ok := val.(*atomic.Uint64)
	if !ok {
		return apiKeys
	}

	idx := counter.Add(1) - 1
	numKeys := uint64(len(apiKeys))

	rem := idx % numKeys
	if rem > uint64(math.MaxInt) {
		return apiKeys
	}

	offset := int(rem)
	if offset == 0 {
		result := make([]string, len(apiKeys))
		copy(result, apiKeys)

		return result
	}

	result := make([]string, len(apiKeys))
	copy(result, apiKeys[offset:])
	copy(result[len(apiKeys)-offset:], apiKeys[:offset])

	return result
}

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

func (provider providerRequestConfig) apiKeys() []string {
	return providerAPIKeys(provider.APIKey, provider.APIKeys)
}

func (provider providerRequestConfig) primaryAPIKey() string {
	return firstAPIKey(provider.apiKeys())
}

func (provider providerRequestConfig) apiKeysForAttempts() []string {
	apiKeys := provider.apiKeys()
	if len(apiKeys) == 0 {
		return []string{""}
	}

	return roundRobinAPIKeys(apiKeys)
}

func (provider providerRequestConfig) withSingleAPIKey(apiKey string) providerRequestConfig {
	provider.APIKey = strings.TrimSpace(apiKey)
	provider.APIKeys = nil

	return provider
}

func (settings tavilySearchConfig) apiKeys() []string {
	return providerAPIKeys(settings.APIKey, settings.APIKeys)
}

func (settings exaSearchConfig) apiKeys() []string {
	return providerAPIKeys(settings.APIKey, settings.APIKeys)
}

func (settings exaSearchConfig) apiKeysForAttempts() []string {
	return roundRobinAPIKeys(settings.apiKeys())
}

func (settings tavilySearchConfig) apiKeysForAttempts() []string {
	return roundRobinAPIKeys(settings.apiKeys())
}

func (settings serpAPIVisualSearchConfig) apiKeys() []string {
	return providerAPIKeys(settings.APIKey, settings.APIKeys)
}

func (settings serpAPIVisualSearchConfig) apiKeysForAttempts() []string {
	return roundRobinAPIKeys(settings.apiKeys())
}
