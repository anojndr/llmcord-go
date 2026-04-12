package main

import (
	"context"
	"io"
	"iter"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

const (
	testStreamedHelloText = "Hello"
	testHeaderPresent     = "present"
	testGeminiHelloPrompt = "hello"
	testGeminiDocumentURI = "https://example.com/files/document"
)

type stubGeminiAPIClient struct {
	generateContentStream func(
		context.Context,
		string,
		[]*genai.Content,
		*genai.GenerateContentConfig,
	) iter.Seq2[*genai.GenerateContentResponse, error]
	uploadFile func(context.Context, io.Reader, *genai.UploadFileConfig) (*genai.File, error)
	getFile    func(context.Context, string, *genai.GetFileConfig) (*genai.File, error)
}

func (client stubGeminiAPIClient) GenerateContentStream(
	ctx context.Context,
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
) iter.Seq2[*genai.GenerateContentResponse, error] {
	return client.generateContentStream(ctx, model, contents, config)
}

func (client stubGeminiAPIClient) UploadFile(
	ctx context.Context,
	reader io.Reader,
	config *genai.UploadFileConfig,
) (*genai.File, error) {
	if client.uploadFile == nil {
		panic("unexpected UploadFile call")
	}

	return client.uploadFile(ctx, reader, config)
}

func (client stubGeminiAPIClient) GetFile(
	ctx context.Context,
	name string,
	config *genai.GetFileConfig,
) (*genai.File, error) {
	if client.getFile == nil {
		panic("unexpected GetFile call")
	}

	return client.getFile(ctx, name, config)
}

func TestBuildChatCompletionRequestUsesGeminiAPIKindForLegacyBaseURL(t *testing.T) {
	t.Parallel()

	loadedConfig := new(config)
	legacyProvider := new(providerConfig)
	legacyProvider.BaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	loadedConfig.Providers = map[string]providerConfig{"google": *legacyProvider}
	loadedConfig.Models = map[string]map[string]any{
		"google/gemini-3-flash-preview": nil,
	}

	request, err := buildChatCompletionRequest(
		*loadedConfig,
		"google/gemini-3-flash-preview",
		[]chatMessage{{Role: messageRoleUser, Content: "<@123>: hi"}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	if request.Provider.APIKind != providerAPIKindGemini {
		t.Fatalf("unexpected provider API kind: %q", request.Provider.APIKind)
	}
}

func TestBuildGeminiGenerateContentRequestConvertsMessagesAndHTTPOptions(t *testing.T) {
	t.Parallel()

	contents, config, err := buildGeminiGenerateContentRequest(
		context.Background(),
		newGeminiBuildTestRequest(),
		nil,
	)
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	assertGeminiConvertedContents(t, contents)
	assertGeminiGenerateContentConfig(t, config)
}

func TestBuildGeminiGenerateContentRequestAddsPlaceholderForImageOnlyUserMessage(t *testing.T) {
	t.Parallel()

	request := newSimpleGeminiStreamRequest()
	request.Messages = []chatMessage{{
		Role: messageRoleUser,
		Content: []contentPart{
			{"type": contentTypeText, "text": ""},
			{
				"type":      contentTypeImageURL,
				"image_url": map[string]string{"url": "data:image/png;base64,aGVsbG8="},
			},
		},
	}}

	contents, _, err := buildGeminiGenerateContentRequest(context.Background(), request, nil)
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	if len(contents) != 1 || len(contents[0].Parts) != 2 {
		t.Fatalf("unexpected gemini contents: %#v", contents)
	}

	if contents[0].Parts[0].Text != fileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected placeholder text part: %#v", contents[0].Parts[0])
	}

	if contents[0].Parts[1].InlineData == nil {
		t.Fatal("expected inline image data")
	}
}

func TestBuildGeminiGenerateContentRequestAddsPlaceholderForDocumentOnlyUserMessage(t *testing.T) {
	t.Parallel()

	state := new(geminiUploadState)
	files := newGeminiMediaUploadStub(t, state)
	request := newSimpleGeminiStreamRequest()
	request.Messages = []chatMessage{{
		Role: messageRoleUser,
		Content: []contentPart{
			{"type": contentTypeText, "text": ""},
			{
				"type":               contentTypeDocument,
				contentFieldBytes:    []byte("document-bytes"),
				contentFieldMIMEType: mimeTypePDF,
				contentFieldFilename: testPDFFilename,
			},
		},
	}}

	contents, _, err := buildGeminiGenerateContentRequest(context.Background(), request, files)
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	if len(contents) != 1 || len(contents[0].Parts) != 2 {
		t.Fatalf("unexpected gemini contents: %#v", contents)
	}

	if contents[0].Parts[0].Text != fileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected placeholder text part: %#v", contents[0].Parts[0])
	}

	if contents[0].Parts[1].FileData == nil || contents[0].Parts[1].FileData.FileURI != testGeminiDocumentURI {
		t.Fatalf("expected uploaded document file part: %#v", contents[0].Parts[1])
	}
}

func TestBuildGeminiGenerateContentRequestIncludesThinkingAliasLevel(t *testing.T) {
	t.Parallel()

	provider := new(providerConfig)
	provider.Type = string(providerAPIKindGemini)

	var loadedConfig config

	loadedConfig.Providers = map[string]providerConfig{
		"google": *provider,
	}
	loadedConfig.Models = map[string]map[string]any{
		"google/gemini-3.1-flash-lite-preview-minimal": nil,
	}

	request, err := buildChatCompletionRequest(
		loadedConfig,
		"google/gemini-3.1-flash-lite-preview-minimal",
		[]chatMessage{{Role: messageRoleUser, Content: testGeminiHelloPrompt}},
	)
	if err != nil {
		t.Fatalf("build chat completion request: %v", err)
	}

	contents, config, err := buildGeminiGenerateContentRequest(
		context.Background(),
		request,
		nil,
	)
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	if len(contents) != 1 ||
		len(contents[0].Parts) != 1 ||
		contents[0].Parts[0].Text != testGeminiHelloPrompt {
		t.Fatalf("unexpected gemini contents: %#v", contents)
	}

	if config == nil || config.ThinkingConfig == nil {
		t.Fatalf("unexpected gemini config: %#v", config)
	}

	if config.ThinkingConfig.ThinkingLevel != genai.ThinkingLevelMinimal {
		t.Fatalf("unexpected thinking level: %#v", config.ThinkingConfig.ThinkingLevel)
	}

	if config.HTTPOptions != nil {
		t.Fatalf("unexpected gemini HTTP options: %#v", config.HTTPOptions)
	}
}

func TestBuildGeminiGenerateContentRequestPromotesThinkingConfigFromExtraBody(t *testing.T) {
	t.Parallel()

	request := newSimpleGeminiStreamRequest()
	request.Provider.ExtraBody = map[string]any{
		"temperature": 0.2,
		"thinkingConfig": map[string]any{
			"includeThoughts": true,
			"thinkingBudget":  int32(64),
		},
	}

	_, config, err := buildGeminiGenerateContentRequest(
		context.Background(),
		request,
		nil,
	)
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	if config == nil || config.ThinkingConfig == nil {
		t.Fatalf("unexpected gemini config: %#v", config)
	}

	if !config.ThinkingConfig.IncludeThoughts {
		t.Fatalf("unexpected thinking config: %#v", config.ThinkingConfig)
	}

	if config.ThinkingConfig.ThinkingBudget == nil || *config.ThinkingConfig.ThinkingBudget != 64 {
		t.Fatalf("unexpected thinking budget: %#v", config.ThinkingConfig.ThinkingBudget)
	}

	if config.HTTPOptions == nil {
		t.Fatalf("expected gemini HTTP options: %#v", config)
	}

	if got, ok := config.HTTPOptions.ExtraBody["temperature"].(float64); !ok || got != 0.2 {
		t.Fatalf("unexpected gemini extra body: %#v", config.HTTPOptions.ExtraBody)
	}

	if _, ok := config.HTTPOptions.ExtraBody["thinkingConfig"]; ok {
		t.Fatalf("unexpected thinkingConfig in extra body: %#v", config.HTTPOptions.ExtraBody)
	}
}

func TestBuildGeminiGenerateContentRequestDefaultsThoughtSummaries(t *testing.T) {
	t.Parallel()

	_, config, err := buildGeminiGenerateContentRequest(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		nil,
	)
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	if config == nil || config.ThinkingConfig == nil {
		t.Fatalf("unexpected gemini config: %#v", config)
	}

	if !config.ThinkingConfig.IncludeThoughts {
		t.Fatalf("expected includeThoughts to default on: %#v", config.ThinkingConfig)
	}
}

func TestBuildGeminiGenerateContentRequestRejectsInvalidThinkingConfig(t *testing.T) {
	t.Parallel()

	request := newSimpleGeminiStreamRequest()
	request.Provider.ExtraBody = map[string]any{
		"thinkingConfig": "invalid",
	}

	_, _, err := buildGeminiGenerateContentRequest(
		context.Background(),
		request,
		nil,
	)
	if err == nil {
		t.Fatal("expected invalid thinkingConfig to fail")
	}
}

func TestGeminiClientStreamChatCompletionEmitsTextAndFinishReason(t *testing.T) {
	t.Parallel()

	var capturedConfig *genai.ClientConfig

	client := geminiClient{
		httpClient: new(http.Client),
		newClient: func(
			_ context.Context,
			config *genai.ClientConfig,
		) (geminiAPIClient, error) {
			capturedConfig = config

			var stubClient stubGeminiAPIClient

			stubClient.generateContentStream = streamGeminiTestChunks(t)

			return stubClient, nil
		},
	}

	var usage *tokenUsage

	joinedText := ""
	finishReason := ""

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(delta streamDelta) error {
			joinedText += delta.Content

			if delta.Usage != nil {
				usage = cloneTokenUsage(delta.Usage)
			}

			if delta.FinishReason != "" {
				finishReason = delta.FinishReason
			}

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if joinedText != testStreamedHelloText {
		t.Fatalf("unexpected streamed text: %q", joinedText)
	}

	if finishReason != finishReasonStop {
		t.Fatalf("unexpected finish reason: %q", finishReason)
	}

	if usage == nil || usage.Input != 15 || usage.Output != 23 {
		t.Fatalf("unexpected usage: %#v", usage)
	}

	assertGeminiClientConfig(t, capturedConfig)
}

func TestGeminiClientStreamChatCompletionEmitsThoughtsSeparately(t *testing.T) {
	t.Parallel()

	client := geminiClient{
		httpClient: new(http.Client),
		newClient: func(
			_ context.Context,
			_ *genai.ClientConfig,
		) (geminiAPIClient, error) {
			var stubClient stubGeminiAPIClient

			stubClient.generateContentStream = func(
				_ context.Context,
				_ string,
				_ []*genai.Content,
				_ *genai.GenerateContentConfig,
			) iter.Seq2[*genai.GenerateContentResponse, error] {
				return func(yield func(*genai.GenerateContentResponse, error) bool) {
					if !yield(
						newGeminiGenerateContentResponseWithParts(
							[]*genai.Part{
								{Text: "Plan.", Thought: true},
								{Text: "Answer."},
							},
							genai.FinishReasonStop,
						),
						nil,
					) {
						return
					}
				}
			}

			return stubClient, nil
		},
	}

	var thoughtText strings.Builder

	var answerText strings.Builder

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(delta streamDelta) error {
			thoughtText.WriteString(delta.Thinking)
			answerText.WriteString(delta.Content)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if thoughtText.String() != "Plan." {
		t.Fatalf("unexpected thought text: %q", thoughtText.String())
	}

	if answerText.String() != "Answer." {
		t.Fatalf("unexpected answer text: %q", answerText.String())
	}
}

func TestGeminiClientStreamChatCompletionReturnsPromptFeedbackErrors(t *testing.T) {
	t.Parallel()

	client := geminiClient{
		httpClient: new(http.Client),
		newClient: func(
			_ context.Context,
			_ *genai.ClientConfig,
		) (geminiAPIClient, error) {
			var stubClient stubGeminiAPIClient

			stubClient.generateContentStream = func(
				_ context.Context,
				_ string,
				_ []*genai.Content,
				_ *genai.GenerateContentConfig,
			) iter.Seq2[*genai.GenerateContentResponse, error] {
				return func(yield func(*genai.GenerateContentResponse, error) bool) {
					response := newGeminiGenerateContentResponse("", genai.FinishReasonUnspecified)
					response.Candidates = nil
					response.PromptFeedback = &genai.GenerateContentResponsePromptFeedback{
						BlockReason:        genai.BlockedReasonSafety,
						BlockReasonMessage: "",
						SafetyRatings:      nil,
					}

					_ = yield(response, nil)
				}
			}

			return stubClient, nil
		},
	}

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(streamDelta) error {
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected prompt feedback error")
	}

	if !containsFold(err.Error(), "blocked the prompt") || !containsFold(err.Error(), "safety") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestGeminiClientStreamChatCompletionReturnsCandidateFinishReasonErrors(t *testing.T) {
	t.Parallel()

	client := geminiClient{
		httpClient: new(http.Client),
		newClient: func(
			_ context.Context,
			_ *genai.ClientConfig,
		) (geminiAPIClient, error) {
			var stubClient stubGeminiAPIClient

			stubClient.generateContentStream = func(
				_ context.Context,
				_ string,
				_ []*genai.Content,
				_ *genai.GenerateContentConfig,
			) iter.Seq2[*genai.GenerateContentResponse, error] {
				return func(yield func(*genai.GenerateContentResponse, error) bool) {
					response := newGeminiGenerateContentResponse("partial", genai.FinishReasonSafety)
					response.Candidates[0].FinishMessage = "response blocked"

					_ = yield(response, nil)
				}
			}

			return stubClient, nil
		},
	}

	var joinedText strings.Builder

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(delta streamDelta) error {
			joinedText.WriteString(delta.Content)

			return nil
		},
	)
	if err == nil {
		t.Fatal("expected finish reason error")
	}

	if joinedText.String() != "partial" {
		t.Fatalf("expected partial content before error, got %q", joinedText.String())
	}

	if !containsFold(err.Error(), "response blocked") || !containsFold(err.Error(), "safety") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestGeminiClientStreamChatCompletionReturnsUnknownFinishReasonErrors(t *testing.T) {
	t.Parallel()

	client := geminiClient{
		httpClient: new(http.Client),
		newClient: func(
			_ context.Context,
			_ *genai.ClientConfig,
		) (geminiAPIClient, error) {
			var stubClient stubGeminiAPIClient

			stubClient.generateContentStream = func(
				_ context.Context,
				_ string,
				_ []*genai.Content,
				_ *genai.GenerateContentConfig,
			) iter.Seq2[*genai.GenerateContentResponse, error] {
				return func(yield func(*genai.GenerateContentResponse, error) bool) {
					response := newGeminiGenerateContentResponse(
						"partial",
						genai.FinishReason("TOO_MANY_TOOL_CALLS"),
					)

					_ = yield(response, nil)
				}
			}

			return stubClient, nil
		},
	}

	var joinedText strings.Builder

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(delta streamDelta) error {
			joinedText.WriteString(delta.Content)

			return nil
		},
	)
	if err == nil {
		t.Fatal("expected finish reason error")
	}

	if joinedText.String() != "partial" {
		t.Fatalf("expected partial content before error, got %q", joinedText.String())
	}

	if !containsFold(err.Error(), "too_many_tool_calls") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestGeminiClientStreamChatCompletionReturnsErrorWithoutFinishReason(t *testing.T) {
	t.Parallel()

	client := geminiClient{
		httpClient: new(http.Client),
		newClient: func(
			_ context.Context,
			_ *genai.ClientConfig,
		) (geminiAPIClient, error) {
			var stubClient stubGeminiAPIClient

			stubClient.generateContentStream = func(
				_ context.Context,
				_ string,
				_ []*genai.Content,
				_ *genai.GenerateContentConfig,
			) iter.Seq2[*genai.GenerateContentResponse, error] {
				return func(yield func(*genai.GenerateContentResponse, error) bool) {
					_ = yield(newGeminiGenerateContentResponse("Hello", genai.FinishReasonUnspecified), nil)
				}
			}

			return stubClient, nil
		},
	}

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(streamDelta) error {
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected missing finish reason error")
	}

	if !containsFold(err.Error(), "without finish reason") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestGeminiWaitForFileActiveRejectsUnknownStates(t *testing.T) {
	t.Parallel()

	file := new(genai.File)
	file.Name = "files/test"
	file.State = genai.FileState("ARCHIVED")

	_, err := geminiWaitForFileActive(context.Background(), nil, file)
	if err == nil {
		t.Fatal("expected unknown file state to fail")
	}

	if !containsFold(err.Error(), "unsupported processing state") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestGeminiWaitForFileActiveRejectsMissingRefreshState(t *testing.T) {
	t.Parallel()

	file := new(genai.File)
	file.Name = "files/test"
	file.State = genai.FileStateProcessing

	files := new(stubGeminiAPIClient)
	files.getFile = func(context.Context, string, *genai.GetFileConfig) (*genai.File, error) {
		return nilGeminiFile(), nil
	}

	_, err := geminiWaitForFileActive(context.Background(), files, file)
	if err == nil {
		t.Fatal("expected missing refreshed file to fail")
	}

	if !containsFold(err.Error(), "missing file state") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func nilGeminiFile() *genai.File {
	return nil
}

func TestBuildGeminiGenerateContentRequestUploadsBinaryFiles(t *testing.T) {
	t.Parallel()

	state := new(geminiUploadState)
	files := newGeminiMediaUploadStub(t, state)
	request := newGeminiMediaUploadRequest()

	contents, config, err := buildGeminiGenerateContentRequest(
		context.Background(),
		request,
		files,
	)
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	if config == nil {
		t.Fatal("expected generate content config")
	}

	assertGeminiMediaUploadCalls(t, state.calls)

	if len(state.refreshedFiles) != 1 || state.refreshedFiles[0] != "files/video" {
		t.Fatalf("unexpected refreshed files: %#v", state.refreshedFiles)
	}

	assertGeminiUploadedMediaParts(t, contents)
}

func TestBuildGeminiGenerateContentRequestUploadsOnlyPDFDocuments(t *testing.T) {
	t.Parallel()

	state := new(geminiUploadState)
	files := newGeminiMediaUploadStub(t, state)
	request := newSimpleGeminiStreamRequest()
	request.Messages = []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": "<@123>: summarize these files"},
				{
					"type":               contentTypeDocument,
					contentFieldBytes:    []byte("pdf-bytes"),
					contentFieldMIMEType: mimeTypePDF,
					contentFieldFilename: testPDFFilename,
				},
				{
					"type":               contentTypeDocument,
					contentFieldBytes:    []byte("docx-bytes"),
					contentFieldMIMEType: mimeTypeDOCX,
					contentFieldFilename: testDOCXFilename,
				},
				{
					"type":               contentTypeDocument,
					contentFieldBytes:    []byte("pptx-bytes"),
					contentFieldMIMEType: mimeTypePPTX,
					contentFieldFilename: testPPTXFilename,
				},
			},
		},
	}

	contents, _, err := buildGeminiGenerateContentRequest(
		context.Background(),
		request,
		files,
	)
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	if len(state.calls) != 1 {
		t.Fatalf("unexpected upload count: %d", len(state.calls))
	}

	if state.calls[0].mimeType != mimeTypePDF {
		t.Fatalf("expected first upload to be PDF: %#v", state.calls[0])
	}

	if len(contents) != 1 || len(contents[0].Parts) != 2 {
		t.Fatalf("unexpected gemini content shape: %#v", contents)
	}

	part := contents[0].Parts[1]
	if part.FileData == nil || part.FileData.FileURI != testGeminiDocumentURI {
		t.Fatalf("unexpected uploaded PDF file part: %#v", part)
	}
}

type geminiUploadCall struct {
	mimeType    string
	displayName string
	body        []byte
}

type geminiUploadState struct {
	calls          []geminiUploadCall
	refreshedFiles []string
}

func newGeminiMediaUploadStub(t *testing.T, state *geminiUploadState) stubGeminiAPIClient {
	t.Helper()

	var files stubGeminiAPIClient

	files.uploadFile = func(
		_ context.Context,
		reader io.Reader,
		config *genai.UploadFileConfig,
	) (*genai.File, error) {
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read upload body: %v", err)
		}

		state.calls = append(state.calls, geminiUploadCall{
			mimeType:    config.MIMEType,
			displayName: config.DisplayName,
			body:        body,
		})

		uploadedFile := new(genai.File)
		uploadedFile.Name = "files/audio"
		uploadedFile.URI = "https://example.com/files/audio"
		uploadedFile.MIMEType = config.MIMEType
		uploadedFile.State = genai.FileStateActive

		if config.MIMEType == mimeTypePDF {
			uploadedFile.Name = "files/document"
			uploadedFile.URI = testGeminiDocumentURI
		}

		if config.MIMEType == mimeTypeDOCX || config.MIMEType == mimeTypePPTX {
			uploadedFile.Name = "files/document"
			uploadedFile.URI = testGeminiDocumentURI
		}

		if config.MIMEType == testVideoMIMEType {
			uploadedFile.Name = "files/video"
			uploadedFile.URI = "https://example.com/files/video"
			uploadedFile.State = genai.FileStateProcessing
		}

		return uploadedFile, nil
	}
	files.getFile = func(
		_ context.Context,
		name string,
		_ *genai.GetFileConfig,
	) (*genai.File, error) {
		state.refreshedFiles = append(state.refreshedFiles, name)

		file := new(genai.File)
		file.Name = name
		file.URI = "https://example.com/" + name
		file.MIMEType = "video/mp4"
		file.State = genai.FileStateActive

		return file, nil
	}

	return files
}

func newGeminiMediaUploadRequest() chatCompletionRequest {
	var provider providerRequestConfig

	provider.APIKind = providerAPIKindGemini

	return chatCompletionRequest{
		Provider:                    provider,
		Model:                       "gemini-3-flash-preview",
		ConfiguredModel:             "",
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		SessionID:                   "",
		PreviousResponseID:          "",
		Messages: []chatMessage{
			{
				Role: messageRoleUser,
				Content: []contentPart{
					{"type": contentTypeText, "text": "<@123>: summarize these"},
					{
						"type":               contentTypeAudioData,
						contentFieldBytes:    []byte("audio-bytes"),
						contentFieldMIMEType: "audio/mpeg",
						contentFieldFilename: "clip.mp3",
					},
					{
						"type":               contentTypeDocument,
						contentFieldBytes:    []byte("document-bytes"),
						contentFieldMIMEType: mimeTypePDF,
						contentFieldFilename: testPDFFilename,
					},
					{
						"type":               contentTypeVideoData,
						contentFieldBytes:    []byte("video-bytes"),
						contentFieldMIMEType: "video/mp4",
						contentFieldFilename: "clip.mp4",
					},
				},
			},
		},
	}
}

func assertGeminiMediaUploadCalls(t *testing.T, uploadCalls []geminiUploadCall) {
	t.Helper()

	if len(uploadCalls) != 3 {
		t.Fatalf("unexpected upload count: %d", len(uploadCalls))
	}

	if uploadCalls[0].mimeType != "audio/mpeg" ||
		uploadCalls[0].displayName != "clip.mp3" ||
		string(uploadCalls[0].body) != "audio-bytes" {
		t.Fatalf("unexpected audio upload call: %#v", uploadCalls[0])
	}

	if uploadCalls[1].mimeType != mimeTypePDF ||
		uploadCalls[1].displayName != testPDFFilename ||
		string(uploadCalls[1].body) != "document-bytes" {
		t.Fatalf("unexpected document upload call: %#v", uploadCalls[1])
	}

	if uploadCalls[2].mimeType != "video/mp4" ||
		uploadCalls[2].displayName != "clip.mp4" ||
		string(uploadCalls[2].body) != "video-bytes" {
		t.Fatalf("unexpected video upload call: %#v", uploadCalls[2])
	}
}

func assertGeminiUploadedMediaParts(t *testing.T, contents []*genai.Content) {
	t.Helper()

	if len(contents) != 1 || len(contents[0].Parts) != 4 {
		t.Fatalf("unexpected gemini contents: %#v", contents)
	}

	if contents[0].Parts[1].FileData == nil {
		t.Fatal("expected uploaded audio file part")
	}

	if contents[0].Parts[1].FileData.FileURI != "https://example.com/files/audio" {
		t.Fatalf("unexpected audio URI: %#v", contents[0].Parts[1].FileData)
	}

	if contents[0].Parts[2].FileData == nil {
		t.Fatal("expected uploaded document file part")
	}

	if contents[0].Parts[2].FileData.FileURI != testGeminiDocumentURI {
		t.Fatalf("unexpected document URI: %#v", contents[0].Parts[2].FileData)
	}

	if contents[0].Parts[3].FileData == nil {
		t.Fatal("expected uploaded video file part")
	}

	if contents[0].Parts[3].FileData.FileURI != "https://example.com/files/video" {
		t.Fatalf("unexpected video URI: %#v", contents[0].Parts[3].FileData)
	}
}

func newGeminiBuildTestRequest() chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindGemini,
			BaseURL:         "https://generativelanguage.googleapis.com/v1beta/openai",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			ExtraHeaders: map[string]any{
				"X-Test": testHeaderPresent,
			},
			ExtraQuery: nil,
			ExtraBody: map[string]any{
				"temperature": 0.2,
			},
		},
		Model:                       "gemini-3-flash-preview",
		ConfiguredModel:             "",
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		SessionID:                   "",
		PreviousResponseID:          "",
		Messages: []chatMessage{
			{Role: "system", Content: "Be concise."},
			{
				Role: messageRoleUser,
				Content: []contentPart{
					{"type": contentTypeText, "text": "<@123>: what is this?"},
					{
						"type":      contentTypeImageURL,
						"image_url": map[string]string{"url": "data:image/png;base64,aGVsbG8="},
					},
				},
			},
			{Role: messageRoleAssistant, Content: "It is a test."},
		},
	}
}

