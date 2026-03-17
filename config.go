package main

import (
	"fmt"
	"net/url"
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
	Type         scalarString     `yaml:"type"`
	BaseURL      scalarString     `yaml:"base_url"`
	APIKey       scalarStringList `yaml:"api_key"`
	ExtraHeaders map[string]any   `yaml:"extra_headers"`
	ExtraQuery   map[string]any   `yaml:"extra_query"`
	ExtraBody    map[string]any   `yaml:"extra_body"`
}

type rawTavilySearchConfig struct {
	APIKey scalarStringList `yaml:"api_key"`
}

type rawSerpAPIVisualSearchConfig struct {
	APIKey scalarStringList `yaml:"api_key"`
}

type rawVisualSearchConfig struct {
	SerpAPI rawSerpAPIVisualSearchConfig `yaml:"serpapi"`
}

type rawWebSearchConfig struct {
	PrimaryProvider scalarString          `yaml:"primary_provider"`
	MaxURLs         *int                  `yaml:"max_urls"`
	Tavily          rawTavilySearchConfig `yaml:"tavily"`
}

type rawDatabaseConfig struct {
	ConnectionString scalarString `yaml:"connection_string"`
}

type providerConfig struct {
	Type         string
	BaseURL      string
	APIKey       string
	APIKeys      []string
	ExtraHeaders map[string]any
	ExtraQuery   map[string]any
	ExtraBody    map[string]any
}

type tavilySearchConfig struct {
	APIKey  string
	APIKeys []string
}

type serpAPIVisualSearchConfig struct {
	APIKey  string
	APIKeys []string
}

type visualSearchConfig struct {
	SerpAPI serpAPIVisualSearchConfig
}

type webSearchProviderKind string

const (
	webSearchProviderKindMCP    webSearchProviderKind = "mcp"
	webSearchProviderKindTavily webSearchProviderKind = "tavily"
)

type webSearchConfig struct {
	PrimaryProvider webSearchProviderKind
	MaxURLs         int
	Tavily          tavilySearchConfig
}

type databaseConfig struct {
	ConnectionString string
}

type providerAPIKind string

const (
	providerAPIKindOpenAI      providerAPIKind = "openai"
	providerAPIKindOpenAICodex providerAPIKind = "openai-codex"
	providerAPIKindGemini      providerAPIKind = "gemini"
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
	WebSearch          rawWebSearchConfig           `yaml:"web_search"`
	VisualSearch       rawVisualSearchConfig        `yaml:"visual_search"`
	Database           rawDatabaseConfig            `yaml:"database"`
	Models             map[string]map[string]any    `yaml:"models"`
	ChannelModelLocks  map[string]scalarString      `yaml:"channel_model_locks"`
	SearchDeciderModel scalarString                 `yaml:"search_decider_model"`
	MediaAnalysisModel scalarString                 `yaml:"media_analysis_model"`
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
	WebSearch          webSearchConfig
	VisualSearch       visualSearchConfig
	Database           databaseConfig
	Models             map[string]map[string]any
	ModelOrder         []string
	ChannelModelLocks  map[string]string
	SearchDeciderModel string
	MediaAnalysisModel string
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

	return buildLoadedConfig(filename, configBytes, rawLoadedConfig)
}

