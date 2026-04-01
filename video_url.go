package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

type downloadedURLVideoContent interface {
	resolvedURL() string
	mediaPart() contentPart
}

type downloadedVideoAugmentationRequest[T downloadedURLVideoContent] struct {
	instance           *bot
	loadedConfig       config
	providerSlashModel string
	videoContents      []T
	warnings           []string
	warningText        string
	label              string
}

func downloadedVideoMediaParts[T downloadedURLVideoContent](
	videoContents []T,
) []contentPart {
	mediaParts := make([]contentPart, 0, len(videoContents))
	for _, videoContent := range videoContents {
		mediaParts = append(mediaParts, cloneContentPart(videoContent.mediaPart()))
	}

	return mediaParts
}

func buildDownloadedVideoAugmentation(
	label string,
	warnings []string,
	mediaParts []contentPart,
	analyses []string,
) preparedConversationAugmentation {
	return newPreparedConversationAugmentation(
		warnings,
		nil,
		func(conversation []chatMessage) ([]chatMessage, error) {
			augmentedConversation := conversation

			if len(mediaParts) > 0 {
				updatedConversation, appendErr := appendMediaPartsToConversation(
					augmentedConversation,
					mediaParts,
				)
				if appendErr != nil {
					return nil, fmt.Errorf(
						"append %s video media to conversation: %w",
						label,
						appendErr,
					)
				}

				augmentedConversation = updatedConversation
			}

			if len(analyses) == 0 {
				return augmentedConversation, nil
			}

			updatedConversation, appendErr := appendMediaAnalysesToConversation(
				augmentedConversation,
				analyses,
			)
			if appendErr != nil {
				return nil, fmt.Errorf(
					"append %s media analyses: %w",
					label,
					appendErr,
				)
			}

			return updatedConversation, nil
		},
	)
}

