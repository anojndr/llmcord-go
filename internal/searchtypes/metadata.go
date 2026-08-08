package searchtypes

// SearchMetadata captures everything a response learned from web or visual
// search so the rendered reply (and persisted nodes) can surface sources.
type SearchMetadata struct {
	Queries             []string                  `json:"queries"`
	Results             []WebSearchResult         `json:"results"`
	MaxURLs             int                       `json:"max_urls"`
	VisualSearchSources []VisualSearchSourceGroup `json:"visual_search_sources"`
}

// WebSearchResult is the fetched text for one web search query.
type WebSearchResult struct {
	Query string `json:"query"`
	Text  string `json:"text"`
}

// SearchSource is one parsed source line within search result text.
type SearchSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// VisualSearchSourceGroup groups a visual search result's source links.
type VisualSearchSourceGroup struct {
	Label   string         `json:"label"`
	Sources []SearchSource `json:"sources"`
}

// NewSearchMetadata wraps queries and results into a metadata value.
func NewSearchMetadata(queries []string, results []WebSearchResult, maxURLs int) *SearchMetadata {
	return &SearchMetadata{
		Queries:             append([]string(nil), queries...),
		Results:             append([]WebSearchResult(nil), results...),
		MaxURLs:             maxURLs,
		VisualSearchSources: nil,
	}
}

// CloneSearchMetadata deep-copies a metadata value.
func CloneSearchMetadata(metadata *SearchMetadata) *SearchMetadata {
	if metadata == nil {
		return nil
	}

	cloned := NewSearchMetadata(metadata.Queries, metadata.Results, metadata.MaxURLs)
	cloned.VisualSearchSources = cloneVisualSearchSourceGroups(metadata.VisualSearchSources)

	return cloned
}

func cloneVisualSearchSourceGroups(
	sourceGroups []VisualSearchSourceGroup,
) []VisualSearchSourceGroup {
	if len(sourceGroups) == 0 {
		return nil
	}

	clonedGroups := make([]VisualSearchSourceGroup, 0, len(sourceGroups))

	for _, sourceGroup := range sourceGroups {
		clonedGroups = append(clonedGroups, VisualSearchSourceGroup{
			Label:   sourceGroup.Label,
			Sources: append([]SearchSource(nil), sourceGroup.Sources...),
		})
	}

	return clonedGroups
}

// MergeSearchMetadata combines two metadata values, preferring the right
// side's max-urls when set.
func MergeSearchMetadata(left, right *SearchMetadata) *SearchMetadata {
	switch {
	case left == nil:
		return CloneSearchMetadata(right)
	case right == nil:
		return CloneSearchMetadata(left)
	}

	merged := CloneSearchMetadata(left)
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

// SearchMetadataHasWebSources reports whether metadata carries any queries or
// results beyond visual-search-only source groups.
func SearchMetadataHasWebSources(metadata *SearchMetadata) bool {
	return metadata != nil && (len(metadata.Queries) > 0 || len(metadata.Results) > 0)
}

// MaxURLsOrDefault returns the metadata max-urls, falling back to the default.
func (metadata *SearchMetadata) MaxURLsOrDefault(defaultMaxURLs int) int {
	if metadata == nil || metadata.MaxURLs <= 0 {
		return defaultMaxURLs
	}

	return metadata.MaxURLs
}
