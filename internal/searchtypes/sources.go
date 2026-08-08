package searchtypes

import "strings"

// ExtractSearchSources parses "Title:"/"URL:" lines (as emitted by search
// formatting) into source entries.
func ExtractSearchSources(resultText string) []SearchSource {
	lines := strings.Split(resultText, "\n")
	sources := make([]SearchSource, 0)
	seenURLs := make(map[string]struct{})

	currentTitle := ""

	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmedLine, "Title:"):
			currentTitle = strings.TrimSpace(strings.TrimPrefix(trimmedLine, "Title:"))
		case strings.HasPrefix(trimmedLine, "URL:"):
			url := strings.TrimSpace(strings.TrimPrefix(trimmedLine, "URL:"))
			if url == "" {
				continue
			}

			foldedURL := strings.ToLower(url)
			if _, ok := seenURLs[foldedURL]; ok {
				currentTitle = ""

				continue
			}

			seenURLs[foldedURL] = struct{}{}

			title := currentTitle
			if title == "" {
				title = url
			}

			sources = append(sources, SearchSource{
				Title: title,
				URL:   url,
			})

			currentTitle = ""
		}
	}

	return sources
}
