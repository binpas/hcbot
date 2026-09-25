package bot

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/config"
	"github.com/binpas/hcbot/internal/db"
)

const (
	colourTeal     = 0x1ABC9C
	colourDarkTeal = 0x11806A

	// maxChannelsPerCategory is Discord's own limit.
	maxChannelsPerCategory = 50
)

var usernameUnsafe = regexp.MustCompile(`[^a-z0-9\-]`)

// sanitiseUsername makes a username safe for a channel name.
func sanitiseUsername(name string) string {
	cleaned := usernameUnsafe.ReplaceAllString(strings.ReplaceAll(strings.ToLower(name), " ", "-"), "")
	if len(cleaned) > 24 {
		cleaned = cleaned[:24]
	}
	if cleaned == "" {
		return "user"
	}
	return cleaned
}

// dm sends a direct message. Closed DMs are ignored.
func (b *Bot) dm(userID, text string) {
	ch, err := b.s.UserChannelCreate(userID)
	if err == nil {
		_, err = b.s.ChannelMessageSend(ch.ID, text)
	}
	if err != nil {
		log.Printf("dm %s: %v", userID, err)
	}
}

func jumpURL(guildID, channelID string) string {
	return fmt.Sprintf("https://discord.com/channels/%s/%s", guildID, channelID)
}

// openTicket creates a ticket channel for a member in TICKET_CAT, visible
// only to the member, the mod role and the bot. It returns nil on failure.
func (b *Bot) openTicket(guildID string, m *discordgo.Member) *discordgo.Channel {
	category := b.channel(b.cfg.ID("TICKET_CAT"))
	if category == nil || category.GuildID != guildID || category.Type != discordgo.ChannelTypeGuildCategory {
		return nil
	}
	number, err := b.db.NextTicketNumber(guildID, m.User.ID)
	if err != nil {
		log.Printf("open ticket: next number: %v", err)
		return nil
	}

	view := int64(discordgo.PermissionViewChannel)
	talk := view | discordgo.PermissionSendMessages | discordgo.PermissionReadMessageHistory
	files := int64(discordgo.PermissionAttachFiles | discordgo.PermissionEmbedLinks)
	overwrites := []*discordgo.PermissionOverwrite{
		{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: view},
		{ID: m.User.ID, Type: discordgo.PermissionOverwriteTypeMember, Allow: talk | files},
		{ID: b.s.State.User.ID, Type: discordgo.PermissionOverwriteTypeMember,
			Allow: talk | discordgo.PermissionManageChannels | discordgo.PermissionManageMessages},
	}
	modRole := b.role(guildID, b.cfg.ID("MOD_ROLE"))
	if modRole != nil {
		overwrites = append(overwrites, &discordgo.PermissionOverwrite{
			ID: modRole.ID, Type: discordgo.PermissionOverwriteTypeRole,
			Allow: talk | files | discordgo.PermissionManageMessages,
		})
	}

	ch, err := b.s.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:                 fmt.Sprintf("open-ticket-%s-%d", sanitiseUsername(m.User.Username), number),
		Type:                 discordgo.ChannelTypeGuildText,
		ParentID:             category.ID,
		PermissionOverwrites: overwrites,
	}, auditReason("Support ticket for "+m.User.String()))
	if err != nil {
		log.Printf("open ticket: create channel: %v", err)
		return nil
	}
	if _, err := b.db.CreateTicket(guildID, ch.ID, m.User.ID, m.User.Username, number); err != nil {
		log.Printf("open ticket: save ticket for #%s: %v", ch.Name, err)
	}

	modMention := "moderator"
	if modRole != nil {
		modMention = roleMention(modRole.ID)
	}
	prompt, err := b.s.ChannelMessageSend(ch.ID, fmt.Sprintf(
		"Hello! %s\nWhat's the issue?\nA %s will help you shortly.\n(React to this message with %s to close this ticket)",
		userMention(m.User.ID), modMention, ticketCloseEmoji))
	if err == nil {
		err = b.s.MessageReactionAdd(ch.ID, prompt.ID, ticketCloseEmoji)
	}
	if err != nil {
		log.Printf("open ticket: welcome message in #%s: %v", ch.Name, err)
	}
	return ch
}

