package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	searchtypes "llmcord-go/internal/searchtypes"
)

func TestIsPayloadTooLargeErrorMatches413StatusError(t *testing.T) {
	t.Parallel()

	payloadError := NewOpenAIProviderStatusError(
		"responses request failed",
		http.StatusRequestEntityTooLarge,
		"413 Request Entity Too Large",
		nil,
		[]byte(`{"error":{"message":"FUNCTION_PAYLOAD_TOO_LARGE","type":"invalid_request_error"}}`),
		false,
	)
	if !IsPayloadTooLargeError(payloadError) {
		t.Fatalf("expected 413 status error to classify as payload too large: %v", payloadError)
	}

	if IsPayloadTooLargeError(nil) {
		t.Fatal("nil error must not classify as payload too large")
	}

	otherError := NewOpenAIProviderStatusError(
		"responses request failed",
		http.StatusBadRequest,
		"400 Bad Request",
		nil,
		[]byte(`{"error":{"message":"bad request","type":"invalid_request_error"}}`),
		false,
	)
	if IsPayloadTooLargeError(otherError) {
		t.Fatalf("400 error must not classify as payload too large: %v", otherError)
	}
}

func TestNearLosslessImageBytesShrinksNoisyPNGWithoutGrowing(t *testing.T) {
	t.Parallel()

	original := noisyPNGBytes(t, 512, 512)

	compressed, mimeType, ok := nearLosslessImageBytes(original)
	if !ok {
		t.Fatal("expected noisy PNG to compress")
	}

	if len(compressed) >= len(original) {
		t.Fatalf("expected smaller payload: original %d, compressed %d", len(original), len(compressed))
	}

	decoded, _, err := image.Decode(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("decode compressed image: %v", err)
	}

	bounds := decoded.Bounds()
	if bounds.Dx() != 512 || bounds.Dy() != 512 {
		t.Fatalf("expected dimensions preserved, got %dx%d", bounds.Dx(), bounds.Dy())
	}

	if mimeType != searchtypes.MimeTypePNG && mimeType != searchtypes.MimeTypeJPEG {
		t.Fatalf("unexpected compressed mime type %q", mimeType)
	}
}

func TestNearLosslessImageBytesKeepsQualityOnDownscale(t *testing.T) {
	t.Parallel()

	original := noisyPNGBytes(t, 2600, 1300)

	compressed, _, ok := nearLosslessImageBytes(original)
	if !ok {
		t.Fatal("expected oversized image to compress")
	}

	decoded, _, err := image.Decode(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("decode compressed image: %v", err)
	}

	bounds := decoded.Bounds()
	if got := max(bounds.Dx(), bounds.Dy()); got != payloadTooLargeImageMaxDimension {
		t.Fatalf("expected longest side %d, got %dx%d", payloadTooLargeImageMaxDimension, bounds.Dx(), bounds.Dy())
	}
}

func TestNearLosslessImageBytesSkipsAnimatedImages(t *testing.T) {
	t.Parallel()

	gifPayload := []byte("GIF89a-animated-payload")

	if _, _, ok := nearLosslessImageBytes(gifPayload); ok {
		t.Fatal("animated GIF must keep its original bytes")
	}

	if !isAnimatedImage(gifPayload) {
		t.Fatal("expected GIF prefix to detect animation")
	}
}

