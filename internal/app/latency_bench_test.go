package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	providers "llmcord-go/internal/providers"

	"github.com/bwmarrin/discordgo"
)

func newLatencyBenchTestConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.Models = map[string]map[string]any{
		firstTestModel:  nil,
		secondTestModel: nil,
	}
	loadedConfig.ModelOrder = []string{firstTestModel, secondTestModel}
	loadedConfig.WebSearch.Tavily = tavilySearchConfig{
		APIKey:  testWebSearchTavilyKey,
		APIKeys: []string{testWebSearchTavilyKey},
	}
	loadedConfig.ChannelModelLocks = map[string]string{
		"channel-a": firstTestModel,
		"channel-b": secondTestModel,
	}

	return loadedConfig
}

func TestNewLatencyBenchCommand(t *testing.T) {
	t.Parallel()

	command := newLatencyBenchCommand()
	if command.Name != latencyBenchCommandName {
		t.Fatalf("command name = %q, want %q", command.Name, latencyBenchCommandName)
	}

	if command.Type != discordgo.ChatApplicationCommand {
		t.Fatalf("command type = %v, want chat", command.Type)
	}

	if len(command.Options) != 1 {
		t.Fatalf("option count = %d, want 1", len(command.Options))
	}

	option := command.Options[0]
	if option.Name != latencyBenchModelOptionName ||
		option.Type != discordgo.ApplicationCommandOptionString ||
		option.Required ||
		!option.Autocomplete {
		t.Fatalf("unexpected model option: %+v", option)
	}
}

func TestLatencyBenchChannelLink(t *testing.T) {
	t.Parallel()

	got := latencyBenchChannelLink("978995704666136646", "1465011430775590942")
	want := "https://discord.com/channels/978995704666136646/1465011430775590942"

	if got != want {
		t.Fatalf("channel link = %q, want %q", got, want)
	}

	if got := latencyBenchChannelLink("", "1465011430775590942"); got != "https://discord.com/channels/@me/1465011430775590942" {
		t.Fatalf("DM channel link = %q", got)
	}
}

func TestLatencyBenchTargetsSkipsBlanksAndSorts(t *testing.T) {
	t.Parallel()

	targets := latencyBenchTargets(map[string]string{
		"channel-b": secondTestModel,
		"":          firstTestModel,
		"channel-a": firstTestModel,
		"channel-c": "  ",
	})

	if len(targets) != 2 {
		t.Fatalf("target count = %d, want 2", len(targets))
	}

	if targets[0].channelID != "channel-a" || targets[1].channelID != "channel-b" {
		t.Fatalf("unexpected targets: %#v", targets)
	}
}

func TestRankLatencyBenchResultsOrdersFailuresLast(t *testing.T) {
	t.Parallel()

	results := []latencyBenchResult{
		{model: secondTestModel, channelID: "channel-b", latency: 300 * time.Millisecond},
		{model: "openai/slow-model", channelID: "channel-c", failed: true, err: errors.New("boom")},
		{model: firstTestModel, channelID: "channel-a", latency: 100 * time.Millisecond},
	}

	rankLatencyBenchResults(results)

	if results[0].model != firstTestModel || results[1].model != secondTestModel || !results[2].failed {
		t.Fatalf("unexpected ranking: %#v", results)
	}
}

func TestFormatLatencyBenchResultsLinksChannels(t *testing.T) {
	t.Parallel()

	results := []latencyBenchResult{
		{model: firstTestModel, channelID: "channel-a", latency: 1500 * time.Millisecond},
		{model: secondTestModel, channelID: "channel-b", failed: true, err: errors.New("boom")},
	}

	formatted := formatLatencyBenchResults(results, "978995704666136646")

	for _, want := range []string{
		"1. `" + firstTestModel + "` — 1.5s",
		"https://discord.com/channels/978995704666136646/channel-a",
		"https://discord.com/channels/978995704666136646/channel-b",
		"failed: boom",
		latencyBenchQuery,
	} {
		if !strings.Contains(formatted, want) {
			t.Fatalf("formatted ranking %q missing %q", formatted, want)
		}
	}
}

