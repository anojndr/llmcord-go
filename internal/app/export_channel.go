package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	exportChannelPageSize         = 100
	exportChannelProgressBarWidth = 20
	exportChannelJSONContentType  = "application/json"
	exportChannelOrder            = "newest_to_oldest"
	// exportChannelTokensPerMessage covers the per-message chat formatting
	// overhead (role and message boundaries) on top of the content tokens,
	// matching the documented OpenAI chat counting shape.
	exportChannelTokensPerMessage = 4
	// exportChannelCharsPerToken is the documented OpenAI rule of thumb
	// (roughly four characters per token) used for the export budget.
	exportChannelCharsPerToken = 4
	// exportChannelProgressEditInterval throttles the countdown bar so large
	// exports do not hit webhook rate limits with an edit per history page.
	exportChannelProgressEditInterval = 2 * time.Second
	exportChannelBarRounding          = 0.5
	exportChannelPercentMultiplier    = 100
	exportChannelSecondsPerMinute     = 60
	// exportChannelRemainingRoundingSecond rounds countdown seconds to the
	// nearest whole second instead of truncating partial seconds away.
	exportChannelRemainingRoundingSecond = 0.5
)

// exportChannelMinTokens floors the tokens option at one via the Discord client.
// It must be addressable because discordgo takes MinValue as *float64.
var exportChannelMinTokens = 1.0 //nolint:gochecknoglobals // discordgo *float64 address

var errExceededExportMaxRetries = errors.New("exceeded max retries HTTP 502")

