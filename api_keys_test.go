package main

import (
	"reflect"
	"testing"
)

func TestNormalizeAPIKeysDeduplicatesAndTrims(t *testing.T) {
	t.Parallel()

	normalized := normalizeAPIKeys([]string{"  key1 ", "", "key2", "key1", "", "  key3 "})

	expected := []string{"key1", "key2", "key3"}
	if !reflect.DeepEqual(normalized, expected) {
		t.Fatalf("expected %#v, got %#v", expected, normalized)
	}
}

func TestNormalizeAPIKeysEmpty(t *testing.T) {
	t.Parallel()

	if normalized := normalizeAPIKeys(nil); normalized != nil {
		t.Fatalf("expected nil, got %#v", normalized)
	}

	if normalized := normalizeAPIKeys([]string{"", " "}); normalized != nil {
		t.Fatalf("expected nil, got %#v", normalized)
	}
}

func TestFirstAPIKey(t *testing.T) {
	t.Parallel()

	if first := firstAPIKey(nil); first != "" {
		t.Fatalf("expected empty, got %q", first)
	}

	if first := firstAPIKey([]string{"key1", "key2"}); first != "key1" {
		t.Fatalf("expected key1, got %q", first)
	}
}

func TestProviderAPIKeys(t *testing.T) {
	t.Parallel()

	keys := providerAPIKeys("primary", []string{"fallback1", "fallback2"})

	expected := []string{"primary", "fallback1", "fallback2"}
	if !reflect.DeepEqual(keys, expected) {
		t.Fatalf("expected %#v, got %#v", expected, keys)
	}
}

func TestPrimaryAPIKey(t *testing.T) {
	t.Parallel()

	t.Run("providerConfig", func(t *testing.T) {
		t.Parallel()

		provider := providerConfig{
			Name:            "openai",
			BaseURL:         "https://api.openai.com/v1",
			APIKey:          t.Name() + "-p1",
			APIKeys:         []string{t.Name() + "-p2", t.Name() + "-p3"},
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		}

		if key := provider.primaryAPIKey(); key != t.Name()+"-p1" {
			t.Fatalf("expected primary key, got %q", key)
		}
	})

	t.Run("providerRequestConfig", func(t *testing.T) {
		t.Parallel()

		provider := providerRequestConfig{
			APIKind:         providerAPIKindOpenAI,
			BaseURL:         "https://api.openai.com/v1",
			APIKey:          t.Name() + "-r1",
			APIKeys:         []string{t.Name() + "-r2", t.Name() + "-r3"},
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		}

		if key := provider.primaryAPIKey(); key != t.Name()+"-r1" {
			t.Fatalf("expected primary key, got %q", key)
		}
	})

	t.Run("searchConfigs", func(t *testing.T) {
		t.Parallel()

		exaCfg := exaSearchConfig{
			APIKey:            t.Name() + "-e1",
			APIKeys:           []string{t.Name() + "-e2"},
			SearchType:        "auto",
			TextMaxCharacters: 15000,
		}
		if key := exaCfg.primaryAPIKey(); key != t.Name()+"-e1" {
			t.Fatalf("expected primary exa key, got %q", key)
		}

		tavilyCfg := tavilySearchConfig{
			APIKey:  t.Name() + "-t1",
			APIKeys: []string{t.Name() + "-t2"},
		}
		if key := tavilyCfg.primaryAPIKey(); key != t.Name()+"-t1" {
			t.Fatalf("expected primary tavily key, got %q", key)
		}

		serpAPICfg := serpAPIVisualSearchConfig{
			APIKey:  t.Name() + "-s1",
			APIKeys: []string{t.Name() + "-s2"},
		}
		if key := serpAPICfg.primaryAPIKey(); key != t.Name()+"-s1" {
			t.Fatalf("expected primary serp api key, got %q", key)
		}
	})
}