func TestFormatLatencyBenchSingleResult(t *testing.T) {
	t.Parallel()

	result := latencyBenchResult{model: firstTestModel, latency: 1500 * time.Millisecond}
	formatted := formatLatencyBenchSingleResult(result)
	want := "Model latency for `" + latencyBenchQuery + "`: `" + firstTestModel + "` — 1.5s"

	if formatted != want {
		t.Fatalf("formatted single result = %q, want %q", formatted, want)
	}

	failed := latencyBenchResult{model: secondTestModel, failed: true, err: errors.New("boom")}
	formatted = formatLatencyBenchSingleResult(failed)

	if !strings.Contains(formatted, "`"+secondTestModel+"`") || !strings.Contains(formatted, "failed: boom") {
		t.Fatalf("unexpected formatted single failure: %q", formatted)
	}
}

func TestLatencyBenchRequestUsesWebSearchTool(t *testing.T) {
	t.Parallel()

	instance := newSearchTestBot(nil, nil)
	loadedConfig := newLatencyBenchTestConfig()

	request, err := instance.latencyBenchRequest(loadedConfig, latencyBenchResult{
		model:     firstTestModel,
		channelID: "channel-a",
	})
	if err != nil {
		t.Fatalf("build benchmark request: %v", err)
	}

	if request.ConfiguredModel != firstTestModel {
		t.Fatalf("configured model = %q, want %q", request.ConfiguredModel, firstTestModel)
	}

	if len(request.Tools) == 0 || request.Tools[0].Name != providers.WebSearchToolName {
		t.Fatalf("expected web_search tool, got %#v", request.Tools)
	}

	content, ok := request.Messages[len(request.Messages)-1].Content.(string)
	if !ok || !strings.Contains(content, latencyBenchQuery) {
		t.Fatalf("expected benchmark query in request, got %#v", request.Messages)
	}
}

func newLatencyBenchStubBot(
	t *testing.T,
	chatClient *stubChatCompletionClient,
	webSearch *stubWebSearchClient,
) *bot {
	t.Helper()

	instance := newSearchTestBot(chatClient, webSearch)

	return instance
}

func TestRunLatencyBenchmarkRanksFastestFirst(t *testing.T) {
	t.Parallel()

	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		// The second model answers through one tool round first, so it must
		// still rank slower than the first model's direct answer.
		if request.ConfiguredModel == secondTestModel && len(request.ToolRounds) == 0 {
			return handle(toolCallDelta(providers.FunctionToolCall{
				ID:        "call_1",
				Name:      providers.WebSearchToolName,
				Arguments: `{"objective": "Find phone news", "search_queries": ["latest smartphone news philippines"]}`,
			}))
		}

		if request.ConfiguredModel == firstTestModel {
			time.Sleep(5 * time.Millisecond)
		} else {
			time.Sleep(50 * time.Millisecond)
		}

		return handle(newStreamDelta("answer", finishReasonStop))
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{{Query: queries[0], Text: testWebSearchResultText}}, nil
	})

	instance := newLatencyBenchStubBot(t, chatClient, webSearch)

	results := instance.runLatencyBenchmark(t.Context(), newLatencyBenchTestConfig())
	if len(results) != 2 {
		t.Fatalf("result count = %d, want 2", len(results))
	}

	if results[0].model != firstTestModel || results[1].model != secondTestModel {
		t.Fatalf("unexpected ranking: %#v", results)
	}

	if results[0].failed || results[1].failed {
		t.Fatalf("unexpected failures: %#v", results)
	}

	if results[0].latency > results[1].latency {
		t.Fatalf("expected fastest first: %v then %v", results[0].latency, results[1].latency)
	}

	if len(webSearch.calls) != 1 {
		t.Fatalf("expected one tool-round search batch, got %d", len(webSearch.calls))
	}
}

func TestRunLatencyBenchmarkMarksStreamFailure(t *testing.T) {
	t.Parallel()

	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		if request.ConfiguredModel == secondTestModel {
			return errors.New("provider down")
		}

		return handle(newStreamDelta("answer", finishReasonStop))
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		return nil, nil
	})

	instance := newLatencyBenchStubBot(t, chatClient, webSearch)

	results := instance.runLatencyBenchmark(t.Context(), newLatencyBenchTestConfig())
	if len(results) != 2 {
		t.Fatalf("result count = %d, want 2", len(results))
	}

	if results[0].model != firstTestModel || results[0].failed {
		t.Fatalf("expected first model to rank first without failure: %#v", results)
	}

	if results[1].model != secondTestModel || !results[1].failed {
		t.Fatalf("expected second model to rank last as failed: %#v", results)
	}
}

func newLatencyBenchCommandInteraction() *discordgo.InteractionCreate {
	return newLatencyBenchCommandInteractionWithModel("")
}

