package providers

import (
	"regexp"
	"slices"
	"strings"

	"llmcord-go/internal/searchtypes"
)

// Bridge source appendices are trailing "Sources\n1. ..." blocks that
// OpenAI-compatible bridge providers (e.g. grok-to-openai,
// perplexity-to-openai) append to assistant text when an include_sources
// flag is set, carrying the search citations that back the "Show Sources"
// button. The streamed answer is rendered without the appendix, and the
// appendix is parsed into search metadata at finalization.

var (
	bridgeSourceAppendixNumberedLinePattern = regexp.MustCompile(`^(?:\d+[\.\)]|\[\d+\]:?|[\-\*\+])\s+(.*)$`)
	bridgeSourceAppendixMarkdownLinkPattern = regexp.MustCompile(`^\[(.+?)\]\((https?://[^\s)]+)\)(.*)$`)
	bridgeSourceAppendixTitleURLPattern     = regexp.MustCompile(`^(.+?)\s*[:\-\(]\s*<?(https?://[^\s>)]+)>?\)?(.*)$`)
	bridgeSourceAppendixBareURLPattern      = regexp.MustCompile(`^<?(https?://[^\s>)]+)>?\s*(.*)$`)
	bridgeSourceAppendixInlineQueryPattern  = regexp.MustCompile("`([^`]+)`")
)

const (
	bridgeSourceAppendixNumberedMatchParts     = 2
	bridgeSourceAppendixMarkdownLinkMatchParts = 4
	bridgeSourceAppendixTitleURLMatchParts     = 4
	bridgeSourceAppendixBareURLMatchParts      = 3
	doubleNewlineSeparatorLength               = 2
)

func sourceAppendixHeaderPrefixesList() []string {
	return []string{
		"sources",
		"source urls",
		"references",
		"citations",
	}
}

// FinalizeBridgeSourceAppendixAnswer strips a trailing bridge source
// appendix from an answer and parses it into search metadata. When the
// request already carries web-search metadata, only the cleaned text is
// returned so existing search results keep feeding the Show Sources button.
func FinalizeBridgeSourceAppendixAnswer(
	answerText string,
	existingMetadata *searchtypes.SearchMetadata,
) (string, *searchtypes.SearchMetadata) {
	cleanedAnswerText, attribution, ok := parseBridgeSourceAppendix(answerText)
	if !ok {
		return answerText, nil
	}

	if searchtypes.SearchMetadataHasWebSources(existingMetadata) {
		return cleanedAnswerText, nil
	}

	return cleanedAnswerText, bridgeSourceAttributionSearchMetadata(attribution)
}

// StreamingBridgeSourceAppendixVisibleText returns the answer text that
// should be visible while streaming: everything before the bridge source
// appendix. The appendix itself is deferred until finalization.
func StreamingBridgeSourceAppendixVisibleText(answerText string) string {
	appendixStart, ok := bridgeSourceAppendixStart(answerText)
	if !ok {
		return answerText
	}

	return strings.TrimRight(answerText[:appendixStart], "\r\n")
}

func normalizedSourceAppendixHeader(line string) string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimLeft(trimmed, "#")
	trimmed = strings.TrimSpace(trimmed)
	trimmed = strings.Trim(trimmed, "*_:")
	trimmed = strings.TrimSpace(trimmed)

	return strings.ToLower(trimmed)
}

func isSourceAppendixHeaderLine(line string) bool {
	return slices.Contains(sourceAppendixHeaderPrefixesList(), normalizedSourceAppendixHeader(line))
}

func isSourceAppendixHeaderPartial(line string) bool {
	partialHeader := normalizedSourceAppendixHeader(line)
	if partialHeader == "" {
		return true
	}

	for _, prefix := range sourceAppendixHeaderPrefixesList() {
		if strings.HasPrefix(prefix, partialHeader) {
			return true
		}
	}

	return false
}

func isSearchQueriesHeaderLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}

	trimmed = strings.TrimLeft(trimmed, "#")
	trimmed = strings.TrimSpace(trimmed)
	trimmed = strings.Trim(trimmed, "*_:")
	trimmed = strings.TrimSpace(trimmed)

	lower := strings.ToLower(trimmed)

	return lower == "search queries" || lower == "search query" || lower == "queries"
}

func bridgeSourceAppendixStart(answerText string) (int, bool) {
	if answerText == "" {
		return 0, false
	}

	normalized, originalOffsets := normalizeNewlinesWithOriginalOffsets(answerText)
	lastIdx := -1
	idx := 0

	for {
		nextSep := strings.Index(normalized[idx:], "\n\n")
		if nextSep < 0 {
			break
		}

		pos := idx + nextSep
		afterSep := normalized[pos+doubleNewlineSeparatorLength:]

		firstLine, _, _ := strings.Cut(afterSep, "\n")
		if isSourceAppendixHeaderLine(firstLine) {
			lastIdx = pos
		}

		idx = pos + 1
	}

	if lastIdx >= 0 {
		return originalOffsets[lastIdx], true
	}

	firstLine, _, _ := strings.Cut(normalized, "\n")
	if isSourceAppendixHeaderLine(firstLine) {
		return 0, true
	}

	if lastDoubleNewline := strings.LastIndex(normalized, "\n\n"); lastDoubleNewline >= 0 {
		tail := normalized[lastDoubleNewline+doubleNewlineSeparatorLength:]
		if !strings.Contains(tail, "\n") && isSourceAppendixHeaderPartial(tail) {
			return originalOffsets[lastDoubleNewline], true
		}
	} else if !strings.Contains(normalized, "\n") && isSourceAppendixHeaderPartial(normalized) {
		return 0, true
	}

	if strings.HasSuffix(normalized, "\n") {
		return originalOffsets[len(normalized)-1], true
	}

	return 0, false
}

func normalizeNewlinesWithOriginalOffsets(text string) (string, []int) {
	var normalized strings.Builder

	normalized.Grow(len(text))
	originalOffsets := make([]int, 1, len(text)+1)

	for index := 0; index < len(text); index++ {
		if text[index] == '\r' {
			if index+1 < len(text) && text[index+1] == '\n' {
				index++
			}

			normalized.WriteByte('\n')
		} else {
			normalized.WriteByte(text[index])
		}

		originalOffsets = append(originalOffsets, index+1)
	}

	return normalized.String(), originalOffsets
}

type bridgeSourceAttribution struct {
	Sources       []bridgeSourceAttributionSource
	SearchQueries []string
}

type bridgeSourceAttributionSource struct {
	Title         string
	URL           string
	SearchQueries []string
}

func parseBridgeSourceAppendix(
	answerText string,
) (string, *bridgeSourceAttribution, bool) {
	normalizedAnswerText := strings.ReplaceAll(answerText, "\r\n", "\n")

	appendixStart, ok := bridgeSourceAppendixStart(normalizedAnswerText)
	if !ok {
		return answerText, nil, false
	}

	cleanedAnswerText := strings.TrimSpace(normalizedAnswerText[:appendixStart])

	appendix := strings.TrimLeft(normalizedAnswerText[appendixStart:], "\n")

	firstLine, restOfAppendix, _ := strings.Cut(appendix, "\n")
	if !isSourceAppendixHeaderLine(firstLine) {
		return answerText, nil, false
	}

	sourcesSection := restOfAppendix
	queriesSection := ""

	lines := strings.Split(restOfAppendix, "\n")
	for i, line := range lines {
		if isSearchQueriesHeaderLine(line) {
			sourcesSection = strings.Join(lines[:i], "\n")
			queriesSection = strings.Join(lines[i+1:], "\n")

			break
		}
	}

	attribution := &bridgeSourceAttribution{
		Sources:       parseBridgeSourcesSection(sourcesSection),
		SearchQueries: parseBridgeQueriesSection(queriesSection),
	}

	if len(attribution.Sources) == 0 && len(attribution.SearchQueries) == 0 {
		return answerText, nil, false
	}

	return cleanedAnswerText, attribution, true
}

