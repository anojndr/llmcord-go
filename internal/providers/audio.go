package providers

import (
	"encoding/base64"
	"strings"

	searchtypes "llmcord-go/internal/searchtypes"
	"llmcord-go/internal/support"
)

// openAIAudioInputPartType is the Chat Completions and Responses API content
// part type for audio input (openai-openapi: ChatCompletionRequestMessageContentPartAudio,
// InputAudio).
const openAIAudioInputPartType = "input_audio"

// openAIAudioInputFormat maps an attachment MIME type to the audio format
// label sent beside the base64 payload. wav and mp3 are the documented
// values; anything else passes its container subtype through (verified:
// 9router accepts "ogg" for oc/mimo-v2.6-flash-free on both APIs).
func openAIAudioInputFormat(mimeType string) string {
	normalizedType := support.NormalizedMIMEType(mimeType)

	switch normalizedType {
	case "audio/wav", "audio/wave", "audio/x-wav":
		return "wav"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	default:
		if subtype, found := strings.CutPrefix(normalizedType, "audio/"); found && strings.TrimSpace(subtype) != "" {
			return subtype
		}

		return "wav"
	}
}

// encodeOpenAIAudioPart extracts the base64 payload and format label for an
// internal audio_data part. Empty or undecodable parts report ok=false so
// callers drop them instead of failing the request.
func encodeOpenAIAudioPart(part ContentPart) (string, string, bool) {
	audioBytes, mimeType, _, err := support.AttachmentBytes(part)
	if err != nil || len(audioBytes) == 0 {
		return "", "", false
	}

	return base64.StdEncoding.EncodeToString(audioBytes), openAIAudioInputFormat(mimeType), true
}

// openAIAudioInputPart converts an internal audio_data part to the shared
// {type: input_audio, input_audio: {data, format}} shape both OpenAI-family
// APIs take. ok=false means the part must be skipped.
func openAIAudioInputPart(part ContentPart) (ContentPart, bool) {
	encodedData, format, ok := encodeOpenAIAudioPart(part)
	if !ok {
		return nil, false
	}

	return ContentPart{
		searchtypes.MessageTypeKey: openAIAudioInputPartType,
		openAIAudioInputPartType: map[string]string{
			"data":   encodedData,
			"format": format,
		},
	}, true
}
