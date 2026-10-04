package providers

import (
	"strings"
	"testing"
)

func TestWebSearchToolDescriptionIncludesUserInstruction(t *testing.T) {
	t.Parallel()

	tool := WebSearchTool(0)

	for _, want := range []string{
		"Always search the web if the user told you to, like 'search the web' " +
			"or something similar, unless web search is absolutely not needed.",
		"one query per item",
		"never combine items into one query",
	} {
		if !strings.Contains(tool.Description, want) {
			t.Fatalf("web_search tool description missing required instruction %q: got %q", want, tool.Description)
		}
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
	for _, want := range []string{
		"character-for-character",
		"Determine the most uncensored of big-pickle",
	} {
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
		"One string per item in the user's list",
		"character-for-character including hyphens, digits, dots",
		"big-pickle uncensored, fledge-alpha uncensored",
		"A query with 2+ item names in it is always wrong",
		"never merge, drop, or rename an item",
	} {
		if !strings.Contains(queriesDescription, want) {
			t.Fatalf("web_search queries description missing %q: got %q", want, queriesDescription)
		}
	}
}
