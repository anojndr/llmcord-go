package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	exaSearchToolName    = "web_search_exa"
	searchWarningText    = "Warning: web search unavailable"
	messageRoleUser      = "user"
	contentTypeAudioData = "audio_data"
	contentTypeDocument  = "document_data"
	contentTypeImageURL  = "image_url"
	contentTypeText      = "text"
	contentTypeVideoData = "video_data"
	contentFieldBytes    = "data"
	contentFieldFilename = "filename"
	contentFieldMIMEType = "mime_type"
	mimeTypePDF          = "application/pdf"
	mimeTypePNG          = "image/png"
	searchAnswerTemplate = `Answer the user query based on the web search results.

User query:
%s

Web search results:
%s`
	searchDeciderPrompt = `You are a search decider.
Your job is to decide whether the assistant needs web search
to answer the user's request.

Return only valid JSON, with no extra text.

Output formats:

If web search is NOT needed:
{"needs_search": false}

If web search IS needed:
{"needs_search": true, "queries": ["query1", "query2"]}

Rules:

1. Use web search only when it is actually needed to answer well.
2. Do not use web search for:
   - casual conversation
   - pure writing/rewriting
   - summarizing text already provided
   - translation
   - simple reasoning or general knowledge that does not depend on recent or external information
3. Use web search when:
   - the user asks for latest/current/recent/news/today information
   - the answer depends on facts that may have changed over time
   - the user asks to verify a claim
   - the user asks about a specific real-world fact and confidence would benefit from checking sources
   - the request refers to content in an image that needs to be identified or looked up
4. When search is needed, generate the smallest useful set of queries.
5. Use only 1 query if 1 query is enough.
6. Use multiple queries only when the request clearly contains
multiple distinct facts/subquestions that should be searched separately.
7. Make queries concrete and directly searchable.
Do not copy vague follow-up phrasing like "verify this" or "search that".
8. Resolve pronouns and follow-ups using conversation context.
9. If the user refers to an image, convert the actual things mentioned or shown into search queries.
10. Prefer short, high-signal queries. Avoid filler words.
11. For verification requests, turn the claim itself into the query.
12. If the request includes multiple entities from an image or list, output one query per entity when appropriate.
13. Do not include explanations, confidence scores, or any keys other than the required JSON fields.

Query construction rules:

- For a single topic needing one lookup:
  "queries": ["latest news"]

- For multiple distinct subquestions:
  "queries": ["gpt 5.2 release date", "gpt 5.2 context window"]

- For vague follow-ups, rewrite using context:
  If prior claim was "Daniel Radcliffe is gay" and the next message is "verify this"
  then output:
  {"needs_search": true, "queries": ["daniel radcliffe is gay?"]}

- For image-based requests:
  If the user says "search everything mentioned in the image"
  and the image contains or depicts apple, orange, and cat
  then output:
  {"needs_search": true, "queries": ["apple", "orange", "cat"]}

Examples:

User: "Write a polite reply to this email"
Output: {"needs_search": false}

User: "What is 17 * 24?"
Output: {"needs_search": false}

User: "latest news"
Output: {"needs_search": true, "queries": ["latest news"]}

User: "release date and context window of gpt 5.2"
Output: {"needs_search": true, "queries": ["gpt 5.2 release date", "gpt 5.2 context window"]}

User: "verify this"
Context from previous turn: "Daniel Radcliffe is gay"
Output: {"needs_search": true, "queries": ["daniel radcliffe is gay?"]}

User: "search everything mentioned in the image"
Context: image shows or contains apple, orange, cat
Output: {"needs_search": true, "queries": ["apple", "orange", "cat"]}

Now decide based on the full conversation context, including prior turns and any attached images, and return only JSON.`
)

var errExaSearchTool = errors.New("exa MCP search tool returned an error")

