// Package config loads and normalizes the bot YAML configuration.
package config

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

// ScopePermissions holds role or channel permission IDs.
type ScopePermissions struct {
	AllowedIDs idList `yaml:"allowed_ids"`
	BlockedIDs idList `yaml:"blocked_ids"`
}

// UserPermissions holds user-level permission IDs.
type UserPermissions struct {
	AdminIDs   idList `yaml:"admin_ids"`
	AllowedIDs idList `yaml:"allowed_ids"`
	BlockedIDs idList `yaml:"blocked_ids"`
}

// PermissionsConfig holds permission scopes.
type PermissionsConfig struct {
	Users    UserPermissions  `yaml:"users"`
	Roles    ScopePermissions `yaml:"roles"`
	Channels ScopePermissions `yaml:"channels"`
}

// RawProviderConfig is the raw YAML provider entry.
type RawProviderConfig struct {
	BaseURL              scalarString     `yaml:"base_url"`
	APIKey               scalarStringList `yaml:"api_key"`
	EnableGrounding      *bool            `yaml:"enable_grounding"`
	DisableSearchDecider *bool            `yaml:"disable_search_decider"`
	ExtraHeaders         map[string]any   `yaml:"extra_headers"`
	ExtraQuery           map[string]any   `yaml:"extra_query"`
	ExtraBody            map[string]any   `yaml:"extra_body"`
}

