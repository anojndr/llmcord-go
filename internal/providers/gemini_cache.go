package providers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/genai"
)

const (
	// geminiCacheOptionKey is the extra_body knob that controls explicit
	// context caching for Gemini providers ("auto" default, "off" disables).
	geminiCacheOptionKey = "context_caching"
	// geminiCacheMinTokenFloor is the minimum cached-prefix size that makes
	// explicit caching worthwhile regardless of model.
	geminiCacheMinTokenFloor = 512
	// geminiCacheDefaultTTL matches the API default when no TTL is set.
	geminiCacheDefaultTTL = time.Hour
	// geminiCacheOptionOff disables explicit caching via extra_body.
	geminiCacheOptionOff = "off"
	// geminiCacheModelMinTokensCurrent is the documented 4096-token minimum
	// for Gemini 3.5 Flash, 3.1 Pro, and 3 Pro models.
	geminiCacheModelMinTokensCurrent = 4096
	// geminiCacheModelMinTokensLegacy is the documented 2048-token minimum
	// for Gemini 2.5 Flash and 2.5 Pro models.
	geminiCacheModelMinTokensLegacy = 2048
	// GeminiCacheDefaultDisplayName is used when the request carries no
	// model or request id.
	GeminiCacheDefaultDisplayName = "llmcord-go"
)

// geminiCacheClient is a geminiAPIClient that also exposes the Caches
// service. The streaming path falls back to implicit caching (a strict
// no-op) when a backend does not support the cache service.
type geminiCacheClient interface {
	geminiAPIClient
	CreateCachedContent(
		ctx context.Context,
		model string,
		config *genai.CreateCachedContentConfig,
	) (*genai.CachedContent, error)
}

func buildGeminiGenerateContentRequestWithCaching(
	ctx context.Context,
	request ChatCompletionRequest,
	apiClient geminiAPIClient,
	files geminiFilesClient,
) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	contents, config, err := BuildGeminiGenerateContentRequest(ctx, request, files)
	if err != nil {
		return nil, nil, err
	}

	cacheClient, supportsCaching := apiClient.(geminiCacheClient)
	if !supportsCaching {
		return contents, config, nil
	}

	cachedName, err := geminiEnsureCachedContent(ctx, request, contents, cacheClient)
	if err != nil {
		return nil, nil, err
	}

	if cachedName != "" {
		config.CachedContent = cachedName
	}

	return contents, config, nil
}

// geminiEnsureCachedContent creates (or re-creates) a cachedContents resource
// for the stable reply-chain prefix of the request, returning its name. It
// returns an empty name when the prefix is too small or caching is disabled.
//
// The cache is deliberately re-created on every request so it always begins
// at the same fixed prefix: Gemini treats cached content as a strict prefix
// of the prompt, and a cache created earlier in the conversation would be
// invalidated the moment the prefix grew past it.
func geminiEnsureCachedContent(
	ctx context.Context,
	request ChatCompletionRequest,
	contents []*genai.Content,
	client geminiCacheClient,
) (string, error) {
	if !geminiCacheEnabled(request.Provider.ExtraBody) {
		return "", nil
	}

	prefix := geminiCachePrefixContents(contents)
	if len(prefix) == 0 {
		return "", nil
	}

	prefixTokens := geminiCachePrefixTokenEstimate(prefix)
	minTokens := geminiCacheMinTokenFloor

	if threshold := geminiCacheMinTokensForModel(request.Model); threshold > minTokens {
		minTokens = threshold
	}

	if prefixTokens < minTokens {
		return "", nil
	}

	displayName := geminiCacheDisplayName(request)
	ttl := geminiCacheTTL(request.Provider.ExtraBody)

	createConfig := new(genai.CreateCachedContentConfig)
	createConfig.Contents = prefix
	createConfig.DisplayName = displayName
	createConfig.TTL = ttl

	cachedContent, err := client.CreateCachedContent(ctx, request.Model, createConfig)
	if err != nil {
		return "", fmt.Errorf("create gemini cached content: %w", err)
	}

	if cachedContent == nil || strings.TrimSpace(cachedContent.Name) == "" {
		return "", fmt.Errorf("create gemini cached content: missing resource name: %w", os.ErrInvalid)
	}

	return strings.TrimSpace(cachedContent.Name), nil
}

func geminiCacheEnabled(extraBody map[string]any) bool {
	rawOption, ok := extraBody[geminiCacheOptionKey]
	if !ok || rawOption == nil {
		return true
	}

	option := strings.ToLower(strings.TrimSpace(stringifyValue(rawOption)))

	return option != geminiCacheOptionOff
}

