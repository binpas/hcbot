package bot

import (
	"fmt"
	"log"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// permLevel is the bot-side check a command runs before its handler, on
// top of the Discord-side default_member_permissions.
type permLevel int

const (
	permNone  permLevel = iota
	permMod             // member has MOD_ROLE or is an Administrator
	permAdmin           // member has the Administrator permission
)

// Commands that still run while SETUP_MODE is on.
var setupModeAllowed = map[string]bool{"setup": true, "permsreport": true}

// command is one slash command, or one subcommand of a group when its name
// has a space ("jail arrest"). Every command is slash-only and guild-only.
type command struct {
	name        string
	description string
	options     []*discordgo.ApplicationCommandOption
	perm        permLevel
	requires    int64 // Discord permissions the invoker must hold, e.g. BanMembers
	defaultPerm int64 // top-level commands only; 0 = everyone
	handler     func(*Ctx)
}

// group is a top-level command that only holds subcommands.
type group struct {
	name        string
	description string
	defaultPerm int64
	subs        []string
}

var guildOnly = &[]discordgo.InteractionContextType{discordgo.InteractionContextGuild}

func (b *Bot) addCommand(c *command) {
	b.commands[c.name] = c
	top, sub, isSub := strings.Cut(c.name, " ")
	if isSub {
		g := b.groups[top]
		if g == nil {
			panic("subcommand " + c.name + " registered before its group")
		}
		g.subs = append(g.subs, sub)
		return
	}
	b.order = append(b.order, c.name)
}

func (b *Bot) addGroup(g *group) {
	b.groups[g.name] = g
	b.order = append(b.order, g.name)
}

// addComponent routes component interactions whose custom ID starts with
// "<prefix>:" to handler.
func (b *Bot) addComponent(prefix string, handler func(*Ctx)) {
	b.components[prefix] = handler
}

// addModal routes modal submits whose custom ID starts with "<prefix>:".
func (b *Bot) addModal(prefix string, handler func(*Ctx)) {
	b.modals[prefix] = handler
}

func (b *Bot) commandDefs() []*discordgo.ApplicationCommand {
	var defs []*discordgo.ApplicationCommand
	for _, name := range b.order {
		def := &discordgo.ApplicationCommand{
			Type:     discordgo.ChatApplicationCommand,
			Name:     name,
			Contexts: guildOnly,
		}
		var perm int64
		if g, ok := b.groups[name]; ok {
			def.Description = g.description
			perm = g.defaultPerm
			for _, sub := range g.subs {
				c := b.commands[name+" "+sub]
				def.Options = append(def.Options, &discordgo.ApplicationCommandOption{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        sub,
					Description: c.description,
					Options:     c.options,
				})
			}
		} else {
			c := b.commands[name]
			def.Description = c.description
			def.Options = c.options
			perm = c.defaultPerm
		}
		if perm != 0 {
			def.DefaultMemberPermissions = &perm
		}
		defs = append(defs, def)
	}
	return defs
}

// syncCommands registers every command, on GUILD_ID alone when it is set
// (instant) or globally otherwise.
func (b *Bot) syncCommands() error {
	_, err := b.s.ApplicationCommandBulkOverwrite(b.s.State.User.ID, b.env.GuildID, b.commandDefs())
	return err
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	c := &Ctx{b: b, s: s, i: i}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in interaction %s: %v\n%s", c.name(), r, debug.Stack())
		}
	}()

	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		b.dispatchCommand(c)
	case discordgo.InteractionMessageComponent:
		b.dispatchCustomID(c, i.MessageComponentData().CustomID, b.components)
	case discordgo.InteractionModalSubmit:
		b.dispatchCustomID(c, i.ModalSubmitData().CustomID, b.modals)
	}
}

func (b *Bot) dispatchCommand(c *Ctx) {
	data := c.i.ApplicationCommandData()
	name := data.Name
	c.opts = data.Options
	if len(c.opts) == 1 && c.opts[0].Type == discordgo.ApplicationCommandOptionSubCommand {
		name += " " + c.opts[0].Name
		c.opts = c.opts[0].Options
	}
	cmd, ok := b.commands[name]
	if !ok || c.i.Member == nil {
		return
	}
	c.cmdName = name

	if b.env.SetupMode && !setupModeAllowed[strings.Fields(name)[0]] {
		c.Reply(true, errEmbed("🚧 This bot is in setup mode — only `/setup` and `/permsreport` are available right now."))
		return
	}
	switch cmd.perm {
	case permMod:
		if !b.isMod(c.i.Member) {
			c.Reply(true, errEmbed("You must be a moderator to use that command."))
			return
		}
	case permAdmin:
		if !isAdmin(c.i.Member) {
			c.Reply(true, errEmbed("You don't have permission to use that command."))
			return
		}
	}
	if cmd.requires != 0 && !isAdmin(c.i.Member) && c.i.Member.Permissions&cmd.requires != cmd.requires {
		c.Reply(true, errEmbed("You don't have permission to use that command."))
		return
	}
	cmd.handler(c)
}

