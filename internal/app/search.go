package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	providers "llmcord-go/internal/providers"
	searchtypes "llmcord-go/internal/searchtypes"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	searchWarningText            = "Warning: web search unavailable"
	searchSourcesSectionCapacity = 2
	searchSourcesUnavailableText = "No sources available."
	messageRoleUser              = "user"
	contentTypeAudioData         = "audio_data"
	contentTypeDocument          = "document_data"
	contentTypeFileData          = "file_data"
	contentTypeImageURL          = "image_url"
	contentTypeText              = "text"
	contentTypeVideoData         = "video_data"
	contentFieldBytes            = "data"
	contentFieldFilename         = "filename"
	contentFieldMIMEType         = "mime_type"
	mimeTypeDOCX                 = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeTypeOctetStream          = "application/octet-stream"
	mimeTypePDF                  = "application/pdf"
	mimeTypePPTX                 = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	mimeTypeJPEG                 = "image/jpeg"
	mimeTypePNG                  = "image/png"
	mimeTypeZIP                  = "application/zip"
	mimeTypeWEBP                 = "image/webp"
	// webSearchToolMaxQueries specifies the default query cap when a limit is configured.
	// In practice, web_search queries are unlimited (0).
	webSearchToolMaxQueries = 0

	// Function call outputs returned to the model for calls that cannot
	// produce search results. Every call must be answered, and the function
	// calling guide suggests returning an error string the model can act on.
	webSearchUnknownToolOutputFormat = "Error: unknown function %q. The only available function is %s."
	webSearchInvalidArgumentsOutput  = "Error: the web_search arguments are not valid JSON " +
		"with a search_queries array of strings."
	webSearchNoQueriesOutput = "Error: web_search needs at least one non-empty search query."
	webSearchFailedOutput    = "Error: the web search failed, so no search results are available."
)

type chatCompletionStreamer interface {
	StreamChatCompletion(
		ctx context.Context,
		request providers.ChatCompletionRequest,
		handle func(providers.StreamDelta) error,
	) error
}

type webSearcher interface {
	search(ctx context.Context, loadedConfig config, queries []string) ([]webSearchResult, error)
}

type searchMetadata = searchtypes.SearchMetadata
type webSearchResult = searchtypes.WebSearchResult
type searchSource = searchtypes.SearchSource
type visualSearchSourceGroup = searchtypes.VisualSearchSourceGroup

type exaSearchClient struct {
	apiEndpoint string
	httpClient  *http.Client
	keys        *apiKeyRotator
}

type tavilySearchClient struct {
	endpoint   string
	httpClient *http.Client
	keys       *apiKeyRotator
}

type parallelSearchClient struct {
	endpoint        string
	extractEndpoint string
	httpClient      *http.Client
	keys            *apiKeyRotator
}

type tinyFishSearchClient struct {
	searchEndpoint string
	fetchEndpoint  string
	httpClient     *http.Client
	keys           *apiKeyRotator
	fetchCache     *tinyFishFetchCache
	// fetchDeadline is the hard limit on fetching search result pages
	// (tinyFishSearchFetchDeadline).
	fetchDeadline time.Duration
}

type routedWebSearchClient struct {
	tinyFish webSearcher
	exa      webSearcher
	tavily   webSearcher
	parallel webSearcher
}

// flexibleInt decodes JSON numbers that may be int or float.
// TinyFish documents numeric fields as `number` and `latency_ms` as `number | null`
// and has been observed returning floats like 12534.00762900128 for latency_ms.
// Strict `int` fails with `json: cannot unmarshal number ... into Go struct field ... of type int`.
type flexibleInt int

func (fi *flexibleInt) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*fi = 0
		return nil
	}

	var i int
	if err := json.Unmarshal(trimmed, &i); err == nil {
		*fi = flexibleInt(i)
		return nil
	}

	var f float64
	if err := json.Unmarshal(trimmed, &f); err == nil {
		*fi = flexibleInt(int(f))
		return nil
	}

	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" || s == "null" {
			*fi = 0
			return nil
		}

		if parsed, err := strconv.ParseFloat(s, 64); err == nil {
			*fi = flexibleInt(int(parsed))
			return nil
		}
	}

	return fmt.Errorf("flexibleInt: cannot unmarshal %s", string(trimmed))
}

func (fi flexibleInt) MarshalJSON() ([]byte, error) {
	return json.Marshal(int(fi))
}

// flexibleFloat64 decodes JSON numbers that may be int or float, handling `number | null`.
type flexibleFloat64 float64

func (ff *flexibleFloat64) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*ff = 0
		return nil
	}

	var f float64
	if err := json.Unmarshal(trimmed, &f); err == nil {
		*ff = flexibleFloat64(f)
		return nil
	}

	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" || s == "null" {
			*ff = 0
			return nil
		}

		if parsed, err := strconv.ParseFloat(s, 64); err == nil {
			*ff = flexibleFloat64(parsed)
			return nil
		}
	}

	return fmt.Errorf("flexibleFloat64: cannot unmarshal %s", string(trimmed))
}

func (ff flexibleFloat64) MarshalJSON() ([]byte, error) {
	return json.Marshal(float64(ff))
}

type tinyFishSearchResponse struct {
	Query        string                 `json:"query"`
	Results      []tinyFishSearchResult `json:"results"`
	TotalResults flexibleInt            `json:"total_results"`
	Page         flexibleInt            `json:"page"`
}

type tinyFishQueryOutcome struct {
	query   string
	results []tinyFishSearchResult
}

type tinyFishSearchResult struct {
	Position     flexibleInt  `json:"position"`
	SiteName     string       `json:"site_name"`
	Title        string       `json:"title"`
	Snippet      string       `json:"snippet"`
	URL          string       `json:"url"`
	Date         *string      `json:"date"`
	Publisher    *string      `json:"publisher"`
	Authors      []string     `json:"authors"`
	Venue        *string      `json:"venue"`
	Year         *flexibleInt `json:"year"`
	CitedByCount *flexibleInt `json:"cited_by_count"`
	PDFURL       *string      `json:"pdf_url"`
}

// ttl is intentionally omitted: omitting it lets Fetch serve any cached
// entry, the fastest server-side path. per_url_timeout_ms bounds each URL's
// server-side fetch+extract so one slow URL cannot stall the whole batch.
type tinyFishFetchRequest struct {
	URLs            []string `json:"urls"`
	Format          string   `json:"format"`
	PerURLTimeoutMS int      `json:"per_url_timeout_ms,omitempty"`
}

type tinyFishFetchResponse struct {
	Results []tinyFishFetchResult `json:"results"`
	Errors  []tinyFishFetchError  `json:"errors"`
}

type tinyFishFetchResult struct {
	URL         string           `json:"url"`
	FinalURL    string           `json:"final_url"`
	Title       *string          `json:"title"`
	Description *string          `json:"description"`
	Language    *string          `json:"language"`
	Format      string           `json:"format"`
	Text        any              `json:"text"`
	LatencyMs   *flexibleFloat64 `json:"latency_ms"`
}

type tinyFishFetchError struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

type tinyFishStatusError struct {
	StatusCode int
	Message    string
	Err        error
}

type exaSearchRequest struct {
	Query      string
	Type       string
	NumResults int
	Contents   exaSearchRequestContents
}

type exaSearchRequestContents struct {
	Text        exaSearchTextRequest
	Highlights  bool
	MaxAgeHours int
}

type exaSearchTextRequest struct {
	MaxCharacters int
	Verbosity     string
}

type exaSearchResponse struct {
	Results []exaSearchResponseResult
	Error   string
}

type exaSearchResponseResult struct {
	ID            *string
	Title         string
	URL           string
	PublishedDate *string
	Author        *string
	Image         *string
	Favicon       *string
	Highlights    []string
	Summary       *string
	Text          *string
}

func normalizeExaSearchType(searchType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(searchType)) {
	case exaSearchTypeInstant:
		return exaSearchTypeInstant, true
	case exaSearchTypeFast:
		return exaSearchTypeFast, true
	case exaSearchTypeAuto:
		return exaSearchTypeAuto, true
	case exaSearchTypeDeepLite:
		return exaSearchTypeDeepLite, true
	case exaSearchTypeDeep:
		return exaSearchTypeDeep, true
	case exaSearchTypeDeepReasoning:
		return exaSearchTypeDeepReasoning, true
	default:
		return "", false
	}
}

func (settings exaSearchConfig) searchType() string {
	searchType, ok := normalizeExaSearchType(settings.SearchType)
	if !ok {
		return defaultExaSearchType
	}

	return searchType
}

func (instance *bot) exaSearchTypeForProvider(loadedConfig config, configuredModel string) string {
	if providerName, _, err := splitConfiguredModel(configuredModel); err == nil {
		if provider, ok := loadedConfig.Providers[providerName]; ok {
			if searchType, ok := normalizeExaSearchType(provider.ExaSearchType); ok {
				return searchType
			}
		}
	}

	return instance.currentExaSearchType()
}

// tavilySearchRequest follows https://docs.tavily.com/documentation/api-reference/endpoint/search.
// Snippets-only: include_raw_content stays unset so search never live-extracts
// result pages (Tavily search has no cache-only mode; omitting it keeps search
// to index snippets and leaves full-page reads to the extraction chain).
type tavilySearchRequest struct {
	Query       string `json:"query"`
	SearchDepth string `json:"search_depth"`
	MaxResults  int    `json:"max_results"`
}

type tavilySearchResponse struct {
	Results []tavilySearchResponseResult `json:"results"`
}

type tavilySearchResponseResult struct {
	Title      string `json:"title"`
	URL        string `json:"url"`
	Content    string `json:"content"`
	RawContent string `json:"raw_content"`
}

type tavilyStatusError struct {
	StatusCode int
	Message    string
	Err        error
}

