package main

import (
	"context"
	"iter"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"
)

type stringCapture struct {
	mutex  sync.Mutex
	values []string
}

func (capture *stringCapture) append(value string) {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()

	capture.values = append(capture.values, value)
}

func (capture *stringCapture) snapshot() []string {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()

	return append([]string(nil), capture.values...)
}

func TestChatCompletionRouterStreamsGeminiImmediately(t *testing.T) {
	t.Parallel()

	releaseStream, release := newReleaseSignal()
	firstDeltaSeen := make(chan struct{})
	chunkCapture := new(stringCapture)

	router := newBlockingGeminiStreamRouter(releaseStream)

	defer release()

	streamErr := startStreamingContentCapture(
		context.Background(),
		router,
		newSimpleGeminiStreamRequest(),
		chunkCapture,
		firstDeltaSeen,
	)

	waitForFirstStreamedDelta(t, "gemini", firstDeltaSeen)

	if !slices.Equal(chunkCapture.snapshot(), []string{"Hel"}) {
		t.Fatalf("unexpected gemini chunks before release: %#v", chunkCapture.snapshot())
	}

	release()

	err := <-streamErr
	if err != nil {
		t.Fatalf("stream gemini completion: %v", err)
	}

	if strings.Join(chunkCapture.snapshot(), "") != testStreamedHelloText {
		t.Fatalf("unexpected streamed gemini content: %q", strings.Join(chunkCapture.snapshot(), ""))
	}
}

func newReleaseSignal() (chan struct{}, func()) {
	releaseStream := make(chan struct{})

	var releaseOnce sync.Once

	release := func() {
		releaseOnce.Do(func() {
			close(releaseStream)
		})
	}

	return releaseStream, release
}

func newBlockingGeminiStreamRouter(releaseStream <-chan struct{}) chatCompletionRouter {
	return chatCompletionRouter{
		openAI: newOpenAIClient(nil),
		gemini: geminiClient{
			httpClient: new(http.Client),
			newClient: func(
				_ context.Context,
				_ *genai.ClientConfig,
			) (geminiAPIClient, error) {
				return stubGeminiAPIClient{
					generateContentStream: func(
						_ context.Context,
						_ string,
						_ []*genai.Content,
						_ *genai.GenerateContentConfig,
					) iter.Seq2[*genai.GenerateContentResponse, error] {
						return func(yield func(*genai.GenerateContentResponse, error) bool) {
							if !yield(newGeminiGenerateContentResponse("Hel", genai.FinishReasonUnspecified), nil) {
								return
							}

							<-releaseStream

							_ = yield(newGeminiGenerateContentResponse("lo", genai.FinishReasonStop), nil)
						}
					},
					uploadFile: nil,
					getFile:    nil,
				}, nil
			},
		},
	}
}

func startStreamingContentCapture(
	ctx context.Context,
	router chatCompletionRouter,
	request chatCompletionRequest,
	chunkCapture *stringCapture,
	firstDeltaSeen chan struct{},
) <-chan error {
	streamErr := make(chan error, 1)

	var firstDeltaOnce sync.Once

	go func() {
		streamErr <- router.streamChatCompletion(ctx, request, func(delta streamDelta) error {
			if delta.Content == "" {
				return nil
			}

			chunkCapture.append(delta.Content)
			firstDeltaOnce.Do(func() {
				close(firstDeltaSeen)
			})

			return nil
		})
	}()

	return streamErr
}

func waitForFirstStreamedDelta(t *testing.T, provider string, firstDeltaSeen <-chan struct{}) {
	t.Helper()

	select {
	case <-firstDeltaSeen:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected %s delta before stream completion", provider)
	}
}
