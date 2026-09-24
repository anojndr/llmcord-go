package app

import (
	"context"
	"strings"
	"sync"
)

// tinyFishEarlyBatcher overlaps fetch with still-running searches without
// splitting the deduplicated batch: each finished query contributes its URLs
// to a shared ordered set, and full 10-URL batches dispatch immediately
// while straggler searches still run. The final enrich pass remains
// authoritative for the remainder (<10 URLs) and any fetch failures, so
// query count, fetch volume, and full content are unchanged. Small unions
// (<10 URLs, the common and tested case) never trigger an early fetch and
// keep the exact single-batch behavior.
type tinyFishEarlyBatcher struct {
	apiKeys    []string
	fetchCache *tinyFishFetchCache
	mu         sync.Mutex
	claimed    map[string]struct{}
	pending    []string
}

func newTinyFishEarlyBatcher(
	apiKeys []string,
	fetchCache *tinyFishFetchCache,
) tinyFishEarlyBatcher {
	return tinyFishEarlyBatcher{
		apiKeys:    append([]string(nil), apiKeys...),
		fetchCache: fetchCache,
		mu:         sync.Mutex{},
		claimed:    make(map[string]struct{}),
		pending:    nil,
	}
}

func (shared *tinyFishEarlyBatcher) addAndMaybeFetch(
	ctx context.Context,
	client tinyFishSearchClient,
	results []tinyFishSearchResult,
) {
	if shared == nil || len(results) == 0 {
		return
	}

	for {
		batch := shared.takeFullBatch(results)
		if len(batch) == 0 {
			return
		}

		results = nil

		fetchResponse, fetchErr := tryAllAPIKeys(
			ctx,
			client.keys,
			shared.apiKeys,
			func(apiKey string) (tinyFishFetchResponse, error) {
				return client.fetchContents(ctx, apiKey, batch)
			},
		)
		if fetchErr != nil {
			return
		}

		for _, fetchResult := range fetchResponse.Results {
			textStr := strings.TrimSpace(tinyFishFetchResultText(fetchResult.Text))
			if textStr == "" {
				continue
			}

			var title string
			if fetchResult.Title != nil {
				title = strings.TrimSpace(*fetchResult.Title)
			}

			var description string
			if fetchResult.Description != nil {
				description = strings.TrimSpace(*fetchResult.Description)
			}

			client.fetchCache.store(fetchResult.URL, fetchResult.FinalURL, textStr, title, description)
		}
	}
}

func (shared *tinyFishEarlyBatcher) takeFullBatch(results []tinyFishSearchResult) []string {
	shared.mu.Lock()
	defer shared.mu.Unlock()

	for _, result := range results {
		trimmedURL := strings.TrimSpace(result.URL)
		if trimmedURL == "" {
			continue
		}

		key := strings.ToLower(trimmedURL)
		if _, claimed := shared.claimed[key]; claimed {
			continue
		}

		if _, _, _, _, _, ok := shared.fetchCache.lookup(trimmedURL); ok {
			shared.claimed[key] = struct{}{}

			continue
		}

		shared.claimed[key] = struct{}{}
		shared.pending = append(shared.pending, trimmedURL)
	}

	if len(shared.pending) < tinyFishFetchURLsPerBatch {
		return nil
	}

	batch := append([]string(nil), shared.pending[:tinyFishFetchURLsPerBatch]...)
	shared.pending = append([]string(nil), shared.pending[tinyFishFetchURLsPerBatch:]...)

	return batch
}
