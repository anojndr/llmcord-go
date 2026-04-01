// Package main prints a ChatGPT OAuth access token that can be used as an
// OpenAI Codex provider api_key.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	chatGPTAuthorizeURL   = "https://auth.openai.com/oauth/authorize"
	chatGPTExchangeURL    = "https://auth.openai.com/oauth/token"
	chatGPTClientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	chatGPTRedirectURI    = "http://localhost:1455/auth/callback"
	chatGPTCallbackAddr   = "localhost:1455"
	chatGPTCallbackPath   = "/auth/callback"
	chatGPTOriginator     = "llmcord-go"
	chatGPTScope          = "openid profile email offline_access"
	chatGPTCodeBytes      = 32
	chatGPTStateBytes     = 16
	chatGPTRequestTimeout = 30 * time.Second
)

const chatGPTSuccessHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Authentication successful</title>
</head>
<body>
  <p>Authentication successful. Return to your terminal to continue.</p>
</body>
</html>`

type authFlow struct {
	client       *http.Client
	authorizeURL string
	tokenURL     string
	redirectURI  string
	callbackAddr string
	originator   string
	input        io.Reader
	stdout       io.Writer
	stderr       io.Writer
}

type authorizationInput struct {
	code  string
	state string
}

type authorizationResult struct {
	code string
	err  error
}

type callbackServer struct {
	httpServer *http.Server
	results    <-chan authorizationResult
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
}

func main() {
	os.Exit(runMain())
}

func runMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	flow := defaultAuthFlow()

	token, err := flow.run(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(flow.stderr, "chatgpt login failed: %v\n", err)

		return 1
	}

	_, err = fmt.Fprintln(flow.stdout, token)
	if err != nil {
		_, _ = fmt.Fprintf(flow.stderr, "write api key: %v\n", err)

		return 1
	}

	return 0
}

func defaultAuthFlow() authFlow {
	httpClient := new(http.Client)
	httpClient.Timeout = chatGPTRequestTimeout

	return authFlow{
		client:       httpClient,
		authorizeURL: chatGPTAuthorizeURL,
		tokenURL:     chatGPTExchangeURL,
		redirectURI:  chatGPTRedirectURI,
		callbackAddr: chatGPTCallbackAddr,
		originator:   chatGPTOriginator,
		input:        os.Stdin,
		stdout:       os.Stdout,
		stderr:       os.Stderr,
	}
}

func (flow authFlow) run(ctx context.Context) (string, error) {
	verifier, err := randomBase64URL(chatGPTCodeBytes)
	if err != nil {
		return "", fmt.Errorf("generate pkce verifier: %w", err)
	}

	state, err := randomBase64URL(chatGPTStateBytes)
	if err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}

	authURL, err := buildAuthorizationURL(
		flow.authorizeURL,
		flow.redirectURI,
		verifier,
		state,
		flow.originator,
	)
	if err != nil {
		return "", fmt.Errorf("build authorization url: %w", err)
	}

	server, err := startCallbackServer(ctx, flow.callbackAddr, state)
	if err != nil {
		_, _ = fmt.Fprintf(
			flow.stderr,
			"warning: could not start local callback server on %s: %v\n",
			flow.callbackAddr,
			err,
		)
	}

	if server != nil {
		defer func() {
			_ = server.close()
		}()
	}

	printLoginInstructions(flow.stderr, authURL, server != nil, isInteractiveReader(flow.input))

	code, err := waitForAuthorizationCode(ctx, flow.input, flow.stderr, server, state)
	if err != nil {
		return "", err
	}

	token, err := exchangeAuthorizationCode(
		ctx,
		flow.client,
		flow.tokenURL,
		flow.redirectURI,
		code,
		verifier,
	)
	if err != nil {
		return "", fmt.Errorf("exchange authorization code: %w", err)
	}

	_, _ = fmt.Fprintln(flow.stderr, "ChatGPT login complete. Copy the token below into your provider api_key.")

	return token, nil
}

func printLoginInstructions(output io.Writer, authURL string, hasCallback bool, hasManualInput bool) {
	_, _ = fmt.Fprintln(output, "Open this URL in your browser and log in to ChatGPT:")
	_, _ = fmt.Fprintln(output, authURL)

	if hasCallback {
		_, _ = fmt.Fprintln(output, "Waiting for the local OAuth callback on http://localhost:1455/auth/callback ...")
	}

	if hasManualInput {
		_, _ = fmt.Fprintln(output, "You can also paste the final redirect URL or authorization code here at any time.")
	}
}

func randomBase64URL(size int) (string, error) {
	randomBytes := make([]byte, size)

	_, err := rand.Read(randomBytes)
	if err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(randomBytes), nil
}

func buildAuthorizationURL(
	baseURL, redirectURI, verifier, state, originator string,
) (string, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse authorize url %q: %w", baseURL, err)
	}

	codeChallenge := sha256.Sum256([]byte(verifier))
	queryValues := parsedURL.Query()
	queryValues.Set("response_type", "code")
	queryValues.Set("client_id", chatGPTClientID)
	queryValues.Set("redirect_uri", redirectURI)
	queryValues.Set("scope", chatGPTScope)
	queryValues.Set("code_challenge", base64.RawURLEncoding.EncodeToString(codeChallenge[:]))
	queryValues.Set("code_challenge_method", "S256")
	queryValues.Set("state", state)
	queryValues.Set("id_token_add_organizations", "true")
	queryValues.Set("codex_cli_simplified_flow", "true")
	queryValues.Set("originator", originator)
	parsedURL.RawQuery = queryValues.Encode()

	return parsedURL.String(), nil
}

func startCallbackServer(ctx context.Context, address string, state string) (*callbackServer, error) {
	var listenerConfig net.ListenConfig

	listener, err := listenerConfig.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for callback: %w", err)
	}

	results := make(chan authorizationResult, 1)
	handler := http.NewServeMux()
	handler.HandleFunc(chatGPTCallbackPath, func(responseWriter http.ResponseWriter, request *http.Request) {
		handleCallback(responseWriter, request, state, results)
	})

	httpServer := new(http.Server)
	httpServer.Handler = handler
	httpServer.ReadHeaderTimeout = chatGPTRequestTimeout

	go func() {
		serveErr := httpServer.Serve(listener)
		if serveErr == nil || errors.Is(serveErr, net.ErrClosed) {
			return
		}

		select {
		case results <- authorizationResult{
			code: "",
			err:  fmt.Errorf("serve callback: %w", serveErr),
		}:
		default:
		}
	}()

	return &callbackServer{httpServer: httpServer, results: results}, nil
}

func handleCallback(
	responseWriter http.ResponseWriter,
	request *http.Request,
	expectedState string,
	results chan<- authorizationResult,
) {
	if request.URL.Path != chatGPTCallbackPath {
		http.NotFound(responseWriter, request)

		return
	}

	if request.URL.Query().Get("state") != expectedState {
		http.Error(responseWriter, "State mismatch", http.StatusBadRequest)

		return
	}

	code := strings.TrimSpace(request.URL.Query().Get("code"))
	if code == "" {
		http.Error(responseWriter, "Missing authorization code", http.StatusBadRequest)

		return
	}

	responseWriter.Header().Set("Content-Type", "text/html; charset=utf-8")
	responseWriter.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(responseWriter, chatGPTSuccessHTML)

	select {
	case results <- authorizationResult{
		code: code,
		err:  nil,
	}:
	default:
	}
}

func (server *callbackServer) close() error {
	if server == nil || server.httpServer == nil {
		return nil
	}

	err := server.httpServer.Close()
	if err != nil {
		return fmt.Errorf("close callback server: %w", err)
	}

	return nil
}

func waitForAuthorizationCode(
	ctx context.Context,
	input io.Reader,
	output io.Writer,
	server *callbackServer,
	expectedState string,
) (string, error) {
	manualInput := startManualInput(input, output)
	if manualInput == nil && server == nil {
		return "", fmt.Errorf("no callback server and stdin is not interactive: %w", os.ErrInvalid)
	}

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for login: %w", ctx.Err())
		case result, ok := <-manualInput:
			if !ok {
				manualInput = nil

				continue
			}

			parsedInput, err := parseAuthorizationInput(result)
			if err != nil {
				return "", err
			}

			err = validateAuthorizationState(parsedInput.state, expectedState)
			if err != nil {
				return "", err
			}

			return parsedInput.code, nil
		case result := <-callbackResults(server):
			if result.err != nil {
				return "", result.err
			}

			return result.code, nil
		}
	}
}

func callbackResults(server *callbackServer) <-chan authorizationResult {
	if server == nil {
		return nil
	}

	return server.results
}

func startManualInput(input io.Reader, output io.Writer) <-chan string {
	if !isInteractiveReader(input) {
		return nil
	}

	results := make(chan string, 1)

	go func() {
		reader := bufio.NewReader(input)

		for {
			_, _ = fmt.Fprint(output, "Paste redirect URL or code and press Enter: ")

			line, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				close(results)

				return
			}

			trimmedLine := strings.TrimSpace(line)
			if trimmedLine != "" {
				results <- trimmedLine

				return
			}

			if errors.Is(err, io.EOF) {
				close(results)

				return
			}
		}
	}()

	return results
}

func isInteractiveReader(input io.Reader) bool {
	file, ok := input.(*os.File)
	if !ok {
		return false
	}

	fileInfo, err := file.Stat()
	if err != nil {
		return false
	}

	return fileInfo.Mode()&os.ModeCharDevice != 0
}

func parseAuthorizationInput(input string) (authorizationInput, error) {
	trimmedInput := strings.TrimSpace(input)
	if trimmedInput == "" {
		return authorizationInput{}, fmt.Errorf("authorization code is required: %w", os.ErrInvalid)
	}

	if strings.Contains(trimmedInput, "://") {
		parsedURL, err := url.Parse(trimmedInput)
		if err != nil {
			return authorizationInput{}, fmt.Errorf("parse redirect url: %w", err)
		}

		code := strings.TrimSpace(parsedURL.Query().Get("code"))
		if code != "" {
			return authorizationInput{
				code:  code,
				state: strings.TrimSpace(parsedURL.Query().Get("state")),
			}, nil
		}
	}

	if strings.Contains(trimmedInput, "code=") {
		queryValues, err := url.ParseQuery(trimmedInput)
		if err == nil {
			code := strings.TrimSpace(queryValues.Get("code"))
			if code != "" {
				return authorizationInput{
					code:  code,
					state: strings.TrimSpace(queryValues.Get("state")),
				}, nil
			}
		}
	}

	if code, state, found := strings.Cut(trimmedInput, "#"); found {
		if strings.TrimSpace(code) == "" {
			return authorizationInput{}, fmt.Errorf("authorization code is required: %w", os.ErrInvalid)
		}

		return authorizationInput{
			code:  strings.TrimSpace(code),
			state: strings.TrimSpace(state),
		}, nil
	}

	return authorizationInput{
		code:  trimmedInput,
		state: "",
	}, nil
}

func validateAuthorizationState(receivedState, expectedState string) error {
	if strings.TrimSpace(receivedState) == "" {
		return nil
	}

	if strings.TrimSpace(receivedState) != strings.TrimSpace(expectedState) {
		return fmt.Errorf("oauth state mismatch: %w", os.ErrInvalid)
	}

	return nil
}

func exchangeAuthorizationCode(
	ctx context.Context,
	httpClient *http.Client,
	tokenURL string,
	redirectURI string,
	code string,
	verifier string,
) (string, error) {
	formValues := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {chatGPTClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		tokenURL,
		strings.NewReader(formValues.Encode()),
	)
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}

	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("send token request: %w", err)
	}

	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		responseBody, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			return "", fmt.Errorf("read token error response: %w", readErr)
		}

		return "", fmt.Errorf(
			"token exchange failed with status %d: %s: %w",
			response.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	var token tokenResponse

	err = json.NewDecoder(response.Body).Decode(&token)
	if err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}

	if strings.TrimSpace(token.AccessToken) == "" {
		return "", fmt.Errorf("token response missing access_token: %w", os.ErrInvalid)
	}

	return token.AccessToken, nil
}
