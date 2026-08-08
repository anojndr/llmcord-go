package providers

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"os"
	"strings"
)

const (
	base64DecodedLengthNumerator   = 3
	base64DecodedLengthDenominator = 4
)

// Base64ImageDataURL is a parsed data: URL image payload.
type Base64ImageDataURL struct {
	MimeType string
	Payload  string
}

// ParseBase64ImageDataURL parses a data: URL into its payload.
func ParseBase64ImageDataURL(imageURL string) (Base64ImageDataURL, error) {
	if !strings.HasPrefix(imageURL, "data:") {
		return Base64ImageDataURL{}, fmt.Errorf("unsupported image URL: %w", os.ErrInvalid)
	}

	metadata, payload, found := strings.Cut(strings.TrimPrefix(imageURL, "data:"), ",")
	if !found {
		return Base64ImageDataURL{}, fmt.Errorf("parse image data URL: %w", os.ErrInvalid)
	}

	mediaType, parameters, hasParameters := strings.Cut(metadata, ";")
	mimeType := "application/octet-stream"

	if strings.TrimSpace(mediaType) != "" {
		parsedMediaType, _, err := mime.ParseMediaType(strings.TrimSpace(mediaType))
		if err != nil {
			return Base64ImageDataURL{}, fmt.Errorf("parse image data URL media type: %w", err)
		}

		mimeType = parsedMediaType
	}

	hasBase64Encoding := false

	for hasParameters {
		var parameter string

		parameter, parameters, hasParameters = strings.Cut(parameters, ";")
		if strings.EqualFold(strings.TrimSpace(parameter), "base64") {
			hasBase64Encoding = true
		}
	}

	if !hasBase64Encoding {
		return Base64ImageDataURL{}, fmt.Errorf(
			"unsupported image data URL encoding: %w",
			os.ErrInvalid,
		)
	}

	return Base64ImageDataURL{
		MimeType: mimeType,
		Payload:  payload,
	}, nil
}

// DecodedLengthEstimate estimates the decoded byte length.
func (data Base64ImageDataURL) DecodedLengthEstimate() int {
	trimmedPayload := strings.TrimRight(strings.TrimSpace(data.Payload), "=")

	return len(trimmedPayload) * base64DecodedLengthNumerator / base64DecodedLengthDenominator
}

func base64DecodedLengthEstimate(payload string) int {
	trimmedPayload := strings.TrimRight(strings.TrimSpace(payload), "=")

	return len(trimmedPayload) * base64DecodedLengthNumerator / base64DecodedLengthDenominator
}

// Decoder returns a reader over the image payload.
func (data Base64ImageDataURL) Decoder() io.Reader {
	return base64.NewDecoder(base64.StdEncoding, strings.NewReader(data.Payload))
}

// Decode returns the raw image bytes.
func (data Base64ImageDataURL) Decode() ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(data.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode image data: %w", err)
	}

	return decoded, nil
}
