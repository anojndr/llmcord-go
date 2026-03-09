package main

import "slices"

type accessContext struct {
	IsDM       bool
	UserID     string
	RoleIDs    []string
	ChannelIDs []string
}

func messageAllowed(loadedConfig config, context accessContext) bool {
	userPermissions := loadedConfig.Permissions.Users
	rolePermissions := loadedConfig.Permissions.Roles
	channelPermissions := loadedConfig.Permissions.Channels

	userIsAdmin := containsID(userPermissions.AdminIDs, context.UserID)

	allowAllUsers := len(userPermissions.AllowedIDs) == 0
	if !context.IsDM {
		allowAllUsers = allowAllUsers && len(rolePermissions.AllowedIDs) == 0
	}

	userAllowed := userIsAdmin ||
		allowAllUsers ||
		containsID(userPermissions.AllowedIDs, context.UserID) ||
		containsAnyID(rolePermissions.AllowedIDs, context.RoleIDs)

	userBlocked := !userAllowed ||
		containsID(userPermissions.BlockedIDs, context.UserID) ||
		containsAnyID(rolePermissions.BlockedIDs, context.RoleIDs)

	channelAllowed := userIsAdmin
	if context.IsDM {
		channelAllowed = channelAllowed || loadedConfig.AllowDMs
	} else {
		channelAllowed = channelAllowed ||
			len(channelPermissions.AllowedIDs) == 0 ||
			containsAnyID(channelPermissions.AllowedIDs, context.ChannelIDs)
	}

	channelBlocked := !channelAllowed ||
		containsAnyID(channelPermissions.BlockedIDs, context.ChannelIDs)

	return !userBlocked && !channelBlocked
}

func containsID(values []string, candidate string) bool {
	return slices.Contains(values, candidate)
}

func containsAnyID(values []string, candidates []string) bool {
	for _, candidate := range candidates {
		if containsID(values, candidate) {
			return true
		}
	}

	return false
}
