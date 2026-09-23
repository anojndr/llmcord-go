package app

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// maxForwardedImageBytes caps one image payload kept for the LLM. Discord,
// PDF, and OOXML callers converge on makeImageContentPart, so the bound
// lives here: without it every multi-megapixel original is duplicated in
// memory, snapshot JSON, and each follow-up request.
const maxForwardedImageBytes = 20 * 1024 * 1024

// supportedLLMImageMIMETypeSet returns the raster types both the OpenAI
// vision guide (png, jpeg, webp, non-animated gif) and the Gemini inline
// path accept. Anything else (heic, avif, svg, tiff, bmp, jpx) is rejected
// at ingest instead of failing late provider-side.
func supportedLLMImageMIMETypeSet() map[string]struct{} {
	return map[string]struct{}{
		mimeTypeJPEG: {},
		mimeTypePNG:  {},
		mimeTypeWEBP: {},
		"image/gif":  {},
	}
}

// makeImageContentPart builds an image_url part from the original bytes.
//
// Images are sent to the LLM exactly as received: no resizing, re-encoding,
// or MIME conversion. Fidelity is controlled provider-side (OpenAI image
// detail high, Gemini per-part media resolution ultra high), so ingest stays
// a byte-identical passthrough for supported types within the byte budget.
// Oversized or unsupported images return false so buildMediaParts drops them
// and the existing unsupported-attachment warning fires.
func makeImageContentPart(mimeType string, imageBytes []byte) (contentPart, bool) {
	normalizedType := normalizedMIMEType(mimeType)
	if _, ok := supportedLLMImageMIMETypeSet()[normalizedType]; !ok {
		return nil, false
	}

	if len(imageBytes) == 0 || len(imageBytes) > maxForwardedImageBytes {
		return nil, false
	}

	trimmedMIME := strings.TrimSpace(mimeType)
	if trimmedMIME == "" {
		return nil, false
	}

	part := make(contentPart)
	part[messageTypeKey] = contentTypeImageURL
	part["image_url"] = map[string]string{
		messageURLKey: fmt.Sprintf(
			"data:%s;base64,%s",
			trimmedMIME,
			base64.StdEncoding.EncodeToString(imageBytes),
		),
	}

	return part, true
}
