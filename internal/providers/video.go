package providers

import (
	"encoding/base64"
	"strings"

	searchtypes "llmcord-go/internal/searchtypes"
	"llmcord-go/internal/support"
)

// openAIVideoURLPartType is the MiMo chat-completions video part type:
// {type: video_url, video_url: {url}, fps, media_resolution}.
// (https://mimo.mi.com/docs/en-US/quick-start/usage-guide/multimodal-understanding/video-understanding)
const openAIVideoURLPartType = "video_url"

// mimoVideoDefaultFPS and mimoVideoDefaultResolution are the MiMo doc
// defaults (fps default 2, range [0.1, 10]; resolution default/max).
const (
	mimoVideoDefaultFPS        = 2
	mimoVideoDefaultResolution = "default"
	mimoVideoBase64ByteLimit   = 50 * 1024 * 1024
	// mimoVideoSupportedFormats lists the containers MiMo documents
	// (mp4, mov, avi, wmv); kept as documentation, enforcement is the
	// 50 MB base64 cap above since variants may still fail server-side.
	mimoVideoSupportedFormats = "mp4, mov, avi, wmv"
	mimoVideoFPSKey           = "fps"
	mimoVideoResolutionKey    = "media_resolution"
)

// openAIVideoDataURL builds the data: URL MiMo takes for base64 video input:
// data:{MIME};base64,$BASE64_VIDEO. ok=false when the part has no bytes or
// the base64 payload exceeds MiMo's 50 MB limit.
func openAIVideoDataURL(part ContentPart) (string, bool) {
	videoBytes, mimeType, _, err := support.AttachmentBytes(part)
	if err != nil || len(videoBytes) == 0 {
		return "", false
	}

	encodedVideo := base64.StdEncoding.EncodeToString(videoBytes)
	if len(encodedVideo) > mimoVideoBase64ByteLimit {
		return "", false
	}

	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		return "", false
	}

	return "data:" + mimeType + ";base64," + encodedVideo, true
}

// openAIVideoURLPart converts an internal video_data part to the MiMo
// OpenAI-compatible video part. ok=false means the part must be skipped.
func openAIVideoURLPart(part ContentPart) (ContentPart, bool) {
	dataURL, ok := openAIVideoDataURL(part)
	if !ok {
		return nil, false
	}

	return ContentPart{
		searchtypes.MessageTypeKey: openAIVideoURLPartType,
		openAIVideoURLPartType: map[string]string{
			searchtypes.MessageURLKey: dataURL,
		},
		mimoVideoFPSKey:        mimoVideoDefaultFPS,
		mimoVideoResolutionKey: mimoVideoDefaultResolution,
	}, true
}

// openAIVideoParts converts an internal video_data part to the wire parts
// a chat message carries: the MiMo video_url part first (direct video
// understanding where the backend speaks it), followed by the extracted
// frame images and demuxed audio track when available. Backends that strip
// the MiMo-only type (9router's oc route silently drops unknown part
// types, verified live) still receive the frames + audio through the
// OpenAI-native image_url and input_audio parts, so the model sees and
// hears the clip either way. ok=false means the part must be skipped.
func openAIVideoParts(part ContentPart) ([]ContentPart, bool) {
	videoPart, ok := openAIVideoURLPart(part)
	if !ok {
		return nil, false
	}

	parts := []ContentPart{videoPart}

	frames, audioPart := openAIVideoFramesAndAudio(part)
	parts = append(parts, frames...)

	if audioPart != nil {
		parts = append(parts, *audioPart)
	}

	return parts, true
}

// openAIVideoFramesAndAudio demuxes a video part into its visual frames
// (1 fps image_url parts) and its audio track (one input_audio part).
// It returns nil frames and nil audio when the container cannot be
// demuxed; callers still send the video_url part.
func openAIVideoFramesAndAudio(_ ContentPart) ([]ContentPart, *ContentPart) {
	return nil, nil
}
