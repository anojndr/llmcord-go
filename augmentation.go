package main

import (
	"fmt"
	"os"
	"strings"
)

const (
	augmentedPromptPrefix     = "Answer the user query based on "
	userQuerySectionName      = "User query"
	youtubeSectionName        = "YouTube URL content"
	redditSectionName         = "Reddit URL content"
	webSearchSectionName      = "Web search results"
	youtubeSectionDescription = "the extracted YouTube URL content"
	redditSectionDescription  = "the extracted Reddit URL content"
	webSearchStandalonePrompt = "the web search results"
	webSearchCombinedPrompt   = "web search results"
	maxPromptSections         = 3
)

type augmentedUserPrompt struct {
	UserQuery        string
	YouTubeContent   string
	RedditContent    string
	WebSearchResults string
}

func (prompt augmentedUserPrompt) render() string {
	trimmedUserQuery := strings.TrimSpace(prompt.UserQuery)

	sections := prompt.activeSections()
	if len(sections) == 0 {
		return trimmedUserQuery
	}

	descriptions := make([]string, 0, len(sections))

	renderedSections := make([]string, 0, len(sections))
	for _, section := range sections {
		descriptions = append(descriptions, section.descriptionForCount(len(sections)))
		renderedSections = append(
			renderedSections,
			fmt.Sprintf("%s:\n%s", section.Name, section.Value),
		)
	}

	return strings.Join(
		append(
			[]string{
				augmentedPromptPrefix + joinPromptDescriptions(descriptions) + ".",
				fmt.Sprintf("%s:\n%s", userQuerySectionName, trimmedUserQuery),
			},
			renderedSections...,
		),
		"\n\n",
	)
}

func parseAugmentedUserPrompt(text string) augmentedUserPrompt {
	trimmedText := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmedText, augmentedPromptPrefix) {
		return augmentedUserPrompt{
			UserQuery:        trimmedText,
			YouTubeContent:   "",
			RedditContent:    "",
			WebSearchResults: "",
		}
	}

	userQueryMarker := "\n\n" + userQuerySectionName + ":\n"

	_, remaining, found := strings.Cut(trimmedText, userQueryMarker)
	if !found {
		return augmentedUserPrompt{
			UserQuery:        trimmedText,
			YouTubeContent:   "",
			RedditContent:    "",
			WebSearchResults: "",
		}
	}

	sectionMatches := findPromptSectionMatches(remaining)
	if len(sectionMatches) == 0 {
		return augmentedUserPrompt{
			UserQuery:        strings.TrimSpace(remaining),
			YouTubeContent:   "",
			RedditContent:    "",
			WebSearchResults: "",
		}
	}

	prompt := augmentedUserPrompt{
		UserQuery:        strings.TrimSpace(remaining[:sectionMatches[0].Start]),
		YouTubeContent:   "",
		RedditContent:    "",
		WebSearchResults: "",
	}

	for index, match := range sectionMatches {
		sectionEnd := len(remaining)
		if index+1 < len(sectionMatches) {
			sectionEnd = sectionMatches[index+1].Start
		}

		setPromptSectionValue(
			&prompt,
			match.Name,
			remaining[match.Start+len(match.Marker):sectionEnd],
		)
	}

	return prompt
}

func appendYouTubeContentToConversation(
	conversation []chatMessage,
	formattedContent string,
) ([]chatMessage, error) {
	return appendContextToConversation(conversation, func(prompt *augmentedUserPrompt) {
		prompt.YouTubeContent = strings.TrimSpace(formattedContent)
	})
}

func appendRedditContentToConversation(
	conversation []chatMessage,
	formattedContent string,
) ([]chatMessage, error) {
	return appendContextToConversation(conversation, func(prompt *augmentedUserPrompt) {
		prompt.RedditContent = strings.TrimSpace(formattedContent)
	})
}

func appendWebSearchResultsToConversation(
	conversation []chatMessage,
	formattedResults string,
) ([]chatMessage, error) {
	return appendContextToConversation(conversation, func(prompt *augmentedUserPrompt) {
		prompt.WebSearchResults = strings.TrimSpace(formattedResults)
	})
}

func appendContextToConversation(
	conversation []chatMessage,
	transform func(*augmentedUserPrompt),
) ([]chatMessage, error) {
	augmentedConversation := make([]chatMessage, len(conversation))
	copy(augmentedConversation, conversation)

	for index := len(augmentedConversation) - 1; index >= 0; index-- {
		if augmentedConversation[index].Role != messageRoleUser {
			continue
		}

		updatedContent, err := appendContextToMessageContent(
			augmentedConversation[index].Content,
			transform,
		)
		if err != nil {
			return nil, fmt.Errorf("update latest user message: %w", err)
		}

		augmentedConversation[index].Content = updatedContent

		return augmentedConversation, nil
	}

	return nil, fmt.Errorf("find latest user message: %w", os.ErrNotExist)
}

