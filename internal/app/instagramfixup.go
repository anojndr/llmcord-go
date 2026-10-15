package app

import (
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// instagramSchemeRegexp matches https://instagram.com and
// https://www.instagram.com case-insensitively, preserving scheme and www.
var instagramSchemeRegexp = regexp.MustCompile(`(?i)(https?://)(www\.)?instagram\.com\b`)

// instagramBareRegexp matches bare instagram.com occurrences with optional
// www prefix, not preceded by alphanumeric to avoid matching inside other
// words or already-fixed eeinstagram.com.
var instagramBareRegexp = regexp.MustCompile(`(?i)(^|[^a-zA-Z0-9])(www\.)?instagram\.com\b`)

func fixupInstagramContent(content string) string {
	// Replace scheme-based URLs first, preserving scheme and www.
	content = instagramSchemeRegexp.ReplaceAllString(content, "${1}${2}eeinstagram.com")
	// Replace bare occurrences.
	content = instagramBareRegexp.ReplaceAllString(content, "${1}${2}eeinstagram.com")

	return content
}

func shouldHandleInstagramFixup(message *discordgo.Message, botUserID string) bool {
	if message == nil || message.Author == nil || message.Author.Bot {
		return false
	}

	if strings.TrimSpace(message.Content) == "" {
		return false
	}

	// Fast substring check.
	if !strings.Contains(strings.ToLower(message.Content), "instagram.com") {
		return false
	}

	if linkFixupExcluded(message, botUserID) {
		return false
	}

	// Check if fixup actually changes content (prevents handling already-fixed messages).
	return fixupInstagramContent(message.Content) != message.Content
}

func (instance *bot) handleInstagramFixup(message *discordgo.Message, botUserID string) bool {
	if !shouldHandleInstagramFixup(message, botUserID) {
		return false
	}

	fixedContent := fixupLinkContent(message.Content)
	if fixedContent == message.Content {
		return false
	}

	return instance.resendFixedMessage(message, fixedContent, "instagram")
}
