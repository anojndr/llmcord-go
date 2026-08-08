package providers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const (
	// SerpAPISearchStatusSuccess is the completed search status.
	SerpAPISearchStatusSuccess = "Success"
	// SerpAPISearchStatusProcessing is the in-progress search status.
	SerpAPISearchStatusProcessing = "Processing"
	// SerpAPISearchStatusQueued is the queued search status.
	SerpAPISearchStatusQueued = "Queued"
	// SerpAPISearchStatusError is the failed search status.
	SerpAPISearchStatusError = "Error"
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
	case http.StatusTooManyRequests:
		return "SerpApi rate limit exceeded"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "invalid SerpApi API key"
	default:
		return statusText
	}
}

// NewSerpAPIProviderError builds a SerpAPI provider status error.
func NewSerpAPIProviderError(
	prefix string,
	statusCode int,
	statusText string,
	responseBody []byte,
) error {
	statusErr := StatusError{
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
		return APIKeyError{Err: statusErr}
	default:
		return statusErr
	}
}

// NewSerpAPISearchStatusError builds a SerpAPI search status error.
func NewSerpAPISearchStatusError(imageURL, status, responseError string) error {
	trimmedStatus := strings.TrimSpace(status)

	trimmedError := strings.TrimSpace(responseError)
	if trimmedError == "" {
		trimmedError = "Search did not complete successfully."
	}

	return StatusError{
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
