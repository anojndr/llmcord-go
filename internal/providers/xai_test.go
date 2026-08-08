package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	searchtypes "llmcord-go/internal/searchtypes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	testXAIProviderResponseID = "resp_123"
	testXAIImageOutputID      = "ig_123"
	testXAIImageURL           = "https://assets.grok.com/generated/image.jpg"
	testXAIImageResultBase64  = "aW1hZ2UtYnl0ZXM="
	testXAIInputImageURL      = "data:image/png;base64,YWJj"
	testXAIUploadedFileID     = "file_uploaded_image"
	testXAIAPIVersion         = "2024-12-01-preview"
	testXAIFileUploadPath     = "/v1/files"
)

var errUnexpectedXAIFileUploadRequest = errors.New("unexpected xAI file upload request")

func TestProviderUsesResponsesAPIForXAIProvider(t *testing.T) {
	t.Parallel()

	provider := ProviderRequestConfig{
		APIKind:         ProviderAPIKindOpenAI,
		BaseURL:         "https://api.x.ai/v1",
		APIKey:          "",
		APIKeys:         nil,
		UseResponsesAPI: false,
		EnableGrounding: false,
		ExtraHeaders:    nil,
		ExtraQuery:      nil,
		ExtraBody:       nil,
	}

	if !ProviderUsesResponsesAPI("x-ai", provider) {
		t.Fatal("expected xAI provider to use the Responses API")
	}
}

func TestBuildXAIResponsesRequestBodyDefaultsBridgeSourceAttribution(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("http://127.0.0.1:8787/v1")

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	sourceAttribution, ok := requestBody["source_attribution"].(map[string]any)
	if !ok {
		t.Fatalf("expected source attribution request body, got %#v", requestBody["source_attribution"])
	}

	if sourceAttribution["include_sources"] != true {
		t.Fatalf("expected include_sources=true, got %#v", sourceAttribution)
	}

	if sourceAttribution["include_search_queries"] != true {
		t.Fatalf("expected include_search_queries=true, got %#v", sourceAttribution)
	}
}

func TestBuildXAIResponsesRequestBodySkipsBridgeSourceAttributionForOfficialAPI(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("https://api.x.ai/v1")

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	if _, ok := requestBody["source_attribution"]; ok {
		t.Fatalf("expected official xAI API request to omit source_attribution: %#v", requestBody)
	}
}

func TestBuildXAIResponsesRequestBodyEncodesDocumentPartsAsInputFiles(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("https://api.x.ai/v1")
	request.Messages[1].Content = []ContentPart{
		{"type": searchtypes.ContentTypeText, "text": "Summarize this file."},
		{
			"type":                           searchtypes.ContentTypeDocument,
			searchtypes.ContentFieldBytes:    []byte("document-bytes"),
			searchtypes.ContentFieldMIMEType: searchtypes.MimeTypePDF,
			searchtypes.ContentFieldFilename: testPDFFilename,
		},
	}

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	inputPayload, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input Payload: %#v", requestBody["input"])
	}

	userContent, contentOK := inputPayload[1]["content"].([]map[string]any)
	if !contentOK || len(userContent) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", inputPayload[1]["content"])
	}

	if userContent[0]["type"] != xAIResponsesInputTextType || userContent[0]["text"] != "Summarize this file." {
		t.Fatalf("unexpected first user content part: %#v", userContent[0])
	}

	if userContent[1]["type"] != xAIResponsesInputFileType {
		t.Fatalf("unexpected second user content part: %#v", userContent[1])
	}

	if userContent[1]["filename"] != testPDFFilename {
		t.Fatalf("unexpected xAI file part filename: %#v", userContent[1]["filename"])
	}

	expectedFileData := "data:" + searchtypes.MimeTypePDF + ";base64," +
		base64.StdEncoding.EncodeToString([]byte("document-bytes"))
	if userContent[1]["file_data"] != expectedFileData {
		t.Fatalf("unexpected xAI file data: %#v", userContent[1]["file_data"])
	}
}

func TestBuildXAIResponsesRequestBodyEncodesTextAttachmentsAsInputFiles(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("https://api.x.ai/v1")
	request.Messages[1].Content = []ContentPart{
		{"type": searchtypes.ContentTypeText, "text": "Summarize this file."},
		{
			"type":                           searchtypes.ContentTypeFileData,
			searchtypes.ContentFieldBytes:    []byte("plain-text file contents"),
			searchtypes.ContentFieldMIMEType: "text/plain",
			searchtypes.ContentFieldFilename: "context.txt",
		},
	}

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	inputPayload, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input Payload: %#v", requestBody["input"])
	}

	userContent, contentOK := inputPayload[1]["content"].([]map[string]any)
	if !contentOK || len(userContent) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", inputPayload[1]["content"])
	}

	if userContent[1]["type"] != xAIResponsesInputFileType {
		t.Fatalf("unexpected text attachment content part: %#v", userContent[1])
	}

	expectedFileData := "data:text/plain;base64," +
		base64.StdEncoding.EncodeToString([]byte("plain-text file contents"))
	if userContent[1]["file_data"] != expectedFileData {
		t.Fatalf("unexpected text attachment file data: %#v", userContent[1]["file_data"])
	}
}

func TestBuildXAIResponsesRequestBodyAddsPlaceholderForImageOnlyUserMessage(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("https://api.x.ai/v1")
	request.Messages[1].Content = []ContentPart{
		{"type": searchtypes.ContentTypeText, "text": ""},
		{
			"type":      searchtypes.ContentTypeImageURL,
			"image_url": map[string]string{"url": testXAIInputImageURL},
		},
	}

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	inputPayload, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input Payload: %#v", requestBody["input"])
	}

	userContent, contentOK := inputPayload[1]["content"].([]map[string]any)
	if !contentOK || len(userContent) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", inputPayload[1]["content"])
	}

	if userContent[0]["type"] != xAIResponsesInputTextType ||
		userContent[0]["text"] != FileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected placeholder user part: %#v", userContent[0])
	}
}

func TestBuildXAIResponsesRequestBodyUsesImageFileIDReferences(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("https://api.x.ai/v1")
	request.Messages[1].Content = []ContentPart{
		{"type": searchtypes.ContentTypeText, "text": testGeminiImagePrompt},
		{
			"type":      searchtypes.ContentTypeImageURL,
			"image_url": map[string]any{"file_id": "file_image_123"},
		},
	}

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	inputPayload, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input Payload: %#v", requestBody["input"])
	}

	userContent, contentOK := inputPayload[1]["content"].([]map[string]any)
	if !contentOK || len(userContent) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", inputPayload[1]["content"])
	}

	if userContent[1]["type"] != xAIResponsesInputImageType {
		t.Fatalf("unexpected image content part: %#v", userContent[1])
	}

	if userContent[1]["file_id"] != "file_image_123" {
		t.Fatalf("unexpected xAI image file_id: %#v", userContent[1]["file_id"])
	}

	if _, exists := userContent[1]["image_url"]; exists {
		t.Fatalf("expected image_url to be omitted when file_id is present: %#v", userContent[1])
	}
}

