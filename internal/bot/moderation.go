package bot

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/db"
)

// More discord.py Colour presets used by the moderation embeds.
const (
	colourYellow     = 0xFEE75C
	colourOrange     = 0xE67E22
	colourDarkOrange = 0xA84300
	colourDarkGrey   = 0x607D8B
	colourGreyple    = 0x99AAB5
	colourGold       = 0xF1C40F
	colourLightGrey  = 0x979C9F
)

const (
	noReason        = "No reason provided"
	memberNotFound  = "Member not found. Use a mention or valid user ID."
	manageMessages  = int64(discordgo.PermissionManageMessages)
	maxEmbedFields  = 25
	bulkDeleteLimit = 14 * 24 * time.Hour
)

func (b *Bot) registerModeration() {
	memberOpt := func(desc string) *discordgo.ApplicationCommandOption { return optUser("member", desc, true) }

	b.addCommand(&command{
		name:        "timeout",
		description: "Take a break – mute yourself for N minutes.",
		options:     []*discordgo.ApplicationCommandOption{optInt("minutes", "Duration in minutes (1–1440, default 10)", false)},
		handler:     b.cmdSelfTimeout,
	})
	b.addCommand(&command{
		name:        "ban",
		description: "Ban a member from the server.",
		options:     []*discordgo.ApplicationCommandOption{memberOpt("Member to ban"), optStr("reason", "Reason for the ban", false)},
		requires:    discordgo.PermissionBanMembers,
		defaultPerm: manageMessages,
		handler:     b.cmdBan,
	})
	b.addCommand(&command{
		name:        "kick",
		description: "Kick a member from the server.",
		options:     []*discordgo.ApplicationCommandOption{memberOpt("Member to kick"), optStr("reason", "Reason for the kick", false)},
		requires:    discordgo.PermissionKickMembers,
		defaultPerm: manageMessages,
		handler:     b.cmdKick,
	})
	b.addCommand(&command{
		name:        "tempban",
		description: "Ban a member for a set number of minutes (mod only).",
		options: []*discordgo.ApplicationCommandOption{
			memberOpt("Member to temp-ban"),
			optInt("duration", "Duration in minutes", false),
			optStr("reason", "Reason for the ban", false),
		},
		requires:    discordgo.PermissionBanMembers,
		defaultPerm: manageMessages,
		handler:     b.cmdTempban,
	})
	b.addCommand(&command{
		name:        "mute",
		description: "Timeout (mute) a member.",
		options: []*discordgo.ApplicationCommandOption{
			memberOpt("Member to mute"),
			optInt("duration", "Duration in minutes", false),
			optStr("reason", "Reason", false),
		},
		requires:    discordgo.PermissionModerateMembers,
		defaultPerm: manageMessages,
		handler:     b.cmdMute,
	})
	b.addCommand(&command{
		name:        "unmute",
		description: "Remove a timeout from a member.",
		options:     []*discordgo.ApplicationCommandOption{memberOpt("Member to unmute")},
		requires:    discordgo.PermissionModerateMembers,
		defaultPerm: manageMessages,
		handler:     b.cmdUnmute,
	})
	amount := optInt("amount", "Number of messages to delete", false)
	one := 1.0
	amount.MinValue = &one
	b.addCommand(&command{
		name:        "purge",
		description: "Delete a number of recent messages (max 100).",
		options:     []*discordgo.ApplicationCommandOption{amount},
		requires:    discordgo.PermissionManageMessages,
		defaultPerm: manageMessages,
		handler:     b.cmdPurge,
	})
	b.addCommand(&command{
		name:        "motd",
		description: "Give a member the Member Of The Day role for 24 h.",
		options:     []*discordgo.ApplicationCommandOption{memberOpt("Member to receive the MOTD role")},
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdMotd,
	})

	b.addGroup(&group{name: "warn", description: "Manage warnings (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{
		name:        "warn add",
		description: "Warn a member.",
		options:     []*discordgo.ApplicationCommandOption{memberOpt("Member to warn"), optStr("reason", "Reason for the warning", false)},
		perm:        permMod,
		handler:     b.cmdWarnAdd,
	})
	b.addCommand(&command{
		name:        "warn history",
		description: "Show all warnings for a member.",
		options:     []*discordgo.ApplicationCommandOption{memberOpt("Member to look up")},
		perm:        permMod,
		handler:     b.cmdRapsheet,
	})
	b.addCommand(&command{
		name:        "warn clear",
		description: "Clear all warnings for a member.",
		options:     []*discordgo.ApplicationCommandOption{memberOpt("Member whose warnings should be cleared")},
		perm:        permMod,
		handler:     b.cmdWarnClear,
	})
	b.addCommand(&command{
		name:        "warn remove",
		description: "Remove a single warning by its ID.",
		options:     []*discordgo.ApplicationCommandOption{optInt("warning_id", "The warning ID shown in the rap sheet", true)},
		perm:        permMod,
		handler:     b.cmdWarnRemove,
	})
	// Top-level alias of /warn history, kept by request.
	b.addCommand(&command{
		name:        "rapsheet",
		description: "Show all warnings for a member (mod only, alias of /warn history).",
		options:     []*discordgo.ApplicationCommandOption{memberOpt("Member to look up")},
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdRapsheet,
	})

	b.addCommand(&command{
		name:        "userinfo",
		description: "Show info about a member (mod only).",
		options:     []*discordgo.ApplicationCommandOption{optUser("member", "Member to look up (defaults to yourself)", false)},
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdUserinfo,
	})
	b.addCommand(&command{
		name:        "serverinfo",
		description: "Show info about this server (mod only).",
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdServerinfo,
	})
}

