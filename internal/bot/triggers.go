package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// normalise lowercases text and removes all whitespace.
func normalise(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), "")
}

// Carl-bot {rand:...}/{random:...} blocks. A custom separator can be given
// as {rand(sep):...}; otherwise a comma or a tilde in the options is used.
// If both appear, the importer asks instead of guessing. Other {...} Carl-bot
// syntax is left as it is and flagged.
var (
	randBlockRE    = regexp.MustCompile(`(?is)\{(?:rand|random)(?:\(([^)]*)\))?:(.*?)\}`)
	carlUnknownRE  = regexp.MustCompile(`\{[a-zA-Z_#][a-zA-Z0-9_]*(?:\([^)]*\))?:[^{}]*\}|\{[a-zA-Z_#][a-zA-Z0-9_]*\}`)
	customEmojiRE  = regexp.MustCompile(`^<a?:(\w+):(\d+)>$`)
	triggerTimeout = 180 * time.Second
)

// triggerParse is the result of parsing one pasted trigger value.
type triggerParse struct {
	Values    []string `json:"values,omitempty"`
	Unknown   []string `json:"unknown,omitempty"`
	Ambiguous bool     `json:"-"`
	// Kept for the separator question when Ambiguous.
	Name     string `json:"name"`
	Before   string `json:"before"`
	Options  string `json:"options"`
	After    string `json:"after"`
	Original string `json:"original"`
}

func unknownBlocks(text string) []string {
	found := carlUnknownRE.FindAllString(text, -1)
	sort.Strings(found)
	return slices.Compact(found)
}

// splitOptions joins before + each non-blank option + after.
func (p triggerParse) splitOptions(sep string) []string {
	var values []string
	for _, o := range strings.Split(p.Options, sep) {
		if o = strings.TrimSpace(o); o != "" {
			values = append(values, strings.TrimSpace(p.Before+o+p.After))
		}
	}
	if len(values) == 0 {
		return []string{strings.TrimSpace(p.Original)}
	}
	return values
}

// parseTriggerValue expands a {rand:...} block into several responses.
func parseTriggerValue(content string) triggerParse {
	m := randBlockRE.FindStringSubmatchIndex(content)
	if m == nil {
		return triggerParse{Values: []string{strings.TrimSpace(content)}, Unknown: unknownBlocks(content)}
	}
	p := triggerParse{
		Before:   content[:m[0]],
		Options:  content[m[4]:m[5]],
		After:    content[m[1]:],
		Original: content,
	}
	p.Unknown = unknownBlocks(p.Before + p.After)
	explicit := ""
	if m[2] >= 0 {
		explicit = content[m[2]:m[3]]
	}
	sep := explicit
	if sep == "" {
		hasComma, hasTilde := strings.Contains(p.Options, ","), strings.Contains(p.Options, "~")
		switch {
		case hasComma && hasTilde:
			p.Ambiguous = true
			return p
		case hasComma:
			sep = ","
		case hasTilde:
			sep = "~"
		}
	}
	if sep == "" {
		p.Values = []string{strings.TrimSpace(p.Before + strings.TrimSpace(p.Options) + p.After)}
	} else {
		p.Values = p.splitOptions(sep)
	}
	return p
}

// reactionEmoji turns a stored emote ("👍" or "<:name:id>") into the form
// the reaction API wants ("👍" or "name:id").
func reactionEmoji(emote string) string {
	emote = strings.TrimSpace(emote)
	if m := customEmojiRE.FindStringSubmatch(emote); m != nil {
		return m[1] + ":" + m[2]
	}
	return emote
}

