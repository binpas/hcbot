package bot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/config"
)

// /setup — paginated, editable configuration UI (admin only, ephemeral).
// The settings are in sections (setupSections); a menu jumps to a section.
//
// The UI keeps no server-side state: each component's custom ID carries
// the invoker, the page, and an expiry, so it also survives a restart.
//   setup:<action>:<invoker>:<page>:<expiry>[:<key>]
//   setupm:<invoker>:<page>:<key>          (modal submit)

const (
	ticketOpenEmoji  = "\U0001F4E9" // :envelope_with_arrow:
	ticketCloseEmoji = "\U0001F512" // :lock:

	setupPageSize = 3
	setupTimeout  = 5 * time.Minute
)

// contentOnlySettings belong to features that need
// ENABLE_MESSAGE_CONTENT_FEATURES (curation, autoreacts, the media cache).
// /setup hides them when the option is off; stored values are kept.
var contentOnlySettings = map[string]bool{
	"CURATED": true, "CURATED_EMOTE": true, "CURATED_THRESHOLD": true,
	"ON_THE_REAL": true, "MEDIA_CACHE_TOTAL_MB": true,
}

// setupSections sets the order of /setup: each section shows its settings
// in this order, on one or more neighbouring pages. Every SetupKey must be
// in exactly one section (TestSetupSectionsCoverEveryKey).
var setupSections = []struct {
	title string
	keys  []string
}{
	{"Logs", []string{"MOD_LOG", "BIG_BROTHER", "SENSITIVE_LOG"}},
	{"Mods and tickets", []string{"MOD_ROLE", "MOD_SUPPORT", "TICKET_CAT"}},
	{"Jail", []string{"JAIL_ROLE", "JAIL_CHANNEL", "JAIL_PROTECTED_ROLES"}},
	{"Warnings", []string{"WARN_ARREST_THRESHOLD", "WARN_ARREST_DURATION", "WARN_KICK_THRESHOLD"}},
	{"Community", []string{"MOTD_ROLE", "CURATED", "CURATED_EMOTE", "CURATED_THRESHOLD", "ON_THE_REAL"}},
	{"Markov chat", []string{"MARKOV_LEARN_CHANNELS", "MARKOV_REPLY_CHANNELS", "MARKOV_MENTION_REPLY",
		"MARKOV_REPLY_CHANCE", "MARKOV_RANDOM_AS_REPLY", "MARKOV_COOLDOWN"}},
	{"File and memory limits", []string{"TRIGGER_IMPORT_MAX_MB", "ASSETS_IMPORT_MAX_MB", "MARKOV_IMPORT_MAX_MB",
		"MEDIA_CACHE_TOTAL_MB"}},
	{"Links and files", []string{"README_PATH", "PRIVACY_URL", "TERMS_URL"}},
}

// setupPage is one page of /setup: part of one section.
type setupPage struct {
	section  int // index in setupSections
	part, of int // this page is part 1..of of its section
	keys     []config.SetupKey
}

// setupPages splits the visible settings of each section into pages.
func (b *Bot) setupPages() []setupPage {
	byKey := map[string]config.SetupKey{}
	for _, k := range config.SetupKeys {
		byKey[k.Key] = k
	}
	var pages []setupPage
	for si, sec := range setupSections {
		var keys []config.SetupKey
		for _, key := range sec.keys {
			if k, ok := byKey[key]; ok && (b.env.MessageContent || !contentOnlySettings[key]) {
				keys = append(keys, k)
			}
		}
		of := (len(keys) + setupPageSize - 1) / setupPageSize
		for i := 0; i < len(keys); i += setupPageSize {
			pages = append(pages, setupPage{section: si, part: i/setupPageSize + 1, of: of,
				keys: keys[i:min(i+setupPageSize, len(keys))]})
		}
	}
	return pages
}

