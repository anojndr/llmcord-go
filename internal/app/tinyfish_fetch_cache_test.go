package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTinyFishFetchCacheStoresAndReturnsResults(t *testing.T) {
	t.Parallel()

	cache := newTinyFishFetchCache()

	if _, _, _, _, _, ok := cache.lookup("https://example.com/a"); ok {
		t.Fatal("expected cache miss before store")
	}

	cache.store("https://example.com/a", "https://example.com/a-final", "full body", "Example", "desc")

	for _, candidate := range []string{
		"https://example.com/a",
		"https://EXAMPLE.com/a ",
		"https://example.com/a-final",
	} {
		text, title, description, resolvedURL, finalURL, ok := cache.lookup(candidate)
		if !ok {
			t.Fatalf("expected cache hit for %q", candidate)
		}

		if text != "full body" || title != "Example" || description != "desc" ||
			resolvedURL != "https://example.com/a" || finalURL != "https://example.com/a-final" {
			t.Fatalf("unexpected cached value for %q: %q %q %q %q %q", candidate, text, title, description, resolvedURL, finalURL)
		}
	}
}

func TestTinyFishFetchCacheExpiresEntries(t *testing.T) {
	t.Parallel()

	cache := newTinyFishFetchCache()
	cache.store("https://example.com/stale", "", "body", "", "")

	cache.mu.Lock()
	entry := cache.entries[normalizeTinyFishFetchCacheKey("https://example.com/stale")]
	entry.expiresAt = time.Now().Add(-time.Minute)
	cache.entries[normalizeTinyFishFetchCacheKey("https://example.com/stale")] = entry
	cache.mu.Unlock()

	if _, _, _, _, _, ok := cache.lookup("https://example.com/stale"); ok {
		t.Fatal("expected expired entry to miss")
	}

	if got := cache.len(); got != 0 {
		t.Fatalf("expected expired entry to be removed, cache size %d", got)
	}
}

func TestTinyFishFetchCacheEvictsBeyondCapacity(t *testing.T) {
	t.Parallel()

	cache := newTinyFishFetchCache()

	for i := range tinyFishFetchCacheMaxEntries + 50 {
		cache.store(fmt.Sprintf("https://example.com/page-%d", i), "", "body", "", "")
	}

	if got := cache.len(); got > tinyFishFetchCacheMaxEntries {
		t.Fatalf("cache exceeded capacity: %d > %d", got, tinyFishFetchCacheMaxEntries)
	}

	if _, _, _, _, _, ok := cache.lookup("https://example.com/page-0"); ok {
		t.Fatal("expected oldest entry to be evicted")
	}

	if _, _, _, _, _, ok := cache.lookup(fmt.Sprintf("https://example.com/page-%d", tinyFishFetchCacheMaxEntries+49)); !ok {
		t.Fatal("expected newest entry to be retained")
	}
}

func TestTinyFishFetchCacheNilReceiverIsSafe(t *testing.T) {
	t.Parallel()

	var cache *tinyFishFetchCache

	if _, _, _, _, _, ok := cache.lookup("https://example.com/a"); ok {
		t.Fatal("expected nil cache to miss")
	}

	cache.store("https://example.com/a", "", "body", "", "")

	if got := cache.len(); got != 0 {
		t.Fatalf("expected nil cache size 0, got %d", got)
	}
}

func TestTinyFishFetchCacheSkipsEmptyText(t *testing.T) {
	t.Parallel()

	cache := newTinyFishFetchCache()
	cache.store("https://example.com/empty", "", "   ", "Title", "")

	if _, _, _, _, _, ok := cache.lookup("https://example.com/empty"); ok {
		t.Fatal("expected empty content to stay uncached")
	}
}

type tinyFishSearchFetchCapture struct {
	searchCalls atomic.Int64
	fetchCalls  atomic.Int64
	fetchedURLs atomic.Value
}