func newLatencyBenchCommandInteractionWithModel(model string) *discordgo.InteractionCreate {
	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.AppID = "application-id"
	interaction.Token = "interaction-token"
	interaction.Type = discordgo.InteractionApplicationCommand
	interaction.GuildID = "978995704666136646"
	interaction.ChannelID = "invoking-channel"

	commandData := discordgo.ApplicationCommandInteractionData{Name: latencyBenchCommandName}

	if strings.TrimSpace(model) != "" {
		option := new(discordgo.ApplicationCommandInteractionDataOption)
		option.Name = latencyBenchModelOptionName
		option.Type = discordgo.ApplicationCommandOptionString
		option.Value = model
		commandData.Options = []*discordgo.ApplicationCommandInteractionDataOption{option}
	}

	result := new(discordgo.InteractionCreate)
	result.Interaction = interaction
	interaction.Data = commandData

	return result
}

func TestHandleLatencyBenchCommandRanksLockedModels(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfigWithExtra(t, `
channel_model_locks:
  channel-a: `+firstTestModel+`
  channel-b: `+secondTestModel+`
`)

	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		if request.ConfiguredModel == secondTestModel {
			time.Sleep(20 * time.Millisecond)
		}

		return handle(newStreamDelta("answer", finishReasonStop))
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{{Query: queries[0], Text: testWebSearchResultText}}, nil
	})

	var capture deferredInteractionCapture

	session := newInteractionTestSessionWithTransport(t, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		capture.requestCount++

		switch capture.requestCount {
		case 1:
			return captureDeferredInteractionRequest(t, request, &capture.deferredResponse)
		case 2, 3:
			return captureEditedInteractionRequest(t, request, &capture.editedResponse)
		default:
			t.Fatalf("unexpected interaction request count: %d", capture.requestCount)

			return nil, errUnexpectedTestRequest
		}
	}))

	instance := newModelTestBot(configPath)
	instance.chatCompletions = chatClient
	instance.webSearch = webSearch

	if err := instance.handleLatencyBenchCommand(session, newLatencyBenchCommandInteraction()); err != nil {
		t.Fatalf("handle latency command: %v", err)
	}

	assertDeferredInteractionResponse(t, &capture.deferredResponse)

	for _, want := range []string{
		"1. `" + firstTestModel + "`",
		"2. `" + secondTestModel + "`",
		"https://discord.com/channels/978995704666136646/channel-a",
		"https://discord.com/channels/978995704666136646/channel-b",
		latencyBenchQuery,
	} {
		if !strings.Contains(capture.editedResponse.Content, want) {
			t.Fatalf("ranking %q missing %q", capture.editedResponse.Content, want)
		}
	}
}

func TestHandleLatencyBenchCommandRejectsEmptyLocks(t *testing.T) {
	t.Parallel()

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)
	instance := newModelTestBot(writeModelConfig(t))

	if err := instance.handleLatencyBenchCommand(session, newLatencyBenchCommandInteraction()); err != nil {
		t.Fatalf("handle latency command: %v", err)
	}

	if capture.editedResponse.Content != "No `channel_model_locks` are configured." {
		t.Fatalf("unexpected response: %q", capture.editedResponse.Content)
	}
}

func TestHandleLatencyBenchCommandBenchmarksSingleModel(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfig(t)
	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		if request.ConfiguredModel != secondTestModel {
			t.Errorf("benchmarked model = %q, want %q", request.ConfiguredModel, secondTestModel)
		}

		return handle(newStreamDelta("answer", finishReasonStop))
	})
	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{{Query: queries[0], Text: testWebSearchResultText}}, nil
	})

	var capture deferredInteractionCapture

	session := newInteractionTestSessionWithTransport(t, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		capture.requestCount++

		switch capture.requestCount {
		case 1:
			return captureDeferredInteractionRequest(t, request, &capture.deferredResponse)
		case 2, 3:
			return captureEditedInteractionRequest(t, request, &capture.editedResponse)
		default:
			t.Fatalf("unexpected interaction request count: %d", capture.requestCount)

			return nil, errUnexpectedTestRequest
		}
	}))

	instance := newModelTestBot(configPath)
	instance.chatCompletions = chatClient
	instance.webSearch = webSearch

	if err := instance.handleLatencyBenchCommand(session, newLatencyBenchCommandInteractionWithModel(secondTestModel)); err != nil {
		t.Fatalf("handle latency command: %v", err)
	}

	assertDeferredInteractionResponse(t, &capture.deferredResponse)

	want := "Model latency for `" + latencyBenchQuery + "`: `" + secondTestModel + "`"
	if !strings.Contains(capture.editedResponse.Content, want) {
		t.Fatalf("single-model result %q missing %q", capture.editedResponse.Content, want)
	}
}