type exaStatusError struct {
	StatusCode int
	Message    string
	Err        error
}

type parallelStatusError struct {
	StatusCode int
	Message    string
	Err        error
}

// parallelSearchRequest follows https://docs.parallel.ai/api-reference/search/search.
// Cache-first retrieval: fetch_policy stays unset so Parallel serves cached
// index content instead of live-fetching the origin.
type parallelSearchRequest struct {
	Objective        string                  `json:"objective,omitempty"`
	SearchQueries    []string                `json:"search_queries"`
	Mode             string                  `json:"mode,omitempty"`
	AdvancedSettings *parallelSearchSettings `json:"advanced_settings,omitempty"`
}

type parallelSearchSettings struct {
	MaxResults      int                      `json:"max_results,omitempty"`
	ExcerptSettings *parallelExcerptSettings `json:"excerpt_settings,omitempty"`
}

type parallelSearchResponse struct {
	SearchID string                       `json:"search_id"`
	Results  []parallelSearchResponseItem `json:"results"`
	Warnings []string                     `json:"warnings"`
}

type parallelSearchResponseItem struct {
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	PublishDate *string  `json:"publish_date"`
	Excerpts    []string `json:"excerpts"`
}

// parallelExtractRequest follows https://docs.parallel.ai/api-reference/extract/extract:
// Search returns compressed excerpts, not full page bodies — full content per
// URL comes from the Extract API with advanced_settings.full_content enabled.
// Cache-first retrieval: fetch_policy stays unset so Parallel serves cached
// index content instead of live-fetching the origin.
type parallelExtractRequest struct {
	URLs             []string                 `json:"urls"`
	Objective        string                   `json:"objective,omitempty"`
	SearchQueries    []string                 `json:"search_queries,omitempty"`
	AdvancedSettings *parallelExtractSettings `json:"advanced_settings,omitempty"`
}

type parallelExtractSettings struct {
	ExcerptSettings *parallelExcerptSettings            `json:"excerpt_settings,omitempty"`
	FullContent     *parallelExtractFullContentSettings `json:"full_content,omitempty"`
}

type parallelExcerptSettings struct {
	MaxCharsPerResult int `json:"max_chars_per_result,omitempty"`
}

type parallelExtractFullContentSettings struct {
	MaxCharsPerResult int `json:"max_chars_per_result,omitempty"`
}

type parallelExtractResponse struct {
	ExtractID string                  `json:"extract_id"`
	Results   []parallelExtractResult `json:"results"`
	Errors    []parallelExtractError  `json:"errors"`
}

type parallelExtractResult struct {
	URL         string   `json:"url"`
	Title       *string  `json:"title"`
	PublishDate *string  `json:"publish_date"`
	Excerpts    []string `json:"excerpts"`
	FullContent *string  `json:"full_content"`
}

type parallelExtractError struct {
	URL            string  `json:"url"`
	ErrorType      string  `json:"error_type"`
	HTTPStatusCode *int    `json:"http_status_code"`
	Content        *string `json:"content"`
}

func (err parallelStatusError) Error() string {
	return err.Message
}

func (err parallelStatusError) Unwrap() error {
	if err.Err == nil {
		return os.ErrInvalid
	}

	return err.Err
}

func (err tinyFishStatusError) Error() string {
	return err.Message
}

func (err tinyFishStatusError) Unwrap() error {
	if err.Err == nil {
		return os.ErrInvalid
	}

	return err.Err
}

func newExaSearchClient(httpClient *http.Client) exaSearchClient {
	return exaSearchClient{
		apiEndpoint: defaultExaSearchEndpoint,
		httpClient:  httpClient,
		keys:        newAPIKeyRotator(),
	}
}

func newTavilySearchClient(httpClient *http.Client) tavilySearchClient {
	return tavilySearchClient{
		endpoint:   defaultTavilySearchEndpoint,
		httpClient: httpClient,
		keys:       newAPIKeyRotator(),
	}
}

func newParallelSearchClient(httpClient *http.Client) parallelSearchClient {
	return parallelSearchClient{
		endpoint:        defaultParallelSearchEndpoint,
		extractEndpoint: defaultParallelExtractEndpoint,
		httpClient:      httpClient,
		keys:            newAPIKeyRotator(),
	}
}

func newTinyFishSearchClient(httpClient *http.Client, fetchCache ...*tinyFishFetchCache) tinyFishSearchClient {
	cache := newTinyFishFetchCache()
	if len(fetchCache) > 0 && fetchCache[0] != nil {
		cache = fetchCache[0]
	}

	return tinyFishSearchClient{
		searchEndpoint: defaultTinyFishSearchEndpoint,
		fetchEndpoint:  defaultTinyFishFetchEndpoint,
		httpClient:     httpClient,
		keys:           newAPIKeyRotator(),
		fetchCache:     cache,
		fetchDeadline:  tinyFishSearchFetchDeadline,
	}
}

func newWebSearchClient(httpClient *http.Client, fetchCache ...*tinyFishFetchCache) routedWebSearchClient {
	return routedWebSearchClient{
		tinyFish: newTinyFishSearchClient(httpClient, fetchCache...),
		exa:      newExaSearchClient(httpClient),
		tavily:   newTavilySearchClient(httpClient),
		parallel: newParallelSearchClient(httpClient),
	}
}

func (err tavilyStatusError) Error() string {
	return err.Message
}

func (err exaStatusError) Error() string {
	return err.Message
}

func (err tavilyStatusError) Unwrap() error {
	if err.Err == nil {
		return os.ErrInvalid
	}

	return err.Err
}

func (err exaStatusError) Unwrap() error {
	if err.Err == nil {
		return os.ErrInvalid
	}

	return err.Err
}

// webSearchToolCall is one function call of a tool round: its web_search
// queries, or the error output that answers a call that cannot run.
type webSearchToolCall struct {
	call        providers.FunctionToolCall
	queries     []string
	errorOutput string
}

// parseWebSearchToolCalls parses every call of a tool round and returns the
// calls together with the union of their queries, trimmed and deduplicated
// in order. Calls to other functions, malformed arguments, and calls without
// queries get an error output instead of queries.
func parseWebSearchToolCalls(toolCalls []providers.FunctionToolCall) ([]webSearchToolCall, []string) {
	parsedCalls := make([]webSearchToolCall, 0, len(toolCalls))
	seenQueries := make(map[string]struct{})

	var queries []string

	for _, toolCall := range toolCalls {
		parsedCall := webSearchToolCall{call: toolCall, queries: nil, errorOutput: ""}

		callQueries, errorOutput := webSearchToolCallQueries(toolCall)
		if errorOutput != "" {
			parsedCall.errorOutput = errorOutput
			parsedCalls = append(parsedCalls, parsedCall)

			continue
		}

		parsedCall.queries = callQueries
		parsedCalls = append(parsedCalls, parsedCall)

		for _, query := range callQueries {
			if _, seen := seenQueries[query]; seen {
				continue
			}

			seenQueries[query] = struct{}{}
			queries = append(queries, query)
		}
	}

	return parsedCalls, queries
}

// webSearchToolCallQueries parses the search queries of one web_search call.
// It accepts both "search_queries" (the Parallel Search schema) and
// "queries" (legacy schema) for backward compatibility. It returns the
// trimmed, deduplicated queries, or the error output for a call that cannot
// run.
func webSearchToolCallQueries(toolCall providers.FunctionToolCall) ([]string, string) {
	name := strings.TrimSpace(toolCall.Name)
	if name != providers.WebSearchToolName {
		return nil, fmt.Sprintf(webSearchUnknownToolOutputFormat, name, providers.WebSearchToolName)
	}

	var arguments struct {
		Objective     string   `json:"objective"`
		SearchQueries []string `json:"search_queries"`
		Queries       []string `json:"queries"`
	}

	err := json.Unmarshal([]byte(toolCall.Arguments), &arguments)
	if err != nil {
		logWarn("parse web search tool arguments", err, "arguments", toolCall.Arguments)

		return nil, webSearchInvalidArgumentsOutput
	}

	rawQueries := arguments.SearchQueries
	if len(rawQueries) == 0 {
		rawQueries = arguments.Queries
	}

	seenQueries := make(map[string]struct{}, len(rawQueries))
	queries := make([]string, 0, len(rawQueries))

	for _, query := range rawQueries {
		query = strings.TrimSpace(query)
		if query == "" {
			continue
		}

		if _, seen := seenQueries[query]; seen {
			continue
		}

		seenQueries[query] = struct{}{}
		queries = append(queries, query)
	}

	if len(queries) == 0 {
		return nil, webSearchNoQueriesOutput
	}

	return queries, ""
}

// extractWebSearchQueries returns the union of the search queries requested
// through the web_search tool calls, trimmed and deduplicated in order.
// Calls to other tools and calls with malformed arguments are ignored.
func extractWebSearchQueries(toolCalls []providers.FunctionToolCall) []string {
	_, queries := parseWebSearchToolCalls(toolCalls)

	return queries
}

// webSearchToolOutputs answers every call of a tool round, in call order.
// A web_search call gets the results for its own queries; results matching
// no call's query go to the first call that ran, so none are lost. Calls
// that could not run keep their error output, and searchErrorOutput (when
// set) answers every call that needed the failed search.
func webSearchToolOutputs(
	parsedCalls []webSearchToolCall,
	results []webSearchResult,
	searchErrorOutput string,
) []providers.FunctionToolOutput {
	outputs := make([]providers.FunctionToolOutput, 0, len(parsedCalls))
	unmatchedResults := webSearchResultsMatchingNoCall(parsedCalls, results)

	for _, parsedCall := range parsedCalls {
		output := parsedCall.errorOutput

		switch {
		case output != "":
		case searchErrorOutput != "":
			output = searchErrorOutput
		default:
			callResults := webSearchResultsForQueries(results, parsedCall.queries)
			callResults = append(callResults, unmatchedResults...)
			unmatchedResults = nil
			output = formatWebSearchResults(callResults)
		}

		outputs = append(outputs, providers.FunctionToolOutput{
			CallID: parsedCall.call.ID,
			Output: output,
		})
	}

	return outputs
}

