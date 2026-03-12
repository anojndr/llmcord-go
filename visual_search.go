package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"golang.org/x/net/html"
)

const (
	defaultYandexVisualSearchEndpoint = "https://yandex.com/images/search"
	visualSearchWarningText           = "Warning: visual search unavailable"
	visualSearchImageWarningText      = "Warning: visual search needs an image attachment"
	visualSearchResponseByteLimit     = 4 * 1024 * 1024
	maxVisualSearchDescriptionRunes   = 500
	maxVisualSearchSnippetRunes       = 280
	maxVisualSearchTitleRunes         = 180
	maxVisualSearchTags               = 5
	maxVisualSearchSimilarItems       = 5
	maxVisualSearchSiteMatches        = 5
	visualSearchTopMatchLineCapacity  = 5
	defaultVisualSearchQuery          = "what is in this image?"
)

type visualSearchClient interface {
	search(ctx context.Context, imageURL string) (visualSearchResult, error)
}

type visualSearchTopMatch struct {
	Title       string
	Subtitle    string
	Description string
	Source      string
	URL         string
}

type visualSearchSimilarImage struct {
	Title string
	URL   string
}

type visualSearchSiteMatch struct {
	Title   string
	Domain  string
	Snippet string
	URL     string
}

type visualSearchResult struct {
	ImageURL      string
	SearchURL     string
	TopMatch      visualSearchTopMatch
	Tags          []string
	TextInImage   []string
	SimilarImages []visualSearchSimilarImage
	SiteMatches   []visualSearchSiteMatch
}

type yandexVisualSearchClient struct {
	endpoint   string
	httpClient *http.Client
	userAgent  string
}

func emptyVisualSearchTopMatch() visualSearchTopMatch {
	return visualSearchTopMatch{
		Title:       "",
		Subtitle:    "",
		Description: "",
		Source:      "",
		URL:         "",
	}
}

func emptyVisualSearchResult() visualSearchResult {
	return visualSearchResult{
		ImageURL:      "",
		SearchURL:     "",
		TopMatch:      emptyVisualSearchTopMatch(),
		Tags:          nil,
		TextInImage:   nil,
		SimilarImages: nil,
		SiteMatches:   nil,
	}
}

func newVisualSearchResult(imageURL string, searchURL string) visualSearchResult {
	result := emptyVisualSearchResult()
	result.ImageURL = strings.TrimSpace(imageURL)
	result.SearchURL = strings.TrimSpace(searchURL)

	return result
}

func newVisualSearchClient(httpClient *http.Client) yandexVisualSearchClient {
	return yandexVisualSearchClient{
		endpoint:   defaultYandexVisualSearchEndpoint,
		httpClient: httpClient,
		userAgent:  youtubeUserAgent,
	}
}

func (client yandexVisualSearchClient) search(
	ctx context.Context,
	imageURL string,
) (visualSearchResult, error) {
	requestURL, err := client.requestURL(imageURL)
	if err != nil {
		return emptyVisualSearchResult(), err
	}

	requestContext, cancel := context.WithTimeout(ctx, websiteRequestTimeout)
	defer cancel()

	httpRequest, err := http.NewRequestWithContext(
		requestContext,
		http.MethodGet,
		requestURL,
		nil,
	)
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"create visual search request %q: %w",
			requestURL,
			err,
		)
	}

	httpRequest.Header.Set(
		"Accept",
		"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.1",
	)
	httpRequest.Header.Set("Accept-Language", youtubeAcceptLanguage)
	httpRequest.Header.Set("User-Agent", client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"send visual search request %q: %w",
			requestURL,
			err,
		)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, visualSearchResponseByteLimit+1))
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"read visual search response %q: %w",
			requestURL,
			err,
		)
	}

	if len(responseBody) > visualSearchResponseByteLimit {
		return emptyVisualSearchResult(), fmt.Errorf(
			"visual search response %q exceeds %d bytes: %w",
			requestURL,
			visualSearchResponseByteLimit,
			os.ErrInvalid,
		)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return emptyVisualSearchResult(), fmt.Errorf(
			"visual search request %q failed with status %d: %s: %w",
			requestURL,
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	finalURL := requestURL
	if httpResponse.Request != nil && httpResponse.Request.URL != nil {
		finalURL = httpResponse.Request.URL.String()
	}

	result, err := parseYandexVisualSearchHTML(finalURL, imageURL, responseBody)
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf(
			"parse visual search HTML %q: %w",
			requestURL,
			err,
		)
	}

	return result, nil
}

