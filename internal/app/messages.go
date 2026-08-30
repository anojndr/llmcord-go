package app

import (
	"context"
	"errors"
	"fmt"
	providers "llmcord-go/internal/providers"
	"log/slog"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	defaultProviderVerbosityLow = "low"
	defaultOpenAIProviderName   = "openai"
)

func (instance *bot) handleMessageCreate(
	_ *discordgo.Session,
	messageCreate *discordgo.MessageCreate,
) {
	if messageCreate == nil || messageCreate.Message == nil {
		return
	}

	message := messageCreate.Message

	if !instance.markMessageSeen(message.ID) {
		slog.Debug("skipping duplicate message event", "message_id", message.ID)

		return
	}

	if instance.enforceMaintenanceMode(message) {
		return
	}

	botUserID := ""
	if instance.session.State != nil && instance.session.State.User != nil {
		botUserID = instance.session.State.User.ID
	}

	loadedConfig, err := loadConfig(instance.configPath)
	if err != nil {
		LogError(
			"load config for incoming message",
			err,
			"message_id",
			message.ID,
			"channel_id",
			message.ChannelID,
		)

		return
	}

	channelIDs, err := instance.messageChannelIDs(message)
	if err != nil {
		logWarn("resolve channel ids", err, "channel_id", message.ChannelID)
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

	if instance.handleXFixup(message, botUserID) {
		instance.nodes.evictExcess()

		return
	}

	facebookVideoReply := shouldReplyWithFacebookVideos(message, botUserID)
	youtubeShortsReply := !facebookVideoReply && shouldReplyWithYouTubeShorts(message, botUserID)

	if shouldIgnoreIncomingMessage(message, botUserID) && !facebookVideoReply && !youtubeShortsReply {
		return
	}

	if facebookVideoReply {
		instance.replyWithFacebookVideos(context.Background(), message)

		instance.nodes.evictExcess()

		return
	}

	if youtubeShortsReply {
		instance.replyWithYouTubeShorts(context.Background(), message)

		instance.nodes.evictExcess()

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
		LogError(
			"respond to message",
			err,
			"message_id",
			message.ID,
			"channel_id",
			message.ChannelID,
		)
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

func shouldReplyWithFacebookVideos(message *discordgo.Message, botUserID string) bool {
	if message == nil || message.Author == nil || message.Author.Bot {
		return false
	}

	if messageMentionsBot(message, botUserID) {
		return false
	}

	return len(extractFacebookURLs(message.Content)) > 0
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

func (instance *bot) channelContextIDs(channelID, guildID string) ([]string, error) {
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
	progress := instance.startRequestProgress(ctx, message, providerSlashModel)

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
		progress.fail(ctx, err)

		return err
	}

	slog.Info(
		"message received",
		"message_id",
		message.ID,
		"user_id",
		message.Author.ID,
		"attachments",
		len(message.Attachments),
		"conversation_length",
		len(request.Messages),
		"content",
		message.Content,
	)

	err = instance.generateAndSendResponse(ctx, loadedConfig, request, tracker, warnings)
	if err != nil {
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
		return chatCompletionRequest{}, nil, nil,
			fmt.Errorf("build message conversation: %w", err)
	}

	if len(messages) == 0 {
		fallbackMessage, fallbackWarnings := fallbackAttachmentDownloadConversation(
			message,
			warnings,
		)
		if fallbackMessage != nil {
			messages = append(messages, *fallbackMessage)
			warnings = fallbackWarnings
		}
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
		return chatCompletionRequest{}, nil, nil,
			fmt.Errorf("augment prepared message response: %w", err)
	}

	provider, err := configuredModelProvider(loadedConfig, providerSlashModel)
	if err != nil {
		return chatCompletionRequest{}, nil, nil, err
	}

	unmutatedMessages := append([]chatMessage(nil), messages...)

	if provider.AutoAppendSearchWeb || provider.AutoAppendShortAnswer || provider.AutoAppendPrioritizeTruth {
		appendedMessages, appendErr := applyAutoAppend(provider, messages)
		if appendErr != nil {
			return chatCompletionRequest{}, nil, nil, appendErr
		}

		messages = appendedMessages
	}

	requestMessages := messages
	if !provider.DontSendSystemPrompt {
		requestMessages = prependSystemPrompt(messages, loadedConfig.SystemPrompt, time.Now())
	}

	request, err := instance.buildPreparedChatCompletionRequest(
		loadedConfig,
		provider,
		providerSlashModel,
		requestMessages,
	)
	if err != nil {
		return chatCompletionRequest{}, nil, nil, err
	}

	request.RequestID = strings.TrimSpace(message.ID)

	providers.AssignOpenAIPromptCacheKey(&request, message, instance.nodes, loadedConfig.MaxMessages)

	progress.advance(requestProgressStageGeneratingResponse)

	tracker := progress.handoff(request.ConfiguredModel, searchMetadata)
	if tracker != nil {
		tracker.originalMessages = unmutatedMessages
	}

	return request, tracker, warnings, nil
}

// buildPreparedChatCompletionRequest assembles the outgoing request for a
// prepared conversation and attaches the web_search tool when the provider's
// models may search the web.
func (instance *bot) buildPreparedChatCompletionRequest(
	loadedConfig config,
	provider providerConfig,
	providerSlashModel string,
	messages []chatMessage,
) (chatCompletionRequest, error) {
	request, err := buildChatCompletionRequest(
		loadedConfig,
		providerSlashModel,
		messages,
		instance.currentGroundingEnabled(provider),
	)
	if err != nil {
		return chatCompletionRequest{}, fmt.Errorf("build chat completion request: %w", err)
	}

	if instance.webSearchToolEnabled(loadedConfig, provider) {
		request.Tools = []providers.FunctionTool{
			providers.WebSearchTool(webSearchToolMaxQueries),
		}
	}

	return request, nil
}

func (instance *bot) buildFallbackRequest(
	loadedConfig config,
	fallbackModel string,
	request chatCompletionRequest,
	tracker *responseTracker,
) (chatCompletionRequest, error) {
	fallbackProvider, err := configuredModelProvider(loadedConfig, fallbackModel)
	if err != nil {
		return chatCompletionRequest{}, err
	}

	messages := request.Messages

	usingOriginalMessages := tracker != nil && len(tracker.originalMessages) > 0
	if usingOriginalMessages {
		messages = tracker.originalMessages
	}

	if fallbackProvider.AutoAppendSearchWeb || fallbackProvider.AutoAppendShortAnswer || fallbackProvider.AutoAppendPrioritizeTruth {
		appendedMessages, appendErr := applyAutoAppend(fallbackProvider, messages)
		if appendErr != nil {
			return chatCompletionRequest{}, appendErr
		}

		messages = appendedMessages
	}

	if usingOriginalMessages && !fallbackProvider.DontSendSystemPrompt {
		messages = prependSystemPrompt(messages, loadedConfig.SystemPrompt, time.Now())
	}

	fallbackRequest, err := buildChatCompletionRequest(
		loadedConfig,
		fallbackModel,
		messages,
		instance.currentGroundingEnabled(fallbackProvider),
	)
	if err != nil {
		return chatCompletionRequest{}, err
	}

	if instance.webSearchToolEnabled(loadedConfig, fallbackProvider) {
		fallbackRequest.Tools = []providers.FunctionTool{
			providers.WebSearchTool(webSearchToolMaxQueries),
		}
	}

	fallbackRequest.RequestID = request.RequestID

	if tracker != nil && tracker.sourceMessage != nil {
		providers.AssignOpenAIPromptCacheKey(
			&fallbackRequest,
			tracker.sourceMessage,
			instance.nodes,
			loadedConfig.MaxMessages,
		)
	}

	return fallbackRequest, nil
}

func fallbackAttachmentDownloadConversation(
	sourceMessage *discordgo.Message,
	warnings []string,
) (*chatMessage, []string) {
	if sourceMessage == nil || len(sourceMessage.Attachments) == 0 {
		return nil, warnings
	}

	content := attachmentDownloadFallbackText

	warningSet := make(map[string]struct{}, len(warnings)+1)
	for _, warning := range warnings {
		appendUniqueWarning(warningSet, warning)
	}

	appendUniqueWarning(warningSet, attachmentDownloadWarningText)

	message := new(chatMessage)
	message.Role = messageRoleUser
	message.Content = content

	return message, sortedWarnings(warningSet)
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
			fmt.Errorf("augment conversation with extracted document content: %w", err)
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
		message,
		messages,
		warnings,
		urlExtractionText,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("augment conversation: %w", err)
	}

	err = instance.persistAugmentedSourceMessage(ctx, message, messages)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("persist augmented source message: %w", err)
	}

	return messages, searchMetadata, warnings, nil
}

func (instance *bot) persistAugmentedSourceMessage(
	ctx context.Context,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) error {
	if sourceMessage == nil {
		return nil
	}

	index, err := latestUserMessageIndex(conversation)
	if err != nil {
		return fmt.Errorf("find latest user message: %w", err)
	}

	text, media, err := retainedMessageNodeContent(conversation[index].Content)
	if err != nil {
		return fmt.Errorf("normalize retained user content: %w", err)
	}

	node := instance.nodes.getOrCreate(sourceMessage.ID)

	node.mu.Lock()
	if !node.initialized {
		instance.initializeNode(ctx, sourceMessage, node)
	}

	node.text = text
	node.media = media
	instance.nodes.cacheLockedNode(sourceMessage.ID, node)
	node.mu.Unlock()

	instance.nodes.persistBestEffort()

	return nil
}

func retainedMessageNodeContent(content any) (string, []contentPart, error) {
	switch typedContent := content.(type) {
	case nil:
		return "", nil, nil
	case string:
		return retainedMessageText(typedContent), nil, nil
	case []contentPart:
		media := make([]contentPart, 0, len(typedContent))
		for _, part := range typedContent {
			partType, _ := part["type"].(string)
			if partType == contentTypeText {
				continue
			}

			media = append(media, cloneContentPart(part))
		}

		return retainedMessageText(contentPartsText(typedContent)), media, nil
	default:
		return "", nil, fmt.Errorf(
			"unsupported retained content type %T: %w",
			content,
			os.ErrInvalid,
		)
	}
}

func retainedMessageText(text string) string {
	prompt := parseAugmentedUserPrompt(text)
	prompt.RepliedMessage = ""

	return prompt.render()
}

func normalizedURLExtractionText(text string) string {
	return strings.TrimSpace(parseAugmentedUserPrompt(text).UserQuery)
}

type preparedAugmentationStage struct {
	name    string
	prepare func(context.Context) (preparedConversationAugmentation, error)
}

func prepareAugmentationStages(
	ctx context.Context,
	stages []preparedAugmentationStage,
) []boundedTaskResult[preparedConversationAugmentation] {
	return runTasksConcurrently(
		ctx,
		len(stages),
		len(stages),
		func(taskContext context.Context, index int) (preparedConversationAugmentation, error) {
			return stages[index].prepare(taskContext)
		},
	)
}

func applyAugmentationStages(
	messages []chatMessage,
	warnings []string,
	stages []preparedAugmentationStage,
	stageResults []boundedTaskResult[preparedConversationAugmentation],
) ([]chatMessage, *searchMetadata, []string, error) {
	augmentedMessages := messages

	var searchMetadata *searchMetadata

	for index, stage := range stages {
		result := stageResults[index]
		if result.err != nil {
			return nil, nil, nil, fmt.Errorf(
				"augment conversation with %s: %w",
				stage.name,
				result.err,
			)
		}

		updatedMessages, err := applyPreparedConversationAugmentation(
			augmentedMessages,
			result.value,
		)
		if err != nil {
			return nil, nil, nil, fmt.Errorf(
				"augment conversation with %s: %w",
				stage.name,
				err,
			)
		}

		augmentedMessages = updatedMessages
		searchMetadata = mergeSearchMetadata(searchMetadata, result.value.metadata)

		warnings = append(warnings, result.value.warnings...)
	}

	return augmentedMessages, searchMetadata, warnings, nil
}

func (instance *bot) augmentConversationWithVideoURLs(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	messages []chatMessage,
	urlExtractionText string,
) ([]chatMessage, []string, error) {
	var stages []preparedAugmentationStage
	stages = append(stages, preparedAugmentationStage{
		name: "tiktok",
		prepare: func(taskContext context.Context) (preparedConversationAugmentation, error) {
			return instance.prepareTikTokAugmentation(
				taskContext,
				loadedConfig,
				providerSlashModel,
				urlExtractionText,
			)
		},
	})

	stages = append(stages, preparedAugmentationStage{
		name: "facebook",
		prepare: func(taskContext context.Context) (preparedConversationAugmentation, error) {
			return instance.prepareFacebookAugmentation(
				taskContext,
				loadedConfig,
				providerSlashModel,
				urlExtractionText,
			)
		},
	})

	stages = append(stages, preparedAugmentationStage{
		name: "youtube shorts",
		prepare: func(taskContext context.Context) (preparedConversationAugmentation, error) {
			return instance.prepareYouTubeShortsAugmentation(
				taskContext,
				loadedConfig,
				providerSlashModel,
				urlExtractionText,
			)
		},
	})

	stageResults := prepareAugmentationStages(ctx, stages)

	augmentedMessages, _, preparedWarnings, err := applyAugmentationStages(
		messages,
		nil,
		stages,
		stageResults,
	)
	if err != nil {
		return nil, nil, err
	}

	return augmentedMessages, preparedWarnings, nil
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
		return nil, nil, fmt.Errorf("check document extraction support: %w", err)
	}

	messages, warnings := instance.buildConversation(
		ctx,
		message,
		contentOptions,
		loadedConfig.MaxMessages,
		useGeminiMediaAnalysis,
		usePDFExtraction,
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
		options.allowFiles = true
		options.allowedDocumentMIMETypes = allowedGeminiDocumentMIMETypes()
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
	sourceMessage *discordgo.Message,
	messages []chatMessage,
	warnings []string,
	urlExtractionText string,
) ([]chatMessage, *searchMetadata, []string, error) {
	stages := []preparedAugmentationStage{
		{
			name: "visual search",
			prepare: func(taskContext context.Context) (preparedConversationAugmentation, error) {
				return instance.prepareVisualSearchAugmentation(
					taskContext,
					loadedConfig,
					sourceMessage,
					messages,
				)
			},
		},
	}

	stages = append(
		stages,
		preparedAugmentationStage{
			name: "website",
			prepare: func(taskContext context.Context) (preparedConversationAugmentation, error) {
				return instance.prepareWebsiteAugmentation(
					taskContext,
					loadedConfig,
					urlExtractionText,
				)
			},
		},
		preparedAugmentationStage{
			name: "youtube",
			prepare: func(taskContext context.Context) (preparedConversationAugmentation, error) {
				return instance.prepareYouTubeAugmentation(taskContext, urlExtractionText)
			},
		},
		preparedAugmentationStage{
			name: "reddit",
			prepare: func(taskContext context.Context) (preparedConversationAugmentation, error) {
				return instance.prepareRedditAugmentation(taskContext, urlExtractionText)
			},
		},
	)

	stageResults := prepareAugmentationStages(ctx, stages)

	augmentedMessages, searchMetadata, warnings, err := applyAugmentationStages(
		messages,
		warnings,
		stages,
		stageResults,
	)
	if err != nil {
		return nil, nil, nil, err
	}

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

	if node.role != messageRoleUser {
		return "", node.parentMessage
	}

	return normalizedURLExtractionText(node.urlScanText), node.parentMessage
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
		Role:    messageRoleSystem,
		Content: systemPromptNow(systemPrompt, now),
	}}, messages...)
}

