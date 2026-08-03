package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

type requestProgressStage int

const (
	requestProgressStageReadingConversation requestProgressStage = iota
	requestProgressStageGatheringContext
	requestProgressStageGeneratingResponse
)

const (
	requestProgressRefreshInterval = 2 * time.Second
	requestProgressBarWidth        = 10
	requestProgressBarHalfPercent  = 50
	requestProgressBarFullPercent  = 100
	requestProgressElapsedHoursDiv = 3600
	requestProgressElapsedMinsDiv  = 60
	requestProgressEmbedExtraLines = 2
	requestProgressTitleText       = "⏳ Working on it…"
	requestProgressFailureTitle    = "❌ Request failed"
	requestProgressDoneMarker      = "✅"
	requestProgressCurrentMarker   = "⏳"
	requestProgressPendingMarker   = "⬜"
	requestProgressBarFilledCell   = "▰"
	requestProgressBarEmptyCell    = "▱"
	requestProgressElapsedPrefix   = "⏱"
)

type requestProgressStageInfo struct {
	label   string
	detail  string
	percent int
}

func requestProgressStageInfos() []requestProgressStageInfo {
	return []requestProgressStageInfo{
		{
			label:   "Reading conversation",
			detail:  "Scanning the message, attachments, and reply history",
			percent: requestProgressReadingPercent,
		},
		{
			label:   "Gathering context",
			detail:  "Collecting links, documents, and search results",
			percent: requestProgressGatheringPercent,
		},
		{
			label:   "Generating response",
			detail:  "Waiting for the model to respond",
			percent: requestProgressGeneratingPercent,
		},
	}
}

const (
	requestProgressReadingPercent    = 10
	requestProgressGatheringPercent  = 45
	requestProgressGeneratingPercent = 80
)

type requestProgress struct {
	instance     *bot
	tracker      *responseTracker
	message      *discordgo.Message
	stageUpdates chan requestProgressStage
	handoffs     chan requestProgressHandoff
	failures     chan requestProgressFailure
	startedAt    time.Time
}

type requestProgressHandoff struct {
	modelName      string
	searchMetadata *searchMetadata
	result         chan *responseTracker
}

type requestProgressFailure struct {
	err  error
	done chan struct{}
}

func (instance *bot) startRequestProgress(
	ctx context.Context,
	sourceMessage *discordgo.Message,
	modelName string,
) *requestProgress {
	progress := new(requestProgress)
	progress.instance = instance
	progress.tracker = newResponseTracker(sourceMessage, modelName)
	progress.startedAt = time.Now()
	progress.stageUpdates = make(chan requestProgressStage, 1)
	progress.handoffs = make(chan requestProgressHandoff)
	progress.failures = make(chan requestProgressFailure)

	var actions responseActions

	progressEmbed := buildRequestProgressEmbed(
		requestProgressStageReadingConversation,
		progress.tracker.modelName,
		0,
		progress.startedAt,
	)

	sentMessage, pending, err := instance.sendEmbedMessage(
		progress.tracker,
		progressEmbed,
		actions,
	)
	if err != nil {
		logWarn(
			"send request progress embed",
			err,
			"source_message_id",
			sourceMessage.ID,
		)
	} else {
		progress.message = sentMessage
		progress.tracker.responseMessages = append(progress.tracker.responseMessages, sentMessage)
		progress.tracker.pendingResponses = append(progress.tracker.pendingResponses, pending)
		progress.tracker.progressActive = true
	}

	go progress.run(withoutCancelContext(ctx))

	return progress
}

func (progress *requestProgress) advance(stage requestProgressStage) {
	if progress == nil {
		return
	}

	select {
	case progress.stageUpdates <- stage:
	default:
		select {
		case <-progress.stageUpdates:
		default:
		}

		select {
		case progress.stageUpdates <- stage:
		default:
		}
	}
}

func (progress *requestProgress) handoff(
	modelName string,
	searchMetadata *searchMetadata,
) *responseTracker {
	if progress == nil {
		return nil
	}

	result := make(chan *responseTracker, 1)
	progress.handoffs <- requestProgressHandoff{
		modelName:      modelName,
		searchMetadata: searchMetadata,
		result:         result,
	}

	return <-result
}

func (progress *requestProgress) fail(ctx context.Context, err error) {
	if progress == nil {
		return
	}

	done := make(chan struct{})
	failure := requestProgressFailure{err: err, done: done}

	select {
	case progress.failures <- failure:
		<-done
	default:
		errorText := userFacingResponseError(err)

		renderErr := progress.instance.renderFailureResponse(
			ctx,
			progress.tracker,
			errorText,
			false,
		)
		if renderErr != nil {
			logWarn(
				"render fallback request progress failure response",
				renderErr,
				"source_message_id",
				progress.tracker.sourceMessage.ID,
			)
		}

		progress.tracker.release(progress.instance.nodes, errorText, "")
		progress.instance.nodes.persistBestEffort()
	}
}

func (progress *requestProgress) renderStageUpdate(ctx context.Context, stage requestProgressStage) {
	if progress.message == nil {
		return
	}

	waitErr := progress.instance.waitForEditSlotForMessage(
		ctx,
		progress.message.ID,
	)
	if waitErr != nil {
		logWarn(
			"wait before request progress embed edit",
			waitErr,
			"message_id",
			progress.message.ID,
		)
	}

	editErr := progress.instance.editEmbedMessage(
		progress.message,
		buildRequestProgressEmbed(
			stage,
			progress.tracker.modelName,
			progress.elapsed(),
			progress.startedAt,
		),
		nil,
	)
	if editErr != nil {
		logWarn(
			"edit request progress embed",
			editErr,
			"message_id",
			progress.message.ID,
		)
	}
}

