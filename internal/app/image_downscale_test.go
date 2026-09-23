package app

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	providers "llmcord-go/internal/providers"
)

func encodeLargeTestJPEG(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}

	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode large test jpeg: %v", err)
	}

	return buffer.Bytes()
}

func encodeLargeTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 64, A: 255})
		}
	}

	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatalf("encode large test png: %v", err)
	}

	return buffer.Bytes()
}

func decodedImageDimensions(t *testing.T, dataURL string) (int, int) {
	t.Helper()

	imageData, err := providers.ParseBase64ImageDataURL(dataURL)
	if err != nil {
		t.Fatalf("parse downscaled image data URL: %v", err)
	}

	decoded, err := imageData.Decode()
	if err != nil {
		t.Fatalf("decode downscaled image: %v", err)
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil {
		t.Fatalf("decode downscaled image config: %v", err)
	}

	return cfg.Width, cfg.Height
}

func imagePartDataURL(t *testing.T, part contentPart) string {
	t.Helper()

	imageURL, ok := part["image_url"].(map[string]string)
	if !ok {
		t.Fatalf("unexpected image part: %#v", part)
	}

	return imageURL["url"]
}

func TestAttachmentImageDownscalesLargeJPEG(t *testing.T) {
	t.Parallel()

	original := encodeLargeTestJPEG(t, 3000, 2000)
	part, ok := attachmentPayloadToContentPart(attachmentPayload{body: original, contentType: mimeTypeJPEG})
	if !ok {
		t.Fatal("expected image part for jpeg payload")
	}

	dataURL := imagePartDataURL(t, part)
	if !strings.HasPrefix(dataURL, "data:image/jpeg;base64,") {
		t.Fatalf("unexpected downscaled mime: %q", dataURL[:64])
	}

	width, height := decodedImageDimensions(t, dataURL)
	if got := max(width, height); got != attachmentImageMaxDimension {
		t.Fatalf("expected longest side %d, got %dx%d", attachmentImageMaxDimension, width, height)
	}

	imageData, err := providers.ParseBase64ImageDataURL(dataURL)
	if err != nil {
		t.Fatalf("parse downscaled URL: %v", err)
	}

	decoded, err := imageData.Decode()
	if err != nil {
		t.Fatalf("decode downscaled image: %v", err)
	}

	if len(decoded) >= len(original) {
		t.Fatalf("expected smaller payload: original %d, downscaled %d", len(original), len(decoded))
	}
}

func TestAttachmentImageLeavesSmallImageUntouched(t *testing.T) {
	t.Parallel()

	original := testJPEGBytes(t)
	part, ok := attachmentPayloadToContentPart(attachmentPayload{body: original, contentType: mimeTypeJPEG})
	if !ok {
		t.Fatal("expected image part for small payload")
	}

	dataURL := imagePartDataURL(t, part)
	width, height := decodedImageDimensions(t, dataURL)

	if width != 1 || height != 1 {
		t.Fatalf("expected 1x1 passthrough, got %dx%d", width, height)
	}
}

func TestAttachmentImageLeavesGIFUntouched(t *testing.T) {
	t.Parallel()

	body := []byte("GIF89a-animated-payload")
	part, ok := attachmentPayloadToContentPart(attachmentPayload{body: body, contentType: "image/gif"})
	if !ok {
		t.Fatal("expected image part for gif payload")
	}

	dataURL := imagePartDataURL(t, part)
	if !strings.HasPrefix(dataURL, "data:image/gif;base64,") {
		t.Fatalf("expected gif passthrough, got %q", dataURL[:64])
	}
}

func TestRestoredHistoryImageDownscalesOnce(t *testing.T) {
	t.Parallel()

	original := encodeLargeTestJPEG(t, 3000, 2000)

	// Simulate a pre-fix snapshot holding the unscaled original.
	rawPart := make(contentPart)
	rawPart[messageTypeKey] = contentTypeImageURL
	rawPart["image_url"] = map[string]string{
		messageURLKey: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(original),
	}

	downscaleContentPartImage(rawPart)

	width, height := decodedImageDimensions(t, imagePartDataURL(t, rawPart))
	if got := max(width, height); got != attachmentImageMaxDimension {
		t.Fatalf("expected restored image capped at %d, got %dx%d", attachmentImageMaxDimension, width, height)
	}
}
func TestDownscaledPNGStaysWithinBudget(t *testing.T) {
	t.Parallel()

	original := encodeLargeTestPNG(t, 2400, 1600)
	scaledBytes, scaledMIME := downscaledImageBytes(original, mimeTypePNG)

	if scaledMIME != mimeTypePNG {
		t.Fatalf("expected png output, got %q", scaledMIME)
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(scaledBytes))
	if err != nil {
		t.Fatalf("decode downscaled png config: %v", err)
	}

	if got := max(cfg.Width, cfg.Height); got != attachmentImageMaxDimension {
		t.Fatalf("expected longest side %d, got %dx%d", attachmentImageMaxDimension, cfg.Width, cfg.Height)
	}
}
