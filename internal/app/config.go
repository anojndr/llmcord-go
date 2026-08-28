package app

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
	BaseURL              scalarString     `yaml:"base_url"`
	APIKey               scalarStringList `yaml:"api_key"`
	EnableGrounding      *bool            `yaml:"enable_grounding"`
	DisableSearchDecider *bool            `yaml:"disable_search_decider"`
	DontSendSystemPrompt *bool            `yaml:"dont_send_system_prompt"`
	AutoAppendSearchWeb  *bool            `yaml:"auto_append_search_web"`
	ExtraHeaders         map[string]any   `yaml:"extra_headers"`
	ExtraQuery           map[string]any   `yaml:"extra_query"`
	ExtraBody            map[string]any   `yaml:"extra_body"`
}

type rawTavilySearchConfig struct {
	APIKey scalarStringList `yaml:"api_key"`
}

type rawTinyFishSearchConfig struct {
	APIKey scalarStringList `yaml:"api_key"`
}

type rawFirecrawlSearchConfig struct {
	APIKey                scalarStringList `yaml:"api_key"`
	MaxMarkdownCharacters *int             `yaml:"max_markdown_characters"`
}

type rawExaSearchConfig struct {
	APIKey             scalarStringList `yaml:"api_key"`
	TextMaxCharacters  *int             `yaml:"text_max_characters"`
	LivecrawlTimeoutMS *int             `yaml:"livecrawl_timeout_ms"`
}

type rawSerpAPIVisualSearchConfig struct {
	APIKey scalarStringList `yaml:"api_key"`
}

type rawVisualSearchConfig struct {
	SerpAPI rawSerpAPIVisualSearchConfig `yaml:"serpapi"`
}

type rawWebSearchConfig struct {
	MaxURLs   *int                     `yaml:"max_urls"`
	Exa       rawExaSearchConfig       `yaml:"exa"`
	Tavily    rawTavilySearchConfig    `yaml:"tavily"`
	Firecrawl rawFirecrawlSearchConfig `yaml:"firecrawl"`
	TinyFish  rawTinyFishSearchConfig  `yaml:"tinyfish"`
}

type rawDatabaseConfig struct {
	ConnectionString scalarString `yaml:"connection_string"`
	StoreKey         scalarString `yaml:"store_key"`
}

type rawGistConfig struct {
	APIKey      scalarStringList `yaml:"api_key"`
	Endpoint    scalarString     `yaml:"endpoint"`
	Public      *bool            `yaml:"public"`
	Description scalarString     `yaml:"description"`
	Filename    scalarString     `yaml:"filename"`
}

type providerConfig struct {
	Name                 string
	BaseURL              string
	APIKey               string
	APIKeys              []string
	EnableGrounding      bool
	DisableSearchDecider bool
	DontSendSystemPrompt bool
	AutoAppendSearchWeb  bool
	ExtraHeaders         map[string]any
	ExtraQuery           map[string]any
	ExtraBody            map[string]any
}

type tavilySearchConfig struct {
	APIKey  string
	APIKeys []string
}

type tinyFishSearchConfig struct {
	APIKey  string
	APIKeys []string
}

type exaSearchConfig struct {
	APIKey             string
	APIKeys            []string
	SearchType         string
	TextMaxCharacters  int
	LivecrawlTimeoutMS int
}

type firecrawlSearchConfig struct {
	APIKey                string
	APIKeys               []string
	MaxMarkdownCharacters int
}

type serpAPIVisualSearchConfig struct {
	APIKey  string
	APIKeys []string
}

type visualSearchConfig struct {
	SerpAPI serpAPIVisualSearchConfig
}

type webSearchConfig struct {
	MaxURLs   int
	Exa       exaSearchConfig
	Tavily    tavilySearchConfig
	Firecrawl firecrawlSearchConfig
	TinyFish  tinyFishSearchConfig
}

type databaseConfig struct {
	ConnectionString string
	StoreKey         string
}

type gistConfig struct {
	APIKey      string
	APIKeys     []string
	Endpoint    string
	Description string
	Filename    string
	Public      bool
}

type providerAPIKind string

const (
	providerAPIKindOpenAI providerAPIKind = "openai"
	providerAPIKindGemini providerAPIKind = "gemini"
)

