package main

import (
	"reflect"
	"slices"
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

func TestRoundRobinAPIKeys(t *testing.T) {
	t.Parallel()

	t.Run("single key", func(t *testing.T) {
		t.Parallel()

		rotator := newAPIKeyRotator()

		keys := rotator.rotate([]string{"key1"})
		if !slices.Equal(keys, []string{"key1"}) {
			t.Fatalf("expected single key unchanged, got %#v", keys)
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()

		rotator := newAPIKeyRotator()

		if keys := rotator.rotate(nil); len(keys) != 0 {
			t.Fatalf("expected no keys, got %#v", keys)
		}
	})

	t.Run("distinct key sets rotate independently", func(t *testing.T) {
		t.Parallel()

		rotator := newAPIKeyRotator()

		set := []string{"a", "b", "c"}
		other := []string{"x", "y"}

		firstCall := rotator.rotate(set)
		secondCall := rotator.rotate(set)
		otherFirst := rotator.rotate(other)
		otherSecond := rotator.rotate(other)

		if !slices.Equal(firstCall, []string{"a", "b", "c"}) {
			t.Fatalf("expected first rotation to start with the primary key, got %#v", firstCall)
		}

		if !slices.Equal(secondCall, []string{"b", "c", "a"}) {
			t.Fatalf("expected second rotation to start with the second key, got %#v", secondCall)
		}

		if !slices.Equal(otherFirst, []string{"x", "y"}) || !slices.Equal(otherSecond, []string{"y", "x"}) {
			t.Fatalf("expected independent rotation for a distinct key set, got %#v and %#v", otherFirst, otherSecond)
		}
	})

	t.Run("wraps around to the first key", func(t *testing.T) {
		t.Parallel()

		rotator := newAPIKeyRotator()

		set := []string{"a", "b"}

		first := rotator.rotate(set)
		if !slices.Equal(first, []string{"a", "b"}) {
			t.Fatalf("expected first rotation to start with the primary key, got %#v", first)
		}

		second := rotator.rotate(set)
		if !slices.Equal(second, []string{"b", "a"}) {
			t.Fatalf("expected second rotation to swap the keys, got %#v", second)
		}

		wrapped := rotator.rotate(set)
		if !slices.Equal(wrapped, []string{"a", "b"}) {
			t.Fatalf("expected rotation to wrap around, got %#v", wrapped)
		}
	})

	t.Run("never mutates the input", func(t *testing.T) {
		t.Parallel()

		rotator := newAPIKeyRotator()

		input := []string{"a", "b", "c"}
		rotated := rotator.rotate(input)

		if !slices.Equal(input, []string{"a", "b", "c"}) {
			t.Fatalf("expected input unchanged, got %#v", input)
		}

		rotated[0] = "changed"

		if !slices.Equal(input, []string{"a", "b", "c"}) {
			t.Fatalf("expected rotation result to be a copy, got %#v", input)
		}
	})
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
