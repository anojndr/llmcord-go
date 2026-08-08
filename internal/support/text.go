package support

import (
	"strings"
)

// RuneCount counts runes in text.
func RuneCount(text string) int {
	return len([]rune(text))
}

// JoinNonEmpty joins non-empty parts with two newlines.
func JoinNonEmpty(parts []string) string {
	nonEmptyParts := make([]string, 0, len(parts))

	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}

		nonEmptyParts = append(nonEmptyParts, strings.TrimSpace(part))
	}

	return strings.Join(nonEmptyParts, "\n\n")
}
