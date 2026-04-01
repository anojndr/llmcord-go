package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

var errVisualSearchBackendUnavailable = errors.New("visual search backend unavailable")

const (
	testVisualSearchPrompt          = "<@123>: vsearch what anime?"
	testRewrittenVisualSearchPrompt = "<@123>: what anime?"
	testVisualSearchAttachmentURL   = "https://cdn.discordapp.com/attachments/image.png"
	testVisualSearchTitle           = "Sword Art Online"
	testVisualSearchTopMatchURL     = "https://ru.ruwiki.ru/wiki/Sword_Art_Online"
	testVisualSearchSimilarImageURL = "https://yandex.com/images/search?cbir_page=similar-1"
	testVisualSearchSiteDomain      = "vampireknightptk.blogspot.com"
	testVisualSearchSiteMatchURL    = "http://vampireknightptk.blogspot.com/2012/09/indonic-hosting.html"
)

type visualSearchAugmentResult struct {
	conversation []chatMessage
	metadata     *searchMetadata
	warnings     []string
	err          error
}

type visualSearchProviderGate struct {
	started chan string
	release chan struct{}
}

func newVisualSearchSourceMessage(messageID, userID string) *discordgo.Message {
	message := new(discordgo.Message)

	message.ID = messageID
	if userID != "" {
		message.Author = newDiscordUser(userID, false)
	}

	message.Attachments = []*discordgo.MessageAttachment{{
		URL:         testVisualSearchAttachmentURL,
		Filename:    "image.png",
		ContentType: "image/png",
	}}

	return message
}

func newStructuredVisualSearchResult(imageURL string) visualSearchResult {
	result := newVisualSearchResult(imageURL, "")
	result.TopMatch = emptyVisualSearchTopMatch()
	result.TopMatch.Title = testVisualSearchTitle
	result.TopMatch.Source = "ru.ruwiki.ru"
	result.TopMatch.URL = testVisualSearchTopMatchURL
	result.SimilarImages = []visualSearchSimilarImage{{
		Title: "AnimePTK",
		URL:   testVisualSearchSimilarImageURL,
	}}
	result.SiteMatches = []visualSearchSiteMatch{{
		Title:   "AnimePTK",
		Domain:  testVisualSearchSiteDomain,
		Snippet: "",
		URL:     testVisualSearchSiteMatchURL,
	}}

	return result
}

func testStructuredVisualSearchMetadata() *searchMetadata {
	return newVisualSearchMetadata([]visualSearchResult{newStructuredVisualSearchResult("")})
}

func assertSingleVisualSearchSourceMetadata(
	t *testing.T,
	metadata *searchMetadata,
	expectedURL string,
) {
	t.Helper()

	if metadata == nil {
		t.Fatal("expected visual search metadata")
	}

	if len(metadata.VisualSearchSources) != 1 {
		t.Fatalf("unexpected visual search source groups: %#v", metadata.VisualSearchSources)
	}

	if len(metadata.VisualSearchSources[0].Sources) != 3 {
		t.Fatalf("unexpected visual search sources: %#v", metadata.VisualSearchSources[0].Sources)
	}

	if metadata.VisualSearchSources[0].Sources[0].URL != expectedURL {
		t.Fatalf("unexpected visual search source url: %#v", metadata.VisualSearchSources[0].Sources[0])
	}
}

type stubVisualSearchClient struct {
	mu       sync.Mutex
	calls    []string
	searchFn func(context.Context, string) (visualSearchResult, error)
}

func (client *stubVisualSearchClient) search(
	ctx context.Context,
	imageURL string,
) (visualSearchResult, error) {
	client.mu.Lock()
	client.calls = append(client.calls, imageURL)
	client.mu.Unlock()

	return client.searchFn(ctx, imageURL)
}

type stubSerpAPIVisualSearchClient struct {
	mu       sync.Mutex
	calls    []string
	searchFn func(context.Context, config, string) (visualSearchResult, error)
}

func (client *stubSerpAPIVisualSearchClient) search(
	ctx context.Context,
	loadedConfig config,
	imageURL string,
) (visualSearchResult, error) {
	client.mu.Lock()
	client.calls = append(client.calls, imageURL)
	client.mu.Unlock()

	return client.searchFn(ctx, loadedConfig, imageURL)
}

func testSerpAPIVisualSearchResult(imageURL string) visualSearchResult {
	result := emptyVisualSearchResult()
	result.ImageURL = imageURL
	result.Provider = serpAPIVisualSearchProviderName
	result.SearchURL = "https://lens.google.com/uploadbyurl?url=" + imageURL
	result.TopMatch = visualSearchTopMatch{
		Title:       testVisualSearchTitle + " figure",
		Subtitle:    "Exact match",
		Description: "",
		Source:      "Example Store",
		URL:         "https://example.com/products/sao-figure",
	}
	result.SiteMatches = []visualSearchSiteMatch{{
		Title:   testVisualSearchTitle + " figure",
		Domain:  "Example Store",
		Snippet: "Price: $29.99; In stock; Exact matches available",
		URL:     "https://example.com/products/sao-figure",
	}}
	result.RelatedContent = []visualSearchRelatedContent{{
		Query: testVisualSearchTitle,
		URL:   "https://www.google.com/search?q=Sword+Art+Online",
	}}

	return result
}

