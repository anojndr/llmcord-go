package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

var errDownloadedVideoFetch = errors.New("downloaded video fetch failed")

type testDownloadedVideoContent struct {
	url  string
	part contentPart
}

func (content testDownloadedVideoContent) resolvedURL() string {
	return content.url
}

func (content testDownloadedVideoContent) mediaPart() contentPart {
	return content.part
}

func TestFetchDownloadedVideosPreservesOrderDeduplicatesAndWarns(t *testing.T) {
	t.Parallel()

	urls := []string{"first", "duplicate", "failed", "last"}

	videoContents, warnings := fetchDownloadedVideos(
		t.Context(),
		urls,
		func(_ context.Context, rawURL string) (testDownloadedVideoContent, error) {
			switch rawURL {
			case "failed":
				return testDownloadedVideoContent{}, errDownloadedVideoFetch
			case "duplicate":
				return testDownloadedVideoContent{url: "first", part: nil}, nil
			default:
				return testDownloadedVideoContent{url: rawURL, part: nil}, nil
			}
		},
		"fetch test video",
		"some videos failed",
	)

	resolvedURLs := make([]string, 0, len(videoContents))
	for _, content := range videoContents {
		resolvedURLs = append(resolvedURLs, content.resolvedURL())
	}

	if !slices.Equal(resolvedURLs, []string{"first", "last"}) {
		t.Fatalf("resolved URLs = %#v", resolvedURLs)
	}

	if !slices.Equal(warnings, []string{"some videos failed"}) {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestDownloadedVideoAnalysesWithGeminiRunConcurrentlyAndKeepOrder(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.chatCompletions = newConcurrentDownloadedVideoAnalysisChatClient(t)

	videoContents := []testDownloadedVideoContent{
		{
			url: "https://example.com/first",
			part: contentPart{
				"type":               contentTypeVideoData,
				contentFieldBytes:    []byte("first-video"),
				contentFieldMIMEType: testVideoMIMEType,
				contentFieldFilename: "first.mp4",
			},
		},
		{
			url: "https://example.com/second",
			part: contentPart{
				"type":               contentTypeVideoData,
				contentFieldBytes:    []byte("second-video"),
				contentFieldMIMEType: testVideoMIMEType,
				contentFieldFilename: "second.mp4",
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	analyses, err := downloadedVideoAnalysesWithGemini(
		ctx,
		instance,
		testMediaAnalysisConfig(),
		videoContents,
		"tiktok",
	)
	if err != nil {
		t.Fatalf("downloaded video analyses with gemini: %v", err)
	}

	expected := []string{
		"analysis for first.mp4",
		"analysis for second.mp4",
	}

	if len(analyses) != len(expected) {
		t.Fatalf("unexpected analysis count: %#v", analyses)
	}

	for index, expectedAnalysis := range expected {
		if analyses[index] != expectedAnalysis {
			t.Fatalf(
				"unexpected analysis at %d: got %q want %q",
				index,
				analyses[index],
				expectedAnalysis,
			)
		}
	}
}

func newConcurrentDownloadedVideoAnalysisChatClient(
	t *testing.T,
) *stubChatCompletionClient {
	t.Helper()

	var (
		startedCount int
		startedMu    sync.Mutex
		release      = make(chan struct{})
	)

	return newStubChatClient(func(
		ctx context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		contentParts, ok := request.Messages[0].Content.([]contentPart)
		if !ok || len(contentParts) != 2 {
			t.Fatalf("unexpected request content: %#v", request.Messages[0].Content)
		}

		filename, _ := contentParts[1][contentFieldFilename].(string)

		startedMu.Lock()
		startedCount++

		if startedCount == 2 {
			close(release)
		}
		startedMu.Unlock()

		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}

		return handle(streamDelta{
			ReasoningTokens:    0,
			Thinking:           "",
			Content:            "analysis for " + filename,
			FinishReason:       finishReasonStop,
			Usage:              nil,
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
	})
}