// webSearchResultsForQueries returns the results for each query in query
// order; a query without results reports that nothing was found.
func webSearchResultsForQueries(results []webSearchResult, queries []string) []webSearchResult {
	queryResults := make([]webSearchResult, 0, len(queries))

	for _, query := range queries {
		found := false

		for _, result := range results {
			if strings.EqualFold(strings.TrimSpace(result.Query), query) {
				queryResults = append(queryResults, result)
				found = true
			}
		}

		if !found {
			queryResults = append(queryResults, webSearchResult{Query: query, Text: ""})
		}
	}

	return queryResults
}

// webSearchResultsMatchingNoCall returns the results whose query was not
// requested by any web_search call of the round.
func webSearchResultsMatchingNoCall(parsedCalls []webSearchToolCall, results []webSearchResult) []webSearchResult {
	requestedQueries := make(map[string]struct{})

	for _, parsedCall := range parsedCalls {
		for _, query := range parsedCall.queries {
			requestedQueries[strings.ToLower(query)] = struct{}{}
		}
	}

	var unmatched []webSearchResult

	for _, result := range results {
		if _, requested := requestedQueries[strings.ToLower(strings.TrimSpace(result.Query))]; !requested {
			unmatched = append(unmatched, result)
		}
	}

	return unmatched
}

// webSearchToolEnabled reports whether the provider's models should be
// offered the web_search tool: explicit opt-out, non-Gemini API, no native
// grounding, and at least one configured search provider.
func (instance *bot) webSearchToolEnabled(
	loadedConfig config,
	provider providerConfig,
) bool {
	return !provider.DisableWebSearch &&
		provider.apiKind() == providerAPIKindOpenAI &&
		!instance.currentGroundingEnabled(provider) &&
		loadedConfig.WebSearch.hasWebSearchAPIKeys()
}

// runWebSearchQueries executes the routed TinyFish -> Exa -> Tavily search
// for the given queries. A provider-level exa_search_type wins over the
// runtime /searchtype value; /searchtype remains the fallback.
func (instance *bot) runWebSearchQueries(
	ctx context.Context,
	loadedConfig config,
	configuredModel string,
	queries []string,
) ([]webSearchResult, error) {
	searchConfig := loadedConfig
	searchConfig.WebSearch.Exa.SearchType = instance.exaSearchTypeForProvider(loadedConfig, configuredModel)

	return instance.webSearch.search(ctx, searchConfig, queries)
}

// runWebSearchToolPhase executes the function calls of one tool round and
// returns one output per call, in call order: the function calling guides
// require every call the model requested to be answered. The queries of
// all web_search calls run through the routed search chain as one batch,
// and each call's output holds the results for its own queries. Calls to
// unknown functions, calls without usable queries, and failed searches are
// answered with an error the model can act on. Search results are also
// retained in the source message's history for later turns. It reports
// whether usable results were produced.
func (instance *bot) runWebSearchToolPhase(
	ctx context.Context,
	loadedConfig config,
	configuredModel string,
	tracker *responseTracker,
	requestMessages []chatMessage,
	warnings []string,
	toolCalls []providers.FunctionToolCall,
) ([]providers.FunctionToolOutput, []string, bool) {
	parsedCalls, queries := parseWebSearchToolCalls(toolCalls)
	if len(queries) == 0 {
		logWarn(
			"web_search tool called without usable queries",
			nil,
			"tool_calls",
			len(toolCalls),
		)

		return webSearchToolOutputs(parsedCalls, nil, ""), warnings, false
	}

	if tracker != nil {
		tracker.progress.showSearchStarted(queries)
	}

	results, err := instance.runWebSearchQueries(ctx, loadedConfig, configuredModel, queries)
	if err != nil {
		logWarn("run web search", err, "queries", queries)

		if tracker != nil {
			tracker.progress.showSearchFinished(queries, 0, true)
		}

		return webSearchToolOutputs(parsedCalls, nil, webSearchFailedOutput),
			append(warnings, searchWarningText),
			false
	}

	if tracker != nil {
		tracker.progress.showSearchFinished(
			queries,
			countWebSearchResultSources(results, loadedConfig.WebSearch.maxURLs()),
			false,
		)

		tracker.searchMetadata = mergeSearchMetadata(
			tracker.searchMetadata,
			newSearchMetadata(queries, results, loadedConfig.WebSearch.maxURLs()),
		)
		tracker.toolSearchResults = append(tracker.toolSearchResults, results...)

		instance.retainWebSearchResults(ctx, tracker, requestMessages)
	}

	return webSearchToolOutputs(parsedCalls, results, ""), warnings, true
}

// retainWebSearchResults stores the attempt's accumulated web_search tool
// results in the source message's history node, so later turns rebuilt from
// the reply chain keep the searched context. The current request is left
// unchanged: its results travel as tool outputs.
func (instance *bot) retainWebSearchResults(
	ctx context.Context,
	tracker *responseTracker,
	requestMessages []chatMessage,
) {
	if tracker.sourceMessage == nil {
		return
	}

	augmentedMessages, err := appendWebSearchResultsToConversation(
		requestMessages,
		formatWebSearchResults(tracker.toolSearchResults),
	)
	if err != nil {
		logWarn("append web search results to retained history", err)

		return
	}

	err = instance.persistAugmentedSourceMessage(ctx, tracker.sourceMessage, augmentedMessages)
	if err != nil {
		logWarn("persist augmented source message after web search", err)
	}
}

func newSearchMetadata(queries []string, results []webSearchResult, maxURLs int) *searchMetadata {
	return &searchMetadata{
		Queries:             append([]string(nil), queries...),
		Results:             append([]webSearchResult(nil), results...),
		MaxURLs:             maxURLs,
		VisualSearchSources: nil,
	}
}

func newVisualSearchMetadata(results []visualSearchResult) *searchMetadata {
	if len(results) == 0 {
		return nil
	}

	metadata := new(searchMetadata)
	metadata.VisualSearchSources = make([]visualSearchSourceGroup, 0, len(results))

	for _, result := range results {
		metadata.VisualSearchSources = append(metadata.VisualSearchSources, visualSearchSourceGroup{
			Label:   visualSearchResultSectionLabel(result, results),
			Sources: extractVisualSearchSources(result),
		})
	}

	return metadata
}

func cloneSearchMetadata(metadata *searchMetadata) *searchMetadata {
	if metadata == nil {
		return nil
	}

	cloned := newSearchMetadata(metadata.Queries, metadata.Results, metadata.MaxURLs)
	cloned.VisualSearchSources = cloneVisualSearchSourceGroups(metadata.VisualSearchSources)

	return cloned
}

func cloneVisualSearchSourceGroups(
	sourceGroups []visualSearchSourceGroup,
) []visualSearchSourceGroup {
	if len(sourceGroups) == 0 {
		return nil
	}

	clonedGroups := make([]visualSearchSourceGroup, 0, len(sourceGroups))

	for _, sourceGroup := range sourceGroups {
		clonedGroups = append(clonedGroups, visualSearchSourceGroup{
			Label:   sourceGroup.Label,
			Sources: append([]searchSource(nil), sourceGroup.Sources...),
		})
	}

	return clonedGroups
}

func mergeSearchMetadata(left, right *searchMetadata) *searchMetadata {
	switch {
	case left == nil:
		return cloneSearchMetadata(right)
	case right == nil:
		return cloneSearchMetadata(left)
	}

	merged := cloneSearchMetadata(left)
	merged.Queries = append(merged.Queries, right.Queries...)
	merged.Results = append(merged.Results, right.Results...)
	merged.VisualSearchSources = append(
		merged.VisualSearchSources,
		cloneVisualSearchSourceGroups(right.VisualSearchSources)...,
	)

	if right.MaxURLs > 0 {
		merged.MaxURLs = right.MaxURLs
	}

	return merged
}

type searchAttemptError struct {
	providerName string
	err          error
}

