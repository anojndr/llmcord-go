package main

import (
	"context"
	"sync"
	"sync/atomic"
)

type boundedTaskResult[T any] struct {
	value T
	err   error
}

func runTasksConcurrently[T any](
	ctx context.Context,
	limit int,
	taskCount int,
	task func(context.Context, int) (T, error),
) []boundedTaskResult[T] {
	if taskCount <= 0 {
		return nil
	}

	workerCount := limit
	if workerCount <= 0 || workerCount > taskCount {
		workerCount = taskCount
	}

	results := make([]boundedTaskResult[T], taskCount)

	var currentTaskIndex atomic.Int64

	var waitGroup sync.WaitGroup

	for range workerCount {
		waitGroup.Go(func() {
			for {
				index := int(currentTaskIndex.Add(1) - 1)
				if index >= taskCount {
					return
				}

				ctxErr := ctx.Err()
				if ctxErr != nil {
					results[index].err = ctxErr

					continue
				}

				value, err := task(ctx, index)

				results[index] = boundedTaskResult[T]{
					value: value,
					err:   err,
				}
			}
		})
	}

	waitGroup.Wait()

	return results
}
