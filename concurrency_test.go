package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

const (
	concurrencyTestFirstValue  = "first"
	concurrencyTestSecondValue = "second"
)

func TestRunTasksConcurrentlyRunsWorkConcurrentlyAndPreservesOrder(t *testing.T) {
	t.Parallel()

	var (
		startedCount int
		startedMu    sync.Mutex
		release      = make(chan struct{})
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	results := runTasksConcurrently(
		ctx,
		2,
		2,
		func(taskContext context.Context, index int) (string, error) {
			startedMu.Lock()
			startedCount++

			if startedCount == 2 {
				close(release)
			}
			startedMu.Unlock()

			select {
			case <-release:
			case <-taskContext.Done():
				return "", taskContext.Err()
			}

			if index == 0 {
				return concurrencyTestFirstValue, nil
			}

			return concurrencyTestSecondValue, nil
		},
	)

	if results[0].err != nil || results[1].err != nil {
		t.Fatalf("unexpected task errors: %#v", results)
	}

	if results[0].value != concurrencyTestFirstValue ||
		results[1].value != concurrencyTestSecondValue {
		t.Fatalf("unexpected ordered results: %#v", results)
	}
}
