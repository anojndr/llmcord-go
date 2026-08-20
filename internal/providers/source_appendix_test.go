package providers

import (
	"strings"
	"testing"

	"llmcord-go/internal/searchtypes"
)

func TestFinalizeBridgeSourceAppendixAnswerParsesSourcesAndStripsAppendix(t *testing.T) {
	t.Parallel()

	answerText := "Answer paragraph.\n\nSources\n" +
		"1. [Example Source](https://example.com/source) (example.com/source) via `latest ai news`\n" +
		"2. [Another Source](https://example.com/other) (example.com/other)\n\n" +
		"Search Queries\n" +
		"1. `latest ai news`\n"

	cleanedText, metadata := FinalizeBridgeSourceAppendixAnswer(answerText, nil)

	if cleanedText != "Answer paragraph." {
		t.Fatalf("unexpected cleaned answer text: %q", cleanedText)
	}

	if metadata == nil {
		t.Fatal("expected parsed bridge search metadata")
	}

	if len(metadata.Queries) != 1 || metadata.Queries[0] != "latest ai news" {
		t.Fatalf("unexpected parsed queries: %#v", metadata.Queries)
	}

	if len(metadata.Results) != 2 {
		t.Fatalf("unexpected parsed result groups: %#v", metadata.Results)
	}

	firstResultSources := searchtypes.ExtractSearchSources(metadata.Results[0].Text)
	if len(firstResultSources) != 1 || firstResultSources[0].URL != "https://example.com/source" {
		t.Fatalf("unexpected scoped source parsing: %#v", firstResultSources)
	}

	secondResultSources := searchtypes.ExtractSearchSources(metadata.Results[1].Text)
	if len(secondResultSources) != 1 || secondResultSources[0].URL != "https://example.com/other" {
		t.Fatalf("unexpected unscoped source parsing: %#v", secondResultSources)
	}
}

func TestFinalizeBridgeSourceAppendixAnswerParsesVariousAppendixFormats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		answerText   string
		expectedText string
		expectedURL  string
	}{
		{
			name:         "markdown h3 header with bracketed index",
			answerText:   "Main answer content.\n\n### Sources:\n[1] [Claude Reference](https://docs.anthropic.com/claude)",
			expectedText: "Main answer content.",
			expectedURL:  "https://docs.anthropic.com/claude",
		},
		{
			name:         "bold references header with hyphen bullet and title colon url",
			answerText:   "Main answer content.\n\n**References:**\n- DeepSeek Docs: https://docs.deepseek.com/api",
			expectedText: "Main answer content.",
			expectedURL:  "https://docs.deepseek.com/api",
		},
		{
			name:         "citations header with asterisk bullet and title dash url",
			answerText:   "Main answer content.\n\nCitations:\n* Llama Info - https://llama.meta.com/info",
			expectedText: "Main answer content.",
			expectedURL:  "https://llama.meta.com/info",
		},
		{
			name:         "source urls header with bare url",
			answerText:   "Main answer content.\n\nSource URLs:\n1. <https://example.org/bare-link>",
			expectedText: "Main answer content.",
			expectedURL:  "https://example.org/bare-link",
		},
		{
			name: "markdown link with via query and host suffix",
			answerText: "Main answer content.\n\nSources\n" +
				"1. [Example Source](https://example.com/source) (example.com/source) via `latest ai news`\n",
			expectedText: "Main answer content.",
			expectedURL:  "https://example.com/source",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cleanedText, metadata := FinalizeBridgeSourceAppendixAnswer(testCase.answerText, nil)
			if cleanedText != testCase.expectedText {
				t.Fatalf("cleaned text mismatch: got %q, want %q", cleanedText, testCase.expectedText)
			}

			if metadata == nil {
				t.Fatalf("expected non-nil search metadata for %s", testCase.name)
			}

			if len(metadata.Results) == 0 {
				t.Fatalf("expected search metadata results for %s", testCase.name)
			}

			sources := searchtypes.ExtractSearchSources(metadata.Results[0].Text)
			if len(sources) != 1 || sources[0].URL != testCase.expectedURL {
				t.Fatalf("unexpected source URL for %s: %#v (expected %q)", testCase.name, sources, testCase.expectedURL)
			}
		})
	}
}

func TestFinalizeBridgeSourceAppendixAnswerDeduplicatesSources(t *testing.T) {
	t.Parallel()

	answerText := "Answer paragraph.\n\nSources\n" +
		"1. [Example Source](https://example.com/source) via `latest ai news`\n" +
		"2. [Example Source Again](https://example.com/source) via `latest ai news`\n"

	cleanedText, metadata := FinalizeBridgeSourceAppendixAnswer(answerText, nil)

	if cleanedText != "Answer paragraph." {
		t.Fatalf("unexpected cleaned answer text: %q", cleanedText)
	}

	if metadata == nil {
		t.Fatal("expected parsed bridge search metadata")
	}

	if len(metadata.Results) != 1 {
		t.Fatalf("unexpected parsed result groups: %#v", metadata.Results)
	}

	sources := searchtypes.ExtractSearchSources(metadata.Results[0].Text)
	if len(sources) != 1 || sources[0].URL != "https://example.com/source" {
		t.Fatalf("expected deduplicated source: %#v", sources)
	}
}

