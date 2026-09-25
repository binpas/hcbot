package bot

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/config"
)

const tSupportMsg = "700"

func ticketBot(t *testing.T) *testBot {
	tb := newTestBot(t)
	tb.config(map[string]string{config.ModSupportMsgKey: tSupportMsg, "MOD_SUPPORT": tChSupport})
	return tb
}

// addChannel puts a text channel in the cache.
func (tb *testBot) addChannel(id, name, parent string) {
	tb.t.Helper()
	if err := tb.s.State.ChannelAdd(&discordgo.Channel{ID: id, GuildID: tGuild, Name: name,
		Type: discordgo.ChannelTypeGuildText, ParentID: parent}); err != nil {
		tb.t.Fatal(err)
	}
}

// ticket records a ticket and, when inState, caches its channel.
func (tb *testBot) ticket(channelID, userID, username string, number int, status string, inState bool) {
	tb.t.Helper()
	if _, err := tb.db.CreateTicket(tGuild, channelID, userID, username, number); err != nil {
		tb.t.Fatal(err)
	}
	if status != "open" {
		tb.sqlExec(`UPDATE tickets SET status=?, closed_at=datetime('now') WHERE channel_id=?`, status, channelID)
	}
	if inState {
		tb.addChannel(channelID, fmt.Sprintf("open-ticket-%s-%d", username, number), tCatTickets)
	}
}

type channelBody struct {
	Name                 string                           `json:"name"`
	Type                 int                              `json:"type"`
	ParentID             string                           `json:"parent_id"`
	PermissionOverwrites []*discordgo.PermissionOverwrite `json:"permission_overwrites"`
}

func channelIn(c apiCall) channelBody {
	var b channelBody
	_ = json.Unmarshal(c.Body, &b)
	return b
}

func overwriteFor(ows []*discordgo.PermissionOverwrite, id string) *discordgo.PermissionOverwrite {
	for _, o := range ows {
		if o.ID == id {
			return o
		}
	}
	return nil
}

