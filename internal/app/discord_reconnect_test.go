package app

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func writeReconnectTestConfig(t *testing.T, statusMessage string) string {
	t.Helper()

	configText := "bot_token: discord-token\n" +
		"providers:\n" +
		"  openai:\n" +
		"    base_url: https://api.example.com/v1\n" +
		"models:\n" +
		"  openai/first-model:\n"
	if statusMessage != "" {
		configText += "status_message: " + statusMessage + "\n"
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	return configPath
}

func newReconnectTestBot(t *testing.T, configPath string) *bot {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	instance := new(bot)
	instance.configPath = configPath
	instance.seedConfigCache(loadedConfig)
	instance.session = session
	instance.onlineOutput = new(bytes.Buffer)
	instance.maintenanceChannels = make(map[string]struct{})

	return instance
}

func TestNewBotEnablesDiscordReconnect(t *testing.T) {
	t.Parallel()

	configPath := writeReconnectTestConfig(t, "")

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	instance, err := newBot(t.Context(), configPath, loadedConfig)
	if err != nil {
		t.Fatalf("newBot: %v", err)
	}

	if !instance.session.ShouldReconnectOnError {
		t.Fatal("expected ShouldReconnectOnError to stay enabled")
	}
}

func TestReconnectRestoresCustomStatusAfterResume(t *testing.T) {
	t.Parallel()

	expectedStatus := statusMessage("reconnected presence")
	configPath := writeReconnectTestConfig(t, "reconnected presence")
	instance := newReconnectTestBot(t, configPath)

	var calls atomic.Int64

	var gotStatus atomic.Value

	instance.updateCustomStatus = func(_ *discordgo.Session, status string) error {
		calls.Add(1)
		gotStatus.Store(status)

		return nil
	}

	instance.handleResumed(nil, new(discordgo.Resumed))

	if calls.Load() != 1 {
		t.Fatalf("expected one status restore, got %d", calls.Load())
	}

	restored, ok := gotStatus.Load().(string)
	if !ok || restored != expectedStatus {
		t.Fatalf("restored status = %q, want %q", restored, expectedStatus)
	}
}

func TestReconnectSkipsStatusRestoreWithoutSession(t *testing.T) {
	t.Parallel()

	configPath := writeReconnectTestConfig(t, "reconnected presence")
	instance := newReconnectTestBot(t, configPath)
	instance.session = nil

	called := new(atomic.Bool)
	instance.updateCustomStatus = func(_ *discordgo.Session, _ string) error {
		called.Store(true)

		return nil
	}

	instance.handleResumed(nil, new(discordgo.Resumed))

	if called.Load() {
		t.Fatal("expected no status restore without a session")
	}
}