func TestBuildXAIResponsesRequestBodyAddsPlaceholderForDocumentOnlyUserMessage(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("https://api.x.ai/v1")
	request.Messages[1].Content = []ContentPart{
		{"type": searchtypes.ContentTypeText, "text": ""},
		{
			"type":                           searchtypes.ContentTypeDocument,
			searchtypes.ContentFieldBytes:    []byte("document-bytes"),
			searchtypes.ContentFieldMIMEType: searchtypes.MimeTypePDF,
			searchtypes.ContentFieldFilename: testPDFFilename,
		},
	}

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	inputPayload, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input Payload: %#v", requestBody["input"])
	}

	userContent, contentOK := inputPayload[1]["content"].([]map[string]any)
	if !contentOK || len(userContent) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", inputPayload[1]["content"])
	}

	if userContent[0]["type"] != xAIResponsesInputTextType ||
		userContent[0]["text"] != FileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected placeholder user part: %#v", userContent[0])
	}

	if userContent[1]["type"] != xAIResponsesInputFileType ||
		userContent[1]["filename"] != testPDFFilename {
		t.Fatalf("unexpected file user part: %#v", userContent[1])
	}
}

func TestBuildXAIResponsesRequestBodyAppendsReplyTargetImageToLatestUserTurn(t *testing.T) {
	t.Parallel()

	requestBody := newXAIReplyTargetImageRequestBody(t, testXAIInputImageURL)

	inputPayload, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input Payload: %#v", requestBody["input"])
	}

	latestUserContent, contentOK := inputPayload[1]["content"].([]map[string]any)
	if !contentOK {
		t.Fatalf("expected multimodal latest user content, got %#v", inputPayload[1]["content"])
	}

	if len(latestUserContent) != 2 {
		t.Fatalf("unexpected latest user content part count: %#v", latestUserContent)
	}

	if latestUserContent[0]["type"] != xAIResponsesInputTextType ||
		latestUserContent[0]["text"] != "describe this" {
		t.Fatalf("unexpected latest user text part: %#v", latestUserContent[0])
	}

	if latestUserContent[1]["type"] != xAIResponsesInputImageType ||
		latestUserContent[1]["image_url"] != testXAIInputImageURL {
		t.Fatalf("unexpected latest user image part: %#v", latestUserContent[1])
	}
}

func TestBuildXAIResponsesRequestBodySkipsReplyChainImageWhenFollowUpHasOwnImage(t *testing.T) {
	t.Parallel()

	const yellowImageURL = "data:image/png;base64,yellow"

	requestBody := newXAIFollowUpRequestBody(
		t,
		"how bout",
		yellowImageURL,
	)

	if requestBody["previous_response_id"] != testXAIProviderResponseID {
		t.Fatalf("unexpected previous response id: %#v", requestBody["previous_response_id"])
	}

	inputPayload, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(inputPayload) != 1 {
		t.Fatalf("unexpected trimmed input Payload: %#v", requestBody["input"])
	}

	latestUserContent, contentOK := inputPayload[0]["content"].([]map[string]any)
	if !contentOK {
		t.Fatalf("expected multimodal latest user content, got %#v", inputPayload[0]["content"])
	}

	if len(latestUserContent) != 2 {
		t.Fatalf("unexpected latest user content part count: %#v", latestUserContent)
	}

	if latestUserContent[1]["type"] != xAIResponsesInputImageType ||
		latestUserContent[1]["image_url"] != yellowImageURL {
		t.Fatalf("unexpected latest user image part: %#v", latestUserContent[1])
	}
}

func TestBuildXAIResponsesRequestBodySkipsReplyChainImageWhenFollowUpHasNoOwnImage(t *testing.T) {
	t.Parallel()

	requestBody := newXAIFollowUpRequestBody(t, "ty", "")

	if requestBody["previous_response_id"] != testXAIProviderResponseID {
		t.Fatalf("unexpected previous response id: %#v", requestBody["previous_response_id"])
	}

	inputPayload, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(inputPayload) != 1 {
		t.Fatalf("unexpected trimmed input Payload: %#v", requestBody["input"])
	}

	if inputPayload[0]["content"] != "ty" {
		t.Fatalf("unexpected text-only follow-up content: %#v", inputPayload[0]["content"])
	}
}

func TestPrepareXAIResponsesRequestBodyUploadsLargeInlineImagesAsFiles(t *testing.T) {
	t.Parallel()

	largeImage := bytes.Repeat([]byte("x"), XAIInlineImageByteLimit+1)
	largeImageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(largeImage)

	uploadCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		uploadCount++

		assertXAIFileUploadRequest(t, request, largeImage)

		responseWriter.Header().Set("Content-Type", "application/json")

		_, err := responseWriter.Write([]byte(`{"id":"file_large_image"}`))
		if err != nil {
			t.Fatalf("write upload response: %v", err)
		}
	}))
	defer server.Close()

	request := newXAIResponsesStreamingRequest(server.URL + "/v1")
	request.Messages[1].Content = []ContentPart{
		{"type": searchtypes.ContentTypeText, "text": testGeminiImagePrompt},
		{
			"type":      searchtypes.ContentTypeImageURL,
			"image_url": map[string]string{"url": largeImageURL},
		},
	}

	requestBody, err := prepareXAIResponsesRequestBody(context.Background(), server.Client(), request)
	if err != nil {
		t.Fatalf("prepare xAI responses request body: %v", err)
	}

	if uploadCount != 1 {
		t.Fatalf("unexpected xAI image upload count: %d", uploadCount)
	}

	inputPayload, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input Payload: %#v", requestBody["input"])
	}

	userContent, contentOK := inputPayload[1]["content"].([]map[string]any)
	if !contentOK || len(userContent) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", inputPayload[1]["content"])
	}

	if userContent[1]["type"] != xAIResponsesInputImageType {
		t.Fatalf("unexpected image content part: %#v", userContent[1])
	}

	if userContent[1]["file_id"] != "file_large_image" {
		t.Fatalf("unexpected uploaded xAI image file_id: %#v", userContent[1]["file_id"])
	}

	if _, exists := userContent[1]["image_url"]; exists {
		t.Fatalf("expected inline image_url to be replaced after upload: %#v", userContent[1])
	}
}

