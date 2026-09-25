package bot

import (
	"strings"
	"testing"
)

func TestRouterPermissionChecks(t *testing.T) {
	tb := newTestBot(t)

	tb.run(tUser, "warn add", userOpt("member", tTarget))
	tb.expect(true, "must be a moderator")

	tb.run(tMod, "setup")
	tb.expect(true, "don't have permission")

	// /ban needs Ban Members on top of the Discord-side default permission.
	tb.invokerPerms = map[string]int64{tUser: 0}
	tb.run(tUser, "ban", userOpt("member", tTarget))
	tb.expect(true, "don't have permission")
	tb.expectCall(0, "PUT", "/guilds/100/bans/.*")

	tb.run(tAdmin, "ban", userOpt("member", tTarget))
	tb.expectCall(1, "PUT", "/guilds/100/bans/300")
}

func TestRouterSetupModeGate(t *testing.T) {
	tb := newTestBot(t)
	tb.env.SetupMode = true

	tb.run(tMod, "warn add", userOpt("member", tTarget))
	tb.expect(true, "setup mode")
	if w, _ := tb.db.Warnings(tTarget, tGuild); len(w) != 0 {
		t.Error("warning saved in setup mode")
	}

	tb.api.reset()
	tb.run(tAdmin, "setup")
	tb.expect(true, "HEALTH Bot Setup")
}

func TestRouterIgnoresUnknown(t *testing.T) {
	tb := newTestBot(t)
	tb.click(tMod, "nosuchprefix:1:2")
	tb.run(tMod, "nosuchcommand")
	if calls := tb.callList(); len(calls) != 0 {
		t.Errorf("unexpected calls: %v", calls)
	}
}

func TestCommandDefinitions(t *testing.T) {
	tb := newTestBot(t)
	seen := map[string]bool{}
	for _, def := range tb.commandDefs() {
		if seen[def.Name] {
			t.Errorf("duplicate command %s", def.Name)
		}
		seen[def.Name] = true
		if def.Description == "" || len(def.Description) > 100 {
			t.Errorf("/%s: bad description length %d", def.Name, len(def.Description))
		}
		for _, o := range def.Options {
			if len(o.Description) > 100 || o.Name != strings.ToLower(o.Name) {
				t.Errorf("/%s %s: bad option", def.Name, o.Name)
			}
			for _, so := range o.Options {
				if len(so.Description) > 100 {
					t.Errorf("/%s %s %s: description too long", def.Name, o.Name, so.Name)
				}
			}
		}
	}
	for _, want := range []string{"setup", "ban", "warn", "jail", "arrest", "help"} {
		if !seen[want] {
			t.Errorf("missing /%s", want)
		}
	}
}

func TestAdminsPassModChecks(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tAdmin, "warn add", userOpt("member", tTarget)) // tAdmin has no MOD_ROLE
	tb.expect(false, "Member Warned")

	// Without MOD_ROLE set, only Administrators can use mod commands.
	tb.config(map[string]string{"MOD_ROLE": ""})
	tb.run(tMod, "warn add", userOpt("member", tTarget))
	tb.expect(true, "must be a moderator")
	tb.run(tAdmin, "jail list")
	tb.expect(true, "Nobody is currently in jail")
}
