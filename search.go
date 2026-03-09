package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	exaSearchToolName    = "web_search_exa"
	searchWarningText    = "Warning: web search unavailable"
	messageRoleUser      = "user"
	contentTypeImageURL  = "image_url"
	contentTypeText      = "text"
	searchAnswerTemplate = `Answer the user query based on the web search results.

User query:
%s

Web search results:
%s`
	searchDeciderPrompt = `Decide whether the user's request requires a web search.

Return valid JSON only, with no extra text.

If no web search is needed, return:
{"needs_search": false}

If a web search is needed, return:
{"needs_search": true, "queries": ["..."]}

Rules:
- Return false if the request can be handled from general knowledge, reasoning,
  or the provided conversation/media alone.
- Return true if the request needs current, changeable, external, niche, or verification-oriented information.
- Generate only the minimum number of queries needed.
- Use one query if multiple would be redundant.
- Split into multiple queries only for distinct facts.
- For follow-ups, rewrite using context from prior turns.
- For image-based requests, query the specific objects/text shown, not vague descriptions.
- Output JSON only.

Examples:
- "latest news" -> {"needs_search": true, "queries": ["latest news"]}
- "release date and context window of gpt 5.2" ->
  {"needs_search": true, "queries": ["gpt 5.2 release date", "gpt 5.2 context window"]}
- Prior: "Daniel Radcliffe is gay" Follow-up: "verify this" ->
  {"needs_search": true, "queries": ["is Daniel Radcliffe gay?"]}
- Image contains apple, orange, cat; user says
  "search everything mentioned in the image" ->
  {"needs_search": true, "queries": ["apple", "orange", "cat"]}`
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
	search(ctx context.Context, queries []string) ([]webSearchResult, error)
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

func newExaSearchClient(httpClient *http.Client) exaSearchClient {
	return exaSearchClient{
		endpoint:   defaultExaMCPEndpoint,
		httpClient: httpClient,
	}
}

func (instance *bot) maybeAugmentConversationWithWebSearch(
	ctx context.Context,
	loadedConfig config,
	conversation []chatMessage,
) ([]chatMessage, *searchMetadata, []string, error) {
	decision, err := instance.decideWebSearch(ctx, loadedConfig, conversation)
	if err != nil {
		slog.Warn("decide web search", "error", err)

		return conversation, nil, []string{searchWarningText}, nil
	}

	if !decision.NeedsSearch {
		return conversation, nil, nil, nil
	}

	results, err := instance.webSearch.search(ctx, decision.Queries)
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

func (instance *bot) decideWebSearch(
	ctx context.Context,
	loadedConfig config,
	conversation []chatMessage,
) (searchDecision, error) {
	searchDeciderModel := instance.currentSearchDeciderModelForConfig(loadedConfig)

	searchDeciderMessages, err := searchDeciderConversation(conversation, searchDeciderModel)
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

	responseText, err := collectChatCompletionText(searchContext, instance.openAI, request)
	if err != nil {
		return searchDecision{}, fmt.Errorf("collect search decider response: %w", err)
	}

	decision, err := parseSearchDecision(responseText)
	if err != nil {
		return searchDecision{}, fmt.Errorf("parse search decider response %q: %w", responseText, err)
	}

	return decision, nil
}

func searchDeciderConversation(
	conversation []chatMessage,
	configuredModel string,
) ([]chatMessage, error) {
	allowImages := isVisionModel(configuredModel)
	sanitizedConversation := make([]chatMessage, len(conversation))

	for index, message := range conversation {
		sanitizedContent, err := sanitizeMessageContentForModel(message.Content, allowImages)
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

func sanitizeMessageContentForModel(content any, allowImages bool) (any, error) {
	switch typedContent := content.(type) {
	case nil:
		return "", nil
	case string:
		return typedContent, nil
	case []contentPart:
		if allowImages {
			clonedContent := make([]contentPart, len(typedContent))
			copy(clonedContent, typedContent)

			return clonedContent, nil
		}

		return contentPartsText(typedContent), nil
	default:
		return nil, fmt.Errorf("unsupported message content type %T: %w", content, os.ErrInvalid)
	}
}

func appendWebSearchResultsToConversation(
	conversation []chatMessage,
	formattedResults string,
) ([]chatMessage, error) {
	augmentedConversation := make([]chatMessage, len(conversation))
	copy(augmentedConversation, conversation)

	for index := len(augmentedConversation) - 1; index >= 0; index-- {
		if augmentedConversation[index].Role != messageRoleUser {
			continue
		}

		updatedContent, err := appendWebSearchResultsToMessageContent(
			augmentedConversation[index].Content,
			formattedResults,
		)
		if err != nil {
			return nil, fmt.Errorf("update latest user message: %w", err)
		}

		augmentedConversation[index].Content = updatedContent

		return augmentedConversation, nil
	}

	return nil, fmt.Errorf("find latest user message: %w", os.ErrNotExist)
}

func appendWebSearchResultsToMessageContent(
	content any,
	formattedResults string,
) (any, error) {
	switch typedContent := content.(type) {
	case nil:
		return buildSearchAnswerPrompt("", formattedResults), nil
	case string:
		return buildSearchAnswerPrompt(typedContent, formattedResults), nil
	case []contentPart:
		updatedContent := make([]contentPart, 0, len(typedContent)+1)
		updatedContent = append(updatedContent, contentPart{
			"type": contentTypeText,
			"text": buildSearchAnswerPrompt(contentPartsText(typedContent), formattedResults),
		})

		for _, part := range typedContent {
			if partType, _ := part["type"].(string); partType != contentTypeImageURL {
				continue
			}

			updatedContent = append(updatedContent, part)
		}

		return updatedContent, nil
	default:
		return nil, fmt.Errorf("unsupported message content type %T: %w", content, os.ErrInvalid)
	}
}

func buildSearchAnswerPrompt(userQuery string, formattedResults string) string {
	return fmt.Sprintf(
		searchAnswerTemplate,
		strings.TrimSpace(userQuery),
		strings.TrimSpace(formattedResults),
	)
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
	queries []string,
) ([]webSearchResult, error) {
	searchContext, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()

	results := make([]webSearchResult, len(queries))
	errorChannel := make(chan error, len(queries))

	var waitGroup sync.WaitGroup

	for index, query := range queries {
		waitGroup.Add(1)

		go func(index int, query string) {
			defer waitGroup.Done()

			result, err := client.searchQuery(searchContext, query)
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
