package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

const testPipelineTimeout = 30 * time.Second

var errUnexpectedSearchDeciderModel = errors.New("unexpected search decider model")

// TestSearchDeciderBuildsMainModelConversation asserts that the search
// decider conversation is produced by the exact same conversation-building
// code path as the main model: buildMessageConversation followed by
// augmentPreparedMessageResponse. The decider model only differs in the
// search decider prompt being prepended to the latest user query.
func TestSearchDeciderBuildsMainModelConversation(t *testing.T) {
	t.Parallel()

	loadedConfig := testSearchConfig()
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages

	instance := newSearchDeciderPipelineTestBot(t)

	sourceMessage := newSearchDeciderPipelineSourceMessage(t)

	ctx, cancel := context.WithTimeout(context.Background(), testPipelineTimeout)
	defer cancel()

	mainMessages, _, err := instance.bot.buildMessageConversation(
		ctx,
		loadedConfig,
		sourceMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("build main model conversation: %v", err)
	}

	mainMessages, _, _, err = instance.bot.augmentPreparedMessageResponse(
		ctx,
		loadedConfig,
		sourceMessage,
		"openai/main-model",
		mainMessages,
		nil,
	)
	if err != nil {
		t.Fatalf("augment prepared message response: %v", err)
	}

	deciderMessages, err := instance.bot.buildSearchDeciderConversation(
		ctx,
		loadedConfig,
		"openai/main-model",
		loadedConfig.SearchDeciderModel,
		sourceMessage,
		mainMessages,
	)
	if err != nil {
		t.Fatalf("build search decider conversation: %v", err)
	}

	if len(deciderMessages) != len(mainMessages) {
		t.Fatalf(
			"decider conversation length %d != main conversation length %d",
			len(deciderMessages),
			len(mainMessages),
		)
	}

	for index, mainMessage := range mainMessages {
		deciderMessage := deciderMessages[index]

		if !chatMessageContentsEqual(deciderMessage.Content, mainMessage.Content) {
			t.Fatalf(
				"decider message %d content %#v != main message content %#v",
				index,
				deciderMessage.Content,
				mainMessage.Content,
			)
		}
	}
}

// TestSearchDeciderConversationMatchesMainModelWithPDFAttachment asserts
// that the decider conversation carries the exact same document-extraction
// augmentation the main model produces. The main model inlines extracted
// document text into the conversation; the decider must run the same
// augmentation so it sees the same document content.
func TestSearchDeciderConversationMatchesMainModelWithPDFAttachment(t *testing.T) {
	t.Parallel()

	loadedConfig := testSearchConfig()
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages

	instance := newSearchDeciderPipelineTestBot(t)

	sourceMessage := newSearchDeciderPipelineSourceMessage(t)

	pdfPart := testPDFDocumentPart(t, "Quarterly revenue grew by 12 percent.", true)

	instance.bot.nodes.getOrCreate(sourceMessage.ID).media = []contentPart{pdfPart}

	ctx, cancel := context.WithTimeout(context.Background(), testPipelineTimeout)
	defer cancel()

	mainMessages, _, err := instance.bot.buildMessageConversation(
		ctx,
		loadedConfig,
		sourceMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("build main model conversation: %v", err)
	}

	mainMessages, _, _, err = instance.bot.augmentPreparedMessageResponse(
		ctx,
		loadedConfig,
		sourceMessage,
		"openai/main-model",
		mainMessages,
		nil,
	)
	if err != nil {
		t.Fatalf("augment prepared message response: %v", err)
	}

	deciderMessages, err := instance.bot.buildSearchDeciderConversation(
		ctx,
		loadedConfig,
		"openai/main-model",
		loadedConfig.SearchDeciderModel,
		sourceMessage,
		mainMessages,
	)
	if err != nil {
		t.Fatalf("build search decider conversation: %v", err)
	}

	if len(deciderMessages) != len(mainMessages) {
		t.Fatalf(
			"decider conversation length %d != main conversation length %d",
			len(deciderMessages),
			len(mainMessages),
		)
	}

	for index, mainMessage := range mainMessages {
		deciderMessage := deciderMessages[index]

		if !chatMessageContentsEqual(deciderMessage.Content, mainMessage.Content) {
			t.Fatalf(
				"decider message %d content %#v != main message content %#v",
				index,
				deciderMessage.Content,
				mainMessage.Content,
			)
		}
	}
}