// closeTicket removes the reporter's access and renames the channel to
// closed-*. It returns false when the channel isn't an open ticket.
func (b *Bot) closeTicket(channelID string, closedBy *discordgo.Member) bool {
	t, err := b.db.TicketByChannel(channelID)
	if err != nil {
		log.Printf("close ticket: %v", err)
		return false
	}
	if t == nil || t.Status != "open" {
		return false
	}

	if err := b.s.ChannelPermissionDelete(channelID, t.UserID, auditReason("Ticket closed")); err != nil {
		log.Printf("close ticket: remove reporter access: %v", err)
	}
	name := ""
	if ch := b.channel(channelID); ch != nil {
		name = ch.Name
	}
	if rest, ok := strings.CutPrefix(name, "open-"); ok {
		name = "closed-" + rest
		if _, err := b.s.ChannelEdit(channelID, &discordgo.ChannelEdit{Name: name},
			auditReason("Ticket closed by "+closedBy.User.String())); err != nil {
			log.Printf("close ticket: rename: %v", err)
		}
	}

	if err := b.db.CloseTicket(channelID); err != nil {
		log.Printf("close ticket: save: %v", err)
	}
	if _, err := b.s.ChannelMessageSendEmbed(channelID, makeEmbed("🔒 Ticket Closed",
		fmt.Sprintf("Closed by %s. The reporter no longer has access.\nMods can archive this with `/ticket archive`.",
			userMention(closedBy.User.ID)),
		colourDarkGrey)); err != nil {
		log.Printf("close ticket: notice: %v", err)
	}
	b.modLog(makeEmbed("🔒 Ticket Closed",
		fmt.Sprintf("`%s` (opened by **%s**) was closed by %s.", name, t.Username, userMention(closedBy.User.ID)),
		colourDarkGrey))
	return true
}

// modOnlyOverwrites hides a channel from @everyone and shows it to MOD_ROLE.
func (b *Bot) modOnlyOverwrites(guildID string) []*discordgo.PermissionOverwrite {
	ows := []*discordgo.PermissionOverwrite{
		{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
	}
	if mod := b.role(guildID, b.cfg.ID("MOD_ROLE")); mod != nil {
		ows = append(ows, &discordgo.PermissionOverwrite{ID: mod.ID, Type: discordgo.PermissionOverwriteTypeRole,
			Allow: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionReadMessageHistory})
	}
	return ows
}

// rolloverCategory returns the newest "<prefix> N" category with room for
// one more channel, or creates "<prefix> N+1" with the given overwrites.
// It reads the channel list from the API, not the cache, so a category
// made or filled a moment ago is counted correctly.
func (b *Bot) rolloverCategory(guildID, prefix string, overwrites []*discordgo.PermissionOverwrite) *discordgo.Channel {
	chans, err := b.s.GuildChannels(guildID)
	if err != nil {
		log.Printf("%s: list channels: %v", prefix, err)
		return nil
	}
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + ` (\d+)$`)
	var newest *discordgo.Channel
	highest := 0
	for _, ch := range chans {
		if ch.Type != discordgo.ChannelTypeGuildCategory {
			continue
		}
		if m := pattern.FindStringSubmatch(ch.Name); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > highest {
				highest, newest = n, ch
			}
		}
	}
	if newest != nil {
		children := 0
		for _, ch := range chans {
			if ch.ParentID == newest.ID {
				children++
			}
		}
		if children < maxChannelsPerCategory {
			return newest
		}
	}
	cat, err := b.s.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:                 fmt.Sprintf("%s %d", prefix, highest+1),
		Type:                 discordgo.ChannelTypeGuildCategory,
		PermissionOverwrites: overwrites,
	}, auditReason(prefix+" category rollover"))
	if err != nil {
		log.Printf("%s: create category: %v", prefix, err)
		return nil
	}
	return cat
}

// archiveTicket moves a closed ticket's channel into the newest Ticket
// Archive category and syncs it to that category's permissions (mods only).
// Nothing is read from the channel, so this needs no Message Content intent.
func (b *Bot) archiveTicket(guildID, channelID string, t *db.Ticket, archivedBy string) (bool, string) {
	cat := b.rolloverCategory(guildID, "Ticket Archive", b.modOnlyOverwrites(guildID))
	if cat == nil {
		return false, "I couldn't find or create a Ticket Archive category (missing permissions?)."
	}
	name := fmt.Sprintf("%s-%d", sanitiseUsername(t.Username), t.Number)
	// Discord has no "sync" flag; syncing means copying the category's overwrites.
	ows := cat.PermissionOverwrites
	if len(ows) == 0 {
		ows = b.modOnlyOverwrites(guildID)
	}
	if _, err := b.s.ChannelEdit(channelID, &discordgo.ChannelEdit{
		Name: name, ParentID: cat.ID, PermissionOverwrites: ows,
	}, auditReason("Ticket archived by "+archivedBy)); err != nil {
		log.Printf("archive ticket: move #%s: %v", name, err)
		return false, fmt.Sprintf("I don't have permission to move/rename %s.", channelMention(channelID))
	}
	if err := b.db.MarkTicketArchived(t.ID); err != nil {
		log.Printf("archive ticket: save: %v", err)
	}
	return true, fmt.Sprintf("Moved to **%s** as `%s`.", cat.Name, name)
}