func TestHandleLatencyBenchCommandRejectsUnknownModel(t *testing.T) {
	t.Parallel()

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)
	instance := newModelTestBot(writeModelConfig(t))

	if err := instance.handleLatencyBenchCommand(session, newLatencyBenchCommandInteractionWithModel("openai/unknown-model")); err != nil {
		t.Fatalf("handle latency command: %v", err)
	}

	assertDeferredInteractionResponse(t, &capture.deferredResponse)

	if capture.editedResponse.Content != "Unknown model." {
		t.Fatalf("unexpected response: %q", capture.editedResponse.Content)
	}
}

func TestHandleLatencyBenchAutocompleteListsModels(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(writeModelConfig(t))

	interaction := newLatencyBenchCommandInteractionWithModel("")
	interaction.Type = discordgo.InteractionApplicationCommandAutocomplete
	interaction.Data = discordgo.ApplicationCommandInteractionData{
		Name: latencyBenchCommandName,
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{
				Name:    latencyBenchModelOptionName,
				Type:    discordgo.ApplicationCommandOptionString,
				Value:   "second",
				Focused: true,
			},
		},
	}

	if err := instance.handleLatencyBenchAutocomplete(session, interaction); err != nil {
		t.Fatalf("handle latency autocomplete: %v", err)
	}

	if response.Type != discordgo.InteractionApplicationCommandAutocompleteResult {
		t.Fatalf("unexpected autocomplete response type: %v", response.Type)
	}

	if response.Data == nil || len(response.Data.Choices) != 1 {
		t.Fatalf("unexpected autocomplete choices: %+v", response.Data)
	}

	if response.Data.Choices[0].Value != secondTestModel {
		t.Fatalf("unexpected autocomplete choice: %+v", response.Data.Choices[0])
	}
}

func TestApplicationCommandDispatchesLatencyBench(t *testing.T) {
	t.Parallel()

	configPath := writeModelConfigWithExtra(t, `
channel_model_locks:
  channel-a: `+firstTestModel+`
`)

	chatClient := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		return handle(newStreamDelta("answer", finishReasonStop))
	})

	webSearch := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{{Query: queries[0], Text: testWebSearchResultText}}, nil
	})

	var capture deferredInteractionCapture

	session := newInteractionTestSessionWithTransport(t, roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		t.Helper()

		capture.requestCount++

		switch capture.requestCount {
		case 1:
			return captureDeferredInteractionRequest(t, request, &capture.deferredResponse)
		case 2, 3:
			return captureEditedInteractionRequest(t, request, &capture.editedResponse)
		default:
			t.Fatalf("unexpected interaction request count: %d", capture.requestCount)

			return nil, errUnexpectedTestRequest
		}
	}))

	instance := newModelTestBot(configPath)
	instance.chatCompletions = chatClient
	instance.webSearch = webSearch

	if err := instance.handleApplicationCommandInteraction(session, newLatencyBenchCommandInteraction()); err != nil {
		t.Fatalf("dispatch latency: %v", err)
	}

	if !strings.Contains(capture.editedResponse.Content, "https://discord.com/channels/978995704666136646/channel-a") {
		t.Fatalf("ranking missing channel link: %q", capture.editedResponse.Content)
	}
}

func TestApplicationCommandDispatchesLatencyBenchAutocomplete(t *testing.T) {
	t.Parallel()

	var response discordgo.InteractionResponse

	session := newInteractionTestSession(t, &response)
	instance := newModelTestBot(writeModelConfig(t))

	interaction := newLatencyBenchCommandInteractionWithModel("")
	interaction.Type = discordgo.InteractionApplicationCommandAutocomplete
	interaction.Data = discordgo.ApplicationCommandInteractionData{
		Name: latencyBenchCommandName,
		Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{
				Name:    latencyBenchModelOptionName,
				Type:    discordgo.ApplicationCommandOptionString,
				Value:   "",
				Focused: true,
			},
		},
	}

	if err := instance.handleApplicationCommandInteraction(session, interaction); err != nil {
		t.Fatalf("dispatch latency autocomplete: %v", err)
	}

	if response.Type != discordgo.InteractionApplicationCommandAutocompleteResult {
		t.Fatalf("unexpected autocomplete response type: %v", response.Type)
	}

	if response.Data == nil || len(response.Data.Choices) != 2 {
		t.Fatalf("unexpected autocomplete choices: %+v", response.Data)
	}
}