func (b *Bot) registerTriggers() {
	name := func(desc string) *discordgo.ApplicationCommandOption { return optStr("name", desc, true) }
	b.addGroup(&group{name: "trigger", description: "Manage custom triggers (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{
		name:        "trigger add",
		description: "Create a trigger, or replace its responses with a single value.",
		options:     []*discordgo.ApplicationCommandOption{name("Trigger keyword"), optStr("value", "Response text or image URL", true)},
		perm:        permMod,
		handler:     b.cmdTriggerAdd,
	})
	b.addCommand(&command{
		name:        "trigger addvalue",
		description: "Add another random response to a trigger.",
		options:     []*discordgo.ApplicationCommandOption{name("Trigger keyword"), optStr("value", "Additional response text or image URL", true)},
		perm:        permMod,
		handler:     b.cmdTriggerAddValue,
	})
	b.addCommand(&command{
		name:        "trigger removevalue",
		description: "Remove a single response from a trigger's pool.",
		options:     []*discordgo.ApplicationCommandOption{name("Trigger keyword"), optStr("value", "The exact response text to remove", true)},
		perm:        permMod,
		handler:     b.cmdTriggerRemoveValue,
	})
	b.addCommand(&command{
		name:        "trigger delete",
		description: "Remove a trigger and all its responses.",
		options:     []*discordgo.ApplicationCommandOption{name("Trigger keyword to remove")},
		perm:        permMod,
		handler:     b.cmdTriggerDelete,
	})
	b.addCommand(&command{
		name:        "trigger list",
		description: "List the names of all defined triggers.",
		perm:        permMod,
		handler:     b.cmdTriggerList,
	})
	b.addCommand(&command{
		name:        "trigger info",
		description: "List all responses currently stored for a trigger.",
		options:     []*discordgo.ApplicationCommandOption{name("Trigger keyword to inspect")},
		perm:        permMod,
		handler:     b.cmdTriggerInfo,
	})
	b.addCommand(&command{
		name:        "trigger import",
		description: "Import a single trigger from a Carl-bot tag.",
		perm:        permMod,
		handler:     b.cmdTriggerImport,
	})
	b.addCommand(&command{
		name:        "trigger export",
		description: "Export all triggers to a JSON file.",
		perm:        permMod,
		handler:     b.cmdTriggerExport,
	})
	b.addCommand(&command{
		name:        "trigger batchimport",
		description: "Import triggers from a JSON file exported by /trigger export.",
		options:     []*discordgo.ApplicationCommandOption{optAttachment("file", "A .json file previously created by /trigger export", true)},
		perm:        permMod,
		handler:     b.cmdTriggerBatchImport,
	})
	b.addModal("trigimport", b.onTriggerImportModal)
	b.addComponent("trigsep", b.onTriggerSeparator)
	b.addConfirm("trigbatch", b.onTriggerBatchConfirm)

	// Autoreacts read ordinary message text, so without the Message
	// Content option the commands are not registered at all.
	if !b.env.MessageContent {
		return
	}
	b.addGroup(&group{name: "autoreact", description: "Manage autoreacts (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{
		name:        "autoreact add",
		description: "React with an emote whenever a word or phrase is said.",
		options: []*discordgo.ApplicationCommandOption{
			optStr("emote", "The emote to react with", true), optStr("phrase", "The word or phrase to watch for", true),
		},
		perm:    permMod,
		handler: func(c *Ctx) { b.cmdAutoreactSet(c, false) },
	})
	b.addCommand(&command{
		name:        "autoreact edit",
		description: "Change the emote used for an existing autoreact.",
		options: []*discordgo.ApplicationCommandOption{
			optStr("emote", "The new emote to react with", true), optStr("phrase", "The existing phrase to update", true),
		},
		perm:    permMod,
		handler: func(c *Ctx) { b.cmdAutoreactSet(c, true) },
	})
	b.addCommand(&command{
		name:        "autoreact remove",
		description: "Remove an autoreact.",
		options:     []*discordgo.ApplicationCommandOption{optStr("phrase", "The phrase to stop reacting to", true)},
		perm:        permMod,
		handler:     b.cmdAutoreactRemove,
	})
	b.addCommand(&command{
		name:        "autoreact list",
		description: "List all autoreacts.",
		perm:        permMod,
		handler:     b.cmdAutoreactList,
	})
}

// dbFailed logs a database error and tells the invoker.
func (c *Ctx) dbFailed(what string, err error) {
	c.logErr(what, err)
	c.Reply(true, errEmbed("Something went wrong with the database — check the bot's log."))
}

func (b *Bot) cmdTriggerAdd(c *Ctx) {
	name, _ := c.Str("name")
	value, _ := c.Str("value")
	if err := b.db.SetTrigger(name, value); err != nil {
		c.dbFailed("set trigger", err)
		return
	}
	c.Reply(true, okEmbed(fmt.Sprintf("Trigger `%s` saved with a single response. "+
		"Use `/trigger addvalue %s <value>` to make it pick randomly between several.", name, name)))
}

func (b *Bot) cmdTriggerAddValue(c *Ctx) {
	name, _ := c.Str("name")
	value, _ := c.Str("value")
	total, err := b.db.AddTriggerValue(name, value)
	if err != nil {
		c.dbFailed("add trigger value", err)
		return
	}
	if total > 1 {
		c.Reply(true, okEmbed(fmt.Sprintf("Added a response to `%s`. It now has **%d** possible responses "+
			"and will pick one at random each time it's triggered.", name, total)))
	} else {
		c.Reply(true, okEmbed(fmt.Sprintf("Trigger `%s` created with 1 response.", name)))
	}
}

