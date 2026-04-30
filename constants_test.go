package main

import (
	"testing"
	"time"
)

func TestSearchDeciderTimeout(t *testing.T) {
	t.Parallel()

	if searchDeciderTimeout != time.Minute {
		t.Fatalf("search decider timeout = %s, want %s", searchDeciderTimeout, time.Minute)
	}
}
