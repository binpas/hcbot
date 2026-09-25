package bot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestNextDailyAndHumanSize(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	for now, want := range map[string]string{
		"2026-09-24T03:59:00Z": "2026-09-24T04:00:00Z",
		"2026-09-24T04:00:00Z": "2026-09-25T04:00:00Z",
		"2026-09-24T23:00:00Z": "2026-09-25T04:00:00Z",
	} {
		if got := nextDaily(at(now), 4, 0); !got.Equal(at(want)) {
			t.Errorf("nextDaily(%s) = %s, want %s", now, got, want)
		}
	}
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KB", 5 << 20: "5.0 MB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

// lastFiles returns the files of the last reply that had any.
func (tb *testBot) lastFiles() map[string][]byte {
	tb.api.mu.Lock()
	defer tb.api.mu.Unlock()
	for i := len(tb.api.calls) - 1; i >= 0; i-- {
		if len(tb.api.calls[i].Files) > 0 {
			return tb.api.calls[i].Files
		}
	}
	return nil
}

func TestDBBackups(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "db backup")
	tb.expect(true, "don't have permission")

	tb.run(tAdmin, "db list")
	tb.expect(true, "No database backups saved yet")

	tb.run(tAdmin, "db backup")
	tb.expect(true, "Backup saved as `health_bot_")
	list := tb.backups()
	if len(list) != 1 {
		t.Fatalf("backups = %v", list)
	}
	name := list[0]
	if f := tb.lastFiles()[name]; len(f) == 0 {
		t.Error("backup file not sent")
	}

	tb.run(tAdmin, "db list")
	tb.expect(true, "Backups (1)", "`"+name+"`")
	tb.run(tAdmin, "db list", strOpt("filename", name))
	tb.expect(true, "`"+name+"`")
	tb.run(tAdmin, "db list", strOpt("filename", "../health_bot.db"))
	tb.expect(true, "No backup named")

	tb.api.fail("PATCH", "/webhooks/.*/messages/@original")
	tb.run(tAdmin, "db list", strOpt("filename", name))
	tb.expect(true, "too large to send")

	tb.run(tAdmin, "db delete", strOpt("filename", "nope.db"))
	tb.expect(true, "doesn't look like a valid backup filename")
	tb.run(tAdmin, "db delete", strOpt("filename", name))
	yes := tb.componentID(tb.expect(true, "Permanently delete `"+name+"`"), "confirm:yes:dbdelete:")
	tb.click(tAdmin, yes)
	tb.expect(true, "Deleted `"+name+"`")
	if len(tb.backups()) != 0 {
		t.Error("backup not deleted")
	}
}

func TestDailyBackupOnlyWhenChanged(t *testing.T) {
	tb := newTestBot(t)
	tb.runDailyBackup() // no backups yet: make one
	if len(tb.backups()) != 1 || !contains(tb.sent(tChModLog), "Automatic Backup") {
		t.Fatalf("first run: %v", tb.backups())
	}
	// The database is older than the newest backup: nothing to do.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tb.db.Path, past, past); err != nil {
		t.Fatal(err)
	}
	tb.runDailyBackup()
	if len(tb.backups()) != 1 {
		t.Fatal("backed up an unchanged database")
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(tb.db.Path, future, future); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // backup names have one-second resolution
	tb.runDailyBackup()
	if len(tb.backups()) != 2 {
		t.Error("changed database not backed up")
	}
}

func TestPermsReport(t *testing.T) {
	tb := newTestBot(t)
	role := discordgo.PermissionOverwriteTypeRole
	view := int64(discordgo.PermissionViewChannel)
	staffOW := []*discordgo.PermissionOverwrite{{ID: tRoleMod, Type: role, Allow: view}}
	add := func(ch *discordgo.Channel) {
		ch.GuildID = tGuild
		if err := tb.s.State.ChannelAdd(ch); err != nil {
			t.Fatal(err)
		}
	}
	add(&discordgo.Channel{ID: "610", Name: "Staff", Type: discordgo.ChannelTypeGuildCategory, Position: 1, PermissionOverwrites: staffOW})
	add(&discordgo.Channel{ID: "611", Name: "synced", ParentID: "610", PermissionOverwrites: staffOW})
	add(&discordgo.Channel{ID: "612", Name: "unsynced", ParentID: "610", PermissionOverwrites: []*discordgo.PermissionOverwrite{
		{ID: tRoleMod, Type: role, Allow: view}, {ID: tTarget, Type: discordgo.PermissionOverwriteTypeMember, Deny: discordgo.PermissionSendMessages}}})

	tb.run(tMod, "permsreport")
	tb.expect(true, "Report generated", "(skipped 1 'archive' category and 1 'ticket' channel(s).)")
	report := string(tb.lastFiles()["permissions_report_"+tGuild+".txt"])
	for _, want := range []string{
		"PERMISSIONS REPORT — Test Guild", "by moddy",
		"📁 Staff", "Synced channels: #synced",
		"# unsynced  (category: Staff)", "target", "Send: ❌",
		"# vip  (no category)", "@VIP", "@Jail",
		"(no explicit overwrites — inherits from @everyone)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report misses %q", want)
		}
	}
	if strings.Contains(report, "old-stuff") || strings.Contains(report, "ticket-0001") {
		t.Error("report lists archive or ticket channels")
	}

	// It also works in setup mode.
	tb.env.SetupMode = true
	tb.run(tMod, "permsreport")
	tb.expect(true, "Report generated")
}

