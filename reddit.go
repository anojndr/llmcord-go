package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRedditBaseURL  = "https://www.reddit.com"
	redditWarningText     = "Warning: Reddit content unavailable"
	redditDefaultDepth    = "10"
	redditDefaultLimit    = "500"
	redditRawJSONValue    = "1"
	minimumRedditListings = 2
)

var redditURLRegexp = regexp.MustCompile(
	`(?i)\b(?:https?://)?(?:[\w-]+\.)?reddit\.com/[^\s<>()]+`,
)

type redditContentClient interface {
	fetch(ctx context.Context, rawURL string) (redditThreadContent, error)
}

type redditClient struct {
	httpClient *http.Client
	baseURL    string
	userAgent  string
}

type redditThreadRequest struct {
	ThreadPath string
	JSONPath   string
}

type redditThreadContent struct {
	URL         string
	JSONURL     string
	Subreddit   string
	Title       string
	Author      string
	Body        string
	Score       int
	UpvoteRatio float64
	NumComments int
	CreatedUTC  float64
	LinkedURL   string
	Comments    []redditThreadComment
}

type redditThreadComment struct {
	Author     string
	Body       string
	Score      int
	CreatedUTC float64
	Permalink  string
	Replies    []redditThreadComment
}

type redditListing struct {
	Data redditListingData `json:"data"`
}

type redditListingData struct {
	Children []redditThing `json:"children"`
}

type redditThing struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type redditPostData struct {
	Author                string  `json:"author"`
	CreatedUTC            float64 `json:"created_utc"`
	NumComments           int     `json:"num_comments"`
	Permalink             string  `json:"permalink"`
	Score                 int     `json:"score"`
	Selftext              string  `json:"selftext"`
	Subreddit             string  `json:"subreddit"`
	SubredditNamePrefixed string  `json:"subreddit_name_prefixed"`
	Title                 string  `json:"title"`
	UpvoteRatio           float64 `json:"upvote_ratio"`
	URL                   string  `json:"url"`
}

type redditCommentData struct {
	Author     string        `json:"author"`
	Body       string        `json:"body"`
	CreatedUTC float64       `json:"created_utc"`
	Permalink  string        `json:"permalink"`
	Replies    redditReplies `json:"replies"`
	Score      int           `json:"score"`
}

type redditReplies struct {
	Listing *redditListing
}

func (replies *redditReplies) UnmarshalJSON(data []byte) error {
	trimmedData := strings.TrimSpace(string(data))
	if trimmedData == "" || trimmedData == "null" || trimmedData == `""` {
		return nil
	}

	var listing redditListing

	err := json.Unmarshal(data, &listing)
	if err != nil {
		return fmt.Errorf("decode reddit replies: %w", err)
	}

	replies.Listing = &listing

	return nil
}

func newRedditClient(httpClient *http.Client) redditClient {
	return redditClient{
		httpClient: newRedditHTTPClient(httpClient),
		baseURL:    defaultRedditBaseURL,
		userAgent:  youtubeUserAgent,
	}
}

func newRedditHTTPClient(baseClient *http.Client) *http.Client {
	if baseClient == nil {
		baseClient = new(http.Client)
	}

	client := new(http.Client)
	*client = *baseClient
	client.Transport = newRedditTransport(baseClient.Transport)

	return client
}

func newRedditTransport(baseTransport http.RoundTripper) http.RoundTripper {
	switch transport := baseTransport.(type) {
	case nil:
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return http.DefaultTransport
		}

		return forceHTTP1Transport(defaultTransport.Clone())
	case *http.Transport:
		return forceHTTP1Transport(transport.Clone())
	default:
		return baseTransport
	}
}

func forceHTTP1Transport(transport *http.Transport) *http.Transport {
	transport.ForceAttemptHTTP2 = false

	transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = new(tls.Config)
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}

	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}

	return transport
}

