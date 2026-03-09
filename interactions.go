package main

import (
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
)

func (instance *bot) handleInteractionCreate(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) {
	commandData := interaction.ApplicationCommandData()
	if commandData.Name != modelCommandName {
		return
	}

	var err error

	switch interaction.Type {
	case discordgo.InteractionApplicationCommand:
		err = instance.handleModelCommand(session, interaction)
	case discordgo.InteractionApplicationCommandAutocomplete:
		err = instance.handleModelAutocomplete(session, interaction)
	case discordgo.InteractionPing,
		discordgo.InteractionMessageComponent,
		discordgo.InteractionModalSubmit:
		return
	default:
		return
	}

	if err != nil {
		slog.Error("handle interaction", "error", err)
	}
}

func (instance *bot) handleModelCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	loadedConfig, err := loadConfig(instance.configPath)
	if err != nil {
		return fmt.Errorf("load config for model command: %w", err)
	}

	requestedModel := interactionOptionString(interaction.ApplicationCommandData().Options)
	currentModel := instance.currentModelForConfig(loadedConfig)

	var responseText string

	switch {
	case requestedModel == currentModel:
		responseText = fmt.Sprintf("Current model: `%s`", currentModel)
	case !loadedConfig.hasModel(requestedModel):
		responseText = "Unknown model."
	case containsID(loadedConfig.Permissions.Users.AdminIDs, interactionUserID(interaction.Interaction)):
		instance.setCurrentModel(requestedModel)
		responseText = fmt.Sprintf("Model switched to: `%s`", requestedModel)
		slog.Info("model switched", "model", requestedModel)
	default:
		responseText = "You don't have permission to change the model."
	}

	err = respondInteractionText(session, interaction.Interaction, responseText)
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

	currentModel := instance.currentModelForConfig(loadedConfig)
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

	err = respondInteractionChoices(session, interaction.Interaction, choices)
	if err != nil {
		return fmt.Errorf("respond to autocomplete: %w", err)
	}

	return nil
}

func interactionOptionString(options []*discordgo.ApplicationCommandInteractionDataOption) string {
	if len(options) == 0 {
		return ""
	}

	return options[0].StringValue()
}

func interactionUserID(interaction *discordgo.Interaction) string {
	if interaction.Member != nil && interaction.Member.User != nil {
		return interaction.Member.User.ID
	}

	if interaction.User != nil {
		return interaction.User.ID
	}

	return ""
}

func respondInteractionText(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	content string,
) error {
	response := new(discordgo.InteractionResponse)
	response.Type = discordgo.InteractionResponseChannelMessageWithSource

	responseData := new(discordgo.InteractionResponseData)
	responseData.Content = content
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
