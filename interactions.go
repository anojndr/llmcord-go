package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

func (instance *bot) handleInteractionCreate(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) {
	var err error

	switch interaction.Type {
	case discordgo.InteractionApplicationCommand,
		discordgo.InteractionApplicationCommandAutocomplete:
		err = instance.handleApplicationCommandInteraction(session, interaction)
	case discordgo.InteractionMessageComponent:
		err = instance.handleMessageComponentInteraction(session, interaction)
	case discordgo.InteractionPing,
		discordgo.InteractionModalSubmit:
		return
	default:
		return
	}

	if err != nil {
		slog.Error("handle interaction", "error", err)
	}
}

func (instance *bot) handleApplicationCommandInteraction(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	commandData := interaction.ApplicationCommandData()

	switch commandData.Name {
	case modelCommandName:
		if interaction.Type == discordgo.InteractionApplicationCommand {
			return instance.handleModelCommand(session, interaction)
		}

		if interaction.Type == discordgo.InteractionApplicationCommandAutocomplete {
			return instance.handleModelAutocomplete(session, interaction)
		}

		return nil
	case searchDeciderModelCommandName:
		if interaction.Type == discordgo.InteractionApplicationCommand {
			return instance.handleSearchDeciderModelCommand(session, interaction)
		}

		if interaction.Type == discordgo.InteractionApplicationCommandAutocomplete {
			return instance.handleSearchDeciderModelAutocomplete(session, interaction)
		}

		return nil
	default:
		return nil
	}
}

func (instance *bot) handleMessageComponentInteraction(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	componentData := interaction.MessageComponentData()

	switch {
	case componentData.CustomID == showSourcesButtonCustomID:
		return instance.handleShowSourcesButton(session, interaction)
	case componentData.CustomID == viewOnRentryButtonCustomID:
		return instance.handleViewOnRentryButton(session, interaction)
	case strings.HasPrefix(componentData.CustomID, showSourcesPageButtonCustomIDPrefix):
		return instance.handleShowSourcesPageButton(session, interaction)
	default:
		return nil
	}
}

func (instance *bot) handleShowSourcesButton(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if interaction == nil || interaction.Message == nil {
		return fmt.Errorf("show sources interaction without message: %w", os.ErrInvalid)
	}

	content, components := instance.showSourcesPageResponse(interaction.Message.ID, 0)

	return respondInteractionMessage(
		session,
		interaction.Interaction,
		discordgo.InteractionResponseChannelMessageWithSource,
		content,
		components,
		discordgo.MessageFlagsEphemeral,
	)
}

func (instance *bot) handleShowSourcesPageButton(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if interaction == nil {
		return fmt.Errorf("show sources page interaction without interaction: %w", os.ErrInvalid)
	}

	messageID, pageIndex, ok := parseShowSourcesPageButtonCustomID(interaction.MessageComponentData().CustomID)
	if !ok {
		return fmt.Errorf("invalid show sources page interaction custom id: %w", os.ErrInvalid)
	}

	content, components := instance.showSourcesPageResponse(messageID, pageIndex)

	return respondInteractionMessage(
		session,
		interaction.Interaction,
		discordgo.InteractionResponseUpdateMessage,
		content,
		components,
		0,
	)
}

func (instance *bot) showSourcesPageResponse(messageID string, pageIndex int) (string, []discordgo.MessageComponent) {
	searchMetadata := instance.searchMetadataForMessage(messageID)
	pages := formatSearchSourcesPages(searchMetadata)

	if pageIndex < 0 {
		pageIndex = 0
	} else if pageIndex >= len(pages) {
		pageIndex = len(pages) - 1
	}

	return formatSearchSourcesPageContent(pages, pageIndex),
		buildShowSourcesPaginationComponents(messageID, pageIndex, len(pages))
}

func (instance *bot) searchMetadataForMessage(messageID string) *searchMetadata {
	messageNode, ok := instance.nodes.get(messageID)
	if !ok {
		return nil
	}

	messageNode.mu.Lock()
	defer messageNode.mu.Unlock()

	return cloneSearchMetadata(messageNode.searchMetadata)
}