func (b *Bot) cmdTriggerRemoveValue(c *Ctx) {
	name, _ := c.Str("name")
	value, _ := c.Str("value")
	removed, err := b.db.RemoveTriggerValue(name, value)
	if err != nil {
		c.dbFailed("remove trigger value", err)
		return
	}
	if !removed {
		c.Reply(true, errEmbed(fmt.Sprintf("No matching response found for `%s`.", name)))
		return
	}
	values, _ := b.db.TriggerValues(name)
	c.Reply(true, okEmbed(fmt.Sprintf("Response removed from `%s`. **%d** response(s) remain.", name, len(values))))
}

func (b *Bot) cmdTriggerDelete(c *Ctx) {
	name, _ := c.Str("name")
	if err := b.db.DeleteTrigger(name); err != nil {
		c.dbFailed("delete trigger", err)
		return
	}
	c.Reply(true, okEmbed(fmt.Sprintf("Trigger `%s` removed (if it existed).", name)))
}

func (b *Bot) cmdTriggerList(c *Ctx) {
	names, err := b.db.TriggerNames()
	if err != nil {
		c.dbFailed("list triggers", err)
		return
	}
	if len(names) == 0 {
		c.Reply(true, makeEmbed("💬 Triggers", "No triggers are defined yet. Create one with `/trigger add`.", colourGreyple))
		return
	}
	parts := make([]string, len(names))
	for i, t := range names {
		parts[i] = "`" + t.Name + "`"
		if t.Count > 1 {
			parts[i] += fmt.Sprintf(" 🔀×%d", t.Count)
		}
	}
	e := makeEmbed("💬 Defined Triggers",
		fmt.Sprintf("**%d** trigger(s) defined:\n\n%s", len(names), truncate(strings.Join(parts, ", "), 3800)),
		colourBlurple)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "🔀 = picks a random response. Use /trigger info <name> to view a trigger's definitions."}
	c.Reply(true, e)
}

func (b *Bot) cmdTriggerInfo(c *Ctx) {
	name, _ := c.Str("name")
	values, err := b.db.TriggerValues(name)
	if err != nil {
		c.dbFailed("read trigger", err)
		return
	}
	if len(values) == 0 {
		c.Reply(true, errEmbed(fmt.Sprintf("No trigger named `%s` exists.", name)))
		return
	}
	title, desc := "💬 Trigger: "+name, fmt.Sprintf("**%d** response(s) on file.", len(values))
	if len(values) > 1 {
		title, desc = "🔀 Trigger: "+name, fmt.Sprintf("**%d** response(s) on file — picked at random when triggered.", len(values))
	}
	e := makeEmbed(title, desc, colourBlurple)
	for i, v := range values[:min(len(values), maxEmbedFields)] {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: fmt.Sprintf("#%d", i+1), Value: truncate(v, 1024)})
	}
	if len(values) > maxEmbedFields {
		e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Showing %d of %d responses.", maxEmbedFields, len(values))}
	}
	c.Reply(true, e)
}

func (b *Bot) cmdTriggerImport(c *Ctx) {
	c.Modal("trigimport", "Import Trigger",
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
			CustomID: "name", Label: "Trigger name", Style: discordgo.TextInputShort, MaxLength: 100, Required: true,
		}}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
			CustomID: "value", Label: "Value — supports {rand:a,b,c}", Style: discordgo.TextInputParagraph,
			MaxLength: 4000, Required: true,
		}}},
	)
}

func (b *Bot) onTriggerImportModal(c *Ctx) {
	name := normalise(c.ModalValue("name"))
	if name == "" {
		c.Reply(true, errEmbed("Trigger name can't be empty."))
		return
	}
	p := parseTriggerValue(c.ModalValue("value"))
	if !p.Ambiguous {
		b.finishTriggerImport(c, name, p.Values, p.Unknown, false)
		return
	}
	p.Name = name
	id, err := b.savePending("trigsep", c.UserID(), p)
	if err != nil {
		c.dbFailed("save pending import", err)
		return
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("Which separator?",
		fmt.Sprintf("Found a random-response block for `%s` but it has both `,` and `~` in it, "+
			"so I can't tell which one splits the options. Pick one, or don't split at all:", name),
		colourOrange)}, separatorButtons(c.UserID(), time.Now().Add(triggerTimeout).Unix(), id, false))
}