func TestImageAttachmentURLsForMessagesDeduplicatesAndFilters(t *testing.T) {
	t.Parallel()

	messages := []*discordgo.Message{
		nil,
		{
			Attachments: []*discordgo.MessageAttachment{
				{
					URL:         testVisualSearchAttachmentURL,
					Filename:    "image.png",
					ContentType: "image/png",
				},
				{
					URL:         testVisualSearchAttachmentURL,
					Filename:    "duplicate.png",
					ContentType: "image/png",
				},
				{
					URL:         "https://cdn.discordapp.com/attachments/file.txt",
					Filename:    "file.txt",
					ContentType: "text/plain",
				},
			},
		},
	}

	imageURLs := imageAttachmentURLsForMessages(messages)
	if len(imageURLs) != 1 {
		t.Fatalf("unexpected image URL count: %#v", imageURLs)
	}

	if imageURLs[0] != testVisualSearchAttachmentURL {
		t.Fatalf("unexpected image URL: %#v", imageURLs)
	}
}

func runVisualSearchAugmentAsync(
	instance *bot,
	loadedConfig config,
	sourceMessage *discordgo.Message,
	conversation []chatMessage,
) <-chan visualSearchAugmentResult {
	resultChannel := make(chan visualSearchAugmentResult, 1)

	go func() {
		augmentedConversation, metadata, warnings, err := instance.maybeAugmentConversationWithVisualSearch(
			context.Background(),
			loadedConfig,
			sourceMessage,
			conversation,
		)

		resultChannel <- visualSearchAugmentResult{
			conversation: augmentedConversation,
			metadata:     metadata,
			warnings:     warnings,
			err:          err,
		}
	}()

	return resultChannel
}

func newVisualSearchProviderGate() visualSearchProviderGate {
	return visualSearchProviderGate{
		started: make(chan string, visualSearchProviderCapacity),
		release: make(chan struct{}),
	}
}

func awaitVisualSearchProviders(t *testing.T, gate visualSearchProviderGate, expected int) {
	t.Helper()

	providersStarted := map[string]struct{}{}
	for len(providersStarted) < expected {
		select {
		case provider := <-gate.started:
			providersStarted[provider] = struct{}{}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for visual search providers to start")
		}
	}
}

func assertCombinedVisualSearchResults(
	t *testing.T,
	result visualSearchAugmentResult,
	yandexSearch *stubVisualSearchClient,
	serpAPISearch *stubSerpAPIVisualSearchClient,
) {
	t.Helper()

	if result.err != nil {
		t.Fatalf("maybe augment conversation with visual search: %v", result.err)
	}

	if len(result.warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", result.warnings)
	}

	if len(yandexSearch.calls) != 1 || len(serpAPISearch.calls) != 1 {
		t.Fatalf(
			"expected both providers to be called once, got yandex=%#v serpapi=%#v",
			yandexSearch.calls,
			serpAPISearch.calls,
		)
	}

	content, ok := result.conversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected augmented content type: %T", result.conversation[0].Content)
	}

	prompt := parseAugmentedUserPrompt(content)
	for _, fragment := range []string{
		yandexVisualSearchProviderName,
		serpAPIVisualSearchProviderName,
		"Related content:",
	} {
		if !containsFold(prompt.VisualSearch, fragment) {
			t.Fatalf("expected fragment %q in visual search prompt: %q", fragment, prompt.VisualSearch)
		}
	}

	if result.metadata == nil {
		t.Fatal("expected visual search metadata")
	}

	if len(result.metadata.VisualSearchSources) != 2 {
		t.Fatalf("unexpected visual search source groups: %#v", result.metadata.VisualSearchSources)
	}

	if result.metadata.VisualSearchSources[0].Label != yandexVisualSearchProviderName {
		t.Fatalf("unexpected first visual search source label: %#v", result.metadata.VisualSearchSources[0])
	}

	if result.metadata.VisualSearchSources[1].Label != serpAPIVisualSearchProviderName {
		t.Fatalf("unexpected second visual search source label: %#v", result.metadata.VisualSearchSources[1])
	}
}

func TestRewriteVisualSearchUserQueryRemovesPrefix(t *testing.T) {
	t.Parallel()

	testCases := map[string]string{
		testVisualSearchPrompt:         testRewrittenVisualSearchPrompt,
		"<@123>: VSEARCH: who is this": "<@123>: who is this",
		"vsearch, identify this":       "identify this",
		"vsearch":                      defaultVisualSearchQuery,
	}

	for input, want := range testCases {
		got, ok := rewriteVisualSearchUserQuery(input)
		if !ok {
			t.Fatalf("expected %q to be treated as a visual search request", input)
		}

		if got != want {
			t.Fatalf("unexpected rewritten query for %q: got %q want %q", input, got, want)
		}
	}

	if _, ok := rewriteVisualSearchUserQuery("<@123>: search what anime?"); ok {
		t.Fatal("expected non-vsearch query to be ignored")
	}
}