func TestPrepareXAIResponsesRequestBodyUploadsBridgeInlineImagesAsFiles(t *testing.T) {
	t.Parallel()

	smallImage := []byte("small-image")
	smallImageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(smallImage)

	uploadCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		uploadCount++

		assertXAIFileUploadRequest(t, request, smallImage)

		responseWriter.Header().Set("Content-Type", "application/json")

		_, err := responseWriter.Write([]byte(`{"id":"file_bridge_image"}`))
		if err != nil {
			t.Fatalf("write upload response: %v", err)
		}
	}))
	defer server.Close()

	request := newXAIResponsesStreamingRequest(server.URL + "/v1")
	request.Messages[1].Content = []ContentPart{
		{"type": searchtypes.ContentTypeText, "text": testGeminiImagePrompt},
		{
			"type":      searchtypes.ContentTypeImageURL,
			"image_url": map[string]string{"url": smallImageURL},
		},
	}

	requestBody, err := prepareXAIResponsesRequestBody(context.Background(), server.Client(), request)
	if err != nil {
		t.Fatalf("prepare xAI responses request body: %v", err)
	}

	if uploadCount != 1 {
		t.Fatalf("unexpected xAI bridge image upload count: %d", uploadCount)
	}

	inputPayload, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input Payload: %#v", requestBody["input"])
	}

	userContent, contentOK := inputPayload[1]["content"].([]map[string]any)
	if !contentOK || len(userContent) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", inputPayload[1]["content"])
	}

	if userContent[1]["type"] != xAIResponsesInputImageType {
		t.Fatalf("unexpected image content part: %#v", userContent[1])
	}

	if userContent[1]["file_id"] != "file_bridge_image" {
		t.Fatalf("unexpected uploaded xAI bridge image file_id: %#v", userContent[1]["file_id"])
	}

	if _, exists := userContent[1]["image_url"]; exists {
		t.Fatalf("expected bridge inline image_url to be replaced after upload: %#v", userContent[1])
	}
}

func TestPrepareXAIResponsesRequestBodyKeepsSmallOfficialInlineImages(t *testing.T) {
	t.Parallel()

	smallImageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("small-image"))

	httpClient := newTestHTTPClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected xAI file upload request: %s %s", request.Method, request.URL.String())

		return nil, errUnexpectedXAIFileUploadRequest
	}))

	request := newXAIResponsesStreamingRequest("https://api.x.ai/v1")
	request.Messages[1].Content = []ContentPart{
		{"type": searchtypes.ContentTypeText, "text": testGeminiImagePrompt},
		{
			"type":      searchtypes.ContentTypeImageURL,
			"image_url": map[string]string{"url": smallImageURL},
		},
	}

	requestBody, err := prepareXAIResponsesRequestBody(context.Background(), httpClient, request)
	if err != nil {
		t.Fatalf("prepare xAI responses request body: %v", err)
	}

	inputPayload, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input Payload: %#v", requestBody["input"])
	}

	userContent, contentOK := inputPayload[1]["content"].([]map[string]any)
	if !contentOK || len(userContent) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", inputPayload[1]["content"])
	}

	if userContent[1]["type"] != xAIResponsesInputImageType {
		t.Fatalf("unexpected image content part: %#v", userContent[1])
	}

	if userContent[1]["image_url"] != smallImageURL {
		t.Fatalf("expected official xAI API to keep the inline image_url: %#v", userContent[1]["image_url"])
	}

	if _, exists := userContent[1]["file_id"]; exists {
		t.Fatalf("did not expect official xAI API to add file_id for a small inline image: %#v", userContent[1])
	}
}

func TestXAIProviderUsesOfficialAPIBaseURL(t *testing.T) {
	t.Parallel()

	provider := ProviderRequestConfig{
		APIKind:         ProviderAPIKindOpenAI,
		BaseURL:         "https://api.x.ai/v1",
		APIKey:          "",
		APIKeys:         nil,
		UseResponsesAPI: false,
		EnableGrounding: false,
		ExtraHeaders:    nil,
		ExtraQuery:      nil,
		ExtraBody:       nil,
	}

	if !ProviderUsesResponsesAPI("x-ai", provider) {
		t.Fatal("expected xAI provider to use the Responses API")
	}
}

func TestOpenAIClientStreamChatCompletionUsesXAIResponsesAPI(t *testing.T) {
	t.Parallel()

	server := newXAIResponsesStreamingTestServer(t)
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := newXAIResponsesStreamingRequest(server.URL + "/v1")

	var (
		joinedContent  strings.Builder
		finishReason   string
		providerRespID string
		metadata       *searchtypes.SearchMetadata
	)

	err := client.streamChatCompletion(context.Background(), request, func(delta StreamDelta) error {
		joinedContent.WriteString(delta.Content)

		if delta.FinishReason != "" {
			finishReason = delta.FinishReason
		}

		if delta.ProviderResponseID != "" {
			providerRespID = delta.ProviderResponseID
		}

		if delta.SearchMetadata != nil {
			metadata = searchtypes.CloneSearchMetadata(delta.SearchMetadata)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("stream xAI responses chat completion: %v", err)
	}

	if joinedContent.String() != testStreamedHelloText {
		t.Fatalf("unexpected streamed content: %q", joinedContent.String())
	}

	if finishReason != finishReasonStop {
		t.Fatalf("unexpected finish reason: %q", finishReason)
	}

	if providerRespID != testXAIProviderResponseID {
		t.Fatalf("unexpected provider response id: %q", providerRespID)
	}

	if metadata == nil {
		t.Fatal("expected xAI source attribution metadata from completed response")
	}

	if len(metadata.Queries) != 1 || metadata.Queries[0] != "latest ai news" {
		t.Fatalf("unexpected search queries: %#v", metadata.Queries)
	}

	if len(metadata.Results) != 1 {
		t.Fatalf("unexpected source result count: %#v", metadata.Results)
	}

	sources := searchtypes.ExtractSearchSources(metadata.Results[0].Text)
	if len(sources) != 1 {
		t.Fatalf("unexpected parsed source count: %#v", sources)
	}

	if sources[0].Title != "Example Source" || sources[0].URL != "https://example.com/source" {
		t.Fatalf("unexpected parsed source: %#v", sources[0])
	}
}

func TestOpenAIClientStreamChatCompletionStreamsXAIImageOutputOnce(t *testing.T) {
	t.Parallel()

	server := newXAIResponsesStreamingImageTestServer(t, true)
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := newXAIResponsesStreamingRequest(server.URL + "/v1")

	var joinedContent strings.Builder

	err := client.streamChatCompletion(context.Background(), request, func(delta StreamDelta) error {
		joinedContent.WriteString(delta.Content)

		return nil
	})
	if err != nil {
		t.Fatalf("stream xAI responses image output: %v", err)
	}

	expected := testStreamedHelloText + "\n\nGenerated image:\n" + testXAIImageURL
	if joinedContent.String() != expected {
		t.Fatalf("unexpected streamed image content: got %q want %q", joinedContent.String(), expected)
	}
}

func TestOpenAIClientStreamChatCompletionFallsBackToCompletedXAIImageOutput(t *testing.T) {
	t.Parallel()

	server := newXAIResponsesStreamingImageTestServer(t, false)
	defer server.Close()

	client := newOpenAIClient(server.Client())
	request := newXAIResponsesStreamingRequest(server.URL + "/v1")

	var joinedContent strings.Builder

	err := client.streamChatCompletion(context.Background(), request, func(delta StreamDelta) error {
		joinedContent.WriteString(delta.Content)

		return nil
	})
	if err != nil {
		t.Fatalf("stream xAI completed image output: %v", err)
	}

	expected := testStreamedHelloText + "\n\nGenerated image:\n" + testXAIImageURL
	if joinedContent.String() != expected {
		t.Fatalf("unexpected completed image content: got %q want %q", joinedContent.String(), expected)
	}
}

func TestAssignXAIPreviousResponseIDUsesAssistantAnchorAndTrimsHistory(t *testing.T) {
	t.Parallel()

	store := newFakeChainStore(8)

	rootUser := newTestDiscordMessage("100")
	assistant := newTestDiscordMessage("200")
	firstFollowUp := newTestDiscordMessage("300")
	secondFollowUp := newTestDiscordMessage("400")

	store.setConversationNode(rootUser.ID, searchtypes.MessageRoleUser, "", "", nil)
	store.setConversationNode(
		assistant.ID,
		searchtypes.MessageRoleAssistant,
		testXAIProviderResponseID,
		"x-ai/grok-4",
		rootUser,
	)
	store.setConversationNode(firstFollowUp.ID, searchtypes.MessageRoleUser, "", "", assistant)
	store.setConversationNode(secondFollowUp.ID, searchtypes.MessageRoleUser, "", "", firstFollowUp)

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "",
		ConfiguredModel:    "x-ai/grok-4",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{Role: searchtypes.MessageRoleUser, Content: "first question"},
			{Role: searchtypes.MessageRoleAssistant, Content: "first answer"},
			{Role: searchtypes.MessageRoleUser, Content: "follow-up one"},
			{Role: searchtypes.MessageRoleUser, Content: "follow-up two"},
		},
	}

	AssignXAIPreviousResponseID(&request, secondFollowUp, store, 25)

	if request.PreviousResponseID != testXAIProviderResponseID {
		t.Fatalf("unexpected previous response id: %q", request.PreviousResponseID)
	}

	if len(request.Messages) != 3 {
		t.Fatalf("unexpected continuation message count: %d", len(request.Messages))
	}

	if request.Messages[0].Role != searchtypes.MessageRoleSystem || request.Messages[0].Content != "You are concise." {
		t.Fatalf("expected system prompt to lead the continuation messages: %#v", request.Messages)
	}

	if request.Messages[1].Content != "follow-up one" || request.Messages[2].Content != "follow-up two" {
		t.Fatalf("unexpected continuation messages: %#v", request.Messages)
	}
}

