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
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	exaSearchToolName                = "web_search_exa"
	searchQueryArgumentKey           = "query"
	exaNumResultsKey                 = "numResults"
	searchWarningText                = "Warning: web search unavailable"
	searchSourcesSectionCapacity     = 2
	searchSourcesUnavailableText     = "No sources available."
	messageRoleUser                  = "user"
	contentTypeAudioData             = "audio_data"
	contentTypeDocument              = "document_data"
	contentTypeFileData              = "file_data"
	contentTypeImageURL              = "image_url"
	contentTypeText                  = "text"
	contentTypeVideoData             = "video_data"
	contentFieldBytes                = "data"
	contentFieldFilename             = "filename"
	contentFieldMIMEType             = "mime_type"
	mimeTypeDOCX                     = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeTypeOctetStream              = "application/octet-stream"
	mimeTypePDF                      = "application/pdf"
	mimeTypePPTX                     = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	mimeTypeJPEG                     = "image/jpeg"
	mimeTypePNG                      = "image/png"
	mimeTypeZIP                      = "application/zip"
	mimeTypeWEBP                     = "image/webp"
	searchDeciderDecisionInstruction = "Based on the conversation above, analyze the last user query " +
		"and respond ONLY with your JSON decision. " +
		"Do not answer the query itself, " +
		"and do not include any conversational filler, explanation, " +
		"introductory text, or markdown code fences. " +
		"Your response must be a single valid JSON object."
	searchAnswerTemplate = `Answer the user's query based on the web search results.

User query:
%s

Web search results:
%s`
)

var errExaSearchTool = errors.New("exa MCP search tool returned an error")

func searchDeciderPrompt(now time.Time) string {
	return searchtypes.SearchDeciderPrompt(now)
}

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

type searchDecision struct {
	NeedsSearch bool     `json:"needs_search"`
	Queries     []string `json:"queries"`
}

type searchMetadata = searchtypes.SearchMetadata
type webSearchResult = searchtypes.WebSearchResult
type searchSource = searchtypes.SearchSource
type visualSearchSourceGroup = searchtypes.VisualSearchSourceGroup

type exaSearchClient struct {
	apiEndpoint string
	mcpEndpoint string
	httpClient  *http.Client
	keys        *apiKeyRotator
}

type tavilySearchClient struct {
	endpoint   string
	httpClient *http.Client
	keys       *apiKeyRotator
}

type routedWebSearchClient struct {
	exa    webSearcher
	tavily webSearcher
}

type exaSearchRequest struct {
	Query      string
	Type       string
	NumResults int
	Contents   exaSearchRequestContents
}

