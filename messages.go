package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
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

	currentModel := instance.currentModelForChannelIDs(loadedConfig, channelIDs)

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

	return !messageMentionsBot(message, botUserID)
}

func isDirectMessage(message *discordgo.Message) bool {
	return message.GuildID == ""
}

func messageMentionsBot(message *discordgo.Message, botUserID string) bool {
	return messageMentionsUser(message, botUserID) || hasAtAIMention(message.Content)
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
	return instance.channelContextIDs(message.ChannelID, message.GuildID)
}

func (instance *bot) channelContextIDs(channelID string, guildID string) ([]string, error) {
	channelIDs := make([]string, 0, smallMapCapacity)
	channelIDSet := make(map[string]struct{}, smallMapCapacity)
	channelIDs = appendUniqueChannelID(channelIDs, channelIDSet, channelID)

	if guildID == "" {
		return channelIDs, nil
	}

	channel, err := instance.channelByID(channelID)
	if err != nil {
		return channelIDs, fmt.Errorf("load channel %s: %w", channelID, err)
	}

	channelIDs = appendUniqueChannelID(channelIDs, channelIDSet, channel.ID)
	channelIDs = appendUniqueChannelID(channelIDs, channelIDSet, channel.ParentID)

	if channel.IsThread() && channel.ParentID != "" {
		parentChannel, parentErr := instance.channelByID(channel.ParentID)
		if parentErr != nil {
			return channelIDs, fmt.Errorf("load parent channel %s: %w", channel.ParentID, parentErr)
		}

		channelIDs = appendUniqueChannelID(channelIDs, channelIDSet, parentChannel.ParentID)
	}

	return channelIDs, nil
}

func appendUniqueChannelID(
	channelIDs []string,
	channelIDSet map[string]struct{},
	channelID string,
) []string {
	if channelID == "" {
		return channelIDs
	}

	if _, ok := channelIDSet[channelID]; ok {
		return channelIDs
	}

	channelIDSet[channelID] = struct{}{}

	return append(channelIDs, channelID)
}

func (instance *bot) respondToMessage(
	ctx context.Context,
	loadedConfig config,
	message *discordgo.Message,
	providerSlashModel string,
) error {
	progress := instance.startRequestProgress(message, providerSlashModel)

	stopTyping := instance.startTyping(ctx, message.ChannelID)
	defer stopTyping()

	request, tracker, warnings, err := instance.prepareMessageResponse(
		ctx,
		loadedConfig,
		message,
		providerSlashModel,
		progress,
	)
	if err != nil {
		return err
	}

	err = instance.generateAndSendResponse(
		ctx,
		request,
		tracker,
		warnings,
		loadedConfig.UsePlainResponses,
	)
	if err != nil {
		instance.renderProgressFailure(tracker)

		return fmt.Errorf("generate and send response: %w", err)
	}

	return nil
}

func (instance *bot) prepareMessageResponse(
	ctx context.Context,
	loadedConfig config,
	message *discordgo.Message,
	providerSlashModel string,
	progress *requestProgress,
) (chatCompletionRequest, *responseTracker, []string, error) {
	messages, warnings, err := instance.buildMessageConversation(
		ctx,
		loadedConfig,
		message,
		providerSlashModel,
	)
	if err != nil {
		progress.fail()

		return chatCompletionRequest{}, nil, nil,
			fmt.Errorf("build message conversation: %w", err)
	}

	progress.advance(requestProgressStageGatheringContext)

	messages, searchMetadata, warnings, err := instance.augmentPreparedMessageResponse(
		ctx,
		loadedConfig,
		message,
		providerSlashModel,
		messages,
		warnings,
	)
	if err != nil {
		progress.fail()

		return chatCompletionRequest{}, nil, nil,
			fmt.Errorf("augment prepared message response: %w", err)
	}

	messages = prependSystemPrompt(messages, loadedConfig.SystemPrompt, time.Now())

	request, err := buildChatCompletionRequest(loadedConfig, providerSlashModel, messages)
	if err != nil {
		progress.fail()

		return chatCompletionRequest{}, nil, nil,
			fmt.Errorf("build chat completion request: %w", err)
	}

	progress.advance(requestProgressStageGeneratingResponse)
	tracker := progress.handoff(request.ConfiguredModel, searchMetadata)

	return request, tracker, warnings, nil
}