func TestAppendVisualSearchResultsToConversationPreservesImages(t *testing.T) {
	t.Parallel()

	assertContextAugmentationPreservesImages(
		t,
		testRewrittenVisualSearchPrompt,
		"Top match: Sword Art Online",
		visualSearchSectionName,
		appendVisualSearchResultsToConversation,
	)
}

func TestMaybeAugmentConversationWithVisualSearchAddsResultsAndStripsPrefix(t *testing.T) {
	t.Parallel()

	sourceMessage := newVisualSearchSourceMessage("message-1", "123")

	visualSearch := new(stubVisualSearchClient)
	visualSearch.searchFn = func(_ context.Context, imageURL string) (visualSearchResult, error) {
		result := newStructuredVisualSearchResult(imageURL)
		result.Tags = []string{"sword art online", "asuna sword art online"}

		return result, nil
	}

	instance := new(bot)
	instance.visualSearch = visualSearch

	conversation := []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": testVisualSearchPrompt},
				{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
			},
		},
	}

	augmentedConversation, metadata, warnings, err := instance.maybeAugmentConversationWithVisualSearch(
		context.Background(),
		testSearchConfig(),
		sourceMessage,
		conversation,
	)
	if err != nil {
		t.Fatalf("maybe augment conversation with visual search: %v", err)
	}

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	assertSingleVisualSearchSourceMetadata(t, metadata, testVisualSearchTopMatchURL)

	if len(visualSearch.calls) != 1 || visualSearch.calls[0] != sourceMessage.Attachments[0].URL {
		t.Fatalf("unexpected visual search calls: %#v", visualSearch.calls)
	}

	parts, ok := augmentedConversation[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("unexpected augmented content type: %T", augmentedConversation[0].Content)
	}

	if len(parts) != 2 {
		t.Fatalf("unexpected part count: %d", len(parts))
	}

	if parts[1]["type"] != contentTypeImageURL {
		t.Fatalf("expected image part to be preserved: %#v", parts[1])
	}

	textValue, _ := parts[0]["text"].(string)
	prompt := parseAugmentedUserPrompt(textValue)

	if prompt.UserQuery != testRewrittenVisualSearchPrompt {
		t.Fatalf("unexpected rewritten user query: %q", prompt.UserQuery)
	}

	if !strings.Contains(prompt.VisualSearch, "Sword Art Online") {
		t.Fatalf("expected visual search results in prompt: %q", prompt.VisualSearch)
	}
}

