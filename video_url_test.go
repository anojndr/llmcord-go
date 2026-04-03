package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

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
			Thinking:           "",
			Content:            "analysis for " + filename,
			FinishReason:       finishReasonStop,
			Usage:              nil,
			ProviderResponseID: "",
			SearchMetadata:     nil,
			ResponseImages:     nil,
		})
	})
}
