package bot

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/db"
)

// The ticket transcripts. With the Message Content intent, the bot keeps
// each message sent in a ticket channel (text, edits, deletes, and the
// attachments as files in TICKET_FILES_DIR). When a ticket is archived,
// the bot reads the channel history once more for the messages that it
// missed (for example while it was offline), and then deletes the channel.

const (
	ticketTimeFormat = "2006-01-02 15:04:05"
	// ticketInlineBudget is how many bytes of pictures a transcript file
	// holds. Discord's upload limit is 10 MB on a server with no boost.
	ticketInlineBudget = 8 << 20
)

var ticketFileUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// ticketFileName makes an attachment's file name safe for the disk.
func ticketFileName(attachmentID, name string) string {
	name = strings.Trim(ticketFileUnsafe.ReplaceAllString(name, "_"), "._")
	if len(name) > 80 {
		name = name[len(name)-80:]
	}
	if name == "" {
		name = "file"
	}
	return attachmentID + "-" + name
}

// ticketMessageText is the text of a message for the transcript: its
// content, the stickers, and (for a bot) its embeds.
func ticketMessageText(m *discordgo.Message) string {
	parts := []string{}
	if m.Content != "" {
		parts = append(parts, m.Content)
	}
	for _, st := range m.StickerItems {
		parts = append(parts, "[sticker: "+st.Name+"]")
	}
	if m.Author != nil && m.Author.Bot {
		for _, e := range m.Embeds {
			var lines []string
			for _, s := range []string{e.Title, e.Description} {
				if s != "" {
					lines = append(lines, s)
				}
			}
			for _, f := range e.Fields {
				lines = append(lines, f.Name+": "+f.Value)
			}
			if len(lines) > 0 {
				parts = append(parts, "[embed] "+strings.Join(lines, "\n"))
			}
		}
	}
	return strings.Join(parts, "\n")
}

// liveTicket returns the ticket of a channel when its messages must be
// kept: an open or closed ticket. It returns nil for other channels.
func (b *Bot) liveTicket(channelID string) *db.Ticket {
	t, err := b.db.TicketByChannel(channelID)
	if err != nil {
		log.Printf("ticket log: find ticket: %v", err)
		return nil
	}
	if t == nil || t.Status == "archived" {
		return nil
	}
	return t
}

// logTicketMessage keeps a new message of a ticket channel.
func (b *Bot) logTicketMessage(m *discordgo.Message) {
	if !b.env.MessageContent || m.GuildID == "" || m.Author == nil {
		return
	}
	t := b.liveTicket(m.ChannelID)
	if t == nil {
		return
	}
	if err := b.saveTicketMessage(t.ID, m); err != nil {
		log.Printf("ticket log: save message in %s: %v", t.Ref(), err)
	}
}

// saveTicketMessage records one message and its attachments.
func (b *Bot) saveTicketMessage(ticketID int64, m *discordgo.Message) error {
	text := ticketMessageText(m)
	if text == "" && len(m.Attachments) == 0 {
		return nil // for example a pin notice
	}
	tm := db.TicketMessage{
		TicketID: ticketID, MessageID: m.ID, AuthorID: m.Author.ID, AuthorName: m.Author.Username,
		Bot: m.Author.Bot, Content: text, SentAt: m.Timestamp.UTC().Format(ticketTimeFormat),
	}
	if m.Timestamp.IsZero() {
		tm.SentAt = time.Now().UTC().Format(ticketTimeFormat)
	}
	if m.EditedTimestamp != nil {
		tm.EditedAt = m.EditedTimestamp.UTC().Format(ticketTimeFormat)
	}
	if _, err := b.db.SaveTicketMessage(tm); err != nil {
		return err
	}
	for _, a := range m.Attachments {
		if err := b.saveTicketFile(ticketID, m.ID, a); err != nil {
			return err
		}
	}
	return nil
}

