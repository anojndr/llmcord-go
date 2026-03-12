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

func appendDownloadedVideosToConversation[T downloadedURLVideoContent](
	conversation []chatMessage,
	videoContents []T,
	warnings []string,
) ([]chatMessage, []string, error) {
	mediaParts := make([]contentPart, 0, len(videoContents))
	for _, videoContent := range videoContents {
		mediaParts = append(mediaParts, cloneContentPart(videoContent.mediaPart()))
	}

	augmentedConversation, err := appendMediaPartsToConversation(
		conversation,
		mediaParts,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("append downloaded video media to conversation: %w", err)
	}

	return augmentedConversation, warnings, nil
}

func augmentConversationWithDownloadedVideos[T downloadedURLVideoContent](
	ctx context.Context,
	instance *bot,
	loadedConfig config,
	providerSlashModel string,
	conversation []chatMessage,
	videoContents []T,
	warnings []string,
	warningText string,
	label string,
) ([]chatMessage, []string, error) {
	replyModelAPIKind, err := configuredModelAPIKind(loadedConfig, providerSlashModel)
	if err != nil {
		return nil, nil, err
	}

	searchDeciderNeedsAnalysis, err := instance.searchDeciderNeedsURLVideoAnalysis(
		loadedConfig,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("check %s search decider support: %w", label, err)
	}

	if replyModelAPIKind == providerAPIKindGemini {
		augmentedConversation, augmentedWarnings, appendErr := appendDownloadedVideosToConversation(
			conversation,
			videoContents,
			warnings,
		)
		if appendErr != nil {
			return nil, nil, appendErr
		}

		if !searchDeciderNeedsAnalysis {
			return augmentedConversation, augmentedWarnings, nil
		}

		return appendDownloadedVideoAnalysesWithGemini(
			ctx,
			instance,
			loadedConfig,
			augmentedConversation,
			videoContents,
			augmentedWarnings,
			label,
		)
	}

	return preprocessDownloadedVideosWithGemini(
		ctx,
		instance,
		loadedConfig,
		providerSlashModel,
		conversation,
		videoContents,
		warnings,
		warningText,
		label,
	)
}

func preprocessDownloadedVideosWithGemini[T downloadedURLVideoContent](
	ctx context.Context,
	instance *bot,
	loadedConfig config,
	providerSlashModel string,
	conversation []chatMessage,
	videoContents []T,
	warnings []string,
	warningText string,
	label string,
) ([]chatMessage, []string, error) {
	canUseMediaAnalysis, err := canUseGeminiMediaAnalysis(
		loadedConfig,
		providerSlashModel,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("check %s media analysis support: %w", label, err)
	}

	if !canUseMediaAnalysis {
		return conversation, mergeURLVideoWarnings(warnings, warningText), nil
	}

	return appendDownloadedVideoAnalysesWithGemini(
		ctx,
		instance,
		loadedConfig,
		conversation,
		videoContents,
		warnings,
		label,
	)
}

func appendDownloadedVideoAnalysesWithGemini[T downloadedURLVideoContent](
	ctx context.Context,
	instance *bot,
	loadedConfig config,
	conversation []chatMessage,
	videoContents []T,
	warnings []string,
	label string,
) ([]chatMessage, []string, error) {
	geminiModel, err := configuredGeminiMediaModel(loadedConfig)
	if err != nil {
		return nil, nil, err
	}

	analyses := make([]string, 0, len(videoContents))
	for index, videoContent := range videoContents {
		analysis, analysisErr := instance.analyzeMediaWithGemini(
			ctx,
			loadedConfig,
			geminiModel,
			cloneContentPart(videoContent.mediaPart()),
		)
		if analysisErr != nil {
			return nil, nil, fmt.Errorf(
				"analyze %s video %d with gemini: %w",
				label,
				index+1,
				analysisErr,
			)
		}

		analyses = append(analyses, analysis)
	}

	augmentedConversation, err := appendMediaAnalysesToConversation(
		conversation,
		analyses,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("append %s media analyses: %w", label, err)
	}

	return augmentedConversation, warnings, nil
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
