package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAuthorizationCode = "test-code"

var errCallbackFailed = errors.New("callback failed")

func TestBuildAuthorizationURL(t *testing.T) {
	t.Parallel()

	authURL, err := buildAuthorizationURL(
		chatGPTAuthorizeURL,
		chatGPTRedirectURI,
		"test-verifier",
		"test-state",
		chatGPTOriginator,
	)
	if err != nil {
		t.Fatalf("build authorization url: %v", err)
	}

	parsedURL, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorization url: %v", err)
	}

	queryValues := parsedURL.Query()

	if queryValues.Get("client_id") != chatGPTClientID {
		t.Fatalf("unexpected client_id: %q", queryValues.Get("client_id"))
	}

	if queryValues.Get("redirect_uri") != chatGPTRedirectURI {
		t.Fatalf("unexpected redirect_uri: %q", queryValues.Get("redirect_uri"))
	}

	if queryValues.Get("code_challenge_method") != "S256" {
		t.Fatalf("unexpected code_challenge_method: %q", queryValues.Get("code_challenge_method"))
	}

	if queryValues.Get("scope") != chatGPTScope {
		t.Fatalf("unexpected scope: %q", queryValues.Get("scope"))
	}

	if queryValues.Get("state") != "test-state" {
		t.Fatalf("unexpected state: %q", queryValues.Get("state"))
	}

	if queryValues.Get("originator") != chatGPTOriginator {
		t.Fatalf("unexpected originator: %q", queryValues.Get("originator"))
	}

	if queryValues.Get("code_challenge") == "" {
		t.Fatal("expected code_challenge to be set")
	}
}

func TestParseAuthorizationInput(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		input         string
		expectedCode  string
		expectedState string
	}{
		{
			name:          "redirect url",
			input:         "http://localhost:1455/auth/callback?code=" + testAuthorizationCode + "&state=test-state",
			expectedCode:  testAuthorizationCode,
			expectedState: "test-state",
		},
		{
			name:          "query string",
			input:         "code=" + testAuthorizationCode + "&state=test-state",
			expectedCode:  testAuthorizationCode,
			expectedState: "test-state",
		},
		{
			name:          "hash separated",
			input:         testAuthorizationCode + "#test-state",
			expectedCode:  testAuthorizationCode,
			expectedState: "test-state",
		},
		{
			name:          "raw code",
			input:         testAuthorizationCode,
			expectedCode:  testAuthorizationCode,
			expectedState: "",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			parsedInput, err := parseAuthorizationInput(testCase.input)
			if err != nil {
				t.Fatalf("parse authorization input: %v", err)
			}

			if parsedInput.code != testCase.expectedCode {
				t.Fatalf("unexpected code: %q", parsedInput.code)
			}

			if parsedInput.state != testCase.expectedState {
				t.Fatalf("unexpected state: %q", parsedInput.state)
			}
		})
	}
}

func TestExchangeAuthorizationCode(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		t.Helper()

		if request.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", request.Method)
		}

		request.Body = http.MaxBytesReader(responseWriter, request.Body, 1024)

		err := request.ParseForm()
		if err != nil {
			t.Fatalf("parse token request form: %v", err)
		}

		if request.Form.Get("grant_type") != "authorization_code" {
			t.Fatalf("unexpected grant_type: %q", request.Form.Get("grant_type"))
		}

		if request.Form.Get("client_id") != chatGPTClientID {
			t.Fatalf("unexpected client_id: %q", request.Form.Get("client_id"))
		}

		if request.Form.Get("code") != testAuthorizationCode {
			t.Fatalf("unexpected code: %q", request.Form.Get("code"))
		}

		if request.Form.Get("code_verifier") != "test-verifier" {
			t.Fatalf("unexpected code_verifier: %q", request.Form.Get("code_verifier"))
		}

		if request.Form.Get("redirect_uri") != chatGPTRedirectURI {
			t.Fatalf("unexpected redirect_uri: %q", request.Form.Get("redirect_uri"))
		}

		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"access_token":"copy-me"}`))
	}))
	defer server.Close()

	token, err := exchangeAuthorizationCode(
		context.Background(),
		server.Client(),
		server.URL,
		chatGPTRedirectURI,
		testAuthorizationCode,
		"test-verifier",
	)
	if err != nil {
		t.Fatalf("exchange authorization code: %v", err)
	}

	if token != "copy-me" {
		t.Fatalf("unexpected token: %q", token)
	}
}

func TestExchangeAuthorizationCodeRejectsMissingAccessToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"refresh_token":"missing-access-token"}`))
	}))
	defer server.Close()

	_, err := exchangeAuthorizationCode(
		context.Background(),
		server.Client(),
		server.URL,
		chatGPTRedirectURI,
		testAuthorizationCode,
		"test-verifier",
	)
	if err == nil {
		t.Fatal("expected missing access token to fail")
	}
}

