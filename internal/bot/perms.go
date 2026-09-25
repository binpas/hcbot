package bot

import "github.com/bwmarrin/discordgo"

// channelPerms computes the permissions a set of roles has in a channel,
// the same way Discord does: @everyone and role permissions, then the
// @everyone overwrite, then the combined role overwrites, then the member
// overwrite when userID is set. Pass an empty userID to check a role on its
// own (as discord.py's permissions_for(role) does).
func channelPerms(g *discordgo.Guild, ch *discordgo.Channel, userID string, roleIDs []string) int64 {
	if userID != "" && userID == g.OwnerID {
		return discordgo.PermissionAll
	}
	has := map[string]bool{g.ID: true}
	for _, id := range roleIDs {
		has[id] = true
	}
	var perms int64
	for _, r := range g.Roles {
		if has[r.ID] {
			perms |= r.Permissions
		}
	}
	if perms&discordgo.PermissionAdministrator != 0 {
		return discordgo.PermissionAll
	}

	var roleAllow, roleDeny int64
	var member *discordgo.PermissionOverwrite
	for _, ow := range ch.PermissionOverwrites {
		switch {
		case ow.Type == discordgo.PermissionOverwriteTypeRole && ow.ID == g.ID:
			perms = perms&^ow.Deny | ow.Allow
		case ow.Type == discordgo.PermissionOverwriteTypeRole && has[ow.ID]:
			roleAllow |= ow.Allow
			roleDeny |= ow.Deny
		case ow.Type == discordgo.PermissionOverwriteTypeMember && userID != "" && ow.ID == userID:
			member = ow
		}
	}
	perms = perms&^roleDeny | roleAllow
	if member != nil {
		perms = perms&^member.Deny | member.Allow
	}

	// No View Channel means no permissions at all there; in a voice channel,
	// no Connect means none of the voice permissions.
	if perms&discordgo.PermissionViewChannel == 0 {
		return 0
	}
	if isVoice(ch) && perms&discordgo.PermissionVoiceConnect == 0 {
		perms &^= discordgo.PermissionVoiceConnect | discordgo.PermissionVoiceSpeak |
			discordgo.PermissionVoiceMuteMembers | discordgo.PermissionVoiceDeafenMembers |
			discordgo.PermissionVoiceMoveMembers | discordgo.PermissionVoiceUseVAD |
			discordgo.PermissionVoicePrioritySpeaker | discordgo.PermissionVoiceStreamVideo
	}
	return perms
}

func isVoice(ch *discordgo.Channel) bool {
	return ch.Type == discordgo.ChannelTypeGuildVoice || ch.Type == discordgo.ChannelTypeGuildStageVoice
}

// roleOverwrite returns a copy of the role's overwrite on the channel, or a
// blank one, so a change can be merged into it instead of replacing it.
func roleOverwrite(ch *discordgo.Channel, roleID string) discordgo.PermissionOverwrite {
	for _, ow := range ch.PermissionOverwrites {
		if ow.Type == discordgo.PermissionOverwriteTypeRole && ow.ID == roleID {
			return *ow
		}
	}
	return discordgo.PermissionOverwrite{ID: roleID, Type: discordgo.PermissionOverwriteTypeRole}
}

// set explicitly allows (true) or denies (false) a permission on the overwrite.
func setPerm(ow *discordgo.PermissionOverwrite, perm int64, allow bool) {
	if allow {
		ow.Allow |= perm
		ow.Deny &^= perm
	} else {
		ow.Deny |= perm
		ow.Allow &^= perm
	}
}

// withOverwrite returns a copy of the channel with the overwrite put in
// place, so permissions can be checked before the state cache catches up.
func withOverwrite(ch *discordgo.Channel, ow discordgo.PermissionOverwrite) *discordgo.Channel {
	cp := *ch
	cp.PermissionOverwrites = nil
	for _, o := range ch.PermissionOverwrites {
		if o.Type != ow.Type || o.ID != ow.ID {
			cp.PermissionOverwrites = append(cp.PermissionOverwrites, o)
		}
	}
	cp.PermissionOverwrites = append(cp.PermissionOverwrites, &ow)
	return &cp
}

// permissionNames are discord.py's names for the permission bits, in bit
// order, as the audit log shows them.
var permissionNames = []struct {
	bit  int64
	name string
}{
	{1 << 0, "create_instant_invite"}, {1 << 1, "kick_members"}, {1 << 2, "ban_members"},
	{1 << 3, "administrator"}, {1 << 4, "manage_channels"}, {1 << 5, "manage_guild"},
	{1 << 6, "add_reactions"}, {1 << 7, "view_audit_log"}, {1 << 8, "priority_speaker"},
	{1 << 9, "stream"}, {1 << 10, "read_messages"}, {1 << 11, "send_messages"},
	{1 << 12, "send_tts_messages"}, {1 << 13, "manage_messages"}, {1 << 14, "embed_links"},
	{1 << 15, "attach_files"}, {1 << 16, "read_message_history"}, {1 << 17, "mention_everyone"},
	{1 << 18, "external_emojis"}, {1 << 19, "view_guild_insights"}, {1 << 20, "connect"},
	{1 << 21, "speak"}, {1 << 22, "mute_members"}, {1 << 23, "deafen_members"},
	{1 << 24, "move_members"}, {1 << 25, "use_voice_activation"}, {1 << 26, "change_nickname"},
	{1 << 27, "manage_nicknames"}, {1 << 28, "manage_roles"}, {1 << 29, "manage_webhooks"},
	{1 << 30, "manage_expressions"}, {1 << 31, "use_application_commands"}, {1 << 32, "request_to_speak"},
	{1 << 33, "manage_events"}, {1 << 34, "manage_threads"}, {1 << 35, "create_public_threads"},
	{1 << 36, "create_private_threads"}, {1 << 37, "external_stickers"}, {1 << 38, "send_messages_in_threads"},
	{1 << 39, "use_embedded_activities"}, {1 << 40, "moderate_members"},
	{1 << 41, "view_creator_monetization_analytics"}, {1 << 42, "use_soundboard"},
	{1 << 43, "create_expressions"}, {1 << 44, "create_events"}, {1 << 45, "use_external_sounds"},
	{1 << 46, "send_voice_messages"}, {1 << 49, "send_polls"}, {1 << 50, "use_external_apps"},
}

// permNames lists the names of the bits set in perms.
func permNames(perms int64) []string {
	var out []string
	for _, p := range permissionNames {
		if perms&p.bit != 0 {
			out = append(out, p.name)
		}
	}
	return out
}
