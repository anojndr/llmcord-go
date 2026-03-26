package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestValidateDiscordGatewaySucceedsWithJSON(t *testing.T) {
	t.Parallel()

	instance := newDiscordGatewayProbeTestBot(
		t,
		http.StatusOK,
		"application/json",
		`{"url":"wss://gateway.discord.gg/"}`,
	)

	err := instance.validateDiscordGateway(context.Background())
	if err != nil {
		t.Fatalf("validateDiscordGateway(...) error = %v", err)
	}
}

func TestValidateDiscordGatewayReportsHTTPStatusAndBody(t *testing.T) {
	t.Parallel()

	instance := newDiscordGatewayProbeTestBot(
		t,
		http.StatusTooManyRequests,
		"text/plain; charset=utf-8",
		"error code: 1015",
	)

	err := instance.validateDiscordGateway(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	expectedSubstring := `discord gateway probe returned unexpected http status: ` +
		`status 429 Too Many Requests, content type "text/plain; charset=utf-8", ` +
		`body "error code: 1015"`
	if !strings.Contains(err.Error(), expectedSubstring) {
		t.Fatalf("error = %q, want substring %q", err.Error(), expectedSubstring)
	}
}

func TestValidateDiscordGatewayReportsNonJSONBody(t *testing.T) {
	t.Parallel()

	instance := newDiscordGatewayProbeTestBot(
		t,
		http.StatusOK,
		"text/html; charset=utf-8",
		"<html><body>Menu</body></html>",
	)

	err := instance.validateDiscordGateway(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	expectedSubstring := `discord gateway probe returned non-json response: ` +
		`content type "text/html; charset=utf-8", body "<html><body>Menu</body></html>"`
	if !strings.Contains(err.Error(), expectedSubstring) {
		t.Fatalf("error = %q, want substring %q", err.Error(), expectedSubstring)
	}
}

func TestSummarizeHTTPErrorBody(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		body     []byte
		expected string
	}{
		{
			name:     "empty",
			body:     nil,
			expected: "<empty>",
		},
		{
			name:     "collapses whitespace",
			body:     []byte("  error\n\ncode:\t1015  "),
			expected: "error code: 1015",
		},
		{
			name:     "truncates long body",
			body:     []byte(strings.Repeat("a", errorBodySnippetMaxLength+10)),
			expected: strings.Repeat("a", errorBodySnippetMaxLength) + "...",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := summarizeHTTPErrorBody(testCase.body)
			if got != testCase.expected {
				t.Fatalf("summarizeHTTPErrorBody(...) = %q, want %q", got, testCase.expected)
			}
		})
	}
}

func newDiscordGatewayProbeTestBot(
	t *testing.T,
	statusCode int,
	contentType string,
	body string,
) *bot {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.Client = new(http.Client)
	session.Client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", request.Method)
		}

		if request.URL.String() != discordgo.EndpointGateway {
			t.Fatalf("unexpected url: %s", request.URL.String())
		}

		response := new(http.Response)
		response.StatusCode = statusCode
		response.Status = fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode))
		response.Header = make(http.Header)
		response.Header.Set("Content-Type", contentType)
		response.Body = io.NopCloser(strings.NewReader(body))
		response.Request = request

		return response, nil
	})

	instance := new(bot)
	instance.session = session

	return instance
}
