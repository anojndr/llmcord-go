package app

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// renderedEmbedMessage is one response message with the embed and
// components a render applied to it.
type renderedEmbedMessage struct {
	message    *discordgo.Message
	embed      *discordgo.MessageEmbed
	components []discordgo.MessageComponent
}

// confirmFinalRender checks a finished reply's messages in the background
// and restores the final render on any message that shows a streaming frame
// instead. Discord has applied an earlier streaming edit after the final
// edit, although each edit is sent only after the previous one returned:
// the message then keeps the final buttons but shows the text of a frame
// sent before them, so the reply looks cut off with no error anywhere. The
// check runs after each of instance.finalRenderCheckDelays, whichever way
// the reply ended, on the messages the final render still owns; a bot
// without delays does not check.
func (instance *bot) confirmFinalRender(ctx context.Context, tracker *responseTracker) {
	if tracker == nil || len(tracker.finalRender) == 0 || len(instance.finalRenderCheckDelays) == 0 {
		return
	}

	rendered := slices.Clone(tracker.finalRender)
	delays := slices.Clone(instance.finalRenderCheckDelays)

	safeGo(func() {
		instance.confirmFinalRenderAfter(ctx, rendered, delays...)
	})
}

// confirmFinalRenderAfter waits each delay in turn, then restores the final
// render on every rendered message that no longer shows it. A late
// streaming edit can land after an earlier check found the reply intact, so
// every check runs.
func (instance *bot) confirmFinalRenderAfter(
	ctx context.Context,
	rendered []renderedEmbedMessage,
	delays ...time.Duration,
) {
	for _, delay := range delays {
		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C:
		}

		if instance.botClosed.Load() {
			return
		}

		for _, message := range rendered {
			instance.restoreFinalRender(ctx, message)
		}
	}
}

// restoreFinalRender re-reads one response message and edits the final
// render back in when the message shows a streaming frame instead.
func (instance *bot) restoreFinalRender(ctx context.Context, rendered renderedEmbedMessage) {
	message := rendered.message
	if instance.session == nil || message == nil {
		return
	}

	current, err := instance.session.ChannelMessage(message.ChannelID, message.ID)
	if err != nil {
		if isUnknownMessageError(err) {
			slog.Debug("reply message deleted before its final render was confirmed", "message_id", message.ID)

			return
		}

		logWarn(
			"fetch reply message to confirm its final render",
			err,
			"channel_id",
			message.ChannelID,
			"message_id",
			message.ID,
		)

		return
	}

	if !showsStreamingFrameInstead(current, rendered.embed) {
		return
	}

	logWarn(
		"reply message shows a streaming frame after its final render; restoring the final render",
		nil,
		"channel_id",
		message.ChannelID,
		"message_id",
		message.ID,
	)

	err = instance.waitForEditSlotForMessage(ctx, message.ID)
	if err == nil {
		err = instance.editEmbedMessage(message, rendered.embed, rendered.components)
	}

	if err != nil {
		logWarn(
			"restore final render",
			err,
			"channel_id",
			message.ChannelID,
			"message_id",
			message.ID,
		)
	}
}

// showsStreamingFrameInstead reports whether a response message shows a
// streaming frame, text ending in the streaming indicator, where its final
// render has other text. Both sides are compared trimmed, so surrounding
// whitespace never counts as a difference.
func showsStreamingFrameInstead(current *discordgo.Message, final *discordgo.MessageEmbed) bool {
	if current == nil || final == nil || len(current.Embeds) == 0 || current.Embeds[0] == nil {
		return false
	}

	shown := strings.TrimSpace(current.Embeds[0].Description)

	return shown != strings.TrimSpace(final.Description) && strings.HasSuffix(shown, streamingIndicator)
}

// isUnknownMessageError reports whether a REST error says the message no
// longer exists.
func isUnknownMessageError(err error) bool {
	var restErr *discordgo.RESTError

	return errors.As(err, &restErr) &&
		restErr.Message != nil &&
		restErr.Message.Code == discordgo.ErrCodeUnknownMessage
}