func TestAssignXAIPreviousResponseIDKeepsSystemPromptLeadingContinuation(t *testing.T) {
	t.Parallel()

	store := newFakeChainStore(8)

	rootUser := newTestDiscordMessage("100")
	assistant := newTestDiscordMessage("200")
	followUp := newTestDiscordMessage("300")

	store.setConversationNode(rootUser.ID, searchtypes.MessageRoleUser, "", "", nil)
	store.setConversationNode(
		assistant.ID,
		searchtypes.MessageRoleAssistant,
		testXAIProviderResponseID,
		"x-ai/grok-4",
		rootUser,
	)
	store.setConversationNode(followUp.ID, searchtypes.MessageRoleUser, "", "", assistant)

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "",
		ConfiguredModel:    "x-ai/grok-4",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{Role: searchtypes.MessageRoleUser, Content: "first question"},
			{Role: searchtypes.MessageRoleAssistant, Content: "first answer"},
			{Role: searchtypes.MessageRoleUser, Content: "follow-up"},
		},
	}

	AssignXAIPreviousResponseID(&request, followUp, store, 25)

	if request.PreviousResponseID != testXAIProviderResponseID {
		t.Fatalf("unexpected previous response id: %q", request.PreviousResponseID)
	}

	if len(request.Messages) != 2 {
		t.Fatalf("unexpected continuation message count: %d", len(request.Messages))
	}

	if request.Messages[0].Role != searchtypes.MessageRoleSystem || request.Messages[0].Content != "You are concise." {
		t.Fatalf("expected system prompt to lead the continuation messages: %#v", request.Messages)
	}

	if request.Messages[1].Content != "follow-up" {
		t.Fatalf("unexpected continuation messages: %#v", request.Messages)
	}
}

func TestAssignXAIPreviousResponseIDSkipsBuiltInOpenAIProvider(t *testing.T) {
	t.Parallel()

	store := newFakeChainStore(4)

	rootUser := newTestDiscordMessage("100")
	assistant := newTestDiscordMessage("200")
	followUp := newTestDiscordMessage("300")

	store.setConversationNode(rootUser.ID, searchtypes.MessageRoleUser, "", "", nil)
	store.setConversationNode(
		assistant.ID,
		searchtypes.MessageRoleAssistant,
		testXAIProviderResponseID,
		"openai/gpt-5.4",
		rootUser,
	)
	store.setConversationNode(followUp.ID, searchtypes.MessageRoleUser, "", "", assistant)

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         testOfficialOpenAIBaseURL,
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "gpt-5.4",
		ConfiguredModel:    "openai/gpt-5.4",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{Role: searchtypes.MessageRoleUser, Content: "first question"},
			{Role: searchtypes.MessageRoleAssistant, Content: "first answer"},
			{Role: searchtypes.MessageRoleUser, Content: "follow-up"},
		},
	}
	originalMessages := append([]ChatMessage(nil), request.Messages...)

	AssignXAIPreviousResponseID(&request, followUp, store, 25)

	if request.PreviousResponseID != "" {
		t.Fatalf("unexpected previous response id: %q", request.PreviousResponseID)
	}

	if !slices.Equal(request.Messages, originalMessages) {
		t.Fatalf("unexpected openai message trimming: %#v", request.Messages)
	}
}

func TestFinalizeXAIResponseAnswerParsesBridgeSourcesAndStripsAppendix(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("http://127.0.0.1:8787/v1")
	answerText := "Answer paragraph.\n\nSources\n" +
		"1. [Example Source](https://example.com/source) (example.com/source) via `latest ai news`\n" +
		"2. [Another Source](https://example.com/other) (example.com/other)\n\n" +
		"Search Queries\n" +
		"1. `latest ai news`\n"

	cleanedText, metadata := FinalizeXAIResponseAnswer(request, answerText, nil)

	if cleanedText != "Answer paragraph." {
		t.Fatalf("unexpected cleaned answer text: %q", cleanedText)
	}

	if metadata == nil {
		t.Fatal("expected parsed xAI bridge search metadata")
	}

	if len(metadata.Queries) != 1 || metadata.Queries[0] != "latest ai news" {
		t.Fatalf("unexpected parsed queries: %#v", metadata.Queries)
	}

	if len(metadata.Results) != 2 {
		t.Fatalf("unexpected parsed result groups: %#v", metadata.Results)
	}

	firstResultSources := searchtypes.ExtractSearchSources(metadata.Results[0].Text)
	if len(firstResultSources) != 1 || firstResultSources[0].URL != "https://example.com/source" {
		t.Fatalf("unexpected scoped source parsing: %#v", firstResultSources)
	}

	secondResultSources := searchtypes.ExtractSearchSources(metadata.Results[1].Text)
	if len(secondResultSources) != 1 || secondResultSources[0].URL != "https://example.com/other" {
		t.Fatalf("unexpected unscoped source parsing: %#v", secondResultSources)
	}
}

