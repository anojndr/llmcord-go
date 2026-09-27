package providers

import (
	"strings"
	"testing"

	searchtypes "llmcord-go/internal/searchtypes"
)

func testVideoContentPart(mimeType string) ContentPart {
	return ContentPart{
		"type":                           searchtypes.ContentTypeVideoData,
		searchtypes.ContentFieldBytes:    []byte("video-bytes"),
		searchtypes.ContentFieldMIMEType: mimeType,
		searchtypes.ContentFieldFilename: "clip.mp4",
	}
}

func TestOpenAIVideoDataURL(t *testing.T) {
	t.Parallel()

	dataURL, ok := openAIVideoDataURL(testVideoContentPart("video/mp4"))
	if !ok {
		t.Fatal("expected video data URL")
	}

	const prefix = "data:video/mp4;base64,"
	if !strings.HasPrefix(dataURL, prefix) {
		t.Fatalf("unexpected video data URL: %q", dataURL)
	}

	if _, ok := openAIVideoDataURL(ContentPart{
		"type":                           searchtypes.ContentTypeVideoData,
		searchtypes.ContentFieldBytes:    []byte{},
		searchtypes.ContentFieldMIMEType: "video/mp4",
	}); ok {
		t.Fatal("expected empty video to be skipped")
	}
}

func TestOpenAIVideoURLPart(t *testing.T) {
	t.Parallel()

	part, ok := openAIVideoURLPart(testVideoContentPart("video/mp4"))
	if !ok {
		t.Fatal("expected video part")
	}

	if part["type"] != openAIVideoURLPartType {
		t.Fatalf("unexpected video part type: %#v", part)
	}

	videoURL, urlOK := part[openAIVideoURLPartType].(map[string]string)
	if !urlOK || !strings.HasPrefix(videoURL[searchtypes.MessageURLKey], "data:video/mp4;base64,") {
		t.Fatalf("unexpected video URL: %#v", part[openAIVideoURLPartType])
	}
	if part[mimoVideoFPSKey] != mimoVideoDefaultFPS || part[mimoVideoResolutionKey] != mimoVideoDefaultResolution {
		t.Fatalf("unexpected video params: %#v", part)
	}
}

func TestBuildChatCompletionRequestBodyConvertsVideoToVideoURL(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind: ProviderAPIKindOpenAI,
			BaseURL: "https://example.com/v1",
			APIKey:  "test-key",
		},
		Model:           "mimo-v2.6-flash",
		ConfiguredModel: "xiaomi/oc/mimo-v2.6-flash-free:vision",
		Messages: []ChatMessage{{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": "What is in this video?"},
				testVideoContentPart("video/mp4"),
			},
		}},
	}

	requestBody := buildChatCompletionRequestBody(request)

	messages, messagesOK := requestBody["messages"].([]ChatMessage)
	if !messagesOK || len(messages) != 1 {
		t.Fatalf("unexpected messages payload: %#v", requestBody["messages"])
	}

	parts, partsOK := messages[0].Content.([]ContentPart)
	if !partsOK || len(parts) != 2 {
		t.Fatalf("unexpected user content payload: %#v", messages[0].Content)
	}

	if parts[1]["type"] != openAIVideoURLPartType {
		t.Fatalf("unexpected video part type: %#v", parts[1])
	}
}

func TestBuildResponsesRequestBodyConvertsVideoToVideoURL(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: true,
		},
		Model:           "mimo-v2.6-flash",
		ConfiguredModel: "xiaomi/oc/mimo-v2.6-flash-free:vision",
		Messages: []ChatMessage{{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": "What is in this video?"},
				testVideoContentPart("video/mp4"),
			},
		}},
	}

	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build responses request body: %v", err)
	}

	input, inputOK := requestBody["input"].([]map[string]any)
	if !inputOK || len(input) != 1 {
		t.Fatalf("unexpected input payload: %#v", requestBody["input"])
	}

	content, contentOK := input[0]["content"].([]map[string]any)
	if !contentOK || len(content) != 2 {
		t.Fatalf("unexpected input content payload: %#v", input[0]["content"])
	}

	if content[1]["type"] != openAIVideoURLPartType {
		t.Fatalf("unexpected video part type: %#v", content[1])
	}

	videoURL, urlOK := content[1][openAIVideoURLPartType].(map[string]string)
	if !urlOK || !strings.HasPrefix(videoURL["url"], "data:video/mp4;base64,") {
		t.Fatalf("unexpected video URL: %#v", content[1][openAIVideoURLPartType])
	}
}

func TestResponsesUserPartDropsEmptyVideo(t *testing.T) {
	t.Parallel()

	part := ContentPart{
		"type":                           searchtypes.ContentTypeVideoData,
		searchtypes.ContentFieldBytes:    []byte{},
		searchtypes.ContentFieldMIMEType: "video/mp4",
	}

	convertedPart, ok, err := responsesUserPart(part)
	if err != nil {
		t.Fatalf("convert video part: %v", err)
	}

	if ok || convertedPart != nil {
		t.Fatalf("expected empty video to be skipped: %#v", convertedPart)
	}
}

func TestOpenAINormalizeContentPartConvertsVideo(t *testing.T) {
	t.Parallel()

	original := testVideoContentPart("video/mp4")

	normalizedPart, changed := openAINormalizeContentPart(original)
	if !changed {
		t.Fatal("expected video part to be converted")
	}

	if normalizedPart["type"] != openAIVideoURLPartType {
		t.Fatalf("unexpected video part type: %#v", normalizedPart)
	}

	if _, exists := original[openAIVideoURLPartType]; exists {
		t.Fatalf("expected original part to remain unchanged: %#v", original)
	}
}