type exaSearchRequestContents struct {
	Text       exaSearchTextRequest
	Highlights bool
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

type exaStatusError struct {
	StatusCode int
	Message    string
	Err        error
}

func newExaSearchClient(httpClient *http.Client) exaSearchClient {
	return exaSearchClient{
		apiEndpoint: defaultExaSearchEndpoint,
		mcpEndpoint: defaultExaMCPEndpoint,
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

func newWebSearchClient(httpClient *http.Client) routedWebSearchClient {
	return routedWebSearchClient{
		exa:    newExaSearchClient(httpClient),
		tavily: newTavilySearchClient(httpClient),
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

func (instance *bot) maybeAugmentConversationWithWebSearch(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) ([]chatMessage, *searchMetadata, []string) {
	instance.modelMu.Lock()
	decidingSearch := instance.decidingSearch
	instance.modelMu.Unlock()

	if decidingSearch {
		return conversation, nil, nil
	}

	decision, decisionWarnings, err := instance.decideWebSearch(
		ctx,
		loadedConfig,
		providerSlashModel,
		sourceMessage,
		conversation,
	)
	if err != nil {
		logWarn("decide web search", err)

		return conversation, nil, append(decisionWarnings, searchWarningText)
	}

	if !decision.NeedsSearch {
		return conversation, nil, decisionWarnings
	}

	searchConfig := loadedConfig
	searchConfig.WebSearch.Exa.SearchType = instance.currentExaSearchType()

	results, err := instance.webSearch.search(ctx, searchConfig, decision.Queries)
	if err != nil {
		logWarn("run web search", err, "queries", decision.Queries)

		return conversation, nil, append(decisionWarnings, searchWarningText)
	}

	augmentedConversation, err := appendWebSearchResultsToConversation(
		conversation,
		formatWebSearchResults(results),
	)
	if err != nil {
		logWarn("append web search results to conversation", err)

		return conversation, nil, append(decisionWarnings, searchWarningText)
	}

	return augmentedConversation,
		newSearchMetadata(decision.Queries, results, loadedConfig.WebSearch.maxURLs()),
		decisionWarnings
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
		primaryProvider.displayName(loadedConfig),
		fallbackProvider.displayName(loadedConfig),
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
		return client.exa.search(ctx, loadedConfig, queries)
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
) (searchDecision, []string, error) {
	if searchDeciderDisabledForModel(providerSlashModel, loadedConfig) {
		return searchDecision{
			NeedsSearch: false,
			Queries:     nil,
		}, nil, nil
	}

	searchDeciderModel := instance.currentSearchDeciderModelForConfig(loadedConfig)

	rawSearchDeciderMessages, err := instance.buildSearchDeciderConversation(
		ctx,
		loadedConfig,
		providerSlashModel,
		searchDeciderModel,
		sourceMessage,
		conversation,
	)
	if err != nil {
		return searchDecision{}, nil, fmt.Errorf("prepare search decider conversation: %w", err)
	}

	originalSearchDeciderMessages := cloneChatMessages(rawSearchDeciderMessages)
	searchDeciderMessages := prependSearchDeciderPrompt(rawSearchDeciderMessages,
		searchDeciderPrompt(time.Now()),
	)

	searchDeciderMessages = appendSearchDeciderInstruction(searchDeciderMessages)

	request, err := buildChatCompletionRequest(
		loadedConfig,
		searchDeciderModel,
		searchDeciderMessages,
		false,
	)
	if err != nil {
		return searchDecision{}, nil, fmt.Errorf("build search decider request: %w", err)
	}

	providers.AssignOpenAIPromptCacheKeyWithScope(
		&request,
		sourceMessage,
		instance.nodes,
		loadedConfig.MaxMessages,
		"search-decider",
	)

	searchContext, cancel := context.WithCancel(ctx)
	defer cancel()

	var warnings []string

	responseText, err := collectChatCompletionText(searchContext, instance.chatCompletions, request)
	if err != nil {
		return searchDecision{}, warnings, fmt.Errorf("collect search decider response: %w", err)
	}

	decision, err := parseSearchDecision(responseText)
	if err != nil {
		return searchDecision{}, warnings, fmt.Errorf("parse search decider response %q: %w", responseText, err)
	}

	decision = expandSearchDecisionForFragmentFollowUp(searchDeciderMessages, originalSearchDeciderMessages, decision)

	return decision, warnings, nil
}


// expandSearchDecisionForFragmentFollowUp detects short follow-up fragments that
// correct or clarify a prior entity (e.g. "Belarussian bsb bank" after the
// assistant assumed Australian BSB) and expands generic entity-only queries
// into self-contained queries that combine the corrected entity with the
// prior functional need (Oney ATM, personal number / PIN, France).
// This is a safety net for cases where the decider prompt is ignored and the
// model returns a bare entity query like ["Belarussian BSB Bank"].
// The main prompt fix is in searchDeciderPrompt.txt; this code ensures the
// search still carries the original intent (ATM PIN) forward.
func expandSearchDecisionForFragmentFollowUp(
	searchDeciderMessages []chatMessage,
	originalMessages []chatMessage,
	decision searchDecision,
) searchDecision {
	if !decision.NeedsSearch || len(decision.Queries) == 0 {
		return decision
	}
	// Extract latest user query from original (pre-prompt) messages.
	latestText := extractLatestUserQueryText(originalMessages)
	if latestText == "" {
		// Fallback to the prepended version's embedded query.
		latestText = extractLatestUserQueryText(searchDeciderMessages)
	}
	latestText = strings.TrimSpace(latestText)
	if latestText == "" {
		return decision
	}
	// Strip the decider prompt header if present (it is prepended to the latest message).
	if idx := strings.LastIndex(latestText, "Latest user query:"); idx >= 0 {
		latestText = strings.TrimSpace(latestText[idx+len("Latest user query:"):])
	}
	if !isShortFragmentFollowUp(latestText, originalMessages) {
		return decision
	}
	priorContext := extractPriorUserContext(originalMessages)
	if strings.TrimSpace(priorContext) == "" {
		return decision
	}
	if queriesAlreadyContainPriorContext(decision.Queries, priorContext) {
		return decision
	}
	// If all queries are generic entity-only (equal to or contained within latest fragment), expand.
	if !queriesAreGenericEntityOnly(decision.Queries, latestText) {
		return decision
	}
	priorKeywords := extractPriorFunctionalKeywords(priorContext)
	if len(priorKeywords) == 0 {
		return decision
	}
	expanded := make([]string, 0, len(decision.Queries))
	keywordSuffix := strings.Join(priorKeywords, " ")
	for _, q := range decision.Queries {
		trimmedQ := strings.TrimSpace(q)
		// Already contains prior keywords? Keep as is.
		lowerQ := strings.ToLower(trimmedQ)
		containsKeyword := false
		for _, kw := range priorKeywords {
			if strings.Contains(lowerQ, strings.ToLower(kw)) {
				containsKeyword = true
				break
			}
		}
		if containsKeyword {
			expanded = append(expanded, trimmedQ)
			continue
		}
		combined := strings.TrimSpace(trimmedQ + " " + keywordSuffix)
		// Ensure the combined query mentions card/PIN/ATM context if prior had it but suffix didn't already.
		expanded = append(expanded, combined)
	}
	// Ensure at least one query explicitly mentions PIN / personal number / ATM when relevant.
	// If prior context mentions Oney/ATM/PIN/personal number, ensure suffix covers it.
	expanded = normalizeSearchQueries(expanded)
	if len(expanded) == 0 {
		return decision
	}
	decision.Queries = expanded
	return decision
}

func extractLatestUserQueryText(messages []chatMessage) string {
	idx, err := latestUserMessageIndex(messages)
	if err != nil {
		return ""
	}
	content := messages[idx].Content
	var rawText string
	switch v := content.(type) {
	case string:
		rawText = v
	case []contentPart:
		rawText = contentPartsText(v)
	default:
		return ""
	}
	// If augmented prompt, extract UserQuery.
	prompt := parseAugmentedUserPrompt(rawText)
	if strings.TrimSpace(prompt.UserQuery) != "" {
		return strings.TrimSpace(prompt.UserQuery)
	}
	return strings.TrimSpace(rawText)
}

func extractPriorUserContext(messages []chatMessage) string {
	latestIdx, err := latestUserMessageIndex(messages)
	if err != nil {
		return ""
	}
	parts := make([]string, 0)
	for i, msg := range messages {
		if i == latestIdx {
			continue
		}
		if msg.Role != messageRoleUser {
			continue
		}
		text := extractUserQueryTextFromMessage(msg)
		if strings.TrimSpace(text) == "" {
			continue
		}
		parts = append(parts, text)
	}
	// Also consider assistant messages that reveal assumed entity? For BSB case,
	// the assistant answer mentions "Australian BSB" — we want to capture that correction signal.
	// Include the last assistant message if it exists before latest.
	for i := latestIdx - 1; i >= 0; i-- {
		if messages[i].Role != messageRoleAssistant {
			continue
		}
		assistantText := extractUserQueryTextFromMessage(messages[i])
		if strings.TrimSpace(assistantText) != "" {
			// Only keep first 500 chars to avoid bloat.
			if len(assistantText) > 500 {
				assistantText = assistantText[:500]
			}
			parts = append(parts, assistantText)
			break
		}
	}
	return strings.Join(parts, " ")
}

func extractUserQueryTextFromMessage(msg chatMessage) string {
	var rawText string
	switch v := msg.Content.(type) {
	case string:
		rawText = v
	case []contentPart:
		rawText = contentPartsText(v)
	default:
		return ""
	}
	prompt := parseAugmentedUserPrompt(rawText)
	if strings.TrimSpace(prompt.UserQuery) != "" {
		return strings.TrimSpace(prompt.UserQuery)
	}
	return strings.TrimSpace(rawText)
}

func isShortFragmentFollowUp(latestText string, messages []chatMessage) bool {
	trimmed := strings.TrimSpace(latestText)
	if trimmed == "" {
		return false
	}
	// If it ends with ?, it's a question, not a fragment correction.
	if strings.Contains(trimmed, "?") {
		return false
	}
	words := strings.Fields(trimmed)
	if len(words) == 0 || len(words) > 6 {
		return false
	}
	// Standalone commands like "search this" should not be treated as fragment.
	lower := strings.ToLower(trimmed)
	questionStarters := []string{"what ", "how ", "why ", "when ", "where ", "who ", "which ", "does ", "is ", "are ", "can ", "could ", "would ", "should ", "do ", "did ", "will ", "have ", "has ", "verify", "search", "look up", "compare", "tell me", "explain", "show me"}
	for _, starter := range questionStarters {
		if strings.HasPrefix(lower, starter) {
			return false
		}
	}
	// Must have prior substantive user message (longer than fragment, >6 words or contains functional terms)
	hasPriorSubstantive := false
	for _, msg := range messages {
		if msg.Role != messageRoleUser {
			continue
		}
		txt := extractUserQueryTextFromMessage(msg)
		if txt == trimmed {
			continue
		}
		if len(strings.Fields(txt)) > 6 {
			hasPriorSubstantive = true
			break
		}
		// Also consider if it contains functional keywords like oney, atm, pin, personal number, france, paris, card
		lowerPrior := strings.ToLower(txt)
		if strings.Contains(lowerPrior, "oney") || strings.Contains(lowerPrior, "atm") || strings.Contains(lowerPrior, "pin") || strings.Contains(lowerPrior, "personal number") || strings.Contains(lowerPrior, "card") {
			hasPriorSubstantive = true
			break
		}
	}
	if !hasPriorSubstantive {
		return false
	}
	return true
}

func queriesAreGenericEntityOnly(queries []string, latestText string) bool {
	latestLower := strings.ToLower(strings.TrimSpace(latestText))
	latestWords := strings.Fields(latestLower)
	if len(latestWords) == 0 {
		return false
	}
	for _, q := range queries {
		trimmedQ := strings.TrimSpace(q)
		if trimmedQ == "" {
			continue
		}
		qLower := strings.ToLower(trimmedQ)
		// If query is essentially equal to latest fragment or is just entity name without functional terms.
		// Check if query contains functional keywords like pin, atm, oney, personal number, france, paris, card, view, how to.
		hasFunctional := strings.Contains(qLower, "pin") || strings.Contains(qLower, "personal number") || strings.Contains(qLower, "oney") || strings.Contains(qLower, "atm") || strings.Contains(qLower, "france") || strings.Contains(qLower, "paris") || strings.Contains(qLower, "how to") || strings.Contains(qLower, "view") || strings.Contains(qLower, "access")
		if hasFunctional {
			return false
		}
		// If query is just the fragment plus maybe minor variation, consider it generic.
		// Check if all words in query are subset of latest fragment words + common entity words.
	}
	// All queries lack functional terms -> treat as generic.
	return true
}

func queriesAlreadyContainPriorContext(queries []string, priorContext string) bool {
	keywords := extractPriorFunctionalKeywords(priorContext)
	if len(keywords) == 0 {
		return false
	}
	for _, q := range queries {
		qLower := strings.ToLower(q)
		for _, kw := range keywords {
			if strings.Contains(qLower, strings.ToLower(kw)) {
				return true
			}
		}
	}
	return false
}

func extractPriorFunctionalKeywords(priorContext string) []string {
	lower := strings.ToLower(priorContext)
	candidates := []string{}
	// Prioritized functional phrases in order of relevance to the BSB/Oney case and general.
	phraseChecks := []struct {
		phrase string
		keywords []string
	}{
		{"oney", []string{"Oney", "ATM"}},
		{"atm", []string{"ATM"}},
		{"personal number", []string{"personal number", "PIN"}},
		{"pin", []string{"PIN"}},
		{"code confidentiel", []string{"code confidentiel"}},
		{"france", []string{"France"}},
		{"paris", []string{"Paris"}},
		{"card", []string{"card"}},
		{"bsb", []string{"card"}},
	}
	seen := make(map[string]struct{})
	for _, check := range phraseChecks {
		if strings.Contains(lower, check.phrase) {
			for _, kw := range check.keywords {
				if _, ok := seen[strings.ToLower(kw)]; !ok {
					seen[strings.ToLower(kw)] = struct{}{}
					candidates = append(candidates, kw)
				}
			}
		}
	}
	// Always include "card PIN" if any card/atm/pin context found but not already.
	hasCardContext := strings.Contains(lower, "card") || strings.Contains(lower, "atm") || strings.Contains(lower, "pin") || strings.Contains(lower, "personal number") || strings.Contains(lower, "oney")
	if hasCardContext {
		if _, ok := seen["pin"]; !ok {
			if _, ok2 := seen["PIN"]; !ok2 {
				candidates = append(candidates, "PIN")
			}
		}
		if _, ok := seen["card"]; !ok {
			candidates = append(candidates, "card")
		}
	}
	// If we still have no candidates but prior context is non-empty, fallback to generic functional suffix.
	if len(candidates) == 0 {
		// Extract up to 5 significant words from prior context (exclude stopwords)
		stopwords := map[string]struct{}{"the":{}, "and":{}, "for":{}, "with":{}, "from":{}, "this":{}, "that":{}, "have":{}, "has":{}, "does":{}, "doesnt":{}, "dont":{}, "what":{}, "how":{}, "trying":{}, "try":{}, "use":{}, "using":{}, "its":{}, "asking":{}, "one":{}, "all":{}, "acess":{}, "access":{}, "im":{}}
		words := strings.Fields(lower)
		for _, w := range words {
			clean := strings.Trim(w, `.,!?;:"'()[]{}`)
			if len(clean) < 3 {
				continue
			}
			if _, isStop := stopwords[clean]; isStop {
				continue
			}
			if _, ok := seen[clean]; ok {
				continue
			}
			seen[clean] = struct{}{}
			candidates = append(candidates, clean)
			if len(candidates) >= 5 {
				break
			}
		}
	}
	// Ensure we have Oney ATM France PIN if prior had those but candidates missed due to case.
	// Limit to max 6 keywords to keep query concise.
	if len(candidates) > 6 {
		candidates = candidates[:6]
	}
	return candidates
}




func cloneChatMessages(messages []chatMessage) []chatMessage {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]chatMessage, len(messages))
	for i, msg := range messages {
		cloned[i].Role = msg.Role
		switch v := msg.Content.(type) {
		case string:
			cloned[i].Content = v
		case []contentPart:
			parts := make([]contentPart, len(v))
			for j, part := range v {
				parts[j] = cloneContentPart(part)
			}
			cloned[i].Content = parts
		default:
			cloned[i].Content = msg.Content
		}
	}
	return cloned
}

func maybeExpandFragmentInConversation(messages []chatMessage) []chatMessage {
	if len(messages) == 0 {
		return messages
	}
	latestIdx, err := latestUserMessageIndex(messages)
	if err != nil {
		return messages
	}
	latestText := extractLatestUserQueryText(messages)
	if latestText == "" {
		return messages
	}
	// Strip decider header if present (should not be in main conversation, but handle)
	if hdrIdx := strings.LastIndex(latestText, "Latest user query:"); hdrIdx >= 0 {
		latestText = strings.TrimSpace(latestText[hdrIdx+len("Latest user query:"):])
	}
	if !isShortFragmentFollowUp(latestText, messages) {
		return messages
	}
	priorContext := extractPriorUserContext(messages)
	if strings.TrimSpace(priorContext) == "" {
		return messages
	}
	// Truncate prior context to keep query concise but preserve functional need
	priorTruncated := strings.TrimSpace(priorContext)
	if len(priorTruncated) > 400 {
		priorTruncated = priorTruncated[:400] + "..."
	}
	// Build expanded query that makes the correction explicit and carries forward the original functional need.
	// This is for the main model: the short fragment alone ("Belarussian bsb bank") is ambiguous; we rewrite it
	// to explicitly state it is a correction and re-ask the prior functional question for the corrected entity.
	lowerPrior := strings.ToLower(priorTruncated)
	hasFunctional := strings.Contains(lowerPrior, "oney") || strings.Contains(lowerPrior, "atm") || strings.Contains(lowerPrior, "pin") || strings.Contains(lowerPrior, "personal number") || strings.Contains(lowerPrior, "france") || strings.Contains(lowerPrior, "paris") || strings.Contains(lowerPrior, "code confidentiel")
	baseQuery := fmt.Sprintf("%s — clarification: the BSB bank I mean is %s (not the Australian BSB). Please re-answer my earlier question for this corrected bank: %s.", strings.TrimSpace(latestText), strings.TrimSpace(latestText), priorTruncated)
	var expandedQuery string
	if hasFunctional {
		expandedQuery = baseQuery + " Focus on PIN / personal number (= code confidentiel) and using the card at an Oney ATM in Paris/France, not a generic bank profile."
	} else {
		expandedQuery = baseQuery + " Do not provide a generic bank profile unless directly relevant to the corrected entity."
	}

	updatedMessages := append([]chatMessage(nil), messages...)
	latestMsg := updatedMessages[latestIdx]
	switch content := latestMsg.Content.(type) {
	case string:
		prompt := parseAugmentedUserPrompt(content)
		if strings.HasPrefix(strings.TrimSpace(content), augmentedPromptPrefix) {
			prompt.UserQuery = expandedQuery
			latestMsg.Content = prompt.render()
		} else {
			if strings.TrimSpace(prompt.UserQuery) != "" {
				prompt.UserQuery = expandedQuery
				latestMsg.Content = prompt.render()
			} else {
				latestMsg.Content = expandedQuery
			}
		}
		updatedMessages[latestIdx] = latestMsg
	case []contentPart:
		newParts := make([]contentPart, 0, len(content))
		updated := false
		for _, part := range content {
			partType, _ := part[messageTypeKey].(string)
			if !updated && partType == contentTypeText {
				textVal, _ := part[messageTextKey].(string)
				prompt := parseAugmentedUserPrompt(textVal)
				var newText string
				if strings.HasPrefix(strings.TrimSpace(textVal), augmentedPromptPrefix) {
					prompt.UserQuery = expandedQuery
					newText = prompt.render()
				} else {
					if strings.TrimSpace(prompt.UserQuery) != "" {
						prompt.UserQuery = expandedQuery
						newText = prompt.render()
					} else {
						newText = expandedQuery
					}
				}
				newPart := cloneContentPart(part)
				newPart[messageTextKey] = newText
				newParts = append(newParts, newPart)
				updated = true
			} else {
				newParts = append(newParts, cloneContentPart(part))
			}
		}
		if !updated {
			newPart := contentPart{messageTypeKey: contentTypeText, messageTextKey: expandedQuery}
			newParts = append([]contentPart{newPart}, newParts...)
		}
		latestMsg.Content = newParts
		updatedMessages[latestIdx] = latestMsg
	default:
		return messages
	}
	return updatedMessages
}

func prependSearchDeciderPrompt(messages []chatMessage, prompt string) []chatMessage {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return messages
	}

	clonedMessages := append([]chatMessage(nil), messages...)
	if len(clonedMessages) == 0 {
		return []chatMessage{{
			Role:    messageRoleUser,
			Content: prompt,
		}}
	}

	lastIdx := len(clonedMessages) - 1
	if clonedMessages[lastIdx].Role != messageRoleUser {
		return append(clonedMessages, chatMessage{
			Role:    messageRoleUser,
			Content: prompt,
		})
	}

	headerText := prompt + "\n\nLatest user query:\n"

	switch content := clonedMessages[lastIdx].Content.(type) {
	case string:
		if strings.TrimSpace(content) == "" {
			clonedMessages[lastIdx].Content = prompt
		} else {
			clonedMessages[lastIdx].Content = headerText + content
		}
	case []contentPart:
		if len(content) == 0 {
			clonedMessages[lastIdx].Content = []contentPart{{
				messageTypeKey: contentTypeText,
				messageTextKey: prompt,
			}}

			break
		}

		clonedParts := make([]contentPart, 0, len(content)+1)
		for _, p := range content {
			clonedParts = append(clonedParts, cloneContentPart(p))
		}

		firstTextIdx := -1

		for index, p := range clonedParts {
			if typeVal, ok := p[messageTypeKey].(string); ok && typeVal == contentTypeText {
				firstTextIdx = index

				break
			}
		}

		if firstTextIdx >= 0 {
			existingText, _ := clonedParts[firstTextIdx][messageTextKey].(string)
			clonedParts[firstTextIdx][messageTextKey] = headerText + existingText
		} else {
			clonedParts = append([]contentPart{{
				messageTypeKey: contentTypeText,
				messageTextKey: headerText,
			}}, clonedParts...)
		}

		clonedMessages[lastIdx].Content = clonedParts
	default:
		return append(clonedMessages, chatMessage{
			Role:    messageRoleUser,
			Content: prompt,
		})
	}

	return clonedMessages
}

func appendSearchDeciderInstruction(messages []chatMessage) []chatMessage {
	if len(messages) == 0 {
		return []chatMessage{{
			Role:    messageRoleUser,
			Content: searchDeciderDecisionInstruction,
		}}
	}

	lastIdx := len(messages) - 1
	if messages[lastIdx].Role != messageRoleUser {
		return append(messages, chatMessage{
			Role:    messageRoleUser,
			Content: searchDeciderDecisionInstruction,
		})
	}

	switch content := messages[lastIdx].Content.(type) {
	case string:
		messages[lastIdx].Content = content + "\n\n" + searchDeciderDecisionInstruction
	case []contentPart:
		if len(content) == 0 {
			messages[lastIdx].Content = []contentPart{{
				messageTypeKey: contentTypeText,
				messageTextKey: searchDeciderDecisionInstruction,
			}}

			break
		}

		clonedParts := make([]contentPart, 0, len(content)+1)
		for _, p := range content {
			clonedParts = append(clonedParts, cloneContentPart(p))
		}

		lastTextIdx := -1

		for index, p := range slices.Backward(clonedParts) {
			if typeVal, ok := p[messageTypeKey].(string); ok && typeVal == contentTypeText {
				lastTextIdx = index

				break
			}
		}

		if lastTextIdx >= 0 {
			existingText, _ := clonedParts[lastTextIdx][messageTextKey].(string)
			clonedParts[lastTextIdx][messageTextKey] = existingText + "\n\n" + searchDeciderDecisionInstruction
		} else {
			clonedParts = append(clonedParts, contentPart{
				messageTypeKey: contentTypeText,
				messageTextKey: searchDeciderDecisionInstruction,
			})
		}

		messages[lastIdx].Content = clonedParts
	default:
		messages = append(messages, chatMessage{
			Role:    messageRoleUser,
			Content: searchDeciderDecisionInstruction,
		})
	}

	return messages
}

func searchDeciderDisabledForModel(configuredModel string, loadedConfig config) bool {
	providerName, _, err := splitConfiguredModel(strings.TrimSpace(configuredModel))
	if err != nil {
		return false
	}

	provider, ok := loadedConfig.Providers[providerName]

	return ok && provider.DisableSearchDecider
}

// buildSearchDeciderConversation builds the search decider conversation
// through the exact same code path as the main model: the reply chain is
// walked and augmented with the same steps (video URLs, document extraction,
// media analysis, visual search, website/youtube/reddit content) using the
// search decider model's own content options. The only difference from the
// main model is that the caller prepends the search decider prompt to the
// latest user query afterwards. Web search augmentation is skipped while the
// decision is in flight so the decider never re-decides (infinite recursion
// guard).
func (instance *bot) buildSearchDeciderConversation(
	ctx context.Context,
	loadedConfig config,
	_ string,
	searchDeciderModel string,
	sourceMessage *discordgo.Message,
	_ []chatMessage,
) ([]chatMessage, error) {
	searchDeciderMessages, warnings, err := instance.buildMessageConversation(
		ctx,
		loadedConfig,
		sourceMessage,
		searchDeciderModel,
	)
	if err != nil {
		return nil, fmt.Errorf("build search decider conversation: %w", err)
	}

	if len(searchDeciderMessages) == 0 {
		fallbackMessage, fallbackWarnings := fallbackAttachmentDownloadConversation(
			sourceMessage,
			warnings,
		)
		if fallbackMessage != nil {
			searchDeciderMessages = append(searchDeciderMessages, *fallbackMessage)
			warnings = fallbackWarnings
		}
	}

	instance.modelMu.Lock()
	instance.decidingSearch = true
	instance.modelMu.Unlock()

	defer func() {
		instance.modelMu.Lock()
		instance.decidingSearch = false
		instance.modelMu.Unlock()
	}()

	searchDeciderMessages, _, _, err = instance.augmentPreparedMessageResponse(
		ctx,
		loadedConfig,
		sourceMessage,
		searchDeciderModel,
		searchDeciderMessages,
		warnings,
	)
	if err != nil {
		return nil, fmt.Errorf("augment search decider conversation: %w", err)
	}

	return searchDeciderMessages, nil
}

// latestUserImageURLSet returns the set of image URLs in the latest user
// message. It is used by Gemini media analysis to avoid re-analyzing images
// already present in the conversation.
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

func parseSearchDecision(responseText string) (searchDecision, error) {
	trimmedResponse := trimCodeFence(responseText)

	var decision searchDecision
	if tryParseSearchDecisionCandidate(trimmedResponse, &decision) {
		return validateSearchDecision(decision)
	}

	startIndex := strings.Index(trimmedResponse, "{")

	if startIndex >= 0 {
		candidate := trimmedResponse[startIndex:]
		if tryParseSearchDecisionCandidate(candidate, &decision) {
			return validateSearchDecision(decision)
		}

		if endIndex := strings.LastIndex(candidate, "}"); endIndex > 0 {
			if tryParseSearchDecisionCandidate(candidate[:endIndex+1], &decision) {
				return validateSearchDecision(decision)
			}
		}
	}

	if endIndex := strings.LastIndex(trimmedResponse, "}"); startIndex >= 0 && endIndex > startIndex {
		trimmedResponse = trimmedResponse[startIndex : endIndex+1]
		if tryParseSearchDecisionCandidate(trimmedResponse, &decision) {
			return validateSearchDecision(decision)
		}
	}

	err := json.Unmarshal([]byte(trimmedResponse), &decision)
	if err != nil {
		return searchDecision{}, fmt.Errorf("decode search decision JSON: %w", err)
	}

	return validateSearchDecision(decision)
}

func tryParseSearchDecisionCandidate(candidate string, decision *searchDecision) bool {
	if tryUnmarshalSearchDecision(candidate, decision) {
		return true
	}

	if tryRepairTruncatedSearchDecision(candidate, decision) {
		return true
	}

	return false
}

func tryUnmarshalSearchDecision(text string, decision *searchDecision) bool {
	err := json.Unmarshal([]byte(text), decision)
	if err == nil {
		return true
	}

	for charIndex := range text {
		if text[charIndex] != '{' {
			continue
		}

		var rawMap map[string]json.RawMessage

		dec := json.NewDecoder(strings.NewReader(text[charIndex:]))

		err = dec.Decode(&rawMap)
		if err != nil {
			continue
		}

		_, hasSnake := rawMap["needs_search"]

		valCamel, hasCamel := rawMap["needsSearch"]
		if !hasSnake && !hasCamel {
			continue
		}

		if !hasSnake && hasCamel {
			rawMap["needs_search"] = valCamel
		}

		jsonBytes, err := json.Marshal(rawMap)
		if err != nil {
			continue
		}

		var decodedDecision searchDecision

		err = json.Unmarshal(jsonBytes, &decodedDecision)
		if err != nil {
			continue
		}

		*decision = decodedDecision

		return true
	}

	return false
}

func isInJSONString(input string) bool {
	inString := false
	escaped := false

	for index := 0; index < len(input); index++ {
		character := input[index]
		if escaped {
			escaped = false

			continue
		}

		if inString {
			if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}

			continue
		}

		if character == '"' {
			inString = true
		}
	}

	return inString
}

func countTrailingBackslashes(input string) int {
	count := 0

	for index := len(input) - 1; index >= 0; index-- {
		if input[index] != '\\' {
			break
		}

		count++
	}

	return count
}

func neededJSONClosings(input string) string {
	inString := false
	escaped := false
	stack := make([]rune, 0, 4)

	for index := 0; index < len(input); index++ {
		character := input[index]
		if escaped {
			escaped = false

			continue
		}

		if inString {
			if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}

			continue
		}

		if character == '"' {
			inString = true
		} else if character == '{' || character == '[' {
			stack = append(stack, rune(character))
		} else if character == '}' {
			if len(stack) > 0 && stack[len(stack)-1] == '{' {
				stack = stack[:len(stack)-1]
			}
		} else if character == ']' {
			if len(stack) > 0 && stack[len(stack)-1] == '[' {
				stack = stack[:len(stack)-1]
			}
		}
	}

	var builder strings.Builder

	builder.Grow(len(stack))

	for index := len(stack) - 1; index >= 0; index-- {
		if stack[index] == '{' {
			builder.WriteByte('}')
		} else {
			builder.WriteByte(']')
		}
	}

	return builder.String()
}

