package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/publicsuffix"
)

const (
	websiteWarningText                  = "Warning: website content unavailable"
	maxWebsiteContentRunes              = 12000
	maxWebsiteDescriptionRunes          = 500
	maxWebsiteResponseBytes             = 2 * 1024 * 1024
	minimumWebsiteContentSelectionRunes = 300
	websiteContentCandidateCapacity     = 7
	websiteSegmentCapacity              = 32
)

var websiteURLRegexp = regexp.MustCompile(
	`(?i)\b(?:https?://)?(?:[\w-]+\.)+[a-z]{2,}(?:/[^\s<>]*)?`,
)

type websiteContentClient interface {
	fetch(ctx context.Context, rawURL string) (websitePageContent, error)
}

type websiteClient struct {
	httpClient *http.Client
	userAgent  string
}

type websitePageContent struct {
	URL         string
	Title       string
	Description string
	Content     string
}

func newWebsiteClient(httpClient *http.Client) websiteClient {
	return websiteClient{
		httpClient: httpClient,
		userAgent:  youtubeUserAgent,
	}
}

func (instance *bot) maybeAugmentConversationWithWebsite(
	ctx context.Context,
	conversation []chatMessage,
	urlExtractionText string,
) ([]chatMessage, []string, error) {
	if instance.website == nil {
		return conversation, nil, nil
	}

	websiteURLs := extractWebsiteURLs(urlExtractionText)
	if len(websiteURLs) == 0 {
		return conversation, nil, nil
	}

	augmentedConversation, _, warnings, err := augmentConversationWithConcurrentURLContent(
		ctx,
		conversation,
		websiteURLs,
		instance.website.fetch,
		"fetch website content",
		websiteWarningText,
		formatWebsiteURLContent,
		appendWebsiteContentToConversation,
		"append website content to conversation",
	)

	return augmentedConversation, warnings, err
}

func latestUserPromptQuery(conversation []chatMessage) (string, error) {
	text, err := latestUserMessageText(conversation)
	if err != nil {
		return "", err
	}

	return parseAugmentedUserPrompt(text).UserQuery, nil
}

func (client websiteClient) fetch(ctx context.Context, rawURL string) (websitePageContent, error) {
	normalizedURL, err := normalizeWebsiteURL(rawURL)
	if err != nil {
		return websitePageContent{}, err
	}

	requestContext, cancel := context.WithTimeout(ctx, websiteRequestTimeout)
	defer cancel()

	responseBody, responseURL, contentType, err := client.doRequest(requestContext, normalizedURL)
	if err != nil {
		return websitePageContent{}, fmt.Errorf("fetch website %q: %w", rawURL, err)
	}

	switch {
	case isHTMLContentType(contentType):
		pageContent, parseErr := parseWebsiteHTML(responseURL, responseBody)
		if parseErr != nil {
			return websitePageContent{}, fmt.Errorf("parse website html %q: %w", rawURL, parseErr)
		}

		return pageContent, nil
	case isPlainTextContentType(contentType):
		content := truncateRunes(strings.TrimSpace(string(responseBody)), maxWebsiteContentRunes)
		if content == "" {
			return websitePageContent{}, fmt.Errorf("empty website text for %q: %w", rawURL, os.ErrInvalid)
		}

		return websitePageContent{
			URL:         responseURL,
			Title:       responseURL,
			Description: "",
			Content:     content,
		}, nil
	default:
		return websitePageContent{}, fmt.Errorf(
			"unsupported website content type %q for %q: %w",
			contentType,
			rawURL,
			os.ErrInvalid,
		)
	}
}

