package db

import (
	"database/sql"
	"errors"
)

// UserNote is one row of the user_notes table. Times are UTC in
// SentenceTimeFormat. The message fields are "" for a note made with
// /note add.
type UserNote struct {
	ID          int64
	UserID      string
	AuthorID    string
	Note        string
	CreatedAt   string
	EditedBy    string
	EditedAt    string
	ChannelID   string
	MessageID   string
	MessageText string
	MessageTime string
}

// NoteSource is the message a note was made from.
type NoteSource struct {
	ChannelID string
	MessageID string
	Text      string
	Time      string
}

const userNoteCols = `id, CAST(user_id AS TEXT), CAST(author_id AS TEXT), note, COALESCE(created_at, ''),
	COALESCE(CAST(edited_by AS TEXT), ''), COALESCE(edited_at, ''),
	COALESCE(CAST(channel_id AS TEXT), ''), COALESCE(CAST(message_id AS TEXT), ''),
	COALESCE(message_text, ''), COALESCE(message_time, '')`

func scanUserNote(s interface{ Scan(...any) error }) (UserNote, error) {
	var n UserNote
	err := s.Scan(&n.ID, &n.UserID, &n.AuthorID, &n.Note, &n.CreatedAt, &n.EditedBy, &n.EditedAt,
		&n.ChannelID, &n.MessageID, &n.MessageText, &n.MessageTime)
	return n, err
}

// AddUserNote records a note on a member and returns its ID. src is nil for
// a note that was not made from a message.
func (d *DB) AddUserNote(guildID, userID, authorID, note string, src *NoteSource) (int64, error) {
	var ch, msg, text, at any
	if src != nil {
		ch, msg, text, at = nullIfEmpty(src.ChannelID), nullIfEmpty(src.MessageID), src.Text, nullIfEmpty(src.Time)
	}
	res, err := d.sql.Exec(
		`INSERT INTO user_notes (guild_id, user_id, author_id, note, channel_id, message_id, message_text, message_time)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		guildID, userID, authorID, note, ch, msg, text, at,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UserNotes returns a member's notes, oldest first.
func (d *DB) UserNotes(guildID, userID string) ([]UserNote, error) {
	rows, err := d.sql.Query(`SELECT `+userNoteCols+` FROM user_notes
		WHERE guild_id=? AND user_id=? ORDER BY id ASC`, guildID, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UserNote
	for rows.Next() {
		n, err := scanUserNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UserNote returns one note, and false when it does not exist.
func (d *DB) UserNote(guildID string, id int64) (UserNote, bool, error) {
	n, err := scanUserNote(d.sql.QueryRow(`SELECT `+userNoteCols+` FROM user_notes
		WHERE guild_id=? AND id=?`, guildID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return UserNote{}, false, nil
	}
	return n, err == nil, err
}

// EditUserNote replaces a note's text and records who edited it and when.
// It reports whether the note existed.
func (d *DB) EditUserNote(guildID string, id int64, editorID, note string) (bool, error) {
	res, err := d.sql.Exec(`UPDATE user_notes SET note=?, edited_by=?, edited_at=datetime('now')
		WHERE guild_id=? AND id=?`, note, editorID, guildID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteUserNote deletes one note and reports whether it existed.
func (d *DB) DeleteUserNote(guildID string, id int64) (bool, error) {
	res, err := d.sql.Exec(`DELETE FROM user_notes WHERE guild_id=? AND id=?`, guildID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
