package bot

import (
	"fmt"
	"strings"
	"testing"

	"github.com/binpas/hcbot/internal/config"
)

// componentID returns the custom ID in the reply that starts with prefix.
func (tb *testBot) componentID(r reply, prefix string) string {
	tb.t.Helper()
	for _, c := range r.Components {
		if strings.HasPrefix(c.CustomID, prefix) {
			return c.CustomID
		}
	}
	tb.t.Fatalf("no component %q in %+v", prefix, r.Components)
	return ""
}

func TestSetupSelectSavesValue(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tAdmin, "setup")
	r := tb.expect(true, "HEALTH Bot Setup", "MOD_LOG", "Page 1/")
	id := tb.componentID(r, "setup:ch:"+tAdmin+":0:")
	if !strings.HasSuffix(id, ":MOD_LOG") {
		t.Fatalf("first select is %q", id)
	}

	tb.click(tAdmin, id, tChGeneral)
	if got := tb.cfg.ID("MOD_LOG"); got != tChGeneral {
		t.Errorf("MOD_LOG = %q", got)
	}
	if got, _ := tb.db.LoadConfig(); got["MOD_LOG"] != tChGeneral {
		t.Errorf("MOD_LOG not saved in the database: %v", got)
	}
	if r := tb.last(); r.Kind != "update" || !strings.Contains(r.text(), channelMention(tChGeneral)) {
		t.Errorf("setup view not refreshed: %s %q", r.Kind, r.text())
	}
}

func TestSetupPagingAndRoleList(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tAdmin, "setup")
	next := tb.componentID(tb.last(), "setup:next:")
	tb.click(tAdmin, next)
	r := tb.last()
	if r.Kind != "update" || !strings.Contains(r.text(), "Page 2/") || !strings.Contains(r.text(), "MOD_SUPPORT") {
		t.Fatalf("next page: %s %q", r.Kind, r.text())
	}
	back := tb.componentID(r, "setup:back:")
	tb.click(tAdmin, back)
	if !strings.Contains(tb.last().text(), "Page 1/") {
		t.Error("back did not return to page 1")
	}

	// A role list select replaces the whole list.
	tb.click(tAdmin, "setup:rlist:"+tAdmin+":4:9999999999:JAIL_PROTECTED_ROLES", tRoleKeep, tRoleVIP)
	if got := tb.cfg.List("JAIL_PROTECTED_ROLES"); strings.Join(got, ",") != tRoleKeep+","+tRoleVIP {
		t.Errorf("JAIL_PROTECTED_ROLES = %v", got)
	}
}

func TestSetupRejectsOtherUserAndExpired(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tAdmin, "setup")
	id := tb.componentID(tb.last(), "setup:ch:")

	tb.click(tMod, id, tChGeneral)
	tb.expect(true, "isn't yours")
	if tb.cfg.ID("MOD_LOG") != tChModLog {
		t.Error("another user changed a setting")
	}

	tb.click(tAdmin, "setup:ch:"+tAdmin+":0:1:MOD_LOG", tChGeneral)
	r := tb.replies()[len(tb.replies())-1]
	if r.Kind != "update" || len(r.Components) == 0 {
		t.Fatalf("expired click: %+v", r)
	}
	for _, c := range r.Components {
		if !c.Disabled {
			t.Errorf("component %s still enabled after expiry", c.CustomID)
		}
	}
	if tb.cfg.ID("MOD_LOG") != tChModLog {
		t.Error("expired click changed a setting")
	}
}

func TestSetupModalInt(t *testing.T) {
	tb := newTestBot(t)
	tb.click(tAdmin, "setup:edit:"+tAdmin+":3:9999999999:WARN_ARREST_DURATION")
	if r := tb.replies(); len(r) == 0 || r[len(r)-1].Kind != "modal" {
		t.Fatalf("edit did not open a modal: %+v", r)
	}

	tb.submit(tAdmin, "setupm:"+tAdmin+":3:WARN_ARREST_DURATION", map[string]string{"value": "abc"})
	tb.expect(true, "isn't a whole number")
	if tb.cfg.Int("WARN_ARREST_DURATION") != 60 {
		t.Error("bad value saved")
	}

	tb.submit(tAdmin, "setupm:"+tAdmin+":3:WARN_ARREST_DURATION", map[string]string{"value": " 12 "})
	if tb.cfg.Int("WARN_ARREST_DURATION") != 12 {
		t.Errorf("WARN_ARREST_DURATION = %d", tb.cfg.Int("WARN_ARREST_DURATION"))
	}
}

