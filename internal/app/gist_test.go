package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGistClientCreateGistPostsJSONAndReturnsURL(t *testing.T) {
	t.Parallel()

	const (
		apiKey       = "gist-test-token"
		description  = "llmcord-go reply"
		filename     = "llmcord-go reply.md"
		gistPagePath = "/abcdef123"
	)

	var serverURL string

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		assertGistCreateRequest(t, request)
		assertGistAuthHeader(t, request, "Bearer "+apiKey)

		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read gist create body: %v", err)
		}

		var createRequest gistCreateRequest

		err = json.Unmarshal(body, &createRequest)
		if err != nil {
			t.Fatalf("decode gist create body: %v", err)
		}

		if createRequest.Description != description {
			t.Fatalf("unexpected gist description: %q", createRequest.Description)
		}

		if createRequest.Public {
			t.Fatalf("expected private gist, got public")
		}

		file, ok := createRequest.Files[filename]
		if !ok {
			t.Fatalf("missing gist file %q: %#v", filename, createRequest.Files)
		}

		if file.Content != testAssistantReply {
			t.Fatalf("unexpected gist file content: %q", file.Content)
		}

		responseWriter.WriteHeader(http.StatusCreated)

		responseBody := `{"html_url":` + strconv.Quote(serverURL+gistPagePath) + `,"id":"1","public":false}`

		_, err = io.WriteString(responseWriter, responseBody)
		if err != nil {
			t.Fatalf("write gist response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	serverURL = server.URL

	client := newGistClient(
		server.Client(),
		server.URL,
		[]string{apiKey},
		description,
		filename,
		false,
	)

	gistURL, err := client.createGist(context.Background(), testAssistantReply)
	if err != nil {
		t.Fatalf("create gist: %v", err)
	}

	if gistURL != server.URL+gistPagePath {
		t.Fatalf("unexpected gist url: got %q want %q", gistURL, server.URL+gistPagePath)
	}
}

func assertGistCreateRequest(t *testing.T, request *http.Request) {
	t.Helper()

	if request == nil {
		t.Fatal("nil gist create request")
	}

	if request.Method != http.MethodPost {
		t.Fatalf("unexpected gist request: %s %s", request.Method, request.URL.Path)
	}

	if request.URL.Path != "/" {
		t.Fatalf("unexpected gist path: %q", request.URL.Path)
	}

	if request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected Content-Type header: %q", request.Header.Get("Content-Type"))
	}

	if request.Header.Get("Accept") != "application/vnd.github+json" {
		t.Fatalf("unexpected Accept header: %q", request.Header.Get("Accept"))
	}

	if request.Header.Get("X-Github-Api-Version") != "2022-11-28" {
		t.Fatalf("unexpected GitHub API version header: %q", request.Header.Get("X-Github-Api-Version"))
	}
}

func assertGistAuthHeader(t *testing.T, request *http.Request, want string) {
	t.Helper()

	if request.Header.Get("Authorization") != want {
		t.Fatalf(
			"unexpected Authorization header: got %q want %q",
			request.Header.Get("Authorization"),
			want,
		)
	}
}

func TestGistClientCreateGistOmitsAuthorizationWithoutKeys(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		assertGistCreateRequest(t, request)
		assertGistAuthHeader(t, request, "")

		responseWriter.WriteHeader(http.StatusCreated)

		_, err := io.WriteString(responseWriter, `{"html_url":"https://gist.github.com/owner/abcdef123"}`)
		if err != nil {
			t.Fatalf("write gist response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := newGistClient(server.Client(), server.URL, nil, "desc", "file.md", true)

	gistURL, err := client.createGist(context.Background(), "hello")
	if err != nil {
		t.Fatalf("create gist: %v", err)
	}

	if gistURL != "https://gist.github.com/owner/abcdef123" {
		t.Fatalf("unexpected gist url: %q", gistURL)
	}
}

func TestGistClientCreateGistReturnsErrorResponseText(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.WriteHeader(http.StatusUnauthorized)

		_, err := io.WriteString(
			responseWriter,
			`{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest"}`,
		)
		if err != nil {
			t.Fatalf("write gist error response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := newGistClient(server.Client(), server.URL, []string{"bad-token"}, "", "f.md", false)

	_, err := client.createGist(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected GitHub gist create status error")
	}

	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("unexpected GitHub gist error: %v", err)
	}
}

func TestGistClientCreateGistRejectsNonURLResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.WriteHeader(http.StatusCreated)

		_, err := io.WriteString(responseWriter, `{"html_url":"not a url"}`)
		if err != nil {
			t.Fatalf("write gist response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := newGistClient(server.Client(), server.URL, []string{"x"}, "desc", "f.md", false)

	_, err := client.createGist(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected GitHub gist invalid response error")
	}

	if !strings.Contains(err.Error(), "invalid GitHub gist response") {
		t.Fatalf("unexpected GitHub gist error: %v", err)
	}
}

func TestGistClientCreateGistSendsNonASCIIBody(t *testing.T) {
	t.Parallel()

	const gistText = "café — non-ascii text\nsecond line"

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request == nil {
			t.Fatal("nil gist create request")
		}

		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read gist create body: %v", err)
		}

		var createRequest gistCreateRequest

		err = json.Unmarshal(body, &createRequest)
		if err != nil {
			t.Fatalf("decode gist create body: %v", err)
		}

		if file, ok := createRequest.Files["f.md"]; !ok || file.Content != gistText {
			t.Fatalf("unexpected gist file content: %q", file.Content)
		}

		responseWriter.WriteHeader(http.StatusCreated)

		_, err = io.WriteString(responseWriter, `{"html_url":"https://gist.github.com/abc/utf8"}`)
		if err != nil {
			t.Fatalf("write gist response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := newGistClient(server.Client(), server.URL, nil, "desc", "f.md", false)

	_, err := client.createGist(context.Background(), gistText)
	if err != nil {
		t.Fatalf("create gist: %v", err)
	}
}

func TestIsGistURL(t *testing.T) {
	t.Parallel()

	validURLs := []string{
		"https://gist.github.com/user/abcdef123",
		"https://gist.githubusercontent.com/abc/def",
		"https://gist.example.com/abcdEF12",
	}

	for _, gistURL := range validURLs {
		if !isGistURL(gistURL) {
			t.Fatalf("expected gist url %q to be valid", gistURL)
		}
	}

	invalidURLs := []string{
		"https://gist.github.com/",
		"",
		"not a url",
		"ftp://gist.github.com/abc",
	}

	for _, gistURL := range invalidURLs {
		if isGistURL(gistURL) {
			t.Fatalf("expected gist url %q to be invalid", gistURL)
		}
	}
}
