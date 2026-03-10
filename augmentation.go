package main

import (
	"fmt"
	"os"
	"strings"
)

const (
	youtubeAnswerTemplate = `Answer the user query based on the extracted YouTube URL content.

User query:
%s

YouTube URL content:
%s`
	youtubeSearchAnswerTemplate = `Answer the user query based on the extracted YouTube URL content and web search results.

User query:
%s

YouTube URL content:
%s

Web search results:
%s`
)

type augmentedUserPrompt struct {
	UserQuery        string
	YouTubeContent   string
	WebSearchResults string
}

func (prompt augmentedUserPrompt) render() string {
	trimmedUserQuery := strings.TrimSpace(prompt.UserQuery)
	trimmedYouTubeContent := strings.TrimSpace(prompt.YouTubeContent)
	trimmedWebSearchResults := strings.TrimSpace(prompt.WebSearchResults)

	switch {
	case trimmedYouTubeContent != "" && trimmedWebSearchResults != "":
		return fmt.Sprintf(
			youtubeSearchAnswerTemplate,
			trimmedUserQuery,
			trimmedYouTubeContent,
			trimmedWebSearchResults,
		)
	case trimmedYouTubeContent != "":
		return fmt.Sprintf(
			youtubeAnswerTemplate,
			trimmedUserQuery,
			trimmedYouTubeContent,
		)
	case trimmedWebSearchResults != "":
		return fmt.Sprintf(
			searchAnswerTemplate,
			trimmedUserQuery,
			trimmedWebSearchResults,
		)
	default:
		return trimmedUserQuery
	}
}

func parseAugmentedUserPrompt(text string) augmentedUserPrompt {
	trimmedText := strings.TrimSpace(text)

	if prompt, ok := parseYouTubeSearchPrompt(trimmedText); ok {
		return prompt
	}

	if prompt, ok := parseYouTubePrompt(trimmedText); ok {
		return prompt
	}

	if prompt, ok := parseWebSearchPrompt(trimmedText); ok {
		return prompt
	}

	return augmentedUserPrompt{
		UserQuery:        trimmedText,
		YouTubeContent:   "",
		WebSearchResults: "",
	}
}

func parseYouTubeSearchPrompt(text string) (augmentedUserPrompt, bool) {
	return parseThreeSectionPrompt(
		text,
		"Answer the user query based on the extracted YouTube URL content and web search results.",
		"YouTube URL content",
		"Web search results",
	)
}

func parseYouTubePrompt(text string) (augmentedUserPrompt, bool) {
	intro := "Answer the user query based on the extracted YouTube URL content."

	prompt, ok := parseTwoSectionPrompt(text, intro, "YouTube URL content")
	if !ok {
		return augmentedUserPrompt{
			UserQuery:        "",
			YouTubeContent:   "",
			WebSearchResults: "",
		}, false
	}

	return prompt, true
}

func parseWebSearchPrompt(text string) (augmentedUserPrompt, bool) {
	intro := "Answer the user query based on the web search results."

	prompt, ok := parseTwoSectionPrompt(text, intro, "Web search results")
	if !ok {
		return augmentedUserPrompt{
			UserQuery:        "",
			YouTubeContent:   "",
			WebSearchResults: "",
		}, false
	}

	return prompt, true
}

func parseTwoSectionPrompt(
	text string,
	intro string,
	sectionName string,
) (augmentedUserPrompt, bool) {
	prefix := intro + "\n\nUser query:\n"
	if !strings.HasPrefix(text, prefix) {
		return augmentedUserPrompt{
			UserQuery:        "",
			YouTubeContent:   "",
			WebSearchResults: "",
		}, false
	}

	remaining := strings.TrimPrefix(text, prefix)

	userQuery, sectionContent, ok := strings.Cut(
		remaining,
		"\n\n"+sectionName+":\n",
	)
	if !ok {
		return augmentedUserPrompt{
			UserQuery:        "",
			YouTubeContent:   "",
			WebSearchResults: "",
		}, false
	}

	prompt := augmentedUserPrompt{
		UserQuery:        strings.TrimSpace(userQuery),
		YouTubeContent:   "",
		WebSearchResults: "",
	}

	switch sectionName {
	case "YouTube URL content":
		prompt.YouTubeContent = strings.TrimSpace(sectionContent)
	case "Web search results":
		prompt.WebSearchResults = strings.TrimSpace(sectionContent)
	default:
		return augmentedUserPrompt{
			UserQuery:        "",
			YouTubeContent:   "",
			WebSearchResults: "",
		}, false
	}

	return prompt, true
}

func parseThreeSectionPrompt(
	text string,
	intro string,
	secondSectionName string,
	thirdSectionName string,
) (augmentedUserPrompt, bool) {
	prefix := intro + "\n\nUser query:\n"
	if !strings.HasPrefix(text, prefix) {
		return augmentedUserPrompt{
			UserQuery:        "",
			YouTubeContent:   "",
			WebSearchResults: "",
		}, false
	}

	remaining := strings.TrimPrefix(text, prefix)

	userQuery, remaining, found := strings.Cut(
		remaining,
		"\n\n"+secondSectionName+":\n",
	)
	if !found {
		return augmentedUserPrompt{
			UserQuery:        "",
			YouTubeContent:   "",
			WebSearchResults: "",
		}, false
	}

	secondSection, thirdSection, found := strings.Cut(
		remaining,
		"\n\n"+thirdSectionName+":\n",
	)
	if !found {
		return augmentedUserPrompt{
			UserQuery:        "",
			YouTubeContent:   "",
			WebSearchResults: "",
		}, false
	}

	return augmentedUserPrompt{
		UserQuery:        strings.TrimSpace(userQuery),
		YouTubeContent:   strings.TrimSpace(secondSection),
		WebSearchResults: strings.TrimSpace(thirdSection),
	}, true
}

func appendYouTubeContentToConversation(
	conversation []chatMessage,
	formattedContent string,
) ([]chatMessage, error) {
	return appendContextToConversation(conversation, func(prompt *augmentedUserPrompt) {
		prompt.YouTubeContent = strings.TrimSpace(formattedContent)
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
			if partType, _ := part["type"].(string); partType != contentTypeImageURL {
				continue
			}

			updatedContent = append(updatedContent, part)
		}

		return updatedContent, nil
	default:
		return nil, fmt.Errorf("unsupported message content type %T: %w", content, os.ErrInvalid)
	}
}
