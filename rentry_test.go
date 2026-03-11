package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRentryClientCreateEntryPostsFormAndReturnsLocation(t *testing.T) {
	t.Parallel()

	const (
		csrfCookie = "cookie-token"
		csrfField  = "form-token"
		entryText  = "# hello from llmcord-go"
		entryPath  = "/abc123"
	)

	var serverURL string

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/":
			cookie := new(http.Cookie)
			cookie.Name = "csrftoken"
			cookie.Value = csrfCookie
			cookie.Path = "/"

			http.SetCookie(responseWriter, cookie)

			_, err := io.WriteString(
				responseWriter,
				`<input type="hidden" name="csrfmiddlewaretoken" value="`+csrfField+`">`,
			)
			if err != nil {
				t.Fatalf("write Rentry form response: %v", err)
			}
		case request.Method == http.MethodPost && request.URL.Path == "/":
			if request.Header.Get("Referer") != serverURL {
				t.Fatalf("unexpected Referer header: %q", request.Header.Get("Referer"))
			}

			cookie, err := request.Cookie("csrftoken")
			if err != nil {
				t.Fatalf("read csrftoken cookie: %v", err)
			}

			if cookie.Value != csrfCookie {
				t.Fatalf("unexpected csrftoken cookie: %q", cookie.Value)
			}

			request.Body = http.MaxBytesReader(responseWriter, request.Body, 1024)

			err = request.ParseForm()
			if err != nil {
				t.Fatalf("parse Rentry form: %v", err)
			}

			if request.PostForm.Get("csrfmiddlewaretoken") != csrfField {
				t.Fatalf(
					"unexpected csrf form field: got %q want %q",
					request.PostForm.Get("csrfmiddlewaretoken"),
					csrfField,
				)
			}

			if request.PostForm.Get("text") != entryText {
				t.Fatalf(
					"unexpected text form field: got %q want %q",
					request.PostForm.Get("text"),
					entryText,
				)
			}

			responseWriter.Header().Set("Location", entryPath)
			responseWriter.WriteHeader(http.StatusFound)
		default:
			t.Fatalf("unexpected Rentry request: %s %s", request.Method, request.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	serverURL = server.URL + "/"

	client := newRentryClient(server.Client(), serverURL)

	entryURL, err := client.createEntry(context.Background(), entryText)
	if err != nil {
		t.Fatalf("create Rentry entry: %v", err)
	}

	if entryURL != server.URL+entryPath {
		t.Fatalf("unexpected Rentry entry url: got %q want %q", entryURL, server.URL+entryPath)
	}
}

func TestRentryClientCreateEntryReturnsStatusErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/":
			cookie := new(http.Cookie)
			cookie.Name = "csrftoken"
			cookie.Value = "cookie-token"
			cookie.Path = "/"

			http.SetCookie(responseWriter, cookie)

			_, err := io.WriteString(
				responseWriter,
				`<input type="hidden" name="csrfmiddlewaretoken" value="form-token">`,
			)
			if err != nil {
				t.Fatalf("write Rentry form response: %v", err)
			}
		case request.Method == http.MethodPost && request.URL.Path == "/":
			responseWriter.WriteHeader(http.StatusForbidden)

			_, err := io.WriteString(responseWriter, "forbidden")
			if err != nil {
				t.Fatalf("write Rentry error response: %v", err)
			}
		default:
			t.Fatalf("unexpected Rentry request: %s %s", request.Method, request.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client := newRentryClient(server.Client(), server.URL+"/")

	_, err := client.createEntry(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected Rentry status error")
	}

	if !containsFold(err.Error(), "unexpected Rentry create status: 403") {
		t.Fatalf("unexpected Rentry error: %v", err)
	}
}
