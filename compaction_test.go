package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const testAutoCompactMainModel = "openai/main-model"

func TestAutoCompactRequestAddsSummaryAndPreservesRecentMessages(t *testing.T) {
	t.Parallel()

	client := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if len(request.Messages) != 2 {
			t.Fatalf("unexpected compaction request message count: %d", len(request.Messages))
		}

		systemPrompt, _ := request.Messages[0].Content.(string)
		if systemPrompt != autoCompactSummarySystemPrompt() &&
			systemPrompt != autoCompactMergeSystemPrompt() {
			t.Fatalf("unexpected compaction system prompt: %q", systemPrompt)
		}

		return handle(newStreamDelta("Condensed earlier context.", ""))
	})

	instance := new(bot)
	instance.chatCompletions = client

	var originalRequest chatCompletionRequest

	originalRequest.ConfiguredModel = testAutoCompactMainModel
	originalRequest.ContextWindow = 200
	originalRequest.Messages = []chatMessage{
		{Role: openAICodexRoleSystem, Content: "Always be helpful."},
		{Role: messageRoleUser, Content: repeatedAutoCompactText("older details", 80)},
		{Role: messageRoleAssistant, Content: "Earlier answer."},
		{Role: messageRoleUser, Content: "Second question."},
		{Role: messageRoleAssistant, Content: "Second answer."},
		{Role: messageRoleUser, Content: "Latest question."},
	}

	compactedRequest, result := instance.autoCompactRequest(context.Background(), originalRequest)
	if !result.Applied {
		t.Fatal("expected auto compaction to apply")
	}

	if result.Strategy != autoCompactStrategySummary {
		t.Fatalf("unexpected auto compaction strategy: %q", result.Strategy)
	}

	if len(client.requests) == 0 {
		t.Fatal("expected at least one compaction request")
	}

	if len(compactedRequest.Messages) != 6 {
		t.Fatalf("unexpected compacted request length: %d", len(compactedRequest.Messages))
	}

	if compactedRequest.Messages[0] != originalRequest.Messages[0] {
		t.Fatalf("expected leading system message to be preserved: %#v", compactedRequest.Messages[0])
	}

	summaryText, ok := compactedRequest.Messages[1].Content.(string)
	if !ok {
		t.Fatalf("unexpected summary content type: %T", compactedRequest.Messages[1].Content)
	}

	if !strings.Contains(summaryText, autoCompactSummaryPrefix) {
		t.Fatalf("expected summary prefix in compacted message: %q", summaryText)
	}

	if !strings.Contains(summaryText, "Condensed earlier context.") {
		t.Fatalf("expected summarized content in compacted message: %q", summaryText)
	}

	for index := 2; index < len(compactedRequest.Messages); index++ {
		if compactedRequest.Messages[index] != originalRequest.Messages[index] {
			t.Fatalf(
				"expected tail message %d to be preserved: got %#v want %#v",
				index,
				compactedRequest.Messages[index],
				originalRequest.Messages[index],
			)
		}
	}
}

func TestDecideWebSearchAutoCompactsSearchDeciderRequest(t *testing.T) {
	t.Parallel()

	client := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if len(request.Messages) >= 1 {
			systemPrompt, _ := request.Messages[0].Content.(string)
			if systemPrompt == autoCompactSummarySystemPrompt() ||
				systemPrompt == autoCompactMergeSystemPrompt() {
				return handle(newStreamDelta("Earlier thread summary.", ""))
			}
		}

		if request.ConfiguredModel != testSearchDeciderModel {
			t.Fatalf("unexpected configured model: %q", request.ConfiguredModel)
		}

		renderedConversation := renderMessagesForAutoCompaction(request.Messages)
		if !containsFold(strings.Join(renderedConversation, "\n\n"), autoCompactSummaryPrefix) {
			t.Fatalf("expected compacted summary in search decider request: %#v", request.Messages)
		}

		return handle(newStreamDelta(`{"needs_search":false,"queries":[]}`, ""))
	})

	instance := newSearchTestBot(client, newStubWebSearchClient(func(
		context.Context,
		config,
		[]string,
	) ([]webSearchResult, error) {
		return nil, nil
	}))
	loadedConfig := testSearchConfig()
	loadedConfig.ModelContextWindows = map[string]int{
		testSearchDeciderModel: 1700,
	}

	conversation := []chatMessage{
		{Role: messageRoleUser, Content: repeatedAutoCompactText("very old context", 100)},
		{Role: messageRoleAssistant, Content: "Assistant reply about the older context."},
		{Role: messageRoleUser, Content: "Should we search for the latest version?"},
	}

	decision, warnings, err := instance.decideWebSearch(
		context.Background(),
		loadedConfig,
		"openai/main-model",
		nil,
		conversation,
	)
	if err != nil {
		t.Fatalf("decide web search: %v", err)
	}

	if decision.NeedsSearch {
		t.Fatal("expected decider to skip web search")
	}

	expectedWarning := autoCompactResult{
		Applied:  true,
		Strategy: autoCompactStrategySummary,
	}.warningForPath("search decider")
	if len(warnings) != 1 || warnings[0] != expectedWarning {
		t.Fatalf("unexpected compaction warnings: %#v", warnings)
	}

	if len(client.requests) < 2 {
		t.Fatalf("expected compaction and final search decider requests: %d", len(client.requests))
	}
}

type prepareMessageResponseAutoCompactFixture struct {
	instance      *bot
	loadedConfig  config
	sourceMessage *discordgo.Message
}

