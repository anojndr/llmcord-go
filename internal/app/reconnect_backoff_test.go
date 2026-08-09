package app

import (
	"testing"
)

func TestReconnectBackoffCapsShrinkWithAttempts(t *testing.T) {
	t.Parallel()

	caps := reconnectAttemptCap(discordReconnectImmediateBackoffCapsSeconds(), 0)
	if caps != 5 {
		t.Fatalf("first attempt cap = %d, want 5", caps)
	}

	caps = reconnectAttemptCap(discordReconnectImmediateBackoffCapsSeconds(), 1)
	if caps != 10 {
		t.Fatalf("second attempt cap = %d, want 10", caps)
	}

	caps = reconnectAttemptCap(discordReconnectImmediateBackoffCapsSeconds(), 2)
	if caps != 20 {
		t.Fatalf("third attempt cap = %d, want 20", caps)
	}

	caps = reconnectAttemptCap(discordReconnectImmediateBackoffCapsSeconds(), 5)
	if caps != 120 {
		t.Fatalf("later attempt cap = %d, want 120 (global cap)", caps)
	}
}

func TestReconnectGlobalCapAppliesWhenTiersExceedIt(t *testing.T) {
	t.Parallel()

	caps := reconnectAttemptCap([]int64{200}, 0)
	if caps != discordReconnectBackoffCapSeconds {
		t.Fatalf("cap = %d, want global cap %d", caps, discordReconnectBackoffCapSeconds)
	}
}

func TestReconnectProbeBackoffCaps(t *testing.T) {
	t.Parallel()

	caps := reconnectAttemptCap(discordReconnectProbeBackoffCapsSeconds(), 0)
	if caps != 20 {
		t.Fatalf("first probe backoff cap = %d, want 20", caps)
	}

	caps = reconnectAttemptCap(discordReconnectProbeBackoffCapsSeconds(), 3)
	if caps != 120 {
		t.Fatalf("fourth probe backoff cap = %d, want 120", caps)
	}

	caps = reconnectAttemptCap(discordReconnectProbeBackoffCapsSeconds(), 9)
	if caps != 120 {
		t.Fatalf("later probe backoff cap = %d, want 120", caps)
	}
}

func TestReconnectCapClampedToGlobalBackoffCap(t *testing.T) {
	t.Parallel()

	caps := reconnectAttemptCap(discordReconnectProbeBackoffCapsSeconds(), 0)
	if caps > discordReconnectBackoffCapSeconds {
		t.Fatalf("cap %d exceeds global cap %d", caps, discordReconnectBackoffCapSeconds)
	}
}

func TestReconnectBackoffBase(t *testing.T) {
	t.Parallel()

	if discordReconnectImmediateBackoffBaseSeconds <= 0 {
		t.Fatal("immediate backoff base must be positive")
	}

	if discordReconnectProbeBackoffBaseSeconds <= discordReconnectImmediateBackoffBaseSeconds {
		t.Fatal("probe backoff base must exceed the immediate base")
	}
}
