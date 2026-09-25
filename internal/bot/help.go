package bot

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"
)

type helpEntry struct{ usage, desc string }

type helpCategory struct {
	title   string
	entries []helpEntry
}

var generalCommands = []helpEntry{
	{"/timeout [minutes]", "Mute yourself for a break. Defaults to 10 minutes, max 1440."},
	{"@BotName <trigger>", "Posts the stored response for a trigger created by a mod."},
	{"/help", "Show the commands available to you."},
}

// modCommandCategories is built per call so the warning thresholds shown
// match the current /setup values.
func (b *Bot) modCommandCategories() []helpCategory {
	cats := b.allModCommandCategories()
	if !b.env.MessageContent {
		// /autoreact is not registered without the Message Content option.
		cats = slices.DeleteFunc(cats, func(c helpCategory) bool { return c.title == "Autoreacts" })
	}
	return cats
}

func (b *Bot) allModCommandCategories() []helpCategory {
	return []helpCategory{
		{"Punishment", []helpEntry{
			{"/ban <member> [reason]", "Permanently ban a member."},
			{"/tempban <member> [duration] [reason]", "Ban for `duration` minutes (default 60), then auto-unban."},
			{"/kick <member> [reason]", "Kick a member from the server."},
			{"/mute <member> [duration] [reason]", "Timeout a member. Duration in minutes, default 10."},
			{"/unmute <member>", "Remove an active timeout."},
		}},
		{"Warnings", []helpEntry{
			{"/warn add <member> [reason]", fmt.Sprintf(
				"Warn a member. Auto-arrests at **%d** warnings (%d min), auto-kicks at **%d**.",
				b.cfg.Int("WARN_ARREST_THRESHOLD"), b.cfg.Int("WARN_ARREST_DURATION"), b.cfg.Int("WARN_KICK_THRESHOLD"))},
			{"/warn history <member>", "Show a member's full warning history. Alias: `/rapsheet`."},
			{"/warn remove <id>", "Remove a single warning by its ID (shown in the warning history)."},
			{"/warn clear <member>", "Wipe all warnings for a member."},
		}},
		{"Server Management", []helpEntry{
			{"/purge [amount]", "Delete up to 100 recent messages. Defaults to 10."},
			{"/motd <member>", "Grant the Member Of The Day role; auto-removed after 24 h."},
		}},
		{"Triggers", []helpEntry{
			{"/trigger add <name> <value>", "Create a trigger, or reset it to a single response."},
			{"/trigger addvalue <name> <value>", "Add another possible response to a trigger."},
			{"/trigger removevalue <name> <value>", "Remove a single response from a trigger's pool."},
			{"/trigger list", "List the names of every defined trigger."},
			{"/trigger info <name>", "List all responses currently stored for a trigger."},
			{"/trigger delete <name>", "Remove a trigger and all of its responses."},
			{"/trigger import", "Import a single Carl-bot tag/autoresponder as a trigger via a popup."},
		}},
		{"Autoreacts", []helpEntry{
			{"/autoreact add <emote> <phrase>", "React with an emote whenever a word or phrase is said."},
			{"/autoreact edit <emote> <phrase>", "Change the emote used for an existing autoreact."},
			{"/autoreact remove <phrase>", "Remove an autoreact."},
			{"/autoreact list", "List all autoreacts."},
		}},
		{"Jail", []helpEntry{
			{"/jail arrest <member> [time] [reason]", "Jail a member. Omit `time` for an indefinite sentence. Alias: `/arrest`."},
			{"/jail list", "List everyone currently in jail, with time served and remaining."},
			{"/jail amend <id> [time] [reason]", "Update a sentence's length and/or reason."},
			{"/jail release <member>", "Release a member early and log it to their rap sheet."},
			{"/jail setup", "Re-apply jail role permission overwrites across every channel."},
			{"/jail audit", "Check (and optionally fix) whether the jail role can still see channels it shouldn't."},
		}},
		{"Permissions", []helpEntry{
			{"/permsreport", "Generate a full channel-permissions report as a text file."},
			{"/permtemplate save <channel> <name>", "Save a channel's permission overwrites as a reusable template."},
			{"/permtemplate list", "List saved permission templates."},
			{"/permtemplate apply <name> <channel>", "Apply a saved permission template to a channel."},
			{"/channelarchive [channel]", "Clone a channel as the new live one, and archive the original."},
		}},
		{"Tickets", []helpEntry{
			{"/ticket close", "Close the ticket in this channel. Users can also self-close with " + ticketCloseEmoji + "."},
			{"/ticket archive", "Archive the current closed ticket by moving it to the Ticket Archive category."},
			{"/ticket archiveall", "Bulk-archive every closed ticket in the server."},
			{"/ticket list <member>", "List every ticket a member has opened."},
			{"/ticket note <username-#> <note>", "Add a mod note to a ticket."},
			{"/ticket notes <username-#>", "Show every note recorded for a ticket."},
			{"/ticket search <query>", "Search ticket notes."},
		}},
		{"Reaction Roles", []helpEntry{
			{"/reactionrole create", "Start the reaction-role creation wizard."},
			{"/reactionrole list", "List all reaction roles and whether they have been posted."},
			{"/reactionrole edit <name>", "Edit a reaction role. If posted, the live message updates."},
			{"/reactionrole post <name>", "Post or repost a saved reaction role."},
			{"/reactionrole delete <name>", "Delete a reaction role and remove its message."},
		}},
		{"Info", []helpEntry{
			{"/userinfo [member]", "Show a member's join date, account age, roles, and warning count. Defaults to yourself."},
			{"/serverinfo", "Show an overview of the server: owner, member count, boosts, roles, etc."},
			{"/readme", "Browse the bot's README in a paginated, ephemeral embed."},
		}},
		{"Admin only", []helpEntry{
			{"/setup", "Configure channel/role/emote/number settings via a private, paginated embed UI."},
			{"/db backup", "Create a database backup now."},
			{"/db list [filename]", "List saved database backups, or download one by name."},
			{"/db delete <filename>", "Delete a saved database backup."},
			{"/assets export", "Export all custom emojis and stickers to a zip file."},
			{"/assets import <file>", "Import emojis/stickers from a zip made by /assets export."},
		}},
	}
}

