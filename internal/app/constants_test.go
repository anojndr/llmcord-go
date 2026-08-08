package app

import (
	"testing"
)

func TestExaSearchTypesLatencyOrder(t *testing.T) {
	t.Parallel()

	expectedOrder := []string{
		exaSearchTypeInstant,
		exaSearchTypeFast,
		exaSearchTypeAuto,
		exaSearchTypeDeepLite,
		exaSearchTypeDeep,
		exaSearchTypeDeepReasoning,
	}

	got := exaSearchTypes()

	if len(got) != len(expectedOrder) {
		t.Fatalf("len(exaSearchTypes()) = %d, want %d", len(got), len(expectedOrder))
	}

	for index, expected := range expectedOrder {
		if got[index] != expected {
			t.Fatalf("exaSearchTypes()[%d] = %q, want %q", index, got[index], expected)
		}
	}
}
