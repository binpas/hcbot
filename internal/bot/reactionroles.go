package bot

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/db"
)

// The reaction role wizard keeps its draft in pending_actions (the text
// alone can be 1800 characters, too big for a custom ID). Custom IDs:
//
//	rrw:<action>:<invoker>:<draft id>     buttons and the channel select
//	rrwm:<field>:<invoker>:<draft id>     modal submits
//	rrpost:<invoker>:<rr id>:<expiry>     "Post Now" after saving a new one

const (
	wizardTimeout  = 10 * time.Minute
	postNowTimeout = 5 * time.Minute
)

var (
	leadingCustomEmojiRE = regexp.MustCompile(`^<a?:\w+:\d+>`)
	snowflakeRE          = regexp.MustCompile(`(\d{15,25})`)
	channelURLRE         = regexp.MustCompile(`discord(?:app)?\.com/channels/(\d{15,25})/(\d{15,25})`)
)

// extractID pulls a snowflake out of a raw ID, a mention, or a channel link.
func extractID(text string) string {
	if m := channelURLRE.FindStringSubmatch(text); m != nil {
		return m[2]
	}
	if m := snowflakeRE.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// parseRolePair reads one "<emote> <role>" line. The role can be a
// mention, an ID, or a name (ignoring case).
func (b *Bot) parseRolePair(guildID, line string) (emoji string, role *discordgo.Role, ok bool) {
	line = strings.TrimSpace(line)
	var rest string
	if m := leadingCustomEmojiRE.FindString(line); m != "" {
		emoji, rest = m, strings.TrimSpace(line[len(m):])
	} else {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return "", nil, false
		}
		emoji = parts[0]
		rest = strings.TrimSpace(strings.TrimPrefix(line, emoji))
	}
	if rest == "" {
		return "", nil, false
	}
	if id := extractID(rest); id != "" {
		role = b.role(guildID, id)
	}
	if role == nil {
		if g, err := b.s.State.Guild(guildID); err == nil {
			for _, r := range g.Roles {
				if strings.EqualFold(r.Name, rest) {
					role = r
					break
				}
			}
		}
	}
	return emoji, role, role != nil
}

// pairLines renders pairs as "emoji — @role" lines.
func (b *Bot) pairLines(guildID string, pairs []db.RolePair) []string {
	lines := make([]string, len(pairs))
	for i, p := range pairs {
		who := fmt.Sprintf("*deleted role (%s)*", p.RoleID)
		if b.role(guildID, p.RoleID) != nil {
			who = roleMention(p.RoleID)
		}
		lines[i] = p.Emoji + " — " + who
	}
	return lines
}

// publishReactionRole posts (or edits) the reaction role message and syncs
// its reactions: it adds missing ones and clears ones no longer in the pairs.
func (b *Bot) publishReactionRole(guildID string, rr *db.ReactionRole, pairs []db.RolePair) (bool, string) {
	ch := b.channel(rr.ChannelID)
	if ch == nil || ch.GuildID != guildID || ch.Type != discordgo.ChannelTypeGuildText {
		return false, "That channel no longer exists."
	}
	desc := strings.TrimSpace(rr.Text)
	if len(pairs) > 0 {
		desc += "\n\n" + strings.Join(b.pairLines(guildID, pairs), "\n")
	}
	embed := makeEmbed("", desc, colourBlurple)

	var msg *discordgo.Message
	if rr.MessageID != "" {
		old, err := b.s.ChannelMessage(ch.ID, rr.MessageID)
		if err == nil {
			msg, err = b.s.ChannelMessageEditEmbed(ch.ID, old.ID, embed)
			if msg != nil {
				msg.Reactions = old.Reactions
			}
		}
		var rest *discordgo.RESTError
		switch {
		case err == nil:
		case errors.As(err, &rest) && rest.Message != nil && rest.Message.Code == discordgo.ErrCodeUnknownMessage:
			msg = nil // deleted: post a fresh one
		default:
			log.Printf("reaction role %q: edit message: %v", rr.Name, err)
			return false, fmt.Sprintf("I can't edit messages in %s.", channelMention(ch.ID))
		}
	}
	if msg == nil {
		var err error
		if msg, err = b.s.ChannelMessageSendEmbed(ch.ID, embed); err != nil {
			log.Printf("reaction role %q: post: %v", rr.Name, err)
			return false, fmt.Sprintf("I can't post in %s.", channelMention(ch.ID))
		}
	}
	if err := b.db.SetReactionRoleMessage(rr.ID, ch.ID, msg.ID); err != nil {
		log.Printf("reaction role %q: save message: %v", rr.Name, err)
	}

	wanted := make([]string, len(pairs))
	for i, p := range pairs {
		wanted[i] = p.Emoji
	}
	var present, failed []string
	for _, r := range msg.Reactions {
		present = append(present, r.Emoji.MessageFormat())
		if !slices.Contains(wanted, r.Emoji.MessageFormat()) {
			if err := b.s.MessageReactionsRemoveEmoji(ch.ID, msg.ID, r.Emoji.APIName()); err != nil {
				log.Printf("reaction role %q: clear %s: %v", rr.Name, r.Emoji.APIName(), err)
			}
		}
	}
	for _, e := range wanted {
		if !slices.Contains(present, e) {
			if err := b.s.MessageReactionAdd(ch.ID, msg.ID, reactionEmoji(e)); err != nil {
				failed = append(failed, e)
			}
		}
	}
	note := fmt.Sprintf("Posted in %s.", channelMention(ch.ID))
	if len(failed) > 0 {
		note += fmt.Sprintf(" I couldn't react with %s — I may not have access to those custom emotes.", strings.Join(failed, " "))
	}
	return true, note
}