func (b *Bot) registerSetup() {
	b.addCommand(&command{
		name:        "setup",
		description: "Configure HEALTH bot settings (admin only).",
		perm:        permAdmin,
		defaultPerm: discordgo.PermissionAdministrator,
		handler: func(c *Ctx) {
			v := setupView{b: b, guildID: c.GuildID(), invoker: c.UserID()}
			c.ReplyComplex(true, []*discordgo.MessageEmbed{v.embed()}, v.components(false))
		},
	})
	b.addComponent("setup", b.onSetupComponent)
	b.addModal("setupm", b.onSetupModal)
}

type setupView struct {
	b       *Bot
	guildID string
	invoker string
	page    int
}

func (v setupView) embed() *discordgo.MessageEmbed {
	pages := v.b.setupPages()
	page := pages[v.page]
	title := "⚙️ HEALTH Bot Setup: " + setupSections[page.section].title
	if page.of > 1 {
		title += fmt.Sprintf(" (%d/%d)", page.part, page.of)
	}
	e := makeEmbed(title,
		"Use the selects/buttons below to change a value, or the menu to go to a section. Only you can see this.",
		colourBlurple)
	for _, k := range page.keys {
		shown := v.b.setupDisplay(v.guildID, k.Kind, v.b.cfg.Raw(k.Key))
		if k.Kind == config.KindChoice {
			value := v.b.cfg.Choice(k.Key)
			shown = fmt.Sprintf("`%s`: %s", value, config.ChoiceLabel(k.Key, value))
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  k.Key,
			Value: fmt.Sprintf("%s\n%s\n%s", sourceLabels[v.b.cfg.Source(k.Key)], shown, k.Description),
		})
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Page %d/%d", v.page+1, len(pages))}
	return e
}

// sourceLabels names where a setting's value comes from, on its own line
// so a long value can't hide it.
var sourceLabels = map[string]string{
	"database": "💾 **Saved**",
	".env":     "📄 **From .env**",
	"default":  "⚙️ **Built-in default** *(not saved)*",
}

func (v setupView) id(action, key string) string {
	id := fmt.Sprintf("setup:%s:%s:%d:%d", action, v.invoker, v.page, time.Now().Add(setupTimeout).Unix())
	if key != "" {
		id += ":" + key
	}
	return id
}

