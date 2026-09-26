package providers

import (
	"testing"

	searchtypes "llmcord-go/internal/searchtypes"
)

func TestOpenAINormalizeContentPartDropsInvalidAudio(t *testing.T) {
	t.Parallel()

	part := ContentPart{
		"type":                           searchtypes.ContentTypeAudioData,
		searchtypes.ContentFieldBytes:    []byte{},
		searchtypes.ContentFieldMIMEType: "audio/mpeg",
	}

	normalizedPart, changed := openAINormalizeContentPart(part)
	if !changed {
		t.Fatal("expected drop to report changed")
	}

	if normalizedPart != nil {
		t.Fatalf("expected invalid audio to be dropped: %#v", normalizedPart)
	}
}

func TestOpenAINormalizeContentPartDropsInvalidVideo(t *testing.T) {
	t.Parallel()

	part := ContentPart{
		"type":                           searchtypes.ContentTypeVideoData,
		searchtypes.ContentFieldBytes:    []byte{},
		searchtypes.ContentFieldMIMEType: "video/mp4",
	}

	normalizedPart, changed := openAINormalizeContentPart(part)
	if !changed {
		t.Fatal("expected drop to report changed")
	}

	if normalizedPart != nil {
		t.Fatalf("expected invalid video to be dropped: %#v", normalizedPart)
	}
}

func TestOpenAINormalizeMessageContentDropsInvalidAudioKeepsText(t *testing.T) {
	t.Parallel()

	content := []ContentPart{
		{"type": searchtypes.ContentTypeText, "text": "hello"},
		{
			"type":                           searchtypes.ContentTypeAudioData,
			searchtypes.ContentFieldBytes:    []byte{},
			searchtypes.ContentFieldMIMEType: "audio/mpeg",
		},
	}

	normalizedContent, changed := openAINormalizeMessageContent(content)
	if !changed {
		t.Fatal("expected drop to report changed")
	}

	parts, ok := normalizedContent.([]ContentPart)
	if !ok || len(parts) != 1 {
		t.Fatalf("unexpected normalized content: %#v", normalizedContent)
	}

	if parts[0]["type"] != searchtypes.ContentTypeText {
		t.Fatalf("expected text to survive: %#v", parts[0])
	}
}

func TestContentPartsNeedPlaceholderForVideoOnly(t *testing.T) {
	t.Parallel()

	parts := []ContentPart{
		{"type": searchtypes.ContentTypeText, "text": ""},
		testVideoContentPart("video/mp4"),
	}

	if !contentPartsNeedFileOrImageOnlyQueryPlaceholder(parts) {
		t.Fatalf("expected video-only content to need placeholder: %#v", parts)
	}
}