// rrDraft is the wizard's state.
type rrDraft struct {
	RRID         int64         `json:"rr_id"`
	Name         string        `json:"name"`
	ChannelID    string        `json:"channel_id"`
	Pairs        []db.RolePair `json:"pairs"`
	Text         string        `json:"text"`
	MessageID    string        `json:"message_id"`
	OldChannelID string        `json:"old_channel_id"`
}

type rrWizard struct {
	b       *Bot
	guildID string
	invoker string
	id      string // pending_actions row
	d       rrDraft
}

func (w *rrWizard) embed() *discordgo.MessageEmbed {
	title := "🎭 New Reaction Role"
	if w.d.RRID != 0 {
		title = "🎭 Edit Reaction Role"
	}
	e := makeEmbed(title, "Fill in each section below, then Save.", colourBlurple)
	field := func(name, value string, inline bool) {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: name, Value: value, Inline: inline})
	}
	field("Name", cmp.Or(w.d.Name, "*not set*"), true)
	channel := "*not set*"
	if ch := w.b.channel(w.d.ChannelID); ch != nil {
		channel = channelMention(ch.ID)
	}
	field("Channel", channel, true)
	if len(w.d.Pairs) > 0 {
		field(fmt.Sprintf("Pairs (%d)", len(w.d.Pairs)), truncate(strings.Join(w.b.pairLines(w.guildID, w.d.Pairs), "\n"), 1024), false)
	} else {
		field("Pairs", "*none set*", false)
	}
	preview := w.d.Text
	if len([]rune(preview)) > 500 {
		preview = truncate(preview, 500) + "…"
	}
	field("Message preview", cmp.Or(preview, "*not set*"), false)
	return e
}

func (w *rrWizard) cid(action string) string {
	return fmt.Sprintf("rrw:%s:%s:%s", action, w.invoker, w.id)
}

func (w *rrWizard) components() []discordgo.MessageComponent {
	row := func(c ...discordgo.MessageComponent) discordgo.MessageComponent {
		return discordgo.ActionsRow{Components: c}
	}
	one := 1
	return []discordgo.MessageComponent{
		row(discordgo.Button{Label: "Edit Name", Style: discordgo.SecondaryButton, CustomID: w.cid("name")}),
		row(discordgo.SelectMenu{MenuType: discordgo.ChannelSelectMenu, CustomID: w.cid("chan"), Placeholder: "Set channel",
			ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText}, MinValues: &one, MaxValues: 1}),
		row(discordgo.Button{Label: "Edit Emote/Role Pairs", Style: discordgo.SecondaryButton, CustomID: w.cid("pairs")}),
		row(discordgo.Button{Label: "Edit Message Text", Style: discordgo.SecondaryButton, CustomID: w.cid("text")}),
		row(
			discordgo.Button{Label: "Save", Style: discordgo.SuccessButton, Emoji: &discordgo.ComponentEmoji{Name: "💾"}, CustomID: w.cid("save")},
			discordgo.Button{Label: "Cancel", Style: discordgo.DangerButton, Emoji: &discordgo.ComponentEmoji{Name: "❌"}, CustomID: w.cid("cancel")},
		),
	}
}

