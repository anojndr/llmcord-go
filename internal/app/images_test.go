package app

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	providers "llmcord-go/internal/providers"
)

func imagePartDataURL(t *testing.T, part contentPart) string {
	t.Helper()

	imageURL, ok := part["image_url"].(map[string]string)
	if !ok {
		t.Fatalf("unexpected image part: %#v", part)
	}

	return imageURL["url"]
}

func testImagePayload(body []byte, contentType string) attachmentPayload {
	return attachmentPayload{
		attachment:  nil,
		body:        body,
		contentType: contentType,
	}
}

func decodedImageBytes(t *testing.T, dataURL string) []byte {
	t.Helper()

	imageData, err := providers.ParseBase64ImageDataURL(dataURL)
	if err != nil {
		t.Fatalf("parse image data URL: %v", err)
	}

	decoded, err := imageData.Decode()
	if err != nil {
		t.Fatalf("decode image data: %v", err)
	}

	return decoded
}

func TestAttachmentImagePreservesOriginalBytes(t *testing.T) {
	t.Parallel()

	original := []byte("original-image-bytes")

	part, ok := attachmentPayloadToContentPart(testImagePayload(original, mimeTypeJPEG))
	if !ok {
		t.Fatal("expected image part for jpeg payload")
	}

	dataURL := imagePartDataURL(t, part)
	if !strings.HasPrefix(dataURL, "data:image/jpeg;base64,") {
		t.Fatalf("unexpected image mime: %q", dataURL[:64])
	}

	if decoded := decodedImageBytes(t, dataURL); !bytes.Equal(decoded, original) {
		t.Fatalf("expected byte-identical passthrough: original %d, sent %d", len(original), len(decoded))
	}
}

func TestAttachmentImageLeavesSmallImageUntouched(t *testing.T) {
	t.Parallel()

	original := testJPEGBytes(t)

	part, ok := attachmentPayloadToContentPart(testImagePayload(original, mimeTypeJPEG))
	if !ok {
		t.Fatal("expected image part for small payload")
	}

	dataURL := imagePartDataURL(t, part)
	if decoded := decodedImageBytes(t, dataURL); !bytes.Equal(decoded, original) {
		t.Fatalf("expected small image passthrough: original %d, sent %d", len(original), len(decoded))
	}
}

func TestAttachmentImageLeavesGIFUntouched(t *testing.T) {
	t.Parallel()

	body := []byte("GIF89a-animated-payload")

	part, ok := attachmentPayloadToContentPart(testImagePayload(body, "image/gif"))
	if !ok {
		t.Fatal("expected image part for gif payload")
	}

	dataURL := imagePartDataURL(t, part)
	if !strings.HasPrefix(dataURL, "data:image/gif;base64,") {
		t.Fatalf("expected gif passthrough, got %q", dataURL[:64])
	}
}
func TestAttachmentImageRejectsOversizedPayload(t *testing.T) {
	t.Parallel()

	oversized := make([]byte, maxForwardedImageBytes+1)
	if _, ok := attachmentPayloadToContentPart(testImagePayload(oversized, mimeTypeJPEG)); ok {
		t.Fatal("expected oversized image to be rejected")
	}
}

func TestAttachmentImageRejectsUnsupportedMIME(t *testing.T) {
	t.Parallel()

	for _, mimeType := range []string{
		"image/heic", "image/avif", "image/svg+xml", "image/tiff", "image/bmp", "image/jpx",
	} {
		if _, ok := attachmentPayloadToContentPart(testImagePayload([]byte("image-bytes"), mimeType)); ok {
			t.Fatalf("expected unsupported mime %q to be rejected", mimeType)
		}
	}
}
func TestRejectedImagePayloadCountFlagsGuardedImages(t *testing.T) {
	t.Parallel()

	oversized := make([]byte, maxForwardedImageBytes+1)
	payloads := []attachmentPayload{
		testImagePayload([]byte("ok-bytes"), mimeTypeJPEG),
		testImagePayload(oversized, mimeTypeJPEG),
		testImagePayload([]byte("heic-bytes"), "image/heic"),
	}

	if got := rejectedImagePayloadCount(payloads); got != 2 {
		t.Fatalf("expected 2 rejected images, got %d", got)
	}
}

func TestRestoredHistoryImageKeepsOriginalBytes(t *testing.T) {
	t.Parallel()

	original := []byte("restored-original-image-bytes")
	rawPart := make(contentPart)
	rawPart[messageTypeKey] = contentTypeImageURL
	rawPart["image_url"] = map[string]string{
		messageURLKey: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(original),
	}

	snapshot, snapshotOK := contentPartSnapshotFromPart(rawPart)
	if !snapshotOK {
		t.Fatal("expected snapshot for image part")
	}

	restored, restoredOK := snapshot.contentPart()
	if !restoredOK {
		t.Fatal("expected restored image part")
	}

	restoredDataURL := imagePartDataURL(t, restored)
	if decoded := decodedImageBytes(t, restoredDataURL); !bytes.Equal(decoded, original) {
		t.Fatalf("expected restored image passthrough: original %d, sent %d", len(original), len(decoded))
	}
}
