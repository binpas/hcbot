package db

import (
	"database/sql"
)

// Ticket is one row of the tickets table.
type Ticket struct {
	ID        int64
	GuildID   string
	ChannelID string
	UserID    string
	Username  string
	Number    int
	Status    string // "open", "closed" or "archived"
	CreatedAt string
	ClosedAt  string
}

const ticketCols = `id, guild_id, channel_id, user_id, username, number, status,
	COALESCE(created_at, ''), COALESCE(closed_at, '')`

func scanTicket(r scanner) (*Ticket, error) {
	var t Ticket
	err := r.Scan(&t.ID, &t.GuildID, &t.ChannelID, &t.UserID, &t.Username, &t.Number, &t.Status,
		&t.CreatedAt, &t.ClosedAt)
	return &t, err
}

func (d *DB) oneTicket(query string, args ...any) (*Ticket, error) {
	t, err := scanTicket(d.sql.QueryRow(query, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// CreateTicket records a new open ticket.
func (d *DB) CreateTicket(guildID, channelID, userID, username string, number int) (int64, error) {
	res, err := d.sql.Exec(
		`INSERT INTO tickets (guild_id, channel_id, user_id, username, number, status)
		 VALUES (?, ?, ?, ?, ?, 'open')`,
		guildID, channelID, userID, username, number,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// OpenTicket returns a member's newest open ticket, or nil.
func (d *DB) OpenTicket(guildID, userID string) (*Ticket, error) {
	return d.oneTicket(`SELECT `+ticketCols+` FROM tickets
		WHERE guild_id=? AND user_id=? AND status='open' ORDER BY id DESC LIMIT 1`, guildID, userID)
}

// TicketByChannel returns the ticket in a channel, or nil.
func (d *DB) TicketByChannel(channelID string) (*Ticket, error) {
	return d.oneTicket(`SELECT `+ticketCols+` FROM tickets WHERE channel_id=?`, channelID)
}

// IsTicketChannel reports whether a channel is a ticket in the tickets table.
func (d *DB) IsTicketChannel(channelID string) (bool, error) {
	t, err := d.TicketByChannel(channelID)
	return t != nil, err
}

// NextTicketNumber is the member's next ticket number. It counts the
// legacy ticket_archive table too, so numbers are never reused.
func (d *DB) NextTicketNumber(guildID, userID string) (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT MAX(
		(SELECT COALESCE(MAX(number), 0) FROM tickets WHERE guild_id=? AND user_id=?),
		(SELECT COALESCE(MAX(number), 0) FROM ticket_archive WHERE guild_id=? AND user_id=?)
	) + 1`, guildID, userID, guildID, userID).Scan(&n)
	return n, err
}

// CloseTicket marks the ticket in a channel closed.
func (d *DB) CloseTicket(channelID string) error {
	_, err := d.sql.Exec(`UPDATE tickets SET status='closed', closed_at=datetime('now') WHERE channel_id=?`, channelID)
	return err
}

// ClosedTickets lists a guild's closed (not yet archived) tickets.
func (d *DB) ClosedTickets(guildID string) ([]*Ticket, error) {
	rows, err := d.sql.Query(`SELECT `+ticketCols+` FROM tickets WHERE guild_id=? AND status='closed'`, guildID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkTicketArchived flags a ticket archived. Its channel was moved, not deleted.
func (d *DB) MarkTicketArchived(id int64) error {
	_, err := d.sql.Exec(`UPDATE tickets SET status='archived' WHERE id=?`, id)
	return err
}

// TicketSummary is one line of a member's ticket history.
type TicketSummary struct {
	Number    int
	Status    string
	OpenedAt  string
	ClosedAt  string
	ChannelID string // "" for a legacy transcript-archived ticket
}

// TicketHistory lists every ticket a member opened, live and legacy
// archived, newest first.
func (d *DB) TicketHistory(guildID, userID string) ([]TicketSummary, error) {
	rows, err := d.sql.Query(`
		SELECT number, status, COALESCE(created_at, ''), COALESCE(closed_at, ''), CAST(channel_id AS TEXT)
		  FROM tickets WHERE guild_id=? AND user_id=?
		UNION ALL
		SELECT COALESCE(number, 0), 'archived', COALESCE(opened_at, ''), COALESCE(closed_at, ''), ''
		  FROM ticket_archive WHERE guild_id=? AND user_id=?
		ORDER BY 1 DESC`, guildID, userID, guildID, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TicketSummary
	for rows.Next() {
		var t TicketSummary
		if err := rows.Scan(&t.Number, &t.Status, &t.OpenedAt, &t.ClosedAt, &t.ChannelID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TicketRef identifies a ticket by its owner, for notes.
type TicketRef struct {
	UserID   string
	Username string
}

// FindTicket looks up a ticket by its "username-#" parts: live tickets
// first, then the legacy archive. It returns nil when there is none.
func (d *DB) FindTicket(guildID, username string, number int) (*TicketRef, error) {
	var r TicketRef
	err := d.sql.QueryRow(`
		SELECT user_id, username FROM tickets WHERE guild_id=? AND username=? AND number=?
		UNION ALL
		SELECT user_id, username FROM ticket_archive WHERE guild_id=? AND username=? AND number=?
		LIMIT 1`, guildID, username, number, guildID, username, number).Scan(&r.UserID, &r.Username)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// TicketNote is one mod note on a ticket.
type TicketNote struct {
	Username  string
	Number    int
	AuthorID  string // "" when unknown
	Note      string
	CreatedAt string
}

// AddTicketNote records a note on a ticket.
func (d *DB) AddTicketNote(guildID, userID, username string, number int, authorID, note string) error {
	_, err := d.sql.Exec(
		`INSERT INTO ticket_notes (guild_id, user_id, username, number, author_id, note) VALUES (?, ?, ?, ?, ?, ?)`,
		guildID, userID, username, number, nullIfEmpty(authorID), note,
	)
	return err
}

func (d *DB) ticketNotes(query string, args ...any) ([]TicketNote, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TicketNote
	for rows.Next() {
		var n TicketNote
		if err := rows.Scan(&n.Username, &n.Number, &n.AuthorID, &n.Note, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

const noteCols = `username, number, COALESCE(CAST(author_id AS TEXT), ''), note, COALESCE(created_at, '')`

// TicketNotes lists a ticket's notes, oldest first.
func (d *DB) TicketNotes(guildID, userID string, number int) ([]TicketNote, error) {
	return d.ticketNotes(`SELECT `+noteCols+` FROM ticket_notes
		WHERE guild_id=? AND user_id=? AND number=? ORDER BY created_at ASC`, guildID, userID, number)
}

// SearchTicketNotes finds notes containing query, newest first.
func (d *DB) SearchTicketNotes(guildID, query string, limit int) ([]TicketNote, error) {
	return d.ticketNotes(`SELECT `+noteCols+` FROM ticket_notes
		WHERE guild_id=? AND note LIKE ? ORDER BY created_at DESC LIMIT ?`, guildID, "%"+query+"%", limit)
}