func TestFinalizeBridgeSourceAppendixAnswerTreatsAppendixOnlyAsEmptyAnswer(t *testing.T) {
	t.Parallel()

	answerText := "\n\nSources\n" +
		"1. [Example Source](https://example.com/source) via `latest ai news`\n\n" +
		"Search Queries\n" +
		"1. `latest ai news`\n"

	cleanedText, metadata := FinalizeBridgeSourceAppendixAnswer(answerText, nil)

	if cleanedText != "" {
		t.Fatalf("expected empty cleaned answer text for appendix-only response, got %q", cleanedText)
	}

	if metadata == nil {
		t.Fatal("expected parsed bridge search metadata for appendix-only response")
	}

	sources := searchtypes.ExtractSearchSources(metadata.Results[0].Text)
	if len(sources) != 1 || sources[0].URL != "https://example.com/source" {
		t.Fatalf("unexpected source parsing for appendix-only response: %#v", sources)
	}
}

func TestFinalizeBridgeSourceAppendixAnswerKeepsExistingSearchMetadata(t *testing.T) {
	t.Parallel()

	existingMetadata := &searchtypes.SearchMetadata{
		Queries: []string{"existing query"},
		Results: []searchtypes.WebSearchResult{{
			Query: "existing query",
			Text:  "Title: Existing Source\nURL: https://example.com/existing\n",
		}},
		MaxURLs:             1,
		VisualSearchSources: nil,
	}

	answerText := "Answer paragraph.\n\nSources\n" +
		"1. [Example Source](https://example.com/source) via `latest ai news`\n"

	cleanedText, metadata := FinalizeBridgeSourceAppendixAnswer(answerText, existingMetadata)

	if cleanedText != "Answer paragraph." {
		t.Fatalf("unexpected cleaned answer text: %q", cleanedText)
	}

	if metadata != nil {
		t.Fatalf("expected existing web search metadata to win: %#v", metadata)
	}
}

func TestFinalizeBridgeSourceAppendixAnswerLeavesNonAppendixTextUntouched(t *testing.T) {
	t.Parallel()

	answerText := "Answer paragraph.\n\nThe summary mentions several sources.\n1. Not an appendix."

	cleanedText, metadata := FinalizeBridgeSourceAppendixAnswer(answerText, nil)

	if cleanedText != answerText {
		t.Fatalf("unexpected cleaned answer text: %q", cleanedText)
	}

	if metadata != nil {
		t.Fatalf("unexpected parsed metadata: %#v", metadata)
	}
}

func TestStreamingBridgeSourceAppendixVisibleTextHidesAppendix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		answer   string
		expected string
	}{
		{
			name:     "complete appendix heading is hidden",
			answer:   "Answer paragraph.\n\nSources\n1. [Example Source](https://example.com/source)",
			expected: "Answer paragraph.",
		},
		{
			name:     "partial appendix heading is hidden",
			answer:   "Answer paragraph.\n\nSo",
			expected: "Answer paragraph.",
		},
		{
			name:     "split paragraph separator is hidden",
			answer:   "Answer paragraph.\n",
			expected: "Answer paragraph.",
		},
		{
			name:     "partial markdown appendix heading is hidden",
			answer:   "Answer paragraph.\n\n### ",
			expected: "Answer paragraph.",
		},
		{
			name:     "complete bold appendix heading is hidden",
			answer:   "Answer paragraph.\n\n**References:**",
			expected: "Answer paragraph.",
		},
		{
			name:     "crlf appendix heading uses original offsets",
			answer:   "First line.\r\nSecond line.\r\n\r\n### Sources:",
			expected: "First line.\r\nSecond line.",
		},
		{
			name:     "overlapping crlf separators hide the appendix",
			answer:   "First line.\r\nSecond line.\r\n\r\n\r\n### Sources:",
			expected: "First line.\r\nSecond line.",
		},
		{
			name:     "non appendix text stays visible",
			answer:   "Answer paragraph.\n\nSummary",
			expected: "Answer paragraph.\n\nSummary",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := StreamingBridgeSourceAppendixVisibleText(testCase.answer)
			if got != testCase.expected {
				t.Fatalf("unexpected visible answer text: got %q want %q", got, testCase.expected)
			}
		})
	}
}

func TestStreamingBridgeSourceAppendixVisibleTextNeverRegressesAcrossChunks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		answer   string
		expected string
	}{
		{
			name:     "plain header",
			answer:   "Answer paragraph.\n\nSources\n1. [Example](https://example.com)",
			expected: "Answer paragraph.",
		},
		{
			name:     "markdown header",
			answer:   "Answer paragraph.\n\n### Sources:\n1. [Example](https://example.com)",
			expected: "Answer paragraph.",
		},
		{
			name:     "bold header",
			answer:   "Answer paragraph.\n\n**References:**\n- Example: https://example.com",
			expected: "Answer paragraph.",
		},
		{
			name:     "crlf header",
			answer:   "First line.\r\nSecond line.\r\n\r\nSource URLs\r\n1. https://example.com",
			expected: "First line.\r\nSecond line.",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			previousVisible := ""

			for end := 1; end <= len(testCase.answer); end++ {
				visible := StreamingBridgeSourceAppendixVisibleText(testCase.answer[:end])
				if !strings.HasPrefix(visible, previousVisible) {
					t.Fatalf(
						"visibility regressed at byte %d: previous %q current %q",
						end,
						previousVisible,
						visible,
					)
				}

				previousVisible = visible
			}

			if previousVisible != testCase.expected {
				t.Fatalf("unexpected final visible text: got %q want %q", previousVisible, testCase.expected)
			}
		})
	}
}