func TestCompressedRequestWithSmallerImagesReplacesBase64Images(t *testing.T) {
	t.Parallel()

	original := noisyPNGBytes(t, 256, 256)
	originalURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(original)

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			API:             "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Messages: []ChatMessage{
			{Role: searchtypes.MessageRoleUser, Content: "hello"},
			{
				Role: searchtypes.MessageRoleUser,
				Content: []ContentPart{
					{searchtypes.MessageTypeKey: searchtypes.ContentTypeText, searchtypes.MessageTextKey: "look"},
					{
						searchtypes.MessageTypeKey: searchtypes.ContentTypeImageURL,
						"image_url":                map[string]string{searchtypes.MessageURLKey: originalURL},
					},
				},
			},
		},
		Tools:                 nil,
		ToolChoice:            "",
		ToolRounds:            nil,
		PreviousResponseID:    "",
		PreviousResponseCount: 0,
	}

	compressedRequest, summary, ok := compressedRequestWithSmallerImages(request)
	if !ok {
		t.Fatal("expected compressible image to rewrite the request")
	}

	if summary.images != 1 || summary.originalBytes != len(original) {
		t.Fatalf("unexpected compression summary: %#v", summary)
	}

	if summary.compressedBytes >= summary.originalBytes {
		t.Fatalf("expected byte savings: %#v", summary)
	}

	parts, partsOK := compressedRequest.Messages[1].Content.([]ContentPart)
	if !partsOK || len(parts) != 2 {
		t.Fatalf("unexpected compressed content: %#v", compressedRequest.Messages[1].Content)
	}

	compressedURL, urlOK := parts[1]["image_url"].(map[string]string)
	if !urlOK || !strings.HasPrefix(compressedURL[searchtypes.MessageURLKey], "data:") {
		t.Fatalf("unexpected compressed image URL: %#v", parts[1]["image_url"])
	}

	if strings.TrimSpace(compressedURL[searchtypes.MessageURLKey]) == originalURL {
		t.Fatal("expected image payload to change")
	}

	if originalParts, _ := request.Messages[1].Content.([]ContentPart); originalParts != nil {
		originalImageURL, _ := originalParts[1]["image_url"].(map[string]string)
		if originalImageURL[searchtypes.MessageURLKey] != originalURL {
			t.Fatal("original request must stay unchanged")
		}
	}
}

func TestCompressedRequestWithSmallerImagesLeavesRemoteURLs(t *testing.T) {
	t.Parallel()

	request := ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			API:             "",
			BaseURL:         "",
			APIKey:          "",
			APIKeys:         nil,
			UseResponsesAPI: false,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Messages: []ChatMessage{{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{{
				searchtypes.MessageTypeKey: searchtypes.ContentTypeImageURL,
				"image_url":                map[string]string{searchtypes.MessageURLKey: "https://example.com/img.png"},
			}},
		}},
		Tools:                 nil,
		ToolChoice:            "",
		ToolRounds:            nil,
		PreviousResponseID:    "",
		PreviousResponseCount: 0,
	}

	if _, _, ok := compressedRequestWithSmallerImages(request); ok {
		t.Fatal("remote image URLs carry no payload weight and must not trigger a retry")
	}
}

func TestStreamResponsesRetriesPayloadTooLargeWithCompressedImages(t *testing.T) {
	t.Parallel()

	original := noisyPNGBytes(t, 512, 512)
	originalURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(original)

	var attempts atomic.Int64

	var secondInputImageURL string

	server := newPayloadTooLargeThenStreamResponsesServer(t, &attempts, &secondInputImageURL)
	defer server.Close()

	request := newPayloadTooLargeRetryRequest(server.URL+"/v1", true, originalURL)

	var joinedContent strings.Builder

	err := newOpenAIClient(server.Client()).streamChatCompletion(
		context.Background(),
		request,
		func(delta StreamDelta) error {
			joinedContent.WriteString(delta.Content)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream with compressed retry: %v", err)
	}

	if attempts.Load() != 2 {
		t.Fatalf("expected one compressed retry, got %d attempts", attempts.Load())
	}

	if joinedContent.String() != "Hi" {
		t.Fatalf("unexpected retried content: %q", joinedContent.String())
	}

	if !strings.HasPrefix(secondInputImageURL, "data:") || secondInputImageURL == originalURL {
		t.Fatalf("expected retried request to carry a recompressed image, got %d chars", len(secondInputImageURL))
	}

	originalPayloadLen := len(base64.StdEncoding.EncodeToString(original))
	if len(secondInputImageURL) >= len(originalURL) ||
		len(secondInputImageURL) >= len("data:image/png;base64,")+originalPayloadLen {
		t.Fatalf("expected smaller retried payload: original %d, retried %d", len(originalURL), len(secondInputImageURL))
	}
}

func TestStreamChatCompletionsRetriesPayloadTooLargeWithCompressedImages(t *testing.T) {
	t.Parallel()

	original := noisyPNGBytes(t, 256, 256)
	originalURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(original)

	var attempts atomic.Int64

	server := newPayloadTooLargeThenStreamChatServer(t, &attempts)
	defer server.Close()

	request := newPayloadTooLargeRetryRequest(server.URL, false, originalURL)

	var joinedContent strings.Builder

	err := newOpenAIClient(server.Client()).streamChatCompletion(
		context.Background(),
		request,
		func(delta StreamDelta) error {
			joinedContent.WriteString(delta.Content)

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream with compressed retry: %v", err)
	}

	if attempts.Load() != 2 {
		t.Fatalf("expected one compressed retry, got %d attempts", attempts.Load())
	}

	if joinedContent.String() != "Hi" {
		t.Fatalf("unexpected retried content: %q", joinedContent.String())
	}
}

func responsesFirstInputImageURL(t *testing.T, httpRequest *http.Request) string {
	t.Helper()

	body, readErr := io.ReadAll(httpRequest.Body)
	if readErr != nil {
		t.Fatalf("read retried request body: %v", readErr)
	}

	var payload struct {
		Input []struct {
			Content []struct {
				Type     string `json:"type"`
				ImageURL string `json:"image_url"`
			} `json:"content"`
		} `json:"input"`
	}

	unmarshalErr := json.Unmarshal(body, &payload)
	if unmarshalErr != nil {
		t.Fatalf("decode retried request body: %v", unmarshalErr)
	}

	for _, item := range payload.Input {
		for _, part := range item.Content {
			if part.Type == responsesInputImageType && strings.HasPrefix(part.ImageURL, "data:") {
				return part.ImageURL
			}
		}
	}

	t.Fatal("retried request carried no image URL")

	return ""
}

func noisyPNGBytes(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{
				R: byte((x * 31) % 256),
				G: byte((y * 57) % 256),
				B: byte(((x + y) * 13) % 256),
				A: 0xff,
			})
		}
	}

	var buffer bytes.Buffer

	encoder := png.Encoder{CompressionLevel: png.NoCompression, BufferPool: nil}

	encodeErr := encoder.Encode(&buffer, img)
	if encodeErr != nil {
		t.Fatalf("encode noisy png: %v", encodeErr)
	}

	return buffer.Bytes()
}

