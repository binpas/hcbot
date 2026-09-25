package bot

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

const tChBB = "510" // BIG_BROTHER

func auditBot(t *testing.T) *testBot {
	tb := newTestBot(t)
	tb.config(map[string]string{"BIG_BROTHER": tChBB})
	return tb
}

// logged returns the text of every BIG_BROTHER entry.
func (tb *testBot) logged() []string { return tb.sent(tChBB) }

// expectLog checks that exactly n entries were logged and that the last
// one contains every want string.
func (tb *testBot) expectLog(n int, want ...string) {
	tb.t.Helper()
	got := tb.logged()
	if len(got) != n {
		tb.t.Fatalf("%d log entries, want %d: %q", len(got), n, got)
	}
	if n == 0 {
		return
	}
	for _, w := range want {
		if !strings.Contains(got[n-1], w) {
			tb.t.Errorf("log entry %q does not contain %q", got[n-1], w)
		}
	}
}

func (tb *testBot) edit(id, content string, editedAt time.Time) {
	tb.onMessageUpdate(tb.s, &discordgo.MessageUpdate{Message: &discordgo.Message{
		ID: id, ChannelID: tChGeneral, GuildID: tGuild, Content: content, EditedTimestamp: &editedAt}})
}

func (tb *testBot) del(id string) {
	tb.onMessageDelete(tb.s, &discordgo.MessageDelete{Message: &discordgo.Message{ID: id, ChannelID: tChGeneral, GuildID: tGuild}})
}

// lastMessageID is the ID tb.say gave the last message.
func (tb *testBot) lastMessageID() string { return fmt.Sprint(20000 + tb.seq) }

func TestAuditMessagesWithoutContent(t *testing.T) {
	tb := auditBot(t)
	tb.say(tUser, tChGeneral, "hello", false)
	id := tb.lastMessageID()
	now := time.Now()

	tb.edit(id, "hello edited", now)
	tb.expectLog(1, "Message Edited", userMention(tUser), "Jump to message", "Message ID: "+id)
	if strings.Contains(tb.logged()[0], "Before") {
		t.Error("before/after text shown without ENABLE_MESSAGE_CONTENT_FEATURES")
	}
	tb.edit(id, "link preview update", now) // same edited_at: not a real edit
	tb.edit("999", "not cached", now)
	tb.expectLog(1)

	tb.del(id)
	tb.expectLog(2, "Message Deleted", userMention(tUser))
	if strings.Contains(tb.logged()[1], "Content") {
		t.Error("content shown without ENABLE_MESSAGE_CONTENT_FEATURES")
	}

	// A message from before the cache: only the time and the channel.
	tb.del("1552686270256390274")
	tb.expectLog(3, "Old Message Deleted", "<t:", channelMention(tChGeneral), "Author unknown")

	// Bot messages are not logged.
	tb.say(tBot, tChGeneral, "bot message", false)
	tb.del(tb.lastMessageID())
	tb.expectLog(3)
}