func TestPermTemplates(t *testing.T) {
	tb := newTestBot(t)
	chOpt := func(name, id string) *discordgo.ApplicationCommandInteractionDataOption {
		return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionChannel, Value: id}
	}
	tb.run(tMod, "permtemplate list")
	tb.expect(true, "No permission templates saved yet")
	tb.run(tMod, "permtemplate save", chOpt("channel", tChVIP), strOpt("name", "VIP Only"))
	tb.expect(true, "Saved template **vip only**", "**2** permission overwrite(s)")
	tpl, _ := tb.db.Template(tGuild, "vip only")
	var entries []map[string]any
	if err := json.Unmarshal([]byte(tpl.Data), &entries); err != nil || len(entries) != 2 {
		t.Fatalf("template data = %s", tpl.Data)
	}
	if _, isNumber := entries[0]["id"].(float64); !isNumber || entries[0]["kind"] != "role" || entries[0]["name"] != "VIP" {
		t.Errorf("not in the Python format: %v", entries[0])
	}
	tb.run(tMod, "permtemplate list")
	tb.expect(true, "**vip only** — from `#vip`, saved by "+userMention(tMod))

	// The support channel has a member overwrite and a Jail overwrite.
	sup, _ := tb.s.State.Channel(tChSupport)
	sup.PermissionOverwrites = []*discordgo.PermissionOverwrite{
		{ID: tTarget, Type: discordgo.PermissionOverwriteTypeMember, Deny: discordgo.PermissionSendMessages},
		{ID: tRoleJail, Type: discordgo.PermissionOverwriteTypeRole, Allow: discordgo.PermissionSendMessages},
	}
	sent := func() []overwriteBody2 {
		c := tb.api.find("PATCH", "/channels/"+tChSupport)
		var body struct {
			Overwrites []overwriteBody2 `json:"permission_overwrites"`
		}
		_ = json.Unmarshal(c[len(c)-1].Body, &body)
		return body.Overwrites
	}

	tb.run(tMod, "permtemplate apply", strOpt("name", "VIP ONLY"), chOpt("channel", tChSupport), strOpt("mode", "replace"))
	tb.expect(true, "**2** overwrite(s) will be set", "**1** existing overwrite(s)")
	tb.click(tMod, tb.componentID(tb.last(), "confirm:yes:ptapply:"))
	tb.expect(true, "Applied **2** overwrite(s) and removed **1** stale one(s)")
	if got := sent(); len(got) != 2 || got[0].ID != tRoleVIP || got[1].ID != tRoleJail || got[1].Deny != int64(discordgo.PermissionViewChannel) || got[1].Allow != 0 {
		t.Errorf("replace sent %+v", got)
	}
	if !contains(tb.sent(tChModLog), "Applied **2**") {
		t.Error("apply not logged")
	}

	tb.run(tMod, "permtemplate apply", strOpt("name", "vip only"), chOpt("channel", tChSupport), strOpt("mode", "add"))
	tb.expect(true, "added to the current ones", "Nothing will be removed")
	tb.click(tMod, tb.componentID(tb.last(), "confirm:yes:ptapply:"))
	tb.expect(true, "Added **2** overwrite(s)")
	got := sent()
	if len(got) != 3 || got[0].ID != tTarget {
		t.Fatalf("add sent %+v", got)
	}
	// Jail: its Send allow stays, the template's View deny is merged in.
	if got[1].ID != tRoleJail || got[1].Allow != int64(discordgo.PermissionSendMessages) || got[1].Deny != int64(discordgo.PermissionViewChannel) {
		t.Errorf("merged jail overwrite = %+v", got[1])
	}

	// A role that no longer exists is skipped; a refused change changes nothing.
	if err := tb.db.SaveTemplate(tGuild, "gone", "x", `[{"kind":"role","id":424242,"name":"Old","allow":1024,"deny":0}]`, ""); err != nil {
		t.Fatal(err)
	}
	tb.api.fail("PATCH", "/channels/"+tChSupport)
	tb.run(tMod, "permtemplate apply", strOpt("name", "gone"), chOpt("channel", tChSupport), strOpt("mode", "replace"))
	tb.expect(true, "couldn't be resolved", "role **Old** (`424242`)")
	tb.click(tMod, tb.componentID(tb.last(), "confirm:yes:ptapply:"))
	tb.expect(true, "Nothing was changed")

	tb.run(tMod, "permtemplate apply", strOpt("name", "nope"), chOpt("channel", tChSupport), strOpt("mode", "add"))
	tb.expect(true, "No template named `nope`")
}

