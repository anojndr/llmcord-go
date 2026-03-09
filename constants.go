package main

import "time"

const (
	defaultConfigPath        = "config.yaml"
	defaultStatusMessage     = "github.com/jakobdylanc/llmcord"
	defaultMaxText           = 100000
	defaultMaxImages         = 5
	defaultMaxMessages       = 25
	maxMessageNodes          = 500
	maxAutocompleteChoices   = 25
	statusMessageMaxLength   = 128
	plainResponseMaxLength   = 4000
	streamingIndicator       = " ..."
	embedResponseMaxLength   = 4096 - len(streamingIndicator)
	embedColorComplete       = 0x006400
	embedColorIncomplete     = 0xffa500
	modelCommandName         = "model"
	modelCommandDescription  = "View or switch the current model"
	modelOptionName          = "model"
	modelOptionDescription   = "Model to view or use"
	attachmentRequestTimeout = 30 * time.Second
	typingRefreshInterval    = 8 * time.Second
	editDelay                = time.Second
	sseScannerInitialBuffer  = 64 * 1024
	sseScannerMaxBuffer      = 1024 * 1024
	mappingNodePairSize      = 2
	smallMapCapacity         = 3
	requestBodyBaseFields    = 3
	configuredModelParts     = 2
)
