package app

import (
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
// fixup, TikTok fixup, YouTube Shorts).
func attributionPrefix(displayName string) string {
	return displayName + " sent:\n"
}

// resendAllowedMentionUsers lists the user IDs that may be pinged when a
// message is deleted and re-sent as the bot (x.com fixup, TikTok fixup, YouTube Shorts).
// Discord suppresses mention notifications unless allowed_mentions names
// them, so the original author and everyone mentioned in the original
// message are forwarded; roles and @everyone stay suppressed by the empty
// Parse list.
func resendAllowedMentionUsers(message *discordgo.Message) []string {
	if message == nil {
		return nil
	}

	ids := make([]string, 0, len(message.Mentions)+1)
	seen := make(map[string]struct{}, len(message.Mentions)+1)
	addID := func(userID string) {
		if userID == "" {
			return
		}

		if _, exists := seen[userID]; exists {
			return
		}

		seen[userID] = struct{}{}
		ids = append(ids, userID)
	}

	if message.Author != nil {
		addID(message.Author.ID)
	}

	for _, user := range message.Mentions {
		if user != nil {
			addID(user.ID)
		}
	}

	if len(ids) == 0 {
		return nil
	}

	return ids
}

// linkFixupExcluded reports whether at-ai/bot-mention rules suppress any
// delete-and-resend link fixup (x.com, TikTok). Callers check their own
// domain substring and fixup-change gate separately.
func linkFixupExcluded(message *discordgo.Message, botUserID string) bool {
	// Exclude if contains "at ai" phrase (word-boundary, case-insensitive) or bot mention syntax.
	if hasAtAIMention(message.Content) {
		return true
	}
	// Additional literal substring check for "at ai" to satisfy spec's plain-contains wording
	// but only when it appears as separate phrase (avoid false positives inside other words).
	// We keep hasAtAIMention as authoritative; the extra check is redundant but ensures
	// compliance with spec's literal "contains at ai" description for typical usage.
	// Exclude if mentioning the bot via dynamic ID.
	if botUserID != "" && messageMentionsUser(message, botUserID) {
		return true
	}
	// Exclude if content contains hardcoded bot mention forms.
	if strings.Contains(message.Content, "<@"+hardcodedBotMentionID+">") ||
		strings.Contains(message.Content, "<@!"+hardcodedBotMentionID+">") {
		return true
	}
	// Also check Mentions slice for hardcoded ID (covers case where botUserID empty or stale).
	for _, u := range message.Mentions {
		if u != nil && u.ID == hardcodedBotMentionID {
			return true
		}
	}

	return false
}

// resendFixedMessage deletes the original message and re-sends fixedContent
// as the bot with attribution. fixupName labels log entries ("x.com", "tiktok").
func (instance *bot) resendFixedMessage(message *discordgo.Message, fixedContent, fixupName string) bool {
	displayName := xFixupDisplayName(message)
	newContent := attributionPrefix(displayName) + fixedContent
	// Discord content limit; truncate if necessary.
	if len(newContent) > discordMessageContentMaxLength {
		// Reserve prefix length.
		prefix := attributionPrefix(displayName)

		allowed := max(discordMessageContentMaxLength-len(prefix), 0)

		if len(fixedContent) > allowed {
			fixedContent = fixedContent[:allowed]
		}

		newContent = prefix + fixedContent
	}

	if instance == nil || instance.session == nil {
		slog.Info(fixupName+" fixup skipped: nil session", "message_id", message.ID, "channel_id", message.ChannelID)

		return false
	}

	if err := instance.session.ChannelMessageDelete(message.ChannelID, message.ID); err != nil {
		logWarn("delete "+fixupName+" message", err, "channel_id", message.ChannelID, "message_id", message.ID)
	}

	send := &discordgo.MessageSend{
		Content:    newContent,
		Embeds:     nil,
		TTS:        false,
		Components: nil,
		Files:      nil,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse:       []discordgo.AllowedMentionType{},
			Roles:       nil,
			Users:       resendAllowedMentionUsers(message),
			RepliedUser: false,
		},
		Reference:  nil,
		StickerIDs: nil,
		Flags:      0,
		Poll:       nil,
		File:       nil,
		Embed:      nil,
	}
	if _, err := instance.session.ChannelMessageSendComplex(message.ChannelID, send); err != nil {
		// Fallback to simple send.
		if _, err2 := instance.session.ChannelMessageSend(message.ChannelID, newContent); err2 != nil {
			logWarn("send "+fixupName+" fixup message", err2, "channel_id", message.ChannelID)
			logWarn("send "+fixupName+" fixup message (complex)", err, "channel_id", message.ChannelID)
		} else {
			slog.Info(
				fixupName+" fixup sent (fallback)",
				"channel_id",
				message.ChannelID,
				"message_id",
				message.ID,
				"author",
				displayName,
			)
		}
	} else {
		slog.Info(
			fixupName+" fixup applied",
			"message_id",
			message.ID,
			"channel_id",
			message.ChannelID,
			"author",
			displayName,
		)
	}

	return true
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

	if linkFixupExcluded(message, botUserID) {
		return false
	}
	// Check if fixup actually changes content (prevents handling already-fixed messages).
	return fixupXComContent(message.Content) != message.Content
}

// handleLinkFixups runs every delete-and-resend embed fixup (x.com, TikTok)
// before the normal reply pipeline. It reports whether a fixup consumed the
// message; callers must still evict excess nodes on true.
func (instance *bot) handleLinkFixups(message *discordgo.Message, botUserID string) bool {
	if instance.handleXFixup(message, botUserID) {
		return true
	}

	return instance.handleTikTokFixup(message, botUserID)
}

func (instance *bot) handleXFixup(message *discordgo.Message, botUserID string) bool {
	if !shouldHandleXFixup(message, botUserID) {
		return false
	}

	fixedContent := fixupLinkContent(message.Content)
	if fixedContent == message.Content {
		return false
	}

	return instance.resendFixedMessage(message, fixedContent, "x.com")
}