func TestSanitiseUsername(t *testing.T) {
	cases := map[string]string{
		"Alice":                          "alice",
		"bob the builder":                "bob-the-builder",
		"ünï_code!!":                     "ncode",
		"":                               "user",
		"___":                            "user",
		"abcdefghijklmnopqrstuvwxyz0123": "abcdefghijklmnopqrstuvwx",
	}
	for in, want := range cases {
		if got := sanitiseUsername(in); got != want {
			t.Errorf("sanitiseUsername(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTicketID(t *testing.T) {
	good := map[string]struct {
		name string
		n    int
	}{"alice-2": {"alice", 2}, " bob-the-builder-10 ": {"bob-the-builder", 10}, "alice--3": {"alice-", 3}}
	for in, want := range good {
		if name, n, ok := parseTicketID(in); !ok || name != want.name || n != want.n {
			t.Errorf("parseTicketID(%q) = %q, %d, %v", in, name, n, ok)
		}
	}
	for _, in := range []string{"alice", "-2", "alice-", "alice-x", "alice-2a"} {
		if name, n, ok := parseTicketID(in); ok {
			t.Errorf("parseTicketID(%q) = %q, %d, ok", in, name, n)
		}
	}
}

func TestTicketOpenByReaction(t *testing.T) {
	tb := ticketBot(t)
	tb.react(tTarget, tChSupport, tSupportMsg, ticketOpenEmoji)

	tb.expectCall(1, "DELETE", "/channels/"+tChSupport+"/messages/"+tSupportMsg+"/reactions/.*/"+tTarget)
	create := tb.expectCall(1, "POST", "/guilds/100/channels")
	if len(create) != 1 {
		t.FailNow()
	}
	ch := channelIn(create[0])
	if ch.Name != "open-ticket-target-1" || ch.ParentID != tCatTickets || ch.Type != int(discordgo.ChannelTypeGuildText) {
		t.Errorf("channel = %+v", ch)
	}
	view := int64(discordgo.PermissionViewChannel)
	checks := []struct {
		id          string
		allow, deny int64
	}{
		{tGuild, 0, view},
		{tTarget, view | discordgo.PermissionSendMessages | discordgo.PermissionAttachFiles, 0},
		{tBot, view | discordgo.PermissionManageChannels, 0},
		{tRoleMod, view | discordgo.PermissionManageMessages, 0},
	}
	for _, c := range checks {
		o := overwriteFor(ch.PermissionOverwrites, c.id)
		if o == nil || o.Allow&c.allow != c.allow || o.Deny&c.deny != c.deny {
			t.Errorf("overwrite for %s = %+v", c.id, o)
		}
	}
	if create[0].Reason != "Support ticket for target" {
		t.Errorf("audit reason = %q", create[0].Reason)
	}

	open, _ := tb.db.OpenTicket(tGuild, tTarget)
	if open == nil || open.ChannelID != "601" || open.Number != 1 {
		t.Fatalf("ticket = %+v", open)
	}
	if w := tb.sent("601"); len(w) != 1 || !strings.Contains(w[0], userMention(tTarget)) ||
		!strings.Contains(w[0], roleMention(tRoleMod)) || !strings.Contains(w[0], ticketCloseEmoji) {
		t.Errorf("welcome = %v", w)
	}
	tb.expectCall(1, "PUT", "/channels/601/messages/8000/reactions/.*/@me")
	if !contains(tb.sent(tChModLog), "Ticket Opened") {
		t.Error("not logged")
	}
}

func TestTicketOpenExisting(t *testing.T) {
	tb := ticketBot(t)
	tb.ticket("650", tTarget, "target", 1, "open", true)
	tb.react(tTarget, tChSupport, tSupportMsg, ticketOpenEmoji)
	tb.expectCall(0, "POST", "/guilds/100/channels")
	if dm := tb.sent("7000"); len(dm) != 1 || !strings.Contains(dm[0], "already have an open ticket: "+jumpURL(tGuild, "650")) {
		t.Errorf("DM = %v", dm)
	}

	// When the old ticket's channel is gone, a new ticket is opened.
	tb.api.reset()
	tb.ticket("651", tUser, "user", 1, "open", false)
	tb.react(tUser, tChSupport, tSupportMsg, ticketOpenEmoji)
	if create := tb.expectCall(1, "POST", "/guilds/100/channels"); len(create) == 1 && channelIn(create[0]).Name != "open-ticket-user-2" {
		t.Errorf("new ticket name = %q", channelIn(create[0]).Name)
	}
	if old, _ := tb.db.TicketByChannel("651"); old.Status != "closed" {
		t.Errorf("old ticket status = %q", old.Status)
	}
}

func TestTicketOpenIgnoredAndFailure(t *testing.T) {
	tb := ticketBot(t)
	tb.react(tTarget, tChSupport, "999", ticketOpenEmoji) // another message
	tb.react(tTarget, tChSupport, tSupportMsg, "👍")       // another emoji
	tb.react(tBot, tChSupport, tSupportMsg, ticketOpenEmoji)
	m, _ := tb.s.State.Member(tGuild, tUser)
	m.User.Bot = true
	tb.react(tUser, tChSupport, tSupportMsg, ticketOpenEmoji)
	m.User.Bot = false
	tb.env.SetupMode = true
	tb.react(tUser, tChSupport, tSupportMsg, ticketOpenEmoji)
	tb.env.SetupMode = false
	if calls := tb.callList(); len(calls) != 0 {
		t.Errorf("unexpected calls: %v", calls)
	}

	tb.config(map[string]string{"TICKET_CAT": ""})
	tb.react(tTarget, tChSupport, tSupportMsg, ticketOpenEmoji)
	tb.expectCall(0, "POST", "/guilds/100/channels")
	if dm := tb.sent("7000"); len(dm) != 1 || !strings.Contains(dm[0], "couldn't create your ticket") {
		t.Errorf("DM = %v", dm)
	}

	tb.config(map[string]string{"TICKET_CAT": tCatTickets})
	tb.api.fail("POST", "/guilds/100/channels")
	tb.react(tTarget, tChSupport, tSupportMsg, ticketOpenEmoji)
	if open, _ := tb.db.OpenTicket(tGuild, tTarget); open != nil {
		t.Error("ticket saved although the channel was not created")
	}
}

func TestTicketCloseByReaction(t *testing.T) {
	tb := ticketBot(t)
	tb.ticket("650", tTarget, "target", 1, "open", true)
	tb.react(tTarget, "650", "123", ticketCloseEmoji)

	tb.expectCall(1, "DELETE", "/channels/650/permissions/"+tTarget)
	if edit := tb.expectCall(1, "PATCH", "/channels/650"); len(edit) == 1 && channelIn(edit[0]).Name != "closed-ticket-target-1" {
		t.Errorf("rename = %s", edit[0].Body)
	}
	if !contains(tb.sent("650"), "Ticket Closed") || !contains(tb.sent(tChModLog), "closed-ticket-target-1") {
		t.Error("close not announced")
	}
	if tk, _ := tb.db.TicketByChannel("650"); tk.Status != "closed" || tk.ClosedAt == "" {
		t.Errorf("ticket = %+v", tk)
	}

	tb.api.reset()
	tb.react(tTarget, "650", "123", ticketCloseEmoji)
	tb.react(tTarget, tChGeneral, "123", ticketCloseEmoji)
	if calls := tb.callList(); len(calls) != 0 {
		t.Errorf("closing twice or outside a ticket made calls: %v", calls)
	}
}

func TestTicketCloseCommand(t *testing.T) {
	tb := ticketBot(t)
	tb.runIn(tChGeneral, tMod, "ticket close")
	tb.expect(true, "isn't a ticket")

	tb.ticket("650", tTarget, "target", 1, "open", true)
	tb.runIn("650", tUser, "ticket close")
	tb.expect(true, "must be a moderator")

	tb.runIn("650", tMod, "ticket close")
	tb.expect(true, "Ticket closed.")
	tb.expectCall(1, "DELETE", "/channels/650/permissions/"+tTarget)

	tb.runIn("650", tMod, "ticket close")
	tb.expect(true, "already closed")
}

func TestTicketArchive(t *testing.T) {
	tb := ticketBot(t)
	tb.ticket("650", tTarget, "target", 3, "open", true)
	tb.runIn("650", tMod, "ticket archive")
	tb.expect(true, "Close the ticket first")

	tb.sqlExec(`UPDATE tickets SET status='closed' WHERE channel_id='650'`)
	tb.runIn("650", tMod, "ticket archive")
	tb.expect(true, "Moved to **Ticket Archive 1** as `target-3`")

	cat := tb.expectCall(1, "POST", "/guilds/100/channels")
	if len(cat) != 1 {
		t.FailNow()
	}
	cb := channelIn(cat[0])
	if cb.Name != "Ticket Archive 1" || cb.Type != int(discordgo.ChannelTypeGuildCategory) ||
		overwriteFor(cb.PermissionOverwrites, tGuild) == nil || overwriteFor(cb.PermissionOverwrites, tRoleMod) == nil {
		t.Errorf("category = %+v", cb)
	}
	if edit := tb.expectCall(1, "PATCH", "/channels/650"); len(edit) == 1 {
		e := channelIn(edit[0])
		if e.Name != "target-3" || e.ParentID != "601" || len(e.PermissionOverwrites) != 2 ||
			overwriteFor(e.PermissionOverwrites, tTarget) != nil {
			t.Errorf("move = %+v", e)
		}
	}
	if tk, _ := tb.db.TicketByChannel("650"); tk.Status != "archived" {
		t.Errorf("status = %q", tk.Status)
	}
	if !contains(tb.sent(tChModLog), "Ticket Archived") {
		t.Error("not logged")
	}

	tb.api.fail("PATCH", "/channels/.*")
	tb.ticket("651", tTarget, "target", 4, "closed", true)
	tb.runIn("651", tMod, "ticket archive")
	tb.expect(true, "don't have permission to move/rename")
}

func TestRolloverCategory(t *testing.T) {
	tb := ticketBot(t)
	if err := tb.s.State.ChannelAdd(&discordgo.Channel{ID: "800", GuildID: tGuild, Name: "Ticket Archive 1",
		Type: discordgo.ChannelTypeGuildCategory}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxChannelsPerCategory-1; i++ {
		tb.addChannel(fmt.Sprint(810+i), "old", "800")
	}
	if cat := tb.rolloverCategory(tGuild, "Ticket Archive", nil); cat == nil || cat.ID != "800" {
		t.Fatalf("with room: %+v", cat)
	}
	tb.expectCall(0, "POST", "/guilds/100/channels")

	tb.addChannel("899", "old", "800") // now full
	if cat := tb.rolloverCategory(tGuild, "Ticket Archive", nil); cat == nil || cat.Name != "Ticket Archive 2" {
		t.Fatalf("when full: %+v", cat)
	}
}

func TestTicketArchiveAll(t *testing.T) {
	tb := ticketBot(t)
	tb.run(tMod, "ticket archiveall")
	tb.expect(true, "no closed tickets")

	tb.ticket("650", tTarget, "target", 1, "closed", true)
	tb.ticket("651", tUser, "user", 1, "closed", true)
	tb.ticket("652", tVIPUser, "vipuser", 1, "closed", false) // channel deleted
	tb.ticket("653", tMod, "moddy", 1, "open", true)
	tb.api.reset()
	tb.run(tMod, "ticket archiveall")
	rs := tb.replies()
	if len(rs) != 2 || !strings.Contains(rs[0].text(), "Processing **3**") {
		t.Fatalf("replies = %+v", rs)
	}
	tb.expect(true, "Archived **2** ticket(s). Skipped **1**.")
	if !contains(tb.sent(tChModLog), "Archive Complete") {
		t.Error("summary not logged")
	}
	tb.expectCall(1, "PATCH", "/channels/650")
	tb.expectCall(1, "PATCH", "/channels/651")
	tb.expectCall(0, "PATCH", "/channels/653")
}

func TestTicketList(t *testing.T) {
	tb := ticketBot(t)
	tb.run(tMod, "ticket list", userOpt("member", tTarget))
	tb.expect(true, "No tickets on record")

	tb.ticket("650", tTarget, "target", 3, "open", true)
	tb.ticket("651", tTarget, "target", 2, "closed", false)
	tb.sqlExec(`INSERT INTO ticket_archive (guild_id, user_id, username, channel_name, number, opened_at, transcript)
		VALUES (?, ?, 'target', 'x', 1, '2020-01-01', '')`, tGuild, tTarget)
	tb.run(tMod, "ticket list", userOpt("member", tTarget))
	r := tb.expect(true, "**3** ticket(s)", "🟢 Ticket #3 — open", channelMention("650"),
		"🔴 Ticket #2 — closed", "*channel deleted*", "📦 Ticket #1 — archived", "check /ticket notes")
	if f := r.Embeds[0].Fields; len(f) != 3 || !strings.Contains(f[0].Name, "#3") || !strings.Contains(f[2].Name, "#1") {
		t.Errorf("order wrong: %v", f)
	}
}

func TestTicketNotes(t *testing.T) {
	tb := ticketBot(t)
	tb.ticket("650", tTarget, "target", 2, "open", false)
	tb.sqlExec(`INSERT INTO ticket_archive (guild_id, user_id, username, channel_name, number, transcript)
		VALUES (?, ?, 'old-user', 'x', 1, '')`, tGuild, tUser)

	tb.run(tMod, "ticket note", strOpt("ticket_id", "target"), strOpt("note", "x"))
	tb.expect(true, "Usage: `/ticket note")
	tb.run(tMod, "ticket note", strOpt("ticket_id", "target-9"), strOpt("note", "x"))
	tb.expect(true, "No ticket found for `target-9`")

	tb.run(tMod, "ticket notes", strOpt("ticket_id", "target-2"))
	tb.expect(true, "No notes recorded yet")

	tb.run(tMod, "ticket note", strOpt("ticket_id", "target-2"), strOpt("note", "Resolved via DM"))
	tb.expect(true, "Note added to `target-2`")
	tb.run(tMod, "ticket note", strOpt("ticket_id", "old-user-1"), strOpt("note", "legacy DM thing"))
	tb.expect(true, "Note added to `old-user-1`")

	tb.run(tMod, "ticket notes", strOpt("ticket_id", "target-2"))
	tb.expect(true, "Notes — target-2", "Resolved via DM", "by "+userMention(tMod))

	tb.run(tMod, "ticket search", strOpt("query", "dm"))
	tb.expect(true, "**2** match(es)", "target-2", "old-user-1")
	tb.run(tMod, "ticket search", strOpt("query", "nothing like this"))
	tb.expect(true, "No ticket notes matched")
}
