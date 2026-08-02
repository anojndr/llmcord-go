package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"path"
	"strings"

	"golang.org/x/net/html"
)

const (
	aliExpressDCDataMarker      = "window._d_c_.DCData"
	maxAliExpressProductImages  = 8
	aliExpressTitleSuffixMarker = " - aliexpress"
)

func extractAliExpressProductMetadata(
	pageURL string,
	responseBody []byte,
	document *html.Node,
	pageTitle string,
) (string, string, bool) {
	productID, isProductPage := aliExpressProductID(pageURL)
	if !isProductPage {
		return "", "", false
	}

	title := normalizeAliExpressProductTitle(firstNonEmptyString(
		extractWebsiteMetaContent(document, "og:title"),
		extractWebsiteMetaContent(document, "twitter:title"),
		pageTitle,
	))

	images := aliExpressProductImages(pageURL, responseBody, document)
	if title == "" && len(images) == 0 {
		return "", "", false
	}

	lines := []string{
		"Product ID: " + productID,
		"Product name: " + firstNonEmptyString(title, pageTitle),
		"Final product URL: " + pageURL,
	}

	if len(images) > 0 {
		lines = append(lines, "Product images:")
		for _, imageURL := range images {
			lines = append(lines, "- "+imageURL)
		}
	}

	return firstNonEmptyString(title, pageTitle), strings.Join(lines, "\n"), true
}

func aliExpressProductID(pageURL string) (string, bool) {
	parsedURL, err := url.Parse(pageURL)
	if err != nil || !isAliExpressHost(parsedURL.Hostname()) {
		return "", false
	}

	fileName := path.Base(parsedURL.Path)
	productID := strings.TrimSuffix(fileName, ".html")

	productID = strings.TrimSuffix(productID, ".htm")
	if productID == "" || productID == fileName {
		return "", false
	}

	for _, character := range productID {
		if character < '0' || character > '9' {
			return "", false
		}
	}

	return productID, true
}

func isAliExpressHost(host string) bool {
	normalizedHost := normalizeWebsiteHost(host)

	return normalizedHost == "aliexpress.com" ||
		normalizedHost == "aliexpress.us" ||
		strings.HasSuffix(normalizedHost, ".aliexpress.com") ||
		strings.HasSuffix(normalizedHost, ".aliexpress.us")
}

func normalizeAliExpressProductTitle(title string) string {
	normalizedTitle := normalizeWebsiteText(title)
	lowerTitle := strings.ToLower(normalizedTitle)

	suffixIndex := strings.LastIndex(lowerTitle, aliExpressTitleSuffixMarker)
	if suffixIndex > 0 {
		return strings.TrimSpace(normalizedTitle[:suffixIndex])
	}

	return normalizedTitle
}

func aliExpressProductImages(
	pageURL string,
	responseBody []byte,
	document *html.Node,
) []string {
	candidates := make([]string, 0, maxAliExpressProductImages)

	dcData := extractAliExpressDCData(responseBody)
	candidates = append(candidates, mapStringSliceValue(dcData, "imagePathList")...)
	candidates = append(candidates, extractWebsiteMetaContents(document, "og:image")...)
	candidates = append(candidates, extractWebsiteMetaContents(document, "twitter:image")...)

	images := make([]string, 0, min(len(candidates), maxAliExpressProductImages))
	for _, candidate := range candidates {
		imageURL := normalizeAliExpressAssetURL(pageURL, candidate)
		if imageURL == "" || slicesContainsFold(images, imageURL) {
			continue
		}

		images = append(images, imageURL)
		if len(images) >= maxAliExpressProductImages {
			break
		}
	}

	return images
}

func extractAliExpressDCData(responseBody []byte) map[string]any {
	objectBytes, found := extractJSONObjectAfterMarker(responseBody, aliExpressDCDataMarker)
	if !found {
		return nil
	}

	var dcData map[string]any

	err := json.Unmarshal(objectBytes, &dcData)
	if err != nil {
		return nil
	}

	return dcData
}

func extractJSONObjectAfterMarker(responseBody []byte, marker string) ([]byte, bool) {
	markerIndex := bytes.Index(responseBody, []byte(marker))
	if markerIndex < 0 {
		return nil, false
	}

	remainingBody := responseBody[markerIndex+len(marker):]

	objectOffset := bytes.IndexByte(remainingBody, '{')
	if objectOffset < 0 {
		return nil, false
	}

	objectStart := markerIndex + len(marker) + objectOffset
	depth := 0
	inString := false
	escaped := false

	for index := objectStart; index < len(responseBody); index++ {
		character := responseBody[index]

		if inString {
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}

			continue
		}

		switch character {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return responseBody[objectStart : index+1], true
			}
		}
	}

	return nil, false
}

func normalizeAliExpressAssetURL(pageURL string, rawAssetURL string) string {
	trimmedAssetURL := strings.TrimSpace(rawAssetURL)
	if trimmedAssetURL == "" {
		return ""
	}

	parsedPageURL, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}

	parsedAssetURL, err := parsedPageURL.Parse(trimmedAssetURL)
	if err != nil || !isWebsiteScheme(parsedAssetURL.Scheme) {
		return ""
	}

	return parsedAssetURL.String()
}

func slicesContainsFold(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(value, expected) {
			return true
		}
	}

	return false
}
