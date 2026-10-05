package app

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	providers "llmcord-go/internal/providers"

	"github.com/bwmarrin/discordgo"
)

// latencyBenchQuery is the fixed end-to-end prompt every benchmarked model
// must answer, so rankings compare the same workload.
const latencyBenchQuery = "latest smartphone news in the philippines"

// latencyBenchCommandName is the slash command that benchmarks models end to
// end. Without an option it benchmarks every channel_model_locks model; with
// the model option it benchmarks one configured model alone.
const latencyBenchCommandName = "latency"

// latencyBenchCommandDescription describes the latency benchmark command.
const latencyBenchCommandDescription = "Benchmark channel_model_locks models or one model end to end"

// latencyBenchModelOptionName is the optional single-model override for the
// latency benchmark command.
const latencyBenchModelOptionName = "model"

// latencyBenchModelOptionDescription describes the single-model override.
const latencyBenchModelOptionDescription = "Single model to benchmark (defaults to every channel_model_locks model)"

const (
	// latencyBenchConcurrency bounds how many benchmarked models run at
	// once, mirroring the external request pool instead of hammering every
	// provider in parallel.
	latencyBenchConcurrency = externalRequestConcurrency
	// latencyBenchRequestTimeout bounds one model's whole benchmark turn
	// (stream plus its single web_search tool round), so one hung model
	// cannot stall the ranking past Discord's 15-minute follow-up window.
	// The normal reply pipeline has no timeout; only this benchmark does.
	latencyBenchRequestTimeout = 5 * time.Minute
	// latencyBenchFailureReasonMaxRunes caps one model's failure detail so
	// the ranked table stays far below the interaction response limit.
	latencyBenchFailureReasonMaxRunes = 80
	// latencyBenchResponseMaxLength caps the whole ranking message, like
	// the paginated source/thinking views.
	latencyBenchResponseMaxLength = 1900
)

// latencyBenchResult is one model's measured end-to-end turn.
type latencyBenchResult struct {
	model     string
	channelID string
	latency   time.Duration
	failed    bool
	err       error
}

// benchmarkStreamOutcome is one streamed benchmark round without any
// Discord rendering: the visible answer plus tool-call/finish signals.
type benchmarkStreamOutcome struct {
	answer           string
	toolCallResponse *providers.ToolCallResponse
	finishReason     string
}

func newLatencyBenchCommand() *discordgo.ApplicationCommand {
	command := new(discordgo.ApplicationCommand)
	command.Name = latencyBenchCommandName
	command.Description = latencyBenchCommandDescription
	command.Type = discordgo.ChatApplicationCommand

	option := new(discordgo.ApplicationCommandOption)
	option.Name = latencyBenchModelOptionName
	option.Description = latencyBenchModelOptionDescription
	option.Type = discordgo.ApplicationCommandOptionString
	option.Required = false
	option.Autocomplete = true

	command.Options = []*discordgo.ApplicationCommandOption{option}

	return command
}

// latencyBenchChannelLink renders the Discord client URL for a benchmarked
// channel, falling back to @me when the guild is unknown (DM invocations).
func latencyBenchChannelLink(guildID, channelID string) string {
	if strings.TrimSpace(guildID) == "" {
		guildID = "@me"
	}

	return fmt.Sprintf("https://discord.com/channels/%s/%s", guildID, channelID)
}

// latencyBenchTargets returns the locked (channel, model) pairs in channel
// ID order, so concurrent workers still rank deterministically.
func latencyBenchTargets(channelModelLocks map[string]string) []latencyBenchResult {
	channelIDs := make([]string, 0, len(channelModelLocks))

	for channelID, model := range channelModelLocks {
		if strings.TrimSpace(channelID) == "" || strings.TrimSpace(model) == "" {
			continue
		}

		channelIDs = append(channelIDs, channelID)
	}

	slices.Sort(channelIDs)

	targets := make([]latencyBenchResult, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		targets = append(targets, latencyBenchResult{model: channelModelLocks[channelID], channelID: channelID})
	}

	return targets
}