func parseBridgeSourcesSection(section string) []bridgeSourceAttributionSource {
	lines := strings.Split(strings.TrimSpace(section), "\n")
	sources := make([]bridgeSourceAttributionSource, 0, len(lines))

	for _, line := range lines {
		lineText := line
		if match := bridgeSourceAppendixNumberedLinePattern.FindStringSubmatch(
			strings.TrimSpace(line),
		); len(match) == bridgeSourceAppendixNumberedMatchParts {
			lineText = strings.TrimSpace(match[1])
		}

		source, parsed := parseBridgeSourceLine(lineText)
		if !parsed {
			continue
		}

		sources = append(sources, source)
	}

	return sources
}

func parseBridgeQueriesSection(section string) []string {
	if strings.TrimSpace(section) == "" {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(section), "\n")
	queries := make([]string, 0, len(lines))

	for _, line := range lines {
		lineText := line
		if match := bridgeSourceAppendixNumberedLinePattern.FindStringSubmatch(
			strings.TrimSpace(line),
		); len(match) == bridgeSourceAppendixNumberedMatchParts {
			lineText = strings.TrimSpace(match[1])
		}

		queries = append(queries, parseBridgeQueryList(lineText)...)
	}

	return normalizeSearchQueries(queries)
}

func parseBridgeSourceLine(line string) (bridgeSourceAttributionSource, bool) {
	var emptySource bridgeSourceAttributionSource

	trimmedLine := strings.TrimSpace(line)
	if trimmedLine == "" {
		return emptySource, false
	}

	var title, rawURL, remainder string

	if match := bridgeSourceAppendixMarkdownLinkPattern.FindStringSubmatch(
		trimmedLine,
	); len(match) == bridgeSourceAppendixMarkdownLinkMatchParts {
		title = strings.TrimSpace(match[1])
		rawURL = strings.TrimSpace(match[2])
		remainder = match[3]
	} else if match := bridgeSourceAppendixTitleURLPattern.FindStringSubmatch(
		trimmedLine,
	); len(match) == bridgeSourceAppendixTitleURLMatchParts {
		title = strings.TrimSpace(match[1])
		rawURL = strings.TrimSpace(match[2])
		remainder = match[3]
	} else if match := bridgeSourceAppendixBareURLPattern.FindStringSubmatch(
		trimmedLine,
	); len(match) == bridgeSourceAppendixBareURLMatchParts {
		rawURL = strings.TrimSpace(match[1])
		title = rawURL
		remainder = match[2]
	} else {
		return emptySource, false
	}

	title = strings.Trim(title, "`\"'")

	source, ok := normalizeBridgeSourceAttributionSource(bridgeSourceAttributionSource{
		Title:         title,
		URL:           rawURL,
		SearchQueries: parseBridgeSourceQueries(remainder),
	})
	if !ok {
		return emptySource, false
	}

	return source, true
}

func parseBridgeSourceQueries(remainder string) []string {
	_, queryText, found := strings.Cut(remainder, " via ")
	if !found {
		return nil
	}

	return parseBridgeQueryList(queryText)
}

func parseBridgeQueryList(text string) []string {
	queryMatches := bridgeSourceAppendixInlineQueryPattern.FindAllStringSubmatch(text, -1)
	if len(queryMatches) > 0 {
		queries := make([]string, 0, len(queryMatches))
		for _, match := range queryMatches {
			if len(match) != bridgeSourceAppendixNumberedMatchParts {
				continue
			}

			queries = append(queries, match[1])
		}

		return normalizeSearchQueries(queries)
	}

	trimmedText := strings.Trim(strings.TrimSpace(text), "`")
	if trimmedText == "" {
		return nil
	}

	return normalizeSearchQueries(strings.Split(trimmedText, ";"))
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
	}

	return normalizedQueries
}