// onTicketReaction handles 📩 on the mod support message and 🔒 in a
// ticket. It reports whether the reaction was a ticket reaction.
func (b *Bot) onTicketReaction(r *discordgo.MessageReactionAdd, m *discordgo.Member) bool {
	emoji := r.Emoji.APIName()
	supportMsg := b.cfg.Raw(config.ModSupportMsgKey)

	if emoji == ticketOpenEmoji && supportMsg != "" && r.MessageID == supportMsg {
		// Always clear the user's reaction so the embed stays clean.
		if err := b.s.MessageReactionRemove(r.ChannelID, r.MessageID, emoji, m.User.ID); err != nil {
			log.Printf("ticket: clear reaction: %v", err)
		}

		existing, err := b.db.OpenTicket(r.GuildID, m.User.ID)
		if err != nil {
			log.Printf("ticket: find open ticket: %v", err)
			return true
		}
		if existing != nil {
			if ch := b.channel(existing.ChannelID); ch != nil {
				b.dm(m.User.ID, "You already have an open ticket: "+jumpURL(r.GuildID, ch.ID))
				return true
			}
			// The channel was deleted; let them open a fresh one.
			if err := b.db.CloseTicket(existing.ChannelID); err != nil {
				log.Printf("ticket: close deleted ticket: %v", err)
			}
		}

		ch := b.openTicket(r.GuildID, m)
		if ch == nil {
			b.dm(m.User.ID, "I couldn't create your ticket — the ticket category may be "+
				"misconfigured or I'm missing permissions. Please contact a mod directly.")
			return true
		}
		b.modLog(makeEmbed("🎫 Ticket Opened",
			fmt.Sprintf("%s opened %s.", userMention(m.User.ID), channelMention(ch.ID)), colourTeal))
		return true
	}

	if emoji == ticketCloseEmoji {
		if t, err := b.db.TicketByChannel(r.ChannelID); err == nil && t != nil && t.Status == "open" {
			b.closeTicket(r.ChannelID, m)
			return true
		}
	}
	return false
}

func (b *Bot) registerTickets() {
	b.addGroup(&group{name: "ticket", description: "Manage support tickets (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{
		name:        "ticket close",
		description: "Close the ticket in this channel.",
		perm:        permMod,
		handler:     b.cmdTicketClose,
	})
	b.addCommand(&command{
		name:        "ticket archive",
		description: "Archive this closed ticket by moving it into the Ticket Archive category.",
		perm:        permMod,
		handler:     b.cmdTicketArchive,
	})
	b.addCommand(&command{
		name:        "ticket archiveall",
		description: "Archive every closed ticket in the server.",
		perm:        permMod,
		handler:     b.cmdTicketArchiveAll,
	})
	b.addCommand(&command{
		name:        "ticket list",
		description: "List all tickets opened by a member.",
		options:     []*discordgo.ApplicationCommandOption{optUser("member", "Member whose tickets to list", true)},
		perm:        permMod,
		handler:     b.cmdTicketList,
	})
	ticketID := optStr("ticket_id", "Ticket identifier, e.g. alice-2 — see /ticket list", true)
	b.addCommand(&command{
		name:        "ticket note",
		description: "Add a note to a ticket, by username-#.",
		options:     []*discordgo.ApplicationCommandOption{ticketID, optStr("note", "The note to record", true)},
		perm:        permMod,
		handler:     b.cmdTicketNote,
	})
	b.addCommand(&command{
		name:        "ticket notes",
		description: "Show every note recorded for a ticket, by username-#.",
		options:     []*discordgo.ApplicationCommandOption{ticketID},
		perm:        permMod,
		handler:     b.cmdTicketNotes,
	})
	b.addCommand(&command{
		name:        "ticket search",
		description: "Search ticket notes.",
		options:     []*discordgo.ApplicationCommandOption{optStr("query", "Text to search for across ticket notes", true)},
		perm:        permMod,
		handler:     b.cmdTicketSearch,
	})
}