// separatorButtons: trigsep:<choice>:<invoker>:<expiry>:<pending id>
func separatorButtons(invoker string, expiry int64, id string, disabled bool) []discordgo.MessageComponent {
	btn := func(choice, label string, style discordgo.ButtonStyle) discordgo.Button {
		return discordgo.Button{Label: label, Style: style, Disabled: disabled,
			CustomID: fmt.Sprintf("trigsep:%s:%s:%d:%s", choice, invoker, expiry, id)}
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		btn("comma", "Split on comma ,", discordgo.PrimaryButton),
		btn("tilde", "Split on tilde ~", discordgo.PrimaryButton),
		btn("none", "Don't split (one response)", discordgo.SecondaryButton),
	}}}
}

func (b *Bot) onTriggerSeparator(c *Ctx) {
	if len(c.Args) < 4 {
		return
	}
	choice, invoker, id := c.Args[0], c.Args[1], c.Args[3]
	var expiry int64
	_, _ = fmt.Sscan(c.Args[2], &expiry)
	if c.UserID() != invoker {
		c.Reply(true, errEmbed("This isn't your import."))
		return
	}
	if time.Now().Unix() > expiry {
		c.Update([]*discordgo.MessageEmbed{errEmbed("Timed out. No changes made.")}, separatorButtons(invoker, expiry, id, true))
		return
	}
	var p triggerParse
	if !b.takePending(c, "trigsep", id, &p) {
		return
	}
	switch choice {
	case "comma":
		b.finishTriggerImport(c, p.Name, p.splitOptions(","), p.Unknown, true)
	case "tilde":
		b.finishTriggerImport(c, p.Name, p.splitOptions("~"), p.Unknown, true)
	default:
		b.finishTriggerImport(c, p.Name, []string{strings.TrimSpace(p.Original)}, nil, true)
	}
}

func (b *Bot) finishTriggerImport(c *Ctx, name string, values, unknown []string, update bool) {
	before, err := b.db.TriggerValues(name)
	if err != nil {
		c.dbFailed("read trigger", err)
		return
	}
	total := len(before)
	for _, v := range values {
		if total, err = b.db.AddTriggerValue(name, v); err != nil {
			c.dbFailed("add trigger value", err)
			return
		}
	}
	desc := fmt.Sprintf("Trigger `%s` saved with **%d** response(s).", name, len(values))
	if len(before) > 0 {
		desc = fmt.Sprintf("Trigger `%s` had **%d** response(s) — now has **%d** after adding **%d** more.",
			name, len(before), total, len(values))
	}
	if total > 1 {
		desc += " Picks one at random each time it fires."
	}
	if len(unknown) > 0 {
		shown := make([]string, 0, 5)
		for _, u := range unknown[:min(len(unknown), 5)] {
			shown = append(shown, "`"+u+"`")
		}
		desc += "\n\n⚠️ Contains syntax I don't interpret, left as-is: " + strings.Join(shown, ", ")
	}
	if update {
		c.Update([]*discordgo.MessageEmbed{okEmbed(desc)}, []discordgo.MessageComponent{})
	} else {
		c.Reply(true, okEmbed(desc))
	}
}

// triggerExport is one entry of the export file.
type triggerExport struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

func (b *Bot) cmdTriggerExport(c *Ctx) {
	names, err := b.db.TriggerNames()
	if err != nil {
		c.dbFailed("list triggers", err)
		return
	}
	if len(names) == 0 {
		c.Reply(true, errEmbed("No triggers are defined yet."))
		return
	}
	data := make([]triggerExport, 0, len(names))
	total := 0
	for _, t := range names {
		values, err := b.db.TriggerValues(t.Name)
		if err != nil {
			c.dbFailed("read trigger", err)
			return
		}
		data = append(data, triggerExport{t.Name, values})
		total += len(values)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		c.logErr("encode export", err)
		return
	}
	_ = c.ReplyFile(true, okEmbed(fmt.Sprintf("Exported **%d** trigger(s) with **%d** total response(s).", len(data), total)),
		&discordgo.File{Name: "triggers-export.json", ContentType: "application/json", Reader: &buf})
}

// batchImport is the data behind the batch import Confirm button.
type batchImport struct {
	Entries    []triggerExport `json:"entries"`
	Duplicates int             `json:"duplicates"`
}

