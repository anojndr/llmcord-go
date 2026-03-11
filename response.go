package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

type segmentAccumulator struct {
	maxLength int
	segments  []string
}

type renderSpec struct {
	content           string
	color             int
	showSourcesButton bool
}

type pendingResponse struct {
	node *messageNode
}

type responseTracker struct {
	sourceMessage    *discordgo.Message
	searchMetadata   *searchMetadata
	modelName        string
	responseMessages []*discordgo.Message
	pendingResponses []pendingResponse
	renderedSpecs    []renderSpec
}

func newSegmentAccumulator(maxLength int) segmentAccumulator {
	return segmentAccumulator{
		maxLength: maxLength,
		segments:  []string{""},
	}
}

func (accumulator *segmentAccumulator) appendText(text string) bool {
	splitOccurred := false
	remainingText := text

	for remainingText != "" {
		lastIndex := len(accumulator.segments) - 1

		availableRunes := accumulator.maxLength - runeCount(accumulator.segments[lastIndex])
		if availableRunes == 0 {
			accumulator.segments = append(accumulator.segments, "")
			lastIndex = len(accumulator.segments) - 1
			availableRunes = accumulator.maxLength
			splitOccurred = true
		}

		prefix, suffix := splitRunesPrefix(remainingText, availableRunes)
		accumulator.segments[lastIndex] += prefix
		remainingText = suffix

		if remainingText != "" {
			accumulator.segments = append(accumulator.segments, "")
			splitOccurred = true
		}
	}

	return splitOccurred
}

func (accumulator *segmentAccumulator) joined() string {
	return strings.Join(accumulator.segments, "")
}

func (accumulator *segmentAccumulator) renderSegments(final bool) []string {
	if len(accumulator.segments) == 0 {
		return nil
	}

	segments := make([]string, 0, len(accumulator.segments))
	segments = append(segments, accumulator.segments...)

	if !final && segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}

	if len(segments) == 1 && segments[0] == "" {
		return nil
	}

	return segments
}

func newResponseTracker(
	sourceMessage *discordgo.Message,
	searchMetadata *searchMetadata,
	modelName string,
) *responseTracker {
	tracker := new(responseTracker)
	tracker.sourceMessage = sourceMessage
	tracker.searchMetadata = cloneSearchMetadata(searchMetadata)
	tracker.modelName = strings.TrimSpace(modelName)

	return tracker
}

func (tracker *responseTracker) release(fullText string) {
	for _, pending := range tracker.pendingResponses {
		pending.node.role = messageRoleAssistant
		pending.node.text = fullText
		pending.node.parentMessage = tracker.sourceMessage
		pending.node.initialized = true
		pending.node.mu.Unlock()
	}
}

func (tracker *responseTracker) releaseJoined(accumulator *segmentAccumulator) {
	tracker.release(accumulator.joined())
}

func (instance *bot) generateAndSendResponse(
	ctx context.Context,
	request chatCompletionRequest,
	sourceMessage *discordgo.Message,
	searchMetadata *searchMetadata,
	warnings []string,
	usePlainResponses bool,
) error {
	maxLength := embedResponseMaxLength
	if usePlainResponses {
		maxLength = plainResponseMaxLength
	}

	accumulator := newSegmentAccumulator(maxLength)

	tracker := newResponseTracker(sourceMessage, searchMetadata, request.ConfiguredModel)
	defer tracker.releaseJoined(&accumulator)

	stopTyping := instance.startTyping(ctx, sourceMessage.ChannelID)
	defer stopTyping()

	var finishReason string

	lastRenderTime := time.Time{}

	streamErr := instance.chatCompletions.streamChatCompletion(ctx, request, func(delta streamDelta) error {
		splitOccurred := false
		if delta.Content != "" {
			splitOccurred = accumulator.appendText(delta.Content)
		}

		if delta.FinishReason != "" {
			finishReason = delta.FinishReason
		}

		if usePlainResponses {
			return nil
		}

		if !shouldRenderProgress(accumulator.renderSegments(false), splitOccurred, lastRenderTime) {
			return nil
		}

		err := instance.renderEmbedResponse(
			ctx,
			tracker,
			warnings,
			accumulator.renderSegments(false),
			finishReason,
			false,
		)
		if err != nil {
			return fmt.Errorf("render embed response: %w", err)
		}

		lastRenderTime = time.Now()

		return nil
	})
	if streamErr != nil && finishReason == "" {
		finishReason = "error"
	}

	if usePlainResponses {
		err := instance.sendPlainResponse(
			ctx,
			tracker,
			accumulator.renderSegments(true),
		)
		if err != nil {
			return fmt.Errorf("send plain response: %w", err)
		}
	} else {
		err := instance.renderEmbedResponse(
			ctx,
			tracker,
			warnings,
			accumulator.renderSegments(true),
			finishReason,
			true,
		)
		if err != nil {
			return fmt.Errorf("render final embed response: %w", err)
		}
	}

	if streamErr != nil {
		return fmt.Errorf("stream response: %w", streamErr)
	}

	return nil
}