func (client yandexVisualSearchClient) requestURL(imageURL string) (string, error) {
	parsedURL, err := neturl.Parse(client.endpoint)
	if err != nil {
		return "", fmt.Errorf("parse visual search endpoint %q: %w", client.endpoint, err)
	}

	queryValues := parsedURL.Query()
	queryValues.Set("rpt", "imageview")
	queryValues.Set("url", strings.TrimSpace(imageURL))
	parsedURL.RawQuery = queryValues.Encode()

	return parsedURL.String(), nil
}

func (instance *bot) maybeAugmentConversationWithVisualSearch(
	ctx context.Context,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) ([]chatMessage, []string, error) {
	if instance.visualSearch == nil {
		return conversation, nil, nil
	}

	latestUserQuery, err := latestUserPromptQuery(conversation)
	if err != nil {
		return nil, nil, fmt.Errorf("extract latest user query: %w", err)
	}

	rewrittenQuery, requested := rewriteVisualSearchUserQuery(latestUserQuery)
	if !requested {
		return conversation, nil, nil
	}

	rewrittenConversation, err := rewriteUserQueryInConversation(conversation, rewrittenQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("rewrite latest user query: %w", err)
	}

	imageURLs := instance.visualSearchImageURLs(ctx, sourceMessage)
	if len(imageURLs) == 0 {
		return rewrittenConversation, []string{visualSearchImageWarningText}, nil
	}

	augmentedConversation, warnings, err := augmentConversationWithConcurrentURLContent(
		ctx,
		rewrittenConversation,
		imageURLs,
		instance.visualSearch.search,
		"run visual search",
		visualSearchWarningText,
		formatVisualSearchResults,
		appendVisualSearchResultsToConversation,
		"append visual search results to conversation",
	)
	if err != nil {
		return nil, nil, err
	}

	return augmentedConversation, warnings, nil
}

func (instance *bot) visualSearchImageURLs(
	ctx context.Context,
	sourceMessage *discordgo.Message,
) []string {
	return imageAttachmentURLsForMessages(
		instance.attachmentAugmentationMessages(ctx, sourceMessage),
	)
}

func imageAttachmentURLsForMessages(messages []*discordgo.Message) []string {
	imageURLs := make([]string, 0)
	seenURLs := make(map[string]struct{})

	for _, message := range messages {
		if message == nil {
			continue
		}

		for _, attachment := range message.Attachments {
			if attachment == nil {
				continue
			}

			contentType := attachmentContentType(attachment)
			if !strings.HasPrefix(strings.TrimSpace(contentType), "image/") {
				continue
			}

			imageURL := strings.TrimSpace(attachment.URL)
			if imageURL == "" {
				continue
			}

			if _, ok := seenURLs[imageURL]; ok {
				continue
			}

			seenURLs[imageURL] = struct{}{}
			imageURLs = append(imageURLs, imageURL)
		}
	}

	return imageURLs
}

func rewriteVisualSearchUserQuery(query string) (string, bool) {
	prefix, body := splitUserMessagePrefix(query)
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(body)), "vsearch") {
		return query, false
	}

	trimmedBody := strings.TrimSpace(body)

	separatorIndex := visualSearchSeparatorIndex(trimmedBody)
	if separatorIndex == -1 {
		return query, false
	}

	rewrittenBody := strings.TrimSpace(trimmedBody[separatorIndex:])
	if rewrittenBody == "" {
		rewrittenBody = defaultVisualSearchQuery
	}

	if prefix == "" {
		return rewrittenBody, true
	}

	return strings.TrimSpace(prefix + rewrittenBody), true
}

