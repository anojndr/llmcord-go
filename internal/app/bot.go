package app

import (
	"context"
	"fmt"
	"io"
	providers "llmcord-go/internal/providers"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

type bot struct {
	configPath                   string
	session                      *discordgo.Session
	httpClient                   *http.Client
	chatCompletions              chatCompletionStreamer
	webSearch                    webSearcher
	visualSearch                 visualSearcher
	serpAPIVisualSearch          serpAPIVisualSearcher
	gist                         gistCreator
	tiktok                       tiktokFetcher
	facebook                     facebookFetcher
	youtubeShorts                youtubeShortsFetcher
	youtube                      youtubeFetcher
	reddit                       redditFetcher
	website                      websiteFetcher
	nodes                        *messageNodeStore
	currentModel                 string
	currentExaSearchTypeValue    string
	currentSearchDeciderModel    string
	currentGroundingEnabledValue *bool
	decidingSearch               bool
	modelMu                      sync.RWMutex
	editMu                       sync.Mutex
	nextEditAtByMessage          map[string]time.Time
	messageDedupMu               sync.Mutex
	messageProcessedAt           map[string]time.Time
	startupMu                    sync.Mutex
	discordReady                 bool
	sessionConfigured            bool
	onlineAnnounced              bool
	onlineOutput                 io.Writer
}

func newOptimizedHTTPTransport() *http.Transport {
	dialer := new(net.Dialer)
	dialer.Timeout = optimizedHTTPDialTimeout
	dialer.KeepAlive = optimizedHTTPDialKeepAlive

	transport := new(http.Transport)
	transport.Proxy = http.ProxyFromEnvironment
	transport.DialContext = dialer.DialContext
	transport.ForceAttemptHTTP2 = true
	transport.MaxIdleConns = optimizedHTTPMaxIdleConns
	transport.MaxIdleConnsPerHost = optimizedHTTPMaxIdleConnsPerHost
	transport.IdleConnTimeout = optimizedHTTPIdleConnTimeout
	transport.TLSHandshakeTimeout = optimizedHTTPTLSHandshakeTimeout
	transport.ExpectContinueTimeout = optimizedHTTPExpectContinueTimeout

	return transport
}

func newOptimizedHTTPClient() *http.Client {
	client := new(http.Client)
	client.Transport = newOptimizedHTTPTransport()

	return client
}

func newBot(ctx context.Context, configPath string, loadedConfig config) (*bot, error) {
	discordSession, err := discordgo.New("Bot " + loadedConfig.BotToken)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}

	if discordSession.Client == nil {
		discordSession.Client = &http.Client{
			Transport:     nil,
			CheckRedirect: nil,
			Jar:           nil,
			Timeout:       discordClientTimeout,
		}
	}

	httpClient := newOptimizedHTTPClient()

	instance := new(bot)
	instance.configPath = configPath
	instance.session = discordSession
	instance.httpClient = httpClient
	instance.chatCompletions = providers.NewChatCompletionRouter(httpClient)
	instance.webSearch = newWebSearchClient(httpClient)
	instance.visualSearch = newVisualSearchClient(httpClient)
	instance.serpAPIVisualSearch = newSerpAPIVisualSearchClient(httpClient)
	instance.gist = newGistClient(
		httpClient,
		loadedConfig.Gist.Endpoint,
		loadedConfig.Gist.APIKeys,
		loadedConfig.Gist.Description,
		loadedConfig.Gist.Filename,
		loadedConfig.Gist.Public,
	)
	instance.tiktok = newTikTokClient(httpClient)

	instance.facebook = newFacebookClient(httpClient)
	instance.youtubeShorts = newYouTubeShortsClient(httpClient)
	instance.youtube = newYouTubeClient(httpClient)
	instance.reddit = newRedditClient(httpClient)
	instance.website = newWebsiteClient(httpClient)
	instance.nodes = newMessageNodeStore(maxMessageNodes)

	store, err := newConfiguredMessageNodeStore(
		ctx,
		maxMessageNodes,
		configPath,
		loadedConfig.Database.StoreKey,
		loadedConfig.Database.ConnectionString,
	)
	if err != nil {
		logWarn("configure persisted message history", err)
	} else {
		instance.nodes = store
	}

	instance.currentModel = loadedConfig.firstModel()
	instance.currentExaSearchTypeValue = defaultExaSearchType
	instance.currentSearchDeciderModel = loadedConfig.SearchDeciderModel
	instance.onlineOutput = os.Stdout

	discordSession.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent
	discordSession.AddHandler(recoverHandler(instance.handleReady))
	discordSession.AddHandler(recoverHandler(instance.handleInteractionCreate))
	discordSession.AddHandler(recoverHandler(instance.handleMessageCreate))

	return instance, nil
}

