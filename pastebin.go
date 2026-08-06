package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const (
	pastebinErrorTextMaxLength = 200
	pastebinAPIOptionPaste     = "paste"
	pastebinResponseMaxLength  = 1024
)

var (
	errPastebinCreateStatus      = errors.New("unexpected Pastebin create status")
	errPastebinInvalidResponse   = errors.New("invalid Pastebin response")
	errPastebinInvalidExpireDate = errors.New("invalid Pastebin expire date")
)

type pastebinCreator interface {
	createPaste(ctx context.Context, text string) (string, error)
}

type httpPastebinClient struct {
	endpoint   string
	transport  http.RoundTripper
	devKey     string
	pasteName  string
	expireDate string
}

func newPastebinClient(
	httpClient *http.Client,
	endpoint string,
	devKey string,
	pasteName string,
	expireDate string,
) *httpPastebinClient {
	client := new(httpPastebinClient)
	client.endpoint = strings.TrimSpace(endpoint)
	client.devKey = strings.TrimSpace(devKey)
	client.pasteName = strings.TrimSpace(pasteName)
	client.expireDate = strings.TrimSpace(expireDate)

	if httpClient != nil {
		client.transport = httpClient.Transport
	}

	return client
}

func (client *httpPastebinClient) createPaste(ctx context.Context, text string) (string, error) {
	formValues := url.Values{
		"api_dev_key":           {client.devKey},
		"api_option":            {pastebinAPIOptionPaste},
		"api_paste_code":        {text},
		"api_paste_name":        {client.pasteName},
		"api_paste_expire_date": {client.expireDate},
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.endpoint,
		strings.NewReader(formValues.Encode()),
	)
	if err != nil {
		return "", fmt.Errorf("build Pastebin create request: %w", err)
	}

	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	httpResponse, err := client.transport.RoundTrip(httpRequest)
	if err != nil {
		return "", fmt.Errorf("create Pastebin paste: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, pastebinResponseMaxLength))
	if err != nil {
		return "", fmt.Errorf("read Pastebin create response: %w", err)
	}

	if httpResponse.StatusCode != http.StatusOK {
		return "", newPastebinStatusError(httpResponse.StatusCode, responseBody)
	}

	responseText := strings.TrimSpace(string(responseBody))
	if strings.HasPrefix(strings.ToLower(responseText), "bad api request") {
		return "", newPastebinStatusError(httpResponse.StatusCode, responseBody)
	}

	pasteURL, err := url.Parse(responseText)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errPastebinInvalidResponse, err)
	}

	if !isPastebinPasteURL(pasteURL) {
		return "", fmt.Errorf("%w: %q", errPastebinInvalidResponse, responseText)
	}

	return pasteURL.String(), nil
}

type pastebinStatusError struct {
	statusCode int
	message    string
}

func (statusError *pastebinStatusError) Error() string {
	return statusError.message
}

func (statusError *pastebinStatusError) Unwrap() error {
	return errPastebinCreateStatus
}

func newPastebinStatusError(statusCode int, responseBody []byte) error {
	errorText := strings.Join(strings.Fields(string(responseBody)), " ")
	errorText = truncateRunes(errorText, pastebinErrorTextMaxLength)

	message := fmt.Sprintf("%s: %d", errPastebinCreateStatus, statusCode)
	if errorText != "" {
		message = fmt.Sprintf("%s: %d: %s", errPastebinCreateStatus, statusCode, errorText)
	}

	return &pastebinStatusError{
		statusCode: statusCode,
		message:    message,
	}
}

// isPastebinPasteURL reports whether parsedURL looks like a paste location the
// Pastebin API could legitimately return: an http(s) URL with a non-empty path
// after the leading slash. It exists to catch malformed or non-URL responses
// from the API before a paste URL is handed to users.
func isPastebinPasteURL(parsedURL *url.URL) bool {
	if parsedURL == nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return false
	}

	switch strings.ToLower(parsedURL.Scheme) {
	case "http", "https":
	default:
		return false
	}

	return strings.Trim(parsedURL.Path, "/") != ""
}

func validatePastebinExpireDate(expireDate string) error {
	expireDate = strings.TrimSpace(expireDate)
	if expireDate == "" {
		return nil
	}

	expireDates := [...]string{"N", "10M", "1H", "1D", "1W", "2W", "1M", "6M", "1Y"}

	if slices.Contains(expireDates[:], expireDate) {
		return nil
	}

	return fmt.Errorf("%w: %q", errPastebinInvalidExpireDate, expireDate)
}