func newTinyFishSearchFetchTestServer(
	t *testing.T,
	capture *tinyFishSearchFetchCapture,
	resultsByQuery map[string][]map[string]any,
	fetchStatus int,
) (*httptest.Server, *httptest.Server) {
	t.Helper()

	searchServer := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		capture.searchCalls.Add(1)

		query := request.URL.Query().Get("query")

		results, ok := resultsByQuery[query]
		if !ok {
			results = []map[string]any{}
		}

		rawResults := make([]any, 0, len(results))
		for _, result := range results {
			rawResults = append(rawResults, result)
		}

		responseWriter.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(responseWriter).Encode(map[string]any{
			"query":         query,
			"results":       rawResults,
			"total_results": len(rawResults),
			"page":          0,
		}); err != nil {
			t.Errorf("encode TinyFish search response: %v", err)
		}
	}))
	t.Cleanup(searchServer.Close)

	fetchServer := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		capture.fetchCalls.Add(1)

		var received struct {
			URLs []string `json:"urls"`
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode TinyFish fetch request: %v", err)
		}

		capture.fetchedURLs.Store(append([]string(nil), received.URLs...))

		responseWriter.Header().Set("Content-Type", "application/json")

		if fetchStatus != http.StatusOK {
			responseWriter.WriteHeader(fetchStatus)
			_, _ = responseWriter.Write([]byte(`{"error":"boom"}`))

			return
		}

		rawResults := make([]any, 0, len(received.URLs))
		for _, rawURL := range received.URLs {
			rawResults = append(rawResults, map[string]any{
				"url":         rawURL,
				"final_url":   rawURL,
				"title":       "Title for " + rawURL,
				"description": "Description for " + rawURL,
				"language":    "en",
				"format":      "markdown",
				"text":        "Full body for " + rawURL,
			})
		}

		if err := json.NewEncoder(responseWriter).Encode(map[string]any{
			"results": rawResults,
			"errors":  []any{},
		}); err != nil {
			t.Errorf("encode TinyFish fetch response: %v", err)
		}
	}))
	t.Cleanup(fetchServer.Close)

	return searchServer, fetchServer
}

func tinyFishSearchTestResult(position int, slug string) map[string]any {
	return map[string]any{
		"position":  position,
		"site_name": "example.com",
		"title":     "Title " + slug,
		"snippet":   "Snippet " + slug,
		"url":       "https://example.com/" + slug,
	}
}

func newTinyFishSearchTestClient(t *testing.T, searchServer, fetchServer *httptest.Server) tinyFishSearchClient {
	t.Helper()

	client := newTinyFishSearchClient(searchServer.Client())
	client.searchEndpoint = searchServer.URL
	client.fetchEndpoint = fetchServer.URL

	return client
}

func tinyFishSearchTestConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.TinyFish = tinyFishSearchConfig{
		APIKey:            "tf-test-key",
		APIKeys:           []string{"tf-test-key"},
		MaxCharsPerResult: defaultTinyFishMaxCharsPerResult,
	}

	return loadedConfig
}

