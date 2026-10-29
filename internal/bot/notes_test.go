package bot

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// runMessageMenu uses a message context menu command on msg.
func (tb *testBot) runMessageMenu(userID, name string, msg *discordgo.Message) {
	tb.t.Helper()
	tb.onInteraction(tb.s, tb.interaction(userID, discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{
		Name: name, CommandType: discordgo.MessageApplicationCommand, TargetID: msg.ID,
		Resolved: &discordgo.ApplicationCommandInteractionDataResolved{Messages: map[string]*discordgo.Message{msg.ID: msg}},
	}))
}

// runUserMenu uses a user context menu command on a fixture user.
func (tb *testBot) runUserMenu(userID, name, targetID string) {
	tb.t.Helper()
	m, err := tb.s.State.Member(tGuild, targetID)
	if err != nil {
		tb.t.Fatalf("no fixture member %s", targetID)
	}
	tb.onInteraction(tb.s, tb.interaction(userID, discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{
		Name: name, CommandType: discordgo.UserApplicationCommand, TargetID: targetID,
		Resolved: &discordgo.ApplicationCommandInteractionDataResolved{Users: map[string]*discordgo.User{targetID: m.User}},
	}))
}

func TestNoteAddListEditDelete(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tUser, "note add", userOpt("user", tTarget), strOpt("text", "hi"))
	tb.expect(true, "must be a moderator")

	tb.run(tMod, "note add", userOpt("user", tTarget), strOpt("text", "Was rude in #general."))
	tb.expect(true, "Note #1 added", "Was rude in #general.", "Added by <@"+tMod+">")
	if !contains(tb.sent(tChModLog), "Note Added") {
		t.Error("add not logged")
	}
	notes, _ := tb.db.UserNotes(tGuild, tTarget)
	if len(notes) != 1 || notes[0].AuthorID != tMod || notes[0].CreatedAt == "" {
		t.Fatalf("notes = %+v", notes)
	}

	// A user who is not in the server can have notes too.
	tb.run(tMod, "note add", userOpt("user", "999999"), strOpt("text", "left the server"))
	tb.expect(true, "Note #2 added")

	tb.run(tMod, "note list", userOpt("user", tTarget))
	r := tb.expect(true, "1 note(s)", "Was rude", "#1 ·")
	if strings.Contains(r.text(), "left the server") {
		t.Error("list shows another user's note")
	}

	// Select a note, then edit it.
	tb.click(tMod, tb.componentID(r, "note:sel:"+tTarget+":0"), "1")
	r = tb.expect(true, "Note #1", "About <@"+tTarget+">")
	tb.click(tMod, tb.componentID(r, "note:edit:1:"))
	if last := tb.replies()[len(tb.replies())-1]; last.Kind != "modal" {
		t.Fatalf("edit did not open a modal: %+v", last)
	}
	tb.submit(tMod, "notemodal:1:0", map[string]string{"text": ""})
	tb.expect(true, "needs some text")
	tb.submit(tAdmin, "notemodal:1:0", map[string]string{"text": "Was rude twice."})
	tb.expect(true, "Note #1 saved", "Was rude twice.", "edited by <@"+tAdmin+">")
	if n, _, _ := tb.db.UserNote(tGuild, 1); n.Note != "Was rude twice." || n.EditedBy != tAdmin || n.AuthorID != tMod {
		t.Errorf("note after edit = %+v", n)
	}

	// Members who are not mods cannot use the buttons.
	tb.click(tUser, "note:del:1:0")
	tb.expect(true, "must be a moderator")

	// Delete asks first; Cancel keeps the note.
	tb.click(tMod, "note:del:1:0")
	r = tb.expect(true, "Delete note #1?")
	tb.click(tMod, tb.componentID(r, "confirm:no:"))
	if _, ok, _ := tb.db.UserNote(tGuild, 1); !ok {
		t.Fatal("cancel deleted the note")
	}
	tb.click(tMod, "note:del:1:0")
	tb.click(tMod, tb.componentID(tb.last(), "confirm:yes:"))
	tb.expect(true, "Note #1 deleted", "No notes on record")
	if _, ok, _ := tb.db.UserNote(tGuild, 1); ok {
		t.Error("note not deleted")
	}
	if !contains(tb.sent(tChModLog), "Note Deleted") || !contains(tb.sent(tChModLog), "Note Edited") {
		t.Errorf("mod log = %v", tb.sent(tChModLog))
	}

	// A click on a deleted note says so.
	tb.click(tMod, "note:edit:1:0")
	tb.expect(true, "no longer exists")
}

func TestNoteFromMessage(t *testing.T) {
	tb := newTestBot(t)
	msg := &discordgo.Message{
		ID: "7777", ChannelID: tChGeneral, Content: "some bad words\nsecond line",
		Author:      &discordgo.User{ID: tTarget, Username: "target"},
		Timestamp:   time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Attachments: []*discordgo.MessageAttachment{{Filename: "proof.png"}},
	}
	tb.runMessageMenu(tUser, noteMenuAdd, msg)
	tb.expect(true, "must be a moderator")

	tb.runMessageMenu(tMod, noteMenuAdd, msg)
	r := tb.expect(true, "added for <@"+tTarget+">", "> some bad words", "> second line", "📎 proof.png",
		"/channels/"+tGuild+"/"+tChGeneral+"/7777", "<t:1788264000:f>")
	n, ok, _ := tb.db.UserNote(tGuild, 1)
	if !ok || n.UserID != tTarget || n.AuthorID != tMod || n.MessageID != "7777" || n.Note != "" {
		t.Fatalf("note = %+v", n)
	}

	// A comment keeps the saved message.
	id := tb.componentID(r, "note:edit:1:")
	if !strings.Contains(fmt.Sprint(r.Components), id) {
		t.Fatal("no edit button")
	}
	tb.submit(tMod, "notemodal:1:0", map[string]string{"text": "Second time this week."})
	tb.expect(true, "Second time this week.", "> some bad words")
	tb.submit(tMod, "notemodal:1:0", map[string]string{"text": ""})
	tb.expect(true, "Note #1 saved", "> some bad words")

	// Bots and webhooks are refused.
	tb.runMessageMenu(tMod, noteMenuAdd, &discordgo.Message{ID: "8", Author: &discordgo.User{ID: tBot, Bot: true}})
	tb.expect(true, "not a message from a bot")

	// Right-click on the member shows the notes.
	tb.runUserMenu(tMod, noteMenuView, tTarget)
	tb.expect(true, "1 note(s)", "> some bad words")
}

func TestNoteListPages(t *testing.T) {
	tb := newTestBot(t)
	for i := 1; i <= 7; i++ {
		if _, err := tb.db.AddUserNote(tGuild, tTarget, tMod, fmt.Sprintf("note number %d %s", i, strings.Repeat("x", maxNoteLen)), nil); err != nil {
			t.Fatal(err)
		}
	}
	tb.run(tMod, "note list", userOpt("user", tTarget))
	r := tb.expect(true, "7 note(s)", "note number 7", "Page 1/2")
	if strings.Contains(r.text(), "note number 2 ") {
		t.Error("page 1 shows an old note")
	}
	if e := r.Embeds[0]; len([]rune(embedText(e))) > 6000 {
		t.Errorf("page is %d characters", len([]rune(embedText(e))))
	}
	for _, f := range r.Embeds[0].Fields {
		if len([]rune(f.Value)) > 1024 {
			t.Errorf("field is %d characters", len([]rune(f.Value)))
		}
	}
	tb.click(tMod, tb.componentID(r, "note:list:"+tTarget+":1"))
	tb.expect(true, "note number 1", "Page 2/2")
}
