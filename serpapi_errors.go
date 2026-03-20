package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const (
	serpAPISearchStatusSuccess    = "Success"
	serpAPISearchStatusProcessing = "Processing"
	serpAPISearchStatusQueued     = "Queued"
	serpAPISearchStatusError      = "Error"
)

type serpAPIErrorResponse struct {
	Error string `json:"error"`
}

func parseSerpAPIHTTPErrorMessage(
	statusCode int,
	statusText string,
	responseBody []byte,
) string {
	var response serpAPIErrorResponse

	err := json.Unmarshal(responseBody, &response)
	if err == nil {
		if errorText := strings.TrimSpace(response.Error); errorText != "" {
			return errorText
		}
	}

	if bodyText := strings.TrimSpace(string(responseBody)); bodyText != "" {
		return bodyText
	}

	switch statusCode {
	case http.StatusBadRequest:
		return "We were unable to process the request."
	case http.StatusUnauthorized:
		return "No valid API key provided."
	case http.StatusForbidden:
		return "The account associated with this API key doesn't have permission to perform the request."
	case http.StatusNotFound:
		return "The requested resource doesn't exist."
	case http.StatusGone:
		return "The search expired and has been deleted from the archive."
	case http.StatusTooManyRequests:
		return "The request exceeded the hourly throughput limit or the account has run out of searches."
	case http.StatusInternalServerError, http.StatusServiceUnavailable:
		return "Something went wrong on SerpApi's end."
	}

	if trimmedStatus := strings.TrimSpace(statusText); trimmedStatus != "" {
		return trimmedStatus
	}

	return providerRequestFailedText
}

func newSerpAPIProviderError(
	prefix string,
	statusCode int,
	statusText string,
	responseBody []byte,
) error {
	statusErr := providerStatusError{
		StatusCode: statusCode,
		Message: fmt.Sprintf(
			"%s with status %d: %s",
			prefix,
			statusCode,
			parseSerpAPIHTTPErrorMessage(statusCode, statusText, responseBody),
		),
		Err: os.ErrInvalid,
	}

	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return providerAPIKeyError{Err: statusErr}
	default:
		return statusErr
	}
}

func newSerpAPISearchStatusError(imageURL string, status string, responseError string) error {
	trimmedStatus := strings.TrimSpace(status)

	trimmedError := strings.TrimSpace(responseError)
	if trimmedError == "" {
		trimmedError = "Search did not complete successfully."
	}

	return providerStatusError{
		StatusCode: http.StatusServiceUnavailable,
		Message: fmt.Sprintf(
			"SerpApi Google Lens search for %q returned status %q: %s",
			imageURL,
			trimmedStatus,
			trimmedError,
		),
		Err: os.ErrInvalid,
	}
}

func shouldRetrySerpAPIAttemptWithNextKey(err error) bool {
	if err == nil {
		return false
	}

	var apiKeyErr providerAPIKeyError
	if errors.As(err, &apiKeyErr) {
		return true
	}

	var statusErr providerStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusTooManyRequests
	}

	return false
}