// search implements the configurable web search fallback chain specified in
// loadedConfig.WebSearch.Order (default: TinyFish -> Exa -> Tavily).
// Providers without configured API keys are skipped.
func (client routedWebSearchClient) search(
	ctx context.Context,
	loadedConfig config,
	queries []string,
) ([]webSearchResult, error) {
	order := loadedConfig.WebSearch.Order
	if len(order) == 0 {
		order = defaultWebSearchOrder
	}

	var failedAttempts []searchAttemptError

	for _, provider := range order {
		switch provider {
		case webSearchProviderTinyFish:
			if len(loadedConfig.WebSearch.TinyFish.apiKeys()) == 0 {
				continue
			}

			if client.tinyFish == nil {
				continue
			}

			results, err := client.tinyFish.search(ctx, loadedConfig, queries)
			if err == nil {
				return results, nil
			}

			logWarn("tinyfish search failed, trying fallback", err)
			failedAttempts = append(failedAttempts, searchAttemptError{
				providerName: "TinyFish Search",
				err:          err,
			})

		case webSearchProviderExa:
			if len(loadedConfig.WebSearch.Exa.apiKeys()) == 0 {
				continue
			}

			if client.exa == nil {
				continue
			}

			results, err := client.exa.search(ctx, loadedConfig, queries)
			if err == nil {
				return results, nil
			}

			failedAttempts = append(failedAttempts, searchAttemptError{
				providerName: "Exa Search API",
				err:          err,
			})

		case webSearchProviderTavily:
			if client.tavily == nil {
				continue
			}

			results, err := client.tavily.search(ctx, loadedConfig, queries)
			if err == nil {
				return results, nil
			}

			failedAttempts = append(failedAttempts, searchAttemptError{
				providerName: "Tavily",
				err:          err,
			})

		case webSearchProviderParallel:
			if client.parallel == nil {
				continue
			}

			results, err := client.parallel.search(ctx, loadedConfig, queries)
			if err == nil {
				return results, nil
			}

			failedAttempts = append(failedAttempts, searchAttemptError{
				providerName: "Parallel Search",
				err:          err,
			})
		}
	}

	if len(failedAttempts) == 0 {
		return nil, fmt.Errorf("no web search providers configured: %w", os.ErrNotExist)
	}

	var joinedErrs []error
	for _, attempt := range failedAttempts {
		joinedErrs = append(joinedErrs, attempt.err)
	}

	joined := errors.Join(joinedErrs...)

	switch len(failedAttempts) {
	case 1:
		return nil, fmt.Errorf(
			"search with %s failed: %w",
			failedAttempts[0].providerName,
			joined,
		)
	case 2:
		return nil, fmt.Errorf(
			"search with %s failed, and %s fallback failed: %w",
			failedAttempts[0].providerName,
			failedAttempts[1].providerName,
			joined,
		)
	case 3:
		return nil, fmt.Errorf(
			"search with %s failed, %s primary fallback failed, and %s fallback failed: %w",
			failedAttempts[0].providerName,
			failedAttempts[1].providerName,
			failedAttempts[2].providerName,
			joined,
		)
	default:
		var parts []string

		for index, attempt := range failedAttempts {
			if index == 0 {
				parts = append(parts, fmt.Sprintf("search with %s failed", attempt.providerName))
			} else if index == len(failedAttempts)-1 {
				parts = append(parts, fmt.Sprintf("and %s fallback failed", attempt.providerName))
			} else {
				parts = append(parts, fmt.Sprintf("%s fallback failed", attempt.providerName))
			}
		}

		return nil, fmt.Errorf("%s: %w", strings.Join(parts, ", "), joined)
	}
}

func contentPartImageURL(part contentPart) (string, error) {
	stringMap, foundStringMap := part["image_url"].(map[string]string)
	if foundStringMap {
		return strings.TrimSpace(stringMap["url"]), nil
	}

	rawImageURL, foundMap := part["image_url"].(map[string]any)
	if !foundMap {
		return "", fmt.Errorf("decode image_url content part: %w", os.ErrInvalid)
	}

	imageURL, _ := rawImageURL["url"].(string)

	return strings.TrimSpace(imageURL), nil
}

func contentPartsText(parts []contentPart) string {
	textParts := make([]string, 0, len(parts))

	for _, part := range parts {
		partType, _ := part["type"].(string)
		if partType != contentTypeText {
			continue
		}

		textValue, _ := part["text"].(string)
		if strings.TrimSpace(textValue) == "" {
			continue
		}

		textParts = append(textParts, textValue)
	}

	return joinNonEmpty(textParts)
}

func formatWebSearchResults(results []webSearchResult) string {
	formattedResults := make([]string, 0, len(results))

	for _, result := range results {
		resultText := strings.TrimSpace(result.Text)
		if resultText == "" {
			resultText = "No search results found."
		}

		formattedResults = append(
			formattedResults,
			fmt.Sprintf("Query: %s\nResults:\n%s", result.Query, resultText),
		)
	}

	return strings.Join(formattedResults, "\n\n")
}

func collectChatCompletionText(
	ctx context.Context,
	client chatCompletionStreamer,
	request chatCompletionRequest,
) (string, error) {
	var responseText strings.Builder

	err := client.StreamChatCompletion(ctx, request, func(delta streamDelta) error {
		responseText.WriteString(delta.Content)

		return nil
	})
	if err != nil {
		return "", err
	}

	return responseText.String(), nil
}

func formatSearchSourcesMessage(metadata *searchMetadata) string {
	if metadata == nil {
		return searchSourcesUnavailableText
	}

	sections := make([]string, 0, searchSourcesSectionCapacity)

	if webSources := formatWebSearchSourcesMessage(metadata); webSources != "" {
		sections = append(sections, webSources)
	}

	if visualSources := formatVisualSearchSourcesMessage(metadata.VisualSearchSources); visualSources != "" {
		sections = append(sections, visualSources)
	}

	if len(sections) == 0 {
		return searchSourcesUnavailableText
	}

	return strings.Join(sections, "\n\n")
}

func formatWebSearchSourcesMessage(metadata *searchMetadata) string {
	if metadata == nil || (len(metadata.Queries) == 0 && len(metadata.Results) == 0) {
		return ""
	}

	var builder strings.Builder

	sourceNumber := 1

	if len(metadata.Queries) > 0 {
		builder.WriteString("Search queries:\n")

		for index, query := range metadata.Queries {
			_, _ = fmt.Fprintf(&builder, numberedListLineFormat, index+1, query)
		}
	}

	for _, result := range metadata.Results {
		builder.WriteString("\n")

		if strings.TrimSpace(result.Query) == "" {
			builder.WriteString("Sources:\n")
		} else {
			_, _ = fmt.Fprintf(&builder, "Sources for %q:\n", result.Query)
		}

		sources := extractSearchSources(result.Text)
		if len(sources) == 0 {
			builder.WriteString("No source URLs were parsed from the search response.\n")

			continue
		}

		for _, source := range sources[:minInt(len(sources), metadata.MaxURLsOrDefault(defaultWebSearchMaxURLs))] {
			_, _ = fmt.Fprintf(&builder, numberedListLineFormat, sourceNumber, formatSearchSourceLine(source))
			sourceNumber++
		}
	}

	return strings.TrimSpace(builder.String())
}

func formatVisualSearchSourcesMessage(sourceGroups []visualSearchSourceGroup) string {
	if len(sourceGroups) == 0 {
		return ""
	}

	var builder strings.Builder

	builder.WriteString("Visual search result URLs:\n")

	for groupIndex, sourceGroup := range sourceGroups {
		if label := strings.TrimSpace(sourceGroup.Label); label != "" {
			if groupIndex > 0 {
				builder.WriteString("\n")
			}

			builder.WriteString(label)
			builder.WriteString(":\n")
		} else if len(sourceGroups) > 1 {
			builder.WriteString("\n")
			_, _ = fmt.Fprintf(&builder, "Image %d:\n", groupIndex+1)
		}

		if len(sourceGroup.Sources) == 0 {
			builder.WriteString("No source URLs were found in the visual search results.\n")

			continue
		}

		for sourceIndex, source := range sourceGroup.Sources {
			_, _ = fmt.Fprintf(&builder, numberedListLineFormat, sourceIndex+1, formatSearchSourceLine(source))
		}
	}

	return strings.TrimSpace(builder.String())
}

func formatSearchSourcesPages(metadata *searchMetadata) []string {
	message := strings.TrimSpace(formatSearchSourcesMessage(metadata))
	if message == "" {
		return []string{searchSourcesUnavailableText}
	}

	return splitMessagePages(message, showSourcesPageBodyMaxLength)
}

func countSearchSources(metadata *searchMetadata) int {
	if metadata == nil {
		return 0
	}

	totalSources := 0

	for _, result := range metadata.Results {
		totalSources += minInt(len(extractSearchSources(result.Text)), metadata.MaxURLsOrDefault(defaultWebSearchMaxURLs))
	}

	for _, sourceGroup := range metadata.VisualSearchSources {
		totalSources += len(sourceGroup.Sources)
	}

	return totalSources
}

func splitMessagePages(text string, limit int) []string {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" || limit <= 0 {
		return nil
	}

	if runeCount(trimmedText) <= limit {
		return []string{trimmedText}
	}

	pages := make([]string, 0, (runeCount(trimmedText)+limit-1)/limit)
	remaining := trimmedText

	for remaining != "" {
		if runeCount(remaining) <= limit {
			pages = append(pages, remaining)

			break
		}

		prefix, suffix := splitRunesPrefix(remaining, limit)

		splitIndex := strings.LastIndex(prefix, "\n\n")
		separatorLength := len("\n\n")

		if splitIndex < 0 {
			splitIndex = strings.LastIndex(prefix, "\n")
			separatorLength = len("\n")
		}

		page := prefix
		remaining = suffix

		if splitIndex > 0 {
			page = prefix[:splitIndex]
			remaining = prefix[splitIndex+separatorLength:] + suffix
		}

		page = strings.TrimSpace(page)
		remaining = strings.TrimLeft(remaining, "\n")

		if page == "" {
			page, remaining = splitRunesPrefix(remaining, limit)
			page = strings.TrimSpace(page)
			remaining = strings.TrimLeft(remaining, "\n")
		}

		if page == "" {
			break
		}

		pages = append(pages, page)
	}

	if len(pages) == 0 {
		return []string{trimmedText}
	}

	return pages
}

func formatSearchSourcesPageContent(pages []string, pageIndex int, totalSources int) string {
	if len(pages) == 0 {
		return searchSourcesUnavailableText
	}

	if pageIndex < 0 {
		pageIndex = 0
	} else if pageIndex >= len(pages) {
		pageIndex = len(pages) - 1
	}

	if totalSources < 0 {
		totalSources = 0
	}

	pageHeader := fmt.Sprintf("Sources (%d total)", totalSources)

	if len(pages) == 1 {
		return fmt.Sprintf("%s\n\n%s", pageHeader, pages[pageIndex])
	}

	return fmt.Sprintf(
		"Sources (%d total, page %d/%d)\n\n%s",
		totalSources,
		pageIndex+1,
		len(pages),
		pages[pageIndex],
	)
}