type overwriteBody2 struct {
	ID    string `json:"id"`
	Allow int64  `json:"allow,string"`
	Deny  int64  `json:"deny,string"`
}

func TestChannelArchive(t *testing.T) {
	tb := newTestBot(t)
	vip, _ := tb.s.State.Channel(tChVIP)
	vip.Topic, vip.RateLimitPerUser = "VIPs only", 10
	tb.run(tMod, "channelarchive", &discordgo.ApplicationCommandInteractionDataOption{
		Name: "channel", Type: discordgo.ApplicationCommandOptionChannel, Value: tChVIP})
	tb.expect(true, channelMention("601")+" is now the live channel", "**Channel Archive 1**")

	posts := tb.expectCall(2, "POST", "/guilds/100/channels")
	if len(posts) == 2 {
		clone, cat := channelIn(posts[0]), channelIn(posts[1])
		if clone.Name != "vip" || len(clone.PermissionOverwrites) != 2 || !strings.Contains(string(posts[0].Body), `"topic":"VIPs only"`) {
			t.Errorf("clone = %s", posts[0].Body)
		}
		if cat.Name != "Channel Archive 1" || cat.Type != int(discordgo.ChannelTypeGuildCategory) {
			t.Errorf("category = %+v", cat)
		}
	}
	if edit := tb.expectCall(1, "PATCH", "/channels/"+tChVIP); len(edit) == 1 {
		if e := channelIn(edit[0]); e.ParentID != "602" || overwriteFor(e.PermissionOverwrites, tGuild) == nil {
			t.Errorf("move = %s", edit[0].Body)
		}
	}
	if !contains(tb.sent(tChModLog), "Channel Archived") {
		t.Error("not logged")
	}

	if err := tb.s.State.ChannelAdd(&discordgo.Channel{ID: "620", GuildID: tGuild, Name: "a thread",
		Type: discordgo.ChannelTypeGuildPublicThread, ParentID: tChGeneral}); err != nil {
		t.Fatal(err)
	}
	tb.runIn("620", tMod, "channelarchive")
	tb.expect(true, "Threads can't be archived")
}

func TestReadme(t *testing.T) {
	path := filepath.Join(t.TempDir(), "README.md")
	para := strings.Repeat("word ", 700) // 3500 characters
	if err := os.WriteFile(path, []byte(para+"\n\n"+para+"\n\n"+strings.Repeat("x", 9000)), 0o644); err != nil {
		t.Fatal(err)
	}
	pages := readmePages(path)
	if len(pages) != 5 { // two paragraphs, then 9000 characters split in 3
		t.Fatalf("pages = %d", len(pages))
	}
	for _, p := range pages {
		if len([]rune(p)) > readmePageLimit {
			t.Error("page too long")
		}
	}

	tb := newTestBot(t)
	tb.config(map[string]string{"README_PATH": path})
	tb.run(tMod, "readme")
	r := tb.expect(true, "HEALTH Bot — README", "Page 1/5")
	if !r.Components[0].Disabled || r.Components[1].Disabled {
		t.Errorf("buttons on page 1 = %+v", r.Components)
	}
	tb.click(tMod, r.Components[1].CustomID)
	if r := tb.last(); r.Kind != "update" || !strings.Contains(r.text(), "Page 2/5") {
		t.Errorf("next = %s %q", r.Kind, r.text())
	}
	tb.click(tMod, "readme:99")
	tb.expect(true, "Page 5/5")

	tb.config(map[string]string{"README_PATH": filepath.Join(t.TempDir(), "missing.md")})
	tb.run(tMod, "readme")
	tb.expect(true, "Couldn't read", "README_PATH")
}