type chatCompletionClient interface {
	streamChatCompletion(
		ctx context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error
}

type webSearchClient interface {
	search(ctx context.Context, loadedConfig config, queries []string) ([]webSearchResult, error)
}

type searchDecision struct {
	NeedsSearch bool     `json:"needs_search"`
	Queries     []string `json:"queries"`
}

type searchMetadata struct {
	Queries []string
	Results []webSearchResult
}

type webSearchResult struct {
	Query string
	Text  string
}

type searchSource struct {
	Title string
	URL   string
}

type exaSearchClient struct {
	endpoint   string
	httpClient *http.Client
}

type tavilySearchClient struct {
	endpoint   string
	httpClient *http.Client
}

type routedWebSearchClient struct {
	mcp    webSearchClient
	tavily webSearchClient
}

type tavilySearchRequest struct {
	Query             string `json:"query"`
	SearchDepth       string `json:"search_depth"`
	MaxResults        int    `json:"max_results"`
	IncludeRawContent string `json:"include_raw_content"`
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

func newExaSearchClient(httpClient *http.Client) exaSearchClient {
	return exaSearchClient{
		endpoint:   defaultExaMCPEndpoint,
		httpClient: httpClient,
	}
}

func newTavilySearchClient(httpClient *http.Client) tavilySearchClient {
	return tavilySearchClient{
		endpoint:   defaultTavilySearchEndpoint,
		httpClient: httpClient,
	}
}

func newWebSearchClient(httpClient *http.Client) routedWebSearchClient {
	return routedWebSearchClient{
		mcp:    newExaSearchClient(httpClient),
		tavily: newTavilySearchClient(httpClient),
	}
}

func (err tavilyStatusError) Error() string {
	return err.Message
}

func (err tavilyStatusError) Unwrap() error {
	if err.Err == nil {
		return os.ErrInvalid
	}

	return err.Err
}

func (err tavilyStatusError) retryWithNextAPIKey() bool {
	switch err.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func (instance *bot) maybeAugmentConversationWithWebSearch(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) ([]chatMessage, *searchMetadata, []string, error) {
	decision, err := instance.decideWebSearch(
		ctx,
		loadedConfig,
		providerSlashModel,
		sourceMessage,
		conversation,
	)
	if err != nil {
		slog.Warn("decide web search", "error", err)

		return conversation, nil, []string{searchWarningText}, nil
	}

	if !decision.NeedsSearch {
		return conversation, nil, nil, nil
	}

	results, err := instance.webSearch.search(ctx, loadedConfig, decision.Queries)
	if err != nil {
		slog.Warn("run web search", "queries", decision.Queries, "error", err)

		return conversation, nil, []string{searchWarningText}, nil
	}

	augmentedConversation, err := appendWebSearchResultsToConversation(
		conversation,
		formatWebSearchResults(results),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("append web search results to conversation: %w", err)
	}

	return augmentedConversation, newSearchMetadata(decision.Queries, results), nil, nil
}

func newSearchMetadata(queries []string, results []webSearchResult) *searchMetadata {
	metadata := new(searchMetadata)
	metadata.Queries = append(metadata.Queries, queries...)
	metadata.Results = append(metadata.Results, results...)

	return metadata
}

func cloneSearchMetadata(metadata *searchMetadata) *searchMetadata {
	if metadata == nil {
		return nil
	}

	return newSearchMetadata(metadata.Queries, metadata.Results)
}

func (client routedWebSearchClient) search(
	ctx context.Context,
	loadedConfig config,
	queries []string,
) ([]webSearchResult, error) {
	primaryProvider, fallbackProvider := loadedConfig.WebSearch.providersInOrder()

	results, err := client.searchWithProvider(ctx, loadedConfig, primaryProvider, queries)
	if err == nil {
		return results, nil
	}

	fallbackResults, fallbackErr := client.searchWithProvider(
		ctx,
		loadedConfig,
		fallbackProvider,
		queries,
	)
	if fallbackErr == nil {
		return fallbackResults, nil
	}

	return nil, fmt.Errorf(
		"search with %s failed, and %s fallback failed: %w",
		primaryProvider.displayName(),
		fallbackProvider.displayName(),
		errors.Join(err, fallbackErr),
	)
}

func (client routedWebSearchClient) searchWithProvider(
	ctx context.Context,
	loadedConfig config,
	provider webSearchProviderKind,
	queries []string,
) ([]webSearchResult, error) {
	switch provider {
	case webSearchProviderKindMCP:
		return client.mcp.search(ctx, loadedConfig, queries)
	case webSearchProviderKindTavily:
		return client.tavily.search(ctx, loadedConfig, queries)
	default:
		return nil, fmt.Errorf("unsupported web search provider %q: %w", provider, os.ErrInvalid)
	}
}

func (instance *bot) decideWebSearch(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) (searchDecision, error) {
	searchDeciderModel := instance.currentSearchDeciderModelForConfig(loadedConfig)

	searchDeciderMessages, err := instance.buildSearchDeciderConversation(
		ctx,
		loadedConfig,
		providerSlashModel,
		searchDeciderModel,
		sourceMessage,
		conversation,
	)
	if err != nil {
		return searchDecision{}, fmt.Errorf("prepare search decider conversation: %w", err)
	}

	searchDeciderMessages = append(
		[]chatMessage{{Role: "system", Content: searchDeciderPrompt}},
		searchDeciderMessages...,
	)

	request, err := buildChatCompletionRequest(
		loadedConfig,
		searchDeciderModel,
		searchDeciderMessages,
	)
	if err != nil {
		return searchDecision{}, fmt.Errorf("build search decider request: %w", err)
	}

	searchContext, cancel := context.WithTimeout(ctx, searchDeciderTimeout)
	defer cancel()

	responseText, err := collectChatCompletionText(searchContext, instance.chatCompletions, request)
	if err != nil {
		return searchDecision{}, fmt.Errorf("collect search decider response: %w", err)
	}

	decision, err := parseSearchDecision(responseText)
	if err != nil {
		return searchDecision{}, fmt.Errorf("parse search decider response %q: %w", responseText, err)
	}

	return decision, nil
}

func (instance *bot) buildSearchDeciderConversation(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	searchDeciderModel string,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) ([]chatMessage, error) {
	searchDeciderConversationWithImages, err := instance.maybeAugmentConversationWithSearchDeciderImages(
		ctx,
		loadedConfig,
		providerSlashModel,
		searchDeciderModel,
		sourceMessage,
		conversation,
	)
	if err != nil {
		return nil, fmt.Errorf("append search decider images: %w", err)
	}

	sanitizedConversation, err := searchDeciderConversation(
		searchDeciderConversationWithImages,
		loadedConfig,
		searchDeciderModel,
	)
	if err != nil {
		return nil, err
	}

	return sanitizedConversation, nil
}

func (instance *bot) maybeAugmentConversationWithSearchDeciderImages(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	searchDeciderModel string,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) ([]chatMessage, error) {
	searchContentOptions, err := messageContentOptionsForModel(
		loadedConfig,
		searchDeciderModel,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"build search decider content options for %q: %w",
			searchDeciderModel,
			err,
		)
	}

	if searchContentOptions.maxImages <= 0 {
		return conversation, nil
	}

	mainContentOptions, err := messageContentOptionsForModel(
		loadedConfig,
		providerSlashModel,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"build main model content options for %q: %w",
			providerSlashModel,
			err,
		)
	}

	if searchContentOptions.maxImages <= mainContentOptions.maxImages {
		return conversation, nil
	}

	remainingImageSlots, err := remainingImageSlotsForConversation(
		conversation,
		searchContentOptions.maxImages,
	)
	if err != nil {
		return nil, err
	}

	if remainingImageSlots == 0 {
		return conversation, nil
	}

	candidateImageParts, err := instance.searchDeciderImagePartsForMessage(
		ctx,
		sourceMessage,
		conversation,
		remainingImageSlots,
	)
	if err != nil {
		return nil, err
	}

	return appendMediaPartsToConversation(conversation, candidateImageParts)
}

func (instance *bot) searchDeciderImagePartsForMessage(
	ctx context.Context,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
	maxImageParts int,
) ([]contentPart, error) {
	if sourceMessage == nil || maxImageParts <= 0 {
		return nil, nil
	}

	imageURLSet, err := latestUserImageURLSet(conversation)
	if err != nil {
		return nil, err
	}

	candidateImageParts := make([]contentPart, 0, maxImageParts)

	appendImagePart := func(imagePart contentPart) error {
		imageURL, imageErr := contentPartImageURL(imagePart)
		if imageErr != nil {
			return imageErr
		}

		if _, exists := imageURLSet[imageURL]; exists {
			return nil
		}

		imageURLSet[imageURL] = struct{}{}

		candidateImageParts = append(candidateImageParts, cloneContentPart(imagePart))

		return nil
	}

	imageParts, err := instance.imagePartsForMessage(ctx, sourceMessage)
	if err != nil {
		return nil, fmt.Errorf("load image parts for search decider: %w", err)
	}

	for _, imagePart := range imageParts {
		appendErr := appendImagePart(imagePart)
		if appendErr != nil {
			return nil, fmt.Errorf(
				"append image attachment for search decider: %w",
				appendErr,
			)
		}

		if len(candidateImageParts) == maxImageParts {
			return candidateImageParts, nil
		}
	}

	documentParts, err := instance.documentPartsForMessage(ctx, sourceMessage)
	if err != nil {
		return nil, fmt.Errorf("load pdf parts for search decider: %w", err)
	}

	for index, documentPart := range documentParts {
		extraction, extractionErr := extractPDFContent(documentPart)
		if extractionErr != nil {
			return nil, fmt.Errorf(
				"extract pdf images for search decider file %d: %w",
				index+1,
				extractionErr,
			)
		}

		for _, imagePart := range extraction.imageParts {
			appendErr := appendImagePart(imagePart)
			if appendErr != nil {
				return nil, fmt.Errorf("append pdf image for search decider: %w", appendErr)
			}

			if len(candidateImageParts) == maxImageParts {
				return candidateImageParts, nil
			}
		}
	}

	return candidateImageParts, nil
}

func searchDeciderConversation(
	conversation []chatMessage,
	loadedConfig config,
	configuredModel string,
) ([]chatMessage, error) {
	contentOptions, err := messageContentOptionsForModel(loadedConfig, configuredModel)
	if err != nil {
		return nil, fmt.Errorf(
			"build content options for %q: %w",
			configuredModel,
			err,
		)
	}

	sanitizedConversation := make([]chatMessage, len(conversation))

	for index, message := range conversation {
		sanitizedContent, err := sanitizeMessageContentForModel(message.Content, contentOptions)
		if err != nil {
			return nil, fmt.Errorf("sanitize message %d: %w", index, err)
		}

		sanitizedConversation[index] = chatMessage{
			Role:    message.Role,
			Content: sanitizedContent,
		}
	}

	return sanitizedConversation, nil
}

func sanitizeMessageContentForModel(
	content any,
	options messageContentOptions,
) (any, error) {
	switch typedContent := content.(type) {
	case nil:
		return "", nil
	case string:
		return typedContent, nil
	case []contentPart:
		filteredContent := filterContentPartsForOptions(typedContent, options)
		if !contentPartsContainNonText(filteredContent) {
			return contentPartsText(filteredContent), nil
		}

		clonedContent := make([]contentPart, len(filteredContent))
		copy(clonedContent, filteredContent)

		return clonedContent, nil
	default:
		return nil, fmt.Errorf("unsupported message content type %T: %w", content, os.ErrInvalid)
	}
}

func latestUserImageURLSet(conversation []chatMessage) (map[string]struct{}, error) {
	index, err := latestUserMessageIndex(conversation)
	if err != nil {
		return nil, err
	}

	imageURLSet := make(map[string]struct{})

	parts, ok := conversation[index].Content.([]contentPart)
	if !ok {
		return imageURLSet, nil
	}

	for _, part := range parts {
		partType, _ := part["type"].(string)
		if partType != contentTypeImageURL {
			continue
		}

		imageURL, imageErr := contentPartImageURL(part)
		if imageErr != nil {
			return nil, imageErr
		}

		imageURLSet[imageURL] = struct{}{}
	}

	return imageURLSet, nil
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

func contentPartsContainNonText(parts []contentPart) bool {
	for _, part := range parts {
		partType, _ := part["type"].(string)
		if partType != contentTypeText {
			return true
		}
	}

	return false
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
	client chatCompletionClient,
	request chatCompletionRequest,
) (string, error) {
	var responseText strings.Builder

	err := client.streamChatCompletion(ctx, request, func(delta streamDelta) error {
		responseText.WriteString(delta.Content)

		return nil
	})
	if err != nil {
		return "", err
	}

	return responseText.String(), nil
}

func parseSearchDecision(responseText string) (searchDecision, error) {
	trimmedResponse := trimCodeFence(responseText)

	var decision searchDecision

	err := json.Unmarshal([]byte(trimmedResponse), &decision)
	if err != nil {
		return searchDecision{}, fmt.Errorf("decode search decision JSON: %w", err)
	}

	if !decision.NeedsSearch {
		decision.Queries = nil

		return decision, nil
	}

	decision.Queries = normalizeSearchQueries(decision.Queries)
	if len(decision.Queries) == 0 {
		return searchDecision{}, fmt.Errorf("missing search queries: %w", os.ErrInvalid)
	}

	return decision, nil
}

func trimCodeFence(text string) string {
	trimmedText := strings.TrimSpace(text)
	trimmedText = strings.TrimPrefix(trimmedText, "```json")
	trimmedText = strings.TrimPrefix(trimmedText, "```")
	trimmedText = strings.TrimSuffix(trimmedText, "```")

	return strings.TrimSpace(trimmedText)
}

func normalizeSearchQueries(queries []string) []string {
	seenQueries := make(map[string]struct{}, len(queries))
	normalizedQueries := make([]string, 0, len(queries))

	for _, query := range queries {
		trimmedQuery := strings.TrimSpace(query)
		if trimmedQuery == "" {
			continue
		}

		foldedQuery := strings.ToLower(trimmedQuery)
		if _, ok := seenQueries[foldedQuery]; ok {
			continue
		}

		seenQueries[foldedQuery] = struct{}{}

		normalizedQueries = append(normalizedQueries, trimmedQuery)

		if len(normalizedQueries) == maxSearchQueries {
			break
		}
	}

	return normalizedQueries
}

func formatSearchSourcesMessage(metadata *searchMetadata) string {
	if metadata == nil {
		return "No web search sources available."
	}

	var builder strings.Builder

	builder.WriteString("Search queries:\n")

	for index, query := range metadata.Queries {
		_, _ = fmt.Fprintf(&builder, "%d. %s\n", index+1, query)
	}

	for _, result := range metadata.Results {
		builder.WriteString("\n")
		_, _ = fmt.Fprintf(&builder, "Sources for %q:\n", result.Query)

		sources := extractSearchSources(result.Text)
		if len(sources) == 0 {
			builder.WriteString("No source URLs were parsed from the search response.\n")

			continue
		}

		for index, source := range sources[:minInt(len(sources), maxSourcesPerQuery)] {
			_, _ = fmt.Fprintf(&builder, "%d. %s\n", index+1, source.Title)
			_, _ = fmt.Fprintf(&builder, "   %s\n", source.URL)
		}
	}

	message := strings.TrimSpace(builder.String())
	if runeCount(message) <= showSourcesMessageMaxLength {
		return message
	}

	truncatedMessage := truncateRunes(message, showSourcesMessageMaxLength-len("\n\n... truncated"))

	return strings.TrimSpace(truncatedMessage) + "\n\n... truncated"
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
	_ config,
	queries []string,
) ([]webSearchResult, error) {
	searchContext, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()

	return searchQueriesConcurrently(searchContext, cancel, queries, client.searchQuery)
}

func (client tavilySearchClient) search(
	ctx context.Context,
	loadedConfig config,
	queries []string,
) ([]webSearchResult, error) {
	apiKeys := loadedConfig.WebSearch.Tavily.apiKeysForAttempts()
	if len(apiKeys) == 0 {
		return nil, fmt.Errorf("tavily fallback is not configured: %w", os.ErrNotExist)
	}

	searchContext, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()

	return searchQueriesConcurrently(searchContext, cancel, queries, func(
		queryContext context.Context,
		query string,
	) (webSearchResult, error) {
		return client.searchQuery(queryContext, apiKeys, query)
	})
}

func searchQueriesConcurrently(
	ctx context.Context,
	cancel context.CancelFunc,
	queries []string,
	searchQuery func(context.Context, string) (webSearchResult, error),
) ([]webSearchResult, error) {
	results := make([]webSearchResult, len(queries))
	errorChannel := make(chan error, len(queries))

	var waitGroup sync.WaitGroup

	for index, query := range queries {
		waitGroup.Add(1)

		go func(index int, query string) {
			defer waitGroup.Done()

			result, err := searchQuery(ctx, query)
			if err != nil {
				cancel()

				errorChannel <- err

				return
			}

			results[index] = result
		}(index, query)
	}

	waitGroup.Wait()
	close(errorChannel)

	if err, ok := <-errorChannel; ok {
		return nil, err
	}

	return results, nil
}

func (client exaSearchClient) searchQuery(
	ctx context.Context,
	query string,
) (webSearchResult, error) {
	implementation := new(mcp.Implementation)
	implementation.Name = "llmcord-go"
	implementation.Version = "1.0.0"

	searchClient := mcp.NewClient(implementation, nil)

	transport := new(mcp.StreamableClientTransport)
	transport.Endpoint = client.endpoint
	transport.HTTPClient = client.httpClient
	transport.MaxRetries = -1
	transport.DisableStandaloneSSE = true

	session, err := searchClient.Connect(ctx, transport, nil)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("connect to Exa MCP: %w", err)
	}

	defer func() {
		_ = session.Close()
	}()

	params := new(mcp.CallToolParams)
	params.Name = exaSearchToolName
	params.Arguments = map[string]any{
		"query": query,
	}

	result, err := session.CallTool(ctx, params)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("call Exa MCP search tool for %q: %w", query, err)
	}

	resultText := mcpResultText(result)
	if result.IsError {
		return webSearchResult{}, fmt.Errorf("%w for %q: %s", errExaSearchTool, query, resultText)
	}

	return webSearchResult{
		Query: query,
		Text:  resultText,
	}, nil
}