// Run starts the bot until the context is cancelled.
func Run(ctx context.Context, configPath string) error {
	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load startup config: %w", err)
	}

	instance, err := newBot(ctx, configPath, loadedConfig)
	if err != nil {
		return fmt.Errorf("create bot: %w", err)
	}

	publicHTTPAddr := publicHTTPAddress(os.Getenv)

	publicHTTPServer, publicHTTPServerErrCh, err := startPublicHTTPServer(
		ctx,
		publicHTTPAddr,
		instance.serviceHealth,
	)
	if err != nil {
		return fmt.Errorf("start public http server: %w", err)
	}

	if publicHTTPServer != nil {
		defer func() {
			shutdownErr := shutdownPublicHTTPServer(ctx, publicHTTPServer)
			if shutdownErr != nil {
				logWarn("shutdown public http server", shutdownErr)
			}
		}()

		slog.Info(
			"public http server listening",
			"address",
			publicHTTPAddr,
			"health_check_path",
			healthCheckPath,
		)
	}

	err = instance.open(ctx, loadedConfig)
	if err != nil {
		closeErr := instance.close()
		if closeErr != nil {
			logWarn("close bot after open failure", closeErr)
		}

		return fmt.Errorf("open bot: %w", err)
	}

	select {
	case <-ctx.Done():
	case err = <-publicHTTPServerErrCh:
		closeErr := instance.close()
		if closeErr != nil {
			logWarn("close bot after http server failure", closeErr)
		}

		return fmt.Errorf("serve public http: %w", err)
	}

	err = instance.close()
	if err != nil {
		return fmt.Errorf("close bot: %w", err)
	}

	return nil
}

func (instance *bot) open(ctx context.Context, loadedConfig config) error {
	err := instance.validateDiscordGateway(ctx)
	if err != nil {
		return fmt.Errorf("validate discord gateway: %w", err)
	}

	err = instance.session.Open()
	if err != nil {
		return fmt.Errorf("open discord session: %w", err)
	}

	err = instance.configureSession(loadedConfig)
	if err != nil {
		return fmt.Errorf("configure discord session: %w", err)
	}

	instance.markSessionConfigured()

	if loadedConfig.ClientID != "" {
		slog.Info(
			"bot invite url",
			"url",
			fmt.Sprintf(
				"https://discord.com/oauth2/authorize?client_id=%s&permissions=412317191168&scope=bot",
				loadedConfig.ClientID,
			),
		)
	}

	return nil
}

func (instance *bot) handleReady(_ *discordgo.Session, _ *discordgo.Ready) {
	instance.markDiscordReady()
}

func (instance *bot) markDiscordReady() {
	instance.startupMu.Lock()
	instance.discordReady = true
	output, announce := instance.onlineAnnouncementLocked()
	instance.startupMu.Unlock()

	if !announce {
		return
	}

	_, _ = fmt.Fprintln(output, readyMessage)
}

func (instance *bot) markSessionConfigured() {
	instance.startupMu.Lock()
	instance.sessionConfigured = true
	output, announce := instance.onlineAnnouncementLocked()
	instance.startupMu.Unlock()

	if !announce {
		return
	}

	_, _ = fmt.Fprintln(output, readyMessage)
}

func (instance *bot) onlineAnnouncementLocked() (io.Writer, bool) {
	if !instance.discordReady || !instance.sessionConfigured || instance.onlineAnnounced {
		return nil, false
	}

	instance.onlineAnnounced = true
	if instance.onlineOutput != nil {
		return instance.onlineOutput, true
	}

	return os.Stdout, true
}

func (instance *bot) configureSession(loadedConfig config) error {
	err := instance.syncCommands()
	if err != nil {
		return fmt.Errorf("sync commands: %w", err)
	}

	err = instance.session.UpdateCustomStatus(statusMessage(loadedConfig.StatusMessage))
	if err != nil {
		return fmt.Errorf("update status message: %w", err)
	}

	return nil
}

func (instance *bot) close() error {
	err := instance.session.Close()
	if err != nil {
		return fmt.Errorf("close discord session: %w", err)
	}

	err = instance.nodes.close()
	if err != nil {
		return fmt.Errorf("close message store: %w", err)
	}

	return nil
}

