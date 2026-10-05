package providers

import (
	"fmt"
	"os"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// Claude thinking/effort wiring. Current models (Opus 5, Sonnet 5, Opus
// 4.6-4.8) use adaptive thinking; budget_tokens is rejected with a 400 on
// them, and temperature/top_p/top_k are removed as well. The bot's
// reasoning_effort (none/minimal/low/medium/high/xhigh/max) maps onto
// output_config.effort; thinking stays adaptive (the default) unless the
// caller opts out explicitly.

func claudeExtraBodyEffort(extraBody map[string]any) (string, bool) {
	for key, value := range extraBody {
		normalizedKey := strings.ToLower(strings.TrimSpace(key))

		switch normalizedKey {
		case "effort", "reasoning_effort", "reasoningEffort", "output_config.effort":
			effort, ok := value.(string)
			if !ok {
				continue
			}

			trimmed := strings.ToLower(strings.TrimSpace(effort))
			if trimmed == "" {
				continue
			}

			return normalizeClaudeEffort(trimmed), true
		}
	}

	return "", false
}

// normalizeClaudeEffort maps the bot's reasoning_effort vocabulary onto
// Claude output_config.effort (low/medium/high/xhigh/max). "none" and
// "minimal" collapse: none disables thinking, minimal rounds up to low.
func normalizeClaudeEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none":
		return "none"
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	default:
		return strings.ToLower(strings.TrimSpace(effort))
	}
}

// IsValidClaudeEffort reports whether effort is a valid Claude effort.
func IsValidClaudeEffort(effort string) bool {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

// claudeThinkingParam builds the Messages thinking config. Adaptive is the
// default on current models (omitting thinking runs adaptive), so this only
// emits an explicit config when the caller opts out (effort none) or pins
// a legacy budget via extra_body thinking block.
func claudeThinkingParam(extraBody map[string]any, _ string) anthropic.ThinkingConfigParamUnion {
	var thinking anthropic.ThinkingConfigParamUnion

	if rawThinking, ok := extraBody["thinking"]; ok {
		if parsed := claudeParseThinkingBlock(rawThinking); parsed.OfAdaptive != nil ||
			parsed.OfEnabled != nil ||
			parsed.OfDisabled != nil ||
			parsed.OfBetweenTools != nil {
			return parsed
		}
	}

	if effort, ok := claudeExtraBodyEffort(extraBody); ok && effort == "none" {
		disabled := anthropic.ThinkingConfigDisabledParam{}
		thinking.OfDisabled = &disabled

		return thinking
	}

	return thinking
}

func claudeParseThinkingBlock(raw any) anthropic.ThinkingConfigParamUnion {
	var thinking anthropic.ThinkingConfigParamUnion

	block, ok := raw.(map[string]any)
	if !ok {
		return thinking
	}

	thinkingType, _ := block["type"].(string)

	switch strings.ToLower(strings.TrimSpace(thinkingType)) {
	case "adaptive":
		adaptive := anthropic.ThinkingConfigAdaptiveParam{}
		thinking.OfAdaptive = &adaptive
	case "disabled":
		disabled := anthropic.ThinkingConfigDisabledParam{}
		thinking.OfDisabled = &disabled
	}

	return thinking
}

// claudeOutputEffort returns the output_config.effort for extraBody, or ""
// when unset. "none" is not an output_config effort: it disables thinking
// instead (handled by claudeThinkingParam).
func claudeOutputEffort(extraBody map[string]any) string {
	effort, ok := claudeExtraBodyEffort(extraBody)
	if !ok || effort == "" || effort == "none" {
		return ""
	}

	return effort
}

// claudeApplySamplingParams applies provider/model sampling options onto
// Messages params: temperature/top_p/top_k pass through, effort maps onto
// output_config, and max_tokens is already set by the caller.
func claudeApplySamplingParams(params *anthropic.MessageNewParams, extraBody map[string]any, model string) {
	if params == nil {
		return
	}

	for key, value := range extraBody {
		normalizedKey := strings.ToLower(strings.TrimSpace(key))

		switch normalizedKey {
		case "temperature":
			if number, ok := claudeExtraBodyFloat(value); ok {
				params.Temperature = param.NewOpt(number)
			}
		case "top_p", "topp":
			if number, ok := claudeExtraBodyFloat(value); ok {
				params.TopP = param.NewOpt(number)
			}
		case "top_k", "topk":
			if number, ok := claudeExtraBodyInt(value); ok {
				params.TopK = param.NewOpt(number)
			}
		}
	}

	if effort := claudeOutputEffort(extraBody); effort != "" {
		if params.OutputConfig.Effort == "" {
			params.OutputConfig.Effort = anthropic.OutputConfigEffort(effort)
		}
	}
}

func claudeExtraBodyFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	default:
		return 0, false
	}
}

// ApplyClaudeReasoningEffort maps a dedicated reasoning_effort onto
// extraBody for Claude: effort rides as output_config effort, "none"
// disables thinking.
func ApplyClaudeReasoningEffort(extraBody map[string]any, effort string) map[string]any {
	normalized := normalizeClaudeEffort(effort)

	if normalized == "" {
		return extraBody
	}

	if extraBody == nil {
		extraBody = make(map[string]any, 1)
	}

	if normalized == "none" {
		extraBody["thinking"] = map[string]any{"type": "disabled"}

		return extraBody
	}

	extraBody["effort"] = normalized

	return extraBody
}

// ValidateClaudeExtraBody rejects Messages-reserved keys in extra_body and
// validates effort values early so config errors surface before streaming.
func ValidateClaudeExtraBody(extraBody map[string]any) error {
	for key, value := range extraBody {
		normalizedKey := strings.ToLower(strings.TrimSpace(key))

		switch normalizedKey {
		case "model", "messages", "system", "tools", "tool_choice", "stream", "thinking":
			return fmt.Errorf("claude extra_body must not override %q: %w", key, os.ErrInvalid)
		case "effort", "reasoning_effort", "reasoningeffort":
			effort, ok := value.(string)
			if !ok {
				return fmt.Errorf("claude extra_body effort must be a string: %w", os.ErrInvalid)
			}

			if !IsValidClaudeEffort(effort) {
				return fmt.Errorf("claude effort %q is invalid: %w", effort, os.ErrInvalid)
			}
		}
	}

	return nil
}