func TestDefaultAuthFlowUsesCLIConfiguration(t *testing.T) {
	t.Parallel()

	flow := defaultAuthFlow()

	if flow.client == nil {
		t.Fatal("expected default auth flow client")
	}

	if flow.client.Timeout != chatGPTRequestTimeout {
		t.Fatalf("client timeout = %s, want %s", flow.client.Timeout, chatGPTRequestTimeout)
	}

	if flow.authorizeURL != chatGPTAuthorizeURL {
		t.Fatalf("authorizeURL = %q, want %q", flow.authorizeURL, chatGPTAuthorizeURL)
	}

	if flow.tokenURL != chatGPTExchangeURL {
		t.Fatalf("tokenURL = %q, want %q", flow.tokenURL, chatGPTExchangeURL)
	}

	if flow.redirectURI != chatGPTRedirectURI {
		t.Fatalf("redirectURI = %q, want %q", flow.redirectURI, chatGPTRedirectURI)
	}

	if flow.callbackAddr != chatGPTCallbackAddr {
		t.Fatalf("callbackAddr = %q, want %q", flow.callbackAddr, chatGPTCallbackAddr)
	}

	if flow.originator != chatGPTOriginator {
		t.Fatalf("originator = %q, want %q", flow.originator, chatGPTOriginator)
	}

	if flow.input != os.Stdin {
		t.Fatal("expected default auth flow to read from stdin")
	}

	if flow.stdout != os.Stdout {
		t.Fatal("expected default auth flow to write token to stdout")
	}

	if flow.stderr != os.Stderr {
		t.Fatal("expected default auth flow to write prompts to stderr")
	}
}

func TestAuthFlowRunCompletesWithLocalCallback(t *testing.T) {
	t.Parallel()

	callbackAddress := reserveCallbackTestAddress(t)
	redirectURI := "http://" + callbackAddress + chatGPTCallbackPath

	tokenServer := newTokenExchangeTestServer(t, redirectURI)
	defer tokenServer.Close()

	stderr := new(synchronizedBuffer)
	flow := authFlow{
		client:       tokenServer.Client(),
		authorizeURL: chatGPTAuthorizeURL,
		tokenURL:     tokenServer.URL,
		redirectURI:  redirectURI,
		callbackAddr: callbackAddress,
		originator:   chatGPTOriginator,
		input:        strings.NewReader(""),
		stdout:       io.Discard,
		stderr:       stderr,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	resultCh := make(chan authorizationResult, 1)

	go func() {
		token, err := flow.run(ctx)
		resultCh <- newAuthorizationResult(token, err)
	}()

	authURL := waitForPrintedURL(t, stderr)

	parsedURL, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse printed auth url: %v", err)
	}

	state := parsedURL.Query().Get("state")
	if strings.TrimSpace(state) == "" {
		t.Fatal("expected oauth state in printed authorization URL")
	}

	response, err := getEventually(
		redirectURI + "?code=" + testAuthorizationCode + "&state=" + url.QueryEscape(state),
	)
	if err != nil {
		t.Fatalf("request callback: %v", err)
	}

	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback status code = %d, want %d", response.StatusCode, http.StatusOK)
	}

	result := waitForAuthorizationResult(t, resultCh)
	if result.err != nil {
		t.Fatalf("auth flow run: %v", result.err)
	}

	if result.code != "oauth-token" {
		t.Fatalf("unexpected token: %q", result.code)
	}

	if !strings.Contains(stderr.String(), "ChatGPT login complete.") {
		t.Fatalf("stderr = %q, want completion message", stderr.String())
	}
}

