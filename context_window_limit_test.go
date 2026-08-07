package main

import (
	"errors"
	"strings"
	"testing"
)

func TestContextWindowLimitExceededErrorIsUserFacing(t *testing.T) {
	t.Parallel()

	text := userFacingResponseError(errContextWindowLimitExceeded)
	if !strings.Contains(text, "context window") {
		t.Fatalf("expected the user-facing error to mention the context window: %q", text)
	}
}

func TestCheckContextWindowAllowsWithinWindow(t *testing.T) {
	t.Parallel()

	request := emptyChatCompletionRequest()
	request.ConfiguredModel = "openai/main-model"
	request.ContextWindow = 1_000
	request.Messages = []chatMessage{
		{Role: messageRoleSystem, Content: "Always be helpful."},
		{Role: messageRoleUser, Content: "Short query inside the window."},
		{Role: messageRoleAssistant, Content: "Earlier answer."},
		{Role: messageRoleUser, Content: "Latest question."},
	}

	limitErr := checkContextWindowLimit(request)
	if limitErr != nil {
		t.Fatalf("unexpected context window limit exceeded error: %v", limitErr)
	}
}

func TestCheckContextWindowExceedsWithinWindowFails(t *testing.T) {
	t.Parallel()

	request := emptyChatCompletionRequest()
	request.ConfiguredModel = "openai/main-model"
	request.ContextWindow = 1_000
	request.Messages = []chatMessage{
		{Role: messageRoleUser, Content: strings.Repeat("a", 1_000_000)},
	}

	limitErr := checkContextWindowLimit(request)
	if !errors.Is(limitErr, errContextWindowLimitExceeded) {
		t.Fatalf("expected a context window limit exceeded error: %v", limitErr)
	}
}

func TestCheckContextWindowWithNoConfiguredWindowIsUnlimited(t *testing.T) {
	t.Parallel()

	request := emptyChatCompletionRequest()
	request.ConfiguredModel = "openai/main-model"
	request.Messages = []chatMessage{
		{Role: messageRoleUser, Content: strings.Repeat("a", 1_000_000)},
	}

	limitErr := checkContextWindowLimit(request)
	if limitErr != nil {
		t.Fatalf("unexpected context window limit exceeded error: %v", limitErr)
	}
}