func TestAuditMessagesWithContent(t *testing.T) {
	tb := auditBot(t)
	tb.env.MessageContent = true
	tb.env.MediaCacheSize, tb.env.MediaCacheMaxBytes = 10, 100
	tb.api.on("GET", "/attachments/pic.png", 200, "PNGDATA")
	tb.api.on("GET", "/attachments/big.bin", 200, strings.Repeat("x", 200))

	send := func(content string, attachments ...*discordgo.MessageAttachment) string {
		m, _ := tb.s.State.Member(tGuild, tUser)
		tb.seq++
		id := fmt.Sprint(20000 + tb.seq)
		tb.onMessageCreate(tb.s, &discordgo.MessageCreate{Message: &discordgo.Message{ID: id,
			ChannelID: tChGeneral, GuildID: tGuild, Author: m.User, Content: content, Attachments: attachments,
			Timestamp: time.Now()}})
		return id
	}
	waitMedia := func(id string) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			tb.media.mu.Lock()
			_, ok := tb.media.items[id]
			tb.media.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("media for %s not cached", id)
	}

	id := send("first")
	tb.edit(id, "second", time.Now())
	tb.expectLog(1, "Before\nfirst", "After\nsecond")
	tb.del(id)
	tb.expectLog(2, "Content\nsecond")

	withPic := send("", &discordgo.MessageAttachment{Filename: "pic.png", Size: 7, URL: "https://cdn.discordapp.com/attachments/pic.png"})
	waitMedia(withPic)
	tb.del(withPic)
	tb.expectLog(3, "*empty / attachment only*", "1 file(s) recovered")
	posts := tb.api.find("POST", "/channels/"+tChBB+"/messages")
	if f := posts[len(posts)-1].Files["pic.png"]; string(f) != "PNGDATA" {
		t.Errorf("uploaded file = %q", f)
	}

	tooBig := send("big", &discordgo.MessageAttachment{Filename: "big.bin", Size: 200, URL: "https://cdn.discordapp.com/attachments/big.bin"})
	time.Sleep(50 * time.Millisecond)
	tb.del(tooBig)
	tb.expectLog(4, "Attachments (not buffered)", "big.bin")

	// When the upload is refused, the entry is posted without the files.
	again := send("", &discordgo.MessageAttachment{Filename: "pic.png", Size: 7, URL: "https://cdn.discordapp.com/attachments/pic.png"})
	waitMedia(again)
	tb.api.onBody("POST", "/channels/"+tChBB+"/messages", "recovered", 1, 413, `{"message":"Request entity too large","code":40005}`)
	tb.del(again)
	posts = tb.api.find("POST", "/channels/"+tChBB+"/messages")
	if last := posts[len(posts)-1]; len(last.Files) != 0 || !strings.Contains(string(last.Body), "recovered") {
		t.Errorf("fallback entry = %s files %d", last.Body, len(last.Files))
	}
}

func TestMediaCacheLimits(t *testing.T) {
	c := newMediaCache()
	file := func(n int) []cachedFile { return []cachedFile{{"f", make([]byte, n)}} }
	c.put("a", file(40), 3, 100)
	c.put("b", file(40), 3, 100)
	c.put("c", file(40), 3, 100) // 120 > 100: "a" goes
	if c.pop("a") != nil || c.pop("b") == nil || c.total != 40 {
		t.Errorf("total limit: total=%d", c.total)
	}
	c.put("d", file(10), 2, 100)
	c.put("e", file(10), 2, 100) // count limit 2: "c" goes
	if c.pop("c") != nil || c.pop("e") == nil {
		t.Error("count limit not applied")
	}
	c.put("huge", file(101), 3, 100) // larger than the total: not kept
	if c.pop("huge") != nil {
		t.Error("kept a file larger than the total limit")
	}
}

func TestAuditBulkDelete(t *testing.T) {
	tb := auditBot(t)
	tb.say(tUser, tChGeneral, "one", false)
	a := tb.lastMessageID()
	tb.say(tMod, tChGeneral, "two", false)
	b := tb.lastMessageID()
	bulk := &discordgo.MessageDeleteBulk{Messages: []string{a, b, "77"}, ChannelID: tChGeneral, GuildID: tGuild}

	tb.onMessageDeleteBulk(tb.s, bulk)
	tb.expectLog(1, "**3** messages deleted in "+channelMention(tChGeneral))
	if posts := tb.api.find("POST", "/channels/"+tChBB+"/messages"); len(posts[0].Files) != 0 {
		t.Error("transcript sent without ENABLE_MESSAGE_CONTENT_FEATURES")
	}

	tb.env.MessageContent = true
	tb.say(tUser, tChGeneral, "one", false)
	a = tb.lastMessageID()
	tb.say(tMod, tChGeneral, "", false)
	b = tb.lastMessageID()
	bulk.Messages = []string{b, a, "77"}
	tb.onMessageDeleteBulk(tb.s, bulk)
	posts := tb.api.find("POST", "/channels/"+tChBB+"/messages")
	transcript := string(posts[len(posts)-1].Files["bulk-delete-general.txt"])
	for _, want := range []string{"user: one", "moddy: [no text]", "(1 older message(s) were not in the bot's cache)"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript %q misses %q", transcript, want)
		}
	}
}

