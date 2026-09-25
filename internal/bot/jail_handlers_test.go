package bot

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/db"
)

// sqlExec runs a statement on the test database through a second connection,
// for fixtures that have no query function yet.
func (tb *testBot) sqlExec(query string, args ...any) {
	tb.t.Helper()
	conn, err := sql.Open("sqlite", tb.dbPath)
	if err != nil {
		tb.t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Exec(query, args...); err != nil {
		tb.t.Fatal(err)
	}
}

// setMemberRoles changes a fixture member's cached roles.
func (tb *testBot) setMemberRoles(userID string, roles ...string) {
	tb.t.Helper()
	m, err := tb.s.State.Member(tGuild, userID)
	if err != nil {
		tb.t.Fatal(err)
	}
	cp := *m
	cp.Roles = roles
	if err := tb.s.State.MemberAdd(&cp); err != nil {
		tb.t.Fatal(err)
	}
}

func (tb *testBot) sentence(guildID, userID, reason, releaseAt string, removed ...string) int64 {
	tb.t.Helper()
	id, err := tb.db.CreateSentence(guildID, userID, "name"+userID, tMod, reason, releaseAt, removed)
	if err != nil {
		tb.t.Fatal(err)
	}
	return id
}

type overwriteBody struct {
	Allow int64 `json:"allow,string"`
	Deny  int64 `json:"deny,string"`
}

func overwriteIn(c apiCall) overwriteBody {
	var o overwriteBody
	_ = json.Unmarshal(c.Body, &o)
	return o
}

func TestArrest(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "jail arrest", userOpt("member", tTarget), strOpt("time", "2h"), strOpt("reason", "spam"))
	tb.expect(false, "Member Arrested", "target", "Length:** 2h", "spam")

	edit := tb.expectCall(1, "PATCH", "/guilds/100/members/"+tTarget)
	if len(edit) == 1 {
		if got := strings.Join(rolesIn(edit[0]), ","); got != tRoleKeep+","+tRoleJail {
			t.Errorf("roles = %s, want the protected role and the jail role", got)
		}
		if edit[0].Reason != "spam" {
			t.Errorf("audit reason = %q", edit[0].Reason)
		}
	}
	s, _ := tb.db.ActiveSentence(tGuild, tTarget)
	if s == nil || strings.Join(s.RemovedRoles, ",") != tRoleStrip || s.Reason != "spam" {
		t.Fatalf("sentence = %+v", s)
	}
	if left := sentenceRemaining(s); left != "1h 59m" && left != "2h" {
		t.Errorf("remaining = %q", left)
	}
	if j := tb.sent(tChJail); !contains(j, "jailed for **2h**") || !contains(j, "Stripped **1**") {
		t.Errorf("jail channel = %v", j)
	}
	if !contains(tb.sent(tChModLog), "Member Arrested") {
		t.Error("arrest not logged")
	}

	tb.run(tMod, "arrest", userOpt("member", tTarget))
	tb.expect(true, "already serving a sentence")
}

func TestArrestReasonInTimeField(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "arrest", userOpt("member", tTarget), strOpt("time", "spamming"), strOpt("reason", "a lot"))
	tb.expect(false, "Length:** indefinite", "spamming a lot")
	if s, _ := tb.db.ActiveSentence(tGuild, tTarget); s == nil || s.ReleaseAt != "" || s.Reason != "spamming a lot" {
		t.Errorf("sentence = %+v", s)
	}
}

func TestArrestProblems(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"JAIL_ROLE": tRoleHigh})
	tb.run(tMod, "arrest", userOpt("member", tTarget))
	tb.expect(true, "above my highest role")

	tb.config(map[string]string{"JAIL_ROLE": ""})
	tb.run(tMod, "arrest", userOpt("member", tTarget))
	tb.expect(true, "No jail role configured")

	tb.run(tMod, "arrest", userOpt("member", "999999"))
	tb.expect(true, memberNotFound)
	tb.expectCall(0, "PATCH", ".*")
}