// channelTicket returns the ticket in the command's channel, or replies
// with an error and returns nil.
func (b *Bot) channelTicket(c *Ctx) *db.Ticket {
	t, err := b.db.TicketByChannel(c.i.ChannelID)
	if err != nil {
		c.logErr("find ticket", err)
		c.Reply(true, errEmbed("I couldn't read the ticket records."))
		return nil
	}
	if t == nil {
		c.Reply(true, errEmbed("This channel isn't a ticket."))
	}
	return t
}

func (b *Bot) cmdTicketClose(c *Ctx) {
	t := b.channelTicket(c)
	if t == nil {
		return
	}
	if t.Status != "open" {
		c.Reply(true, errEmbed("That ticket is already closed."))
		return
	}
	c.Defer(true) // a rename can be slow
	if b.closeTicket(c.i.ChannelID, c.Member()) {
		c.Reply(true, okEmbed("Ticket closed."))
	} else {
		c.Reply(true, errEmbed("That ticket is already closed."))
	}
}

func (b *Bot) cmdTicketArchive(c *Ctx) {
	t := b.channelTicket(c)
	if t == nil {
		return
	}
	if t.Status != "closed" {
		c.Reply(true, errEmbed("Close the ticket first with `/ticket close`."))
		return
	}
	c.Defer(true)
	ok, note := b.archiveTicket(c.GuildID(), c.i.ChannelID, t, c.Member().User.String())
	if !ok {
		c.Reply(true, errEmbed(note))
		return
	}
	c.Reply(true, okEmbed(note))
	b.modLog(makeEmbed("📦 Ticket Archived",
		fmt.Sprintf("Ticket #%d (opened by **%s**) archived by %s. %s", t.Number, t.Username, userMention(c.UserID()), note),
		colourDarkTeal))
}

func (b *Bot) cmdTicketArchiveAll(c *Ctx) {
	closed, err := b.db.ClosedTickets(c.GuildID())
	if err != nil {
		c.logErr("list closed tickets", err)
		c.Reply(true, errEmbed("I couldn't read the ticket records."))
		return
	}
	if len(closed) == 0 {
		c.Reply(true, makeEmbed("Nothing to archive", "There are no closed tickets.", colourGreyple))
		return
	}
	c.Reply(true, makeEmbed("📦 Archiving…",
		fmt.Sprintf("Processing **%d** closed ticket(s). This may take a moment.", len(closed)), colourBlurple))

	archived, skipped := 0, 0
	by := c.Member().User.String()
	for _, t := range closed {
		ch := b.channel(t.ChannelID)
		if ch == nil || ch.GuildID != c.GuildID() || ch.Type != discordgo.ChannelTypeGuildText {
			skipped++
			continue
		}
		if ok, _ := b.archiveTicket(c.GuildID(), ch.ID, t, by); ok {
			archived++
		} else {
			skipped++
		}
	}
	msg := fmt.Sprintf("Archived **%d** ticket(s).", archived)
	if skipped > 0 {
		msg += fmt.Sprintf(" Skipped **%d**.", skipped)
	}
	summary := makeEmbed("📦 Archive Complete", msg, colourDarkTeal)
	b.modLog(summary)
	c.Reply(true, summary)
}

var ticketStatusIcons = map[string]string{"open": "🟢", "closed": "🔴", "archived": "📦"}

func (b *Bot) cmdTicketList(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	title := "🎫 Tickets — " + m.DisplayName()
	rows, err := b.db.TicketHistory(c.GuildID(), m.User.ID)
	if err != nil {
		c.logErr("ticket history", err)
		c.Reply(true, errEmbed("I couldn't read the ticket records."))
		return
	}
	if len(rows) == 0 {
		c.Reply(true, makeEmbed(title, "No tickets on record.", colourGreen))
		return
	}
	e := makeEmbed(title, fmt.Sprintf("**%d** ticket(s) on record for %s", len(rows), userMention(m.User.ID)), colourBlurple)
	e.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: m.AvatarURL("")}
	for _, t := range rows[:min(len(rows), maxEmbedFields)] {
		where := "*archived before per-category archiving — no channel, check /ticket notes*"
		if t.ChannelID != "" {
			where = "*channel deleted*"
			if ch := b.channel(t.ChannelID); ch != nil {
				where = channelMention(ch.ID)
			}
		}
		closed := ""
		if t.ClosedAt != "" {
			closed = " • closed " + t.ClosedAt
		}
		icon, ok := ticketStatusIcons[t.Status]
		if !ok {
			icon = "•"
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("%s Ticket #%d — %s", icon, t.Number, t.Status),
			Value: fmt.Sprintf("%s\nOpened %s%s", where, t.OpenedAt, closed),
		})
	}
	if len(rows) > maxEmbedFields {
		e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Showing %d of %d tickets.", maxEmbedFields, len(rows))}
	}
	c.Reply(true, e)
}