func TestAuditChannels(t *testing.T) {
	tb := auditBot(t)
	ch := &discordgo.Channel{ID: "700", GuildID: tGuild, Name: "news", Type: discordgo.ChannelTypeGuildText, ParentID: tCatTickets}
	tb.onChannelCreate(tb.s, &discordgo.ChannelCreate{Channel: ch})
	tb.expectLog(1, "Channel Created", channelMention("700"), "`news`", "**Type:** text", "**Category:** Tickets")

	after := *ch
	after.Name, after.Topic, after.ParentID, after.NSFW, after.RateLimitPerUser = "news-2", "Daily news", "", true, 30
	after.PermissionOverwrites = []*discordgo.PermissionOverwrite{{ID: tRoleMod, Type: discordgo.PermissionOverwriteTypeRole,
		Allow: discordgo.PermissionViewChannel, Deny: discordgo.PermissionSendMessages}}
	tb.onChannelUpdate(tb.s, &discordgo.ChannelUpdate{Channel: &after, BeforeUpdate: ch})
	tb.expectLog(2, "Channel Updated", "**Name:** `news` → `news-2`", "**Topic:** *none* → Daily news",
		"`Tickets` → `None`", "**NSFW:** `False` → `True`", "`0s` → `30s`", "Mod: +read_messages, -send_messages")

	tb.onChannelUpdate(tb.s, &discordgo.ChannelUpdate{Channel: &after, BeforeUpdate: &after}) // nothing changed
	tb.onChannelUpdate(tb.s, &discordgo.ChannelUpdate{Channel: &after})                       // no before
	tb.expectLog(2)

	tb.onChannelDelete(tb.s, &discordgo.ChannelDelete{Channel: &after})
	tb.expectLog(3, "Channel Deleted", "**#news-2**", "**Category:** None")
}

func TestAuditThreads(t *testing.T) {
	tb := auditBot(t)
	th := &discordgo.Channel{ID: "800", GuildID: tGuild, Name: "help me", ParentID: tChGeneral, OwnerID: tUser,
		Type: discordgo.ChannelTypeGuildPublicThread, ThreadMetadata: &discordgo.ThreadMetadata{}}
	tb.onThreadCreate(tb.s, &discordgo.ThreadCreate{Channel: th, NewlyCreated: false}) // joined, not created
	tb.expectLog(0)
	tb.onThreadCreate(tb.s, &discordgo.ThreadCreate{Channel: th, NewlyCreated: true})
	tb.expectLog(1, "Thread Created", "`help me`", "**Parent:** "+channelMention(tChGeneral), "**Creator:** "+userMention(tUser))

	after := *th
	after.Name, after.ThreadMetadata = "solved", &discordgo.ThreadMetadata{Archived: true, Locked: true}
	tb.onThreadUpdate(tb.s, &discordgo.ThreadUpdate{Channel: &after}) // before from the snapshot
	tb.expectLog(2, "Thread Updated", "`help me` → `solved`", "**Archived:** `False` → `True`", "**Locked:** `False` → `True`")

	tb.onThreadDelete(tb.s, &discordgo.ThreadDelete{Channel: &discordgo.Channel{ID: "800", GuildID: tGuild, ParentID: tChGeneral}})
	tb.expectLog(3, "Thread Deleted", "**solved**")
}