func buildShowSourcesPaginationComponents(
	messageID string,
	pageIndex int,
	pageCount int,
) []discordgo.MessageComponent {
	if pageCount <= 1 {
		return []discordgo.MessageComponent{}
	}

	previousPageIndex := pageIndex
	if previousPageIndex > 0 {
		previousPageIndex--
	}

	nextPageIndex := pageIndex
	if nextPageIndex < pageCount-1 {
		nextPageIndex++
	}

	previousButton := new(discordgo.Button)
	previousButton.CustomID = showSourcesPageButtonCustomID(messageID, previousPageIndex)
	previousButton.Label = showSourcesPreviousButtonLabel
	previousButton.Style = discordgo.SecondaryButton
	previousButton.Disabled = pageIndex == 0

	nextButton := new(discordgo.Button)
	nextButton.CustomID = showSourcesPageButtonCustomID(messageID, nextPageIndex)
	nextButton.Label = showSourcesNextButtonLabel
	nextButton.Style = discordgo.SecondaryButton
	nextButton.Disabled = pageIndex >= pageCount-1

	row := new(discordgo.ActionsRow)
	row.Components = []discordgo.MessageComponent{previousButton, nextButton}

	return []discordgo.MessageComponent{row}
}

func showSourcesPageButtonCustomID(messageID string, pageIndex int) string {
	return fmt.Sprintf("%s%s:%d", showSourcesPageButtonCustomIDPrefix, messageID, pageIndex)
}

func parseShowSourcesPageButtonCustomID(customID string) (string, int, bool) {
	remainder, ok := strings.CutPrefix(customID, showSourcesPageButtonCustomIDPrefix)
	if !ok {
		return "", 0, false
	}

	separatorIndex := strings.LastIndex(remainder, ":")
	if separatorIndex <= 0 || separatorIndex >= len(remainder)-1 {
		return "", 0, false
	}

	pageIndex, err := strconv.Atoi(remainder[separatorIndex+1:])
	if err != nil || pageIndex < 0 {
		return "", 0, false
	}

	messageID := strings.TrimSpace(remainder[:separatorIndex])
	if messageID == "" {
		return "", 0, false
	}

	return messageID, pageIndex, true
}

func (instance *bot) handleViewOnRentryButton(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if interaction == nil || interaction.Message == nil {
		return fmt.Errorf("view on Rentry interaction without message: %w", os.ErrInvalid)
	}

	messageNode, ok := instance.nodes.get(interaction.Message.ID)
	if !ok {
		return respondInteractionTextWithFlags(
			session,
			interaction.Interaction,
			"No response content available.",
			discordgo.MessageFlagsEphemeral,
		)
	}

	messageNode.mu.Lock()
	cachedURL := strings.TrimSpace(messageNode.rentryURL)
	responseText := messageNode.text
	initialized := messageNode.initialized
	messageNode.mu.Unlock()

	if cachedURL != "" {
		return respondInteractionTextWithFlags(
			session,
			interaction.Interaction,
			"View on Rentry: "+cachedURL,
			discordgo.MessageFlagsEphemeral,
		)
	}

	if !initialized || strings.TrimSpace(responseText) == "" {
		return respondInteractionTextWithFlags(
			session,
			interaction.Interaction,
			"Response text is not ready yet.",
			discordgo.MessageFlagsEphemeral,
		)
	}

	if instance.rentry == nil {
		return respondInteractionTextWithFlags(
			session,
			interaction.Interaction,
			"Rentry is unavailable right now.",
			discordgo.MessageFlagsEphemeral,
		)
	}

	rentryURL, err := instance.rentry.createEntry(context.Background(), responseText)
	if err != nil {
		slog.Warn("create Rentry entry", "message_id", interaction.Message.ID, "error", err)

		return respondInteractionTextWithFlags(
			session,
			interaction.Interaction,
			"Couldn't create a Rentry page right now.",
			discordgo.MessageFlagsEphemeral,
		)
	}

	messageNode.mu.Lock()
	if strings.TrimSpace(messageNode.rentryURL) == "" {
		messageNode.rentryURL = rentryURL
	} else {
		rentryURL = messageNode.rentryURL
	}

	instance.nodes.cacheLockedNode(interaction.Message.ID, messageNode)
	messageNode.mu.Unlock()

	instance.nodes.persistBestEffort()

	return respondInteractionTextWithFlags(
		session,
		interaction.Interaction,
		"View on Rentry: "+rentryURL,
		discordgo.MessageFlagsEphemeral,
	)
}

