package config

const (
	// defaultExaContentsLivecrawlTimeoutMS is the default livecrawl timeout
	// for Exa contents fetches (moved here with config normalization).
	defaultExaContentsLivecrawlTimeoutMS = 15000
	// defaultExaResearchBaseURL is the default base URL for Exa API search.
	defaultExaResearchBaseURL = "https://api.exa.ai"
	// defaultExaSearchTextMaxCharacters caps the text returned per Exa result.
	defaultExaSearchTextMaxCharacters = 15000
	// defaultExaSearchType is the default Exa search type.
	defaultExaSearchType = "auto"
	// defaultGistFilename is the default GitHub gist filename.
	defaultGistFilename = "llmcord-go reply.md"
	// defaultGithubGistEndpoint is the default GitHub gists API endpoint.
	defaultGithubGistEndpoint = "https://api.github.com/gists"
	// defaultMaxImages is the default max image count per message.
	defaultMaxImages = 5
	// defaultMaxMessages is the default conversation length in messages.
	defaultMaxMessages = 25
	// defaultWebSearchMaxURLs is the default max URLs per web search.
	defaultWebSearchMaxURLs = 5
	// defaultFirecrawlMaxMarkdownCharacters caps Firecrawl markdown per scrape.
	defaultFirecrawlMaxMarkdownCharacters = 12000
	// mappingNodePairSize is the YAML mapping key/value pair size.
	mappingNodePairSize = 2
	// openRouterHost is the OpenRouter host used for transforms detection.
	openRouterHost = "openrouter.ai"
)

// ExaSearchTypes returns the Exa search types in latency order.
func ExaSearchTypes() []string {
	return []string{
		ExaSearchTypeInstant,
		ExaSearchTypeFast,
		ExaSearchTypeAuto,
		ExaSearchTypeDeepLite,
		ExaSearchTypeDeep,
		ExaSearchTypeDeepReasoning,
	}
}

const (
	// ExaSearchTypeInstant is the instant search type.
	ExaSearchTypeInstant = "instant"
	// ExaSearchTypeFast is an Exa search type.
	ExaSearchTypeFast = "fast"
	// ExaSearchTypeAuto is an Exa search type.
	ExaSearchTypeAuto = "auto"
	// ExaSearchTypeDeepLite is an Exa search type.
	ExaSearchTypeDeepLite = "deep-lite"
	// ExaSearchTypeDeep is an Exa search type.
	ExaSearchTypeDeep = "deep"
	// ExaSearchTypeDeepReasoning is an Exa search type.
	ExaSearchTypeDeepReasoning = "deep-reasoning"
)

// Environment variable and default path names.
const (
	DefaultConfigPath                   = "config.yaml"
	ConfigPathEnvironmentVariable       = "LLMCORD_CONFIG_PATH"
	LegacyConfigPathEnvironmentVariable = "CONFIG_PATH"
	HTTPAddressEnvironmentVariable      = "LLMCORD_HTTP_ADDR"
	PortEnvironmentVariable             = "PORT"
)
