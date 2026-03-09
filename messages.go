package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (instance *bot) handleMessageCreate(
	_ *discordgo.Session,
	messageCreate *discordgo.MessageCreate,
) {
	if messageCreate == nil || messageCreate.Message == nil {
		return
	}

	message := messageCreate.Message

	botUserID := ""
	if instance.session.State != nil && instance.session.State.User != nil {
		botUserID = instance.session.State.User.ID
	}

	if shouldIgnoreIncomingMessage(message, botUserID) {
		return
	}

	loadedConfig, err := loadConfig(instance.configPath)
	if err != nil {
		slog.Error("load config for incoming message", "error", err)

		return
	}

	channelIDs, err := instance.messageChannelIDs(message)
	if err != nil {
		slog.Warn("resolve channel ids", "channel_id", message.ChannelID, "error", err)
		channelIDs = []string{message.ChannelID}
	}

	access := accessContext{
		IsDM:       isDirectMessage(message),
		UserID:     message.Author.ID,
		RoleIDs:    messageRoleIDs(message),
		ChannelIDs: channelIDs,
	}
	if !messageAllowed(loadedConfig, access) {
		return
	}

	currentModel := instance.currentModelForConfig(loadedConfig)

	err = instance.respondToMessage(
		context.Background(),
		loadedConfig,
		message,
		currentModel,
	)
	if err != nil {
		slog.Error("respond to message", "error", err, "message_id", message.ID)
	}

	instance.nodes.evictExcess()
}

func shouldIgnoreIncomingMessage(message *discordgo.Message, botUserID string) bool {
	if message.Author == nil || message.Author.Bot {
		return true
	}

	if isDirectMessage(message) {
		return false
	}

	if botUserID == "" {
		return true
	}

	return !messageMentionsUser(message, botUserID)
}

func isDirectMessage(message *discordgo.Message) bool {
	return message.GuildID == ""
}

func messageMentionsUser(message *discordgo.Message, userID string) bool {
	for _, mentionedUser := range message.Mentions {
		if mentionedUser != nil && mentionedUser.ID == userID {
			return true
		}
	}

	return false
}

func messageRoleIDs(message *discordgo.Message) []string {
	if message.Member == nil {
		return nil
	}

	roles := make([]string, 0, len(message.Member.Roles))
	roles = append(roles, message.Member.Roles...)

	return roles
}

func (instance *bot) messageChannelIDs(message *discordgo.Message) ([]string, error) {
	channelIDSet := make(map[string]struct{}, smallMapCapacity)
	channelIDSet[message.ChannelID] = struct{}{}

	if isDirectMessage(message) {
		return channelIDSetKeys(channelIDSet), nil
	}

	channel, err := instance.channelByID(message.ChannelID)
	if err != nil {
		return channelIDSetKeys(channelIDSet), fmt.Errorf("load channel %s: %w", message.ChannelID, err)
	}

	channelIDSet[channel.ID] = struct{}{}
	if channel.ParentID != "" {
		channelIDSet[channel.ParentID] = struct{}{}
	}

	if channel.IsThread() && channel.ParentID != "" {
		parentChannel, parentErr := instance.channelByID(channel.ParentID)
		if parentErr != nil {
			return channelIDSetKeys(channelIDSet), fmt.Errorf("load parent channel %s: %w", channel.ParentID, parentErr)
		}

		if parentChannel.ParentID != "" {
			channelIDSet[parentChannel.ParentID] = struct{}{}
		}
	}

	return channelIDSetKeys(channelIDSet), nil
}

func channelIDSetKeys(channelIDSet map[string]struct{}) []string {
	channelIDs := make([]string, 0, len(channelIDSet))
	for channelID := range channelIDSet {
		channelIDs = append(channelIDs, channelID)
	}

	return channelIDs
}

func (instance *bot) respondToMessage(
	ctx context.Context,
	loadedConfig config,
	message *discordgo.Message,
	providerSlashModel string,
) error {
	providerName, modelName, err := splitConfiguredModel(providerSlashModel)
	if err != nil {
		return fmt.Errorf("parse current model %q: %w", providerSlashModel, err)
	}

	provider, ok := loadedConfig.Providers[providerName]
	if !ok {
		return fmt.Errorf("find provider %q: %w", providerName, os.ErrNotExist)
	}

	maxImages := loadedConfig.MaxImages
	if !isVisionModel(providerSlashModel) {
		maxImages = 0
	}

	messages, warnings := instance.buildConversation(
		ctx,
		message,
		loadedConfig.MaxText,
		maxImages,
		loadedConfig.MaxMessages,
	)

	slog.Info(
		"message received",
		"user_id",
		message.Author.ID,
		"attachments",
		len(message.Attachments),
		"conversation_length",
		len(messages),
		"content",
		message.Content,
	)

	if loadedConfig.SystemPrompt != "" {
		systemMessage := chatMessage{
			Role:    "system",
			Content: systemPromptNow(loadedConfig.SystemPrompt, time.Now()),
		}
		messages = append([]chatMessage{systemMessage}, messages...)
	}

	modelParameters := loadedConfig.Models[providerSlashModel]
	request := chatCompletionRequest{
		BaseURL:      provider.BaseURL,
		APIKey:       providerAPIKey(provider.APIKey),
		Model:        modelName,
		Messages:     messages,
		ExtraHeaders: provider.ExtraHeaders,
		ExtraQuery:   provider.ExtraQuery,
		ExtraBody:    mergeExtraBody(provider.ExtraBody, modelParameters),
	}

	err = instance.generateAndSendResponse(
		ctx,
		request,
		message,
		warnings,
		loadedConfig.UsePlainResponses,
	)
	if err != nil {
		return fmt.Errorf("generate and send response: %w", err)
	}

	return nil
}

func providerAPIKey(apiKey string) string {
	if apiKey == "" {
		return "sk-no-key-required"
	}

	return apiKey
}

func mergeExtraBody(providerExtraBody map[string]any, modelParameters map[string]any) map[string]any {
	if len(providerExtraBody) == 0 && len(modelParameters) == 0 {
		return nil
	}

	mergedBody := make(map[string]any, len(providerExtraBody)+len(modelParameters))
	maps.Copy(mergedBody, providerExtraBody)
	maps.Copy(mergedBody, modelParameters)

	return mergedBody
}