// targetMember returns the "member" option, replying with an error when the
// user isn't in the guild.
func (c *Ctx) targetMember() *discordgo.Member {
	m := c.MemberOpt("member")
	if m == nil {
		c.Reply(true, errEmbed(memberNotFound))
	}
	return m
}

func (c *Ctx) reason() string {
	if r, ok := c.Str("reason"); ok && strings.TrimSpace(r) != "" {
		return r
	}
	return noReason
}

// replyAndLog posts a public action embed, then sends it to the mod log
// with a jump link to the reply.
func (c *Ctx) replyAndLog(e *discordgo.MessageEmbed, jump bool) {
	c.Reply(false, e)
	logged := *e
	if jump {
		if url := c.ResponseURL(); url != "" {
			logged.Description += fmt.Sprintf("\n[Jump to context](%s)", url)
		}
	}
	c.b.modLog(&logged)
}

// actionFailed logs a failed moderation API call and tells the invoker.
func (c *Ctx) actionFailed(what string, err error) {
	c.logErr(what, err)
	c.Reply(true, errEmbed(fmt.Sprintf("I couldn't %s — check my permissions and role position.", what)))
}

func (b *Bot) cmdSelfTimeout(c *Ctx) {
	minutes := c.Int("minutes", 10)
	if minutes < 1 || minutes > 1440 {
		c.Reply(true, errEmbed("Choose a duration between 1 and 1440 minutes."))
		return
	}
	until := time.Now().Add(time.Duration(minutes) * time.Minute)
	if err := b.s.GuildMemberTimeout(c.GuildID(), c.UserID(), &until,
		auditReason("Self-requested break")); err != nil {
		c.logErr("self timeout", err)
		c.Reply(true, errEmbed("I don't have permission to timeout you."))
		return
	}
	c.Reply(true, okEmbed(fmt.Sprintf("Enjoy your break, %s! You'll be unmuted in **%d min**.",
		userMention(c.UserID()), minutes)))
}

func (b *Bot) cmdBan(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	reason := c.reason()
	if err := b.s.GuildBanCreateWithReason(c.GuildID(), m.User.ID, reason, 0, auditReason(reason)); err != nil {
		c.actionFailed("ban "+m.User.String(), err)
		return
	}
	c.replyAndLog(makeEmbed("🔨 Member Banned",
		fmt.Sprintf("**%s** was banned by %s.\n**Reason:** %s", m.User, userMention(c.UserID()), reason),
		colourRed), true)
}

