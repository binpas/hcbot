package bot

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/binpas/hcbot/internal/db"
)

func TestSelfTimeout(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tUser, "timeout", intOpt("minutes", 0))
	tb.expect(true, "between 1 and 1440")
	tb.expectCall(0, "PATCH", "/guilds/100/members/.*")

	tb.run(tUser, "timeout", intOpt("minutes", 15))
	tb.expect(true, "Enjoy your break", "15 min")
	calls := tb.expectCall(1, "PATCH", "/guilds/100/members/"+tUser)
	if len(calls) == 1 {
		until, _ := calls[0].JSON()["communication_disabled_until"].(string)
		ts, err := time.Parse(time.RFC3339, until)
		if err != nil || time.Until(ts) < 14*time.Minute || time.Until(ts) > 16*time.Minute {
			t.Errorf("communication_disabled_until = %q", until)
		}
	}

	tb.api.fail("PATCH", "/guilds/100/members/"+tUser)
	tb.run(tUser, "timeout")
	tb.expect(true, "don't have permission to timeout you")
}

func TestBanKick(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "ban", userOpt("member", tTarget), strOpt("reason", "spam"))
	tb.expect(false, "Member Banned", "target", "spam")
	if c := tb.expectCall(1, "PUT", "/guilds/100/bans/"+tTarget); len(c) == 1 && c[0].Reason != "spam" {
		t.Errorf("ban reason not sent: %+v", c[0])
	}
	if log := tb.sent(tChModLog); !contains(log, "Member Banned") || !contains(log, "Jump to context") {
		t.Errorf("mod log = %v", log)
	}

	tb.run(tMod, "kick", userOpt("member", tTarget))
	tb.expect(false, "Member Kicked", noReason)
	if c := tb.expectCall(1, "DELETE", "/guilds/100/members/"+tTarget); len(c) == 1 && c[0].Reason != noReason {
		t.Errorf("kick audit reason = %q", c[0].Reason)
	}

	tb.run(tMod, "kick", userOpt("member", "999999"))
	tb.expect(true, memberNotFound)

	tb.api.fail("PUT", "/guilds/100/bans/.*")
	tb.run(tMod, "ban", userOpt("member", tTarget))
	tb.expect(true, "I couldn't ban target")
}

func TestTempbanSchedulesUnban(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "tempban", userOpt("member", tTarget), intOpt("duration", 30), strOpt("reason", "cool off"))
	tb.expect(false, "Temp-Banned", "30 min", "cool off")
	if c := tb.expectCall(1, "PUT", "/guilds/100/bans/"+tTarget); len(c) == 1 &&
		c[0].Reason != "[Temp-ban: 30 min] cool off" {
		t.Errorf("tempban reason missing: %+v", c[0])
	}
	if due, _ := tb.db.DueActions(); len(due) != 0 {
		t.Fatalf("unban due too early: %+v", due)
	}

	// A zero-minute tempban is due at once; the loop unbans and logs it.
	tb.run(tMod, "tempban", userOpt("member", tVIPUser), intOpt("duration", 0))
	tb.runScheduledActions()
	tb.expectCall(1, "DELETE", "/guilds/100/bans/"+tVIPUser)
	if !contains(tb.sent(tChModLog), "Temp-Ban Expired") {
		t.Error("expiry not logged")
	}
	if due, _ := tb.db.DueActions(); len(due) != 0 {
		t.Error("action not removed after running")
	}

	// A failed unban is still removed, so it can't retry forever.
	tb.api.fail("DELETE", "/guilds/100/bans/.*")
	if err := tb.db.ScheduleAction(tGuild, tUser, "user", db.ActionUnban, "", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	tb.runScheduledActions()
	if due, _ := tb.db.DueActions(); len(due) != 0 {
		t.Error("failed action not removed")
	}
}

func TestMuteUnmute(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "mute", userOpt("member", tTarget), intOpt("duration", 5))
	tb.expect(false, "Member Muted", "5 min")
	c := tb.expectCall(1, "PATCH", "/guilds/100/members/"+tTarget)
	if len(c) == 1 && c[0].JSON()["communication_disabled_until"] == nil {
		t.Error("mute sent no timeout")
	}

	tb.api.reset()
	tb.run(tMod, "unmute", userOpt("member", tTarget))
	tb.expect(false, "Member Unmuted")
	c = tb.expectCall(1, "PATCH", "/guilds/100/members/"+tTarget)
	if len(c) == 1 {
		if v, ok := c[0].JSON()["communication_disabled_until"]; !ok || v != nil {
			t.Errorf("unmute body = %s", c[0].Body)
		}
	}
	if log := tb.sent(tChModLog); !contains(log, "Member Unmuted") || contains(log, "Jump to context") {
		t.Errorf("unmute mod log = %v", log)
	}

	tb.api.fail("PATCH", "/guilds/100/members/.*")
	tb.run(tMod, "mute", userOpt("member", tTarget))
	tb.expect(true, "I couldn't mute")
}