// startWizard stores a draft and shows the wizard.
func (b *Bot) startWizard(c *Ctx, d rrDraft) {
	data, _ := json.Marshal(d)
	id, err := b.db.SavePending("rrwizard", c.UserID(), string(data), time.Now().Add(wizardTimeout))
	if err != nil {
		c.dbFailed("save wizard", err)
		return
	}
	w := &rrWizard{b: b, guildID: c.GuildID(), invoker: c.UserID(), id: strconv.FormatInt(id, 10), d: d}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{w.embed()}, w.components())
}

// loadWizard reads the draft for a click or modal submit. It answers and
// returns nil when the click is someone else's or the draft has expired.
func (b *Bot) loadWizard(c *Ctx) *rrWizard {
	if len(c.Args) < 3 {
		return nil
	}
	invoker, id := c.Args[1], c.Args[2]
	if c.UserID() != invoker {
		c.Reply(true, errEmbed("This wizard isn't yours."))
		return nil
	}
	n, _ := strconv.ParseInt(id, 10, 64)
	payload, ok, err := b.db.LoadPending(n, "rrwizard", invoker)
	w := &rrWizard{b: b, guildID: c.GuildID(), invoker: invoker, id: id}
	if err == nil && ok {
		err = json.Unmarshal([]byte(payload), &w.d)
	}
	if err != nil {
		c.logErr("load wizard", err)
	}
	if err != nil || !ok {
		c.Update([]*discordgo.MessageEmbed{errEmbed("This wizard has expired. Run the command again.")}, []discordgo.MessageComponent{})
		return nil
	}
	return w
}

// save stores the changed draft (restarting its timeout) and redraws the wizard.
func (w *rrWizard) save(c *Ctx) {
	data, _ := json.Marshal(w.d)
	n, _ := strconv.ParseInt(w.id, 10, 64)
	if err := w.b.db.UpdatePending(n, string(data), time.Now().Add(wizardTimeout)); err != nil {
		c.dbFailed("save wizard", err)
		return
	}
	c.Update([]*discordgo.MessageEmbed{w.embed()}, w.components())
}

func (b *Bot) onWizardComponent(c *Ctx) {
	w := b.loadWizard(c)
	if w == nil {
		return
	}
	modal := func(field, title, label string, style discordgo.TextInputStyle, value string, maxLen int) {
		c.Modal(fmt.Sprintf("rrwm:%s:%s:%s", field, w.invoker, w.id), title,
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
				CustomID: "value", Label: label, Style: style, Value: value, MaxLength: maxLen, Required: true,
			}}})
	}
	switch c.Args[0] {
	case "name":
		modal("name", "Reaction Role Name", "Name (used to reference it later)", discordgo.TextInputShort, w.d.Name, 100)
	case "pairs":
		lines := make([]string, len(w.d.Pairs))
		for i, p := range w.d.Pairs {
			lines[i] = p.Emoji + " " + p.RoleID
			if b.role(w.guildID, p.RoleID) != nil {
				lines[i] = p.Emoji + " " + roleMention(p.RoleID)
			}
		}
		modal("pairs", "Emote / Role Pairs", "One pair per line: <emote> <role>", discordgo.TextInputParagraph,
			strings.Join(lines, "\n"), 2000)
	case "text":
		modal("text", "Reaction Role Message", "Message text (pair list added below it)", discordgo.TextInputParagraph, w.d.Text, 1800)
	case "chan":
		if v := c.Values(); len(v) == 1 {
			w.d.ChannelID = v[0]
		}
		w.save(c)
	case "cancel":
		n, _ := strconv.ParseInt(w.id, 10, 64)
		if err := b.db.DeletePending(n); err != nil {
			c.logErr("delete wizard", err)
		}
		c.Update([]*discordgo.MessageEmbed{errEmbed("Cancelled. No changes were made.")}, []discordgo.MessageComponent{})
	case "save":
		b.saveWizard(c, w)
	}
}