func TestFinalizeXAIResponseAnswerParsesBridgeSourcesForNonGrokModels(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "http://127.0.0.1:8787/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "gpt-4o",
		ConfiguredModel:    "openai/gpt-4o",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages:           nil,
	}
	answerText := "Answer paragraph.\n\nSources\n" +
		"1. [Example Source](https://example.com/source) (example.com/source) via `latest ai news`\n"

	cleanedText, metadata := FinalizeXAIResponseAnswer(request, answerText, nil)

	if cleanedText != "Answer paragraph." {
		t.Fatalf("unexpected cleaned answer text: %q", cleanedText)
	}

	if metadata == nil {
		t.Fatal("expected parsed search metadata for non-grok model")
	}

	if len(metadata.Results) != 1 {
		t.Fatalf("unexpected parsed result groups: %#v", metadata.Results)
	}

	sources := searchtypes.ExtractSearchSources(metadata.Results[0].Text)
	if len(sources) != 1 || sources[0].URL != "https://example.com/source" {
		t.Fatalf("unexpected source parsing for non-grok model: %#v", sources)
	}
}

func TestFinalizeXAIResponseAnswerParsesVariousSourceAppendixFormatsForNonGrokModels(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "https://openrouter.ai/api/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "claude-3-5-sonnet",
		ConfiguredModel:    "anthropic/claude-3-5-sonnet",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages:           nil,
	}

	tests := []struct {
		name         string
		answerText   string
		expectedText string
		expectedURL  string
	}{
		{
			name:         "markdown h3 header with bracketed index",
			answerText:   "Main answer content.\n\n### Sources:\n[1] [Claude Reference](https://docs.anthropic.com/claude)",
			expectedText: "Main answer content.",
			expectedURL:  "https://docs.anthropic.com/claude",
		},
		{
			name:         "bold references header with hyphen bullet and title colon url",
			answerText:   "Main answer content.\n\n**References:**\n- DeepSeek Docs: https://docs.deepseek.com/api",
			expectedText: "Main answer content.",
			expectedURL:  "https://docs.deepseek.com/api",
		},
		{
			name:         "citations header with asterisk bullet and title dash url",
			answerText:   "Main answer content.\n\nCitations:\n* Llama Info - https://llama.meta.com/info",
			expectedText: "Main answer content.",
			expectedURL:  "https://llama.meta.com/info",
		},
		{
			name:         "source urls header with bare url",
			answerText:   "Main answer content.\n\nSource URLs:\n1. <https://example.org/bare-link>",
			expectedText: "Main answer content.",
			expectedURL:  "https://example.org/bare-link",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cleanedText, metadata := FinalizeXAIResponseAnswer(request, testCase.answerText, nil)
			if cleanedText != testCase.expectedText {
				t.Fatalf("cleaned text mismatch: got %q, want %q", cleanedText, testCase.expectedText)
			}

			if metadata == nil {
				t.Fatalf("expected non-nil search metadata for %s", testCase.name)
			}

			if len(metadata.Results) == 0 {
				t.Fatalf("expected search metadata results for %s", testCase.name)
			}

			sources := searchtypes.ExtractSearchSources(metadata.Results[0].Text)
			if len(sources) != 1 || sources[0].URL != testCase.expectedURL {
				t.Fatalf("unexpected source URL for %s: %#v (expected %q)", testCase.name, sources, testCase.expectedURL)
			}
		})
	}
}

func TestXAIStreamingVisibleAnswerTextHidesBridgeSourceAppendix(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("http://127.0.0.1:8787/v1")

	tests := []struct {
		name     string
		answer   string
		expected string
	}{
		{
			name:     "complete appendix heading is hidden",
			answer:   "Answer paragraph.\n\nSources\n1. [Example Source](https://example.com/source)",
			expected: "Answer paragraph.",
		},
		{
			name:     "partial appendix heading is hidden",
			answer:   "Answer paragraph.\n\nSo",
			expected: "Answer paragraph.",
		},
		{
			name:     "split paragraph separator is hidden",
			answer:   "Answer paragraph.\n",
			expected: "Answer paragraph.",
		},
		{
			name:     "partial markdown appendix heading is hidden",
			answer:   "Answer paragraph.\n\n### ",
			expected: "Answer paragraph.",
		},
		{
			name:     "complete bold appendix heading is hidden",
			answer:   "Answer paragraph.\n\n**References:**",
			expected: "Answer paragraph.",
		},
		{
			name:     "crlf appendix heading uses original offsets",
			answer:   "First line.\r\nSecond line.\r\n\r\n### Sources:",
			expected: "First line.\r\nSecond line.",
		},
		{
			name:     "overlapping crlf separators hide the appendix",
			answer:   "First line.\r\nSecond line.\r\n\r\n\r\n### Sources:",
			expected: "First line.\r\nSecond line.",
		},
		{
			name:     "non appendix text stays visible",
			answer:   "Answer paragraph.\n\nSummary",
			expected: "Answer paragraph.\n\nSummary",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := XAIStreamingVisibleAnswerText(request, testCase.answer)
			if got != testCase.expected {
				t.Fatalf("unexpected visible answer text: got %q want %q", got, testCase.expected)
			}
		})
	}
}

func TestXAIStreamingVisibleAnswerTextNeverRegressesAcrossSourceAppendixChunks(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("http://127.0.0.1:8787/v1")
	tests := []struct {
		name     string
		answer   string
		expected string
	}{
		{
			name:     "plain header",
			answer:   "Answer paragraph.\n\nSources\n1. [Example](https://example.com)",
			expected: "Answer paragraph.",
		},
		{
			name:     "markdown header",
			answer:   "Answer paragraph.\n\n### Sources:\n1. [Example](https://example.com)",
			expected: "Answer paragraph.",
		},
		{
			name:     "bold header",
			answer:   "Answer paragraph.\n\n**References:**\n- Example: https://example.com",
			expected: "Answer paragraph.",
		},
		{
			name:     "crlf header",
			answer:   "First line.\r\nSecond line.\r\n\r\nSource URLs\r\n1. https://example.com",
			expected: "First line.\r\nSecond line.",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			previousVisible := ""

			for end := 1; end <= len(testCase.answer); end++ {
				visible := XAIStreamingVisibleAnswerText(request, testCase.answer[:end])
				if !strings.HasPrefix(visible, previousVisible) {
					t.Fatalf(
						"visibility regressed at byte %d: previous %q current %q",
						end,
						previousVisible,
						visible,
					)
				}

				previousVisible = visible
			}

			if previousVisible != testCase.expected {
				t.Fatalf("unexpected final visible text: got %q want %q", previousVisible, testCase.expected)
			}
		})
	}
}

