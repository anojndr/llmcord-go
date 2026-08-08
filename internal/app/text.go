package app

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

var atAIMentionRegexp = regexp.MustCompile(`(?i)\bat\s+ai\b`)
var atAIMentionPrefixRegexp = regexp.MustCompile(`(?i)^\s*at\s+ai(?:$|[\s,.:;!?-]+)`)
var atAIMentionStripRegexp = regexp.MustCompile(`(?i)\bat\s+ai\b(?:[^\S\n]*[,.:;!?-]+)?`)
var userMessagePrefixRegexp = regexp.MustCompile(`^\s*(<@!?[^>]+>:\s*)`)
var horizontalWhitespaceRegexp = regexp.MustCompile(`[^\S\n]+`)
var whitespaceBeforePunctuationRegexp = regexp.MustCompile(`([^\S\n]+)([,.:;!?])`)

func runeCount(text string) int {
	count := 0

	for range text {
		count++
	}

	return count
}

func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}

	count := 0

	for idx := range text {
		if count == limit {
			return text[:idx]
		}

		count++
	}

	return text
}

func splitRunesPrefix(text string, limit int) (string, string) {
	if limit <= 0 {
		return "", text
	}

	count := 0

	for idx := range text {
		if count == limit {
			return text[:idx], text[idx:]
		}

		count++
	}

	return text, ""
}

func joinNonEmpty(parts []string) string {
	var builder strings.Builder

	capacity := len(parts) - 1
	for _, part := range parts {
		capacity += len(part)
	}

	if capacity > 0 {
		builder.Grow(capacity)
	}

	first := true

	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}

		if !first {
			builder.WriteByte('\n')
		}

		builder.WriteString(part)

		first = false
	}

	return builder.String()
}

func appendUniqueWarning(warnings map[string]struct{}, warning string) {
	if warning == "" {
		return
	}

	warnings[warning] = struct{}{}
}

func sortedWarnings(warnings map[string]struct{}) []string {
	items := make([]string, 0, len(warnings))
	for warning := range warnings {
		items = append(items, warning)
	}

	slices.Sort(items)

	return items
}

func reverseChatMessages(messages []chatMessage) {
	slices.Reverse(messages)
}

func containsFold(text, fragment string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(fragment))
}

func trimBotMention(text, botID string) string {
	if strings.HasPrefix(text, "<@") {
		for _, mention := range []string{
			"<@" + botID + ">",
			"<@!" + botID + ">",
		} {
			trimmedText, found := strings.CutPrefix(text, mention)
			if found {
				return strings.TrimSpace(trimmedText)
			}
		}
	}

	prefixStripped := false

	if loc := atAIMentionPrefixRegexp.FindStringIndex(text); loc != nil {
		text = text[loc[1]:]
		prefixStripped = true
	}

	if !hasAtAIMention(text) {
		if prefixStripped {
			return strings.TrimSpace(text)
		}

		return text
	}

	text = atAIMentionStripRegexp.ReplaceAllString(text, " ")
	text = horizontalWhitespaceRegexp.ReplaceAllString(text, " ")
	text = whitespaceBeforePunctuationRegexp.ReplaceAllString(text, "$2")

	return strings.TrimSpace(text)
}

func hasAtAIMention(text string) bool {
	return atAIMentionRegexp.FindStringIndex(text) != nil
}

func splitUserMessagePrefix(text string) (string, string) {
	match := userMessagePrefixRegexp.FindStringSubmatch(text)
	if len(match) == 0 {
		return "", text
	}

	prefix := match[1]

	return prefix, text[len(match[0]):]
}

func systemPromptNow(template string, now time.Time) string {
	replacedText := strings.ReplaceAll(
		template,
		"{date}",
		now.Format("January 02 2006"),
	)
	replacedText = strings.ReplaceAll(
		replacedText,
		"{time}",
		now.Format("15:04:05 MST-0700"),
	)

	return strings.TrimSpace(replacedText)
}

func statusMessage(text string) string {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" {
		trimmedText = defaultStatusMessage
	}

	return truncateRunes(trimmedText, statusMessageMaxLength)
}

func isGoodFinishReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case finishReasonStop, "end_turn":
		return true
	default:
		return false
	}
}

func visionModelTags() []string {
	tags := [...]string{
		"claude",
		"gemini",
		"gemma",
		"gpt-4",
		"gpt-5",
		"grok",
		"llama",
		"llava",
		"mistral",
		"o3",
		"o4",
		"vision",
		"vl",
	}

	return append([]string(nil), tags[:]...)
}

func isVisionModel(modelName string) bool {
	lowerModelName := strings.ToLower(modelName)

	for _, tag := range visionModelTags() {
		if strings.Contains(lowerModelName, tag) {
			return true
		}
	}

	return false
}

func minInt(left, right int) int {
	if left < right {
		return left
	}

	return right
}

func splitConfiguredModel(configuredModel string) (string, string, error) {
	trimmedModel := trimConfiguredModelLocalSuffixes(configuredModel)

	parts := strings.SplitN(trimmedModel, "/", configuredModelParts)
	if len(parts) != configuredModelParts {
		return "", "", fmt.Errorf(
			"split configured model %q: %w",
			configuredModel,
			os.ErrInvalid,
		)
	}

	return parts[0], parts[1], nil
}

func trimConfiguredModelLocalSuffixes(model string) string {
	return strings.TrimSuffix(model, ":vision")
}
