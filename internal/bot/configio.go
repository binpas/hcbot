package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/binpas/hcbot/internal/config"
	"github.com/bwmarrin/discordgo"
)

// configImportLimit is the largest settings file /db configimport reads.
const configImportLimit = 1 << 20

// cmdConfigExport sends the config table as a JSON object. It holds only
// the values saved in the database, not .env values or defaults.
func (b *Bot) cmdConfigExport(c *Ctx) {
	values, err := b.db.LoadConfig()
	if err != nil {
		c.dbFailed("read settings", err)
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(values); err != nil { // maps encode with sorted keys
		c.logErr("encode settings", err)
		return
	}
	_ = c.ReplyFile(true, okEmbed(fmt.Sprintf("Exported **%d** saved setting(s).", len(values))),
		&discordgo.File{Name: "config-export.json", ContentType: "application/json", Reader: &buf})
}

// configImport is the data behind the settings import Confirm button.
type configImport struct {
	Values  map[string]string `json:"values"`
	Replace bool              `json:"replace"`
}

func (b *Bot) cmdConfigImport(c *Ctx) {
	mode, _ := c.Str("mode")
	raw, ok := b.readAttachment(c, c.Attachment("file"), configImportLimit, "That file is larger than **1 MB**.")
	if !ok {
		return
	}
	raw = bytes.ToValidUTF8(raw, []byte("\ufffd"))
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil || data == nil {
		msg := "Expected a JSON object of `\"KEY\": \"value\"` pairs — the format `/db configexport` produces."
		if err != nil {
			msg = "That file isn't a valid settings file: " + err.Error()
		}
		c.Reply(true, errEmbed(msg))
		return
	}

	job := configImport{Values: map[string]string{}, Replace: mode != "merge"}
	var problems []string
	for _, k := range slices.Sorted(maps.Keys(data)) {
		v, isStr := data[k].(string)
		switch {
		case strings.TrimSpace(k) == "":
			problems = append(problems, "An empty key")
		case !isStr:
			problems = append(problems, fmt.Sprintf("`%s`: the value must be text in quotes", k))
		default:
			if err := config.CheckValue(k, v); err != nil {
				problems = append(problems, fmt.Sprintf("`%s`: %v", k, err))
			}
		}
		job.Values[k] = v
	}
	if len(problems) > 0 {
		c.Reply(true, errEmbed("Nothing was imported. Fix these values and try again:\n"+listLines(problems, 15)))
		return
	}

	current, err := b.db.LoadConfig()
	if err != nil {
		c.dbFailed("read settings", err)
		return
	}
	var added, changed, removed, unknown []string
	for _, k := range slices.Sorted(maps.Keys(job.Values)) {
		old, exists := current[k]
		switch {
		case !exists:
			added = append(added, "`"+k+"`")
		case old != job.Values[k]:
			changed = append(changed, "`"+k+"`")
		}
		if !config.Known(k) {
			unknown = append(unknown, "`"+k+"`")
		}
	}
	if job.Replace {
		for _, k := range slices.Sorted(maps.Keys(current)) {
			if _, kept := job.Values[k]; !kept {
				removed = append(removed, "`"+k+"`")
			}
		}
	}
	if len(added)+len(changed)+len(removed) == 0 {
		c.Reply(true, errEmbed("The file matches the current settings — nothing to import."))
		return
	}

	how := "**Merge:** settings that are not in the file stay as they are."
	if job.Replace {
		how = "**Replace:** the file becomes the full set of saved settings."
	}
	summary := how + "\n"
	for _, part := range []struct {
		label string
		keys  []string
	}{{"Add", added}, {"Change", changed}, {"Remove", removed}} {
		if len(part.keys) > 0 {
			summary += fmt.Sprintf("\n**%s (%d):** %s", part.label, len(part.keys), joinCapped(part.keys, 40))
		}
	}
	if len(unknown) > 0 {
		summary += fmt.Sprintf("\n\n⚠️ The bot does not use these keys, but they will be imported: %s", joinCapped(unknown, 20))
	}
	summary += "\n\nThe bot makes a database backup before it imports."

	id, err := b.savePending("cfgimport", c.UserID(), job)
	if err != nil {
		c.dbFailed("save pending import", err)
		return
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("📥 Confirm Settings Import", summary, colourOrange)},
		confirmButtons("cfgimport", c.UserID(), id))
}

func (b *Bot) onConfigImportConfirm(c *Ctx, extra []string) {
	if len(extra) < 1 {
		return
	}
	var job configImport
	if !b.takePending(c, "cfgimport", extra[0], &job) {
		return
	}
	backup, _, err := b.makeBackup()
	if err != nil {
		c.logErr("backup before settings import", err)
		c.Update([]*discordgo.MessageEmbed{errEmbed("The backup failed, so nothing was imported: " + err.Error())},
			[]discordgo.MessageComponent{})
		return
	}
	if err := b.db.ImportConfig(job.Values, job.Replace); err != nil {
		c.logErr("import settings", err)
		c.Update([]*discordgo.MessageEmbed{errEmbed("The import failed. No settings were changed.")},
			[]discordgo.MessageComponent{})
		return
	}
	values, err := b.db.LoadConfig()
	if err != nil {
		c.logErr("reload settings", err)
		c.Update([]*discordgo.MessageEmbed{errEmbed("Imported, but the bot couldn't reload its settings. Restart the bot.")},
			[]discordgo.MessageComponent{})
		return
	}
	b.cfg.Replace(values)
	c.Update([]*discordgo.MessageEmbed{okEmbed(fmt.Sprintf(
		"Imported **%d** setting(s). The bot now has **%d** saved setting(s).\nBackup from before the import: `%s`",
		len(job.Values), len(values), backup))}, []discordgo.MessageComponent{})
}

// listLines formats items as a bullet list of at most n lines.
func listLines(items []string, n int) string {
	lines := items[:min(len(items), n)]
	text := "• " + strings.Join(lines, "\n• ")
	if len(items) > n {
		text += fmt.Sprintf("\n…and %d more", len(items)-n)
	}
	return text
}

// joinCapped joins at most n items with commas.
func joinCapped(items []string, n int) string {
	text := strings.Join(items[:min(len(items), n)], ", ")
	if len(items) > n {
		text += fmt.Sprintf(", …and %d more", len(items)-n)
	}
	return text
}