func TestSetupPostModSupport(t *testing.T) {
	tb := newTestBot(t)
	tb.click(tAdmin, "setup:post:"+tAdmin+":1:9999999999")
	tb.expect(true, "MOD_SUPPORT is not set")

	tb.config(map[string]string{"MOD_SUPPORT": tChSupport})
	tb.click(tAdmin, "setup:post:"+tAdmin+":1:9999999999")
	tb.expect(true, "Mod support embed posted")
	if !contains(tb.sent(tChSupport), "Raise an issue") {
		t.Error("embed not posted")
	}
	tb.expectCall(1, "PUT", "/channels/"+tChSupport+"/messages/8000/reactions/.*/@me")
	if got := tb.cfg.Raw(config.ModSupportMsgKey); got != "8000" {
		t.Errorf("%s = %q", config.ModSupportMsgKey, got)
	}

	tb.api.fail("POST", "/channels/"+tChSupport+"/messages")
	tb.click(tAdmin, "setup:post:"+tAdmin+":1:9999999999")
	tb.expect(true, "don't have permission to post")
}

func TestSetupHidesMessageContentSettings(t *testing.T) {
	names := func(tb *testBot) string {
		var all []string
		for _, page := range tb.setupPages() {
			for _, k := range page {
				all = append(all, k.Key)
			}
		}
		return strings.Join(all, ",")
	}
	off, on := newTestBot(t), newTestBot(t, withMessageContent)
	for key := range contentOnlySettings {
		if strings.Contains(","+names(off)+",", ","+key+",") {
			t.Errorf("%s shown without the option", key)
		}
		if !strings.Contains(","+names(on)+",", ","+key+",") {
			t.Errorf("%s missing with the option", key)
		}
	}
	// An old button for a hidden setting can't change it.
	off.submit(tAdmin, "setupm:"+tAdmin+":0:CURATED_THRESHOLD", map[string]string{"value": "3"})
	off.expect(true, "belongs to a feature that is off")
	if off.cfg.Int("CURATED_THRESHOLD") != 7 {
		t.Error("hidden setting changed")
	}
}

func TestSetupShowsSource(t *testing.T) {
	t.Setenv("TRIGGER_IMPORT_MAX_MB", "")
	t.Setenv("WARN_KICK_THRESHOLD", "8")
	tb := newTestBot(t)
	ids := make([]string, 25)
	for i := range ids {
		ids[i] = fmt.Sprint(1000000000000000000 + i)
	}
	tb.config(map[string]string{"JAIL_PROTECTED_ROLES": strings.Join(ids, ",")})

	fields := map[string]string{}
	v := setupView{b: tb.Bot, guildID: tGuild, invoker: tAdmin}
	for v.page = range tb.setupPages() {
		for _, f := range v.embed().Fields {
			fields[f.Name] = f.Value
			if len(f.Value) > 1024 {
				t.Errorf("%s is %d characters, over the 1024 limit", f.Name, len(f.Value))
			}
		}
	}
	for key, want := range map[string]string{
		"MOD_LOG":               "💾 **Saved**\n",
		"JAIL_PROTECTED_ROLES":  "💾 **Saved**\n",
		"WARN_KICK_THRESHOLD":   "📄 **From .env**\n",
		"TRIGGER_IMPORT_MAX_MB": "⚙️ **Built-in default** *(not saved)*\n",
	} {
		if !strings.HasPrefix(fields[key], want) {
			t.Errorf("%s = %q, want it to start with %q", key, fields[key], want)
		}
	}
}
