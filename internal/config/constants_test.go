package config

import (
	"testing"
)

func TestExaSearchTypesLatencyOrder(t *testing.T) {
	t.Parallel()

	expectedOrder := []string{
		ExaSearchTypeInstant,
		ExaSearchTypeFast,
		ExaSearchTypeAuto,
		ExaSearchTypeDeepLite,
		ExaSearchTypeDeep,
		ExaSearchTypeDeepReasoning,
	}

	got := ExaSearchTypes()

	if len(got) != len(expectedOrder) {
		t.Fatalf("len(ExaSearchTypes()) = %d, want %d", len(got), len(expectedOrder))
	}

	for index, expected := range expectedOrder {
		if got[index] != expected {
			t.Fatalf("ExaSearchTypes()[%d] = %q, want %q", index, got[index], expected)
		}
	}
}