func TestPurge(t *testing.T) {
	tb := newTestBot(t)
	now := time.Now().UTC()
	old := now.Add(-20 * 24 * time.Hour)
	msg := func(id, author string, ts time.Time) string {
		return fmt.Sprintf(`{"id":%q,"channel_id":%q,"timestamp":%q,"author":{"id":"1%s","username":%q,"discriminator":"0"}}`,
			id, tChGeneral, ts.Format(time.RFC3339), id, author)
	}
	tb.api.on("GET", "/channels/"+tChGeneral+"/messages", 200,
		"["+msg("1", "alice", now)+","+msg("2", "alice", now)+","+msg("3", "bob", now)+","+msg("4", "carol", old)+"]")

	tb.run(tMod, "purge", intOpt("amount", 4))
	tb.expect(true, "Deleted **4** messages")
	if r := tb.replies(); r[0].Kind != "defer" || !r[0].Ephemeral {
		t.Errorf("purge did not defer ephemerally: %+v", r[0])
	}
	if get := tb.api.find("GET", "/channels/"+tChGeneral+"/messages"); len(get) != 1 || !strings.Contains(get[0].Query, "limit=4") {
		t.Errorf("fetch = %+v", get)
	}
	bulk := tb.expectCall(1, "POST", "/channels/"+tChGeneral+"/messages/bulk-delete")
	if len(bulk) == 1 && !strings.Contains(string(bulk[0].Body), `"1","2","3"`) {
		t.Errorf("bulk delete body = %s", bulk[0].Body)
	}
	tb.expectCall(1, "DELETE", "/channels/"+tChGeneral+"/messages/4")
	if log := tb.sent(tChModLog); !contains(log, "**alice**: 2") || !contains(log, "**carol**: 1") {
		t.Errorf("purge mod log = %v", log)
	}

	tb.api.fail("GET", "/channels/"+tChGeneral+"/messages")
	tb.run(tMod, "purge")
	tb.expect(true, "couldn't read this channel's messages")
}

func TestMotd(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "motd", userOpt("member", tTarget))
	tb.expect(false, "Member Of The Day")
	tb.expectCall(1, "PUT", "/guilds/100/members/"+tTarget+"/roles/"+tRoleMOTD)
	if err := tb.db.ScheduleAction(tGuild, tTarget, "target", db.ActionRemoveRole, tRoleMOTD, time.Now()); err != nil {
		t.Fatal(err)
	}
	tb.runScheduledActions()
	tb.expectCall(1, "DELETE", "/guilds/100/members/"+tTarget+"/roles/"+tRoleMOTD)
	if !contains(tb.sent(tChModLog), "MOTD Expired") {
		t.Error("MOTD expiry not logged")
	}

	tb.config(map[string]string{"MOTD_ROLE": "424242"})
	tb.run(tMod, "motd", userOpt("member", tTarget))
	tb.expect(true, "MOTD role not found")
}

