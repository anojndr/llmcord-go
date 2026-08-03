package main

import (
	"context"
	"strings"

	"github.com/bwmarrin/discordgo"
)

type requestProgressStage int

const (
	requestProgressStageReadingConversation requestProgressStage = iota
	requestProgressStageGatheringContext
	requestProgressStageGeneratingResponse
)

type requestProgress struct {
	instance     *bot
	tracker      *responseTracker
	message      *discordgo.Message
	stageUpdates chan requestProgressStage
	handoffs     chan requestProgressHandoff
	failures     chan requestProgressFailure
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
	progress.stageUpdates = make(chan requestProgressStage, 1)
	progress.handoffs = make(chan requestProgressHandoff)
	progress.failures = make(chan requestProgressFailure)

	var actions responseActions

	progressEmbed := buildRequestProgressEmbed(
		requestProgressStageReadingConversation,
		progress.tracker.modelName,
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
		buildRequestProgressEmbed(stage, progress.tracker.modelName),
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

	for {
		select {
		case stage := <-progress.stageUpdates:
			if stage <= currentStage {
				continue
			}

			currentStage = stage
			progress.renderStageUpdate(ctx, stage)
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
) *discordgo.MessageEmbed {
	description := strings.Join([]string{
		"Working on it.",
		"",
		formatRequestProgressLine(requestProgressStageReadingConversation, stage, "Reading conversation"),
		formatRequestProgressLine(requestProgressStageGatheringContext, stage, "Gathering context"),
		formatRequestProgressLine(requestProgressStageGeneratingResponse, stage, "Generating response"),
	}, "\n")

	return buildResponseEmbed(description, modelName, embedColorIncomplete, nil, "")
}

func buildRequestProgressFailureEmbed(
	modelName, errorText string,
) *discordgo.MessageEmbed {
	return buildResponseEmbed(
		strings.TrimSpace(errorText),
		modelName,
		embedColorFailure,
		nil,
		"",
	)
}

func formatRequestProgressLine(
	lineStage requestProgressStage,
	currentStage requestProgressStage,
	label string,
) string {
	switch {
	case lineStage < currentStage:
		return "[x] " + label
	case lineStage == currentStage:
		return "[>] " + label
	default:
		return "[ ] " + label
	}
}