// countWebSearchResultSources counts the distinct source URLs of search
// results, at most maxURLs per result as in Show Sources: a page found by
// several queries counts once.
func countWebSearchResultSources(results []webSearchResult, maxURLs int) int {
	seenURLs := make(map[string]struct{})

	for _, result := range results {
		sources := extractSearchSources(result.Text)
		if maxURLs > 0 && len(sources) > maxURLs {
			sources = sources[:maxURLs]
		}

		for _, source := range sources {
			seenURLs[strings.ToLower(strings.TrimSpace(source.URL))] = struct{}{}
		}
	}

	return len(seenURLs)
}

func formatSearchSourceLine(source searchSource) string {
	if strings.EqualFold(strings.TrimSpace(source.Title), strings.TrimSpace(source.URL)) {
		return "<" + source.URL + ">"
	}

	return source.Title + " <" + source.URL + ">"
}

func extractSearchSources(resultText string) []searchSource {
	lines := strings.Split(resultText, "\n")
	sources := make([]searchSource, 0)
	seenURLs := make(map[string]struct{})

	currentTitle := ""

	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmedLine, "Title:"):
			currentTitle = strings.TrimSpace(strings.TrimPrefix(trimmedLine, "Title:"))
		case strings.HasPrefix(trimmedLine, "URL:"):
			url := strings.TrimSpace(strings.TrimPrefix(trimmedLine, "URL:"))
			if url == "" {
				continue
			}

			foldedURL := strings.ToLower(url)
			if _, ok := seenURLs[foldedURL]; ok {
				currentTitle = ""

				continue
			}

			seenURLs[foldedURL] = struct{}{}

			title := currentTitle
			if title == "" {
				title = url
			}

			sources = append(sources, searchSource{
				Title: title,
				URL:   url,
			})
			currentTitle = ""
		}
	}

	return sources
}

func (client exaSearchClient) search(
	ctx context.Context,
	loadedConfig config,
	queries []string,
) ([]webSearchResult, error) {
	exaAPIKeys := loadedConfig.WebSearch.Exa.apiKeys()
	if len(exaAPIKeys) == 0 {
		return nil, fmt.Errorf("exa search is not configured: %w", os.ErrNotExist)
	}

	maxURLs := loadedConfig.WebSearch.maxURLs()
	searchType := loadedConfig.WebSearch.Exa.searchType()

	return searchQueriesConcurrently(ctx, queries, func(
		queryContext context.Context,
		query string,
	) (webSearchResult, error) {
		return tryAllAPIKeys(queryContext, client.keys, exaAPIKeys, func(apiKey string) (webSearchResult, error) {
			return client.searchAPIQuery(
				queryContext,
				apiKey,
				query,
				maxURLs,
				searchType,
				loadedConfig.WebSearch.Exa.textMaxCharacters(),
			)
		})
	})
}

func (client tavilySearchClient) search(
	ctx context.Context,
	loadedConfig config,
	queries []string,
) ([]webSearchResult, error) {
	tavilyAPIKeys := loadedConfig.WebSearch.Tavily.apiKeys()
	if len(tavilyAPIKeys) == 0 {
		return nil, fmt.Errorf("tavily fallback is not configured: %w", os.ErrNotExist)
	}

	maxURLs := loadedConfig.WebSearch.maxURLs()
	maxChars := loadedConfig.WebSearch.Tavily.maxCharsPerResult()

	return searchQueriesConcurrently(ctx, queries, func(
		queryContext context.Context,
		query string,
	) (webSearchResult, error) {
		return tryAllAPIKeys(queryContext, client.keys, tavilyAPIKeys, func(apiKey string) (webSearchResult, error) {
			return client.searchQuery(queryContext, apiKey, query, maxURLs, maxChars)
		})
	})
}

func searchQueriesConcurrently[T any](
	ctx context.Context,
	queries []string,
	searchQuery func(context.Context, string) (T, error),
) ([]T, error) {
	taskContext, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		firstErr     error
		firstErrOnce sync.Once
	)

	taskResults := runTasksConcurrently(
		taskContext,
		externalRequestConcurrency,
		len(queries),
		func(queryContext context.Context, index int) (T, error) {
			result, err := searchQuery(queryContext, queries[index])
			if err != nil {
				firstErrOnce.Do(func() {
					firstErr = err

					cancel()
				})
			}

			return result, err
		},
	)

	if firstErr != nil {
		return nil, firstErr
	}

	results := make([]T, len(taskResults))
	for index, result := range taskResults {
		if result.err != nil {
			return nil, result.err
		}

		results[index] = result.value
	}

	return results, nil
}

func (client exaSearchClient) searchAPIQuery(
	ctx context.Context,
	apiKey string,
	query string,
	maxURLs int,
	searchType string,
	textMaxCharacters int,
) (webSearchResult, error) {
	return client.searchAPIQueryOnce(
		ctx,
		query,
		apiKey,
		maxURLs,
		searchType,
		textMaxCharacters,
	)
}

