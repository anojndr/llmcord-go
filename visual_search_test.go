package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

var errVisualSearchBackendUnavailable = errors.New("visual search backend unavailable")

const (
	testVisualSearchPrompt          = "<@123>: vsearch what anime?"
	testRewrittenVisualSearchPrompt = "<@123>: what anime?"
	testVisualSearchAttachmentURL   = "https://cdn.discordapp.com/attachments/image.png"
	testVisualSearchTopMatchURL     = "https://ru.ruwiki.ru/wiki/Sword_Art_Online"
	testVisualSearchSimilarImageURL = "https://yandex.com/images/search?cbir_page=similar-1"
	testVisualSearchSiteDomain      = "vampireknightptk.blogspot.com"
	testVisualSearchSiteMatchURL    = "http://vampireknightptk.blogspot.com/2012/09/indonic-hosting.html"
)

func newVisualSearchSourceMessage(messageID string, userID string) *discordgo.Message {
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
	result.TopMatch.Title = "Sword Art Online"
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
