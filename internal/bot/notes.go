package bot

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/db"
)

// User notes: mods keep private notes on members, with /note, or by a
// right-click on a message (Apps → Add to user notes). Discord sends the
// text of that one message in the interaction, so the Message Content
// intent is not needed. All replies are ephemeral.
//
// Custom IDs:
//
//	note:list:<user>:<page>   show a page of a member's notes
//	note:sel:<user>:<page>    select menu: show one note
//	note:edit:<id>:<page>     open the edit modal (notemodal:<id>:<page>)
//	note:del:<id>:<page>      ask to delete (confirm action "notedel")

const (
	maxNoteLen    = 1500
	notesPerPage  = 5
	maxNoteSource = 4000 // Discord's own message length limit (with Nitro)

	noteMenuAdd  = "Add to user notes"
	noteMenuView = "View user notes"
)

func (b *Bot) registerNotes() {
	text := optStr("text", "The note", true)
	text.MaxLength = maxNoteLen
	b.addGroup(&group{name: "note", description: "Manage mod notes on members (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{
		name:        "note add",
		description: "Add a note to a member.",
		options:     []*discordgo.ApplicationCommandOption{optUser("user", "Member to add a note to", true), text},
		perm:        permMod,
		handler:     b.cmdNoteAdd,
	})
	b.addCommand(&command{
		name:        "note list",
		description: "Show a member's notes, and edit or delete them.",
		options:     []*discordgo.ApplicationCommandOption{optUser("user", "Member whose notes to show", true)},
		perm:        permMod,
		handler:     b.cmdNoteList,
	})
	b.addCommand(&command{
		name:        noteMenuAdd,
		kind:        discordgo.MessageApplicationCommand,
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdNoteFromMessage,
	})
	b.addCommand(&command{
		name:        noteMenuView,
		kind:        discordgo.UserApplicationCommand,
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdNoteViewUser,
	})
	b.addComponent("note", b.onNoteComponent)
	b.addModal("notemodal", b.onNoteModal)
	b.addConfirm("notedel", b.onNoteDeleteConfirm)
}

func (b *Bot) cmdNoteAdd(c *Ctx) {
	u := c.User("user")
	text, _ := c.Str("text")
	text = strings.TrimSpace(text)
	if u == nil || text == "" {
		c.Reply(true, errEmbed("Usage: `/note add <user> <text>`"))
		return
	}
	b.addNote(c, u.ID, truncate(text, maxNoteLen), nil)
}

func (b *Bot) cmdNoteFromMessage(c *Ctx) {
	msg := c.TargetMessage()
	if msg == nil || msg.Author == nil || msg.WebhookID != "" || msg.Author.Bot {
		c.Reply(true, errEmbed("I can only add a member's message to their notes, not a message from a bot or webhook."))
		return
	}
	src := &db.NoteSource{
		ChannelID: cmp.Or(msg.ChannelID, c.i.ChannelID),
		MessageID: msg.ID,
		Text:      truncate(noteSourceText(msg), maxNoteSource),
	}
	if !msg.Timestamp.IsZero() {
		src.Time = msg.Timestamp.UTC().Format(db.SentenceTimeFormat)
	}
	b.addNote(c, msg.Author.ID, "", src)
}

// noteSourceText is the text kept from a message: its content, then a line
// for each attachment and sticker.
func noteSourceText(m *discordgo.Message) string {
	lines := []string{}
	if s := strings.TrimSpace(m.Content); s != "" {
		lines = append(lines, s)
	}
	for _, a := range m.Attachments {
		lines = append(lines, "📎 "+a.Filename)
	}
	for _, s := range m.StickerItems {
		lines = append(lines, "🏷️ Sticker: "+s.Name)
	}
	if len(lines) == 0 && len(m.Embeds) > 0 {
		lines = append(lines, "*(embeds only)*")
	}
	if len(lines) == 0 {
		lines = append(lines, "*(no text)*")
	}
	return strings.Join(lines, "\n")
}