func (instance *bot) maybeAugmentConversationWithReddit(
	ctx context.Context,
	conversation []chatMessage,
	urlExtractionText string,
) ([]chatMessage, []string, error) {
	if instance.reddit == nil {
		return conversation, nil, nil
	}

	redditURLs := extractRedditURLs(urlExtractionText)
	if len(redditURLs) == 0 {
		return conversation, nil, nil
	}

	augmentedConversation, warnings, err := augmentConversationWithConcurrentURLContent(
		ctx,
		conversation,
		redditURLs,
		instance.reddit.fetch,
		"fetch reddit content",
		redditWarningText,
		formatRedditURLContent,
		appendRedditContentToConversation,
		"append reddit content to conversation",
	)

	return augmentedConversation, warnings, err
}

func (client redditClient) fetch(ctx context.Context, rawURL string) (redditThreadContent, error) {
	threadRequest, err := parseRedditThreadURL(rawURL)
	if err != nil {
		return redditThreadContent{}, err
	}

	requestURL, err := resolveRedditRequestURL(client.baseURL, threadRequest.JSONPath)
	if err != nil {
		return redditThreadContent{}, fmt.Errorf("resolve reddit request url for %q: %w", rawURL, err)
	}

	requestContext, cancel := context.WithTimeout(ctx, redditRequestTimeout)
	defer cancel()

	responseBody, err := client.doRequest(requestContext, requestURL)
	if err != nil {
		return redditThreadContent{}, fmt.Errorf("fetch reddit thread for %q: %w", rawURL, err)
	}

	threadContent, err := parseRedditThreadResponse(responseBody)
	if err != nil {
		return redditThreadContent{}, fmt.Errorf("parse reddit thread for %q: %w", rawURL, err)
	}

	threadContent.URL = publicRedditURL(threadRequest.ThreadPath)
	threadContent.JSONURL = publicRedditURL(threadRequest.JSONPath)

	return threadContent, nil
}

func (client redditClient) doRequest(ctx context.Context, requestURL string) ([]byte, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create reddit request %q: %w", requestURL, err)
	}

	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Accept-Language", youtubeAcceptLanguage)
	httpRequest.Header.Set("User-Agent", client.userAgent)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("send reddit request %q: %w", requestURL, err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, fmt.Errorf("read reddit response %q: %w", requestURL, err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf(
			"reddit request %q failed with status %d: %s: %w",
			requestURL,
			httpResponse.StatusCode,
			strings.TrimSpace(string(responseBody)),
			os.ErrInvalid,
		)
	}

	return responseBody, nil
}

func extractRedditURLs(text string) []string {
	text = normalizedURLExtractionText(text)

	matches := redditURLRegexp.FindAllString(text, -1)
	normalizedURLs := make([]string, 0, len(matches))
	seenJSONPaths := make(map[string]struct{}, len(matches))

	for _, match := range matches {
		threadRequest, err := parseRedditThreadURL(match)
		if err != nil {
			continue
		}

		if _, ok := seenJSONPaths[threadRequest.JSONPath]; ok {
			continue
		}

		seenJSONPaths[threadRequest.JSONPath] = struct{}{}
		normalizedURLs = append(normalizedURLs, publicRedditURL(threadRequest.JSONPath))
	}

	return normalizedURLs
}

func parseRedditThreadURL(rawURL string) (redditThreadRequest, error) {
	cleanedURL := strings.TrimSpace(rawURL)
	cleanedURL = strings.Trim(cleanedURL, `"'<>[]()`)

	cleanedURL = strings.TrimRight(cleanedURL, ".,!?;:")
	if cleanedURL == "" {
		return redditThreadRequest{}, fmt.Errorf("empty reddit url: %w", os.ErrInvalid)
	}

	if !strings.Contains(cleanedURL, "://") {
		cleanedURL = "https://" + cleanedURL
	}

	parsedURL, err := url.Parse(cleanedURL)
	if err != nil {
		return redditThreadRequest{}, fmt.Errorf("parse reddit url %q: %w", rawURL, err)
	}

	if !isRedditHost(parsedURL.Hostname()) {
		return redditThreadRequest{}, fmt.Errorf("unsupported reddit host in %q: %w", rawURL, os.ErrInvalid)
	}

	threadPath, err := normalizeRedditThreadPath(parsedURL.Path)
	if err != nil {
		return redditThreadRequest{}, fmt.Errorf("normalize reddit thread path for %q: %w", rawURL, err)
	}

	jsonPath := strings.TrimSuffix(threadPath, "/") + ".json"
	queryValues := make(url.Values)
	queryValues.Set("depth", redditDefaultDepth)
	queryValues.Set("limit", redditDefaultLimit)
	queryValues.Set("raw_json", redditRawJSONValue)

	if encodedQuery := queryValues.Encode(); encodedQuery != "" {
		jsonPath += "?" + encodedQuery
	}

	return redditThreadRequest{
		ThreadPath: threadPath,
		JSONPath:   jsonPath,
	}, nil
}

