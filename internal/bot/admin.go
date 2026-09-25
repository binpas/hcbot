package bot

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------
// Database backups (admin only: a backup holds everything)
// ---------------------------------------------------------------------

var backupNameRE = regexp.MustCompile(`^health_bot_\d{8}_\d{6}\.db$`)

// humanSize matches the Python bot's _human_size.
func humanSize(n int64) string {
	size := float64(n)
	for _, unit := range []string{"B", "KB", "MB", "GB", "TB"} {
		if size < 1024 {
			if unit == "B" {
				return fmt.Sprintf("%d B", int64(size))
			}
			return fmt.Sprintf("%.1f %s", size, unit)
		}
		size /= 1024
	}
	return fmt.Sprintf("%.1f PB", size)
}

// backups lists backup file names, oldest first.
func (b *Bot) backups() []string {
	entries, err := os.ReadDir(b.backupDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && backupNameRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// makeBackup writes a new backup and returns its name and size.
func (b *Bot) makeBackup() (string, int64, error) {
	if err := os.MkdirAll(b.backupDir, 0o755); err != nil {
		return "", 0, err
	}
	name := "health_bot_" + time.Now().UTC().Format("20060102_150405") + ".db"
	path := filepath.Join(b.backupDir, name)
	if err := b.db.Backup(path); err != nil {
		return "", 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	return name, info.Size(), nil
}

// dbChangedSinceBackup reports whether the database file is newer than
// the newest backup.
func (b *Bot) dbChangedSinceBackup() bool {
	dbInfo, err := os.Stat(b.db.Path)
	if err != nil {
		return false
	}
	list := b.backups()
	if len(list) == 0 {
		return true
	}
	newest, err := os.Stat(filepath.Join(b.backupDir, list[len(list)-1]))
	return err != nil || dbInfo.ModTime().After(newest.ModTime())
}

// runDailyBackup backs the database up once a day, only if it changed.
func (b *Bot) runDailyBackup() {
	if !b.dbChangedSinceBackup() {
		return
	}
	name, size, err := b.makeBackup()
	if err != nil {
		log.Printf("daily backup: %v", err)
		return
	}
	b.modLog(makeEmbed("🗄️ Automatic Backup", fmt.Sprintf("Saved `%s` (%s).", name, humanSize(size)), colourBlurple))
}

// nextDaily returns the next time at hour:minute UTC after now.
func nextDaily(now time.Time, hour, minute int) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// daily runs fn every day at hour:minute UTC until the bot closes.
func (b *Bot) daily(hour, minute int, fn func()) {
	go func() {
		for {
			t := time.NewTimer(time.Until(nextDaily(time.Now(), hour, minute)))
			select {
			case <-t.C:
				fn()
			case <-b.stop:
				t.Stop()
				return
			}
		}
	}()
}

func (b *Bot) sendBackup(c *Ctx, name string, prefix string) {
	path := filepath.Join(b.backupDir, name)
	f, err := os.Open(path)
	if err != nil {
		c.Reply(true, errEmbed(fmt.Sprintf("No backup named `%s`.", name)))
		return
	}
	defer func() { _ = f.Close() }()
	size := int64(0)
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	if err := c.ReplyFile(true, okEmbed(fmt.Sprintf(prefix, name, humanSize(size))),
		&discordgo.File{Name: name, ContentType: "application/octet-stream", Reader: f}); err != nil {
		c.Reply(true, errEmbed(fmt.Sprintf("`%s` is on disk, but it's too large to send through Discord.", name)))
	}
}

func (b *Bot) cmdDBBackup(c *Ctx) {
	c.Defer(true)
	name, _, err := b.makeBackup()
	if err != nil {
		c.logErr("backup", err)
		c.Reply(true, errEmbed("Backup failed: "+err.Error()))
		return
	}
	b.sendBackup(c, name, "Backup saved as `%s` (%s).")
}

func (b *Bot) cmdDBList(c *Ctx) {
	if name, ok := c.Str("filename"); ok {
		if !backupNameRE.MatchString(name) || !slices.Contains(b.backups(), name) {
			c.Reply(true, errEmbed(fmt.Sprintf("No backup named `%s`. Run `/db list` with no name to see what's saved.", name)))
			return
		}
		c.Defer(true)
		b.sendBackup(c, name, "`%s` (%s)")
		return
	}
	list := b.backups()
	if len(list) == 0 {
		c.Reply(true, makeEmbed("No backups", "No database backups saved yet. Run `/db backup` to make one.", colourGreyple))
		return
	}
	var lines []string
	for i := len(list) - 1; i >= 0 && len(lines) < 25; i-- { // newest first
		info, err := os.Stat(filepath.Join(b.backupDir, list[i]))
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("`%s` — %s — %s", list[i], humanSize(info.Size()),
			info.ModTime().UTC().Format("2006-01-02 15:04 UTC")))
	}
	desc := strings.Join(lines, "\n")
	if len(list) > 25 {
		desc += fmt.Sprintf("\n… and %d more.", len(list)-25)
	}
	c.Reply(true, makeEmbed(fmt.Sprintf("🗄️ Backups (%d)", len(list)), desc, colourBlurple))
}

func (b *Bot) cmdDBDelete(c *Ctx) {
	name, _ := c.Str("filename")
	if !backupNameRE.MatchString(name) {
		c.Reply(true, errEmbed("That doesn't look like a valid backup filename."))
		return
	}
	if !slices.Contains(b.backups(), name) {
		c.Reply(true, errEmbed(fmt.Sprintf("No backup named `%s`.", name)))
		return
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("Delete backup?",
		fmt.Sprintf("Permanently delete `%s`? This can't be undone.", name), colourOrange)},
		confirmButtons("dbdelete", c.UserID(), name))
}

func (b *Bot) onDBDeleteConfirm(c *Ctx, extra []string) {
	if len(extra) < 1 || !backupNameRE.MatchString(extra[0]) {
		return
	}
	name := extra[0]
	if err := os.Remove(filepath.Join(b.backupDir, name)); err != nil {
		c.Update([]*discordgo.MessageEmbed{errEmbed(fmt.Sprintf("Couldn't delete `%s`: %v", name, err))}, []discordgo.MessageComponent{})
		return
	}
	c.Update([]*discordgo.MessageEmbed{okEmbed(fmt.Sprintf("Deleted `%s`.", name))}, []discordgo.MessageComponent{})
}

// ---------------------------------------------------------------------
// /permsreport
// ---------------------------------------------------------------------

func permSymbol(ow *discordgo.PermissionOverwrite, perm int64) string {
	switch {
	case ow.Allow&perm != 0:
		return "✅"
	case ow.Deny&perm != 0:
		return "❌"
	}
	return "➖"
}

func (b *Bot) overwriteLabel(guildID string, ow *discordgo.PermissionOverwrite) string {
	if ow.Type == discordgo.PermissionOverwriteTypeRole {
		if r := b.role(guildID, ow.ID); r != nil {
			return "@" + r.Name
		}
		return fmt.Sprintf("unknown role (%s)", ow.ID)
	}
	if m := b.resolveMember(guildID, ow.ID); m != nil && m.User != nil {
		return m.User.String()
	}
	return fmt.Sprintf("unknown member (%s)", ow.ID)
}

func (b *Bot) overwriteLines(guildID string, ows []*discordgo.PermissionOverwrite) []string {
	if len(ows) == 0 {
		return []string{"    (no explicit overwrites — inherits from @everyone)"}
	}
	lines := make([]string, len(ows))
	for i, ow := range ows {
		lines[i] = fmt.Sprintf("    %-32s View: %s   Send: %s", b.overwriteLabel(guildID, ow),
			permSymbol(ow, discordgo.PermissionViewChannel), permSymbol(ow, discordgo.PermissionSendMessages))
	}
	return lines
}

// permsReport builds the report text and returns it with the number of
// unsynced channels and the skip note.
func (b *Bot) permsReport(guildID, author string) (report string, unsynced int, skipNote string) {
	g, chans := b.guildChannels(guildID)
	if g == nil {
		return "", 0, ""
	}
	byPosition := func(list []*discordgo.Channel) {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Position < list[j].Position })
	}
	var categories, loose []*discordgo.Channel
	children := map[string][]*discordgo.Channel{}
	for _, ch := range chans {
		switch {
		case ch.Type == discordgo.ChannelTypeGuildCategory:
			categories = append(categories, ch)
		case ch.ParentID == "":
			loose = append(loose, ch)
		default:
			children[ch.ParentID] = append(children[ch.ParentID], ch)
		}
	}
	byPosition(categories)
	byPosition(loose)

	var syncedBlocks, unsyncedBlocks []string
	skippedCats, skippedChans := 0, 0
	for _, cat := range categories {
		if strings.Contains(strings.ToLower(cat.Name), "archive") {
			skippedCats++
			continue
		}
		kids := children[cat.ID]
		byPosition(kids)
		var synced []string
		var own []*discordgo.Channel
		for _, ch := range kids {
			if strings.Contains(strings.ToLower(ch.Name), "ticket") {
				skippedChans++
				continue
			}
			if sameOverwrites(ch.PermissionOverwrites, cat.PermissionOverwrites) {
				synced = append(synced, "#"+ch.Name)
			} else {
				own = append(own, ch)
			}
		}
		block := append([]string{"📁 " + cat.Name}, b.overwriteLines(guildID, cat.PermissionOverwrites)...)
		if len(synced) > 0 {
			block = append(block, "    Synced channels: "+strings.Join(synced, ", "))
		}
		syncedBlocks = append(syncedBlocks, strings.Join(block, "\n"))
		for _, ch := range own {
			unsyncedBlocks = append(unsyncedBlocks, strings.Join(append(
				[]string{fmt.Sprintf("# %s  (category: %s)", ch.Name, cat.Name)},
				b.overwriteLines(guildID, ch.PermissionOverwrites)...), "\n"))
		}
	}
	for _, ch := range loose {
		if strings.Contains(strings.ToLower(ch.Name), "ticket") {
			skippedChans++
			continue
		}
		unsyncedBlocks = append(unsyncedBlocks, strings.Join(append(
			[]string{fmt.Sprintf("# %s  (no category)", ch.Name)},
			b.overwriteLines(guildID, ch.PermissionOverwrites)...), "\n"))
	}

	if skippedCats > 0 || skippedChans > 0 {
		word := "categories"
		if skippedCats == 1 {
			word = "category"
		}
		skipNote = fmt.Sprintf("Skipped %d 'archive' %s and %d 'ticket' channel(s).", skippedCats, word, skippedChans)
	}
	rule := strings.Repeat("=", 64)
	lines := []string{
		"PERMISSIONS REPORT — " + g.Name,
		fmt.Sprintf("Generated %s by %s", time.Now().UTC().Format("2006-01-02 15:04 UTC"), author),
		"Legend: ✅ explicit allow   ❌ explicit deny   ➖ not set (inherits)",
	}
	if skipNote != "" {
		lines = append(lines, skipNote)
	}
	synced := "(no categories)"
	if len(syncedBlocks) > 0 {
		synced = strings.Join(syncedBlocks, "\n\n")
	}
	unsyncedText := "(every channel is synced to its category)"
	if len(unsyncedBlocks) > 0 {
		unsyncedText = strings.Join(unsyncedBlocks, "\n\n")
	}
	lines = append(lines, "", rule, "SYNCED CHANNELS (share their category's permissions)", rule, "", synced, "",
		rule, fmt.Sprintf("UNSYNCED CHANNELS (%d channel(s) with their own overwrites)", len(unsyncedBlocks)), rule, "", unsyncedText)
	return strings.Join(lines, "\n"), len(unsyncedBlocks), skipNote
}