func mergeExtraBody(providerExtraBody, modelParameters map[string]any) map[string]any {
	if len(providerExtraBody) == 0 && len(modelParameters) == 0 {
		return nil
	}

	mergedBody := make(map[string]any, len(providerExtraBody)+len(modelParameters))
	maps.Copy(mergedBody, providerExtraBody)
	maps.Copy(mergedBody, modelParameters)

	return mergedBody
}

func defaultProviderVerbosity(
	providerName string,
	providerAPIKind providerAPIKind,
	extraBody map[string]any,
) map[string]any {
	if !usesDefaultProviderVerbosity(providerName, providerAPIKind) || requestBodyHasVerbosity(extraBody) {
		return extraBody
	}

	if extraBody == nil {
		extraBody = make(map[string]any, 1)
	}

	extraBody["verbosity"] = defaultProviderVerbosityLow

	return extraBody
}

func usesBuiltInOpenAIProvider(
	providerName string,
	providerAPIKind providerAPIKind,
) bool {
	return providerAPIKind == providerAPIKindOpenAI &&
		strings.EqualFold(strings.TrimSpace(providerName), defaultOpenAIProviderName)
}

func usesDefaultProviderVerbosity(providerName string, providerAPIKind providerAPIKind) bool {
	return usesBuiltInOpenAIProvider(providerName, providerAPIKind)
}

