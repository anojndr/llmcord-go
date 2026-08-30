package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/bwmarrin/discordgo"

	providers "llmcord-go/internal/providers"
)

const geminiVideoAnalysisPrompt = `Describe the video and transcribe it using timestamps.

Divide the video into sequential timestamp segments that cover the entire duration.

The output must follow this format:

Example: 30-second video

<output>
Video description per timestamp:

0s to 10s: A cat jumps down from the cabinet
10s to 20s: The cat licks its toes
20s to 30s: The cat opens its mouth, probably meowing

Visible on-screen text:

0s to 30s: Watch this cat jump down from the cabinet.

Video transcription per timestamp:

0s to 10s: Come on, jump down, kitty ` + geminiKittyWord + `
10s to 20s: You are so cute while licking your toes
20s to 30s: Why are you meowing? What do you want?
</output>`

const geminiKittyWord = "kitty"

const geminiAudioAnalysisPrompt = `Transcribe this audio using timestamps.

Divide the audio into sequential timestamp segments that cover the entire duration.

The output must follow this format:

Example: 30-second audio

<output>
Audio transcription per timestamp:

0s to 10s: Come on, jump down, kitty ` + geminiKittyWord + `
10s to 20s: You are so cute while licking your toes
20s to 30s: Why are you meowing? What do you want?
</output>`

const geminiMediaAnalysisConcurrency = 4

func (instance *bot) maybeAugmentConversationWithGeminiMedia(
	ctx context.Context,
	loadedConfig config,
	providerSlashModel string,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) ([]chatMessage, error) {
	apiKind, err := configuredModelAPIKind(loadedConfig, providerSlashModel)
	if err != nil {
		return nil, err
	}

	if apiKind == providerAPIKindGemini {
		return conversation, nil
	}

	mediaParts, err := instance.audioVideoPartsForMessages(
		ctx,
		instance.attachmentAugmentationMessages(ctx, sourceMessage),
	)
	if err != nil {
		return nil, fmt.Errorf("load media parts for gemini analysis: %w", err)
	}

	if len(mediaParts) == 0 {
		return conversation, nil
	}

	geminiModel, err := configuredGeminiMediaModel(loadedConfig)
	if err != nil {
		return nil, err
	}

	analyses := make([]string, 0, len(mediaParts))
	results := runTasksConcurrently(
		ctx,
		geminiMediaAnalysisConcurrency,
		len(mediaParts),
		func(taskContext context.Context, index int) (string, error) {
			return instance.analyzeMediaWithGemini(
				taskContext,
				loadedConfig,
				geminiModel,
				cloneContentPart(mediaParts[index]),
			)
		},
	)

	for index, result := range results {
		if result.err != nil {
			return nil, fmt.Errorf(
				"analyze media file %d with gemini: %w",
				index+1,
				result.err,
			)
		}

		analyses = append(analyses, result.value)
	}

	augmentedConversation, err := appendMediaAnalysesToConversation(
		conversation,
		analyses,
	)
	if err != nil {
		return nil, fmt.Errorf("append gemini media analyses: %w", err)
	}

	return augmentedConversation, nil
}

func configuredModelAPIKind(
	loadedConfig config,
	providerSlashModel string,
) (providerAPIKind, error) {
	provider, err := configuredModelProvider(loadedConfig, providerSlashModel)
	if err != nil {
		return "", err
	}

	return provider.apiKind(), nil
}

func configuredGeminiMediaModel(loadedConfig config) (string, error) {
	if strings.TrimSpace(loadedConfig.MediaAnalysisModel) != "" {
		return strings.TrimSpace(loadedConfig.MediaAnalysisModel), nil
	}

	candidates := make([]string, 0, len(loadedConfig.ModelOrder))
	candidates = append(candidates, loadedConfig.ModelOrder...)

	seenModels := make(map[string]struct{}, len(candidates))

	for _, candidate := range candidates {
		trimmedCandidate := strings.TrimSpace(candidate)
		if trimmedCandidate == "" {
			continue
		}

		if _, seen := seenModels[trimmedCandidate]; seen {
			continue
		}

		seenModels[trimmedCandidate] = struct{}{}

		apiKind, err := configuredModelAPIKind(loadedConfig, trimmedCandidate)
		if err != nil {
			return "", fmt.Errorf("inspect configured model %q: %w", trimmedCandidate, err)
		}

		if apiKind == providerAPIKindGemini {
			return trimmedCandidate, nil
		}
	}

	return "", fmt.Errorf("find configured gemini model: %w", os.ErrNotExist)
}

func (instance *bot) audioVideoPartsForMessages(
	ctx context.Context,
	messages []*discordgo.Message,
) ([]contentPart, error) {
	return instance.messagePartsForMessages(
		ctx,
		messages,
		partNeedsGeminiMediaAnalysis,
	)
}

func (instance *bot) imagePartsForMessage(
	ctx context.Context,
	message *discordgo.Message,
) ([]contentPart, error) {
	return instance.messagePartsForMessage(ctx, message, partIsImage)
}

