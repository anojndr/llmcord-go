package main

import "time"

const (
	defaultConfigPath                    = "config.yaml"
	configPathEnvironmentVariable        = "LLMCORD_CONFIG_PATH"
	legacyConfigPathEnvironmentVariable  = "CONFIG_PATH"
	httpAddressEnvironmentVariable       = "LLMCORD_HTTP_ADDR"
	portEnvironmentVariable              = "PORT"
	readyMessage                         = "bot is online"
	healthCheckPath                      = "/healthz"
	defaultExaResearchBaseURL            = "https://api.exa.ai"
	defaultExaSearchEndpoint             = "https://api.exa.ai/search"
	defaultExaContentsEndpoint           = "https://api.exa.ai/contents"
	defaultExaMCPEndpoint                = "https://mcp.exa.ai/mcp?tools=web_search_exa"
	defaultSerpAPIGoogleLensEndpoint     = "https://serpapi.com/search.json"
	defaultTavilySearchEndpoint          = "https://api.tavily.com/search"
	defaultTavilyExtractEndpoint         = "https://api.tavily.com/extract"
	defaultRentryEndpoint                = "https://rentry.co/"
	defaultStatusMessage                 = "github.com/jakobdylanc/llmcord"
	defaultMaxText                       = 100000
	defaultMaxImages                     = 5
	defaultMaxMessages                   = 25
	maxMessageNodes                      = 500
	registeredCommandCount               = 3
	maxAutocompleteChoices               = 25
	statusMessageMaxLength               = 128
	plainResponseMaxLength               = 4000
	streamingIndicator                   = " ..."
	unknownText                          = "Unknown"
	applicationJSONContentType           = "application/json"
	contentTypeHeader                    = "Content-Type"
	httpStatusOKText                     = "200 OK"
	fileExtensionAVIF                    = ".avif"
	fileExtensionGIF                     = ".gif"
	fileExtensionJPEG                    = ".jpeg"
	fileExtensionJPG                     = ".jpg"
	fileExtensionPNG                     = ".png"
	fileExtensionWEBP                    = ".webp"
	embedResponseMaxLength               = 4096 - len(streamingIndicator)
	embedColorComplete                   = 0x006400
	embedColorIncomplete                 = 0xffa500
	embedColorFailure                    = 0x8b0000
	modelCommandName                     = "model"
	modelCommandDescription              = "View or switch the current model"
	modelOptionName                      = "model"
	modelOptionDescription               = "Model to view or use"
	searchTypeCommandName                = "searchtype"
	searchTypeCommandDescription         = "View or switch the current Exa search type"
	searchTypeOptionName                 = "type"
	searchTypeOptionDescription          = "Exa search type to view or use"
	searchDeciderModelCommandName        = "searchdecidermodel"
	searchDeciderModelCommandDescription = "View or switch the current search decider model"
	searchDeciderModelOptionName         = "model"
	searchDeciderModelOptionDescription  = "Search decider model to view or use"
	showSourcesButtonCustomID            = "show_sources"
	showSourcesPageButtonCustomIDPrefix  = "show_sources_page:"
	showSourcesButtonLabel               = "Show Sources"
	showThinkingButtonCustomID           = "show_thinking"
	showThinkingPageButtonCustomIDPrefix = "show_thinking_page:"
	showThinkingButtonLabel              = "Show Thinking"
	showSourcesPreviousButtonLabel       = "Previous"
	showSourcesNextButtonLabel           = "Next"
	viewOnRentryButtonCustomID           = "view_on_rentry"
	viewOnRentryButtonLabel              = "View on Rentry"
	messageRoleAssistant                 = "assistant"
	finishReasonStop                     = "stop"
	finishReasonLength                   = "length"
	maxSearchQueries                     = 500
	defaultWebSearchMaxURLs              = 5
	exaSearchTypeAuto                    = "auto"
	exaSearchTypeFast                    = "fast"
	exaSearchTypeInstant                 = "instant"
	exaSearchTypeDeep                    = "deep"
	exaSearchTypeDeepReasoning           = "deep-reasoning"
	defaultExaSearchType                 = exaSearchTypeAuto
	defaultExaSearchTextMaxCharacters    = 15000
	exaContentsLivecrawlTimeoutMS        = 12000
	maxYouTubeComments                   = 50
	showSourcesMessageMaxLength          = 1900
	showSourcesPageBodyMaxLength         = showSourcesMessageMaxLength - 64
	showThinkingUnavailableText          = "No thinking process available."
	showThinkingMessageMaxLength         = 1900
	showThinkingPageBodyMaxLength        = showThinkingMessageMaxLength - 64
	attachmentRequestTimeout             = 30 * time.Second
	attachmentDownloadMaxAttempts        = 3
	attachmentRetryBaseDelay             = 100 * time.Millisecond
	geminiFilePollInterval               = 500 * time.Millisecond
	geminiFileProcessingTimeout          = 2 * time.Minute
	geminiInlineImageByteLimit           = 4 * 1024 * 1024
	openRouterHost                       = "openrouter.ai"
	openRouterTransformsField            = "transforms"
	openRouterMiddleOutTransform         = "middle-out"
	searchDeciderTimeout                 = time.Minute
	chatCompletionTimeout                = 5 * time.Minute
	openAIResponsesChatCompletionTimeout = 30 * time.Minute
	facebookRequestTimeout               = 30 * time.Second
	tikTokRenderPollInterval             = 500 * time.Millisecond
	tikTokRenderTimeout                  = 2 * time.Minute
	tikTokRequestTimeout                 = 30 * time.Second
	webSearchTimeout                     = 30 * time.Second
	rentryRequestTimeout                 = 30 * time.Second
	redditRequestTimeout                 = 30 * time.Second
	websiteRequestTimeout                = 30 * time.Second
	tavilyExtractTimeoutSeconds          = 10
	youtubeRequestTimeout                = 30 * time.Second
	youtubeShortsLoaderPollInterval      = 500 * time.Millisecond
	youtubeShortsRequestTimeout          = 2 * time.Minute
	typingRefreshInterval                = 8 * time.Second
	editDelay                            = time.Second
	publicHTTPReadHeaderTimeout          = 5 * time.Second
	publicHTTPIdleTimeout                = 30 * time.Second
	publicHTTPShutdownTimeout            = 5 * time.Second
	discordStartupProbeReadLimit         = 4096
	errorBodySnippetMaxLength            = 200
	handleStreamDeltaErrorFormat         = "handle stream delta: %w"
	numberedListLineFormat               = "%d. %s\n"
	sseScannerInitialBuffer              = 64 * 1024
	sseScannerMaxBuffer                  = 1024 * 1024
	mappingNodePairSize                  = 2
	smallMapCapacity                     = 3
	tavilyResultFieldCapacity            = 4
	userAgentHeader                      = "User-Agent"
	requestBodyBaseFields                = 3
	configuredModelParts                 = 2
)

func exaSearchTypes() []string {
	return []string{
		exaSearchTypeAuto,
		exaSearchTypeFast,
		exaSearchTypeInstant,
		exaSearchTypeDeep,
		exaSearchTypeDeepReasoning,
	}
}