// rankLatencyBenchResults orders benchmark turns fastest to slowest with
// failures last, breaking ties by model name for stable output.
func rankLatencyBenchResults(results []latencyBenchResult) {
	slices.SortStableFunc(results, func(left, right latencyBenchResult) int {
		if left.failed != right.failed {
			if left.failed {
				return 1
			}

			return -1
		}

		if !left.failed && left.latency != right.latency {
			return cmp.Compare(left.latency, right.latency)
		}

		return strings.Compare(left.model, right.model)
	})
}

// formatLatencyBenchResults renders the ranked benchmark table with one
// channel link per locked model. Failure reasons are dropped when the full
// table would exceed the interaction response limit.
func formatLatencyBenchResults(results []latencyBenchResult, guildID string) string {
	formatted := formatLatencyBenchTable(results, guildID, true)
	if runeCount(formatted) <= latencyBenchResponseMaxLength {
		return formatted
	}

	return formatLatencyBenchTable(results, guildID, false)
}

// formatLatencyBenchSingleResult renders one model's benchmark turn without
// a channel link: single-model runs are not tied to a locked channel.
func formatLatencyBenchSingleResult(result latencyBenchResult) string {
	return fmt.Sprintf(
		"Model latency for `%s`: `%s` — %s",
		latencyBenchQuery,
		result.model,
		formatLatencyBenchLatency(result, true),
	)
}

func formatLatencyBenchTable(results []latencyBenchResult, guildID string, withReasons bool) string {
	lines := make([]string, 0, len(results)+1)
	lines = append(lines, "Model latency (fastest to slowest) for `"+latencyBenchQuery+"`:")

	for rank, result := range results {
		lines = append(lines, formatLatencyBenchLine(rank+1, result, guildID, withReasons))
	}

	return strings.Join(lines, "\n")
}

func formatLatencyBenchLine(rank int, result latencyBenchResult, guildID string, withReason bool) string {
	link := latencyBenchChannelLink(guildID, result.channelID)

	return fmt.Sprintf(
		"%d. `%s` — %s ([channel](%s))",
		rank,
		result.model,
		formatLatencyBenchLatency(result, withReason),
		link,
	)
}

// formatLatencyBenchLatency renders one benchmark turn's measured latency,
// or its failure reason when the model produced no answer.
func formatLatencyBenchLatency(result latencyBenchResult, withReason bool) string {
	if !result.failed {
		return result.latency.Round(time.Millisecond).String()
	}

	if !withReason {
		return "failed"
	}

	reason := strings.TrimSpace(userFacingResponseError(result.err))
	if reason == "" {
		reason = "failed"
	}

	return "failed: " + truncateRunes(reason, latencyBenchFailureReasonMaxRunes)
}

// orderedChannelModelLockModels lists every locked model once, in channel
// ID order, for the benchmark's progress update.
func orderedChannelModelLockModels(channelModelLocks map[string]string) []string {
	models := make([]string, 0, len(channelModelLocks))

	for _, target := range latencyBenchTargets(channelModelLocks) {
		if !slices.Contains(models, target.model) {
			models = append(models, target.model)
		}
	}

	return models
}

// latencyBenchRequest builds one model's benchmark request: the fixed query
// through the normal per-model request builder (provider routing, system
// prompt, auto-append suffixes, web_search tool).
func (instance *bot) latencyBenchRequest(
	loadedConfig config,
	target latencyBenchResult,
) (chatCompletionRequest, error) {
	provider, err := configuredModelProvider(loadedConfig, target.model)
	if err != nil {
		return chatCompletionRequest{}, fmt.Errorf("resolve provider for %q: %w", target.model, err)
	}

	messages := []chatMessage{{Role: messageRoleUser, Content: latencyBenchQuery}}

	if autoAppendEnabled(provider) {
		messages, err = applyAutoAppend(provider, loadedConfig.AutoAppendPhrases, messages)
		if err != nil {
			return chatCompletionRequest{}, fmt.Errorf("append auto suffix for %q: %w", target.model, err)
		}
	}

	requestMessages := messages
	if !provider.DontSendSystemPrompt {
		requestMessages = prependSystemPrompt(messages, loadedConfig.SystemPrompt, time.Now())
	}

	request, err := instance.buildPreparedChatCompletionRequest(
		loadedConfig,
		provider,
		target.model,
		requestMessages,
	)
	if err != nil {
		return chatCompletionRequest{}, fmt.Errorf("build benchmark request for %q: %w", target.model, err)
	}

	request.RequestID = "latency-bench"

	return request, nil
}