func (b *Bot) cmdKick(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	reason := c.reason()
	if err := b.s.GuildMemberDeleteWithReason(c.GuildID(), m.User.ID, reason, auditReason(reason)); err != nil {
		c.actionFailed("kick "+m.User.String(), err)
		return
	}
	c.replyAndLog(makeEmbed("👢 Member Kicked",
		fmt.Sprintf("**%s** was kicked by %s.\n**Reason:** %s", m.User, userMention(c.UserID()), reason),
		colourOrange), true)
}

func (b *Bot) cmdTempban(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	duration := c.Int("duration", 60)
	reason := c.reason()
	banReason := fmt.Sprintf("[Temp-ban: %d min] %s", duration, reason)
	if err := b.s.GuildBanCreateWithReason(c.GuildID(), m.User.ID, banReason, 0, auditReason(banReason)); err != nil {
		c.actionFailed("ban "+m.User.String(), err)
		return
	}
	c.replyAndLog(makeEmbed("⏱️ Member Temp-Banned",
		fmt.Sprintf("**%s** was temp-banned by %s for **%d min**.\n**Reason:** %s",
			m.User, userMention(c.UserID()), duration, reason),
		colourRed), true)

	// Stored so the unban survives a restart; runScheduledActions does it.
	if err := b.db.ScheduleAction(c.GuildID(), m.User.ID, m.User.String(), db.ActionUnban, "",
		time.Now().Add(time.Duration(duration)*time.Minute)); err != nil {
		c.logErr("schedule unban", err)
		c.Reply(true, errEmbed("I couldn't save the unban timer — unban them by hand when it's up."))
	}
}

func (b *Bot) cmdMute(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	duration := c.Int("duration", 10)
	reason := c.reason()
	until := time.Now().Add(time.Duration(duration) * time.Minute)
	if err := b.s.GuildMemberTimeout(c.GuildID(), m.User.ID, &until, auditReason(reason)); err != nil {
		c.actionFailed("mute "+m.User.String(), err)
		return
	}
	c.replyAndLog(makeEmbed("🔇 Member Muted",
		fmt.Sprintf("**%s** was muted by %s for **%d min**.\n**Reason:** %s",
			m.User, userMention(c.UserID()), duration, reason),
		colourDarkGrey), true)
}

func (b *Bot) cmdUnmute(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	if err := b.s.GuildMemberTimeout(c.GuildID(), m.User.ID, nil); err != nil {
		c.actionFailed("unmute "+m.User.String(), err)
		return
	}
	c.replyAndLog(makeEmbed("🔊 Member Unmuted",
		fmt.Sprintf("**%s** was unmuted by %s.", m.User, userMention(c.UserID())), colourGreen), false)
}

func (b *Bot) cmdPurge(c *Ctx) {
	amount := int(min(c.Int("amount", 10), 100))
	channelID := c.i.ChannelID
	c.Defer(true)

	msgs, err := b.s.ChannelMessages(channelID, amount, "", "", "")
	if err != nil {
		c.actionFailed("read this channel's messages", err)
		return
	}

	// Bulk delete only accepts messages under 14 days old; older ones
	// must go one at a time.
	cutoff := time.Now().Add(-bulkDeleteLimit + time.Minute)
	var recent []string
	var deleted []*discordgo.Message
	for _, m := range msgs {
		if m.Timestamp.After(cutoff) {
			recent = append(recent, m.ID)
			deleted = append(deleted, m)
		} else if err := b.s.ChannelMessageDelete(channelID, m.ID); err == nil {
			deleted = append(deleted, m)
		}
	}
	switch {
	case len(recent) == 1:
		err = b.s.ChannelMessageDelete(channelID, recent[0])
	case len(recent) > 1:
		err = b.s.ChannelMessagesBulkDelete(channelID, recent)
	}
	if err != nil {
		c.actionFailed("delete those messages", err)
		return
	}

	var order []string
	tally := map[string]int{}
	for _, m := range deleted {
		name := m.Author.String()
		if tally[name] == 0 {
			order = append(order, name)
		}
		tally[name]++
	}
	var summary []string
	for _, name := range order {
		summary = append(summary, fmt.Sprintf("**%s**: %d", name, tally[name]))
	}
	b.modLog(makeEmbed("🗑️ Messages Purged",
		fmt.Sprintf("%s deleted **%d** messages in %s.\n\n%s",
			userMention(c.UserID()), len(deleted), channelMention(channelID), strings.Join(summary, "\n")),
		colourGreyple))

	c.Reply(true, okEmbed(fmt.Sprintf("Deleted **%d** messages.", len(deleted))))
	time.AfterFunc(5*time.Second, c.DeleteResponse)
}

