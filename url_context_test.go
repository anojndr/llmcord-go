package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

var errURLContextFetch = errors.New("url content fetch failed")

func TestFetchConcurrentURLContentPreservesOrderAndPartialSuccess(t *testing.T) {
	t.Parallel()

	urls := []string{"first", "failed", "third"}

	results, warnings := fetchConcurrentURLContent(
		t.Context(),
		urls,
		func(_ context.Context, rawURL string) (string, error) {
			if rawURL == "failed" {
				return "", errURLContextFetch
			}

			return strings.ToUpper(rawURL), nil
		},
		"fetch test URL",
		"some URLs failed",
	)

	if !slices.Equal(results, []string{"FIRST", "THIRD"}) {
		t.Fatalf("results = %#v", results)
	}

	if !slices.Equal(warnings, []string{"some URLs failed"}) {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func assertURLAugmentationIgnoresDocumentOnlyURLs(
	t *testing.T,
	documentURL string,
	augment func(context.Context, []chatMessage, string) ([]chatMessage, []string, error),
) {
	t.Helper()

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: "<@123>: summarize the report",
		},
	}

	conversation, err := appendDocumentContentToConversation(
		conversation,
		strings.Join([]string{
			ooxmlContentOpenTag,
			"Extracted text:",
			documentURL,
			ooxmlContentCloseTag,
		}, "\n"),
	)
	if err != nil {
		t.Fatalf("append document content to conversation: %v", err)
	}

	augmentedConversation, warnings, err := augment(
		context.Background(),
		conversation,
		"<@123>: summarize the report",
	)
	if err != nil {
		t.Fatalf("augment conversation: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	expectedContent, contentOK := conversation[0].Content.(string)
	if !contentOK {
		t.Fatalf("unexpected baseline content type: %T", conversation[0].Content)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if content != expectedContent {
		t.Fatalf("unexpected conversation content: %q", content)
	}
}
