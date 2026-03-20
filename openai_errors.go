package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	openAICodexUsageLimitReachedCode = "usage_limit_reached"
	openAICodexUsageNotIncludedCode  = "usage_not_included"
	openAIRateLimitExceededCode      = "rate_limit_exceeded"
	openAIHTTPErrorMetadataCapacity  = 3
)

type openAIHTTPErrorInfo struct {
	Message         string
	FriendlyMessage string
	Code            string
	Type            string
	Param           string
}

func parseOpenAIHTTPErrorResponse(
	statusCode int,
	statusText string,
	responseBody []byte,
	includeFriendlyUsageLimit bool,
) openAIHTTPErrorInfo {
	errorInfo := openAIHTTPErrorInfo{
		Message:         strings.TrimSpace(string(responseBody)),
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
	responseBody []byte,
	includeFriendlyUsageLimit bool,
) providerStatusError {
	errorInfo := parseOpenAIHTTPErrorResponse(
		statusCode,
		statusText,
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
