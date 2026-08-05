package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const (
	rentryErrorTextMaxLength  = 200
	rentryCSRFTokenMatchCount = 2
)

var (
	errRentryCSRFTokenMissing    = errors.New("rentry csrf token missing")
	errRentryFormStatus          = errors.New("unexpected Rentry form status")
	errRentryCreateStatus        = errors.New("unexpected Rentry create status")
	errRentryCloudflareChallenge = errors.New("rentry Cloudflare challenge")
	rentryCSRFTokenRegexp        = regexp.MustCompile(`name="csrfmiddlewaretoken" value="([^"]+)"`)
)

type rentryCreator interface {
	createEntry(ctx context.Context, text string) (string, error)
}

type rentryBrowserCreator interface {
	createEntry(ctx context.Context, endpoint string, text string) (string, error)
}

type httpRentryClient struct {
	endpoint       string
	transport      http.RoundTripper
	browserCreator rentryBrowserCreator
}

func newRentryClient(
	httpClient *http.Client,
	endpoint string,
	browserCreator rentryBrowserCreator,
) *httpRentryClient {
	client := new(httpRentryClient)
	client.endpoint = strings.TrimSpace(endpoint)

	if httpClient != nil {
		client.transport = httpClient.Transport
	}

	client.browserCreator = browserCreator

	return client
}

func (client *httpRentryClient) createEntry(ctx context.Context, text string) (string, error) {
	endpointURL, err := url.Parse(client.endpoint)
	if err != nil {
		return "", fmt.Errorf("parse Rentry endpoint %q: %w", client.endpoint, err)
	}

	httpClient, err := client.newHTTPClient()
	if err != nil {
		return "", err
	}

	csrfToken, err := client.loadCSRFToken(ctx, httpClient, endpointURL)
	if err != nil {
		if isRentryCloudflareChallenge(err) {
			return client.createEntryWithBrowserFallback(ctx, text, err)
		}

		return "", err
	}

	entryURL, err := client.submitEntry(ctx, httpClient, endpointURL, csrfToken, text)
	if err != nil {
		if isRentryCloudflareChallenge(err) {
			return client.createEntryWithBrowserFallback(ctx, text, err)
		}

		return "", err
	}

	return entryURL, nil
}

func (client *httpRentryClient) createEntryWithBrowserFallback(
	ctx context.Context,
	text string,
	httpErr error,
) (string, error) {
	if client.browserCreator == nil {
		return "", httpErr
	}

	entryURL, browserErr := client.browserCreator.createEntry(ctx, client.endpoint, text)
	if browserErr != nil {
		return "", fmt.Errorf("%w; browser fallback: %w", httpErr, browserErr)
	}

	return entryURL, nil
}

func isRentryCloudflareChallenge(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, errRentryCloudflareChallenge) {
		return true
	}

	errorText := strings.ToLower(err.Error())

	if !strings.Contains(errorText, "just a moment") {
		return false
	}

	var statusError *rentryStatusError

	if errors.As(err, &statusError) {
		switch statusError.statusCode {
		case http.StatusForbidden, http.StatusServiceUnavailable:
			return true
		}

		return false
	}

	return true
}

func isCloudflareChallengeBody(responseBody []byte) bool {
	return strings.Contains(strings.ToLower(string(responseBody)), "just a moment")
}

func (client *httpRentryClient) newHTTPClient() (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create Rentry cookie jar: %w", err)
	}

	httpClient := new(http.Client)
	httpClient.Transport = client.transport
	httpClient.Jar = jar
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return httpClient, nil
}

func (client *httpRentryClient) loadCSRFToken(
	ctx context.Context,
	httpClient *http.Client,
	endpointURL *url.URL,
) (string, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build Rentry form request: %w", err)
	}

	httpResponse, err := httpClient.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("request Rentry form: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return "", fmt.Errorf("read Rentry form response: %w", err)
	}

	if httpResponse.StatusCode != http.StatusOK {
		if isCloudflareChallengeBody(responseBody) {
			return "", newRentryStatusError(httpResponse.StatusCode, responseBody)
		}

		return "", fmt.Errorf("%w: %d", errRentryFormStatus, httpResponse.StatusCode)
	}

	csrfToken, err := extractRentryCSRFToken(responseBody)
	if err != nil {
		return "", fmt.Errorf("extract Rentry CSRF token: %w", err)
	}

	return csrfToken, nil
}

func (client *httpRentryClient) submitEntry(
	ctx context.Context,
	httpClient *http.Client,
	endpointURL *url.URL,
	csrfToken string,
	text string,
) (string, error) {
	formValues := url.Values{
		"csrfmiddlewaretoken": {csrfToken},
		messageTextKey:        {text},
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpointURL.String(),
		strings.NewReader(formValues.Encode()),
	)
	if err != nil {
		return "", fmt.Errorf("build Rentry create request: %w", err)
	}

	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpRequest.Header.Set("Referer", endpointURL.String())

	httpResponse, err := httpClient.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("create Rentry entry: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	if httpResponse.StatusCode < http.StatusMultipleChoices ||
		httpResponse.StatusCode >= http.StatusBadRequest {
		responseBody, readErr := io.ReadAll(httpResponse.Body)
		if readErr != nil {
			return "", fmt.Errorf("read Rentry error response: %w", readErr)
		}

		return "", newRentryStatusError(httpResponse.StatusCode, responseBody)
	}

	responseBody, readErr := io.ReadAll(httpResponse.Body)
	if readErr != nil {
		return "", fmt.Errorf("read Rentry create response: %w", readErr)
	}

	if isCloudflareChallengeBody(responseBody) {
		return "", newRentryStatusError(httpResponse.StatusCode, responseBody)
	}

	locationURL, err := httpResponse.Location()
	if err != nil {
		return "", fmt.Errorf("read Rentry location header: %w", err)
	}

	return locationURL.String(), nil
}

type rentryStatusError struct {
	statusCode int
	message    string
}

func (statusError *rentryStatusError) Error() string {
	return statusError.message
}

func (statusError *rentryStatusError) Unwrap() error {
	return errRentryCreateStatus
}

func newRentryStatusError(statusCode int, responseBody []byte) error {
	errorText := strings.Join(strings.Fields(string(responseBody)), " ")
	errorText = truncateRunes(errorText, rentryErrorTextMaxLength)

	message := fmt.Sprintf("%s: %d", errRentryCreateStatus, statusCode)
	if errorText != "" {
		message = fmt.Sprintf("%s: %d: %s", errRentryCreateStatus, statusCode, errorText)
	}

	return &rentryStatusError{
		statusCode: statusCode,
		message:    message,
	}
}

func extractRentryCSRFToken(responseBody []byte) (string, error) {
	matches := rentryCSRFTokenRegexp.FindSubmatch(responseBody)
	if len(matches) < rentryCSRFTokenMatchCount {
		return "", fmt.Errorf("%w: %w", errRentryCSRFTokenMissing, os.ErrInvalid)
	}

	return string(matches[1]), nil
}
