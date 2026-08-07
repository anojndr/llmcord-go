package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	gistErrorTextMaxLength = 200
	gistResponseMaxLength  = 64 * 1024
)

var (
	errGistCreateStatus    = errors.New("unexpected GitHub gist create status")
	errGistInvalidResponse = errors.New("invalid GitHub gist response")
)

type gistCreator interface {
	createGist(ctx context.Context, text string) (string, error)
}

type httpGistClient struct {
	transport   http.RoundTripper
	rotator     *apiKeyRotator
	apiKeys     []string
	endpoint    string
	description string
	filename    string
	public      bool
}

type gistCreateRequest struct {
	Description string              `json:"description"`
	Public      bool                `json:"public"`
	Files       map[string]gistFile `json:"files"`
}

type gistFile struct {
	Content string `json:"content"`
}

type gistCreateResponse struct {
	HTMLURL string `json:"html_url"`
}

func newGistClient(
	httpClient *http.Client,
	endpoint string,
	apiKeys []string,
	description string,
	filename string,
	public bool,
) *httpGistClient {
	client := new(httpGistClient)
	client.endpoint = strings.TrimSpace(endpoint)
	client.description = strings.TrimSpace(description)
	client.filename = strings.TrimSpace(filename)
	client.public = public

	if httpClient != nil {
		client.transport = httpClient.Transport
	}

	client.apiKeys = normalizeAPIKeys(apiKeys)
	if len(client.apiKeys) > 0 {
		client.rotator = newAPIKeyRotator()
	}

	return client
}

func (client *httpGistClient) createGist(ctx context.Context, text string) (string, error) {
	requestBody, err := json.Marshal(gistCreateRequest{
		Description: client.description,
		Public:      client.public,
		Files:       map[string]gistFile{client.filename: {Content: text}},
	})
	if err != nil {
		return "", fmt.Errorf("build GitHub gist create request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.endpoint,
		bytes.NewReader(requestBody),
	)
	if err != nil {
		return "", fmt.Errorf("build GitHub gist create request: %w", err)
	}

	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/vnd.github+json")
	httpRequest.Header.Set("X-Github-Api-Version", "2022-11-28")
	httpRequest.Header.Set(userAgentHeader, "llmcord-go")

	if client.rotator != nil {
		rotatedKeys := client.rotator.rotate(client.apiKeys)
		if len(rotatedKeys) > 0 {
			httpRequest.Header.Set("Authorization", "Bearer "+rotatedKeys[0])
		}
	}

	httpResponse, err := client.transport.RoundTrip(httpRequest)
	if err != nil {
		return "", fmt.Errorf("create GitHub gist: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, gistResponseMaxLength))
	if err != nil {
		return "", fmt.Errorf("read GitHub gist response: %w", err)
	}

	if httpResponse.StatusCode != http.StatusCreated {
		return "", newGistStatusError(httpResponse.StatusCode, responseBody)
	}

	var response gistCreateResponse

	err = json.Unmarshal(responseBody, &response)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errGistInvalidResponse, err)
	}

	if !isGistURL(response.HTMLURL) {
		return "", fmt.Errorf("%w: %q", errGistInvalidResponse, response.HTMLURL)
	}

	return response.HTMLURL, nil
}

type gistStatusError struct {
	statusCode int
	message    string
}

func (statusError *gistStatusError) Error() string {
	return statusError.message
}

func (statusError *gistStatusError) Unwrap() error {
	return errGistCreateStatus
}

func newGistStatusError(statusCode int, responseBody []byte) error {
	errorText := strings.Join(strings.Fields(string(responseBody)), " ")
	errorText = truncateRunes(errorText, gistErrorTextMaxLength)

	message := fmt.Sprintf("%s: %d", errGistCreateStatus, statusCode)
	if errorText != "" {
		message = fmt.Sprintf("%s: %d: %s", errGistCreateStatus, statusCode, errorText)
	}

	return &gistStatusError{
		statusCode: statusCode,
		message:    message,
	}
}

// isGistURL reports whether responseURL looks like a gist location the GitHub
// API could legitimately return: an http(s) URL with a non-empty path after the
// leading slash. It exists to catch malformed or non-URL responses before a
// gist URL is handed to users.
func isGistURL(responseURL string) bool {
	parsedURL, err := url.Parse(responseURL)
	if err != nil {
		return false
	}

	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return false
	}

	switch strings.ToLower(parsedURL.Scheme) {
	case "http", "https":
	default:
		return false
	}

	return strings.Trim(parsedURL.Path, "/") != ""
}
