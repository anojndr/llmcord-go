package main

import (
	"context"
	"iter"
	"net/http"
	"testing"
	"time"

	"google.golang.org/genai"
)

type stubGeminiModelsClient struct {
	generateContentStream func(
		context.Context,
		string,
		[]*genai.Content,
		*genai.GenerateContentConfig,
	) iter.Seq2[*genai.GenerateContentResponse, error]
}

func (client stubGeminiModelsClient) GenerateContentStream(
	ctx context.Context,
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
) iter.Seq2[*genai.GenerateContentResponse, error] {
	return client.generateContentStream(ctx, model, contents, config)
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

	contents, config, err := buildGeminiGenerateContentRequest(newGeminiBuildTestRequest())
	if err != nil {
		t.Fatalf("build gemini generate content request: %v", err)
	}

	assertGeminiConvertedContents(t, contents)
	assertGeminiGenerateContentConfig(t, config)
}

func TestGeminiClientStreamChatCompletionEmitsTextAndFinishReason(t *testing.T) {
	t.Parallel()

	var capturedConfig *genai.ClientConfig

	client := geminiClient{
		httpClient: new(http.Client),
		newClient: func(
			_ context.Context,
			config *genai.ClientConfig,
		) (geminiModelsClient, error) {
			capturedConfig = config

			return stubGeminiModelsClient{
				generateContentStream: streamGeminiTestChunks(t),
			}, nil
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

	if joinedText != "Hello" {
		t.Fatalf("unexpected streamed text: %q", joinedText)
	}

	if finishReason != finishReasonStop {
		t.Fatalf("unexpected finish reason: %q", finishReason)
	}

	assertGeminiClientConfig(t, capturedConfig)
}

func newGeminiBuildTestRequest() chatCompletionRequest {
	return chatCompletionRequest{
		Provider: providerRequestConfig{
			APIKind: providerAPIKindGemini,
			BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
			APIKey:  "",
			ExtraHeaders: map[string]any{
				"X-Test": "present",
			},
			ExtraQuery: nil,
			ExtraBody: map[string]any{
				"temperature": 0.2,
			},
		},
		Model: "gemini-3-flash-preview",
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
			ExtraHeaders: nil,
			ExtraQuery:   nil,
			ExtraBody:    nil,
		},
		Model:    "gemini-3-flash-preview",
		Messages: []chatMessage{{Role: messageRoleUser, Content: "hello"}},
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

	if contents[0].Parts[1].InlineData.MIMEType != "image/png" {
		t.Fatalf("unexpected image MIME type: %q", contents[0].Parts[1].InlineData.MIMEType)
	}

	if string(contents[0].Parts[1].InlineData.Data) != "hello" {
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

	if config.HTTPOptions.BaseURL != "https://generativelanguage.googleapis.com" {
		t.Fatalf("unexpected gemini base URL: %q", config.HTTPOptions.BaseURL)
	}

	if config.HTTPOptions.APIVersion != "v1beta" {
		t.Fatalf("unexpected gemini API version: %q", config.HTTPOptions.APIVersion)
	}

	if config.HTTPOptions.Headers.Get("X-Test") != "present" {
		t.Fatalf("unexpected gemini extra header: %q", config.HTTPOptions.Headers.Get("X-Test"))
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
}