func (instance *bot) handleModelCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	loadedConfig, err := loadConfig(instance.configPath)
	if err != nil {
		return fmt.Errorf("load config for model command: %w", err)
	}

	channelIDs, err := instance.interactionChannelIDs(interaction)
	if err != nil {
		slog.Warn("resolve interaction channel ids", "channel_id", interaction.ChannelID, "error", err)
		channelIDs = []string{interaction.ChannelID}
	}

	if lockedModel, ok := loadedConfig.lockedModelForChannelIDs(channelIDs); ok {
		return respondInteractionText(
			session,
			interaction.Interaction,
			fmt.Sprintf("This channel is locked to `%s`. `/model` is disabled here.", lockedModel),
		)
	}

	return handleConfiguredModelCommand(
		session,
		interaction,
		instance.currentModelForConfig(loadedConfig),
		instance.setCurrentModel,
		loadedConfig,
		"Current model",
		"Model switched to",
		"model switched",
	)
}

func (instance *bot) handleSearchDeciderModelCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	loadedConfig, err := loadConfig(instance.configPath)
	if err != nil {
		return fmt.Errorf("load config for search decider model command: %w", err)
	}

	return handleConfiguredModelCommand(
		session,
		interaction,
		instance.currentSearchDeciderModelForConfig(loadedConfig),
		instance.setCurrentSearchDeciderModel,
		loadedConfig,
		"Current search decider model",
		"Search decider model switched to",
		"search decider model switched",
	)
}

func handleConfiguredModelCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
	currentModel string,
	setCurrentModel func(string),
	loadedConfig config,
	currentLabel string,
	switchedLabel string,
	logMessage string,
) error {
	requestedModel := interactionOptionString(interaction.ApplicationCommandData().Options)

	var responseText string

	switch {
	case requestedModel == currentModel:
		responseText = fmt.Sprintf("%s: `%s`", currentLabel, currentModel)
	case !loadedConfig.hasModel(requestedModel):
		responseText = "Unknown model."
	default:
		setCurrentModel(requestedModel)
		responseText = fmt.Sprintf("%s: `%s`", switchedLabel, requestedModel)
		slog.Info(logMessage, "model", requestedModel)
	}

	err := respondInteractionText(session, interaction.Interaction, responseText)
	if err != nil {
		return fmt.Errorf("respond to model command: %w", err)
	}

	return nil
}

func (instance *bot) handleModelAutocomplete(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	loadedConfig, err := loadConfig(instance.configPath)
	if err != nil {
		return fmt.Errorf("load config for autocomplete: %w", err)
	}

	channelIDs, err := instance.interactionChannelIDs(interaction)
	if err != nil {
		slog.Warn("resolve interaction channel ids", "channel_id", interaction.ChannelID, "error", err)
		channelIDs = []string{interaction.ChannelID}
	}

	if lockedModel, ok := loadedConfig.lockedModelForChannelIDs(channelIDs); ok {
		return respondInteractionChoices(
			session,
			interaction.Interaction,
			lockedModelAutocompleteChoices(
				lockedModel,
				interactionOptionString(interaction.ApplicationCommandData().Options),
			),
		)
	}

	return handleConfiguredModelAutocomplete(
		session,
		interaction,
		instance.currentModelForConfig(loadedConfig),
		loadedConfig,
	)
}

