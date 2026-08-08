package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

func TestRunTasksConcurrentlyLimitsWorkers(t *testing.T) {
	t.Parallel()

	taskCount := externalRequestConcurrency + 1
	started := make(chan struct{}, taskCount)
	release := make(chan struct{})

	var releaseOnce sync.Once

	t.Cleanup(func() {
		releaseOnce.Do(func() {
			close(release)
		})
	})

	resultChannel := make(chan []boundedTaskResult[int], 1)

	go func() {
		resultChannel <- runTasksConcurrently(
			t.Context(),
			externalRequestConcurrency,
			taskCount,
			func(taskContext context.Context, index int) (int, error) {
				started <- struct{}{}

				select {
				case <-release:
					return index, nil
				case <-taskContext.Done():
					return 0, taskContext.Err()
				}
			},
		)
	}()

	for range externalRequestConcurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for bounded workers")
		}
	}

	select {
	case <-started:
		t.Fatalf("more than %d tasks started concurrently", externalRequestConcurrency)
	case <-time.After(100 * time.Millisecond):
	}

	releaseOnce.Do(func() {
		close(release)
	})

	select {
	case results := <-resultChannel:
		if len(results) != taskCount {
			t.Fatalf("result count = %d, want %d", len(results), taskCount)
		}

		for index, result := range results {
			if result.err != nil {
				t.Fatalf("result %d error: %v", index, result.err)
			}

			if result.value != index {
				t.Fatalf("result %d value = %d", index, result.value)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bounded tasks")
	}
}

func TestRunTasksConcurrentlySkipsTasksAfterCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var calls atomic.Int32

	results := runTasksConcurrently(
		ctx,
		externalRequestConcurrency,
		externalRequestConcurrency+1,
		func(context.Context, int) (int, error) {
			calls.Add(1)

			return 0, nil
		},
	)

	if calls.Load() != 0 {
		t.Fatalf("task calls = %d, want 0", calls.Load())
	}

	for index, result := range results {
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("result %d error = %v, want context canceled", index, result.err)
		}
	}
}