// streamBenchmarkOnce streams one benchmark round with no Discord rendering,
// accumulating the visible answer and the tool-call/finish signals.
func (instance *bot) streamBenchmarkOnce(
	ctx context.Context,
	request chatCompletionRequest,
) (benchmarkStreamOutcome, error) {
	var outcome benchmarkStreamOutcome

	var answer strings.Builder

	err := instance.chatCompletions.StreamChatCompletion(ctx, request, func(delta streamDelta) error {
		answer.WriteString(delta.Content)

		if delta.ToolCallResponse != nil {
			outcome.toolCallResponse = delta.ToolCallResponse
		}

		if delta.FinishReason != "" {
			outcome.finishReason = delta.FinishReason
		}

		return nil
	})
	if err != nil {
		return outcome, err
	}

	outcome.answer = answer.String()

	return outcome, nil
}

// streamBenchmarkRound streams one benchmark round with the reply pipeline's
// same-model retry: a clean close without a finish reason or a transient
// failure is attempted again, so a truncated stream does not fail the model.
func (instance *bot) streamBenchmarkRound(
	ctx context.Context,
	request chatCompletionRequest,
) (benchmarkStreamOutcome, error) {
	outcome, err := instance.streamBenchmarkOnce(ctx, request)
	if err == nil && outcome.toolCallResponse == nil && strings.TrimSpace(outcome.answer) == "" {
		return outcome, errEmptyModelResponse
	}

	for attempt := 2; attempt <= prematureStreamRetryMaxAttempts; attempt++ {
		if !shouldRetryGenerationRound(outcome.finishReason, err) {
			break
		}

		logWarn(
			"stream ended without finish reason; retrying benchmark",
			err,
			"configured_model",
			request.ConfiguredModel,
			"attempt",
			attempt,
			"max_attempts",
			prematureStreamRetryMaxAttempts,
		)

		sleepErr := sleepPrematureStreamRetry(ctx, prematureStreamRetryFixedDelay)
		if sleepErr != nil {
			return outcome, sleepErr
		}

		outcome, err = instance.streamBenchmarkOnce(ctx, request)

		if err == nil && outcome.toolCallResponse == nil && strings.TrimSpace(outcome.answer) == "" {
			return outcome, errEmptyModelResponse
		}
	}

	return outcome, err
}

// benchmarkToolRoundAnswer runs the single web_search tool round of a
// benchmark turn (all calls executed as one search batch) and streams the
// forced final answer, mirroring the reply pipeline's tool_choice "none"
// then tool-free fallback.
func (instance *bot) benchmarkToolRoundAnswer(
	ctx context.Context,
	loadedConfig config,
	request chatCompletionRequest,
	outcome benchmarkStreamOutcome,
) (string, error) {
	outputs, _, _ := instance.runWebSearchToolPhase(
		ctx,
		loadedConfig,
		request.ConfiguredModel,
		nil,
		request.Messages,
		nil,
		outcome.toolCallResponse.Calls,
	)

	request.ToolRounds = append(
		slices.Clone(request.ToolRounds),
		finalWebSearchToolRound(outcome.toolCallResponse, outputs),
	)

	if instance.ignoresToolChoiceNone(request.ConfiguredModel) {
		return instance.streamBenchmarkToolFreeAnswer(ctx, request)
	}

	request.ToolChoice = providers.ToolChoiceNone

	final, err := instance.streamBenchmarkRound(ctx, request)
	if err != nil {
		return "", err
	}

	if final.toolCallResponse == nil {
		return final.answer, nil
	}

	logWarn(
		"model emitted tool calls although tool_choice was none; answering benchmark without tools",
		nil,
		"configured_model",
		request.ConfiguredModel,
		"tool_calls",
		len(final.toolCallResponse.Calls),
	)

	instance.rememberToolChoiceNoneIgnored(request.ConfiguredModel)

	return instance.streamBenchmarkToolFreeAnswer(ctx, request)
}