func (b *Bot) onWizardModal(c *Ctx) {
	w := b.loadWizard(c)
	if w == nil {
		return
	}
	value := c.ModalValue("value")
	switch c.Args[0] {
	case "name":
		name := strings.TrimSpace(value)
		existing, err := b.db.ReactionRoleByName(w.guildID, name)
		if err != nil {
			c.dbFailed("find reaction role", err)
			return
		}
		if existing != nil && existing.ID != w.d.RRID {
			c.Reply(true, errEmbed(fmt.Sprintf("A reaction role named `%s` already exists.", name)))
			return
		}
		w.d.Name = name
		w.save(c)
	case "text":
		w.d.Text = strings.TrimSpace(value)
		w.save(c)
	case "pairs":
		top := b.botTopRole(w.guildID)
		var pairs []db.RolePair
		var problems []string
		seen := map[string]bool{}
		for _, line := range strings.Split(value, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			emoji, role, ok := b.parseRolePair(w.guildID, line)
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("Couldn't read: `%s`", strings.TrimSpace(line)))
			case seen[emoji]:
				problems = append(problems, emoji+" used more than once — keeping the first.")
			case !roleBelow(role, top):
				problems = append(problems, fmt.Sprintf("Can't assign %s — it's above my highest role.", roleMention(role.ID)))
			default:
				seen[emoji] = true
				pairs = append(pairs, db.RolePair{Emoji: emoji, RoleID: role.ID})
			}
		}
		if len(pairs) == 0 {
			c.Reply(true, errEmbed("No valid pairs found. Use `<emote> <role>`, one per line."))
			return
		}
		w.d.Pairs = pairs
		w.save(c)
		if len(problems) > 0 {
			c.Reply(true, errEmbed("Some lines were skipped:\n"+strings.Join(problems[:min(len(problems), 10)], "\n")))
		}
	}
}

func (b *Bot) saveWizard(c *Ctx, w *rrWizard) {
	d := w.d
	if d.Name == "" || d.ChannelID == "" || len(d.Pairs) == 0 || d.Text == "" {
		c.Reply(true, errEmbed("Set the name, channel, at least one pair, and the message text before saving."))
		return
	}
	if d.RRID == 0 {
		existing, err := b.db.ReactionRoleByName(w.guildID, d.Name)
		if err != nil {
			c.dbFailed("find reaction role", err)
			return
		}
		if existing != nil {
			c.Reply(true, errEmbed(fmt.Sprintf("A reaction role named `%s` already exists.", d.Name)))
			return
		}
	}
	id, err := b.db.SaveReactionRole(d.RRID, w.guildID, d.Name, d.ChannelID, d.Text, d.Pairs)
	if err != nil {
		c.dbFailed("save reaction role", err)
		return
	}
	n, _ := strconv.ParseInt(w.id, 10, 64)
	if err := b.db.DeletePending(n); err != nil {
		c.logErr("delete wizard", err)
	}

	messageID := d.MessageID
	if messageID != "" && d.ChannelID != d.OldChannelID {
		// Moved to another channel: remove the old message; it is posted fresh.
		if err := b.s.ChannelMessageDelete(d.OldChannelID, messageID); err != nil {
			log.Printf("reaction role %q: delete old message: %v", d.Name, err)
		}
		if err := b.db.SetReactionRoleMessage(id, d.ChannelID, ""); err != nil {
			c.logErr("clear message", err)
		}
		messageID = ""
	}

	if messageID != "" {
		rr := &db.ReactionRole{ID: id, GuildID: w.guildID, Name: d.Name, ChannelID: d.ChannelID, MessageID: messageID, Text: d.Text}
		if ok, note := b.publishReactionRole(w.guildID, rr, d.Pairs); ok {
			c.Update([]*discordgo.MessageEmbed{okEmbed("Saved. " + note)}, []discordgo.MessageComponent{})
		} else {
			c.Update([]*discordgo.MessageEmbed{errEmbed(note)}, []discordgo.MessageComponent{})
		}
		return
	}
	expiry := time.Now().Add(postNowTimeout).Unix()
	c.Update([]*discordgo.MessageEmbed{okEmbed(fmt.Sprintf(
		"Saved `%s`. Post it to %s now, or later with `/reactionrole post %s`.", d.Name, channelMention(d.ChannelID), d.Name))},
		[]discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.Button{
			Label: "Post Now", Style: discordgo.SuccessButton, Emoji: &discordgo.ComponentEmoji{Name: "📮"},
			CustomID: fmt.Sprintf("rrpost:%s:%d:%d", w.invoker, id, expiry),
		}}}})
}

