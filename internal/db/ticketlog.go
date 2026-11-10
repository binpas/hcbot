package db

import (
	"database/sql"
	"strings"
)

// TicketMessage is one message of a ticket transcript.
type TicketMessage struct {
	TicketID   int64
	MessageID  string
	AuthorID   string
	AuthorName string
	Bot        bool
	Content    string
	SentAt     string // "2006-01-02 15:04:05", UTC
	EditedAt   string
	DeletedAt  string
	Edits      []TicketEdit // the text before each edit, oldest first
	Files      []TicketFile
}

// TicketEdit is the text of a message before an edit.
type TicketEdit struct {
	Content    string
	ReplacedAt string
}

// TicketFile is an attachment of a ticket message.
type TicketFile struct {
	ID           int64
	TicketID     int64
	MessageID    string
	AttachmentID string
	Filename     string
	ContentType  string
	Size         int64
	Path         string // relative to TICKET_FILES_DIR; "" when not saved
	Note         string // why the file was not saved
}

// SaveTicketMessage records a ticket message. For a message that is
// already recorded with other text, it keeps the old text as an edit. It
// reports whether the message was new or changed.
func (d *DB) SaveTicketMessage(m TicketMessage) (bool, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var old string
	err = tx.QueryRow(`SELECT content FROM ticket_messages WHERE message_id=?`, m.MessageID).Scan(&old)
	switch {
	case err == sql.ErrNoRows:
		_, err = tx.Exec(`INSERT INTO ticket_messages
			(ticket_id, message_id, author_id, author_name, bot, content, sent_at, edited_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			m.TicketID, m.MessageID, m.AuthorID, m.AuthorName, m.Bot, m.Content, m.SentAt, nullIfEmpty(m.EditedAt))
	case err != nil:
		return false, err
	case old == m.Content:
		return false, nil
	default:
		if _, err = tx.Exec(`INSERT INTO ticket_edits (message_id, content) VALUES (?, ?)`, m.MessageID, old); err == nil {
			edited := m.EditedAt
			if edited == "" {
				edited = "now"
			}
			_, err = tx.Exec(`UPDATE ticket_messages SET content=?,
				edited_at=CASE WHEN ?='now' THEN datetime('now') ELSE ? END WHERE message_id=?`,
				m.Content, edited, edited, m.MessageID)
		}
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// MarkTicketMessageDeleted records that a ticket message was deleted. The
// text stays in the transcript.
func (d *DB) MarkTicketMessageDeleted(messageID string) error {
	_, err := d.sql.Exec(`UPDATE ticket_messages SET deleted_at=datetime('now')
		WHERE message_id=? AND deleted_at IS NULL`, messageID)
	return err
}

// HasTicketFile reports whether an attachment is already recorded.
func (d *DB) HasTicketFile(attachmentID string) (bool, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM ticket_files WHERE attachment_id=?`, attachmentID).Scan(&n)
	return n > 0, err
}

// AddTicketFile records an attachment. A second record of the same
// attachment is ignored.
func (d *DB) AddTicketFile(f TicketFile) error {
	_, err := d.sql.Exec(`INSERT OR IGNORE INTO ticket_files
		(ticket_id, message_id, attachment_id, filename, content_type, size, path, note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		f.TicketID, f.MessageID, f.AttachmentID, f.Filename, f.ContentType, f.Size, f.Path, f.Note)
	return err
}

const ticketFileCols = `id, ticket_id, message_id, attachment_id, filename, content_type, size, path, note`

func scanTicketFile(r scanner) (TicketFile, error) {
	var f TicketFile
	err := r.Scan(&f.ID, &f.TicketID, &f.MessageID, &f.AttachmentID, &f.Filename, &f.ContentType, &f.Size, &f.Path, &f.Note)
	return f, err
}

// TicketFileByID returns one recorded attachment, or nil.
func (d *DB) TicketFileByID(id int64) (*TicketFile, error) {
	f, err := scanTicketFile(d.sql.QueryRow(`SELECT `+ticketFileCols+` FROM ticket_files WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// TicketTranscript returns the messages of a ticket in the order they were
// sent, with their edits and files.
func (d *DB) TicketTranscript(ticketID int64) ([]*TicketMessage, error) {
	rows, err := d.sql.Query(`SELECT message_id, author_id, author_name, bot, content, sent_at,
		COALESCE(edited_at, ''), COALESCE(deleted_at, '')
		FROM ticket_messages WHERE ticket_id=? ORDER BY CAST(message_id AS INTEGER)`, ticketID)
	if err != nil {
		return nil, err
	}
	var out []*TicketMessage
	byID := map[string]*TicketMessage{}
	for rows.Next() {
		m := &TicketMessage{TicketID: ticketID}
		if err := rows.Scan(&m.MessageID, &m.AuthorID, &m.AuthorName, &m.Bot, &m.Content, &m.SentAt,
			&m.EditedAt, &m.DeletedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, m)
		byID[m.MessageID] = m
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = d.sql.Query(`SELECT e.message_id, e.content, COALESCE(e.replaced_at, '') FROM ticket_edits e
		JOIN ticket_messages m ON m.message_id = e.message_id WHERE m.ticket_id=? ORDER BY e.id`, ticketID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var e TicketEdit
		if err := rows.Scan(&id, &e.Content, &e.ReplacedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if m := byID[id]; m != nil {
			m.Edits = append(m.Edits, e)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = d.sql.Query(`SELECT `+ticketFileCols+` FROM ticket_files WHERE ticket_id=? ORDER BY id`, ticketID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		f, err := scanTicketFile(rows)
		if err != nil {
			return nil, err
		}
		if m := byID[f.MessageID]; m != nil {
			m.Files = append(m.Files, f)
		}
	}
	return out, rows.Err()
}

// LegacyTicket is a transcript from the legacy ticket_archive table.
type LegacyTicket struct {
	ID           int64
	UserID       string
	Username     string
	Number       int
	ChannelName  string
	OpenedAt     string
	ClosedAt     string
	ArchivedAt   string
	MessageCount int
	Transcript   string
}

// LegacyTranscript returns a legacy archived ticket, or nil.
func (d *DB) LegacyTranscript(id int64) (*LegacyTicket, error) {
	var t LegacyTicket
	err := d.sql.QueryRow(`SELECT id, CAST(user_id AS TEXT), username, COALESCE(number, 0), channel_name,
		COALESCE(opened_at, ''), COALESCE(closed_at, ''), COALESCE(archived_at, ''), COALESCE(message_count, 0), transcript
		FROM ticket_archive WHERE id=?`, id).Scan(&t.ID, &t.UserID, &t.Username, &t.Number, &t.ChannelName,
		&t.OpenedAt, &t.ClosedAt, &t.ArchivedAt, &t.MessageCount, &t.Transcript)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// TicketHit is a ticket message (or a legacy transcript) that matches a
// search.
type TicketHit struct {
	ID         int64 // the tickets row, or 0
	LegacyID   int64
	Username   string
	Number     int
	AuthorName string
	Text       string // the whole message, or the whole legacy transcript
	SentAt     string
}

// Ref is the ticket identifier of the hit.
func (h TicketHit) Ref() string { return TicketRefName(h.Username, h.Number) }

// likePattern makes a LIKE pattern that finds text anywhere. The escape
// character is a backslash.
func likePattern(text string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(text) + "%"
}

// SearchTicketMessages finds ticket messages (now or before an edit) and
// legacy transcripts that contain text, newest first.
func (d *DB) SearchTicketMessages(guildID, text string, limit int) ([]TicketHit, error) {
	like := likePattern(text)
	rows, err := d.sql.Query(`
		SELECT t.id, 0, t.username, t.number, m.author_name, m.content, m.sent_at
		  FROM ticket_messages m JOIN tickets t ON t.id = m.ticket_id
		 WHERE t.guild_id=? AND (m.content LIKE ? ESCAPE '\' OR EXISTS (
		       SELECT 1 FROM ticket_edits e WHERE e.message_id = m.message_id AND e.content LIKE ? ESCAPE '\'))
		UNION ALL
		SELECT 0, a.id, a.username, COALESCE(a.number, 0), '', a.transcript, COALESCE(a.archived_at, '')
		  FROM ticket_archive a WHERE a.guild_id=? AND a.transcript LIKE ? ESCAPE '\'
		ORDER BY 7 DESC LIMIT ?`, guildID, like, like, guildID, like, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TicketHit
	for rows.Next() {
		var h TicketHit
		if err := rows.Scan(&h.ID, &h.LegacyID, &h.Username, &h.Number, &h.AuthorName, &h.Text, &h.SentAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// TicketListFilter limits ListTickets. Empty fields match all.
type TicketListFilter struct {
	User  string // a user ID, or a part of a username (opener or added user)
	Text  string // a part of a message, an older version of one, or a legacy transcript
	Limit int
}

// TicketRow is one ticket in a list, live or legacy.
type TicketRow struct {
	ID         int64
	LegacyID   int64
	UserID     string
	Username   string
	Number     int
	Kind       string
	Status     string
	OpenedAt   string
	ClosedAt   string
	ArchivedAt string
	ChannelID  string
	Members    string // the users of a mod ticket, comma-separated
	Messages   int
}

// Ref is the ticket identifier of the row.
func (t TicketRow) Ref() string { return TicketRefName(t.Username, t.Number) }

// ListTickets lists a guild's tickets, newest first.
func (d *DB) ListTickets(guildID string, f TicketListFilter) ([]TicketRow, error) {
	user, userLike := strings.TrimSpace(f.User), likePattern(strings.TrimSpace(f.User))
	text, textLike := strings.TrimSpace(f.Text), likePattern(strings.TrimSpace(f.Text))
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.sql.Query(`
		SELECT t.id, 0, CAST(t.user_id AS TEXT), t.username, t.number, t.kind, t.status, COALESCE(t.created_at, ''),
		       COALESCE(t.closed_at, ''), COALESCE(t.archived_at, ''), CAST(t.channel_id AS TEXT),
		       COALESCE((SELECT GROUP_CONCAT(username, ', ') FROM ticket_members WHERE ticket_id = t.id), ''),
		       (SELECT COUNT(*) FROM ticket_messages WHERE ticket_id = t.id)
		  FROM tickets t
		 WHERE t.guild_id=?
		   AND (?='' OR (t.kind='member' AND (CAST(t.user_id AS TEXT)=? OR t.username LIKE ? ESCAPE '\'))
		        OR EXISTS (SELECT 1 FROM ticket_members tm WHERE tm.ticket_id = t.id
		                   AND (tm.user_id=? OR tm.username LIKE ? ESCAPE '\')))
		   AND (?='' OR EXISTS (SELECT 1 FROM ticket_messages m WHERE m.ticket_id = t.id AND (m.content LIKE ? ESCAPE '\'
		        OR EXISTS (SELECT 1 FROM ticket_edits e WHERE e.message_id = m.message_id AND e.content LIKE ? ESCAPE '\'))))
		UNION ALL
		SELECT 0, a.id, CAST(a.user_id AS TEXT), a.username, COALESCE(a.number, 0), 'member', 'archived',
		       COALESCE(a.opened_at, ''), COALESCE(a.closed_at, ''), COALESCE(a.archived_at, ''), '', '',
		       COALESCE(a.message_count, 0)
		  FROM ticket_archive a
		 WHERE a.guild_id=? AND (?='' OR CAST(a.user_id AS TEXT)=? OR a.username LIKE ? ESCAPE '\')
		   AND (?='' OR a.transcript LIKE ? ESCAPE '\')
		ORDER BY 8 DESC, 1 DESC LIMIT ?`,
		guildID, user, user, userLike, user, userLike, text, textLike, textLike,
		guildID, user, user, userLike, text, textLike, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TicketRow
	for rows.Next() {
		var t TicketRow
		if err := rows.Scan(&t.ID, &t.LegacyID, &t.UserID, &t.Username, &t.Number, &t.Kind, &t.Status, &t.OpenedAt,
			&t.ClosedAt, &t.ArchivedAt, &t.ChannelID, &t.Members, &t.Messages); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
