package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// ClaudeAPIName selects the native Claude Messages wire protocol in
// config.yaml (`api: claude-messages`). The provider name is free-form;
// pairing it with Claude model IDs (claude-opus-5, claude-sonnet-5, ...)
// routes through api.anthropic.com by default.
const ClaudeAPIName = "claude-messages"

// ProviderAPIKindClaude is the native Claude Messages family.
const ProviderAPIKindClaude ProviderAPIKind = "claude"

// Claude API defaults.
const (
	claudeDefaultBaseURL = "https://api.anthropic.com"
	claudeDefaultVersion = "2023-06-01"
	// claudeDefaultMaxTokens matches the skill's non-streaming default: a
	// thinking-capable reply needs headroom above the thinking budget, and
	// omitting max_tokens is a 400.
	claudeDefaultMaxTokens = int64(16000)
	// claudeStreamDefaultMaxTokens matches the skill's streaming default:
	// timeouts are not a concern on SSE, so give the model room.
	claudeStreamDefaultMaxTokens = int64(64000)
	// claudeMaxTokensCap is the largest max_tokens the current model family
	// accepts (1M-context models: 128K output).
	claudeMaxTokensCap            = int64(128000)
	claudeAnthropicVersionHeader  = "anthropic-version"
	claudeClientRequestIDHeader   = "X-Client-Request-Id"
	claudeHTTPTimeoutMilliseconds = 600000
)

// claudeMessagesStreamer issues one streaming Claude Messages request. The
// production value is the Anthropic SDK message service; tests swap in a
// stub to drive client logic without the network.
type claudeMessagesStreamer interface {
	NewStreaming(
		ctx context.Context,
		params anthropic.MessageNewParams,
		opts ...option.RequestOption,
	) *ssestream.Stream[anthropic.MessageStreamEventUnion]
}

type liveClaudeMessagesStreamer struct {
	client anthropic.Client
}

func (streamer liveClaudeMessagesStreamer) NewStreaming(
	ctx context.Context,
	params anthropic.MessageNewParams,
	opts ...option.RequestOption,
) *ssestream.Stream[anthropic.MessageStreamEventUnion] {
	return streamer.client.Messages.NewStreaming(ctx, params, opts...)
}

// claudeMessagesClientFactory builds a streamer for one request so key
// rotation and per-request headers do not mutate shared client state.
type claudeMessagesClientFactory func(apiKey string, provider ProviderRequestConfig) claudeMessagesStreamer

type claudeClient struct {
	httpClient  *http.Client
	newStreamer claudeMessagesClientFactory
}

func newClaudeClient(httpClient *http.Client) claudeClient {
	return claudeClient{
		httpClient: httpClient,
		newStreamer: func(apiKey string, provider ProviderRequestConfig) claudeMessagesStreamer {
			return liveClaudeMessagesClient(apiKey, provider, httpClient)
		},
	}
}

func liveClaudeMessagesClient(apiKey string, provider ProviderRequestConfig, httpClient *http.Client) claudeMessagesStreamer {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if httpClient != nil {
		opts = append(opts, option.WithHTTPClient(httpClient))
	}

	if baseURL := claudeProviderBaseURL(provider); baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}

	if version := claudeProviderVersion(provider); version != "" {
		opts = append(opts, option.WithHeader(claudeAnthropicVersionHeader, version))
	}

	for key, value := range provider.ExtraHeaders {
		if strings.EqualFold(strings.TrimSpace(key), claudeAnthropicVersionHeader) {
			continue
		}

		opts = append(opts, option.WithHeader(key, stringifyValue(value)))
	}

	return liveClaudeMessagesStreamer{client: anthropic.NewClient(opts...)}
}

// ClaudeProviderSelected reports whether a provider request uses the native
// Claude Messages API: explicit `api: claude-messages`, or a Claude model
// ID on the default Anthropic endpoint.
func ClaudeProviderSelected(provider ProviderRequestConfig, model string, providerName string) bool {
	if strings.EqualFold(strings.TrimSpace(provider.API), ClaudeAPIName) {
		return true
	}

	if provider.APIKind == ProviderAPIKindClaude {
		return true
	}

	if strings.TrimSpace(provider.API) != "" {
		return false
	}

	if baseURL := strings.TrimSpace(provider.BaseURL); baseURL != "" && !claudeBaseURLIsDefault(baseURL) {
		return false
	}

	if !IsClaudeModel(model) && !IsClaudeModel(providerName) && !claudeNameHintsClaude(providerName) {
		return false
	}

	return true
}