func (b *Bot) cmdPermsReport(c *Ctx) {
	c.Reply(true, makeEmbed("📋 Building report…",
		"Reading permissions for every channel — this may take a moment on a large server.", colourBlurple))
	report, unsynced, skipNote := b.permsReport(c.GuildID(), c.Member().User.String())
	if report == "" {
		c.Reply(true, errEmbed("I couldn't read this server's channels."))
		return
	}
	msg := fmt.Sprintf("Report generated — **%d** channel(s) have their own overwrites.", unsynced)
	if skipNote != "" {
		msg += " (" + strings.ToLower(skipNote[:1]) + skipNote[1:] + ")"
	}
	if err := c.ReplyFile(true, okEmbed(msg), &discordgo.File{
		Name: "permissions_report_" + c.GuildID() + ".txt", ContentType: "text/plain", Reader: strings.NewReader(report),
	}); err != nil {
		c.Reply(true, errEmbed("The report is too large to send through Discord."))
	}
}

// ---------------------------------------------------------------------
// /permtemplate
// ---------------------------------------------------------------------

// templateEntry is one overwrite in a template, in the Python bot's JSON
// format (numeric IDs), so both bots can read each other's templates.
type templateEntry struct {
	Kind  string `json:"kind"` // "role" or "member"
	ID    uint64 `json:"id"`
	Name  string `json:"name"`
	Allow int64  `json:"allow"`
	Deny  int64  `json:"deny"`
}

