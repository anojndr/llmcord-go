package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// requestProgressStage identifies one step of the request lifecycle. Stages
// are strictly ordered: a stage may only move forward.
type requestProgressStage int

const (
	requestProgressStageReadingConversation requestProgressStage = iota
	requestProgressStageGatheringContext
	requestProgressStageGeneratingResponse
	// requestProgressStageCount bounds the enum; it is not a real stage.
	requestProgressStageCount
)

const (
	// requestProgressRefreshInterval paces the live card: each tick advances
	// the spinner frame and re-renders the elapsed time.
	requestProgressRefreshInterval = 2 * time.Second

	requestProgressFailureTitle = "Request failed"

	// Step-rail glyphs: completed steps collapse to a struck-through check,
	// the active step carries the caret, queued steps stay hollow.
	requestProgressDoneGlyph    = "✓"
	requestProgressCurrentGlyph = "›"
	requestProgressPendingGlyph = "○"
)

// requestProgressSpinnerFrames are cycled one per refresh tick so the periodic
// edit produces visible motion instead of an identical re-render.
var requestProgressSpinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

func requestProgressSpinnerFrame(ticks int) string {
	if len(requestProgressSpinnerFrames) == 0 {
		return ""
	}

	frame := ticks % len(requestProgressSpinnerFrames)
	if frame < 0 {
		frame += len(requestProgressSpinnerFrames)
	}

	return string(requestProgressSpinnerFrames[frame])
}

// requestProgressStageTable is indexed by stage. The tripwire below ensures
// its length matches the number of declared stages.
var requestProgressStageTable = [...]requestProgressStageInfo{
	requestProgressStageReadingConversation: {
		label:  "Reading conversation",
		detail: "Scanning the message, attachments, and reply history",
	},
	requestProgressStageGatheringContext: {
		label:  "Gathering context",
		detail: "Collecting links, documents, and search results",
	},
	requestProgressStageGeneratingResponse: {
		label:  "Generating response",
		detail: "Waiting for the model to respond",
	},
}

// Compile-time tripwire: the build fails when a stage lacks a table entry.
var _ [requestProgressStageCount]struct{} = [len(requestProgressStageTable)]struct{}{}

type requestProgressStageInfo struct {
	label  string
	detail string
}

func requestProgressStageInfoFor(stage requestProgressStage) requestProgressStageInfo {
	if stage < 0 || int(stage) >= len(requestProgressStageTable) {
		return requestProgressStageInfo{}
	}

	return requestProgressStageTable[stage]
}