func (client websiteClient) doRequest(
	ctx context.Context,
	requestURL string,
) ([]byte, string, string, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("create website request %q: %w", requestURL, err)
	}

	httpRequest.Header.Set(
		"Accept",
		"text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.1",
	)
	httpRequest.Header.Set("Accept-Language", youtubeAcceptLanguage)
	httpRequest.Header.Set("User-Agent", client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return nil, "", "", fmt.Errorf("send website request %q: %w", requestURL, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxWebsiteResponseBytes+1))
	if err != nil {
		return nil, "", "", fmt.Errorf("read website response %q: %w", requestURL, err)
	}

	if len(responseBody) > maxWebsiteResponseBytes {
		return nil, "", "", fmt.Errorf(
			"website response %q exceeds %d bytes: %w",
			requestURL,
			maxWebsiteResponseBytes,
			os.ErrInvalid,
		)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, "", "", fmt.Errorf(
			"website request %q failed with status %d: %s: %w",
			requestURL,
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	responseURL := requestURL
	if httpResponse.Request != nil && httpResponse.Request.URL != nil {
		responseURL = httpResponse.Request.URL.String()
	}

	return responseBody, responseURL, httpResponse.Header.Get("Content-Type"), nil
}

func extractWebsiteURLs(text string) []string {
	text = normalizedURLExtractionText(text)

	matchIndices := websiteURLRegexp.FindAllStringIndex(text, -1)
	normalizedURLs := make([]string, 0, len(matchIndices))
	seenURLs := make(map[string]struct{}, len(matchIndices))

	for _, matchIndex := range matchIndices {
		rawURL := text[matchIndex[0]:matchIndex[1]]

		normalizedURL, err := normalizeWebsiteURL(rawURL)
		if err != nil || !shouldExtractWebsiteURL(text, matchIndex[0], rawURL, normalizedURL) {
			continue
		}

		if _, ok := seenURLs[normalizedURL]; ok {
			continue
		}

		seenURLs[normalizedURL] = struct{}{}
		normalizedURLs = append(normalizedURLs, normalizedURL)
	}

	return normalizedURLs
}

func shouldExtractWebsiteURL(
	text string,
	matchStart int,
	rawURL string,
	normalizedURL string,
) bool {
	if hasExplicitWebsiteScheme(rawURL) {
		return true
	}

	if matchStart > 0 && text[matchStart-1] == '@' {
		return false
	}

	parsedURL, err := url.Parse(normalizedURL)
	if err != nil {
		return false
	}

	return hasRecognizedWebsiteSuffix(parsedURL.Hostname())
}

func hasExplicitWebsiteScheme(rawURL string) bool {
	trimmedURL := strings.ToLower(strings.TrimSpace(rawURL))

	return strings.HasPrefix(trimmedURL, "http://") ||
		strings.HasPrefix(trimmedURL, "https://")
}

func hasRecognizedWebsiteSuffix(host string) bool {
	normalizedHost := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if normalizedHost == "" {
		return false
	}

	suffix, icann := publicsuffix.PublicSuffix(normalizedHost)

	return icann || strings.Contains(suffix, ".")
}

func normalizeWebsiteURL(rawURL string) (string, error) {
	cleanedURL := cleanWebsiteURL(rawURL)

	if cleanedURL == "" {
		return "", fmt.Errorf("empty website url: %w", os.ErrInvalid)
	}

	if !strings.Contains(cleanedURL, "://") {
		cleanedURL = "https://" + cleanedURL
	}

	parsedURL, err := url.Parse(cleanedURL)
	if err != nil {
		return "", fmt.Errorf("parse website url %q: %w", rawURL, err)
	}

	if !isWebsiteScheme(parsedURL.Scheme) || strings.TrimSpace(parsedURL.Hostname()) == "" {
		return "", fmt.Errorf("unsupported website url %q: %w", rawURL, os.ErrInvalid)
	}

	if isExcludedWebsiteHost(parsedURL.Hostname()) {
		return "", fmt.Errorf("excluded website host in %q: %w", rawURL, os.ErrInvalid)
	}

	parsedURL.Scheme = strings.ToLower(parsedURL.Scheme)
	parsedURL.Host = strings.ToLower(parsedURL.Host)
	parsedURL.Fragment = ""

	return parsedURL.String(), nil
}

func cleanWebsiteURL(rawURL string) string {
	cleanedURL := strings.TrimSpace(rawURL)
	cleanedURL = strings.Trim(cleanedURL, `"'<>[]`)
	cleanedURL = strings.TrimRight(cleanedURL, ".,!?;:")

	for strings.HasSuffix(cleanedURL, ")") &&
		strings.Count(cleanedURL, "(") < strings.Count(cleanedURL, ")") {
		cleanedURL = strings.TrimSuffix(cleanedURL, ")")
	}

	return cleanedURL
}

func isWebsiteScheme(scheme string) bool {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http", "https":
		return true
	default:
		return false
	}
}

func isExcludedWebsiteHost(host string) bool {
	return isTikTokHost(host) ||
		isYouTubeHost(host) ||
		isRedditHost(host) ||
		isFacebookHost(host)
}

func isTikTokHost(host string) bool {
	normalizedHost := strings.ToLower(strings.TrimSpace(host))
	normalizedHost = strings.TrimPrefix(normalizedHost, "www.")

	return normalizedHost == "tiktok.com" ||
		normalizedHost == "tnktok.com" ||
		strings.HasSuffix(normalizedHost, ".tiktok.com") ||
		strings.HasSuffix(normalizedHost, ".tnktok.com")
}

func isYouTubeHost(host string) bool {
	normalizedHost := strings.ToLower(strings.TrimSpace(host))
	normalizedHost = strings.TrimPrefix(normalizedHost, "www.")

	return normalizedHost == "youtu.be" ||
		normalizedHost == "youtube.com" ||
		normalizedHost == "youtube-nocookie.com" ||
		strings.HasSuffix(normalizedHost, ".youtube.com") ||
		strings.HasSuffix(normalizedHost, ".youtube-nocookie.com")
}

