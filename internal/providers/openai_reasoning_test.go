package providers

import (
	"strings"
	"testing"
)

func TestOpenAIReasoningEffortAliasMax(t *testing.T) {
	t.Parallel()

	resolvedModel, reasoningEffort, hasAlias := openAIReasoningEffortAlias("openai/gpt-5.4-max")
	if !hasAlias {
		t.Fatal("expected -max alias to resolve")
	}

	if !strings.HasSuffix(resolvedModel, "gpt-5.4") {
		t.Fatalf("unexpected resolved model: %q", resolvedModel)
	}

	if reasoningEffort != OpenAIReasoningEffortMax {
		t.Fatalf("unexpected reasoning effort: %q", reasoningEffort)
	}
}

func TestOpenAIReasoningEffortAliasMaxNotAppliedToNonReasoningModels(t *testing.T) {
	t.Parallel()

	_, _, hasAlias := openAIReasoningEffortAlias("openai/gpt-4.1-max")
	if hasAlias {
		t.Fatal("expected -max alias not to resolve for non-reasoning model")
	}
}

func TestNormalizeOpenAIReasoningEffortKeepsMax(t *testing.T) {
	t.Parallel()

	for _, model := range []string{
		"gpt-5.4",
		"gpt-5.6",
		"openai/gpt-5.6",
	} {
		normalized := normalizeOpenAIReasoningEffort(model, OpenAIReasoningEffortMax)
		if normalized != OpenAIReasoningEffortMax {
			t.Fatalf("unexpected normalized effort for %s: %q", model, normalized)
		}
	}
}

func TestNormalizeOpenAIReasoningEffortClampsMaxForGPT51(t *testing.T) {
	t.Parallel()

	normalized := normalizeOpenAIReasoningEffort(openAIReasoningModelGPT51, OpenAIReasoningEffortMax)
	if normalized != "high" {
		t.Fatalf("unexpected normalized effort: %q", normalized)
	}
}

func TestNormalizeOpenAIChatCompletionsModelAliasMax(t *testing.T) {
	t.Parallel()

	resolvedModel, normalizedExtraBody := NormalizeOpenAIChatCompletionsModelAlias(
		"openai/gpt-5.4-max",
		map[string]any{"temperature": 0.2},
	)

	if !strings.HasSuffix(resolvedModel, "gpt-5.4") {
		t.Fatalf("unexpected resolved model: %q", resolvedModel)
	}

	if normalizedExtraBody["reasoning_effort"] != OpenAIReasoningEffortMax {
		t.Fatalf("unexpected reasoning effort: %#v", normalizedExtraBody["reasoning_effort"])
	}

	if normalizedExtraBody["temperature"] != 0.2 {
		t.Fatalf("unexpected temperature: %#v", normalizedExtraBody["temperature"])
	}
}

func TestNormalizeOpenAIResponsesModelAliasMax(t *testing.T) {
	t.Parallel()

	resolvedModel, normalizedExtraBody := NormalizeOpenAIResponsesModelAlias(
		"openai/gpt-5.6-max",
		map[string]any{"reasoning": map[string]any{"summary": OpenAIReasoningSummaryAuto}},
	)

	if !strings.HasSuffix(resolvedModel, "gpt-5.6") {
		t.Fatalf("unexpected resolved model: %q", resolvedModel)
	}

	reasoningConfig, reasoningConfigOK := normalizedExtraBody["reasoning"].(map[string]any)
	if !reasoningConfigOK {
		t.Fatalf("unexpected reasoning config: %#v", normalizedExtraBody["reasoning"])
	}

	if reasoningConfig["effort"] != OpenAIReasoningEffortMax {
		t.Fatalf("unexpected reasoning effort: %#v", reasoningConfig["effort"])
	}

	if _, exists := normalizedExtraBody["reasoning_effort"]; exists {
		t.Fatalf("unexpected top-level reasoning_effort: %#v", normalizedExtraBody["reasoning_effort"])
	}
}