func (client tavilySearchClient) searchQuery(
	ctx context.Context,
	apiKeys []string,
	query string,
) (webSearchResult, error) {
	attemptErrors := make([]error, 0, len(apiKeys))

	for index, apiKey := range apiKeys {
		result, err := client.searchQueryOnce(ctx, query, apiKey)
		if err == nil {
			return result, nil
		}

		attemptErrors = append(attemptErrors, err)
		if !shouldRetryWithNextAPIKey(err) || index == len(apiKeys)-1 {
			if len(attemptErrors) == 1 {
				return webSearchResult{}, err
			}

			if !shouldRetryWithNextAPIKey(err) {
				return webSearchResult{}, err
			}

			return webSearchResult{}, fmt.Errorf(
				"all configured Tavily API keys failed for %q: %w",
				query,
				errors.Join(attemptErrors...),
			)
		}
	}

	return webSearchResult{}, fmt.Errorf("missing Tavily API key attempt for %q: %w", query, os.ErrInvalid)
}

func (client tavilySearchClient) searchQueryOnce(
	ctx context.Context,
	query string,
	apiKey string,
) (webSearchResult, error) {
	requestBody := tavilySearchRequest{
		Query:             query,
		SearchDepth:       "advanced",
		MaxResults:        maxSourcesPerQuery,
		IncludeRawContent: "text",
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

	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	httpRequest.Header.Set("Content-Type", "application/json")

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
		Text:  formatTavilySearchResultText(response.Results),
	}, nil
}