func (v setupView) components(disabled bool) []discordgo.MessageComponent {
	pages := v.b.setupPages()
	// Row 1 jumps to a section, rows 2-4 hold settings, and row 5 turns pages.
	var options []discordgo.SelectMenuOption
	for i, p := range pages {
		if p.part == 1 {
			options = append(options, discordgo.SelectMenuOption{
				Label: setupSections[p.section].title, Value: strconv.Itoa(i),
				Default: p.section == pages[v.page].section,
			})
		}
	}
	rows := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.SelectMenu{
			MenuType: discordgo.StringSelectMenu, CustomID: v.id("sec", ""),
			Placeholder: "Go to a section", Options: options, Disabled: disabled,
		},
	}}}
	zero := 0
	hasModSupport := false

	for _, k := range pages[v.page].keys {
		var comp discordgo.MessageComponent
		switch k.Kind {
		case config.KindChannel, config.KindCategory:
			types := []discordgo.ChannelType{discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews}
			if k.Kind == config.KindCategory {
				types = []discordgo.ChannelType{discordgo.ChannelTypeGuildCategory}
			}
			comp = discordgo.SelectMenu{
				MenuType: discordgo.ChannelSelectMenu, CustomID: v.id("ch", k.Key),
				Placeholder: "Set " + k.Key, ChannelTypes: types, MaxValues: 1, Disabled: disabled,
			}
		case config.KindRole:
			comp = discordgo.SelectMenu{
				MenuType: discordgo.RoleSelectMenu, CustomID: v.id("role", k.Key),
				Placeholder: "Set " + k.Key, MaxValues: 1, Disabled: disabled,
			}
		case config.KindRoleList:
			var defaults []discordgo.SelectMenuDefaultValue
			for _, id := range v.b.cfg.List(k.Key) {
				if len(defaults) == 25 {
					break
				}
				defaults = append(defaults, discordgo.SelectMenuDefaultValue{ID: id, Type: discordgo.SelectMenuDefaultValueRole})
			}
			comp = discordgo.SelectMenu{
				MenuType: discordgo.RoleSelectMenu, CustomID: v.id("rlist", k.Key),
				Placeholder: "Set " + k.Key + " (replaces the whole list)",
				MinValues:   &zero, MaxValues: 25, DefaultValues: defaults, Disabled: disabled,
			}
		case config.KindChanList:
			var defaults []discordgo.SelectMenuDefaultValue
			for _, id := range v.b.cfg.List(k.Key) {
				if len(defaults) == 25 {
					break
				}
				defaults = append(defaults, discordgo.SelectMenuDefaultValue{ID: id, Type: discordgo.SelectMenuDefaultValueChannel})
			}
			comp = discordgo.SelectMenu{
				MenuType: discordgo.ChannelSelectMenu, CustomID: v.id("clist", k.Key),
				Placeholder:  "Set " + k.Key + " (replaces the whole list)",
				ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews},
				MinValues:    &zero, MaxValues: 25, DefaultValues: defaults, Disabled: disabled,
			}
		case config.KindBool:
			label, style := "Off — click to turn on", discordgo.SecondaryButton
			if v.b.cfg.Bool(k.Key) {
				label, style = "On — click to turn off", discordgo.SuccessButton
			}
			comp = discordgo.Button{
				Label: truncate(k.Key+": "+label, 80), Style: style,
				CustomID: v.id("toggle", k.Key), Disabled: disabled,
			}
		case config.KindChoice:
			current := v.b.cfg.Choice(k.Key)
			var options []discordgo.SelectMenuOption
			for _, ch := range config.Choices[k.Key] {
				options = append(options, discordgo.SelectMenuOption{
					Label: truncate(ch.Value+": "+ch.Label, 100), Value: ch.Value, Default: ch.Value == current,
				})
			}
			comp = discordgo.SelectMenu{
				MenuType: discordgo.StringSelectMenu, CustomID: v.id("choice", k.Key),
				Placeholder: "Set " + k.Key, Options: options, Disabled: disabled,
			}
		default: // int / emoji / text — no native select component, use a modal
			comp = discordgo.Button{
				Label: "Edit " + k.Key, Style: discordgo.SecondaryButton,
				CustomID: v.id("edit", k.Key), Disabled: disabled,
			}
		}
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{comp}})
		if k.Key == "MOD_SUPPORT" {
			hasModSupport = true
		}
	}

	nav := []discordgo.MessageComponent{
		discordgo.Button{
			Label: "◀ Back", Style: discordgo.PrimaryButton,
			CustomID: v.id("back", ""), Disabled: disabled || v.page == 0,
		},
		discordgo.Button{
			Label: fmt.Sprintf("Page %d/%d", v.page+1, len(pages)), Style: discordgo.SecondaryButton,
			CustomID: v.id("page", ""), Disabled: true,
		},
		discordgo.Button{
			Label: "Next ▶", Style: discordgo.PrimaryButton,
			CustomID: v.id("next", ""), Disabled: disabled || v.page == len(pages)-1,
		},
	}
	if hasModSupport {
		nav = append(nav, discordgo.Button{
			Label: "Post/Repost Mod Support Embed", Style: discordgo.SuccessButton,
			CustomID: v.id("post", ""), Disabled: disabled,
		})
	}
	rows = append(rows, discordgo.ActionsRow{Components: nav})
	return rows
}

// refresh redraws the setup message (and restarts its timeout).
func (v setupView) refresh(c *Ctx) {
	c.Update([]*discordgo.MessageEmbed{v.embed()}, v.components(false))
}