func TestXAIStreamingVisibleAnswerTextLeavesOfficialAPIUntouched(t *testing.T) {
	t.Parallel()

	request := newXAIResponsesStreamingRequest("https://api.x.ai/v1")
	answerText := "Answer paragraph.\n\nSources\n1. [Example Source](https://example.com/source)"

	if got := XAIStreamingVisibleAnswerText(request, answerText); got != answerText {
		t.Fatalf("unexpected official xAI streaming answer text: %q", got)
	}
}

func newXAIResponsesStreamingTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		if request.URL.Path == testXAIFileUploadPath {
			assertXAIFileUploadRequest(t, request, []byte("abc"))
			writeXAIFileUploadResponse(t, responseWriter)

			return
		}

		assertXAIUploadedResponsesRequest(t, request)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		flusher, responseOK := responseWriter.(http.Flusher)
		if !responseOK {
			t.Fatal("expected response writer to support flushing")
		}

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n",
		)
		flusher.Flush()

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n",
		)
		flusher.Flush()

		writeStreamChunk(t, responseWriter, xAIResponseCompletedChunk())
		flusher.Flush()

		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func newXAIResponsesStreamingImageTestServer(
	t *testing.T,
	includeOutputItemDone bool,
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		request *http.Request,
	) {
		t.Helper()

		if request.URL.Path == testXAIFileUploadPath {
			assertXAIFileUploadRequest(t, request, []byte("abc"))
			writeXAIFileUploadResponse(t, responseWriter)

			return
		}

		assertXAIUploadedResponsesRequest(t, request)

		responseWriter.Header().Set("Content-Type", "text/event-stream")

		flusher, responseOK := responseWriter.(http.Flusher)
		if !responseOK {
			t.Fatal("expected response writer to support flushing")
		}

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n",
		)
		flusher.Flush()

		writeStreamChunk(
			t,
			responseWriter,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n",
		)
		flusher.Flush()

		if includeOutputItemDone {
			writeStreamChunk(t, responseWriter, xAIResponseOutputItemDoneChunk())
			flusher.Flush()
		}

		writeStreamChunk(t, responseWriter, xAIResponseCompletedChunkWithImageOutput())
		flusher.Flush()

		writeStreamChunk(t, responseWriter, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func assertXAIUploadedResponsesRequest(t *testing.T, request *http.Request) {
	t.Helper()

	assertXAIResponsesRequestWithImageAssertion(t, request, assertXAIUploadedResponsesUserMessage)
}

func assertXAIResponsesRequestWithImageAssertion(
	t *testing.T,
	request *http.Request,
	assertUserMessage func(*testing.T, any),
) {
	t.Helper()

	if request.URL.Path != "/v1/responses" {
		t.Fatalf("unexpected path: %s", request.URL.Path)
	}

	if request.URL.Query().Get("api-version") != testXAIAPIVersion {
		t.Fatalf("unexpected query string: %s", request.URL.RawQuery)
	}

	if request.Header.Get("Authorization") != testXAIAuthHeader {
		t.Fatalf("unexpected authorization header: %q", request.Header.Get("Authorization"))
	}

	if request.Header.Get("X-Test") != "present" {
		t.Fatalf("unexpected extra header: %q", request.Header.Get("X-Test"))
	}

	var payload map[string]any

	err := json.NewDecoder(request.Body).Decode(&payload)
	if err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	if payload["model"] != "grok-4" {
		t.Fatalf("unexpected model: %#v", payload["model"])
	}

	if payload["stream"] != true {
		t.Fatalf("unexpected stream flag: %#v", payload["stream"])
	}

	sourceAttribution, sourceAttributionOK := payload["source_attribution"].(map[string]any)
	if !sourceAttributionOK {
		t.Fatalf("unexpected source_attribution Payload: %#v", payload["source_attribution"])
	}

	if sourceAttribution["include_sources"] != true || sourceAttribution["include_search_queries"] != true {
		t.Fatalf("unexpected source_attribution settings: %#v", sourceAttribution)
	}

	inputPayload, inputOK := payload["input"].([]any)
	if !inputOK || len(inputPayload) != 2 {
		t.Fatalf("unexpected input Payload: %#v", payload["input"])
	}

	assertXAIResponsesSystemMessage(t, inputPayload[0])
	assertUserMessage(t, inputPayload[1])
}

func assertXAIResponsesSystemMessage(t *testing.T, rawMessage any) {
	t.Helper()

	systemMessage, messageOK := rawMessage.(map[string]any)
	if !messageOK {
		t.Fatalf("unexpected system message Payload: %#v", rawMessage)
	}

	if systemMessage["role"] != searchtypes.MessageRoleSystem || systemMessage["content"] != "You are concise." {
		t.Fatalf("unexpected system message: %#v", systemMessage)
	}
}

func assertXAIUploadedResponsesUserMessage(t *testing.T, rawMessage any) {
	t.Helper()

	userMessage, messageOK := rawMessage.(map[string]any)
	if !messageOK {
		t.Fatalf("unexpected user message Payload: %#v", rawMessage)
	}

	userContent, contentOK := userMessage["content"].([]any)
	if !contentOK || len(userContent) != 2 {
		t.Fatalf("unexpected user content Payload: %#v", userMessage["content"])
	}

	firstPart, firstPartOK := userContent[0].(map[string]any)
	if !firstPartOK {
		t.Fatalf("unexpected first user content part: %#v", userContent[0])
	}

	if firstPart["type"] != xAIResponsesInputTextType || firstPart["text"] != "What is in this image?" {
		t.Fatalf("unexpected first user content part: %#v", firstPart)
	}

	secondPart, secondPartOK := userContent[1].(map[string]any)
	if !secondPartOK {
		t.Fatalf("unexpected second user content part: %#v", userContent[1])
	}

	if secondPart["type"] != xAIResponsesInputImageType {
		t.Fatalf("unexpected second user content part: %#v", secondPart)
	}

	if secondPart["file_id"] != testXAIUploadedFileID {
		t.Fatalf("unexpected uploaded image file_id: %#v", secondPart["file_id"])
	}

	if _, exists := secondPart["image_url"]; exists {
		t.Fatalf("did not expect uploaded bridge image request to keep image_url: %#v", secondPart)
	}
}

func assertXAIFileUploadRequest(
	t *testing.T,
	request *http.Request,
	expectedBytes []byte,
) {
	t.Helper()

	if request.URL.Path != testXAIFileUploadPath {
		t.Fatalf("unexpected upload path: %s", request.URL.Path)
	}

	if request.URL.Query().Get("api-version") != testXAIAPIVersion {
		t.Fatalf("unexpected upload query string: %s", request.URL.RawQuery)
	}

	if request.Header.Get("Authorization") != testXAIAuthHeader {
		t.Fatalf("unexpected upload authorization header: %q", request.Header.Get("Authorization"))
	}

	if request.Header.Get("X-Test") != "present" {
		t.Fatalf("unexpected upload extra header: %q", request.Header.Get("X-Test"))
	}

	multipartReader, err := request.MultipartReader()
	if err != nil {
		t.Fatalf("build xAI upload multipart reader: %v", err)
	}

	var (
		purposeValue    string
		fileBytes       []byte
		fileContentType string
		fileName        string
	)

	for {
		part, partErr := multipartReader.NextPart()
		if partErr == io.EOF {
			break
		}

		if partErr != nil {
			t.Fatalf("read xAI upload part: %v", partErr)
		}

		partBody, readErr := io.ReadAll(part)
		if readErr != nil {
			t.Fatalf("read xAI upload part body: %v", readErr)
		}

		switch part.FormName() {
		case "purpose":
			purposeValue = string(partBody)
		case "file":
			fileBytes = partBody
			fileContentType = part.Header.Get("Content-Type")
			fileName = part.FileName()
		default:
			t.Fatalf("unexpected xAI upload form field: %q", part.FormName())
		}
	}

	if purposeValue != XAIResponsesUploadPurposeUserData {
		t.Fatalf("unexpected xAI upload purpose: %q", purposeValue)
	}

	if fileContentType != searchtypes.MimeTypePNG {
		t.Fatalf("unexpected xAI upload content type: %q", fileContentType)
	}

	if fileName != xAIInputImageUploadFilename {
		t.Fatalf("unexpected xAI upload filename: %q", fileName)
	}

	if !bytes.Equal(fileBytes, expectedBytes) {
		t.Fatal("unexpected uploaded xAI image bytes")
	}
}

func writeXAIFileUploadResponse(t *testing.T, responseWriter http.ResponseWriter) {
	t.Helper()

	responseWriter.Header().Set("Content-Type", "application/json")

	_, err := responseWriter.Write([]byte(`{"id":"` + testXAIUploadedFileID + `"}`))
	if err != nil {
		t.Fatalf("write upload response: %v", err)
	}
}

func xAIResponseCompletedChunk() string {
	return "data: {\"type\":\"response.completed\",\"response\":{" +
		"\"id\":\"" + testXAIProviderResponseID + "\"," +
		"\"status\":\"completed\"," +
		"\"usage\":{\"input_tokens\":12,\"output_tokens\":34}," +
		"\"source_attribution\":{" +
		"\"search_queries\":[\"latest ai news\"]," +
		"\"sources\":[{" +
		"\"title\":\"Example Source\"," +
		"\"url\":\"https://example.com/source\"," +
		"\"search_queries\":[\"latest ai news\"]" +
		"}]}}}\n\n"
}

func xAIResponseOutputItemDoneChunk() string {
	return "data: {\"type\":\"response.output_item.done\",\"item\":{" +
		"\"id\":\"" + testXAIImageOutputID + "\"," +
		"\"type\":\"image_generation_call\"," +
		"\"status\":\"completed\"," +
		"\"result_url\":\"" + testXAIImageURL + "\"," +
		"\"mime_type\":\"image/jpeg\"," +
		"\"action\":\"generate\"," +
		"\"prompt\":\"Generate an image of a cat.\"}}\n\n"
}

func xAIResponseCompletedChunkWithImageOutput() string {
	return "data: {\"type\":\"response.completed\",\"response\":{" +
		"\"id\":\"" + testXAIProviderResponseID + "\"," +
		"\"status\":\"completed\"," +
		"\"usage\":{\"input_tokens\":12,\"output_tokens\":34}," +
		"\"output\":[{" +
		"\"id\":\"" + testXAIImageOutputID + "\"," +
		"\"type\":\"image_generation_call\"," +
		"\"status\":\"completed\"," +
		"\"result\":\"" + testXAIImageResultBase64 + "\"," +
		"\"result_url\":\"" + testXAIImageURL + "\"," +
		"\"mime_type\":\"image/jpeg\"," +
		"\"action\":\"generate\"," +
		"\"prompt\":\"Generate an image of a cat.\"}]," +
		"\"source_attribution\":{" +
		"\"search_queries\":[\"latest ai news\"]," +
		"\"sources\":[{" +
		"\"title\":\"Example Source\"," +
		"\"url\":\"https://example.com/source\"," +
		"\"search_queries\":[\"latest ai news\"]" +
		"}]}}}\n\n"
}

func newXAIResponsesStreamingRequest(baseURL string) ChatCompletionRequest {
	return ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         baseURL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders: map[string]any{
				"X-Test": "present",
			},
			ExtraQuery: map[string]any{
				"api-version": testXAIAPIVersion,
			},
			ExtraBody: nil,
		},
		Model:              "grok-4",
		ConfiguredModel:    "x-ai/grok-4",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{
				Role: searchtypes.MessageRoleUser,
				Content: []ContentPart{
					{"type": searchtypes.ContentTypeText, "text": "What is in this image?"},
					{
						"type":      searchtypes.ContentTypeImageURL,
						"image_url": map[string]string{"url": testXAIInputImageURL},
					},
				},
			},
		},
	}
}

