package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

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
			input:         "http://localhost:1455/auth/callback?code=test-code&state=test-state",
			expectedCode:  "test-code",
			expectedState: "test-state",
		},
		{
			name:          "query string",
			input:         "code=test-code&state=test-state",
			expectedCode:  "test-code",
			expectedState: "test-state",
		},
		{
			name:          "hash separated",
			input:         "test-code#test-state",
			expectedCode:  "test-code",
			expectedState: "test-state",
		},
		{
			name:          "raw code",
			input:         "test-code",
			expectedCode:  "test-code",
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

		if request.Form.Get("code") != "test-code" {
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
		"test-code",
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
		"test-code",
		"test-verifier",
	)
	if err == nil {
		t.Fatal("expected missing access token to fail")
	}
}