type rawTavilySearchConfig struct {
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
	PrimaryProvider scalarString             `yaml:"primary_provider"`
	MaxURLs         *int                     `yaml:"max_urls"`
	Exa             rawExaSearchConfig       `yaml:"exa"`
	Tavily          rawTavilySearchConfig    `yaml:"tavily"`
	Firecrawl       rawFirecrawlSearchConfig `yaml:"firecrawl"`
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

// ProviderConfig is one normalized provider entry.
type ProviderConfig struct {
	Name                 string
	BaseURL              string
	APIKey               string
	APIKeys              []string
	EnableGrounding      bool
	DisableSearchDecider bool
	ExtraHeaders         map[string]any
	ExtraQuery           map[string]any
	ExtraBody            map[string]any
}

// TavilySearchConfig holds Tavily search credentials.
type TavilySearchConfig struct {
	APIKey  string
	APIKeys []string
}

// ExaSearchConfig holds Exa search settings.
type ExaSearchConfig struct {
	APIKey             string
	APIKeys            []string
	SearchType         string
	TextMaxCharacters  int
	LivecrawlTimeoutMS int
}

// FirecrawlSearchConfig holds Firecrawl scrape credentials.
type FirecrawlSearchConfig struct {
	APIKey                string
	APIKeys               []string
	MaxMarkdownCharacters int
}

// SerpAPIVisualSearchConfig holds SerpAPI Google Lens credentials.
type SerpAPIVisualSearchConfig struct {
	APIKey  string
	APIKeys []string
}

// VisualSearchConfig holds visual search settings.
type VisualSearchConfig struct {
	SerpAPI SerpAPIVisualSearchConfig
}

// WebSearchProviderKind identifies a web search provider.
type WebSearchProviderKind string

const (
	// WebSearchProviderKindMCP is the Exa MCP provider.
	WebSearchProviderKindMCP WebSearchProviderKind = "mcp"
	// WebSearchProviderKindTavily is the Tavily provider.
	WebSearchProviderKindTavily WebSearchProviderKind = "tavily"
)

// WebSearchConfig holds normalized web search settings.
type WebSearchConfig struct {
	PrimaryProvider WebSearchProviderKind
	MaxURLs         int
	Exa             ExaSearchConfig
	Tavily          TavilySearchConfig
	Firecrawl       FirecrawlSearchConfig
}

// DatabaseConfig holds message-history persistence settings.
type DatabaseConfig struct {
	ConnectionString string
	StoreKey         string
}

// GistConfig holds GitHub gist upload settings.
type GistConfig struct {
	APIKey      string
	APIKeys     []string
	Endpoint    string
	Description string
	Filename    string
	Public      bool
}

// ProviderAPIKind identifies the provider wire protocol family.
type ProviderAPIKind string

const (
	// ProviderAPIKindOpenAI is the OpenAI-compatible family.
	ProviderAPIKindOpenAI ProviderAPIKind = "openai"
	// ProviderAPIKindGemini is the native Gemini family.
	ProviderAPIKindGemini ProviderAPIKind = "gemini"
)

// RawConfig is the unnormalized YAML configuration schema.
type RawConfig struct {
	BotToken           scalarString                 `yaml:"bot_token"`
	ClientID           scalarString                 `yaml:"client_id"`
	StatusMessage      string                       `yaml:"status_message"`
	MaxImages          *int                         `yaml:"max_images"`
	MaxMessages        *int                         `yaml:"max_messages"`
	AllowDMs           *bool                        `yaml:"allow_dms"`
	Permissions        PermissionsConfig            `yaml:"permissions"`
	Providers          map[string]RawProviderConfig `yaml:"providers"`
	WebSearch          rawWebSearchConfig           `yaml:"web_search"`
	VisualSearch       rawVisualSearchConfig        `yaml:"visual_search"`
	Database           rawDatabaseConfig            `yaml:"database"`
	Gist               rawGistConfig                `yaml:"gist"`
	Models             map[string]map[string]any    `yaml:"models"`
	ChannelModelLocks  map[string]scalarString      `yaml:"channel_model_locks"`
	SearchDeciderModel scalarString                 `yaml:"search_decider_model"`
	MediaAnalysisModel scalarString                 `yaml:"media_analysis_model"`
	SystemPrompt       string                       `yaml:"system_prompt"`
}

// Config is the normalized runtime configuration.
type Config struct {
	BotToken           string
	ClientID           string
	StatusMessage      string
	MaxImages          int
	MaxMessages        int
	AllowDMs           bool
	Permissions        PermissionsConfig
	Providers          map[string]ProviderConfig
	WebSearch          WebSearchConfig
	VisualSearch       VisualSearchConfig
	Database           DatabaseConfig
	Gist               GistConfig
	Models             map[string]map[string]any
	ModelOrder         []string
	ChannelModelLocks  map[string]string
	SearchDeciderModel string
	MediaAnalysisModel string
	SystemPrompt       string
}

func loadConfig(filename string) (Config, error) {
	configBytes, err := os.ReadFile(filepath.Clean(filename))
	if err != nil {
		return Config{}, fmt.Errorf("read Config %q: %w", filename, err)
	}

	var rawLoadedConfig RawConfig

	err = yaml.Unmarshal(configBytes, &rawLoadedConfig)
	if err != nil {
		return Config{}, fmt.Errorf("parse Config %q: %w", filename, err)
	}

	return buildLoadedConfig(filename, configBytes, rawLoadedConfig)
}

func buildLoadedConfig(
	filename string,
	configBytes []byte,
	rawLoadedConfig RawConfig,
) (Config, error) {
	var rootNode yaml.Node

	err := yaml.Unmarshal(configBytes, &rootNode)
	if err != nil {
		return Config{}, fmt.Errorf("parse Config node %q: %w", filename, err)
	}

	modelOrder, err := orderedMappingKeys(&rootNode, "models")
	if err != nil {
		return Config{}, fmt.Errorf("read model order from %q: %w", filename, err)
	}

	err = validateNoDeprecatedConfigSections(&rootNode)
	if err != nil {
		return Config{}, fmt.Errorf("validate Config %q: %w", filename, err)
	}

	loadedProviders := make(map[string]ProviderConfig, len(rawLoadedConfig.Providers))
	for providerName, rawProvider := range rawLoadedConfig.Providers {
		loadedProviders[providerName] = normalizeProviderConfig(providerName, rawProvider)
	}

	serpAPIVisualSearchKeys := normalizeAPIKeys([]string(rawLoadedConfig.VisualSearch.SerpAPI.APIKey))
	allowDMs := boolValueOrDefault(rawLoadedConfig.AllowDMs, true)
	searchDeciderModel := normalizedSearchDeciderModel(rawLoadedConfig.SearchDeciderModel, modelOrder)

	mediaAnalysisModel := strings.TrimSpace(string(rawLoadedConfig.MediaAnalysisModel))
	channelModelLocks := normalizeStringScalarMap(rawLoadedConfig.ChannelModelLocks)

	loadedConfig := Config{
		BotToken:      string(rawLoadedConfig.BotToken),
		ClientID:      string(rawLoadedConfig.ClientID),
		StatusMessage: rawLoadedConfig.StatusMessage,
		MaxImages:     intValueOrDefault(rawLoadedConfig.MaxImages, defaultMaxImages),
		MaxMessages:   intValueOrDefault(rawLoadedConfig.MaxMessages, defaultMaxMessages),
		AllowDMs:      allowDMs,
		Permissions:   rawLoadedConfig.Permissions,
		Providers:     loadedProviders,
		WebSearch:     normalizeWebSearchConfig(rawLoadedConfig.WebSearch),
		VisualSearch: VisualSearchConfig{
			SerpAPI: SerpAPIVisualSearchConfig{
				APIKey:  firstAPIKey(serpAPIVisualSearchKeys),
				APIKeys: serpAPIVisualSearchKeys,
			},
		},
		Database:           normalizeDatabaseConfig(rawLoadedConfig.Database),
		Gist:               normalizeGistConfig(rawLoadedConfig.Gist),
		Models:             rawLoadedConfig.Models,
		ModelOrder:         modelOrder,
		ChannelModelLocks:  channelModelLocks,
		SearchDeciderModel: searchDeciderModel,
		MediaAnalysisModel: mediaAnalysisModel,
		SystemPrompt:       rawLoadedConfig.SystemPrompt,
	}

	err = validateConfig(loadedConfig)
	if err != nil {
		return Config{}, fmt.Errorf("validate Config %q: %w", filename, err)
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

func normalizeProviderConfig(providerName string, rawProvider RawProviderConfig) ProviderConfig {
	apiKeys := normalizeAPIKeys([]string(rawProvider.APIKey))
	baseURL := strings.TrimSpace(string(rawProvider.BaseURL))

	if strings.Contains(strings.ToLower(providerName), providerNameSuffixExa) && baseURL == "" {
		baseURL = defaultExaResearchBaseURL
	}

	return ProviderConfig{
		Name:                 strings.TrimSpace(providerName),
		BaseURL:              baseURL,
		APIKey:               firstAPIKey(apiKeys),
		APIKeys:              apiKeys,
		EnableGrounding:      boolValueOrDefault(rawProvider.EnableGrounding, false),
		DisableSearchDecider: boolValueOrDefault(rawProvider.DisableSearchDecider, false),
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

func normalizeDatabaseConfig(rawLoadedConfig rawDatabaseConfig) DatabaseConfig {
	return DatabaseConfig{
		ConnectionString: strings.TrimSpace(string(rawLoadedConfig.ConnectionString)),
		StoreKey:         string(rawLoadedConfig.StoreKey),
	}
}

func normalizeGistConfig(rawLoadedConfig rawGistConfig) GistConfig {
	apiKeys := normalizeAPIKeys([]string(rawLoadedConfig.APIKey))

	endpoint := strings.TrimSpace(string(rawLoadedConfig.Endpoint))
	if endpoint == "" {
		endpoint = defaultGithubGistEndpoint
	}

	filename := strings.TrimSpace(string(rawLoadedConfig.Filename))
	if filename == "" {
		filename = defaultGistFilename
	}

	return GistConfig{
		APIKey:      firstAPIKey(apiKeys),
		APIKeys:     apiKeys,
		Endpoint:    endpoint,
		Description: strings.TrimSpace(string(rawLoadedConfig.Description)),
		Filename:    filename,
		Public:      boolValueOrDefault(rawLoadedConfig.Public, false),
	}
}

func normalizeWebSearchConfig(rawLoadedConfig rawWebSearchConfig) WebSearchConfig {
	exaAPIKeys := normalizeAPIKeys([]string(rawLoadedConfig.Exa.APIKey))
	tavilyAPIKeys := normalizeAPIKeys([]string(rawLoadedConfig.Tavily.APIKey))
	firecrawlAPIKeys := normalizeAPIKeys([]string(rawLoadedConfig.Firecrawl.APIKey))

	return WebSearchConfig{
		PrimaryProvider: normalizeWebSearchProvider(rawLoadedConfig.PrimaryProvider),
		MaxURLs:         intValueOrDefault(rawLoadedConfig.MaxURLs, defaultWebSearchMaxURLs),
		Exa: ExaSearchConfig{
			APIKey:             firstAPIKey(exaAPIKeys),
			APIKeys:            exaAPIKeys,
			SearchType:         defaultExaSearchType,
			TextMaxCharacters:  intValueOrDefault(rawLoadedConfig.Exa.TextMaxCharacters, defaultExaSearchTextMaxCharacters),
			LivecrawlTimeoutMS: intValueOrDefault(rawLoadedConfig.Exa.LivecrawlTimeoutMS, defaultExaContentsLivecrawlTimeoutMS),
		},
		Tavily: TavilySearchConfig{
			APIKey:  firstAPIKey(tavilyAPIKeys),
			APIKeys: tavilyAPIKeys,
		},
		Firecrawl: FirecrawlSearchConfig{
			APIKey:  firstAPIKey(firecrawlAPIKeys),
			APIKeys: firecrawlAPIKeys,
			MaxMarkdownCharacters: intValueOrDefault(
				rawLoadedConfig.Firecrawl.MaxMarkdownCharacters,
				defaultFirecrawlMaxMarkdownCharacters,
			),
		},
	}
}

func normalizeWebSearchProvider(rawValue scalarString) WebSearchProviderKind {
	trimmedValue := strings.ToLower(strings.TrimSpace(string(rawValue)))
	if trimmedValue == "" {
		return WebSearchProviderKindMCP
	}

	return WebSearchProviderKind(trimmedValue)
}

// MaxURLsOrDefault returns the web search URL cap with its default.
func (loadedConfig WebSearchConfig) MaxURLsOrDefault() int {
	if loadedConfig.MaxURLs <= 0 {
		return defaultWebSearchMaxURLs
	}

	return loadedConfig.MaxURLs
}

// ExaUsesAPI reports whether Exa API search is configured.
func (loadedConfig WebSearchConfig) ExaUsesAPI() bool {
	return len(loadedConfig.Exa.AllKeys()) > 0
}

// TextMaxCharactersOrDefault returns the text cap with its default.
func (settings ExaSearchConfig) TextMaxCharactersOrDefault() int {
	if settings.TextMaxCharacters <= 0 {
		return defaultExaSearchTextMaxCharacters
	}

	return settings.TextMaxCharacters
}

// LivecrawlTimeoutMSOrDefault returns the livecrawl timeout with its default.
func (settings ExaSearchConfig) LivecrawlTimeoutMSOrDefault() int {
	if settings.LivecrawlTimeoutMS <= 0 {
		return defaultExaContentsLivecrawlTimeoutMS
	}

	return settings.LivecrawlTimeoutMS
}

func validateConfig(loadedConfig Config) error {
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

	return validateMediaAnalysisModel(loadedConfig)
}

func validateConfigBasics(loadedConfig Config) error {
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

func validateConfiguredModels(loadedConfig Config) error {
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

func validateWebSearchConfig(loadedConfig WebSearchConfig) error {
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

	switch loadedConfig.PrimaryProvider {
	// Web search provider kinds.
	case WebSearchProviderKindMCP, WebSearchProviderKindTavily:
		return nil
	default:
		return fmt.Errorf(
			"web_search.primary_provider %q is unsupported: %w",
			loadedConfig.PrimaryProvider,
			os.ErrInvalid,
		)
	}
}

func validateChannelModelLocks(loadedConfig Config) error {
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

func validateDatabaseConfig(loadedConfig DatabaseConfig) error {
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

func validateMediaAnalysisModel(loadedConfig Config) error {
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

	apiKind, err := modelAPIKind(
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

	if apiKind != ProviderAPIKindGemini {
		return fmt.Errorf(
			"media_analysis_model %q must use a gemini provider: %w",
			loadedConfig.MediaAnalysisModel,
			os.ErrInvalid,
		)
	}

	return nil
}

// FirstModel returns the first configured model.
func (loadedConfig Config) FirstModel() string {
	if len(loadedConfig.ModelOrder) == 0 {
		return ""
	}

	return loadedConfig.ModelOrder[0]
}

// LockedModelForChannelIDs returns a channel-locked model if one applies.
func (loadedConfig Config) LockedModelForChannelIDs(channelIDs []string) (string, bool) {
	for _, channelID := range channelIDs {
		modelName, ok := loadedConfig.ChannelModelLocks[channelID]
		if ok {
			return modelName, true
		}
	}

	return "", false
}

const (
	providerNameSuffixGemini = "gemini"
	providerNameSuffixExa    = "exa"
)

// UsesOpenRouter reports whether the provider points at OpenRouter.
func (provider ProviderConfig) UsesOpenRouter() bool {
	if provider.apiKind() != ProviderAPIKindOpenAI {
		return false
	}

	parsedURL, err := url.Parse(strings.TrimSpace(provider.BaseURL))
	if err != nil {
		return false
	}

	host := strings.ToLower(strings.TrimSpace(parsedURL.Hostname()))

	return host == openRouterHost || strings.HasSuffix(host, "."+openRouterHost)
}

// apiKind infers the API kind from the provider name: names containing
// "gemini" use the native Gemini API, and "exa" is an OpenAI-compatible
// research provider. Everything else is treated as OpenAI-compatible, unless
// the base URL points at Gemini's OpenAI compatibility endpoint.
func (provider ProviderConfig) apiKind() ProviderAPIKind {
	providerName := strings.ToLower(strings.TrimSpace(provider.Name))

	switch {
	case strings.Contains(providerName, providerNameSuffixGemini):
		return ProviderAPIKindGemini
	case strings.Contains(providerName, providerNameSuffixExa):
		return ProviderAPIKindOpenAI
	default:
		if looksLikeGeminiCompatibilityBaseURL(provider.BaseURL) {
			return ProviderAPIKindGemini
		}

		return ProviderAPIKindOpenAI
	}
}

func (provider ProviderConfig) validate(providerName string) error {
	// Provider API kinds.
	if provider.apiKind() != ProviderAPIKindOpenAI {
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

// splitConfiguredModel splits a "provider/model" slash model into its
// provider and model halves. Local resolution suffixes are trimmed first.
func splitConfiguredModel(configuredModel string) (string, string, error) {
	trimmedModel := strings.TrimSuffix(configuredModel, ":vision")

	parts := strings.SplitN(trimmedModel, "/", configuredModelParts)
	if len(parts) != configuredModelParts {
		return "", "", fmt.Errorf(
			"split configured model %q: %w",
			configuredModel,
			os.ErrInvalid,
		)
	}

	return parts[0], parts[1], nil
}

const configuredModelParts = 2

// modelAPIKind resolves the API kind for a configured model.
func modelAPIKind(loadedConfig Config, providerSlashModel string) (ProviderAPIKind, error) {
	providerName, _, err := splitConfiguredModel(providerSlashModel)
	if err != nil {
		return "", err
	}

	provider, ok := loadedConfig.Providers[providerName]
	if !ok {
		return "", fmt.Errorf(
			"find provider %q: %w",
			providerName,
			os.ErrNotExist,
		)
	}

	return provider.apiKind(), nil
}

// ProviderModel resolves the provider for a configured model.
func ProviderModel(loadedConfig Config, providerSlashModel string) (ProviderConfig, error) {
	providerName, _, err := splitConfiguredModel(providerSlashModel)
	if err != nil {
		return ProviderConfig{}, fmt.Errorf(
			"parse configured model %q: %w",
			providerSlashModel,
			err,
		)
	}

	provider, ok := loadedConfig.Providers[providerName]
	if !ok {
		return ProviderConfig{}, fmt.Errorf(
			"find provider %q: %w",
			providerName,
			os.ErrNotExist,
		)
	}

	return provider, nil
}

// ResolveProviderModelAPIKind resolves the provider API kind and normalized
// model name for a configured "provider/model" slash model, trimming local
// resolution suffixes (":vision") from the model half.
func ResolveProviderModelAPIKind(
	loadedConfig Config,
	providerSlashModel string,
) (ProviderAPIKind, string, error) {
	provider, modelName, err := splitConfiguredModel(providerSlashModel)
	if err != nil {
		return "", "", err
	}

	ProviderConfig, ok := loadedConfig.Providers[provider]
	if !ok {
		return "", "", fmt.Errorf(
			"find provider %q: %w",
			provider,
			os.ErrNotExist,
		)
	}

	return ProviderConfig.apiKind(), modelName, nil
}

// ResolveModelAPIKind is like ResolveProviderModelAPIKind for callers that
// only need the normalized model name.
func ResolveModelAPIKind(
	loadedConfig Config,
	providerSlashModel string,
) (ProviderAPIKind, string, error) {
	return ResolveProviderModelAPIKind(loadedConfig, providerSlashModel)
}

func (loadedConfig Config) hasModel(modelName string) bool {
	_, ok := loadedConfig.Models[modelName]

	return ok
}