func bridgeSourceAttributionSearchMetadata(
	attribution *bridgeSourceAttribution,
) *searchtypes.SearchMetadata {
	if attribution == nil {
		return nil
	}

	queries := normalizeSearchQueries(attribution.SearchQueries)
	querySources := make(map[string][]searchtypes.SearchSource, len(queries))
	seenURLsByQuery := make(map[string]map[string]struct{}, len(queries))

	for _, query := range queries {
		querySources[query] = nil
		seenURLsByQuery[query] = make(map[string]struct{})
	}

	unscopedSources := make([]searchtypes.SearchSource, 0, len(attribution.Sources))
	seenUnscopedURLs := make(map[string]struct{}, len(attribution.Sources))

	for _, rawSource := range attribution.Sources {
		normalizedSource, ok := normalizeBridgeSourceAttributionSource(rawSource)
		if !ok {
			continue
		}

		source := bridgeSourceAttributionSearchSource(normalizedSource)

		sourceQueries := normalizedSource.SearchQueries
		if len(sourceQueries) == 0 {
			unscopedSources = appendBridgeSourceIfUnique(unscopedSources, seenUnscopedURLs, source)

			continue
		}

		for _, query := range sourceQueries {
			if _, ok := querySources[query]; !ok {
				queries = append(queries, query)
				querySources[query] = nil
				seenURLsByQuery[query] = make(map[string]struct{})
			}

			querySources[query] = appendBridgeSourceIfUnique(
				querySources[query],
				seenURLsByQuery[query],
				source,
			)
		}
	}

	results := make([]searchtypes.WebSearchResult, 0, len(queries)+1)

	for _, query := range queries {
		sources := querySources[query]
		if len(sources) == 0 {
			continue
		}

		results = append(results, searchtypes.WebSearchResult{
			Query: query,
			Text:  bridgeSourceAppendixResultText(sources),
		})
	}

	if len(unscopedSources) > 0 {
		results = append(results, searchtypes.WebSearchResult{
			Query: "",
			Text:  bridgeSourceAppendixResultText(unscopedSources),
		})
	}

	if len(queries) == 0 && len(results) == 0 {
		return nil
	}

	maxURLs := len(attribution.Sources)
	if maxURLs == 0 {
		for _, result := range results {
			sourceCount := len(searchtypes.ExtractSearchSources(result.Text))
			if sourceCount > maxURLs {
				maxURLs = sourceCount
			}
		}
	}

	return &searchtypes.SearchMetadata{
		Queries:             queries,
		Results:             results,
		MaxURLs:             maxURLs,
		VisualSearchSources: nil,
	}
}

func normalizeBridgeSourceAttributionSource(
	source bridgeSourceAttributionSource,
) (bridgeSourceAttributionSource, bool) {
	var emptySource bridgeSourceAttributionSource

	source.URL = strings.TrimSpace(source.URL)
	source.Title = strings.TrimSpace(source.Title)
	source.SearchQueries = normalizeSearchQueries(source.SearchQueries)

	if source.URL == "" {
		return emptySource, false
	}

	return source, true
}

func bridgeSourceAttributionSearchSource(source bridgeSourceAttributionSource) searchtypes.SearchSource {
	return searchtypes.SearchSource{
		Title: source.Title,
		URL:   source.URL,
	}
}

func appendBridgeSourceIfUnique(
	sources []searchtypes.SearchSource,
	seenURLs map[string]struct{},
	source searchtypes.SearchSource,
) []searchtypes.SearchSource {
	foldedURL := strings.ToLower(strings.TrimSpace(source.URL))
	if foldedURL == "" {
		return sources
	}

	if _, ok := seenURLs[foldedURL]; ok {
		return sources
	}

	seenURLs[foldedURL] = struct{}{}

	return append(sources, source)
}

func bridgeSourceAppendixResultText(sources []searchtypes.SearchSource) string {
	var builder strings.Builder

	for index, source := range sources {
		if index > 0 {
			builder.WriteString("\n\n")
		}

		title := strings.TrimSpace(source.Title)
		if title != "" {
			builder.WriteString("Title: ")
			builder.WriteString(title)
			builder.WriteString("\n")
		}

		builder.WriteString("URL: ")
		builder.WriteString(strings.TrimSpace(source.URL))
		builder.WriteString("\n")
	}

	return builder.String()
}