func (b *Bot) addNote(c *Ctx, userID, text string, src *db.NoteSource) {
	id, err := b.db.AddUserNote(c.GuildID(), userID, c.UserID(), text, src)
	if err != nil {
		c.logErr("add note", err)
		c.Reply(true, errEmbed("I couldn't save the note — check the bot's log."))
		return
	}
	n, ok, err := b.db.UserNote(c.GuildID(), id)
	if err != nil || !ok {
		c.logErr("read note", err)
		c.Reply(true, okEmbed(fmt.Sprintf("Note #%d added for %s.", id, userMention(userID))))
		return
	}
	b.logNote(c, "🗒️ Note Added", colourBlurple,
		fmt.Sprintf("%s added note #%d about %s.", userMention(c.UserID()), id, userMention(userID)), n)
	embeds, components := b.noteView(c.GuildID(), n, 0)
	c.ReplyComplex(true, append([]*discordgo.MessageEmbed{
		okEmbed(fmt.Sprintf("Note #%d added for %s.", id, userMention(userID))),
	}, embeds...), components)
}

func (b *Bot) cmdNoteList(c *Ctx) {
	if u := c.User("user"); u != nil {
		b.replyNoteList(c, u.ID)
	}
}

func (b *Bot) cmdNoteViewUser(c *Ctx) {
	if u := c.TargetUser(); u != nil {
		b.replyNoteList(c, u.ID)
	}
}

func (b *Bot) replyNoteList(c *Ctx, userID string) {
	embeds, components, err := b.noteListView(c.GuildID(), userID, 0)
	if err != nil {
		c.logErr("read notes", err)
		c.Reply(true, errEmbed("I couldn't read the notes — check the bot's log."))
		return
	}
	c.ReplyComplex(true, embeds, components)
}

// noteTime renders a stored UTC time as a Discord timestamp tag.
func noteTime(s, style string) string {
	t, err := time.ParseInLocation(db.SentenceTimeFormat, s, time.UTC)
	if err != nil {
		return s
	}
	return discordTimestamp(t.Unix(), style)
}

// noteBody renders a note: its text, the quoted message it came from (if
// any), and who added and edited it. textMax and quoteMax limit the note
// text and the quote, so the body fits in an embed field or description.
func (b *Bot) noteBody(guildID string, n db.UserNote, textMax, quoteMax int) string {
	var sb strings.Builder
	if n.Note != "" {
		sb.WriteString(truncateEllipsis(n.Note, textMax) + "\n")
	}
	if n.MessageID != "" {
		for _, line := range strings.Split(truncateEllipsis(n.MessageText, quoteMax), "\n") {
			sb.WriteString("> " + line + "\n")
		}
		fmt.Fprintf(&sb, "[Jump to message](%s) in %s", jumpToMessage(guildID, n.ChannelID, n.MessageID), channelMention(n.ChannelID))
		if n.MessageTime != "" {
			sb.WriteString(", sent " + noteTime(n.MessageTime, "f"))
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "-# Added by %s %s", userMention(n.AuthorID), noteTime(n.CreatedAt, "f"))
	if n.EditedBy != "" {
		fmt.Fprintf(&sb, " · edited by %s %s", userMention(n.EditedBy), noteTime(n.EditedAt, "R"))
	}
	return sb.String()
}

// truncateEllipsis shortens s to at most n runes, with "…" at the end when
// it cut text.
func truncateEllipsis(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return truncate(s, n-1) + "…"
}

// noteListView renders one page of a member's notes, newest first, with a
// menu to select a note and page buttons.
func (b *Bot) noteListView(guildID, userID string, page int) ([]*discordgo.MessageEmbed, []discordgo.MessageComponent, error) {
	notes, err := b.db.UserNotes(guildID, userID)
	if err != nil {
		return nil, nil, err
	}
	if len(notes) == 0 {
		e := makeEmbed("🗒️ User Notes", fmt.Sprintf("No notes on record for %s.", userMention(userID)), colourGreen)
		return []*discordgo.MessageEmbed{e}, []discordgo.MessageComponent{}, nil
	}
	for i, j := 0, len(notes)-1; i < j; i, j = i+1, j-1 {
		notes[i], notes[j] = notes[j], notes[i]
	}
	pages := (len(notes) + notesPerPage - 1) / notesPerPage
	page = max(0, min(page, pages-1))
	shown := notes[page*notesPerPage : min(len(notes), (page+1)*notesPerPage)]

	e := makeEmbed("🗒️ User Notes",
		fmt.Sprintf("**%d note(s)** on record for %s, newest first.", len(notes), userMention(userID)), colourBlurple)
	e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Page %d/%d · Select a note to edit or delete it.", page+1, pages)}
	if u, err := b.s.State.Member(guildID, userID); err == nil && u.User != nil {
		e.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: u.AvatarURL("")}
	}
	var options []discordgo.SelectMenuOption
	for _, n := range shown {
		// Each field is at most about 1000 characters, so 5 fit in 6000.
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("#%d · %s UTC", n.ID, n.CreatedAt[:min(16, len(n.CreatedAt))]),
			Value: truncateEllipsis(b.noteBody(guildID, n, 450, 300), 1024),
		})
		desc := strings.TrimSpace(strings.SplitN(cmp.Or(n.Note, n.MessageText), "\n", 2)[0])
		options = append(options, discordgo.SelectMenuOption{
			Label: fmt.Sprintf("Note #%d", n.ID), Value: strconv.FormatInt(n.ID, 10),
			Description: truncateEllipsis(cmp.Or(desc, "(no text)"), 100),
		})
	}
	id := func(action string, p int) string { return fmt.Sprintf("note:%s:%s:%d", action, userID, p) }
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
			MenuType: discordgo.StringSelectMenu, CustomID: id("sel", page), Placeholder: "Select a note", Options: options,
		}}},
	}
	if pages > 1 {
		components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "◀ Back", Style: discordgo.PrimaryButton, CustomID: id("list", page-1), Disabled: page == 0},
			discordgo.Button{Label: "Next ▶", Style: discordgo.PrimaryButton, CustomID: id("list", page+1), Disabled: page == pages-1},
		}})
	}
	return []*discordgo.MessageEmbed{e}, components, nil
}