func newSimpleGeminiStreamRequest() chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind:         providerAPIKindGemini,
			BaseURL:         "",
			APIKey:          "gemini-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:                       "gemini-3-flash-preview",
		ConfiguredModel:             "",
		ContextWindow:               0,
		AutoCompactThresholdPercent: 0,
		SessionID:                   "",
		PreviousResponseID:          "",
		Messages:                    []chatMessage{{Role: messageRoleUser, Content: "hello"}},
	}
}

func assertGeminiConvertedContents(t *testing.T, contents []*genai.Content) {
	t.Helper()

	if len(contents) != 2 {
		t.Fatalf("unexpected content count: %d", len(contents))
	}

	if contents[0].Role != string(genai.RoleUser) {
		t.Fatalf("unexpected first role: %q", contents[0].Role)
	}

	if len(contents[0].Parts) != 2 {
		t.Fatalf("unexpected first part count: %d", len(contents[0].Parts))
	}

	if contents[0].Parts[0].Text != "<@123>: what is this?" {
		t.Fatalf("unexpected first text part: %q", contents[0].Parts[0].Text)
	}

	if contents[0].Parts[1].InlineData == nil {
		t.Fatal("expected inline image data")
	}

	if contents[0].Parts[1].InlineData.MIMEType != mimeTypePNG {
		t.Fatalf("unexpected image MIME type: %q", contents[0].Parts[1].InlineData.MIMEType)
	}

	if string(contents[0].Parts[1].InlineData.Data) != testGeminiHelloPrompt {
		t.Fatalf("unexpected image bytes: %q", string(contents[0].Parts[1].InlineData.Data))
	}

	if contents[1].Role != string(genai.RoleModel) {
		t.Fatalf("unexpected second role: %q", contents[1].Role)
	}

	if contents[1].Parts[0].Text != "It is a test." {
		t.Fatalf("unexpected assistant content: %q", contents[1].Parts[0].Text)
	}
}

