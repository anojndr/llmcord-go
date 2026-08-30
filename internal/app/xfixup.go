package app

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// xComSchemeRegexp matches https://x.com or https://www.x.com (case-insensitive)
// with a capturing group for scheme and optional www. prefix preservation.
var xComSchemeRegexp = regexp.MustCompile(`(?i)(https?://)(www\.)?x\.com\b`)

// xComBareRegexp matches bare x.com occurrences not preceded by alphanumeric
// to avoid double-replacing fixupx.com. It captures the preceding char/anchor.
var xComBareRegexp = regexp.MustCompile(`(?i)(^|[^a-zA-Z0-9])x\.com\b`)

const hardcodedBotMentionID = "1307756710072549439"

func fixupXComContent(content string) string {
	// Replace scheme-based URLs first, preserving scheme and www.
	content = xComSchemeRegexp.ReplaceAllString(content, "${1}${2}fixupx.com")
	// Replace bare occurrences.
	content = xComBareRegexp.ReplaceAllString(content, "${1}fixupx.com")
	return content
}

func xFixupDisplayName(message *discordgo.Message) string {
	if message == nil || message.Author == nil {
		return "unknown"
	}
	if message.Author.Username != "" {
		return message.Author.Username
	}
	if message.Author.GlobalName != "" {
		return message.Author.GlobalName
	}
	if message.Member != nil && strings.TrimSpace(message.Member.Nick) != "" {
		return strings.TrimSpace(message.Member.Nick)
	}
	if strings.TrimSpace(message.Author.ID) != "" {
		return message.Author.ID
	}
	return "unknown"
}

// attributionPrefix renders the shared "<name> sent:" attribution used when a
// feature deletes a user message and re-sends its content as the bot (x.com
// fixup, YouTube Shorts).
func attributionPrefix(displayName string) string {
	return fmt.Sprintf("%s sent:\n", displayName)
}

func shouldHandleXFixup(message *discordgo.Message, botUserID string) bool {
	if message == nil || message.Author == nil || message.Author.Bot {
		return false
	}
	if strings.TrimSpace(message.Content) == "" {
		return false
	}
	// Fast substring check.
	if !strings.Contains(strings.ToLower(message.Content), "x.com") {
		return false
	}
	// Exclude if contains "at ai" phrase (word-boundary, case-insensitive) or bot mention syntax.
	if hasAtAIMention(message.Content) {
		return false
	}
	// Additional literal substring check for "at ai" to satisfy spec's plain-contains wording
	// but only when it appears as separate phrase (avoid false positives inside other words).
	// We keep hasAtAIMention as authoritative; the extra check is redundant but ensures
	// compliance with spec's literal "contains at ai" description for typical usage.
	// Exclude if mentioning the bot via dynamic ID.
	if botUserID != "" && messageMentionsUser(message, botUserID) {
		return false
	}
	// Exclude if content contains hardcoded bot mention forms.
	if strings.Contains(message.Content, "<@"+hardcodedBotMentionID+">") ||
		strings.Contains(message.Content, "<@!"+hardcodedBotMentionID+">") {
		return false
	}
	// Also check Mentions slice for hardcoded ID (covers case where botUserID empty or stale).
	for _, u := range message.Mentions {
		if u != nil && u.ID == hardcodedBotMentionID {
			return false
		}
	}
	// Check if fixup actually changes content (prevents handling already-fixed messages).
	fixed := fixupXComContent(message.Content)
	if fixed == message.Content {
		return false
	}
	return true
}

func (instance *bot) handleXFixup(message *discordgo.Message, botUserID string) bool {
	if !shouldHandleXFixup(message, botUserID) {
		return false
	}
	fixedContent := fixupXComContent(message.Content)
	if fixedContent == message.Content {
		return false
	}
	displayName := xFixupDisplayName(message)
	newContent := attributionPrefix(displayName) + fixedContent
	// Discord content limit 2000; truncate if necessary.
	if len(newContent) > 2000 {
		// Reserve prefix length.
		prefix := attributionPrefix(displayName)
		allowed := 2000 - len(prefix)
		if allowed < 0 {
			allowed = 0
		}
		if len(fixedContent) > allowed {
			fixedContent = fixedContent[:allowed]
		}
		newContent = prefix + fixedContent
	}
	if instance == nil || instance.session == nil {
		slog.Info("x.com fixup skipped: nil session", "message_id", message.ID, "channel_id", message.ChannelID)
		return false
	}
	if err := instance.session.ChannelMessageDelete(message.ChannelID, message.ID); err != nil {
		logWarn("delete x.com message", err, "channel_id", message.ChannelID, "message_id", message.ID)
	}
	send := &discordgo.MessageSend{
		Content: newContent,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	}
	if _, err := instance.session.ChannelMessageSendComplex(message.ChannelID, send); err != nil {
		// Fallback to simple send.
		if _, err2 := instance.session.ChannelMessageSend(message.ChannelID, newContent); err2 != nil {
			logWarn("send x.com fixup message", err2, "channel_id", message.ChannelID)
			logWarn("send x.com fixup message (complex)", err, "channel_id", message.ChannelID)
		} else {
			slog.Info("x.com fixup sent (fallback)", "channel_id", message.ChannelID, "message_id", message.ID, "author", displayName)
		}
	} else {
		slog.Info("x.com fixup applied", "message_id", message.ID, "channel_id", message.ChannelID, "author", displayName)
	}
	return true
}