func TestPrepareMessageResponseAutoCompactsMainRequest(t *testing.T) {
	t.Parallel()

	fixture := newPrepareMessageResponseAutoCompactFixture(t)

	progress := fixture.instance.startRequestProgress(fixture.sourceMessage, testAutoCompactMainModel)

	request, tracker, warnings, err := fixture.instance.prepareMessageResponse(
		context.Background(),
		fixture.loadedConfig,
		fixture.sourceMessage,
		testAutoCompactMainModel,
		progress,
	)

	if tracker != nil {
		tracker.release(fixture.instance.nodes, "test response", "")
	}

	if err != nil {
		t.Fatalf("prepare message response: %v", err)
	}

	expectedWarning := autoCompactResult{
		Applied:  true,
		Strategy: autoCompactStrategySummary,
	}.warningForPath("main model")
	if !containsFold(strings.Join(warnings, "\n"), expectedWarning) {
		t.Fatalf("expected auto compaction warning in response warnings: %#v", warnings)
	}

	if len(request.Messages) < 3 {
		t.Fatalf("unexpected compacted main request length: %d", len(request.Messages))
	}

	if request.Messages[0].Role != openAICodexRoleSystem {
		t.Fatalf("expected system prompt to stay first: %#v", request.Messages[0])
	}

	summaryText, ok := request.Messages[1].Content.(string)
	if !ok {
		t.Fatalf("unexpected main summary content type: %T", request.Messages[1].Content)
	}

	if !strings.Contains(summaryText, autoCompactSummaryPrefix) {
		t.Fatalf("expected main request summary prefix: %q", summaryText)
	}

	if !strings.Contains(summaryText, "Main request summary.") {
		t.Fatalf("expected main request summary text: %q", summaryText)
	}
}

func newPrepareMessageResponseAutoCompactFixture(
	t *testing.T,
) prepareMessageResponseAutoCompactFixture {
	t.Helper()

	const (
		botUserID          = "bot-user"
		channelID          = "channel-1"
		userID             = "user-1"
		progressMessageID  = "progress-message"
		oldestMessageID    = "user-message-1"
		assistantMessageID = "assistant-message-1"
		followUpMessageID  = "user-message-2"
	)

	session := newPrepareMessageResponseAutoCompactSession(
		t,
		channelID,
		botUserID,
		progressMessageID,
	)
	client := newPrepareMessageResponseAutoCompactClient(t)

	instance := new(bot)
	instance.session = session
	instance.chatCompletions = client
	instance.nodes = newMessageNodeStore(10)

	oldestMessage := new(discordgo.Message)
	oldestMessage.ID = oldestMessageID
	oldestMessage.ChannelID = channelID
	oldestMessage.Author = newDiscordUser(userID, false)
	oldestMessage.Content = "at ai " + repeatedAutoCompactText("older context", 100)

	assistantMessage := new(discordgo.Message)
	assistantMessage.ID = assistantMessageID
	assistantMessage.ChannelID = channelID
	assistantMessage.Author = newDiscordUser(botUserID, true)
	assistantMessage.Type = discordgo.MessageTypeReply
	assistantMessage.Content = repeatedAutoCompactText("assistant answer", 40)
	assistantMessage.MessageReference = oldestMessage.Reference()
	assistantMessage.ReferencedMessage = oldestMessage

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = followUpMessageID
	sourceMessage.ChannelID = channelID
	sourceMessage.Author = newDiscordUser(userID, false)
	sourceMessage.Content = "<@" + botUserID + "> " + repeatedAutoCompactText("follow up question", 60)
	sourceMessage.Mentions = []*discordgo.User{newDiscordUser(botUserID, false)}
	sourceMessage.MessageReference = assistantMessage.Reference()
	sourceMessage.ReferencedMessage = assistantMessage

	loadedConfig := testSearchConfig()
	loadedConfig.MaxText = defaultMaxText
	loadedConfig.MaxImages = defaultMaxImages
	loadedConfig.MaxMessages = defaultMaxMessages
	loadedConfig.SystemPrompt = "Always help the user."
	loadedConfig.ModelContextWindows = map[string]int{
		testAutoCompactMainModel: 800,
		testSearchDeciderModel:   100000,
	}

	return prepareMessageResponseAutoCompactFixture{
		instance:      instance,
		loadedConfig:  loadedConfig,
		sourceMessage: sourceMessage,
	}
}

func newPrepareMessageResponseAutoCompactSession(
	t *testing.T,
	channelID string,
	botUserID string,
	progressMessageID string,
) *discordgo.Session {
	t.Helper()

	progressMessage := new(discordgo.Message)
	progressMessage.ID = progressMessageID
	progressMessage.ChannelID = channelID
	progressMessage.Author = newDiscordUser(botUserID, true)

	return newDirectMessageTestSession(t, channelID, botUserID, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages":
			return newJSONResponse(t, request, progressMessage), nil
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/v9/channels/"+channelID+"/messages/"+progressMessageID:
			return newJSONResponse(t, request, progressMessage), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}))
}

func newPrepareMessageResponseAutoCompactClient(
	t *testing.T,
) *stubChatCompletionClient {
	t.Helper()

	return newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		t.Helper()

		if request.ConfiguredModel == testSearchDeciderModel {
			return handle(newStreamDelta(`{"needs_search":false,"queries":[]}`, ""))
		}

		if len(request.Messages) >= 1 {
			systemPrompt, _ := request.Messages[0].Content.(string)
			if systemPrompt == autoCompactSummarySystemPrompt() ||
				systemPrompt == autoCompactMergeSystemPrompt() {
				return handle(newStreamDelta("Main request summary.", ""))
			}
		}

		t.Fatalf("unexpected request during prepareMessageResponse: %#v", request.Messages)

		return nil
	})
}

func repeatedAutoCompactText(fragment string, repeats int) string {
	return strings.TrimSpace(strings.Repeat(fragment+" ", repeats))
}