func (b *Bot) templateData(ch *discordgo.Channel) (string, error) {
	entries := []templateEntry{}
	for _, ow := range ch.PermissionOverwrites {
		id, _ := strconv.ParseUint(ow.ID, 10, 64)
		e := templateEntry{Kind: "member", ID: id, Allow: ow.Allow, Deny: ow.Deny}
		if ow.Type == discordgo.PermissionOverwriteTypeRole {
			e.Kind = "role"
			e.Name = "unknown role"
			if r := b.role(ch.GuildID, ow.ID); r != nil {
				e.Name = r.Name
			}
		} else {
			e.Name = "unknown member"
			if m := b.resolveMember(ch.GuildID, ow.ID); m != nil && m.User != nil {
				e.Name = m.User.String()
			}
		}
		entries = append(entries, e)
	}
	data, err := json.Marshal(entries)
	return string(data), err
}

// resolveTemplate turns template entries into overwrites for targets that
// still exist, and lists the ones that don't.
func (b *Bot) resolveTemplate(guildID, data string) ([]*discordgo.PermissionOverwrite, []string, error) {
	var entries []templateEntry
	if err := json.Unmarshal([]byte(data), &entries); err != nil {
		return nil, nil, err
	}
	var resolved []*discordgo.PermissionOverwrite
	var missing []string
	for _, e := range entries {
		id := strconv.FormatUint(e.ID, 10)
		ow := &discordgo.PermissionOverwrite{ID: id, Allow: e.Allow, Deny: e.Deny}
		found := false
		if e.Kind == "role" {
			ow.Type = discordgo.PermissionOverwriteTypeRole
			found = b.role(guildID, id) != nil
		} else {
			ow.Type = discordgo.PermissionOverwriteTypeMember
			found = b.resolveMember(guildID, id) != nil
		}
		if found {
			resolved = append(resolved, ow)
		} else {
			missing = append(missing, fmt.Sprintf("%s **%s** (`%s`)", e.Kind, e.Name, id))
		}
	}
	return resolved, missing, nil
}

