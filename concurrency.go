package main

import (
	"context"
	"sync"
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
	indices := make(chan int)

	var waitGroup sync.WaitGroup

	for range workerCount {
		waitGroup.Go(func() {
			for index := range indices {
				value, err := task(ctx, index)
				results[index] = boundedTaskResult[T]{
					value: value,
					err:   err,
				}
			}
		})
	}

	for index := range taskCount {
		indices <- index
	}

	close(indices)
	waitGroup.Wait()

	return results
}