func requestBodyHasVerbosity(extraBody map[string]any) bool {
	if len(extraBody) == 0 {
		return false
	}

	if _, ok := extraBody["verbosity"]; ok {
		return true
	}

	rawTextConfig, hasTextConfig := extraBody["text"]
	if !hasTextConfig {
		return false
	}

	textConfig, textConfigOK := rawTextConfig.(map[string]any)
	if !textConfigOK {
		return false
	}

	_, hasVerbosity := textConfig["verbosity"]

	return hasVerbosity
}

func defaultOpenRouterTransforms(provider providerConfig, extraBody map[string]any) map[string]any {
	if !provider.usesOpenRouter() {
		return extraBody
	}

	if _, ok := extraBody[openRouterTransformsField]; ok {
		return extraBody
	}

	if extraBody == nil {
		extraBody = make(map[string]any, 1)
	}

	extraBody[openRouterTransformsField] = []string{openRouterMiddleOutTransform}

	return extraBody
}

func dedicatedReasoningEffort(provider providerConfig, modelParameters map[string]any) (string, bool) {
	if effort, ok := modelReasoningEffortValue(modelParameters); ok {
		return effort, true
	}
	trimmed := strings.TrimSpace(provider.ReasoningEffort)
	if trimmed != "" {
		return strings.ToLower(trimmed), true
	}
	return "", false
}