// templatePlan computes a channel's new overwrites. In "replace" mode the
// channel ends up with exactly the template's overwrites; in "add" mode
// each template overwrite is merged into the current one for that target
// (pattern 9) and nothing is removed.
func templatePlan(current, resolved []*discordgo.PermissionOverwrite, mode string) (result []*discordgo.PermissionOverwrite, removed int) {
	if mode == "replace" {
		for _, ow := range current {
			if !slices.ContainsFunc(resolved, func(r *discordgo.PermissionOverwrite) bool { return r.ID == ow.ID }) {
				removed++
			}
		}
		return resolved, removed
	}
	result = make([]*discordgo.PermissionOverwrite, 0, len(current)+len(resolved))
	byID := map[string]*discordgo.PermissionOverwrite{}
	for _, ow := range current {
		cp := *ow
		result = append(result, &cp)
		byID[ow.ID] = &cp
	}
	for _, t := range resolved {
		ow, ok := byID[t.ID]
		if !ok {
			cp := *t
			result = append(result, &cp)
			continue
		}
		for _, p := range permissionNames {
			switch {
			case t.Allow&p.bit != 0:
				setPerm(ow, p.bit, true)
			case t.Deny&p.bit != 0:
				setPerm(ow, p.bit, false)
			}
		}
	}
	return result, 0
}