func appendContextToMessageContent(
	content any,
	transform func(*augmentedUserPrompt),
) (any, error) {
	switch typedContent := content.(type) {
	case nil:
		prompt := augmentedUserPrompt{
			UserQuery:        "",
			YouTubeContent:   "",
			RedditContent:    "",
			WebSearchResults: "",
		}
		transform(&prompt)

		return prompt.render(), nil
	case string:
		prompt := parseAugmentedUserPrompt(typedContent)
		transform(&prompt)

		return prompt.render(), nil
	case []contentPart:
		prompt := parseAugmentedUserPrompt(contentPartsText(typedContent))
		transform(&prompt)

		updatedContent := make([]contentPart, 0, len(typedContent)+1)
		updatedContent = append(updatedContent, contentPart{
			"type": contentTypeText,
			"text": prompt.render(),
		})

		for _, part := range typedContent {
			if partType, _ := part["type"].(string); partType == contentTypeText {
				continue
			}

			updatedContent = append(updatedContent, part)
		}

		return updatedContent, nil
	default:
		return nil, fmt.Errorf("unsupported message content type %T: %w", content, os.ErrInvalid)
	}
}

type promptSection struct {
	Name                  string
	Value                 string
	StandaloneDescription string
	CombinedDescription   string
}

type promptSectionMatch struct {
	Name   string
	Start  int
	Marker string
}

func (prompt augmentedUserPrompt) activeSections() []promptSection {
	sections := make([]promptSection, 0, maxPromptSections)
	if trimmedValue := strings.TrimSpace(prompt.YouTubeContent); trimmedValue != "" {
		sections = append(sections, promptSection{
			Name:                  youtubeSectionName,
			Value:                 trimmedValue,
			StandaloneDescription: youtubeSectionDescription,
			CombinedDescription:   youtubeSectionDescription,
		})
	}

	if trimmedValue := strings.TrimSpace(prompt.RedditContent); trimmedValue != "" {
		sections = append(sections, promptSection{
			Name:                  redditSectionName,
			Value:                 trimmedValue,
			StandaloneDescription: redditSectionDescription,
			CombinedDescription:   redditSectionDescription,
		})
	}

	if trimmedValue := strings.TrimSpace(prompt.WebSearchResults); trimmedValue != "" {
		sections = append(sections, promptSection{
			Name:                  webSearchSectionName,
			Value:                 trimmedValue,
			StandaloneDescription: webSearchStandalonePrompt,
			CombinedDescription:   webSearchCombinedPrompt,
		})
	}

	return sections
}

func (section promptSection) descriptionForCount(sectionCount int) string {
	if sectionCount <= 1 {
		return section.StandaloneDescription
	}

	return section.CombinedDescription
}

func joinPromptDescriptions(descriptions []string) string {
	switch len(descriptions) {
	case 0:
		return ""
	case 1:
		return descriptions[0]
	case configuredModelParts:
		return descriptions[0] + " and " + descriptions[1]
	default:
		return strings.Join(descriptions[:len(descriptions)-1], ", ") +
			", and " + descriptions[len(descriptions)-1]
	}
}

func findPromptSectionMatches(text string) []promptSectionMatch {
	matches := make([]promptSectionMatch, 0, maxPromptSections)

	for _, sectionName := range []string{
		youtubeSectionName,
		redditSectionName,
		webSearchSectionName,
	} {
		marker := "\n\n" + sectionName + ":\n"

		start := strings.Index(text, marker)
		if start == -1 {
			continue
		}

		matches = append(matches, promptSectionMatch{
			Name:   sectionName,
			Start:  start,
			Marker: marker,
		})
	}

	for left := 0; left < len(matches); left++ {
		for right := left + 1; right < len(matches); right++ {
			if matches[right].Start < matches[left].Start {
				matches[left], matches[right] = matches[right], matches[left]
			}
		}
	}

	return matches
}

func setPromptSectionValue(prompt *augmentedUserPrompt, sectionName string, value string) {
	trimmedValue := strings.TrimSpace(value)

	switch sectionName {
	case youtubeSectionName:
		prompt.YouTubeContent = trimmedValue
	case redditSectionName:
		prompt.RedditContent = trimmedValue
	case webSearchSectionName:
		prompt.WebSearchResults = trimmedValue
	}
}
