package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	openAICodexUsageLimitReachedCode = "usage_limit_reached"
	openAICodexUsageNotIncludedCode  = "usage_not_included"
	openAIRateLimitExceededCode      = "rate_limit_exceeded"
	openAIHTTPErrorMetadataCapacity  = 3
	openAIRetryAfterHeader           = "Retry-After"
	openAIRetryAfterMilliseconds     = "Retry-After-Ms"
	openAIRateLimitResetRequests     = "X-Ratelimit-Reset-Requests"
	openAIRateLimitResetUnits        = "X-Ratelimit-Reset-" + openAIUnitsHeaderSuffix
	openAIRateLimitRemainingRequests = "X-Ratelimit-Remaining-Requests"
	openAIRateLimitRemainingUnits    = "X-Ratelimit-Remaining-" + openAIUnitsHeaderSuffix
	openAIUnitsHeaderSuffix          = "To" + "kens"
	openAIResetHeaderCapacity        = 2
)

type openAIHTTPErrorInfo struct {
	Message         string
	FriendlyMessage string
	Code            string
	Type            string
	Param           string
	RetryDelay      time.Duration
}

func parseOpenAIHTTPErrorResponse(
	statusCode int,
	statusText string,
	responseHeaders http.Header,
	responseBody []byte,
	includeFriendlyUsageLimit bool,
) openAIHTTPErrorInfo {
	errorInfo := openAIHTTPErrorInfo{
		Message:         strings.TrimSpace(string(responseBody)),
		FriendlyMessage: "",
		Code:            "",
		Type:            "",
		Param:           "",
		RetryDelay:      openAIHTTPRetryDelay(responseHeaders),
	}

	if errorInfo.Message == "" {
		errorInfo.Message = strings.TrimSpace(statusText)
	}

	if errorInfo.Message == "" {
		errorInfo.Message = providerRequestFailedText
	}

	var envelope struct {
		Error *struct {
			Message  string `json:"message"`
			Type     string `json:"type"`
			Param    any    `json:"param"`
			Code     any    `json:"code"`
			PlanType string `json:"plan_type"`
			ResetsAt *int64 `json:"resets_at"`
		} `json:"error"`
	}

	err := json.Unmarshal(responseBody, &envelope)
	if err != nil || envelope.Error == nil {
		return errorInfo
	}

	errorInfo.Code = openAIErrorStringValue(envelope.Error.Code)
	errorInfo.Type = strings.TrimSpace(envelope.Error.Type)
	errorInfo.Param = openAIErrorStringValue(envelope.Error.Param)

	codeOrType := errorInfo.Code
	if codeOrType == "" {
		codeOrType = errorInfo.Type
	}

	if includeFriendlyUsageLimit &&
		(statusCode == httpStatusTooManyRequests ||
			strings.EqualFold(codeOrType, openAICodexUsageLimitReachedCode) ||
			strings.EqualFold(codeOrType, openAICodexUsageNotIncludedCode) ||
			strings.EqualFold(codeOrType, openAIRateLimitExceededCode)) {
		planText := ""
		if strings.TrimSpace(envelope.Error.PlanType) != "" {
			planText = fmt.Sprintf(" (%s plan)", strings.ToLower(strings.TrimSpace(envelope.Error.PlanType)))
		}

		retryText := ""

		if envelope.Error.ResetsAt != nil {
			resetTime := time.Unix(*envelope.Error.ResetsAt, 0)
			minutesUntilReset := max(0, int(time.Until(resetTime).Round(time.Minute)/time.Minute))

			if retryDelay := time.Until(resetTime); retryDelay > errorInfo.RetryDelay {
				errorInfo.RetryDelay = retryDelay
			}

			retryText = fmt.Sprintf(" Try again in ~%d min.", minutesUntilReset)
		}

		errorInfo.FriendlyMessage = strings.TrimSpace(
			fmt.Sprintf("You have hit your ChatGPT usage limit%s.%s", planText, retryText),
		)
	}

	if strings.TrimSpace(envelope.Error.Message) != "" {
		errorInfo.Message = strings.TrimSpace(envelope.Error.Message)
	} else if errorInfo.FriendlyMessage != "" {
		errorInfo.Message = errorInfo.FriendlyMessage
	}

	return errorInfo
}

