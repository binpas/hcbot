package db

import (
	"database/sql"
	"fmt"
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
	// Kind is "member" (opened with a reaction) or "mod" (opened with
	// /ticket open: UserID is "0" and Username is "mod").
	Kind     string
	OpenedBy string // the mod who opened a mod ticket
	// ArchivedAt is set when the transcript was saved and the channel
	// deleted. An archived ticket without it was moved to a Ticket Archive
	// category (the old way).
	ArchivedAt string
}

// Ref is the ticket identifier that mods type, e.g. "alice-2" or "mod-3".
func (t *Ticket) Ref() string { return TicketRefName(t.Username, t.Number) }

// TicketRefName makes a ticket identifier from its parts.
func TicketRefName(username string, number int) string {
	return fmt.Sprintf("%s-%d", username, number)
}

// ModTicketUser is the user_id and username of a mod ticket.
const (
	ModTicketUserID   = "0"
	ModTicketUsername = "mod"
)

const ticketCols = `id, CAST(guild_id AS TEXT), CAST(channel_id AS TEXT), CAST(user_id AS TEXT), username, number, status,
	COALESCE(created_at, ''), COALESCE(closed_at, ''), kind, opened_by, COALESCE(archived_at, '')`

func scanTicket(r scanner) (*Ticket, error) {
	var t Ticket
	err := r.Scan(&t.ID, &t.GuildID, &t.ChannelID, &t.UserID, &t.Username, &t.Number, &t.Status,
		&t.CreatedAt, &t.ClosedAt, &t.Kind, &t.OpenedBy, &t.ArchivedAt)
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
	return d.tickets(`SELECT `+ticketCols+` FROM tickets WHERE guild_id=? AND status='closed' ORDER BY id`, guildID)
}

// MovedTickets lists the tickets that were archived the old way: the
// channel was moved to a Ticket Archive category, and no transcript saved.
func (d *DB) MovedTickets(guildID string) ([]*Ticket, error) {
	return d.tickets(`SELECT `+ticketCols+` FROM tickets
		WHERE guild_id=? AND status='archived' AND archived_at IS NULL ORDER BY id`, guildID)
}

// TicketByID returns a ticket by its row ID, or nil.
func (d *DB) TicketByID(id int64) (*Ticket, error) {
	return d.oneTicket(`SELECT `+ticketCols+` FROM tickets WHERE id=?`, id)
}

func (d *DB) tickets(query string, args ...any) ([]*Ticket, error) {
	rows, err := d.sql.Query(query, args...)
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

// MarkTicketArchived flags a ticket archived: its transcript is saved and
// its channel can be deleted.
func (d *DB) MarkTicketArchived(id int64) error {
	_, err := d.sql.Exec(`UPDATE tickets SET status='archived', archived_at=datetime('now'),
		closed_at=COALESCE(closed_at, datetime('now')) WHERE id=?`, id)
	return err
}

// TicketMember is a user that a mod added to a mod ticket.
type TicketMember struct {
	UserID   string
	Username string
}

// CreateModTicket records a new open mod ticket and its users.
func (d *DB) CreateModTicket(guildID, channelID, openedBy string, number int, members []TicketMember) (int64, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`INSERT INTO tickets (guild_id, channel_id, user_id, username, number, status, kind, opened_by)
		 VALUES (?, ?, ?, ?, ?, 'open', 'mod', ?)`,
		guildID, channelID, ModTicketUserID, ModTicketUsername, number, openedBy,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, m := range members {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO ticket_members (ticket_id, user_id, username) VALUES (?, ?, ?)`,
			id, m.UserID, m.Username); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// TicketMembers lists the users of a mod ticket.
func (d *DB) TicketMembers(ticketID int64) ([]TicketMember, error) {
	rows, err := d.sql.Query(`SELECT user_id, username FROM ticket_members WHERE ticket_id=? ORDER BY username`, ticketID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TicketMember
	for rows.Next() {
		var m TicketMember
		if err := rows.Scan(&m.UserID, &m.Username); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// TicketSummary is one line of a member's ticket history.
type TicketSummary struct {
	ID         int64 // 0 for a legacy transcript
	LegacyID   int64 // the ticket_archive row, for a legacy transcript
	Username   string
	Number     int
	Kind       string
	Status     string
	OpenedAt   string
	ClosedAt   string
	ArchivedAt string
	ChannelID  string // "" for a legacy transcript
}

// Ref is the ticket identifier, e.g. "alice-2" or "mod-3".
func (t TicketSummary) Ref() string { return TicketRefName(t.Username, t.Number) }

// TicketHistory lists every ticket a member opened or was added to, live
// and legacy archived: member tickets first, then mod tickets, each with
// the highest number first.
func (d *DB) TicketHistory(guildID, userID string) ([]TicketSummary, error) {
	rows, err := d.sql.Query(`
		SELECT id, 0, username, number, kind, status, COALESCE(created_at, ''), COALESCE(closed_at, ''),
		       COALESCE(archived_at, ''), CAST(channel_id AS TEXT)
		  FROM tickets WHERE guild_id=? AND (user_id=? OR id IN (SELECT ticket_id FROM ticket_members WHERE user_id=?))
		UNION ALL
		SELECT 0, id, username, COALESCE(number, 0), 'member', 'archived', COALESCE(opened_at, ''), COALESCE(closed_at, ''),
		       COALESCE(archived_at, ''), ''
		  FROM ticket_archive WHERE guild_id=? AND user_id=?
		ORDER BY 5, 4 DESC`, guildID, userID, userID, guildID, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TicketSummary
	for rows.Next() {
		var t TicketSummary
		if err := rows.Scan(&t.ID, &t.LegacyID, &t.Username, &t.Number, &t.Kind, &t.Status, &t.OpenedAt, &t.ClosedAt,
			&t.ArchivedAt, &t.ChannelID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TicketRef identifies a ticket by its owner, for notes, and says where
// its transcript is.
type TicketRef struct {
	ID       int64 // the tickets row, or 0 for a legacy transcript
	LegacyID int64 // the ticket_archive row, or 0
	UserID   string
	Username string
}

// FindTicket looks up a ticket by its "username-#" parts: mod tickets
// ("mod-#") first, then member tickets, then the legacy archive. It
// returns nil when there is none.
func (d *DB) FindTicket(guildID, username string, number int) (*TicketRef, error) {
	var r TicketRef
	err := d.sql.QueryRow(`
		SELECT id, 0, CAST(user_id AS TEXT), username, kind='mod' FROM tickets WHERE guild_id=? AND username=? AND number=?
		UNION ALL
		SELECT 0, id, CAST(user_id AS TEXT), username, 0 FROM ticket_archive WHERE guild_id=? AND username=? AND number=?
		ORDER BY 5 DESC, 1 DESC LIMIT 1`, guildID, username, number, guildID, username, number).
		Scan(&r.ID, &r.LegacyID, &r.UserID, &r.Username, new(bool))
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
