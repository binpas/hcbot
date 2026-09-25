package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestOpenMigratesLegacyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// A database from before trigger_values and sentences.removed_roles.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE triggers (name TEXT PRIMARY KEY, value TEXT)`,
		`INSERT INTO triggers VALUES ('hi', 'hello')`,
		`CREATE TABLE sentences (id INTEGER PRIMARY KEY AUTOINCREMENT, guild_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL, username TEXT NOT NULL, reason TEXT, jailed_by INTEGER,
			jailed_at TEXT, release_at TEXT, released_at TEXT, active INTEGER NOT NULL DEFAULT 1)`,
	} {
		if _, err := legacy.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = d.Close() }()

	var value string
	if err := d.sql.QueryRow(`SELECT value FROM trigger_values WHERE name='hi'`).Scan(&value); err != nil || value != "hello" {
		t.Fatalf("migrated trigger = %q, %v", value, err)
	}
	if _, err := d.sql.Exec(`UPDATE sentences SET removed_roles='[]'`); err != nil {
		t.Fatalf("removed_roles column missing: %v", err)
	}

	// Opening again must be a no-op.
	if err := d.init(); err != nil {
		t.Fatalf("second init: %v", err)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	if err := d.SetConfig("MOD_ROLE", "1"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetConfig("MOD_ROLE", "2"); err != nil {
		t.Fatal(err)
	}
	got, err := d.LoadConfig()
	if err != nil || got["MOD_ROLE"] != "2" || len(got) != 1 {
		t.Fatalf("LoadConfig = %v, %v", got, err)
	}
}

func TestScheduledActions(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	now := time.Now()
	if err := d.ScheduleAction("1", "2", "alice", ActionUnban, "", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := d.ScheduleAction("1", "3", "bob", ActionRemoveRole, "99", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	due, err := d.DueActions()
	if err != nil || len(due) != 1 || due[0].Username != "alice" || due[0].GuildID != "1" || due[0].RoleID != "" {
		t.Fatalf("DueActions = %+v, %v", due, err)
	}
	if err := d.DeleteAction(due[0].ID); err != nil {
		t.Fatal(err)
	}
	if due, _ := d.DueActions(); len(due) != 0 {
		t.Fatalf("after delete: %+v", due)
	}
}

func TestSentences(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	past := time.Now().UTC().Add(-time.Minute).Format(SentenceTimeFormat)
	a, err := d.CreateSentence("1", "2", "alice", "9", "spam", past, []string{"100", "200"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.CreateSentence("1", "3", "bob", "9", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	s, err := d.ActiveSentence("1", "2")
	if err != nil || s == nil || s.ID != a || s.Reason != "spam" || !s.Active ||
		len(s.RemovedRoles) != 2 || s.RemovedRoles[1] != "200" || s.GuildID != "1" {
		t.Fatalf("ActiveSentence = %+v, %v", s, err)
	}
	if list, err := d.ActiveSentences("1"); err != nil || len(list) != 2 {
		t.Fatalf("ActiveSentences = %v, %v", list, err)
	}
	if exp, err := d.ExpiredSentences(); err != nil || len(exp) != 1 || exp[0].ID != a {
		t.Fatalf("ExpiredSentences = %v, %v", exp, err)
	}

	empty, reason := "", "new reason"
	if err := d.AmendSentence(a, &empty, &reason); err != nil {
		t.Fatal(err)
	}
	if s, _ := d.SentenceByID(a); s.ReleaseAt != "" || s.Reason != "new reason" {
		t.Fatalf("after amend: %+v", s)
	}
	if exp, _ := d.ExpiredSentences(); len(exp) != 0 {
		t.Fatalf("indefinite sentence expired: %v", exp)
	}

	if err := d.ReleaseSentence(b); err != nil {
		t.Fatal(err)
	}
	if s, _ := d.ActiveSentence("1", "3"); s != nil {
		t.Fatalf("released sentence still active: %+v", s)
	}
	if s, _ := d.SentenceByID(999); s != nil {
		t.Fatal("missing sentence found")
	}
	if ok, err := d.IsTicketChannel("5"); ok || err != nil {
		t.Fatalf("IsTicketChannel = %v, %v", ok, err)
	}
}

func TestTicketNumbers(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	if n, err := d.NextTicketNumber("1", "2"); err != nil || n != 1 {
		t.Fatalf("first = %d, %v", n, err)
	}
	if _, err := d.CreateTicket("1", "50", "2", "alice", 1); err != nil {
		t.Fatal(err)
	}
	// A legacy archived ticket with a higher number is counted too.
	if _, err := d.sql.Exec(`INSERT INTO ticket_archive (guild_id, user_id, username, channel_name, number, transcript)
		VALUES (1, 2, 'alice', 'x', 5, '')`); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.NextTicketNumber("1", "2"); n != 6 {
		t.Errorf("next = %d, want 6", n)
	}
	if n, _ := d.NextTicketNumber("1", "3"); n != 1 {
		t.Errorf("other user = %d, want 1", n)
	}
	if ref, _ := d.FindTicket("1", "alice", 5); ref == nil || ref.UserID != "2" {
		t.Errorf("FindTicket legacy = %+v", ref)
	}
	h, err := d.TicketHistory("1", "2")
	if err != nil || len(h) != 2 || h[0].Number != 5 || h[0].ChannelID != "" || h[1].ChannelID != "50" {
		t.Errorf("history = %+v, %v", h, err)
	}
}

func TestBackupAndTemplates(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if err := d.SaveTemplate("1", "Staff", "general", `[{"kind":"role","id":5,"name":"x","allow":1024,"deny":0}]`, "9"); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveTemplate("1", "STAFF", "other", `[]`, ""); err != nil { // replaces
		t.Fatal(err)
	}
	if list, _ := d.Templates("1"); len(list) != 1 || list[0].Name != "staff" || list[0].Source != "other" || list[0].CreatedBy != "" {
		t.Fatalf("templates = %+v", list)
	}

	dest := filepath.Join(dir, "copy.db")
	if err := d.Backup(dest); err != nil {
		t.Fatal(err)
	}
	c, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if tpl, _ := c.Template("1", "Staff"); tpl == nil || tpl.Source != "other" {
		t.Errorf("backup copy = %+v", tpl)
	}
	if err := d.Backup(dest); err == nil {
		t.Error("backup overwrote an existing file")
	}
}