// noteView renders one note in full, with Edit, Delete, and All notes
// buttons. page is the list page to go back to.
func (b *Bot) noteView(guildID string, n db.UserNote, page int) ([]*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	e := makeEmbed(fmt.Sprintf("🗒️ Note #%d", n.ID),
		fmt.Sprintf("About %s\n\n%s", userMention(n.UserID), b.noteBody(guildID, n, maxNoteLen, 2000)), colourBlurple)
	edit := "Edit"
	if n.Note == "" {
		edit = "Add comment"
	}
	return []*discordgo.MessageEmbed{e}, []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: edit, Style: discordgo.PrimaryButton, Emoji: &discordgo.ComponentEmoji{Name: "✏️"},
			CustomID: fmt.Sprintf("note:edit:%d:%d", n.ID, page)},
		discordgo.Button{Label: "Delete", Style: discordgo.DangerButton, Emoji: &discordgo.ComponentEmoji{Name: "🗑️"},
			CustomID: fmt.Sprintf("note:del:%d:%d", n.ID, page)},
		discordgo.Button{Label: "All notes", Style: discordgo.SecondaryButton,
			CustomID: fmt.Sprintf("note:list:%s:%d", n.UserID, page)},
	}}}
}

// loadNote reads the note in a custom ID field. When it is gone, it
// answers on the message and returns false.
func (b *Bot) loadNote(c *Ctx, idField string) (db.UserNote, bool) {
	id, _ := strconv.ParseInt(idField, 10, 64)
	n, ok, err := b.db.UserNote(c.GuildID(), id)
	if err != nil {
		c.logErr("read note", err)
		c.Reply(true, errEmbed("I couldn't read the note — check the bot's log."))
		return n, false
	}
	if !ok {
		c.Update([]*discordgo.MessageEmbed{errEmbed(fmt.Sprintf("Note #%d no longer exists.", id))}, []discordgo.MessageComponent{})
		return n, false
	}
	return n, true
}