// saveTicketFile downloads one attachment into TICKET_FILES_DIR and
// records it. A file that cannot be saved is recorded with the reason. It
// returns an error only when the database fails.
func (b *Bot) saveTicketFile(ticketID int64, messageID string, a *discordgo.MessageAttachment) error {
	if has, err := b.db.HasTicketFile(a.ID); err != nil || has {
		return err
	}
	f := db.TicketFile{TicketID: ticketID, MessageID: messageID, AttachmentID: a.ID, Filename: a.Filename,
		ContentType: a.ContentType, Size: int64(a.Size)}
	maxMB := max(b.cfg.Int("TICKET_FILE_MAX_MB"), 1)
	maxBytes := int64(maxMB) << 20
	switch {
	case b.env.TicketFilesDir == "":
		f.Note = "The bot does not save files (TICKET_FILES_DIR is not set)."
	case f.Size > maxBytes:
		f.Note = fmt.Sprintf("Larger than %d MB (TICKET_FILE_MAX_MB).", maxMB)
	default:
		data, err := b.fetchTicketFile(a.URL, maxBytes)
		if err != nil {
			log.Printf("ticket log: download %s: %v", a.Filename, err)
			f.Note = "The download failed."
			break
		}
		rel := filepath.Join(fmt.Sprint(ticketID), ticketFileName(a.ID, a.Filename))
		if err := writeTicketFile(filepath.Join(b.env.TicketFilesDir, rel), data); err != nil {
			log.Printf("ticket log: write %s: %v", rel, err)
			f.Note = "The bot could not write the file."
			break
		}
		f.Path, f.Size = rel, int64(len(data))
		if f.ContentType == "" {
			f.ContentType = http.DetectContentType(data)
		}
	}
	return b.db.AddTicketFile(f)
}

func (b *Bot) fetchTicketFile(url string, maxBytes int64) ([]byte, error) {
	resp, err := b.s.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxBytes)
	}
	return data, nil
}

func writeTicketFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readTicketFile reads a saved attachment. The path comes from the
// database, but it is checked to stay in TICKET_FILES_DIR.
func (b *Bot) readTicketFile(f db.TicketFile) ([]byte, error) {
	if f.Path == "" || b.env.TicketFilesDir == "" {
		return nil, os.ErrNotExist
	}
	clean := filepath.Clean(f.Path)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(filepath.Join(b.env.TicketFilesDir, clean))
}

func (b *Bot) onTicketMessageUpdate(_ *discordgo.Session, m *discordgo.MessageUpdate) {
	defer recoverHandler("ticket message update handler")
	if b.env.SetupMode || !b.env.MessageContent || m.GuildID == "" || m.Author == nil || m.EditedTimestamp == nil {
		return // a link preview or another partial update
	}
	t := b.liveTicket(m.ChannelID)
	if t == nil {
		return
	}
	if err := b.saveTicketMessage(t.ID, m.Message); err != nil {
		log.Printf("ticket log: save edit in %s: %v", t.Ref(), err)
	}
}

func (b *Bot) onTicketMessageDelete(_ *discordgo.Session, m *discordgo.MessageDelete) {
	defer recoverHandler("ticket message delete handler")
	if b.env.SetupMode || m.GuildID == "" {
		return
	}
	if err := b.db.MarkTicketMessageDeleted(m.ID); err != nil {
		log.Printf("ticket log: mark deleted: %v", err)
	}
}

func (b *Bot) onTicketMessageDeleteBulk(_ *discordgo.Session, m *discordgo.MessageDeleteBulk) {
	defer recoverHandler("ticket bulk delete handler")
	if b.env.SetupMode || m.GuildID == "" {
		return
	}
	for _, id := range m.Messages {
		if err := b.db.MarkTicketMessageDeleted(id); err != nil {
			log.Printf("ticket log: mark deleted: %v", err)
		}
	}
}

// backfillTicket reads the whole history of a ticket channel and records
// each message that the bot does not have yet (or has with other text).
func (b *Bot) backfillTicket(t *db.Ticket) error {
	before := ""
	for {
		msgs, err := b.s.ChannelMessages(t.ChannelID, 100, before, "", "")
		if err != nil {
			return fmt.Errorf("read messages: %w", err)
		}
		for _, m := range msgs {
			if m.Author == nil {
				continue
			}
			m.GuildID = t.GuildID
			if err := b.saveTicketMessage(t.ID, m); err != nil {
				return fmt.Errorf("save message: %w", err)
			}
		}
		if len(msgs) < 100 {
			return nil
		}
		before = msgs[len(msgs)-1].ID
	}
}

// saveTicketTranscript completes the transcript of a ticket from its
// channel and marks the ticket archived. The channel stays; the caller
// deletes it. Without the Message Content intent, the bot cannot read the
// messages, so it does not archive.
func (b *Bot) saveTicketTranscript(t *db.Ticket) error {
	if !b.env.MessageContent {
		return fmt.Errorf("the Message Content intent is off")
	}
	if err := b.backfillTicket(t); err != nil {
		return err
	}
	return b.db.MarkTicketArchived(t.ID)
}

// deleteTicketChannel deletes the channel of an archived ticket.
func (b *Bot) deleteTicketChannel(t *db.Ticket, by string) error {
	_, err := b.s.ChannelDelete(t.ChannelID, auditReason("Ticket "+t.Ref()+" archived by "+by))
	if err != nil && isDiscordError(err, discordgo.ErrCodeUnknownChannel) {
		return nil
	}
	return err
}