func assertGeminiGenerateContentConfig(
	t *testing.T,
	config *genai.GenerateContentConfig,
) {
	t.Helper()

	if config == nil {
		t.Fatal("expected Gemini generate content config")
	}

	if config.SystemInstruction == nil {
		t.Fatal("expected system instruction")
	}

	if config.SystemInstruction.Parts[0].Text != "Be concise." {
		t.Fatalf("unexpected system instruction: %q", config.SystemInstruction.Parts[0].Text)
	}

	if config.HTTPOptions == nil {
		t.Fatal("expected HTTP options")
	}

	if got, ok := config.HTTPOptions.ExtraBody["temperature"].(float64); !ok || got != 0.2 {
		t.Fatalf("unexpected gemini extra body: %#v", config.HTTPOptions.ExtraBody)
	}

	if config.ThinkingConfig == nil || !config.ThinkingConfig.IncludeThoughts {
		t.Fatalf("expected gemini thought summaries to be enabled: %#v", config.ThinkingConfig)
	}
}

func streamGeminiTestChunks(
	t *testing.T,
) func(
	context.Context,
	string,
	[]*genai.Content,
	*genai.GenerateContentConfig,
) iter.Seq2[*genai.GenerateContentResponse, error] {
	t.Helper()

	return func(
		_ context.Context,
		model string,
		contents []*genai.Content,
		config *genai.GenerateContentConfig,
	) iter.Seq2[*genai.GenerateContentResponse, error] {
		if model != "gemini-3-flash-preview" {
			t.Fatalf("unexpected model: %q", model)
		}

		if len(contents) != 1 || contents[0].Parts[0].Text != "hello" {
			t.Fatalf("unexpected gemini contents: %#v", contents)
		}

		if config == nil {
			t.Fatal("expected gemini config")
		}

		if config.SystemInstruction != nil || config.HTTPOptions != nil {
			t.Fatalf("unexpected gemini config contents: %#v", config)
		}

		if config.ThinkingConfig == nil || !config.ThinkingConfig.IncludeThoughts {
			t.Fatalf("expected gemini thought summaries to be enabled: %#v", config)
		}

		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			if !yield(newGeminiGenerateContentResponse("Hel", ""), nil) {
				return
			}

			finalResponse := newGeminiGenerateContentResponse("lo", genai.FinishReasonStop)
			finalResponse.UsageMetadata = newGeminiUsageMetadata(11, 4, 19, 4)

			_ = yield(finalResponse, nil)
		}
	}
}

