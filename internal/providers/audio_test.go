package providers

import (
	"encoding/base64"
	"strings"
	"testing"

	searchtypes "llmcord-go/internal/searchtypes"
)

func TestOpenAIAudioInputFormat(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		mimeType string
		want     string
	}{
		{name: "wav", mimeType: "audio/wav", want: "wav"},
		{name: "wave alias", mimeType: "audio/wave", want: "wav"},
		{name: "x-wav alias", mimeType: "audio/x-wav", want: "wav"},
		{name: "mpeg", mimeType: "audio/mpeg", want: "mp3"},
		{name: "mp3 alias", mimeType: "audio/mp3", want: "mp3"},
		{name: "ogg passthrough", mimeType: "audio/ogg", want: "ogg"},
		{name: "parameters stripped", mimeType: "audio/wav; codecs=1", want: "wav"},
		{name: "uppercase", mimeType: "Audio/MPEG", want: "mp3"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := openAIAudioInputFormat(testCase.mimeType); got != testCase.want {
				t.Fatalf("openAIAudioInputFormat(%q) = %q, want %q", testCase.mimeType, got, testCase.want)
			}
		})
	}
}

func testAudioContentPart(mimeType string) ContentPart {
	return ContentPart{
		"type":                           searchtypes.ContentTypeAudioData,
		searchtypes.ContentFieldBytes:    []byte("audio-bytes"),
		searchtypes.ContentFieldMIMEType: mimeType,
		searchtypes.ContentFieldFilename: "clip.mp3",
	}
}

func assertAudioInputPayload(t *testing.T, payload map[string]string, mimeType string) {
	t.Helper()

	wantData := base64.StdEncoding.EncodeToString([]byte("audio-bytes"))
	if payload["data"] != wantData {
		t.Fatalf("unexpected audio data: %q", payload["data"])
	}

	if payload["format"] != openAIAudioInputFormat(mimeType) {
		t.Fatalf("unexpected audio format: %q", payload["format"])
	}
}

func TestBuildChatCompletionRequestBodyConvertsAudioToInputAudio(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind: ProviderAPIKindOpenAI,
			BaseURL: "https://example.com/v1",
			APIKey:  "test-key",
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		Messages: []ChatMessage{{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": "What is in this recording?"},
				testAudioContentPart("audio/mpeg"),
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

	if parts[1]["type"] != openAIAudioInputPartType {
		t.Fatalf("unexpected audio part type: %#v", parts[1])
	}

	payload, payloadOK := parts[1][openAIAudioInputPartType].(map[string]string)
	if !payloadOK {
		t.Fatalf("unexpected audio payload: %#v", parts[1][openAIAudioInputPartType])
	}

	assertAudioInputPayload(t, payload, "audio/mpeg")
}

func TestBuildChatCompletionRequestBodyKeepsAudioOnlyPlaceholder(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind: ProviderAPIKindOpenAI,
			BaseURL: "https://example.com/v1",
			APIKey:  "test-key",
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		Messages: []ChatMessage{{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": ""},
				testAudioContentPart("audio/wav"),
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

	if parts[0]["type"] != searchtypes.ContentTypeText || parts[0]["text"] != FileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected placeholder text part: %#v", parts[0])
	}

	if parts[1]["type"] != openAIAudioInputPartType {
		t.Fatalf("unexpected audio part type: %#v", parts[1])
	}
}

func TestBuildResponsesRequestBodyConvertsAudioToInputAudio(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			BaseURL:         "https://example.com/v1",
			APIKey:          "test-key",
			UseResponsesAPI: true,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		Messages: []ChatMessage{{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{"type": searchtypes.ContentTypeText, "text": "What is in this recording?"},
				testAudioContentPart("audio/ogg"),
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

	if content[1]["type"] != openAIAudioInputPartType {
		t.Fatalf("unexpected audio part type: %#v", content[1])
	}

	payload, payloadOK := content[1][openAIAudioInputPartType].(map[string]string)
	if !payloadOK {
		t.Fatalf("unexpected audio payload: %#v", content[1][openAIAudioInputPartType])
	}

	assertAudioInputPayload(t, payload, "audio/ogg")
}

func TestResponsesUserPartDropsEmptyAudio(t *testing.T) {
	t.Parallel()

	part := ContentPart{
		"type":                           searchtypes.ContentTypeAudioData,
		searchtypes.ContentFieldBytes:    []byte{},
		searchtypes.ContentFieldMIMEType: "audio/mpeg",
	}

	convertedPart, ok, err := responsesUserPart(part)
	if err != nil {
		t.Fatalf("convert audio part: %v", err)
	}

	if ok || convertedPart != nil {
		t.Fatalf("expected empty audio to be skipped: %#v", convertedPart)
	}
}

func TestOpenAINormalizeContentPartConvertsAudio(t *testing.T) {
	t.Parallel()

	original := testAudioContentPart("audio/wav")

	normalizedPart, changed := openAINormalizeContentPart(original)
	if !changed {
		t.Fatal("expected audio part to be converted")
	}

	if normalizedPart["type"] != openAIAudioInputPartType {
		t.Fatalf("unexpected audio part type: %#v", normalizedPart)
	}

	payload, payloadOK := normalizedPart[openAIAudioInputPartType].(map[string]string)
	if !payloadOK {
		t.Fatalf("unexpected audio payload: %#v", normalizedPart[openAIAudioInputPartType])
	}

	assertAudioInputPayload(t, payload, "audio/wav")

	if _, exists := original[openAIAudioInputPartType]; exists {
		t.Fatalf("expected original part to remain unchanged: %#v", original)
	}
}

func TestContentPartsNeedPlaceholderForAudioOnly(t *testing.T) {
	t.Parallel()

	parts := []ContentPart{
		{"type": searchtypes.ContentTypeText, "text": ""},
		testAudioContentPart("audio/mpeg"),
	}

	if !contentPartsNeedFileOrImageOnlyQueryPlaceholder(parts) {
		t.Fatalf("expected audio-only content to need placeholder: %#v", parts)
	}

	normalized, changed := messageContentWithFileOrImageOnlyQueryPlaceholder(searchtypes.MessageRoleUser, parts)
	if !changed {
		t.Fatal("expected placeholder to be added")
	}

	normalizedParts, partsOK := normalized.([]ContentPart)
	if !partsOK || len(normalizedParts) != 2 {
		t.Fatalf("unexpected normalized content: %#v", normalized)
	}

	if normalizedParts[0]["text"] != FileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected placeholder text: %#v", normalizedParts[0])
	}

	partType, partTypeOK := normalizedParts[1]["type"].(string)
	if !partTypeOK ||
		(!strings.EqualFold(partType, openAIAudioInputPartType) && partType != searchtypes.ContentTypeAudioData) {
		t.Fatalf("unexpected audio part: %#v", normalizedParts[1])
	}
}