func visualSearchSeparatorIndex(text string) int {
	if !strings.HasPrefix(strings.ToLower(text), "vsearch") {
		return -1
	}

	if len(text) == len("vsearch") {
		return len(text)
	}

	nextRune, size := utf8.DecodeRuneInString(text[len("vsearch"):])
	if nextRune == ' ' || nextRune == '\t' || nextRune == '\n' || strings.ContainsRune(",.:;!?-", nextRune) {
		return len("vsearch") + size
	}

	if nextRune == utf8.RuneError && size == 0 {
		return len(text)
	}

	if nextRune == utf8.RuneError && size == 1 {
		return -1
	}

	return -1
}

func parseYandexVisualSearchHTML(
	searchURL string,
	imageURL string,
	htmlBody []byte,
) (visualSearchResult, error) {
	document, err := html.Parse(bytes.NewReader(htmlBody))
	if err != nil {
		return emptyVisualSearchResult(), fmt.Errorf("parse HTML: %w", err)
	}

	result := newVisualSearchResult(imageURL, searchURL)

	objectResponse := findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirObjectResponse-Container")
	})
	if objectResponse != nil {
		result.TopMatch = parseVisualSearchTopMatch(searchURL, objectResponse)
	}

	tagsSection := findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirTags")
	})
	if tagsSection != nil {
		result.Tags = parseVisualSearchTags(tagsSection)
	}

	ocrSection := findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirOcr")
	})
	if ocrSection != nil {
		result.TextInImage = parseVisualSearchOCRText(ocrSection)
	}

	similarSection := findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirSimilarList")
	})
	if similarSection != nil {
		result.SimilarImages = parseVisualSearchSimilarImages(searchURL, similarSection)
	}

	sitesSection := findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirSitesList")
	})
	if sitesSection != nil {
		result.SiteMatches = parseVisualSearchSiteMatches(searchURL, sitesSection)
	}

	return result, nil
}

func parseVisualSearchTopMatch(searchURL string, root *html.Node) visualSearchTopMatch {
	topMatch := visualSearchTopMatch{
		Title: findVisualSearchNodeText(
			root,
			"CbirObjectResponse-Title",
			maxVisualSearchTitleRunes,
		),
		Subtitle: findVisualSearchNodeText(
			root,
			"CbirObjectResponse-Subtitle",
			maxVisualSearchTitleRunes,
		),
		Description: findVisualSearchNodeText(
			root,
			"CbirObjectResponse-Description",
			maxVisualSearchDescriptionRunes,
		),
		Source: "",
		URL:    "",
	}

	sourceLink := findWebsiteNode(root, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirObjectResponse-SourceLink")
	})
	if sourceLink != nil {
		topMatch.Source = truncateRunes(nodeTextContent(sourceLink), maxVisualSearchTitleRunes)
		topMatch.URL = resolveVisualSearchURL(searchURL, htmlAttribute(sourceLink, "href"))
	}

	if topMatch.URL == "" {
		thumbLink := findWebsiteNode(root, func(node *html.Node) bool {
			return hasHTMLClass(node, "CbirObjectResponse-Thumb")
		})
		topMatch.URL = resolveVisualSearchURL(searchURL, htmlAttribute(thumbLink, "href"))
	}

	return topMatch
}

func parseVisualSearchTags(root *html.Node) []string {
	tagNodes := findVisualSearchNodes(root, func(node *html.Node) bool {
		return hasHTMLClass(node, "Tags-Item")
	})

	tags := make([]string, 0, len(tagNodes))
	seenTags := make(map[string]struct{})

	for _, tagNode := range tagNodes {
		if tagNode.Type != html.ElementNode || tagNode.Data != atomA {
			continue
		}

		tagText := truncateRunes(nodeTextContent(tagNode), maxVisualSearchTitleRunes)
		if tagText == "" {
			continue
		}

		foldedText := strings.ToLower(tagText)
		if _, ok := seenTags[foldedText]; ok {
			continue
		}

		seenTags[foldedText] = struct{}{}

		tags = append(tags, tagText)
		if len(tags) == maxVisualSearchTags {
			break
		}
	}

	return tags
}

func parseVisualSearchOCRText(root *html.Node) []string {
	textNodes := findVisualSearchNodes(root, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirOcr-TextBox")
	})

	texts := make([]string, 0, len(textNodes))
	seenTexts := make(map[string]struct{})

	for _, textNode := range textNodes {
		text := truncateRunes(nodeTextContent(textNode), maxVisualSearchTitleRunes)
		if text == "" {
			continue
		}

		foldedText := strings.ToLower(text)
		if _, ok := seenTexts[foldedText]; ok {
			continue
		}

		seenTexts[foldedText] = struct{}{}

		texts = append(texts, text)
	}

	return texts
}

