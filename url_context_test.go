package main

import (
	"context"
	"strings"
	"testing"
)

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
