package app

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	providers "llmcord-go/internal/providers"
	searchtypes "llmcord-go/internal/searchtypes"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

func TestParseBase64ImageDataURL(t *testing.T) {
	t.Parallel()

	imageData, err := providers.ParseBase64ImageDataURL("data:image/png;charset=utf-8;base64,aGVsbG8=")
	if err != nil {
		t.Fatalf("parse base64 image data URL: %v", err)
	}

	if imageData.MimeType != searchtypes.MimeTypePNG {
		t.Fatalf("unexpected image MIME type: %q", imageData.MimeType)
	}

	if imageData.DecodedLengthEstimate() != len("hello") {
		t.Fatalf("unexpected decoded length estimate: %d", imageData.DecodedLengthEstimate())
	}

	decoded, err := io.ReadAll(imageData.Decoder())
	if err != nil {
		t.Fatalf("stream decoded image: %v", err)
	}

	if string(decoded) != "hello" {
		t.Fatalf("unexpected decoded image: %q", decoded)
	}
}

func TestXAIFileUploadPayloadStreamsMultipartBody(t *testing.T) {
	t.Parallel()

	imageBytes := []byte("streamed-image-bytes")

	requestBody, contentType, contentLength, err := providers.XAIFileUploadPayload(
		bytes.NewReader(imageBytes),
		len(imageBytes),
		searchtypes.MimeTypePNG,
	)
	if err != nil {
		t.Fatalf("build streaming xAI file upload payload: %v", err)
	}

	httpRequest, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"https://example.com/files",
		requestBody,
	)
	if err != nil {
		t.Fatalf("build xAI upload request: %v", err)
	}

	httpRequest.Header.Set("Content-Type", contentType)
	httpRequest.ContentLength = contentLength

	multipartReader, err := httpRequest.MultipartReader()
	if err != nil {
		t.Fatalf("build multipart reader: %v", err)
	}

	assertStreamingXAIUploadParts(t, multipartReader, imageBytes)
}

func assertStreamingXAIUploadParts(
	t *testing.T,
	multipartReader *multipart.Reader,
	expectedImage []byte,
) {
	t.Helper()

	purposePart, err := multipartReader.NextPart()
	if err != nil {
		t.Fatalf("read xAI purpose part: %v", err)
	}

	purpose, err := io.ReadAll(purposePart)
	if err != nil {
		t.Fatalf("read xAI purpose: %v", err)
	}

	if string(purpose) != providers.XAIResponsesUploadPurposeUserData {
		t.Fatalf("unexpected xAI upload purpose: %q", purpose)
	}

	filePart, err := multipartReader.NextPart()
	if err != nil {
		t.Fatalf("read xAI file part: %v", err)
	}

	actualImage, err := io.ReadAll(filePart)
	if err != nil {
		t.Fatalf("read streamed xAI image: %v", err)
	}

	if !bytes.Equal(actualImage, expectedImage) {
		t.Fatal("unexpected streamed xAI image bytes")
	}

	_, err = multipartReader.NextPart()

	if !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected trailing xAI upload part: %v", err)
	}
}

func BenchmarkStreamLargeBase64Image(b *testing.B) {
	imageBytes := bytes.Repeat([]byte("x"), providers.XAIInlineImageByteLimit+1)
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes)

	imageData, err := providers.ParseBase64ImageDataURL(imageURL)
	if err != nil {
		b.Fatalf("parse benchmark image: %v", err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(imageBytes)))
	b.ResetTimer()

	for range b.N {
		written, copyErr := io.Copy(io.Discard, imageData.Decoder())
		if copyErr != nil {
			b.Fatalf("stream benchmark image: %v", copyErr)
		}

		if written != int64(len(imageBytes)) {
			b.Fatalf("unexpected streamed byte count: %d", written)
		}
	}
}

func BenchmarkBuildStreamingXAIFileUploadPayload(b *testing.B) {
	imageBytes := bytes.Repeat([]byte("x"), providers.XAIInlineImageByteLimit+1)
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes)

	imageData, err := providers.ParseBase64ImageDataURL(imageURL)
	if err != nil {
		b.Fatalf("parse benchmark image: %v", err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(imageBytes)))
	b.ResetTimer()

	for range b.N {
		requestBody, _, contentLength, payloadErr := providers.XAIFileUploadPayload(
			imageData.Decoder(),
			imageData.DecodedLengthEstimate(),
			imageData.MimeType,
		)
		if payloadErr != nil {
			b.Fatalf("build benchmark xAI upload payload: %v", payloadErr)
		}

		written, copyErr := io.Copy(io.Discard, requestBody)
		if copyErr != nil {
			b.Fatalf("consume benchmark xAI upload payload: %v", copyErr)
		}

		if written != contentLength {
			b.Fatalf("unexpected multipart byte count: got %d want %d", written, contentLength)
		}
	}
}

func BenchmarkParseBase64ImageDataURL(b *testing.B) {
	imageURL := "data:image/png;charset=utf-8;base64," + strings.Repeat("YWFh", 256)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_, err := providers.ParseBase64ImageDataURL(imageURL)
		if err != nil {
			b.Fatalf("parse benchmark image: %v", err)
		}
	}
}