type requestProgress struct {
	instance  *bot
	tracker   *responseTracker
	message   *discordgo.Message
	stages    chan requestProgressStage
	handoffs  chan requestProgressHandoff
	failures  chan requestProgressFailure
	startedAt time.Time
	ticks     int
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

// startRequestProgress posts the live progress card for sourceMessage and
// starts its refresh loop. The card message becomes tracker.responseMessages[0]
// so the final answer later edits in place over it.
func (instance *bot) startRequestProgress(
	ctx context.Context,
	sourceMessage *discordgo.Message,
	modelName string,
) *requestProgress {
	progress := &requestProgress{
		instance:  instance,
		tracker:   newResponseTracker(sourceMessage, modelName),
		startedAt: time.Now(),
		stages:    make(chan requestProgressStage, 1),
		handoffs:  make(chan requestProgressHandoff),
		failures:  make(chan requestProgressFailure),
	}

	sentMessage, pending, err := instance.sendEmbedMessage(
		progress.tracker,
		buildRequestProgressEmbed(
			requestProgressStageReadingConversation,
			progress.tracker.modelName,
			0,
			progress.startedAt,
			requestProgressSpinnerFrame(progress.ticks),
		),
		responseActions{},
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

	go progress.run(ctx)

	return progress
}

// advance queues a stage change without ever blocking the caller. If a stale
// update is still queued it is replaced by the newer stage (latest wins).
func (progress *requestProgress) advance(stage requestProgressStage) {
	if progress == nil {
		return
	}

	select {
	case progress.stages <- stage:
	default:
		select {
		case <-progress.stages:
		default:
		}

		select {
		case progress.stages <- stage:
		default:
		}
	}
}

// handoff transfers ownership of the response tracker back to the caller and
// stops the refresh loop. It blocks until the loop has drained pending stage
// updates and stamped the handoff metadata onto the tracker.
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

// fail reports a terminal failure through the refresh loop so the failure card
// replaces the live card on Discord. When the loop is no longer running (the
// card already handed off) it falls back to rendering directly.
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

		renderErr := progress.instance.renderFailureResponse(ctx, progress.tracker, errorText)
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

// render pushes a new card revision. Each render advances the spinner frame,
// so repeated renders animate even when the stage is unchanged.
func (progress *requestProgress) render(ctx context.Context, stage requestProgressStage) {
	if progress.message == nil {
		return
	}

	spinnerFrame := requestProgressSpinnerFrame(progress.ticks)
	progress.ticks++

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
			spinnerFrame,
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

// drainPendingStage applies the newest queued stage update before a handoff so
// the card never hands off showing a stale step.
func (progress *requestProgress) drainPendingStage(ctx context.Context, currentStage *requestProgressStage) {
	select {
	case stage := <-progress.stages:
		if stage > *currentStage && progress.message != nil {
			*currentStage = stage
			progress.render(ctx, stage)
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
		case stage := <-progress.stages:
			if stage <= currentStage {
				continue
			}

			currentStage = stage
			progress.render(ctx, stage)
		case <-ticker.C:
			progress.render(ctx, currentStage)
		case handoff := <-progress.handoffs:
			progress.drainPendingStage(ctx, &currentStage)

			tracker.modelName = strings.TrimSpace(handoff.modelName)
			tracker.originalModel = tracker.modelName
			tracker.searchMetadata = cloneSearchMetadata(handoff.searchMetadata)

			handoff.result <- tracker

			return
		case failure := <-progress.failures:
			errorText := userFacingResponseError(failure.err)

			err := progress.instance.renderFailureResponse(ctx, tracker, errorText)
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

// buildRequestProgressEmbed renders the live card: a spinner headline naming
// the active step above a step rail of struck-through, active, and queued
// steps. Step position and elapsed time move to the footer so the body stays
// scannable.
func buildRequestProgressEmbed(
	stage requestProgressStage,
	modelName string,
	elapsed time.Duration,
	startedAt time.Time,
	spinnerFrame string,
) *discordgo.MessageEmbed {
	headline := requestProgressStageInfoFor(stage)

	lines := make([]string, 0, 3*len(requestProgressStageTable))
	lines = append(lines, "### "+spinnerFrame+" "+headline.label, "")

	for index := range requestProgressStageTable {
		if index > 0 {
			lines = append(lines, "")
		}

		lines = append(lines, formatRequestProgressStepLine(
			requestProgressStage(index),
			stage,
			requestProgressStageTable[index],
		))
	}

	embed := buildResponseEmbed(
		strings.Join(lines, "\n"),
		modelName,
		embedColorIncomplete,
		nil,
		formatRequestProgressFooter(stage, elapsed),
	)

	if !startedAt.IsZero() {
		embed.Timestamp = startedAt.Format(time.RFC3339)
	}

	return embed
}

// buildRequestProgressFailureEmbed renders the terminal card. The description
// stays exactly the trimmed error text: callers surface that string verbatim
// when the progress message could not be edited.
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

// formatRequestProgressStepLine renders one step-rail entry for the step's
// state relative to currentStage.
func formatRequestProgressStepLine(
	lineStage requestProgressStage,
	currentStage requestProgressStage,
	info requestProgressStageInfo,
) string {
	switch {
	case lineStage < currentStage:
		return requestProgressDoneGlyph + " ~~" + info.label + "~~"
	case lineStage == currentStage:
		line := requestProgressCurrentGlyph + " **" + info.label + "**"

		if detail := strings.TrimSpace(info.detail); detail != "" {
			line += " — *" + detail + "*"
		}

		return line
	default:
		return requestProgressPendingGlyph + " " + info.label
	}
}

func formatRequestProgressFooter(
	stage requestProgressStage,
	elapsed time.Duration,
) string {
	step := max(1, min(len(requestProgressStageTable), int(stage)+1))

	return fmt.Sprintf(
		"Step %d of %d · %s",
		step,
		len(requestProgressStageTable),
		formatRequestProgressElapsed(elapsed),
	)
}

func formatRequestProgressElapsed(elapsed time.Duration) string {
	totalSeconds := max(0, int(elapsed.Seconds()))

	minutes := totalSeconds / 60
	seconds := totalSeconds % 60

	if minutes >= 60 {
		hours := minutes / 60

		return fmt.Sprintf("%d:%02d:%02d", hours, minutes%60, seconds)
	}

	return fmt.Sprintf("%d:%02d", minutes, seconds)
}
