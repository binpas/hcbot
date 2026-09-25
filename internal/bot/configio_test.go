package bot

import (
	"encoding/json"
	"testing"
)

func TestConfigExportImport(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "db configexport")
	tb.expect(true, "don't have permission")

	tb.run(tAdmin, "db configexport")
	tb.expect(true, "Exported **")
	var exported map[string]string
	if err := json.Unmarshal(tb.lastFiles()["config-export.json"], &exported); err != nil ||
		exported["TICKET_CAT"] != tCatTickets {
		t.Fatalf("export = %v, %v", exported, err)
	}

	// Replace: remove one key, change one, and add one the bot doesn't use.
	delete(exported, "TICKET_CAT")
	exported["WARN_KICK_THRESHOLD"] = "9"
	exported["OLD_KEY"] = "x"
	edited, _ := json.Marshal(exported)
	tb.run(tAdmin, "db configimport", tb.attachmentOpt("file", "c.json", string(edited)), strOpt("mode", "replace"))
	r := tb.expect(true, "**Replace:**", "**Add (2):** `OLD_KEY`, `WARN_KICK_THRESHOLD`",
		"**Remove (1):** `TICKET_CAT`", "does not use these keys, but they will be imported: `OLD_KEY`")
	tb.click(tAdmin, tb.componentID(r, "confirm:yes:cfgimport:"))
	tb.expect(true, "Imported **", "Backup from before the import: `health_bot_")
	if len(tb.backups()) != 1 {
		t.Errorf("backups = %v", tb.backups())
	}
	if tb.cfg.Get("TICKET_CAT") != "" || tb.cfg.Int("WARN_KICK_THRESHOLD") != 9 || tb.cfg.Get("OLD_KEY") != "x" {
		t.Error("settings in memory were not replaced")
	}
	if saved, _ := tb.db.LoadConfig(); len(saved) != len(exported) {
		t.Errorf("saved = %v, want %v", saved, exported)
	}

	// Merge: only the key in the file changes.
	tb.run(tAdmin, "db configimport", tb.attachmentOpt("file", "m.json", `{"TICKET_CAT": "`+tCatTickets+`"}`),
		strOpt("mode", "merge"))
	r = tb.expect(true, "**Merge:**", "**Add (1):** `TICKET_CAT`")
	tb.click(tAdmin, tb.componentID(r, "confirm:yes:cfgimport:"))
	if tb.cfg.Get("TICKET_CAT") != tCatTickets || tb.cfg.Get("OLD_KEY") != "x" {
		t.Error("merge changed the wrong settings")
	}

	// Cancel changes nothing.
	tb.run(tAdmin, "db configimport", tb.attachmentOpt("file", "e.json", `{}`), strOpt("mode", "replace"))
	r = tb.expect(true, "**Remove (")
	tb.click(tAdmin, tb.componentID(r, "confirm:no:cfgimport:"))
	tb.expect(true, "Cancelled")
	if tb.cfg.Get("TICKET_CAT") == "" {
		t.Error("cancel changed the settings")
	}

	tb.run(tAdmin, "db configimport", tb.attachmentOpt("file", "m.json", `{"TICKET_CAT": "`+tCatTickets+`"}`),
		strOpt("mode", "merge"))
	tb.expect(true, "nothing to import")
	tb.run(tAdmin, "db configimport", tb.attachmentOpt("file", "bad.json", `{"WARN_KICK_THRESHOLD": "five", "MOD_LOG": 5}`),
		strOpt("mode", "merge"))
	tb.expect(true, "Nothing was imported", "`MOD_LOG`: the value must be text", "`WARN_KICK_THRESHOLD`: must be a whole number")
	tb.run(tAdmin, "db configimport", tb.attachmentOpt("file", "list.json", `["a"]`), strOpt("mode", "merge"))
	tb.expect(true, "isn't a valid settings file")
	tb.run(tAdmin, "db configimport", tb.attachmentOpt("file", "null.json", `null`), strOpt("mode", "merge"))
	tb.expect(true, "Expected a JSON object")
	if tb.cfg.Int("WARN_KICK_THRESHOLD") != 9 {
		t.Error("a refused file changed the settings")
	}
}