func TestJailList(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "jail list")
	tb.expect(true, "Nobody is currently in jail")

	future := time.Now().UTC().Add(3 * time.Hour).Format(db.SentenceTimeFormat)
	tb.sentence(tGuild, tTarget, "spam", future)
	tb.sentence(tGuild, "777", "", "")
	tb.sentence("other-guild", tUser, "", "")
	tb.run(tMod, "jail list")
	r := tb.expect(true, "2** member(s) in jail", userMention(tTarget), "name777** *(left server)*", "none given", "indefinite", "Remaining:** 2h 59m")
	if r.Kind != "edit" {
		t.Errorf("jail list should defer then edit, got %s", r.Kind)
	}
}

func TestJailAmend(t *testing.T) {
	tb := newTestBot(t)
	id := tb.sentence(tGuild, tTarget, "old", "")
	amend := func(opts ...*discordgo.ApplicationCommandInteractionDataOption) {
		tb.run(tMod, "jail amend", append([]*discordgo.ApplicationCommandInteractionDataOption{intOpt("sentence_id", int(id))}, opts...)...)
	}

	amend()
	tb.expect(true, "Nothing to change")

	amend(strOpt("time", "90m"))
	tb.expect(false, "Sentence Amended", "length → **1h 30m** from arrest time")
	s, _ := tb.db.SentenceByID(id)
	jailed, _ := parseSentenceTime(s.JailedAt)
	release, _ := parseSentenceTime(s.ReleaseAt)
	if release.Sub(jailed) != 90*time.Minute {
		t.Errorf("release - jailed = %v", release.Sub(jailed))
	}

	amend(strOpt("time", "Indefinite"), strOpt("reason", "new"))
	tb.expect(false, "length → **indefinite**", "reason → **new**")
	if s, _ := tb.db.SentenceByID(id); s.ReleaseAt != "" || s.Reason != "new" {
		t.Errorf("after amend: %+v", s)
	}

	amend(strOpt("time", "was rude"))
	tb.expect(false, "reason → **was rude**")

	tb.run(tMod, "jail amend", intOpt("sentence_id", 424242), strOpt("reason", "x"))
	tb.expect(true, "No sentence with ID `424242`")

	other := tb.sentence("other-guild", tUser, "", "")
	tb.run(tMod, "jail amend", intOpt("sentence_id", int(other)), strOpt("reason", "x"))
	tb.expect(true, "No sentence with ID")

	if err := tb.db.ReleaseSentence(id); err != nil {
		t.Fatal(err)
	}
	amend(strOpt("reason", "x"))
	tb.expect(true, "already been served")
}

func TestJailRelease(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "jail release", userOpt("member", tTarget))
	tb.expect(true, "isn't currently in jail")

	tb.setMemberRoles(tTarget, tRoleKeep, tRoleJail)
	id := tb.sentence(tGuild, tTarget, "spam", "", tRoleStrip, tRoleHigh, "424242")
	tb.run(tMod, "jail release", userOpt("member", tTarget))
	tb.expect(false, "Sentence Commuted", "Logged to their rap sheet")

	edit := tb.expectCall(1, "PATCH", "/guilds/100/members/"+tTarget)
	if len(edit) == 1 {
		// The jail role goes; the stripped role returns; a role above the
		// bot or one that no longer exists is skipped.
		if got := strings.Join(rolesIn(edit[0]), ","); got != tRoleKeep+","+tRoleStrip {
			t.Errorf("roles = %s", got)
		}
		if edit[0].Reason != "Commuted by moddy" {
			t.Errorf("audit reason = %q", edit[0].Reason)
		}
	}
	if s, _ := tb.db.SentenceByID(id); s.Active {
		t.Error("sentence still active")
	}
	w, _ := tb.db.Warnings(tTarget, tGuild)
	if len(w) != 1 || !strings.Contains(w[0].Reason, "Jailed for") || !strings.Contains(w[0].Reason, "spam") ||
		!strings.Contains(w[0].Reason, "commuted by moddy") {
		t.Errorf("rap sheet = %+v", w)
	}
}