func (b *Bot) dispatchCustomID(c *Ctx, customID string, routes map[string]func(*Ctx)) {
	parts := strings.Split(customID, ":")
	handler, ok := routes[parts[0]]
	if !ok {
		return
	}
	c.cmdName = customID
	c.Args = parts[1:]
	handler(c)
}

// isMod reports whether a member may use mod-only commands: members with
// MOD_ROLE, and Administrators (who already have full control).
func (b *Bot) isMod(m *discordgo.Member) bool {
	if isAdmin(m) {
		return true
	}
	role := b.cfg.ID("MOD_ROLE")
	return m != nil && role != "" && slices.Contains(m.Roles, role)
}

func isAdmin(m *discordgo.Member) bool {
	return m != nil && m.Permissions&discordgo.PermissionAdministrator != 0
}

// Ctx is one interaction: a slash command, component click, or modal submit.
type Ctx struct {
	b       *Bot
	s       *discordgo.Session
	i       *discordgo.InteractionCreate
	opts    []*discordgo.ApplicationCommandInteractionDataOption
	cmdName string

	// Args holds a component or modal custom ID's fields after the prefix.
	Args []string

	responded bool
	deferred  bool // deferred and not yet replied to
}

func (c *Ctx) name() string {
	if c.cmdName != "" {
		return c.cmdName
	}
	return fmt.Sprint(c.i.Type)
}

// GuildID is the guild the interaction came from.
func (c *Ctx) GuildID() string { return c.i.GuildID }

// Member is the invoking member.
func (c *Ctx) Member() *discordgo.Member { return c.i.Member }

// UserID is the invoking user's ID. A click in a DM has no member, only a user.
func (c *Ctx) UserID() string {
	if c.i.Member != nil && c.i.Member.User != nil {
		return c.i.Member.User.ID
	}
	if c.i.User != nil {
		return c.i.User.ID
	}
	return ""
}

func (c *Ctx) logErr(what string, err error) {
	if err != nil {
		log.Printf("%s: %s: %v", c.name(), what, err)
	}
}

// Reply sends embeds as the interaction response, or as a follow-up if the
// interaction already has one (or was deferred).
func (c *Ctx) Reply(ephemeral bool, embeds ...*discordgo.MessageEmbed) {
	c.ReplyComplex(ephemeral, embeds, nil)
}

// ReplyComplex is Reply with message components.
func (c *Ctx) ReplyComplex(ephemeral bool, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent) {
	_ = c.reply(ephemeral, embeds, components, nil)
}

// ReplyFile is Reply with an attached file. It returns the error when
// Discord refuses the reply (for example, a file that is too large).
func (c *Ctx) ReplyFile(ephemeral bool, embed *discordgo.MessageEmbed, file *discordgo.File) error {
	return c.reply(ephemeral, []*discordgo.MessageEmbed{embed}, nil, []*discordgo.File{file})
}

func (c *Ctx) reply(ephemeral bool, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent, files []*discordgo.File) error {
	var flags discordgo.MessageFlags
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}
	if c.deferred {
		c.deferred = false
		edit := &discordgo.WebhookEdit{Embeds: &embeds, Files: files}
		if components != nil {
			edit.Components = &components
		}
		_, err := c.s.InteractionResponseEdit(c.i.Interaction, edit)
		c.logErr("edit deferred", err)
		return err
	}
	if !c.responded {
		c.responded = true
		err := c.s.InteractionRespond(c.i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Embeds: embeds, Components: components, Flags: flags, Files: files},
		})
		c.logErr("respond", err)
		return err
	}
	_, err := c.s.FollowupMessageCreate(c.i.Interaction, true, &discordgo.WebhookParams{
		Embeds: embeds, Components: components, Flags: flags, Files: files,
	})
	c.logErr("follow-up", err)
	return err
}

// Defer acknowledges the interaction so a slow handler has up to 15
// minutes to reply.
func (c *Ctx) Defer(ephemeral bool) {
	var flags discordgo.MessageFlags
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}
	c.responded = true
	c.deferred = true
	c.logErr("defer", c.s.InteractionRespond(c.i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: flags},
	}))
}

// DeferUpdate acknowledges a component click without changing its message
// yet. A later Reply edits that message.
func (c *Ctx) DeferUpdate() {
	c.responded = true
	c.deferred = true
	c.logErr("defer update", c.s.InteractionRespond(c.i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}))
}

// FinishUpdate replaces the message a component is on (after DeferUpdate)
// with one embed, and removes its buttons and attached files.
func (c *Ctx) FinishUpdate(embed *discordgo.MessageEmbed) {
	embeds := []*discordgo.MessageEmbed{embed}
	components := []discordgo.MessageComponent{}
	attachments := []*discordgo.MessageAttachment{}
	_, err := c.s.InteractionResponseEdit(c.i.Interaction, &discordgo.WebhookEdit{
		Embeds: &embeds, Components: &components, Attachments: &attachments,
	})
	c.logErr("finish update", err)
}

