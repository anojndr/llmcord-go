package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseOpenAIHTTPErrorResponseBuildsFriendlyUsageLimit(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(2 * time.Minute).Unix()
	responseBody := fmt.Appendf(nil,
		`{"error":{"code":"usage_limit_reached","plan_type":"PLUS","resets_at":%d}}`,
		resetAt,
	)

	errorInfo := parseOpenAIHTTPErrorResponse(
		httpStatusTooManyRequests,
		"",
		http.Header{},
		responseBody,
		true,
	)

	if !strings.Contains(errorInfo.FriendlyMessage, "usage limit") {
		t.Fatalf("unexpected friendly usage limit message: %#v", errorInfo)
	}

	if errorInfo.RetryDelay <= 0 {
		t.Fatalf("expected retry delay to be populated: %#v", errorInfo)
	}

	if errorInfo.Message != errorInfo.FriendlyMessage {
		t.Fatalf("expected friendly message to be surfaced: %#v", errorInfo)
	}
}

func TestParseOpenAIHTTPErrorResponseFallsBackToStatusText(t *testing.T) {
	t.Parallel()

	errorInfo := parseOpenAIHTTPErrorResponse(
		http.StatusBadGateway,
		"bad gateway",
		http.Header{},
		nil,
		false,
	)

	if errorInfo.Message != "bad gateway" {
		t.Fatalf("unexpected fallback error message: %#v", errorInfo)
	}
}