func TestJailReleaseLoop(t *testing.T) {
	tb := newTestBot(t)
	past := time.Now().UTC().Add(-time.Minute).Format(db.SentenceTimeFormat)
	future := time.Now().UTC().Add(time.Hour).Format(db.SentenceTimeFormat)
	tb.setMemberRoles(tTarget, tRoleKeep, tRoleJail)
	due := tb.sentence(tGuild, tTarget, "spam", past, tRoleStrip)
	gone := tb.sentence(tGuild, "777", "", past)
	later := tb.sentence(tGuild, tUser, "", future)

	tb.runJailReleases()

	if edit := tb.expectCall(1, "PATCH", "/guilds/100/members/"+tTarget); len(edit) == 1 {
		if got := strings.Join(rolesIn(edit[0]), ","); got != tRoleKeep+","+tRoleStrip {
			t.Errorf("roles = %s", got)
		}
		if edit[0].Reason != "Sentence served" {
			t.Errorf("audit reason = %q", edit[0].Reason)
		}
	}
	for id, wantActive := range map[int64]bool{due: false, gone: false, later: true} {
		if s, _ := tb.db.SentenceByID(id); s.Active != wantActive {
			t.Errorf("sentence %d active = %v", id, s.Active)
		}
	}
	if log := tb.sent(tChModLog); len(log) != 2 || !contains(log, "name300** was released") || !contains(log, "Reason:** spam") {
		t.Errorf("mod log = %v", log)
	}
	tb.expectCall(1, "POST", "/users/@me/channels")
	if dm := tb.sent("7000"); len(dm) != 1 || !strings.Contains(dm[0], "released from jail in **Test Guild**") {
		t.Errorf("DM = %v", dm)
	}

	tb.api.reset()
	tb.runJailReleases()
	if calls := tb.callList(); len(calls) != 0 {
		t.Errorf("second run made calls: %v", calls)
	}
}

func TestJailSetup(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "jail setup")
	tb.expect(true, "applied to **8** channel(s)", "Double-check")
	if strings.Contains(tb.last().text(), "failed") || strings.Contains(tb.last().text(), "doesn't appear able to speak") {
		t.Errorf("unexpected warning: %q", tb.last().text())
	}
	if tb.cfg.Raw("JAIL_OVERWRITES_APPLIED") != "1" {
		t.Error("JAIL_OVERWRITES_APPLIED not set")
	}

	send := int64(discordgo.PermissionSendMessages)
	view := int64(discordgo.PermissionViewChannel)
	threads := int64(discordgo.PermissionCreatePublicThreads | discordgo.PermissionCreatePrivateThreads)
	for _, c := range tb.expectCall(8, "PUT", "/channels/[0-9]+/permissions/"+tRoleJail) {
		o := overwriteIn(c)
		if o.Deny&threads != threads {
			t.Errorf("%s: threads not denied", c.Path)
		}
		switch c.Path {
		case "/channels/" + tChJail + "/permissions/" + tRoleJail:
			if o.Allow&send == 0 || o.Deny&send != 0 {
				t.Errorf("jail channel: send not allowed: %+v", o)
			}
		case "/channels/" + tChVIP + "/permissions/" + tRoleJail:
			// Merged: the View Channel deny that was already there survives.
			if o.Deny&view == 0 || o.Deny&send == 0 {
				t.Errorf("vip channel overwrite not merged: %+v", o)
			}
		default:
			if o.Deny&send == 0 || o.Allow != 0 {
				t.Errorf("%s: %+v", c.Path, o)
			}
		}
	}
	tb.expectCall(0, "PUT", "/channels/"+tCatArchive+"/.*")
}