// setOverwrites replaces all of a channel's overwrites in one API call, so
// a failure can't leave the channel half changed.
func (b *Bot) setOverwrites(channelID string, ows []*discordgo.PermissionOverwrite, reason string) error {
	if ows == nil {
		ows = []*discordgo.PermissionOverwrite{}
	}
	body := map[string]any{"permission_overwrites": ows}
	_, err := b.s.RequestWithBucketID("PATCH", discordgo.EndpointChannel(channelID), body,
		discordgo.EndpointChannel(channelID), auditReason(reason))
	return err
}

func (b *Bot) cmdTemplateSave(c *Ctx) {
	ch := c.Channel("channel")
	name, _ := c.Str("name")
	name = strings.TrimSpace(name)
	if ch == nil || name == "" {
		c.Reply(true, errEmbed("Usage: `/permtemplate save #channel <name>`."))
		return
	}
	if full := b.channel(ch.ID); full != nil {
		ch = full // the option's channel has no overwrites
	}
	data, err := b.templateData(ch)
	if err == nil {
		err = b.db.SaveTemplate(c.GuildID(), name, ch.Name, data, c.UserID())
	}
	if err != nil {
		c.dbFailed("save template", err)
		return
	}
	c.Reply(true, okEmbed(fmt.Sprintf("Saved template **%s** from %s — captured **%d** permission overwrite(s).",
		strings.ToLower(name), channelMention(ch.ID), len(ch.PermissionOverwrites))))
}

func (b *Bot) cmdTemplateList(c *Ctx) {
	rows, err := b.db.Templates(c.GuildID())
	if err != nil {
		c.dbFailed("list templates", err)
		return
	}
	if len(rows) == 0 {
		c.Reply(true, errEmbed("No permission templates saved yet. Use `/permtemplate save #channel <name>`."))
		return
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		creator := "unknown"
		if r.CreatedBy != "" {
			creator = userMention(r.CreatedBy)
		}
		lines[i] = fmt.Sprintf("• **%s** — from `#%s`, saved by %s on %s", r.Name, r.Source, creator, r.CreatedAt)
	}
	desc := strings.Join(lines, "\n")
	if len([]rune(desc)) > 4000 {
		desc = truncate(desc, 4000) + "\n… (truncated)"
	}
	c.Reply(true, makeEmbed(fmt.Sprintf("📑 Permission Templates (%d)", len(rows)), desc, colourBlurple))
}

// templateApply is the data behind the apply Confirm button.
type templateApply struct {
	Name      string `json:"name"`
	ChannelID string `json:"channel"`
	Mode      string `json:"mode"`
}

