package app

import (
	"context"
)

type urlContentFetcher[T any] func(context.Context, string) (T, error)

func fetchConcurrentURLContent[T any](
	ctx context.Context,
	urls []string,
	fetcher urlContentFetcher[T],
	logMessage string,
	warningText string,
) ([]T, []string) {
	taskResults := runTasksConcurrently(
		ctx,
		externalRequestConcurrency,
		len(urls),
		func(taskContext context.Context, index int) (T, error) {
			return fetcher(taskContext, urls[index])
		},
	)

	fetchFailed := false
	formattedResults := make([]T, 0, len(taskResults))

	for index, result := range taskResults {
		if result.err != nil {
			logWarn(logMessage, result.err, "url", urls[index])

			fetchFailed = true

			continue
		}

		formattedResults = append(formattedResults, result.value)
	}

	warnings := make([]string, 0, 1)
	if fetchFailed {
		warnings = append(warnings, warningText)
	}

	return formattedResults, warnings
}

func prepareConcurrentURLContentAugmentation[T any](
	ctx context.Context,
	urls []string,
	fetcher urlContentFetcher[T],
	logMessage string,
	warningText string,
	formatContent func([]T) string,
	appendContent func([]chatMessage, string) ([]chatMessage, error),
) (preparedConversationAugmentation, error) {
	results, warnings := fetchConcurrentURLContent(
		ctx,
		urls,
		fetcher,
		logMessage,
		warningText,
	)

	if len(results) == 0 {
		return warningPreparedConversationAugmentation(warnings), nil
	}

	formattedContent := formatContent(results)

	return newPreparedConversationAugmentation(
		warnings,
		nil,
		func(conversation []chatMessage) ([]chatMessage, error) {
			return appendContent(conversation, formattedContent)
		},
	), nil
}
