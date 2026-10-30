package db

import (
	"database/sql"
	"errors"
	"time"
)

// Reminder is one row of the reminders table: AuthorID asked the bot to
// remind TargetID (often the same member) of Text at At, in reply to the
// message MessageID.
type Reminder struct {
	ID        int64
	GuildID   string
	ChannelID string
	MessageID string
	AuthorID  string
	TargetID  string
	Text      string
	At        time.Time
}

const reminderCols = `id, guild_id, channel_id, message_id, author_id, target_id, text, remind_at`

func scanReminders(rows *sql.Rows) ([]Reminder, error) {
	defer func() { _ = rows.Close() }()
	var out []Reminder
	for rows.Next() {
		var r Reminder
		var at int64
		if err := rows.Scan(&r.ID, &r.GuildID, &r.ChannelID, &r.MessageID, &r.AuthorID, &r.TargetID, &r.Text, &at); err != nil {
			return nil, err
		}
		r.At = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddReminder stores a reminder and returns its ID.
func (d *DB) AddReminder(r Reminder) (int64, error) {
	res, err := d.sql.Exec(
		`INSERT INTO reminders (guild_id, channel_id, message_id, author_id, target_id, text, remind_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.GuildID, r.ChannelID, r.MessageID, r.AuthorID, r.TargetID, r.Text, r.At.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DueReminders returns the reminders whose time is now or before, oldest
// first.
func (d *DB) DueReminders(now time.Time) ([]Reminder, error) {
	rows, err := d.sql.Query(`SELECT `+reminderCols+` FROM reminders WHERE remind_at <= ? ORDER BY remind_at, id`, now.Unix())
	if err != nil {
		return nil, err
	}
	return scanReminders(rows)
}

// Reminders returns the reminders of a server, soonest first: all of them
// when userID is "", else the ones that userID set or will get.
func (d *DB) Reminders(guildID, userID string) ([]Reminder, error) {
	rows, err := d.sql.Query(`SELECT `+reminderCols+` FROM reminders
		WHERE guild_id = ? AND (? = '' OR author_id = ? OR target_id = ?)
		ORDER BY remind_at, id`, guildID, userID, userID, userID)
	if err != nil {
		return nil, err
	}
	return scanReminders(rows)
}

// GetReminder returns one reminder, or nil when there is none.
func (d *DB) GetReminder(id int64) (*Reminder, error) {
	rows, err := d.sql.Query(`SELECT `+reminderCols+` FROM reminders WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	out, err := scanReminders(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// CountReminders returns how many reminders a member has set.
func (d *DB) CountReminders(authorID string) (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM reminders WHERE author_id = ?`, authorID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// DeleteReminder removes a reminder.
func (d *DB) DeleteReminder(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM reminders WHERE id = ?`, id)
	return err
}
