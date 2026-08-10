package main

import (
	"path/filepath"
	"testing"

	app "llmcord-go/internal/app"
)

func TestRunMainReturnsFailureWhenStartupConfigCannotLoad(t *testing.T) {
	missingConfigPath := filepath.Join(t.TempDir(), "missing-config.yaml")
	t.Setenv(app.ConfigPathEnvironmentVariable, missingConfigPath)

	if got := runMain(); got != 1 {
		t.Fatalf("runMain() = %d, want 1", got)
	}
}