func (b *Bot) cmdTemplateApply(c *Ctx) {
	name, _ := c.Str("name")
	mode, _ := c.Str("mode")
	opt := c.Channel("channel")
	tpl, err := b.db.Template(c.GuildID(), name)
	if err != nil {
		c.dbFailed("find template", err)
		return
	}
	if tpl == nil {
		c.Reply(true, errEmbed(fmt.Sprintf("No template named `%s`. Use `/permtemplate list` to see what's saved.", name)))
		return
	}
	ch := b.channel(opt.ID)
	if ch == nil {
		c.Reply(true, errEmbed("I can't find that channel."))
		return
	}
	resolved, missing, err := b.resolveTemplate(c.GuildID(), tpl.Data)
	if err != nil {
		c.logErr("read template", err)
		c.Reply(true, errEmbed("That template's data is damaged."))
		return
	}
	_, removed := templatePlan(ch.PermissionOverwrites, resolved, mode)

	summary := fmt.Sprintf("Apply template **%s** (from `#%s`) to %s?\n\n", tpl.Name, tpl.Source, channelMention(ch.ID))
	if mode == "replace" {
		summary += fmt.Sprintf("• **%d** overwrite(s) will be set\n• **%d** existing overwrite(s) on %s will be removed to match the template",
			len(resolved), removed, channelMention(ch.ID))
	} else {
		summary += fmt.Sprintf("• **%d** overwrite(s) will be added to the current ones (existing values for other permissions are kept)\n"+
			"• Nothing will be removed", len(resolved))
	}
	if len(missing) > 0 {
		word := "entries"
		if len(missing) == 1 {
			word = "entry"
		}
		summary += fmt.Sprintf("\n• **%d** %s in the template couldn't be resolved and will be skipped:\n", len(missing), word)
		for _, m := range missing[:min(len(missing), 10)] {
			summary += "   - " + m + "\n"
		}
		if len(missing) > 10 {
			summary += fmt.Sprintf("   …and %d more.", len(missing)-10)
		}
	}
	id, err := b.savePending("ptapply", c.UserID(), templateApply{tpl.Name, ch.ID, mode})
	if err != nil {
		c.dbFailed("save pending apply", err)
		return
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("📑 Confirm Template Apply", summary, colourOrange)},
		confirmButtons("ptapply", c.UserID(), id))
}

func (b *Bot) onTemplateApplyConfirm(c *Ctx, extra []string) {
	if len(extra) < 1 {
		return
	}
	var job templateApply
	if !b.takePending(c, "ptapply", extra[0], &job) {
		return
	}
	done := func(e *discordgo.MessageEmbed) {
		c.Update([]*discordgo.MessageEmbed{e}, []discordgo.MessageComponent{})
	}
	tpl, err := b.db.Template(c.GuildID(), job.Name)
	ch := b.channel(job.ChannelID)
	if err != nil || tpl == nil || ch == nil {
		done(errEmbed("The template or the channel no longer exists."))
		return
	}
	resolved, _, err := b.resolveTemplate(c.GuildID(), tpl.Data)
	if err != nil {
		done(errEmbed("That template's data is damaged."))
		return
	}
	result, removed := templatePlan(ch.PermissionOverwrites, resolved, job.Mode)
	reason := fmt.Sprintf("Template '%s' applied by %s", tpl.Name, c.Member().User.String())
	if err := b.setOverwrites(ch.ID, result, reason); err != nil {
		c.logErr("apply template", err)
		done(errEmbed(fmt.Sprintf("I couldn't change %s (missing permissions?). Nothing was changed.", channelMention(ch.ID))))
		return
	}
	msg := fmt.Sprintf("Applied **%d** overwrite(s) and removed **%d** stale one(s) on %s.", len(resolved), removed, channelMention(ch.ID))
	if job.Mode != "replace" {
		msg = fmt.Sprintf("Added **%d** overwrite(s) from template **%s** to %s. Nothing was removed.", len(resolved), tpl.Name, channelMention(ch.ID))
	}
	e := okEmbed(msg)
	done(e)
	b.modLog(e)
}

// ---------------------------------------------------------------------
// /channelarchive
// ---------------------------------------------------------------------