func parseVisualSearchSimilarImages(
	searchURL string,
	root *html.Node,
) []visualSearchSimilarImage {
	imageNodes := findVisualSearchNodes(root, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirSimilarList-ThumbImage")
	})

	items := make([]visualSearchSimilarImage, 0, len(imageNodes))
	seenTitles := make(map[string]struct{})

	for _, imageNode := range imageNodes {
		title := truncateRunes(nodeTextContent(imageNode), maxVisualSearchTitleRunes)
		if title == "" {
			title = truncateRunes(strings.TrimSpace(htmlAttribute(imageNode, "aria-label")), maxVisualSearchTitleRunes)
		}

		if title == "" {
			continue
		}

		foldedTitle := strings.ToLower(title)
		if _, ok := seenTitles[foldedTitle]; ok {
			continue
		}

		seenTitles[foldedTitle] = struct{}{}

		items = append(items, visualSearchSimilarImage{
			Title: title,
			URL:   resolveVisualSearchURL(searchURL, htmlAttribute(imageNode, "href")),
		})
		if len(items) == maxVisualSearchSimilarItems {
			break
		}
	}

	return items
}

func parseVisualSearchSiteMatches(
	searchURL string,
	root *html.Node,
) []visualSearchSiteMatch {
	itemNodes := findVisualSearchNodes(root, func(node *html.Node) bool {
		return hasHTMLClass(node, "CbirSites-Item")
	})

	items := make([]visualSearchSiteMatch, 0, len(itemNodes))
	seenURLs := make(map[string]struct{})

	for _, itemNode := range itemNodes {
		titleNode := findWebsiteNode(itemNode, func(node *html.Node) bool {
			return hasHTMLClass(node, "CbirSites-ItemTitle")
		})
		domainNode := findWebsiteNode(itemNode, func(node *html.Node) bool {
			return hasHTMLClass(node, "CbirSites-ItemDomain")
		})
		descriptionNode := findWebsiteNode(itemNode, func(node *html.Node) bool {
			return hasHTMLClass(node, "CbirSites-ItemDescription")
		})

		titleLink := firstVisualSearchLink(titleNode)

		itemURL := resolveVisualSearchURL(searchURL, htmlAttribute(titleLink, "href"))
		if itemURL == "" {
			continue
		}

		if _, ok := seenURLs[itemURL]; ok {
			continue
		}

		seenURLs[itemURL] = struct{}{}

		items = append(items, visualSearchSiteMatch{
			Title:   truncateRunes(nodeTextContent(titleNode), maxVisualSearchTitleRunes),
			Domain:  truncateRunes(nodeTextContent(domainNode), maxVisualSearchTitleRunes),
			Snippet: truncateRunes(nodeTextContent(descriptionNode), maxVisualSearchSnippetRunes),
			URL:     itemURL,
		})
		if len(items) == maxVisualSearchSiteMatches {
			break
		}
	}

	return items
}

