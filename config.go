package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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

type rawExaSearchConfig struct {
	APIKey            scalarStringList `yaml:"api_key"`
	TextMaxCharacters *int             `yaml:"text_max_characters"`
}

type rawSerpAPIVisualSearchConfig struct {
	APIKey scalarStringList `yaml:"api_key"`
}

type rawVisualSearchConfig struct {
	SerpAPI rawSerpAPIVisualSearchConfig `yaml:"serpapi"`
}

type rawFacebookConfig struct {
	PrimaryProvider  scalarString `yaml:"primary_provider"`
	FallbackProvider scalarString `yaml:"fallback_provider"`
}

type rawWebSearchConfig struct {
	PrimaryProvider scalarString          `yaml:"primary_provider"`
	MaxURLs         *int                  `yaml:"max_urls"`
	Exa             rawExaSearchConfig    `yaml:"exa"`
	Tavily          rawTavilySearchConfig `yaml:"tavily"`
}

type rawDatabaseConfig struct {
	ConnectionString scalarString `yaml:"connection_string"`
	StoreKey         scalarString `yaml:"store_key"`
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

type exaSearchConfig struct {
	APIKey            string
	APIKeys           []string
	SearchType        string
	TextMaxCharacters int
}

type serpAPIVisualSearchConfig struct {
	APIKey  string
	APIKeys []string
}

type visualSearchConfig struct {
	SerpAPI serpAPIVisualSearchConfig
}

type facebookExtractorProviderKind string

const (
	facebookExtractorProviderKindFDownloader facebookExtractorProviderKind = "fdownloader"
	facebookExtractorProviderKindGetMyFB     facebookExtractorProviderKind = "getmyfb"
	facebookExtractorProviderCount                                         = 2
)

type facebookExtractorConfig struct {
	PrimaryProvider  facebookExtractorProviderKind
	FallbackProvider facebookExtractorProviderKind
}

type webSearchProviderKind string

const (
	webSearchProviderKindMCP    webSearchProviderKind = "mcp"
	webSearchProviderKindTavily webSearchProviderKind = "tavily"
)

type webSearchConfig struct {
	PrimaryProvider webSearchProviderKind
	MaxURLs         int
	Exa             exaSearchConfig
	Tavily          tavilySearchConfig
}

type databaseConfig struct {
	ConnectionString string
	StoreKey         string
}

type providerAPIKind string

const (
	providerAPIKindOpenAI                     providerAPIKind = "openai"
	providerAPIKindOpenAICodex                providerAPIKind = "openai-codex"
	providerAPIKindGemini                     providerAPIKind = "gemini"
	providerTypeExa                                           = "exa"
	modelConfigContextWindowKey                               = "context_window"
	modelConfigAutoCompactThresholdPercentKey                 = "auto_compact_threshold_percent"
)

type rawConfig struct {
	BotToken                    scalarString                 `yaml:"bot_token"`
	ClientID                    scalarString                 `yaml:"client_id"`
	StatusMessage               string                       `yaml:"status_message"`
	MaxText                     *int                         `yaml:"max_text"`
	MaxImages                   *int                         `yaml:"max_images"`
	MaxMessages                 *int                         `yaml:"max_messages"`
	UsePlainResponses           bool                         `yaml:"use_plain_responses"`
	AllowDMs                    *bool                        `yaml:"allow_dms"`
	Permissions                 permissionsConfig            `yaml:"permissions"`
	Providers                   map[string]rawProviderConfig `yaml:"providers"`
	Facebook                    rawFacebookConfig            `yaml:"facebook"`
	WebSearch                   rawWebSearchConfig           `yaml:"web_search"`
	VisualSearch                rawVisualSearchConfig        `yaml:"visual_search"`
	Database                    rawDatabaseConfig            `yaml:"database"`
	AutoCompactThresholdPercent *int                         `yaml:"auto_compact_threshold_percent"`
	Models                      map[string]map[string]any    `yaml:"models"`
	ChannelModelLocks           map[string]scalarString      `yaml:"channel_model_locks"`
	SearchDeciderModel          scalarString                 `yaml:"search_decider_model"`
	MediaAnalysisModel          scalarString                 `yaml:"media_analysis_model"`
	SystemPrompt                string                       `yaml:"system_prompt"`
}

type config struct {
	BotToken                    string
	ClientID                    string
	StatusMessage               string
	MaxText                     int
	MaxImages                   int
	MaxMessages                 int
	UsePlainResponses           bool
	AllowDMs                    bool
	Permissions                 permissionsConfig
	Providers                   map[string]providerConfig
	Facebook                    facebookExtractorConfig
	WebSearch                   webSearchConfig
	VisualSearch                visualSearchConfig
	Database                    databaseConfig
	AutoCompactThresholdPercent int
	Models                      map[string]map[string]any
	ModelContextWindows         map[string]int
	ModelOrder                  []string
	ChannelModelLocks           map[string]string
	SearchDeciderModel          string
	MediaAnalysisModel          string
	SystemPrompt                string
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
		loadedProviders[providerName] = normalizeProviderConfig(rawProvider)
	}

	serpAPIVisualSearchKeys := normalizeAPIKeys([]string(rawLoadedConfig.VisualSearch.SerpAPI.APIKey))

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

	modelContextWindows, err := effectiveModelContextWindows(loadedProviders, rawLoadedConfig.Models)
	if err != nil {
		return config{}, fmt.Errorf("resolve model context windows from %q: %w", filename, err)
	}

	err = validateNoModelLocalAutoCompactThreshold(rawLoadedConfig.Models)
	if err != nil {
		return config{}, fmt.Errorf(
			"validate model settings from %q: %w",
			filename,
			err,
		)
	}

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
		Facebook:          normalizeFacebookConfig(rawLoadedConfig.Facebook),
		WebSearch:         normalizeWebSearchConfig(rawLoadedConfig.WebSearch),
		VisualSearch: visualSearchConfig{
			SerpAPI: serpAPIVisualSearchConfig{
				APIKey:  firstAPIKey(serpAPIVisualSearchKeys),
				APIKeys: serpAPIVisualSearchKeys,
			},
		},
		Database: normalizeDatabaseConfig(rawLoadedConfig.Database),
		AutoCompactThresholdPercent: intValueOrDefault(
			rawLoadedConfig.AutoCompactThresholdPercent,
			autoCompactDefaultThresholdPercent,
		),
		Models:              rawLoadedConfig.Models,
		ModelContextWindows: modelContextWindows,
		ModelOrder:          modelOrder,
		ChannelModelLocks:   channelModelLocks,
		SearchDeciderModel:  searchDeciderModel,
		MediaAnalysisModel:  mediaAnalysisModel,
		SystemPrompt:        rawLoadedConfig.SystemPrompt,
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

func normalizeProviderConfig(rawProvider rawProviderConfig) providerConfig {
	apiKeys := normalizeAPIKeys([]string(rawProvider.APIKey))
	providerType := strings.TrimSpace(string(rawProvider.Type))
	baseURL := strings.TrimSpace(string(rawProvider.BaseURL))

	if strings.EqualFold(providerType, providerTypeExa) && baseURL == "" {
		baseURL = defaultExaResearchBaseURL
	}

	return providerConfig{
		Type:         providerType,
		BaseURL:      baseURL,
		APIKey:       firstAPIKey(apiKeys),
		APIKeys:      apiKeys,
		ExtraHeaders: rawProvider.ExtraHeaders,
		ExtraQuery:   rawProvider.ExtraQuery,
		ExtraBody:    rawProvider.ExtraBody,
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

type modelIntSetting struct {
	GroupKey string
	Value    int
	Explicit bool
}

type modelIntSettingGroups struct {
	Values  map[string]int
	Sources map[string]string
}

const errPositiveIntMustBeGreaterThanZero = "must be greater than zero: %w"

func effectiveModelContextWindows(
	providers map[string]providerConfig,
	models map[string]map[string]any,
) (map[string]int, error) {
	return effectiveAliasedModelIntSettings(
		providers,
		models,
		modelConfigContextWindowKey,
		nil,
	)
}

func effectiveAliasedModelIntSettings(
	providers map[string]providerConfig,
	models map[string]map[string]any,
	settingKey string,
	validate func(int) error,
) (map[string]int, error) {
	if len(models) == 0 {
		return map[string]int{}, nil
	}

	settings := make(map[string]modelIntSetting, len(models))
	groups := modelIntSettingGroups{
		Values:  make(map[string]int),
		Sources: make(map[string]string),
	}

	for configuredModel, modelParameters := range models {
		setting, err := readAliasedModelIntSetting(
			providers,
			configuredModel,
			modelParameters,
			settingKey,
			validate,
		)
		if err != nil {
			return nil, err
		}

		settings[configuredModel] = setting

		err = groups.register(configuredModel, setting, settingKey)
		if err != nil {
			return nil, err
		}
	}

	if len(groups.Values) == 0 {
		return map[string]int{}, nil
	}

	effectiveValues := effectiveAliasedModelIntSettingValues(settings, groups.Values)

	if len(effectiveValues) == 0 {
		return map[string]int{}, nil
	}

	return effectiveValues, nil
}

func readAliasedModelIntSetting(
	providers map[string]providerConfig,
	configuredModel string,
	modelParameters map[string]any,
	settingKey string,
	validate func(int) error,
) (modelIntSetting, error) {
	groupKey, err := aliasedModelIntSettingGroupKey(providers, configuredModel)
	if err != nil {
		return modelIntSetting{}, err
	}

	value, explicit, err := modelPositiveIntSettingValue(modelParameters, settingKey)
	if err != nil {
		return modelIntSetting{}, fmt.Errorf("read %s for model %q: %w", settingKey, configuredModel, err)
	}

	err = validateAliasedModelIntSetting(value, explicit, validate, settingKey, configuredModel)
	if err != nil {
		return modelIntSetting{}, err
	}

	return modelIntSetting{
		GroupKey: groupKey,
		Value:    value,
		Explicit: explicit,
	}, nil
}

func aliasedModelIntSettingGroupKey(
	providers map[string]providerConfig,
	configuredModel string,
) (string, error) {
	providerName, modelName, err := splitConfiguredModel(configuredModel)
	if err != nil {
		return "", fmt.Errorf("parse model %q: %w", configuredModel, err)
	}

	baseModelName := modelName
	if provider, ok := providers[providerName]; ok {
		baseModelName, err = modelLocalSettingBaseModel(provider, modelName)
		if err != nil {
			return "", fmt.Errorf("normalize base model for %q: %w", configuredModel, err)
		}
	}

	return providerName + "/" + baseModelName, nil
}

func validateAliasedModelIntSetting(
	value int,
	explicit bool,
	validate func(int) error,
	settingKey string,
	configuredModel string,
) error {
	if !explicit || validate == nil {
		return nil
	}

	err := validate(value)
	if err != nil {
		return fmt.Errorf("validate %s for model %q: %w", settingKey, configuredModel, err)
	}

	return nil
}

func (groups modelIntSettingGroups) register(
	configuredModel string,
	setting modelIntSetting,
	settingKey string,
) error {
	if !setting.Explicit {
		return nil
	}

	if previousValue, ok := groups.Values[setting.GroupKey]; ok && previousValue != setting.Value {
		return fmt.Errorf(
			"models %q and %q must share the same %s because they resolve to base model %q: %w",
			groups.Sources[setting.GroupKey],
			configuredModel,
			settingKey,
			setting.GroupKey,
			os.ErrInvalid,
		)
	}

	groups.Values[setting.GroupKey] = setting.Value
	groups.Sources[setting.GroupKey] = configuredModel

	return nil
}

func effectiveAliasedModelIntSettingValues(
	settings map[string]modelIntSetting,
	groupValues map[string]int,
) map[string]int {
	effectiveValues := make(map[string]int, len(settings))
	for configuredModel, setting := range settings {
		if setting.Explicit {
			effectiveValues[configuredModel] = setting.Value

			continue
		}

		value, ok := groupValues[setting.GroupKey]
		if ok {
			effectiveValues[configuredModel] = value
		}
	}

	return effectiveValues
}

func modelLocalSettingBaseModel(provider providerConfig, modelName string) (string, error) {
	switch provider.apiKind() {
	case providerAPIKindOpenAI:
		return modelName, nil
	case providerAPIKindGemini:
		baseModelName, _, err := normalizeGeminiModelAlias(modelName, nil)
		if err != nil {
			return "", err
		}

		return baseModelName, nil
	case providerAPIKindOpenAICodex:
		baseModelName, _ := normalizeOpenAICodexModelAlias(modelName, nil)

		return baseModelName, nil
	default:
		return modelName, nil
	}
}

func modelPositiveIntSettingValue(
	modelParameters map[string]any,
	settingKey string,
) (int, bool, error) {
	if len(modelParameters) == 0 {
		return 0, false, nil
	}

	rawValue, ok := modelParameters[settingKey]
	if !ok {
		return 0, false, nil
	}

	value, err := anyPositiveIntValue(rawValue)
	if err != nil {
		return 0, false, err
	}

	return value, true, nil
}

func validateNoModelLocalAutoCompactThreshold(models map[string]map[string]any) error {
	for configuredModel, modelParameters := range models {
		if len(modelParameters) == 0 {
			continue
		}

		if _, ok := modelParameters[modelConfigAutoCompactThresholdPercentKey]; !ok {
			continue
		}

		return fmt.Errorf(
			"model %q must not define %s; use top-level %s instead: %w",
			configuredModel,
			modelConfigAutoCompactThresholdPercentKey,
			modelConfigAutoCompactThresholdPercentKey,
			os.ErrInvalid,
		)
	}

	return nil
}

func validateAutoCompactThresholdPercent(value int) error {
	if value <= 0 {
		return fmt.Errorf(errPositiveIntMustBeGreaterThanZero, os.ErrInvalid)
	}

	if value > autoCompactPercentBase {
		return fmt.Errorf(
			"must be less than or equal to %d: %w",
			autoCompactPercentBase,
			os.ErrInvalid,
		)
	}

	return nil
}

func anyPositiveIntValue(value any) (int, error) {
	maxIntValue := int(^uint(0) >> 1)

	switch typedValue := value.(type) {
	case int:
		if typedValue <= 0 {
			return 0, fmt.Errorf(errPositiveIntMustBeGreaterThanZero, os.ErrInvalid)
		}

		return typedValue, nil
	case int64:
		if typedValue <= 0 || typedValue > int64(maxIntValue) {
			return 0, fmt.Errorf(errPositiveIntMustBeGreaterThanZero, os.ErrInvalid)
		}

		return int(typedValue), nil
	case uint64:
		if typedValue == 0 || typedValue > uint64(maxIntValue) {
			return 0, fmt.Errorf(errPositiveIntMustBeGreaterThanZero, os.ErrInvalid)
		}

		parsedValue, err := strconv.Atoi(strconv.FormatUint(typedValue, 10))
		if err != nil {
			return 0, fmt.Errorf("parse positive integer %d: %w", typedValue, err)
		}

		return parsedValue, nil
	case float64:
		if typedValue <= 0 || typedValue != float64(int(typedValue)) {
			return 0, fmt.Errorf("must be a positive integer: %w", os.ErrInvalid)
		}

		return int(typedValue), nil
	default:
		return 0, fmt.Errorf("must be a positive integer, got %T: %w", value, os.ErrInvalid)
	}
}

func normalizeDatabaseConfig(rawLoadedConfig rawDatabaseConfig) databaseConfig {
	return databaseConfig{
		ConnectionString: strings.TrimSpace(string(rawLoadedConfig.ConnectionString)),
		StoreKey:         string(rawLoadedConfig.StoreKey),
	}
}

func normalizeFacebookConfig(rawLoadedConfig rawFacebookConfig) facebookExtractorConfig {
	primaryProvider := normalizeFacebookExtractorProvider(rawLoadedConfig.PrimaryProvider)
	if primaryProvider == "" {
		primaryProvider = facebookExtractorProviderKindFDownloader
	}

	fallbackProvider := normalizeFacebookExtractorProvider(rawLoadedConfig.FallbackProvider)
	if fallbackProvider == "" {
		fallbackProvider = defaultFacebookFallbackProvider(primaryProvider)
	}

	return facebookExtractorConfig{
		PrimaryProvider:  primaryProvider,
		FallbackProvider: fallbackProvider,
	}
}

func normalizeFacebookExtractorProvider(rawValue scalarString) facebookExtractorProviderKind {
	return facebookExtractorProviderKind(strings.ToLower(strings.TrimSpace(string(rawValue))))
}

func defaultFacebookFallbackProvider(
	primaryProvider facebookExtractorProviderKind,
) facebookExtractorProviderKind {
	switch primaryProvider {
	case facebookExtractorProviderKindFDownloader:
		return facebookExtractorProviderKindGetMyFB
	case facebookExtractorProviderKindGetMyFB:
		return facebookExtractorProviderKindFDownloader
	default:
		return facebookExtractorProviderKindGetMyFB
	}
}

func normalizeWebSearchConfig(rawLoadedConfig rawWebSearchConfig) webSearchConfig {
	exaAPIKeys := normalizeAPIKeys([]string(rawLoadedConfig.Exa.APIKey))
	tavilyAPIKeys := normalizeAPIKeys([]string(rawLoadedConfig.Tavily.APIKey))

	return webSearchConfig{
		PrimaryProvider: normalizeWebSearchProvider(rawLoadedConfig.PrimaryProvider),
		MaxURLs:         intValueOrDefault(rawLoadedConfig.MaxURLs, defaultWebSearchMaxURLs),
		Exa: exaSearchConfig{
			APIKey:            firstAPIKey(exaAPIKeys),
			APIKeys:           exaAPIKeys,
			SearchType:        defaultExaSearchType,
			TextMaxCharacters: intValueOrDefault(rawLoadedConfig.Exa.TextMaxCharacters, defaultExaSearchTextMaxCharacters),
		},
		Tavily: tavilySearchConfig{
			APIKey:  firstAPIKey(tavilyAPIKeys),
			APIKeys: tavilyAPIKeys,
		},
	}
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

func (loadedConfig webSearchConfig) exaUsesAPI() bool {
	return len(loadedConfig.Exa.apiKeysForAttempts()) > 0
}

func (settings exaSearchConfig) textMaxCharacters() int {
	if settings.TextMaxCharacters <= 0 {
		return defaultExaSearchTextMaxCharacters
	}

	return settings.TextMaxCharacters
}

func (loadedConfig facebookExtractorConfig) providersInOrder() []facebookExtractorProviderKind {
	providers := make([]facebookExtractorProviderKind, 0, facebookExtractorProviderCount)
	providers = append(providers, loadedConfig.PrimaryProvider)

	if loadedConfig.FallbackProvider != loadedConfig.PrimaryProvider {
		providers = append(providers, loadedConfig.FallbackProvider)
	}

	return providers
}

func validateConfig(loadedConfig config) error {
	err := validateConfigBasics(loadedConfig)
	if err != nil {
		return err
	}

	err = validateFacebookConfig(loadedConfig.Facebook)
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

	err = validateAutoCompactThresholdPercent(loadedConfig.AutoCompactThresholdPercent)
	if err != nil {
		return fmt.Errorf("auto_compact_threshold_percent %w", err)
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

func validateConfigBasics(loadedConfig config) error {
	switch {
	case strings.TrimSpace(loadedConfig.BotToken) == "":
		return fmt.Errorf("bot_token is required: %w", os.ErrInvalid)
	case len(loadedConfig.ModelOrder) == 0:
		return fmt.Errorf("models must contain at least one entry: %w", os.ErrInvalid)
	case loadedConfig.MaxText <= 0:
		return fmt.Errorf("max_text must be greater than zero: %w", os.ErrInvalid)
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

func validateFacebookConfig(loadedConfig facebookExtractorConfig) error {
	switch loadedConfig.PrimaryProvider {
	case facebookExtractorProviderKindFDownloader, facebookExtractorProviderKindGetMyFB:
	default:
		return fmt.Errorf(
			"facebook.primary_provider %q is unsupported: %w",
			loadedConfig.PrimaryProvider,
			os.ErrInvalid,
		)
	}

	switch loadedConfig.FallbackProvider {
	case facebookExtractorProviderKindFDownloader, facebookExtractorProviderKindGetMyFB:
	default:
		return fmt.Errorf(
			"facebook.fallback_provider %q is unsupported: %w",
			loadedConfig.FallbackProvider,
			os.ErrInvalid,
		)
	}

	if loadedConfig.PrimaryProvider == loadedConfig.FallbackProvider {
		return fmt.Errorf(
			"facebook.primary_provider and facebook.fallback_provider must differ: %w",
			os.ErrInvalid,
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

func (loadedConfig config) modelContextWindow(modelName string) int {
	if len(loadedConfig.ModelContextWindows) == 0 {
		return 0
	}

	return loadedConfig.ModelContextWindows[modelName]
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
	case providerTypeExa:
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
