package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type scalarString string

func (value *scalarString) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!null" {
		*value = ""

		return nil
	}

	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("decode scalar string: %w", os.ErrInvalid)
	}

	*value = scalarString(node.Value)

	return nil
}

type idList []string

func (list *idList) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!null" {
		*list = nil

		return nil
	}

	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("decode id list: %w", os.ErrInvalid)
	}

	items := make([]string, 0, len(node.Content))

	for _, childNode := range node.Content {
		if childNode.Kind != yaml.ScalarNode {
			return fmt.Errorf("decode id list item: %w", os.ErrInvalid)
		}

		items = append(items, childNode.Value)
	}

	*list = items

	return nil
}

type scopePermissions struct {
	AllowedIDs idList `yaml:"allowed_ids"`
	BlockedIDs idList `yaml:"blocked_ids"`
}

type userPermissions struct {
	AdminIDs   idList `yaml:"admin_ids"`
	AllowedIDs idList `yaml:"allowed_ids"`
	BlockedIDs idList `yaml:"blocked_ids"`
}

type permissionsConfig struct {
	Users    userPermissions  `yaml:"users"`
	Roles    scopePermissions `yaml:"roles"`
	Channels scopePermissions `yaml:"channels"`
}

type rawProviderConfig struct {
	Type         scalarString   `yaml:"type"`
	BaseURL      scalarString   `yaml:"base_url"`
	APIKey       scalarString   `yaml:"api_key"`
	ExtraHeaders map[string]any `yaml:"extra_headers"`
	ExtraQuery   map[string]any `yaml:"extra_query"`
	ExtraBody    map[string]any `yaml:"extra_body"`
}

type providerConfig struct {
	Type         string
	BaseURL      string
	APIKey       string
	ExtraHeaders map[string]any
	ExtraQuery   map[string]any
	ExtraBody    map[string]any
}

type providerAPIKind string

const (
	providerAPIKindOpenAI providerAPIKind = "openai"
	providerAPIKindGemini providerAPIKind = "gemini"
)

type rawConfig struct {
	BotToken           scalarString                 `yaml:"bot_token"`
	ClientID           scalarString                 `yaml:"client_id"`
	StatusMessage      string                       `yaml:"status_message"`
	MaxText            *int                         `yaml:"max_text"`
	MaxImages          *int                         `yaml:"max_images"`
	MaxMessages        *int                         `yaml:"max_messages"`
	UsePlainResponses  bool                         `yaml:"use_plain_responses"`
	AllowDMs           *bool                        `yaml:"allow_dms"`
	Permissions        permissionsConfig            `yaml:"permissions"`
	Providers          map[string]rawProviderConfig `yaml:"providers"`
	Models             map[string]map[string]any    `yaml:"models"`
	SearchDeciderModel scalarString                 `yaml:"search_decider_model"`
	SystemPrompt       string                       `yaml:"system_prompt"`
}

type config struct {
	BotToken           string
	ClientID           string
	StatusMessage      string
	MaxText            int
	MaxImages          int
	MaxMessages        int
	UsePlainResponses  bool
	AllowDMs           bool
	Permissions        permissionsConfig
	Providers          map[string]providerConfig
	Models             map[string]map[string]any
	ModelOrder         []string
	SearchDeciderModel string
	SystemPrompt       string
}

func loadConfig(filename string) (config, error) {
	configBytes, err := os.ReadFile(filepath.Clean(filename))
	if err != nil {
		return config{}, fmt.Errorf("read config %q: %w", filename, err)
	}

	var rawLoadedConfig rawConfig

	err = yaml.Unmarshal(configBytes, &rawLoadedConfig)
	if err != nil {
		return config{}, fmt.Errorf("parse config %q: %w", filename, err)
	}

	var rootNode yaml.Node

	err = yaml.Unmarshal(configBytes, &rootNode)
	if err != nil {
		return config{}, fmt.Errorf("parse config node %q: %w", filename, err)
	}

	modelOrder, err := orderedMappingKeys(&rootNode, "models")
	if err != nil {
		return config{}, fmt.Errorf("read model order from %q: %w", filename, err)
	}

	loadedProviders := make(map[string]providerConfig, len(rawLoadedConfig.Providers))
	for providerName, rawProvider := range rawLoadedConfig.Providers {
		loadedProviders[providerName] = providerConfig{
			Type:         string(rawProvider.Type),
			BaseURL:      string(rawProvider.BaseURL),
			APIKey:       string(rawProvider.APIKey),
			ExtraHeaders: rawProvider.ExtraHeaders,
			ExtraQuery:   rawProvider.ExtraQuery,
			ExtraBody:    rawProvider.ExtraBody,
		}
	}

	allowDMs := true
	if rawLoadedConfig.AllowDMs != nil {
		allowDMs = *rawLoadedConfig.AllowDMs
	}

	searchDeciderModel := strings.TrimSpace(string(rawLoadedConfig.SearchDeciderModel))
	if searchDeciderModel == "" && len(modelOrder) > 0 {
		searchDeciderModel = modelOrder[0]
	}

	loadedConfig := config{
		BotToken:           string(rawLoadedConfig.BotToken),
		ClientID:           string(rawLoadedConfig.ClientID),
		StatusMessage:      rawLoadedConfig.StatusMessage,
		MaxText:            intValueOrDefault(rawLoadedConfig.MaxText, defaultMaxText),
		MaxImages:          intValueOrDefault(rawLoadedConfig.MaxImages, defaultMaxImages),
		MaxMessages:        intValueOrDefault(rawLoadedConfig.MaxMessages, defaultMaxMessages),
		UsePlainResponses:  rawLoadedConfig.UsePlainResponses,
		AllowDMs:           allowDMs,
		Permissions:        rawLoadedConfig.Permissions,
		Providers:          loadedProviders,
		Models:             rawLoadedConfig.Models,
		ModelOrder:         modelOrder,
		SearchDeciderModel: searchDeciderModel,
		SystemPrompt:       rawLoadedConfig.SystemPrompt,
	}

	err = validateConfig(loadedConfig)
	if err != nil {
		return config{}, fmt.Errorf("validate config %q: %w", filename, err)
	}

	return loadedConfig, nil
}