func (progress *requestProgress) elapsed() time.Duration {
	if progress == nil || progress.startedAt.IsZero() {
		return 0
	}

	elapsed := time.Since(progress.startedAt)
	if elapsed < 0 {
		return 0
	}

	return elapsed
}

func (progress *requestProgress) handlePendingHandoffStage(ctx context.Context, currentStage *requestProgressStage) {
	select {
	case stage := <-progress.stageUpdates:
		if stage > *currentStage && progress.message != nil {
			*currentStage = stage
			progress.renderStageUpdate(ctx, stage)
		}
	default:
	}
}

func (progress *requestProgress) run(ctx context.Context) {
	currentStage := requestProgressStageReadingConversation
	tracker := progress.tracker

	ticker := time.NewTicker(requestProgressRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case stage := <-progress.stageUpdates:
			if stage <= currentStage {
				continue
			}

			currentStage = stage
			progress.renderStageUpdate(ctx, stage)
		case <-ticker.C:
			progress.renderStageUpdate(ctx, currentStage)
		case handoff := <-progress.handoffs:
			progress.handlePendingHandoffStage(ctx, &currentStage)

			tracker.modelName = strings.TrimSpace(handoff.modelName)
			tracker.originalModel = tracker.modelName
			tracker.searchMetadata = cloneSearchMetadata(handoff.searchMetadata)

			handoff.result <- tracker

			return
		case failure := <-progress.failures:
			errorText := userFacingResponseError(failure.err)

			err := progress.instance.renderFailureResponse(
				ctx,
				tracker,
				errorText,
				false,
			)
			if err != nil {
				logWarn(
					"render request progress failure response",
					err,
					"source_message_id",
					tracker.sourceMessage.ID,
				)
			}

			tracker.release(progress.instance.nodes, errorText, "")
			progress.instance.nodes.persistBestEffort()
			close(failure.done)

			return
		}
	}
}

func buildRequestProgressEmbed(
	stage requestProgressStage,
	modelName string,
	elapsed time.Duration,
	startedAt time.Time,
) *discordgo.MessageEmbed {
	infos := requestProgressStageInfos()

	lines := make([]string, 0, len(infos)+requestProgressEmbedExtraLines)
	lines = append(
		lines,
		buildRequestProgressStatusLine(stage, elapsed),
		"",
	)

	for lineStage, info := range infos {
		lines = append(
			lines,
			formatRequestProgressLine(requestProgressStage(lineStage), stage, info),
		)
	}

	embed := buildResponseEmbed(strings.Join(lines, "\n"), modelName, embedColorIncomplete, nil, "")
	embed.Title = requestProgressTitleText

	if !startedAt.IsZero() {
		embed.Timestamp = startedAt.Format(time.RFC3339)
	}

	return embed
}

func buildRequestProgressFailureEmbed(
	modelName, errorText string,
) *discordgo.MessageEmbed {
	embed := buildResponseEmbed(
		strings.TrimSpace(errorText),
		modelName,
		embedColorFailure,
		nil,
		"",
	)
	embed.Title = requestProgressFailureTitle

	return embed
}

func formatRequestProgressLine(
	lineStage requestProgressStage,
	currentStage requestProgressStage,
	info requestProgressStageInfo,
) string {
	marker := requestProgressPendingMarker

	switch {
	case lineStage < currentStage:
		marker = requestProgressDoneMarker
	case lineStage == currentStage:
		marker = requestProgressCurrentMarker
	}

	line := marker + " " + info.label
	if lineStage == currentStage && strings.TrimSpace(info.detail) != "" {
		line += " — " + info.detail
	}

	return line
}

func buildRequestProgressStatusLine(stage requestProgressStage, elapsed time.Duration) string {
	info := requestProgressStageInfoFor(stage)

	return fmt.Sprintf(
		"%s **%d%%** · %s **%s**",
		buildRequestProgressBar(info.percent),
		info.percent,
		requestProgressElapsedPrefix,
		formatRequestProgressElapsed(elapsed),
	)
}

func requestProgressStageInfoFor(stage requestProgressStage) requestProgressStageInfo {
	infos := requestProgressStageInfos()

	if stage < 0 || int(stage) >= len(infos) {
		return requestProgressStageInfo{
			label:   "",
			detail:  "",
			percent: 0,
		}
	}

	return infos[stage]
}

func buildRequestProgressBar(percent int) string {
	percent = max(0, min(requestProgressBarFullPercent, percent))

	if percent == 0 {
		return strings.Repeat(requestProgressBarEmptyCell, requestProgressBarWidth)
	}

	if percent == requestProgressBarFullPercent {
		return strings.Repeat(requestProgressBarFilledCell, requestProgressBarWidth)
	}

	filled := (percent*requestProgressBarWidth + requestProgressBarHalfPercent) /
		requestProgressBarFullPercent

	return strings.Repeat(requestProgressBarFilledCell, filled) +
		strings.Repeat(requestProgressBarEmptyCell, requestProgressBarWidth-filled)
}

func formatRequestProgressElapsed(elapsed time.Duration) string {
	totalSeconds := max(0, int(elapsed.Seconds()))

	hours := totalSeconds / requestProgressElapsedHoursDiv
	minutes := (totalSeconds % requestProgressElapsedHoursDiv) / requestProgressElapsedMinsDiv
	seconds := totalSeconds % requestProgressElapsedMinsDiv

	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
	}

	return fmt.Sprintf("%d:%02d", minutes, seconds)
}