func (b *Bot) onSetupComponent(c *Ctx) {
	if len(c.Args) < 4 {
		return
	}
	action, invoker := c.Args[0], c.Args[1]
	page, _ := strconv.Atoi(c.Args[2])
	expiry, _ := strconv.ParseInt(c.Args[3], 10, 64)
	key := ""
	if len(c.Args) > 4 {
		key = c.Args[4]
	}
	if page < 0 || page >= len(b.setupPages()) {
		page = 0
	}
	v := setupView{b: b, guildID: c.GuildID(), invoker: invoker, page: page}

	if time.Now().Unix() > expiry {
		c.Update([]*discordgo.MessageEmbed{v.embed()}, v.components(true))
		return
	}
	if c.UserID() != invoker {
		c.Reply(true, errEmbed("This setup session isn't yours — run `/setup` yourself."))
		return
	}

	switch action {
	case "ch", "role":
		if vals := c.Values(); len(vals) == 1 && b.setupSave(c, key, vals[0]) {
			v.refresh(c)
		}
	case "rlist", "clist":
		if b.setupSave(c, key, strings.Join(c.Values(), ",")) {
			v.refresh(c)
		}
	case "choice":
		if vals := c.Values(); len(vals) == 1 && b.setupSave(c, key, vals[0]) {
			v.refresh(c)
		}
	case "toggle":
		value := "on"
		if b.cfg.Bool(key) {
			value = "off"
		}
		if b.setupSave(c, key, value) {
			v.refresh(c)
		}
	case "edit":
		b.openSetupModal(c, v, key)
	case "post":
		if ok, msg := b.postModSupportEmbed(c.GuildID()); ok {
			c.Reply(true, okEmbed(msg))
		} else {
			c.Reply(true, errEmbed(msg))
		}
	case "sec":
		if vals := c.Values(); len(vals) == 1 {
			if to, err := strconv.Atoi(vals[0]); err == nil && to >= 0 && to < len(b.setupPages()) {
				v.page = to
			}
			v.refresh(c)
		}
	case "back":
		v.page = max(0, v.page-1)
		v.refresh(c)
	case "next":
		v.page = min(len(b.setupPages())-1, v.page+1)
		v.refresh(c)
	}
}

func (b *Bot) setupSave(c *Ctx, key, value string) bool {
	if !b.env.MessageContent && contentOnlySettings[key] {
		c.Reply(true, errEmbed("`"+key+"` belongs to a feature that is off (ENABLE_MESSAGE_CONTENT_FEATURES)."))
		return false
	}
	if err := b.setConfig(key, value); err != nil {
		c.logErr("save "+key, err)
		c.Reply(true, errEmbed("Couldn't save that setting — check the bot's log."))
		return false
	}
	return true
}

func (b *Bot) openSetupModal(c *Ctx, v setupView, key string) {
	var desc string
	for _, k := range config.SetupKeys {
		if k.Key == key {
			desc = k.Description
		}
	}
	c.Modal(
		fmt.Sprintf("setupm:%s:%d:%s", v.invoker, v.page, key),
		truncate("Set "+key, 45),
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.TextInput{
				CustomID: "value", Label: truncate(desc, 45), Style: discordgo.TextInputShort,
				Value: b.cfg.Raw(key), Required: true, MaxLength: 100,
			},
		}},
	)
}

