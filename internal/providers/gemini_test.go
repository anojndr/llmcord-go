package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	searchtypes "llmcord-go/internal/searchtypes"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"
)

const (
	testStreamedHelloText = "Hello"
	testHeaderPresent     = "present"
	testGeminiHelloPrompt = "hello"
	testGeminiDocumentURI = "https://example.com/files/document"
	testGeminiImagePrompt = "What is in this image?"
	testGeminiMediaURI    = "https://example.com/files/audio"
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

func TestProviderUsesGeminiAPIKindForLegacyBaseURL(t *testing.T) {
	t.Parallel()

	provider := ProviderRequestConfig{
		APIKind:         ProviderAPIKindGemini,
		BaseURL:         "https://generativelanguage.googleapis.com/v1beta/openai",
		APIKey:          "",
		APIKeys:         nil,
		UseResponsesAPI: false,
		EnableGrounding: false,
		ExtraHeaders:    nil,
		ExtraQuery:      nil,
		ExtraBody:       nil,
	}

	if provider.APIKind != ProviderAPIKindGemini {
		t.Fatalf("unexpected provider API kind: %q", provider.APIKind)
	}
}

func TestBuildGeminiGenerateContentRequestConvertsMessagesAndHTTPOptions(t *testing.T) {
	t.Parallel()

	request := newSimpleGeminiStreamRequest()
	request.Provider.APIKind = ProviderAPIKindGemini
	request.ConfiguredModel = "gemini/gemini-3.5-flash-lite-minimal"
	request.Model = "gemini-3.5-flash-lite-minimal"
	request.Provider.ExtraBody = map[string]any{"temperature": 0.2}
	request.Messages = []ChatMessage{
		{Role: searchtypes.MessageRoleSystem, Content: "Be concise."},
		{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{searchtypes.MessageTypeKey: searchtypes.ContentTypeText, searchtypes.MessageTextKey: "<@123>: what is this?"},
				{
					searchtypes.MessageTypeKey: searchtypes.ContentTypeImageURL,
					"image_url":                map[string]string{"url": "data:image/png;base64,aGVsbG8="},
				},
			},
		},
		{Role: searchtypes.MessageRoleAssistant, Content: "It is a test."},
	}

	contents, config, err := BuildGeminiGenerateContentRequest(
		context.Background(),
		request,
		nil,
	)
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	assertGeminiConvertedContents(t, contents)
	assertGeminiGenerateContentConfig(t, config)
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

	_, config, err := BuildGeminiGenerateContentRequest(
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

	_, config, err := BuildGeminiGenerateContentRequest(
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

	_, _, err := BuildGeminiGenerateContentRequest(
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

	joinedText := ""
	finishReason := ""

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(delta StreamDelta) error {
			joinedText += delta.Content

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
		func(delta StreamDelta) error {
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

func TestGeminiClientStreamChatCompletionEmitsGroundingMetadata(t *testing.T) {
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
					resp := newGeminiGenerateContentResponse("Answer text", genai.FinishReasonStop)
					resp.Candidates[0].GroundingMetadata = &genai.GroundingMetadata{
						ImageSearchQueries: nil,
						GroundingChunks: []*genai.GroundingChunk{
							{
								Image:            nil,
								Maps:             nil,
								RetrievedContext: nil,
								Web: &genai.GroundingChunkWeb{
									Domain: "",
									Title:  "Tokyo Weather",
									URI:    "https://weather.com/tokyo",
								},
							},
						},
						GroundingSupports:            nil,
						RetrievalMetadata:            nil,
						SearchEntryPoint:             nil,
						WebSearchQueries:             []string{"current weather Tokyo"},
						GoogleMapsWidgetContextToken: "",
						RetrievalQueries:             nil,
						SourceFlaggingUris:           nil,
					}

					_ = yield(resp, nil)
				}
			}

			return stubClient, nil
		},
	}

	var metadata *searchtypes.SearchMetadata

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(delta StreamDelta) error {
			if delta.SearchMetadata != nil {
				metadata = searchtypes.MergeSearchMetadata(metadata, delta.SearchMetadata)
			}

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if metadata == nil {
		t.Fatal("expected non-nil SearchMetadata")
	}

	if len(metadata.Queries) != 1 || metadata.Queries[0] != "current weather Tokyo" {
		t.Fatalf("unexpected search queries: %#v", metadata.Queries)
	}

	if len(metadata.Results) != 1 {
		t.Fatalf("unexpected search results count: %d", len(metadata.Results))
	}

	sources := searchtypes.ExtractSearchSources(metadata.Results[0].Text)
	if len(sources) != 1 || sources[0].Title != "Tokyo Weather" || sources[0].URL != "https://weather.com/tokyo" {
		t.Fatalf("unexpected search sources: %#v", sources)
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
		func(StreamDelta) error {
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
		func(delta StreamDelta) error {
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
		func(delta StreamDelta) error {
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
		func(StreamDelta) error {
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

	contents, config, err := BuildGeminiGenerateContentRequest(
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
	request.Provider.ExtraBody = map[string]any{"temperature": 0.2}
	request.Messages = []ChatMessage{
		{Role: searchtypes.MessageRoleSystem, Content: "Be concise."},
		{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": "<@123>: summarize these files"},
				{
					"type":                           searchtypes.ContentTypeDocument,
					searchtypes.ContentFieldBytes:    []byte("pdf-bytes"),
					searchtypes.ContentFieldMIMEType: searchtypes.MimeTypePDF,
					searchtypes.ContentFieldFilename: testPDFFilename,
				},
				{
					"type":                           searchtypes.ContentTypeDocument,
					searchtypes.ContentFieldBytes:    []byte("docx-bytes"),
					searchtypes.ContentFieldMIMEType: searchtypes.MimeTypeDOCX,
					searchtypes.ContentFieldFilename: testDOCXFilename,
				},
				{
					"type":                           searchtypes.ContentTypeDocument,
					searchtypes.ContentFieldBytes:    []byte("pptx-bytes"),
					searchtypes.ContentFieldMIMEType: searchtypes.MimeTypePPTX,
					searchtypes.ContentFieldFilename: testPPTXFilename,
				},
			},
		},
	}

	contents, _, err := BuildGeminiGenerateContentRequest(
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

	if state.calls[0].mimeType != searchtypes.MimeTypePDF {
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

func TestBuildGeminiGenerateContentRequestUploadsGenericFiles(t *testing.T) {
	t.Parallel()

	state := new(geminiUploadState)
	files := newGeminiMediaUploadStub(t, state)
	request := newSimpleGeminiStreamRequest()
	request.Provider.ExtraBody = map[string]any{"temperature": 0.2}
	request.Messages = []ChatMessage{
		{Role: searchtypes.MessageRoleSystem, Content: "Be concise."},
		{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": "<@123>: summarize this archive"},
				{
					"type":                           searchtypes.ContentTypeFileData,
					searchtypes.ContentFieldBytes:    []byte("zip-bytes"),
					searchtypes.ContentFieldMIMEType: searchtypes.MimeTypeZIP,
					searchtypes.ContentFieldFilename: testZIPFilename,
				},
			},
		},
	}

	contents, _, err := BuildGeminiGenerateContentRequest(
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

	if state.calls[0].mimeType != searchtypes.MimeTypeZIP || state.calls[0].displayName != testZIPFilename {
		t.Fatalf("unexpected generic file upload call: %#v", state.calls[0])
	}

	if len(contents) != 1 || len(contents[0].Parts) != 2 {
		t.Fatalf("unexpected gemini content shape: %#v", contents)
	}

	if contents[0].Parts[1].FileData == nil {
		t.Fatalf("expected uploaded generic file part: %#v", contents[0].Parts[1])
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
		uploadedFile.URI = testGeminiMediaURI
		uploadedFile.MIMEType = config.MIMEType
		uploadedFile.State = genai.FileStateActive

		if config.MIMEType == searchtypes.MimeTypePDF {
			uploadedFile.Name = "files/document"
			uploadedFile.URI = testGeminiDocumentURI
		}

		if config.MIMEType == searchtypes.MimeTypeDOCX || config.MIMEType == searchtypes.MimeTypePPTX {
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

func newGeminiMediaUploadRequest() ChatCompletionRequest {
	var provider ProviderRequestConfig

	provider.APIKind = ProviderAPIKindGemini

	return ChatCompletionRequest{
		Provider:           provider,
		Model:              "gemini-3-flash-preview",
		ConfiguredModel:    "",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages: []ChatMessage{
			{
				Role: searchtypes.MessageRoleUser,
				Content: []ContentPart{
					{"type": searchtypes.ContentTypeText, "text": "<@123>: summarize these"},
					{
						"type":                           searchtypes.ContentTypeAudioData,
						searchtypes.ContentFieldBytes:    []byte("audio-bytes"),
						searchtypes.ContentFieldMIMEType: "audio/mpeg",
						searchtypes.ContentFieldFilename: "clip.mp3",
					},
					{
						"type":                           searchtypes.ContentTypeDocument,
						searchtypes.ContentFieldBytes:    []byte("document-bytes"),
						searchtypes.ContentFieldMIMEType: searchtypes.MimeTypePDF,
						searchtypes.ContentFieldFilename: testPDFFilename,
					},
					{
						"type":                           searchtypes.ContentTypeVideoData,
						searchtypes.ContentFieldBytes:    []byte("video-bytes"),
						searchtypes.ContentFieldMIMEType: "video/mp4",
						searchtypes.ContentFieldFilename: "clip.mp4",
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

	if uploadCalls[1].mimeType != searchtypes.MimeTypePDF ||
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

	if contents[0].Parts[1].FileData.FileURI != testGeminiMediaURI {
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

func newGeminiBuildTestRequest() ChatCompletionRequest {
	return ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindGemini,
			BaseURL:         "https://generativelanguage.googleapis.com/v1beta/openai",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders: map[string]any{
				"X-Test": testHeaderPresent,
			},
			ExtraQuery: nil,
			ExtraBody: map[string]any{
				"temperature": 0.2,
			},
		},
		Model:              "gemini-3-flash-preview",
		ConfiguredModel:    "",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages: []ChatMessage{
			{Role: "system", Content: "Be concise."},
			{
				Role: searchtypes.MessageRoleUser,
				Content: []ContentPart{
					{"type": searchtypes.ContentTypeText, "text": "<@123>: what is this?"},
					{
						"type":      searchtypes.ContentTypeImageURL,
						"image_url": map[string]string{"url": "data:image/png;base64,aGVsbG8="},
					},
				},
			},
			{Role: searchtypes.MessageRoleAssistant, Content: "It is a test."},
		},
	}
}

func newSimpleGeminiStreamRequest() ChatCompletionRequest {
	return ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindGemini,
			BaseURL:         "",
			APIKey:          "gemini-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "gemini-3-flash-preview",
		ConfiguredModel:    "",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages:           []ChatMessage{{Role: searchtypes.MessageRoleUser, Content: "hello"}},
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
		t.Fatalf("unexpected prompt text part: %q", contents[0].Parts[0].Text)
	}

	if contents[0].Parts[1].InlineData == nil {
		t.Fatal("expected inline image data second")
	}

	if contents[0].Parts[1].InlineData.MIMEType != searchtypes.MimeTypePNG {
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

	expectedInstruction := "Be concise.\n\n" + GeminiNoToolsInstruction
	if config.SystemInstruction.Parts[0].Text != expectedInstruction {
		t.Fatalf("unexpected system instruction: %q", config.SystemInstruction.Parts[0].Text)
	}

	if config.HTTPOptions == nil {
		t.Fatal("expected HTTP options")
	}

	if got, ok := config.HTTPOptions.ExtraBody["temperature"].(float64); !ok || got != 0.2 {
		t.Fatalf("unexpected gemini extra body: %#v", config.HTTPOptions.ExtraBody)
	}

	if got, ok := config.HTTPOptions.ExtraBody["service_tier"].(string); !ok || got != "priority" {
		t.Fatalf("expected priority service tier in extra body: %#v", config.HTTPOptions.ExtraBody)
	}

	if config.ThinkingConfig == nil || !config.ThinkingConfig.IncludeThoughts {
		t.Fatalf("expected gemini thought summaries to be enabled by default: %#v", config.ThinkingConfig)
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

		if config.SystemInstruction == nil || len(config.SystemInstruction.Parts) != 1 {
			t.Fatalf("expected gemini system instruction: %#v", config)
		}

		if config.SystemInstruction.Parts[0].Text != GeminiNoToolsInstruction {
			t.Fatalf("unexpected gemini system instruction: %#v", config.SystemInstruction)
		}

		if config.HTTPOptions == nil || config.HTTPOptions.ExtraBody["service_tier"] != "priority" {
			t.Fatalf("expected priority service tier in HTTP options: %#v", config.HTTPOptions)
		}

		if config.ThinkingConfig == nil || !config.ThinkingConfig.IncludeThoughts {
			t.Fatalf("expected gemini thought summaries to be enabled by default: %#v", config)
		}

		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			if !yield(newGeminiGenerateContentResponse("Hel", ""), nil) {
				return
			}

			finalResponse := newGeminiGenerateContentResponse("lo", genai.FinishReasonStop)

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
		ModelStatus:     nil,
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

func TestBuildGeminiGenerateContentRequestDefaultsServiceTierToPriority(t *testing.T) {
	t.Parallel()

	request := newSimpleGeminiStreamRequest()

	_, config, err := BuildGeminiGenerateContentRequest(context.Background(), request, nil)
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	if config == nil || config.HTTPOptions == nil {
		t.Fatalf("expected HTTPOptions in config: %#v", config)
	}

	got, ok := config.HTTPOptions.ExtraBody["service_tier"].(string)
	if !ok || got != "priority" {
		t.Fatalf("expected default service_tier to be priority, got %#v", config.HTTPOptions.ExtraBody)
	}
}

func TestBuildGeminiGenerateContentRequestPreservesCustomServiceTier(t *testing.T) {
	t.Parallel()

	request := newSimpleGeminiStreamRequest()
	request.Provider.ExtraBody = map[string]any{
		"service_tier": "standard",
	}

	_, config, err := BuildGeminiGenerateContentRequest(context.Background(), request, nil)
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	if config == nil || config.HTTPOptions == nil {
		t.Fatalf("expected HTTPOptions in config: %#v", config)
	}

	got, ok := config.HTTPOptions.ExtraBody["service_tier"].(string)
	if !ok || got != "standard" {
		t.Fatalf("expected custom service_tier to be preserved as standard, got %#v", config.HTTPOptions.ExtraBody)
	}
}

func TestGeminiClientStreamChatCompletion_ReturnsErrEmptyModelResponseWhenNoContent(t *testing.T) {
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
					resp := newGeminiGenerateContentResponse("", genai.FinishReasonStop)
					yield(resp, nil)
				}
			}

			return stubClient, nil
		},
	}

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(_ StreamDelta) error {
			return nil
		},
	)
	if !errors.Is(err, ErrEmptyModelResponse) {
		t.Fatalf("expected ErrEmptyModelResponse, got: %v", err)
	}
}

func TestGeminiStreamDeltaSeparatesThinkingFromAnswer(t *testing.T) {
	t.Parallel()

	thoughtPart := genai.NewPartFromText(
		"Query: does premid require discord client\n\n" +
			"Let's do a quick search or evaluate based on knowledge of PreMiD.",
	)
	thoughtPart.Thought = true

	answerPart := genai.NewPartFromText(
		"Yes, PreMiD requires the official Discord desktop client to be running.",
	)
	answerPart.Thought = false

	response := newGeminiGenerateContentResponseWithParts(
		[]*genai.Part{thoughtPart, answerPart},
		genai.FinishReasonStop,
	)

	delta, err := geminiStreamDelta(response)
	if err != nil {
		t.Fatalf("geminiStreamDelta failed: %v", err)
	}

	expectedThinking := "Query: does premid require discord client\n\n" +
		"Let's do a quick search or evaluate based on knowledge of PreMiD."
	expectedContent := "Yes, PreMiD requires the official Discord desktop client to be running."

	if delta.Thinking != expectedThinking {
		t.Errorf("unexpected Thinking:\ngot:  %q\nwant: %q", delta.Thinking, expectedThinking)
	}

	if delta.Content != expectedContent {
		t.Errorf("unexpected Content:\ngot:  %q\nwant: %q", delta.Content, expectedContent)
	}
}

func TestGeminiThoughtSplitterSeparatesInlineThoughtMarkers(t *testing.T) {
	t.Parallel()

	for _, testCase := range geminiThoughtSplitterCases() {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			splitter := newGeminiThoughtSplitter()

			var thinkingText strings.Builder

			var answerText strings.Builder

			for _, chunk := range testCase.chunks {
				splitThinking, splitAnswer := splitter.split(chunk)
				thinkingText.WriteString(splitThinking)
				answerText.WriteString(splitAnswer)
			}

			splitThinking, splitAnswer := splitter.finalize()
			thinkingText.WriteString(splitThinking)
			answerText.WriteString(splitAnswer)

			if thinkingText.String() != testCase.expectedThinking {
				t.Errorf(
					"unexpected thinking:\ngot:  %q\nwant: %q",
					thinkingText.String(),
					testCase.expectedThinking,
				)
			}

			if answerText.String() != testCase.expectedAnswer {
				t.Errorf(
					"unexpected answer:\ngot:  %q\nwant: %q",
					answerText.String(),
					testCase.expectedAnswer,
				)
			}
		})
	}
}

func geminiThoughtSplitterCases() []struct {
	name             string
	chunks           []string
	expectedThinking string
	expectedAnswer   string
} {
	return []struct {
		name             string
		chunks           []string
		expectedThinking string
		expectedAnswer   string
	}{
		{
			name:             "plain content passes through",
			chunks:           []string{"Hello", " world"},
			expectedThinking: "",
			expectedAnswer:   "Hello world",
		},
		{
			name:             "single thought block",
			chunks:           []string{"<<<Plan first.>>>Final answer."},
			expectedThinking: "Plan first.",
			expectedAnswer:   "Final answer.",
		},
		{
			name:             "thought block split across chunks",
			chunks:           []string{"<<<Plan ", "first.>>>Final ", "answer."},
			expectedThinking: "Plan first.",
			expectedAnswer:   "Final answer.",
		},
		{
			name:             "open marker split across chunks",
			chunks:           []string{"Intro <", "<", "<Plan.>>>Final."},
			expectedThinking: "Plan.",
			expectedAnswer:   "Intro Final.",
		},
		{
			name:             "close marker split across chunks",
			chunks:           []string{"<<<Plan", ">>", ">Final."},
			expectedThinking: "Plan",
			expectedAnswer:   "Final.",
		},
		{
			name:             "unclosed thought block",
			chunks:           []string{"<<<Plan first."},
			expectedThinking: "Plan first.",
			expectedAnswer:   "",
		},
		{
			name:             "multiple thought blocks",
			chunks:           []string{"<<<A>>>X<<<B>>>Y"},
			expectedThinking: "AB",
			expectedAnswer:   "XY",
		},
		{
			name:             "stray closing marker stays literal",
			chunks:           []string{"A>>>B"},
			expectedThinking: "",
			expectedAnswer:   "A>>>B",
		},
		{
			name:             "partial open marker never completes",
			chunks:           []string{"A<<", "B"},
			expectedThinking: "",
			expectedAnswer:   "A<<B",
		},
	}
}

func TestGeminiClientStreamChatCompletionSeparatesInlineThoughtMarkers(t *testing.T) {
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
					if !yield(newGeminiGenerateContentResponse("<<<Plan ", ""), nil) {
						return
					}

					if !yield(newGeminiGenerateContentResponse("first.>>>Final ", ""), nil) {
						return
					}

					if !yield(newGeminiGenerateContentResponse("answer.", genai.FinishReasonStop), nil) {
						return
					}
				}
			}

			return stubClient, nil
		},
	}

	var thinkingText strings.Builder

	var answerText strings.Builder

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(delta StreamDelta) error {
			thinkingText.WriteString(delta.Thinking)
			answerText.WriteString(delta.Content)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if thinkingText.String() != "Plan first." {
		t.Fatalf("unexpected thinking text: %q", thinkingText.String())
	}

	if answerText.String() != "Final answer." {
		t.Fatalf("unexpected answer text: %q", answerText.String())
	}
}

func TestGeminiClientStreamChatCompletionKeepsStrayClosingMarkerInAnswer(t *testing.T) {
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
					_ = yield(newGeminiGenerateContentResponse("A>>>B", genai.FinishReasonStop), nil)
				}
			}

			return stubClient, nil
		},
	}

	var answerText strings.Builder

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(delta StreamDelta) error {
			answerText.WriteString(delta.Content)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if answerText.String() != "A>>>B" {
		t.Fatalf("unexpected answer text: %q", answerText.String())
	}
}

func TestGeminiClientStreamChatCompletionPreservesMetadataOnMarkerOnlyChunk(t *testing.T) {
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
					markerOnly := newGeminiGenerateContentResponse("<<<", "")
					markerOnly.Candidates[0].GroundingMetadata = &genai.GroundingMetadata{
						ImageSearchQueries: nil,
						GroundingChunks: []*genai.GroundingChunk{
							{
								Image:            nil,
								Maps:             nil,
								RetrievedContext: nil,
								Web: &genai.GroundingChunkWeb{
									Domain: "",
									Title:  "Tokyo Weather",
									URI:    "https://weather.com/tokyo",
								},
							},
						},
						GroundingSupports:            nil,
						RetrievalMetadata:            nil,
						SearchEntryPoint:             nil,
						WebSearchQueries:             []string{"current weather Tokyo"},
						GoogleMapsWidgetContextToken: "",
						RetrievalQueries:             nil,
						SourceFlaggingUris:           nil,
					}

					if !yield(markerOnly, nil) {
						return
					}

					_ = yield(newGeminiGenerateContentResponse("Final.", genai.FinishReasonStop), nil)
				}
			}

			return stubClient, nil
		},
	}

	var metadata *searchtypes.SearchMetadata

	err := client.streamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(delta StreamDelta) error {
			if delta.SearchMetadata != nil {
				metadata = searchtypes.MergeSearchMetadata(metadata, delta.SearchMetadata)
			}

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if metadata == nil {
		t.Fatal("expected search metadata from marker-only chunk")
	}

	if len(metadata.Queries) != 1 || metadata.Queries[0] != "current weather Tokyo" {
		t.Fatalf("unexpected search queries: %#v", metadata.Queries)
	}
}

// testGeminiTransientStreamError builds a Gemini back-end error shaped like
// the one observed in production logs: "Stream interrupted: NetworkError
// type=upstream_error: invalid argument", which surfaces through the genai
// iterator as a stream error and is wrapped by "stream gemini content:".
func testGeminiTransientStreamError() error {
	return fmt.Errorf(
		"Stream interrupted: NetworkError type=upstream_error: invalid argument: %w",
		io.ErrUnexpectedEOF,
	)
}

// newStubGeminiStreamingRouter builds a ChatCompletionRouter that dispatches
// gemini-kind requests to a geminiClient whose GenerateContentStream stub is
// `streamFn`, and routes openai requests nowhere (an error). The stub is
// re-created per attempt so each retry gets its own iterator.
func newStubGeminiStreamingRouter(
	streamFn func() iter.Seq2[*genai.GenerateContentResponse, error],
) ChatCompletionRouter {
	return ChatCompletionRouter{
		openAI: newOpenAIClient(nil),
		keys:   NewAPIKeyRotator(),
		gemini: geminiClient{
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
					return streamFn()
				}

				return stubClient, nil
			},
		},
	}
}

func TestGeminiClientRetriesStreamInterruptedNetworkError(t *testing.T) {
	t.Parallel()

	calls := new(atomic.Int32)

	router := newStubGeminiStreamingRouter(func() iter.Seq2[*genai.GenerateContentResponse, error] {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			if calls.Add(1) == 1 {
				// First attempt yields the upstream NetworkError, then the
				// stream ends. The retry proceeds to a stop.
				_ = yield(nil, testGeminiTransientStreamError())

				return
			}

			_ = yield(newGeminiGenerateContentResponse("retried", genai.FinishReasonStop), nil)
		}
	})

	var joinedText strings.Builder

	err := router.StreamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(delta StreamDelta) error {
			joinedText.WriteString(delta.Content)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream gemini completion: %v", err)
	}

	if calls.Load() != 2 {
		t.Fatalf("gemini calls = %d, want 2 (transient error then retry)", calls.Load())
	}

	if joinedText.String() != "retried" {
		t.Fatalf("unexpected gemini content: %q", joinedText.String())
	}
}

func TestGeminiClientExhaustsStreamInterruptedRetries(t *testing.T) {
	t.Parallel()

	calls := new(atomic.Int32)

	router := newStubGeminiStreamingRouter(func() iter.Seq2[*genai.GenerateContentResponse, error] {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			calls.Add(1)

			_ = yield(nil, testGeminiTransientStreamError())
		}
	})

	err := router.StreamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(StreamDelta) error { return nil },
	)
	if err == nil {
		t.Fatal("expected exhausted gemini transient retries to fail")
	}

	if calls.Load() != 2 {
		t.Fatalf("gemini calls = %d, want 2 (retry budget exhausted)", calls.Load())
	}

	if !containsFold(err.Error(), "Stream interrupted") {
		t.Fatalf("unexpected gemini error: %v", err)
	}
}