func (b *Bot) onPostNow(c *Ctx) {
	if len(c.Args) < 3 {
		return
	}
	invoker := c.Args[0]
	id, _ := strconv.ParseInt(c.Args[1], 10, 64)
	expiry, _ := strconv.ParseInt(c.Args[2], 10, 64)
	if c.UserID() != invoker {
		c.Reply(true, errEmbed("This isn't your prompt."))
		return
	}
	if time.Now().Unix() > expiry {
		c.Update([]*discordgo.MessageEmbed{errEmbed("Timed out. Post it with `/reactionrole post <name>`.")}, []discordgo.MessageComponent{})
		return
	}
	rr, pairs, ok := b.reactionRoleByID(c, id)
	if !ok {
		return
	}
	c.DeferUpdate()
	if ok, note := b.publishReactionRole(c.GuildID(), rr, pairs); ok {
		c.ReplyComplex(true, []*discordgo.MessageEmbed{okEmbed(note)}, []discordgo.MessageComponent{})
	} else {
		c.ReplyComplex(true, []*discordgo.MessageEmbed{errEmbed(note)}, []discordgo.MessageComponent{})
	}
}

// reactionRoleByID loads a reaction role of this guild for the Post Now button.
func (b *Bot) reactionRoleByID(c *Ctx, id int64) (*db.ReactionRole, []db.RolePair, bool) {
	rows, err := b.db.ReactionRoles(c.GuildID())
	if err != nil {
		c.dbFailed("list reaction roles", err)
		return nil, nil, false
	}
	for i := range rows {
		if rows[i].ID == id {
			pairs, err := b.db.RolePairs(id)
			if err != nil {
				c.dbFailed("read pairs", err)
				return nil, nil, false
			}
			return &rows[i], pairs, true
		}
	}
	c.Update([]*discordgo.MessageEmbed{errEmbed("That reaction role no longer exists.")}, []discordgo.MessageComponent{})
	return nil, nil, false
}

func (b *Bot) registerReactionRoles() {
	nameOpt := func(desc string) []*discordgo.ApplicationCommandOption {
		return []*discordgo.ApplicationCommandOption{optStr("name", desc, true)}
	}
	b.addGroup(&group{name: "reactionrole", description: "Manage reaction roles (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{
		name:        "reactionrole create",
		description: "Start the reaction-role creation wizard.",
		perm:        permMod,
		handler:     func(c *Ctx) { b.startWizard(c, rrDraft{}) },
	})
	b.addCommand(&command{
		name:        "reactionrole edit",
		description: "Edit a reaction role.",
		options:     nameOpt("Name of the reaction role to edit"),
		perm:        permMod,
		handler:     b.cmdReactionRoleEdit,
	})
	b.addCommand(&command{
		name:        "reactionrole post",
		description: "Post or repost a saved reaction role.",
		options:     nameOpt("Name of the reaction role to post"),
		perm:        permMod,
		handler:     b.cmdReactionRolePost,
	})
	b.addCommand(&command{
		name:        "reactionrole list",
		description: "List all reaction roles and whether they're posted.",
		perm:        permMod,
		handler:     b.cmdReactionRoleList,
	})
	b.addCommand(&command{
		name:        "reactionrole delete",
		description: "Delete a reaction role and remove its message.",
		options:     nameOpt("Name of the reaction role to delete"),
		perm:        permMod,
		handler:     b.cmdReactionRoleDelete,
	})
	b.addComponent("rrw", b.onWizardComponent)
	b.addModal("rrwm", b.onWizardModal)
	b.addComponent("rrpost", b.onPostNow)
}

// namedReactionRole finds the "name" option's reaction role, or answers
// with an error and returns nil.
func (b *Bot) namedReactionRole(c *Ctx) *db.ReactionRole {
	name, _ := c.Str("name")
	rr, err := b.db.ReactionRoleByName(c.GuildID(), strings.TrimSpace(name))
	if err != nil {
		c.dbFailed("find reaction role", err)
		return nil
	}
	if rr == nil {
		c.Reply(true, errEmbed(fmt.Sprintf("No reaction role named `%s`. Check `/reactionrole list`.", name)))
	}
	return rr
}