func (b *Bot) onSetupModal(c *Ctx) {
	if len(c.Args) < 3 {
		return
	}
	page, _ := strconv.Atoi(c.Args[1])
	if page < 0 || page >= len(b.setupPages()) {
		page = 0
	}
	key := c.Args[2]
	v := setupView{b: b, guildID: c.GuildID(), invoker: c.Args[0], page: page}

	raw := strings.TrimSpace(c.ModalValue("value"))
	for _, k := range config.SetupKeys {
		if k.Key == key && k.Kind == config.KindInt {
			n, err := strconv.Atoi(raw)
			if err != nil {
				c.Reply(true, errEmbed(fmt.Sprintf("`%s` isn't a whole number.", raw)))
				return
			}
			raw = strconv.Itoa(n)
		}
		if k.Key == key && k.Kind == config.KindDecimal {
			f, err := config.ParseDecimal(raw)
			if err != nil {
				c.Reply(true, errEmbed(fmt.Sprintf("`%s` isn't a number. Use a form like `0.05`.", raw)))
				return
			}
			raw = strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	if b.setupSave(c, key, raw) {
		v.refresh(c)
	}
}

// setupDisplay renders a stored config value as its human-readable form.
func (b *Bot) setupDisplay(guildID, kind, raw string) string {
	if raw == "" {
		return "*not set*"
	}
	switch kind {
	case config.KindChannel, config.KindCategory:
		if _, err := b.s.State.Channel(raw); err == nil {
			return channelMention(raw)
		}
		return fmt.Sprintf("`%s` *(not found)*", raw)
	case config.KindRole:
		if _, err := b.s.State.Role(guildID, raw); err == nil {
			return roleMention(raw)
		}
		return fmt.Sprintf("`%s` *(not found)*", raw)
	case config.KindRoleList, config.KindChanList:
		ids := config.ParseIDList(raw)
		if len(ids) == 0 {
			return "*none*"
		}
		mentions := make([]string, len(ids))
		for i, id := range ids {
			if kind == config.KindChanList {
				if _, err := b.s.State.Channel(id); err == nil {
					mentions[i] = channelMention(id)
				} else {
					mentions[i] = fmt.Sprintf("`%s` *(not found)*", id)
				}
			} else if _, err := b.s.State.Role(guildID, id); err == nil {
				mentions[i] = roleMention(id)
			} else {
				mentions[i] = fmt.Sprintf("`%s` *(not found)*", id)
			}
		}
		text := strings.Join(mentions, ", ")
		if len(text) > 800 { // leave room for the source and description in the 1024-char field
			cut := text[:780]
			if i := strings.LastIndex(cut, ","); i >= 0 {
				cut = cut[:i]
			}
			text = fmt.Sprintf("%s, …and %d total", cut, len(ids))
		}
		return text
	case config.KindEmoji:
		return raw
	case config.KindBool:
		if v, ok := config.ParseBool(raw); ok && v {
			return "✅ On"
		} else if ok {
			return "❌ Off"
		}
		return fmt.Sprintf("`%s` *(not on or off)*", raw)
	}
	return "`" + raw + "`" // int
}

// postModSupportEmbed posts (or reposts) the ticket-creation embed and
// remembers its message ID.
func (b *Bot) postModSupportEmbed(guildID string) (bool, string) {
	id := b.cfg.ID("MOD_SUPPORT")
	ch := b.channel(id)
	if ch == nil || ch.GuildID != guildID ||
		(ch.Type != discordgo.ChannelTypeGuildText && ch.Type != discordgo.ChannelTypeGuildNews) {
		return false, "MOD_SUPPORT is not set to a valid text channel. Set it in `/setup` first."
	}
	embed := makeEmbed("Raise an issue with the mod team",
		"To create a ticket, react with "+ticketOpenEmoji, colourBlurple)
	msg, err := b.s.ChannelMessageSendEmbed(ch.ID, embed)
	if err == nil {
		err = b.s.MessageReactionAdd(ch.ID, msg.ID, ticketOpenEmoji)
	}
	if err != nil {
		return false, fmt.Sprintf("I don't have permission to post or react in %s.", channelMention(ch.ID))
	}
	if err := b.setConfig(config.ModSupportMsgKey, msg.ID); err != nil {
		return false, "Posted the embed, but couldn't save its message ID — check the bot's log."
	}
	return true, fmt.Sprintf("Mod support embed posted in %s.", channelMention(ch.ID))
}

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