func TestMaybeAugmentConversationWithVisualSearchWarnsWhenImageMissing(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	visualSearch := new(stubVisualSearchClient)
	visualSearch.searchFn = func(context.Context, string) (visualSearchResult, error) {
		t.Fatal("expected visual search not to run without an image")

		return emptyVisualSearchResult(), nil
	}
	instance.visualSearch = visualSearch

	conversation := []chatMessage{
		{Role: messageRoleUser, Content: testVisualSearchPrompt},
	}

	augmentedConversation, metadata, warnings, err := instance.maybeAugmentConversationWithVisualSearch(
		context.Background(),
		testSearchConfig(),
		new(discordgo.Message),
		conversation,
	)
	if err != nil {
		t.Fatalf("maybe augment conversation with visual search: %v", err)
	}

	if len(warnings) != 1 || warnings[0] != visualSearchImageWarningText {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if metadata != nil {
		t.Fatalf("expected nil visual search metadata: %#v", metadata)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if content != testRewrittenVisualSearchPrompt {
		t.Fatalf("unexpected rewritten content: %q", content)
	}
}

func TestMaybeAugmentConversationWithVisualSearchReturnsWarningOnSearchFailure(t *testing.T) {
	t.Parallel()

	sourceMessage := newVisualSearchSourceMessage("message-2", "")

	instance := new(bot)
	visualSearch := new(stubVisualSearchClient)
	visualSearch.searchFn = func(context.Context, string) (visualSearchResult, error) {
		return emptyVisualSearchResult(), errVisualSearchBackendUnavailable
	}
	instance.visualSearch = visualSearch

	conversation := []chatMessage{
		{Role: messageRoleUser, Content: testVisualSearchPrompt},
	}

	augmentedConversation, metadata, warnings, err := instance.maybeAugmentConversationWithVisualSearch(
		context.Background(),
		testSearchConfig(),
		sourceMessage,
		conversation,
	)
	if err != nil {
		t.Fatalf("maybe augment conversation with visual search: %v", err)
	}

	if len(warnings) != 1 || warnings[0] != visualSearchWarningText {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if metadata != nil {
		t.Fatalf("expected nil visual search metadata: %#v", metadata)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if content != testRewrittenVisualSearchPrompt {
		t.Fatalf("unexpected rewritten content: %q", content)
	}
}

func TestMaybeAugmentConversationWithVisualSearchCombinesConcurrentProviderResults(t *testing.T) {
	t.Parallel()

	sourceMessage := newVisualSearchSourceMessage("message-3", "123")

	yandexSearch := new(stubVisualSearchClient)
	serpAPISearch := new(stubSerpAPIVisualSearchClient)

	gate := newVisualSearchProviderGate()

	yandexSearch.searchFn = func(_ context.Context, imageURL string) (visualSearchResult, error) {
		gate.started <- "yandex"

		<-gate.release

		result := newStructuredVisualSearchResult(imageURL)
		result.Provider = yandexVisualSearchProviderName

		return result, nil
	}

	serpAPISearch.searchFn = func(_ context.Context, _ config, imageURL string) (visualSearchResult, error) {
		gate.started <- "serpapi"

		<-gate.release

		return testSerpAPIVisualSearchResult(imageURL), nil
	}

	instance := new(bot)
	instance.visualSearch = yandexSearch
	instance.serpAPIVisualSearch = serpAPISearch

	loadedConfig := testSearchConfig()
	loadedConfig.VisualSearch.SerpAPI = serpAPIVisualSearchConfig{
		APIKey:  "serp-key",
		APIKeys: []string{"serp-key"},
	}

	conversation := []chatMessage{{
		Role:    messageRoleUser,
		Content: testVisualSearchPrompt,
	}}

	resultChannel := runVisualSearchAugmentAsync(instance, loadedConfig, sourceMessage, conversation)

	awaitVisualSearchProviders(t, gate, visualSearchProviderCapacity)

	close(gate.release)

	result := <-resultChannel
	assertCombinedVisualSearchResults(t, result, yandexSearch, serpAPISearch)
}

func TestMaybeAugmentConversationWithVisualSearchReturnsWarningWhenOneProviderFails(t *testing.T) {
	t.Parallel()

	sourceMessage := newVisualSearchSourceMessage("message-4", "123")

	instance := new(bot)
	instance.visualSearch = &stubVisualSearchClient{
		mu:    sync.Mutex{},
		calls: nil,
		searchFn: func(_ context.Context, imageURL string) (visualSearchResult, error) {
			result := newStructuredVisualSearchResult(imageURL)
			result.Provider = yandexVisualSearchProviderName

			return result, nil
		},
	}
	instance.serpAPIVisualSearch = &stubSerpAPIVisualSearchClient{
		mu:    sync.Mutex{},
		calls: nil,
		searchFn: func(context.Context, config, string) (visualSearchResult, error) {
			return emptyVisualSearchResult(), errVisualSearchBackendUnavailable
		},
	}

	loadedConfig := testSearchConfig()
	loadedConfig.VisualSearch.SerpAPI = serpAPIVisualSearchConfig{
		APIKey:  "serp-key",
		APIKeys: []string{"serp-key"},
	}

	conversation := []chatMessage{{Role: messageRoleUser, Content: testVisualSearchPrompt}}

	augmentedConversation, metadata, warnings, err := instance.maybeAugmentConversationWithVisualSearch(
		context.Background(),
		loadedConfig,
		sourceMessage,
		conversation,
	)
	if err != nil {
		t.Fatalf("maybe augment conversation with visual search: %v", err)
	}

	if len(warnings) != 1 || warnings[0] != visualSearchPartialWarningText {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if metadata == nil || len(metadata.VisualSearchSources) != 1 {
		t.Fatalf("unexpected visual search metadata: %#v", metadata)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	prompt := parseAugmentedUserPrompt(content)
	if !containsFold(prompt.VisualSearch, testVisualSearchTitle) {
		t.Fatalf("expected successful provider content in prompt: %q", prompt.VisualSearch)
	}
}

func TestParseYandexVisualSearchHTMLExtractsStructuredResults(t *testing.T) {
	t.Parallel()

	htmlText := strings.Join([]string{
		"<html><body>",
		`  <div class="CbirObjectResponse-Container">`,
		`    <a class="Link CbirObjectResponse-Thumb" href="https://ru.ruwiki.ru/wiki/Sword_Art_Online"></a>`,
		`    <div class="CbirObjectResponse-Content">`,
		`      <h2 class="CbirObjectResponse-Title">Sword Art Online</h2>`,
		`      <div class="CbirObjectResponse-Subtitle">Light novel</div>`,
		`      <div class="CbirObjectResponse-Description">A long-running anime and light novel franchise.</div>`,
		`      <div class="CbirObjectResponse-Source">`,
		`        <a class="Link Link_view_quaternary CbirObjectResponse-SourceLink"`,
		`           href="https://ru.ruwiki.ru/wiki/Sword_Art_Online">ru.ruwiki.ru</a>`,
		`      </div>`,
		`    </div>`,
		`  </div>`,
		`  <section class="CbirSection CbirTags">`,
		`    <a class="Tags-Item" href="/images/search?text=sword%20art%20online">`,
		`      <span class="Button-Text">sword art online</span>`,
		`    </a>`,
		`    <a class="Tags-Item" href="/images/search?text=asuna">`,
		`      <span class="Button-Text">asuna sword art online</span>`,
		`    </a>`,
		`  </section>`,
		`  <section class="CbirSection CbirOcr CbirOcr_legacy">`,
		`    <div class="CbirOcr-TextBox">SWORD ART ONLINE</div>`,
		`  </section>`,
		`  <section class="CbirSection CbirSimilarList">`,
		`    <a class="Link CbirSimilarList-ThumbImage"`,
		`       href="/images/search?cbir_page=similar-1"`,
		`       aria-label="AnimePTK"></a>`,
		`    <a class="Link CbirSimilarList-ThumbImage"`,
		`       href="/images/search?cbir_page=similar-2"`,
		`       aria-label="Top 9 Anime Series with Piano Music That Will Tug At Your Heart - MyAnimeList.net"></a>`,
		`  </section>`,
		`  <section class="CbirSection CbirSitesList">`,
		`    <ul class="CbirSites-Items">`,
		`      <li class="CbirSites-Item">`,
		`        <div class="CbirSites-ItemInfo">`,
		`          <div class="CbirSites-ItemTitle">`,
		`            <a href="http://vampireknightptk.blogspot.com/2012/09/indonic-hosting.html">AnimePTK</a>`,
		`          </div>`,
		`          <a class="CbirSites-ItemDomain"`,
		`             href="http://vampireknightptk.blogspot.com/2012/09/indonic-hosting.html">`,
		`            vampireknightptk.blogspot.com`,
		`          </a>`,
		`          <div class="CbirSites-ItemDescription">Nonton Sekarang!</div>`,
		`        </div>`,
		`      </li>`,
		`    </ul>`,
		`  </section>`,
		"</body></html>",
	}, "\n")

	result, err := parseYandexVisualSearchHTML(
		"https://yandex.com/images/search?rpt=imageview&url=https%3A%2F%2Fcdn.example.com%2Fimage.png",
		"https://cdn.example.com/image.png",
		[]byte(htmlText),
	)
	if err != nil {
		t.Fatalf("parse yandex visual search html: %v", err)
	}

	if result.TopMatch.Title != "Sword Art Online" {
		t.Fatalf("unexpected top match title: %#v", result.TopMatch)
	}

	if result.TopMatch.URL != "https://ru.ruwiki.ru/wiki/Sword_Art_Online" {
		t.Fatalf("unexpected top match URL: %#v", result.TopMatch)
	}

	if len(result.Tags) != 2 || result.Tags[0] != "sword art online" {
		t.Fatalf("unexpected tags: %#v", result.Tags)
	}

	if len(result.TextInImage) != 1 || result.TextInImage[0] != "SWORD ART ONLINE" {
		t.Fatalf("unexpected OCR text: %#v", result.TextInImage)
	}

	if len(result.SimilarImages) != 2 || result.SimilarImages[0].Title != "AnimePTK" {
		t.Fatalf("unexpected similar images: %#v", result.SimilarImages)
	}

	if !strings.HasPrefix(result.SimilarImages[0].URL, "https://yandex.com/images/search?") {
		t.Fatalf("expected similar image URL to resolve against Yandex: %#v", result.SimilarImages[0])
	}

	if len(result.SiteMatches) != 1 || result.SiteMatches[0].Domain != "vampireknightptk.blogspot.com" {
		t.Fatalf("unexpected site matches: %#v", result.SiteMatches)
	}
}

func TestExtractVisualSearchSourcesIncludesUniqueURLs(t *testing.T) {
	t.Parallel()

	result := newStructuredVisualSearchResult("")
	result.SiteMatches = append(result.SiteMatches, visualSearchSiteMatch{
		Title:   "Duplicate top match",
		Domain:  "ru.ruwiki.ru",
		Snippet: "",
		URL:     testVisualSearchTopMatchURL,
	})

	sources := extractVisualSearchSources(result)
	if len(sources) != 3 {
		t.Fatalf("unexpected visual search source count: %#v", sources)
	}

	if sources[0].Title != "Top match: Sword Art Online (ru.ruwiki.ru)" {
		t.Fatalf("unexpected top match source: %#v", sources[0])
	}

	if sources[1].Title != "Similar image: AnimePTK" {
		t.Fatalf("unexpected similar image source: %#v", sources[1])
	}

	if sources[2].Title != "Site match: AnimePTK (vampireknightptk.blogspot.com)" {
		t.Fatalf("unexpected site match source: %#v", sources[2])
	}
}

func TestVisualSearchResultSectionLabelUsesProviderWhenResultsShareImage(t *testing.T) {
	t.Parallel()

	results := []visualSearchResult{{
		ImageIndex:     0,
		Provider:       yandexVisualSearchProviderName,
		ImageURL:       "",
		SearchURL:      "",
		TopMatch:       emptyVisualSearchTopMatch(),
		Tags:           nil,
		TextInImage:    nil,
		SimilarImages:  nil,
		SiteMatches:    nil,
		RelatedContent: nil,
	}, {
		ImageIndex:     0,
		Provider:       serpAPIVisualSearchProviderName,
		ImageURL:       "",
		SearchURL:      "",
		TopMatch:       emptyVisualSearchTopMatch(),
		Tags:           nil,
		TextInImage:    nil,
		SimilarImages:  nil,
		SiteMatches:    nil,
		RelatedContent: nil,
	}}

	if label := visualSearchResultSectionLabel(results[0], results); label != yandexVisualSearchProviderName {
		t.Fatalf("unexpected visual search label: %q", label)
	}

	if label := visualSearchResultSectionLabel(results[1], results); label != serpAPIVisualSearchProviderName {
		t.Fatalf("unexpected visual search label: %q", label)
	}
}

func TestParseSerpAPIGoogleLensResponseExtractsStructuredResults(t *testing.T) {
	t.Parallel()

	inStock := true
	result := parseSerpAPIGoogleLensResponse(
		"https://cdn.example.com/image.png",
		serpAPIGoogleLensResponse{
			SearchMetadata: serpAPIGoogleLensSearchMetadata{
				Status:        "Success",
				JSONEndpoint:  "",
				GoogleLensURL: "https://lens.google.com/uploadbyurl?url=https%3A%2F%2Fcdn.example.com%2Fimage.png",
			},
			VisualMatches: []serpAPIGoogleLensVisualMatch{
				{
					Title:        "Sword Art Online figure",
					Link:         "https://example.com/products/sao-figure",
					Source:       "Example Store",
					Rating:       0,
					Reviews:      0,
					Price:        serpAPIGoogleLensPrice{Value: "$29.99"},
					InStock:      &inStock,
					Condition:    "",
					ExactMatches: true,
				},
				{
					Title:        "Sword Art Online merch",
					Link:         "https://example.com/products/sao-merch",
					Source:       "Example Store",
					Price:        serpAPIGoogleLensPrice{Value: ""},
					InStock:      nil,
					Condition:    "Used",
					Rating:       4.8,
					Reviews:      123,
					ExactMatches: false,
				},
			},
			RelatedContent: []serpAPIGoogleLensRelatedResult{{
				Query: "Sword Art Online",
				Link:  "https://www.google.com/search?q=Sword+Art+Online",
			}},
			Error: "",
		},
	)

	if result.Provider != serpAPIVisualSearchProviderName {
		t.Fatalf("unexpected provider: %#v", result)
	}

	if result.TopMatch.Title != testVisualSearchTitle+" figure" {
		t.Fatalf("unexpected top match: %#v", result.TopMatch)
	}

	if result.TopMatch.Subtitle != "Exact match" {
		t.Fatalf("unexpected top match subtitle: %#v", result.TopMatch)
	}

	if !containsFold(result.TopMatch.Description, "$29.99") || !containsFold(result.TopMatch.Description, "In stock") {
		t.Fatalf("unexpected top match description: %#v", result.TopMatch)
	}

	if len(result.SiteMatches) != 1 {
		t.Fatalf("unexpected SerpApi site matches: %#v", result.SiteMatches)
	}

	if !containsFold(result.SiteMatches[0].Snippet, "Used") || !containsFold(result.SiteMatches[0].Snippet, "4.8") {
		t.Fatalf("unexpected SerpApi site match snippet: %#v", result.SiteMatches[0])
	}

	if len(result.RelatedContent) != 1 || result.RelatedContent[0].Query != testVisualSearchTitle {
		t.Fatalf("unexpected SerpApi related content: %#v", result.RelatedContent)
	}
}

func newTestHTTPClient(transport roundTripFunc) *http.Client {
	httpClient := new(http.Client)
	httpClient.Transport = transport

	return httpClient
}

func newTestHTTPResponse(request *http.Request, statusCode int, body string) *http.Response {
	response := new(http.Response)
	response.StatusCode = statusCode
	response.Status = http.StatusText(statusCode)
	response.Body = io.NopCloser(strings.NewReader(body))
	response.Header = make(http.Header)
	response.Request = request

	return response
}

func marshalSerpAPIErrorResponse(t *testing.T, errorMessage string) string {
	t.Helper()

	response := new(serpAPIErrorResponse)
	response.Error = errorMessage

	responseBody, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal SerpApi error response: %v", err)
	}

	return string(responseBody)
}

func newTestSerpAPIGoogleLensResponse(
	status, googleLensURL, errorMessage string,
) serpAPIGoogleLensResponse {
	response := new(serpAPIGoogleLensResponse)
	response.SearchMetadata = serpAPIGoogleLensSearchMetadata{
		Status:        status,
		JSONEndpoint:  "",
		GoogleLensURL: googleLensURL,
	}
	response.VisualMatches = nil
	response.RelatedContent = nil
	response.Error = errorMessage

	return *response
}

func marshalSerpAPIGoogleLensResponse(
	t *testing.T,
	status string,
	googleLensURL string,
	errorMessage string,
) string {
	t.Helper()

	responseBody, err := json.Marshal(newTestSerpAPIGoogleLensResponse(status, googleLensURL, errorMessage))
	if err != nil {
		t.Fatalf("marshal SerpApi response: %v", err)
	}

	return string(responseBody)
}

func runSerpAPIGoogleLensHTTPStatusErrorTest(
	t *testing.T,
	statusCode int,
	errorMessage string,
	wantAPIKeyError bool,
) {
	t.Helper()

	httpClient := newTestHTTPClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return newTestHTTPResponse(
			request,
			statusCode,
			marshalSerpAPIErrorResponse(t, errorMessage),
		), nil
	}))

	client := newSerpAPIVisualSearchClient(httpClient)

	_, err := client.searchOnce(context.Background(), testVisualSearchAttachmentURL, "serp-key")
	if err == nil {
		t.Fatal("expected SerpApi error")
	}

	var statusErr providerStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected provider status error, got %T", err)
	}

	if statusErr.StatusCode != statusCode {
		t.Fatalf("unexpected status code: got %d want %d", statusErr.StatusCode, statusCode)
	}

	var apiKeyErr providerAPIKeyError
	if got := errors.As(err, &apiKeyErr); got != wantAPIKeyError {
		t.Fatalf("unexpected api key classification for %v: got %t want %t", err, got, wantAPIKeyError)
	}

	if !strings.Contains(err.Error(), errorMessage) {
		t.Fatalf("expected error message %q in %q", errorMessage, err.Error())
	}
}

func runSerpAPIGoogleLensNonSuccessStatusTest(t *testing.T, status string) {
	t.Helper()

	client := newSerpAPIVisualSearchClient(new(http.Client))

	_, err := client.parseResponse(
		"https://serpapi.com/search.json?engine=google_lens",
		"https://cdn.example.com/image.png",
		[]byte(marshalSerpAPIGoogleLensResponse(
			t,
			status,
			"",
			"We couldn't get valid results for this search. Please try again later.",
		)),
	)
	if err == nil {
		t.Fatal("expected SerpApi status error")
	}

	var statusErr providerStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected provider status error, got %T", err)
	}

	if statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf(
			"unexpected status code for search status %q: got %d want %d",
			status,
			statusErr.StatusCode,
			http.StatusServiceUnavailable,
		)
	}

	if !strings.Contains(err.Error(), status) {
		t.Fatalf("expected status %q in error %q", status, err.Error())
	}
}