// exportChannelMessage is one user-authored Discord message in newest-to-oldest
// order. Bot and webhook messages are never included.
type exportChannelMessage struct {
	ID        string `json:"id"`
	AuthorID  string `json:"author_id"`
	Username  string `json:"username"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Tokens    int    `json:"tokens"`
}

// exportChannelPayload is the JSON document attached to the export reply.
type exportChannelPayload struct {
	ChannelID    string                 `json:"channel_id"`
	ExportedAt   string                 `json:"exported_at"`
	TokenLimit   int                    `json:"token_limit"`
	Tokens       int                    `json:"tokens"`
	MessageCount int                    `json:"message_count"`
	Order        string                 `json:"order"`
	Messages     []exportChannelMessage `json:"messages"`
}

// exportChannelProgress snapshots one fetched page for the progress bar.
type exportChannelProgress struct {
	kept    int
	scanned int
	tokens  int
	elapsed time.Duration
}

func newExportChannelCommand() *discordgo.ApplicationCommand {
	command := new(discordgo.ApplicationCommand)
	command.Name = exportChannelCommandName
	command.Description = exportChannelCommandDescription
	command.Type = discordgo.ChatApplicationCommand

	channelIDOption := new(discordgo.ApplicationCommandOption)
	channelIDOption.Name = exportChannelChannelIDOptionName
	channelIDOption.Description = exportChannelChannelIDOptionDescription
	channelIDOption.Type = discordgo.ApplicationCommandOptionString
	channelIDOption.Required = true

	tokensOption := new(discordgo.ApplicationCommandOption)
	tokensOption.Name = exportChannelTokensOptionName
	tokensOption.Description = exportChannelTokensOptionDescription
	tokensOption.Type = discordgo.ApplicationCommandOptionInteger
	tokensOption.Required = true
	tokensOption.MinValue = &exportChannelMinTokens

	command.Options = []*discordgo.ApplicationCommandOption{channelIDOption, tokensOption}

	return command
}

func (instance *bot) handleExportChannelCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if interaction == nil || interaction.Interaction == nil {
		return fmt.Errorf("export interaction is required: %w", os.ErrInvalid)
	}

	// Defer first: Discord expires the interaction token if the initial
	// response takes longer than 3 seconds. Every return below must go
	// through the deferred follow-up edit, never a direct response.
	err := respondInteractionDeferredWithFlags(
		session,
		interaction.Interaction,
		0,
	)
	if err != nil {
		return fmt.Errorf("defer export interaction response: %w", err)
	}

	invokerID := maintenanceInvokerID(interaction)
	if invokerID != maintenanceOwnerID {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"You do not have permission to export channels.",
		)
	}

	channelID, tokenLimit := exportChannelInputOptions(interaction.ApplicationCommandData())

	if channelID == "" {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"`channelid` is required.",
		)
	}

	if tokenLimit <= 0 {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"`tokens` must be a positive integer.",
		)
	}

	return instance.runExportChannel(session, interaction.Interaction, channelID, tokenLimit)
}

func exportChannelInputOptions(
	commandData discordgo.ApplicationCommandInteractionData,
) (string, int) {
	channelID := ""
	tokenLimit := 0

	channelIDOption := commandData.GetOption(exportChannelChannelIDOptionName)
	tokensOption := commandData.GetOption(exportChannelTokensOptionName)

	if channelIDOption != nil {
		channelID = normalizeExportChannelID(channelIDOption.StringValue())
	}

	if tokensOption != nil {
		tokenLimit = int(tokensOption.IntValue())
	}

	return channelID, tokenLimit
}

// normalizeExportChannelID trims whitespace and unwraps channel mentions
// (`<#123>`, pasted with brackets) to the raw channel ID Discord expects.
func normalizeExportChannelID(rawChannelID string) string {
	trimmed := strings.TrimSpace(rawChannelID)
	if strings.HasPrefix(trimmed, "<#") && strings.HasSuffix(trimmed, ">") {
		trimmed = strings.TrimSuffix(strings.TrimPrefix(trimmed, "<#"), ">")
	}

	return strings.TrimSpace(trimmed)
}

func (instance *bot) runExportChannel(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	channelID string,
	tokenLimit int,
) error {
	if instance == nil || instance.session == nil {
		return fmt.Errorf("export channel without bot session: %w", os.ErrInvalid)
	}

	// All Discord reads use the bot session (authenticated identity with
	// channel access), never the interaction parameter: the gateway session
	// carries the token and caches the bot resolves channelByID against.
	channel, err := instance.channelByID(channelID)
	if err != nil {
		logWarn("export channel failed to load channel", err, "channel_id", channelID)

		return editInteractionResponseText(session, interaction, describeExportChannelError(channelID, err))
	}

	if message := validateExportChannel(channel, channelID); message != "" {
		return editInteractionResponseText(session, interaction, message)
	}

	exportSession := instance.session

	messages, tokens, err := instance.fetchExportWithProgress(exportSession, interaction, channelID, tokenLimit)
	if err != nil {
		logWarn("export channel failed to load messages", err, "channel_id", channelID)

		return editInteractionResponseText(session, interaction, describeExportChannelError(channelID, err))
	}

	if len(messages) == 0 {
		return editInteractionResponseText(
			session,
			interaction,
			"No user messages fit within the token limit.",
		)
	}

	exportJSON, err := json.MarshalIndent(buildExportChannelPayload(channelID, tokenLimit, messages, tokens), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal channel export: %w", err)
	}

	return sendExportChannelFile(session, interaction, channelID, messages, tokens, tokenLimit, exportJSON)
}

func (instance *bot) fetchExportWithProgress(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	channelID string,
	tokenLimit int,
) ([]exportChannelMessage, int, error) {
	startedAt := time.Now()
	lastProgressEdit := time.Time{}

	onPage := func(kept, scanned, tokens int) {
		now := time.Now()

		if !lastProgressEdit.IsZero() && now.Sub(lastProgressEdit) < exportChannelProgressEditInterval {
			return
		}

		lastProgressEdit = now
		instance.editExportProgress(session, interaction, exportChannelProgress{
			kept:    kept,
			scanned: scanned,
			tokens:  tokens,
			elapsed: now.Sub(startedAt),
		}, channelID, tokenLimit)
	}

	return fetchExportChannelMessages(session, channelID, tokenLimit, onPage)
}

func buildExportChannelPayload(
	channelID string,
	tokenLimit int,
	messages []exportChannelMessage,
	tokens int,
) exportChannelPayload {
	return exportChannelPayload{
		ChannelID:    channelID,
		ExportedAt:   time.Now().UTC().Format(time.RFC3339),
		TokenLimit:   tokenLimit,
		Tokens:       tokens,
		MessageCount: len(messages),
		Order:        exportChannelOrder,
		Messages:     messages,
	}
}

func sendExportChannelFile(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	channelID string,
	messages []exportChannelMessage,
	tokens, tokenLimit int,
	exportJSON []byte,
) error {
	content := fmt.Sprintf(
		"Exported %d user messages (%d/%d tokens) from <#%s>.",
		len(messages),
		tokens,
		tokenLimit,
		channelID,
	)
	webhookEdit := new(discordgo.WebhookEdit)
	webhookEdit.Content = &content
	webhookEdit.Files = []*discordgo.File{{
		Name:        exportChannelFilename(channelID),
		ContentType: exportChannelJSONContentType,
		Reader:      bytes.NewReader(exportJSON),
	}}

	_, err := session.InteractionResponseEdit(interaction, webhookEdit)
	if err != nil {
		logWarn("edit interaction response with channel export", err, "channel_id", channelID)

		return editInteractionResponseText(
			session,
			interaction,
			"Couldn't export the channel right now.",
		)
	}

	return nil
}

// describeExportChannelError maps a Discord history failure to an actionable
// reply: unknown channel (wrong ID), missing access (bot not in the channel
// or no View/History), rate limited (back off and retry), or the raw API
// message otherwise. The fallback keeps the transport detail (`HTTP 403 ...`,
// timeout) so the generic reply still names the cause.
func describeExportChannelError(channelID string, err error) string {
	var rateLimitErr *discordgo.RateLimitError
	if errors.As(err, &rateLimitErr) {
		return fmt.Sprintf(
			"Discord rate-limited the export of channel `%s`. Wait a minute and try again.",
			channelID,
		)
	}

	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Message != nil {
		switch restErr.Message.Code {
		case discordgo.ErrCodeUnknownChannel:
			return fmt.Sprintf(
				"Channel `%s` was not found. Check the `channelid` (right-click the channel > Copy Channel ID).",
				channelID,
			)
		case discordgo.ErrCodeMissingAccess, discordgo.ErrCodeMissingPermissions:
			return fmt.Sprintf(
				"Cannot read channel `%s`: the bot lacks access. Check View Channel and Read Message History.",
				channelID,
			)
		default:
			if strings.TrimSpace(restErr.Message.Message) != "" {
				return fmt.Sprintf("Failed to export channel `%s`: Discord says %q.", channelID, restErr.Message.Message)
			}
		}
	}

	if err != nil {
		return fmt.Sprintf("Failed to export channel `%s`: %v.", channelID, err)
	}

	return fmt.Sprintf("Failed to export channel `%s`.", channelID)
}

// validateExportChannel rejects channels that cannot hold exportable message
// history (categories, voice, forum roots), mirroring DiscordChatExporter's
// forum guard: those channel objects resolve fine but their message listing
// never returns usable history.
func validateExportChannel(channel *discordgo.Channel, channelID string) string {
	if channel == nil {
		return fmt.Sprintf(
			"Channel `%s` was not found. Check the `channelid` (right-click the channel > Copy Channel ID).",
			channelID,
		)
	}

	if exportChannelHasHistory(channel.Type) {
		return ""
	}

	return fmt.Sprintf(
		"Channel `%s` is a %s and has no message history to export. Pick a text channel, thread, or DM instead.",
		channelID,
		exportChannelTypeLabel(channel.Type),
	)
}

// exportChannelHasHistory reports whether a Discord channel type can hold
// message history worth exporting. Text-like channels, threads, news, DMs,
// and stores qualify; categories, voice/stage, forums, media, and directory
// roots never return usable history from the messages endpoint.
func exportChannelHasHistory(channelType discordgo.ChannelType) bool {
	switch channelType {
	case discordgo.ChannelTypeGuildText,
		discordgo.ChannelTypeDM,
		discordgo.ChannelTypeGroupDM,
		discordgo.ChannelTypeGuildNews,
		discordgo.ChannelTypeGuildStore,
		discordgo.ChannelTypeGuildNewsThread,
		discordgo.ChannelTypeGuildPublicThread,
		discordgo.ChannelTypeGuildPrivateThread:
		return true
	case discordgo.ChannelTypeGuildVoice,
		discordgo.ChannelTypeGuildCategory,
		discordgo.ChannelTypeGuildStageVoice,
		discordgo.ChannelTypeGuildDirectory,
		discordgo.ChannelTypeGuildForum,
		discordgo.ChannelTypeGuildMedia:
		return false
	default:
		return false
	}
}

func exportChannelTypeLabel(channelType discordgo.ChannelType) string {
	switch channelType {
	case discordgo.ChannelTypeGuildText,
		discordgo.ChannelTypeDM,
		discordgo.ChannelTypeGroupDM,
		discordgo.ChannelTypeGuildNews,
		discordgo.ChannelTypeGuildStore,
		discordgo.ChannelTypeGuildNewsThread,
		discordgo.ChannelTypeGuildPublicThread,
		discordgo.ChannelTypeGuildPrivateThread:
		return "message channel"
	case discordgo.ChannelTypeGuildCategory:
		return "category"
	case discordgo.ChannelTypeGuildVoice:
		return "voice channel"
	case discordgo.ChannelTypeGuildStageVoice:
		return "stage channel"
	case discordgo.ChannelTypeGuildForum:
		return "forum channel"
	case discordgo.ChannelTypeGuildMedia:
		return "media channel"
	case discordgo.ChannelTypeGuildDirectory:
		return "directory"
	default:
		return "channel"
	}
}

// fetchExportChannelMessages pages a channel newest-to-oldest, keeps only
// user-authored messages, and stops before the OpenAI token budget overflows.
// The returned slice stays newest-to-oldest. onPage reports progress after
// every fetched page so the caller can refresh the countdown bar.
func fetchExportChannelMessages(
	session *discordgo.Session,
	channelID string,
	tokenLimit int,
	onPage func(kept, scanned, tokens int),
) ([]exportChannelMessage, int, error) {
	messages := make([]exportChannelMessage, 0, exportChannelPageSize)
	scanned := 0
	tokens := 0
	beforeID := ""
	stalls := 0

	for {
		page, err := loadExportChannelPage(session, channelID, beforeID)
		if err != nil {
			return nil, 0, err
		}

		if len(page) == 0 {
			break
		}

		var done bool

		messages, tokens, done = appendExportChannelPage(messages, tokens, page, tokenLimit)
		scanned += len(page)

		if onPage != nil {
			onPage(len(messages), scanned, tokens)
		}

		if done {
			break
		}

		oldestID := page[len(page)-1].ID
		if oldestID == "" || oldestID == beforeID {
			stalls++

			if stalls >= exportChannelPageStallMaxAttempts {
				break
			}

			continue
		}

		stalls = 0
		beforeID = oldestID
	}

	return messages, tokens, nil
}

// loadExportChannelPage fetches one newest-to-oldest history page, retrying
// DiscordChatExporter-style transient failures (429/5xx, timeouts, EOF) with
// exponential backoff so one flaky page never fails the whole export.
// Fatal failures (unknown channel, missing access) return immediately.
func loadExportChannelPage(
	session *discordgo.Session,
	channelID string,
	beforeID string,
) ([]*discordgo.Message, error) {
	var err error

	var page []*discordgo.Message

	for attempt := range exportChannelRetryMaxAttempts {
		if attempt > 0 {
			time.Sleep(exportChannelRetryBaseDelay * time.Duration(1<<uint(attempt-1)))
		}

		page, err = session.ChannelMessages(channelID, exportChannelPageSize, beforeID, "", "")
		if err == nil {
			return page, nil
		}

		if !isRetryableExportChannelError(err) {
			break
		}

		logWarn(
			"export channel page failed transiently; retrying",
			err,
			"channel_id",
			channelID,
			"attempt",
			attempt+1,
			"max_attempts",
			exportChannelRetryMaxAttempts,
		)
	}

	return nil, fmt.Errorf("load channel messages for export: %w", err)
}

// isRetryableExportChannelError reports transient Discord failures worth a
// retry: rate limits, 5xx, timeouts, EOF hiccups. Unknown channel, missing
// access, and other client errors are fatal and must surface immediately.
func isRetryableExportChannelError(err error) bool {
	var rateLimitErr *discordgo.RateLimitError
	if errors.As(err, &rateLimitErr) {
		return true
	}

	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) {
		if restErr.Response != nil {
			if restErr.Response.StatusCode == http.StatusTooManyRequests {
				return true
			}

			if restErr.Response.StatusCode >= http.StatusInternalServerError {
				return true
			}
		}

		return false
	}

	// discordgo absorbs 502s internally up to MaxRestRetries, then surfaces
	// a plain "Exceeded Max retries HTTP 502 ..." error instead of a
	// *RESTError. A persistent 502 must keep backing off, not abort. Match
	// case-insensitively: discordgo capitalizes, the sentinel does not.
	if errors.Is(err, errExceededExportMaxRetries) ||
		strings.Contains(strings.ToLower(err.Error()), errExceededExportMaxRetries.Error()) {
		return true
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Timeout() || urlErr.Temporary()
	}

	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// appendExportChannelPage keeps one newest-to-oldest page of user messages
// without overflowing the token budget. It reports whether the budget is
// exhausted and the caller must stop paging.
func appendExportChannelPage(
	messages []exportChannelMessage,
	tokens int,
	page []*discordgo.Message,
	tokenLimit int,
) ([]exportChannelMessage, int, bool) {
	for _, message := range page {
		entry, ok := exportChannelEntry(message)
		if !ok {
			continue
		}

		if tokens+entry.Tokens > tokenLimit {
			return messages, tokens, true
		}

		messages = append(messages, entry)
		tokens += entry.Tokens
	}

	return messages, tokens, false
}

// exportChannelEntry snapshots one message for export, reporting false for
// bot, webhook, or unattributed messages which must never be exported.
func exportChannelEntry(message *discordgo.Message) (exportChannelMessage, bool) {
	if message == nil || message.Author == nil || message.Author.Bot {
		return exportChannelMessage{ID: "", AuthorID: "", Username: "", Content: "", Timestamp: "", Tokens: 0}, false
	}

	if message.WebhookID != "" {
		return exportChannelMessage{ID: "", AuthorID: "", Username: "", Content: "", Timestamp: "", Tokens: 0}, false
	}

	username := xFixupDisplayName(message)
	entry := exportChannelMessage{
		ID:        message.ID,
		AuthorID:  message.Author.ID,
		Username:  username,
		Content:   message.Content,
		Timestamp: "",
		Tokens:    estimateExportMessageTokens(username, message.Content),
	}

	if !message.Timestamp.IsZero() {
		entry.Timestamp = message.Timestamp.UTC().Format(time.RFC3339)
	}

	return entry, true
}

// estimateExportMessageTokens approximates one exported message in OpenAI
// tokens: content plus username at roughly four characters per token, plus
// the per-message chat formatting overhead.
func estimateExportMessageTokens(username, content string) int {
	return estimateOpenAITextTokens(content) +
		estimateOpenAITextTokens(username) +
		exportChannelTokensPerMessage
}

// estimateOpenAITextTokens approximates plain text in OpenAI tokens at
// roughly four characters per token, floored at one token for non-empty text.
func estimateOpenAITextTokens(text string) int {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" {
		return 0
	}

	return max((len(trimmedText)+exportChannelCharsPerToken-1)/exportChannelCharsPerToken, 1)
}