func marshalExaSearchRequest(requestBody exaSearchRequest) ([]byte, error) {
	contentsMap := map[string]any{
		"highlights": requestBody.Contents.Highlights,
		// Cache-only retrieval: serve any cached copy regardless of age and
		// never livecrawl the origin. livecrawlTimeout stays omitted for the
		// same reason: no live crawl means no live-crawl budget to bound.
		"maxAgeHours": exaSearchNeverLivecrawlMaxAgeHours,
	}

	if requestBody.Contents.Text.MaxCharacters > 0 {
		textMap := map[string]any{
			"maxCharacters": requestBody.Contents.Text.MaxCharacters,
		}
		if requestBody.Contents.Text.Verbosity != "" {
			textMap["verbosity"] = requestBody.Contents.Text.Verbosity
		}

		contentsMap[messageTextKey] = textMap
	}

	requestBytes, err := json.Marshal(map[string]any{
		"query":              requestBody.Query,
		searchTypeOptionName: requestBody.Type,
		"numResults":         requestBody.NumResults,
		"contents":           contentsMap,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal Exa search request payload: %w", err)
	}

	return requestBytes, nil
}

func (client exaSearchClient) searchAPIQueryOnce(
	ctx context.Context,
	query string,
	apiKey string,
	maxURLs int,
	searchType string,
	textMaxCharacters int,
) (webSearchResult, error) {
	requestBody := exaSearchRequest{
		Query: query,
		Type:  searchType,
		Contents: exaSearchRequestContents{
			Text: exaSearchTextRequest{
				MaxCharacters: textMaxCharacters,
				Verbosity:     "full",
			},
			Highlights:  true,
			MaxAgeHours: exaSearchNeverLivecrawlMaxAgeHours,
		},
		NumResults: maxURLs,
	}

	requestBytes, err := marshalExaSearchRequest(requestBody)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("marshal Exa search request for %q: %w", query, err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.apiEndpoint,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("create Exa search request for %q: %w", query, err)
	}

	httpRequest.Header.Set("Accept", applicationJSONContentType)
	httpRequest.Header.Set(contentTypeHeader, applicationJSONContentType)
	httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	httpRequest.Header.Set("X-Api-Key", strings.TrimSpace(apiKey))

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("send Exa search request for %q: %w", query, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return webSearchResult{}, fmt.Errorf(
				"read Exa error response for %q after status %d: %w",
				query,
				httpResponse.StatusCode,
				readErr,
			)
		}

		return webSearchResult{}, exaStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"Exa search request failed for %q with status %d: %s",
				query,
				httpResponse.StatusCode,
				strings.TrimSpace(extractExaErrorMessage(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	var rawResponse map[string]any

	err = json.NewDecoder(httpResponse.Body).Decode(&rawResponse)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("decode Exa search response for %q: %w", query, err)
	}

	response, err := parseExaSearchResponse(rawResponse)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("parse Exa search response for %q: %w", query, err)
	}

	return webSearchResult{
		Query: query,
		Text:  formatExaSearchResultText(response.Results),
	}, nil
}

func (client tavilySearchClient) searchQuery(
	ctx context.Context,
	apiKey string,
	query string,
	maxURLs int,
	maxCharsPerResult int,
) (webSearchResult, error) {
	return client.searchQueryOnce(ctx, query, apiKey, maxURLs, maxCharsPerResult)
}

func (client tavilySearchClient) searchQueryOnce(
	ctx context.Context,
	query string,
	apiKey string,
	maxURLs int,
	maxCharsPerResult int,
) (webSearchResult, error) {
	requestBody := tavilySearchRequest{
		Query:       query,
		SearchDepth: "advanced",
		MaxResults:  maxURLs,
	}

	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("marshal Tavily search request for %q: %w", query, err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.endpoint,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("create Tavily search request for %q: %w", query, err)
	}

	httpRequest.Header.Set("Accept", applicationJSONContentType)
	httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	httpRequest.Header.Set(contentTypeHeader, applicationJSONContentType)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("send Tavily search request for %q: %w", query, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return webSearchResult{}, fmt.Errorf(
				"read Tavily error response for %q after status %d: %w",
				query,
				httpResponse.StatusCode,
				readErr,
			)
		}

		return webSearchResult{}, tavilyStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"Tavily search request failed for %q with status %d: %s",
				query,
				httpResponse.StatusCode,
				strings.TrimSpace(string(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	var response tavilySearchResponse

	err = json.NewDecoder(httpResponse.Body).Decode(&response)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("decode Tavily search response for %q: %w", query, err)
	}

	return webSearchResult{
		Query: query,
		Text:  formatTavilySearchResultText(response.Results, maxCharsPerResult),
	}, nil
}

func (client parallelSearchClient) search(
	ctx context.Context,
	loadedConfig config,
	queries []string,
) ([]webSearchResult, error) {
	parallelAPIKeys := loadedConfig.WebSearch.Parallel.apiKeys()
	if len(parallelAPIKeys) == 0 {
		return nil, fmt.Errorf("parallel search is not configured: %w", os.ErrNotExist)
	}

	maxURLs := loadedConfig.WebSearch.maxURLs()
	maxChars := loadedConfig.WebSearch.Parallel.maxCharsPerResult()

	return searchQueriesConcurrently(ctx, queries, func(
		queryContext context.Context,
		query string,
	) (webSearchResult, error) {
		return tryAllAPIKeys(queryContext, client.keys, parallelAPIKeys, func(apiKey string) (webSearchResult, error) {
			return client.searchQuery(queryContext, apiKey, query, maxURLs, maxChars)
		})
	})
}

func (client parallelSearchClient) searchQuery(
	ctx context.Context,
	apiKey string,
	query string,
	maxURLs int,
	maxCharsPerResult int,
) (webSearchResult, error) {
	return client.searchQueryOnce(ctx, query, apiKey, maxURLs, maxCharsPerResult)
}

func (client parallelSearchClient) searchQueryOnce(
	ctx context.Context,
	query string,
	apiKey string,
	maxURLs int,
	maxCharsPerResult int,
) (webSearchResult, error) {
	searchCtx, cancel := context.WithTimeout(ctx, parallelSearchRequestTimeout)
	defer cancel()

	requestBody := parallelSearchRequest{
		Objective:     query,
		SearchQueries: []string{query},
		Mode:          "fast",
		AdvancedSettings: &parallelSearchSettings{
			MaxResults: maxURLs,
			ExcerptSettings: &parallelExcerptSettings{
				MaxCharsPerResult: maxCharsPerResult,
			},
		},
	}

	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("marshal Parallel search request for %q: %w", query, err)
	}

	httpRequest, err := http.NewRequestWithContext(
		searchCtx,
		http.MethodPost,
		client.endpoint,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("create Parallel search request for %q: %w", query, err)
	}

	httpRequest.Header.Set("Accept", applicationJSONContentType)
	httpRequest.Header.Set("X-Api-Key", strings.TrimSpace(apiKey))
	httpRequest.Header.Set(contentTypeHeader, applicationJSONContentType)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("send Parallel search request for %q: %w", query, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return webSearchResult{}, fmt.Errorf(
				"read Parallel error response for %q after status %d: %w",
				query,
				httpResponse.StatusCode,
				readErr,
			)
		}

		return webSearchResult{}, parallelStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"Parallel search request failed for %q with status %d: %s",
				query,
				httpResponse.StatusCode,
				strings.TrimSpace(string(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	var response parallelSearchResponse

	err = json.NewDecoder(httpResponse.Body).Decode(&response)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("decode Parallel search response for %q: %w", query, err)
	}

	urls := make([]string, 0, len(response.Results))
	for _, result := range response.Results {
		if trimmedURL := strings.TrimSpace(result.URL); trimmedURL != "" {
			urls = append(urls, trimmedURL)
		}
	}

	// Search returns compressed excerpts, not full page bodies. Enrich every
	// result with full content from the Extract API (same API key as the
	// search, mirroring tinyFish search+fetch). Extract failures are
	// non-fatal: excerpts alone still produce a usable result.
	fetchedContentMap, fetchedTitleMap := client.fetchFullContents(ctx, apiKey, urls, query, maxCharsPerResult)

	return webSearchResult{
		Query: query,
		Text:  formatParallelSearchResultText(response.Results, fetchedContentMap, fetchedTitleMap, maxCharsPerResult),
	}, nil
}

// extractURLForRequest resolves the Extract endpoint, falling back to the
// search endpoint for test clients that override only endpoint.
func (client parallelSearchClient) extractURLForRequest() string {
	if strings.TrimSpace(client.extractEndpoint) != "" {
		return client.extractEndpoint
	}

	if strings.TrimSpace(client.endpoint) != "" && client.endpoint != defaultParallelSearchEndpoint {
		return client.endpoint
	}

	return defaultParallelExtractEndpoint
}

func (client parallelSearchClient) fetchFullContents(
	ctx context.Context,
	apiKey string,
	urls []string,
	query string,
	maxCharsPerResult int,
) (map[string]string, map[string]string) {
	fetchedContentMap := make(map[string]string)
	fetchedTitleMap := make(map[string]string)

	if len(urls) == 0 {
		return fetchedContentMap, fetchedTitleMap
	}

	batchSize := parallelExtractMaxURLsPerRequest
	if batchSize <= 0 {
		batchSize = len(urls)
	}

	batchCount := (len(urls) + batchSize - 1) / batchSize
	if batchCount == 1 {
		extractResponse, err := client.fetchExtractBatch(ctx, apiKey, urls, query, maxCharsPerResult)
		if err != nil {
			logWarn("parallel extract for search enrichment failed", err, "query", query)

			return fetchedContentMap, fetchedTitleMap
		}

		mergeParallelExtractResults(extractResponse, fetchedContentMap, fetchedTitleMap)

		return fetchedContentMap, fetchedTitleMap
	}

	taskResults := runTasksConcurrently(
		ctx,
		externalRequestConcurrency,
		batchCount,
		func(taskCtx context.Context, index int) (parallelExtractResponse, error) {
			start := index * batchSize
			end := min(start+batchSize, len(urls))

			return client.fetchExtractBatch(taskCtx, apiKey, urls[start:end], query, maxCharsPerResult)
		},
	)

	hasSuccess := false

	var firstErr error

	for _, result := range taskResults {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}

			logWarn("parallel extract batch failed", result.err, "query", query)

			continue
		}

		hasSuccess = true

		mergeParallelExtractResults(result.value, fetchedContentMap, fetchedTitleMap)
	}

	if !hasSuccess {
		logWarn("parallel extract for search enrichment failed", firstErr, "query", query)
	}

	return fetchedContentMap, fetchedTitleMap
}

func mergeParallelExtractResults(
	extractResponse parallelExtractResponse,
	fetchedContentMap map[string]string,
	fetchedTitleMap map[string]string,
) {
	if len(extractResponse.Errors) > 0 {
		for _, extractErr := range extractResponse.Errors {
			logWarn(
				"parallel extract reported URL error",
				nil,
				"url", extractErr.URL,
				"error_type", extractErr.ErrorType,
			)
		}
	}

	for _, extractResult := range extractResponse.Results {
		content := trimmedOptionalString(extractResult.FullContent)
		if content == "" {
			continue
		}

		trimmedURL := strings.TrimSpace(extractResult.URL)
		if trimmedURL == "" {
			continue
		}

		key := strings.ToLower(trimmedURL)
		fetchedContentMap[key] = content

		if title := trimmedOptionalString(extractResult.Title); title != "" {
			fetchedTitleMap[key] = title
		}
	}
}