type rawConfig struct {
	BotToken                       scalarString                 `yaml:"bot_token"`
	ClientID                       scalarString                 `yaml:"client_id"`
	StatusMessage                  string                       `yaml:"status_message"`
	MaxImages                      *int                         `yaml:"max_images"`
	MaxMessages                    *int                         `yaml:"max_messages"`
	AllowDMs                       *bool                        `yaml:"allow_dms"`
	Permissions                    permissionsConfig            `yaml:"permissions"`
	Providers                      map[string]rawProviderConfig `yaml:"providers"`
	WebSearch                      rawWebSearchConfig           `yaml:"web_search"`
	VisualSearch                   rawVisualSearchConfig        `yaml:"visual_search"`
	Database                       rawDatabaseConfig            `yaml:"database"`
	Gist                           rawGistConfig                `yaml:"gist"`
	Models                         map[string]map[string]any    `yaml:"models"`
	ChannelModelLocks              map[string]scalarString      `yaml:"channel_model_locks"`
	ChannelSearchDeciderModelLocks map[string]scalarString      `yaml:"channel_search_decider_model_locks"`
	SearchDeciderModel             scalarString                 `yaml:"search_decider_model"`
	MediaAnalysisModel             scalarString                 `yaml:"media_analysis_model"`
	FallbackModel                  scalarString                 `yaml:"fallback_model"`
	SystemPrompt                   string                       `yaml:"system_prompt"`
}

type config struct {
	BotToken                       string
	ClientID                       string
	StatusMessage                  string
	MaxImages                      int
	MaxMessages                    int
	AllowDMs                       bool
	Permissions                    permissionsConfig
	Providers                      map[string]providerConfig
	WebSearch                      webSearchConfig
	VisualSearch                   visualSearchConfig
	Database                       databaseConfig
	Gist                           gistConfig
	Models                         map[string]map[string]any
	ModelOrder                     []string
	ChannelModelLocks              map[string]string
	ChannelSearchDeciderModelLocks map[string]string
	SearchDeciderModel             string
	MediaAnalysisModel             string
	FallbackModel                  string
	SystemPrompt                   string
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

	err = validateNoDeprecatedConfigSections(&rootNode)
	if err != nil {
		return config{}, fmt.Errorf("validate config %q: %w", filename, err)
	}

	loadedProviders := make(map[string]providerConfig, len(rawLoadedConfig.Providers))
	for providerName, rawProvider := range rawLoadedConfig.Providers {
		loadedProviders[providerName] = normalizeProviderConfig(providerName, rawProvider)
	}

	serpAPIVisualSearchKeys := normalizeAPIKeys([]string(rawLoadedConfig.VisualSearch.SerpAPI.APIKey))
	allowDMs := boolValueOrDefault(rawLoadedConfig.AllowDMs, true)
	searchDeciderModel := normalizedSearchDeciderModel(rawLoadedConfig.SearchDeciderModel, modelOrder)

	mediaAnalysisModel := strings.TrimSpace(string(rawLoadedConfig.MediaAnalysisModel))
	fallbackModel := normalizedFallbackModel(rawLoadedConfig.FallbackModel, rawLoadedConfig.Models)
	channelModelLocks := normalizeStringScalarMap(rawLoadedConfig.ChannelModelLocks)
	channelSearchDeciderModelLocks := normalizeStringScalarMap(rawLoadedConfig.ChannelSearchDeciderModelLocks)

	loadedConfig := config{
		BotToken:      string(rawLoadedConfig.BotToken),
		ClientID:      string(rawLoadedConfig.ClientID),
		StatusMessage: rawLoadedConfig.StatusMessage,
		MaxImages:     intValueOrDefault(rawLoadedConfig.MaxImages, defaultMaxImages),
		MaxMessages:   intValueOrDefault(rawLoadedConfig.MaxMessages, defaultMaxMessages),
		AllowDMs:      allowDMs,
		Permissions:   rawLoadedConfig.Permissions,
		Providers:     loadedProviders,
		WebSearch:     normalizeWebSearchConfig(rawLoadedConfig.WebSearch),
		VisualSearch: visualSearchConfig{
			SerpAPI: serpAPIVisualSearchConfig{
				APIKey:  firstAPIKey(serpAPIVisualSearchKeys),
				APIKeys: serpAPIVisualSearchKeys,
			},
		},
		Database:                       normalizeDatabaseConfig(rawLoadedConfig.Database),
		Gist:                           normalizeGistConfig(rawLoadedConfig.Gist),
		Models:                         rawLoadedConfig.Models,
		ModelOrder:                     modelOrder,
		ChannelModelLocks:              channelModelLocks,
		ChannelSearchDeciderModelLocks: channelSearchDeciderModelLocks,
		SearchDeciderModel:             searchDeciderModel,
		MediaAnalysisModel:             mediaAnalysisModel,
		FallbackModel:                  fallbackModel,
		SystemPrompt:                   rawLoadedConfig.SystemPrompt,
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

func validateNoDeprecatedConfigSections(rootNode *yaml.Node) error {
	hasFacebookSection, err := topLevelConfigFieldExists(rootNode, "facebook")
	if err != nil {
		return err
	}

	if hasFacebookSection {
		return fmt.Errorf(
			"facebook settings are no longer supported; remove the facebook section: %w",
			os.ErrInvalid,
		)
	}

	return nil
}

func topLevelConfigFieldExists(rootNode *yaml.Node, fieldName string) (bool, error) {
	if len(rootNode.Content) == 0 {
		return false, fmt.Errorf("decode document root: %w", os.ErrInvalid)
	}

	mappingNode := rootNode.Content[0]
	if mappingNode.Kind != yaml.MappingNode {
		return false, fmt.Errorf("decode mapping root: %w", os.ErrInvalid)
	}

	for index := 0; index < len(mappingNode.Content)-1; index += mappingNodePairSize {
		if mappingNode.Content[index].Value == fieldName {
			return true, nil
		}
	}

	return false, nil
}

func intValueOrDefault(value *int, fallback int) int {
	if value == nil {
		return fallback
	}

	return *value
}

func boolValueOrDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}

	return *value
}