func TestTinyFishSearchDeduplicatesFetchAcrossQueries(t *testing.T) {
	t.Parallel()

	var capture tinyFishSearchFetchCapture

	searchServer, fetchServer := newTinyFishSearchFetchTestServer(t, &capture, map[string][]map[string]any{
		"query one": {tinyFishSearchTestResult(1, "a"), tinyFishSearchTestResult(2, "shared")},
		"query two": {tinyFishSearchTestResult(1, "shared"), tinyFishSearchTestResult(2, "b")},
	}, http.StatusOK)

	client := newTinyFishSearchTestClient(t, searchServer, fetchServer)

	results, err := client.search(context.Background(), tinyFishSearchTestConfig(), []string{"query one", "query two"})
	if err != nil {
		t.Fatalf("search returned error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("unexpected result count: %d", len(results))
	}

	if got := capture.fetchCalls.Load(); got != 1 {
		t.Fatalf("expected 1 shared fetch request, got %d", got)
	}

	fetched, _ := capture.fetchedURLs.Load().([]string)
	if len(fetched) != 3 {
		t.Fatalf("expected 3 deduplicated fetch URLs, got %v", fetched)
	}

	seen := make(map[string]int)
	for _, rawURL := range fetched {
		seen[rawURL]++
	}

	for _, rawURL := range []string{"https://example.com/a", "https://example.com/shared", "https://example.com/b"} {
		if seen[rawURL] != 1 {
			t.Fatalf("expected exactly one fetch of %q, got %v", rawURL, fetched)
		}
	}

	for _, result := range results {
		if !strings.Contains(result.Text, "Full body for https://example.com/shared") {
			t.Fatalf("expected shared full content in %q result: %q", result.Query, result.Text)
		}
	}

	if !strings.Contains(results[0].Text, "Full body for https://example.com/a") {
		t.Fatalf("expected query-one content missing: %q", results[0].Text)
	}

	if !strings.Contains(results[1].Text, "Full body for https://example.com/b") {
		t.Fatalf("expected query-two content missing: %q", results[1].Text)
	}
}

func TestTinyFishSearchServesRepeatQueriesFromCache(t *testing.T) {
	t.Parallel()

	var capture tinyFishSearchFetchCapture

	searchServer, fetchServer := newTinyFishSearchFetchTestServer(t, &capture, map[string][]map[string]any{
		"repeat query": {tinyFishSearchTestResult(1, "cached")},
	}, http.StatusOK)

	client := newTinyFishSearchTestClient(t, searchServer, fetchServer)
	loadedConfig := tinyFishSearchTestConfig()

	first, err := client.search(context.Background(), loadedConfig, []string{"repeat query"})
	if err != nil {
		t.Fatalf("first search returned error: %v", err)
	}

	second, err := client.search(context.Background(), loadedConfig, []string{"repeat query"})
	if err != nil {
		t.Fatalf("second search returned error: %v", err)
	}

	if got := capture.fetchCalls.Load(); got != 1 {
		t.Fatalf("expected repeat query to skip fetch, fetch calls %d", got)
	}

	if got := capture.searchCalls.Load(); got != 2 {
		t.Fatalf("expected search to run on every call, search calls %d", got)
	}

	for index, results := range [][]webSearchResult{first, second} {
		if len(results) != 1 || !strings.Contains(results[0].Text, "Full body for https://example.com/cached") {
			t.Fatalf("call %d missing cached full content: %#v", index+1, results)
		}
	}
}

func TestTinyFishSearchFallsBackToSnippetsWhenFetchFails(t *testing.T) {
	t.Parallel()

	var capture tinyFishSearchFetchCapture

	searchServer, fetchServer := newTinyFishSearchFetchTestServer(t, &capture, map[string][]map[string]any{
		"fragile query": {tinyFishSearchTestResult(1, "fragile")},
	}, http.StatusInternalServerError)

	client := newTinyFishSearchTestClient(t, searchServer, fetchServer)

	results, err := client.search(context.Background(), tinyFishSearchTestConfig(), []string{"fragile query"})
	if err != nil {
		t.Fatalf("search returned error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("unexpected result count: %d", len(results))
	}

	if !strings.Contains(results[0].Text, "Snippet fragile") {
		t.Fatalf("expected snippet fallback without fetch content: %q", results[0].Text)
	}

	if strings.Contains(results[0].Text, "Content:") {
		t.Fatalf("expected no fetched content section after fetch failure: %q", results[0].Text)
	}
}

func TestTinyFishSearchReportsNoResultsPerQuery(t *testing.T) {
	t.Parallel()

	var capture tinyFishSearchFetchCapture

	searchServer, fetchServer := newTinyFishSearchFetchTestServer(t, &capture, map[string][]map[string]any{}, http.StatusOK)

	client := newTinyFishSearchTestClient(t, searchServer, fetchServer)

	results, err := client.search(context.Background(), tinyFishSearchTestConfig(), []string{"empty query"})
	if err != nil {
		t.Fatalf("search returned error: %v", err)
	}

	if len(results) != 1 || results[0].Text != "No search results found." {
		t.Fatalf("unexpected empty results: %#v", results)
	}

	if got := capture.fetchCalls.Load(); got != 0 {
		t.Fatalf("expected no fetch for empty results, got %d", got)
	}
}

func TestWebsiteFetchWithTinyFishFetchUsesCache(t *testing.T) {
	t.Parallel()

	var fetchCalls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		fetchCalls.Add(1)
		responseWriter.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(responseWriter).Encode(map[string]any{
			"results": []any{map[string]any{
				"url":       "https://example.com/article",
				"final_url": "https://example.com/article",
				"title":     "Example Article",
				"format":    "markdown",
				"text":      "# Example Article\n\nFull cached body.",
			}},
			"errors": []any{},
		}); err != nil {
			t.Errorf("encode TinyFish response: %v", err)
		}
	}))
	defer server.Close()

	client := newWebsiteTestClientWithTinyFish(server.Client(), server.URL, "", "")
	loadedConfig := tinyFishSearchTestConfig()

	first, err := client.fetchWithTinyFishFetch(
		context.Background(),
		"https://example.com/article",
		"tf-test-key",
		loadedConfig.WebSearch.TinyFish.maxCharsPerResult(),
	)
	if err != nil {
		t.Fatalf("first fetch returned error: %v", err)
	}

	second, err := client.fetchWithTinyFishFetch(
		context.Background(),
		"https://example.com/article",
		"tf-test-key",
		loadedConfig.WebSearch.TinyFish.maxCharsPerResult(),
	)
	if err != nil {
		t.Fatalf("second fetch returned error: %v", err)
	}

	if got := fetchCalls.Load(); got != 1 {
		t.Fatalf("expected cached second fetch, server calls %d", got)
	}

	if first.Content != second.Content || !strings.Contains(second.Content, "Full cached body.") {
		t.Fatalf("unexpected cached content: %#v vs %#v", first, second)
	}
}

