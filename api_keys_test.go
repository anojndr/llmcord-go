package main

import (
	"reflect"
	"sync"
	"testing"
)

func TestRoundRobinAPIKeysBasic(t *testing.T) {
	t.Parallel()

	if res := roundRobinAPIKeys(nil); res != nil {
		t.Fatalf("expected nil, got %#v", res)
	}

	single := []string{"key1"}
	if res := roundRobinAPIKeys(single); !reflect.DeepEqual(res, single) {
		t.Fatalf("expected %#v, got %#v", single, res)
	}
}

func TestRoundRobinAPIKeysRotation(t *testing.T) {
	t.Parallel()

	keys := []string{t.Name() + "-k1", t.Name() + "-k2", t.Name() + "-k3"}

	first := roundRobinAPIKeys(keys)
	if expected := []string{t.Name() + "-k1", t.Name() + "-k2", t.Name() + "-k3"}; !reflect.DeepEqual(first, expected) {
		t.Fatalf("expected %#v, got %#v", expected, first)
	}

	second := roundRobinAPIKeys(keys)
	if expected := []string{t.Name() + "-k2", t.Name() + "-k3", t.Name() + "-k1"}; !reflect.DeepEqual(second, expected) {
		t.Fatalf("expected %#v, got %#v", expected, second)
	}

	third := roundRobinAPIKeys(keys)
	if expected := []string{t.Name() + "-k3", t.Name() + "-k1", t.Name() + "-k2"}; !reflect.DeepEqual(third, expected) {
		t.Fatalf("expected %#v, got %#v", expected, third)
	}

	fourth := roundRobinAPIKeys(keys)
	if expected := []string{t.Name() + "-k1", t.Name() + "-k2", t.Name() + "-k3"}; !reflect.DeepEqual(fourth, expected) {
		t.Fatalf("expected %#v, got %#v", expected, fourth)
	}
}

func TestRoundRobinAPIKeysConcurrent(t *testing.T) {
	t.Parallel()

	const (
		numGoroutines = 10
		numIterations = 100
	)

	keys := []string{t.Name() + "-c1", t.Name() + "-c2", t.Name() + "-c3"}

	var waitGroup sync.WaitGroup

	waitGroup.Add(numGoroutines)

	for range numGoroutines {
		go func() {
			defer waitGroup.Done()

			for range numIterations {
				res := roundRobinAPIKeys(keys)
				if len(res) != 3 {
					t.Errorf("expected 3 keys, got %d", len(res))
				}
			}
		}()
	}

	waitGroup.Wait()
}

func TestAPIKeysForAttemptsRoundRobin(t *testing.T) {
	t.Parallel()

	t.Run("providerRequestConfig", func(t *testing.T) {
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

		req := chatCompletionRequest{
			Provider: providerRequestConfig{
				APIKind:         providerAPIKindOpenAI,
				BaseURL:         "https://api.openai.com/v1",
				APIKey:          provider.primaryAPIKey(),
				APIKeys:         provider.apiKeys(),
				UseResponsesAPI: false,
				EnableGrounding: false,
				ExtraHeaders:    nil,
				ExtraQuery:      nil,
				ExtraBody:       nil,
			},
			Model:                       "gpt-test",
			ConfiguredModel:             "gpt-test",
			ContextWindow:               128000,
			AutoCompactThresholdPercent: 90,
			SessionID:                   "",
			PreviousResponseID:          "",
			RequestID:                   "",
			Messages:                    nil,
		}

		first := req.Provider.apiKeysForAttempts()

		expectedFirst := []string{t.Name() + "-p1", t.Name() + "-p2", t.Name() + "-p3"}
		if !reflect.DeepEqual(first, expectedFirst) {
			t.Fatalf("expected %#v, got %#v", expectedFirst, first)
		}

		second := req.Provider.apiKeysForAttempts()

		expectedSecond := []string{t.Name() + "-p2", t.Name() + "-p3", t.Name() + "-p1"}
		if !reflect.DeepEqual(second, expectedSecond) {
			t.Fatalf("expected %#v, got %#v", expectedSecond, second)
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

		e1 := exaCfg.apiKeysForAttempts()

		expectedE1 := []string{t.Name() + "-e1", t.Name() + "-e2"}
		if !reflect.DeepEqual(e1, expectedE1) {
			t.Fatalf("expected %#v, got %#v", expectedE1, e1)
		}

		e2 := exaCfg.apiKeysForAttempts()

		expectedE2 := []string{t.Name() + "-e2", t.Name() + "-e1"}
		if !reflect.DeepEqual(e2, expectedE2) {
			t.Fatalf("expected %#v, got %#v", expectedE2, e2)
		}
	})
}