// archiveChannel clones a channel in place (the clone is the new live
// channel), then moves the original into the newest "Channel Archive N"
// category with its permissions synced there (mods only). Nothing is read
// from the channel, so no Message Content intent is needed.
func (b *Bot) archiveChannel(guildID string, ch *discordgo.Channel, by string) (bool, string) {
	if ch.Type == discordgo.ChannelTypeGuildCategory {
		return false, "Categories can't be archived this way — pick a channel inside one instead."
	}
	if ch.IsThread() {
		return false, "Threads can't be archived this way — pick a channel instead."
	}
	clone, err := b.s.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name: ch.Name, Type: ch.Type, Topic: ch.Topic, Bitrate: ch.Bitrate, UserLimit: ch.UserLimit,
		RateLimitPerUser: ch.RateLimitPerUser, Position: ch.Position, PermissionOverwrites: ch.PermissionOverwrites,
		ParentID: ch.ParentID, NSFW: ch.NSFW,
	}, auditReason(fmt.Sprintf("Channel archived by %s (replacement)", by)))
	if err != nil {
		log.Printf("channel archive: clone #%s: %v", ch.Name, err)
		return false, "I don't have permission to create a replacement channel here."
	}
	cat := b.rolloverCategory(guildID, "Channel Archive", b.modOnlyOverwrites(guildID))
	if cat == nil {
		return false, fmt.Sprintf("Created %s as the replacement, but couldn't find or create a Channel Archive "+
			"category (missing permissions?). The original channel is untouched.", channelMention(clone.ID))
	}
	ows := cat.PermissionOverwrites
	if len(ows) == 0 {
		ows = b.modOnlyOverwrites(guildID)
	}
	if _, err := b.s.ChannelEdit(ch.ID, &discordgo.ChannelEdit{ParentID: cat.ID, PermissionOverwrites: ows},
		auditReason("Channel archived by "+by)); err != nil {
		log.Printf("channel archive: move #%s: %v", ch.Name, err)
		return false, fmt.Sprintf("Created %s as the replacement, but couldn't move the original into **%s** (missing permissions?).",
			channelMention(clone.ID), cat.Name)
	}
	return true, fmt.Sprintf("%s is now the live channel. The original was moved to **%s**.", channelMention(clone.ID), cat.Name)
}

func (b *Bot) cmdChannelArchive(c *Ctx) {
	id := c.i.ChannelID
	if opt := c.Channel("channel"); opt != nil {
		id = opt.ID
	}
	ch := b.channel(id)
	if ch == nil || ch.GuildID != c.GuildID() {
		c.Reply(true, errEmbed("I can't find that channel."))
		return
	}
	c.Defer(true)
	ok, note := b.archiveChannel(c.GuildID(), ch, c.Member().User.String())
	if !ok {
		c.Reply(true, errEmbed(note))
		return
	}
	c.Reply(true, okEmbed(note))
	b.modLog(makeEmbed("🗄️ Channel Archived",
		fmt.Sprintf("%s archived **#%s**. %s", userMention(c.UserID()), ch.Name, note), colourDarkTeal))
}

// ---------------------------------------------------------------------
// /readme
// ---------------------------------------------------------------------

const readmePageLimit = 3900 // under the embed description limit

// readmePages splits the README into embed-sized pages, preferring breaks
// at blank lines.
func readmePages(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{fmt.Sprintf("*Couldn't read `%s` — check `README_PATH` in `/setup`.*", path)}
	}
	var pages []string
	current := ""
	for _, para := range strings.Split(string(raw), "\n\n") {
		candidate := para
		if current != "" {
			candidate = current + "\n\n" + para
		}
		if len([]rune(candidate)) <= readmePageLimit {
			current = candidate
			continue
		}
		if current != "" {
			pages = append(pages, current)
			current = ""
		}
		r := []rune(para)
		if len(r) <= readmePageLimit {
			current = para
			continue
		}
		for i := 0; i < len(r); i += readmePageLimit {
			pages = append(pages, string(r[i:min(i+readmePageLimit, len(r))]))
		}
	}
	if current != "" {
		pages = append(pages, current)
	}
	if len(pages) == 0 {
		return []string{"*The README is empty.*"}
	}
	return pages
}

// readmeView renders one page; the page number is in the custom IDs
// ("readme:<page>"), and the file is read again on each click.
func (b *Bot) readmeView(page int) ([]*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	pages := readmePages(b.cfg.Str("README_PATH"))
	page = max(0, min(page, len(pages)-1))
	e := makeEmbed("📄 HEALTH Bot — README", pages[page], colourBlurple)
	e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Page %d/%d", page+1, len(pages))}
	return []*discordgo.MessageEmbed{e}, []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "◀ Back", Style: discordgo.PrimaryButton, CustomID: fmt.Sprintf("readme:%d", page-1), Disabled: page == 0},
		discordgo.Button{Label: "Next ▶", Style: discordgo.PrimaryButton, CustomID: fmt.Sprintf("readme:%d", page+1), Disabled: page == len(pages)-1},
	}}}
}

