package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

type urlContentFetcher[T any] func(context.Context, string) (T, error)

func augmentConversationWithConcurrentURLContent[T any](
	ctx context.Context,
	conversation []chatMessage,
	urls []string,
	fetcher urlContentFetcher[T],
	logMessage string,
	warningText string,
	formatContent func([]T) string,
	appendContent func([]chatMessage, string) ([]chatMessage, error),
	appendErrorMessage string,
) ([]chatMessage, []T, []string, error) {
	results := make([]T, len(urls))
	successful := make([]bool, len(urls))

	var (
		fetchFailed bool
		failedMu    sync.Mutex
		waitGroup   sync.WaitGroup
	)

	for index, rawURL := range urls {
		waitGroup.Add(1)

		go func(index int, rawURL string) {
			defer waitGroup.Done()

			result, fetchErr := fetcher(ctx, rawURL)
			if fetchErr != nil {
				slog.Warn(logMessage, "url", rawURL, "error", fetchErr)
				failedMu.Lock()
				fetchFailed = true
				failedMu.Unlock()

				return
			}

			results[index] = result
			successful[index] = true
		}(index, rawURL)
	}

	waitGroup.Wait()

	formattedResults := make([]T, 0, len(results))
	for index, result := range results {
		if !successful[index] {
			continue
		}

		formattedResults = append(formattedResults, result)
	}

	warnings := make([]string, 0, 1)
	if fetchFailed {
		warnings = append(warnings, warningText)
	}

	if len(formattedResults) == 0 {
		return conversation, nil, warnings, nil
	}

	augmentedConversation, err := appendContent(
		conversation,
		formatContent(formattedResults),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", appendErrorMessage, err)
	}

	return augmentedConversation, formattedResults, warnings, nil
}