func isFacebookHost(host string) bool {
	normalizedHost := strings.ToLower(strings.TrimSpace(host))
	normalizedHost = strings.TrimPrefix(normalizedHost, "www.")

	return normalizedHost == "facebook.com" ||
		normalizedHost == "fb.watch" ||
		strings.HasSuffix(normalizedHost, ".facebook.com") ||
		strings.HasSuffix(normalizedHost, ".fb.watch")
}

func isHTMLContentType(contentType string) bool {
	trimmedContentType := strings.ToLower(strings.TrimSpace(contentType))

	return trimmedContentType == "" ||
		strings.HasPrefix(trimmedContentType, "text/html") ||
		strings.HasPrefix(trimmedContentType, "application/xhtml+xml")
}

func isPlainTextContentType(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/plain")
}

func parseWebsiteHTML(pageURL string, responseBody []byte) (websitePageContent, error) {
	document, err := html.Parse(bytes.NewReader(responseBody))
	if err != nil {
		return websitePageContent{}, fmt.Errorf("parse html document: %w", err)
	}

	title := normalizeWebsiteText(extractWebsiteTitle(document))
	description := truncateRunes(
		normalizeWebsiteText(extractWebsiteDescription(document)),
		maxWebsiteDescriptionRunes,
	)

	content := truncateRunes(extractWebsiteBodyText(document), maxWebsiteContentRunes)
	if content == "" {
		content = description
	}

	if content == "" {
		return websitePageContent{}, fmt.Errorf("extract website content: %w", os.ErrInvalid)
	}

	if title == "" {
		title = pageURL
	}

	return websitePageContent{
		URL:         pageURL,
		Title:       title,
		Description: description,
		Content:     content,
	}, nil
}

func extractWebsiteTitle(document *html.Node) string {
	titleNode := findWebsiteNode(document, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.DataAtom == atom.Title
	})
	if titleNode == nil || titleNode.FirstChild == nil {
		return ""
	}

	return titleNode.FirstChild.Data
}

func extractWebsiteDescription(document *html.Node) string {
	metaNode := findWebsiteNode(document, func(node *html.Node) bool {
		if node.Type != html.ElementNode || node.DataAtom != atom.Meta {
			return false
		}

		name := strings.ToLower(strings.TrimSpace(htmlAttribute(node, "name")))
		property := strings.ToLower(strings.TrimSpace(htmlAttribute(node, "property")))

		return name == "description" || property == "og:description" || name == "twitter:description"
	})
	if metaNode == nil {
		return ""
	}

	return htmlAttribute(metaNode, "content")
}

func extractWebsiteBodyText(document *html.Node) string {
	candidates := websiteContentCandidates(document)
	fallback := ""

	for index, candidate := range candidates {
		text := renderWebsiteText(candidate)
		if text == "" {
			continue
		}

		if index == len(candidates)-1 || runeCount(text) >= minimumWebsiteContentSelectionRunes {
			return text
		}

		if fallback == "" || runeCount(text) > runeCount(fallback) {
			fallback = text
		}
	}

	return fallback
}

func websiteContentCandidates(document *html.Node) []*html.Node {
	candidates := make([]*html.Node, 0, websiteContentCandidateCapacity)

	appendCandidate := func(candidate *html.Node) {
		if candidate == nil || slices.Contains(candidates, candidate) {
			return
		}

		candidates = append(candidates, candidate)
	}

	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return htmlAttribute(node, "id") == "mw-content-text"
	}))
	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return hasHTMLClass(node, "mw-parser-output")
	}))
	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.DataAtom == atom.Article
	}))
	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.DataAtom == atom.Main
	}))
	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return strings.EqualFold(htmlAttribute(node, "role"), "main")
	}))
	appendCandidate(findWebsiteNode(document, hasWebsiteContentHint))
	appendCandidate(findWebsiteNode(document, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.DataAtom == atom.Body
	}))

	return candidates
}

func hasWebsiteContentHint(node *html.Node) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}

	if node.DataAtom != atom.Div && node.DataAtom != atom.Section {
		return false
	}

	for _, keyword := range []string{
		"article",
		"content",
		"entry",
		"main",
		"post",
		"story",
		"wiki",
	} {
		if containsFold(htmlAttribute(node, "id"), keyword) ||
			containsFold(htmlAttribute(node, "class"), keyword) {
			return true
		}
	}

	return false
}

func findWebsiteNode(node *html.Node, predicate func(*html.Node) bool) *html.Node {
	if node == nil {
		return nil
	}

	if predicate(node) {
		return node
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		match := findWebsiteNode(child, predicate)
		if match != nil {
			return match
		}
	}

	return nil
}

