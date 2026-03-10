package main

import "testing"

func assertContextAugmentationPreservesImages(
	t *testing.T,
	inputText string,
	formattedContent string,
	sectionName string,
	appendContent func([]chatMessage, string) ([]chatMessage, error),
) {
	t.Helper()

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": inputText},
				{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
			},
		},
	}

	augmentedConversation, err := appendContent(conversation, formattedContent)
	if err != nil {
		t.Fatalf("append context content: %v", err)
	}

	parts, ok := augmentedConversation[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected image to be preserved: %#v", parts[1])
	}

	textValue, _ := parts[0]["text"].(string)
	if !containsFold(textValue, sectionName+":") {
		t.Fatalf("expected %s prompt in text part: %q", sectionName, textValue)
	}
}
