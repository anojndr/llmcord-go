package main

import (
	"context"
	"strings"
	"testing"
)

func assertURLAugmentationIgnoresPDFOnlyURLs(
	t *testing.T,
	pdfURL string,
	augment func(context.Context, []chatMessage, string) ([]chatMessage, []string, error),
) {
	t.Helper()

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: strings.Join([]string{
				"<@123>: summarize the report",
				pdfContentOpenTag,
				"Extracted text:",
				pdfURL,
				pdfContentCloseTag,
			}, "\n"),
		},
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

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if content != conversation[0].Content {
		t.Fatalf("unexpected conversation content: %q", content)
	}
}
