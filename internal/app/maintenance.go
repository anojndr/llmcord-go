package app

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"
)

func (instance *bot) isMaintenanceChannel(channelID string) bool {
	if instance == nil || channelID == "" {
		return false
	}

	instance.maintenanceMu.RLock()
	defer instance.maintenanceMu.RUnlock()

	if instance.maintenanceChannels == nil {
		return false
	}

	_, ok := instance.maintenanceChannels[channelID]

	return ok
}

func (instance *bot) setMaintenanceChannel(channelID string) {
	if instance == nil || channelID == "" {
		return
	}

	instance.maintenanceMu.Lock()
	defer instance.maintenanceMu.Unlock()

	if instance.maintenanceChannels == nil {
		instance.maintenanceChannels = make(map[string]struct{})
	}

	instance.maintenanceChannels[channelID] = struct{}{}
}

func (instance *bot) clearMaintenanceChannel(channelID string) {
	if instance == nil || channelID == "" {
		return
	}

	instance.maintenanceMu.Lock()
	defer instance.maintenanceMu.Unlock()

	if instance.maintenanceChannels == nil {
		return
	}

	delete(instance.maintenanceChannels, channelID)
}

func (instance *bot) isAllowedMaintenanceBypass(userID string) bool {
	return userID == maintenanceOwnerID || userID == maintenanceBotID
}

func maintenanceInvokerID(interaction *discordgo.InteractionCreate) string {
	if interaction == nil || interaction.Interaction == nil {
		return ""
	}

	if interaction.Member != nil && interaction.Member.User != nil && interaction.Member.User.ID != "" {
		return interaction.Member.User.ID
	}

	if interaction.User != nil && interaction.User.ID != "" {
		return interaction.User.ID
	}

	return ""
}

func newMaintenanceCommand() *discordgo.ApplicationCommand {
	command := new(discordgo.ApplicationCommand)
	command.Name = maintenanceCommandName
	command.Description = maintenanceCommandDescription
	command.Type = discordgo.ChatApplicationCommand

	channelIDOption := func() *discordgo.ApplicationCommandOption {
		option := new(discordgo.ApplicationCommandOption)
		option.Name = maintenanceChannelIDOptionName
		option.Description = maintenanceChannelIDOptionDescription
		option.Type = discordgo.ApplicationCommandOptionString
		option.Required = true

		return option
	}

	startSubcommand := new(discordgo.ApplicationCommandOption)
	startSubcommand.Name = maintenanceStartSubcommandName
	startSubcommand.Description = maintenanceStartSubcommandDescription
	startSubcommand.Type = discordgo.ApplicationCommandOptionSubCommand
	startSubcommand.Options = []*discordgo.ApplicationCommandOption{channelIDOption()}

	stopSubcommand := new(discordgo.ApplicationCommandOption)
	stopSubcommand.Name = maintenanceStopSubcommandName
	stopSubcommand.Description = maintenanceStopSubcommandDescription
	stopSubcommand.Type = discordgo.ApplicationCommandOptionSubCommand
	stopSubcommand.Options = []*discordgo.ApplicationCommandOption{channelIDOption()}

	command.Options = []*discordgo.ApplicationCommandOption{startSubcommand, stopSubcommand}

	return command
}

func (instance *bot) handleMaintenanceCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if interaction == nil || interaction.Interaction == nil {
		return fmt.Errorf("maintenance interaction is required: %w", os.ErrInvalid)
	}

	invokerID := maintenanceInvokerID(interaction)
	if invokerID != maintenanceOwnerID {
		return respondInteractionText(
			session,
			interaction.Interaction,
			"You do not have permission to manage maintenance mode.",
		)
	}

	commandData := interaction.ApplicationCommandData()
	if len(commandData.Options) == 0 {
		return respondInteractionText(
			session,
			interaction.Interaction,
			"Missing subcommand. Use `/maintenance start` or `/maintenance stop`.",
		)
	}

	subcommand := commandData.Options[0]

	channelID := maintenanceChannelIDFromSubcommand(subcommand)
	if channelID == "" {
		return respondInteractionText(
			session,
			interaction.Interaction,
			fmt.Sprintf("`%s` is required.", maintenanceChannelIDOptionName),
		)
	}

	err := respondInteractionDeferredWithFlags(
		session,
		interaction.Interaction,
		0,
	)
	if err != nil {
		return fmt.Errorf("defer maintenance interaction response: %w", err)
	}

	switch subcommand.Name {
	case maintenanceStartSubcommandName:
		return instance.handleMaintenanceStart(session, interaction, channelID, invokerID)
	case maintenanceStopSubcommandName:
		return instance.handleMaintenanceStop(session, interaction, channelID, invokerID)
	default:
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"Unknown maintenance subcommand. Use `start` or `stop`.",
		)
	}
}

func maintenanceChannelIDFromSubcommand(
	subcommand *discordgo.ApplicationCommandInteractionDataOption,
) string {
	if subcommand == nil {
		return ""
	}

	for _, option := range subcommand.Options {
		if option != nil && option.Name == maintenanceChannelIDOptionName {
			if value := strings.TrimSpace(option.StringValue()); value != "" {
				return value
			}

			break
		}
	}

	if opt := subcommand.GetOption(maintenanceChannelIDOptionName); opt != nil {
		return strings.TrimSpace(opt.StringValue())
	}

	return ""
}

