package db

import (
	"database/sql"
	"errors"
	"strings"
)

// SetComicCharacter stores the comic character a member chose.
func (d *DB) SetComicCharacter(guildID, userID, character string) error {
	_, err := d.sql.Exec(`INSERT INTO comic_characters (guild_id, user_id, character) VALUES (?, ?, ?)
		ON CONFLICT (guild_id, user_id) DO UPDATE SET character = excluded.character`, guildID, userID, character)
	return err
}

// ComicCharacter returns the character a member chose, or "".
func (d *DB) ComicCharacter(guildID, userID string) (string, error) {
	var c string
	err := d.sql.QueryRow(`SELECT character FROM comic_characters WHERE guild_id = ? AND user_id = ?`,
		guildID, userID).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return c, err
}

// ComicCharacters returns the chosen characters of some members, by user ID.
func (d *DB) ComicCharacters(guildID string, userIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(userIDs) == 0 {
		return out, nil
	}
	args := []any{guildID}
	for _, id := range userIDs {
		args = append(args, id)
	}
	rows, err := d.sql.Query(`SELECT user_id, character FROM comic_characters WHERE guild_id = ? AND user_id IN (?`+
		strings.Repeat(", ?", len(userIDs)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var u, c string
		if err := rows.Scan(&u, &c); err != nil {
			return nil, err
		}
		out[u] = c
	}
	return out, rows.Err()
}

// DeleteComicCharacter removes a member's choice.
func (d *DB) DeleteComicCharacter(guildID, userID string) error {
	_, err := d.sql.Exec(`DELETE FROM comic_characters WHERE guild_id = ? AND user_id = ?`, guildID, userID)
	return err
}

// ComicMember is a member with a comic character.
type ComicMember struct {
	UserID, Character string
	Locked            bool
}

// ComicMembers returns the members with a character in a server.
func (d *DB) ComicMembers(guildID string) ([]ComicMember, error) {
	rows, err := d.sql.Query(`SELECT user_id, character, locked FROM comic_characters WHERE guild_id = ? ORDER BY character, user_id`, guildID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ComicMember
	for rows.Next() {
		var m ComicMember
		if err := rows.Scan(&m.UserID, &m.Character, &m.Locked); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetComicMember stores a member's character and lock.
func (d *DB) SetComicMember(guildID, userID, character string, locked bool) error {
	_, err := d.sql.Exec(`INSERT INTO comic_characters (guild_id, user_id, character, locked) VALUES (?, ?, ?, ?)
		ON CONFLICT (guild_id, user_id) DO UPDATE SET character = excluded.character, locked = excluded.locked`,
		guildID, userID, character, locked)
	return err
}

// ComicLocked reports whether a mod locked a member's character.
func (d *DB) ComicLocked(guildID, userID string) (bool, error) {
	var locked bool
	err := d.sql.QueryRow(`SELECT locked FROM comic_characters WHERE guild_id = ? AND user_id = ?`, guildID, userID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return locked, err
}

// ComicHidden returns the hidden characters and backgrounds.
func (d *DB) ComicHidden() (chars, backs []string, err error) {
	rows, err := d.sql.Query(`SELECT kind, name FROM comic_hidden`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return nil, nil, err
		}
		if kind == "character" {
			chars = append(chars, name)
		} else {
			backs = append(backs, name)
		}
	}
	return chars, backs, rows.Err()
}

// SetComicHidden hides or shows a character or background.
func (d *DB) SetComicHidden(kind, name string, hidden bool) error {
	var err error
	if hidden {
		_, err = d.sql.Exec(`INSERT OR IGNORE INTO comic_hidden (kind, name) VALUES (?, ?)`, kind, name)
	} else {
		_, err = d.sql.Exec(`DELETE FROM comic_hidden WHERE kind = ? AND name = ?`, kind, name)
	}
	return err
}
