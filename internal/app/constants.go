package app

import "time"

const (
	openAICacheBreakpointModeExplicit    = "explicit"
	readyMessage                         = "bot is online"
	healthCheckPath                      = "/healthz"
	defaultExaResearchBaseURL            = "https://api.exa.ai"
	defaultExaSearchEndpoint             = "https://api.exa.ai/search"
	defaultExaContentsEndpoint           = "https://api.exa.ai/contents"
	defaultExaMCPEndpoint                = "https://mcp.exa.ai/mcp?tools=web_search_exa"
	defaultSerpAPIGoogleLensEndpoint     = "https://serpapi.com/search.json"
	defaultTavilySearchEndpoint          = "https://api.tavily.com/search"
	defaultTavilyExtractEndpoint         = "https://api.tavily.com/extract"
	defaultFirecrawlScrapeEndpoint       = "https://api.firecrawl.dev/v2/scrape"
	defaultGithubGistEndpoint            = "https://api.github.com/gists"
	defaultGistFilename                  = "llmcord-go reply.md"
	defaultStatusMessage                 = "github.com/jakobdylanc/llmcord"
	defaultMaxImages                     = 5
	defaultMaxMessages                   = 25
	maxMessageNodes                      = 500
	registeredCommandCount               = 6
	maxAutocompleteChoices               = 25
	statusMessageMaxLength               = 128
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
	searchTypeCommandDescription         = "View or switch current Exa search type (ordered lowest to highest latency)"
	searchTypeOptionName                 = "type"
	searchTypeOptionDescription          = "Exa search type to view or use (ordered lowest to highest latency)"
	searchDeciderModelCommandName        = "searchdecidermodel"
	searchDeciderModelCommandDescription = "View or switch the current search decider model"
	searchDeciderModelOptionName         = "model"
	searchDeciderModelOptionDescription  = "Search decider model to view or use"
	groundingCommandName                 = "grounding"
	groundingCommandDescription          = "View or switch Gemini grounding (native search)"
	groundingOptionName                  = "enabled"
	groundingOptionDescription           = "Whether grounding should be enabled"

	editChannelNameCommandName                = "editchannelname"
	editChannelNameCommandDescription         = "Rename a Discord channel"
	editChannelNameChannelIDOptionName        = "channelid"
	editChannelNameChannelIDOptionDescription = "ID of the channel to rename"
	editChannelNameOptionName                 = "newchannelname"
	editChannelNameOptionDescription          = "New name for the channel"

	moveChannelCommandName                = "movechannel"
	moveChannelCommandDescription         = "Move a Discord channel up or down"
	moveChannelChannelIDOptionName        = "channelid"
	moveChannelChannelIDOptionDescription = "ID of the channel to move"
	moveChannelMovementOptionName         = "movement"
	moveChannelMovementOptionDescription  = "Direction to move the channel (`up` or `down`)"
	moveChannelHowManyOptionName          = "howmany"
	moveChannelHowManyOptionDescription   = "How many positions to move the channel"
	moveChannelMovementUp                 = "up"
	moveChannelMovementDown               = "down"

	optimizedHTTPDialTimeout                      = 30 * time.Second
	optimizedHTTPDialKeepAlive                    = 30 * time.Second
	optimizedHTTPMaxIdleConns                     = 100
	optimizedHTTPMaxIdleConnsPerHost              = 100
	optimizedHTTPIdleConnTimeout                  = 90 * time.Second
	optimizedHTTPTLSHandshakeTimeout              = 10 * time.Second
	optimizedHTTPExpectContinueTimeout            = 1 * time.Second
	showSourcesButtonCustomID                     = "show_sources"
	showSourcesPageButtonCustomIDPrefix           = "show_sources_page:"
	showSourcesButtonLabel                        = "Show Sources"
	showThinkingButtonCustomID                    = "show_thinking"
	showThinkingPageButtonCustomIDPrefix          = "show_thinking_page:"
	showThinkingButtonLabel                       = "Show Thinking"
	showSourcesPreviousButtonLabel                = "Previous"
	showSourcesNextButtonLabel                    = "Next"
	createGistButtonCustomID                      = "create_gist"
	createGistButtonLabel                         = "View response better on GitHub Gist"
	messageRoleAssistant                          = "assistant"
	finishReasonStop                              = "stop"
	finishReasonLength                            = "length"
	externalRequestConcurrency                    = 8
	discordReconnectEnvVarName                    = "LLMCORD_RECONNECT"
	maxSearchQueries                              = 500
	defaultWebSearchMaxURLs                       = 5
	defaultFirecrawlMaxMarkdownCharacters         = 12000
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
	discordReconnectBackoffCapSeconds             = 120
	discordReconnectImmediateBackoffBaseSeconds   = 3
	discordReconnectProbeBackoffBaseSeconds       = 15
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

// discordReconnectImmediateBackoffCapsSeconds returns the attempt caps
// applied in order while the network is unreachable (probe failing); the
// library doubles its own delay each attempt, so scaling the cap down after
// each observed attempt keeps the wait bounded and shrinking.
func discordReconnectImmediateBackoffCapsSeconds() []int64 {
	return []int64{5, 10, 20}
}

// discordReconnectProbeBackoffCapsSeconds returns the attempt caps applied
// in order while the gateway probe still succeeds; the backoff begins at a
// small value and grows to these bounds, then stays capped.
func discordReconnectProbeBackoffCapsSeconds() []int64 {
	return []int64{20, 40, 60, 120}
}