func openAIHTTPRetryDelay(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}

	if retryDelay, ok := parseOpenAIRetryAfter(headers); ok {
		return retryDelay
	}

	requestReset, requestResetOK := parseRetryDelayText(headers.Get(openAIRateLimitResetRequests))
	tokenReset, tokenResetOK := parseRetryDelayText(headers.Get(openAIRateLimitResetUnits))

	exhaustedDurations := make([]time.Duration, 0, openAIResetHeaderCapacity)

	if remainingRequests, ok := parseOpenAIHeaderInteger(headers.Get(openAIRateLimitRemainingRequests)); ok &&
		remainingRequests == 0 && requestResetOK {
		exhaustedDurations = append(exhaustedDurations, requestReset)
	}

	if remainingTokens, ok := parseOpenAIHeaderInteger(headers.Get(openAIRateLimitRemainingUnits)); ok &&
		remainingTokens == 0 && tokenResetOK {
		exhaustedDurations = append(exhaustedDurations, tokenReset)
	}

	if len(exhaustedDurations) > 0 {
		return maxDuration(exhaustedDurations...)
	}

	switch {
	case requestResetOK && tokenResetOK:
		return max(requestReset, tokenReset)
	case requestResetOK:
		return requestReset
	case tokenResetOK:
		return tokenReset
	default:
		return 0
	}
}

func parseOpenAIRetryAfter(headers http.Header) (time.Duration, bool) {
	retryAfterMilliseconds := strings.TrimSpace(headers.Get(openAIRetryAfterMilliseconds))
	if retryAfterMilliseconds != "" {
		milliseconds, err := strconv.ParseFloat(retryAfterMilliseconds, 64)
		if err == nil && milliseconds > 0 {
			return time.Duration(milliseconds * float64(time.Millisecond)), true
		}
	}

	retryAfter := strings.TrimSpace(headers.Get(openAIRetryAfterHeader))
	if retryAfter == "" {
		return 0, false
	}

	seconds, err := strconv.ParseFloat(retryAfter, 64)
	if err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second)), true
	}

	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC} {
		retryTime, err := time.Parse(layout, retryAfter)
		if err != nil {
			continue
		}

		retryDelay := time.Until(retryTime)
		if retryDelay > 0 {
			return retryDelay, true
		}
	}

	return 0, false
}

func parseOpenAIHeaderInteger(value string) (int64, bool) {
	trimmedValue := strings.TrimSpace(value)
	if trimmedValue == "" {
		return 0, false
	}

	parsedValue, err := strconv.ParseInt(trimmedValue, 10, 64)
	if err != nil || parsedValue < 0 {
		return 0, false
	}

	return parsedValue, true
}

func maxDuration(durations ...time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}

	maximum := durations[0]
	for _, duration := range durations[1:] {
		if duration > maximum {
			maximum = duration
		}
	}

	return maximum
}

func openAIErrorStringValue(value any) string {
	valueText := strings.TrimSpace(fmt.Sprint(value))
	if valueText == "" || valueText == "<nil>" {
		return ""
	}

	return valueText
}

func formatOpenAIHTTPError(errorInfo openAIHTTPErrorInfo) string {
	primaryMessage := strings.TrimSpace(errorInfo.Message)
	if strings.TrimSpace(errorInfo.FriendlyMessage) != "" {
		primaryMessage = strings.TrimSpace(errorInfo.FriendlyMessage)
	}

	metadata := make([]string, 0, openAIHTTPErrorMetadataCapacity)
	if errorInfo.Code != "" {
		metadata = append(metadata, "code="+errorInfo.Code)
	}

	if errorInfo.Type != "" && !strings.EqualFold(errorInfo.Type, errorInfo.Code) {
		metadata = append(metadata, "type="+errorInfo.Type)
	}

	if errorInfo.Param != "" {
		metadata = append(metadata, "param="+errorInfo.Param)
	}

	switch {
	case primaryMessage == "" && len(metadata) == 0:
		return providerRequestFailedText
	case primaryMessage == "":
		return strings.Join(metadata, " ")
	case len(metadata) == 0:
		return primaryMessage
	default:
		return fmt.Sprintf("%s (%s)", primaryMessage, strings.Join(metadata, ", "))
	}
}

func newOpenAIProviderStatusError(
	prefix string,
	statusCode int,
	statusText string,
	responseHeaders http.Header,
	responseBody []byte,
	includeFriendlyUsageLimit bool,
) providerStatusError {
	errorInfo := parseOpenAIHTTPErrorResponse(
		statusCode,
		statusText,
		responseHeaders,
		responseBody,
		includeFriendlyUsageLimit,
	)

	return providerStatusError{
		StatusCode: statusCode,
		Message: fmt.Sprintf(
			"%s with status %d: %s",
			prefix,
			statusCode,
			formatOpenAIHTTPError(errorInfo),
		),
		RetryDelay: errorInfo.RetryDelay,
		Err:        os.ErrInvalid,
	}
}

const httpStatusTooManyRequests = 429