func buildChatCompletionRequest(
	loadedConfig config,
	providerSlashModel string,
	messages []chatMessage,
	groundingEnabled bool,
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
	useResponsesAPI := providers.ProviderUsesResponsesAPI(providerName, providers.ProviderRequestConfig{
		APIKind:         providers.ProviderAPIKind(providerAPIKind),
		API:             provider.API,
		BaseURL:         provider.BaseURL,
		APIKey:          "",
		APIKeys:         nil,
		UseResponsesAPI: false,
		EnableGrounding: false,
		ExtraHeaders:    nil,
		ExtraQuery:      nil,
		ExtraBody:       nil,
	})
	extraBody := mergeExtraBody(provider.ExtraBody, modelParameters)
	extraBody = defaultProviderVerbosity(providerName, providerAPIKind, extraBody)

	modelName, extraBody, err = normalizeModelAliasForProvider(
		provider,
		providerAPIKind,
		modelName,
		extraBody,
		modelParameters,
		useResponsesAPI,
	)
	if err != nil {
		return chatCompletionRequest{}, err
	}

	extraBody = defaultOpenRouterTransforms(provider, extraBody)

	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providers.ProviderAPIKind(providerAPIKind),
			API:             provider.API,
			BaseURL:         provider.BaseURL,
			APIKey:          provider.primaryAPIKey(),
			APIKeys:         provider.apiKeys(),
			UseResponsesAPI: useResponsesAPI,
			EnableGrounding: groundingEnabled,
			ExtraHeaders:    provider.ExtraHeaders,
			ExtraQuery:      provider.ExtraQuery,
			ExtraBody:       extraBody,
		},
		Model:           modelName,
		ConfiguredModel: providerSlashModel,
		SessionID:       "",
		RequestID:       "",
		Messages:        messages,
		Tools:           nil,
	}, nil
}

