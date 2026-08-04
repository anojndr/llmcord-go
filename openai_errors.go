package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	openAIRateLimitExceededCode     = "rate_limit_exceeded"
	openAIHTTPErrorBodyPreviewRunes = 512
	openAIHTTPErrorMetadataCapacity = 3
	providerRequestFailedText       = "Request failed"
)

type openAIHTTPErrorInfo struct {
	Message         string
	FriendlyMessage string
	Code            string
	Type            string
	Param           string
}

type openAIHTTPErrorEnvelope struct {
	Error *openAIHTTPErrorPayload `json:"error"`
}

type openAIHTTPErrorPayload struct {
	Message  string `json:"message"`
	Type     string `json:"type"`
	Param    any    `json:"param"`
	Code     any    `json:"code"`
	PlanType string `json:"plan_type"`
	ResetsAt *int64 `json:"resets_at"`
}

func parseOpenAIHTTPErrorResponse(
	statusCode int,
	statusText string,
	_ http.Header,
	responseBody []byte,
	includeFriendlyUsageLimit bool,
) openAIHTTPErrorInfo {
	errorInfo := defaultOpenAIHTTPErrorInfo(statusText, responseBody)

	errorPayload, ok := parseOpenAIHTTPErrorPayload(responseBody)
	if !ok {
		return errorInfo
	}

	errorInfo = updateOpenAIHTTPErrorInfoFromPayload(errorInfo, errorPayload)

	errorInfo = applyOpenAIFriendlyUsageLimit(
		errorInfo,
		errorPayload,
		statusCode,
		includeFriendlyUsageLimit,
	)
	if strings.TrimSpace(errorPayload.Message) != "" {
		errorInfo.Message = strings.TrimSpace(errorPayload.Message)
	} else if errorInfo.FriendlyMessage != "" {
		errorInfo.Message = errorInfo.FriendlyMessage
	}

	return errorInfo
}

func defaultOpenAIHTTPErrorInfo(
	statusText string,
	responseBody []byte,
) openAIHTTPErrorInfo {
	errorInfo := openAIHTTPErrorInfo{
		Message:         openAIHTTPErrorBodyMessage(responseBody),
		FriendlyMessage: "",
		Code:            "",
		Type:            "",
		Param:           "",
	}

	if errorInfo.Message == "" {
		errorInfo.Message = strings.TrimSpace(statusText)
	}

	if errorInfo.Message == "" {
		errorInfo.Message = providerRequestFailedText
	}

	return errorInfo
}

func openAIHTTPErrorBodyMessage(responseBody []byte) string {
	if len(responseBody) == 0 {
		return ""
	}

	trimmedBody := bytes.TrimSpace(responseBody)
	if len(trimmedBody) == 0 {
		return ""
	}

	bodyText := strings.TrimSpace(string(bytes.ToValidUTF8(trimmedBody, []byte{})))
	if bodyText == "" {
		return ""
	}

	if runeCount(bodyText) > openAIHTTPErrorBodyPreviewRunes {
		return ""
	}

	if openAIHTTPErrorBodyLooksOpaque(bodyText) {
		return ""
	}

	return bodyText
}

func openAIHTTPErrorBodyLooksOpaque(bodyText string) bool {
	fields := strings.Fields(bodyText)
	if len(fields) == 0 {
		return false
	}

	longestFieldRunes := 0

	for _, field := range fields {
		fieldRunes := runeCount(field)
		if fieldRunes > longestFieldRunes {
			longestFieldRunes = fieldRunes
		}
	}

	return longestFieldRunes > openAIHTTPErrorBodyPreviewRunes/2
}

func parseOpenAIHTTPErrorPayload(responseBody []byte) (*openAIHTTPErrorPayload, bool) {
	var envelope openAIHTTPErrorEnvelope

	err := json.Unmarshal(responseBody, &envelope)
	if err != nil || envelope.Error == nil {
		return nil, false
	}

	return envelope.Error, true
}

func updateOpenAIHTTPErrorInfoFromPayload(
	errorInfo openAIHTTPErrorInfo,
	errorPayload *openAIHTTPErrorPayload,
) openAIHTTPErrorInfo {
	errorInfo.Code = openAIErrorStringValue(errorPayload.Code)
	errorInfo.Type = strings.TrimSpace(errorPayload.Type)
	errorInfo.Param = openAIErrorStringValue(errorPayload.Param)

	return errorInfo
}

func applyOpenAIFriendlyUsageLimit(
	errorInfo openAIHTTPErrorInfo,
	errorPayload *openAIHTTPErrorPayload,
	statusCode int,
	includeFriendlyUsageLimit bool,
) openAIHTTPErrorInfo {
	if !includeFriendlyUsageLimit || !openAIHTTPErrorIsUsageLimit(statusCode, errorInfo) {
		return errorInfo
	}

	errorInfo.FriendlyMessage = openAIFriendlyUsageLimitMessage(errorPayload.PlanType, errorPayload.ResetsAt)

	return errorInfo
}

func openAIHTTPErrorIsUsageLimit(statusCode int, errorInfo openAIHTTPErrorInfo) bool {
	codeOrType := errorInfo.Code
	if codeOrType == "" {
		codeOrType = errorInfo.Type
	}

	return statusCode == httpStatusTooManyRequests ||
		strings.EqualFold(codeOrType, openAIRateLimitExceededCode)
}

func openAIFriendlyUsageLimitMessage(
	planType string,
	resetsAt *int64,
) string {
	planText := ""
	if strings.TrimSpace(planType) != "" {
		planText = fmt.Sprintf(" (%s plan)", strings.ToLower(strings.TrimSpace(planType)))
	}

	retryText := ""

	if resetsAt != nil {
		resetTime := time.Unix(*resetsAt, 0)

		minutesUntilReset := max(0, int(time.Until(resetTime).Round(time.Minute)/time.Minute))

		retryText = fmt.Sprintf(" Try again in ~%d min.", minutesUntilReset)
	}

	return strings.TrimSpace(
		fmt.Sprintf("You have hit your ChatGPT usage limit%s.%s", planText, retryText),
	)
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
		Err: os.ErrInvalid,
	}
}

const httpStatusTooManyRequests = 429