func renderWebsiteText(root *html.Node) string {
	segments := make([]string, 0, websiteSegmentCapacity)

	var current strings.Builder

	flush := func() {
		text := normalizeWebsiteText(current.String())
		if text == "" {
			current.Reset()

			return
		}

		segments = append(segments, text)

		current.Reset()
	}

	var walk func(*html.Node)

	walk = func(node *html.Node) {
		if node == nil {
			return
		}

		if node.Type == html.TextNode {
			appendWebsiteTextChunk(&current, node.Data)

			return
		}

		if node.Type != html.ElementNode {
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				walk(child)
			}

			return
		}

		if shouldSkipWebsiteNode(node) {
			return
		}

		if node.DataAtom == atom.Br || isWebsiteBlockNode(node) {
			flush()
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}

		if isWebsiteBlockNode(node) {
			flush()
		}
	}

	walk(root)
	flush()

	return truncateRunes(strings.Join(segments, "\n"), maxWebsiteContentRunes)
}

func appendWebsiteTextChunk(builder *strings.Builder, rawText string) {
	text := normalizeWebsiteText(rawText)
	if text == "" {
		return
	}

	if builder.Len() > 0 {
		builder.WriteByte(' ')
	}

	builder.WriteString(text)
}

func normalizeWebsiteText(text string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
}

func shouldSkipWebsiteNode(node *html.Node) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}

	if hasHTMLAttribute(node, "hidden") || strings.EqualFold(htmlAttribute(node, "aria-hidden"), "true") {
		return true
	}

	if containsFold(htmlAttribute(node, "style"), "display:none") ||
		containsFold(htmlAttribute(node, "style"), "visibility:hidden") {
		return true
	}

	if isIgnoredWebsiteAtom(node.DataAtom) {
		return true
	}

	for _, keyword := range []string{
		"advert",
		"banner",
		"breadcrumb",
		"breadcrumbs",
		"comment",
		"comments",
		"consent",
		"cookie",
		"footer",
		"header",
		"infobox",
		"modal",
		"nav",
		"navbox",
		"reference",
		"references",
		"related",
		"share",
		"sidebar",
		"social",
		"subscribe",
		"toc",
	} {
		if containsFold(htmlAttribute(node, "id"), keyword) ||
			containsFold(htmlAttribute(node, "class"), keyword) {
			return true
		}
	}

	return false
}

func isIgnoredWebsiteAtom(dataAtom atom.Atom) bool {
	return slices.Contains([]atom.Atom{
		atom.Aside,
		atom.Button,
		atom.Canvas,
		atom.Datalist,
		atom.Dialog,
		atom.Figure,
		atom.Footer,
		atom.Form,
		atom.Header,
		atom.Iframe,
		atom.Img,
		atom.Input,
		atom.Label,
		atom.Menu,
		atom.Nav,
		atom.Noscript,
		atom.Object,
		atom.Option,
		atom.Picture,
		atom.Script,
		atom.Select,
		atom.Style,
		atom.Sup,
		atom.Svg,
		atom.Textarea,
		atom.Video,
	}, dataAtom)
}

func isWebsiteBlockNode(node *html.Node) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}

	return slices.Contains([]atom.Atom{
		atom.Article,
		atom.Blockquote,
		atom.Div,
		atom.H1,
		atom.H2,
		atom.H3,
		atom.H4,
		atom.H5,
		atom.H6,
		atom.Li,
		atom.Main,
		atom.Ol,
		atom.P,
		atom.Pre,
		atom.Section,
		atom.Table,
		atom.Tbody,
		atom.Td,
		atom.Th,
		atom.Thead,
		atom.Tr,
		atom.Ul,
	}, node.DataAtom)
}

func htmlAttribute(node *html.Node, key string) string {
	if node == nil {
		return ""
	}

	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, key) {
			return attribute.Val
		}
	}

	return ""
}

func hasHTMLAttribute(node *html.Node, key string) bool {
	return htmlAttribute(node, key) != ""
}

func hasHTMLClass(node *html.Node, className string) bool {
	return slices.Contains(strings.Fields(htmlAttribute(node, "class")), className)
}

func formatWebsiteURLContent(contents []websitePageContent) string {
	formattedContents := make([]string, 0, len(contents))

	for _, content := range contents {
		lines := []string{
			"URL: " + content.URL,
			"Title: " + content.Title,
		}

		if trimmedDescription := strings.TrimSpace(content.Description); trimmedDescription != "" {
			lines = append(lines, "Description: "+trimmedDescription)
		}

		pageContent := strings.TrimSpace(content.Content)
		if pageContent == "" {
			pageContent = "[No extracted content]"
		}

		lines = append(lines, "Content:\n"+pageContent)
		formattedContents = append(formattedContents, strings.Join(lines, "\n"))
	}

	return strings.Join(formattedContents, "\n\n")
}
