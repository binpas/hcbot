package db

import (
	"database/sql"
	"strings"
)

// Triggers are global (not per guild), as in the Python bot. Names are
// stored lowercase.

// TriggerValues returns every response stored for a trigger.
func (d *DB) TriggerValues(name string) ([]string, error) {
	rows, err := d.sql.Query(`SELECT value FROM trigger_values WHERE name=?`, strings.ToLower(name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// TriggerCount is a trigger name and how many responses it has.
type TriggerCount struct {
	Name  string
	Count int
}

// TriggerNames lists every trigger, alphabetically.
func (d *DB) TriggerNames() ([]TriggerCount, error) {
	rows, err := d.sql.Query(`SELECT name, COUNT(*) FROM trigger_values GROUP BY name ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TriggerCount
	for rows.Next() {
		var t TriggerCount
		if err := rows.Scan(&t.Name, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTrigger replaces a trigger's responses with one value.
func (d *DB) SetTrigger(name, value string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM trigger_values WHERE name=?`, strings.ToLower(name)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO trigger_values (name, value) VALUES (?, ?)`, strings.ToLower(name), value); err != nil {
		return err
	}
	return tx.Commit()
}

// AddTriggerValue adds a response (a duplicate is ignored) and returns the new total.
func (d *DB) AddTriggerValue(name, value string) (int, error) {
	if _, err := d.sql.Exec(`INSERT OR IGNORE INTO trigger_values (name, value) VALUES (?, ?)`,
		strings.ToLower(name), value); err != nil {
		return 0, err
	}
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM trigger_values WHERE name=?`, strings.ToLower(name)).Scan(&n)
	return n, err
}

// RemoveTriggerValue removes one response and reports whether it existed.
func (d *DB) RemoveTriggerValue(name, value string) (bool, error) {
	res, err := d.sql.Exec(`DELETE FROM trigger_values WHERE name=? AND value=?`, strings.ToLower(name), value)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteTrigger removes a trigger and all its responses.
func (d *DB) DeleteTrigger(name string) error {
	_, err := d.sql.Exec(`DELETE FROM trigger_values WHERE name=?`, strings.ToLower(name))
	return err
}

// Autoreact is one row of the autoreacts table.
type Autoreact struct {
	Emote  string
	Phrase string
}

// AutoreactExists reports whether a phrase has an autoreact in the guild.
func (d *DB) AutoreactExists(guildID, phrase string) (bool, error) {
	var id int64
	err := d.sql.QueryRow(`SELECT id FROM autoreacts WHERE guild_id=? AND phrase=?`, guildID, phrase).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// AddAutoreact saves a new autoreact.
func (d *DB) AddAutoreact(guildID, emote, phrase, createdBy string) error {
	_, err := d.sql.Exec(`INSERT INTO autoreacts (guild_id, emote, phrase, created_by) VALUES (?, ?, ?, ?)`,
		guildID, emote, phrase, createdBy)
	return err
}

// SetAutoreactEmote changes an autoreact's emote.
func (d *DB) SetAutoreactEmote(guildID, phrase, emote string) error {
	_, err := d.sql.Exec(`UPDATE autoreacts SET emote=? WHERE guild_id=? AND phrase=?`, emote, guildID, phrase)
	return err
}

// RemoveAutoreact deletes an autoreact and reports whether it existed.
func (d *DB) RemoveAutoreact(guildID, phrase string) (bool, error) {
	res, err := d.sql.Exec(`DELETE FROM autoreacts WHERE guild_id=? AND phrase=?`, guildID, phrase)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Autoreacts lists a guild's autoreacts by phrase.
func (d *DB) Autoreacts(guildID string) ([]Autoreact, error) {
	rows, err := d.sql.Query(`SELECT emote, phrase FROM autoreacts WHERE guild_id=? ORDER BY phrase`, guildID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Autoreact
	for rows.Next() {
		var a Autoreact
		if err := rows.Scan(&a.Emote, &a.Phrase); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
