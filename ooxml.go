package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
)

func extractDOCXContent(documentBytes []byte) (string, []contentPart, error) {
	return extractOOXMLContent(documentBytes, "word/", "word/media/")
}

func extractPPTXContent(documentBytes []byte) (string, []contentPart, error) {
	return extractOOXMLContent(documentBytes, "ppt/", "ppt/media/")
}

func extractOOXMLContent(
	documentBytes []byte,
	xmlPrefix string,
	mediaPrefix string,
) (string, []contentPart, error) {
	xmlFiles, mediaFiles, err := ooxmlArchiveFiles(documentBytes, xmlPrefix, mediaPrefix)
	if err != nil {
		return "", nil, err
	}

	text, err := extractOOXMLArchiveText(xmlFiles)
	if err != nil {
		return "", nil, err
	}

	imageParts, err := extractOOXMLArchiveImages(mediaFiles)
	if err != nil {
		return "", nil, err
	}

	return text, imageParts, nil
}

func ooxmlArchiveFiles(
	documentBytes []byte,
	xmlPrefix string,
	mediaPrefix string,
) ([]*zip.File, []*zip.File, error) {
	archiveReader, err := zip.NewReader(bytes.NewReader(documentBytes), int64(len(documentBytes)))
	if err != nil {
		return nil, nil, fmt.Errorf("open OOXML archive: %w", err)
	}

	normalizedXMLPrefix := strings.ToLower(strings.TrimSpace(xmlPrefix))
	normalizedMediaPrefix := strings.ToLower(strings.TrimSpace(mediaPrefix))

	xmlFiles := make([]*zip.File, 0)
	mediaFiles := make([]*zip.File, 0)

	for _, archiveFile := range archiveReader.File {
		if archiveFile == nil || archiveFile.FileInfo().IsDir() {
			continue
		}

		normalizedName := strings.ToLower(strings.TrimSpace(archiveFile.Name))

		switch {
		case strings.HasPrefix(normalizedName, normalizedXMLPrefix) &&
			strings.HasSuffix(normalizedName, ".xml"):
			xmlFiles = append(xmlFiles, archiveFile)
		case strings.HasPrefix(normalizedName, normalizedMediaPrefix):
			mediaFiles = append(mediaFiles, archiveFile)
		}
	}

	sort.Slice(xmlFiles, func(left int, right int) bool {
		return strings.ToLower(xmlFiles[left].Name) < strings.ToLower(xmlFiles[right].Name)
	})

	sort.Slice(mediaFiles, func(left int, right int) bool {
		return strings.ToLower(mediaFiles[left].Name) < strings.ToLower(mediaFiles[right].Name)
	})

	return xmlFiles, mediaFiles, nil
}

func extractOOXMLArchiveText(xmlFiles []*zip.File) (string, error) {
	textBlocks := make([]string, 0, len(xmlFiles))

	for _, xmlFile := range xmlFiles {
		xmlBytes, err := archiveFileBytes(xmlFile)
		if err != nil {
			return "", fmt.Errorf("read OOXML XML file %q: %w", xmlFile.Name, err)
		}

		extractedText, err := extractOOXMLText(xmlBytes)
		if err != nil {
			return "", fmt.Errorf("extract OOXML XML text from %q: %w", xmlFile.Name, err)
		}

		if extractedText == "" {
			continue
		}

		textBlocks = append(textBlocks, extractedText)
	}

	return strings.TrimSpace(strings.Join(textBlocks, "\n\n")), nil
}

func extractOOXMLArchiveImages(mediaFiles []*zip.File) ([]contentPart, error) {
	imageParts := make([]contentPart, 0, len(mediaFiles))

	for _, mediaFile := range mediaFiles {
		imagePart, ok, err := ooxmlImagePart(mediaFile)
		if err != nil {
			return nil, fmt.Errorf("extract OOXML image %q: %w", mediaFile.Name, err)
		}

		if !ok {
			continue
		}

		imageParts = append(imageParts, imagePart)
	}

	return imageParts, nil
}

func archiveFileBytes(archiveFile *zip.File) ([]byte, error) {
	reader, err := archiveFile.Open()
	if err != nil {
		return nil, fmt.Errorf("open archive file %q: %w", archiveFile.Name, err)
	}

	defer func() {
		_ = reader.Close()
	}()

	fileBytes, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read archive file %q: %w", archiveFile.Name, err)
	}

	return fileBytes, nil
}

