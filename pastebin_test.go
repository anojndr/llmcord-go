package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestPastebinClientCreatePastePostsFormAndReturnsURL(t *testing.T) {
	t.Parallel()

	const (
		devKey    = "test-dev-key"
		pasteText = "# hello from llmcord-go"
		pasteName = "llmcord-go reply"
		expire    = "1D"
	)

	var serverURL string

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		assertPastebinCreateRequest(t, responseWriter, request)
		assertPastebinFormField(t, request, "api_dev_key", devKey)
		assertPastebinFormField(t, request, "api_paste_code", pasteText)
		assertPastebinFormField(t, request, "api_paste_name", pasteName)
		assertPastebinFormField(t, request, "api_paste_expire_date", expire)

		_, err := io.WriteString(responseWriter, serverURL+"/UIFdu235s")
		if err != nil {
			t.Fatalf("write Pastebin response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	serverURL = server.URL

	client := newPastebinClient(
		server.Client(),
		server.URL+"/api/api_post.php",
		devKey,
		pasteName,
		expire,
	)

	pasteURL, err := client.createPaste(context.Background(), pasteText)
	if err != nil {
		t.Fatalf("create Pastebin paste: %v", err)
	}

	if pasteURL != server.URL+"/UIFdu235s" {
		t.Fatalf("unexpected Pastebin paste url: got %q want %q", pasteURL, server.URL+"/UIFdu235s")
	}
}

func assertPastebinCreateRequest(t *testing.T, responseWriter http.ResponseWriter, request *http.Request) {
	t.Helper()

	if request.Method != http.MethodPost {
		t.Fatalf("unexpected Pastebin request: %s %s", request.Method, request.URL.Path)
	}

	if request.URL.Path != "/api/api_post.php" {
		t.Fatalf("unexpected Pastebin path: %q", request.URL.Path)
	}

	if request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf(
			"unexpected Content-Type header: %q",
			request.Header.Get("Content-Type"),
		)
	}

	request.Body = http.MaxBytesReader(responseWriter, request.Body, 16*1024)

	err := request.ParseForm()
	if err != nil {
		t.Fatalf("parse Pastebin form: %v", err)
	}

	assertPastebinFormField(t, request, "api_option", "paste")
}

func assertPastebinFormField(t *testing.T, request *http.Request, field string, want string) {
	t.Helper()

	if request.PostForm.Get(field) != want {
		t.Fatalf(
			"unexpected %s form field: got %q want %q",
			field,
			request.PostForm.Get(field),
			want,
		)
	}
}

func TestPastebinClientCreatePasteOmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	var serverURL string

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		err := request.ParseForm()
		if err != nil {
			t.Fatalf("parse Pastebin form: %v", err)
		}

		assertPastebinFormField(t, request, "api_paste_name", "")
		assertPastebinFormField(t, request, "api_paste_expire_date", "")

		_, err = io.WriteString(responseWriter, serverURL+"/abc123")
		if err != nil {
			t.Fatalf("write Pastebin response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	serverURL = server.URL

	client := newPastebinClient(server.Client(), server.URL, "test-dev-key", "", "")

	pasteURL, err := client.createPaste(context.Background(), "hello")
	if err != nil {
		t.Fatalf("create Pastebin paste: %v", err)
	}

	if pasteURL != server.URL+"/abc123" {
		t.Fatalf("unexpected Pastebin paste url: got %q want %q", pasteURL, server.URL+"/abc123")
	}
}

func TestPastebinClientCreatePasteReturnsErrorResponseText(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.WriteHeader(http.StatusOK)

		_, err := io.WriteString(responseWriter, "Bad API request, invalid api_dev_key")
		if err != nil {
			t.Fatalf("write Pastebin error response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := newPastebinClient(server.Client(), server.URL, "test-dev-key", "", "")

	_, err := client.createPaste(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected Pastebin API error")
	}

	if !containsFold(err.Error(), "Bad API request, invalid api_dev_key") {
		t.Fatalf("unexpected Pastebin API error: %v", err)
	}
}

func TestPastebinClientCreatePasteReturnsHTTPStatusErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.WriteHeader(http.StatusInternalServerError)

		_, err := io.WriteString(responseWriter, "boom")
		if err != nil {
			t.Fatalf("write Pastebin error response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := newPastebinClient(server.Client(), server.URL, "test-dev-key", "", "")

	_, err := client.createPaste(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected Pastebin HTTP status error")
	}

	if !containsFold(err.Error(), "unexpected Pastebin create status: 500") {
		t.Fatalf("unexpected Pastebin HTTP status error: %v", err)
	}
}

func TestPastebinClientCreatePasteRejectsNonURLResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(responseWriter, "just some text, not a url")
		if err != nil {
			t.Fatalf("write Pastebin response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := newPastebinClient(server.Client(), server.URL, "test-dev-key", "", "")

	_, err := client.createPaste(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected Pastebin invalid response error")
	}
}

func TestPastebinClientCreatePasteRoundTripsPasteNameInForm(t *testing.T) {
	t.Parallel()

	const pasteName = "name with spaces & ampersands"

	var serverURL string

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		err := request.ParseForm()
		if err != nil {
			t.Fatalf("parse Pastebin form: %v", err)
		}

		assertPastebinFormField(t, request, "api_paste_name", pasteName)

		_, err = io.WriteString(responseWriter, serverURL+"/roundtrip")
		if err != nil {
			t.Fatalf("write Pastebin response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	serverURL = server.URL

	client := newPastebinClient(server.Client(), server.URL, "test-dev-key", pasteName, "1W")

	_, err := client.createPaste(context.Background(), "hello")
	if err != nil {
		t.Fatalf("create Pastebin paste: %v", err)
	}
}

func TestPastebinClientCreatePasteSendsUTF8PasteCode(t *testing.T) {
	t.Parallel()

	const pasteText = "café — non-ascii text\nsecond line"

	var serverURL string

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		err := request.ParseForm()
		if err != nil {
			t.Fatalf("parse Pastebin form: %v", err)
		}

		assertPastebinFormField(t, request, "api_paste_code", pasteText)

		_, err = io.WriteString(responseWriter, serverURL+"/utf8")
		if err != nil {
			t.Fatalf("write Pastebin response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	serverURL = server.URL

	client := newPastebinClient(server.Client(), server.URL, "test-dev-key", "", "")

	_, err := client.createPaste(context.Background(), pasteText)
	if err != nil {
		t.Fatalf("create Pastebin paste: %v", err)
	}
}

func TestValidatePastebinExpireDate(t *testing.T) {
	t.Parallel()

	validDates := []string{
		"N", "10M", "1H", "1D", "1W", "2W", "1M", "6M", "1Y", "",
	}

	for _, expireDate := range validDates {
		err := validatePastebinExpireDate(expireDate)
		if err != nil {
			t.Fatalf("expected valid Pastebin expire date %q: %v", expireDate, err)
		}
	}

	invalidDates := []string{"0D", "1Y1M", "5M", "1S", "never"}

	for _, expireDate := range invalidDates {
		err := validatePastebinExpireDate(expireDate)
		if err == nil {
			t.Fatalf("expected invalid Pastebin expire date %q", expireDate)
		}
	}
}

func TestIsPastebinPasteURL(t *testing.T) {
	t.Parallel()

	validURLs := []string{
		"https://pastebin.com/UIFdu235s",
		"https://pastebin.com/abc123XYZ",
		"https://pastebin.example.com/abcdEF12",
	}

	for _, pasteURL := range validURLs {
		parsedURL, err := url.Parse(pasteURL)
		if err != nil {
			t.Fatalf("parse Pastebin url %q: %v", pasteURL, err)
		}

		if !isPastebinPasteURL(parsedURL) {
			t.Fatalf("expected Pastebin url %q to be valid", pasteURL)
		}
	}

	invalidURLs := []string{
		"https://pastebin.com/",
		"https://pastebin.example.com/",
		"ftp://pastebin.com/abc123",
		"not a url",
	}

	for _, pasteURL := range invalidURLs {
		parsedURL, err := url.Parse(pasteURL)
		if err != nil {
			continue
		}

		if isPastebinPasteURL(parsedURL) {
			t.Fatalf("expected Pastebin url %q to be invalid", pasteURL)
		}
	}
}
