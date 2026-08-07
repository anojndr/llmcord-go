package main

import (
	"errors"
)

// errContextWindowLimitExceeded is returned when a request's estimated token
// count reaches the model's context window. The request is never truncated or
// summarized: the pipeline reports the limit back to the user instead.
var errContextWindowLimitExceeded = errors.New("that query would exceed the model context window")

// checkContextWindowLimit returns an error when the request's messages do not
// fit in the model's context window (estimated with Codex's byte ratio). A
// request with no configured window is never limited.
func checkContextWindowLimit(request chatCompletionRequest) error {
	if request.ContextWindow <= 0 {
		return nil
	}

	if estimateChatCompletionRequestTokens(request) >= request.ContextWindow {
		return errContextWindowLimitExceeded
	}

	return nil
}