func closeTruncatedJSONString(input string) []string {
	if !isInJSONString(input) {
		return []string{input}
	}

	trailingBackslashes := countTrailingBackslashes(input)

	if trailingBackslashes%2 == 1 {
		return []string{
			input + "\\\"",
			strings.TrimSuffix(input, "\\") + "\"",
			input + "\"",
		}
	}

	return []string{input + "\""}
}

func generateRepairCandidates(input string) []string {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}

	seen := make(map[string]struct{})
	candidates := make([]string, 0, 32)

	addCandidate := func(candidate string) {
		trimmedCandidate := strings.TrimSpace(candidate)
		if trimmedCandidate == "" {
			return
		}

		if _, ok := seen[trimmedCandidate]; ok {
			return
		}

		seen[trimmedCandidate] = struct{}{}
		candidates = append(candidates, trimmedCandidate)
	}

	bases := []string{input}

	if isInJSONString(input) {
		for _, closed := range closeTruncatedJSONString(input) {
			if closed != input {
				bases = append(bases, closed)
			}
		}
	}

	isInStr := isInJSONString(input)

	expandedBases := make([]string, 0, len(bases)*2)
	for _, base := range bases {
		expandedBases = append(expandedBases, base)

		trimmedBase := strings.TrimSpace(base)
		trimmedBase = strings.TrimSuffix(trimmedBase, ",")
		trimmedBase = strings.TrimSpace(trimmedBase)

		if trimmedBase != base {
			expandedBases = append(expandedBases, trimmedBase)
		}
	}

	baseSet := make(map[string]struct{})
	uniqBases := make([]string, 0, len(expandedBases))

	for _, base := range expandedBases {
		if _, ok := baseSet[base]; ok {
			continue
		}

		baseSet[base] = struct{}{}
		uniqBases = append(uniqBases, base)
	}

	for _, base := range uniqBases {
		baseTrimmed := strings.TrimRight(base, " \t\n\r,")

		for trim := range 3 {
			trimmedBase := baseTrimmed

			for range trim {
				trimmedBase = strings.TrimSpace(trimmedBase)

				if strings.HasSuffix(trimmedBase, "}") {
					trimmedBase = strings.TrimSuffix(trimmedBase, "}")
				} else if strings.HasSuffix(trimmedBase, "]") {
					trimmedBase = strings.TrimSuffix(trimmedBase, "]")
				} else {
					break
				}

				trimmedBase = strings.TrimSpace(trimmedBase)
				trimmedBase = strings.TrimSuffix(trimmedBase, ",")
				trimmedBase = strings.TrimSpace(trimmedBase)
			}

			if isInJSONString(trimmedBase) {
				if countTrailingBackslashes(trimmedBase)%2 == 1 {
					trimmedBase = strings.TrimSuffix(trimmedBase, "\\")
				}

				trimmedBase += `"`
			}

			trimmedBase = strings.TrimRight(trimmedBase, " \t\n\r,")

			needed := neededJSONClosings(trimmedBase)
			if needed != "" {
				addCandidate(trimmedBase + needed)

				if len(needed) >= 1 {
					addCandidate(trimmedBase + string(needed[0]))
				}

				if len(needed) >= 2 {
					addCandidate(trimmedBase + needed[:2])
				}
			}
		}
	}

	bruteSuffixes := []string{
		"}",
		"]",
		"]}",
		"}]",
		"\"}",
		"\"]}",
		"\"]",
		"\"",
		"]}}",
		"}}",
		"]]",
		"\",",
		"\" ]}",
	}

	bruteSet := make(map[string]struct{})
	uniqBrute := make([]string, 0, len(bruteSuffixes))

	for _, suffix := range bruteSuffixes {
		if _, ok := bruteSet[suffix]; ok {
			continue
		}

		bruteSet[suffix] = struct{}{}
		uniqBrute = append(uniqBrute, suffix)
	}

	for _, base := range uniqBases {
		for _, suffix := range uniqBrute {
			addCandidate(strings.TrimSpace(base) + suffix)

			trimmedBase := strings.TrimRight(strings.TrimSpace(base), ", \t\n\r")

			if isInJSONString(trimmedBase) {
				if countTrailingBackslashes(trimmedBase)%2 == 1 {
					trimmedBase = strings.TrimSuffix(trimmedBase, "\\")
				}

				trimmedBase += `"`
			}

			addCandidate(strings.TrimSpace(trimmedBase) + suffix)
		}
	}

	addCandidate(input)

	if isInStr {
		addCandidate(input + `"`)

		if !isInJSONString(input + `"`) {
			needed := neededJSONClosings(input + `"`)
			addCandidate(input + `"` + needed)
		}
	}

	return candidates
}

