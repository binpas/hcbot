package bot

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// role returns a guild role from the state cache, or nil.
func (b *Bot) role(guildID, roleID string) *discordgo.Role {
	if roleID == "" {
		return nil
	}
	r, err := b.s.State.Role(guildID, roleID)
	if err != nil {
		return nil
	}
	return r
}

// roleBelow reports whether role a sorts below role b in the hierarchy.
// Equal positions fall back to ID order: the older role ranks higher.
func roleBelow(a, b *discordgo.Role) bool {
	if a.Position != b.Position {
		return a.Position < b.Position
	}
	ai, _ := strconv.ParseUint(a.ID, 10, 64)
	bi, _ := strconv.ParseUint(b.ID, 10, 64)
	return ai > bi
}

// memberRoles returns the member's roles (without @everyone), highest first.
func (b *Bot) memberRoles(guildID string, m *discordgo.Member) []*discordgo.Role {
	var roles []*discordgo.Role
	for _, id := range m.Roles {
		if r := b.role(guildID, id); r != nil {
			roles = append(roles, r)
		}
	}
	slices.SortFunc(roles, func(x, y *discordgo.Role) int {
		switch {
		case roleBelow(y, x):
			return -1
		case roleBelow(x, y):
			return 1
		}
		return 0
	})
	return roles
}

// topRole returns the member's highest role, or @everyone.
func (b *Bot) topRole(guildID string, m *discordgo.Member) *discordgo.Role {
	if roles := b.memberRoles(guildID, m); len(roles) > 0 {
		return roles[0]
	}
	if r := b.role(guildID, guildID); r != nil {
		return r
	}
	return &discordgo.Role{ID: guildID}
}

// botTopRole returns the bot's own highest role in the guild.
func (b *Bot) botTopRole(guildID string) *discordgo.Role {
	me := b.resolveMember(guildID, b.s.State.User.ID)
	if me == nil {
		return &discordgo.Role{ID: guildID}
	}
	return b.topRole(guildID, me)
}

// memberColour is the colour of the member's highest coloured role.
func (b *Bot) memberColour(guildID string, m *discordgo.Member) int {
	for _, r := range b.memberRoles(guildID, m) {
		if r.Color != 0 {
			return r.Color
		}
	}
	return 0
}

func userMention(id string) string { return "<@" + id + ">" }

// discordTimestamp renders a Discord timestamp tag, e.g. style "D" for a date.
func discordTimestamp(unix int64, style string) string {
	return fmt.Sprintf("<t:%d:%s>", unix, style)
}

// auditReason sets a request's audit log reason. Discord expects the
// header URL-encoded; this encodes it the same way discord.py does, so
// non-ASCII text and "%" show correctly in the audit log.
func auditReason(reason string) discordgo.RequestOption {
	return discordgo.WithAuditLogReason(reasonEscaper.Replace(url.PathEscape(reason)))
}

var reasonEscaper = strings.NewReplacer("%20", " ", "%2F", "/")
