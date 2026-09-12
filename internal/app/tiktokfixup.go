package app

import (
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// tiktokSchemeRegexp matches https://tiktok.com and https://<sub>.tiktok.com
// (www, vm, vt, m, ...) case-insensitively, preserving scheme and subdomain.
var tiktokSchemeRegexp = regexp.MustCompile(`(?i)(https?://)((?:[A-Za-z0-9-]+\.)?)tiktok\.com\b`)

// tiktokBareRegexp matches bare tiktok.com occurrences with optional subdomain,
// not preceded by alphanumeric to avoid matching inside other words.
var tiktokBareRegexp = regexp.MustCompile(`(?i)(^|[^a-zA-Z0-9])((?:[A-Za-z0-9-]+\.)?)tiktok\.com\b`)

func fixupTikTokContent(content string) string {
	// Replace scheme-based URLs first, preserving scheme and subdomain.
	content = tiktokSchemeRegexp.ReplaceAllString(content, "${1}${2}tnktok.com")
	// Replace bare occurrences.
	content = tiktokBareRegexp.ReplaceAllString(content, "${1}${2}tnktok.com")

	return content
}

// fixupLinkContent applies every delete-and-resend embed fixup in one pass so
// a message containing both x.com and tiktok.com links needs a single resend.
func fixupLinkContent(content string) string {
	return fixupTikTokContent(fixupXComContent(content))
}

func shouldHandleTikTokFixup(message *discordgo.Message, botUserID string) bool {
	if message == nil || message.Author == nil || message.Author.Bot {
		return false
	}

	if strings.TrimSpace(message.Content) == "" {
		return false
	}
	// Fast substring check.
	if !strings.Contains(strings.ToLower(message.Content), "tiktok.com") {
		return false
	}

	if linkFixupExcluded(message, botUserID) {
		return false
	}
	// Check if fixup actually changes content (prevents handling already-fixed messages).
	return fixupTikTokContent(message.Content) != message.Content
}

func (instance *bot) handleTikTokFixup(message *discordgo.Message, botUserID string) bool {
	if !shouldHandleTikTokFixup(message, botUserID) {
		return false
	}

	fixedContent := fixupLinkContent(message.Content)
	if fixedContent == message.Content {
		return false
	}

	return instance.resendFixedMessage(message, fixedContent, "tiktok")
}