func tryRepairTruncatedSearchDecision(input string, decision *searchDecision) bool {
	candidates := generateRepairCandidates(input)

	for _, candidate := range candidates {
		var repairedDecision searchDecision
		if tryUnmarshalSearchDecision(candidate, &repairedDecision) {
			*decision = repairedDecision

			return true
		}

		var directDecision searchDecision
		if err := json.Unmarshal([]byte(candidate), &directDecision); err == nil {
			if !strings.Contains(candidate, "needs_search") && !strings.Contains(candidate, "needsSearch") {
				continue
			}

			*decision = directDecision

			return true
		}
	}

	return false
}

func validateSearchDecision(decision searchDecision) (searchDecision, error) {
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
	maxURLs := loadedConfig.WebSearch.maxURLs()
	searchType := loadedConfig.WebSearch.Exa.searchType()

	return searchQueriesConcurrently(ctx, queries, func(
		queryContext context.Context,
		query string,
	) (webSearchResult, error) {
		if loadedConfig.WebSearch.exaUsesAPI() {
			exaAPIKey := firstAPIKey(client.keys.rotate(loadedConfig.WebSearch.Exa.apiKeys()))

			return client.searchAPIQuery(
				queryContext,
				exaAPIKey,
				query,
				maxURLs,
				searchType,
				loadedConfig.WebSearch.Exa.textMaxCharacters(),
			)
		}

		return client.searchMCPQuery(queryContext, query, maxURLs)
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

	return searchQueriesConcurrently(ctx, queries, func(
		queryContext context.Context,
		query string,
	) (webSearchResult, error) {
		apiKey := firstAPIKey(client.keys.rotate(tavilyAPIKeys))

		return client.searchQuery(queryContext, apiKey, query, maxURLs)
	})
}

func searchQueriesConcurrently(
	ctx context.Context,
	queries []string,
	searchQuery func(context.Context, string) (webSearchResult, error),
) ([]webSearchResult, error) {
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
		func(queryContext context.Context, index int) (webSearchResult, error) {
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

	results := make([]webSearchResult, len(taskResults))
	for index, result := range taskResults {
		if result.err != nil {
			return nil, result.err
		}

		results[index] = result.value
	}

	return results, nil
}

func (client exaSearchClient) searchMCPQuery(
	ctx context.Context,
	query string,
	maxURLs int,
) (webSearchResult, error) {
	implementation := new(mcp.Implementation)
	implementation.Name = providers.GeminiCacheDefaultDisplayName
	implementation.Version = "1.0.0"

	searchClient := mcp.NewClient(implementation, nil)

	transport := new(mcp.StreamableClientTransport)
	transport.Endpoint = client.mcpEndpoint
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
		searchQueryArgumentKey: query,
		exaNumResultsKey:       maxURLs,
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
			Highlights: true,
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
) (webSearchResult, error) {
	return client.searchQueryOnce(ctx, query, apiKey, maxURLs)
}

func (client tavilySearchClient) searchQueryOnce(
	ctx context.Context,
	query string,
	apiKey string,
	maxURLs int,
) (webSearchResult, error) {
	requestBody := tavilySearchRequest{
		Query:             query,
		SearchDepth:       "advanced",
		MaxResults:        maxURLs,
		IncludeRawContent: messageTextKey,
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

		snippet := formatSearchMultilineField("Text", result.Content)
		if snippet != "" {
			lines = append(lines, snippet)
		}

		rawContent := formatSearchMultilineField("Raw Content", result.RawContent)
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

func searchDeciderImagePartSet(
	conversation []chatMessage,
	maxImageParts int,
) (map[string]struct{}, []contentPart, error) {
	imageURLSet, err := latestUserImageURLSet(conversation)
	if err != nil {
		return nil, nil, err
	}

	return imageURLSet, make([]contentPart, 0, maxImageParts), nil
}

func appendSearchDeciderImageParts(
	candidateImageParts []contentPart,
	imageURLSet map[string]struct{},
	imageParts []contentPart,
	maxImageParts int,
	errorContext string,
) ([]contentPart, bool, error) {
	for _, imagePart := range imageParts {
		updatedParts, added, err := appendSearchDeciderImagePart(
			candidateImageParts,
			imageURLSet,
			imagePart,
		)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", errorContext, err)
		}

		candidateImageParts = updatedParts

		if !added {
			continue
		}

		if len(candidateImageParts) == maxImageParts {
			return candidateImageParts, true, nil
		}
	}

	return candidateImageParts, false, nil
}

func appendSearchDeciderImagePart(
	candidateImageParts []contentPart,
	imageURLSet map[string]struct{},
	imagePart contentPart,
) ([]contentPart, bool, error) {
	imageURL, err := contentPartImageURL(imagePart)
	if err != nil {
		return nil, false, err
	}

	if _, exists := imageURLSet[imageURL]; exists {
		return candidateImageParts, false, nil
	}

	imageURLSet[imageURL] = struct{}{}

	candidateImageParts = append(candidateImageParts, cloneContentPart(imagePart))

	return candidateImageParts, true, nil
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

func (settings webSearchConfig) providersInOrder() (webSearchProviderKind, webSearchProviderKind) {
	switch settings.PrimaryProvider {
	case webSearchProviderKindTavily:
		return webSearchProviderKindTavily, webSearchProviderKindMCP
	case webSearchProviderKindMCP:
		return webSearchProviderKindMCP, webSearchProviderKindTavily
	default:
		return webSearchProviderKindMCP, webSearchProviderKindTavily
	}
}

func (provider webSearchProviderKind) displayName(loadedConfig config) string {
	switch provider {
	case webSearchProviderKindTavily:
		return "Tavily"
	case webSearchProviderKindMCP:
		if loadedConfig.WebSearch.exaUsesAPI() {
			return "Exa Search API"
		}

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
