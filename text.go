package main

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

var atAIMentionRegexp = regexp.MustCompile(`(?i)\bat\s+ai\b`)
var atAIMentionPrefixRegexp = regexp.MustCompile(`(?i)^\s*at\s+ai(?:$|[\s,.:;!?-]+)`)
var atAIMentionStripRegexp = regexp.MustCompile(`(?i)\bat\s+ai\b(?:[^\S\n]*[,.:;!?-]+)?`)
var userMessagePrefixRegexp = regexp.MustCompile(`^\s*(<@!?[^>]+>:\s*)`)
var horizontalWhitespaceRegexp = regexp.MustCompile(`[^\S\n]+`)
var whitespaceBeforePunctuationRegexp = regexp.MustCompile(`([^\S\n]+)([,.:;!?])`)

func runeCount(text string) int {
	return utf8.RuneCountInString(text)
}

func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}

	if runeCount(text) <= limit {
		return text
	}

	return string([]rune(text)[:limit])
}

func splitRunesPrefix(text string, limit int) (string, string) {
	if limit <= 0 {
		return "", text
	}

	runes := []rune(text)
	if len(runes) <= limit {
		return text, ""
	}

	return string(runes[:limit]), string(runes[limit:])
}

func joinNonEmpty(parts []string) string {
	filteredParts := make([]string, 0, len(parts))

	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}

		filteredParts = append(filteredParts, part)
	}

	return strings.Join(filteredParts, "\n")
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
	for _, mention := range []string{
		fmt.Sprintf("<@%s>", botID),
		fmt.Sprintf("<@!%s>", botID),
	} {
		trimmedText, found := strings.CutPrefix(text, mention)
		if found {
			return strings.TrimSpace(trimmedText)
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
	return []string{
		"claude",
		"gemini",
		"gemma",
		"gpt-4",
		"gpt-5",
		"grok-4",
		"llama",
		"llava",
		"mistral",
		"o3",
		"o4",
		"vision",
		"vl",
	}
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