func (instance *bot) augmentPreparedMessageResponse(
	ctx context.Context,
	loadedConfig config,
	message *discordgo.Message,
	providerSlashModel string,
	messages []chatMessage,
	warnings []string,
) ([]chatMessage, *searchMetadata, []string, error) {
	urlExtractionText := instance.sourceMessageURLExtractionText(ctx, message)

	messages, videoWarnings, err := instance.augmentConversationWithVideoURLs(
		ctx,
		loadedConfig,
		providerSlashModel,
		messages,
		urlExtractionText,
	)
	if err != nil {
		return nil, nil, nil,
			fmt.Errorf("augment conversation with video urls: %w", err)
	}

	warnings = append(warnings, videoWarnings...)

	messages, err = instance.maybeAugmentConversationWithPDFContents(
		ctx,
		loadedConfig,
		providerSlashModel,
		message,
		messages,
	)
	if err != nil {
		return nil, nil, nil,
			fmt.Errorf("augment conversation with extracted pdf content: %w", err)
	}

	messages, err = instance.maybeAugmentConversationWithGeminiMedia(
		ctx,
		loadedConfig,
		providerSlashModel,
		message,
		messages,
	)
	if err != nil {
		return nil, nil, nil,
			fmt.Errorf("augment conversation with gemini media: %w", err)
	}

	messages, searchMetadata, warnings, err := instance.augmentConversation(
		ctx,
		loadedConfig,
		providerSlashModel,
		message,
		messages,
		warnings,
		urlExtractionText,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("augment conversation: %w", err)
	}

	return messages, searchMetadata, warnings, nil
}

func (instance *bot) augmentConversationWithVideoURLs(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	messages []chatMessage,
	urlExtractionText string,
) ([]chatMessage, []string, error) {
	augmentedMessages, warnings, err := instance.maybeAugmentConversationWithTikTok(
		ctx,
		loadedConfig,
		providerSlashModel,
		messages,
		urlExtractionText,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("augment conversation with tiktok: %w", err)
	}

	var facebookWarnings []string

	augmentedMessages, facebookWarnings, err = instance.maybeAugmentConversationWithFacebook(
		ctx,
		loadedConfig,
		providerSlashModel,
		augmentedMessages,
		urlExtractionText,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("augment conversation with facebook: %w", err)
	}

	warnings = append(warnings, facebookWarnings...)

	return augmentedMessages, warnings, nil
}

func (instance *bot) buildMessageConversation(
	ctx context.Context,
	loadedConfig config,
	message *discordgo.Message,
	providerSlashModel string,
) ([]chatMessage, []string, error) {
	contentOptions, err := messageContentOptionsForModel(
		loadedConfig,
		providerSlashModel,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build message content options: %w", err)
	}

	useGeminiMediaAnalysis, err := canUseGeminiMediaAnalysis(
		loadedConfig,
		providerSlashModel,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("check gemini media analysis support: %w", err)
	}

	usePDFExtraction, err := canExtractPDFContents(
		loadedConfig,
		providerSlashModel,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("check pdf extraction support: %w", err)
	}

	messages, warnings := instance.buildConversation(
		ctx,
		message,
		loadedConfig.MaxText,
		contentOptions,
		loadedConfig.MaxMessages,
		useGeminiMediaAnalysis,
		usePDFExtraction,
	)
	if message.MessageReference != nil && len(messages) >= 2 {
		messages, err = appendReplyTargetToConversation(
			messages,
			messages[len(messages)-2],
		)
		if err != nil {
			return nil, nil, fmt.Errorf("append reply target to latest user message: %w", err)
		}
	}

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

	return messages, warnings, nil
}

func messageContentOptionsForModel(
	loadedConfig config,
	providerSlashModel string,
) (messageContentOptions, error) {
	provider, err := configuredModelProvider(loadedConfig, providerSlashModel)
	if err != nil {
		return messageContentOptions{}, err
	}

	var options messageContentOptions
	if isVisionModel(providerSlashModel) {
		options.maxImages = loadedConfig.MaxImages
	}

	if provider.apiKind() == providerAPIKindGemini {
		options.allowAudio = true
		options.allowDocuments = true
		options.allowVideo = true
	}

	return options, nil
}

func canUseGeminiMediaAnalysis(
	loadedConfig config,
	providerSlashModel string,
) (bool, error) {
	apiKind, err := configuredModelAPIKind(loadedConfig, providerSlashModel)
	if err != nil {
		return false, err
	}

	if apiKind == providerAPIKindGemini {
		return false, nil
	}

	_, err = configuredGeminiMediaModel(loadedConfig)
	if err == nil {
		return true, nil
	}

	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return false, err
}

func configuredModelProvider(
	loadedConfig config,
	providerSlashModel string,
) (providerConfig, error) {
	providerName, _, err := splitConfiguredModel(providerSlashModel)
	if err != nil {
		return providerConfig{}, fmt.Errorf(
			"parse configured model %q: %w",
			providerSlashModel,
			err,
		)
	}

	provider, ok := loadedConfig.Providers[providerName]
	if !ok {
		return providerConfig{}, fmt.Errorf(
			"find provider %q: %w",
			providerName,
			os.ErrNotExist,
		)
	}

	return provider, nil
}

