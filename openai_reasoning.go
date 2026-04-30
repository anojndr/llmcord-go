package main

import (
	"maps"
	"strings"
)

const (
	openAIReasoningEffortNone     = "none"
	openAIReasoningEffortMinimal  = "minimal"
	openAIReasoningEffortLow      = defaultProviderVerbosityLow
	openAIReasoningEffortMedium   = openAICodexVerbosityMedium
	openAIReasoningEffortXHigh    = "xhigh"
	openAIReasoningSummaryAuto    = "auto"
	openAIReasoningSummaryConcise = "concise"
	openAIReasoningModelGPT51     = "gpt-5.1"
	openAIReasoningModelGPT54     = "gpt-5.4"
)

func normalizeOpenAIResponsesModelAlias(model string, extraBody map[string]any) (string, map[string]any) {
	resolvedModel, reasoningEffort, hasAlias := openAIReasoningEffortAlias(model)
	if !hasAlias {
		return model, extraBody
	}

	normalizedExtraBody := maps.Clone(extraBody)
	if normalizedExtraBody == nil {
		normalizedExtraBody = make(map[string]any, 1)
	}

	reasoningConfig := openAIReasoningConfigExtraBody(normalizedExtraBody)
	reasoningConfig["effort"] = normalizeOpenAIReasoningEffort(resolvedModel, reasoningEffort)
	normalizedExtraBody["reasoning"] = reasoningConfig
	delete(normalizedExtraBody, "reasoning_effort")

	return resolvedModel, normalizedExtraBody
}

func normalizeOpenAIChatCompletionsModelAlias(model string, extraBody map[string]any) (string, map[string]any) {
	resolvedModel, reasoningEffort, hasAlias := openAIReasoningEffortAlias(model)
	if !hasAlias {
		return model, extraBody
	}

	normalizedExtraBody := maps.Clone(extraBody)
	if normalizedExtraBody == nil {
		normalizedExtraBody = make(map[string]any, 1)
	}

	normalizedExtraBody["reasoning_effort"] = normalizeOpenAIReasoningEffort(
		resolvedModel,
		reasoningEffort,
	)

	return resolvedModel, normalizedExtraBody
}

func normalizeOpenAIResponsesExtraBody(model string, extraBody map[string]any) map[string]any {
	normalizedExtraBody := maps.Clone(extraBody)
	if normalizedExtraBody == nil {
		normalizedExtraBody = make(map[string]any, 1)
	}

	if reasoningEffort, ok := normalizedExtraBody["reasoning_effort"]; ok {
		reasoningConfig := nestedRequestBodyMap(normalizedExtraBody, "reasoning")
		if _, exists := reasoningConfig["effort"]; !exists {
			reasoningConfig["effort"] = reasoningEffort
		}

		delete(normalizedExtraBody, "reasoning_effort")
	}

	if reasoningSummary, ok := normalizedExtraBody["reasoning_summary"]; ok {
		reasoningConfig := nestedRequestBodyMap(normalizedExtraBody, "reasoning")
		if _, exists := reasoningConfig["summary"]; !exists {
			reasoningConfig["summary"] = reasoningSummary
		}

		delete(normalizedExtraBody, "reasoning_summary")
	}

	existingReasoningConfig, reasoningConfigExists := normalizedExtraBody["reasoning"]
	if !reasoningConfigExists || existingReasoningConfig == nil {
		normalizedExtraBody["reasoning"] = map[string]any{
			"summary": openAIReasoningSummaryAuto,
		}

		return normalizedExtraBody
	}

	reasoningConfig, ok := existingReasoningConfig.(map[string]any)
	if !ok {
		return normalizedExtraBody
	}

	clonedReasoningConfig := maps.Clone(reasoningConfig)
	if effort, effortOK := clonedReasoningConfig["effort"].(string); effortOK {
		clonedReasoningConfig["effort"] = normalizeOpenAIReasoningEffort(model, effort)
	}

	if _, summaryExists := clonedReasoningConfig["summary"]; !summaryExists {
		clonedReasoningConfig["summary"] = openAIReasoningSummaryAuto
	}

	normalizedExtraBody["reasoning"] = clonedReasoningConfig

	return normalizedExtraBody
}

func openAIReasoningEffortAlias(model string) (string, string, bool) {
	model = trimConfiguredModelLocalSuffixes(model)

	lowerModel := strings.ToLower(model)
	for _, alias := range []struct {
		suffix          string
		reasoningEffort string
	}{
		{
			suffix:          "-none",
			reasoningEffort: openAIReasoningEffortNone,
		},
		{
			suffix:          "-minimal",
			reasoningEffort: openAIReasoningEffortMinimal,
		},
		{
			suffix:          "-low",
			reasoningEffort: openAIReasoningEffortLow,
		},
		{
			suffix:          "-medium",
			reasoningEffort: openAIReasoningEffortMedium,
		},
		{
			suffix:          "-high",
			reasoningEffort: "high",
		},
		{
			suffix:          "-xhigh",
			reasoningEffort: openAIReasoningEffortXHigh,
		},
	} {
		if !strings.HasSuffix(lowerModel, alias.suffix) || len(model) <= len(alias.suffix) {
			continue
		}

		resolvedModel := model[:len(model)-len(alias.suffix)]
		if !openAIReasoningAliasModel(resolvedModel) {
			return "", "", false
		}

		return resolvedModel, alias.reasoningEffort, true
	}

	return "", "", false
}

func openAIReasoningAliasModel(model string) bool {
	return strings.HasPrefix(openAIReasoningModelID(model), "gpt-5")
}

func normalizeOpenAIReasoningEffort(model, effort string) string {
	normalizedEffort := strings.ToLower(strings.TrimSpace(effort))
	if normalizedEffort == "" {
		return ""
	}

	modelID := openAIReasoningModelID(model)

	switch {
	case modelID == openAIReasoningModelGPT51 && normalizedEffort == openAIReasoningEffortXHigh:
		return openAICodexReasoningHigh
	case (strings.HasPrefix(modelID, "gpt-5.2") ||
		strings.HasPrefix(modelID, "gpt-5.3") ||
		strings.HasPrefix(modelID, openAIReasoningModelGPT54)) &&
		normalizedEffort == openAIReasoningEffortMinimal:
		return openAIReasoningEffortLow
	default:
		return normalizedEffort
	}
}

func openAIReasoningModelID(model string) string {
	modelID := strings.TrimSpace(trimConfiguredModelLocalSuffixes(model))
	if slashIndex := strings.LastIndex(modelID, "/"); slashIndex >= 0 {
		modelID = modelID[slashIndex+1:]
	}

	return modelID
}

func openAIReasoningConfigExtraBody(extraBody map[string]any) map[string]any {
	existingReasoningConfig, reasoningConfigExists := extraBody["reasoning"]
	if !reasoningConfigExists || existingReasoningConfig == nil {
		return make(map[string]any, 1)
	}

	reasoningConfig, ok := existingReasoningConfig.(map[string]any)
	if !ok {
		return make(map[string]any, 1)
	}

	return maps.Clone(reasoningConfig)
}
