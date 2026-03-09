package main

import "time"

const (
	defaultConfigPath                    = "config.yaml"
	defaultExaMCPEndpoint                = "https://mcp.exa.ai/mcp?tools=web_search_exa"
	defaultStatusMessage                 = "github.com/jakobdylanc/llmcord"
	defaultMaxText                       = 100000
	defaultMaxImages                     = 5
	defaultMaxMessages                   = 25
	maxMessageNodes                      = 500
	registeredCommandCount               = 2
	maxAutocompleteChoices               = 25
	statusMessageMaxLength               = 128
	plainResponseMaxLength               = 4000
	streamingIndicator                   = " ..."
	embedResponseMaxLength               = 4096 - len(streamingIndicator)
	embedColorComplete                   = 0x006400
	embedColorIncomplete                 = 0xffa500
	modelCommandName                     = "model"
	modelCommandDescription              = "View or switch the current model"
	modelOptionName                      = "model"
	modelOptionDescription               = "Model to view or use"
	searchDeciderModelCommandName        = "searchdecidermodel"
	searchDeciderModelCommandDescription = "View or switch the current search decider model"
	searchDeciderModelOptionName         = "model"
	searchDeciderModelOptionDescription  = "Search decider model to view or use"
	showSourcesButtonCustomID            = "show_sources"
	showSourcesButtonLabel               = "Show Sources"
	maxSearchQueries                     = 5
	maxSourcesPerQuery                   = 3
	showSourcesMessageMaxLength          = 1900
	attachmentRequestTimeout             = 30 * time.Second
	searchDeciderTimeout                 = 30 * time.Second
	webSearchTimeout                     = 30 * time.Second
	typingRefreshInterval                = 8 * time.Second
	editDelay                            = time.Second
	sseScannerInitialBuffer              = 64 * 1024
	sseScannerMaxBuffer                  = 1024 * 1024
	mappingNodePairSize                  = 2
	smallMapCapacity                     = 3
	requestBodyBaseFields                = 3
	configuredModelParts                 = 2
)