func TestAuditRoles(t *testing.T) {
	tb := auditBot(t)
	role := &discordgo.Role{ID: "900000", Name: "Helpers", Color: 0x1abc9c, Position: 3,
		Permissions: discordgo.PermissionSendMessages | discordgo.PermissionKickMembers}
	tb.onRoleCreate(tb.s, &discordgo.GuildRoleCreate{GuildRole: &discordgo.GuildRole{Role: role, GuildID: tGuild}})
	tb.expectLog(1, "Role Created", "**Helpers**", "`#1abc9c`", "`3`")

	after := *role
	after.Name, after.Hoist, after.Permissions = "Helpers+", true, discordgo.PermissionSendMessages|discordgo.PermissionBanMembers
	tb.onRoleUpdate(tb.s, &discordgo.GuildRoleUpdate{GuildRole: &discordgo.GuildRole{Role: &after, GuildID: tGuild}})
	tb.expectLog(2, "Role Updated", "`Helpers` → `Helpers+`", "**Hoisted:** `False` → `True`",
		"**Permissions granted:** ban_members", "**Permissions revoked:** kick_members")

	tb.onRoleUpdate(tb.s, &discordgo.GuildRoleUpdate{GuildRole: &discordgo.GuildRole{Role: &discordgo.Role{ID: "123"}, GuildID: tGuild}})
	tb.expectLog(2) // unknown before: nothing to compare

	tb.onRoleDelete(tb.s, &discordgo.GuildRoleDelete{RoleID: "900000", GuildID: tGuild})
	tb.expectLog(3, "Role Deleted", "**Helpers+**")

	// Roles from the start-up guild data are known too.
	tb.onGuildCreate(tb.s, &discordgo.GuildCreate{Guild: &discordgo.Guild{ID: tGuild, Roles: []*discordgo.Role{{ID: "55", Name: "Old"}}}})
	tb.onRoleDelete(tb.s, &discordgo.GuildRoleDelete{RoleID: "55", GuildID: tGuild})
	tb.expectLog(4, "**Old**")
}

func TestAuditVoice(t *testing.T) {
	tb := auditBot(t)
	m, _ := tb.s.State.Member(tGuild, tUser)
	voice := func(before *discordgo.VoiceState, after discordgo.VoiceState) {
		after.GuildID, after.UserID, after.Member = tGuild, tUser, m
		tb.onVoiceStateUpdate(tb.s, &discordgo.VoiceStateUpdate{VoiceState: &after, BeforeUpdate: before})
	}
	voice(nil, discordgo.VoiceState{ChannelID: tChGeneral})
	tb.expectLog(1, "Joined Voice", userMention(tUser), "**general**", "User ID: "+tUser)
	voice(&discordgo.VoiceState{ChannelID: tChGeneral}, discordgo.VoiceState{ChannelID: tChSupport})
	tb.expectLog(2, "Moved Voice", "**general** → **support**")
	voice(&discordgo.VoiceState{ChannelID: tChSupport}, discordgo.VoiceState{ChannelID: tChSupport, SelfMute: true, SelfVideo: true})
	tb.expectLog(3, "Voice State Changed", "self-muted, turned camera on in **support**")
	voice(&discordgo.VoiceState{ChannelID: tChSupport}, discordgo.VoiceState{ChannelID: tChSupport})
	tb.expectLog(3)
	voice(&discordgo.VoiceState{ChannelID: tChSupport}, discordgo.VoiceState{})
	tb.expectLog(4, "Left Voice", "**support**")

	m.User.Bot = true
	voice(nil, discordgo.VoiceState{ChannelID: tChGeneral})
	m.User.Bot = false
	tb.expectLog(4)
}

