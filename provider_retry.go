package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"google.golang.org/genai"
)

type apiKeyRetryableError interface {
	retryWithNextAPIKey() bool
}

type providerStatusError struct {
	StatusCode int
	Message    string
	Err        error
}

func (err providerStatusError) Error() string {
	return err.Message
}

func (err providerStatusError) Unwrap() error {
	if err.Err == nil {
		return os.ErrInvalid
	}

	return err.Err
}

func (err providerStatusError) retryWithNextAPIKey() bool {
	switch err.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

type providerAPIKeyError struct {
	Err error
}

func (err providerAPIKeyError) Error() string {
	if err.Err == nil {
		return "provider API key error"
	}

	return err.Err.Error()
}

func (err providerAPIKeyError) Unwrap() error {
	if err.Err == nil {
		return os.ErrInvalid
	}

	return err.Err
}

func (providerAPIKeyError) retryWithNextAPIKey() bool {
	return true
}

func shouldRetryWithNextAPIKey(err error) bool {
	var retryableErr apiKeyRetryableError
	if errors.As(err, &retryableErr) {
		return retryableErr.retryWithNextAPIKey()
	}

	var geminiErr genai.APIError
	if errors.As(err, &geminiErr) {
		return shouldRetryGeminiAPIKey(geminiErr)
	}

	return false
}

func shouldRetryGeminiAPIKey(err genai.APIError) bool {
	switch err.Code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	case http.StatusBadRequest:
		errorText := strings.ToLower(strings.TrimSpace(err.Message + " " + fmt.Sprint(err.Details)))

		return strings.Contains(errorText, "api key") || strings.Contains(errorText, "api_key")
	default:
		return false
	}
}
