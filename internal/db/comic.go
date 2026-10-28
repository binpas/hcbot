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