func (b *Bot) cmdReactionRoleEdit(c *Ctx) {
	rr := b.namedReactionRole(c)
	if rr == nil {
		return
	}
	pairs, err := b.db.RolePairs(rr.ID)
	if err != nil {
		c.dbFailed("read pairs", err)
		return
	}
	b.startWizard(c, rrDraft{RRID: rr.ID, Name: rr.Name, ChannelID: rr.ChannelID, Pairs: pairs, Text: rr.Text,
		MessageID: rr.MessageID, OldChannelID: rr.ChannelID})
}

func (b *Bot) cmdReactionRolePost(c *Ctx) {
	rr := b.namedReactionRole(c)
	if rr == nil {
		return
	}
	pairs, err := b.db.RolePairs(rr.ID)
	if err != nil {
		c.dbFailed("read pairs", err)
		return
	}
	if len(pairs) == 0 {
		c.Reply(true, errEmbed("That reaction role has no emote/role pairs yet."))
		return
	}
	c.Defer(true) // adding reactions takes a moment each
	if ok, note := b.publishReactionRole(c.GuildID(), rr, pairs); ok {
		c.Reply(true, okEmbed(note))
	} else {
		c.Reply(true, errEmbed(note))
	}
}

func (b *Bot) cmdReactionRoleList(c *Ctx) {
	rows, err := b.db.ReactionRoles(c.GuildID())
	if err != nil {
		c.dbFailed("list reaction roles", err)
		return
	}
	if len(rows) == 0 {
		c.Reply(true, makeEmbed("🎭 Reaction Roles", "None defined yet. Create one with `/reactionrole create`.", colourGreyple))
		return
	}
	e := makeEmbed("🎭 Reaction Roles", fmt.Sprintf("**%d** defined", len(rows)), colourBlurple)
	for _, r := range rows[:min(len(rows), maxEmbedFields)] {
		status := "⚪ not posted"
		if r.MessageID != "" {
			status = "🟢 posted"
		}
		where := "*no channel*"
		if ch := b.channel(r.ChannelID); ch != nil {
			where = channelMention(ch.ID)
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name: r.Name, Value: fmt.Sprintf("%s • %d pair(s) • %s", status, r.PairCount, where),
		})
	}
	footer := "Edit with /reactionrole edit <name> • post with /reactionrole post <name>"
	if len(rows) > maxEmbedFields {
		footer = fmt.Sprintf("Showing %d of %d. ", maxEmbedFields, len(rows)) + footer
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: footer}
	c.Reply(true, e)
}

func (b *Bot) cmdReactionRoleDelete(c *Ctx) {
	rr := b.namedReactionRole(c)
	if rr == nil {
		return
	}
	if rr.MessageID != "" && rr.ChannelID != "" {
		if err := b.s.ChannelMessageDelete(rr.ChannelID, rr.MessageID); err != nil {
			c.logErr("delete reaction role message", err)
		}
	}
	if err := b.db.DeleteReactionRole(rr.ID); err != nil {
		c.dbFailed("delete reaction role", err)
		return
	}
	c.Reply(true, okEmbed(fmt.Sprintf("Reaction role `%s` deleted.", rr.Name)))
}

// onReactionRole gives or removes the role for a reaction on a reaction
// role message. It reports whether the message is a reaction role message.
func (b *Bot) onReactionRole(guildID, messageID, userID string, emoji discordgo.Emoji, add bool) bool {
	rr, err := b.db.ReactionRoleByMessage(messageID)
	if err != nil {
		log.Printf("reaction role lookup: %v", err)
		return false
	}
	if rr == nil {
		return false
	}
	m := b.resolveMember(guildID, userID)
	if m == nil || m.User == nil || m.User.Bot {
		return true
	}
	b.applyReactionRole(guildID, userID, rr, emoji, add)
	return true
}

// applyReactionRole gives (add) or removes the role paired with emoji.
func (b *Bot) applyReactionRole(guildID, userID string, rr *db.ReactionRole, emoji discordgo.Emoji, add bool) {
	pairs, err := b.db.RolePairs(rr.ID)
	if err != nil {
		log.Printf("reaction role %q: pairs: %v", rr.Name, err)
		return
	}
	for _, p := range pairs {
		if p.Emoji != emoji.MessageFormat() {
			continue
		}
		if b.role(guildID, p.RoleID) == nil {
			return
		}
		// Adding a role the member has (or removing one they lack) is
		// harmless, so the possibly old member cache is not checked.
		reason := auditReason("Reaction role: " + rr.Name)
		if add {
			err = b.s.GuildMemberRoleAdd(guildID, userID, p.RoleID, reason)
		} else {
			err = b.s.GuildMemberRoleRemove(guildID, userID, p.RoleID, reason)
		}
		if err != nil {
			log.Printf("reaction role %q: %v", rr.Name, err)
		}
		return
	}
}