func orderedMappingKeys(rootNode *yaml.Node, fieldName string) ([]string, error) {
	if len(rootNode.Content) == 0 {
		return nil, fmt.Errorf("decode document root: %w", os.ErrInvalid)
	}

	mappingNode := rootNode.Content[0]
	if mappingNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("decode mapping root: %w", os.ErrInvalid)
	}

	for index := 0; index < len(mappingNode.Content)-1; index += mappingNodePairSize {
		keyNode := mappingNode.Content[index]
		valueNode := mappingNode.Content[index+1]

		if keyNode.Value != fieldName {
			continue
		}

		if valueNode.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("decode mapping field %q: %w", fieldName, os.ErrInvalid)
		}

		keys := make([]string, 0, len(valueNode.Content)/mappingNodePairSize)
		for valueIndex := 0; valueIndex < len(valueNode.Content)-1; valueIndex += mappingNodePairSize {
			keys = append(keys, valueNode.Content[valueIndex].Value)
		}

		return keys, nil
	}

	return nil, fmt.Errorf("find mapping field %q: %w", fieldName, os.ErrNotExist)
}

func intValueOrDefault(value *int, fallback int) int {
	if value == nil {
		return fallback
	}

	return *value
}

func validateConfig(loadedConfig config) error {
	if strings.TrimSpace(loadedConfig.BotToken) == "" {
		return fmt.Errorf("bot_token is required: %w", os.ErrInvalid)
	}

	if len(loadedConfig.ModelOrder) == 0 {
		return fmt.Errorf("models must contain at least one entry: %w", os.ErrInvalid)
	}

	if loadedConfig.MaxText <= 0 {
		return fmt.Errorf("max_text must be greater than zero: %w", os.ErrInvalid)
	}

	if loadedConfig.MaxImages < 0 {
		return fmt.Errorf("max_images must not be negative: %w", os.ErrInvalid)
	}

	if loadedConfig.MaxMessages <= 0 {
		return fmt.Errorf("max_messages must be greater than zero: %w", os.ErrInvalid)
	}

	for _, modelName := range loadedConfig.ModelOrder {
		providerName, _, err := splitConfiguredModel(modelName)
		if err != nil {
			return fmt.Errorf("parse model %q: %w", modelName, err)
		}

		provider, ok := loadedConfig.Providers[providerName]
		if !ok {
			return fmt.Errorf("model %q references unknown provider %q: %w", modelName, providerName, os.ErrNotExist)
		}

		err = provider.validate(providerName)
		if err != nil {
			return err
		}
	}

	if !loadedConfig.hasModel(loadedConfig.SearchDeciderModel) {
		return fmt.Errorf(
			"search_decider_model %q is not defined in models: %w",
			loadedConfig.SearchDeciderModel,
			os.ErrNotExist,
		)
	}

	return nil
}

func (loadedConfig config) firstModel() string {
	if len(loadedConfig.ModelOrder) == 0 {
		return ""
	}

	return loadedConfig.ModelOrder[0]
}

func (loadedConfig config) hasModel(modelName string) bool {
	_, ok := loadedConfig.Models[modelName]

	return ok
}

func (provider providerConfig) apiKind() providerAPIKind {
	switch strings.ToLower(strings.TrimSpace(provider.Type)) {
	case "", string(providerAPIKindOpenAI):
		if looksLikeGeminiCompatibilityBaseURL(provider.BaseURL) {
			return providerAPIKindGemini
		}

		return providerAPIKindOpenAI
	case string(providerAPIKindGemini):
		return providerAPIKindGemini
	default:
		return providerAPIKind(strings.ToLower(strings.TrimSpace(provider.Type)))
	}
}

func (provider providerConfig) validate(providerName string) error {
	switch provider.apiKind() {
	case providerAPIKindOpenAI:
		if strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("provider %q is missing base_url: %w", providerName, os.ErrInvalid)
		}

		return nil
	case providerAPIKindGemini:
		return nil
	default:
		return fmt.Errorf(
			"provider %q has unsupported type %q: %w",
			providerName,
			strings.TrimSpace(provider.Type),
			os.ErrInvalid,
		)
	}
}

func looksLikeGeminiCompatibilityBaseURL(baseURL string) bool {
	return strings.Contains(
		strings.ToLower(strings.TrimSpace(baseURL)),
		"generativelanguage.googleapis.com",
	)
}
