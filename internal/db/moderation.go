package db

import (
	"database/sql"
	"encoding/json"
	"strconv"
)

// Warning is one row of the warnings table.
type Warning struct {
	ID        int64
	Reason    string
	Timestamp string
}

// AddWarning records a warning and returns the member's new total.
func (d *DB) AddWarning(userID, guildID, reason string) (int, error) {
	if _, err := d.sql.Exec(
		`INSERT INTO warnings (user_id, guild_id, reason) VALUES (?, ?, ?)`,
		userID, guildID, reason,
	); err != nil {
		return 0, err
	}
	var n int
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM warnings WHERE user_id=? AND guild_id=?`, userID, guildID,
	).Scan(&n)
	return n, err
}

// Warnings returns a member's warnings, oldest first.
func (d *DB) Warnings(userID, guildID string) ([]Warning, error) {
	rows, err := d.sql.Query(
		`SELECT id, COALESCE(reason, ''), COALESCE(timestamp, '') FROM warnings
		 WHERE user_id=? AND guild_id=? ORDER BY timestamp ASC`,
		userID, guildID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Warning
	for rows.Next() {
		var w Warning
		if err := rows.Scan(&w.ID, &w.Reason, &w.Timestamp); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ClearWarnings deletes every warning for a member.
func (d *DB) ClearWarnings(userID, guildID string) error {
	_, err := d.sql.Exec(`DELETE FROM warnings WHERE user_id=? AND guild_id=?`, userID, guildID)
	return err
}

// RemoveWarning deletes one warning by ID and reports whether it existed.
func (d *DB) RemoveWarning(warningID int64, guildID string) (bool, error) {
	res, err := d.sql.Exec(`DELETE FROM warnings WHERE id=? AND guild_id=?`, warningID, guildID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SentenceTimeFormat is how sentence timestamps are stored (UTC).
const SentenceTimeFormat = "2006-01-02 15:04:05"

// CreateSentence records a new active jail sentence and returns its ID.
// releaseAt is "" for an indefinite sentence.
func (d *DB) CreateSentence(guildID, userID, username, jailedBy, reason, releaseAt string, removedRoles []string) (int64, error) {
	ids := make([]int64, 0, len(removedRoles))
	for _, r := range removedRoles {
		if id, err := strconv.ParseInt(r, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	// Stored as a JSON array of integers, matching the Python bot.
	roles, err := json.Marshal(ids)
	if err != nil {
		return 0, err
	}
	res, err := d.sql.Exec(
		`INSERT INTO sentences (guild_id, user_id, username, reason, jailed_by, release_at, removed_roles)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		guildID, userID, username, nullIfEmpty(reason), jailedBy, nullIfEmpty(releaseAt), string(roles),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// HasActiveSentence reports whether a member is currently jailed.
func (d *DB) HasActiveSentence(guildID, userID string) (bool, error) {
	var id int64
	err := d.sql.QueryRow(
		`SELECT id FROM sentences WHERE guild_id=? AND user_id=? AND active=1 LIMIT 1`,
		guildID, userID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