func TestAuditBansAndSetupMode(t *testing.T) {
	tb := auditBot(t)
	u := &discordgo.User{ID: "555", Username: "spammer", Discriminator: "0"}
	tb.onBanAdd(tb.s, &discordgo.GuildBanAdd{User: u, GuildID: tGuild})
	tb.onBanRemove(tb.s, &discordgo.GuildBanRemove{User: u, GuildID: tGuild})
	log := tb.sent(tChModLog)
	if len(log) != 2 || !strings.Contains(log[0], "**spammer** (`555`) was banned") || !strings.Contains(log[1], "was unbanned") {
		t.Errorf("mod log = %q", log)
	}

	tb.env.SetupMode = true
	tb.api.reset()
	tb.say(tUser, tChGeneral, "x", false)
	tb.del(tb.lastMessageID())
	tb.onBanAdd(tb.s, &discordgo.GuildBanAdd{User: u, GuildID: tGuild})
	tb.onChannelCreate(tb.s, &discordgo.ChannelCreate{Channel: &discordgo.Channel{ID: "1", GuildID: tGuild}})
	if calls := tb.callList(); len(calls) != 0 {
		t.Errorf("setup mode made calls: %v", calls)
	}
}

func TestAuditInvites(t *testing.T) {
	tb := auditBot(t)
	tb.onInviteCreate(tb.s, &discordgo.InviteCreate{GuildID: tGuild, ChannelID: tChGeneral, Invite: &discordgo.Invite{
		Code: "abc123", MaxUses: 5, Inviter: &discordgo.User{ID: tUser, Username: "user"}}})
	log := tb.sent(tChModLog)
	if len(log) != 1 || !strings.Contains(log[0], "**Code:** `abc123`") || !strings.Contains(log[0], "Default avatar:** Yes ⚠️") ||
		!strings.Contains(log[0], "**Max uses:** 5") {
		t.Fatalf("invite log = %q", log)
	}
	tb.expectCall(1, "PUT", "/channels/"+tChModLog+"/messages/8000/reactions/❌/@me")

	var embed discordgo.MessageEmbed
	posts := tb.api.find("POST", "/channels/"+tChModLog+"/messages")
	var body struct {
		Embeds []discordgo.MessageEmbed `json:"embeds"`
	}
	_ = json.Unmarshal(posts[0].Body, &body)
	embed = body.Embeds[0]
	logMsg, _ := json.Marshal(map[string]any{"id": "8000", "channel_id": tChModLog,
		"author": map[string]any{"id": tBot, "username": "healthbot"}, "embeds": []any{embed}})
	tb.api.on("GET", "/channels/"+tChModLog+"/messages/8000", 200, string(logMsg))
	tb.api.on("GET", "/channels/"+tChModLog+"/messages/8001", 200,
		`{"id":"8001","author":{"id":"`+tUser+`"},"embeds":[{"title":"📨 Invite Created","description":"**Code:** `+"`fake`"+`"}]}`)

	// Discord answers an invite delete with the invite.
	tb.api.on("DELETE", "/invites/.*", 200, `{"code":"abc123"}`)
	tb.api.reset()
	tb.react(tUser, tChModLog, "8001", "❌")  // not the bot's message
	tb.react(tUser, tChGeneral, "8000", "❌") // not in MOD_LOG
	tb.react(tBot, tChModLog, "8000", "❌")   // the bot's own reaction
	tb.expectCall(0, "DELETE", "/invites/.*")

	tb.react(tMod, tChModLog, "8000", "❌")
	if c := tb.expectCall(1, "DELETE", "/invites/abc123"); len(c) == 1 && c[0].Reason != "Deleted via reaction by moddy" {
		t.Errorf("audit reason = %q", c[0].Reason)
	}
	if !contains(tb.sent(tChModLog), "Invite `abc123` deleted by "+userMention(tMod)) {
		t.Error("delete not logged")
	}

	// A second ❌ finds the invite gone: nothing more is logged.
	tb.api.on("DELETE", "/invites/abc123", 404, `{"message":"Unknown Invite","code":10006}`)
	tb.api.reset()
	tb.react(tMod, tChModLog, "8000", "❌")
	tb.expectCall(0, "POST", ".*")
}