func TestJailSetupWarnings(t *testing.T) {
	tb := newTestBot(t)
	// @everyone can't send in the jail channel and the jail overwrite fails.
	ch, _ := tb.s.State.Channel(tChJail)
	ch.PermissionOverwrites = []*discordgo.PermissionOverwrite{{
		ID: tGuild, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionSendMessages,
	}}
	tb.api.fail("PUT", "/channels/"+tChJail+"/permissions/.*")
	tb.run(tMod, "jail setup")
	tb.expect(true, "applied to **7**", "**1** failed", "doesn't appear able to speak")

	tb.config(map[string]string{"JAIL_CHANNEL": ""})
	tb.run(tMod, "jail setup")
	tb.expect(true, "No jail channel configured")
}

// auditFixture adds a live ticket and a jailed VIP member, then runs /jail audit.
func auditFixture(t *testing.T) *testBot {
	tb := newTestBot(t)
	tb.sqlExec(`INSERT INTO tickets (guild_id, channel_id, user_id, username, number) VALUES (?, ?, ?, 'u', 1)`,
		tGuild, tChTicket, tUser)
	tb.sentence(tGuild, tVIPUser, "", "")
	tb.run(tMod, "jail audit")
	return tb
}

func TestJailAuditFindsProblems(t *testing.T) {
	tb := auditFixture(t)
	rs := tb.replies()
	if len(rs) != 3 {
		t.Fatalf("replies = %d, want progress + conflicts + prompt", len(rs))
	}
	conflict := rs[1].text()
	if !strings.Contains(conflict, channelMention(tChVIP)) || !strings.Contains(conflict, "**VIP**") ||
		!strings.Contains(conflict, userMention(tVIPUser)) {
		t.Errorf("conflict report = %q", conflict)
	}
	prompt := tb.expect(true, "4 channel(s) still visible to @Jail")
	for _, id := range []string{tChGeneral, tChModLog, tChSens, tChSupport} {
		if !strings.Contains(prompt.text(), channelMention(id)) {
			t.Errorf("prompt misses %s", id)
		}
	}
	for _, id := range []string{tChJail, tChArchived, tChTicket, tChVIP} {
		if strings.Contains(prompt.text(), channelMention(id)) {
			t.Errorf("prompt lists %s, which should be skipped", id)
		}
	}
	if len(prompt.Components) != 2 {
		t.Fatalf("components = %+v", prompt.Components)
	}
	tb.expectCall(0, "PUT", ".*")
}

func TestJailAuditConfirm(t *testing.T) {
	tb := auditFixture(t)
	yes := tb.componentID(tb.last(), "confirm:yes:jailaudit:"+tMod+":")

	tb.click(tUser, yes)
	tb.expect(true, "isn't your prompt")
	tb.expectCall(0, "PUT", ".*")

	tb.click(tMod, yes)
	rs := tb.replies()
	if rs[len(rs)-2].Kind != "defer-update" {
		t.Errorf("confirm did not defer: %+v", rs[len(rs)-2])
	}
	r := tb.expect(true, "Denied View Channel", "**4** channel(s)")
	if !r.Cleared {
		t.Error("buttons not removed after confirm")
	}
	for _, c := range tb.expectCall(4, "PUT", "/channels/[0-9]+/permissions/"+tRoleJail) {
		if overwriteIn(c).Deny&discordgo.PermissionViewChannel == 0 {
			t.Errorf("%s: view not denied", c.Path)
		}
		if c.Reason != "Jail visibility audit by moddy" {
			t.Errorf("audit reason = %q", c.Reason)
		}
	}
	if !contains(tb.sent(tChModLog), "Denied View Channel") {
		t.Error("fix not logged")
	}
}

