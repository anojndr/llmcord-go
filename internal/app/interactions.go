package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

const discordUnknownInteractionCode = 10062

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
		if isUnknownInteractionError(err) {
			slog.Info("discard expired interaction", "interaction_id", interaction.ID, "type", interaction.Type)

			return
		}

		LogError(
			"handle interaction",
			err,
			"interaction_id",
			interaction.ID,
			"type",
			interaction.Type,
			"channel_id",
			interaction.ChannelID,
		)
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
	case searchTypeCommandName:
		if interaction.Type == discordgo.InteractionApplicationCommand {
			return instance.handleSearchTypeCommand(session, interaction)
		}

		if interaction.Type == discordgo.InteractionApplicationCommandAutocomplete {
			return instance.handleSearchTypeAutocomplete(session, interaction)
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
	case groundingCommandName:
		return instance.handleGroundingCommand(session, interaction)
	case createChannelCommandName:
		return instance.handleCreateChannelCommand(session, interaction)
	case editChannelNameCommandName:
		return instance.handleEditChannelNameCommand(session, interaction)
	case moveChannelCommandName:
		return instance.handleMoveChannelCommand(session, interaction)
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
	case componentData.CustomID == showImagesButtonCustomID:
		return instance.handleShowImagesButton(session, interaction)
	case componentData.CustomID == showThinkingButtonCustomID:
		return instance.handleShowThinkingButton(session, interaction)
	case componentData.CustomID == createGistButtonCustomID:
		return instance.handleCreateGistButton(session, interaction)
	case strings.HasPrefix(componentData.CustomID, showSourcesPageButtonCustomIDPrefix):
		return instance.handleShowSourcesPageButton(session, interaction)
	case strings.HasPrefix(componentData.CustomID, showImagesPageButtonCustomIDPrefix):
		return instance.handleShowImagesPageButton(session, interaction)
	case strings.HasPrefix(componentData.CustomID, showThinkingPageButtonCustomIDPrefix):
		return instance.handleShowThinkingPageButton(session, interaction)
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

func (instance *bot) handleShowImagesButton(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if interaction == nil || interaction.Message == nil {
		return fmt.Errorf("show images interaction without message: %w", os.ErrInvalid)
	}

	err := respondInteractionDeferredWithFlags(
		session,
		interaction.Interaction,
		discordgo.MessageFlagsEphemeral,
	)
	if err != nil {
		return fmt.Errorf("defer show images interaction response: %w", err)
	}

	query := instance.imageSearchQueryForMessage(interaction.Message.ID)
	if strings.TrimSpace(query) == "" {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"No search query available for images.",
		)
	}

	if instance.imageSearch == nil {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"Image search is unavailable right now.",
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), imageSearchTimeout)
	defer cancel()

	result, err := instance.imageSearch.search(ctx, query, 1, maxImagesLimit)
	if err != nil {
		logWarn("image search failed", err, "message_id", interaction.Message.ID, "query", query)
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Failed to search images for %q.", query),
		)
	}

	if result == nil || len(result.Items) == 0 {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("No images found for %q.", query),
		)
	}

	downloadedItems := instance.imageSearch.downloadImages(ctx, result.Items)
	embeds, files, attachments := buildImageEmbedsAndFiles(query, downloadedItems)

	content := fmt.Sprintf("Top %d images for %q:", len(embeds), query)
	webhookEdit := new(discordgo.WebhookEdit)
	webhookEdit.Content = &content
	webhookEdit.Embeds = &embeds
	if len(files) > 0 {
		webhookEdit.Files = files
		webhookEdit.Attachments = &attachments
	}

	_, err = session.InteractionResponseEdit(interaction.Interaction, webhookEdit)
	if err != nil {
		logWarn("edit interaction response with images", err, "message_id", interaction.Message.ID)
		return err
	}

	return nil
}