func extractOOXMLText(xmlBytes []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(xmlBytes))

	lines := make([]string, 0)

	var lineBuilder strings.Builder

	flushLine := func() {
		trimmedLine := strings.TrimSpace(lineBuilder.String())
		if trimmedLine != "" {
			lines = append(lines, trimmedLine)
		}

		lineBuilder.Reset()
	}

	textElementDepth := 0

	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return "", fmt.Errorf("decode OOXML XML token: %w", err)
		}

		switch typedToken := token.(type) {
		case xml.StartElement:
			elementName := strings.ToLower(typedToken.Name.Local)

			switch {
			case ooxmlTextElement(elementName):
				textElementDepth++
			case ooxmlLineBreakElement(elementName):
				flushLine()
			case elementName == "tab":
				appendOOXMLTextChunk(&lineBuilder, " ")
			}
		case xml.EndElement:
			elementName := strings.ToLower(typedToken.Name.Local)

			switch {
			case ooxmlTextElement(elementName):
				if textElementDepth > 0 {
					textElementDepth--
				}
			case ooxmlParagraphElement(elementName):
				flushLine()
			}
		case xml.CharData:
			if textElementDepth == 0 {
				continue
			}

			appendOOXMLTextChunk(&lineBuilder, string(typedToken))
		}
	}

	flushLine()

	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}

func ooxmlTextElement(elementName string) bool {
	switch elementName {
	case "t", "deltext", "instrtext":
		return true
	default:
		return false
	}
}

func ooxmlLineBreakElement(elementName string) bool {
	return elementName == "br" || elementName == "cr"
}

func ooxmlParagraphElement(elementName string) bool {
	return elementName == "p"
}

func appendOOXMLTextChunk(builder *strings.Builder, textChunk string) {
	normalizedChunk := strings.Join(strings.Fields(strings.TrimSpace(textChunk)), " ")
	if normalizedChunk == "" {
		return
	}

	if builder.Len() > 0 {
		builder.WriteByte(' ')
	}

	builder.WriteString(normalizedChunk)
}

func ooxmlImagePart(archiveFile *zip.File) (contentPart, bool, error) {
	imageBytes, err := archiveFileBytes(archiveFile)
	if err != nil {
		return nil, false, err
	}

	if len(imageBytes) == 0 {
		return contentPart{}, false, nil
	}

	mimeType, ok := ooxmlImageMIMEType(archiveFile.Name, imageBytes)
	if !ok {
		return contentPart{}, false, nil
	}

	part := make(contentPart)
	part["type"] = contentTypeImageURL
	part["image_url"] = map[string]string{
		"url": fmt.Sprintf(
			"data:%s;base64,%s",
			mimeType,
			base64.StdEncoding.EncodeToString(imageBytes),
		),
	}

	return part, true, nil
}

func ooxmlImageMIMEType(fileName string, imageBytes []byte) (string, bool) {
	fileExtension := strings.ToLower(strings.TrimSpace(filepath.Ext(fileName)))
	if mimeType, ok := ooxmlImageExtensionMIMEType(fileExtension); ok {
		return mimeType, true
	}

	detectedMIMEType := strings.ToLower(strings.TrimSpace(http.DetectContentType(imageBytes)))
	detectedMIMEType, _, _ = strings.Cut(detectedMIMEType, ";")

	if !strings.HasPrefix(detectedMIMEType, "image/") {
		return "", false
	}

	return detectedMIMEType, true
}

func ooxmlImageExtensionMIMEType(fileExtension string) (string, bool) {
	switch fileExtension {
	case ".avif":
		return "image/avif", true
	case ".bmp":
		return "image/bmp", true
	case ".gif":
		return "image/gif", true
	case ".heic":
		return "image/heic", true
	case ".jpeg", ".jpg":
		return "image/jpeg", true
	case ".png":
		return mimeTypePNG, true
	case ".svg":
		return "image/svg+xml", true
	case ".tif", ".tiff":
		return "image/tiff", true
	case ".webp":
		return "image/webp", true
	default:
		return "", false
	}
}
