package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type fakeRentryClient struct {
	url       string
	err       error
	callCount int
	texts     []string
}

type editedInteractionResponse struct {
	Content string `json:"content"`
}

type deferredInteractionCapture struct {
	requestCount     int
	deferredResponse discordgo.InteractionResponse
	editedResponse   editedInteractionResponse
}

var errFakeRentryUnavailable = errors.New("rentry unavailable")

func (client *fakeRentryClient) createEntry(_ context.Context, text string) (string, error) {
	client.callCount++
	client.texts = append(client.texts, text)

	return client.url, client.err
}

func TestHandleModelCommandAllowsNonAdminSwitch(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfig(t)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	interaction := newModelCommandInteraction("member-user", secondTestModel)

	err := instance.handleModelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle model command: %v", err)
	}

	if instance.currentModel != secondTestModel {
		t.Fatalf("unexpected current model: %q", instance.currentModel)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := fmt.Sprintf("Model switched to: `%s`", secondTestModel)
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleSearchDeciderModelCommandAllowsSwitch(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfig(t)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	instance.currentSearchDeciderModel = firstTestModel
	interaction := newSearchDeciderModelCommandInteraction("member-user", secondTestModel)

	err := instance.handleSearchDeciderModelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle search decider model command: %v", err)
	}

	if instance.currentSearchDeciderModel != secondTestModel {
		t.Fatalf("unexpected current search decider model: %q", instance.currentSearchDeciderModel)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := fmt.Sprintf("Search decider model switched to: `%s`", secondTestModel)
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleSearchTypeCommandAllowsSwitchWhenExaAPIConfigured(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfigWithExtra(
		t,
		`
web_search:
  exa:
    api_key: exa-key
`,
	)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	interaction := newSearchTypeCommandInteraction("member-user", exaSearchTypeDeepReasoning)

	err := instance.handleSearchTypeCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle search type command: %v", err)
	}

	if instance.currentExaSearchTypeValue != exaSearchTypeDeepReasoning {
		t.Fatalf("unexpected current Exa search type: %q", instance.currentExaSearchTypeValue)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := fmt.Sprintf("Exa search type switched to: `%s`", exaSearchTypeDeepReasoning)
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleSearchTypeCommandRejectsWhenExaAPIIsNotConfigured(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfig(t)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	interaction := newSearchTypeCommandInteraction("member-user", exaSearchTypeFast)

	err := instance.handleSearchTypeCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle search type command: %v", err)
	}

	if instance.currentExaSearchTypeValue != defaultExaSearchType {
		t.Fatalf("unexpected current Exa search type: %q", instance.currentExaSearchTypeValue)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := "Exa Search API is not configured. Set `web_search.exa.api_key` to use `/searchtype`."
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleSearchTypeAutocompleteListsAllOptions(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.currentExaSearchTypeValue = defaultExaSearchType

	err := instance.handleSearchTypeAutocomplete(
		session,
		newSearchTypeAutocompleteInteraction("member-user", ""),
	)
	if err != nil {
		t.Fatalf("handle search type autocomplete: %v", err)
	}

	if response.Type != discordgo.InteractionApplicationCommandAutocompleteResult {
		t.Fatalf("unexpected autocomplete response type: %v", response.Type)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	searchTypes := exaSearchTypes()

	if len(response.Data.Choices) != len(searchTypes) {
		t.Fatalf("unexpected choice count: %d", len(response.Data.Choices))
	}

	expectedNames := []string{
		"* auto (current)",
		"o fast",
		"o instant",
		"o deep",
		"o deep-reasoning",
	}

	for index, choice := range response.Data.Choices {
		if choice.Name != expectedNames[index] {
			t.Fatalf("unexpected choice name at %d: got %q want %q", index, choice.Name, expectedNames[index])
		}

		expectedValue := searchTypes[index]
		if choice.Value != expectedValue {
			t.Fatalf("unexpected choice value at %d: got %#v want %q", index, choice.Value, expectedValue)
		}
	}
}

func TestHandleModelCommandRejectsLockedChannelSwitch(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfigWithExtra(
		t,
		fmt.Sprintf(
			`
channel_model_locks:
  locked-channel: %s
`,
			secondTestModel,
		),
	)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	interaction := newModelCommandInteractionInChannel(
		"member-user",
		firstTestModel,
		"locked-channel",
	)

	err := instance.handleModelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle model command: %v", err)
	}

	if instance.currentModel != firstTestModel {
		t.Fatalf("unexpected current model: %q", instance.currentModel)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := fmt.Sprintf(
		"This channel is locked to `%s`. `/model` is disabled here.",
		secondTestModel,
	)
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestHandleSearchDeciderModelCommandAllowsSwitchInLockedChannel(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfigWithExtra(
		t,
		fmt.Sprintf(
			`
channel_model_locks:
  locked-channel: %s
`,
			secondTestModel,
		),
	)

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(configPath)
	instance.currentSearchDeciderModel = firstTestModel
	interaction := newSearchDeciderModelCommandInteractionInChannel(
		"member-user",
		secondTestModel,
		"locked-channel",
	)

	err := instance.handleSearchDeciderModelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle search decider model command: %v", err)
	}

	if instance.currentSearchDeciderModel != secondTestModel {
		t.Fatalf("unexpected current search decider model: %q", instance.currentSearchDeciderModel)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedContent := fmt.Sprintf("Search decider model switched to: `%s`", secondTestModel)
	if response.Data.Content != expectedContent {
		t.Fatalf("unexpected response content: got %q want %q", response.Data.Content, expectedContent)
	}
}

func TestLockedModelAutocompleteChoices(t *testing.T) {
	t.Parallel()

	choices := lockedModelAutocompleteChoices(secondTestModel, "second")
	if len(choices) != 1 {
		t.Fatalf("unexpected choice count: %d", len(choices))
	}

	if choices[0].Name != fmt.Sprintf("x %s (locked)", secondTestModel) {
		t.Fatalf("unexpected choice name: %q", choices[0].Name)
	}

	if choices[0].Value != secondTestModel {
		t.Fatalf("unexpected choice value: %#v", choices[0].Value)
	}

	choices = lockedModelAutocompleteChoices(secondTestModel, "first")
	if len(choices) != 0 {
		t.Fatalf("unexpected filtered choice count: %d", len(choices))
	}
}

func TestHandleInteractionCreateRespondsToShowSourcesButton(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.searchMetadata = &searchMetadata{
		Queries: []string{"latest ai news"},
		Results: []webSearchResult{{
			Query: "latest ai news",
			Text:  "Title: Example Source\nURL: https://example.com/source\n",
		}},
		MaxURLs:             defaultWebSearchMaxURLs,
		VisualSearchSources: nil,
	}
	node.mu.Unlock()

	interaction := newShowSourcesInteraction()

	instance.handleInteractionCreate(session, interaction)

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if response.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected response flags: %v", response.Data.Flags)
	}

	if !containsFold(response.Data.Content, "latest ai news") {
		t.Fatalf("expected query in response content: %q", response.Data.Content)
	}

	if !containsFold(response.Data.Content, "https://example.com/source") {
		t.Fatalf("expected source URL in response content: %q", response.Data.Content)
	}
}

func TestHandleInteractionCreateRespondsToShowSourcesButtonForVisualSearch(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.searchMetadata = testStructuredVisualSearchMetadata()
	node.mu.Unlock()

	interaction := newShowSourcesInteraction()

	instance.handleInteractionCreate(session, interaction)

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if response.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected response flags: %v", response.Data.Flags)
	}

	for _, fragment := range []string{
		"visual search result urls",
		"https://ru.ruwiki.ru/wiki/Sword_Art_Online",
		"https://yandex.com/images/search?cbir_page=similar-1",
		"http://vampireknightptk.blogspot.com/2012/09/indonic-hosting.html",
	} {
		if !containsFold(response.Data.Content, fragment) {
			t.Fatalf("expected fragment %q in response content: %q", fragment, response.Data.Content)
		}
	}
}

func TestHandleInteractionCreateRespondsToShowSourcesButtonAfterPendingRelease(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "source-message"

	tracker := newResponseTracker(sourceMessage, "")
	tracker.searchMetadata = &searchMetadata{
		Queries: []string{"latest ai news"},
		Results: []webSearchResult{{
			Query: "latest ai news",
			Text:  "Title: Example Source\nURL: https://example.com/source\n",
		}},
		MaxURLs:             defaultWebSearchMaxURLs,
		VisualSearchSources: nil,
	}
	tracker.pendingResponses = []pendingResponse{
		{
			messageID: "response-message",
			node:      instance.nodes.addPending("response-message", sourceMessage),
		},
	}

	tracker.release(instance.nodes, "assistant reply", "")

	interaction := newShowSourcesInteraction()

	instance.handleInteractionCreate(session, interaction)

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if response.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected response flags: %v", response.Data.Flags)
	}

	if !containsFold(response.Data.Content, "latest ai news") {
		t.Fatalf("expected query in response content: %q", response.Data.Content)
	}

	if !containsFold(response.Data.Content, "https://example.com/source") {
		t.Fatalf("expected source URL in response content: %q", response.Data.Content)
	}
}

func TestHandleInteractionCreateRespondsToPaginatedShowSourcesButton(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.searchMetadata = testPaginatedSearchMetadata()
	node.mu.Unlock()

	interaction := newShowSourcesInteraction()

	instance.handleInteractionCreate(session, interaction)

	if response.Type != discordgo.InteractionResponseChannelMessageWithSource {
		t.Fatalf("unexpected response type: %v", response.Type)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if response.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected response flags: %v", response.Data.Flags)
	}

	if !containsFold(response.Data.Content, "page 1/") {
		t.Fatalf("expected first page indicator in response content: %q", response.Data.Content)
	}

	if containsFold(response.Data.Content, "... truncated") {
		t.Fatalf("expected paginated response to avoid truncation marker: %q", response.Data.Content)
	}

	if len(response.Data.Components) != 1 {
		t.Fatalf("unexpected component count: %d", len(response.Data.Components))
	}

	row, rowOK := response.Data.Components[0].(*discordgo.ActionsRow)
	if !rowOK {
		t.Fatalf("expected actions row, got %T", response.Data.Components[0])
	}

	if len(row.Components) != 2 {
		t.Fatalf("unexpected pagination button count: %d", len(row.Components))
	}

	previousButton, previousOK := row.Components[0].(*discordgo.Button)
	if !previousOK {
		t.Fatalf("expected previous button, got %T", row.Components[0])
	}

	if previousButton.Label != showSourcesPreviousButtonLabel {
		t.Fatalf("unexpected previous button label: %q", previousButton.Label)
	}

	if !previousButton.Disabled {
		t.Fatal("expected previous button to be disabled on the first page")
	}

	nextButton, nextOK := row.Components[1].(*discordgo.Button)
	if !nextOK {
		t.Fatalf("expected next button, got %T", row.Components[1])
	}

	if nextButton.Label != showSourcesNextButtonLabel {
		t.Fatalf("unexpected next button label: %q", nextButton.Label)
	}

	if nextButton.Disabled {
		t.Fatal("expected next button to be enabled on the first page")
	}

	messageID, pageIndex, ok := parseShowSourcesPageButtonCustomID(nextButton.CustomID)
	if !ok {
		t.Fatalf("expected parsable next button custom id: %q", nextButton.CustomID)
	}

	if messageID != "response-message" || pageIndex != 1 {
		t.Fatalf("unexpected next button target: message=%q page=%d", messageID, pageIndex)
	}
}

func TestHandleInteractionCreateRespondsToShowThinkingButton(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = visibleResponseText("Plan first.", "Final answer.")
	node.thinkingText = "Plan first."
	node.initialized = true
	node.mu.Unlock()

	interaction := newShowThinkingInteraction()

	instance.handleInteractionCreate(session, interaction)

	if response.Type != discordgo.InteractionResponseChannelMessageWithSource {
		t.Fatalf("unexpected response type: %v", response.Type)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if response.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected response flags: %v", response.Data.Flags)
	}

	if !containsFold(response.Data.Content, "Thinking Process") {
		t.Fatalf("expected thinking header in response content: %q", response.Data.Content)
	}

	if !containsFold(response.Data.Content, "Plan first.") {
		t.Fatalf("expected thinking content in response: %q", response.Data.Content)
	}
}

func TestHandleInteractionCreateRespondsToShowThinkingButtonUsingPersistedFallback(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = visibleResponseText("Plan first.", "Final answer.")
	node.initialized = true
	node.mu.Unlock()

	interaction := newShowThinkingInteraction()

	instance.handleInteractionCreate(session, interaction)

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if !containsFold(response.Data.Content, "Plan first.") {
		t.Fatalf("expected extracted thinking content in response: %q", response.Data.Content)
	}
}

func TestHandleInteractionCreateRespondsToPaginatedShowThinkingButton(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.thinkingText = strings.Repeat("Thoughts on the problem.\n", 200)
	node.initialized = true
	node.mu.Unlock()

	interaction := newShowThinkingInteraction()

	instance.handleInteractionCreate(session, interaction)

	if response.Type != discordgo.InteractionResponseChannelMessageWithSource {
		t.Fatalf("unexpected response type: %v", response.Type)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if !containsFold(response.Data.Content, "page 1/") {
		t.Fatalf("expected first page indicator in response content: %q", response.Data.Content)
	}

	if len(response.Data.Components) != 1 {
		t.Fatalf("unexpected component count: %d", len(response.Data.Components))
	}

	row, rowOK := response.Data.Components[0].(*discordgo.ActionsRow)
	if !rowOK {
		t.Fatalf("expected actions row, got %T", response.Data.Components[0])
	}

	if len(row.Components) != 2 {
		t.Fatalf("unexpected pagination button count: %d", len(row.Components))
	}

	nextButton, nextOK := row.Components[1].(*discordgo.Button)
	if !nextOK {
		t.Fatalf("expected next button, got %T", row.Components[1])
	}

	messageID, pageIndex, ok := parseShowThinkingPageButtonCustomID(nextButton.CustomID)
	if !ok {
		t.Fatalf("expected parsable next button custom id: %q", nextButton.CustomID)
	}

	if messageID != "response-message" || pageIndex != 1 {
		t.Fatalf("unexpected next button target: message=%q page=%d", messageID, pageIndex)
	}
}

func TestHandleInteractionCreateUpdatesShowThinkingPaginationPage(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.thinkingText = strings.Repeat("Thoughts on the problem.\n", 200)
	node.mu.Unlock()

	pageCount := len(formatThinkingPages(node.thinkingText))
	if pageCount < 2 {
		t.Fatalf("expected multiple pages, got %d", pageCount)
	}

	targetPageIndex := pageCount - 1
	interaction := newComponentInteraction(
		"ephemeral-message",
		showThinkingPageButtonCustomID("response-message", targetPageIndex),
	)

	instance.handleInteractionCreate(session, interaction)

	if response.Type != discordgo.InteractionResponseUpdateMessage {
		t.Fatalf("unexpected response type: %v", response.Type)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedPageIndicator := fmt.Sprintf("page %d/%d", targetPageIndex+1, pageCount)
	if !containsFold(response.Data.Content, expectedPageIndicator) {
		t.Fatalf("expected page indicator %q in response content: %q", expectedPageIndicator, response.Data.Content)
	}

	if len(response.Data.Components) != 1 {
		t.Fatalf("unexpected component count: %d", len(response.Data.Components))
	}

	row, rowOK := response.Data.Components[0].(*discordgo.ActionsRow)
	if !rowOK {
		t.Fatalf("expected actions row, got %T", response.Data.Components[0])
	}

	if len(row.Components) != 2 {
		t.Fatalf("unexpected pagination button count: %d", len(row.Components))
	}

	previousButton, previousOK := row.Components[0].(*discordgo.Button)
	if !previousOK {
		t.Fatalf("expected previous button, got %T", row.Components[0])
	}

	if previousButton.Disabled {
		t.Fatal("expected previous button to be enabled on the final page")
	}

	nextButton, nextOK := row.Components[1].(*discordgo.Button)
	if !nextOK {
		t.Fatalf("expected next button, got %T", row.Components[1])
	}

	if !nextButton.Disabled {
		t.Fatal("expected next button to be disabled on the final page")
	}
}

func TestHandleInteractionCreateUpdatesShowSourcesPaginationPage(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)

	metadata := testPaginatedSearchMetadata()
	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.searchMetadata = metadata
	node.mu.Unlock()

	pageCount := len(formatSearchSourcesPages(metadata))
	if pageCount < 2 {
		t.Fatalf("expected multiple pages, got %d", pageCount)
	}

	targetPageIndex := pageCount - 1
	interaction := newComponentInteraction(
		"ephemeral-message",
		showSourcesPageButtonCustomID("response-message", targetPageIndex),
	)

	instance.handleInteractionCreate(session, interaction)

	if response.Type != discordgo.InteractionResponseUpdateMessage {
		t.Fatalf("unexpected response type: %v", response.Type)
	}

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	expectedPageIndicator := fmt.Sprintf("page %d/%d", targetPageIndex+1, pageCount)
	if !containsFold(response.Data.Content, expectedPageIndicator) {
		t.Fatalf("expected page indicator %q in response content: %q", expectedPageIndicator, response.Data.Content)
	}

	if !containsFold(response.Data.Content, "https://example.com/agent-frameworks/5") {
		t.Fatalf("expected last source URL in final page content: %q", response.Data.Content)
	}

	if len(response.Data.Components) != 1 {
		t.Fatalf("unexpected component count: %d", len(response.Data.Components))
	}

	row, rowOK := response.Data.Components[0].(*discordgo.ActionsRow)
	if !rowOK {
		t.Fatalf("expected actions row, got %T", response.Data.Components[0])
	}

	if len(row.Components) != 2 {
		t.Fatalf("unexpected pagination button count: %d", len(row.Components))
	}

	previousButton, previousOK := row.Components[0].(*discordgo.Button)
	if !previousOK {
		t.Fatalf("expected previous button, got %T", row.Components[0])
	}

	if previousButton.Disabled {
		t.Fatal("expected previous button to be enabled on the final page")
	}

	nextButton, nextOK := row.Components[1].(*discordgo.Button)
	if !nextOK {
		t.Fatalf("expected next button, got %T", row.Components[1])
	}

	if !nextButton.Disabled {
		t.Fatal("expected next button to be disabled on the final page")
	}
}

func TestHandleInteractionCreateRespondsToViewOnRentryButton(t *testing.T) {
	t.Parallel()

	rentry := new(fakeRentryClient)
	rentry.url = "https://rentry.co/example"

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.rentry = rentry

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = testAssistantReply
	node.initialized = true
	node.mu.Unlock()

	interaction := newComponentInteraction("response-message", viewOnRentryButtonCustomID)

	instance.handleInteractionCreate(session, interaction)

	assertDeferredEphemeralInteractionResponse(t, &capture.deferredResponse)

	if !containsFold(capture.editedResponse.Content, rentry.url) {
		t.Fatalf("expected Rentry url in edited response content: %q", capture.editedResponse.Content)
	}

	if rentry.callCount != 1 {
		t.Fatalf("unexpected Rentry call count: %d", rentry.callCount)
	}

	if len(rentry.texts) != 1 || rentry.texts[0] != testAssistantReply {
		t.Fatalf("unexpected Rentry request texts: %#v", rentry.texts)
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	if node.rentryURL != rentry.url {
		t.Fatalf("unexpected cached Rentry url: %q", node.rentryURL)
	}

	if capture.requestCount != 2 {
		t.Fatalf("unexpected request count: %d", capture.requestCount)
	}
}

func TestHandleInteractionCreateReusesCachedRentryURL(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	rentry := new(fakeRentryClient)

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.rentry = rentry

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = testAssistantReply
	node.initialized = true
	node.rentryURL = "https://rentry.co/cached"
	node.mu.Unlock()

	interaction := newComponentInteraction("response-message", viewOnRentryButtonCustomID)

	instance.handleInteractionCreate(session, interaction)

	if response.Data == nil {
		t.Fatal("expected interaction response data")
	}

	if !containsFold(response.Data.Content, node.rentryURL) {
		t.Fatalf("expected cached Rentry url in response content: %q", response.Data.Content)
	}

	if rentry.callCount != 0 {
		t.Fatalf("expected cached Rentry url to skip creation, got %d calls", rentry.callCount)
	}
}

func TestHandleInteractionCreateRespondsToViewOnRentryButtonFailure(t *testing.T) {
	t.Parallel()

	rentry := new(fakeRentryClient)
	rentry.err = errFakeRentryUnavailable

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.rentry = rentry

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.text = testAssistantReply
	node.initialized = true
	node.mu.Unlock()

	interaction := newComponentInteraction("response-message", viewOnRentryButtonCustomID)

	instance.handleInteractionCreate(session, interaction)

	assertDeferredEphemeralInteractionResponse(t, &capture.deferredResponse)

	expectedContent := "Couldn't create a Rentry page right now."
	if capture.editedResponse.Content != expectedContent {
		t.Fatalf(
			"unexpected edited failure response content: got %q want %q",
			capture.editedResponse.Content,
			expectedContent,
		)
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	if node.rentryURL != "" {
		t.Fatalf("expected empty cached Rentry url, got %q", node.rentryURL)
	}

	if capture.requestCount != 2 {
		t.Fatalf("unexpected request count: %d", capture.requestCount)
	}
}

func TestIsUnknownInteractionError(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf(
		"wrap: %w",
		newDiscordRESTError(discordUnknownInteractionCode, "Unknown interaction"),
	)

	if !isUnknownInteractionError(err) {
		t.Fatal("expected unknown interaction error to be detected")
	}
}

func TestIsUnknownInteractionErrorIgnoresOtherErrors(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf(
		"wrap: %w",
		newDiscordRESTError(http.StatusNotFound, "Not Found"),
	)

	if isUnknownInteractionError(err) {
		t.Fatal("expected non-interaction error to be ignored")
	}
}

func writeModelConfig(t *testing.T) string {
	t.Helper()

	return writeModelConfigWithExtra(t, "")
}

func writeModelConfigWithExtra(t *testing.T, extraText string) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configText := fmt.Sprintf(`
bot_token: discord-token
permissions:
  users:
    admin_ids: []
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  %s:
  %s:
`, firstTestModel, secondTestModel) + extraText

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	return configPath
}

func newInteractionTestSession(
	t *testing.T,
	response *discordgo.InteractionResponse,
) *discordgo.Session {
	t.Helper()

	return newInteractionTestSessionWithTransport(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if request.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", request.Method)
		}

		expectedPath := "/api/v9/interactions/interaction-id/interaction-token/callback"
		if request.URL.Path != expectedPath {
			t.Fatalf("unexpected request path: got %q want %q", request.URL.Path, expectedPath)
		}

		responseBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}

		var decoded struct {
			Type discordgo.InteractionResponseType `json:"type"`
			Data *struct {
				Content    string                                      `json:"content"`
				Flags      discordgo.MessageFlags                      `json:"flags,omitempty"`
				Choices    []*discordgo.ApplicationCommandOptionChoice `json:"choices,omitempty"`
				Components []json.RawMessage                           `json:"components"`
			} `json:"data"`
		}

		err = json.Unmarshal(responseBody, &decoded)
		if err != nil {
			t.Fatalf("decode interaction response: %v", err)
		}

		response.Type = decoded.Type
		response.Data = nil

		if decoded.Data != nil {
			response.Data = new(discordgo.InteractionResponseData)
			response.Data.Content = decoded.Data.Content
			response.Data.Flags = decoded.Data.Flags
			response.Data.Choices = decoded.Data.Choices

			if decoded.Data.Components != nil {
				response.Data.Components = make([]discordgo.MessageComponent, 0, len(decoded.Data.Components))
				for _, rawComponent := range decoded.Data.Components {
					component, componentErr := discordgo.MessageComponentFromJSON(rawComponent)
					if componentErr != nil {
						t.Fatalf("decode interaction component: %v", componentErr)
					}

					response.Data.Components = append(response.Data.Components, component)
				}
			}
		}

		return newNoContentResponse(request), nil
	}))
}

func newInteractionTestSessionWithTransport(
	t *testing.T,
	transport roundTripFunc,
) *discordgo.Session {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	client := new(http.Client)
	client.Transport = transport
	session.Client = client

	return session
}

func newDeferredInteractionTestSession(
	t *testing.T,
	capture *deferredInteractionCapture,
) *discordgo.Session {
	t.Helper()

	return newInteractionTestSessionWithTransport(
		t,
		roundTripFunc(func(request *http.Request) (*http.Response, error) {
			t.Helper()

			capture.requestCount++

			switch capture.requestCount {
			case 1:
				return captureDeferredInteractionRequest(t, request, &capture.deferredResponse)
			case 2:
				return captureEditedInteractionRequest(t, request, &capture.editedResponse)
			default:
				t.Fatalf("unexpected interaction request count: %d", capture.requestCount)

				return nil, errUnexpectedTestRequest
			}
		}),
	)
}

func newNoContentResponse(request *http.Request) *http.Response {
	response := new(http.Response)
	response.Status = "204 No Content"
	response.StatusCode = http.StatusNoContent
	response.Body = http.NoBody
	response.ContentLength = 0
	response.Header = make(http.Header)
	response.Request = request

	return response
}

func newInteractionJSONResponse(request *http.Request, statusCode int, body string) *http.Response {
	response := new(http.Response)
	response.Status = fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode))
	response.StatusCode = statusCode
	response.Body = io.NopCloser(strings.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header = make(http.Header)
	response.Header.Set("Content-Type", "application/json")
	response.Request = request

	return response
}

