package app

import (
	"testing"
	"time"
)

func TestTinyFishFetchLatencyBudgets(t *testing.T) {
	t.Parallel()

	if tinyFishFetchPerURLTimeoutMS <= 0 || tinyFishFetchPerURLTimeoutMS > 5000 {
		t.Fatalf("tinyFishFetchPerURLTimeoutMS = %d, want in (0, 5000]", tinyFishFetchPerURLTimeoutMS)
	}

	if tinyFishFetchRequestTimeout <= 0 || tinyFishFetchRequestTimeout > 30*time.Second {
		t.Fatalf("tinyFishFetchRequestTimeout = %s, want in (0, 30s]", tinyFishFetchRequestTimeout)
	}

	if tinyFishFetchRequestTimeout <= time.Duration(tinyFishFetchPerURLTimeoutMS)*time.Millisecond {
		t.Fatalf(
			"tinyFishFetchRequestTimeout = %s must exceed per-URL budget %dms to fit a batch",
			tinyFishFetchRequestTimeout,
			tinyFishFetchPerURLTimeoutMS,
		)
	}
}

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
