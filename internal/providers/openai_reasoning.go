package providers

import (
	"maps"
	"strings"
)

const (
	// OpenAIReasoningEffortNone disables reasoning.
	OpenAIReasoningEffortNone = "none"
	// OpenAIReasoningEffortMinimal uses minimal reasoning.
	OpenAIReasoningEffortMinimal = "minimal"
	// OpenAIReasoningEffortLow is a reasoning effort level.
	OpenAIReasoningEffortLow = "low"
	// OpenAIReasoningEffortMedium is a reasoning effort level.
	OpenAIReasoningEffortMedium = "medium"
	// OpenAIReasoningEffortHigh is a reasoning effort level.
	OpenAIReasoningEffortHigh = "high"
	openAIReasoningEffortXHigh  = "xhigh"
	// OpenAIReasoningEffortMax is a reasoning effort level.
	OpenAIReasoningEffortMax = "max"
	// OpenAIReasoningSummaryAuto uses automatic summaries.
	OpenAIReasoningSummaryAuto = "auto"
	// OpenAIReasoningSummaryConcise uses concise summaries.
	OpenAIReasoningSummaryConcise = "concise"
	openAIReasoningModelGPT51     = "gpt-5.1"
	// OpenAIReasoningModelGPT54 is the gpt-5.4 model id.
	OpenAIReasoningModelGPT54 = "gpt-5.4"
)

// IsValidOpenAIReasoningEffort reports whether effort is a valid OpenAI reasoning effort.
func IsValidOpenAIReasoningEffort(effort string) bool {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case OpenAIReasoningEffortNone,
		OpenAIReasoningEffortMinimal,
		OpenAIReasoningEffortLow,
		OpenAIReasoningEffortMedium,
		OpenAIReasoningEffortHigh,
		openAIReasoningEffortXHigh,
		OpenAIReasoningEffortMax:
		return true
	default:
		return false
	}
}

// NormalizeOpenAIReasoningEffortForModel normalizes effort for a model.
func NormalizeOpenAIReasoningEffortForModel(model, effort string) string {
	return normalizeOpenAIReasoningEffort(model, effort)
}

// ApplyDedicatedReasoningEffort applies a dedicated reasoning effort to extraBody for the correct API.
func ApplyDedicatedReasoningEffort(extraBody map[string]any, model string, effort string, isResponsesAPI bool) map[string]any {
	trimmedEffort := strings.TrimSpace(effort)
	if trimmedEffort == "" {
		return extraBody
	}
	normalizedEffort := normalizeOpenAIReasoningEffort(model, trimmedEffort)
	if normalizedEffort == "" {
		return extraBody
	}
	normalizedExtraBody := maps.Clone(extraBody)
	if normalizedExtraBody == nil {
		normalizedExtraBody = make(map[string]any, 1)
	}
	if isResponsesAPI {
		reasoningConfig := openAIReasoningConfigExtraBody(normalizedExtraBody)
		reasoningConfig["effort"] = normalizedEffort
		if _, hasSummary := reasoningConfig["summary"]; !hasSummary {
			reasoningConfig["summary"] = OpenAIReasoningSummaryAuto
		}
		normalizedExtraBody["reasoning"] = reasoningConfig
		delete(normalizedExtraBody, "reasoning_effort")
		delete(normalizedExtraBody, "reasoning_summary")
	} else {
		normalizedExtraBody["reasoning_effort"] = normalizedEffort
		delete(normalizedExtraBody, "reasoning")
		delete(normalizedExtraBody, "reasoning_summary")
	}
	return normalizedExtraBody
}

// NormalizeOpenAIResponsesModelAlias resolves Responses API aliases.
func NormalizeOpenAIResponsesModelAlias(model string, extraBody map[string]any) (string, map[string]any) {
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

// NormalizeOpenAIChatCompletionsModelAlias resolves chat-completions aliases.
func NormalizeOpenAIChatCompletionsModelAlias(model string, extraBody map[string]any) (string, map[string]any) {
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

// NormalizeOpenAIResponsesExtraBody normalizes reasoning options.
func NormalizeOpenAIResponsesExtraBody(model string, extraBody map[string]any) map[string]any {
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
			"summary": OpenAIReasoningSummaryAuto,
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
		clonedReasoningConfig["summary"] = OpenAIReasoningSummaryAuto
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
			suffix: "-none",
			// Reasoning effort levels for OpenAI-compatible providers.
			reasoningEffort: OpenAIReasoningEffortNone,
		},
		{
			suffix:          "-minimal",
			reasoningEffort: OpenAIReasoningEffortMinimal,
		},
		{
			suffix:          "-low",
			reasoningEffort: OpenAIReasoningEffortLow,
		},
		{
			suffix:          "-medium",
			reasoningEffort: OpenAIReasoningEffortMedium,
		},
		{
			suffix:          "-high",
			reasoningEffort: "high",
		},
		{
			suffix:          "-xhigh",
			reasoningEffort: openAIReasoningEffortXHigh,
		},
		{
			suffix:          "-max",
			reasoningEffort: OpenAIReasoningEffortMax,
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
	case modelID == openAIReasoningModelGPT51 &&
		(normalizedEffort == openAIReasoningEffortXHigh || normalizedEffort == OpenAIReasoningEffortMax):
		return "high"
	case (strings.HasPrefix(modelID, "gpt-5.2") ||
		strings.HasPrefix(modelID, "gpt-5.3") ||
		strings.HasPrefix(modelID, OpenAIReasoningModelGPT54)) &&
		normalizedEffort == OpenAIReasoningEffortMinimal:
		return OpenAIReasoningEffortLow
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

func trimConfiguredModelLocalSuffixes(model string) string {
	return strings.TrimSuffix(model, ":vision")
}