func TestWarnFlow(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"WARN_ARREST_THRESHOLD": "2", "WARN_KICK_THRESHOLD": "3", "WARN_ARREST_DURATION": "30"})

	tb.run(tMod, "warn add", userOpt("member", tTarget), strOpt("reason", "first"))
	tb.expect(false, "Member Warned", "first", "Total warnings:** 1")
	tb.expectCall(0, "PATCH", "/guilds/100/members/"+tTarget)

	tb.run(tMod, "warn add", userOpt("member", tTarget))
	tb.expect(false, "Auto-Arrested", "30m", "2 warnings")
	edit := tb.expectCall(1, "PATCH", "/guilds/100/members/"+tTarget)
	if len(edit) == 1 && strings.Join(rolesIn(edit[0]), ",") != tRoleKeep+","+tRoleJail {
		t.Errorf("auto-arrest roles = %v", rolesIn(edit[0]))
	}
	if s, _ := tb.db.ActiveSentence(tGuild, tTarget); s == nil || s.ReleaseAt == "" {
		t.Errorf("auto-arrest sentence = %+v", s)
	}

	tb.run(tMod, "warn add", userOpt("member", tTarget))
	tb.expect(false, "Auto-Kicked", "3 warnings")
	if c := tb.expectCall(1, "DELETE", "/guilds/100/members/"+tTarget); len(c) == 1 && c[0].Reason != "Auto-kick: reached 3 warnings" {
		t.Errorf("auto-kick reason = %q", c[0].Reason)
	}

	tb.run(tMod, "warn history", userOpt("member", tTarget))
	r := tb.expect(true, "Rap Sheet", "3 warning(s)", "first", noReason)
	if len(r.Embeds[0].Fields) != 3 {
		t.Errorf("rap sheet fields = %d", len(r.Embeds[0].Fields))
	}

	rows, _ := tb.db.Warnings(tTarget, tGuild)
	tb.run(tMod, "warn remove", intOpt("warning_id", int(rows[0].ID)))
	tb.expect(false, "Warning Removed")
	tb.run(tMod, "warn remove", intOpt("warning_id", int(rows[0].ID)))
	tb.expect(true, "No warning with ID")

	tb.run(tMod, "warn clear", userOpt("member", tTarget))
	tb.expect(false, "Warnings Cleared")
	tb.run(tMod, "rapsheet", userOpt("member", tTarget))
	tb.expect(true, "No warnings on record")
}

func TestWarnAutoArrestFailure(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"WARN_ARREST_THRESHOLD": "1"})
	tb.api.fail("PATCH", "/guilds/100/members/.*")
	tb.run(tMod, "warn add", userOpt("member", tTarget))
	tb.expect(false, "Auto-Arrest Failed", "couldn't update")
	if s, _ := tb.db.ActiveSentence(tGuild, tTarget); s != nil {
		t.Error("sentence saved although the role change failed")
	}
}

func TestRapsheetCapsFields(t *testing.T) {
	tb := newTestBot(t)
	for i := 0; i < 30; i++ {
		if _, err := tb.db.AddWarning(tTarget, tGuild, fmt.Sprint("w", i)); err != nil {
			t.Fatal(err)
		}
	}
	tb.run(tMod, "rapsheet", userOpt("member", tTarget))
	r := tb.expect(true, "30 warning(s)", "showing the oldest 25")
	if len(r.Embeds[0].Fields) != 25 {
		t.Errorf("fields = %d", len(r.Embeds[0].Fields))
	}
}

func TestUserinfoRedirects(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "userinfo", userOpt("member", tTarget))
	tb.expect(true, "Sent to "+channelMention(tChSens))
	if log := tb.sent(tChSens); !contains(log, "target") || !contains(log, "Roles (2)") {
		t.Errorf("sensitive log = %v", log)
	}

	tb.api.reset()
	tb.config(map[string]string{"SENSITIVE_LOG": "", "BIG_BROTHER": ""})
	tb.run(tMod, "userinfo")
	r := tb.replies()
	if len(r) != 2 || !strings.Contains(r[0].text(), "No sensitive-log channel") ||
		!strings.Contains(r[1].text(), "moddy") || !r[1].Ephemeral {
		t.Errorf("fallback replies = %+v", r)
	}
}

func TestServerinfo(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "serverinfo")
	tb.expect(true, "Test Guild", userMention(tOwner), "Server ID: "+tGuild)
}

func TestHelp(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tUser, "help")
	r := tb.expect(true, "Command Reference", "/timeout")
	if strings.Contains(r.text(), "/ban") {
		t.Error("non-mod sees mod commands")
	}

	tb.config(map[string]string{"WARN_ARREST_THRESHOLD": "4"})
	tb.run(tMod, "help")
	tb.expect(true, "/ban", "/jail audit", "Auto-arrests at **4** warnings")
}

func TestAuditReasonEncoding(t *testing.T) {
	tb := newTestBot(t)
	reason := "100% spam — ünïcode 🚨 a/b"
	tb.run(tMod, "ban", userOpt("member", tTarget), strOpt("reason", reason))
	c := tb.expectCall(1, "PUT", "/guilds/100/bans/"+tTarget)
	if len(c) == 1 && c[0].Reason != reason {
		t.Errorf("decoded reason = %q, want %q", c[0].Reason, reason)
	}
}
