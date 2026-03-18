package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"sort"
	"strings"
	"testing"
)

func TestExtractDOCXContentReturnsTextAndImages(t *testing.T) {
	t.Parallel()

	documentBytes := buildTestDOCX(t, "Quarterly revenue grew by 12 percent.")

	text, imageParts, err := extractDOCXContent(documentBytes)
	if err != nil {
		t.Fatalf("extract DOCX content: %v", err)
	}

	if !strings.Contains(text, "Quarterly revenue grew by 12 percent.") {
		t.Fatalf("unexpected extracted DOCX text: %q", text)
	}

	if len(imageParts) != 1 {
		t.Fatalf("unexpected extracted DOCX image count: %d", len(imageParts))
	}

	imageURL, ok := imageParts[0]["image_url"].(map[string]string)
	if !ok {
		t.Fatalf("unexpected extracted DOCX image part: %#v", imageParts[0])
	}

	if !strings.HasPrefix(imageURL["url"], "data:image/jpeg;base64,") {
		t.Fatalf("unexpected extracted DOCX image URL: %q", imageURL["url"])
	}
}

func TestExtractPPTXContentReturnsTextAndImages(t *testing.T) {
	t.Parallel()

	documentBytes := buildTestPPTX(t, "Slide 1: revenue grew by 12 percent.")

	text, imageParts, err := extractPPTXContent(documentBytes)
	if err != nil {
		t.Fatalf("extract PPTX content: %v", err)
	}

	if !strings.Contains(text, "Slide 1: revenue grew by 12 percent.") {
		t.Fatalf("unexpected extracted PPTX text: %q", text)
	}

	if len(imageParts) != 1 {
		t.Fatalf("unexpected extracted PPTX image count: %d", len(imageParts))
	}

	imageURL, ok := imageParts[0]["image_url"].(map[string]string)
	if !ok {
		t.Fatalf("unexpected extracted PPTX image part: %#v", imageParts[0])
	}

	if !strings.HasPrefix(imageURL["url"], "data:image/jpeg;base64,") {
		t.Fatalf("unexpected extracted PPTX image URL: %q", imageURL["url"])
	}
}

func testDOCXDocumentPart(
	t *testing.T,
	text string,
) contentPart {
	t.Helper()

	return contentPart{
		"type":               contentTypeDocument,
		contentFieldBytes:    buildTestDOCX(t, text),
		contentFieldMIMEType: mimeTypeDOCX,
		contentFieldFilename: testDOCXFilename,
	}
}

func testPPTXDocumentPart(
	t *testing.T,
	text string,
) contentPart {
	t.Helper()

	return contentPart{
		"type":               contentTypeDocument,
		contentFieldBytes:    buildTestPPTX(t, text),
		contentFieldMIMEType: mimeTypePPTX,
		contentFieldFilename: testPPTXFilename,
	}
}

func buildTestDOCX(t *testing.T, text string) []byte {
	t.Helper()

	files := map[string][]byte{
		"[Content_Types].xml": []byte(strings.Join([]string{
			`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`,
			`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`,
		}, "")),
		"word/document.xml": []byte(strings.Join([]string{
			`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`,
			`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">`,
			`<w:body><w:p><w:r><w:t>`,
			escapeXMLText(t, text),
			`</w:t></w:r></w:p></w:body>`,
			`</w:document>`,
		}, "")),
	}

	files["word/media/image1.jpeg"] = testJPEGBytes(t)

	return buildTestOOXMLArchive(t, files)
}

func buildTestPPTX(t *testing.T, text string) []byte {
	t.Helper()

	files := map[string][]byte{
		"[Content_Types].xml": []byte(strings.Join([]string{
			`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`,
			`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`,
		}, "")),
		"ppt/slides/slide1.xml": []byte(strings.Join([]string{
			`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`,
			`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"`,
			` xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">`,
			`<p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>`,
			escapeXMLText(t, text),
			`</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld>`,
			`</p:sld>`,
		}, "")),
	}

	files["ppt/media/image1.jpeg"] = testJPEGBytes(t)

	return buildTestOOXMLArchive(t, files)
}

func buildTestOOXMLArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var archiveBuffer bytes.Buffer

	archiveWriter := zip.NewWriter(&archiveBuffer)

	fileNames := make([]string, 0, len(files))
	for fileName := range files {
		fileNames = append(fileNames, fileName)
	}

	sort.Strings(fileNames)

	for _, fileName := range fileNames {
		writer, err := archiveWriter.Create(fileName)
		if err != nil {
			t.Fatalf("create OOXML test file %q: %v", fileName, err)
		}

		_, err = writer.Write(files[fileName])
		if err != nil {
			t.Fatalf("write OOXML test file %q: %v", fileName, err)
		}
	}

	err := archiveWriter.Close()
	if err != nil {
		t.Fatalf("finalize OOXML test archive: %v", err)
	}

	return archiveBuffer.Bytes()
}

func escapeXMLText(t *testing.T, text string) string {
	t.Helper()

	var escapedText bytes.Buffer

	err := xml.EscapeText(&escapedText, []byte(text))
	if err != nil {
		t.Fatalf("escape XML text %q: %v", text, err)
	}

	return escapedText.String()
}
