package api

import (
	"github.com/Gurkunwar/asyncflow/internal/models"
	"github.com/bwmarrin/discordgo"
)

// isGuildAdmin reports whether the user owns the guild or has an
// Administrator / Manage Server permission through any of their roles.
// Channel overrides are ignored on purpose: this check is conservative.
func (s *Server) isGuildAdmin(userID, guildID string) bool {
	if userID == "" || guildID == "" {
		return false
	}

	guild, err := s.Session.Guild(guildID)
	if err != nil {
		return false
	}
	if guild.OwnerID == userID {
		return true
	}

	member, err := s.Session.GuildMember(guildID, userID)
	if err != nil {
		return false
	}

	memberRoles := make(map[string]bool, len(member.Roles))
	for _, r := range member.Roles {
		memberRoles[r] = true
	}

	var perms int64
	for _, role := range guild.Roles {
		// @everyone role shares the guild ID; it always applies to members.
		if role.ID == guildID || memberRoles[role.ID] {
			perms |= role.Permissions
		}
	}

	return perms&discordgo.PermissionAdministrator != 0 ||
		perms&discordgo.PermissionManageGuild != 0
}

// canManageStandup allows the standup manager or an admin of the standup's guild.
func (s *Server) canManageStandup(userID string, standup models.Standup) bool {
	if standup.ManagerID == userID {
		return true
	}
	return s.isGuildAdmin(userID, standup.GuildID)
}
