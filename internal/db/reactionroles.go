package db

import "database/sql"

// ReactionRole is one row of the reaction_roles table.
type ReactionRole struct {
	ID        int64
	GuildID   string
	Name      string
	ChannelID string // "" when not set
	MessageID string // "" when not posted
	Text      string
	PairCount int // filled by ReactionRoles only
}

// RolePair is one emote → role pair of a reaction role.
type RolePair struct {
	Emoji  string `json:"emoji"`
	RoleID string `json:"role"`
}

const rrCols = `id, guild_id, name, COALESCE(CAST(channel_id AS TEXT), ''), COALESCE(CAST(message_id AS TEXT), ''), text`

func (d *DB) oneRR(query string, args ...any) (*ReactionRole, error) {
	var r ReactionRole
	err := d.sql.QueryRow(query, args...).Scan(&r.ID, &r.GuildID, &r.Name, &r.ChannelID, &r.MessageID, &r.Text)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ReactionRoleByName finds a reaction role by name, ignoring case.
func (d *DB) ReactionRoleByName(guildID, name string) (*ReactionRole, error) {
	return d.oneRR(`SELECT `+rrCols+` FROM reaction_roles WHERE guild_id=? AND name=? COLLATE NOCASE`, guildID, name)
}

// ReactionRoleByMessage finds the reaction role posted as a message.
func (d *DB) ReactionRoleByMessage(messageID string) (*ReactionRole, error) {
	return d.oneRR(`SELECT `+rrCols+` FROM reaction_roles WHERE message_id=?`, messageID)
}

// SaveReactionRole creates (id 0) or updates a reaction role with its
// pairs in one transaction, and returns its ID.
func (d *DB) SaveReactionRole(id int64, guildID, name, channelID, text string, pairs []RolePair) (int64, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if id == 0 {
		res, err := tx.Exec(`INSERT INTO reaction_roles (guild_id, name, channel_id, text) VALUES (?, ?, ?, ?)`,
			guildID, name, nullIfEmpty(channelID), text)
		if err != nil {
			return 0, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	} else if _, err := tx.Exec(`UPDATE reaction_roles SET name=?, channel_id=?, text=? WHERE id=?`,
		name, nullIfEmpty(channelID), text, id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM reaction_role_pairs WHERE rr_id=?`, id); err != nil {
		return 0, err
	}
	for i, p := range pairs {
		if _, err := tx.Exec(`INSERT INTO reaction_role_pairs (rr_id, emoji, role_id, position) VALUES (?, ?, ?, ?)`,
			id, p.Emoji, p.RoleID, i); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// SetReactionRoleMessage records where a reaction role is posted
// (messageID "" means not posted).
func (d *DB) SetReactionRoleMessage(id int64, channelID, messageID string) error {
	_, err := d.sql.Exec(`UPDATE reaction_roles SET channel_id=?, message_id=? WHERE id=?`,
		nullIfEmpty(channelID), nullIfEmpty(messageID), id)
	return err
}

// RolePairs returns a reaction role's pairs in order.
func (d *DB) RolePairs(id int64) ([]RolePair, error) {
	rows, err := d.sql.Query(`SELECT emoji, CAST(role_id AS TEXT) FROM reaction_role_pairs WHERE rr_id=? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RolePair
	for rows.Next() {
		var p RolePair
		if err := rows.Scan(&p.Emoji, &p.RoleID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReactionRoles lists a guild's reaction roles by name, with pair counts.
func (d *DB) ReactionRoles(guildID string) ([]ReactionRole, error) {
	rows, err := d.sql.Query(`SELECT `+rrCols+`,
		(SELECT COUNT(*) FROM reaction_role_pairs p WHERE p.rr_id=reaction_roles.id)
		FROM reaction_roles WHERE guild_id=? ORDER BY name`, guildID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ReactionRole
	for rows.Next() {
		var r ReactionRole
		if err := rows.Scan(&r.ID, &r.GuildID, &r.Name, &r.ChannelID, &r.MessageID, &r.Text, &r.PairCount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteReactionRole removes a reaction role and its pairs.
func (d *DB) DeleteReactionRole(id int64) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM reaction_role_pairs WHERE rr_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM reaction_roles WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimCurated records a message as curated. It returns false when the
// message was curated before, so only one caller posts it.
func (d *DB) ClaimCurated(messageID string) (bool, error) {
	res, err := d.sql.Exec(`INSERT OR IGNORE INTO curated_messages (message_id) VALUES (?)`, messageID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// IsCurated reports whether a message was curated.
func (d *DB) IsCurated(messageID string) (bool, error) {
	var id int64
	err := d.sql.QueryRow(`SELECT message_id FROM curated_messages WHERE message_id=?`, messageID).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}
