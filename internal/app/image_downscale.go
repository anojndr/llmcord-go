package app

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"strings"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// makeImageContentPart builds an image_url part, downscaling oversized
// sources once so every follow-up reuses the small payload instead of
// resending a phone-camera original.
func makeImageContentPart(mimeType string, imageBytes []byte) contentPart {
	scaledBytes, scaledMIMEType := downscaledImageBytes(imageBytes, mimeType)

	part := make(contentPart)
	part[messageTypeKey] = contentTypeImageURL
	part["image_url"] = map[string]string{
		messageURLKey: fmt.Sprintf(
			"data:%s;base64,%s",
			scaledMIMEType,
			base64.StdEncoding.EncodeToString(scaledBytes),
		),
	}

	return part
}

// downscaleContentPartImage shrinks a restored image_url part in place so
// history written before downscaling (huge base64 originals) pays the
// resize cost once on load instead of resending the original on every
// follow-up. Non-image parts, remote URLs, and undecodable payloads are
// left untouched.
func downscaleContentPartImage(part contentPart) {
	partType, _ := part[messageTypeKey].(string)
	if partType != contentTypeImageURL {
		return
	}

	imageURL, err := contentPartImageURL(part)
	if err != nil || !isBase64ImageDataURL(imageURL) {
		return
	}

	mimeType, imageBytes, ok := decodeBase64ImageDataURL(imageURL)
	if !ok {
		return
	}

	scaledBytes, scaledMIMEType := downscaledImageBytes(imageBytes, mimeType)
	if len(scaledBytes) == len(imageBytes) && scaledMIMEType == mimeType {
		return
	}

	part["image_url"] = map[string]string{
		messageURLKey: fmt.Sprintf(
			"data:%s;base64,%s",
			scaledMIMEType,
			base64.StdEncoding.EncodeToString(scaledBytes),
		),
	}
}

func isBase64ImageDataURL(imageURL string) bool {
	return strings.HasPrefix(imageURL, "data:image/") && strings.Contains(imageURL, ";base64,")
}

func decodeBase64ImageDataURL(imageURL string) (string, []byte, bool) {
	metadata, payload, found := strings.Cut(strings.TrimPrefix(imageURL, "data:"), ",")
	if !found || strings.TrimSpace(payload) == "" {
		return "", nil, false
	}

	mediaType, _, _ := strings.Cut(metadata, ";")
	if !strings.Contains(strings.ToLower(metadata), "base64") {
		return "", nil, false
	}

	mimeType := strings.TrimSpace(mediaType)
	if mimeType == "" {
		return "", nil, false
	}

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(decoded) == 0 {
		return "", nil, false
	}

	return mimeType, decoded, true
}

// downscaledImageBytes shrinks JPEG/PNG/WebP sources whose longest side
// exceeds attachmentImageMaxDimension, returning the (possibly re-encoded)
// bytes and effective MIME type. Small images, GIFs (animation), unknown
// types, and undecodable payloads return untouched so callers never lose
// data on the fast path.
func downscaledImageBytes(imageBytes []byte, mimeType string) ([]byte, string) {
	normalizedType := normalizedMIMEType(mimeType)

	var (
		decodeConfig func([]byte) (image.Config, error)
		decodeImage  func([]byte) (image.Image, error)
		encodeMIME   string
		encodeJPEG   bool
	)

	switch normalizedType {
	case mimeTypeJPEG:
		decodeConfig = jpegDecodeConfig
		decodeImage = jpegDecode
		encodeMIME = mimeTypeJPEG
		encodeJPEG = true
	case mimeTypePNG:
		decodeConfig = pngDecodeConfig
		decodeImage = pngDecode
		encodeMIME = mimeTypePNG
	case mimeTypeWEBP:
		decodeConfig = webpDecodeConfig
		decodeImage = webpDecode
		encodeMIME = mimeTypeJPEG
		encodeJPEG = true
	default:
		return imageBytes, mimeType
	}

	if len(imageBytes) == 0 || attachmentImageMaxDimension <= 0 {
		return imageBytes, mimeType
	}

	cfg, err := decodeConfig(imageBytes)
	if err != nil {
		return imageBytes, mimeType
	}

	if max(cfg.Width, cfg.Height) <= attachmentImageMaxDimension {
		return imageBytes, mimeType
	}

	src, err := decodeImage(imageBytes)
	if err != nil {
		return imageBytes, mimeType
	}

	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	if width <= 0 || height <= 0 || max(width, height) <= attachmentImageMaxDimension {
		return imageBytes, mimeType
	}

	newWidth, newHeight := scaledImageDimensions(width, height, attachmentImageMaxDimension)
	dst := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))

	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, bounds, draw.Src, nil)

	var encoded bytes.Buffer

	if encodeJPEG {
		if err := jpeg.Encode(&encoded, dst, &jpeg.Options{Quality: attachmentImageJPEGQuality}); err != nil {
			return imageBytes, mimeType
		}
	} else if err := png.Encode(&encoded, dst); err != nil {
		return imageBytes, mimeType
	}

	if encoded.Len() == 0 {
		return imageBytes, mimeType
	}

	// A noisy PNG can re-encode larger than the original; keep whichever
	// payload is smaller so the round trip never costs bytes.
	if encoded.Len() >= len(imageBytes) && strings.EqualFold(encodeMIME, normalizedType) {
		return imageBytes, mimeType
	}

	return encoded.Bytes(), encodeMIME
}

func scaledImageDimensions(width, height, maxDimension int) (int, int) {
	longest := max(width, height)
	if longest <= 0 || longest <= maxDimension {
		return width, height
	}

	return max(width*maxDimension/longest, 1), max(height*maxDimension/longest, 1)
}

func jpegDecodeConfig(imageBytes []byte) (image.Config, error) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(imageBytes))
	if err != nil {
		return image.Config{}, fmt.Errorf("decode jpeg image config: %w", err)
	}

	return cfg, nil
}

func jpegDecode(imageBytes []byte) (image.Image, error) {
	img, err := jpeg.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return nil, fmt.Errorf("decode jpeg image: %w", err)
	}

	return img, nil
}

func pngDecodeConfig(imageBytes []byte) (image.Config, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(imageBytes))
	if err != nil {
		return image.Config{}, fmt.Errorf("decode png image config: %w", err)
	}

	return cfg, nil
}

func pngDecode(imageBytes []byte) (image.Image, error) {
	img, err := png.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return nil, fmt.Errorf("decode png image: %w", err)
	}

	return img, nil
}

func webpDecodeConfig(imageBytes []byte) (image.Config, error) {
	cfg, err := webp.DecodeConfig(bytes.NewReader(imageBytes))
	if err != nil {
		return image.Config{}, fmt.Errorf("decode webp image config: %w", err)
	}

	return cfg, nil
}

func webpDecode(imageBytes []byte) (image.Image, error) {
	img, err := webp.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return nil, fmt.Errorf("decode webp image: %w", err)
	}

	return img, nil
}
