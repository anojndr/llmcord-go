package providers

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	// Register the GIF decoder so animated input is recognized before the
	// compression helpers decide to leave it untouched.
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"maps"
	"net/http"
	"strings"

	"golang.org/x/image/draw"

	searchtypes "llmcord-go/internal/searchtypes"
)

const (
	// payloadTooLargeImageMaxDimension caps the longest image side on the
	// 413 compressed retry. High-detail vision downscales past 2048
	// server-side, so this removes only bytes the model never sees.
	payloadTooLargeImageMaxDimension = 2048
	// payloadTooLargeImageJPEGQuality is the JPEG quality for the 413
	// compressed retry: visually transparent yet far smaller than the PNG
	// screenshots and high-quality JPEGs Discord delivers.
	payloadTooLargeImageJPEGQuality = 92
	// downscaleImageRoundHalfUp rounds scaled dimensions to whole pixels.
	downscaleImageRoundHalfUp = 0.5
)

// IsPayloadTooLargeError reports whether err is an HTTP 413 Content Too
// Large failure (e.g. a proxy FUNCTION_PAYLOAD_TOO_LARGE rejection of an
// image-bearing request). Callers use it to decide on a smaller retry.
func IsPayloadTooLargeError(err error) bool {
	if err == nil {
		return false
	}

	var statusErr StatusError
	if !errors.As(err, &statusErr) {
		return false
	}

	return statusErr.StatusCode == http.StatusRequestEntityTooLarge
}

// compressedImagesSummary totals the byte savings of one compressed retry.
type compressedImagesSummary struct {
	images          int
	originalBytes   int
	compressedBytes int
}

// compressedRequestWithSmallerImages clones request with every embedded
// base64 image replaced by its near-lossless compressed form (see
// nearLosslessImageBytes). Remote URLs carry no payload weight and pass
// through untouched. ok=false means nothing shrank, so retrying would
// resend the same bytes.
func compressedRequestWithSmallerImages(
	request ChatCompletionRequest,
) (ChatCompletionRequest, compressedImagesSummary, bool) {
	var summary compressedImagesSummary

	if len(request.Messages) == 0 {
		return request, summary, false
	}

	messages := make([]ChatMessage, len(request.Messages))
	changed := false

	for index, message := range request.Messages {
		compressedContent, contentChanged := compressedMessageContentImages(message.Content, &summary)
		if !contentChanged {
			messages[index] = message

			continue
		}

		changed = true
		messages[index] = ChatMessage{Role: message.Role, Content: compressedContent}
	}

	if !changed {
		return request, summary, false
	}

	request.Messages = messages

	return request, summary, true
}

func compressedMessageContentImages(
	content any,
	summary *compressedImagesSummary,
) (any, bool) {
	switch typedContent := content.(type) {
	case []ContentPart:
		parts := make([]ContentPart, len(typedContent))
		changed := false

		for index, part := range typedContent {
			compressedPart, partChanged := compressedContentPartImage(part, summary)
			if !partChanged {
				parts[index] = part

				continue
			}

			changed = true
			parts[index] = compressedPart
		}

		if !changed {
			return content, false
		}

		return parts, true
	case []map[string]any:
		parts := make([]map[string]any, len(typedContent))
		changed := false

		for index, part := range typedContent {
			compressedFields, partChanged := compressedImagePartFields(part, summary)
			if !partChanged {
				parts[index] = part

				continue
			}

			changed = true
			parts[index] = compressedFields
		}

		if !changed {
			return content, false
		}

		return parts, true
	default:
		return content, false
	}
}

func compressedContentPartImage(
	part ContentPart,
	summary *compressedImagesSummary,
) (ContentPart, bool) {
	compressedFields, compressedOK := compressedImagePartFields(map[string]any(part), summary)
	if !compressedOK {
		return part, false
	}

	return ContentPart(compressedFields), true
}

func compressedImagePartFields(
	fields map[string]any,
	summary *compressedImagesSummary,
) (map[string]any, bool) {
	partType, _ := fields[searchtypes.MessageTypeKey].(string)
	if partType != searchtypes.ContentTypeImageURL {
		return nil, false
	}

	rawImageURL, exists := fields["image_url"]
	if !exists || rawImageURL == nil {
		return nil, false
	}

	imageURL, imageURLOK := imagePartDataURLString(rawImageURL)
	if !imageURLOK || strings.TrimSpace(imageURL) == "" || !strings.HasPrefix(imageURL, "data:") {
		return nil, false
	}

	compressedURL, originalBytes, shrunkBytes, compressedOK := compressedImageDataURL(imageURL)
	if !compressedOK {
		return nil, false
	}

	cloned := maps.Clone(fields)
	cloned["image_url"] = replaceImageURLValue(rawImageURL, compressedURL)
	summary.images++
	summary.originalBytes += originalBytes
	summary.compressedBytes += shrunkBytes

	return cloned, true
}

func imagePartDataURLString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case map[string]string:
		imageURL, _ := typed[searchtypes.MessageURLKey]

		return imageURL, true
	case map[string]any:
		imageURL, _ := typed[searchtypes.MessageURLKey].(string)

		return imageURL, true
	default:
		return "", false
	}
}