// archiveTicket saves the transcript and deletes the channel. It returns
// false when the ticket was not archived, and a note for the mod.
func (b *Bot) archiveTicket(t *db.Ticket, by string) (bool, string) {
	if err := b.saveTicketTranscript(t); err != nil {
		log.Printf("archive ticket %s: %v", t.Ref(), err)
		return false, fmt.Sprintf("I could not save the transcript of `%s`, so I did not delete its channel.", t.Ref())
	}
	if err := b.deleteTicketChannel(t, by); err != nil {
		log.Printf("archive ticket %s: delete channel: %v", t.Ref(), err)
		return true, fmt.Sprintf("I saved the transcript of `%s`, but I could not delete %s.", t.Ref(), channelMention(t.ChannelID))
	}
	return true, fmt.Sprintf("Saved the transcript of `%s` and deleted its channel.", t.Ref())
}

// ---- The transcript file. ----

// ticketTranscriptHTML makes a transcript page. Pictures are put in the
// page up to ticketInlineBudget bytes; other files are listed by name.
func (b *Bot) ticketTranscriptHTML(title string, facts []string, msgs []*db.TicketMessage) []byte {
	var w bytes.Buffer
	esc := html.EscapeString
	fmt.Fprintf(&w, `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>%s</title>
<style>
body{font:15px/1.45 system-ui,sans-serif;margin:0 auto;max-width:900px;padding:16px;background:#111;color:#eee}
h1{font-size:20px}.facts{color:#aaa}.msg{border-top:1px solid #333;padding:8px 0}
.who{font-weight:600}.bot{color:#8ab4f8}.when,.note{color:#999;font-size:13px}
.text{white-space:pre-wrap;overflow-wrap:anywhere}.old{white-space:pre-wrap;color:#bbb;border-left:3px solid #555;padding-left:8px;margin:4px 0}
.deleted{color:#f28b82}img,video{display:block;max-width:100%%;max-height:480px;margin:4px 0;border-radius:4px}
</style></head><body><h1>%s</h1>`, esc(title), esc(title))
	for _, f := range facts {
		fmt.Fprintf(&w, `<div class="facts">%s</div>`, esc(f))
	}
	if len(msgs) == 0 {
		w.WriteString(`<p class="note">The bot has no messages for this ticket.</p>`)
	}
	budget := int64(ticketInlineBudget)
	for _, m := range msgs {
		who := "who"
		if m.Bot {
			who += " bot"
		}
		fmt.Fprintf(&w, `<div class="msg"><span class="%s">%s</span> <span class="when">(%s) · %s UTC`,
			who, esc(m.AuthorName), esc(m.AuthorID), esc(m.SentAt))
		if m.EditedAt != "" {
			fmt.Fprintf(&w, ` · edited %s`, esc(m.EditedAt))
		}
		w.WriteString(`</span>`)
		if m.DeletedAt != "" {
			fmt.Fprintf(&w, ` <span class="deleted">deleted %s UTC</span>`, esc(m.DeletedAt))
		}
		for _, e := range m.Edits {
			fmt.Fprintf(&w, `<div class="old"><span class="note">Before the edit:</span>%s</div>`, esc(e.Content))
		}
		if m.Content != "" {
			fmt.Fprintf(&w, `<div class="text">%s</div>`, esc(m.Content))
		}
		for _, f := range m.Files {
			if f.Path == "" {
				fmt.Fprintf(&w, `<div class="note">📎 %s (not saved: %s)</div>`, esc(f.Filename), esc(f.Note))
				continue
			}
			kind := ticketMediaKind(f.ContentType)
			if kind != "" && f.Size*4/3 <= budget {
				if data, err := b.readTicketFile(f); err == nil {
					src := "data:" + f.ContentType + ";base64," + base64.StdEncoding.EncodeToString(data)
					budget -= int64(len(src))
					if kind == "img" {
						fmt.Fprintf(&w, `<img src="%s" alt="%s">`, src, esc(f.Filename))
					} else {
						fmt.Fprintf(&w, `<%s controls src="%s"></%s>`, kind, src, kind)
					}
					continue
				}
			}
			fmt.Fprintf(&w, `<div class="note">📎 %s (%s, saved on the bot server; open it in the web interface)</div>`,
				esc(f.Filename), humanBytes(f.Size))
		}
		w.WriteString(`</div>`)
	}
	w.WriteString(`</body></html>`)
	return w.Bytes()
}

// ticketMediaKind is the HTML element that shows a file type, or "".
func ticketMediaKind(contentType string) string {
	switch ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])); {
	case ct == "image/png", ct == "image/jpeg", ct == "image/gif", ct == "image/webp":
		return "img"
	case ct == "video/mp4", ct == "video/webm", ct == "video/quicktime":
		return "video"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	}
	return ""
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
