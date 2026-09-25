package db

import (
	"database/sql"
	"strings"
)

// Backup writes a consistent copy of the database to dest (which must not
// exist). VACUUM INTO is safe while the bot keeps writing.
func (d *DB) Backup(dest string) error {
	_, err := d.sql.Exec(`VACUUM INTO ?`, dest)
	return err
}

// PermTemplate is one row of the perm_templates table. Data is the JSON
// list of overwrites, in the Python bot's format.
type PermTemplate struct {
	Name      string
	Source    string
	Data      string
	CreatedBy string // "" when unknown
	CreatedAt string
}

// SaveTemplate saves (or replaces) a permission template. Names are lowercase.
func (d *DB) SaveTemplate(guildID, name, source, data, createdBy string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	name = strings.ToLower(name)
	if _, err := tx.Exec(`DELETE FROM perm_templates WHERE guild_id=? AND name=?`, guildID, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO perm_templates (guild_id, name, source, data, created_by) VALUES (?, ?, ?, ?, ?)`,
		guildID, name, source, data, nullIfEmpty(createdBy)); err != nil {
		return err
	}
	return tx.Commit()
}

const templateCols = `name, source, data, COALESCE(CAST(created_by AS TEXT), ''), COALESCE(created_at, '')`

// Templates lists a guild's templates by name.
func (d *DB) Templates(guildID string) ([]PermTemplate, error) {
	rows, err := d.sql.Query(`SELECT `+templateCols+` FROM perm_templates WHERE guild_id=? ORDER BY name`, guildID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PermTemplate
	for rows.Next() {
		var t PermTemplate
		if err := rows.Scan(&t.Name, &t.Source, &t.Data, &t.CreatedBy, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Template returns a template by name (any case), or nil.
func (d *DB) Template(guildID, name string) (*PermTemplate, error) {
	var t PermTemplate
	err := d.sql.QueryRow(`SELECT `+templateCols+` FROM perm_templates WHERE guild_id=? AND name=?`,
		guildID, strings.ToLower(name)).Scan(&t.Name, &t.Source, &t.Data, &t.CreatedBy, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