func (instance *bot) syncCommands() error {
	if instance.session.State == nil || instance.session.State.User == nil {
		return fmt.Errorf("sync commands without discord user state: %w", os.ErrInvalid)
	}

	commands := make([]*discordgo.ApplicationCommand, 0, registeredCommandCount)
	commands = append(commands, newModelCommand())
	commands = append(commands, newSearchTypeCommand())
	commands = append(commands, newSearchDeciderModelCommand())
	commands = append(commands, newGroundingCommand())
	commands = append(commands, newEditChannelNameCommand())
	commands = append(commands, newMoveChannelCommand())

	_, err := instance.session.ApplicationCommandBulkOverwrite(
		instance.session.State.User.ID,
		"",
		commands,
	)
	if err != nil {
		return fmt.Errorf("overwrite application commands: %w", err)
	}

	return nil
}

func newModelCommand() *discordgo.ApplicationCommand {
	return newConfiguredModelCommand(
		modelCommandName,
		modelCommandDescription,
		modelOptionName,
		modelOptionDescription,
	)
}

func newSearchDeciderModelCommand() *discordgo.ApplicationCommand {
	return newConfiguredModelCommand(
		searchDeciderModelCommandName,
		searchDeciderModelCommandDescription,
		searchDeciderModelOptionName,
		searchDeciderModelOptionDescription,
	)
}

func newGroundingCommand() *discordgo.ApplicationCommand {
	command := new(discordgo.ApplicationCommand)
	command.Name = groundingCommandName
	command.Description = groundingCommandDescription

	option := new(discordgo.ApplicationCommandOption)
	option.Type = discordgo.ApplicationCommandOptionBoolean
	option.Name = groundingOptionName
	option.Description = groundingOptionDescription
	option.Required = false

	command.Options = []*discordgo.ApplicationCommandOption{option}

	return command
}

func newEditChannelNameCommand() *discordgo.ApplicationCommand {
	command := new(discordgo.ApplicationCommand)
	command.Name = editChannelNameCommandName
	command.Description = editChannelNameCommandDescription
	command.Type = discordgo.ChatApplicationCommand

	channelIDOption := new(discordgo.ApplicationCommandOption)
	channelIDOption.Name = editChannelNameChannelIDOptionName
	channelIDOption.Description = editChannelNameChannelIDOptionDescription
	channelIDOption.Type = discordgo.ApplicationCommandOptionString
	channelIDOption.Required = true

	newNameOption := new(discordgo.ApplicationCommandOption)
	newNameOption.Name = editChannelNameOptionName
	newNameOption.Description = editChannelNameOptionDescription
	newNameOption.Type = discordgo.ApplicationCommandOptionString
	newNameOption.Required = true

	command.Options = []*discordgo.ApplicationCommandOption{channelIDOption, newNameOption}

	return command
}

func newMoveChannelCommand() *discordgo.ApplicationCommand {
	command := new(discordgo.ApplicationCommand)
	command.Name = moveChannelCommandName
	command.Description = moveChannelCommandDescription
	command.Type = discordgo.ChatApplicationCommand

	channelIDOption := new(discordgo.ApplicationCommandOption)
	channelIDOption.Name = moveChannelChannelIDOptionName
	channelIDOption.Description = moveChannelChannelIDOptionDescription
	channelIDOption.Type = discordgo.ApplicationCommandOptionString
	channelIDOption.Required = true

	movementOption := new(discordgo.ApplicationCommandOption)
	movementOption.Name = moveChannelMovementOptionName
	movementOption.Description = moveChannelMovementOptionDescription
	movementOption.Type = discordgo.ApplicationCommandOptionString
	movementOption.Required = true

	upChoice := new(discordgo.ApplicationCommandOptionChoice)
	upChoice.Name = moveChannelMovementUp
	upChoice.Value = moveChannelMovementUp

	downChoice := new(discordgo.ApplicationCommandOptionChoice)
	downChoice.Name = moveChannelMovementDown
	downChoice.Value = moveChannelMovementDown

	movementOption.Choices = []*discordgo.ApplicationCommandOptionChoice{upChoice, downChoice}

	howManyOption := new(discordgo.ApplicationCommandOption)
	howManyOption.Name = moveChannelHowManyOptionName
	howManyOption.Description = moveChannelHowManyOptionDescription
	howManyOption.Type = discordgo.ApplicationCommandOptionInteger
	howManyOption.Required = true

	command.Options = []*discordgo.ApplicationCommandOption{channelIDOption, movementOption, howManyOption}

	return command
}

func newSearchTypeCommand() *discordgo.ApplicationCommand {
	return newConfiguredModelCommand(
		searchTypeCommandName,
		searchTypeCommandDescription,
		searchTypeOptionName,
		searchTypeOptionDescription,
	)
}