func captureDeferredInteractionRequest(
	t *testing.T,
	request *http.Request,
	response *discordgo.InteractionResponse,
) (*http.Response, error) {
	t.Helper()

	if request.Method != http.MethodPost {
		t.Fatalf("unexpected method: %s", request.Method)
	}

	expectedPath := "/api/v9/interactions/interaction-id/interaction-token/callback"
	if request.URL.Path != expectedPath {
		t.Fatalf("unexpected request path: got %q want %q", request.URL.Path, expectedPath)
	}

	responseBody, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("read deferred response body: %v", err)
	}

	var decoded struct {
		Type discordgo.InteractionResponseType `json:"type"`
		Data *struct {
			Flags discordgo.MessageFlags `json:"flags,omitempty"`
		} `json:"data"`
	}

	err = json.Unmarshal(responseBody, &decoded)
	if err != nil {
		t.Fatalf("decode deferred interaction response: %v", err)
	}

	response.Type = decoded.Type
	if decoded.Data != nil {
		response.Data = new(discordgo.InteractionResponseData)
		response.Data.Flags = decoded.Data.Flags
	}

	return newNoContentResponse(request), nil
}

func captureEditedInteractionRequest(
	t *testing.T,
	request *http.Request,
	response *editedInteractionResponse,
) (*http.Response, error) {
	t.Helper()

	if request.Method != http.MethodPatch {
		t.Fatalf("unexpected edit method: %s", request.Method)
	}

	expectedPath := "/api/v9/webhooks/application-id/interaction-token/messages/@original"
	if request.URL.Path != expectedPath {
		t.Fatalf("unexpected edit path: got %q want %q", request.URL.Path, expectedPath)
	}

	responseBody, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("read edited response body: %v", err)
	}

	err = json.Unmarshal(responseBody, response)
	if err != nil {
		t.Fatalf("decode edited interaction response: %v", err)
	}

	return newInteractionJSONResponse(request, http.StatusOK, `{"id":"edited-message"}`), nil
}

