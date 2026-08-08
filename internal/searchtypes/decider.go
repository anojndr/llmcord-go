package searchtypes

import (
	_ "embed"
	"strings"
	"time"
)

// rawSearchDeciderPrompt is populated by go:embed from searchDeciderPrompt.txt.
//
//go:embed searchDeciderPrompt.txt
var rawSearchDeciderPrompt string

// SearchDeciderPrompt renders the embedded search-decider prompt with the
// current date/time substituted.
func SearchDeciderPrompt(now time.Time) string {
	replacedText := strings.ReplaceAll(
		rawSearchDeciderPrompt,
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
