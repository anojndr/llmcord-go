package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var errFakeRentryBrowserFail = errors.New("browser failed")

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
			cookie.HttpOnly = true
			cookie.Secure = true
			cookie.SameSite = http.SameSiteStrictMode

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

	client := newRentryClient(server.Client(), serverURL, new(fakeRentryBrowserCreator))

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
			cookie.HttpOnly = true
			cookie.Secure = true
			cookie.SameSite = http.SameSiteStrictMode

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

	client := newRentryClient(server.Client(), server.URL+"/", new(fakeRentryBrowserCreator))

	_, err := client.createEntry(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected Rentry status error")
	}

	if !containsFold(err.Error(), "unexpected Rentry create status: 403") {
		t.Fatalf("unexpected Rentry error: %v", err)
	}
}

func TestRentryClientCreateEntryFallsBackToBrowserOnCloudflareChallenge(t *testing.T) {
	t.Parallel()

	server := newRentryChallengeTestServer(t)

	client := newRentryClient(server.Client(), server.URL+"/", new(fakeRentryBrowserCreator))

	browser := new(fakeRentryBrowserCreator)
	browser.url = server.URL + "/browser-created"
	browser.err = nil
	client.browserCreator = browser

	entryURL, err := client.createEntry(context.Background(), "hello from the fallback")
	if err != nil {
		t.Fatalf("create Rentry entry with browser fallback: %v", err)
	}

	if entryURL != server.URL+"/browser-created" {
		t.Fatalf("unexpected fallback entry url: got %q want %q", entryURL, server.URL+"/browser-created")
	}

	if browser.callCount != 1 {
		t.Fatalf("unexpected browser fallback call count: %d", browser.callCount)
	}

	if browser.texts[0] != "hello from the fallback" {
		t.Fatalf("unexpected fallback text: got %q want %q", browser.texts[0], "hello from the fallback")
	}
}

func TestRentryClientCreateEntryDoesNotFallBackToBrowserOnFormStatusError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/" {
			responseWriter.WriteHeader(http.StatusInternalServerError)

			return
		}

		t.Fatalf("unexpected Rentry request: %s %s", request.Method, request.URL.Path)
	}))
	t.Cleanup(server.Close)

	client := newRentryClient(server.Client(), server.URL+"/", new(fakeRentryBrowserCreator))

	browser := new(fakeRentryBrowserCreator)
	client.browserCreator = browser

	_, err := client.createEntry(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected Rentry form status error")
	}

	if browser.callCount != 0 {
		t.Fatalf("unexpected browser fallback call count: %d", browser.callCount)
	}
}

func TestRentryClientCreateEntryReturnsBrowserFallbackErrors(t *testing.T) {
	t.Parallel()

	server := newRentryChallengeTestServer(t)

	client := newRentryClient(server.Client(), server.URL+"/", new(fakeRentryBrowserCreator))

	browser := new(fakeRentryBrowserCreator)
	browser.err = errFakeRentryBrowserFail
	client.browserCreator = browser

	_, err := client.createEntry(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected browser fallback error")
	}

	if !containsFold(err.Error(), "browser failed") {
		t.Fatalf("unexpected browser fallback error: %v", err)
	}
}

func TestRentryClientCreateEntryFallsBackToBrowserWhenFormGetIsChallenged(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/" {
			responseWriter.WriteHeader(http.StatusForbidden)

			_, err := io.WriteString(
				responseWriter,
				`<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title></head>`+
					`<body>Enable JavaScript and cookies to continue</body></html>`,
			)
			if err != nil {
				t.Fatalf("write Rentry challenge response: %v", err)
			}

			return
		}

		t.Fatalf("unexpected Rentry request: %s %s", request.Method, request.URL.Path)
	}))
	t.Cleanup(server.Close)

	client := newRentryClient(server.Client(), server.URL+"/", new(fakeRentryBrowserCreator))

	browser := new(fakeRentryBrowserCreator)
	browser.url = server.URL + "/browser-created"
	client.browserCreator = browser

	entryURL, err := client.createEntry(context.Background(), "hello from the fallback")
	if err != nil {
		t.Fatalf("create Rentry entry with browser fallback: %v", err)
	}

	if entryURL != server.URL+"/browser-created" {
		t.Fatalf("unexpected fallback entry url: got %q want %q", entryURL, server.URL+"/browser-created")
	}

	if browser.callCount != 1 {
		t.Fatalf("unexpected browser fallback call count: %d", browser.callCount)
	}
}

func TestRentryClientCreateEntryDoesNotFallBackToBrowserWhenFormGetReturnsPlainError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/" {
			responseWriter.WriteHeader(http.StatusInternalServerError)

			_, err := io.WriteString(responseWriter, "internal error")
			if err != nil {
				t.Fatalf("write Rentry error response: %v", err)
			}

			return
		}

		t.Fatalf("unexpected Rentry request: %s %s", request.Method, request.URL.Path)
	}))
	t.Cleanup(server.Close)

	client := newRentryClient(server.Client(), server.URL+"/", new(fakeRentryBrowserCreator))

	browser := new(fakeRentryBrowserCreator)
	client.browserCreator = browser

	_, err := client.createEntry(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected Rentry form status error")
	}

	if browser.callCount != 0 {
		t.Fatalf("unexpected browser fallback call count: %d", browser.callCount)
	}
}

func newRentryChallengeTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/":
			cookie := new(http.Cookie)
			cookie.Name = "csrftoken"
			cookie.Value = "cookie-token"
			cookie.Path = "/"
			cookie.HttpOnly = true
			cookie.Secure = true
			cookie.SameSite = http.SameSiteStrictMode

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

			_, err := io.WriteString(
				responseWriter,
				`<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title>`+
					`<meta http-equiv="Content-Type" content="text/html; charset=UTF-8">`+
					`</head><body>Enable JavaScript and cookies to continue</body></html>`,
			)
			if err != nil {
				t.Fatalf("write Rentry challenge response: %v", err)
			}
		default:
			t.Fatalf("unexpected Rentry request: %s %s", request.Method, request.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	return server
}

type fakeRentryBrowserCreator struct {
	url       string
	err       error
	callCount int
	texts     []string
}

func (fake *fakeRentryBrowserCreator) createEntry(
	_ context.Context,
	endpoint string,
	text string,
) (string, error) {
	fake.callCount++
	fake.texts = append(fake.texts, text)

	if strings.TrimSpace(endpoint) == "" {
		panic("createEntry called with empty endpoint")
	}

	if fake.err != nil {
		return "", fake.err
	}

	return fake.url, nil
}
