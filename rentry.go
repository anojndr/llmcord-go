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
	errRentryCSRFTokenMissing = errors.New("rentry csrf token missing")
	errRentryFormStatus       = errors.New("unexpected Rentry form status")
	errRentryCreateStatus     = errors.New("unexpected Rentry create status")
	rentryCSRFTokenRegexp     = regexp.MustCompile(`name="csrfmiddlewaretoken" value="([^"]+)"`)
)

type rentryClient interface {
	createEntry(ctx context.Context, text string) (string, error)
}

type httpRentryClient struct {
	endpoint  string
	transport http.RoundTripper
}

func newRentryClient(httpClient *http.Client, endpoint string) *httpRentryClient {
	client := new(httpRentryClient)
	client.endpoint = strings.TrimSpace(endpoint)

	if httpClient != nil {
		client.transport = httpClient.Transport
	}

	return client
}

func (client *httpRentryClient) createEntry(ctx context.Context, text string) (string, error) {
	requestContext, cancel := context.WithTimeout(ctx, rentryRequestTimeout)
	defer cancel()

	endpointURL, err := url.Parse(client.endpoint)
	if err != nil {
		return "", fmt.Errorf("parse Rentry endpoint %q: %w", client.endpoint, err)
	}

	httpClient, err := client.newHTTPClient()
	if err != nil {
		return "", err
	}

	csrfToken, err := client.loadCSRFToken(requestContext, httpClient, endpointURL)
	if err != nil {
		return "", err
	}

	return client.submitEntry(requestContext, httpClient, endpointURL, csrfToken, text)
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

	if httpResponse.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: %d", errRentryFormStatus, httpResponse.StatusCode)
	}

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return "", fmt.Errorf("read Rentry form response: %w", err)
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
		"text":                {text},
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

	locationURL, err := httpResponse.Location()
	if err != nil {
		return "", fmt.Errorf("read Rentry location header: %w", err)
	}

	return locationURL.String(), nil
}

func newRentryStatusError(statusCode int, responseBody []byte) error {
	errorText := strings.Join(strings.Fields(string(responseBody)), " ")
	errorText = truncateRunes(errorText, rentryErrorTextMaxLength)

	if errorText == "" {
		return fmt.Errorf("%w: %d", errRentryCreateStatus, statusCode)
	}

	return fmt.Errorf("%w: %d: %s", errRentryCreateStatus, statusCode, errorText)
}

func extractRentryCSRFToken(responseBody []byte) (string, error) {
	matches := rentryCSRFTokenRegexp.FindSubmatch(responseBody)
	if len(matches) < rentryCSRFTokenMatchCount {
		return "", fmt.Errorf("%w: %w", errRentryCSRFTokenMissing, os.ErrInvalid)
	}

	return string(matches[1]), nil
}