func (instance *bot) augmentConversation(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	sourceMessage *discordgo.Message,
	messages []chatMessage,
	warnings []string,
	urlExtractionText string,
) ([]chatMessage, *searchMetadata, []string, error) {
	augmentedMessages, visualSearchWarnings, err := instance.maybeAugmentConversationWithVisualSearch(
		ctx,
		sourceMessage,
		messages,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("augment conversation with visual search: %w", err)
	}

	warnings = append(warnings, visualSearchWarnings...)

	augmentedMessages, websiteWarnings, err := instance.maybeAugmentConversationWithWebsite(
		ctx,
		augmentedMessages,
		urlExtractionText,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("augment conversation with website: %w", err)
	}

	warnings = append(warnings, websiteWarnings...)

	augmentedMessages, youtubeWarnings, err := instance.maybeAugmentConversationWithYouTube(
		ctx,
		augmentedMessages,
		urlExtractionText,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("augment conversation with youtube: %w", err)
	}

	warnings = append(warnings, youtubeWarnings...)

	augmentedMessages, redditWarnings, err := instance.maybeAugmentConversationWithReddit(
		ctx,
		augmentedMessages,
		urlExtractionText,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("augment conversation with reddit: %w", err)
	}

	warnings = append(warnings, redditWarnings...)

	augmentedMessages, searchMetadata, searchWarnings, err := instance.maybeAugmentConversationWithWebSearch(
		ctx,
		loadedConfig,
		providerSlashModel,
		sourceMessage,
		augmentedMessages,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("augment conversation with web search: %w", err)
	}

	warnings = append(warnings, searchWarnings...)

	return augmentedMessages, searchMetadata, warnings, nil
}

func (instance *bot) sourceMessageURLExtractionText(
	ctx context.Context,
	sourceMessage *discordgo.Message,
) string {
	sourceText, parentMessage := instance.messageNodeURLExtractionText(ctx, sourceMessage)
	if sourceMessage == nil || sourceMessage.MessageReference == nil {
		return sourceText
	}

	replyTargetText, _ := instance.messageNodeURLExtractionText(ctx, parentMessage)

	return joinNonEmpty([]string{replyTargetText, sourceText})
}

func (instance *bot) messageNodeURLExtractionText(
	ctx context.Context,
	message *discordgo.Message,
) (string, *discordgo.Message) {
	if message == nil {
		return "", nil
	}

	node := instance.nodes.getOrCreate(message.ID)
	node.mu.Lock()
	defer node.mu.Unlock()

	if !node.initialized {
		instance.initializeNode(ctx, message, node)
	}

	return strings.TrimSpace(node.urlScanText), node.parentMessage
}

func prependSystemPrompt(
	messages []chatMessage,
	systemPrompt string,
	now time.Time,
) []chatMessage {
	if systemPrompt == "" {
		return messages
	}

	return append([]chatMessage{{
		Role:    "system",
		Content: systemPromptNow(systemPrompt, now),
	}}, messages...)
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

func buildChatCompletionRequest(
	loadedConfig config,
	providerSlashModel string,
	messages []chatMessage,
) (chatCompletionRequest, error) {
	providerName, modelName, err := splitConfiguredModel(providerSlashModel)
	if err != nil {
		return chatCompletionRequest{}, fmt.Errorf(
			"parse current model %q: %w",
			providerSlashModel,
			err,
		)
	}

	provider, ok := loadedConfig.Providers[providerName]
	if !ok {
		return chatCompletionRequest{}, fmt.Errorf(
			"find provider %q: %w",
			providerName,
			os.ErrNotExist,
		)
	}

	modelParameters := loadedConfig.Models[providerSlashModel]
	providerAPIKind := provider.apiKind()
	extraBody := mergeExtraBody(provider.ExtraBody, modelParameters)

	if providerAPIKind == providerAPIKindGemini {
		resolvedModelName, normalizedExtraBody, normalizeErr := normalizeGeminiModelAlias(
			modelName,
			extraBody,
		)
		if normalizeErr != nil {
			return chatCompletionRequest{}, fmt.Errorf(
				"normalize gemini model alias %q: %w",
				modelName,
				normalizeErr,
			)
		}

		modelName = resolvedModelName
		extraBody = normalizedExtraBody
	}

	if providerAPIKind == providerAPIKindOpenAICodex {
		modelName, extraBody = normalizeOpenAICodexModelAlias(modelName, extraBody)
	}

	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:      providerAPIKind,
			BaseURL:      provider.BaseURL,
			APIKey:       provider.primaryAPIKey(),
			APIKeys:      provider.apiKeys(),
			ExtraHeaders: provider.ExtraHeaders,
			ExtraQuery:   provider.ExtraQuery,
			ExtraBody:    extraBody,
		},
		Model:           modelName,
		ConfiguredModel: providerSlashModel,
		Messages:        messages,
	}, nil
}