// EditResponse replaces the embeds of the interaction's original response.
func (c *Ctx) EditResponse(embeds ...*discordgo.MessageEmbed) {
	_, err := c.s.InteractionResponseEdit(c.i.Interaction, &discordgo.WebhookEdit{Embeds: &embeds})
	c.logErr("edit response", err)
}

// DeleteResponse deletes the interaction's original response.
func (c *Ctx) DeleteResponse() {
	c.logErr("delete response", c.s.InteractionResponseDelete(c.i.Interaction))
}

// ResponseURL returns a jump link to the interaction's original response,
// or "" if it can't be fetched (e.g. it was ephemeral).
func (c *Ctx) ResponseURL() string {
	msg, err := c.s.InteractionResponse(c.i.Interaction)
	if err != nil {
		c.logErr("fetch response", err)
		return ""
	}
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", c.GuildID(), msg.ChannelID, msg.ID)
}

// Update edits the message a component (or a modal opened from one) is on.
func (c *Ctx) Update(embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent) {
	c.responded = true
	c.logErr("update", c.s.InteractionRespond(c.i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Embeds: embeds, Components: components},
	}))
}

// Modal opens a modal in response to the interaction.
func (c *Ctx) Modal(customID, title string, components ...discordgo.MessageComponent) {
	c.responded = true
	c.logErr("modal", c.s.InteractionRespond(c.i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{CustomID: customID, Title: title, Components: components},
	}))
}

// Values returns a select menu's chosen values.
func (c *Ctx) Values() []string {
	return c.i.MessageComponentData().Values
}

// ModalValue returns the value of the text input with the given custom ID.
func (c *Ctx) ModalValue(customID string) string {
	for _, row := range c.i.ModalSubmitData().Components {
		ar, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, comp := range ar.Components {
			if ti, ok := comp.(*discordgo.TextInput); ok && ti.CustomID == customID {
				return ti.Value
			}
		}
	}
	return ""
}

func (c *Ctx) opt(name string) *discordgo.ApplicationCommandInteractionDataOption {
	for _, o := range c.opts {
		if o.Name == name {
			return o
		}
	}
	return nil
}

// Str returns a string option, and whether it was given.
func (c *Ctx) Str(name string) (string, bool) {
	if o := c.opt(name); o != nil {
		return o.StringValue(), true
	}
	return "", false
}

// Int returns an integer option, or def when it was not given.
func (c *Ctx) Int(name string, def int64) int64 {
	if o := c.opt(name); o != nil {
		return o.IntValue()
	}
	return def
}

// User returns a user option, or nil.
func (c *Ctx) User(name string) *discordgo.User {
	o := c.opt(name)
	if o == nil {
		return nil
	}
	id := o.UserValue(nil).ID
	if res := c.i.ApplicationCommandData().Resolved; res != nil && res.Users[id] != nil {
		return res.Users[id]
	}
	return o.UserValue(c.s)
}

// MemberOpt returns a user option as a member of this guild, or nil when
// the option was not given or the user is not in the guild.
func (c *Ctx) MemberOpt(name string) *discordgo.Member {
	o := c.opt(name)
	if o == nil {
		return nil
	}
	id := o.UserValue(nil).ID
	res := c.i.ApplicationCommandData().Resolved
	if res != nil {
		if m, ok := res.Members[id]; ok && res.Users[id] != nil {
			cp := *m
			cp.User = res.Users[id]
			cp.GuildID = c.GuildID()
			return &cp
		}
	}
	return c.b.resolveMember(c.GuildID(), id)
}

// Channel returns a channel option, or nil.
func (c *Ctx) Channel(name string) *discordgo.Channel {
	if o := c.opt(name); o != nil {
		return o.ChannelValue(c.s)
	}
	return nil
}

// Attachment returns an attachment option, or nil.
func (c *Ctx) Attachment(name string) *discordgo.MessageAttachment {
	o := c.opt(name)
	if o == nil {
		return nil
	}
	id, _ := o.Value.(string)
	if res := c.i.ApplicationCommandData().Resolved; res != nil {
		return res.Attachments[id]
	}
	return nil
}

// Role returns a role option, or nil.
func (c *Ctx) Role(name string) *discordgo.Role {
	if o := c.opt(name); o != nil {
		return o.RoleValue(c.s, c.GuildID())
	}
	return nil
}

// Option builders for command definitions.

func optUser(name, desc string, required bool) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionUser, Name: name, Description: desc, Required: required,
	}
}

func optStr(name, desc string, required bool) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: name, Description: desc, Required: required,
	}
}

// optChoice is a string option that accepts only the given values.
func optChoice(name, desc string, required bool, values ...string) *discordgo.ApplicationCommandOption {
	o := optStr(name, desc, required)
	for _, v := range values {
		o.Choices = append(o.Choices, &discordgo.ApplicationCommandOptionChoice{Name: v, Value: v})
	}
	return o
}

func optInt(name, desc string, required bool) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionInteger, Name: name, Description: desc, Required: required,
	}
}

func optAttachment(name, desc string, required bool) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionAttachment, Name: name, Description: desc, Required: required,
	}
}
