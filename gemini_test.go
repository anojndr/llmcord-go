package main

import (
	"context"
	"io"
	"iter"
	"net/http"
	"testing"
	"time"

	"google.golang.org/genai"
)

const (
	testStreamedHelloText = "Hello"
	testHeaderPresent     = "present"
	testGeminiHelloPrompt = "hello"
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

	if config == nil || config.HTTPOptions == nil {
		t.Fatalf("unexpected gemini config: %#v", config)
	}

	thinkingConfig, ok := config.HTTPOptions.ExtraBody["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected thinking config: %#v", config.HTTPOptions.ExtraBody)
	}

	if thinkingConfig["thinkingLevel"] != genai.ThinkingLevelMinimal {
		t.Fatalf("unexpected thinking level: %#v", thinkingConfig["thinkingLevel"])
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
		func(delta streamDelta) error {
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
			uploadedFile.URI = "https://example.com/files/document"
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
		Provider:        provider,
		Model:           "gemini-3-flash-preview",
		ConfiguredModel: "",
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

	if contents[0].Parts[2].FileData.FileURI != "https://example.com/files/document" {
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
			APIKind: providerAPIKindGemini,
			BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
			APIKey:  "",
			APIKeys: nil,
			ExtraHeaders: map[string]any{
				"X-Test": testHeaderPresent,
			},
			ExtraQuery: nil,
			ExtraBody: map[string]any{
				"temperature": 0.2,
			},
		},
		Model:           "gemini-3-flash-preview",
		ConfiguredModel: "",
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
			APIKind:      providerAPIKindGemini,
			BaseURL:      "",
			APIKey:       "gemini-key",
			APIKeys:      nil,
			ExtraHeaders: nil,
			ExtraQuery:   nil,
			ExtraBody:    nil,
		},
		Model:           "gemini-3-flash-preview",
		ConfiguredModel: "",
		Messages:        []chatMessage{{Role: messageRoleUser, Content: "hello"}},
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

		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			if !yield(newGeminiGenerateContentResponse("Hel", ""), nil) {
				return
			}

			_ = yield(
				newGeminiGenerateContentResponse("lo", genai.FinishReasonStop),
				nil,
			)
		}
	}
}

func newGeminiGenerateContentResponse(
	text string,
	finishReason genai.FinishReason,
) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				Content:      genai.NewContentFromText(text, genai.RoleModel),
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