func newGeminiGenerateContentResponse(
	text string,
	finishReason genai.FinishReason,
) *genai.GenerateContentResponse {
	return newGeminiGenerateContentResponseWithParts(
		[]*genai.Part{{Text: text}},
		finishReason,
	)
}

func newGeminiGenerateContentResponseWithParts(
	parts []*genai.Part,
	finishReason genai.FinishReason,
) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				Content: &genai.Content{
					Role:  string(genai.RoleModel),
					Parts: parts,
				},
				FinishReason: finishReason,
			},
		},
		SDKHTTPResponse: nil,
		CreateTime:      time.Time{},
		ModelVersion:    "",
		PromptFeedback:  nil,
		ResponseID:      "",
		UsageMetadata:   nil,
	}
}

func newGeminiUsageMetadata(
	promptTokens, toolUsePromptTokens, candidateTokens, thoughtTokens int32,
) *genai.GenerateContentResponseUsageMetadata {
	return &genai.GenerateContentResponseUsageMetadata{
		CacheTokensDetails:         nil,
		CachedContentTokenCount:    0,
		CandidatesTokenCount:       candidateTokens,
		CandidatesTokensDetails:    nil,
		PromptTokenCount:           promptTokens,
		PromptTokensDetails:        nil,
		ThoughtsTokenCount:         thoughtTokens,
		ToolUsePromptTokenCount:    toolUsePromptTokens,
		ToolUsePromptTokensDetails: nil,
		TotalTokenCount:            promptTokens + toolUsePromptTokens + candidateTokens + thoughtTokens,
		TrafficType:                "",
	}
}