// normalizeModelAliasForProvider resolves model alias suffixes and merges
// per-model reasoning effort into the request extra body.
func normalizeModelAliasForProvider(
	provider providerConfig,
	providerAPIKind providerAPIKind,
	modelName string,
	extraBody map[string]any,
	modelParameters map[string]any,
	useResponsesAPI bool,
) (string, map[string]any, error) {
	if providerAPIKind == providerAPIKindGemini {
		resolvedModelName, normalizedExtraBody, normalizeErr := providers.NormalizeGeminiModelAlias(
			modelName,
			extraBody,
		)
		if normalizeErr != nil {
			return "", nil, fmt.Errorf(
				"normalize gemini model alias %q: %w",
				modelName,
				normalizeErr,
			)
		}

		return resolvedModelName, normalizedExtraBody, nil
	}

	if providerAPIKind == providerAPIKindOpenAI {
		if useResponsesAPI {
			modelName, extraBody = providers.NormalizeOpenAIResponsesModelAlias(modelName, extraBody)
			extraBody = providers.NormalizeOpenAIResponsesExtraBody(modelName, extraBody)
		} else {
			modelName, extraBody = providers.NormalizeOpenAIChatCompletionsModelAlias(modelName, extraBody)
		}

		if dedicatedEffort, ok := dedicatedReasoningEffort(provider, modelParameters); ok {
			extraBody = providers.ApplyDedicatedReasoningEffort(extraBody, modelName, dedicatedEffort, useResponsesAPI)
		}
	}

	return modelName, extraBody, nil
}