func (instance *bot) messagePartsForMessage(
	ctx context.Context,
	message *discordgo.Message,
	includePart func(contentPart) bool,
) ([]contentPart, error) {
	if message == nil {
		return nil, nil
	}

	node := instance.nodes.getOrCreate(message.ID)

	node.mu.Lock()
	defer node.mu.Unlock()

	if !node.initialized {
		instance.initializeNode(ctx, message, node)
	}

	parts := make([]contentPart, 0, len(node.media))

	for _, part := range node.media {
		if !includePart(part) {
			continue
		}

		parts = append(parts, cloneContentPart(part))
	}

	return parts, nil
}

func (instance *bot) messagePartsForMessages(
	ctx context.Context,
	messages []*discordgo.Message,
	includePart func(contentPart) bool,
) ([]contentPart, error) {
	parts := make([]contentPart, 0)

	for _, message := range messages {
		messageParts, err := instance.messagePartsForMessage(
			ctx,
			message,
			includePart,
		)
		if err != nil {
			return nil, err
		}

		parts = append(parts, messageParts...)
	}

	return parts, nil
}

func (instance *bot) attachmentAugmentationMessages(
	ctx context.Context,
	sourceMessage *discordgo.Message,
) []*discordgo.Message {
	if sourceMessage == nil {
		return nil
	}

	messages := make([]*discordgo.Message, 0, smallMapCapacity)
	messageIDs := make(map[string]struct{}, smallMapCapacity)
	messages = appendUniqueAttachmentContextMessage(
		messages,
		messageIDs,
		sourceMessage,
	)

	replyTarget := instance.immediateReplyTargetMessage(ctx, sourceMessage)
	messages = appendUniqueAttachmentContextMessage(
		messages,
		messageIDs,
		replyTarget,
	)
	messages = appendUniqueAttachmentContextMessage(
		messages,
		messageIDs,
		instance.replyTargetAttachmentSourceMessage(ctx, replyTarget),
	)

	return messages
}

func (instance *bot) attachmentPreprocessingMessageIDSet(
	ctx context.Context,
	sourceMessage *discordgo.Message,
) map[string]struct{} {
	messages := instance.attachmentAugmentationMessages(ctx, sourceMessage)
	messageIDs := make(map[string]struct{}, len(messages))

	for _, message := range messages {
		if message == nil {
			continue
		}

		messageID := strings.TrimSpace(message.ID)
		if messageID == "" {
			continue
		}

		messageIDs[messageID] = struct{}{}
	}

	return messageIDs
}

func (instance *bot) immediateReplyTargetMessage(
	ctx context.Context,
	sourceMessage *discordgo.Message,
) *discordgo.Message {
	if sourceMessage == nil || sourceMessage.MessageReference == nil {
		return nil
	}

	node := instance.nodes.getOrCreate(sourceMessage.ID)

	node.mu.Lock()
	defer node.mu.Unlock()

	if !node.initialized {
		instance.initializeNode(ctx, sourceMessage, node)
	}

	if node.parentMessage == nil {
		return nil
	}

	if node.parentMessage.ID != strings.TrimSpace(sourceMessage.MessageReference.MessageID) {
		return nil
	}

	return node.parentMessage
}

func appendUniqueAttachmentContextMessage(
	messages []*discordgo.Message,
	messageIDs map[string]struct{},
	message *discordgo.Message,
) []*discordgo.Message {
	if message == nil {
		return messages
	}

	messageID := strings.TrimSpace(message.ID)
	if messageID == "" {
		return messages
	}

	if _, ok := messageIDs[messageID]; ok {
		return messages
	}

	messageIDs[messageID] = struct{}{}

	return append(messages, message)
}

func (instance *bot) replyTargetAttachmentSourceMessage(
	ctx context.Context,
	replyTarget *discordgo.Message,
) *discordgo.Message {
	if replyTarget == nil {
		return nil
	}

	node := instance.nodes.getOrCreate(replyTarget.ID)

	node.mu.Lock()
	defer node.mu.Unlock()

	if !node.initialized {
		instance.initializeNode(ctx, replyTarget, node)
	}

	if node.role != messageRoleAssistant {
		return nil
	}

	return node.parentMessage
}

func partNeedsGeminiMediaAnalysis(part contentPart) bool {
	partType, _ := part["type"].(string)

	return partType == contentTypeAudioData || partType == contentTypeVideoData
}

func partIsImage(part contentPart) bool {
	partType, _ := part["type"].(string)

	return partType == contentTypeImageURL
}

func cloneContentPart(part contentPart) contentPart {
	clonedPart := make(contentPart, len(part))

	for key, value := range part {
		if bytesValue, ok := value.([]byte); ok {
			clonedBytes := make([]byte, len(bytesValue))
			copy(clonedBytes, bytesValue)

			clonedPart[key] = clonedBytes

			continue
		}

		clonedPart[key] = value
	}

	return clonedPart
}