func newPayloadTooLargeThenStreamResponsesServer(
	t *testing.T,
	attempts *atomic.Int64,
	secondInputImageURL *string,
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		httpRequest *http.Request,
	) {
		attempt := attempts.Add(1)

		if attempt == 1 {
			responseWriter.Header().Set("Content-Type", "application/json")
			responseWriter.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = responseWriter.Write([]byte(
				`{"error":{"message":"FUNCTION_PAYLOAD_TOO_LARGE","type":"invalid_request_error"}}`,
			))

			return
		}

		*secondInputImageURL = responsesFirstInputImageURL(t, httpRequest)

		responseWriter.Header().Set("Content-Type", "text/event-stream")
		_, _ = responseWriter.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n"))
		_, _ = responseWriter.Write([]byte(openAIResponsesCompletedChunk()))
		_, _ = responseWriter.Write([]byte("data: [DONE]\n\n"))
	}))
}

func newPayloadTooLargeRetryRequest(baseURL string, useResponsesAPI bool, originalURL string) ChatCompletionRequest {
	return ChatCompletionRequest{
		Provider: ProviderRequestConfig{
			APIKind:         ProviderAPIKindOpenAI,
			API:             "",
			BaseURL:         baseURL,
			APIKey:          "test-key",
			APIKeys:         nil,
			UseResponsesAPI: useResponsesAPI,
			EnableGrounding: false,
			ExtraHeaders:    nil,
			ExtraQuery:      nil,
			ExtraBody:       nil,
		},
		Model:           "gpt-test",
		ConfiguredModel: "",
		SessionID:       "",
		RequestID:       "",
		Messages: []ChatMessage{{
			Role: searchtypes.MessageRoleUser,
			Content: []ContentPart{
				{searchtypes.MessageTypeKey: searchtypes.ContentTypeText, searchtypes.MessageTextKey: "look"},
				{
					searchtypes.MessageTypeKey: searchtypes.ContentTypeImageURL,
					"image_url":                map[string]string{searchtypes.MessageURLKey: originalURL},
				},
			},
		}},
		Tools:                 nil,
		ToolChoice:            "",
		ToolRounds:            nil,
		PreviousResponseID:    "",
		PreviousResponseCount: 0,
	}
}

func newPayloadTooLargeThenStreamChatServer(t *testing.T, attempts *atomic.Int64) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		attempt := attempts.Add(1)

		if attempt == 1 {
			responseWriter.Header().Set("Content-Type", "application/json")
			responseWriter.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = responseWriter.Write([]byte(
				`{"error":{"message":"FUNCTION_PAYLOAD_TOO_LARGE","type":"invalid_request_error"}}`,
			))

			return
		}

		responseWriter.Header().Set("Content-Type", "text/event-stream")
		_, _ = responseWriter.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"))
		_, _ = responseWriter.Write([]byte("data: [DONE]\n\n"))
	}))
}