func (b *Bot) cmdTriggerBatchImport(c *Ctx) {
	limitMB := max(b.cfg.Int("TRIGGER_IMPORT_MAX_MB"), 1)
	raw, ok := b.readAttachment(c, c.Attachment("file"), int64(limitMB)<<20,
		fmt.Sprintf("That file is larger than **%d MB** (`TRIGGER_IMPORT_MAX_MB` in `/setup`).", limitMB))
	if !ok {
		return
	}

	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		c.Reply(true, errEmbed("That file isn't valid JSON: "+err.Error()))
		return
	}
	items, ok := data.([]any)
	if !ok {
		c.Reply(true, errEmbed("Expected a JSON list of `{\"name\": ..., \"values\": [...]}` entries — "+
			"the format `/trigger export` produces."))
		return
	}

	var job batchImport
	malformed := 0
	for _, item := range items {
		name, values, ok := validTriggerEntry(item)
		if !ok {
			malformed++
			continue
		}
		stored, err := b.db.TriggerValues(name)
		if err != nil {
			c.dbFailed("read trigger", err)
			return
		}
		seen := map[string]bool{}
		var fresh []string
		for _, v := range values {
			if slices.Contains(stored, v) || seen[v] {
				job.Duplicates++
				continue
			}
			seen[v] = true
			fresh = append(fresh, v)
		}
		if len(fresh) > 0 {
			job.Entries = append(job.Entries, triggerExport{name, fresh})
		}
	}
	if len(job.Entries) == 0 {
		reason := "No valid trigger entries found in that file."
		if job.Duplicates > 0 {
			reason = "Every response in that file already exists — nothing new to import."
		}
		c.Reply(true, errEmbed(reason))
		return
	}

	totalNew := 0
	for _, e := range job.Entries {
		totalNew += len(e.Values)
	}
	summary := fmt.Sprintf("Found **%d** new response(s) across **%d** trigger(s) in this file.", totalNew, len(job.Entries))
	if job.Duplicates > 0 {
		summary += fmt.Sprintf("\n**%d** response(s) already exist and will be skipped.", job.Duplicates)
	}
	if malformed > 0 {
		word := "entries"
		if malformed == 1 {
			word = "entry"
		}
		summary += fmt.Sprintf("\n**%d** %s skipped (malformed).", malformed, word)
	}
	summary += "\n\nExisting triggers with the same name get these added as extra responses, not replaced."

	id, err := b.savePending("trigbatch", c.UserID(), job)
	if err != nil {
		c.dbFailed("save pending import", err)
		return
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("📥 Confirm Batch Import", summary, colourOrange)},
		confirmButtons("trigbatch", c.UserID(), id))
}