func assertDeferredEphemeralInteractionResponse(
	t *testing.T,
	response *discordgo.InteractionResponse,
) {
	t.Helper()

	if response.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
		t.Fatalf(
			"unexpected deferred response type: got %v want %v",
			response.Type,
			discordgo.InteractionResponseDeferredChannelMessageWithSource,
		)
	}

	if response.Data == nil {
		t.Fatal("expected deferred interaction response data")
	}

	if response.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected deferred response flags: %v", response.Data.Flags)
	}
}

func newDiscordRESTError(code int, message string) *discordgo.RESTError {
	return &discordgo.RESTError{
		Request:      nil,
		Response:     nil,
		ResponseBody: nil,
		Message: &discordgo.APIErrorMessage{
			Code:    code,
			Message: message,
		},
	}
}

func newModelTestBot(configPath string) *bot {
	instance := new(bot)
	instance.configPath = configPath
	instance.currentModel = firstTestModel
	instance.currentExaSearchTypeValue = defaultExaSearchType

	return instance
}

func newModelCommandInteraction(userID string, modelName string) *discordgo.InteractionCreate {
	return newModelCommandInteractionInChannel(userID, modelName, "")
}

func newModelCommandInteractionInChannel(
	userID string,
	modelName string,
	channelID string,
) *discordgo.InteractionCreate {
	return newConfiguredModelCommandInteraction(
		userID,
		modelName,
		modelCommandName,
		modelOptionName,
		channelID,
	)
}