func (instance *bot) handleShowThinkingButton(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if interaction == nil || interaction.Message == nil {
		return fmt.Errorf("show thinking interaction without message: %w", os.ErrInvalid)
	}

	content, components := instance.showThinkingPageResponse(interaction.Message.ID, 0)

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

func (instance *bot) handleShowImagesPageButton(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if interaction == nil {
		return fmt.Errorf("show images page interaction without interaction: %w", os.ErrInvalid)
	}

	messageID, pageIndex, ok := parseShowImagesPageButtonCustomID(interaction.MessageComponentData().CustomID)
	if !ok {
		return fmt.Errorf("invalid show images page interaction custom id: %w", os.ErrInvalid)
	}

	content, components := instance.showImagesPageResponse(messageID, pageIndex)

	return respondInteractionMessage(
		session,
		interaction.Interaction,
		discordgo.InteractionResponseUpdateMessage,
		content,
		components,
		0,
	)
}

func (instance *bot) handleShowThinkingPageButton(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if interaction == nil {
		return fmt.Errorf("show thinking page interaction without interaction: %w", os.ErrInvalid)
	}

	messageID, pageIndex, ok := parseShowThinkingPageButtonCustomID(interaction.MessageComponentData().CustomID)
	if !ok {
		return fmt.Errorf("invalid show thinking page interaction custom id: %w", os.ErrInvalid)
	}

	content, components := instance.showThinkingPageResponse(messageID, pageIndex)

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
	totalSources := countSearchSources(searchMetadata)

	if pageIndex < 0 {
		pageIndex = 0
	} else if pageIndex >= len(pages) {
		pageIndex = len(pages) - 1
	}

	return formatSearchSourcesPageContent(pages, pageIndex, totalSources),
		buildShowSourcesPaginationComponents(messageID, pageIndex, len(pages))
}

func (instance *bot) showThinkingPageResponse(messageID string, pageIndex int) (string, []discordgo.MessageComponent) {
	thinkingText := instance.thinkingTextForMessage(messageID)
	pages := formatThinkingPages(thinkingText)

	if pageIndex < 0 {
		pageIndex = 0
	} else if pageIndex >= len(pages) {
		pageIndex = len(pages) - 1
	}

	return formatThinkingPageContent(pages, pageIndex),
		buildShowThinkingPaginationComponents(messageID, pageIndex, len(pages))
}

func (instance *bot) showImagesPageResponse(messageID string, pageIndex int) (string, []discordgo.MessageComponent) {
	query := instance.imageSearchQueryForMessage(messageID)
	if strings.TrimSpace(query) == "" {
		return "No search query available for images.", []discordgo.MessageComponent{}
	}

	if instance.imageSearch == nil {
		return "Image search is unavailable right now.", []discordgo.MessageComponent{}
	}

	const imagesPerPage = maxImagesLimit
	if pageIndex < 0 {
		pageIndex = 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), imageSearchTimeout)
	defer cancel()

	result, err := instance.imageSearch.search(ctx, query, pageIndex+1, imagesPerPage)
	if err != nil {
		logWarn("image search failed", err, "message_id", messageID, "query", query)
		return fmt.Sprintf("Failed to search images for %q.", query), []discordgo.MessageComponent{}
	}

	if result == nil || len(result.Items) == 0 {
		return fmt.Sprintf("No images found for %q.", query), []discordgo.MessageComponent{}
	}

	totalPages := result.TotalPages
	if totalPages < 1 {
		totalPages = 1
	}

	content := formatImageSearchResultsContent(query, result.Items, pageIndex, totalPages)
	components := buildShowImagesPaginationComponents(messageID, pageIndex, totalPages)

	return content, components
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

func (instance *bot) thinkingTextForMessage(messageID string) string {
	messageNode, ok := instance.nodes.get(messageID)
	if !ok {
		return ""
	}

	messageNode.mu.Lock()
	defer messageNode.mu.Unlock()

	thinkingText := strings.TrimSpace(messageNode.thinkingText)
	if thinkingText != "" {
		return thinkingText
	}

	return extractThinkingText(messageNode.text)
}

func (instance *bot) imageSearchQueryForMessage(messageID string) string {
	messageNode, ok := instance.nodes.get(messageID)
	if !ok {
		return ""
	}

	messageNode.mu.Lock()
	defer messageNode.mu.Unlock()

	if messageNode.searchMetadata != nil && len(messageNode.searchMetadata.Queries) > 0 {
		return strings.TrimSpace(messageNode.searchMetadata.Queries[0])
	}

	if messageNode.parentMessage != nil {
		botUserID := ""
		if instance != nil && instance.session != nil && instance.session.State != nil && instance.session.State.User != nil {
			botUserID = instance.session.State.User.ID
		}
		parentText := trimBotMention(messageNode.parentMessage.Content, botUserID)
		if strings.TrimSpace(parentText) != "" {
			return strings.TrimSpace(parentText)
		}
	}

	return ""
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

func buildShowThinkingPaginationComponents(
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
	previousButton.CustomID = showThinkingPageButtonCustomID(messageID, previousPageIndex)
	previousButton.Label = showSourcesPreviousButtonLabel
	previousButton.Style = discordgo.SecondaryButton
	previousButton.Disabled = pageIndex == 0

	nextButton := new(discordgo.Button)
	nextButton.CustomID = showThinkingPageButtonCustomID(messageID, nextPageIndex)
	nextButton.Label = showSourcesNextButtonLabel
	nextButton.Style = discordgo.SecondaryButton
	nextButton.Disabled = pageIndex >= pageCount-1

	row := new(discordgo.ActionsRow)
	row.Components = []discordgo.MessageComponent{previousButton, nextButton}

	return []discordgo.MessageComponent{row}
}

func buildShowImagesPaginationComponents(
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
	previousButton.CustomID = showImagesPageButtonCustomID(messageID, previousPageIndex)
	previousButton.Label = showSourcesPreviousButtonLabel
	previousButton.Style = discordgo.SecondaryButton
	previousButton.Disabled = pageIndex == 0

	nextButton := new(discordgo.Button)
	nextButton.CustomID = showImagesPageButtonCustomID(messageID, nextPageIndex)
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

func showImagesPageButtonCustomID(messageID string, pageIndex int) string {
	return fmt.Sprintf("%s%s:%d", showImagesPageButtonCustomIDPrefix, messageID, pageIndex)
}

func showThinkingPageButtonCustomID(messageID string, pageIndex int) string {
	return fmt.Sprintf("%s%s:%d", showThinkingPageButtonCustomIDPrefix, messageID, pageIndex)
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

func parseShowImagesPageButtonCustomID(customID string) (string, int, bool) {
	remainder, ok := strings.CutPrefix(customID, showImagesPageButtonCustomIDPrefix)
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

func parseShowThinkingPageButtonCustomID(customID string) (string, int, bool) {
	remainder, ok := strings.CutPrefix(customID, showThinkingPageButtonCustomIDPrefix)
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

func formatThinkingPages(thinkingText string) []string {
	trimmedThinkingText := strings.TrimSpace(thinkingText)
	if trimmedThinkingText == "" {
		return []string{showThinkingUnavailableText}
	}

	return splitMessagePages(trimmedThinkingText, showThinkingPageBodyMaxLength)
}

func formatThinkingPageContent(pages []string, pageIndex int) string {
	if len(pages) == 0 {
		return showThinkingUnavailableText
	}

	if pageIndex < 0 {
		pageIndex = 0
	} else if pageIndex >= len(pages) {
		pageIndex = len(pages) - 1
	}

	if len(pages) == 1 {
		return "Thinking Process\n\n" + pages[pageIndex]
	}

	return fmt.Sprintf("Thinking Process (page %d/%d)\n\n%s", pageIndex+1, len(pages), pages[pageIndex])
}

func formatImageSearchResultsContent(
	query string,
	items []imageSearchResultItem,
	pageIndex int,
	totalPages int,
) string {
	var builder strings.Builder
	if totalPages > 1 {
		_, _ = fmt.Fprintf(&builder, "Images for %q (page %d/%d):\n\n", query, pageIndex+1, totalPages)
	} else {
		_, _ = fmt.Fprintf(&builder, "Images for %q:\n\n", query)
	}

	for index, item := range items {
		num := index + 1 + (pageIndex * maxImagesLimit)
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = "Image"
		}
		landingURL := strings.TrimSpace(item.LandingURL)
		imageURL := strings.TrimSpace(item.URL)
		thumbURL := strings.TrimSpace(item.Thumbnail)

		bestURL := imageURL
		if bestURL == "" {
			bestURL = thumbURL
		}
		if bestURL == "" {
			bestURL = landingURL
		}

		if landingURL != "" && landingURL != bestURL {
			_, _ = fmt.Fprintf(&builder, "%d. [%s](<%s>) - <%s>\n", num, title, landingURL, bestURL)
		} else {
			_, _ = fmt.Fprintf(&builder, "%d. %s - <%s>\n", num, title, bestURL)
		}
	}

	return strings.TrimSpace(builder.String())
}

func (instance *bot) handleCreateGistButton(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if interaction == nil || interaction.Message == nil {
		return fmt.Errorf("create gist interaction without message: %w", os.ErrInvalid)
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
	cachedURL := strings.TrimSpace(messageNode.gistURL)
	responseText := messageNode.text
	initialized := messageNode.initialized
	messageNode.mu.Unlock()

	if cachedURL != "" {
		return respondInteractionTextWithFlags(
			session,
			interaction.Interaction,
			"View response better on GitHub Gist: <"+cachedURL+">",
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

	if instance.gist == nil {
		return respondInteractionTextWithFlags(
			session,
			interaction.Interaction,
			"GitHub Gist is unavailable right now.",
			discordgo.MessageFlagsEphemeral,
		)
	}

	err := respondInteractionDeferredWithFlags(
		session,
		interaction.Interaction,
		discordgo.MessageFlagsEphemeral,
	)
	if err != nil {
		return fmt.Errorf("defer gist interaction response: %w", err)
	}

	gistURL, err := instance.gist.createGist(context.Background(), responseText)
	if err != nil {
		logWarn("create GitHub gist", err, "message_id", interaction.Message.ID)

		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"Couldn't create a GitHub gist right now.",
		)
	}

	messageNode.mu.Lock()
	if strings.TrimSpace(messageNode.gistURL) == "" {
		messageNode.gistURL = gistURL
	} else {
		gistURL = messageNode.gistURL
	}

	instance.nodes.cacheLockedNode(interaction.Message.ID, messageNode)
	messageNode.mu.Unlock()

	instance.nodes.persistBestEffort()

	return editInteractionResponseText(
		session,
		interaction.Interaction,
		"View response better on GitHub Gist: <"+gistURL+">",
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
		logWarn("resolve interaction channel ids", err, "channel_id", interaction.ChannelID)
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
		configuredModelCommandOptions{
			currentModel:    instance.currentModelForConfig(loadedConfig),
			setCurrentModel: instance.setCurrentModel,
			loadedConfig:    loadedConfig,
			currentLabel:    "Current model",
			switchedLabel:   "Model switched to",
			logMessage:      "model switched",
		},
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

	channelIDs, err := instance.interactionChannelIDs(interaction)
	if err != nil {
		logWarn("resolve interaction channel ids", err, "channel_id", interaction.ChannelID)
		channelIDs = []string{interaction.ChannelID}
	}

	if lockedModel, ok := loadedConfig.lockedSearchDeciderModelForChannelIDs(channelIDs); ok {
		return respondInteractionText(
			session,
			interaction.Interaction,
			fmt.Sprintf(
				"This channel is locked to `%s`. `/searchdecidermodel` is disabled here.",
				lockedModel,
			),
		)
	}

	return handleConfiguredModelCommand(
		session,
		interaction,
		configuredModelCommandOptions{
			currentModel:    instance.currentSearchDeciderModelForConfig(loadedConfig),
			setCurrentModel: instance.setCurrentSearchDeciderModel,
			loadedConfig:    loadedConfig,
			currentLabel:    "Current search decider model",
			switchedLabel:   "Search decider model switched to",
			logMessage:      "search decider model switched",
		},
	)
}

type configuredModelCommandOptions struct {
	currentModel    string
	setCurrentModel func(string)
	loadedConfig    config
	currentLabel    string
	switchedLabel   string
	logMessage      string
}

func handleConfiguredModelCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
	options configuredModelCommandOptions,
) error {
	requestedModel := interactionOptionString(interaction.ApplicationCommandData().Options)

	var responseText string

	switch {
	case requestedModel == options.currentModel:
		responseText = fmt.Sprintf("%s: `%s`", options.currentLabel, options.currentModel)
	case !options.loadedConfig.hasModel(requestedModel):
		responseText = "Unknown model."
	default:
		options.setCurrentModel(requestedModel)
		responseText = fmt.Sprintf("%s: `%s`", options.switchedLabel, requestedModel)
		slog.Info(options.logMessage, "model", requestedModel)
	}

	err := respondInteractionText(session, interaction.Interaction, responseText)
	if err != nil {
		return fmt.Errorf("respond to model command: %w", err)
	}

	return nil
}

func (instance *bot) handleSearchTypeCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	loadedConfig, err := loadConfig(instance.configPath)
	if err != nil {
		return fmt.Errorf("load config for search type command: %w", err)
	}

	if !loadedConfig.WebSearch.exaUsesAPI() {
		return respondInteractionText(
			session,
			interaction.Interaction,
			"Exa Search API is not configured. Set `web_search.exa.api_key` to use `/searchtype`.",
		)
	}

	requestedSearchType, ok := normalizeExaSearchType(
		interactionOptionString(interaction.ApplicationCommandData().Options),
	)
	if !ok {
		return respondInteractionText(
			session,
			interaction.Interaction,
			"Unknown Exa search type. Available options (ordered lowest to highest latency): "+
				formattedExaSearchTypeOptions()+".",
		)
	}

	currentSearchType := instance.currentExaSearchType()

	responseText := fmt.Sprintf("Current Exa search type: `%s`", currentSearchType)
	if requestedSearchType != currentSearchType {
		instance.setCurrentExaSearchType(requestedSearchType)
		responseText = fmt.Sprintf("Exa search type switched to: `%s`", requestedSearchType)
		slog.Info("search type switched", "type", requestedSearchType)
	}

	err = respondInteractionText(session, interaction.Interaction, responseText)
	if err != nil {
		return fmt.Errorf("respond to search type command: %w", err)
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
		logWarn("resolve interaction channel ids", err, "channel_id", interaction.ChannelID)
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

	channelIDs, err := instance.interactionChannelIDs(interaction)
	if err != nil {
		logWarn("resolve interaction channel ids", err, "channel_id", interaction.ChannelID)
		channelIDs = []string{interaction.ChannelID}
	}

	if lockedModel, ok := loadedConfig.lockedSearchDeciderModelForChannelIDs(channelIDs); ok {
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
		instance.currentSearchDeciderModelForConfig(loadedConfig),
		loadedConfig,
	)
}

func (instance *bot) handleSearchTypeAutocomplete(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	return respondInteractionChoices(
		session,
		interaction.Interaction,
		exaSearchTypeAutocompleteChoices(
			instance.currentExaSearchType(),
			interactionOptionString(interaction.ApplicationCommandData().Options),
		),
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
	lockedModel, currentText string,
) []*discordgo.ApplicationCommandOptionChoice {
	if !containsFold(lockedModel, currentText) {
		return nil
	}

	choice := new(discordgo.ApplicationCommandOptionChoice)
	choice.Name = "x " + lockedModel + " (locked)"
	choice.Value = lockedModel

	return []*discordgo.ApplicationCommandOptionChoice{choice}
}

func exaSearchTypeAutocompleteChoices(
	currentSearchType, currentText string,
) []*discordgo.ApplicationCommandOptionChoice {
	searchTypes := exaSearchTypes()
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(searchTypes))

	if containsFold(currentSearchType, currentText) {
		choice := new(discordgo.ApplicationCommandOptionChoice)
		choice.Name = "* " + currentSearchType + " (current)"
		choice.Value = currentSearchType
		choices = append(choices, choice)
	}

	for _, searchType := range searchTypes {
		if searchType == currentSearchType || !containsFold(searchType, currentText) {
			continue
		}

		choice := new(discordgo.ApplicationCommandOptionChoice)
		choice.Name = "o " + searchType
		choice.Value = searchType
		choices = append(choices, choice)
	}

	return choices
}

func formattedExaSearchTypeOptions() string {
	searchTypes := exaSearchTypes()
	formattedValues := make([]string, 0, len(searchTypes))

	for _, searchType := range searchTypes {
		formattedValues = append(formattedValues, fmt.Sprintf("`%s`", searchType))
	}

	return strings.Join(formattedValues, ", ")
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

func respondInteractionDeferredWithFlags(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	flags discordgo.MessageFlags,
) error {
	response := new(discordgo.InteractionResponse)
	response.Type = discordgo.InteractionResponseDeferredChannelMessageWithSource

	responseData := new(discordgo.InteractionResponseData)
	responseData.Flags = flags
	response.Data = responseData

	err := session.InteractionRespond(interaction, response)
	if err != nil {
		return fmt.Errorf("send deferred interaction response: %w", err)
	}

	return nil
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

func editInteractionResponseText(
	session *discordgo.Session,
	interaction *discordgo.Interaction,
	content string,
) error {
	response := new(discordgo.WebhookEdit)
	response.Content = &content

	_, err := session.InteractionResponseEdit(interaction, response)
	if err != nil {
		return fmt.Errorf("edit interaction response: %w", err)
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

func isUnknownInteractionError(err error) bool {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) {
		return isUnknownInteractionRESTError(restErr)
	}

	var restErrValue discordgo.RESTError
	if errors.As(err, &restErrValue) {
		return isUnknownInteractionRESTError(&restErrValue)
	}

	return false
}

func isUnknownInteractionRESTError(err *discordgo.RESTError) bool {
	return err != nil &&
		err.Message != nil &&
		err.Message.Code == discordUnknownInteractionCode
}

func (instance *bot) handleGroundingCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	options := interaction.ApplicationCommandData().Options

	var requestedEnabled *bool

	if len(options) > 0 {
		val := options[0].BoolValue()
		requestedEnabled = &val
	}

	loadedConfig, err := loadConfig(instance.configPath)
	if err != nil {
		return fmt.Errorf("load config for grounding command: %w", err)
	}

	channelIDs, err := instance.interactionChannelIDs(interaction)
	if err != nil {
		logWarn("resolve interaction channel ids", err, "channel_id", interaction.ChannelID)
		channelIDs = []string{interaction.ChannelID}
	}

	currentModel := instance.currentModelForChannelIDs(loadedConfig, channelIDs)

	provider, err := configuredModelProvider(loadedConfig, currentModel)
	if err != nil {
		return respondInteractionText(session, interaction.Interaction, "Could not resolve current provider.")
	}

	if provider.apiKind() != providerAPIKindGemini {
		return respondInteractionText(session, interaction.Interaction, "Grounding is only supported for Gemini models.")
	}

	currentEnabled := instance.currentGroundingEnabled(provider)

	if requestedEnabled == nil {
		return respondInteractionText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Current Gemini grounding: `%t`", currentEnabled),
		)
	}

	if *requestedEnabled == currentEnabled {
		return respondInteractionText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Gemini grounding is already `%t`.", currentEnabled),
		)
	}

	instance.setCurrentGroundingEnabled(requestedEnabled)
	slog.Info("grounding switched", "enabled", *requestedEnabled)

	return respondInteractionText(
		session,
		interaction.Interaction,
		fmt.Sprintf("Gemini grounding switched to: `%t`", *requestedEnabled),
	)
}

func (instance *bot) handleCreateChannelCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	commandData := interaction.ApplicationCommandData()

	nameOption := commandData.GetOption(createChannelNameOptionName)

	channelName := ""
	if nameOption != nil {
		channelName = nameOption.StringValue()
	}

	if channelName == "" {
		return respondInteractionText(
			session,
			interaction.Interaction,
			"`channelname` is required.",
		)
	}

	guildID := interaction.GuildID
	if guildID == "" {
		return respondInteractionText(
			session,
			interaction.Interaction,
			"This command can only be used in a guild.",
		)
	}

	err := respondInteractionDeferredWithFlags(
		session,
		interaction.Interaction,
		0,
	)
	if err != nil {
		return fmt.Errorf("defer create channel interaction response: %w", err)
	}

	parentID := ""

	if interaction.ChannelID != "" {
		currentChannel, err := session.Channel(interaction.ChannelID)
		if err != nil {
			logWarn(
				"create channel command failed to load current channel",
				err,
				"channel_id",
				interaction.ChannelID,
			)

			return editInteractionResponseText(
				session,
				interaction.Interaction,
				fmt.Sprintf("Failed to load channel `%s`.", interaction.ChannelID),
			)
		}

		parentID = currentChannel.ParentID
	}

	createData := discordgo.GuildChannelCreateData{
		Name:                 channelName,
		Type:                 discordgo.ChannelTypeGuildText,
		Topic:                "",
		Bitrate:              0,
		UserLimit:            0,
		RateLimitPerUser:     0,
		Position:             0,
		PermissionOverwrites: nil,
		ParentID:             parentID,
		NSFW:                 false,
	}

	createdChannel, err := session.GuildChannelCreateComplex(guildID, createData)
	if err != nil {
		logWarn("create channel command failed", err, "guild_id", guildID, "name", channelName)

		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Failed to create channel `%s`.", channelName),
		)
	}

	slog.Info(
		"channel created",
		"channel_id",
		createdChannel.ID,
		"guild_id",
		guildID,
		"name",
		createdChannel.Name,
	)

	return editInteractionResponseText(
		session,
		interaction.Interaction,
		fmt.Sprintf("Created channel `%s`.", createdChannel.Name),
	)
}

func (instance *bot) handleEditChannelNameCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	commandData := interaction.ApplicationCommandData()

	channelIDOption := commandData.GetOption(editChannelNameChannelIDOptionName)
	newNameOption := commandData.GetOption(editChannelNameOptionName)

	channelID := ""
	if channelIDOption != nil {
		channelID = channelIDOption.StringValue()
	}

	newName := ""
	if newNameOption != nil {
		newName = newNameOption.StringValue()
	}

	if channelID == "" || newName == "" {
		return respondInteractionText(
			session,
			interaction.Interaction,
			"Both `channelid` and `newchannelname` are required.",
		)
	}

	err := respondInteractionDeferredWithFlags(
		session,
		interaction.Interaction,
		0,
	)
	if err != nil {
		return fmt.Errorf("defer edit channel name interaction response: %w", err)
	}

	channelEdit := new(discordgo.ChannelEdit)
	channelEdit.Name = newName

	editedChannel, err := session.ChannelEdit(channelID, channelEdit)
	if err != nil {
		logWarn("edit channel name command failed", err, "channel_id", channelID)

		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Failed to rename channel `%s`.", channelID),
		)
	}

	slog.Info("channel renamed", "channel_id", channelID, "name", editedChannel.Name)

	return editInteractionResponseText(
		session,
		interaction.Interaction,
		fmt.Sprintf("Renamed channel to `%s`.", editedChannel.Name),
	)
}

func (instance *bot) handleMoveChannelCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	channelID, movement, howMany := moveChannelInputOptions(interaction.ApplicationCommandData())

	if channelID == "" {
		return respondInteractionText(
			session,
			interaction.Interaction,
			"`channelid` is required.",
		)
	}

	if movement != moveChannelMovementUp && movement != moveChannelMovementDown {
		return respondInteractionText(
			session,
			interaction.Interaction,
			"`movement` must be `up` or `down`.",
		)
	}

	if howMany <= 0 {
		return respondInteractionText(
			session,
			interaction.Interaction,
			"`howmany` must be a positive integer.",
		)
	}

	err := respondInteractionDeferredWithFlags(
		session,
		interaction.Interaction,
		0,
	)
	if err != nil {
		return fmt.Errorf("defer move channel interaction response: %w", err)
	}

	channel, err := session.Channel(channelID)
	if err != nil {
		logWarn("move channel command failed to load channel", err, "channel_id", channelID)

		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Failed to load channel `%s`.", channelID),
		)
	}

	guildID := interaction.GuildID
	if guildID == "" {
		guildID = channel.GuildID
	}

	if guildID == "" {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Channel `%s` is not in a guild.", channel.Name),
		)
	}

	channels, err := session.GuildChannels(guildID)
	if err != nil {
		logWarn("move channel command failed to load guild channels", err, "guild_id", guildID)

		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Failed to load channels for guild `%s`.", guildID),
		)
	}

	orderedGuild := orderedGuildChannels(channels)
	orderedSiblings := orderedSiblingChannels(orderedGuild, channel)

	targetIndex := channelIndex(orderedSiblings, channelID)
	if targetIndex < 0 {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Failed to find channel `%s` in its current section.", channel.Name),
		)
	}

	destinationIndex := targetIndex
	if movement == moveChannelMovementUp {
		destinationIndex -= howMany
	} else {
		destinationIndex += howMany
	}

	if destinationIndex < 0 {
		destinationIndex = 0
	}

	if destinationIndex >= len(orderedSiblings) {
		destinationIndex = len(orderedSiblings) - 1
	}

	actualMove := targetIndex - destinationIndex
	if actualMove < 0 {
		actualMove = -actualMove
	}

	if actualMove == 0 {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Channel `%s` is already as far %s as possible.", channel.Name, movement),
		)
	}

	orderedAfterMove := moveChannelInOrder(orderedSiblings, targetIndex, destinationIndex)
	orderedAfterGuild := guildOrderAfterSiblingMove(orderedGuild, orderedSiblings, orderedAfterMove)

	updates := channelPositionUpdates(orderedGuild, orderedAfterGuild)

	if err := session.GuildChannelsReorder(guildID, updates); err != nil {
		logWarn("move channel command failed", err, "channel_id", channelID, "guild_id", guildID)

		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Failed to move channel `%s`.", channelID),
		)
	}

	slog.Info(
		"channel moved",
		"channel_id",
		channelID,
		"guild_id",
		guildID,
		"movement",
		movement,
		"how_many",
		howMany,
		"actual_move",
		actualMove,
	)

	return editInteractionResponseText(
		session,
		interaction.Interaction,
		fmt.Sprintf(
			"Moved channel `%s` %s %d visible channel(s).",
			channel.Name,
			movement,
			actualMove,
		),
	)
}