// streamBenchmarkToolFreeAnswer streams the final benchmark answer with no
// tools offered, carrying the executed round's outputs as conversation text.
// Like the reply pipeline's runToolFreeFinalAnswerRound, a backend that
// adds tools of its own (9router's decoy tools on OpenCode requests) gets
// retried with unavailable-tool feedback, up to
// unofferedToolCallMaxAttempts attempts.
func (instance *bot) streamBenchmarkToolFreeAnswer(
	ctx context.Context,
	request chatCompletionRequest,
) (string, error) {
	toolFreeRequest, err := toolFreeFinalAnswerRequest(request)
	if err != nil {
		return "", err
	}

	for attempt := 1; ; attempt++ {
		final, err := instance.streamBenchmarkRound(ctx, toolFreeRequest)
		if err != nil {
			return "", err
		}

		if final.toolCallResponse == nil {
			return final.answer, nil
		}

		if attempt >= unofferedToolCallMaxAttempts {
			logWarn(
				"benchmark kept calling tools that were not offered; giving up",
				nil,
				"configured_model",
				toolFreeRequest.ConfiguredModel,
				"tool_calls",
				toolCallNames(final.toolCallResponse),
				"max_attempts",
				unofferedToolCallMaxAttempts,
			)

			return "", errEmptyModelResponse
		}

		logWarn(
			"benchmark called tools that were not offered; retrying without them",
			nil,
			"configured_model",
			toolFreeRequest.ConfiguredModel,
			"tool_calls",
			toolCallNames(final.toolCallResponse),
			"attempt",
			attempt+1,
			"max_attempts",
			unofferedToolCallMaxAttempts,
		)

		feedbackMessages, feedbackErr := appendUnofferedToolFeedback(
			toolFreeRequest.Messages,
			toolCallNames(final.toolCallResponse),
		)
		if feedbackErr != nil {
			logWarn(
				"append unavailable tool feedback to benchmark; retrying without it",
				feedbackErr,
				"configured_model",
				toolFreeRequest.ConfiguredModel,
			)
		} else {
			toolFreeRequest.Messages = feedbackMessages
		}

		if sleepErr := sleepPrematureStreamRetry(ctx, prematureStreamRetryFixedDelay); sleepErr != nil {
			return "", sleepErr
		}
	}
}

// benchmarkLockedModel runs one model's benchmark turn end to end (stream
// plus its single web_search tool round) and measures wall-clock latency
// from start to the finished answer.
func (instance *bot) benchmarkLockedModel(
	ctx context.Context,
	loadedConfig config,
	target latencyBenchResult,
	request chatCompletionRequest,
) latencyBenchResult {
	benchCtx, cancel := context.WithTimeout(ctx, latencyBenchRequestTimeout)
	defer cancel()

	start := time.Now()

	outcome, err := instance.streamBenchmarkRound(benchCtx, request)
	if err != nil {
		target.failed = true
		target.err = err
		target.latency = time.Since(start)

		return target
	}

	if outcome.toolCallResponse != nil {
		finalAnswer, err := instance.benchmarkToolRoundAnswer(benchCtx, loadedConfig, request, outcome)
		target.latency = time.Since(start)

		if err != nil {
			target.failed = true
			target.err = err

			return target
		}

		if strings.TrimSpace(finalAnswer) == "" {
			target.failed = true
			target.err = errEmptyModelResponse

			return target
		}

		return target
	}

	target.latency = time.Since(start)

	return target
}

type latencyBenchJob struct {
	target   latencyBenchResult
	request  chatCompletionRequest
	ok       bool
	buildErr error
}

// runLatencyBenchmark benchmarks every locked model concurrently and
// returns the turns ranked fastest to slowest. A model whose request
// cannot even be built is ranked as failed without blocking the rest.
func (instance *bot) runLatencyBenchmark(
	ctx context.Context,
	loadedConfig config,
) []latencyBenchResult {
	return instance.runLatencyBenchmarkTargets(ctx, loadedConfig, latencyBenchTargets(loadedConfig.ChannelModelLocks))
}