func TestAuthFlowRunFailsWithoutInteractiveInputOrCallbackServer(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer

	flow := authFlow{
		client:       new(http.Client),
		authorizeURL: chatGPTAuthorizeURL,
		tokenURL:     chatGPTExchangeURL,
		redirectURI:  chatGPTRedirectURI,
		callbackAddr: "127.0.0.1:not-a-port",
		originator:   chatGPTOriginator,
		input:        strings.NewReader(""),
		stdout:       io.Discard,
		stderr:       &stderr,
	}

	_, err := flow.run(t.Context())
	if err == nil {
		t.Fatal("expected auth flow to fail without callback server or interactive stdin")
	}

	if !strings.Contains(err.Error(), "no callback server") {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stderr.String(), "warning: could not start local callback server") {
		t.Fatalf("stderr = %q, want startup warning", stderr.String())
	}
}

func TestPrintLoginInstructions(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	printLoginInstructions(&output, "https://example.com/login", true, true)

	if !strings.Contains(output.String(), "Open this URL in your browser and log in to ChatGPT:") {
		t.Fatalf("output = %q, want prompt", output.String())
	}

	if !strings.Contains(output.String(), "https://example.com/login") {
		t.Fatalf("output = %q, want auth url", output.String())
	}

	if !strings.Contains(output.String(), "Waiting for the local OAuth callback") {
		t.Fatalf("output = %q, want callback instructions", output.String())
	}

	if !strings.Contains(output.String(), "You can also paste the final redirect URL") {
		t.Fatalf("output = %q, want manual input instructions", output.String())
	}
}

func TestRandomBase64URL(t *testing.T) {
	t.Parallel()

	value, err := randomBase64URL(chatGPTCodeBytes)
	if err != nil {
		t.Fatalf("randomBase64URL: %v", err)
	}

	if strings.ContainsAny(value, "+/=") {
		t.Fatalf("value = %q, want raw-url-safe base64", value)
	}

	decodedValue, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("decode random value: %v", err)
	}

	if len(decodedValue) != chatGPTCodeBytes {
		t.Fatalf("decoded length = %d, want %d", len(decodedValue), chatGPTCodeBytes)
	}
}

func TestStartCallbackServerReceivesAuthorizationCode(t *testing.T) {
	t.Parallel()

	callbackAddress := reserveCallbackTestAddress(t)

	server, err := startCallbackServer(t.Context(), callbackAddress, "expected-state")
	if err != nil {
		t.Fatalf("start callback server: %v", err)
	}

	t.Cleanup(func() {
		_ = server.close()
	})

	response, err := getEventually(
		"http://" + callbackAddress + chatGPTCallbackPath +
			"?code=" + testAuthorizationCode + "&state=expected-state",
	)
	if err != nil {
		t.Fatalf("request callback: %v", err)
	}

	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want %d", response.StatusCode, http.StatusOK)
	}

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read callback response: %v", err)
	}

	if !strings.Contains(string(responseBody), "Authentication successful") {
		t.Fatalf("response body = %q, want success HTML", string(responseBody))
	}

	result := waitForAuthorizationResult(t, server.results)
	if result.err != nil {
		t.Fatalf("callback result error: %v", result.err)
	}

	if result.code != testAuthorizationCode {
		t.Fatalf("callback code = %q, want %q", result.code, testAuthorizationCode)
	}
}

func TestStartCallbackServerRejectsInvalidAddress(t *testing.T) {
	t.Parallel()

	_, err := startCallbackServer(t.Context(), "127.0.0.1:not-a-port", "state")
	if err == nil {
		t.Fatal("expected invalid callback address to fail")
	}
}

func TestHandleCallbackRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		requestURL string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "wrong path",
			requestURL: "http://localhost/other?code=" + testAuthorizationCode + "&state=expected-state",
			wantStatus: http.StatusNotFound,
			wantBody:   "404 page not found",
		},
		{
			name:       "state mismatch",
			requestURL: "http://localhost/auth/callback?code=" + testAuthorizationCode + "&state=wrong-state",
			wantStatus: http.StatusBadRequest,
			wantBody:   "State mismatch",
		},
		{
			name:       "missing code",
			requestURL: "http://localhost/auth/callback?state=expected-state",
			wantStatus: http.StatusBadRequest,
			wantBody:   "Missing authorization code",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			results := make(chan authorizationResult, 1)
			responseRecorder := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				testCase.requestURL,
				nil,
			)

			handleCallback(responseRecorder, request, "expected-state", results)

			if responseRecorder.Code != testCase.wantStatus {
				t.Fatalf("status code = %d, want %d", responseRecorder.Code, testCase.wantStatus)
			}

			if !strings.Contains(responseRecorder.Body.String(), testCase.wantBody) {
				t.Fatalf("response body = %q, want substring %q", responseRecorder.Body.String(), testCase.wantBody)
			}

			select {
			case result := <-results:
				t.Fatalf("unexpected callback result: %#v", result)
			default:
			}
		})
	}
}

func TestCallbackServerCloseNil(t *testing.T) {
	t.Parallel()

	err := (*callbackServer)(nil).close()
	if err != nil {
		t.Fatalf("nil callback server close error = %v", err)
	}

	server := new(callbackServer)

	err = server.close()
	if err != nil {
		t.Fatalf("empty callback server close error = %v", err)
	}
}

func TestWaitForAuthorizationCodeUsesCallbackResult(t *testing.T) {
	t.Parallel()

	results := make(chan authorizationResult, 1)
	results <- newAuthorizationResult("callback-code", nil)

	server := newCallbackServer(results)

	code, err := waitForAuthorizationCode(
		t.Context(),
		strings.NewReader(""),
		io.Discard,
		server,
		"expected-state",
	)
	if err != nil {
		t.Fatalf("wait for authorization code: %v", err)
	}

	if code != "callback-code" {
		t.Fatalf("code = %q, want %q", code, "callback-code")
	}
}

func TestWaitForAuthorizationCodeUsesCallbackAfterManualInputCloses(t *testing.T) {
	t.Parallel()

	interactiveReader := openInteractiveReader(t)
	if !isInteractiveReader(interactiveReader) {
		t.Skipf("%s is not reported as an interactive reader in this environment", os.DevNull)
	}

	results := make(chan authorizationResult, 1)
	server := newCallbackServer(results)

	go func() {
		time.Sleep(50 * time.Millisecond)

		results <- newAuthorizationResult("callback-code", nil)
	}()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	code, err := waitForAuthorizationCode(
		ctx,
		interactiveReader,
		io.Discard,
		server,
		"expected-state",
	)
	if err != nil {
		t.Fatalf("wait for authorization code: %v", err)
	}

	if code != "callback-code" {
		t.Fatalf("code = %q, want %q", code, "callback-code")
	}
}

