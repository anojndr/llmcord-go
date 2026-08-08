package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireInstanceLockRejectsSecondInstanceSameConfig(t *testing.T) {
	t.Parallel()
	configPath := writeInstanceLockTestConfig(t)

	firstLock, err := AcquireInstanceLock(configPath)
	if err != nil {
		t.Fatalf("first instance lock: %v", err)
	}
	defer firstLock.Release()

	secondLock, err := AcquireInstanceLock(configPath)
	if err == nil {
		secondLock.Release()

		t.Fatal("expected second instance lock for the same config to be rejected")
	}
}

func TestAcquireInstanceLockAllowsDifferentConfigs(t *testing.T) {
	t.Parallel()

	firstLock, err := AcquireInstanceLock(writeInstanceLockTestConfig(t))
	if err != nil {
		t.Fatalf("lock config a: %v", err)
	}
	defer firstLock.Release()

	secondLock, err := AcquireInstanceLock(writeInstanceLockTestConfig(t))
	if err != nil {
		t.Fatalf("lock config b: %v", err)
	}
	defer secondLock.Release()
}

func TestAcquireInstanceLockAllowsReacquisitionAfterRelease(t *testing.T) {
	t.Parallel()
	configPath := writeInstanceLockTestConfig(t)

	firstLock, err := AcquireInstanceLock(configPath)
	if err != nil {
		t.Fatalf("first instance lock: %v", err)
	}

	firstLock.Release()

	secondLock, err := AcquireInstanceLock(configPath)
	if err != nil {
		t.Fatalf("lock reacquisition after release: %v", err)
	}
	defer secondLock.Release()
}

func TestAcquireInstanceLockContentMatch(t *testing.T) {
	t.Parallel()
	configPath := writeInstanceLockTestConfig(t)

	lock, err := AcquireInstanceLock(configPath)
	if err != nil {
		t.Fatalf("instance lock: %v", err)
	}
	defer lock.Release()

	if lock.Path != filepath.Clean(configPath) {
		t.Fatalf("lock path = %q, want %q", lock.Path, filepath.Clean(configPath))
	}
}

func TestAcquireInstanceLockRejectsMissingConfig(t *testing.T) {
	t.Parallel()
	missingConfigPath := filepath.Join(t.TempDir(), "missing.yaml")

	lock, err := AcquireInstanceLock(missingConfigPath)
	if err == nil {
		lock.Release()

		t.Fatal("expected lock on a missing config to fail")
	}
}

func writeInstanceLockTestConfig(t *testing.T) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.yaml")

	err := os.WriteFile(configPath, []byte("bot_token: test\n"), 0o600)
	if err != nil {
		t.Fatalf("write test config: %v", err)
	}

	return configPath
}