func buildLoadedConfig(
	filename string,
	configBytes []byte,
	rawLoadedConfig rawConfig,
) (config, error) {
	var rootNode yaml.Node

	err := yaml.Unmarshal(configBytes, &rootNode)
	if err != nil {
		return config{}, fmt.Errorf("parse config node %q: %w", filename, err)
	}

	modelOrder, err := orderedMappingKeys(&rootNode, "models")
	if err != nil {
		return config{}, fmt.Errorf("read model order from %q: %w", filename, err)
	}

	loadedProviders := make(map[string]providerConfig, len(rawLoadedConfig.Providers))
	for providerName, rawProvider := range rawLoadedConfig.Providers {
		apiKeys := normalizeAPIKeys([]string(rawProvider.APIKey))

		loadedProviders[providerName] = providerConfig{
			Type:         string(rawProvider.Type),
			BaseURL:      string(rawProvider.BaseURL),
			APIKey:       firstAPIKey(apiKeys),
			APIKeys:      apiKeys,
			ExtraHeaders: rawProvider.ExtraHeaders,
			ExtraQuery:   rawProvider.ExtraQuery,
			ExtraBody:    rawProvider.ExtraBody,
		}
	}

	tavilyAPIKeys := normalizeAPIKeys([]string(rawLoadedConfig.WebSearch.Tavily.APIKey))
	serpAPIVisualSearchKeys := normalizeAPIKeys([]string(rawLoadedConfig.VisualSearch.SerpAPI.APIKey))
	primarySearchProvider := normalizeWebSearchProvider(rawLoadedConfig.WebSearch.PrimaryProvider)

	allowDMs := true
	if rawLoadedConfig.AllowDMs != nil {
		allowDMs = *rawLoadedConfig.AllowDMs
	}

	searchDeciderModel := strings.TrimSpace(string(rawLoadedConfig.SearchDeciderModel))
	if searchDeciderModel == "" && len(modelOrder) > 0 {
		searchDeciderModel = modelOrder[0]
	}

	mediaAnalysisModel := strings.TrimSpace(string(rawLoadedConfig.MediaAnalysisModel))
	channelModelLocks := normalizeStringScalarMap(rawLoadedConfig.ChannelModelLocks)

	loadedConfig := config{
		BotToken:          string(rawLoadedConfig.BotToken),
		ClientID:          string(rawLoadedConfig.ClientID),
		StatusMessage:     rawLoadedConfig.StatusMessage,
		MaxText:           intValueOrDefault(rawLoadedConfig.MaxText, defaultMaxText),
		MaxImages:         intValueOrDefault(rawLoadedConfig.MaxImages, defaultMaxImages),
		MaxMessages:       intValueOrDefault(rawLoadedConfig.MaxMessages, defaultMaxMessages),
		UsePlainResponses: rawLoadedConfig.UsePlainResponses,
		AllowDMs:          allowDMs,
		Permissions:       rawLoadedConfig.Permissions,
		Providers:         loadedProviders,
		WebSearch: webSearchConfig{
			PrimaryProvider: primarySearchProvider,
			MaxURLs:         intValueOrDefault(rawLoadedConfig.WebSearch.MaxURLs, defaultWebSearchMaxURLs),
			Tavily: tavilySearchConfig{
				APIKey:  firstAPIKey(tavilyAPIKeys),
				APIKeys: tavilyAPIKeys,
			},
		},
		VisualSearch: visualSearchConfig{
			SerpAPI: serpAPIVisualSearchConfig{
				APIKey:  firstAPIKey(serpAPIVisualSearchKeys),
				APIKeys: serpAPIVisualSearchKeys,
			},
		},
		Database: databaseConfig{
			ConnectionString: strings.TrimSpace(string(rawLoadedConfig.Database.ConnectionString)),
		},
		Models:             rawLoadedConfig.Models,
		ModelOrder:         modelOrder,
		ChannelModelLocks:  channelModelLocks,
		SearchDeciderModel: searchDeciderModel,
		MediaAnalysisModel: mediaAnalysisModel,
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

func normalizeStringScalarMap(rawValues map[string]scalarString) map[string]string {
	if len(rawValues) == 0 {
		return nil
	}

	values := make(map[string]string, len(rawValues))
	for key, value := range rawValues {
		values[strings.TrimSpace(key)] = strings.TrimSpace(string(value))
	}

	return values
}

func normalizeWebSearchProvider(rawValue scalarString) webSearchProviderKind {
	trimmedValue := strings.ToLower(strings.TrimSpace(string(rawValue)))
	if trimmedValue == "" {
		return webSearchProviderKindMCP
	}

	return webSearchProviderKind(trimmedValue)
}

func (loadedConfig webSearchConfig) maxURLs() int {
	if loadedConfig.MaxURLs <= 0 {
		return defaultWebSearchMaxURLs
	}

	return loadedConfig.MaxURLs
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

	err := validateWebSearchConfig(loadedConfig.WebSearch)
	if err != nil {
		return err
	}

	err = validateDatabaseConfig(loadedConfig.Database)
	if err != nil {
		return err
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

	err = validateChannelModelLocks(loadedConfig)
	if err != nil {
		return err
	}

	err = validateMediaAnalysisModel(loadedConfig)
	if err != nil {
		return err
	}

	return nil
}

func validateWebSearchConfig(loadedConfig webSearchConfig) error {
	if loadedConfig.MaxURLs <= 0 {
		return fmt.Errorf("web_search.max_urls must be greater than zero: %w", os.ErrInvalid)
	}

	switch loadedConfig.PrimaryProvider {
	case webSearchProviderKindMCP, webSearchProviderKindTavily:
		return nil
	default:
		return fmt.Errorf(
			"web_search.primary_provider %q is unsupported: %w",
			loadedConfig.PrimaryProvider,
			os.ErrInvalid,
		)
	}
}

func validateChannelModelLocks(loadedConfig config) error {
	for channelID, modelName := range loadedConfig.ChannelModelLocks {
		if channelID == "" {
			return fmt.Errorf("channel_model_locks contains an empty channel id: %w", os.ErrInvalid)
		}

		if !loadedConfig.hasModel(modelName) {
			return fmt.Errorf(
				"channel_model_locks %q references undefined model %q: %w",
				channelID,
				modelName,
				os.ErrNotExist,
			)
		}
	}

	return nil
}

func validateDatabaseConfig(loadedConfig databaseConfig) error {
	trimmedConnectionString := strings.TrimSpace(loadedConfig.ConnectionString)
	if trimmedConnectionString == "" {
		return nil
	}

	parsedURL, err := url.Parse(trimmedConnectionString)
	if err != nil {
		return fmt.Errorf("database.connection_string is invalid: %w", err)
	}

	if parsedURL.Scheme != "postgres" && parsedURL.Scheme != "postgresql" {
		return fmt.Errorf("database.connection_string must use postgres:// or postgresql://: %w", os.ErrInvalid)
	}

	return nil
}

func validateMediaAnalysisModel(loadedConfig config) error {
	if strings.TrimSpace(loadedConfig.MediaAnalysisModel) == "" {
		return nil
	}

	if !loadedConfig.hasModel(loadedConfig.MediaAnalysisModel) {
		return fmt.Errorf(
			"media_analysis_model %q is not defined in models: %w",
			loadedConfig.MediaAnalysisModel,
			os.ErrNotExist,
		)
	}

	apiKind, err := configuredModelAPIKind(
		loadedConfig,
		loadedConfig.MediaAnalysisModel,
	)
	if err != nil {
		return fmt.Errorf(
			"inspect media_analysis_model %q: %w",
			loadedConfig.MediaAnalysisModel,
			err,
		)
	}

	if apiKind != providerAPIKindGemini {
		return fmt.Errorf(
			"media_analysis_model %q must use a gemini provider: %w",
			loadedConfig.MediaAnalysisModel,
			os.ErrInvalid,
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

func (loadedConfig config) lockedModelForChannelIDs(channelIDs []string) (string, bool) {
	for _, channelID := range channelIDs {
		modelName, ok := loadedConfig.ChannelModelLocks[channelID]
		if ok {
			return modelName, true
		}
	}

	return "", false
}

func (provider providerConfig) apiKind() providerAPIKind {
	switch strings.ToLower(strings.TrimSpace(provider.Type)) {
	case "", string(providerAPIKindOpenAI):
		if looksLikeGeminiCompatibilityBaseURL(provider.BaseURL) {
			return providerAPIKindGemini
		}

		return providerAPIKindOpenAI
	case string(providerAPIKindOpenAICodex):
		return providerAPIKindOpenAICodex
	case string(providerAPIKindGemini):
		return providerAPIKindGemini
	default:
		return providerAPIKind(strings.ToLower(strings.TrimSpace(provider.Type)))
	}
}

func (provider providerConfig) usesOpenRouter() bool {
	if provider.apiKind() != providerAPIKindOpenAI {
		return false
	}

	parsedURL, err := url.Parse(strings.TrimSpace(provider.BaseURL))
	if err != nil {
		return false
	}

	host := strings.ToLower(strings.TrimSpace(parsedURL.Hostname()))

	return host == openRouterHost || strings.HasSuffix(host, "."+openRouterHost)
}

func (provider providerConfig) validate(providerName string) error {
	switch provider.apiKind() {
	case providerAPIKindOpenAI:
		if strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("provider %q is missing base_url: %w", providerName, os.ErrInvalid)
		}

		return nil
	case providerAPIKindOpenAICodex:
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
