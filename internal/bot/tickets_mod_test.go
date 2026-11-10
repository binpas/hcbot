package bot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/db"
)

// submitUsers sends a modal with one user select, the way Discord does (in
// a Label), as a raw gateway event.
func (tb *testBot) submitUsers(userID, customID, selectID string, ids ...string) {
	tb.t.Helper()
	tb.seq++
	member, _ := json.Marshal(tb.invoker(userID))
	raw, _ := json.Marshal(map[string]any{
		"id": fmt.Sprint(10000 + tb.seq), "application_id": tBot, "type": 5, "token": fmt.Sprintf("tok%d", tb.seq),
		"guild_id": tGuild, "channel_id": tChGeneral, "member": json.RawMessage(member),
		"data": map[string]any{"custom_id": customID, "components": []any{map[string]any{"type": 18,
			"component": map[string]any{"type": 5, "custom_id": selectID, "values": append([]string{}, ids...)}}}},
	})
	tb.onRawInteraction(tb.s, &discordgo.Event{Type: "INTERACTION_CREATE", RawData: raw})
}

// modTicket records a mod ticket with its users and caches its channel.
func (tb *testBot) modTicket(channelID string, number int, users ...string) *db.Ticket {
	tb.t.Helper()
	var members []db.TicketMember
	for _, u := range users {
		members = append(members, db.TicketMember{UserID: u, Username: "user" + u})
	}
	if _, err := tb.db.CreateModTicket(tGuild, channelID, tMod, number, members); err != nil {
		tb.t.Fatal(err)
	}
	tb.addChannel(channelID, fmt.Sprintf("open-ticket-mod-%d", number), tCatTickets)
	t, _ := tb.db.TicketByChannel(channelID)
	return t
}

func TestModTicketOpen(t *testing.T) {
	tb := ticketBot(t)
	tb.run(tUser, "ticket open")
	tb.expect(true, "must be a moderator")

	tb.api.reset()
	tb.run(tMod, "ticket open")
	if cb := tb.expectCall(1, "POST", "/interactions/.*/callback"); len(cb) == 1 {
		body := string(cb[0].Body)
		if !strings.Contains(body, `"custom_id":"ticketopen:new"`) || !strings.Contains(body, `"type":5`) ||
			!strings.Contains(body, `"max_values":10`) {
			t.Errorf("modal = %s", body)
		}
	}

	tb.api.reset()
	tb.submitUsers(tMod, "ticketopen:new", "users", tTarget, tUser, tBot)
	create := tb.expectCall(1, "POST", "/guilds/100/channels")
	if len(create) != 1 {
		t.FailNow()
	}
	ch := channelIn(create[0])
	if ch.Name != "open-ticket-mod-1" || ch.ParentID != tCatTickets {
		t.Errorf("channel = %+v", ch)
	}
	view := int64(discordgo.PermissionViewChannel)
	for _, id := range []string{tTarget, tUser, tBot, tRoleMod} {
		if o := overwriteFor(ch.PermissionOverwrites, id); o == nil || o.Allow&view == 0 {
			t.Errorf("overwrite for %s = %+v", id, o)
		}
	}
	if o := overwriteFor(ch.PermissionOverwrites, tGuild); o == nil || o.Deny&view == 0 {
		t.Errorf("everyone = %+v", o)
	}
	tb.expect(true, "Opened", "`mod-1`", userMention(tTarget), userMention(tUser), "Not added", "(a bot)")
	tk, _ := tb.db.TicketByChannel("601")
	if tk == nil || tk.Kind != "mod" || tk.Ref() != "mod-1" || tk.OpenedBy != tMod {
		t.Fatalf("ticket = %+v", tk)
	}
	if members, _ := tb.db.TicketMembers(tk.ID); len(members) != 2 {
		t.Errorf("members = %+v", members)
	}
	if w := tb.sent("601"); len(w) != 1 || !strings.Contains(w[0], userMention(tTarget)) || !strings.Contains(w[0], userMention(tMod)) {
		t.Errorf("welcome = %v", w)
	}
	if !contains(tb.sent(tChModLog), "Mod Ticket Opened") {
		t.Error("not logged")
	}

	// The numbers of mod tickets count up on their own.
	tb.api.reset()
	tb.submitUsers(tMod, "ticketopen:new", "users", tTarget)
	if create := tb.expectCall(1, "POST", "/guilds/100/channels"); len(create) == 1 && channelIn(create[0]).Name != "open-ticket-mod-2" {
		t.Errorf("second name = %q", channelIn(create[0]).Name)
	}

	// Only bots: nothing is opened.
	tb.api.reset()
	tb.submitUsers(tMod, "ticketopen:new", "users", tBot)
	tb.expect(true, "could not add any")
	tb.expectCall(0, "POST", "/guilds/100/channels")

	tb.config(map[string]string{"TICKET_CAT": ""})
	tb.run(tMod, "ticket open")
	tb.expect(true, "Set the ticket category first")
}