func (b *Bot) cmdMotd(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	guildID := c.GuildID()
	role := b.role(guildID, b.cfg.ID("MOTD_ROLE"))
	if role == nil {
		c.Reply(true, errEmbed("MOTD role not found. Set it with `/setup`."))
		return
	}
	if err := b.s.GuildMemberRoleAdd(guildID, m.User.ID, role.ID,
		auditReason("Member Of The Day")); err != nil {
		c.actionFailed("give "+m.User.String()+" the MOTD role", err)
		return
	}
	c.replyAndLog(makeEmbed("🌟 Member Of The Day",
		fmt.Sprintf("%s is today's **Member Of The Day**! Role removed in 24 h.", userMention(m.User.ID)),
		colourGold), false)

	// Stored so the removal survives a restart; runScheduledActions does it.
	if err := b.db.ScheduleAction(guildID, m.User.ID, m.User.String(), db.ActionRemoveRole, role.ID,
		time.Now().Add(24*time.Hour)); err != nil {
		c.logErr("schedule motd removal", err)
		c.Reply(true, errEmbed("I couldn't save the removal timer — remove the MOTD role by hand tomorrow."))
	}
}

func (b *Bot) cmdWarnAdd(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	guildID := c.GuildID()
	reason := c.reason()
	total, err := b.db.AddWarning(m.User.ID, guildID, reason)
	if err != nil {
		c.logErr("add warning", err)
		c.Reply(true, errEmbed("Couldn't save the warning — check the bot's log."))
		return
	}
	c.replyAndLog(makeEmbed("⚠️ Member Warned",
		fmt.Sprintf("**%s** was warned by %s.\n**Reason:** %s\n**Total warnings:** %d",
			m.User, userMention(c.UserID()), reason, total),
		colourYellow), true)

	// Warning thresholds
	var result *discordgo.MessageEmbed
	switch total {
	case b.cfg.Int("WARN_KICK_THRESHOLD"):
		kickReason := fmt.Sprintf("Auto-kick: reached %d warnings", total)
		if err := b.s.GuildMemberDeleteWithReason(guildID, m.User.ID, kickReason, auditReason(kickReason)); err != nil {
			c.logErr("auto-kick", err)
			result = makeEmbed("⚠️ Auto-Kick Failed",
				fmt.Sprintf("%s hit **%d warnings** but couldn't be kicked.", userMention(m.User.ID), total), colourRed)
		} else {
			result = makeEmbed("👢 Auto-Kicked",
				fmt.Sprintf("%s was automatically kicked after reaching **%d warnings**.", userMention(m.User.ID), total),
				colourRed)
		}
	case b.cfg.Int("WARN_ARREST_THRESHOLD"):
		minutes := int64(b.cfg.Int("WARN_ARREST_DURATION"))
		ok, problem, sentenceID := b.arrestMember(guildID, m, b.s.State.User.ID, minutes*60,
			fmt.Sprintf("Automatic arrest: reached %d warnings", total))
		if ok {
			result = makeEmbed("🚔 Auto-Arrested",
				fmt.Sprintf("%s was automatically jailed for **%s** after reaching **%d warnings**.\n**Sentence:** `#%d`",
					userMention(m.User.ID), humaniseDuration(minutes*60), total, sentenceID),
				colourDarkOrange)
		} else {
			result = makeEmbed("⚠️ Auto-Arrest Failed",
				fmt.Sprintf("%s hit **%d warnings** but couldn't be jailed: %s", userMention(m.User.ID), total, problem),
				colourRed)
		}
	default:
		return
	}
	c.Reply(false, result)
	b.modLog(result)
}