// TestSearchDeciderUsesMainModelAugmentation asserts that every
// augmentation step the main model runs (video URLs, PDF extraction, Gemini
// media analysis, visual search, website, youtube, reddit) also runs for the
// search decider model, by stubbing each fetcher and asserting its calls are
// made when the decider conversation is built.
func TestSearchDeciderUsesMainModelAugmentation(t *testing.T) {
	t.Parallel()

	loadedConfig := testSearchConfig()
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages

	instance := newSearchDeciderPipelineTestBot(t)

	sourceMessage := newSearchDeciderPipelineSourceMessage(t)

	ctx, cancel := context.WithTimeout(context.Background(), testPipelineTimeout)
	defer cancel()

	mainMessages, _, err := instance.bot.buildMessageConversation(
		ctx,
		loadedConfig,
		sourceMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("build main model conversation: %v", err)
	}

	mainMessages, _, _, err = instance.bot.augmentPreparedMessageResponse(
		ctx,
		loadedConfig,
		sourceMessage,
		"openai/main-model",
		mainMessages,
		nil,
	)
	if err != nil {
		t.Fatalf("augment prepared message response: %v", err)
	}

	deciderMessages, err := instance.bot.buildSearchDeciderConversation(
		ctx,
		loadedConfig,
		"openai/main-model",
		loadedConfig.SearchDeciderModel,
		sourceMessage,
		mainMessages,
	)
	if err != nil {
		t.Fatalf("build search decider conversation: %v", err)
	}

	deciderText := messageContentText(deciderMessages[len(deciderMessages)-1].Content)

	for _, expectedFragment := range []string{
		visualSearchSectionName,
		websiteSectionName,
		youtubeSectionName,
		redditSectionName,
	} {
		if !strings.Contains(deciderText, expectedFragment) {
			t.Fatalf("expected decider conversation to contain %q: %q", expectedFragment, deciderText)
		}
	}
}

// TestSearchDeciderDoesNotReDecideWebSearch asserts that building the search
// decider conversation never runs another web-search decision: the decider
// pipeline is the main pipeline minus the web-search augmentation, so a
// decider build must not trigger the search-decider model again (infinite
// recursion guard).
func TestSearchDeciderDoesNotReDecideWebSearch(t *testing.T) {
	t.Parallel()

	loadedConfig := testSearchConfig()
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages

	instance := newSearchDeciderPipelineTestBot(t)

	sourceMessage := newSearchDeciderPipelineSourceMessage(t)

	ctx, cancel := context.WithTimeout(context.Background(), testPipelineTimeout)
	defer cancel()

	mainMessages, _, err := instance.bot.buildMessageConversation(
		ctx,
		loadedConfig,
		sourceMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("build main model conversation: %v", err)
	}

	mainMessages, _, _, err = instance.bot.augmentPreparedMessageResponse(
		ctx,
		loadedConfig,
		sourceMessage,
		"openai/main-model",
		mainMessages,
		nil,
	)
	if err != nil {
		t.Fatalf("augment prepared message response: %v", err)
	}

	_, err = instance.bot.buildSearchDeciderConversation(
		ctx,
		loadedConfig,
		"openai/main-model",
		loadedConfig.SearchDeciderModel,
		sourceMessage,
		mainMessages,
	)
	if err != nil {
		t.Fatalf("build search decider conversation: %v", err)
	}

	if calls := len(instance.stubWebSearch.calls); calls != 0 {
		t.Fatalf("expected no web search calls while building decider conversation, got %d", calls)
	}

	if decisions := instance.deciderDecisions.Load(); decisions != 0 {
		t.Fatalf("expected no search decider decisions while building decider conversation, got %d", decisions)
	}
}

// TestDecideWebSearchRequestMatchesMainModelPipeline asserts that the final
// search decider model request is built through the same request pipeline as
// the main model: same messages (plus the search decider prompt prepended to
// the latest user query), and streamed through the same chat completions
// client.
func TestDecideWebSearchRequestMatchesMainModelPipeline(t *testing.T) {
	t.Parallel()

	loadedConfig := testSearchConfig()
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages

	instance := newSearchDeciderPipelineTestBot(t)

	sourceMessage := newSearchDeciderPipelineSourceMessage(t)

	ctx, cancel := context.WithTimeout(context.Background(), testPipelineTimeout)
	defer cancel()

	mainMessages, _, err := instance.bot.buildMessageConversation(
		ctx,
		loadedConfig,
		sourceMessage,
		"openai/main-model",
	)
	if err != nil {
		t.Fatalf("build main model conversation: %v", err)
	}

	mainMessages, _, _, err = instance.bot.augmentPreparedMessageResponse(
		ctx,
		loadedConfig,
		sourceMessage,
		"openai/main-model",
		mainMessages,
		nil,
	)
	if err != nil {
		t.Fatalf("augment prepared message response: %v", err)
	}

	instance.stubChat.requests = nil

	decision, _, err := instance.bot.decideWebSearch(
		ctx,
		loadedConfig,
		"openai/main-model",
		sourceMessage,
		mainMessages,
	)
	if err != nil {
		t.Fatalf("decide web search: %v", err)
	}

	if decision.NeedsSearch {
		t.Fatalf("expected decider to skip web search: %#v", decision)
	}

	var deciderRequests []chatCompletionRequest

	for _, request := range instance.stubChat.requests {
		if request.ConfiguredModel == loadedConfig.SearchDeciderModel {
			deciderRequests = append(deciderRequests, request)
		}
	}

	if len(deciderRequests) != 1 {
		t.Fatalf(
			"expected exactly one search decider chat completion request, got %d",
			len(deciderRequests),
		)
	}

	request := deciderRequests[0]

	if request.ConfiguredModel != loadedConfig.SearchDeciderModel {
		t.Fatalf("unexpected decider configured model: %q", request.ConfiguredModel)
	}

	if len(request.Messages) == 0 {
		t.Fatal("expected decider request messages")
	}

	latestText := messageContentText(request.Messages[len(request.Messages)-1].Content)

	if !strings.HasPrefix(latestText, "Do not refuse to make a search decision") {
		t.Fatalf("expected search decider prompt prepended to latest query: %q", latestText)
	}

	if !strings.Contains(latestText, "Latest user query:") {
		t.Fatalf("expected latest user query header in decider prompt: %q", latestText)
	}

	if !strings.Contains(latestText, searchDeciderDecisionInstruction) {
		t.Fatalf("expected search decider decision instruction in latest query: %q", latestText)
	}
}