func runSerpAPIGoogleLensRetryableFailureTest(
	t *testing.T,
	statusCode int,
	errorMessage string,
) {
	t.Helper()

	var (
		requestedKeys []string
		requestsMu    sync.Mutex
	)

	httpClient := newTestHTTPClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		apiKey := request.URL.Query().Get("api_key")

		requestsMu.Lock()

		requestedKeys = append(requestedKeys, apiKey)

		requestsMu.Unlock()

		if apiKey == testTavilyPrimaryAPIKey {
			return newTestHTTPResponse(
				request,
				statusCode,
				marshalSerpAPIErrorResponse(t, errorMessage),
			), nil
		}

		if apiKey != testTavilyBackupAPIKey {
			t.Fatalf("unexpected api key: %q", apiKey)
		}

		return newTestHTTPResponse(
			request,
			http.StatusOK,
			marshalSerpAPIGoogleLensResponse(
				t,
				serpAPISearchStatusSuccess,
				"https://lens.google.com/uploadbyurl?url=https%3A%2F%2Fcdn.example.com%2Fimage.png",
				"",
			),
		), nil
	}))

	client := newSerpAPIVisualSearchClient(httpClient)
	loadedConfig := testSearchConfig()
	loadedConfig.VisualSearch.SerpAPI = serpAPIVisualSearchConfig{
		APIKey:  testTavilyPrimaryAPIKey,
		APIKeys: []string{testTavilyBackupAPIKey},
	}

	result, err := client.search(context.Background(), loadedConfig, testVisualSearchAttachmentURL)
	if err != nil {
		t.Fatalf("search SerpApi Google Lens: %v", err)
	}

	if result.Provider != serpAPIVisualSearchProviderName {
		t.Fatalf("unexpected provider: %#v", result)
	}

	if len(requestedKeys) != 2 ||
		requestedKeys[0] != testTavilyPrimaryAPIKey ||
		requestedKeys[1] != testTavilyBackupAPIKey {
		t.Fatalf("unexpected requested keys: %#v", requestedKeys)
	}
}

