package main

import (
	"fmt"
	"os"
	"strings"
)

const (
	augmentedPromptPrefix        = "Answer the user's query based on "
	mediaAnalysisCloseTag        = "</media_analysis>"
	mediaAnalysisOpenTag         = "<media_analysis>"
	replyTargetSectionName       = "Replied message"
	userQuerySectionName         = "User query"
	youtubeSectionName           = "YouTube URL content"
	redditSectionName            = "Reddit URL content"
	websiteSectionName           = "Website URL content"
	documentContentSectionName   = "Document attachment content"
	visualSearchSectionName      = "Visual search results"
	webSearchSectionName         = "Web search results"
	replyTargetDescription       = "the replied message"
	youtubeSectionDescription    = "the extracted YouTube URL content"
	redditSectionDescription     = "the extracted Reddit URL content"
	websiteSectionDescription    = "the extracted website URL content"
	documentSectionDescription   = "the extracted document attachment content"
	visualSearchStandalonePrompt = "the visual search results"
	visualSearchCombinedPrompt   = "visual search results"
	webSearchStandalonePrompt    = "the web search results"
	webSearchCombinedPrompt      = "web search results"
	maxPromptSections            = 7
)

type augmentedUserPrompt struct {
	RepliedMessage   string
	UserQuery        string
	YouTubeContent   string
	RedditContent    string
	WebsiteContent   string
	DocumentContent  string
	VisualSearch     string
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
			RepliedMessage:   "",
			UserQuery:        trimmedText,
			YouTubeContent:   "",
			RedditContent:    "",
			WebsiteContent:   "",
			DocumentContent:  "",
			VisualSearch:     "",
			WebSearchResults: "",
		}
	}

	userQueryMarker := "\n\n" + userQuerySectionName + ":\n"

	_, remaining, found := strings.Cut(trimmedText, userQueryMarker)
	if !found {
		return augmentedUserPrompt{
			RepliedMessage:   "",
			UserQuery:        trimmedText,
			YouTubeContent:   "",
			RedditContent:    "",
			WebsiteContent:   "",
			DocumentContent:  "",
			VisualSearch:     "",
			WebSearchResults: "",
		}
	}

	sectionMatches := findPromptSectionMatches(remaining)
	if len(sectionMatches) == 0 {
		return augmentedUserPrompt{
			RepliedMessage:   "",
			UserQuery:        strings.TrimSpace(remaining),
			YouTubeContent:   "",
			RedditContent:    "",
			WebsiteContent:   "",
			DocumentContent:  "",
			VisualSearch:     "",
			WebSearchResults: "",
		}
	}

	prompt := augmentedUserPrompt{
		RepliedMessage:   "",
		UserQuery:        strings.TrimSpace(remaining[:sectionMatches[0].Start]),
		YouTubeContent:   "",
		RedditContent:    "",
		WebsiteContent:   "",
		DocumentContent:  "",
		VisualSearch:     "",
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

func appendWebsiteContentToConversation(
	conversation []chatMessage,
	formattedContent string,
) ([]chatMessage, error) {
	return appendContextToConversation(conversation, func(prompt *augmentedUserPrompt) {
		prompt.WebsiteContent = strings.TrimSpace(formattedContent)
	})
}

func appendDocumentContentToConversation(
	conversation []chatMessage,
	formattedContent string,
) ([]chatMessage, error) {
	return appendContextToConversation(conversation, func(prompt *augmentedUserPrompt) {
		prompt.DocumentContent = strings.TrimSpace(formattedContent)
	})
}

func appendVisualSearchResultsToConversation(
	conversation []chatMessage,
	formattedResults string,
) ([]chatMessage, error) {
	return appendContextToConversation(conversation, func(prompt *augmentedUserPrompt) {
		prompt.VisualSearch = strings.TrimSpace(formattedResults)
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

func rewriteUserQueryInConversation(
	conversation []chatMessage,
	userQuery string,
) ([]chatMessage, error) {
	return appendContextToConversation(conversation, func(prompt *augmentedUserPrompt) {
		prompt.UserQuery = strings.TrimSpace(userQuery)
	})
}

func appendMediaAnalysesToConversation(
	conversation []chatMessage,
	analyses []string,
) ([]chatMessage, error) {
	renderedAnalyses := renderMediaAnalyses(analyses)
	if renderedAnalyses == "" {
		return conversation, nil
	}

	return appendContextToConversation(conversation, func(prompt *augmentedUserPrompt) {
		prompt.UserQuery = appendPromptUserQuery(prompt.UserQuery, renderedAnalyses)
	})
}

func appendReplyTargetToConversation(
	conversation []chatMessage,
	replyTarget chatMessage,
) ([]chatMessage, error) {
	replyText, replyMediaParts, err := replyTargetTextAndMedia(replyTarget.Content)
	if err != nil {
		return nil, fmt.Errorf("extract reply target content: %w", err)
	}

	augmentedConversation := conversation

	if replyText != "" {
		augmentedConversation, err = appendContextToConversation(
			augmentedConversation,
			func(prompt *augmentedUserPrompt) {
				prompt.RepliedMessage = replyText
			},
		)
		if err != nil {
			return nil, fmt.Errorf("append replied message text: %w", err)
		}
	}

	if len(replyMediaParts) == 0 {
		return augmentedConversation, nil
	}

	augmentedConversation, err = appendMediaPartsToConversation(
		augmentedConversation,
		replyMediaParts,
	)
	if err != nil {
		return nil, fmt.Errorf("append replied message media: %w", err)
	}

	return augmentedConversation, nil
}

func appendMediaPartsToConversation(
	conversation []chatMessage,
	mediaParts []contentPart,
) ([]chatMessage, error) {
	if len(mediaParts) == 0 {
		return conversation, nil
	}

	augmentedConversation := make([]chatMessage, len(conversation))
	copy(augmentedConversation, conversation)

	index, err := latestUserMessageIndex(augmentedConversation)
	if err != nil {
		return nil, fmt.Errorf("find latest user message: %w", err)
	}

	updatedContent, err := appendMediaPartsToMessageContent(
		augmentedConversation[index].Content,
		mediaParts,
	)
	if err != nil {
		return nil, fmt.Errorf("append media parts to latest user message: %w", err)
	}

	augmentedConversation[index].Content = updatedContent

	return augmentedConversation, nil
}

func appendContextToConversation(
	conversation []chatMessage,
	transform func(*augmentedUserPrompt),
) ([]chatMessage, error) {
	augmentedConversation := make([]chatMessage, len(conversation))
	copy(augmentedConversation, conversation)

	index, err := latestUserMessageIndex(augmentedConversation)
	if err != nil {
		return nil, fmt.Errorf("find latest user message: %w", err)
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

func latestUserMessageIndex(conversation []chatMessage) (int, error) {
	for index := len(conversation) - 1; index >= 0; index-- {
		if conversation[index].Role == messageRoleUser {
			return index, nil
		}
	}

	return 0, os.ErrNotExist
}

func appendContextToMessageContent(
	content any,
	transform func(*augmentedUserPrompt),
) (any, error) {
	switch typedContent := content.(type) {
	case nil:
		prompt := augmentedUserPrompt{
			RepliedMessage:   "",
			UserQuery:        "",
			YouTubeContent:   "",
			RedditContent:    "",
			WebsiteContent:   "",
			DocumentContent:  "",
			VisualSearch:     "",
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

func appendMediaPartsToMessageContent(
	content any,
	mediaParts []contentPart,
) (any, error) {
	clonedMediaParts := make([]contentPart, 0, len(mediaParts))
	for _, part := range mediaParts {
		clonedMediaParts = append(clonedMediaParts, cloneContentPart(part))
	}

	switch typedContent := content.(type) {
	case nil:
		return clonedMediaParts, nil
	case string:
		updatedContent := make([]contentPart, 0, len(clonedMediaParts)+1)
		updatedContent = append(updatedContent, contentPart{
			"type": contentTypeText,
			"text": typedContent,
		})
		updatedContent = append(updatedContent, clonedMediaParts...)

		return updatedContent, nil
	case []contentPart:
		updatedContent := make([]contentPart, 0, len(typedContent)+len(clonedMediaParts))
		for _, part := range typedContent {
			updatedContent = append(updatedContent, cloneContentPart(part))
		}

		updatedContent = append(updatedContent, clonedMediaParts...)

		return updatedContent, nil
	default:
		return nil, fmt.Errorf("unsupported message content type %T: %w", content, os.ErrInvalid)
	}
}

func replyTargetTextAndMedia(content any) (string, []contentPart, error) {
	switch typedContent := content.(type) {
	case nil:
		return "", nil, nil
	case string:
		return strings.TrimSpace(typedContent), nil, nil
	case []contentPart:
		replyMediaParts := make([]contentPart, 0, len(typedContent))

		for _, part := range typedContent {
			partType, _ := part["type"].(string)
			if partType == contentTypeText {
				continue
			}

			replyMediaParts = append(replyMediaParts, cloneContentPart(part))
		}

		return strings.TrimSpace(contentPartsText(typedContent)), replyMediaParts, nil
	default:
		return "", nil, fmt.Errorf("unsupported reply target content type %T: %w", content, os.ErrInvalid)
	}
}

func renderMediaAnalyses(analyses []string) string {
	blocks := make([]string, 0, len(analyses))

	for _, analysis := range analyses {
		trimmedAnalysis := strings.TrimSpace(analysis)
		if trimmedAnalysis == "" {
			continue
		}

		blocks = append(
			blocks,
			mediaAnalysisOpenTag+"\n"+trimmedAnalysis+"\n"+mediaAnalysisCloseTag,
		)
	}

	return strings.Join(blocks, "\n\n")
}

func appendPromptUserQuery(userQuery string, addition string) string {
	trimmedQuery := strings.TrimSpace(userQuery)
	trimmedAddition := strings.TrimSpace(addition)

	if trimmedAddition == "" {
		return trimmedQuery
	}

	if trimmedQuery == "" {
		return trimmedAddition
	}

	return trimmedQuery + "\n\n" + trimmedAddition
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
	if trimmedValue := strings.TrimSpace(prompt.RepliedMessage); trimmedValue != "" {
		sections = append(sections, promptSection{
			Name:                  replyTargetSectionName,
			Value:                 trimmedValue,
			StandaloneDescription: replyTargetDescription,
			CombinedDescription:   replyTargetDescription,
		})
	}

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

	if trimmedValue := strings.TrimSpace(prompt.WebsiteContent); trimmedValue != "" {
		sections = append(sections, promptSection{
			Name:                  websiteSectionName,
			Value:                 trimmedValue,
			StandaloneDescription: websiteSectionDescription,
			CombinedDescription:   websiteSectionDescription,
		})
	}

	if trimmedValue := strings.TrimSpace(prompt.DocumentContent); trimmedValue != "" {
		sections = append(sections, promptSection{
			Name:                  documentContentSectionName,
			Value:                 trimmedValue,
			StandaloneDescription: documentSectionDescription,
			CombinedDescription:   documentSectionDescription,
		})
	}

	if trimmedValue := strings.TrimSpace(prompt.VisualSearch); trimmedValue != "" {
		sections = append(sections, promptSection{
			Name:                  visualSearchSectionName,
			Value:                 trimmedValue,
			StandaloneDescription: visualSearchStandalonePrompt,
			CombinedDescription:   visualSearchCombinedPrompt,
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
		replyTargetSectionName,
		youtubeSectionName,
		redditSectionName,
		websiteSectionName,
		documentContentSectionName,
		visualSearchSectionName,
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
	case replyTargetSectionName:
		prompt.RepliedMessage = trimmedValue
	case youtubeSectionName:
		prompt.YouTubeContent = trimmedValue
	case redditSectionName:
		prompt.RedditContent = trimmedValue
	case websiteSectionName:
		prompt.WebsiteContent = trimmedValue
	case documentContentSectionName:
		prompt.DocumentContent = trimmedValue
	case visualSearchSectionName:
		prompt.VisualSearch = trimmedValue
	case webSearchSectionName:
		prompt.WebSearchResults = trimmedValue
	}
}