func (b *Bot) cmdRapsheet(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	title := "📋 Rap Sheet — " + m.DisplayName()
	rows, err := b.db.Warnings(m.User.ID, c.GuildID())
	if err != nil {
		c.logErr("read warnings", err)
		c.Reply(true, errEmbed("Couldn't read warnings — check the bot's log."))
		return
	}
	if len(rows) == 0 {
		c.Reply(true, makeEmbed(title, "No warnings on record.", colourGreen))
		return
	}
	e := makeEmbed(title, fmt.Sprintf("**%d warning(s)** on record for %s", len(rows), userMention(m.User.ID)), colourOrange)
	e.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: m.AvatarURL("")}
	// An embed holds at most 25 fields.
	if len(rows) > maxEmbedFields {
		e.Description += fmt.Sprintf(" (showing the oldest %d)", maxEmbedFields)
		rows = rows[:maxEmbedFields]
	}
	for _, w := range rows {
		reason := w.Reason
		if reason == "" {
			reason = noReason
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name: fmt.Sprintf("#%d — %s", w.ID, w.Timestamp), Value: reason,
		})
	}
	c.Reply(true, e)
}

func (b *Bot) cmdWarnClear(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	if err := b.db.ClearWarnings(m.User.ID, c.GuildID()); err != nil {
		c.logErr("clear warnings", err)
		c.Reply(true, errEmbed("Couldn't clear warnings — check the bot's log."))
		return
	}
	c.replyAndLog(makeEmbed("🗑️ Warnings Cleared",
		fmt.Sprintf("All warnings for %s have been cleared by %s.", userMention(m.User.ID), userMention(c.UserID())),
		colourGreen), false)
}

func (b *Bot) cmdWarnRemove(c *Ctx) {
	id := c.Int("warning_id", 0)
	deleted, err := b.db.RemoveWarning(id, c.GuildID())
	if err != nil {
		c.logErr("remove warning", err)
		c.Reply(true, errEmbed("Couldn't remove the warning — check the bot's log."))
		return
	}
	if !deleted {
		c.Reply(true, errEmbed(fmt.Sprintf("No warning with ID `%d` found for this server.", id)))
		return
	}
	c.replyAndLog(makeEmbed("🗑️ Warning Removed",
		fmt.Sprintf("Warning **#%d** was removed by %s.", id, userMention(c.UserID())), colourGreen), false)
}

func (b *Bot) cmdUserinfo(c *Ctx) {
	guildID := c.GuildID()
	var m *discordgo.Member
	if c.opt("member") != nil {
		if m = c.targetMember(); m == nil {
			return
		}
	} else {
		cp := *c.Member()
		cp.GuildID = guildID
		m = &cp
	}

	warnings, err := b.db.Warnings(m.User.ID, guildID)
	if err != nil {
		c.logErr("read warnings", err)
	}
	joined := "Unknown"
	if !m.JoinedAt.IsZero() {
		joined = discordTimestamp(m.JoinedAt.Unix(), "D")
	}
	created := "Unknown"
	if t, err := discordgo.SnowflakeTimestamp(m.User.ID); err == nil {
		created = discordTimestamp(t.Unix(), "D")
	}
	var roles []string
	for _, r := range b.memberRoles(guildID, m) {
		roles = append(roles, roleMention(r.ID))
	}
	rolesText := "None"
	if len(roles) > 0 {
		rolesText = truncate(strings.Join(roles, " "), 1024)
	}
	isBot := "No"
	if m.User.Bot {
		isBot = "Yes"
	}
	colour := b.memberColour(guildID, m)
	if colour == 0 {
		colour = colourBlurple
	}
	channelName := c.i.ChannelID
	if ch := b.channel(c.i.ChannelID); ch != nil {
		channelName = ch.Name
	}

	e := makeEmbed("👤 "+m.DisplayName(), "", colour)
	e.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: m.AvatarURL("")}
	e.Fields = []*discordgo.MessageEmbedField{
		{Name: "Username", Value: m.User.String(), Inline: true},
		{Name: "ID", Value: m.User.ID, Inline: true},
		{Name: "Bot?", Value: isBot, Inline: true},
		{Name: "Joined Server", Value: joined, Inline: true},
		{Name: "Account Created", Value: created, Inline: true},
		{Name: "Warnings", Value: fmt.Sprint(len(warnings)), Inline: true},
		{Name: fmt.Sprintf("Roles (%d)", len(roles)), Value: rolesText},
	}
	e.Footer = &discordgo.MessageEmbedFooter{
		Text: fmt.Sprintf("Requested by %s in #%s", c.Member().User, channelName),
	}
	b.redirectSensitive(c, e)
}