func TestSerpAPIGoogleLensSearchOnceHandlesDocumentedHTTPStatusErrors(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		statusCode      int
		errorMessage    string
		wantAPIKeyError bool
	}{
		{
			name:            "bad request",
			statusCode:      http.StatusBadRequest,
			errorMessage:    "Missing query `q` parameter.",
			wantAPIKeyError: false,
		},
		{
			name:            "unauthorized",
			statusCode:      http.StatusUnauthorized,
			errorMessage:    "Invalid API key. Your API key should be here: https://serpapi.com/manage-api-key",
			wantAPIKeyError: true,
		},
		{
			name:            "forbidden",
			statusCode:      http.StatusForbidden,
			errorMessage:    "The account associated with this API key doesn't have permission to perform the request.",
			wantAPIKeyError: true,
		},
		{
			name:            "not found",
			statusCode:      http.StatusNotFound,
			errorMessage:    "The requested resource doesn't exist.",
			wantAPIKeyError: false,
		},
		{
			name:            "gone",
			statusCode:      http.StatusGone,
			errorMessage:    "The search expired and has been deleted from the archive.",
			wantAPIKeyError: false,
		},
		{
			name:            "too many requests",
			statusCode:      http.StatusTooManyRequests,
			errorMessage:    "Your account has run out of searches.",
			wantAPIKeyError: false,
		},
		{
			name:            "internal server error",
			statusCode:      http.StatusInternalServerError,
			errorMessage:    "Something went wrong on SerpApi's end. Please contact support@serpapi.com for assistance.",
			wantAPIKeyError: false,
		},
		{
			name:            "service unavailable",
			statusCode:      http.StatusServiceUnavailable,
			errorMessage:    "Something went wrong on SerpApi's end. Please contact support@serpapi.com for assistance.",
			wantAPIKeyError: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			runSerpAPIGoogleLensHTTPStatusErrorTest(
				t,
				testCase.statusCode,
				testCase.errorMessage,
				testCase.wantAPIKeyError,
			)
		})
	}
}

