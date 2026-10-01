package providers

import (
	"strings"
	"testing"
)

func TestWebSearchToolDescriptionIncludesUserInstruction(t *testing.T) {
	t.Parallel()

	tool := WebSearchTool(0)

	const want = "Always search the web if the user told you to, like 'search the web' or something similar, unless web search is absolutely not needed."
	if !strings.Contains(tool.Description, want) {
		t.Fatalf("web_search tool description missing required instruction %q: got %q", want, tool.Description)
	}
}

func TestWebSearchToolQueriesRequireVerbatimEntity(t *testing.T) {
	t.Parallel()

	tool := WebSearchTool(0)

	properties, propertiesOK := tool.Parameters["properties"].(map[string]any)
	if !propertiesOK {
		t.Fatalf("unexpected parameters properties: %#v", tool.Parameters["properties"])
	}

	objective, objectiveOK := properties["objective"].(map[string]any)
	if !objectiveOK {
		t.Fatalf("unexpected objective property: %#v", properties["objective"])
	}

	objectiveDescription, _ := objective["description"].(string)
	for _, want := range []string{"verbatim", "never correct or substitute"} {
		if !strings.Contains(objectiveDescription, want) {
			t.Fatalf("web_search objective missing %q: got %q", want, objectiveDescription)
		}
	}

	queries, queriesOK := properties["search_queries"].(map[string]any)
	if !queriesOK {
		t.Fatalf("unexpected search_queries property: %#v", properties["search_queries"])
	}

	queriesDescription, _ := queries["description"].(string)
	for _, want := range []string{
		"Repeat the exact same key entity or topic verbatim in every query",
		"even if the entity looks unreleased, unfamiliar, or misspelled",
		"Pixel 10 Pro XL price Philippines",
		"never 'Pixel 9 Pro XL' or 'Pixel 8 Pro'",
	} {
		if !strings.Contains(queriesDescription, want) {
			t.Fatalf("web_search queries description missing %q: got %q", want, queriesDescription)
		}
	}
}
