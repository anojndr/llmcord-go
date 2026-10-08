package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
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
	Username  string `json:"username"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	tokens    int
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
		return finishExportChannelReply(
			session,
			interaction.Interaction,
			interaction.ChannelID,
			"You do not have permission to export channels.",
			false,
		)
	}

	channelID, tokenLimit := exportChannelInputOptions(interaction.ApplicationCommandData())

	if channelID == "" {
		return finishExportChannelReply(
			session,
			interaction.Interaction,
			interaction.ChannelID,
			"`channelid` is required.",
			false,
		)
	}

	if tokenLimit <= 0 {
		return finishExportChannelReply(
			session,
			interaction.Interaction,
			interaction.ChannelID,
			"`tokens` must be a positive integer.",
			false,
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

		return finishExportChannelReply(session, interaction, channelID, describeExportChannelError(channelID, err), false)
	}

	if message := validateExportChannel(channel, channelID); message != "" {
		return finishExportChannelReply(session, interaction, channelID, message, false)
	}

	exportSession := instance.session

	messages, tokens, fetchErr := instance.fetchExportWithProgress(exportSession, interaction, channelID, tokenLimit)
	if fetchErr.err != nil {
		logWarn("export channel failed to load messages", fetchErr.err, "channel_id", channelID)

		return finishExportChannelReply(
			session,
			interaction,
			channelID,
			describeExportChannelError(channelID, fetchErr.err),
			fetchErr.tokenDead,
		)
	}

	if len(messages) == 0 {
		return finishExportChannelReply(
			session,
			interaction,
			channelID,
			"No user messages fit within the token limit.",
			fetchErr.tokenDead,
		)
	}

	payload := buildExportChannelPayload(channelID, tokenLimit, messages, tokens)

	exportJSON, err := marshalExportChannelPayload(&payload)
	if err != nil {
		return fmt.Errorf("marshal channel export: %w", err)
	}

	if payload.Tokens > tokenLimit {
		messages, payload, exportJSON = trimExportChannelPayload(channelID, tokenLimit, messages)

		if len(messages) == 0 {
			return finishExportChannelReply(
				session,
				interaction,
				channelID,
				"No user messages fit within the token limit.",
				fetchErr.tokenDead,
			)
		}
	}

	return sendExportChannelFile(
		session,
		interaction,
		channelID,
		messages,
		payload.Tokens,
		tokenLimit,
		exportJSON,
		fetchErr.tokenDead,
	)
}

// exportChannelFetchOutcome carries the fetched history plus whether the
// interaction token died mid-export. Token death stops progress edits but
// never aborts history paging: the run still finishes into the channel.
type exportChannelFetchOutcome struct {
	messages  []exportChannelMessage
	tokens    int
	err       error
	tokenDead bool
}

func (instance *bot) fetchExportWithProgress(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	channelID string,
	tokenLimit int,
) ([]exportChannelMessage, int, exportChannelFetchOutcome) {
	outcome := exportChannelFetchOutcome{
		messages:  nil,
		tokens:    0,
		err:       nil,
		tokenDead: false,
	}
	startedAt := time.Now()
	lastProgressEdit := time.Time{}

	onPage := func(kept, scanned, tokens int) {
		now := time.Now()

		if outcome.tokenDead {
			return
		}

		if !lastProgressEdit.IsZero() && now.Sub(lastProgressEdit) < exportChannelProgressEditInterval {
			return
		}

		lastProgressEdit = now

		if err := instance.editExportProgress(session, interaction, exportChannelProgress{
			kept:    kept,
			scanned: scanned,
			tokens:  tokens,
			elapsed: now.Sub(startedAt),
		}, channelID, tokenLimit); err != nil && isExpiredInteractionTokenError(err) {
			outcome.tokenDead = true
		}
	}

	outcome.messages, outcome.tokens, outcome.err = fetchExportChannelMessages(session, channelID, tokenLimit, onPage)

	return outcome.messages, outcome.tokens, outcome
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

// marshalExportChannelPayload serializes the payload with its Tokens field
// stamped to the file's own size, iterated to a fixpoint: stamping the
// count changes the file bytes (digit width), which changes the count.
// Convergence is by digit width (a handful of iterations); the loop cap is
// a safety net that keeps the last rendering.
func marshalExportChannelPayload(payload *exportChannelPayload) ([]byte, error) {
	for range exportChannelTokenStampMaxIterations {
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("encode channel export for export: %w", err)
		}

		stamped := estimateOpenAITextTokens(string(encoded))
		if stamped == payload.Tokens {
			return encoded, nil
		}

		payload.Tokens = stamped
	}

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode channel export for export: %w", err)
	}

	payload.Tokens = estimateOpenAITextTokens(string(encoded))

	final, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode channel export for export: %w", err)
	}

	return final, nil
}

// trimExportChannelPayload drops oldest messages until the stamped file fits
// the token limit. The running estimates overshoot slightly (reserve widths,
// separator commas), so the stamped total can exceed the limit by a digit
// width even when paging stopped in budget; trimming oldest-first keeps the
// newest messages the command promises.
func trimExportChannelPayload(
	channelID string,
	tokenLimit int,
	messages []exportChannelMessage,
) ([]exportChannelMessage, exportChannelPayload, []byte) {
	payload := buildExportChannelPayload(channelID, tokenLimit, messages, 0)

	for len(messages) > 0 {
		encoded, err := marshalExportChannelPayload(&payload)
		if err != nil || payload.Tokens <= tokenLimit {
			return messages, payload, encoded
		}

		messages = messages[:len(messages)-1]
		payload = buildExportChannelPayload(channelID, tokenLimit, messages, 0)
	}

	encoded, _ := marshalExportChannelPayload(&payload)

	return nil, payload, encoded
}

// exportChannelPayloadHeaderTokens reserves budget for the JSON envelope
// around the messages array (header keys, braces, indentation), so the
// running message total plus this reserve tracks the final file size. The
// Tokens field uses the limit's digit width (an upper bound the final stamp
// can only shrink) and MessageCount uses the same width; ExportedAt renders
// at fixed RFC3339 width, so the reserve never undercounts the envelope.
func exportChannelPayloadHeaderTokens(channelID string, tokenLimit int) int {
	header := exportChannelPayload{
		ChannelID:    channelID,
		ExportedAt:   time.Now().UTC().Format(time.RFC3339),
		TokenLimit:   tokenLimit,
		Tokens:       tokenLimit,
		MessageCount: tokenLimit,
		Order:        exportChannelOrder,
		Messages:     []exportChannelMessage{},
	}

	encoded, err := json.MarshalIndent(header, "", "  ")
	if err != nil {
		return estimateOpenAITextTokens(channelID) + exportChannelTokensPerMessage
	}

	return estimateOpenAITextTokens(string(encoded))
}

func sendExportChannelFile(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	channelID string,
	messages []exportChannelMessage,
	tokens, tokenLimit int,
	exportJSON []byte,
	tokenDead bool,
) error {
	content := fmt.Sprintf(
		"Exported %d user messages (%d/%d tokens) from <#%s>.",
		len(messages),
		tokens,
		tokenLimit,
		channelID,
	)

	if !tokenDead {
		webhookEdit := new(discordgo.WebhookEdit)
		webhookEdit.Content = &content
		webhookEdit.Files = []*discordgo.File{{
			Name:        exportChannelFilename(channelID),
			ContentType: exportChannelJSONContentType,
			Reader:      bytes.NewReader(exportJSON),
		}}

		if _, err := session.InteractionResponseEdit(interaction, webhookEdit); err != nil {
			if !isExpiredInteractionTokenError(err) {
				logWarn("edit interaction response with channel export", err, "channel_id", channelID)

				return finishExportChannelReply(
					session,
					interaction,
					channelID,
					"Couldn't export the channel right now.",
					false,
				)
			}

			tokenDead = true
		}
	}

	if tokenDead {
		return sendExportChannelMessage(session, interaction, channelID, content, exportJSON)
	}

	return nil
}

// finishExportChannelReply delivers one terminal export outcome. It tries the
// interaction edit first and, only when the token is already dead or the edit
// proves it dead, falls back to a regular channel message so the result is
// never lost to a 401/50027 storm.
func finishExportChannelReply(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	channelID string,
	content string,
	tokenDead bool,
) error {
	if tokenDead {
		return sendExportChannelMessage(session, interaction, channelID, content, nil)
	}

	if err := editInteractionResponseText(session, interaction, content); err != nil {
		if !isExpiredInteractionTokenError(err) {
			return fmt.Errorf("edit export reply: %w", err)
		}

		return sendExportChannelMessage(session, interaction, channelID, content, nil)
	}

	return nil
}

// sendExportChannelMessage posts the export outcome to the invoking channel.
// File bytes stay attached on success (nil means a text-only error reply).
func sendExportChannelMessage(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	channelID string,
	content string,
	exportJSON []byte,
) error {
	targetID := exportChannelReplyTarget(interaction, channelID)
	if targetID == "" {
		return fmt.Errorf("export channel reply without target channel: %w", os.ErrInvalid)
	}

	if mention := exportChannelReplyMention(interaction); mention != "" {
		content = "<@" + mention + "> " + strings.TrimSpace(content)
	}

	send := &discordgo.MessageSend{
		Content:         content,
		Embeds:          nil,
		TTS:             false,
		Components:      nil,
		Files:           nil,
		AllowedMentions: nil,
		Reference:       nil,
		StickerIDs:      nil,
		Flags:           0,
		Poll:            nil,
		File:            nil,
		Embed:           nil,
	}
	if len(exportJSON) > 0 {
		send.Files = []*discordgo.File{{
			Name:        exportChannelFilename(channelID),
			ContentType: exportChannelJSONContentType,
			Reader:      bytes.NewReader(exportJSON),
		}}
	}

	if _, err := session.ChannelMessageSendComplex(targetID, send); err != nil {
		return fmt.Errorf("send export channel message: %w", err)
	}

	slog.Info(
		"export channel fell back to channel message",
		"channel_id",
		targetID,
		"export_channel_id",
		channelID,
	)

	return nil
}

// exportChannelReplyTarget prefers the channel the slash command ran in so a
// dead token still lands where the invoker watches. The exported channel ID
// is the fallback; both are the same for same-channel exports.
func exportChannelReplyTarget(interaction *discordgo.Interaction, channelID string) string {
	if interaction != nil && strings.TrimSpace(interaction.ChannelID) != "" {
		return interaction.ChannelID
	}

	return channelID
}

// exportChannelReplyMention attributes the fallback message to the invoker
// when Discord supplied one; otherwise the message stays untargeted.
func exportChannelReplyMention(interaction *discordgo.Interaction) string {
	if interaction != nil && interaction.Member != nil && interaction.Member.User != nil {
		return interaction.Member.User.ID
	}

	if interaction != nil && interaction.User != nil {
		return interaction.User.ID
	}

	return ""
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
	tokens := exportChannelPayloadHeaderTokens(channelID, tokenLimit)
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

		page, err = loadExportChannelPageOnce(session, channelID, beforeID)
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

// loadExportChannelPageOnce fetches one history page with a component-tolerant
// decode: discordgo's Message unmarshal fails the whole page on any unknown
// component type (e.g. newer modal-only types leaking into messages), while
// the export only needs author/content/timestamp. The raw request reuses the
// session transport (auth, rate limits), then each message decodes with
// unknown components stripped before the discordgo parse.
func loadExportChannelPageOnce(
	session *discordgo.Session,
	channelID string,
	beforeID string,
) ([]*discordgo.Message, error) {
	endpoint := discordgo.EndpointChannelMessages(channelID)

	query := url.Values{}
	query.Set("limit", strconv.Itoa(exportChannelPageSize))

	if beforeID != "" {
		query.Set("before", beforeID)
	}

	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}

	body, err := session.RequestWithBucketID("GET", endpoint, nil, discordgo.EndpointChannelMessages(channelID))
	if err != nil {
		return nil, fmt.Errorf("fetch channel messages for export: %w", err)
	}

	return decodeExportChannelMessages(body)
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

// decodeExportChannelMessages parses one raw history page, dropping message
// components discordgo cannot decode yet (newer types like modal-only kinds
// leaking into message payloads). Dropped components never affect the export:
// only author, content, and timestamp are snapshotted downstream.
func decodeExportChannelMessages(body []byte) ([]*discordgo.Message, error) {
	var rawMessages []json.RawMessage
	if err := json.Unmarshal(body, &rawMessages); err != nil {
		return nil, fmt.Errorf("decode channel messages for export: %w", err)
	}

	messages := make([]*discordgo.Message, 0, len(rawMessages))

	for _, rawMessage := range rawMessages {
		message, err := decodeExportChannelMessage(rawMessage)
		if err != nil {
			return nil, err
		}

		messages = append(messages, message)
	}

	return messages, nil
}

func decodeExportChannelMessage(rawMessage json.RawMessage) (*discordgo.Message, error) {
	var message discordgo.Message
	if err := json.Unmarshal(rawMessage, &message); err == nil {
		return &message, nil
	}

	stripped, stripErr := stripExportMessageComponents(rawMessage)
	if stripErr != nil {
		return nil, fmt.Errorf("decode channel message for export: %w", stripErr)
	}

	if err := json.Unmarshal(stripped, &message); err != nil {
		return nil, fmt.Errorf("decode channel message for export: %w", err)
	}

	return &message, nil
}

func stripExportMessageComponents(rawMessage json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rawMessage, &fields); err != nil {
		return nil, fmt.Errorf("split channel message fields for export: %w", err)
	}

	delete(fields, "components")

	// Replies, forwards, and thread starters nest whole messages that can
	// carry the same unknown components; strip them recursively so one
	// nested payload cannot fail the page either.
	for _, nestedKey := range []string{"referenced_message", "message_snapshots", "interaction_metadata"} {
		nested, ok := fields[nestedKey]
		if !ok {
			continue
		}

		strippedNested, err := stripExportNestedComponents(nested)
		if err != nil {
			return nil, err
		}

		fields[nestedKey] = strippedNested
	}

	stripped, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode stripped channel message for export: %w", err)
	}

	return stripped, nil
}

func stripExportNestedComponents(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || strings.EqualFold(trimmed, "null") {
		return raw, nil
	}

	if strings.HasPrefix(trimmed, "[") {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("split nested messages for export: %w", err)
		}

		for index, item := range items {
			stripped, err := stripExportSnapshotValue(item)
			if err != nil {
				return nil, err
			}

			items[index] = stripped
		}

		encoded, err := json.Marshal(items)
		if err != nil {
			return nil, fmt.Errorf("encode stripped nested messages for export: %w", err)
		}

		return encoded, nil
	}

	return stripExportMessageValue(raw)
}

// stripExportSnapshotValue strips a message_snapshot wrapper: the real message
// lives under `message`, so the wrapper itself has no components to strip.
// Without unwrapping, a forwarded message with an unknown component still
// fails the page on the second parse.
func stripExportSnapshotValue(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("split nested snapshot for export: %w", err)
	}

	inner, ok := fields["message"]
	if !ok {
		return stripExportMessageValue(raw)
	}

	stripped, err := stripExportMessageValue(inner)
	if err != nil {
		return nil, err
	}

	fields["message"] = stripped

	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode stripped nested snapshot for export: %w", err)
	}

	return encoded, nil
}

func stripExportMessageValue(raw json.RawMessage) (json.RawMessage, error) {
	stripped, err := stripExportMessageComponents(raw)
	if err != nil {
		return nil, err
	}

	return stripped, nil
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

		if tokens+entry.tokens > tokenLimit {
			return messages, tokens, true
		}

		messages = append(messages, entry)
		tokens += entry.tokens
	}

	return messages, tokens, false
}

// exportChannelEntry snapshots one message for export, reporting false for
// bot, webhook, or unattributed messages which must never be exported.
func exportChannelEntry(message *discordgo.Message) (exportChannelMessage, bool) {
	if message == nil || message.Author == nil || message.Author.Bot {
		return exportChannelMessage{Username: "", Content: "", Timestamp: "", tokens: 0}, false
	}

	if message.WebhookID != "" {
		return exportChannelMessage{Username: "", Content: "", Timestamp: "", tokens: 0}, false
	}

	username := xFixupDisplayName(message)
	entry := exportChannelMessage{
		Username:  username,
		Content:   message.Content,
		Timestamp: "",
		tokens:    0,
	}

	if !message.Timestamp.IsZero() {
		entry.Timestamp = message.Timestamp.UTC().Format(time.RFC3339)
	}

	entry.tokens = estimateExportMessageTokens(entry)

	return entry, true
}

// estimateExportMessageTokens approximates one exported message's share of
// the final JSON file in OpenAI tokens. The budget counts the entire JSON
// document, so every serialized field (username, content, timestamp, keys,
// punctuation, and indentation) contributes. Entries marshal with the same
// two-space indent as the final file, nested two levels deep (payload object
// plus messages array) with a trailing separator comma.
func estimateExportMessageTokens(entry exportChannelMessage) int {
	encoded, err := json.MarshalIndent(entry, "    ", "  ")
	if err != nil {
		return estimateOpenAITextTokens(entry.Content) +
			estimateOpenAITextTokens(entry.Username) +
			estimateOpenAITextTokens(entry.Timestamp) +
			exportChannelTokensPerMessage
	}

	return estimateOpenAITextTokens(string(encoded) + ",")
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
) error {
	content := formatExportProgressContent(progress, channelID, tokenLimit)

	if err := editInteractionResponseText(session, interaction, content); err != nil {
		if isExpiredInteractionTokenError(err) {
			slog.Info("export channel interaction token expired; stopping progress edits", "channel_id", channelID)

			return err
		}

		logWarn("edit export progress", err, "channel_id", channelID)

		return err
	}

	return nil
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