func TestSerpAPIGoogleLensParseResponseAllowsSuccessWithEmptyResultsError(t *testing.T) {
	t.Parallel()

	client := newSerpAPIVisualSearchClient(new(http.Client))

	result, err := client.parseResponse(
		"https://serpapi.com/search.json?engine=google_lens",
		"https://cdn.example.com/image.png",
		[]byte(marshalSerpAPIGoogleLensResponse(
			t,
			serpAPISearchStatusSuccess,
			"https://lens.google.com/uploadbyurl?url=https%3A%2F%2Fcdn.example.com%2Fimage.png",
			"Google hasn't returned any results for this query.",
		)),
	)
	if err != nil {
		t.Fatalf("parse SerpApi response: %v", err)
	}

	if result.Provider != serpAPIVisualSearchProviderName {
		t.Fatalf("unexpected provider: %#v", result)
	}

	if result.SearchURL != "https://lens.google.com/uploadbyurl?url=https%3A%2F%2Fcdn.example.com%2Fimage.png" {
		t.Fatalf("unexpected search url: %#v", result)
	}

	if result.TopMatch != emptyVisualSearchTopMatch() {
		t.Fatalf("expected empty top match for empty results: %#v", result.TopMatch)
	}
}

func TestSerpAPIGoogleLensParseResponseRejectsNonSuccessSearchStatuses(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		status string
	}{
		{name: "queued", status: serpAPISearchStatusQueued},
		{name: "processing", status: serpAPISearchStatusProcessing},
		{name: "error", status: serpAPISearchStatusError},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			runSerpAPIGoogleLensNonSuccessStatusTest(t, testCase.status)
		})
	}
}