// parseTicketID splits "alice-2" into "alice" and 2.
func parseTicketID(s string) (username string, number int, ok bool) {
	s = strings.TrimSpace(s)
	i := strings.LastIndex(s, "-")
	if i <= 0 {
		return "", 0, false
	}
	digits := s[i+1:]
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return "", 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return "", 0, false
	}
	return s[:i], n, true
}

// findTicketOpt resolves the ticket_id option, or replies with usage and returns nil.
func (b *Bot) findTicketOpt(c *Ctx, usage string) (*db.TicketRef, int) {
	raw, _ := c.Str("ticket_id")
	username, number, ok := parseTicketID(raw)
	if !ok {
		c.Reply(true, errEmbed(usage))
		return nil, 0
	}
	ref, err := b.db.FindTicket(c.GuildID(), username, number)
	if err != nil {
		c.logErr("find ticket", err)
		c.Reply(true, errEmbed("I couldn't read the ticket records."))
		return nil, 0
	}
	if ref == nil {
		c.Reply(true, errEmbed(fmt.Sprintf("No ticket found for `%s`. Check `/ticket list <member>` for the right number.", raw)))
	}
	return ref, number
}

func (b *Bot) cmdTicketNote(c *Ctx) {
	ref, number := b.findTicketOpt(c, "Usage: `/ticket note <username-#> <note>`, e.g. `/ticket note alice-2 Resolved via DM.`")
	if ref == nil {
		return
	}
	note, _ := c.Str("note")
	if err := b.db.AddTicketNote(c.GuildID(), ref.UserID, ref.Username, number, c.UserID(), note); err != nil {
		c.logErr("add note", err)
		c.Reply(true, errEmbed("I couldn't save the note."))
		return
	}
	c.Reply(true, okEmbed(fmt.Sprintf("Note added to `%s-%d`.", ref.Username, number)))
}

func (b *Bot) cmdTicketNotes(c *Ctx) {
	ref, number := b.findTicketOpt(c, "Usage: `/ticket notes <username-#>`, e.g. `/ticket notes alice-2`")
	if ref == nil {
		return
	}
	notes, err := b.db.TicketNotes(c.GuildID(), ref.UserID, number)
	if err != nil {
		c.logErr("read notes", err)
		c.Reply(true, errEmbed("I couldn't read the notes."))
		return
	}
	if len(notes) == 0 {
		c.Reply(true, makeEmbed(fmt.Sprintf("🗒️ %s-%d", ref.Username, number), "No notes recorded yet.", colourGreyple))
		return
	}
	e := makeEmbed(fmt.Sprintf("🗒️ Notes — %s-%d", ref.Username, number), "", colourBlurple)
	for _, n := range notes[:min(len(notes), maxEmbedFields)] {
		author := "unknown"
		if n.AuthorID != "" {
			author = userMention(n.AuthorID)
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name: n.CreatedAt, Value: truncate(n.Note, 1000) + "\n*by " + author + "*",
		})
	}
	c.Reply(true, e)
}

func (b *Bot) cmdTicketSearch(c *Ctx) {
	query, _ := c.Str("query")
	results, err := b.db.SearchTicketNotes(c.GuildID(), query, 10)
	if err != nil {
		c.logErr("search notes", err)
		c.Reply(true, errEmbed("I couldn't search the notes."))
		return
	}
	if len(results) == 0 {
		c.Reply(true, makeEmbed("🔍 No Matches", fmt.Sprintf("No ticket notes matched `%s`.", query), colourGreyple))
		return
	}
	e := makeEmbed("🔍 Ticket Note Search", fmt.Sprintf("**%d** match(es) for `%s`", len(results), query), colourBlurple)
	for _, n := range results {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("🎫 %s-%d", n.Username, n.Number),
			Value: truncate(n.Note, 300) + "\n*" + n.CreatedAt + "*",
		})
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Use /ticket notes <username-#> to see every note on a ticket."}
	c.Reply(true, e)
}
