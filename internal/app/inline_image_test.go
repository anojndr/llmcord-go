package app

import (
	"bytes"
	"encoding/base64"
	"io"
	providers "llmcord-go/internal/providers"
	searchtypes "llmcord-go/internal/searchtypes"
	"strings"
	"testing"
)

const inlineImageByteLimit = 4 * 1024 * 1024

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

func BenchmarkStreamLargeBase64Image(b *testing.B) {
	imageBytes := bytes.Repeat([]byte("x"), inlineImageByteLimit+1)
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