func (b *Bot) onNoteComponent(c *Ctx) {
	if len(c.Args) < 3 {
		return
	}
	if !b.isMod(c.Member()) {
		c.Reply(true, errEmbed("You must be a moderator to use that."))
		return
	}
	action, target := c.Args[0], c.Args[1]
	page, _ := strconv.Atoi(c.Args[2])
	switch action {
	case "list":
		embeds, components, err := b.noteListView(c.GuildID(), target, page)
		if err != nil {
			c.logErr("read notes", err)
			c.Reply(true, errEmbed("I couldn't read the notes — check the bot's log."))
			return
		}
		c.Update(embeds, components)
	case "sel":
		vals := c.Values()
		if len(vals) != 1 {
			return
		}
		if n, ok := b.loadNote(c, vals[0]); ok {
			c.Update(b.noteView(c.GuildID(), n, page))
		}
	case "edit":
		n, ok := b.loadNote(c, target)
		if !ok {
			return
		}
		title, label := fmt.Sprintf("Edit note #%d", n.ID), "Note"
		if n.MessageID != "" {
			label = "Comment (the saved message does not change)"
		}
		c.Modal(fmt.Sprintf("notemodal:%d:%d", n.ID, page), title, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.TextInput{CustomID: "text", Label: label, Style: discordgo.TextInputParagraph,
				Value: n.Note, MaxLength: maxNoteLen, Required: n.MessageID == ""},
		}})
	case "del":
		n, ok := b.loadNote(c, target)
		if !ok {
			return
		}
		ask := makeEmbed(fmt.Sprintf("Delete note #%d?", n.ID), "This cannot be undone.", colourOrange)
		view, _ := b.noteView(c.GuildID(), n, page)
		c.Update(append([]*discordgo.MessageEmbed{ask}, view...),
			confirmButtons("notedel", c.UserID(), strconv.FormatInt(n.ID, 10), strconv.Itoa(page)))
	}
}

func (b *Bot) onNoteModal(c *Ctx) {
	if len(c.Args) < 2 {
		return
	}
	if !b.isMod(c.Member()) {
		c.Reply(true, errEmbed("You must be a moderator to use that."))
		return
	}
	n, ok := b.loadNote(c, c.Args[0])
	if !ok {
		return
	}
	page, _ := strconv.Atoi(c.Args[1])
	text := truncate(strings.TrimSpace(c.ModalValue("text")), maxNoteLen)
	if text == "" && n.MessageID == "" {
		c.Reply(true, errEmbed("A note needs some text. To remove it, use **Delete**."))
		return
	}
	if _, err := b.db.EditUserNote(c.GuildID(), n.ID, c.UserID(), text); err != nil {
		c.logErr("edit note", err)
		c.Reply(true, errEmbed("I couldn't save the note — check the bot's log."))
		return
	}
	if n, ok = b.loadNote(c, c.Args[0]); !ok {
		return
	}
	b.logNote(c, "✏️ Note Edited", colourYellow,
		fmt.Sprintf("%s edited note #%d about %s.", userMention(c.UserID()), n.ID, userMention(n.UserID)), n)
	embeds, components := b.noteView(c.GuildID(), n, page)
	c.Update(append([]*discordgo.MessageEmbed{okEmbed(fmt.Sprintf("Note #%d saved.", n.ID))}, embeds...), components)
}

func (b *Bot) onNoteDeleteConfirm(c *Ctx, extra []string) {
	if len(extra) < 2 {
		return
	}
	if !b.isMod(c.Member()) {
		c.Reply(true, errEmbed("You must be a moderator to use that."))
		return
	}
	n, ok := b.loadNote(c, extra[0])
	if !ok {
		return
	}
	page, _ := strconv.Atoi(extra[1])
	if _, err := b.db.DeleteUserNote(c.GuildID(), n.ID); err != nil {
		c.logErr("delete note", err)
		c.Update([]*discordgo.MessageEmbed{errEmbed("I couldn't delete the note — check the bot's log.")}, []discordgo.MessageComponent{})
		return
	}
	b.logNote(c, "🗑️ Note Deleted", colourRed,
		fmt.Sprintf("%s deleted note #%d about %s.", userMention(c.UserID()), n.ID, userMention(n.UserID)), n)
	embeds, components, err := b.noteListView(c.GuildID(), n.UserID, page)
	if err != nil {
		c.logErr("read notes", err)
		embeds, components = nil, []discordgo.MessageComponent{}
	}
	c.Update(append([]*discordgo.MessageEmbed{okEmbed(fmt.Sprintf("Note #%d deleted.", n.ID))}, embeds...), components)
}

// logNote sends a note change to the mod log.
func (b *Bot) logNote(c *Ctx, title string, colour int, what string, n db.UserNote) {
	b.modLog(makeEmbed(title, what+"\n\n"+b.noteBody(c.GuildID(), n, 1500, 1500), colour))
}
