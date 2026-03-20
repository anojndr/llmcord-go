package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"google.golang.org/genai"
)

const providerRequestFailedText = "Request failed"
const geminiRetryInfoType = "type.googleapis.com/google.rpc.RetryInfo"
const retryDelayPatternMatchParts = 2

var geminiRetryDelayPattern = regexp.MustCompile(
	`(?i)\bretry in\s+([0-9]+(?:\.[0-9]+)?(?:ns|us|µs|μs|ms|s|m|h))\b`,
)

type providerStatusError struct {
	StatusCode int
	Message    string
	RetryDelay time.Duration
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

func waitForRetryDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for retry delay: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func retryDelayForProvider(apiKind providerAPIKind, err error) (time.Duration, bool) {
	switch apiKind {
	case providerAPIKindGemini:
		return geminiRetryDelay(err)
	case providerAPIKindOpenAI, providerAPIKindOpenAICodex:
		return openAIRetryDelay(err)
	default:
		return 0, false
	}
}

func openAIRetryDelay(err error) (time.Duration, bool) {
	var statusErr providerStatusError
	if !errors.As(err, &statusErr) || statusErr.RetryDelay <= 0 {
		return 0, false
	}

	return statusErr.RetryDelay, true
}

func geminiRetryDelay(err error) (time.Duration, bool) {
	apiErr, found := geminiAPIError(err)
	if !found {
		return 0, false
	}

	if apiErr.Code != httpStatusTooManyRequests &&
		!strings.EqualFold(strings.TrimSpace(apiErr.Status), "RESOURCE_EXHAUSTED") {
		return 0, false
	}

	retryDelay := time.Duration(0)

	for _, detail := range apiErr.Details {
		if !strings.EqualFold(strings.TrimSpace(stringifyValue(detail["@type"])), geminiRetryInfoType) {
			continue
		}

		candidateDelay, ok := parseRetryDelayText(stringifyValue(detail["retryDelay"]))
		if ok && candidateDelay > retryDelay {
			retryDelay = candidateDelay
		}
	}

	messageDelay, ok := geminiRetryDelayFromMessage(apiErr.Message)
	if ok && messageDelay > retryDelay {
		retryDelay = messageDelay
	}

	if retryDelay <= 0 {
		return 0, false
	}

	return retryDelay, true
}

func geminiAPIError(err error) (*genai.APIError, bool) {
	var apiErrPtr *genai.APIError
	if errors.As(err, &apiErrPtr) && apiErrPtr != nil {
		return apiErrPtr, true
	}

	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return &apiErr, true
	}

	return nil, false
}

func geminiRetryDelayFromMessage(message string) (time.Duration, bool) {
	matches := geminiRetryDelayPattern.FindStringSubmatch(message)
	if len(matches) != retryDelayPatternMatchParts {
		return 0, false
	}

	return parseRetryDelayText(matches[1])
}

func parseRetryDelayText(delayText string) (time.Duration, bool) {
	retryDelay, err := time.ParseDuration(strings.TrimSpace(delayText))
	if err != nil || retryDelay <= 0 {
		return 0, false
	}

	return retryDelay, true
}