func TestJailAuditCancelAndExpiry(t *testing.T) {
	tb := auditFixture(t)
	no := tb.componentID(tb.last(), "confirm:no:jailaudit:")
	tb.click(tMod, no)
	r := tb.expect(true, "Cancelled. No changes made.")
	if r.Kind != "update" || !r.Cleared {
		t.Errorf("cancel reply = %+v", r)
	}

	tb.click(tMod, "confirm:yes:jailaudit:"+tMod+":1")
	r = tb.expect(true, "Timed out. No changes made.")
	if r.Kind != "update" || len(r.Components) != 2 || !r.Components[0].Disabled || !r.Components[1].Disabled {
		t.Errorf("expired reply = %+v", r)
	}
	tb.expectCall(0, "PUT", ".*")
}

func TestJailAuditNothingToFix(t *testing.T) {
	tb := newTestBot(t)
	// Without View Channel on @everyone, the jail role sees nothing at all,
	// not even the jail channel.
	g, _ := tb.s.State.Guild(tGuild)
	g.Roles[0].Permissions = 0
	tb.run(tMod, "jail audit")
	tb.expect(true, "can't see anything outside", "doesn't currently appear able to see")
	if r := tb.last(); len(r.Components) != 0 {
		t.Error("prompt shown with nothing to fix")
	}
}

func TestReleaseUsesFreshRoles(t *testing.T) {
	tb := newTestBot(t)
	// The cache still has the roles from before a mod added VIP by hand.
	tb.setMemberRoles(tTarget, tRoleKeep, tRoleJail)
	tb.api.on("GET", "/guilds/100/members/"+tTarget, 200,
		`{"user":{"id":"300","username":"target","discriminator":"0"},"roles":["13","11","16"]}`)
	tb.sentence(tGuild, tTarget, "", "", tRoleStrip)
	tb.run(tMod, "jail release", userOpt("member", tTarget))
	if edit := tb.expectCall(1, "PATCH", "/guilds/100/members/"+tTarget); len(edit) == 1 {
		if got := strings.Join(rolesIn(edit[0]), ","); got != tRoleKeep+","+tRoleVIP+","+tRoleStrip {
			t.Errorf("roles = %s, want the API's roles minus jail plus the stripped role", got)
		}
	}

	// A member who left is released without a role edit.
	tb.api.reset()
	tb.sentence(tGuild, "777", "", time.Now().UTC().Add(-time.Minute).Format(db.SentenceTimeFormat))
	tb.runJailReleases()
	tb.expectCall(0, "PATCH", ".*")
}

func TestArrestKeepsManagedRoles(t *testing.T) {
	tb := newTestBot(t)
	tb.setMemberRoles(tTarget, tRoleStrip, tRoleBoost)
	tb.run(tMod, "arrest", userOpt("member", tTarget))
	tb.expect(false, "Member Arrested")
	if edit := tb.expectCall(1, "PATCH", "/guilds/100/members/"+tTarget); len(edit) == 1 {
		if got := strings.Join(rolesIn(edit[0]), ","); got != tRoleBoost+","+tRoleJail {
			t.Errorf("roles = %s, want the managed role kept", got)
		}
	}
	s, _ := tb.db.ActiveSentence(tGuild, tTarget)
	if s == nil || strings.Join(s.RemovedRoles, ",") != tRoleStrip {
		t.Errorf("sentence = %+v", s)
	}

	// A managed role in an old sentence record is never added back.
	tb.api.reset()
	tb.setMemberRoles(tTarget, tRoleJail)
	if err := tb.db.ReleaseSentence(s.ID); err != nil {
		t.Fatal(err)
	}
	tb.sentence(tGuild, tTarget, "", "", tRoleStrip, tRoleBoost)
	tb.run(tMod, "jail release", userOpt("member", tTarget))
	if edit := tb.expectCall(1, "PATCH", "/guilds/100/members/"+tTarget); len(edit) == 1 {
		if got := strings.Join(rolesIn(edit[0]), ","); got != tRoleStrip {
			t.Errorf("roles = %s", got)
		}
	}
}