func shouldRenderProgress(
	segments []string,
	splitOccurred bool,
	lastRenderTime time.Time,
) bool {
	if len(segments) == 0 {
		return false
	}

	if splitOccurred {
		return true
	}

	if lastRenderTime.IsZero() {
		return true
	}

	return time.Since(lastRenderTime) >= editDelay
}

func buildRenderSpecs(
	segments []string,
	finishReason string,
	final bool,
	hasSearchMetadata bool,
) []renderSpec {
	specs := make([]renderSpec, 0, len(segments))

	for index, segment := range segments {
		settled := index < len(segments)-1 || final

		spec := renderSpec{
			content:           segment,
			color:             0,
			showSourcesButton: final && hasSearchMetadata && index == len(segments)-1,
		}

		switch {
		case !settled:
			spec.content += streamingIndicator
			spec.color = embedColorIncomplete
		case index < len(segments)-1 || isGoodFinishReason(finishReason):
			spec.color = embedColorComplete
		default:
			spec.color = embedColorIncomplete
		}

		specs = append(specs, spec)
	}

	return specs
}

func (instance *bot) renderEmbedResponse(
	ctx context.Context,
	tracker *responseTracker,
	warnings []string,
	segments []string,
	finishReason string,
	final bool,
) error {
	if len(segments) == 0 {
		return nil
	}

	desiredSpecs := buildRenderSpecs(
		segments,
		finishReason,
		final,
		tracker.searchMetadata != nil,
	)
	for index, spec := range desiredSpecs {
		if index < len(tracker.renderedSpecs) && tracker.renderedSpecs[index] == spec {
			continue
		}

		embed := buildResponseEmbed(
			spec.content,
			tracker.modelName,
			spec.color,
			warnings,
		)

		err := instance.waitForEditSlot(ctx)
		if err != nil {
			return fmt.Errorf("wait before embed update: %w", err)
		}

		if index < len(tracker.responseMessages) {
			err := instance.editEmbedMessage(
				tracker.responseMessages[index],
				embed,
				buildEmbedComponents(spec.showSourcesButton),
			)
			if err != nil {
				return fmt.Errorf("edit embed message: %w", err)
			}
		} else {
			sentMessage, pending, err := instance.sendEmbedMessage(
				tracker,
				embed,
				spec.showSourcesButton,
			)
			if err != nil {
				return fmt.Errorf("send embed message: %w", err)
			}

			tracker.responseMessages = append(tracker.responseMessages, sentMessage)
			tracker.pendingResponses = append(tracker.pendingResponses, pending)
		}

		if index < len(tracker.renderedSpecs) {
			tracker.renderedSpecs[index] = spec
		} else {
			tracker.renderedSpecs = append(tracker.renderedSpecs, spec)
		}
	}

	return nil
}

func (instance *bot) sendPlainResponse(
	_ context.Context,
	tracker *responseTracker,
	segments []string,
) error {
	for index, segment := range segments {
		showSourcesButton := tracker.searchMetadata != nil && index == len(segments)-1

		sentMessage, pending, err := instance.sendPlainMessage(
			tracker,
			segment,
			showSourcesButton,
		)
		if err != nil {
			return fmt.Errorf("send plain message: %w", err)
		}

		tracker.responseMessages = append(tracker.responseMessages, sentMessage)
		tracker.pendingResponses = append(tracker.pendingResponses, pending)
	}

	return nil
}

func (instance *bot) sendEmbedMessage(
	tracker *responseTracker,
	embed *discordgo.MessageEmbed,
	showSourcesButton bool,
) (*discordgo.Message, pendingResponse, error) {
	send := newReplyMessage(referenceTarget(tracker))
	send.Embeds = append(send.Embeds, embed)
	send.Components = buildEmbedComponents(showSourcesButton)

	return instance.sendReplyMessage(tracker, send)
}