// redirectSensitive posts a sensitive command's output to SENSITIVE_LOG
// (falling back to BIG_BROTHER) instead of the invoking channel, leaving
// only an ephemeral pointer behind.
func (b *Bot) redirectSensitive(c *Ctx, e *discordgo.MessageEmbed) {
	id := b.cfg.ID("SENSITIVE_LOG")
	if id == "" {
		id = b.cfg.ID("BIG_BROTHER")
	}
	ch := b.channel(id)
	if ch == nil {
		c.Reply(true, errEmbed("No sensitive-log channel is configured (set SENSITIVE_LOG in `/setup`) — showing it here instead."))
		c.Reply(true, e)
		return
	}
	if _, err := b.s.ChannelMessageSendEmbed(ch.ID, e); err != nil {
		c.logErr("sensitive log", err)
		c.Reply(true, errEmbed(fmt.Sprintf("Couldn't post to %s (missing permissions?) — showing it here instead.", channelMention(ch.ID))))
		c.Reply(true, e)
		return
	}
	c.Reply(true, okEmbed(fmt.Sprintf("Sent to %s — check there for the output.", channelMention(ch.ID))))
}

var verificationLevels = []string{"None", "Low", "Medium", "High", "Highest"}

func (b *Bot) cmdServerinfo(c *Ctx) {
	g, err := b.s.State.Guild(c.GuildID())
	if err != nil {
		c.logErr("guild state", err)
		c.Reply(true, errEmbed("I couldn't read this server's info."))
		return
	}
	created := "Unknown"
	if t, err := discordgo.SnowflakeTimestamp(g.ID); err == nil {
		created = discordTimestamp(t.Unix(), "D")
	}
	owner := "Unknown"
	if g.OwnerID != "" {
		owner = userMention(g.OwnerID)
	}
	verification := fmt.Sprint(g.VerificationLevel)
	if int(g.VerificationLevel) < len(verificationLevels) {
		verification = verificationLevels[g.VerificationLevel]
	}

	e := makeEmbed(g.Name, "", colourBlurple)
	if g.Icon != "" {
		e.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: g.IconURL("")}
	}
	e.Fields = []*discordgo.MessageEmbedField{
		{Name: "Owner", Value: owner, Inline: true},
		{Name: "Created", Value: created, Inline: true},
		{Name: "Members", Value: fmt.Sprint(g.MemberCount), Inline: true},
		{Name: "Roles", Value: fmt.Sprint(len(g.Roles)), Inline: true},
		{Name: "Channels", Value: fmt.Sprint(len(g.Channels)), Inline: true},
		{Name: "Boost Level", Value: fmt.Sprint(int(g.PremiumTier)), Inline: true},
		{Name: "Boosts", Value: fmt.Sprint(g.PremiumSubscriptionCount), Inline: true},
		{Name: "Verification", Value: verification, Inline: true},
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Server ID: " + g.ID}
	c.Reply(true, e)
}