func TestWaitForAuthorizationCodeReturnsCallbackError(t *testing.T) {
	t.Parallel()

	results := make(chan authorizationResult, 1)
	results <- newAuthorizationResult("", errCallbackFailed)

	server := newCallbackServer(results)

	_, err := waitForAuthorizationCode(
		t.Context(),
		strings.NewReader(""),
		io.Discard,
		server,
		"expected-state",
	)
	if err == nil {
		t.Fatal("expected callback error")
	}

	if !strings.Contains(err.Error(), "callback failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForAuthorizationCodeRequiresManualInputOrCallback(t *testing.T) {
	t.Parallel()

	_, err := waitForAuthorizationCode(
		t.Context(),
		strings.NewReader(""),
		io.Discard,
		nil,
		"expected-state",
	)
	if err == nil {
		t.Fatal("expected missing input sources to fail")
	}
}

func TestCallbackResultsNilServer(t *testing.T) {
	t.Parallel()

	if callbackResults(nil) != nil {
		t.Fatal("expected nil callback results channel for nil server")
	}
}

func TestStartManualInput(t *testing.T) {
	t.Parallel()

	if got := startManualInput(strings.NewReader("code\n"), io.Discard); got != nil {
		t.Fatalf("startManualInput(non-interactive) = %#v, want nil", got)
	}

	interactiveReader := openInteractiveReader(t)
	if !isInteractiveReader(interactiveReader) {
		t.Skipf("%s is not reported as an interactive reader in this environment", os.DevNull)
	}

	var output bytes.Buffer

	results := startManualInput(interactiveReader, &output)
	if results == nil {
		t.Fatal("expected manual input channel for interactive reader")
	}

	select {
	case _, ok := <-results:
		if ok {
			t.Fatal("expected EOF on interactive reader to close manual input channel")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for manual input goroutine to exit")
	}

	if !strings.Contains(output.String(), "Paste redirect URL or code and press Enter: ") {
		t.Fatalf("output = %q, want prompt", output.String())
	}
}

func TestIsInteractiveReader(t *testing.T) {
	t.Parallel()

	if isInteractiveReader(strings.NewReader("code")) {
		t.Fatal("expected strings.Reader to be non-interactive")
	}

	tempFile, err := os.CreateTemp(t.TempDir(), "manual-input-*.txt")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}

	t.Cleanup(func() {
		_ = tempFile.Close()
	})

	if isInteractiveReader(tempFile) {
		t.Fatal("expected regular file to be non-interactive")
	}

	interactiveReader := openInteractiveReader(t)
	if !isInteractiveReader(interactiveReader) {
		t.Skipf("%s is not reported as an interactive reader in this environment", os.DevNull)
	}
}

func TestParseAuthorizationInputRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	testCases := []string{
		"   ",
		"http://%zz",
		"#missing-code",
	}

	for _, input := range testCases {
		_, err := parseAuthorizationInput(input)
		if err == nil {
			t.Fatalf("expected parseAuthorizationInput(%q) to fail", input)
		}
	}
}

func TestValidateAuthorizationState(t *testing.T) {
	t.Parallel()

	err := validateAuthorizationState("", "expected-state")
	if err != nil {
		t.Fatalf("blank received state should be accepted: %v", err)
	}

	err = validateAuthorizationState(" expected-state ", "expected-state")
	if err != nil {
		t.Fatalf("trimmed matching state should be accepted: %v", err)
	}

	err = validateAuthorizationState("wrong-state", "expected-state")
	if err == nil {
		t.Fatal("expected mismatched state to fail")
	}
}