func newSearchDeciderModelCommandInteraction(
	userID string,
	modelName string,
) *discordgo.InteractionCreate {
	return newSearchDeciderModelCommandInteractionInChannel(userID, modelName, "")
}

func newSearchDeciderModelCommandInteractionInChannel(
	userID string,
	modelName string,
	channelID string,
) *discordgo.InteractionCreate {
	return newConfiguredModelCommandInteraction(
		userID,
		modelName,
		searchDeciderModelCommandName,
		searchDeciderModelOptionName,
		channelID,
	)
}

func newSearchTypeCommandInteraction(userID string, searchType string) *discordgo.InteractionCreate {
	return newConfiguredStringCommandInteraction(
		userID,
		searchType,
		searchTypeCommandName,
		searchTypeOptionName,
		"",
		discordgo.InteractionApplicationCommand,
	)
}

func newSearchTypeAutocompleteInteraction(userID string, currentText string) *discordgo.InteractionCreate {
	return newConfiguredStringCommandInteraction(
		userID,
		currentText,
		searchTypeCommandName,
		searchTypeOptionName,
		"",
		discordgo.InteractionApplicationCommandAutocomplete,
	)
}

func newConfiguredModelCommandInteraction(
	userID string,
	modelName string,
	commandName string,
	optionName string,
	channelID string,
) *discordgo.InteractionCreate {
	return newConfiguredStringCommandInteraction(
		userID,
		modelName,
		commandName,
		optionName,
		channelID,
		discordgo.InteractionApplicationCommand,
	)
}

