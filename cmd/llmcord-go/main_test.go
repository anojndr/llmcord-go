package main

import (
	"path/filepath"
	"testing"

	config "llmcord-go/internal/config"
)

func TestRunMainReturnsFailureWhenStartupConfigCannotLoad(t *testing.T) {
	missingConfigPath := filepath.Join(t.TempDir(), "missing-config.yaml")
	t.Setenv(config.ConfigPathEnvironmentVariable, missingConfigPath)

	if got := runMain(); got != 1 {
		t.Fatalf("runMain() = %d, want 1", got)
	}
}