// IsClaudeModel reports whether a model ID belongs to the Claude family
// (skill shared/models.md aliases plus dated full IDs).
func IsClaudeModel(model string) bool {
	normalized := strings.ToLower(strings.TrimSpace(model))
	normalized = strings.TrimSuffix(normalized, ":vision")

	if _, name := splitClaudeConfiguredModel(normalized); name != "" {
		normalized = name
	}

	for _, prefix := range []string{
		"claude-opus-5", "claude-opus-4", "claude-sonnet-5", "claude-sonnet-4",
		"claude-haiku-4", "claude-haiku-3", "claude-fable-", "claude-mythos-",
		"claude-3-", "claude-2",
	} {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}

	return normalized == "claude-opus-5" || normalized == "claude-sonnet-5" || normalized == "claude-haiku-4-5"
}

func claudeNameHintsClaude(name string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(name)), "claude")
}

func splitClaudeConfiguredModel(configuredModel string) (string, string) {
	parts := strings.SplitN(strings.TrimSuffix(strings.TrimSpace(configuredModel), ":vision"), "/", 2)
	if len(parts) != 2 {
		return "", strings.TrimSpace(configuredModel)
	}

	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

// claudeBaseURLIsDefault reports whether baseURL points at the Anthropic API.
func claudeBaseURLIsDefault(baseURL string) bool {
	normalized := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))

	return normalized == "" ||
		normalized == "https://api.anthropic.com" ||
		strings.HasPrefix(normalized, "https://api.anthropic.com/v1")
}

func claudeProviderBaseURL(provider ProviderRequestConfig) string {
	trimmed := strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
	if trimmed == "" {
		return claudeDefaultBaseURL
	}

	// OpenAI-compatible proxies mount the Messages API under /v1 (e.g.
	// 9router serves POST /v1/messages). The SDK resolves the "v1/messages"
	// path against BaseURL: a BaseURL that already ends in /v1 would
	// produce /v1/v1/messages, so strip a trailing /v1 here.
	lowered := strings.ToLower(trimmed)
	if strings.HasSuffix(lowered, "/v1") {
		trimmed = strings.TrimRight(trimmed[:len(trimmed)-len("/v1")], "/")
		if trimmed == "" {
			return claudeDefaultBaseURL
		}
	}

	return trimmed
}

func claudeProviderVersion(provider ProviderRequestConfig) string {
	for key, value := range provider.ExtraHeaders {
		if strings.EqualFold(strings.TrimSpace(key), claudeAnthropicVersionHeader) {
			if version := strings.TrimSpace(stringifyValue(value)); version != "" {
				return version
			}
		}
	}

	return claudeDefaultVersion
}

// claudeRequestExtraBody returns the provider extra body plus query options
// that map onto Messages fields (max_tokens), with those keys removed.
func claudeRequestExtraBody(extraBody map[string]any) (map[string]any, int64, bool, error) {
	maxTokens := claudeDefaultMaxTokens
	hasMaxTokens := false

	if len(extraBody) == 0 {
		return map[string]any{}, maxTokens, hasMaxTokens, nil
	}

	cloned := make(map[string]any, len(extraBody))

	for key, value := range extraBody {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "max_tokens", "maxtokens", "max-tokens":
			parsed, ok := claudeExtraBodyInt(value)
			if !ok || parsed <= 0 {
				return nil, 0, false, fmt.Errorf("claude extra_body max_tokens must be a positive integer: %w", os.ErrInvalid)
			}

			maxTokens = min(parsed, claudeMaxTokensCap)
			hasMaxTokens = true
		default:
			cloned[key] = value
		}
	}

	return cloned, maxTokens, hasMaxTokens, nil
}

func claudeExtraBodyInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > uint64(claudeMaxTokensCap*8) {
			return 0, false
		}

		return int64(typed), true
	case float32:
		return int64(typed), float64(int64(typed)) == float64(typed)
	case float64:
		return int64(typed), float64(int64(typed)) == typed
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}

		return parsed, true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0, false
		}

		return parsed, true
	default:
		return 0, false
	}
}

// claudeOptString wraps a string in an SDK optional: empty strings stay
// omitted so they never render as explicit nulls in the request JSON.
func claudeOptString(value string) param.Opt[string] {
	if value == "" {
		return param.Opt[string]{}
	}

	return anthropic.String(value)
}

func (client claudeClient) streamChatCompletion(
	ctx context.Context,
	request ChatCompletionRequest,
	handle func(StreamDelta) error,
) error {
	apiKey := firstAPIKey(request.Provider.APIKeys)
	if apiKey == "" {
		apiKey = request.Provider.APIKey
	}

	streamer := client.newStreamer(apiKey, request.Provider)

	params, err := claudeMessageParams(request)
	if err != nil {
		return err
	}

	opts := claudeRequestOptions(request)

	contentSent := false

	wrappedHandle := func(delta StreamDelta) error {
		if deltaReferencesContent(delta) {
			contentSent = true
		}

		return handle(delta)
	}

	stream := streamer.NewStreaming(ctx, params, opts...)

	finished, streamErr := claudeConsumeStream(stream, wrappedHandle)
	if streamErr != nil {
		if contentSent || !IsPayloadTooLargeError(streamErr) {
			return streamErr
		}

		return client.streamChatCompletionWithCompressedImages(ctx, request, streamErr, handle)
	}

	if !finished {
		return fmt.Errorf("claude stream ended before message_stop: %w", io.ErrUnexpectedEOF)
	}

	if !contentSent {
		return ErrEmptyModelResponse
	}

	return nil
}

// streamChatCompletionWithCompressedImages retries a 413 rejection with
// every embedded image recompressed near-losslessly. The original failure
// returns unchanged when nothing shrank.
func (client claudeClient) streamChatCompletionWithCompressedImages(
	ctx context.Context,
	request ChatCompletionRequest,
	statusErr error,
	handle func(StreamDelta) error,
) error {
	compressedRequest, summary, ok := compressedRequestWithSmallerImages(request)
	if !ok {
		return statusErr
	}

	logWarn(
		"retrying claude request with compressed images after payload too large",
		statusErr,
		"images",
		summary.images,
		"original_bytes",
		summary.originalBytes,
		"compressed_bytes",
		summary.compressedBytes,
	)

	return client.streamChatCompletion(ctx, compressedRequest, handle)
}

// claudeRequestOptions maps provider query/header overrides onto SDK
// request options. ExtraBody keys are applied by the caller through
// WithJSONSet so unknown Messages fields still pass through.
func claudeRequestOptions(request ChatCompletionRequest) []option.RequestOption {
	var opts []option.RequestOption

	for key, value := range request.Provider.ExtraQuery {
		opts = append(opts, option.WithQuery(key, stringifyValue(value)))
	}

	for key, value := range request.Provider.ExtraHeaders {
		if strings.EqualFold(strings.TrimSpace(key), claudeAnthropicVersionHeader) {
			continue
		}

		opts = append(opts, option.WithHeader(key, stringifyValue(value)))
	}

	for key, value := range request.Provider.ExtraBody {
		normalizedKey := strings.ToLower(strings.TrimSpace(key))
		if claudeExtraBodyMappedKey(normalizedKey) {
			continue
		}

		opts = append(opts, option.WithJSONSet(key, value))
	}

	if strings.TrimSpace(request.RequestID) != "" {
		opts = append(opts, option.WithHeader(claudeClientRequestIDHeader, strings.TrimSpace(request.RequestID)))
	}

	opts = append(opts, option.WithRequestTimeout(claudeHTTPTimeoutMilliseconds*1000000))

	return opts
}

func claudeExtraBodyMappedKey(normalizedKey string) bool {
	switch normalizedKey {
	case "max_tokens", "maxtokens", "max-tokens",
		"model", "messages", "system", "tools", "tool_choice",
		"stream", "thinking", "effort", "temperature", "top_p", "top_k":
		return true
	default:
		return false
	}
}