func newConfiguredModelCommand(
	commandName, commandDescription, optionName, optionDescription string,
) *discordgo.ApplicationCommand {
	command := new(discordgo.ApplicationCommand)
	command.Name = commandName
	command.Description = commandDescription
	command.Type = discordgo.ChatApplicationCommand

	option := new(discordgo.ApplicationCommandOption)
	option.Name = optionName
	option.Description = optionDescription
	option.Type = discordgo.ApplicationCommandOptionString
	option.Required = true
	option.Autocomplete = true

	command.Options = append(command.Options, option)

	return command
}

func (instance *bot) startTyping(ctx context.Context, channelID string) func() {
	stop := make(chan struct{})

	instance.sendTypingIndicator(channelID)

	safeGo(func() {
		ticker := time.NewTicker(typingRefreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
			}

			instance.sendTypingIndicator(channelID)
		}
	})

	return func() {
		close(stop)
	}
}

func (instance *bot) sendTypingIndicator(channelID string) {
	err := instance.session.ChannelTyping(channelID)
	if err != nil {
		logWarn("send typing indicator", err, "channel_id", channelID)
	}
}

func (instance *bot) waitForEditSlotForMessage(
	ctx context.Context,
	messageID string,
) error {
	waitDuration := instance.reserveEditDelay(messageID)
	if waitDuration <= 0 {
		return nil
	}

	timer := time.NewTimer(waitDuration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for edit slot: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (instance *bot) reserveEditDelay(messageID string) time.Duration {
	instance.editMu.Lock()
	defer instance.editMu.Unlock()

	editKey := strings.TrimSpace(messageID)
	if editKey == "" {
		return 0
	}

	if instance.nextEditAtByMessage == nil {
		instance.nextEditAtByMessage = make(map[string]time.Time)
	}

	now := time.Now()
	nextEditAt := instance.nextEditAtByMessage[editKey]

	waitDuration := time.Duration(0)

	if now.Before(nextEditAt) {
		waitDuration = time.Until(nextEditAt)
	}

	instance.nextEditAtByMessage[editKey] = now.Add(waitDuration).Add(editDelay)

	return waitDuration
}

func (instance *bot) currentModelForConfig(loadedConfig config) string {
	instance.modelMu.Lock()
	defer instance.modelMu.Unlock()

	if instance.currentModel == "" || !loadedConfig.hasModel(instance.currentModel) {
		instance.currentModel = loadedConfig.firstModel()
	}

	return instance.currentModel
}

func (instance *bot) currentModelForChannelIDs(
	loadedConfig config,
	channelIDs []string,
) string {
	if modelName, ok := loadedConfig.lockedModelForChannelIDs(channelIDs); ok {
		return modelName
	}

	return instance.currentModelForConfig(loadedConfig)
}

func (instance *bot) setCurrentModel(modelName string) {
	instance.modelMu.Lock()
	defer instance.modelMu.Unlock()

	instance.currentModel = modelName
}

func (instance *bot) currentExaSearchType() string {
	instance.modelMu.Lock()
	defer instance.modelMu.Unlock()

	searchType, ok := normalizeExaSearchType(instance.currentExaSearchTypeValue)
	if !ok {
		instance.currentExaSearchTypeValue = defaultExaSearchType

		return defaultExaSearchType
	}

	instance.currentExaSearchTypeValue = searchType

	return instance.currentExaSearchTypeValue
}

func (instance *bot) setCurrentExaSearchType(searchType string) {
	instance.modelMu.Lock()
	defer instance.modelMu.Unlock()

	normalizedSearchType, ok := normalizeExaSearchType(searchType)
	if !ok {
		normalizedSearchType = defaultExaSearchType
	}

	instance.currentExaSearchTypeValue = normalizedSearchType
}

func (instance *bot) currentSearchDeciderModelForConfig(loadedConfig config) string {
	instance.modelMu.Lock()
	defer instance.modelMu.Unlock()

	if instance.currentSearchDeciderModel == "" || !loadedConfig.hasModel(instance.currentSearchDeciderModel) {
		instance.currentSearchDeciderModel = loadedConfig.SearchDeciderModel
	}

	return instance.currentSearchDeciderModel
}

func (instance *bot) setCurrentSearchDeciderModel(modelName string) {
	instance.modelMu.Lock()
	defer instance.modelMu.Unlock()

	instance.currentSearchDeciderModel = modelName
}

func (instance *bot) currentGroundingEnabled(provider providerConfig) bool {
	instance.modelMu.Lock()
	defer instance.modelMu.Unlock()

	if instance.currentGroundingEnabledValue != nil {
		return *instance.currentGroundingEnabledValue
	}

	return provider.EnableGrounding
}

func (instance *bot) setCurrentGroundingEnabled(enabled *bool) {
	instance.modelMu.Lock()
	defer instance.modelMu.Unlock()

	instance.currentGroundingEnabledValue = enabled
}
