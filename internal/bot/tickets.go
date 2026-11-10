package bot

import (
	"bytes"
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

// ticketCategory returns TICKET_CAT when it is a category of the guild.
func (b *Bot) ticketCategory(guildID string) *discordgo.Channel {
	category := b.channel(b.cfg.ID("TICKET_CAT"))
	if category == nil || category.GuildID != guildID || category.Type != discordgo.ChannelTypeGuildCategory {
		return nil
	}
	return category
}

// ticketOverwrites makes a ticket channel visible only to the given users,
// the mod role and the bot. It returns the mod role too (nil when unset).
func (b *Bot) ticketOverwrites(guildID string, userIDs ...string) ([]*discordgo.PermissionOverwrite, *discordgo.Role) {
	view := int64(discordgo.PermissionViewChannel)
	talk := view | discordgo.PermissionSendMessages | discordgo.PermissionReadMessageHistory
	files := int64(discordgo.PermissionAttachFiles | discordgo.PermissionEmbedLinks)
	overwrites := []*discordgo.PermissionOverwrite{
		{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: view},
	}
	for _, id := range userIDs {
		overwrites = append(overwrites, &discordgo.PermissionOverwrite{
			ID: id, Type: discordgo.PermissionOverwriteTypeMember, Allow: talk | files})
	}
	overwrites = append(overwrites, &discordgo.PermissionOverwrite{
		ID: b.s.State.User.ID, Type: discordgo.PermissionOverwriteTypeMember,
		Allow: talk | discordgo.PermissionManageChannels | discordgo.PermissionManageMessages})
	modRole := b.role(guildID, b.cfg.ID("MOD_ROLE"))
	if modRole != nil {
		overwrites = append(overwrites, &discordgo.PermissionOverwrite{
			ID: modRole.ID, Type: discordgo.PermissionOverwriteTypeRole,
			Allow: talk | files | discordgo.PermissionManageMessages,
		})
	}
	return overwrites, modRole
}

// openTicket creates a ticket channel for a member in TICKET_CAT, visible
// only to the member, the mod role and the bot. It returns nil on failure.
func (b *Bot) openTicket(guildID string, m *discordgo.Member) *discordgo.Channel {
	category := b.ticketCategory(guildID)
	if category == nil {
		return nil
	}
	number, err := b.db.NextTicketNumber(guildID, m.User.ID)
	if err != nil {
		log.Printf("open ticket: next number: %v", err)
		return nil
	}
	overwrites, modRole := b.ticketOverwrites(guildID, m.User.ID)
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

// openModTicket creates a mod ticket channel in TICKET_CAT, visible only
// to the given users, the mod role and the bot. It returns the channel
// and the ticket identifier, or nil on failure.
func (b *Bot) openModTicket(guildID string, mod *discordgo.Member, members []db.TicketMember) (*discordgo.Channel, string) {
	category := b.ticketCategory(guildID)
	if category == nil {
		return nil, ""
	}
	number, err := b.db.NextTicketNumber(guildID, db.ModTicketUserID)
	if err != nil {
		log.Printf("open mod ticket: next number: %v", err)
		return nil, ""
	}
	ids := make([]string, len(members))
	mentions := make([]string, len(members))
	for i, m := range members {
		ids[i], mentions[i] = m.UserID, userMention(m.UserID)
	}
	overwrites, _ := b.ticketOverwrites(guildID, ids...)
	ref := db.TicketRefName(db.ModTicketUsername, number)
	ch, err := b.s.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:                 "open-ticket-" + ref,
		Type:                 discordgo.ChannelTypeGuildText,
		ParentID:             category.ID,
		PermissionOverwrites: overwrites,
	}, auditReason("Mod ticket opened by "+mod.User.String()))
	if err != nil {
		log.Printf("open mod ticket: create channel: %v", err)
		return nil, ""
	}
	if _, err := b.db.CreateModTicket(guildID, ch.ID, mod.User.ID, number, members); err != nil {
		log.Printf("open mod ticket: save ticket for #%s: %v", ch.Name, err)
	}
	_, err = b.s.ChannelMessageSend(ch.ID, fmt.Sprintf(
		"Hello %s!\n%s, a moderator, opened this ticket to talk with you.\nOnly you, the moderators and the bot can see this channel.",
		strings.Join(mentions, " "), userMention(mod.User.ID)))
	if err != nil {
		log.Printf("open mod ticket: welcome message in #%s: %v", ch.Name, err)
	}
	return ch, ref
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

	who := "The reporter no longer has access."
	users := []string{t.UserID}
	if t.Kind == "mod" {
		who = "The added users no longer have access."
		members, err := b.db.TicketMembers(t.ID)
		if err != nil {
			log.Printf("close ticket: read users: %v", err)
		}
		users = users[:0]
		for _, m := range members {
			users = append(users, m.UserID)
		}
	}
	for _, id := range users {
		if err := b.s.ChannelPermissionDelete(channelID, id, auditReason("Ticket closed")); err != nil {
			log.Printf("close ticket: remove access of %s: %v", id, err)
		}
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
		fmt.Sprintf("Closed by %s. %s\nMods can archive this with `/ticket archive`.",
			userMention(closedBy.User.ID), who),
		colourDarkGrey)); err != nil {
		log.Printf("close ticket: notice: %v", err)
	}
	b.modLog(makeEmbed("🔒 Ticket Closed",
		fmt.Sprintf("`%s` (%s) was closed by %s.", name, ticketOwner(t), userMention(closedBy.User.ID)),
		colourDarkGrey))
	return true
}

