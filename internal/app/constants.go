package app

import "time"

const (
	openAICacheBreakpointModeExplicit = "explicit"
	readyMessage                      = "bot is online"
	healthCheckPath                   = "/healthz"
	defaultExaSearchEndpoint          = "https://api.exa.ai/search"
	defaultExaContentsEndpoint        = "https://api.exa.ai/contents"
	defaultExaMCPEndpoint             = "https://mcp.exa.ai/mcp?tools=web_search_exa"
	defaultSerpAPIGoogleLensEndpoint  = "https://serpapi.com/search.json"
	defaultTavilySearchEndpoint       = "https://api.tavily.com/search"
	defaultTavilyExtractEndpoint      = "https://api.tavily.com/extract"
	defaultFirecrawlScrapeEndpoint    = "https://api.firecrawl.dev/v2/scrape"
	defaultTinyFishSearchEndpoint     = "https://api.search.tinyfish.ai"
	defaultTinyFishFetchEndpoint      = "https://api.fetch.tinyfish.ai"
	defaultParallelSearchEndpoint     = "https://api.parallel.ai/v1/search"
	defaultGithubGistEndpoint         = "https://api.github.com/gists"
	defaultGistFilename               = "llmcord-go reply.md"
	defaultStatusMessage              = "github.com/jakobdylanc/llmcord"
	defaultMaxImages                  = 100
	defaultMaxMessages                = 25
	maxMessageNodes                   = 500
	registeredCommandCount            = 7
	maxAutocompleteChoices            = 25
	statusMessageMaxLength            = 128
	streamingIndicator                = " ..."
	unknownText                       = "Unknown"
	applicationJSONContentType        = "application/json"
	contentTypeHeader                 = "Content-Type"
	httpStatusOKText                  = "200 OK"
	fileExtensionAVIF                 = ".avif"
	fileExtensionGIF                  = ".gif"
	fileExtensionJPEG                 = ".jpeg"
	fileExtensionJPG                  = ".jpg"
	fileExtensionPNG                  = ".png"
	fileExtensionWEBP                 = ".webp"
	embedResponseMaxLength            = 4096 - len(streamingIndicator)
	embedColorComplete                = 0x006400
	embedColorIncomplete              = 0xffa500
	embedColorFailure                 = 0x8b0000
	modelCommandName                  = "model"
	modelCommandDescription           = "View or switch the current model"
	modelOptionName                   = "model"
	modelOptionDescription            = "Model to view or use"
	searchTypeCommandName             = "searchtype"
	searchTypeCommandDescription      = "View or switch current Exa search type (ordered lowest to highest latency)"
	searchTypeOptionName              = "type"
	searchTypeOptionDescription       = "Exa search type to view or use (ordered lowest to highest latency)"
	groundingCommandName              = "grounding"
	groundingCommandDescription       = "View or switch Gemini grounding (native search)"
	groundingOptionName               = "enabled"
	groundingOptionDescription        = "Whether grounding should be enabled"

	createChannelCommandName           = "createchannel"
	createChannelCommandDescription    = "Create a Discord text channel"
	createChannelNameOptionName        = "channelname"
	createChannelNameOptionDescription = "Name for the new channel"

	editChannelNameCommandName                = "editchannelname"
	editChannelNameCommandDescription         = "Rename a Discord channel"
	editChannelNameChannelIDOptionName        = "channelid"
	editChannelNameChannelIDOptionDescription = "ID of the channel to rename"
	editChannelNameOptionName                 = "newchannelname"
	editChannelNameOptionDescription          = "New name for the channel"

	moveChannelCommandName                = "movechannel"
	moveChannelCommandDescription         = "Move a Discord channel up or down within its section"
	moveChannelChannelIDOptionName        = "channelid"
	moveChannelChannelIDOptionDescription = "ID of the channel to move"
	moveChannelMovementOptionName         = "movement"
	moveChannelMovementOptionDescription  = "Direction to move the channel (`up` or `down`)"
	moveChannelHowManyOptionName          = "howmany"
	moveChannelHowManyOptionDescription   = "How many channels to cross in the visible category order"
	moveChannelMovementUp                 = "up"
	moveChannelMovementDown               = "down"

	maintenanceCommandName                = "maintenance"
	maintenanceCommandDescription         = "Manage maintenance mode for a channel"
	maintenanceStartSubcommandName        = "start"
	maintenanceStartSubcommandDescription = "Enable maintenance mode for a channel"
	maintenanceStopSubcommandName         = "stop"
	maintenanceStopSubcommandDescription  = "Disable maintenance mode for a channel"
	maintenanceChannelIDOptionName        = "channel_id"
	maintenanceChannelIDOptionDescription = "ID of the channel to manage"
	maintenanceOwnerID                    = "676735636656357396"
	maintenanceBotID                      = "1307756710072549439"

	optimizedHTTPDialTimeout             = 30 * time.Second
	optimizedHTTPDialKeepAlive           = 30 * time.Second
	optimizedHTTPMaxIdleConns            = 100
	optimizedHTTPMaxIdleConnsPerHost     = 100
	optimizedHTTPIdleConnTimeout         = 90 * time.Second
	optimizedHTTPTLSHandshakeTimeout     = 10 * time.Second
	optimizedHTTPExpectContinueTimeout   = 1 * time.Second
	showSourcesButtonCustomID            = "show_sources"
	showSourcesPageButtonCustomIDPrefix  = "show_sources_page:"
	showSourcesButtonLabel               = "Show Sources"
	showImagesButtonCustomID             = "show_images"
	showImagesPageButtonCustomIDPrefix   = "show_images_page:"
	showImagesButtonLabel                = "Show Images"
	showThinkingButtonCustomID           = "show_thinking"
	showThinkingPageButtonCustomIDPrefix = "show_thinking_page:"
	showThinkingButtonLabel              = "Show Thinking"
	showSourcesPreviousButtonLabel       = "Previous"
	showSourcesNextButtonLabel           = "Next"
	createGistButtonCustomID             = "create_gist"
	createGistButtonLabel                = "View response better on GitHub Gist"
	messageRoleAssistant                 = "assistant"
	finishReasonStop                     = "stop"
	finishReasonLength                   = "length"
	prematureStreamRetryMaxAttempts      = 5
	// maxWebSearchToolRounds bounds the web_search tool loop: tool calling is
	// never disabled, so this is the only runaway guard.
	maxWebSearchToolRounds                = 3
	prematureStreamRetryFixedDelay        = 1 * time.Second
	externalRequestConcurrency            = 8
	discordReconnectEnvVarName            = "LLMCORD_RECONNECT"
	DefaultConfigPath                     = "config.yaml"
	ConfigPathEnvironmentVariable         = "LLMCORD_CONFIG_PATH"
	LegacyConfigPathEnvironmentVariable   = "CONFIG_PATH"
	HTTPAddressEnvironmentVariable        = "LLMCORD_HTTP_ADDR"
	PortEnvironmentVariable               = "PORT"
	defaultWebSearchMaxURLs               = 5
	defaultFirecrawlMaxMarkdownCharacters = 12000
	// TinyFish latency bounds: per_url_timeout_ms caps each URL's
	// server-side fetch+extract work (API default 110 s per URL, 120 s CDN
	// ceiling per batch); the request timeouts cap wall clock per HTTP call
	// so a stalled connection cannot hang search enrichment or website
	// fetching.
	tinyFishFetchPerURLTimeoutMS                  = 30000
	tinyFishSearchRequestTimeout                  = 20 * time.Second
	tinyFishFetchRequestTimeout                   = 60 * time.Second
	parallelSearchRequestTimeout                  = 20 * time.Second
	exaSearchTypeInstant                          = "instant"
	exaSearchTypeFast                             = "fast"
	exaSearchTypeAuto                             = "auto"
	exaSearchTypeDeepLite                         = "deep-lite"
	exaSearchTypeDeep                             = "deep"
	exaSearchTypeDeepReasoning                    = "deep-reasoning"
	defaultExaSearchType                          = exaSearchTypeAuto
	messageRoleSystem                             = "system"
	messageContentKey                             = "content"
	messageRoleKey                                = "role"
	messageTypeKey                                = "type"
	messageTextKey                                = "text"
	messageURLKey                                 = "url"
	messagePlatformKey                            = "platform"
	messageVideoIDKey                             = "video_id"
	messageCookieHeader                           = "Cookie"
	messageDetailKey                              = "detail"
	messageKindValue                              = "message"
	defaultExaSearchTextMaxCharacters             = 15000
	defaultExaContentsLivecrawlTimeoutMS          = 15000
	exaContentsLivecrawlExtendedTimeoutMultiplier = 2
	exaContentsLivecrawlRetryMaxAttempts          = 3
	maxYouTubeComments                            = 50
	showSourcesMessageMaxLength                   = 1900
	showSourcesPageBodyMaxLength                  = showSourcesMessageMaxLength - 64
	showThinkingUnavailableText                   = "No thinking process available."
	showThinkingMessageMaxLength                  = 1900
	showThinkingPageBodyMaxLength                 = showThinkingMessageMaxLength - 64
	geminiFilePollInterval                        = 500 * time.Millisecond
	geminiInlineImageByteLimit                    = 4 * 1024 * 1024
	openRouterHost                                = "openrouter.ai"
	openRouterTransformsField                     = "transforms"
	openRouterMiddleOutTransform                  = "middle-out"
	tikTokRenderPollInterval                      = 500 * time.Millisecond
	tavilyExtractTimeoutSeconds                   = 10
	youtubeShortsInfoRetryDelay                   = time.Second
	youtubeShortsInfoRetryMaxAttempts             = 3
	youtubeShortsLoaderPollInterval               = 500 * time.Millisecond
	typingRefreshInterval                         = 8 * time.Second
	discordClientTimeout                          = 20 * time.Second
	editDelay                                     = time.Second
	publicHTTPReadHeaderTimeout                   = 5 * time.Second
	publicHTTPIdleTimeout                         = 30 * time.Second
	discordStartupProbeReadLimit                  = 4096
	errorBodySnippetMaxLength                     = 200
	discordHeartbeatAckMissedIntervals            = 4
	discordAwakeProbeTimeout                      = 10 * time.Second
	discordAwakeTestProbeInterval                 = 50 * time.Millisecond
	discordAwakeProbePollInterval                 = 15 * time.Second
	discordAwakeProbeSuccessStatuses              = 2
	discordReconnectSessionReopenDelay            = 2 * time.Second
	discordReconnectSessionCloseDelay             = 2 * time.Second
	handleStreamDeltaErrorFormat                  = "handle stream delta: %w"
	numberedListLineFormat                        = "%d. %s\n"
	sseScannerInitialBuffer                       = 64 * 1024
	sseScannerMaxBuffer                           = 1024 * 1024
	mappingNodePairSize                           = 2
	smallMapCapacity                              = 3
	tavilyResultFieldCapacity                     = 4
	userAgentHeader                               = "User-Agent"
	requestBodyBaseFields                         = 3
	configuredModelParts                          = 2
)

func exaSearchTypes() []string {
	return []string{
		exaSearchTypeInstant,
		exaSearchTypeFast,
		exaSearchTypeAuto,
		exaSearchTypeDeepLite,
		exaSearchTypeDeep,
		exaSearchTypeDeepReasoning,
	}
}