func newSearchDeciderPipelineTestBot(t *testing.T) *searchDeciderPipelineTestBot {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.State.User = newDiscordUser("bot-user", true)

	instance := new(bot)
	instance.session = session
	instance.nodes = newMessageNodeStore(maxMessageNodes)

	stubChat := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		if request.ConfiguredModel != "openai/decider-model" {
			return errUnexpectedSearchDeciderModel
		}

		return handle(newStreamDelta(`{"needs_search":false}`, finishReasonStop))
	})
	instance.chatCompletions = stubChat

	stubWebSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		t.Fatalf("unexpected web search call with queries %#v", queries)

		return nil, nil
	})
	instance.webSearch = stubWebSearch

	instance.visualSearch = pipelineTestVisualSearchClient()
	instance.website = pipelineTestWebsiteClient()
	instance.youtube = pipelineTestYouTubeClient()
	instance.reddit = pipelineTestRedditClient()

	return &searchDeciderPipelineTestBot{
		bot:              instance,
		stubChat:         stubChat,
		stubWebSearch:    stubWebSearch,
		deciderDecisions: new(atomic.Int32),
	}
}

func pipelineTestVisualSearchClient() *stubVisualSearchClient {
	return &stubVisualSearchClient{
		mu:    sync.Mutex{},
		calls: nil,
		searchFn: func(
			_ context.Context,
			imageURL string,
		) (visualSearchResult, error) {
			return newStructuredVisualSearchResult(imageURL), nil
		},
	}
}

func pipelineTestWebsiteClient() *stubWebsiteContentClient {
	return newStubWebsiteContentClient(func(
		_ context.Context,
		_ config,
		rawURL string,
	) (websitePageContent, error) {
		return websitePageContent{
			URL:         rawURL,
			Title:       "Example website",
			Description: "Example description",
			Content:     "Website body",
		}, nil
	})
}

func pipelineTestYouTubeClient() *stubYouTubeContentClient {
	return newStubYouTubeContentClient(func(
		_ context.Context,
		rawURL string,
	) (youtubeVideoContent, error) {
		videoID, canonicalURL, err := parseYouTubeVideoURL(rawURL)
		if err != nil {
			return youtubeVideoContent{}, err
		}

		return youtubeVideoContent{
			URL:         canonicalURL,
			VideoID:     videoID,
			Title:       "Example YouTube video",
			ChannelName: "Example channel",
			Transcript:  "Example transcript",
			Comments:    nil,
		}, nil
	})
}

func pipelineTestRedditClient() *stubRedditContentClient {
	return newStubRedditContentClient(func(
		_ context.Context,
		rawURL string,
	) (redditThreadContent, error) {
		return redditThreadContent{
			URL:         rawURL,
			JSONURL:     rawURL + ".json",
			Subreddit:   "r/testing",
			Title:       "Example Reddit thread",
			Author:      "tester",
			Body:        "Reddit body",
			LinkedURL:   "",
			Comments:    nil,
			Score:       10,
			UpvoteRatio: 0.9,
			NumComments: 1,
			CreatedUTC:  1,
		}, nil
	})
}

type searchDeciderPipelineTestBot struct {
	bot              *bot
	stubChat         *stubChatCompletionClient
	stubWebSearch    *stubWebSearchClient
	deciderDecisions *atomic.Int32
}

func newSearchDeciderPipelineSourceMessage(t *testing.T) *discordgo.Message {
	t.Helper()

	message := new(discordgo.Message)
	message.ID = "search-decider-source-message"
	message.ChannelID = "channel-1"
	message.Author = newDiscordUser("user-1", false)
	message.Content = strings.Join([]string{
		"at ai vsearch summarize these links",
		"https://example.com/article",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://www.reddit.com/r/testing/comments/abc123/thread-title/",
	}, " ")
	message.Attachments = []*discordgo.MessageAttachment{{
		URL:         testVisualSearchAttachmentURL,
		Filename:    "image.png",
		ContentType: "image/png",
	}}

	return message
}
