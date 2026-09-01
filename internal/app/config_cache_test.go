package app

import (
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestLoadConfigCachedReturnsCachedUntilFileChanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte("bot_token: test-token\nproviders:\n  alpha:\n    base_url: https://alpha.example\n  beta:\n    base_url: https://beta.example\nmodels:\n  alpha/provider:na:\n  beta/provider:nb:\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	instance := new(bot)
	instance.configPath = path

	loadedConfig, err := instance.loadConfigCached()
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}

	if len(loadedConfig.ModelOrder) != 2 || loadedConfig.ModelOrder[0] != "alpha/provider:na" {
		t.Fatalf("unexpected initial model order: %v", loadedConfig.ModelOrder)
	}

	if instance.configCache == nil {
		t.Fatal("expected config cache to be populated")
	}

	// Cached repeat load must not re-parse.
	instance.configCache.ModelOrder[0] = "sentinel-from-cache"

	reloaded, err := instance.loadConfigCached()
	if err != nil {
		t.Fatalf("cached load: %v", err)
	}

	if reloaded.ModelOrder[0] != "sentinel-from-cache" {
		t.Fatalf("expected cached config reuse, got %v", reloaded.ModelOrder)
	}

	// Touch the file (new content) and force a distinct mtime.
	if err := os.WriteFile(path, []byte("bot_token: test-token\nproviders:\n  gamma:\n    base_url: https://gamma.example\nmodels:\n  gamma/provider:ng:\n"), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	refreshed, err := instance.loadConfigCached()
	if err != nil {
		t.Fatalf("refresh load: %v", err)
	}

	if len(refreshed.ModelOrder) != 1 || refreshed.ModelOrder[0] != "gamma/provider:ng" {
		t.Fatalf("expected refreshed config after file change, got %v", refreshed.ModelOrder)
	}

	if instance.configCache.ModelOrder[0] != "gamma/provider:ng" {
		t.Fatal("expected cache to store refreshed config")
	}
}

func TestSeedConfigCachePopulatesCache(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte("models:\n  alpha/provider:na:\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	instance := new(bot)
	instance.configPath = path
	instance.seedConfigCache(config{ModelOrder: []string{"alpha/provider:na"}})

	if instance.configCache == nil {
		t.Fatal("expected seeded cache")
	}

	loadedConfig, err := instance.loadConfigCached()
	if err != nil {
		t.Fatalf("cached load: %v", err)
	}

	if loadedConfig.ModelOrder[0] != "alpha/provider:na" {
		t.Fatalf("unexpected seeded model order: %v", loadedConfig.ModelOrder)
	}
}

func TestChannelByIDUsesTTLCache(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)

		return newInteractionJSONResponse(request, 200, `{"id":"chan-1","name":"general"}`), nil
	})
	session.Client = client

	instance := new(bot)
	instance.session = session

	first, err := instance.channelByID("chan-1")
	if err != nil {
		t.Fatalf("first channelByID: %v", err)
	}

	second, err := instance.channelByID("chan-1")
	if err != nil {
		t.Fatalf("second channelByID: %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("channel mismatch: %q vs %q", first.ID, second.ID)
	}

	if requests.Load() != 1 {
		t.Fatalf("expected 1 REST fetch, got %d", requests.Load())
	}

	// Expired entry triggers a second fetch.
	entry := instance.channelCache["chan-1"]
	entry.expires = time.Now().Add(-time.Second)
	instance.channelCache["chan-1"] = entry

	_, err = instance.channelByID("chan-1")
	if err != nil {
		t.Fatalf("third channelByID: %v", err)
	}

	if requests.Load() != 2 {
		t.Fatalf("expected 2 REST fetches after expiry, got %d", requests.Load())
	}
}