func TestExchangeAuthorizationCodeRejectsNonSuccessStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		http.Error(responseWriter, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	_, err := exchangeAuthorizationCode(
		context.Background(),
		server.Client(),
		server.URL,
		chatGPTRedirectURI,
		testAuthorizationCode,
		"test-verifier",
	)
	if err == nil {
		t.Fatal("expected non-success status to fail")
	}

	if !strings.Contains(err.Error(), "token exchange failed with status 400") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExchangeAuthorizationCodeRejectsUnreadableErrorResponse(t *testing.T) {
	t.Parallel()

	httpClient := new(http.Client)
	httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := new(http.Response)
		response.StatusCode = http.StatusBadRequest
		response.Header = make(http.Header)
		response.Body = io.NopCloser(errorReader{err: io.ErrUnexpectedEOF})
		response.Request = request

		return response, nil
	})

	_, err := exchangeAuthorizationCode(
		context.Background(),
		httpClient,
		"https://example.com/token",
		chatGPTRedirectURI,
		testAuthorizationCode,
		"test-verifier",
	)
	if err == nil {
		t.Fatal("expected unreadable error response to fail")
	}

	if !strings.Contains(err.Error(), "read token error response") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExchangeAuthorizationCodeRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(responseWriter, "{")
	}))
	defer server.Close()

	_, err := exchangeAuthorizationCode(
		context.Background(),
		server.Client(),
		server.URL,
		chatGPTRedirectURI,
		testAuthorizationCode,
		"test-verifier",
	)
	if err == nil {
		t.Fatal("expected invalid json to fail")
	}

	if !strings.Contains(err.Error(), "decode token response") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExchangeAuthorizationCodeRejectsInvalidTokenURL(t *testing.T) {
	t.Parallel()

	_, err := exchangeAuthorizationCode(
		context.Background(),
		new(http.Client),
		"://bad-url",
		chatGPTRedirectURI,
		testAuthorizationCode,
		"test-verifier",
	)
	if err == nil {
		t.Fatal("expected invalid token URL to fail")
	}

	if !strings.Contains(err.Error(), "create token request") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func newTokenExchangeTestServer(t *testing.T, redirectURI string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		t.Helper()

		if request.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", request.Method)
		}

		request.Body = http.MaxBytesReader(responseWriter, request.Body, 1024)

		err := request.ParseForm()
		if err != nil {
			t.Fatalf("parse token request form: %v", err)
		}

		if request.Form.Get("code") != testAuthorizationCode {
			t.Fatalf("unexpected code: %q", request.Form.Get("code"))
		}

		if request.Form.Get("redirect_uri") != redirectURI {
			t.Fatalf("unexpected redirect uri: %q", request.Form.Get("redirect_uri"))
		}

		if strings.TrimSpace(request.Form.Get("code_verifier")) == "" {
			t.Fatal("expected non-empty code verifier")
		}

		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(responseWriter, `{"access_token":"oauth-token"}`)
	}))
}

func newAuthorizationResult(code string, err error) authorizationResult {
	var result authorizationResult

	result.code = code
	result.err = err

	return result
}

func newCallbackServer(results <-chan authorizationResult) *callbackServer {
	server := new(callbackServer)
	server.results = results

	return server
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (buffer *synchronizedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()

	_, _ = buffer.buf.Write(data)

	return len(data), nil
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()

	return buffer.buf.String()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type errorReader struct {
	err error
}

func (reader errorReader) Read(_ []byte) (int, error) {
	return 0, reader.err
}

func reserveCallbackTestAddress(t *testing.T) string {
	t.Helper()

	listenConfig := new(net.ListenConfig)

	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve callback address: %v", err)
	}

	address := listener.Addr().String()

	err = listener.Close()
	if err != nil {
		t.Fatalf("release reserved callback address: %v", err)
	}

	return address
}

func getEventually(requestURL string) (*http.Response, error) {
	httpClient := new(http.Client)
	httpClient.Timeout = time.Second

	deadline := time.Now().Add(2 * time.Second)

	var lastErr error

	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodGet,
			requestURL,
			nil,
		)
		if err != nil {
			return nil, fmt.Errorf("build get request: %w", err)
		}

		response, err := httpClient.Do(request)
		if err == nil {
			return response, nil
		}

		lastErr = err

		time.Sleep(10 * time.Millisecond)
	}

	return nil, lastErr
}

func waitForPrintedURL(t *testing.T, output *synchronizedBuffer) string {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		for line := range strings.SplitSeq(output.String(), "\n") {
			trimmedLine := strings.TrimSpace(line)
			if strings.HasPrefix(trimmedLine, "https://") || strings.HasPrefix(trimmedLine, "http://") {
				return trimmedLine
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for printed authorization URL")

	return ""
}

func waitForAuthorizationResult(
	t *testing.T,
	results <-chan authorizationResult,
) authorizationResult {
	t.Helper()

	select {
	case result := <-results:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for authorization result")
	}

	var result authorizationResult

	return result
}

func openInteractiveReader(t *testing.T) *os.File {
	t.Helper()

	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}

	t.Cleanup(func() {
		_ = file.Close()
	})

	return file
}
