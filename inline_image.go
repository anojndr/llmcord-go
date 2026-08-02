package main

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

type base64ImageDataURL struct {
	mimeType string
	payload  string
}

func parseBase64ImageDataURL(imageURL string) (base64ImageDataURL, error) {
	if !strings.HasPrefix(imageURL, "data:") {
		return base64ImageDataURL{}, fmt.Errorf("unsupported image URL: %w", os.ErrInvalid)
	}

	metadata, payload, found := strings.Cut(strings.TrimPrefix(imageURL, "data:"), ",")
	if !found {
		return base64ImageDataURL{}, fmt.Errorf("parse image data URL: %w", os.ErrInvalid)
	}

	mediaType, parameters, hasParameters := strings.Cut(metadata, ";")
	mimeType := "application/octet-stream"

	if strings.TrimSpace(mediaType) != "" {
		parsedMediaType, _, err := mime.ParseMediaType(strings.TrimSpace(mediaType))
		if err != nil {
			return base64ImageDataURL{}, fmt.Errorf("parse image data URL media type: %w", err)
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
		return base64ImageDataURL{}, fmt.Errorf(
			"unsupported image data URL encoding: %w",
			os.ErrInvalid,
		)
	}

	return base64ImageDataURL{
		mimeType: mimeType,
		payload:  payload,
	}, nil
}

func (data base64ImageDataURL) decodedLengthEstimate() int {
	trimmedPayload := strings.TrimRight(strings.TrimSpace(data.payload), "=")

	return len(trimmedPayload) * base64DecodedLengthNumerator / base64DecodedLengthDenominator
}

func base64DecodedLengthEstimate(payload string) int {
	trimmedPayload := strings.TrimRight(strings.TrimSpace(payload), "=")

	return len(trimmedPayload) * base64DecodedLengthNumerator / base64DecodedLengthDenominator
}

func (data base64ImageDataURL) decoder() io.Reader {
	return base64.NewDecoder(base64.StdEncoding, strings.NewReader(data.payload))
}

func (data base64ImageDataURL) decode() ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(data.payload)
	if err != nil {
		return nil, fmt.Errorf("decode image data: %w", err)
	}

	return decoded, nil
}