func (instance *bot) handleMaintenanceStart(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
	channelID string,
	invokerID string,
) error {
	if instance.isMaintenanceChannel(channelID) {
		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Channel `%s` is already in maintenance mode.", channelID),
		)
	}

	err := instance.applyMaintenancePermissions(session, channelID)
	if err != nil {
		logWarn("maintenance start failed", err, "channel_id", channelID)

		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Failed to enable maintenance for channel `%s`.", channelID),
		)
	}

	instance.setMaintenanceChannel(channelID)
	slog.Info("maintenance enabled", "channel_id", channelID, "invoker_id", invokerID)

	return editInteractionResponseText(
		session,
		interaction.Interaction,
		fmt.Sprintf(
			"Maintenance enabled for channel `%s`. Only <@%s> can send messages here now.",
			channelID,
			maintenanceOwnerID,
		),
	)
}

func (instance *bot) handleMaintenanceStop(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
	channelID string,
	invokerID string,
) error {
	if !instance.isMaintenanceChannel(channelID) {
		_ = instance.removeMaintenancePermissions(session, channelID)

		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Channel `%s` is not in maintenance mode.", channelID),
		)
	}

	err := instance.removeMaintenancePermissions(session, channelID)
	if err != nil {
		logWarn("maintenance stop failed", err, "channel_id", channelID)

		return editInteractionResponseText(
			session,
			interaction.Interaction,
			fmt.Sprintf("Failed to disable maintenance for channel `%s`.", channelID),
		)
	}

	instance.clearMaintenanceChannel(channelID)
	slog.Info("maintenance disabled", "channel_id", channelID, "invoker_id", invokerID)

	return editInteractionResponseText(
		session,
		interaction.Interaction,
		fmt.Sprintf("Maintenance disabled for channel `%s`.", channelID),
	)
}

func (instance *bot) applyMaintenancePermissions(
	session *discordgo.Session,
	channelID string,
) error {
	if session == nil {
		return fmt.Errorf("discord session is required: %w", os.ErrInvalid)
	}

	channel, err := session.Channel(channelID)
	if err != nil {
		return fmt.Errorf("load channel %s: %w", channelID, err)
	}

	guildID := channel.GuildID
	if guildID == "" {
		return fmt.Errorf("channel %s is not in a guild: %w", channelID, os.ErrInvalid)
	}

	err = session.ChannelPermissionSet(
		channelID,
		guildID,
		discordgo.PermissionOverwriteTypeRole,
		0,
		discordgo.PermissionSendMessages,
	)
	if err != nil {
		return fmt.Errorf("set @everyone deny: %w", err)
	}

	err = session.ChannelPermissionSet(
		channelID,
		maintenanceOwnerID,
		discordgo.PermissionOverwriteTypeMember,
		discordgo.PermissionViewChannel|discordgo.PermissionSendMessages,
		0,
	)
	if err != nil {
		return fmt.Errorf("set owner allow: %w", err)
	}

	err = session.ChannelPermissionSet(
		channelID,
		maintenanceBotID,
		discordgo.PermissionOverwriteTypeMember,
		discordgo.PermissionViewChannel|discordgo.PermissionSendMessages,
		0,
	)
	if err != nil {
		return fmt.Errorf("set bot allow: %w", err)
	}

	return nil
}

func (instance *bot) removeMaintenancePermissions(
	session *discordgo.Session,
	channelID string,
) error {
	if session == nil {
		return fmt.Errorf("discord session is required: %w", os.ErrInvalid)
	}

	channel, err := session.Channel(channelID)
	if err != nil {
		return fmt.Errorf("load channel %s: %w", channelID, err)
	}

	guildID := channel.GuildID
	if guildID == "" {
		return fmt.Errorf("channel %s is not in a guild: %w", channelID, os.ErrInvalid)
	}

	errEveryone := session.ChannelPermissionDelete(channelID, guildID)
	errOwner := session.ChannelPermissionDelete(channelID, maintenanceOwnerID)
	errBot := session.ChannelPermissionDelete(channelID, maintenanceBotID)

	if errEveryone != nil && errOwner != nil && errBot != nil {
		return fmt.Errorf("delete permissions: everyone: %w owner: %w bot: %w", errEveryone, errOwner, errBot)
	}

	if errEveryone != nil {
		if errOwner == nil && errBot == nil {
			return nil
		}

		return fmt.Errorf("delete @everyone permission: %w", errEveryone)
	}

	if errOwner != nil {
		return fmt.Errorf("delete owner permission: %w", errOwner)
	}

	if errBot != nil {
		return fmt.Errorf("delete bot permission: %w", errBot)
	}

	return nil
}

func (instance *bot) enforceMaintenanceMode(message *discordgo.Message) bool {
	if message == nil || message.Author == nil {
		return false
	}

	if instance.isAllowedMaintenanceBypass(message.Author.ID) {
		return false
	}

	channelIDs, err := instance.messageChannelIDs(message)
	if err != nil {
		channelIDs = []string{message.ChannelID}
	}

	if !slices.ContainsFunc(channelIDs, instance.isMaintenanceChannel) {
		return false
	}

	if instance.session != nil {
		if err := instance.session.ChannelMessageDelete(message.ChannelID, message.ID); err != nil {
			logWarn("maintenance delete blocked message", err, "channel_id", message.ChannelID, "message_id", message.ID, "user_id", message.Author.ID)
		}
	}

	slog.Info(
		"blocked message in maintenance channel",
		"channel_id",
		message.ChannelID,
		"message_id",
		message.ID,
		"user_id",
		message.Author.ID,
	)

	return true
}