// ticketOwner says who a ticket is for, for the mod log.
func ticketOwner(t *db.Ticket) string {
	if t.Kind == "mod" {
		return "a mod ticket"
	}
	return "opened by **" + t.Username + "**"
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
			if t.Kind == "mod" && !b.isMod(m) {
				return true // only a mod closes a mod ticket
			}
			b.closeTicket(r.ChannelID, m)
			return true
		}
	}
	return false
}

func (b *Bot) registerTickets() {
	b.addGroup(&group{name: "ticket", description: "Manage support tickets (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{
		name:        "ticket open",
		description: "Open a mod ticket and add users to it.",
		perm:        permMod,
		handler:     b.cmdTicketOpen,
	})
	b.addCommand(&command{
		name:        "ticket close",
		description: "Close the ticket in this channel.",
		perm:        permMod,
		handler:     b.cmdTicketClose,
	})
	b.addCommand(&command{
		name:        "ticket archive",
		description: "Save the transcript of this closed ticket, then delete its channel.",
		perm:        permMod,
		handler:     b.cmdTicketArchive,
	})
	b.addCommand(&command{
		name:        "ticket archiveall",
		description: "Save the transcripts of all closed tickets, then delete their channels.",
		perm:        permMod,
		handler:     b.cmdTicketArchiveAll,
	})
	b.addCommand(&command{
		name:        "ticket convertold",
		description: "Save transcripts of the channels in the old Ticket Archive categories, then delete them.",
		perm:        permAdmin,
		handler:     b.cmdTicketConvertOld,
	})
	b.addCommand(&command{
		name:        "ticket list",
		description: "List all tickets of a member (opened by them, or mod tickets they are in).",
		options:     []*discordgo.ApplicationCommandOption{optUser("member", "Member whose tickets to list", true)},
		perm:        permMod,
		handler:     b.cmdTicketList,
	})
	ticketID := optStr("ticket_id", "Ticket identifier, e.g. alice-2 or mod-3 — see /ticket list", true)
	b.addCommand(&command{
		name:        "ticket transcript",
		description: "Get the transcript of a ticket, by username-# or mod-#.",
		options:     []*discordgo.ApplicationCommandOption{ticketID},
		perm:        permMod,
		handler:     b.cmdTicketTranscript,
	})
	b.addCommand(&command{
		name:        "ticket note",
		description: "Add a note to a ticket, by username-# or mod-#.",
		options:     []*discordgo.ApplicationCommandOption{ticketID, optStr("note", "The note to record", true)},
		perm:        permMod,
		handler:     b.cmdTicketNote,
	})
	b.addCommand(&command{
		name:        "ticket notes",
		description: "Show every note recorded for a ticket, by username-# or mod-#.",
		options:     []*discordgo.ApplicationCommandOption{ticketID},
		perm:        permMod,
		handler:     b.cmdTicketNotes,
	})
	b.addCommand(&command{
		name:        "ticket search",
		description: "Search ticket notes and the messages of ticket transcripts.",
		options:     []*discordgo.ApplicationCommandOption{optStr("query", "Text to search for", true)},
		perm:        permMod,
		handler:     b.cmdTicketSearch,
	})
	b.addModal("ticketopen", b.onTicketOpenModal)
	b.addConfirm("ticketarchive", b.onTicketArchiveConfirm)
	b.addConfirm("ticketarchiveall", b.onTicketArchiveAllConfirm)
	b.addConfirm("ticketconvert", b.onTicketConvertConfirm)
	for _, h := range []any{b.onTicketMessageUpdate, b.onTicketMessageDelete, b.onTicketMessageDeleteBulk} {
		b.s.AddHandler(h)
	}
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
	if t == nil || t.Status == "archived" && t.ArchivedAt != "" {
		c.Reply(true, errEmbed("This channel isn't a ticket."))
		return nil
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

// maxTicketUsers is the most users a mod can add to a mod ticket at once
// (the limit of a user select).
const maxTicketUsers = 10

func (b *Bot) cmdTicketOpen(c *Ctx) {
	if b.ticketCategory(c.GuildID()) == nil {
		c.Reply(true, errEmbed("Set the ticket category first: `TICKET_CAT` in `/setup`."))
		return
	}
	c.Modal("ticketopen:new", "Open a mod ticket", modalLabel{
		Label:       "Users to add",
		Description: fmt.Sprintf("These users and the mods can see the ticket. Choose 1 to %d users.", maxTicketUsers),
		Component:   modalUserSelect{CustomID: "users", Min: 1, Max: maxTicketUsers},
	})
}

func (b *Bot) onTicketOpenModal(c *Ctx) {
	ids := c.ModalValues("users")
	if len(ids) == 0 {
		c.Reply(true, errEmbed("Choose at least one user."))
		return
	}
	c.Defer(true)
	var members []db.TicketMember
	var skipped []string
	for _, id := range ids[:min(len(ids), maxTicketUsers)] {
		m := b.resolveMember(c.GuildID(), id)
		switch {
		case m == nil || m.User == nil:
			skipped = append(skipped, userMention(id)+" (not in the server)")
		case m.User.Bot:
			skipped = append(skipped, userMention(id)+" (a bot)")
		default:
			members = append(members, db.TicketMember{UserID: m.User.ID, Username: m.User.Username})
		}
	}
	if len(members) == 0 {
		c.Reply(true, errEmbed("I could not add any of these users: "+strings.Join(skipped, ", ")+"."))
		return
	}
	ch, t := b.openModTicket(c.GuildID(), c.Member(), members)
	if ch == nil {
		c.Reply(true, errEmbed("I couldn't create the ticket. The ticket category may be wrong, or I am missing permissions."))
		return
	}
	var names []string
	for _, m := range members {
		names = append(names, userMention(m.UserID))
	}
	msg := fmt.Sprintf("Opened %s (`%s`) with %s.", channelMention(ch.ID), t, strings.Join(names, ", "))
	if len(skipped) > 0 {
		msg += "\nNot added: " + strings.Join(skipped, ", ") + "."
	}
	c.Reply(true, okEmbed(msg))
	b.modLog(makeEmbed("🎫 Mod Ticket Opened",
		fmt.Sprintf("%s opened %s (`%s`) with %s.", userMention(c.UserID()), channelMention(ch.ID), t, strings.Join(names, ", ")),
		colourTeal))
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
	if !b.env.MessageContent {
		c.Reply(true, errEmbed("Archiving needs the Message Content intent (`ENABLE_MESSAGE_CONTENT_FEATURES`), to read the messages."))
		return
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("📦 Archive this ticket?",
		fmt.Sprintf("I will save the transcript of `%s` and then **delete this channel**.\n"+
			"Read the transcript later with `/ticket transcript %s`.", t.Ref(), t.Ref()), colourOrange)},
		confirmButtons("ticketarchive", c.UserID(), strconv.FormatInt(t.ID, 10)))
}

func (b *Bot) onTicketArchiveConfirm(c *Ctx, extra []string) {
	if len(extra) < 1 {
		return
	}
	id, _ := strconv.ParseInt(extra[0], 10, 64)
	t, err := b.db.TicketByID(id)
	if err != nil || t == nil || t.Status != "closed" {
		c.Update([]*discordgo.MessageEmbed{errEmbed("This ticket is not a closed ticket any more. No changes made.")},
			[]discordgo.MessageComponent{})
		return
	}
	c.DeferUpdate()
	by := c.Member().User.String()
	if err := b.saveTicketTranscript(t); err != nil {
		log.Printf("archive ticket %s: %v", t.Ref(), err)
		c.FinishUpdate(errEmbed("I could not save the transcript, so I did not delete the channel."))
		return
	}
	c.FinishUpdate(okEmbed("Transcript saved. I am deleting this channel now."))
	note := "The channel is deleted."
	if err := b.deleteTicketChannel(t, by); err != nil {
		log.Printf("archive ticket %s: delete channel: %v", t.Ref(), err)
		note = fmt.Sprintf("I could not delete %s.", channelMention(t.ChannelID))
	}
	b.modLog(makeEmbed("📦 Ticket Archived",
		fmt.Sprintf("`%s` was archived by %s. %s\nRead it with `/ticket transcript %s`.", t.Ref(), userMention(c.UserID()), note, t.Ref()),
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
	if !b.env.MessageContent {
		c.Reply(true, errEmbed("Archiving needs the Message Content intent (`ENABLE_MESSAGE_CONTENT_FEATURES`), to read the messages."))
		return
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("📦 Archive all closed tickets?",
		fmt.Sprintf("I will save the transcripts of **%d** closed ticket(s) and then **delete their channels**.", len(closed)),
		colourOrange)}, confirmButtons("ticketarchiveall", c.UserID()))
}

func (b *Bot) onTicketArchiveAllConfirm(c *Ctx, _ []string) {
	c.DeferUpdate()
	closed, err := b.db.ClosedTickets(c.GuildID())
	if err != nil {
		c.logErr("list closed tickets", err)
		c.FinishUpdate(errEmbed("I couldn't read the ticket records."))
		return
	}
	archived, skipped := 0, 0
	var problems []string
	by := c.Member().User.String()
	for _, t := range closed {
		ch := b.channel(t.ChannelID)
		if ch == nil || ch.GuildID != c.GuildID() || ch.Type != discordgo.ChannelTypeGuildText {
			skipped++
			continue
		}
		ok, note := b.archiveTicket(t, by)
		if ok {
			archived++
		} else {
			skipped++
		}
		if !strings.HasPrefix(note, "Saved") {
			problems = append(problems, note)
		}
	}
	msg := fmt.Sprintf("Archived **%d** ticket(s).", archived)
	if skipped > 0 {
		msg += fmt.Sprintf(" Skipped **%d**.", skipped)
	}
	if len(problems) > 0 {
		msg += "\n" + truncate(strings.Join(problems, "\n"), 3000)
	}
	summary := makeEmbed("📦 Archive Complete", msg, colourDarkTeal)
	b.modLog(summary)
	c.FinishUpdate(summary)
}

var ticketArchiveCategory = regexp.MustCompile(`^Ticket Archive \d+$`)

func (b *Bot) cmdTicketConvertOld(c *Ctx) {
	moved, err := b.db.MovedTickets(c.GuildID())
	if err != nil {
		c.logErr("list moved tickets", err)
		c.Reply(true, errEmbed("I couldn't read the ticket records."))
		return
	}
	if !b.env.MessageContent {
		c.Reply(true, errEmbed("This needs the Message Content intent (`ENABLE_MESSAGE_CONTENT_FEATURES`), to read the messages."))
		return
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("📦 Convert the old archived tickets?",
		fmt.Sprintf("**%d** ticket(s) were archived the old way (moved to a `Ticket Archive` category). "+
			"I will save the transcript of each one that still has its channel, and then **delete the channel**. "+
			"Then I delete each `Ticket Archive N` category that is empty.", len(moved)),
		colourOrange)}, confirmButtons("ticketconvert", c.UserID()))
}

func (b *Bot) onTicketConvertConfirm(c *Ctx, _ []string) {
	c.DeferUpdate()
	moved, err := b.db.MovedTickets(c.GuildID())
	if err != nil {
		c.logErr("list moved tickets", err)
		c.FinishUpdate(errEmbed("I couldn't read the ticket records."))
		return
	}
	converted, gone := 0, 0
	var problems []string
	by := c.Member().User.String()
	for _, t := range moved {
		ch := b.channel(t.ChannelID)
		if ch == nil || ch.GuildID != c.GuildID() {
			gone++
			continue
		}
		ok, note := b.archiveTicket(t, by)
		if ok {
			converted++
		}
		if !strings.HasPrefix(note, "Saved") {
			problems = append(problems, note)
		}
	}
	removed := b.removeEmptyArchiveCategories(c.GuildID(), by)
	msg := fmt.Sprintf("Converted **%d** ticket(s). Deleted **%d** empty `Ticket Archive` categories.", converted, removed)
	if gone > 0 {
		msg += fmt.Sprintf(" **%d** ticket(s) had no channel any more; they keep only their notes.", gone)
	}
	if len(problems) > 0 {
		msg += "\n" + truncate(strings.Join(problems, "\n"), 3000)
	}
	summary := makeEmbed("📦 Old Archives Converted", msg, colourDarkTeal)
	b.modLog(summary)
	c.FinishUpdate(summary)
}

// removeEmptyArchiveCategories deletes each empty "Ticket Archive N"
// category and returns how many it deleted.
func (b *Bot) removeEmptyArchiveCategories(guildID, by string) int {
	chans, err := b.s.GuildChannels(guildID)
	if err != nil {
		log.Printf("convert old tickets: list channels: %v", err)
		return 0
	}
	used := map[string]bool{}
	for _, ch := range chans {
		used[ch.ParentID] = true
	}
	removed := 0
	for _, ch := range chans {
		if ch.Type != discordgo.ChannelTypeGuildCategory || !ticketArchiveCategory.MatchString(ch.Name) || used[ch.ID] {
			continue
		}
		if _, err := b.s.ChannelDelete(ch.ID, auditReason("Empty ticket archive category, removed by "+by)); err != nil {
			log.Printf("convert old tickets: delete %s: %v", ch.Name, err)
			continue
		}
		removed++
	}
	return removed
}

func (b *Bot) cmdTicketTranscript(c *Ctx) {
	ref, _ := b.findTicketOpt(c, "Usage: `/ticket transcript <username-#>`, e.g. `/ticket transcript alice-2` or `/ticket transcript mod-3`")
	if ref == nil {
		return
	}
	c.Defer(true)
	if ref.LegacyID != 0 {
		lt, err := b.db.LegacyTranscript(ref.LegacyID)
		if err != nil || lt == nil {
			c.logErr("read legacy transcript", err)
			c.Reply(true, errEmbed("I couldn't read the transcript."))
			return
		}
		name := db.TicketRefName(lt.Username, lt.Number)
		e := makeEmbed("📜 Transcript — "+name, fmt.Sprintf("An older transcript: **%d** message(s), archived %s.",
			lt.MessageCount, lt.ArchivedAt), colourBlurple)
		_ = c.ReplyFiles(true, e, &discordgo.File{Name: name + ".txt", ContentType: "text/plain; charset=utf-8",
			Reader: strings.NewReader(lt.Transcript)})
		return
	}
	t, err := b.db.TicketByID(ref.ID)
	if err != nil || t == nil {
		c.logErr("read ticket", err)
		c.Reply(true, errEmbed("I couldn't read the ticket records."))
		return
	}
	msgs, err := b.db.TicketTranscript(t.ID)
	if err != nil {
		c.logErr("read transcript", err)
		c.Reply(true, errEmbed("I couldn't read the transcript."))
		return
	}
	facts := b.ticketFacts(t)
	files := 0
	for _, m := range msgs {
		files += len(m.Files)
	}
	desc := fmt.Sprintf("**%d** message(s) and **%d** file(s).\n%s", len(msgs), files, strings.Join(facts, "\n"))
	if t.Status != "archived" {
		desc += "\nThe ticket is not archived yet, so the transcript can be incomplete."
	}
	page := b.ticketTranscriptHTML("Ticket "+t.Ref(), facts, msgs)
	_ = c.ReplyFiles(true, makeEmbed("📜 Transcript — "+t.Ref(), truncate(desc, 4000), colourBlurple),
		&discordgo.File{Name: t.Ref() + ".html", ContentType: "text/html; charset=utf-8", Reader: bytes.NewReader(page)})
}

// ticketFacts are lines about a ticket for its transcript: who, and when.
func (b *Bot) ticketFacts(t *db.Ticket) []string {
	var out []string
	if t.Kind == "mod" {
		line := "A mod ticket"
		if t.OpenedBy != "" {
			line += ", opened by user " + t.OpenedBy
		}
		out = append(out, line+".")
		if members, err := b.db.TicketMembers(t.ID); err == nil && len(members) > 0 {
			var names []string
			for _, m := range members {
				names = append(names, fmt.Sprintf("%s (%s)", m.Username, m.UserID))
			}
			out = append(out, "Users: "+strings.Join(names, ", "))
		}
	} else {
		out = append(out, fmt.Sprintf("Opened by %s (%s).", t.Username, t.UserID))
	}
	out = append(out, "Opened "+t.CreatedAt+" UTC.")
	if t.ClosedAt != "" {
		out = append(out, "Closed "+t.ClosedAt+" UTC.")
	}
	if t.ArchivedAt != "" {
		out = append(out, "Archived "+t.ArchivedAt+" UTC.")
	}
	return out
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
		var where string
		switch {
		case t.LegacyID != 0 || t.ArchivedAt != "":
			where = "Transcript: `/ticket transcript " + t.Ref() + "`"
		case b.channel(t.ChannelID) != nil:
			where = channelMention(t.ChannelID)
		default:
			where = "*channel deleted*"
		}
		closed := ""
		if t.ClosedAt != "" {
			closed = " • closed " + t.ClosedAt
		}
		icon, ok := ticketStatusIcons[t.Status]
		if !ok {
			icon = "•"
		}
		kind := ""
		if t.Kind == "mod" {
			kind = " (mod ticket)"
		}
		opened := "Opened " + cmpOr(t.OpenedAt, "at an unknown time")
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("%s %s — %s%s", icon, t.Ref(), t.Status, kind),
			Value: fmt.Sprintf("%s\n%s%s", where, opened, closed),
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

// ticketSearchMax is the most notes, and the most messages, that a search
// shows: with 200 characters each, the embed stays under Discord's 6000.
const ticketSearchMax = 8

func (b *Bot) cmdTicketSearch(c *Ctx) {
	query, _ := c.Str("query")
	notes, err := b.db.SearchTicketNotes(c.GuildID(), query, ticketSearchMax)
	if err != nil {
		c.logErr("search notes", err)
		c.Reply(true, errEmbed("I couldn't search the tickets."))
		return
	}
	hits, err := b.db.SearchTicketMessages(c.GuildID(), query, ticketSearchMax)
	if err != nil {
		c.logErr("search messages", err)
		c.Reply(true, errEmbed("I couldn't search the tickets."))
		return
	}
	if len(notes) == 0 && len(hits) == 0 {
		c.Reply(true, makeEmbed("🔍 No Matches", fmt.Sprintf("No ticket notes or messages matched `%s`.", query), colourGreyple))
		return
	}
	e := makeEmbed("🔍 Ticket Search", fmt.Sprintf("**%d** note(s) and **%d** message(s) match `%s`", len(notes), len(hits), query),
		colourBlurple)
	for _, n := range notes {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("🗒️ %s-%d (note)", n.Username, n.Number),
			Value: truncate(n.Note, 200) + "\n*" + n.CreatedAt + "*",
		})
	}
	for _, h := range hits {
		who := h.AuthorName
		if h.LegacyID != 0 {
			who = "older transcript"
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("💬 %s — %s", h.Ref(), who),
			Value: truncate(textAround(h.Text, query, 200), 200) + "\n*" + h.SentAt + "*",
		})
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Use /ticket transcript <ticket-id> to read a whole ticket."}
	c.Reply(true, e)
}

// textAround returns about n characters of text around the first match of
// query (case is ignored), with "…" where it cut.
func textAround(text, query string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	at := strings.Index(strings.ToLower(text), strings.ToLower(query))
	start := 0
	if at > 0 {
		start = max(len([]rune(text[:at]))-n/3, 0)
	}
	end := min(start+n-2, len(runes))
	out := string(runes[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}