func (client parallelSearchClient) fetchExtractBatch(
	ctx context.Context,
	apiKey string,
	batch []string,
	query string,
	maxCharsPerResult int,
) (parallelExtractResponse, error) {
	if len(batch) == 0 {
		return parallelExtractResponse{ExtractID: "", Results: nil, Errors: nil}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, parallelExtractRequestTimeout)
	defer cancel()

	requestBody := parallelExtractRequest{
		URLs:          batch,
		Objective:     query,
		SearchQueries: []string{query},
		AdvancedSettings: &parallelExtractSettings{
			ExcerptSettings: &parallelExcerptSettings{
				MaxCharsPerResult: maxCharsPerResult,
			},
			FullContent: &parallelExtractFullContentSettings{
				MaxCharsPerResult: maxCharsPerResult,
			},
		},
	}

	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return parallelExtractResponse{}, fmt.Errorf("marshal Parallel extract request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.extractURLForRequest(),
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return parallelExtractResponse{}, fmt.Errorf("create Parallel extract request: %w", err)
	}

	httpRequest.Header.Set("Accept", applicationJSONContentType)
	httpRequest.Header.Set("X-Api-Key", strings.TrimSpace(apiKey))
	httpRequest.Header.Set(contentTypeHeader, applicationJSONContentType)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return parallelExtractResponse{}, fmt.Errorf("send Parallel extract request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return parallelExtractResponse{}, fmt.Errorf(
				"read Parallel extract error response after status %d: %w",
				httpResponse.StatusCode,
				readErr,
			)
		}

		return parallelExtractResponse{}, parallelStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"Parallel extract request failed with status %d: %s",
				httpResponse.StatusCode,
				strings.TrimSpace(string(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	var extractResponse parallelExtractResponse

	if err := json.NewDecoder(httpResponse.Body).Decode(&extractResponse); err != nil {
		return parallelExtractResponse{}, fmt.Errorf("decode Parallel extract response: %w", err)
	}

	return extractResponse, nil
}

func (client tinyFishSearchClient) search(
	ctx context.Context,
	loadedConfig config,
	queries []string,
) ([]webSearchResult, error) {
	apiKeys := loadedConfig.WebSearch.TinyFish.apiKeys()
	if len(apiKeys) == 0 {
		return nil, fmt.Errorf("tinyfish search is not configured: %w", os.ErrNotExist)
	}

	maxURLs := loadedConfig.WebSearch.maxURLs()
	maxChars := loadedConfig.WebSearch.TinyFish.maxCharsPerResult()

	// Phase 1: searches run concurrently; the fetch in phase 2 needs every
	// query's URLs first, so enrichment cannot overlap the searches.
	outcomes, err := searchQueriesConcurrently(ctx, queries, func(
		queryContext context.Context,
		query string,
	) (tinyFishQueryOutcome, error) {
		return tryAllAPIKeys(queryContext, client.keys, apiKeys, func(apiKey string) (tinyFishQueryOutcome, error) {
			searchResults, err := client.searchQuery(queryContext, apiKey, query)
			if err != nil {
				return tinyFishQueryOutcome{}, err
			}

			if len(searchResults) > maxURLs {
				searchResults = searchResults[:maxURLs]
			}

			return tinyFishQueryOutcome{query: query, results: searchResults}, nil
		})
	})
	if err != nil {
		return nil, err
	}

	// Phase 2: one deduplicated fetch across all queries, bounded by the
	// fetch deadline. Overlapping URLs (common when the model issues related
	// queries) are fetched once, and repeat URLs inside the cache TTL cost
	// no round trip at all.
	fetchedTextMap, fetchedTitleMap := client.enrichTinyFishOutcomes(ctx, apiKeys, outcomes)

	formatted := make([]webSearchResult, len(outcomes))
	for index, outcome := range outcomes {
		if len(outcome.results) == 0 {
			formatted[index] = webSearchResult{
				Query: outcome.query,
				Text:  "No search results found.",
			}

			continue
		}

		formatted[index] = webSearchResult{
			Query: outcome.query,
			Text:  formatTinyFishSearchResultText(outcome.results, fetchedTextMap, fetchedTitleMap, maxChars),
		}
	}

	return formatted, nil
}

func (client tinyFishSearchClient) enrichTinyFishOutcomes(
	ctx context.Context,
	apiKeys []string,
	outcomes []tinyFishQueryOutcome,
) (map[string]string, map[string]string) {
	// Canonicalize fetch results by lower-cased URL for case-insensitive lookup.
	fetchedTextMap := make(map[string]string)
	fetchedTitleMap := make(map[string]string)

	uniqueURLs := make([]string, 0)
	seenURLs := make(map[string]struct{})

	for _, outcome := range outcomes {
		for _, result := range outcome.results {
			trimmedURL := strings.TrimSpace(result.URL)
			if trimmedURL == "" {
				continue
			}

			key := strings.ToLower(trimmedURL)
			if _, seen := seenURLs[key]; seen {
				continue
			}

			seenURLs[key] = struct{}{}

			uniqueURLs = append(uniqueURLs, trimmedURL)
		}
	}

	missingURLs := make([]string, 0, len(uniqueURLs))

	for _, rawURL := range uniqueURLs {
		if text, title, _, _, _, ok := client.fetchCache.lookup(rawURL); ok {
			key := strings.ToLower(strings.TrimSpace(rawURL))
			fetchedTextMap[key] = text

			if title != "" {
				fetchedTitleMap[key] = title
			}

			continue
		}

		missingURLs = append(missingURLs, rawURL)
	}

	if len(missingURLs) == 0 {
		return fetchedTextMap, fetchedTitleMap
	}

	fetchResults, fetchErr := client.fetchSearchPages(ctx, apiKeys, missingURLs)
	if fetchErr != nil {
		queries := make([]string, 0, len(outcomes))
		for _, outcome := range outcomes {
			queries = append(queries, outcome.query)
		}

		logWarn("tinyfish fetch for search enrichment failed", fetchErr, "queries", queries)
	}

	for _, fetchResult := range fetchResults {
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

		for _, rawURL := range []string{fetchResult.URL, fetchResult.FinalURL} {
			trimmed := strings.TrimSpace(rawURL)
			if trimmed == "" {
				continue
			}

			key := strings.ToLower(trimmed)
			fetchedTextMap[key] = textStr

			if title != "" {
				fetchedTitleMap[key] = title
			}
		}
	}

	return fetchedTextMap, fetchedTitleMap
}

func (client tinyFishSearchClient) searchQuery(
	ctx context.Context,
	apiKey string,
	query string,
) ([]tinyFishSearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, tinyFishSearchRequestTimeout)
	defer cancel()

	queryValues := url.Values{}
	queryValues.Set("query", query)

	searchURL := client.searchEndpoint
	if strings.Contains(searchURL, "?") {
		searchURL += "&" + queryValues.Encode()
	} else {
		searchURL += "?" + queryValues.Encode()
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create TinyFish search request for %q: %w", query, err)
	}

	httpRequest.Header.Set("Accept", applicationJSONContentType)
	httpRequest.Header.Set("X-API-Key", strings.TrimSpace(apiKey))

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("send TinyFish search request for %q: %w", query, err)
	}
	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return nil, fmt.Errorf(
				"read TinyFish search error response for %q after status %d: %w",
				query,
				httpResponse.StatusCode,
				readErr,
			)
		}

		return nil, tinyFishStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"TinyFish search request failed for %q with status %d: %s",
				query,
				httpResponse.StatusCode,
				strings.TrimSpace(string(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	var response tinyFishSearchResponse

	err = json.NewDecoder(httpResponse.Body).Decode(&response)
	if err != nil {
		return nil, fmt.Errorf("decode TinyFish search response for %q: %w", query, err)
	}

	return response.Results, nil
}

// fetchSearchPages fetches search result pages under one hard deadline
// (fetchDeadline). Every page gets its own request: a Fetch batch answers
// only once its slowest URL finishes, so batching would let one slow page
// hold every other page of the batch past the deadline. Pages that miss the
// deadline are cut off and keep their search snippet. The results that
// arrived are returned even when some requests failed; the error reports
// failures other than the deadline, such as rejected API keys.
func (client tinyFishSearchClient) fetchSearchPages(
	ctx context.Context,
	apiKeys []string,
	urls []string,
) ([]tinyFishFetchResult, error) {
	deadline := client.fetchDeadline
	if deadline <= 0 {
		deadline = tinyFishSearchFetchDeadline
	}

	fetchCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	taskResults := runTasksConcurrently(
		fetchCtx,
		tinyFishSearchFetchConcurrency,
		len(urls),
		func(taskCtx context.Context, index int) (tinyFishFetchResponse, error) {
			return tryAllAPIKeys(taskCtx, client.keys, apiKeys, func(apiKey string) (tinyFishFetchResponse, error) {
				return client.fetchTinyFishBatch(taskCtx, apiKey, urls[index:index+1])
			})
		},
	)

	var (
		results     []tinyFishFetchResult
		missedPages int
		failedPages int
		firstErr    error
	)

	for _, taskResult := range taskResults {
		switch {
		case taskResult.err == nil:
			results = append(results, taskResult.value.Results...)
		case errors.Is(taskResult.err, context.DeadlineExceeded) || errors.Is(taskResult.err, context.Canceled):
			missedPages++
		default:
			failedPages++

			if firstErr == nil {
				firstErr = taskResult.err
			}
		}
	}

	if missedPages > 0 && errors.Is(fetchCtx.Err(), context.DeadlineExceeded) {
		slog.Info(
			"tinyfish fetch deadline reached; missed pages keep their search snippet",
			"deadline", deadline,
			"pages", len(urls),
			"missed_pages", missedPages,
		)
	}

	if firstErr != nil {
		return results, fmt.Errorf("fetch %d of %d search result pages: %w", failedPages, len(urls), firstErr)
	}

	return results, nil
}

func (client tinyFishSearchClient) fetchTinyFishBatch(
	ctx context.Context,
	apiKey string,
	batch []string,
) (tinyFishFetchResponse, error) {
	if len(batch) == 0 {
		return tinyFishFetchResponse{}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, tinyFishFetchRequestTimeout)
	defer cancel()

	requestBody := tinyFishFetchRequest{
		URLs:            batch,
		Format:          "markdown",
		PerURLTimeoutMS: tinyFishFetchPerURLTimeoutMS,
	}

	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return tinyFishFetchResponse{}, fmt.Errorf("marshal TinyFish fetch request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.fetchEndpoint,
		bytes.NewReader(requestBytes),
	)
	if err != nil {
		return tinyFishFetchResponse{}, fmt.Errorf("create TinyFish fetch request: %w", err)
	}

	httpRequest.Header.Set("Accept", applicationJSONContentType)
	httpRequest.Header.Set(contentTypeHeader, applicationJSONContentType)
	httpRequest.Header.Set("X-API-Key", strings.TrimSpace(apiKey))

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return tinyFishFetchResponse{}, fmt.Errorf("send TinyFish fetch request: %w", err)
	}
	defer func() { _ = httpResponse.Body.Close() }()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return tinyFishFetchResponse{}, fmt.Errorf("read TinyFish fetch error response after status %d: %w", httpResponse.StatusCode, readErr)
		}

		return tinyFishFetchResponse{}, tinyFishStatusError{
			StatusCode: httpResponse.StatusCode,
			Message: fmt.Sprintf(
				"TinyFish fetch request failed with status %d: %s",
				httpResponse.StatusCode,
				strings.TrimSpace(string(responseBody)),
			),
			Err: os.ErrInvalid,
		}
	}

	var batchResponse tinyFishFetchResponse
	if err := json.NewDecoder(httpResponse.Body).Decode(&batchResponse); err != nil {
		return tinyFishFetchResponse{}, fmt.Errorf("decode TinyFish fetch response: %w", err)
	}

	return batchResponse, nil
}

func tinyFishFetchResultText(rawText any) string {
	switch value := rawText.(type) {
	case string:
		return value
	case map[string]any:
		if formatted, err := json.Marshal(value); err == nil {
			return string(formatted)
		}

		return fmt.Sprint(value)
	case nil:
		return ""
	default:
		formatted, err := json.Marshal(value)
		if err == nil {
			return string(formatted)
		}

		return fmt.Sprint(value)
	}
}

