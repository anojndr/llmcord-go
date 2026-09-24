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

func TestWebSearchToolAsksForOneCallWithAllQueries(t *testing.T) {
	t.Parallel()

	tool := WebSearchTool(0)

	const wantDescription = "make a single web_search call with ALL of your queries in search_queries"
	if !strings.Contains(tool.Description, wantDescription) {
		t.Fatalf("web_search tool description missing %q: got %q", wantDescription, tool.Description)
	}

	properties, propertiesOK := tool.Parameters["properties"].(map[string]any)
	if !propertiesOK {
		t.Fatalf("unexpected parameters properties: %#v", tool.Parameters["properties"])
	}

	queries, queriesOK := properties["search_queries"].(map[string]any)
	if !queriesOK {
		t.Fatalf("unexpected search_queries property: %#v", properties["search_queries"])
	}

	const wantQueries = "Provide ALL of your keyword queries in this single call"

	queriesDescription, _ := queries["description"].(string)
	if !strings.Contains(queriesDescription, wantQueries) {
		t.Fatalf("search_queries description missing %q: got %q", wantQueries, queriesDescription)
	}
}