func resolveDownloadedVideoAugmentation[T downloadedURLVideoContent](
	ctx context.Context,
	request downloadedVideoAugmentationRequest[T],
) ([]contentPart, []string, []string, error) {
	replyModelAPIKind, err := configuredModelAPIKind(
		request.loadedConfig,
		request.providerSlashModel,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	searchDeciderNeedsAnalysis, err := request.instance.searchDeciderNeedsURLVideoAnalysis(
		request.loadedConfig,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf(
			"check %s search decider support: %w",
			request.label,
			err,
		)
	}

	mediaParts := downloadedVideoMediaParts(request.videoContents)

	switch {
	case replyModelAPIKind == providerAPIKindGemini && !searchDeciderNeedsAnalysis:
		return mediaParts, nil, request.warnings, nil
	case replyModelAPIKind == providerAPIKindGemini && searchDeciderNeedsAnalysis:
		analyses, analysisErr := downloadedVideoAnalysesWithGemini(
			ctx,
			request.instance,
			request.loadedConfig,
			request.videoContents,
			request.label,
		)
		if analysisErr != nil {
			return nil, nil, nil, analysisErr
		}

		return mediaParts, analyses, request.warnings, nil
	default:
		return resolveDownloadedVideoAnalysesForNonGeminiModel(ctx, request)
	}
}

func resolveDownloadedVideoAnalysesForNonGeminiModel[T downloadedURLVideoContent](
	ctx context.Context,
	request downloadedVideoAugmentationRequest[T],
) ([]contentPart, []string, []string, error) {
	canUseMediaAnalysis, err := canUseGeminiMediaAnalysis(
		request.loadedConfig,
		request.providerSlashModel,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf(
			"check %s media analysis support: %w",
			request.label,
			err,
		)
	}

	if !canUseMediaAnalysis {
		return nil, nil, mergeURLVideoWarnings(request.warnings, request.warningText), nil
	}

	analyses, err := downloadedVideoAnalysesWithGemini(
		ctx,
		request.instance,
		request.loadedConfig,
		request.videoContents,
		request.label,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	return nil, analyses, request.warnings, nil
}

func prepareDownloadedVideoAugmentation[T downloadedURLVideoContent](
	ctx context.Context,
	request downloadedVideoAugmentationRequest[T],
) (preparedConversationAugmentation, error) {
	mediaParts, analyses, resolvedWarnings, err := resolveDownloadedVideoAugmentation(
		ctx,
		request,
	)
	if err != nil {
		return emptyPreparedConversationAugmentation(), err
	}

	return buildDownloadedVideoAugmentation(
		request.label,
		resolvedWarnings,
		mediaParts,
		analyses,
	), nil
}

func downloadedVideoAnalysesWithGemini[T downloadedURLVideoContent](
	ctx context.Context,
	instance *bot,
	loadedConfig config,
	videoContents []T,
	label string,
) ([]string, error) {
	geminiModel, err := configuredGeminiMediaModel(loadedConfig)
	if err != nil {
		return nil, err
	}

	analyses := make([]string, 0, len(videoContents))
	results := runTasksConcurrently(
		ctx,
		geminiMediaAnalysisConcurrency,
		len(videoContents),
		func(taskContext context.Context, index int) (string, error) {
			return instance.analyzeMediaWithGemini(
				taskContext,
				loadedConfig,
				geminiModel,
				cloneContentPart(videoContents[index].mediaPart()),
			)
		},
	)

	for index, result := range results {
		if result.err != nil {
			return nil, fmt.Errorf(
				"analyze %s video %d with gemini: %w",
				label,
				index+1,
				result.err,
			)
		}

		analyses = append(analyses, result.value)
	}

	return analyses, nil
}

func (instance *bot) searchDeciderNeedsURLVideoAnalysis(
	loadedConfig config,
) (bool, error) {
	searchDeciderModel := instance.currentSearchDeciderModelForConfig(loadedConfig)

	apiKind, err := configuredModelAPIKind(loadedConfig, searchDeciderModel)
	if err != nil {
		return false, err
	}

	return apiKind != providerAPIKindGemini, nil
}

func mergeURLVideoWarnings(warnings []string, warningText string) []string {
	warningSet := make(map[string]struct{}, len(warnings)+1)
	for _, warning := range warnings {
		appendUniqueWarning(warningSet, warning)
	}

	appendUniqueWarning(warningSet, warningText)

	return sortedWarnings(warningSet)
}

func fetchDownloadedVideos[T downloadedURLVideoContent](
	ctx context.Context,
	urls []string,
	fetcher urlContentFetcher[T],
	logMessage string,
	warningText string,
) ([]T, []string) {
	results := make([]T, len(urls))
	successful := make([]bool, len(urls))

	var (
		failed    bool
		failedMu  sync.Mutex
		waitGroup sync.WaitGroup
	)

	for index, rawURL := range urls {
		waitGroup.Add(1)

		go func(index int, rawURL string) {
			defer waitGroup.Done()

			videoContent, err := fetcher(ctx, rawURL)
			if err != nil {
				slog.Warn(logMessage, "url", rawURL, "error", err)
				failedMu.Lock()
				failed = true
				failedMu.Unlock()

				return
			}

			results[index] = videoContent
			successful[index] = true
		}(index, rawURL)
	}

	waitGroup.Wait()

	videoContents := make([]T, 0, len(results))
	seenResolvedURLs := make(map[string]struct{}, len(results))

	for index, result := range results {
		if !successful[index] {
			continue
		}

		resolvedURL := strings.TrimSpace(result.resolvedURL())
		if resolvedURL == "" {
			resolvedURL = urls[index]
		}

		if _, seen := seenResolvedURLs[resolvedURL]; seen {
			continue
		}

		seenResolvedURLs[resolvedURL] = struct{}{}

		videoContents = append(videoContents, result)
	}

	warnings := make([]string, 0, 1)
	if failed {
		warnings = append(warnings, warningText)
	}

	return videoContents, warnings
}
