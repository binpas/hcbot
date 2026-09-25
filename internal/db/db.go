// Package db wraps the bot's SQLite database. The schema matches the
// Python bot's health_bot.db, so either bot can run against the same file.
package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// DB is the bot's database handle.
type DB struct {
	sql  *sql.DB
	Path string
}

// Open opens (creating if needed) the database at path and brings its
// schema up to date.
func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// One connection serialises writes the same way the Python bot's
	// per-call connections did, and avoids SQLITE_BUSY between our own calls.
	conn.SetMaxOpenConns(1)
	d := &DB{sql: conn, Path: path}
	if err := d.init(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return d, nil
}

// Close closes the database.
func (d *DB) Close() error {
	return d.sql.Close()
}

var schema = []string{
	// One row per (trigger name, response value) pair; a trigger with
	// several values picks one at random.
	`CREATE TABLE IF NOT EXISTS trigger_values (
		name  TEXT NOT NULL,
		value TEXT NOT NULL,
		PRIMARY KEY (name, value)
	)`,
	`CREATE TABLE IF NOT EXISTS warnings (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id   INTEGER NOT NULL,
		guild_id  INTEGER NOT NULL,
		reason    TEXT,
		timestamp TEXT DEFAULT (datetime('now'))
	)`,
	`CREATE TABLE IF NOT EXISTS curated_messages (
		message_id INTEGER PRIMARY KEY
	)`,
	// Server configuration set via /setup
	`CREATE TABLE IF NOT EXISTS config (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS tickets (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id    INTEGER NOT NULL,
		channel_id  INTEGER NOT NULL UNIQUE,
		user_id     INTEGER NOT NULL,
		username    TEXT    NOT NULL,
		number      INTEGER NOT NULL,
		status      TEXT    NOT NULL DEFAULT 'open',
		created_at  TEXT    DEFAULT (datetime('now')),
		closed_at   TEXT
	)`,
	// Legacy archived-ticket transcripts. No longer written to; kept so
	// old data isn't lost and ticket numbering still sees it.
	`CREATE TABLE IF NOT EXISTS ticket_archive (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		ticket_id     INTEGER,
		guild_id      INTEGER NOT NULL,
		user_id       INTEGER NOT NULL,
		username      TEXT    NOT NULL,
		channel_name  TEXT    NOT NULL,
		number        INTEGER,
		opened_at     TEXT,
		closed_at     TEXT,
		archived_at   TEXT    DEFAULT (datetime('now')),
		message_count INTEGER DEFAULT 0,
		transcript    TEXT    NOT NULL
	)`,
	// Mod notes on a ticket, keyed by (guild, user, number).
	`CREATE TABLE IF NOT EXISTS ticket_notes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id   INTEGER NOT NULL,
		user_id    INTEGER NOT NULL,
		username   TEXT    NOT NULL,
		number     INTEGER NOT NULL,
		author_id  INTEGER,
		note       TEXT    NOT NULL,
		created_at TEXT    DEFAULT (datetime('now'))
	)`,
	`CREATE TABLE IF NOT EXISTS sentences (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id      INTEGER NOT NULL,
		user_id       INTEGER NOT NULL,
		username      TEXT    NOT NULL,
		reason        TEXT,
		jailed_by     INTEGER,
		jailed_at     TEXT    DEFAULT (datetime('now')),
		release_at    TEXT,
		released_at   TEXT,
		active        INTEGER NOT NULL DEFAULT 1,
		removed_roles TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS reaction_roles (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id   INTEGER NOT NULL,
		name       TEXT    NOT NULL,
		channel_id INTEGER,
		message_id INTEGER,
		text       TEXT    NOT NULL DEFAULT '',
		created_at TEXT    DEFAULT (datetime('now')),
		UNIQUE (guild_id, name)
	)`,
	`CREATE TABLE IF NOT EXISTS reaction_role_pairs (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		rr_id    INTEGER NOT NULL,
		emoji    TEXT    NOT NULL,
		role_id  INTEGER NOT NULL,
		position INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY (rr_id) REFERENCES reaction_roles(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS perm_templates (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id   INTEGER NOT NULL,
		name       TEXT    NOT NULL,
		source     TEXT    NOT NULL,
		data       TEXT    NOT NULL,
		created_by INTEGER,
		created_at TEXT    DEFAULT (datetime('now')),
		UNIQUE (guild_id, name)
	)`,
	`CREATE TABLE IF NOT EXISTS autoreacts (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id   INTEGER NOT NULL,
		emote      TEXT    NOT NULL,
		phrase     TEXT    NOT NULL,
		created_by INTEGER,
		created_at TEXT    DEFAULT (datetime('now')),
		UNIQUE (guild_id, phrase)
	)`,
	// Timed follow-ups (tempban unban, MOTD role removal) that must survive
	// a restart. Go bot only; the Python bot ignores this table.
	`CREATE TABLE IF NOT EXISTS scheduled_actions (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id   INTEGER NOT NULL,
		user_id    INTEGER NOT NULL,
		username   TEXT    NOT NULL,
		action     TEXT    NOT NULL,
		role_id    INTEGER,
		run_at     TEXT    NOT NULL,
		created_at TEXT    DEFAULT (datetime('now'))
	)`,
	// Data a button needs that is too big for its custom ID (a pending
	// import, for example). The custom ID holds the row ID. Go bot only.
	`CREATE TABLE IF NOT EXISTS pending_actions (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		kind       TEXT    NOT NULL,
		user_id    INTEGER NOT NULL,
		payload    TEXT    NOT NULL,
		expires_at TEXT    NOT NULL
	)`,
}

// Columns added after a table was first created. Each ALTER fails harmlessly
// when the column already exists, which keeps old databases working.
var migrations = []string{
	// JSON array of role IDs stripped at arrest time, restored on release.
	`ALTER TABLE sentences ADD COLUMN removed_roles TEXT`,
}

func (d *DB) init() error {
	for _, stmt := range schema {
		if _, err := d.sql.Exec(stmt); err != nil {
			return err
		}
	}
	for _, stmt := range migrations {
		_, _ = d.sql.Exec(stmt) // column already exists
	}
	return d.migrateLegacyTriggers()
}

// migrateLegacyTriggers moves the old single-value `triggers` table into
// trigger_values, once.
func (d *DB) migrateLegacyTriggers() error {
	var name string
	err := d.sql.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='triggers'`,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO trigger_values (name, value) SELECT name, value FROM triggers`,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE triggers`); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadConfig returns every row of the config table.
func (d *DB) LoadConfig() (map[string]string, error) {
	rows, err := d.sql.Query(`SELECT key, value FROM config`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ImportConfig stores values in one transaction. With replace, it first
// removes every row, so the table ends up holding exactly values.
func (d *DB) ImportConfig(values map[string]string, replace bool) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if replace {
		if _, err := tx.Exec(`DELETE FROM config`); err != nil {
			return err
		}
	}
	for k, v := range values {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetConfig stores one config value.
func (d *DB) SetConfig(key, value string) error {
	_, err := d.sql.Exec(
		`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`, key, value,
	)
	return err
}
