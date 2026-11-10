package bot

import (
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/binpas/hcbot/internal/db"
)

// The Tickets area of the web interface: find tickets by member or by text,
// and read a transcript with its files. It only reads.

const webTicketsMax = 100

type webTicketRow struct {
	Key      string `json:"key"` // "t<id>" or "l<id>" (legacy)
	Ref      string `json:"ref"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	UserID   string `json:"userId,omitempty"`
	Username string `json:"username,omitempty"`
	Members  string `json:"members,omitempty"`
	Opened   string `json:"opened"`
	Closed   string `json:"closed,omitempty"`
	Archived string `json:"archived,omitempty"`
	Messages int    `json:"messages"`
	Link     string `json:"link,omitempty"` // the channel, while it is there
}

type webTicketFile struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Media string `json:"media,omitempty"` // img, video or audio, to show it in the page
	Note  string `json:"note,omitempty"`  // why it was not saved
	Saved bool   `json:"saved"`
}

type webTicketEdit struct {
	Content string `json:"content"`
	At      string `json:"at"`
}

type webTicketMessage struct {
	ID       string          `json:"id"`
	Author   string          `json:"author"`
	AuthorID string          `json:"authorId"`
	Bot      bool            `json:"bot,omitempty"`
	Content  string          `json:"content"`
	Sent     string          `json:"sent"`
	Edited   string          `json:"edited,omitempty"`
	Deleted  string          `json:"deleted,omitempty"`
	Edits    []webTicketEdit `json:"edits,omitempty"`
	Files    []webTicketFile `json:"files,omitempty"`
}

type webTicketNote struct {
	Author string `json:"author"`
	Note   string `json:"note"`
	At     string `json:"at"`
}

func (b *Bot) registerWebTickets(route func(string, webLevel, func(http.ResponseWriter, *http.Request, *webSession))) {
	route("GET /api/tickets", webMod, b.webTickets)
	route("GET /api/tickets/{key}", webMod, b.webTicket)
	route("GET /files/ticket/{id}", webMod, b.webTicketFile)
}

func webTicketKey(t db.TicketRow) string {
	if t.LegacyID != 0 {
		return "l" + strconv.FormatInt(t.LegacyID, 10)
	}
	return "t" + strconv.FormatInt(t.ID, 10)
}

func (b *Bot) webTickets(w http.ResponseWriter, r *http.Request, _ *webSession) {
	q := r.URL.Query()
	user, text := strings.TrimSpace(q.Get("user")), strings.TrimSpace(q.Get("q"))
	if len(user) > 100 || len(text) > 200 {
		webFail(w, http.StatusBadRequest, "The search text is too long.")
		return
	}
	rows, err := b.db.ListTickets(b.env.GuildID, db.TicketListFilter{User: user, Text: text, Limit: webTicketsMax + 1})
	if err != nil {
		log.Printf("web: list tickets: %v", err)
		webFail(w, http.StatusInternalServerError, "I could not read the tickets.")
		return
	}
	more := len(rows) > webTicketsMax
	rows = rows[:min(len(rows), webTicketsMax)]
	out := make([]webTicketRow, len(rows))
	for i, t := range rows {
		out[i] = webTicketRow{Key: webTicketKey(t), Ref: t.Ref(), Kind: t.Kind, Status: t.Status, Members: t.Members,
			Opened: t.OpenedAt, Closed: t.ClosedAt, Archived: t.ArchivedAt, Messages: t.Messages}
		if t.Kind != "mod" {
			out[i].UserID, out[i].Username = t.UserID, t.Username
		}
		if t.ChannelID != "" && t.Status != "archived" {
			out[i].Link = jumpURL(b.env.GuildID, t.ChannelID)
		}
	}
	webJSON(w, http.StatusOK, map[string]any{"tickets": out, "more": more})
}

func (b *Bot) webTicket(w http.ResponseWriter, r *http.Request, _ *webSession) {
	key := r.PathValue("key")
	id, err := strconv.ParseInt(strings.TrimLeft(key, "tl"), 10, 64)
	if err != nil || len(key) < 2 || (key[0] != 't' && key[0] != 'l') {
		webFail(w, http.StatusNotFound, "There is no such ticket.")
		return
	}
	if key[0] == 'l' {
		b.webLegacyTicket(w, id)
		return
	}
	t, err := b.db.TicketByID(id)
	if err != nil {
		log.Printf("web: ticket %d: %v", id, err)
		webFail(w, http.StatusInternalServerError, "I could not read the ticket.")
		return
	}
	if t == nil || t.GuildID != b.env.GuildID {
		webFail(w, http.StatusNotFound, "There is no such ticket.")
		return
	}
	msgs, err := b.db.TicketTranscript(t.ID)
	if err != nil {
		log.Printf("web: transcript %d: %v", id, err)
		webFail(w, http.StatusInternalServerError, "I could not read the transcript.")
		return
	}
	out := make([]webTicketMessage, len(msgs))
	for i, m := range msgs {
		wm := webTicketMessage{ID: m.MessageID, Author: m.AuthorName, AuthorID: m.AuthorID, Bot: m.Bot,
			Content: m.Content, Sent: m.SentAt, Edited: m.EditedAt, Deleted: m.DeletedAt}
		for _, e := range m.Edits {
			wm.Edits = append(wm.Edits, webTicketEdit{Content: e.Content, At: e.ReplacedAt})
		}
		for _, f := range m.Files {
			wf := webTicketFile{ID: f.ID, Name: f.Filename, Size: f.Size, Note: f.Note, Saved: f.Path != ""}
			if wf.Saved {
				wf.Media = ticketMediaKind(f.ContentType)
			}
			wm.Files = append(wm.Files, wf)
		}
		out[i] = wm
	}
	body := map[string]any{"ref": t.Ref(), "kind": t.Kind, "status": t.Status, "facts": b.ticketFacts(t),
		"messages": out, "notes": b.webTicketNotes(t.UserID, t.Number)}
	if t.ChannelID != "" && t.Status != "archived" {
		body["link"] = jumpURL(b.env.GuildID, t.ChannelID)
	}
	webJSON(w, http.StatusOK, body)
}

func (b *Bot) webLegacyTicket(w http.ResponseWriter, id int64) {
	lt, err := b.db.LegacyTranscript(id)
	if err != nil {
		log.Printf("web: legacy transcript %d: %v", id, err)
		webFail(w, http.StatusInternalServerError, "I could not read the transcript.")
		return
	}
	if lt == nil {
		webFail(w, http.StatusNotFound, "There is no such ticket.")
		return
	}
	facts := []string{"An older transcript, from before the bot kept each message.",
		"Opened by " + lt.Username + " (" + lt.UserID + ")."}
	for _, f := range [][2]string{{"Opened", lt.OpenedAt}, {"Closed", lt.ClosedAt}, {"Archived", lt.ArchivedAt}} {
		if f[1] != "" {
			facts = append(facts, f[0]+" "+f[1]+" UTC.")
		}
	}
	webJSON(w, http.StatusOK, map[string]any{"ref": db.TicketRefName(lt.Username, lt.Number), "kind": "member",
		"status": "archived", "facts": facts, "legacy": lt.Transcript, "messages": []webTicketMessage{},
		"notes": b.webTicketNotes(lt.UserID, lt.Number)})
}

func (b *Bot) webTicketNotes(userID string, number int) []webTicketNote {
	notes, err := b.db.TicketNotes(b.env.GuildID, userID, number)
	if err != nil {
		log.Printf("web: ticket notes: %v", err)
	}
	out := make([]webTicketNote, len(notes))
	for i, n := range notes {
		out[i] = webTicketNote{Author: n.AuthorID, Note: n.Note, At: n.CreatedAt}
		if n.AuthorID != "" {
			if m := b.resolveMember(b.env.GuildID, n.AuthorID); m != nil && m.User != nil {
				out[i].Author = m.User.Username
			}
		}
	}
	return out
}

// webTicketFile serves a saved ticket attachment. A picture, video or
// sound file shows in the page; any other file is a download. The file
// cannot run a script on this site.
func (b *Bot) webTicketFile(w http.ResponseWriter, r *http.Request, _ *webSession) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		webFail(w, http.StatusNotFound, "Not found.")
		return
	}
	f, err := b.db.TicketFileByID(id)
	if err != nil || f == nil || f.Path == "" || b.env.TicketFilesDir == "" {
		webFail(w, http.StatusNotFound, "Not found.")
		return
	}
	if t, err := b.db.TicketByID(f.TicketID); err != nil || t == nil || t.GuildID != b.env.GuildID {
		webFail(w, http.StatusNotFound, "Not found.")
		return
	}
	clean := filepath.Clean(f.Path)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		webFail(w, http.StatusNotFound, "Not found.")
		return
	}
	file, err := os.Open(filepath.Join(b.env.TicketFilesDir, clean))
	if err != nil {
		webFail(w, http.StatusNotFound, "The file is not on the server any more.")
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		webFail(w, http.StatusNotFound, "Not found.")
		return
	}
	h := w.Header()
	// The browser must not keep a ticket file after the logout (webGuard
	// sets no-store).
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	disposition := "attachment"
	if ticketMediaKind(f.ContentType) != "" {
		h.Set("Content-Type", f.ContentType)
		disposition = "inline"
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": f.Filename}))
	http.ServeContent(w, r, "", info.ModTime(), file)
}