func (instance *bot) sendPlainMessage(
	tracker *responseTracker,
	content string,
	showSourcesButton bool,
) (*discordgo.Message, pendingResponse, error) {
	send := newReplyMessage(referenceTarget(tracker))
	send.Flags |= discordgo.MessageFlagsIsComponentsV2
	send.Components = buildPlainComponents(content, showSourcesButton)

	return instance.sendReplyMessage(tracker, send)
}

func referenceTarget(tracker *responseTracker) *discordgo.Message {
	if len(tracker.responseMessages) == 0 {
		return tracker.sourceMessage
	}

	return tracker.responseMessages[len(tracker.responseMessages)-1]
}

func newReplyMessage(reference *discordgo.Message) *discordgo.MessageSend {
	send := new(discordgo.MessageSend)

	allowedMentions := new(discordgo.MessageAllowedMentions)
	allowedMentions.Parse = []discordgo.AllowedMentionType{
		discordgo.AllowedMentionTypeRoles,
		discordgo.AllowedMentionTypeUsers,
		discordgo.AllowedMentionTypeEveryone,
	}
	allowedMentions.RepliedUser = false

	send.AllowedMentions = allowedMentions
	send.Reference = reference.Reference()
	send.Flags = discordgo.MessageFlagsSuppressNotifications

	return send
}

func (instance *bot) sendReplyMessage(
	tracker *responseTracker,
	send *discordgo.MessageSend,
) (*discordgo.Message, pendingResponse, error) {
	target := referenceTarget(tracker)

	sentMessage, err := instance.session.ChannelMessageSendComplex(target.ChannelID, send)
	if err != nil {
		return nil, pendingResponse{}, fmt.Errorf("send reply message: %w", err)
	}

	pending := pendingResponse{
		node: instance.nodes.addPending(sentMessage.ID, tracker.sourceMessage),
	}
	pending.node.searchMetadata = cloneSearchMetadata(tracker.searchMetadata)

	return sentMessage, pending, nil
}

func (instance *bot) editEmbedMessage(
	message *discordgo.Message,
	embed *discordgo.MessageEmbed,
	components []discordgo.MessageComponent,
) error {
	edit := discordgo.NewMessageEdit(message.ChannelID, message.ID)
	edit.SetEmbeds([]*discordgo.MessageEmbed{embed})
	edit.Components = &components

	_, err := instance.session.ChannelMessageEditComplex(edit)
	if err != nil {
		return fmt.Errorf("edit message %s: %w", message.ID, err)
	}

	return nil
}

func buildResponseEmbed(
	content string,
	modelName string,
	color int,
	warnings []string,
) *discordgo.MessageEmbed {
	embed := new(discordgo.MessageEmbed)
	embed.Description = content
	embed.Color = color

	if modelName != "" {
		author := new(discordgo.MessageEmbedAuthor)
		author.Name = modelName
		embed.Author = author
	}

	for _, warning := range warnings {
		field := new(discordgo.MessageEmbedField)
		field.Name = warning
		field.Value = "."
		field.Inline = false
		embed.Fields = append(embed.Fields, field)
	}

	return embed
}

func buildEmbedComponents(showSourcesButton bool) []discordgo.MessageComponent {
	if !showSourcesButton {
		return nil
	}

	button := new(discordgo.Button)
	button.CustomID = showSourcesButtonCustomID
	button.Label = showSourcesButtonLabel
	button.Style = discordgo.SecondaryButton

	row := new(discordgo.ActionsRow)
	row.Components = []discordgo.MessageComponent{button}

	return []discordgo.MessageComponent{row}
}

func buildPlainComponents(content string, showSourcesButton bool) []discordgo.MessageComponent {
	if !showSourcesButton {
		textDisplay := new(discordgo.TextDisplay)
		textDisplay.Content = content

		return []discordgo.MessageComponent{textDisplay}
	}

	section := new(discordgo.Section)
	section.Components = []discordgo.MessageComponent{
		discordgo.TextDisplay{Content: content},
	}

	button := new(discordgo.Button)
	button.CustomID = showSourcesButtonCustomID
	button.Label = showSourcesButtonLabel
	button.Style = discordgo.SecondaryButton

	section.Accessory = button

	return []discordgo.MessageComponent{section}
}