func newConfiguredStringCommandInteraction(
	userID string,
	optionValue string,
	commandName string,
	optionName string,
	channelID string,
	interactionType discordgo.InteractionType,
) *discordgo.InteractionCreate {
	user := new(discordgo.User)
	user.ID = userID

	member := new(discordgo.Member)
	member.User = user

	option := new(discordgo.ApplicationCommandInteractionDataOption)
	option.Name = optionName
	option.Type = discordgo.ApplicationCommandOptionString
	option.Value = optionValue
	option.Focused = interactionType == discordgo.InteractionApplicationCommandAutocomplete

	var commandData discordgo.ApplicationCommandInteractionData

	commandData.Name = commandName
	commandData.Options = []*discordgo.ApplicationCommandInteractionDataOption{option}

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.AppID = "application-id"
	interaction.Token = "interaction-token"
	interaction.Type = interactionType
	interaction.ChannelID = channelID
	interaction.Member = member
	interaction.Data = commandData

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}

func newShowSourcesInteraction() *discordgo.InteractionCreate {
	return newComponentInteraction("response-message", showSourcesButtonCustomID)
}

func newShowThinkingInteraction() *discordgo.InteractionCreate {
	return newComponentInteraction("response-message", showThinkingButtonCustomID)
}

func newComponentInteraction(messageID string, customID string) *discordgo.InteractionCreate {
	message := new(discordgo.Message)
	message.ID = messageID

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.AppID = "application-id"
	interaction.Token = "interaction-token"
	interaction.Type = discordgo.InteractionMessageComponent
	interaction.Message = message

	componentData := new(discordgo.MessageComponentInteractionData)
	componentData.CustomID = customID
	interaction.Data = *componentData

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction

	return result
}
