package db

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
)

// Sentence is one row of the sentences table.
type Sentence struct {
	ID           int64
	GuildID      string
	UserID       string
	Username     string
	Reason       string // "" when none was given
	JailedAt     string // SentenceTimeFormat, UTC
	ReleaseAt    string // "" for an indefinite sentence
	Active       bool
	RemovedRoles []string // roles stripped at arrest, restored on release
}

const sentenceCols = `id, guild_id, user_id, username, COALESCE(reason, ''), COALESCE(jailed_at, ''),
	COALESCE(release_at, ''), active, COALESCE(removed_roles, '')`

type scanner interface{ Scan(...any) error }

func scanSentence(r scanner) (*Sentence, error) {
	var s Sentence
	var roles string
	if err := r.Scan(&s.ID, &s.GuildID, &s.UserID, &s.Username, &s.Reason, &s.JailedAt,
		&s.ReleaseAt, &s.Active, &roles); err != nil {
		return nil, err
	}
	var ids []int64
	if roles != "" {
		_ = json.Unmarshal([]byte(roles), &ids) // a bad value just restores nothing
	}
	for _, id := range ids {
		s.RemovedRoles = append(s.RemovedRoles, strconv.FormatInt(id, 10))
	}
	return &s, nil
}

func (d *DB) oneSentence(query string, args ...any) (*Sentence, error) {
	s, err := scanSentence(d.sql.QueryRow(query, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

func (d *DB) sentences(query string, args ...any) ([]*Sentence, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Sentence
	for rows.Next() {
		s, err := scanSentence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ActiveSentence returns a member's current sentence, or nil.
func (d *DB) ActiveSentence(guildID, userID string) (*Sentence, error) {
	return d.oneSentence(`SELECT `+sentenceCols+` FROM sentences
		WHERE guild_id=? AND user_id=? AND active=1 ORDER BY id DESC LIMIT 1`, guildID, userID)
}

// SentenceByID returns a sentence by ID, or nil.
func (d *DB) SentenceByID(id int64) (*Sentence, error) {
	return d.oneSentence(`SELECT `+sentenceCols+` FROM sentences WHERE id=?`, id)
}

// ActiveSentences lists a guild's current sentences, oldest first.
func (d *DB) ActiveSentences(guildID string) ([]*Sentence, error) {
	return d.sentences(`SELECT `+sentenceCols+` FROM sentences
		WHERE guild_id=? AND active=1 ORDER BY jailed_at`, guildID)
}

// ExpiredSentences lists active sentences whose release time has passed.
func (d *DB) ExpiredSentences() ([]*Sentence, error) {
	return d.sentences(`SELECT ` + sentenceCols + ` FROM sentences
		WHERE active=1 AND release_at IS NOT NULL AND datetime(release_at) <= datetime('now')`)
}

// AmendSentence updates a sentence. A nil field is left as it is; an empty
// releaseAt makes the sentence indefinite.
func (d *DB) AmendSentence(id int64, releaseAt, reason *string) error {
	var sets []string
	var args []any
	if releaseAt != nil {
		sets = append(sets, "release_at=?")
		args = append(args, nullIfEmpty(*releaseAt))
	}
	if reason != nil {
		sets = append(sets, "reason=?")
		args = append(args, nullIfEmpty(*reason))
	}
	if len(sets) == 0 {
		return nil
	}
	_, err := d.sql.Exec(`UPDATE sentences SET `+strings.Join(sets, ", ")+` WHERE id=?`, append(args, id)...)
	return err
}

// ReleaseSentence marks a sentence served.
func (d *DB) ReleaseSentence(id int64) error {
	_, err := d.sql.Exec(`UPDATE sentences SET active=0, released_at=datetime('now') WHERE id=?`, id)
	return err
}