func normalizeRedditThreadPath(path string) (string, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return "", fmt.Errorf("empty reddit path: %w", os.ErrInvalid)
	}

	trimmedPath = strings.TrimSuffix(trimmedPath, ".json")

	trimmedPath = strings.Trim(trimmedPath, "/")
	if trimmedPath == "" {
		return "", fmt.Errorf("empty reddit path: %w", os.ErrInvalid)
	}

	segments := strings.Split(trimmedPath, "/")
	if !slices.Contains(segments, "comments") {
		return "", fmt.Errorf("reddit path %q does not reference a thread: %w", path, os.ErrInvalid)
	}

	return "/" + strings.Join(segments, "/") + "/", nil
}

func isRedditHost(host string) bool {
	normalizedHost := strings.ToLower(strings.TrimSpace(host))
	normalizedHost = strings.TrimPrefix(normalizedHost, "www.")

	return normalizedHost == "reddit.com" || strings.HasSuffix(normalizedHost, ".reddit.com")
}

func resolveRedditRequestURL(baseURL string, requestPath string) (string, error) {
	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse reddit base url %q: %w", baseURL, err)
	}

	parsedRequestPath, err := url.Parse(requestPath)
	if err != nil {
		return "", fmt.Errorf("parse reddit request path %q: %w", requestPath, err)
	}

	return parsedBaseURL.ResolveReference(parsedRequestPath).String(), nil
}

func publicRedditURL(path string) string {
	return defaultRedditBaseURL + path
}

func parseRedditThreadResponse(responseBody []byte) (redditThreadContent, error) {
	var listings []redditListing

	err := json.Unmarshal(responseBody, &listings)
	if err != nil {
		return redditThreadContent{}, fmt.Errorf("decode reddit thread response: %w", err)
	}

	if len(listings) < minimumRedditListings {
		return redditThreadContent{}, fmt.Errorf("reddit thread response missing listings: %w", os.ErrInvalid)
	}

	postThing, err := firstRedditThingOfKind(listings[0].Data.Children, "t3")
	if err != nil {
		return redditThreadContent{}, err
	}

	var post redditPostData

	err = json.Unmarshal(postThing.Data, &post)
	if err != nil {
		return redditThreadContent{}, fmt.Errorf("decode reddit post: %w", err)
	}

	comments, err := parseRedditComments(listings[1].Data.Children)
	if err != nil {
		return redditThreadContent{}, fmt.Errorf("parse reddit comments: %w", err)
	}

	subreddit := strings.TrimSpace(post.SubredditNamePrefixed)
	if subreddit == "" && strings.TrimSpace(post.Subreddit) != "" {
		subreddit = "r/" + strings.TrimSpace(post.Subreddit)
	}

	return redditThreadContent{
		URL:         publicRedditURL(post.Permalink),
		JSONURL:     "",
		Subreddit:   subreddit,
		Title:       strings.TrimSpace(post.Title),
		Author:      defaultRedditAuthor(post.Author),
		Body:        strings.TrimSpace(post.Selftext),
		Score:       post.Score,
		UpvoteRatio: post.UpvoteRatio,
		NumComments: post.NumComments,
		CreatedUTC:  post.CreatedUTC,
		LinkedURL:   strings.TrimSpace(post.URL),
		Comments:    comments,
	}, nil
}

func firstRedditThingOfKind(things []redditThing, kind string) (redditThing, error) {
	for _, thing := range things {
		if thing.Kind == kind {
			return thing, nil
		}
	}

	return redditThing{}, fmt.Errorf("reddit response missing %s payload: %w", kind, os.ErrInvalid)
}