func (instance *bot) handleSearchDeciderModelAutocomplete(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	loadedConfig, err := loadConfig(instance.configPath)
	if err != nil {
		return fmt.Errorf("load config for search decider autocomplete: %w", err)
	}

	return handleConfiguredModelAutocomplete(
		session,
		interaction,
		instance.currentSearchDeciderModelForConfig(loadedConfig),
		loadedConfig,
	)
}

func handleConfiguredModelAutocomplete(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
	currentModel string,
	loadedConfig config,
) error {
	currentText := interactionOptionString(interaction.ApplicationCommandData().Options)
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, maxAutocompleteChoices)

	if containsFold(currentModel, currentText) {
		choice := new(discordgo.ApplicationCommandOptionChoice)
		choice.Name = "* " + currentModel + " (current)"
		choice.Value = currentModel
		choices = append(choices, choice)
	}

	for _, modelName := range loadedConfig.ModelOrder {
		if modelName == currentModel || !containsFold(modelName, currentText) {
			continue
		}

		choice := new(discordgo.ApplicationCommandOptionChoice)
		choice.Name = "o " + modelName
		choice.Value = modelName
		choices = append(choices, choice)

		if len(choices) == maxAutocompleteChoices {
			break
		}
	}

	err := respondInteractionChoices(session, interaction.Interaction, choices)
	if err != nil {
		return fmt.Errorf("respond to autocomplete: %w", err)
	}

	return nil
}

func lockedModelAutocompleteChoices(
	lockedModel string,
	currentText string,
) []*discordgo.ApplicationCommandOptionChoice {
	if !containsFold(lockedModel, currentText) {
		return nil
	}

	choice := new(discordgo.ApplicationCommandOptionChoice)
	choice.Name = "x " + lockedModel + " (locked)"
	choice.Value = lockedModel

	return []*discordgo.ApplicationCommandOptionChoice{choice}
}

func (instance *bot) interactionChannelIDs(
	interaction *discordgo.InteractionCreate,
) ([]string, error) {
	if interaction == nil || interaction.Interaction == nil {
		return nil, fmt.Errorf("interaction is required: %w", os.ErrInvalid)
	}

	return instance.channelContextIDs(interaction.ChannelID, interaction.GuildID)
}

func interactionOptionString(options []*discordgo.ApplicationCommandInteractionDataOption) string {
	if len(options) == 0 {
		return ""
	}

	return options[0].StringValue()
}

func respondInteractionText(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	content string,
) error {
	return respondInteractionTextWithFlags(session, interaction, content, 0)
}

func respondInteractionTextWithFlags(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	content string,
	flags discordgo.MessageFlags,
) error {
	return respondInteractionMessage(
		session,
		interaction,
		discordgo.InteractionResponseChannelMessageWithSource,
		content,
		nil,
		flags,
	)
}

func respondInteractionMessage(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	responseType discordgo.InteractionResponseType,
	content string,
	components []discordgo.MessageComponent,
	flags discordgo.MessageFlags,
) error {
	response := new(discordgo.InteractionResponse)
	response.Type = responseType

	responseData := new(discordgo.InteractionResponseData)
	responseData.Content = content

	responseData.Flags = flags
	if components != nil {
		responseData.Components = components
	}

	response.Data = responseData

	err := session.InteractionRespond(interaction, response)
	if err != nil {
		return fmt.Errorf("send interaction response: %w", err)
	}

	return nil
}

func respondInteractionChoices(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	choices []*discordgo.ApplicationCommandOptionChoice,
) error {
	response := new(discordgo.InteractionResponse)
	response.Type = discordgo.InteractionApplicationCommandAutocompleteResult

	responseData := new(discordgo.InteractionResponseData)
	responseData.Choices = choices
	response.Data = responseData

	err := session.InteractionRespond(interaction, response)
	if err != nil {
		return fmt.Errorf("send interaction choices: %w", err)
	}

	return nil
}
