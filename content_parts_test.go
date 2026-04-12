package main

import "testing"

const testDocumentBody = "document-bytes"

func TestRequestMessagesWithFileOrImageOnlyQueryPlaceholderAddsPlaceholderForDocumentOnlyUserMessage(t *testing.T) {
	t.Parallel()

	messages := []chatMessage{{
		Role: messageRoleUser,
		Content: []contentPart{
			{"type": contentTypeText, "text": ""},
			{
				"type":               contentTypeDocument,
				contentFieldBytes:    []byte(testDocumentBody),
				contentFieldMIMEType: mimeTypePDF,
				contentFieldFilename: testPDFFilename,
			},
		},
	}}

	normalizedMessages := requestMessagesWithFileOrImageOnlyQueryPlaceholder(messages)

	parts, ok := normalizedMessages[0].Content.([]contentPart)
	if !ok || len(parts) != 2 {
		t.Fatalf("unexpected normalized content: %#v", normalizedMessages[0].Content)
	}

	if parts[0]["type"] != contentTypeText || parts[0]["text"] != fileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected placeholder text part: %#v", parts[0])
	}

	documentBytes, documentBytesOK := parts[1][contentFieldBytes].([]byte)
	if parts[1]["type"] != contentTypeDocument || !documentBytesOK || string(documentBytes) != testDocumentBody {
		t.Fatalf("unexpected document part: %#v", parts[1])
	}

	originalParts, originalPartsOK := messages[0].Content.([]contentPart)
	if !originalPartsOK || originalParts[0]["text"] != "" {
		t.Fatalf("expected original request messages to remain unchanged: %#v", messages[0].Content)
	}
}

func TestRequestMessagesWithFileOrImageOnlyQueryPlaceholderAddsPlaceholderForFileOnlyUserMessage(t *testing.T) {
	t.Parallel()

	messages := []chatMessage{{
		Role: messageRoleUser,
		Content: []contentPart{
			{"type": contentTypeText, "text": ""},
			{
				"type":               contentTypeFileData,
				contentFieldBytes:    []byte(testDocumentBody),
				contentFieldMIMEType: mimeTypeOctetStream,
				contentFieldFilename: testBinaryFilename,
			},
		},
	}}

	normalizedMessages := requestMessagesWithFileOrImageOnlyQueryPlaceholder(messages)

	parts, ok := normalizedMessages[0].Content.([]contentPart)
	if !ok || len(parts) != 2 {
		t.Fatalf("unexpected normalized content: %#v", normalizedMessages[0].Content)
	}

	if parts[0]["type"] != contentTypeText || parts[0]["text"] != fileOrImageOnlyQueryPlaceholder {
		t.Fatalf("unexpected placeholder text part: %#v", parts[0])
	}

	if parts[1]["type"] != contentTypeFileData || parts[1][contentFieldFilename] != testBinaryFilename {
		t.Fatalf("unexpected file part: %#v", parts[1])
	}
}

func TestRequestMessagesWithFileOrImageOnlyQueryPlaceholderAddsPlaceholderForEmptyTextUserMessage(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		content any
	}{
		{
			name:    "empty string",
			content: " \n\t ",
		},
		{
			name:    "nil content",
			content: nil,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			messages := []chatMessage{{
				Role:    messageRoleUser,
				Content: testCase.content,
			}}

			normalizedMessages := requestMessagesWithFileOrImageOnlyQueryPlaceholder(messages)

			if normalizedMessages[0].Content != fileOrImageOnlyQueryPlaceholder {
				t.Fatalf("unexpected normalized content: %#v", normalizedMessages[0].Content)
			}
		})
	}
}