func parseRedditComments(things []redditThing) ([]redditThreadComment, error) {
	comments := make([]redditThreadComment, 0, len(things))
	for _, thing := range things {
		if thing.Kind != "t1" {
			continue
		}

		comment, err := parseRedditComment(thing)
		if err != nil {
			return nil, err
		}

		comments = append(comments, comment)
	}

	return comments, nil
}

func parseRedditComment(thing redditThing) (redditThreadComment, error) {
	var commentData redditCommentData

	err := json.Unmarshal(thing.Data, &commentData)
	if err != nil {
		return redditThreadComment{}, fmt.Errorf("decode reddit comment: %w", err)
	}

	replies := []redditThreadComment(nil)

	if commentData.Replies.Listing != nil {
		parsedReplies, err := parseRedditComments(commentData.Replies.Listing.Data.Children)
		if err != nil {
			return redditThreadComment{}, fmt.Errorf("parse reddit replies: %w", err)
		}

		replies = parsedReplies
	}

	return redditThreadComment{
		Author:     defaultRedditAuthor(commentData.Author),
		Body:       strings.TrimSpace(commentData.Body),
		Score:      commentData.Score,
		CreatedUTC: commentData.CreatedUTC,
		Permalink:  publicRedditURL(commentData.Permalink),
		Replies:    replies,
	}, nil
}

func defaultRedditAuthor(author string) string {
	trimmedAuthor := strings.TrimSpace(author)
	if trimmedAuthor == "" {
		return "[deleted]"
	}

	return trimmedAuthor
}

func formatRedditURLContent(contents []redditThreadContent) string {
	formattedContents := make([]string, 0, len(contents))
	for _, content := range contents {
		formattedContents = append(formattedContents, formatRedditThread(content))
	}

	return strings.Join(formattedContents, "\n\n")
}

func formatRedditThread(content redditThreadContent) string {
	commentLines := formatRedditCommentLines(content.Comments, nil)

	commentText := "No comments found."
	if len(commentLines) > 0 {
		commentText = strings.Join(commentLines, "\n\n")
	}

	postContent := strings.TrimSpace(content.Body)
	if postContent == "" {
		postContent = "[No post body]"
	}

	lines := []string{
		"Thread URL: " + content.URL,
		"JSON URL: " + content.JSONURL,
		"Subreddit: " + content.Subreddit,
		"Title: " + content.Title,
		"Author: " + content.Author,
		fmt.Sprintf("Score: %d", content.Score),
		fmt.Sprintf("Upvote ratio: %.2f", content.UpvoteRatio),
		"Created: " + formatUnixTimestamp(content.CreatedUTC),
		fmt.Sprintf("Comment count: %d", content.NumComments),
	}

	if content.LinkedURL != "" && content.LinkedURL != content.URL {
		lines = append(lines, "Linked URL: "+content.LinkedURL)
	}

	lines = append(
		lines,
		"Post content:\n"+postContent,
		"Comments:\n"+commentText,
	)

	return strings.Join(lines, "\n")
}

func formatRedditCommentLines(
	comments []redditThreadComment,
	parentNumbers []int,
) []string {
	lines := make([]string, 0, len(comments))
	for index, comment := range comments {
		commentNumbers := append(append([]int(nil), parentNumbers...), index+1)

		commentBody := comment.Body
		if commentBody == "" {
			commentBody = "[No comment body]"
		}

		lines = append(
			lines,
			fmt.Sprintf(
				"%s. Author: %s | Score: %d | Created: %s | Permalink: %s\n%s",
				formatRedditCommentNumber(commentNumbers),
				comment.Author,
				comment.Score,
				formatUnixTimestamp(comment.CreatedUTC),
				comment.Permalink,
				commentBody,
			),
		)

		lines = append(lines, formatRedditCommentLines(comment.Replies, commentNumbers)...)
	}

	return lines
}

func formatRedditCommentNumber(numbers []int) string {
	if len(numbers) == 0 {
		return "0"
	}

	parts := make([]string, 0, len(numbers))
	for _, number := range numbers {
		parts = append(parts, strconv.Itoa(number))
	}

	return strings.Join(parts, ".")
}

func formatUnixTimestamp(unixSeconds float64) string {
	if unixSeconds <= 0 {
		return unknownText
	}

	return time.Unix(int64(unixSeconds), 0).UTC().Format(time.RFC3339)
}