// runLatencyBenchmarkTargets benchmarks the given targets concurrently and
// returns the turns ranked fastest to slowest. A model whose request
// cannot even be built is ranked as failed without blocking the rest.
func (instance *bot) runLatencyBenchmarkTargets(
	ctx context.Context,
	loadedConfig config,
	targets []latencyBenchResult,
) []latencyBenchResult {
	jobs := make([]latencyBenchJob, len(targets))
	for index, target := range targets {
		request, err := instance.latencyBenchRequest(loadedConfig, target)
		if err != nil {
			jobs[index] = latencyBenchJob{target: target, buildErr: err}

			continue
		}

		jobs[index] = latencyBenchJob{target: target, request: request, ok: true}
	}

	results := runTasksConcurrently(
		ctx,
		latencyBenchConcurrency,
		len(jobs),
		func(taskCtx context.Context, index int) (latencyBenchResult, error) {
			if !jobs[index].ok {
				failed := jobs[index].target
				failed.failed = true
				failed.err = jobs[index].buildErr

				return failed, nil
			}

			return instance.benchmarkLockedModel(taskCtx, loadedConfig, jobs[index].target, jobs[index].request), nil
		},
	)

	ranked := make([]latencyBenchResult, 0, len(results))

	for index, result := range results {
		if result.err != nil {
			failed := targets[index]
			failed.failed = true
			failed.err = result.err
			ranked = append(ranked, failed)

			continue
		}

		ranked = append(ranked, result.value)
	}

	rankLatencyBenchResults(ranked)

	return ranked
}

func (instance *bot) handleLatencyBenchCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	// Defer first: every benchmark streams a full reply, so the result
	// takes far longer than Discord's 3-second initial-response window.
	if err := respondInteractionDeferredWithFlags(
		session,
		interaction.Interaction,
		0,
	); err != nil {
		return fmt.Errorf("defer latency command interaction response: %w", err)
	}

	loadedConfig, err := instance.loadConfigCached()
	if err != nil {
		logWarn("load config for latency command", err)

		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"Failed to load configuration.",
		)
	}

	requestedModel := strings.TrimSpace(interactionOptionString(interaction.ApplicationCommandData().Options))
	if requestedModel != "" {
		return instance.handleLatencyBenchSingleModel(session, interaction, loadedConfig, requestedModel)
	}

	if len(loadedConfig.ChannelModelLocks) == 0 {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"No `channel_model_locks` are configured.",
		)
	}

	targets := latencyBenchTargets(loadedConfig.ChannelModelLocks)
	models := orderedChannelModelLockModels(loadedConfig.ChannelModelLocks)

	progressText := fmt.Sprintf(
		"Benchmarking %d locked channels (`%s`) for `%s` …",
		len(targets),
		strings.Join(models, "`, `"),
		latencyBenchQuery,
	)
	if err := editInteractionResponseText(session, interaction.Interaction, progressText); err != nil {
		logWarn("update latency command progress", err)
	}

	results := instance.runLatencyBenchmark(context.Background(), loadedConfig)

	guildID := ""
	if interaction != nil && interaction.Interaction != nil {
		guildID = interaction.GuildID
	}

	slog.Info("latency benchmark finished", "models", len(results))

	return editInteractionResponseText(
		session,
		interaction.Interaction,
		formatLatencyBenchResults(results, guildID),
	)
}

func (instance *bot) handleLatencyBenchAutocomplete(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	loadedConfig, err := instance.loadConfigCached()
	if err != nil {
		return fmt.Errorf("load config for autocomplete: %w", err)
	}

	return handleConfiguredModelAutocomplete(
		session,
		interaction,
		instance.currentModelForConfig(loadedConfig),
		loadedConfig,
	)
}

// handleLatencyBenchSingleModel benchmarks one configured model alone,
// without requiring channel_model_locks.
func (instance *bot) handleLatencyBenchSingleModel(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
	loadedConfig config,
	requestedModel string,
) error {
	if !loadedConfig.hasModel(requestedModel) {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"Unknown model.",
		)
	}

	progressText := fmt.Sprintf("Benchmarking `%s` for `%s` …", requestedModel, latencyBenchQuery)
	if err := editInteractionResponseText(session, interaction.Interaction, progressText); err != nil {
		logWarn("update latency command progress", err)
	}

	results := instance.runLatencyBenchmarkTargets(
		context.Background(),
		loadedConfig,
		[]latencyBenchResult{{model: requestedModel}},
	)
	if len(results) != 1 {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"Failed to benchmark model.",
		)
	}

	slog.Info("latency benchmark finished", "model", requestedModel)

	return editInteractionResponseText(
		session,
		interaction.Interaction,
		formatLatencyBenchSingleResult(results[0]),
	)
}