func moveChannelInputOptions(
	commandData discordgo.ApplicationCommandInteractionData,
) (string, string, int) {
	channelID := ""
	movement := ""
	howMany := 0

	channelIDOption := commandData.GetOption(moveChannelChannelIDOptionName)
	movementOption := commandData.GetOption(moveChannelMovementOptionName)
	howManyOption := commandData.GetOption(moveChannelHowManyOptionName)

	if channelIDOption != nil {
		channelID = channelIDOption.StringValue()
	}

	if movementOption != nil {
		movement = movementOption.StringValue()
	}

	if howManyOption != nil {
		howMany = int(howManyOption.IntValue())
	}

	return channelID, movement, howMany
}

func orderedGuildChannels(channels []*discordgo.Channel) []*discordgo.Channel {
	ordered := append([]*discordgo.Channel(nil), channels...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Position != ordered[j].Position {
			return ordered[i].Position < ordered[j].Position
		}

		return compareMessageIDs(ordered[i].ID, ordered[j].ID) < 0
	})

	return ordered
}

func orderedSiblingChannels(channels []*discordgo.Channel, channel *discordgo.Channel) []*discordgo.Channel {
	ordered := orderedGuildChannels(channels)
	section := channelRootSection(ordered, channel)
	siblings := make([]*discordgo.Channel, 0, len(channels))

	for _, candidate := range ordered {
		if candidate.ParentID == channel.ParentID &&
			candidate.Type != discordgo.ChannelTypeGuildCategory &&
			(candidate.ParentID != "" || channelRootSection(ordered, candidate) == section) {
			siblings = append(siblings, candidate)
		}
	}

	return siblings
}