// geminiMediaAnalysisStaticFallbackModels are Gemini models verified to
// accept audio input, tried after the configured media-analysis model rejects
// the media. The Gemini API reports models without audio support (e.g.
// gemini-3.5-flash-lite) as a generic 500 INTERNAL error, so the analysis
// retries on the same family's flash model and on broadly available
// audio-capable models before giving up.
var geminiMediaAnalysisStaticFallbackModels = []string{
	"gemini-3.5-flash",
	"gemini-2.5-flash",
}

func (instance *bot) analyzeMediaWithGemini(
	ctx context.Context,
	loadedConfig config,
	geminiModel string,
	mediaPart contentPart,
) (string, error) {
	prompt, err := geminiMediaAnalysisPrompt(mediaPart)
	if err != nil {
		return "", err
	}

	messages := []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{messageTypeKey: contentTypeText, messageTextKey: prompt},
				mediaPart,
			},
		},
	}

	responseText, primaryErr := instance.analyzeMediaWithGeminiModel(
		ctx,
		loadedConfig,
		geminiModel,
		messages,
	)
	if primaryErr == nil {
		return responseText, nil
	}

	if !geminiMediaAnalysisMayFallback(primaryErr) {
		return "", primaryErr
	}

	for _, candidateModel := range geminiMediaAnalysisCandidateModels(geminiModel) {
		logWarn(
			"retry gemini media analysis with fallback model",
			primaryErr,
			"configured_model",
			geminiModel,
			"fallback_model",
			candidateModel,
		)

		responseText, fallbackErr := instance.analyzeMediaWithGeminiModel(
			ctx,
			loadedConfig,
			candidateModel,
			messages,
		)
		if fallbackErr == nil {
			return responseText, nil
		}

		logWarn(
			"gemini media analysis fallback model failed",
			fallbackErr,
			"configured_model",
			geminiModel,
			"fallback_model",
			candidateModel,
		)
	}

	return "", primaryErr
}

func (instance *bot) analyzeMediaWithGeminiModel(
	ctx context.Context,
	loadedConfig config,
	geminiModel string,
	messages []chatMessage,
) (string, error) {
	request, err := buildChatCompletionRequest(
		loadedConfig,
		geminiModel,
		messages,
		false,
	)
	if err != nil {
		return "", fmt.Errorf("build gemini media analysis request: %w", err)
	}

	responseText, err := collectChatCompletionText(ctx, instance.chatCompletions, request)
	if err != nil {
		return "", fmt.Errorf("collect gemini media analysis: %w", err)
	}

	trimmedResponse := strings.TrimSpace(responseText)
	if trimmedResponse == "" {
		return "", fmt.Errorf("empty gemini media analysis: %w", os.ErrInvalid)
	}

	return trimmedResponse, nil
}

// geminiMediaAnalysisMayFallback reports whether a media analysis failure is
// worth retrying on another Gemini model: the API rejected the request with an
// internal server error (its generic response to media the model cannot
// process, e.g. audio on gemini-3.5-flash-lite) or the model no longer exists.
func geminiMediaAnalysisMayFallback(err error) bool {
	statusCode, found := providers.GeminiAPIStatusCode(err)
	if !found {
		return false
	}

	return statusCode == http.StatusInternalServerError ||
		statusCode == http.StatusNotFound
}

// geminiMediaAnalysisCandidateModels returns the models tried after the
// configured media-analysis model fails: first the same model with the
// "-lite" family dropped (gemini-3.5-flash-lite -> gemini-3.5-flash, keeping
// provider and alias suffixes), then the static audio-capable fallbacks on
// the same provider.
func geminiMediaAnalysisCandidateModels(configuredModel string) []string {
	provider, modelPart, found := strings.Cut(configuredModel, "/")
	if !found {
		return nil
	}

	seenModels := make(map[string]struct{}, 1+len(geminiMediaAnalysisStaticFallbackModels))
	candidates := make([]string, 0, 1+len(geminiMediaAnalysisStaticFallbackModels))

	appendCandidate := func(candidate string) {
		if candidate == "" || candidate == configuredModel {
			return
		}

		if _, seen := seenModels[candidate]; seen {
			return
		}

		seenModels[candidate] = struct{}{}
		candidates = append(candidates, candidate)
	}

	appendCandidate(provider + "/" + strings.Replace(modelPart, "flash-lite", "flash", 1))

	for _, fallbackModel := range geminiMediaAnalysisStaticFallbackModels {
		appendCandidate(provider + "/" + fallbackModel)
	}

	return candidates
}

func geminiMediaAnalysisPrompt(mediaPart contentPart) (string, error) {
	partType, _ := mediaPart["type"].(string)

	switch partType {
	case contentTypeAudioData:
		return geminiAudioAnalysisPrompt, nil
	case contentTypeVideoData:
		return geminiVideoAnalysisPrompt, nil
	default:
		return "", fmt.Errorf(
			"unsupported media type %q for gemini analysis: %w",
			partType,
			os.ErrInvalid,
		)
	}
}