func (b *Bot) registerAdmin() {
	admin := int64(discordgo.PermissionAdministrator)
	b.addGroup(&group{name: "db", description: "Manage database backups (admin only).", defaultPerm: admin})
	b.addCommand(&command{name: "db backup", description: "Create a database backup now.", perm: permAdmin, handler: b.cmdDBBackup})
	b.addCommand(&command{
		name: "db list", description: "List saved database backups, or download one by name.", perm: permAdmin,
		options: []*discordgo.ApplicationCommandOption{optStr("filename", "A specific backup filename to download (see the list if omitted)", false)},
		handler: b.cmdDBList,
	})
	b.addCommand(&command{
		name: "db delete", description: "Delete a saved database backup.", perm: permAdmin,
		options: []*discordgo.ApplicationCommandOption{optStr("filename", "Backup filename to delete (see /db list)", true)},
		handler: b.cmdDBDelete,
	})
	b.addConfirm("dbdelete", b.onDBDeleteConfirm)

	b.addCommand(&command{
		name: "permsreport", description: "Generate a full channel-permissions report: synced vs. unsynced (mod only).",
		perm: permMod, defaultPerm: manageMessages, handler: b.cmdPermsReport,
	})

	anyChannel := func(name, desc string, required bool) *discordgo.ApplicationCommandOption {
		o := &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionChannel, Name: name, Description: desc, Required: required}
		o.ChannelTypes = []discordgo.ChannelType{discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildVoice,
			discordgo.ChannelTypeGuildCategory, discordgo.ChannelTypeGuildNews, discordgo.ChannelTypeGuildStageVoice,
			discordgo.ChannelTypeGuildForum, discordgo.ChannelTypeGuildMedia}
		return o
	}
	b.addGroup(&group{name: "permtemplate", description: "Manage channel permission templates (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{
		name: "permtemplate save", description: "Save a channel's current permission overwrites as a reusable template.", perm: permMod,
		options: []*discordgo.ApplicationCommandOption{anyChannel("channel", "Channel to copy permissions from", true),
			optStr("name", "Name to save the template as", true)},
		handler: b.cmdTemplateSave,
	})
	b.addCommand(&command{name: "permtemplate list", description: "List saved permission templates.", perm: permMod, handler: b.cmdTemplateList})
	mode := optStr("mode", "Add to the channel's current permissions, or replace them all", true)
	mode.Choices = []*discordgo.ApplicationCommandOptionChoice{
		{Name: "Add to current permissions", Value: "add"},
		{Name: "Replace all (remove what isn't in the template)", Value: "replace"},
	}
	b.addCommand(&command{
		name: "permtemplate apply", description: "Apply a saved permission template to a channel.", perm: permMod,
		options: []*discordgo.ApplicationCommandOption{optStr("name", "Template name (see /permtemplate list)", true),
			anyChannel("channel", "Channel to apply it to", true), mode},
		handler: b.cmdTemplateApply,
	})
	b.addConfirm("ptapply", b.onTemplateApplyConfirm)

	archivable := anyChannel("channel", "Channel to archive (defaults to the current channel)", false)
	archivable.ChannelTypes = slices.DeleteFunc(archivable.ChannelTypes, func(t discordgo.ChannelType) bool {
		return t == discordgo.ChannelTypeGuildCategory
	})
	b.addCommand(&command{
		name: "channelarchive", description: "Clone this channel as the new live one, and archive the original (mod only).",
		perm: permMod, defaultPerm: manageMessages, options: []*discordgo.ApplicationCommandOption{archivable},
		handler: b.cmdChannelArchive,
	})

	b.addCommand(&command{
		name: "readme", description: "Browse the bot's README (mod only).", perm: permMod, defaultPerm: manageMessages,
		handler: func(c *Ctx) {
			embeds, components := b.readmeView(0)
			c.ReplyComplex(true, embeds, components)
		},
	})
	b.addComponent("readme", func(c *Ctx) {
		page := 0
		if len(c.Args) > 0 {
			page, _ = strconv.Atoi(c.Args[0])
		}
		c.Update(b.readmeView(page))
	})
}
