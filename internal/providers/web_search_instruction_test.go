package providers

import (
	"strings"
	"testing"
)

func TestWebSearchToolDescriptionIncludesUserInstruction(t *testing.T) {
	t.Parallel()

	tool := WebSearchTool(0)

	const want = "Always search the web if the user told you to, like 'search the web' or something similar."
	if !strings.Contains(tool.Description, want) {
		t.Fatalf("web_search tool description missing required instruction %q: got %q", want, tool.Description)
	}
}