func TestModTicketClose(t *testing.T) {
	tb := ticketBot(t)
	tb.modTicket("660", 1, tTarget, tUser)

	tb.react(tTarget, "660", "123", ticketCloseEmoji) // not a mod
	tb.expectCall(0, "PATCH", "/channels/660")

	tb.react(tMod, "660", "123", ticketCloseEmoji)
	tb.expectCall(1, "DELETE", "/channels/660/permissions/"+tTarget)
	tb.expectCall(1, "DELETE", "/channels/660/permissions/"+tUser)
	if edit := tb.expectCall(1, "PATCH", "/channels/660"); len(edit) == 1 && channelIn(edit[0]).Name != "closed-ticket-mod-1" {
		t.Errorf("rename = %s", edit[0].Body)
	}
	if !contains(tb.sent("660"), "added users no longer have access") || !contains(tb.sent(tChModLog), "a mod ticket") {
		t.Error("close not announced")
	}
}

// ticketMessage sends a message with attachments through the real handler.
func (tb *testBot) ticketMessage(id, channelID, userID, text string, files ...*discordgo.MessageAttachment) {
	tb.t.Helper()
	m, _ := tb.s.State.Member(tGuild, userID)
	tb.onMessageCreate(tb.s, &discordgo.MessageCreate{Message: &discordgo.Message{ID: id, ChannelID: channelID,
		GuildID: tGuild, Author: m.User, Content: text, Attachments: files,
		Timestamp: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}})
}

// cdnFile is an attachment that the fake CDN serves.
func (tb *testBot) cdnFile(id, name, contentType, data string) *discordgo.MessageAttachment {
	tb.api.on("GET", "/attachments/2/"+id+"/"+name, 200, data)
	return &discordgo.MessageAttachment{ID: id, Filename: name, ContentType: contentType, Size: len(data),
		URL: "https://cdn.discordapp.com/attachments/2/" + id + "/" + name}
}

func TestTicketMessageLog(t *testing.T) {
	tb := transcriptBot(t)
	tb.ticket("650", tTarget, "target", 1, "open", true)
	tk, _ := tb.db.TicketByChannel("650")

	pic := tb.cdnFile("31", "pic.png", "image/png", "PNGDATA")
	big := &discordgo.MessageAttachment{ID: "32", Filename: "huge.zip", Size: 30 << 20,
		URL: "https://cdn.discordapp.com/attachments/2/32/huge.zip"}
	tb.ticketMessage("20100", "650", tTarget, "hello, this is my problem", pic, big)
	tb.ticketMessage("20101", tChGeneral, tTarget, "not in a ticket")

	edited := time.Date(2026, 11, 1, 12, 5, 0, 0, time.UTC)
	m, _ := tb.s.State.Member(tGuild, tTarget)
	tb.onTicketMessageUpdate(tb.s, &discordgo.MessageUpdate{Message: &discordgo.Message{ID: "20100", ChannelID: "650",
		GuildID: tGuild, Author: m.User, Content: "hello, this is my new problem", EditedTimestamp: &edited}})
	tb.ticketMessage("20102", "650", tTarget, "oops")
	tb.onTicketMessageDelete(tb.s, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "20102", ChannelID: "650", GuildID: tGuild}})

	msgs, err := tb.db.TicketTranscript(tk.ID)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("transcript = %+v, %v", msgs, err)
	}
	first := msgs[0]
	if first.Content != "hello, this is my new problem" || len(first.Edits) != 1 ||
		first.Edits[0].Content != "hello, this is my problem" || first.EditedAt != "2026-11-01 12:05:00" {
		t.Errorf("first = %+v", first)
	}
	if len(first.Files) != 2 || first.Files[0].Path == "" || first.Files[1].Path != "" ||
		!strings.Contains(first.Files[1].Note, "Larger than 25 MB") {
		t.Fatalf("files = %+v", first.Files)
	}
	data, err := os.ReadFile(filepath.Join(tb.env.TicketFilesDir, first.Files[0].Path))
	if err != nil || string(data) != "PNGDATA" {
		t.Errorf("saved file = %q, %v", data, err)
	}
	if msgs[1].Content != "oops" || msgs[1].DeletedAt == "" {
		t.Errorf("deleted = %+v", msgs[1])
	}

	// The transcript file.
	tb.run(tMod, "ticket transcript", strOpt("ticket_id", "target-1"))
	r := tb.expect(true, "Transcript — target-1", "**2** message(s) and **2** file(s)", "not archived yet")
	_ = r
	calls := tb.api.find("PATCH", "/webhooks/.*")
	if len(calls) == 0 {
		t.Fatal("no transcript file")
	}
	page := string(calls[len(calls)-1].Files["target-1.html"])
	for _, want := range []string{"hello, this is my new problem", "Before the edit:", "hello, this is my problem",
		"data:image/png;base64,UE5HREFUQQ==", "huge.zip (not saved", "deleted 20", "oops"} {
		if !strings.Contains(page, want) {
			t.Errorf("page has no %q", want)
		}
	}

	// Search finds the text now and before the edit.
	tb.run(tMod, "ticket search", strOpt("query", "MY PROBLEM"))
	tb.expect(true, "**1** message(s)", "target-1 — target")

	// An archived ticket is not logged any more.
	tb.sqlExec(`UPDATE tickets SET status='archived', archived_at=datetime('now') WHERE id=?`, tk.ID)
	tb.ticketMessage("20103", "650", tTarget, "after the archive")
	if msgs, _ := tb.db.TicketTranscript(tk.ID); len(msgs) != 2 {
		t.Errorf("logged after the archive: %d messages", len(msgs))
	}
}