func TestSerpAPIGoogleLensSearchRetriesBackupKeyForRetryableFailures(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		statusCode   int
		errorMessage string
	}{
		{
			name:         "unauthorized",
			statusCode:   http.StatusUnauthorized,
			errorMessage: "Invalid API key. Your API key should be here: https://serpapi.com/manage-api-key",
		},
		{
			name:         "forbidden",
			statusCode:   http.StatusForbidden,
			errorMessage: "The account associated with this API key doesn't have permission to perform the request.",
		},
		{
			name:         "too many requests",
			statusCode:   http.StatusTooManyRequests,
			errorMessage: "Your account has run out of searches.",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			runSerpAPIGoogleLensRetryableFailureTest(
				t,
				testCase.statusCode,
				testCase.errorMessage,
			)
		})
	}
}

func TestSerpAPIGoogleLensSearchStopsAfterNonRetryableFailure(t *testing.T) {
	t.Parallel()

	var (
		requestedKeys []string
		requestsMu    sync.Mutex
	)

	httpClient := newTestHTTPClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		apiKey := request.URL.Query().Get("api_key")

		requestsMu.Lock()

		requestedKeys = append(requestedKeys, apiKey)

		requestsMu.Unlock()

		return newTestHTTPResponse(
			request,
			http.StatusBadRequest,
			marshalSerpAPIErrorResponse(t, "Missing query `q` parameter."),
		), nil
	}))

	client := newSerpAPIVisualSearchClient(httpClient)
	loadedConfig := testSearchConfig()
	loadedConfig.VisualSearch.SerpAPI = serpAPIVisualSearchConfig{
		APIKey:  testTavilyPrimaryAPIKey,
		APIKeys: []string{testTavilyBackupAPIKey},
	}

	_, err := client.search(context.Background(), loadedConfig, testVisualSearchAttachmentURL)
	if err == nil {
		t.Fatal("expected SerpApi search error")
	}

	if !strings.Contains(err.Error(), "Missing query `q` parameter.") {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(requestedKeys) != 1 || requestedKeys[0] != testTavilyPrimaryAPIKey {
		t.Fatalf("expected only the primary key attempt, got %#v", requestedKeys)
	}
}

func TestFormatSearchSourcesMessageIncludesVisualSearchProviderLabels(t *testing.T) {
	t.Parallel()

	metadata := &searchMetadata{
		Queries: nil,
		Results: nil,
		MaxURLs: 0,
		VisualSearchSources: []visualSearchSourceGroup{{
			Label: yandexVisualSearchProviderName,
			Sources: []searchSource{{
				Title: "Top match: Example",
				URL:   "https://example.com/yandex",
			}},
		}, {
			Label: serpAPIVisualSearchProviderName,
			Sources: []searchSource{{
				Title: "Top match: Example product",
				URL:   "https://example.com/serpapi",
			}},
		}},
	}

	message := formatSearchSourcesMessage(metadata)

	for _, fragment := range []string{
		yandexVisualSearchProviderName + ":\n1. Top match: Example <https://example.com/yandex>",
		serpAPIVisualSearchProviderName + ":\n1. Top match: Example product <https://example.com/serpapi>",
	} {
		if !strings.Contains(message, fragment) {
			t.Fatalf("expected fragment %q in message: %q", fragment, message)
		}
	}
}