func formatTavilySearchResultText(results []tavilySearchResponseResult) string {
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

		snippet := formatTavilyMultilineField("Text", result.Content)
		if snippet != "" {
			lines = append(lines, snippet)
		}

		rawContent := formatTavilyMultilineField("Raw Content", result.RawContent)
		if rawContent != "" {
			lines = append(lines, rawContent)
		}

		if len(lines) == 0 {
			continue
		}

		formattedResults = append(formattedResults, strings.Join(lines, "\n"))
	}

	return strings.Join(formattedResults, "\n\n")
}

func formatTavilyMultilineField(label string, value string) string {
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

func (settings webSearchConfig) providersInOrder() (webSearchProviderKind, webSearchProviderKind) {
	if settings.PrimaryProvider == webSearchProviderKindTavily {
		return webSearchProviderKindTavily, webSearchProviderKindMCP
	}

	return webSearchProviderKindMCP, webSearchProviderKindTavily
}

func (provider webSearchProviderKind) displayName() string {
	switch provider {
	case webSearchProviderKindTavily:
		return "Tavily"
	case webSearchProviderKindMCP:
		return "Exa MCP"
	default:
		return string(provider)
	}
}

func mcpResultText(result *mcp.CallToolResult) string {
	textParts := make([]string, 0, len(result.Content))

	for _, content := range result.Content {
		textContent, ok := content.(*mcp.TextContent)
		if !ok {
			continue
		}

		if strings.TrimSpace(textContent.Text) == "" {
			continue
		}

		textParts = append(textParts, textContent.Text)
	}

	return joinNonEmpty(textParts)
}