func TestTicketMessageLogNeedsMessageContent(t *testing.T) {
	tb := ticketBot(t)
	tb.ticket("650", tTarget, "target", 1, "open", true)
	tb.ticketMessage("20100", "650", tTarget, "hello")
	tk, _ := tb.db.TicketByChannel("650")
	if msgs, _ := tb.db.TicketTranscript(tk.ID); len(msgs) != 0 {
		t.Errorf("logged without the intent: %+v", msgs)
	}
}

func TestTicketTranscriptLegacy(t *testing.T) {
	tb := transcriptBot(t)
	tb.sqlExec(`INSERT INTO ticket_archive (guild_id, user_id, username, channel_name, number, message_count, transcript)
		VALUES (?, ?, 'old-user', 'x', 1, 3, 'line one\nline two')`, tGuild, tUser)
	tb.run(tMod, "ticket transcript", strOpt("ticket_id", "old-user-1"))
	tb.expect(true, "older transcript", "**3** message(s)")
	calls := tb.api.find("PATCH", "/webhooks/.*")
	if len(calls) == 0 || !strings.Contains(string(calls[len(calls)-1].Files["old-user-1.txt"]), "line two") {
		t.Error("no legacy transcript file")
	}
	tb.run(tMod, "ticket search", strOpt("query", "line one"))
	tb.expect(true, "old-user-1 — older transcript")
	tb.run(tMod, "ticket transcript", strOpt("ticket_id", "nobody-1"))
	tb.expect(true, "No ticket found")
}

func TestTicketListModTickets(t *testing.T) {
	tb := ticketBot(t)
	tb.modTicket("660", 4, tTarget)
	tb.run(tMod, "ticket list", userOpt("member", tTarget))
	tb.expect(true, "🟢 mod-4 — open (mod ticket)", channelMention("660"))
	tb.run(tMod, "ticket list", userOpt("member", tUser))
	tb.expect(true, "No tickets on record")

	tb.run(tMod, "ticket note", strOpt("ticket_id", "mod-4"), strOpt("note", "talked it out"))
	tb.expect(true, "Note added to `mod-4`")
	tb.run(tMod, "ticket notes", strOpt("ticket_id", "mod-4"))
	tb.expect(true, "talked it out")
}

func TestTicketConvertOld(t *testing.T) {
	tb := transcriptBot(t)
	tb.ticket("670", tTarget, "target", 1, "archived", true)
	tb.ticket("671", tUser, "user", 1, "archived", false) // its channel is gone
	tb.run(tMod, "ticket convertold")
	tb.expect(true, "don't have permission")

	tb.api.on("GET", "/channels/670/messages", 200, `[{"id":"20600","channel_id":"670",
		"author":{"id":"300","username":"target"},"content":"an old problem","timestamp":"2025-01-01T10:00:00Z"}]`)
	tb.api.on("GET", "/guilds/100/channels", 200, `[{"id":"800","type":4,"name":"Ticket Archive 1"},
		{"id":"801","type":4,"name":"Ticket Archive 2"},{"id":"802","type":0,"name":"x","parent_id":"801"},
		{"id":"803","type":4,"name":"Other"}]`)
	tb.run(tAdmin, "ticket convertold")
	yes := tb.componentID(tb.expect(true, "**2** ticket(s) were archived the old way"), "confirm:yes:ticketconvert:")
	tb.click(tAdmin, yes)
	tb.expect(true, "Converted **1** ticket(s)", "Deleted **1** empty", "**1** ticket(s) had no channel")
	tb.expectCall(1, "DELETE", "/channels/670")
	tb.expectCall(1, "DELETE", "/channels/800")
	tb.expectCall(0, "DELETE", "/channels/801")
	tb.expectCall(0, "DELETE", "/channels/803")
	tk, _ := tb.db.TicketByChannel("670")
	if tk.ArchivedAt == "" {
		t.Errorf("ticket = %+v", tk)
	}
	if msgs, _ := tb.db.TicketTranscript(tk.ID); len(msgs) != 1 || msgs[0].Content != "an old problem" {
		t.Errorf("transcript = %+v", msgs)
	}
	if moved, _ := tb.db.MovedTickets(tGuild); len(moved) != 1 {
		t.Errorf("moved = %d, want only the one without a channel", len(moved))
	}
}
