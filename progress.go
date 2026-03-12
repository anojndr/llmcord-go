package main

import (
	"log/slog"
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
	done chan struct{}
}

func (instance *bot) startRequestProgress(
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
		slog.Warn(
			"send request progress embed",
			"source_message_id",
			sourceMessage.ID,
			"error",
			err,
		)
	} else {
		progress.message = sentMessage
		progress.tracker.responseMessages = append(progress.tracker.responseMessages, sentMessage)
		progress.tracker.pendingResponses = append(progress.tracker.pendingResponses, pending)
		progress.tracker.progressActive = true
	}

	go progress.run()

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

func (progress *requestProgress) fail() {
	if progress == nil {
		return
	}

	done := make(chan struct{})
	progress.failures <- requestProgressFailure{done: done}

	<-done
}

func (progress *requestProgress) run() {
	currentStage := requestProgressStageReadingConversation
	tracker := progress.tracker

	for {
		select {
		case stage := <-progress.stageUpdates:
			if stage <= currentStage {
				continue
			}

			currentStage = stage

			if progress.message == nil {
				continue
			}

			editErr := progress.instance.editEmbedMessage(
				progress.message,
				buildRequestProgressEmbed(currentStage, tracker.modelName),
				nil,
			)
			if editErr != nil {
				slog.Warn(
					"edit request progress embed",
					"message_id",
					progress.message.ID,
					"error",
					editErr,
				)
			}
		case handoff := <-progress.handoffs:
			tracker.modelName = strings.TrimSpace(handoff.modelName)

			tracker.searchMetadata = cloneSearchMetadata(handoff.searchMetadata)
			handoff.result <- tracker

			return
		case failure := <-progress.failures:
			if progress.message != nil {
				tracker.progressActive = false

				editErr := progress.instance.editEmbedMessage(
					progress.message,
					buildRequestProgressFailureEmbed(tracker.modelName),
					nil,
				)
				if editErr != nil {
					slog.Warn(
						"edit request progress failure embed",
						"message_id",
						progress.message.ID,
						"error",
						editErr,
					)
				}
			}

			close(failure.done)

			return
		}
	}
}

func (instance *bot) renderProgressFailure(tracker *responseTracker) {
	if tracker == nil || !tracker.progressActive || len(tracker.responseMessages) == 0 {
		return
	}

	tracker.progressActive = false

	err := instance.editEmbedMessage(
		tracker.responseMessages[0],
		buildRequestProgressFailureEmbed(tracker.modelName),
		nil,
	)
	if err != nil {
		slog.Warn(
			"edit request progress failure embed",
			"message_id",
			tracker.responseMessages[0].ID,
			"error",
			err,
		)
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

	return buildResponseEmbed(description, modelName, embedColorIncomplete, nil)
}

func buildRequestProgressFailureEmbed(modelName string) *discordgo.MessageEmbed {
	return buildResponseEmbed(
		"Request failed before a response could be sent.",
		modelName,
		embedColorFailure,
		nil,
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