func newXAIReplyTargetImageRequestBody(
	t *testing.T,
	replyTargetImage string,
) map[string]any {
	t.Helper()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "https://api.x.ai/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "grok-4",
		ConfiguredModel:    "x-ai/grok-4:vision",
		SessionID:          "",
		PreviousResponseID: "",
		RequestID:          "",
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
			{
				Role: searchtypes.MessageRoleUser,
				Content: []ContentPart{
					{searchtypes.MessageTypeKey: searchtypes.ContentTypeText, searchtypes.MessageTextKey: "describe this"},
					{
						searchtypes.MessageTypeKey: searchtypes.ContentTypeImageURL,
						"image_url":                map[string]string{"url": replyTargetImage},
					},
				},
			},
		},
	}

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	return requestBody
}

func newXAIFollowUpRequestBody(
	t *testing.T,
	followUpText string,
	followUpImage string,
) map[string]any {
	t.Helper()

	messages := []ChatMessage{
		{Role: searchtypes.MessageRoleSystem, Content: "You are concise."},
		{Role: searchtypes.MessageRoleUser, Content: "first question"},
		{Role: searchtypes.MessageRoleAssistant, Content: "first answer"},
	}

	followUpParts := []ContentPart{
		{searchtypes.MessageTypeKey: searchtypes.ContentTypeText, searchtypes.MessageTextKey: followUpText},
	}
	if followUpImage != "" {
		followUpParts = append(followUpParts, ContentPart{
			searchtypes.MessageTypeKey: searchtypes.ContentTypeImageURL,
			"image_url":                map[string]string{"url": followUpImage},
		})
	}

	messages = append(messages, ChatMessage{Role: searchtypes.MessageRoleUser, Content: followUpParts})

	// AssignXAIPreviousResponseID trims the conversation to the follow-up
	// turn when a previous response id anchors the chain; mirror that here.
	messages = messages[len(messages)-1:]

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "http://127.0.0.1:8787/v1",
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: true,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:              "grok-4",
		ConfiguredModel:    "x-ai/grok-4",
		SessionID:          "",
		PreviousResponseID: testXAIProviderResponseID,
		RequestID:          "",
		Messages:           messages,
	}

	requestBody, err := buildXAIResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	return requestBody
}

