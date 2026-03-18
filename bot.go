package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

type bot struct {
	configPath                string
	session                   *discordgo.Session
	httpClient                *http.Client
	chatCompletions           chatCompletionClient
	webSearch                 webSearchClient
	visualSearch              visualSearchClient
	serpAPIVisualSearch       serpAPIVisualSearchClient
	rentry                    rentryClient
	tiktok                    tiktokContentClient
	facebook                  facebookContentClient
	youtube                   youtubeContentClient
	reddit                    redditContentClient
	website                   websiteContentClient
	nodes                     *messageNodeStore
	currentModel              string
	currentSearchDeciderModel string
	modelMu                   sync.RWMutex
	editMu                    sync.Mutex
	nextEditAt                time.Time
	startupMu                 sync.Mutex
	discordReady              bool
	sessionConfigured         bool
	onlineAnnounced           bool
	onlineOutput              io.Writer
}

func newBot(ctx context.Context, configPath string, loadedConfig config) (*bot, error) {
	discordSession, err := discordgo.New("Bot " + loadedConfig.BotToken)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}

	httpClient := new(http.Client)

	instance := new(bot)
	instance.configPath = configPath
	instance.session = discordSession
	instance.httpClient = httpClient
	instance.chatCompletions = newChatCompletionRouter(httpClient)
	instance.webSearch = newWebSearchClient(httpClient)
	instance.visualSearch = newVisualSearchClient(httpClient)
	instance.serpAPIVisualSearch = newSerpAPIVisualSearchClient(httpClient)
	instance.rentry = newRentryClient(httpClient, defaultRentryEndpoint)
	instance.tiktok = newTikTokClient(httpClient)

	instance.facebook, err = newFacebookClient(httpClient)
	if err != nil {
		return nil, fmt.Errorf("create facebook client: %w", err)
	}

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
		slog.Warn("configure persisted message history", "error", err)
	} else {
		instance.nodes = store
	}

	instance.currentModel = loadedConfig.firstModel()
	instance.currentSearchDeciderModel = loadedConfig.SearchDeciderModel
	instance.onlineOutput = os.Stdout

	discordSession.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent
	discordSession.AddHandler(instance.handleReady)
	discordSession.AddHandler(instance.handleInteractionCreate)
	discordSession.AddHandler(instance.handleMessageCreate)

	return instance, nil
}

func run(ctx context.Context, configPath string) error {
	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load startup config: %w", err)
	}

	instance, err := newBot(ctx, configPath, loadedConfig)
	if err != nil {
		return fmt.Errorf("create bot: %w", err)
	}

	err = instance.open(loadedConfig)
	if err != nil {
		_ = instance.close()

		return fmt.Errorf("open bot: %w", err)
	}

	<-ctx.Done()

	err = instance.close()
	if err != nil {
		return fmt.Errorf("close bot: %w", err)
	}

	return nil
}

func (instance *bot) open(loadedConfig config) error {
	err := instance.session.Open()
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
	commands = append(commands, newSearchDeciderModelCommand())

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

func newConfiguredModelCommand(
	commandName string,
	commandDescription string,
	optionName string,
	optionDescription string,
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

	go func() {
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
	}()

	return func() {
		close(stop)
	}
}

func (instance *bot) sendTypingIndicator(channelID string) {
	err := instance.session.ChannelTyping(channelID)
	if err != nil {
		slog.Warn("send typing indicator", "channel_id", channelID, "error", err)
	}
}

func (instance *bot) waitForEditSlot(ctx context.Context) error {
	waitDuration := instance.reserveEditDelay()
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

func (instance *bot) reserveEditDelay() time.Duration {
	instance.editMu.Lock()
	defer instance.editMu.Unlock()

	now := time.Now()

	waitDuration := time.Duration(0)

	if now.Before(instance.nextEditAt) {
		waitDuration = time.Until(instance.nextEditAt)
	}

	instance.nextEditAt = now.Add(waitDuration).Add(editDelay)

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