func (b *Bot) registerHelp() {
	b.addCommand(&command{
		name:        "help",
		description: "Show the commands available to you.",
		handler:     b.cmdHelp,
	})
}

func helpField(title string, entries []helpEntry, replace func(string) string) *discordgo.MessageEmbedField {
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = fmt.Sprintf("**%s** — %s", replace(e.usage), e.desc)
	}
	return &discordgo.MessageEmbedField{
		Name:  fmt.Sprintf("━━━━━━  %s  ━━━━━━", title),
		Value: strings.Join(lines, "\n"),
	}
}

func (b *Bot) cmdHelp(c *Ctx) {
	isMod := b.isMod(c.Member())
	title := "📖 Command Reference"
	if isMod {
		title = "🛡️ Command Reference"
	}
	e := makeEmbed(title,
		"Every command below is a slash command. Arguments in `<>` are required, `[]` are optional.",
		colourBlurple)

	mention := strings.NewReplacer("@BotName", userMention(b.s.State.User.ID)).Replace
	e.Fields = append(e.Fields, helpField("General", generalCommands, mention))

	if isMod {
		same := func(s string) string { return s }
		for _, cat := range b.modCommandCategories() {
			e.Fields = append(e.Fields, helpField(cat.title, cat.entries, same))
		}
		e.Footer = &discordgo.MessageEmbedFooter{Text: "Showing moderator commands because you have the mod role."}
	} else {
		e.Footer = &discordgo.MessageEmbedFooter{Text: "Ask a moderator if you think you're missing a command here."}
	}
	if links := b.legalLinks(); links != "" {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "\u200b", Value: "-# " + links})
	}
	c.Reply(true, e)
}

// legalLinks returns the Privacy Policy and Terms of Service links for the
// bottom of /help, or "" when neither is set to an http(s) URL. An embed
// footer can't hold links, so they go in a last field as small text.
func (b *Bot) legalLinks() string {
	var links []string
	for _, l := range []struct{ label, key string }{
		{"Privacy Policy", "PRIVACY_URL"}, {"Terms of Service", "TERMS_URL"},
	} {
		raw := strings.TrimSpace(b.cfg.Str(l.key))
		if u, err := url.Parse(raw); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" &&
			!strings.ContainsAny(raw, "()<> ") {
			links = append(links, fmt.Sprintf("[%s](%s)", l.label, raw))
		}
	}
	return strings.Join(links, " · ")
}