func channelRootSection(channels []*discordgo.Channel, channel *discordgo.Channel) int {
	if channel.ParentID != "" {
		return 0
	}

	section := 0

	for _, candidate := range channels {
		if candidate.ID == channel.ID {
			break
		}

		if candidate.Type == discordgo.ChannelTypeGuildCategory {
			section++
		}
	}

	return section
}

func channelIndex(channels []*discordgo.Channel, channelID string) int {
	for index, channel := range channels {
		if channel.ID == channelID {
			return index
		}
	}

	return -1
}

func moveChannelInOrder(channels []*discordgo.Channel, from, to int) []*discordgo.Channel {
	result := append([]*discordgo.Channel(nil), channels...)
	channel := result[from]
	copy(result[from:], result[from+1:])
	copy(result[to+1:], result[to:])
	result[to] = channel

	return result
}

func guildOrderAfterSiblingMove(
	guildChannels, beforeSiblings, afterSiblings []*discordgo.Channel,
) []*discordgo.Channel {
	result := append([]*discordgo.Channel(nil), guildChannels...)
	siblingPositions := make(map[string]int, len(beforeSiblings))

	for index, channel := range beforeSiblings {
		siblingPositions[channel.ID] = index
	}

	for index := range result {
		if position, ok := siblingPositions[result[index].ID]; ok {
			result[index] = afterSiblings[position]
		}
	}

	return result
}

func channelPositionUpdates(
	before, after []*discordgo.Channel,
) []*discordgo.Channel {
	seenPositions := make(map[int]struct{}, len(before))
	duplicatePosition := false

	for _, channel := range before {
		if _, seen := seenPositions[channel.Position]; seen {
			duplicatePosition = true
			break
		}

		seenPositions[channel.Position] = struct{}{}
	}

	updates := make([]*discordgo.Channel, 0, len(before))
	for index, channel := range after {
		if !duplicatePosition && before[index].ID == channel.ID {
			continue
		}

		position := before[index].Position
		if duplicatePosition {
			position = index
		}

		updates = append(updates, &discordgo.Channel{ID: channel.ID, Position: position})
	}

	return updates
}