func findVisualSearchNodes(
	root *html.Node,
	predicate func(*html.Node) bool,
) []*html.Node {
	nodes := make([]*html.Node, 0)

	var walk func(*html.Node)

	walk = func(node *html.Node) {
		if node == nil {
			return
		}

		if predicate(node) {
			nodes = append(nodes, node)
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(root)

	return nodes
}

func firstVisualSearchLink(root *html.Node) *html.Node {
	return findWebsiteNode(root, func(node *html.Node) bool {
		return node != nil && node.Type == html.ElementNode && node.Data == atomA
	})
}

func findVisualSearchNodeText(
	root *html.Node,
	className string,
	maxRunes int,
) string {
	return truncateRunes(
		nodeTextContent(findWebsiteNode(root, func(node *html.Node) bool {
			return hasHTMLClass(node, className)
		})),
		maxRunes,
	)
}

func nodeTextContent(root *html.Node) string {
	if root == nil {
		return ""
	}

	var builder strings.Builder

	var walk func(*html.Node)

	walk = func(node *html.Node) {
		if node == nil {
			return
		}

		if node.Type == html.TextNode {
			appendWebsiteTextChunk(&builder, node.Data)

			return
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(root)

	return strings.TrimSpace(builder.String())
}

func resolveVisualSearchURL(baseURL string, rawURL string) string {
	trimmedURL := strings.TrimSpace(rawURL)
	if trimmedURL == "" {
		return ""
	}

	base, err := neturl.Parse(baseURL)
	if err != nil {
		return trimmedURL
	}

	relative, err := neturl.Parse(trimmedURL)
	if err != nil {
		return trimmedURL
	}

	return base.ResolveReference(relative).String()
}

func formatVisualSearchResults(results []visualSearchResult) string {
	if len(results) == 0 {
		return "No visual search results found."
	}

	formattedResults := make([]string, 0, len(results))

	for index, result := range results {
		formattedResults = append(
			formattedResults,
			formatSingleVisualSearchResult(index, len(results), result),
		)
	}

	return strings.Join(formattedResults, "\n\n")
}

func formatSingleVisualSearchResult(
	index int,
	total int,
	result visualSearchResult,
) string {
	lines := make([]string, 0)

	if total > 1 {
		lines = append(lines, fmt.Sprintf("Image %d:", index+1))
	}

	lines = append(lines, formatVisualSearchTopMatch(result.TopMatch)...)

	if len(result.Tags) > 0 {
		lines = append(lines, "Image appears to contain: "+strings.Join(result.Tags, ", "))
	}

	if len(result.TextInImage) > 0 {
		lines = append(lines, "Text in image: "+strings.Join(result.TextInImage, " | "))
	}

	if similarImages := formatVisualSearchSimilarImages(result.SimilarImages); similarImages != "" {
		lines = append(lines, similarImages)
	}

	if siteMatches := formatVisualSearchSiteMatches(result.SiteMatches); siteMatches != "" {
		lines = append(lines, siteMatches)
	}

	if len(lines) == 0 {
		lines = append(lines, "No visual search results found.")
	}

	return strings.Join(lines, "\n")
}

func formatVisualSearchTopMatch(topMatch visualSearchTopMatch) []string {
	lines := make([]string, 0, visualSearchTopMatchLineCapacity)

	if title := strings.TrimSpace(topMatch.Title); title != "" {
		lines = append(lines, "Top match: "+title)
	}

	if subtitle := strings.TrimSpace(topMatch.Subtitle); subtitle != "" {
		lines = append(lines, "Top match type: "+subtitle)
	}

	if description := strings.TrimSpace(topMatch.Description); description != "" {
		lines = append(lines, "Top match description: "+description)
	}

	if source := strings.TrimSpace(topMatch.Source); source != "" {
		lines = append(lines, "Top match source: "+source)
	}

	if url := strings.TrimSpace(topMatch.URL); url != "" {
		lines = append(lines, "Top match URL: "+url)
	}

	return lines
}

func formatVisualSearchSimilarImages(items []visualSearchSimilarImage) string {
	if len(items) == 0 {
		return ""
	}

	lines := make([]string, 0, len(items)+1)
	lines = append(lines, "Similar images:")

	for itemIndex, item := range items {
		line := fmt.Sprintf("%d. %s", itemIndex+1, item.Title)
		if item.URL != "" {
			line += " <" + item.URL + ">"
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

func formatVisualSearchSiteMatches(items []visualSearchSiteMatch) string {
	if len(items) == 0 {
		return ""
	}

	lines := make([]string, 0, len(items)*3+1)
	lines = append(lines, "Sites with information about the image:")

	for itemIndex, item := range items {
		titleLine := fmt.Sprintf("%d. %s", itemIndex+1, item.Title)
		if item.Domain != "" && !strings.EqualFold(item.Title, item.Domain) {
			titleLine += " (" + item.Domain + ")"
		}

		lines = append(lines, titleLine)

		if item.Snippet != "" {
			lines = append(lines, "Snippet: "+item.Snippet)
		}

		if item.URL != "" {
			lines = append(lines, "URL: "+item.URL)
		}
	}

	return strings.Join(lines, "\n")
}

const atomA = "a"