func assertGeminiClientConfig(t *testing.T, capturedConfig *genai.ClientConfig) {
	t.Helper()

	if capturedConfig == nil {
		t.Fatal("expected gemini client config")
	}

	if capturedConfig.Backend != genai.BackendGeminiAPI {
		t.Fatalf("unexpected gemini backend: %v", capturedConfig.Backend)
	}

	if capturedConfig.APIKey != "gemini-key" {
		t.Fatalf("unexpected gemini API key: %q", capturedConfig.APIKey)
	}

	if capturedConfig.HTTPOptions.BaseURL != "" {
		t.Fatalf("unexpected gemini base URL: %q", capturedConfig.HTTPOptions.BaseURL)
	}

	if capturedConfig.HTTPOptions.APIVersion != "" {
		t.Fatalf("unexpected gemini API version: %q", capturedConfig.HTTPOptions.APIVersion)
	}
}

func TestBuildGeminiClientConfigUsesProviderHTTPOptions(t *testing.T) {
	t.Parallel()

	clientConfig, err := buildGeminiClientConfig(newGeminiBuildTestRequest().Provider, new(http.Client))
	if err != nil {
		t.Fatalf("build gemini client config: %v", err)
	}

	if clientConfig.HTTPOptions.BaseURL != "https://generativelanguage.googleapis.com" {
		t.Fatalf("unexpected gemini base URL: %q", clientConfig.HTTPOptions.BaseURL)
	}

	if clientConfig.HTTPOptions.APIVersion != "v1beta" {
		t.Fatalf("unexpected gemini API version: %q", clientConfig.HTTPOptions.APIVersion)
	}

	if clientConfig.HTTPOptions.Headers.Get("X-Test") != testHeaderPresent {
		t.Fatalf("unexpected gemini extra header: %q", clientConfig.HTTPOptions.Headers.Get("X-Test"))
	}
}