// readAttachment downloads an attachment of at most limit bytes, and
// defers the reply first. tooBig is the error for a larger file. When it
// returns false, it has already answered.
func (b *Bot) readAttachment(c *Ctx, file *discordgo.MessageAttachment, limit int64, tooBig string) ([]byte, bool) {
	if file == nil {
		c.Reply(true, errEmbed("Couldn't read that attachment."))
		return nil, false
	}
	if int64(file.Size) > limit {
		c.Reply(true, errEmbed(tooBig))
		return nil, false
	}
	c.Defer(true)
	resp, err := b.s.Client.Get(file.URL)
	if err != nil {
		c.logErr("download attachment", err)
		c.Reply(true, errEmbed("Couldn't read that attachment."))
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || resp.StatusCode != 200 {
		c.logErr("read attachment", fmt.Errorf("status %d: %v", resp.StatusCode, err))
		c.Reply(true, errEmbed("Couldn't read that attachment."))
		return nil, false
	}
	if int64(len(raw)) > limit {
		c.Reply(true, errEmbed(tooBig))
		return nil, false
	}
	return bytes.ToValidUTF8(raw, []byte("�")), true
}

// validTriggerEntry checks one {"name": str, "values": [str, ...]} entry.
func validTriggerEntry(item any) (name string, values []string, ok bool) {
	m, isMap := item.(map[string]any)
	if !isMap {
		return "", nil, false
	}
	n, isStr := m["name"].(string)
	list, isList := m["values"].([]any)
	if !isStr || strings.TrimSpace(n) == "" || !isList || len(list) == 0 {
		return "", nil, false
	}
	for _, v := range list {
		s, isStr := v.(string)
		if !isStr || strings.TrimSpace(s) == "" {
			return "", nil, false
		}
		values = append(values, s)
	}
	return strings.TrimSpace(n), values, true
}

func (b *Bot) onTriggerBatchConfirm(c *Ctx, extra []string) {
	if len(extra) < 1 {
		return
	}
	var job batchImport
	if !b.takePending(c, "trigbatch", extra[0], &job) {
		return
	}
	triggers, values := 0, 0
	for _, e := range job.Entries {
		for _, v := range e.Values {
			if _, err := b.db.AddTriggerValue(e.Name, v); err != nil {
				c.logErr("add trigger value", err)
				continue
			}
			values++
		}
		triggers++
	}
	msg := fmt.Sprintf("Imported **%d** new response(s) across **%d** trigger(s).", values, triggers)
	if job.Duplicates > 0 {
		msg += fmt.Sprintf(" Skipped **%d** already-existing response(s).", job.Duplicates)
	}
	c.Update([]*discordgo.MessageEmbed{okEmbed(msg)}, []discordgo.MessageComponent{})
}

// cmdAutoreactSet adds (or, with edit, changes) an autoreact. The emote is
// checked by reacting with it to the bot's own reply first.
func (b *Bot) cmdAutoreactSet(c *Ctx, edit bool) {
	emote, _ := c.Str("emote")
	phrase, _ := c.Str("phrase")
	norm := normalise(phrase)
	if norm == "" {
		c.Reply(true, errEmbed("Usage: `/autoreact add <emote> <word or phrase>`."))
		return
	}
	exists, err := b.db.AutoreactExists(c.GuildID(), norm)
	if err != nil {
		c.dbFailed("find autoreact", err)
		return
	}
	switch {
	case !edit && exists:
		c.Reply(true, errEmbed(fmt.Sprintf("An autoreact already exists for **%s**. Use `/autoreact edit` to change its emote, "+
			"or `/autoreact remove` to delete it.", phrase)))
		return
	case edit && !exists:
		c.Reply(true, errEmbed(fmt.Sprintf("No autoreact exists for **%s**. Use `/autoreact add` to create one, "+
			"or `/autoreact list` to see what's saved.", phrase)))
		return
	}

	c.Reply(false, okEmbed(fmt.Sprintf("%s will now be added whenever someone says **%s**.", emote, phrase)))
	msg, err := c.s.InteractionResponse(c.i.Interaction)
	if err == nil {
		err = c.s.MessageReactionAdd(msg.ChannelID, msg.ID, reactionEmoji(emote))
	}
	if err != nil {
		c.logErr("test autoreact emote", err)
		nothing := "nothing saved"
		if edit {
			nothing = "nothing changed"
		}
		c.EditResponse(errEmbed(fmt.Sprintf("`%s` doesn't look like an emote I can react with — %s.", emote, nothing)))
		return
	}
	if edit {
		err = b.db.SetAutoreactEmote(c.GuildID(), norm, emote)
	} else {
		err = b.db.AddAutoreact(c.GuildID(), emote, norm, c.UserID())
	}
	if err != nil {
		c.logErr("save autoreact", err)
		c.EditResponse(errEmbed("Couldn't save the autoreact — check the bot's log."))
	}
}

func (b *Bot) cmdAutoreactRemove(c *Ctx) {
	phrase, _ := c.Str("phrase")
	removed, err := b.db.RemoveAutoreact(c.GuildID(), normalise(phrase))
	if err != nil {
		c.dbFailed("remove autoreact", err)
		return
	}
	if !removed {
		c.Reply(true, errEmbed(fmt.Sprintf("No autoreact exists for **%s**.", phrase)))
		return
	}
	c.Reply(true, okEmbed(fmt.Sprintf("Removed the autoreact for **%s**.", phrase)))
}

func (b *Bot) cmdAutoreactList(c *Ctx) {
	rows, err := b.db.Autoreacts(c.GuildID())
	if err != nil {
		c.dbFailed("list autoreacts", err)
		return
	}
	if len(rows) == 0 {
		c.Reply(true, errEmbed("No autoreacts saved yet. Use `/autoreact add <emote> <word or phrase>`."))
		return
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = fmt.Sprintf("%s — **%s**", r.Emote, r.Phrase)
	}
	desc := strings.Join(lines, "\n")
	if len([]rune(desc)) > 4000 {
		desc = truncate(desc, 4000) + "\n… (truncated)"
	}
	c.Reply(true, makeEmbed(fmt.Sprintf("😀 Autoreacts (%d)", len(rows)), desc, colourBlurple))
}