func TestGeminiClientDoesNotRetryAfterContentProduced(t *testing.T) {
	t.Parallel()

	calls := new(atomic.Int32)

	router := newStubGeminiStreamingRouter(func() iter.Seq2[*genai.GenerateContentResponse, error] {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			calls.Add(1)

			// The transient-yielding attempt streams content first, so it
			// must never be retried.
			_ = yield(newGeminiGenerateContentResponse("partial", genai.FinishReasonUnspecified), nil)
			_ = yield(nil, testGeminiTransientStreamError())
		}
	})

	err := router.StreamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(StreamDelta) error { return nil },
	)
	if err == nil {
		t.Fatal("expected gemini stream to fail after partial content")
	}

	if calls.Load() != 1 {
		t.Fatalf("gemini calls = %d, want 1 (no retry after content produced)", calls.Load())
	}

	if !containsFold(err.Error(), "Stream interrupted") {
		t.Fatalf("unexpected gemini error: %v", err)
	}
}

func TestGeminiClientRetriesEmptyModelResponse(t *testing.T) {
	t.Parallel()

	calls := new(atomic.Int32)

	router := newStubGeminiStreamingRouter(func() iter.Seq2[*genai.GenerateContentResponse, error] {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			calls.Add(1)

			if calls.Load() == 1 {
				// No content and a finish reason: empty model response.
				_ = yield(newGeminiGenerateContentResponse("", genai.FinishReasonStop), nil)

				return
			}

			_ = yield(newGeminiGenerateContentResponse("retried", genai.FinishReasonStop), nil)
		}
	})

	var joinedText strings.Builder

	err := router.StreamChatCompletion(
		context.Background(),
		newSimpleGeminiStreamRequest(),
		func(delta StreamDelta) error {
			joinedText.WriteString(delta.Content)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream gemini completion: %v", err)
	}

	if calls.Load() != 2 {
		t.Fatalf("gemini calls = %d, want 2 (empty response then retry)", calls.Load())
	}

	if joinedText.String() != "retried" {
		t.Fatalf("unexpected gemini content: %q", joinedText.String())
	}
}

func containsFold(text, fragment string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(fragment))
}

const (
	testPDFFilename   = "attachment.pdf"
	testDOCXFilename  = "attachment.docx"
	testPPTXFilename  = "attachment.pptx"
	testZIPFilename   = "attachment.zip"
	testVideoMIMEType = "video/mp4"
)