func normalizedSearchDeciderModel(rawValue scalarString, modelOrder []string) string {
	searchDeciderModel := strings.TrimSpace(string(rawValue))
	if searchDeciderModel == "" && len(modelOrder) > 0 {
		return modelOrder[0]
	}

	return searchDeciderModel
}

const defaultStableModelFallback = "9router/stable_model:vision"

func normalizedFallbackModel(rawValue scalarString, models map[string]map[string]any) string {
	fallbackModel := strings.TrimSpace(string(rawValue))
	if fallbackModel != "" {
		return fallbackModel
	}

	if _, ok := models[defaultStableModelFallback]; ok {
		return defaultStableModelFallback
	}

	return ""
}

func normalizeProviderConfig(providerName string, rawProvider rawProviderConfig) providerConfig {
	apiKeys := normalizeAPIKeys([]string(rawProvider.APIKey))
	baseURL := strings.TrimSpace(string(rawProvider.BaseURL))

	return providerConfig{
		Name:                 strings.TrimSpace(providerName),
		BaseURL:              baseURL,
		APIKey:               firstAPIKey(apiKeys),
		APIKeys:              apiKeys,
		EnableGrounding:      boolValueOrDefault(rawProvider.EnableGrounding, false),
		DisableSearchDecider: boolValueOrDefault(rawProvider.DisableSearchDecider, false),
		DontSendSystemPrompt: boolValueOrDefault(rawProvider.DontSendSystemPrompt, false),
		AutoAppendSearchWeb:  boolValueOrDefault(rawProvider.AutoAppendSearchWeb, false),
		ExtraHeaders:         rawProvider.ExtraHeaders,
		ExtraQuery:           rawProvider.ExtraQuery,
		ExtraBody:            rawProvider.ExtraBody,
	}
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

func normalizeDatabaseConfig(rawLoadedConfig rawDatabaseConfig) databaseConfig {
	return databaseConfig{
		ConnectionString: strings.TrimSpace(string(rawLoadedConfig.ConnectionString)),
		StoreKey:         string(rawLoadedConfig.StoreKey),
	}
}

func normalizeGistConfig(rawLoadedConfig rawGistConfig) gistConfig {
	apiKeys := normalizeAPIKeys([]string(rawLoadedConfig.APIKey))

	endpoint := strings.TrimSpace(string(rawLoadedConfig.Endpoint))
	if endpoint == "" {
		endpoint = defaultGithubGistEndpoint
	}

	filename := strings.TrimSpace(string(rawLoadedConfig.Filename))
	if filename == "" {
		filename = defaultGistFilename
	}

	return gistConfig{
		APIKey:      firstAPIKey(apiKeys),
		APIKeys:     apiKeys,
		Endpoint:    endpoint,
		Description: strings.TrimSpace(string(rawLoadedConfig.Description)),
		Filename:    filename,
		Public:      boolValueOrDefault(rawLoadedConfig.Public, false),
	}
}

func normalizeWebSearchConfig(rawLoadedConfig rawWebSearchConfig) webSearchConfig {
	exaAPIKeys := normalizeAPIKeys([]string(rawLoadedConfig.Exa.APIKey))
	tavilyAPIKeys := normalizeAPIKeys([]string(rawLoadedConfig.Tavily.APIKey))
	firecrawlAPIKeys := normalizeAPIKeys([]string(rawLoadedConfig.Firecrawl.APIKey))
	tinyFishAPIKeys := normalizeAPIKeys([]string(rawLoadedConfig.TinyFish.APIKey))

	return webSearchConfig{
		MaxURLs: intValueOrDefault(rawLoadedConfig.MaxURLs, defaultWebSearchMaxURLs),
		Exa: exaSearchConfig{
			APIKey:             firstAPIKey(exaAPIKeys),
			APIKeys:            exaAPIKeys,
			SearchType:         defaultExaSearchType,
			TextMaxCharacters:  intValueOrDefault(rawLoadedConfig.Exa.TextMaxCharacters, defaultExaSearchTextMaxCharacters),
			LivecrawlTimeoutMS: intValueOrDefault(rawLoadedConfig.Exa.LivecrawlTimeoutMS, defaultExaContentsLivecrawlTimeoutMS),
		},
		Tavily: tavilySearchConfig{
			APIKey:  firstAPIKey(tavilyAPIKeys),
			APIKeys: tavilyAPIKeys,
		},
		Firecrawl: firecrawlSearchConfig{
			APIKey:  firstAPIKey(firecrawlAPIKeys),
			APIKeys: firecrawlAPIKeys,
			MaxMarkdownCharacters: intValueOrDefault(
				rawLoadedConfig.Firecrawl.MaxMarkdownCharacters,
				defaultFirecrawlMaxMarkdownCharacters,
			),
		},
		TinyFish: tinyFishSearchConfig{
			APIKey:  firstAPIKey(tinyFishAPIKeys),
			APIKeys: tinyFishAPIKeys,
		},
	}
}

func (loadedConfig webSearchConfig) maxURLs() int {
	if loadedConfig.MaxURLs <= 0 {
		return defaultWebSearchMaxURLs
	}

	return loadedConfig.MaxURLs
}

func (loadedConfig webSearchConfig) exaUsesAPI() bool {
	return len(loadedConfig.Exa.apiKeys()) > 0
}

func (settings exaSearchConfig) textMaxCharacters() int {
	if settings.TextMaxCharacters <= 0 {
		return defaultExaSearchTextMaxCharacters
	}

	return settings.TextMaxCharacters
}

func (settings exaSearchConfig) livecrawlTimeoutMS() int {
	if settings.LivecrawlTimeoutMS <= 0 {
		return defaultExaContentsLivecrawlTimeoutMS
	}

	return settings.LivecrawlTimeoutMS
}

func validateConfig(loadedConfig config) error {
	err := validateConfigBasics(loadedConfig)
	if err != nil {
		return err
	}

	err = validateWebSearchConfig(loadedConfig.WebSearch)
	if err != nil {
		return err
	}

	err = validateDatabaseConfig(loadedConfig.Database)
	if err != nil {
		return err
	}

	err = validateConfiguredModels(loadedConfig)
	if err != nil {
		return err
	}

	err = validateChannelModelLocks(loadedConfig)
	if err != nil {
		return err
	}

	err = validateChannelSearchDeciderModelLocks(loadedConfig)
	if err != nil {
		return err
	}

	err = validateMediaAnalysisModel(loadedConfig)
	if err != nil {
		return err
	}

	return validateFallbackModel(loadedConfig)
}

func validateConfigBasics(loadedConfig config) error {
	switch {
	case strings.TrimSpace(loadedConfig.BotToken) == "":
		return fmt.Errorf("bot_token is required: %w", os.ErrInvalid)
	case len(loadedConfig.ModelOrder) == 0:
		return fmt.Errorf("models must contain at least one entry: %w", os.ErrInvalid)
	case loadedConfig.MaxImages < 0:
		return fmt.Errorf("max_images must not be negative: %w", os.ErrInvalid)
	case loadedConfig.MaxMessages <= 0:
		return fmt.Errorf("max_messages must be greater than zero: %w", os.ErrInvalid)
	default:
		return nil
	}
}

func validateConfiguredModels(loadedConfig config) error {
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

func validateWebSearchConfig(loadedConfig webSearchConfig) error {
	if loadedConfig.MaxURLs <= 0 {
		return fmt.Errorf("web_search.max_urls must be greater than zero: %w", os.ErrInvalid)
	}

	if loadedConfig.Exa.TextMaxCharacters <= 0 {
		return fmt.Errorf(
			"web_search.exa.text_max_characters must be greater than zero: %w",
			os.ErrInvalid,
		)
	}

	if loadedConfig.Exa.LivecrawlTimeoutMS <= 0 {
		return fmt.Errorf(
			"web_search.exa.livecrawl_timeout_ms must be greater than zero: %w",
			os.ErrInvalid,
		)
	}

	if loadedConfig.Firecrawl.MaxMarkdownCharacters <= 0 {
		return fmt.Errorf(
			"web_search.firecrawl.max_markdown_characters must be greater than zero: %w",
			os.ErrInvalid,
		)
	}

	return nil
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

func validateChannelSearchDeciderModelLocks(loadedConfig config) error {
	for channelID, modelName := range loadedConfig.ChannelSearchDeciderModelLocks {
		if channelID == "" {
			return fmt.Errorf(
				"channel_search_decider_model_locks contains an empty channel id: %w",
				os.ErrInvalid,
			)
		}

		if !loadedConfig.hasModel(modelName) {
			return fmt.Errorf(
				"channel_search_decider_model_locks %q references undefined model %q: %w",
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
	trimmedStoreKey := strings.TrimSpace(loadedConfig.StoreKey)

	if loadedConfig.StoreKey != "" && trimmedStoreKey == "" {
		return fmt.Errorf("database.store_key must not be only whitespace: %w", os.ErrInvalid)
	}

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

func validateFallbackModel(loadedConfig config) error {
	if strings.TrimSpace(loadedConfig.FallbackModel) == "" {
		return nil
	}

	if !loadedConfig.hasModel(loadedConfig.FallbackModel) {
		return fmt.Errorf(
			"fallback_model %q is not defined in models: %w",
			loadedConfig.FallbackModel,
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

func (loadedConfig config) lockedModelForChannelIDs(channelIDs []string) (string, bool) {
	for _, channelID := range channelIDs {
		modelName, ok := loadedConfig.ChannelModelLocks[channelID]
		if ok {
			return modelName, true
		}
	}

	return "", false
}

func (loadedConfig config) lockedSearchDeciderModelForChannelIDs(channelIDs []string) (string, bool) {
	for _, channelID := range channelIDs {
		modelName, ok := loadedConfig.ChannelSearchDeciderModelLocks[channelID]
		if ok {
			return modelName, true
		}
	}

	return "", false
}

const (
	providerNameSuffixGemini = "gemini"
)

// apiKind infers the API kind from the provider name: names containing
// "gemini" use the native Gemini API. Everything else is treated as
// OpenAI-compatible, unless the base URL points at Gemini's OpenAI
// compatibility endpoint.
func (provider providerConfig) apiKind() providerAPIKind {
	providerName := strings.ToLower(strings.TrimSpace(provider.Name))

	switch {
	case strings.Contains(providerName, providerNameSuffixGemini):
		return providerAPIKindGemini
	default:
		if looksLikeGeminiCompatibilityBaseURL(provider.BaseURL) {
			return providerAPIKindGemini
		}

		return providerAPIKindOpenAI
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
	if provider.apiKind() != providerAPIKindOpenAI {
		return nil
	}

	if strings.TrimSpace(provider.BaseURL) == "" {
		return fmt.Errorf("provider %q is missing base_url: %w", providerName, os.ErrInvalid)
	}

	return nil
}

func looksLikeGeminiCompatibilityBaseURL(baseURL string) bool {
	return strings.Contains(
		strings.ToLower(strings.TrimSpace(baseURL)),
		"generativelanguage.googleapis.com",
	)
}
