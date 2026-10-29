package db

import "time"

// MemberNick is the server nickname of a member that the bot saw ("" is
// no nickname, or not a member).
type MemberNick struct {
	GuildID, UserID, Nick string
	Seen                  time.Time
}

// SaveMemberNicks stores changed nicknames and deletes the ones last seen
// before expire, in one transaction.
func (d *DB) SaveMemberNicks(nicks []MemberNick, expire time.Time) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, n := range nicks {
		if _, err := tx.Exec(`INSERT INTO member_nicks (guild_id, user_id, nick, seen_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (guild_id, user_id) DO UPDATE SET nick = excluded.nick, seen_at = excluded.seen_at`,
			n.GuildID, n.UserID, n.Nick, n.Seen.Unix()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM member_nicks WHERE seen_at < ?`, expire.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// MemberNicks returns the newest nicknames seen since since, newest first.
func (d *DB) MemberNicks(since time.Time, limit int) ([]MemberNick, error) {
	rows, err := d.sql.Query(`SELECT guild_id, user_id, nick, seen_at FROM member_nicks
		WHERE seen_at >= ? ORDER BY seen_at DESC LIMIT ?`, since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []MemberNick
	for rows.Next() {
		var n MemberNick
		var seen int64
		if err := rows.Scan(&n.GuildID, &n.UserID, &n.Nick, &seen); err != nil {
			return nil, err
		}
		n.Seen = time.Unix(seen, 0)
		out = append(out, n)
	}
	return out, rows.Err()
}