func newTestDiscordMessage(messageID string) *discordgo.Message {
	message := new(discordgo.Message)
	message.ID = messageID
	message.ChannelID = "channel-1"

	return message
}

type fakeChainStore struct {
	nodes map[string]NodeSnapshot
}

func newFakeChainStore(capacity int) *fakeChainStore {
	return &fakeChainStore{nodes: make(map[string]NodeSnapshot, capacity)}
}

func (store *fakeChainStore) Get(messageID string) (NodeSnapshot, bool) {
	node, ok := store.nodes[messageID]

	return node, ok
}

func (store *fakeChainStore) setConversationNode(
	messageID string,
	role string,
	providerResponseID string,
	providerResponseModel string,
	parentMessage *discordgo.Message,
) {
	store.nodes[messageID] = NodeSnapshot{
		Role:                  role,
		ProviderResponseID:    providerResponseID,
		ProviderResponseModel: providerResponseModel,
		ParentMessage:         parentMessage,
	}
}

func TestGrokVisionModels(t *testing.T) {
	t.Parallel()

	grokModels := []string{
		"x-ai/grok-4.5-auto",
		"x-ai/grok-4.5-fast",
		"x-ai/grok-4.5-expert",
		"x-ai/grok-4.5-heavy",
		"x-ai/grok-4.5-beta",
		"x-ai/grok-latest",
		"x-ai/grok 4.5 (beta)",
		"x-ai/grok-420-computer-use-sa",
	}

	for _, model := range grokModels {
		if !XAIConfiguredModel(model) {
			t.Errorf("expected XAIConfiguredModel(%q) to be true", model)
		}
	}
}

func TestGrokBridgeCustomProvider(t *testing.T) {
	t.Parallel()

	provider := ProviderRequestConfig{
		APIKind:         ProviderAPIKindOpenAI,
		BaseURL:         "http://127.0.0.1:8787/v1",
		APIKey:          "test-key",
		APIKeys:         nil,
		UseResponsesAPI: false,
		EnableGrounding: false,
		ExtraHeaders:    nil,
		ExtraQuery:      nil,
		ExtraBody:       nil,
	}

	if !ProviderUsesResponsesAPI("grok-bridge", provider) {
		t.Error("expected provider 'grok-bridge' to use Responses API")
	}

	if !XAIConfiguredModel("grok-bridge/grok-4.5-auto") {
		t.Error("expected xAIConfiguredModel('grok-bridge/grok-4.5-auto') to be true")
	}
}

func TestGrokReasoningEffortNormalization(t *testing.T) {
	t.Parallel()

	baseRequest := newXAIResponsesStreamingRequest("http://127.0.0.1:8787/v1")
	baseRequest.ConfiguredModel = "x-ai/grok-4.5-auto"
	baseRequest.Model = "grok-4.5-auto"
	baseRequest.Provider.ExtraBody = map[string]any{"reasoning_effort": "high"}
	baseRequest.Provider.UseResponsesAPI = true

	requestBody, err := buildXAIResponsesRequestBody(baseRequest)
	if err != nil {
		t.Fatalf("build xAI responses request body: %v", err)
	}

	reasoningConfig, ok := requestBody["reasoning"].(map[string]any)
	if !ok {
		t.Fatal("expected reasoning config in request body")
	}

	mode, hasMode := reasoningConfig["effort"].(string)
	if !hasMode {
		t.Fatal("expected reasoning effort in request body")
	}

	if mode != "high" {
		t.Fatalf("unexpected reasoning effort: %q", mode)
	}
}

func TestOpenAIResponsesCacheBreakpointMessagesUncomparableContent(t *testing.T) {
	t.Parallel()

	// Regression test: when the breakpoint message's Content is a slice
	// ([]map[string]any or []ContentPart), the breakpoint comparison used to
	// panic with "runtime error: comparing uncomparable type ...". Slices
	// that are already fully marked must pass through unchanged instead.
	messages := []ChatMessage{
		{Role: searchtypes.MessageRoleUser, Content: "older user"},
		{Role: searchtypes.MessageRoleAssistant, Content: "assistant turn"},
		{Role: searchtypes.MessageRoleUser, Content: []map[string]any{
			{"type": xAIResponsesInputTextType, "text": "latest user"},
			{
				"type": xAIResponsesInputTextType,
				"text": "second part",
				openAICacheBreakpointKey: map[string]any{
					openAICacheOptionsModeKey: openAICacheBreakpointModeExplicit,
				},
			},
		}},
	}

	assertSameContent := func(t *testing.T, before, after []ChatMessage) {
		t.Helper()

		if len(before) != len(after) {
			t.Fatalf(
				"expected post-breakpoint message count to match input: got %d, want %d",
				len(after),
				len(before),
			)
		}

		for index := range before {
			if after[index].Role != before[index].Role {
				t.Fatalf("message %d role changed: got %q, want %q", index, after[index].Role, before[index].Role)
			}

			if !reflect.DeepEqual(after[index].Content, before[index].Content) {
				t.Fatalf("message %d content changed: got %#v, want %#v", index, after[index].Content, before[index].Content)
			}
		}
	}

	// The stable-prefix breakpoint lands on the message after the last
	// assistant turn, i.e. the tail user slice message. Its last part is
	// already marked, so the conversion is a pass-through that must not
	// panic while comparing the uncomparable slice.
	normalized := openAIResponsesCacheBreakpointMessages(messages)
	assertSameContent(t, messages, normalized)
}

func TestOpenAIResponsesCacheBreakpointMessagesThreadsThroughMapSlice(t *testing.T) {
	t.Parallel()

	// An unmarked tail part gets marked once; the breakpoint is sitting on
	// the message after the last assistant turn, whose Content is an
	// uncomparable []map[string]any slice. The comparison must not panic.
	messages := []ChatMessage{
		{Role: searchtypes.MessageRoleUser, Content: "older user"},
		{Role: searchtypes.MessageRoleAssistant, Content: "assistant turn"},
		{Role: searchtypes.MessageRoleUser, Content: []map[string]any{
			{"type": xAIResponsesInputTextType, "text": "latest user"},
			{"type": xAIResponsesInputTextType, "text": "plain tail"},
		}},
	}

	normalized := openAIResponsesCacheBreakpointMessages(messages)

	content, ok := normalized[2].Content.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map[string]any content, got %T", normalized[2].Content)
	}

	lastPart := content[len(content)-1]
	if _, marked := lastPart[openAICacheBreakpointKey]; !marked {
		t.Fatal("expected last content part to be marked with a cache breakpoint")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func newTestHTTPClient(transport roundTripFunc) *http.Client {
	httpClient := new(http.Client)
	httpClient.Transport = transport

	return httpClient
}

const testXAIAuthHeader = "Bearer test-key"