func TestTinyFishFetchCacheDualKeyStoreRespectsCapacity(t *testing.T) {
	t.Parallel()

	cache := newTinyFishFetchCache()

	for i := range tinyFishFetchCacheMaxEntries {
		cache.store(fmt.Sprintf("https://example.com/solo-%d", i), "", "body", "", "")
	}

	if got := cache.len(); got != tinyFishFetchCacheMaxEntries {
		t.Fatalf("expected full cache, size %d", got)
	}

	// A redirect-style result inserts two distinct keys; the cap must still hold.
	cache.store("https://example.com/short", "https://example.com/final-destination", "body", "", "")

	if got := cache.len(); got > tinyFishFetchCacheMaxEntries {
		t.Fatalf("dual-key store exceeded capacity: %d > %d", got, tinyFishFetchCacheMaxEntries)
	}

	if _, _, _, _, _, ok := cache.lookup("https://example.com/short"); !ok {
		t.Fatal("expected freshly stored redirect source to be cached")
	}

	if _, _, _, _, _, ok := cache.lookup("https://example.com/final-destination"); !ok {
		t.Fatal("expected freshly stored redirect target to be cached")
	}
}

func TestWebsiteFetchWithTinyFishFetchCacheHitMatchesColdResult(t *testing.T) {
	t.Parallel()

	var fetchCalls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		fetchCalls.Add(1)
		responseWriter.Header().Set("Content-Type", "application/json")

		// Redirect-style response with no title: cold path must prefer
		// final_url for the page URL and fall back the title to the
		// request URL chain.
		if err := json.NewEncoder(responseWriter).Encode(map[string]any{
			"results": []any{map[string]any{
				"url":       "https://example.com/old",
				"final_url": "https://example.com/new",
				"title":     nil,
				"format":    "markdown",
				"text":      "# Redirected\n\nFull cached body.",
			}},
			"errors": []any{},
		}); err != nil {
			t.Errorf("encode TinyFish response: %v", err)
		}
	}))
	defer server.Close()

	client := newWebsiteTestClientWithTinyFish(server.Client(), server.URL, "", "")
	loadedConfig := tinyFishSearchTestConfig()

	first, err := client.fetchWithTinyFishFetch(
		context.Background(),
		"https://example.com/old",
		"tf-test-key",
		loadedConfig.WebSearch.TinyFish.maxCharsPerResult(),
	)
	if err != nil {
		t.Fatalf("first fetch returned error: %v", err)
	}

	second, err := client.fetchWithTinyFishFetch(
		context.Background(),
		"https://example.com/old",
		"tf-test-key",
		loadedConfig.WebSearch.TinyFish.maxCharsPerResult(),
	)
	if err != nil {
		t.Fatalf("second fetch returned error: %v", err)
	}

	if got := fetchCalls.Load(); got != 1 {
		t.Fatalf("expected cached second fetch, server calls %d", got)
	}

	if first != second {
		t.Fatalf("cache hit diverged from cold result:\ncold: %#v\nwarm: %#v", first, second)
	}

	if second.URL != "https://example.com/new" {
		t.Fatalf("expected final URL on cache hit, got %q", second.URL)
	}
}