func (instance *bot) editExportProgress(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	progress exportChannelProgress,
	channelID string,
	tokenLimit int,
) {
	content := formatExportProgressContent(progress, channelID, tokenLimit)

	if err := editInteractionResponseText(session, interaction, content); err != nil {
		logWarn("edit export progress", err, "channel_id", channelID)
	}
}

func formatExportProgressContent(
	progress exportChannelProgress,
	channelID string,
	tokenLimit int,
) string {
	fraction := 0.0
	if tokenLimit > 0 {
		fraction = float64(progress.tokens) / float64(tokenLimit)
	}

	remaining := "calculating…"
	if progress.tokens > 0 && fraction > 0 {
		remaining = "~" + formatExportRemaining(time.Duration(float64(progress.elapsed)*(1-fraction)/fraction))
	}

	return fmt.Sprintf(
		"Exporting <#%s>… %d scanned, %d kept (%d/%d tokens)\n%s %d%% — %s remaining",
		channelID,
		progress.scanned,
		progress.kept,
		progress.tokens,
		tokenLimit,
		buildExportProgressBar(fraction, exportChannelProgressBarWidth),
		int(fraction*exportChannelPercentMultiplier),
		remaining,
	)
}

// buildExportProgressBar renders a fixed-width countdown bar for a 0-1
// fraction, clamping out-of-range input to an empty or full bar.
func buildExportProgressBar(fraction float64, width int) string {
	if width <= 0 {
		return "[]"
	}

	clamped := min(max(fraction, 0), 1)
	filled := int(clamped*float64(width) + exportChannelBarRounding)
	filled = min(max(filled, 0), width)

	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

// formatExportRemaining renders a countdown duration as seconds or minutes.
func formatExportRemaining(remaining time.Duration) string {
	if remaining < 0 {
		remaining = 0
	}

	seconds := int(remaining.Seconds() + exportChannelRemainingRoundingSecond)
	if seconds < exportChannelSecondsPerMinute {
		return fmt.Sprintf("%ds", seconds)
	}

	return fmt.Sprintf("%dm%02ds", seconds/exportChannelSecondsPerMinute, seconds%exportChannelSecondsPerMinute)
}

func exportChannelFilename(channelID string) string {
	return "export-" + channelID + ".json"
}
