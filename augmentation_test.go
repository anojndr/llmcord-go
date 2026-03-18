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

func TestAppendReplyTargetToConversationAddsRepliedMessageSection(t *testing.T) {
	t.Parallel()

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: "<@user-1>: jandron",
		},
		{
			Role:    messageRoleUser,
			Content: "<@user-1>: what is the text inside this file",
		},
	}

	augmentedConversation, err := appendReplyTargetToConversation(
		conversation,
		conversation[0],
	)
	if err != nil {
		t.Fatalf("append reply target to conversation: %v", err)
	}

	latestContent, ok := augmentedConversation[len(augmentedConversation)-1].Content.(string)
	if !ok {
		t.Fatalf("unexpected latest content type: %T", augmentedConversation[len(augmentedConversation)-1].Content)
	}

	if !containsFold(latestContent, replyTargetSectionName+":") {
		t.Fatalf("expected replied message section in latest content: %q", latestContent)
	}

	if !containsFold(latestContent, "jandron") {
		t.Fatalf("expected replied message text in latest content: %q", latestContent)
	}
}

func TestAppendReplyTargetToConversationAppendsReplyMedia(t *testing.T) {
	t.Parallel()

	replyTarget := chatMessage{
		Role: messageRoleUser,
		Content: []contentPart{
			{"type": contentTypeText, "text": "<@user-1>: what is in this image"},
			{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
		},
	}

	conversation := []chatMessage{
		replyTarget,
		{
			Role:    messageRoleUser,
			Content: "<@user-1>: describe this",
		},
	}

	augmentedConversation, err := appendReplyTargetToConversation(
		conversation,
		replyTarget,
	)
	if err != nil {
		t.Fatalf("append reply target to conversation: %v", err)
	}

	parts, ok := augmentedConversation[len(augmentedConversation)-1].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected latest content type: %T", augmentedConversation[len(augmentedConversation)-1].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected latest part count: %d", len(parts))
	}

	textValue, _ := parts[0]["text"].(string)
	if !containsFold(textValue, replyTargetSectionName+":") {
		t.Fatalf("expected replied message section in latest text part: %q", textValue)
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected replied image part to be appended: %#v", parts[1])
	}
}

func TestAppendDocumentContentToConversationPreservesImages(t *testing.T) {
	t.Parallel()

	assertContextAugmentationPreservesImages(
		t,
		"<@123>: summarize this report",
		"Filename: report.docx\nExtracted text:\nQuarterly revenue grew by 12 percent.",
		documentContentSectionName,
		appendDocumentContentToConversation,
	)
}
