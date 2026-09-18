package app

import (
	"bytes"
	"encoding/json"
	"strings"

	"golang.org/x/net/html"
)

func fillVisualSearchResultFromState(htmlBody []byte, searchURL string, result *visualSearchResult) {
	state, ok := yandexImagesAppState(htmlBody)
	if !ok {
		return
	}

	fillVisualSearchTopMatchFromState(state, result)
	fillVisualSearchTagsFromState(state, result)
	fillVisualSearchOCRTextFromState(state, result)
	fillVisualSearchSimilarImagesFromState(state, searchURL, result)
	fillVisualSearchSiteMatchesFromState(state, searchURL, result)
}

func yandexImagesAppState(htmlBody []byte) (map[string]any, bool) {
	rootIndex := bytes.Index(htmlBody, []byte(yandexImagesAppRootMarker))
	if rootIndex == -1 {
		return nil, false
	}

	attributeIndex := bytes.Index(htmlBody[rootIndex:], []byte(yandexImagesStateAttribute))
	if attributeIndex == -1 {
		return nil, false
	}

	valueStart := rootIndex + attributeIndex + len(yandexImagesStateAttribute)

	valueEnd := bytes.Index(htmlBody[valueStart:], []byte(yandexImagesHydrateAttribute))
	if valueEnd == -1 {
		return nil, false
	}

	var decoded map[string]any

	err := json.Unmarshal([]byte(html.UnescapeString(string(htmlBody[valueStart:valueStart+valueEnd]))), &decoded)
	if err != nil {
		return nil, false
	}

	state, ok := decoded["initialState"].(map[string]any)
	if !ok {
		return nil, false
	}

	return state, true
}

func yandexStateObject(value any) map[string]any {
	object, _ := value.(map[string]any)

	return object
}

func yandexStateItems(value any) []any {
	items, _ := value.([]any)

	return items
}

func yandexStateString(value any) string {
	text, _ := value.(string)

	return strings.TrimSpace(text)
}

func fillVisualSearchTopMatchFromState(state map[string]any, result *visualSearchResult) {
	if strings.TrimSpace(result.TopMatch.Title) != "" {
		return
	}

	responses := yandexStateItems(yandexStateObject(state["cbirObjectResponses"])["objectResponses"])
	if len(responses) == 0 {
		return
	}

	topMatch := yandexStateObject(responses[0])
	if len(topMatch) == 0 {
		return
	}

	result.TopMatch = visualSearchTopMatch{
		Title:       truncateRunes(yandexStateString(topMatch["title"]), maxVisualSearchTitleRunes),
		Subtitle:    "",
		Description: truncateRunes(yandexStateString(topMatch["description"]), maxVisualSearchDescriptionRunes),
		Source:      truncateRunes(yandexStateString(topMatch["sourceName"]), maxVisualSearchTitleRunes),
		URL:         yandexStateString(topMatch["sourceUrl"]),
	}
}

func fillVisualSearchTagsFromState(state map[string]any, result *visualSearchResult) {
	if len(result.Tags) > 0 {
		return
	}

	tags := yandexStateItems(yandexStateObject(state["cbirTags"])["tags"])

	for _, item := range tags {
		tagText := truncateRunes(yandexStateString(yandexStateObject(item)["text"]), maxVisualSearchTitleRunes)
		if tagText == "" {
			continue
		}

		result.Tags = append(result.Tags, tagText)
		if len(result.Tags) == maxVisualSearchTags {
			break
		}
	}
}

func fillVisualSearchOCRTextFromState(state map[string]any, result *visualSearchResult) {
	if len(result.TextInImage) > 0 {
		return
	}

	ocr := yandexStateObject(state["cbirOcr"])
	if text := truncateRunes(yandexStateString(ocr["plainText"]), maxVisualSearchTitleRunes); text != "" {
		result.TextInImage = append(result.TextInImage, text)

		return
	}

	words := make([]string, 0)

	for _, block := range yandexStateItems(ocr["blocks"]) {
		for _, box := range yandexStateItems(yandexStateObject(block)["boxes"]) {
			for _, word := range yandexStateItems(yandexStateObject(box)["words"]) {
				if text := yandexStateString(yandexStateObject(word)["word"]); text != "" {
					words = append(words, text)
				}
			}
		}
	}

	if text := truncateRunes(strings.Join(words, " "), maxVisualSearchTitleRunes); text != "" {
		result.TextInImage = append(result.TextInImage, text)
	}
}

func fillVisualSearchSimilarImagesFromState(state map[string]any, searchURL string, result *visualSearchResult) {
	if len(result.SimilarImages) > 0 {
		return
	}

	thumbs := yandexStateItems(yandexStateObject(state["cbirSimilar"])["thumbs"])
	seenTitles := make(map[string]struct{})

	for _, thumb := range thumbs {
		entry := yandexStateObject(thumb)
		title := truncateRunes(yandexStateString(entry["title"]), maxVisualSearchTitleRunes)

		if title == "" {
			continue
		}

		foldedTitle := strings.ToLower(title)
		if _, ok := seenTitles[foldedTitle]; ok {
			continue
		}

		seenTitles[foldedTitle] = struct{}{}

		result.SimilarImages = append(result.SimilarImages, visualSearchSimilarImage{
			Title: title,
			URL:   resolveVisualSearchURL(searchURL, yandexStateString(entry["linkUrl"])),
		})
		if len(result.SimilarImages) == maxVisualSearchSimilarItems {
			break
		}
	}
}

func fillVisualSearchSiteMatchesFromState(state map[string]any, searchURL string, result *visualSearchResult) {
	if len(result.SiteMatches) > 0 {
		return
	}

	sites := yandexStateItems(yandexStateObject(state["cbirSites"])["sites"])
	seenURLs := make(map[string]struct{})

	for _, site := range sites {
		entry := yandexStateObject(site)
		itemURL := resolveVisualSearchURL(searchURL, yandexStateString(entry["url"]))

		if itemURL == "" {
			continue
		}

		if _, ok := seenURLs[itemURL]; ok {
			continue
		}

		seenURLs[itemURL] = struct{}{}

		result.SiteMatches = append(result.SiteMatches, visualSearchSiteMatch{
			Title:   truncateRunes(yandexStateString(entry["title"]), maxVisualSearchTitleRunes),
			Domain:  truncateRunes(yandexStateString(entry["domain"]), maxVisualSearchTitleRunes),
			Snippet: truncateRunes(yandexStateString(entry["description"]), maxVisualSearchSnippetRunes),
			URL:     itemURL,
		})
		if len(result.SiteMatches) == maxVisualSearchSiteMatches {
			break
		}
	}
}