// curate posts a message to CURATED once it has CURATED_THRESHOLD of
// CURATED_EMOTE: an info card (author and count), then a native forward of
// the message itself. Discord refuses a forward of a message whose text the
// bot can't read (error 160014), so curation runs only with
// ENABLE_MESSAGE_CONTENT_FEATURES (and the intent's switch in the Developer
// Portal); without it, curation is off completely.
func (b *Bot) curate(r *discordgo.MessageReaction) {
	if !b.env.MessageContent {
		return
	}
	emote := b.cfg.Str("CURATED_EMOTE")
	if emote == "" || r.Emoji.MessageFormat() != emote {
		return
	}
	if done, err := b.db.IsCurated(r.MessageID); err != nil || done {
		return
	}
	src := b.channel(r.ChannelID)
	if src == nil || (src.Type != discordgo.ChannelTypeGuildText && src.Type != discordgo.ChannelTypeGuildNews) {
		return
	}
	curatedID := b.cfg.ID("CURATED")
	if src.ID == curatedID {
		return // don't curate messages already in the curated channel
	}
	curated := b.channel(curatedID)
	if curated == nil || (curated.Type != discordgo.ChannelTypeGuildText && curated.Type != discordgo.ChannelTypeGuildNews) {
		return
	}
	msg, err := b.s.ChannelMessage(src.ID, r.MessageID)
	if err != nil {
		log.Printf("curate: fetch message: %v", err)
		return
	}
	count := 0
	for _, re := range msg.Reactions {
		if re.Emoji.MessageFormat() == emote {
			count = re.Count
		}
	}
	if count < b.cfg.Int("CURATED_THRESHOLD") {
		return
	}
	// Claim it first, in one step, so two reactions at once can't both post.
	if claimed, err := b.db.ClaimCurated(msg.ID); err != nil || !claimed {
		return
	}

	name := msg.Author.GlobalName
	if name == "" {
		name = msg.Author.Username
	}
	card := &discordgo.MessageEmbed{
		Title:       name,
		Description: fmt.Sprintf("%s **%d**", emote, count),
		Color:       colourGold,
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: msg.Author.AvatarURL("")},
	}
	if _, err := b.s.ChannelMessageSendComplex(curated.ID, &discordgo.MessageSend{
		Content: fmt.Sprintf("%s **%d** | %s", emote, count, channelMention(src.ID)),
		Embeds:  []*discordgo.MessageEmbed{card},
	}); err != nil {
		log.Printf("curate: info card: %v", err)
	}
	b.forwardOrLink(curated.ID, msg, r.GuildID)
}

// forwardOrLink forwards a message to a channel and checks that the
// forward is there. Discord sometimes refuses or loses a forward for a
// moment, so after forwardRetry it checks again and tries once more; if
// there is still no forward, it posts a link to the message instead.
func (b *Bot) forwardOrLink(channelID string, msg *discordgo.Message, guildID string) {
	forward := func() string {
		sent, err := b.s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
			Reference: &discordgo.MessageReference{
				Type: discordgo.MessageReferenceTypeForward, MessageID: msg.ID, ChannelID: msg.ChannelID, GuildID: guildID,
			},
		})
		if err != nil {
			log.Printf("forward %s: %v", msg.ID, err)
			return ""
		}
		return sent.ID
	}
	exists := func(id string) bool {
		if id == "" {
			return false
		}
		_, err := b.s.ChannelMessage(channelID, id)
		return err == nil
	}

	first := forward()
	if exists(first) {
		return
	}
	time.Sleep(b.forwardRetry)
	if exists(first) || exists(forward()) {
		return
	}
	link := fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, msg.ChannelID, msg.ID)
	if _, err := b.s.ChannelMessageSend(channelID, "I couldn't forward the message. Jump to it: "+link); err != nil {
		log.Printf("forward %s: fallback link: %v", msg.ID, err)
	}
}