func formatTinyFishSearchResultText(
	results []tinyFishSearchResult,
	fetchedTextMap map[string]string,
	fetchedTitleMap map[string]string,
	maxCharsPerResult int,
) string {
	if maxCharsPerResult <= 0 {
		maxCharsPerResult = defaultTinyFishMaxCharsPerResult
	}

	formattedResults := make([]string, 0, len(results))
	for _, result := range results {
		lines := make([]string, 0, 6)

		title := strings.TrimSpace(result.Title)
		if title == "" {
			if fetchedTitle := strings.TrimSpace(fetchedTitleMap[strings.ToLower(strings.TrimSpace(result.URL))]); fetchedTitle != "" {
				title = fetchedTitle
			}
		}

		if title != "" {
			lines = append(lines, "Title: "+title)
		}

		trimmedURL := strings.TrimSpace(result.URL)
		if trimmedURL != "" {
			lines = append(lines, "URL: "+trimmedURL)
		}

		if siteName := strings.TrimSpace(result.SiteName); siteName != "" {
			lines = append(lines, "Site: "+siteName)
		}

		if snippet := strings.TrimSpace(result.Snippet); snippet != "" {
			lines = append(lines, formatSearchMultilineField("Snippet", snippet))
		}

		fetchedText := fetchedTextMap[strings.ToLower(strings.TrimSpace(result.URL))]
		if fetchedText != "" {
			fetchedText = truncateRunes(strings.TrimSpace(fetchedText), maxCharsPerResult)
			lines = append(lines, formatSearchMultilineField("Content", fetchedText))
		} else if snippet := strings.TrimSpace(result.Snippet); snippet == "" {
			lines = append(lines, "Content: [No extracted content — fetch failed]")
		}

		if len(lines) == 0 {
			continue
		}

		formattedResults = append(formattedResults, strings.Join(lines, "\n"))
	}

	return strings.Join(formattedResults, "\n\n")
}

func formatTavilySearchResultText(results []tavilySearchResponseResult, maxCharsPerResult int) string {
	if maxCharsPerResult <= 0 {
		maxCharsPerResult = defaultTavilyMaxCharsPerResult
	}

	formattedResults := make([]string, 0, len(results))

	for _, result := range results {
		lines := make([]string, 0, tavilyResultFieldCapacity)

		title := strings.TrimSpace(result.Title)
		if title != "" {
			lines = append(lines, "Title: "+title)
		}

		url := strings.TrimSpace(result.URL)
		if url != "" {
			lines = append(lines, "URL: "+url)
		}

		if content := truncateRunes(strings.TrimSpace(result.Content), maxCharsPerResult); content != "" {
			if snippet := formatSearchMultilineField("Text", content); snippet != "" {
				lines = append(lines, snippet)
			}
		}

		if rawContent := truncateRunes(strings.TrimSpace(result.RawContent), maxCharsPerResult); rawContent != "" {
			if formatted := formatSearchMultilineField("Raw Content", rawContent); formatted != "" {
				lines = append(lines, formatted)
			}
		}

		if len(lines) == 0 {
			continue
		}

		formattedResults = append(formattedResults, strings.Join(lines, "\n"))
	}

	return strings.Join(formattedResults, "\n\n")
}

func formatParallelSearchResultText(
	results []parallelSearchResponseItem,
	fetchedContentMap map[string]string,
	fetchedTitleMap map[string]string,
	maxCharsPerResult int,
) string {
	if maxCharsPerResult <= 0 {
		maxCharsPerResult = defaultParallelMaxCharsPerResult
	}

	formattedResults := make([]string, 0, len(results))
	for _, result := range results {
		lines := make([]string, 0, 5)

		trimmedURL := strings.TrimSpace(result.URL)
		lowerURL := strings.ToLower(trimmedURL)

		title := strings.TrimSpace(result.Title)
		if title == "" {
			title = strings.TrimSpace(fetchedTitleMap[lowerURL])
		}

		if title != "" {
			lines = append(lines, "Title: "+title)
		}

		if trimmedURL != "" {
			lines = append(lines, "URL: "+trimmedURL)
		}

		if publishedDate := trimmedOptionalString(result.PublishDate); publishedDate != "" {
			lines = append(lines, "Published Date: "+publishedDate)
		}

		excerpts := formatSearchListField("Excerpts", result.Excerpts)
		if excerpts != "" {
			lines = append(lines, excerpts)
		}

		fetchedText := strings.TrimSpace(fetchedContentMap[lowerURL])
		if fetchedText != "" {
			fetchedText = truncateRunes(fetchedText, maxCharsPerResult)
			lines = append(lines, formatSearchMultilineField("Content", fetchedText))
		} else if excerpts == "" {
			lines = append(lines, "Content: [No extracted content — fetch failed]")
		}

		if len(lines) == 0 {
			continue
		}

		formattedResults = append(formattedResults, strings.Join(lines, "\n"))
	}

	return strings.Join(formattedResults, "\n\n")
}

func formatExaSearchResultText(results []exaSearchResponseResult) string {
	formattedResults := make([]string, 0, len(results))

	for _, result := range results {
		lines := formatExaSearchResultLines(result)
		if len(lines) == 0 {
			continue
		}

		formattedResults = append(formattedResults, strings.Join(lines, "\n"))
	}

	return strings.Join(formattedResults, "\n\n")
}

func formatExaSearchResultLines(result exaSearchResponseResult) []string {
	lines := make([]string, 0, defaultWebSearchMaxURLs)

	title := strings.TrimSpace(result.Title)
	if title != "" {
		lines = append(lines, "Title: "+title)
	}

	url := strings.TrimSpace(result.URL)
	if url != "" {
		lines = append(lines, "URL: "+url)
	}

	if publishedDate := trimmedOptionalString(result.PublishedDate); publishedDate != "" {
		lines = append(lines, "Published Date: "+publishedDate)
	}

	if author := trimmedOptionalString(result.Author); author != "" {
		lines = append(lines, "Author: "+author)
	}

	highlights := formatSearchListField("Highlights", result.Highlights)
	if highlights != "" {
		lines = append(lines, highlights)
	}

	summary := formatSearchMultilineField("Summary", trimmedOptionalString(result.Summary))
	if summary != "" {
		lines = append(lines, summary)
	}

	text := formatSearchMultilineField("Text", trimmedOptionalString(result.Text))
	if text != "" {
		lines = append(lines, text)
	}

	return lines
}

func formatSearchMultilineField(label, value string) string {
	trimmedValue := strings.TrimSpace(value)
	if trimmedValue == "" {
		return ""
	}

	lines := strings.Split(trimmedValue, "\n")
	for index, line := range lines {
		lines[index] = "| " + strings.TrimSpace(line)
	}

	return label + ":\n" + strings.Join(lines, "\n")
}

func formatSearchListField(label string, values []string) string {
	lines := make([]string, 0, len(values))

	for _, value := range values {
		trimmedValue := strings.TrimSpace(value)
		if trimmedValue == "" {
			continue
		}

		for line := range strings.SplitSeq(trimmedValue, "\n") {
			lines = append(lines, "| "+strings.TrimSpace(line))
		}
	}

	if len(lines) == 0 {
		return ""
	}

	return label + ":\n" + strings.Join(lines, "\n")
}

func trimmedOptionalString(value *string) string {
	if value == nil {
		return ""
	}

	return strings.TrimSpace(*value)
}

func extractExaErrorMessage(responseBody []byte) string {
	var rawResponse map[string]any

	err := json.Unmarshal(responseBody, &rawResponse)
	if err == nil {
		response, parseErr := parseExaSearchResponse(rawResponse)
		if parseErr == nil && strings.TrimSpace(response.Error) != "" {
			return response.Error
		}
	}

	return string(responseBody)
}

func parseExaSearchResponse(rawResponse map[string]any) (exaSearchResponse, error) {
	response := exaSearchResponse{Results: nil, Error: mapStringValue(rawResponse, "error")}

	rawResults, hasResults := rawResponse["results"]
	if !hasResults || rawResults == nil {
		return response, nil
	}

	results, isList := rawResults.([]any)
	if !isList {
		return exaSearchResponse{}, fmt.Errorf("decode Exa results: %w", os.ErrInvalid)
	}

	response.Results = make([]exaSearchResponseResult, 0, len(results))

	for _, rawResult := range results {
		resultMap, ok := rawResult.(map[string]any)
		if !ok {
			return exaSearchResponse{}, fmt.Errorf("decode Exa result: %w", os.ErrInvalid)
		}

		response.Results = append(response.Results, exaSearchResponseResult{
			ID:            mapOptionalStringValue(resultMap, "id"),
			Title:         mapStringValue(resultMap, "title"),
			URL:           mapStringValue(resultMap, "url"),
			PublishedDate: mapOptionalStringValue(resultMap, "publishedDate"),
			Author:        mapOptionalStringValue(resultMap, "author"),
			Image:         mapOptionalStringValue(resultMap, "image"),
			Favicon:       mapOptionalStringValue(resultMap, "favicon"),
			Highlights:    mapStringSliceValue(resultMap, "highlights"),
			Summary:       mapOptionalStringValue(resultMap, "summary"),
			Text:          mapOptionalStringValue(resultMap, "text"),
		})
	}

	return response, nil
}

func mapStringValue(values map[string]any, key string) string {
	value, isString := values[key].(string)
	if !isString {
		return ""
	}

	return strings.TrimSpace(value)
}

func mapOptionalStringValue(values map[string]any, key string) *string {
	value, hasValue := values[key]
	if !hasValue || value == nil {
		return nil
	}

	stringValue, ok := value.(string)
	if !ok {
		return nil
	}

	trimmedValue := strings.TrimSpace(stringValue)
	if trimmedValue == "" {
		return nil
	}

	return &trimmedValue
}

func mapStringSliceValue(values map[string]any, key string) []string {
	rawValues, ok := values[key].([]any)
	if !ok {
		return nil
	}

	stringValues := make([]string, 0, len(rawValues))
	for _, rawValue := range rawValues {
		stringValue, ok := rawValue.(string)
		if !ok {
			continue
		}

		trimmedValue := strings.TrimSpace(stringValue)
		if trimmedValue == "" {
			continue
		}

		stringValues = append(stringValues, trimmedValue)
	}

	if len(stringValues) == 0 {
		return nil
	}

	return stringValues
}
