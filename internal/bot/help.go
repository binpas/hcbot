package bot

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
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
	{"@BotName <text>", "Chat with the bot in its Markov chat channels."},
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
	if b.env.TTSURL == "" {
		// /say and /voices are registered only with a text to speech worker.
		cats = slices.DeleteFunc(cats, func(c helpCategory) bool { return c.title == "Speech" })
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
		{"Markov Chat", []helpEntry{
			{"/markov status", "Show the size of the chain, its channels and the mode."},
			{"/markov lookup <text>", "Look up a word or phrase in the chain."},
			{"/markov forget <text>", "Remove a word or phrase from the chain."},
			{"/markov blockadd <text>", "Block a word or phrase, and offer to remove it from the chain. `word*` also blocks each word that starts with `word`."},
			{"/markov blockremove <text>", "Unblock a word or phrase."},
			{"/markov blocklist", "List the blocked words and phrases."},
			{"/markov import <file>", "Train from a .txt file (one message per line) or a DiscordChatExporter .json."},
			{"/markov prune", "Remove word steps seen only once, to shrink the database."},
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
		{"User Notes", []helpEntry{
			{"/note add <user> <text>", "Add a private mod note to a member. The bot records who added it and when."},
			{"/note list <user>", "Show a member's notes. Select a note to edit or delete it."},
			{"Apps → " + noteMenuAdd, "Right-click a message to add it to its author's notes."},
			{"Apps → " + noteMenuView, "Right-click a member to show their notes."},
		}},
		{"Videos and Posts", []helpEntry{
			{"/download <url>", "Download a video (YouTube, X, and other sites) and get it in a private reply."},
			{"Apps → " + downloadMenu, "Right-click a message to download the video from its first link."},
			{"/tweetshot <url> [public]", "Make an image of a post on X. Only you see it, unless `public` is true."},
			{"Apps → " + tweetshotMenu, "Right-click a message to make an image of the first X post it links to."},
		}},
		{"Speech", []helpEntry{
			{"/say", "Open a form to make a voice message or an audio clip of a text, and post it in the channel. Clips wait in a queue; each takes about 15–30 seconds."},
			{"/voices", "List the voices for `/say`. Admins can reload the list."},
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
			{"/markov reset", "Delete the whole Markov chain. The blocklist stays."},
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
	b.addComponent("help", b.onHelpComponent)
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
	if b.isMod(c.Member()) {
		embeds, components := b.modHelpPage(c.UserID(), 0)
		c.ReplyComplex(true, embeds, components)
		return
	}
	e := makeEmbed("📖 Command Reference",
		"Every command below is a slash command. Arguments in `<>` are required, `[]` are optional.",
		colourBlurple)
	e.Fields = append(e.Fields, helpField("General", generalCommands, b.helpMention()))
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Ask a moderator if you think you're missing a command here."}
	b.addLegalLinks(e)
	c.Reply(true, e)
}

func (b *Bot) helpMention() func(string) string {
	return strings.NewReplacer("@BotName", userMention(b.s.State.User.ID)).Replace
}

func (b *Bot) addLegalLinks(e *discordgo.MessageEmbed) {
	if links := b.legalLinks(); links != "" {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "\u200b", Value: "-# " + links})
	}
}

// modHelpPage builds one page of the mod help: page 0 is General with a
// list of the sections, and each later page is one section. The buttons
// and the section select carry the invoker, so only they can turn pages.
func (b *Bot) modHelpPage(invoker string, page int) ([]*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	cats := b.modCommandCategories()
	page = max(0, min(page, len(cats)))

	e := makeEmbed("🛡️ Command Reference",
		"Every command below is a slash command. Arguments in `<>` are required, `[]` are optional.",
		colourBlurple)
	if page == 0 {
		e.Fields = append(e.Fields, helpField("General", generalCommands, b.helpMention()))
		titles := make([]string, len(cats))
		for i, cat := range cats {
			titles[i] = fmt.Sprintf("%d. %s", i+2, cat.title)
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  "━━━━━━  Sections  ━━━━━━",
			Value: strings.Join(titles, "\n") + "\n\nUse the buttons or the menu below to see a section.",
		})
	} else {
		cat := cats[page-1]
		e.Fields = append(e.Fields, helpField(cat.title, cat.entries, func(s string) string { return s }))
	}
	e.Footer = &discordgo.MessageEmbedFooter{
		Text: fmt.Sprintf("Page %d/%d · Showing moderator commands because you have the mod role.", page+1, len(cats)+1),
	}
	b.addLegalLinks(e)

	options := []discordgo.SelectMenuOption{{Label: "General", Value: "0", Default: page == 0}}
	for i, cat := range cats {
		options = append(options, discordgo.SelectMenuOption{
			Label: cat.title, Value: strconv.Itoa(i + 1), Default: page == i+1,
		})
	}
	id := func(to string) string { return "help:" + invoker + ":" + to }
	return []*discordgo.MessageEmbed{e}, []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
			MenuType: discordgo.StringSelectMenu, CustomID: id("sel"), Placeholder: "Go to a section", Options: options,
		}}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "◀ Back", Style: discordgo.PrimaryButton, CustomID: id(strconv.Itoa(page - 1)), Disabled: page == 0},
			discordgo.Button{Label: "Next ▶", Style: discordgo.PrimaryButton, CustomID: id(strconv.Itoa(page + 1)), Disabled: page == len(cats)},
		}},
	}
}

// onHelpComponent turns a mod help page: help:<invoker>:<page|sel>.
func (b *Bot) onHelpComponent(c *Ctx) {
	if len(c.Args) < 2 {
		return
	}
	if c.UserID() != c.Args[0] {
		c.Reply(true, errEmbed("This help isn't yours — run `/help` yourself."))
		return
	}
	if !b.isMod(c.Member()) {
		c.Reply(true, errEmbed("You must be a moderator to see these pages."))
		return
	}
	to := c.Args[1]
	if to == "sel" {
		if vals := c.Values(); len(vals) == 1 {
			to = vals[0]
		}
	}
	page, err := strconv.Atoi(to)
	if err != nil {
		return
	}
	c.Update(b.modHelpPage(c.Args[0], page))
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