// geminiCachePrefixContents returns the leading contents that are stable
// across requests: every turn up to (but not including) the latest user
// message. Only complete turns are cached so the dynamic tail never
// invalidates the prefix.
func geminiCachePrefixContents(contents []*genai.Content) []*genai.Content {
	lastUserIndex := -1

	for index, content := range contents {
		if content != nil && content.Role == string(genai.RoleUser) {
			lastUserIndex = index
		}
	}

	if lastUserIndex <= 0 {
		return nil
	}

	prefix := make([]*genai.Content, lastUserIndex)
	copy(prefix, contents[:lastUserIndex])

	return prefix
}

// geminiCachePrefixTokenEstimate estimates tokens for the cached prefix using
// a bytes-per-token approximation (roughly four bytes per token).
func geminiCachePrefixTokenEstimate(contents []*genai.Content) int {
	totalTokens := 0

	for _, content := range contents {
		if content == nil {
			continue
		}

		for _, part := range content.Parts {
			if part == nil {
				continue
			}

			switch {
			case strings.TrimSpace(part.Text) != "":
				totalTokens += geminiCacheTextTokenEstimate(part.Text)
			case part.FileData != nil:
				totalTokens += geminiCacheTextTokenEstimate("file-attachment-placeholder")
			case part.InlineData != nil:
				totalTokens += geminiCacheTextTokenEstimate("inline-data-placeholder")
			}
		}
	}

	return totalTokens
}

// geminiCacheTextTokenEstimate approximates a text part's token count as
// ceil(bytes/4), floored at one token for non-empty text.
func geminiCacheTextTokenEstimate(text string) int {
	trimmedText := strings.TrimSpace(text)
	if trimmedText == "" {
		return 0
	}

	return max((len(trimmedText)+geminiCacheTextTokenBytes-1)/geminiCacheTextTokenBytes, 1)
}

// geminiCacheTextTokenBytes is the assumed bytes-per-token ratio used by
// geminiCacheTextTokenEstimate.
const geminiCacheTextTokenBytes = 4

// geminiCacheMinTokensForModel returns the documented minimum input token
// count for explicit context caching on the given model, or zero when the
// model is not in the table. The effective floor is
// geminiCacheMinTokenFloor so tiny prefixes never pay cache-write cost.
func geminiCacheMinTokensForModel(model string) int {
	lowerModel := strings.ToLower(strings.TrimSpace(model))

	switch {
	case strings.Contains(lowerModel, "3.5-flash"),
		strings.Contains(lowerModel, "3.1-pro"),
		strings.Contains(lowerModel, "3-pro"):
		return geminiCacheModelMinTokensCurrent
	case strings.Contains(lowerModel, "2.5-flash"),
		strings.Contains(lowerModel, "2.5-pro"):
		return geminiCacheModelMinTokensLegacy
	default:
		return 0
	}
}

// geminiCacheTTL resolves the cache TTL from extra_body
// (context_caching.ttl) and falls back to the API default of one hour.
func geminiCacheTTL(extraBody map[string]any) time.Duration {
	rawOptions, isOptionsMap := extraBody[geminiCacheOptionKey].(map[string]any)
	if !isOptionsMap {
		return geminiCacheDefaultTTL
	}

	rawTTL, hasTTL := rawOptions["ttl"]
	if !hasTTL || rawTTL == nil {
		return geminiCacheDefaultTTL
	}

	ttl, err := time.ParseDuration(strings.TrimSpace(stringifyValue(rawTTL)))
	if err != nil || ttl <= 0 {
		return geminiCacheDefaultTTL
	}

	return ttl
}

// geminiCacheDisplayName builds a human-readable display name for the cache
// so it is easy to identify in the Gemini API console.
func geminiCacheDisplayName(request ChatCompletionRequest) string {
	const cacheDisplayNameCapacity = 3

	parts := make([]string, 0, cacheDisplayNameCapacity)

	configuredModel := strings.TrimSpace(request.ConfiguredModel)
	if configuredModel == "" {
		configuredModel = strings.TrimSpace(request.Model)
	}

	if configuredModel != "" {
		parts = append(parts, configuredModel)
	}

	if requestID := strings.TrimSpace(request.RequestID); requestID != "" {
		parts = append(parts, requestID)
	}

	if len(parts) == 0 {
		return GeminiCacheDefaultDisplayName
	}

	return GeminiCacheDefaultDisplayName + " " + strings.Join(parts, " ")
}