func replaceImageURLValue(value any, imageURL string) any {
	switch typed := value.(type) {
	case string:
		return imageURL
	case map[string]string:
		cloned := maps.Clone(typed)
		cloned[searchtypes.MessageURLKey] = imageURL

		return cloned
	case map[string]any:
		cloned := maps.Clone(typed)
		cloned[searchtypes.MessageURLKey] = imageURL

		return cloned
	default:
		return value
	}
}

func compressedImageDataURL(imageURL string) (string, int, int, bool) {
	imageData, err := ParseBase64ImageDataURL(imageURL)
	if err != nil {
		return "", 0, 0, false
	}

	originalBytes, err := imageData.Decode()
	if err != nil || len(originalBytes) == 0 {
		return "", 0, 0, false
	}

	compressedBytes, mimeType, ok := nearLosslessImageBytes(originalBytes)
	if !ok {
		return "", 0, 0, false
	}

	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(compressedBytes),
		len(originalBytes),
		len(compressedBytes),
		true
}

// nearLosslessImageBytes re-encodes raw into the smallest near-lossless
// form: the longest side is capped at payloadTooLargeImageMaxDimension
// (vision backends downscale past it anyway), then the smaller of
// best-compression PNG (lossless, keeps alpha) and quality-92 JPEG
// (visually transparent) wins. ok=false keeps the original bytes: animated
// images stay untouched so no frames are lost, undecodable formats (WebP
// has no stdlib decoder) fall back to the original payload, and
// already-optimal payloads are never grown by a pointless re-encode.
func nearLosslessImageBytes(raw []byte) ([]byte, string, bool) {
	if isAnimatedImage(raw) {
		return nil, "", false
	}

	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", false
	}

	decoded = downscaleImageToMaxDimension(decoded)

	best := raw
	mimeType := ""

	if encoded := encodeBestCompressionPNG(decoded); encoded != nil && len(encoded) < len(best) {
		best = encoded
		mimeType = searchtypes.MimeTypePNG
	}

	// JPEG drops alpha, so it is only a candidate for fully opaque
	// content; transparent images keep the lossless PNG instead.
	if imageIsOpaque(decoded) {
		if encoded := encodeHighQualityJPEG(decoded); encoded != nil && len(encoded) < len(best) {
			best = encoded
			mimeType = searchtypes.MimeTypeJPEG
		}
	}

	if len(best) >= len(raw) {
		return nil, "", false
	}

	return best, mimeType, true
}

// isAnimatedImage reports whether raw may carry animation that a single-frame
// decode would silently drop. GIF is always animated-or-animatable, APNG
// carries an acTL chunk, animated WebP an ANIM chunk; each check fails safe
// toward skipping compression.
func isAnimatedImage(raw []byte) bool {
	if bytes.HasPrefix(raw, []byte("GIF87a")) || bytes.HasPrefix(raw, []byte("GIF89a")) {
		return true
	}

	if bytes.HasPrefix(raw, []byte("\x89PNG")) {
		return bytes.Contains(raw, []byte("acTL"))
	}

	if bytes.HasPrefix(raw, []byte("RIFF")) && len(raw) >= 12 &&
		string(raw[8:12]) == "WEBP" {
		return bytes.Contains(raw, []byte("ANIM"))
	}

	return false
}

func downscaleImageToMaxDimension(source image.Image) image.Image {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	if max(width, height) <= payloadTooLargeImageMaxDimension {
		return source
	}

	scale := float64(payloadTooLargeImageMaxDimension) / float64(max(width, height))
	scaled := image.NewRGBA(image.Rect(
		0,
		0,
		max(1, int(float64(width)*scale+downscaleImageRoundHalfUp)),
		max(1, int(float64(height)*scale+downscaleImageRoundHalfUp)),
	))
	draw.CatmullRom.Scale(scaled, scaled.Bounds(), source, bounds, draw.Over, nil)

	return scaled
}

func encodeBestCompressionPNG(source image.Image) []byte {
	var buffer bytes.Buffer

	encoder := png.Encoder{CompressionLevel: png.BestCompression, BufferPool: nil}

	encodeErr := encoder.Encode(&buffer, source)
	if encodeErr != nil {
		return nil
	}

	return buffer.Bytes()
}

func encodeHighQualityJPEG(source image.Image) []byte {
	var buffer bytes.Buffer

	options := jpeg.Options{Quality: payloadTooLargeImageJPEGQuality}

	encodeErr := jpeg.Encode(&buffer, source, &options)
	if encodeErr != nil {
		return nil
	}

	return buffer.Bytes()
}

// opaqueImage is implemented by the stdlib image types that can report
// opacity without a pixel scan.
type opaqueImage interface {
	Opaque() bool
}

// imageIsOpaque reports whether decoded has no transparent pixels. Types
// without an Opaque method read as non-opaque so the JPEG candidate (which
// drops alpha) is skipped and the lossless PNG candidate wins.
func imageIsOpaque(decoded image.Image) bool {
	opaque, ok := decoded.(opaqueImage)

	return ok && opaque.Opaque()
}
