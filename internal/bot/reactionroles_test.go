package bot

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/binpas/hcbot/internal/db"
)

func TestParseRolePair(t *testing.T) {
	tb := newTestBot(t)
	cases := []struct{ line, emoji, role string }{
		{"🍕 <@&" + tRoleGamer + ">", "🍕", tRoleGamer},
		{"  🍕   " + tRoleGamer + "  ", "🍕", tRoleGamer},
		{"<:blob:1234567890> strip", "<:blob:1234567890>", tRoleStrip},
		{"<a:dance:1234567890>vip", "<a:dance:1234567890>", tRoleVIP},
		{"🎮 Server Booster", "🎮", tRoleBoost},
		{"🍕 https://discord.com/channels/111111111111111111/" + tRoleGamer, "🍕", tRoleGamer},
	}
	for _, c := range cases {
		emoji, role, ok := tb.parseRolePair(tGuild, c.line)
		if !ok || emoji != c.emoji || role.ID != c.role {
			t.Errorf("parseRolePair(%q) = %q, %v, %v", c.line, emoji, role, ok)
		}
	}
	for _, line := range []string{"", "🍕", "🍕 nosuchrole", "<:blob:1234567890>"} {
		if _, _, ok := tb.parseRolePair(tGuild, line); ok {
			t.Errorf("parseRolePair(%q) should fail", line)
		}
	}
}

// wizardID returns a wizard custom ID for an action from the last reply.
func (tb *testBot) wizardID(action string) string {
	tb.t.Helper()
	return tb.componentID(tb.last(), "rrw:"+action+":")
}

// modalFor turns a wizard button's custom ID into its modal's custom ID.
func modalFor(buttonID string) string {
	return "rrwm:" + strings.TrimPrefix(buttonID, "rrw:")
}

func TestReactionRoleWizard(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "reactionrole create")
	tb.expect(true, "New Reaction Role", "*not set*", "*none set*")
	name, pairs, text, save := tb.wizardID("name"), tb.wizardID("pairs"), tb.wizardID("text"), tb.wizardID("save")

	tb.click(tUser, name)
	tb.expect(true, "This wizard isn't yours")

	tb.click(tMod, save)
	tb.expect(true, "Set the name, channel, at least one pair")

	tb.click(tMod, name)
	if r := tb.replies(); r[len(r)-1].Kind != "modal" {
		t.Fatal("Edit Name did not open a modal")
	}
	tb.submit(tMod, modalFor(name), map[string]string{"value": "  Games "})
	tb.expect(true, "Games")
	tb.click(tMod, tb.wizardID("chan"), tChGeneral)
	tb.expect(true, channelMention(tChGeneral))

	tb.api.reset()
	tb.submit(tMod, modalFor(pairs), map[string]string{"value": "🎮 <@&" + tRoleGamer + ">\n\n🍕 strip\n🎮 vip\nbad line\n🔥 High"})
	rs := tb.replies()
	if len(rs) != 2 || rs[0].Kind != "update" || !strings.Contains(rs[0].text(), "Pairs (2)") {
		t.Fatalf("pairs replies = %+v", rs)
	}
	for _, want := range []string{"used more than once", "Couldn't read: `bad line`", "above my highest role"} {
		if !strings.Contains(rs[1].text(), want) {
			t.Errorf("skipped lines report misses %q: %q", want, rs[1].text())
		}
	}
	tb.submit(tMod, modalFor(pairs), map[string]string{"value": "nothing valid"})
	tb.expect(true, "No valid pairs found")

	tb.submit(tMod, modalFor(text), map[string]string{"value": " Pick your roles! "})
	tb.expect(true, "Pick your roles!")

	tb.click(tMod, save)
	r := tb.expect(true, "Saved `Games`. Post it to "+channelMention(tChGeneral))
	rr, _ := tb.db.ReactionRoleByName(tGuild, "games")
	if rr == nil || rr.Text != "Pick your roles!" || rr.ChannelID != tChGeneral || rr.MessageID != "" {
		t.Fatalf("saved = %+v", rr)
	}
	if p, _ := tb.db.RolePairs(rr.ID); len(p) != 2 || p[0].Emoji != "🎮" || p[1].RoleID != tRoleStrip {
		t.Errorf("pairs = %+v", p)
	}

	// The draft is gone after Save.
	tb.click(tMod, name)
	tb.expect(true, "This wizard has expired")

	tb.api.reset()
	tb.click(tMod, tb.componentID(r, "rrpost:"))
	tb.expect(true, "Posted in "+channelMention(tChGeneral))
	if !contains(tb.sent(tChGeneral), "Pick your roles!") || !contains(tb.sent(tChGeneral), "🍕 — "+roleMention(tRoleStrip)) {
		t.Errorf("posted = %v", tb.sent(tChGeneral))
	}
	tb.expectCall(2, "PUT", "/channels/"+tChGeneral+"/messages/8000/reactions/.*/@me")
	if rr, _ := tb.db.ReactionRoleByName(tGuild, "Games"); rr.MessageID != "8000" {
		t.Errorf("message ID = %q", rr.MessageID)
	}

	// A second wizard can't take the same name.
	tb.run(tMod, "reactionrole create")
	tb.submit(tMod, modalFor(tb.wizardID("name")), map[string]string{"value": "GAMES"})
	tb.expect(true, "already exists")
}

// savedRR stores a posted reaction role for tests.
func (tb *testBot) savedRR(name, channelID, messageID string, pairs ...db.RolePair) int64 {
	tb.t.Helper()
	id, err := tb.db.SaveReactionRole(0, tGuild, name, channelID, "text", pairs)
	if err == nil && messageID != "" {
		err = tb.db.SetReactionRoleMessage(id, channelID, messageID)
	}
	if err != nil {
		tb.t.Fatal(err)
	}
	return id
}

func TestReactionRoleEditSyncsReactions(t *testing.T) {
	tb := newTestBot(t)
	tb.savedRR("food", tChGeneral, "8100", db.RolePair{Emoji: "🍕", RoleID: tRoleStrip}, db.RolePair{Emoji: "🍔", RoleID: tRoleVIP})
	tb.api.on("GET", "/channels/"+tChGeneral+"/messages/8100", 200,
		`{"id":"8100","channel_id":"500","reactions":[{"count":3,"emoji":{"name":"🍕"}},{"count":1,"emoji":{"name":"🍔"}}]}`)

	tb.run(tMod, "reactionrole edit", strOpt("name", "FOOD"))
	tb.expect(true, "Edit Reaction Role", "🍕 — "+roleMention(tRoleStrip))
	tb.submit(tMod, modalFor(tb.wizardID("pairs")), map[string]string{"value": "🍕 strip\n🌭 vip"})
	save := tb.wizardID("save")
	tb.api.reset()
	tb.click(tMod, save)
	tb.expect(true, "Saved. Posted in")

	if edit := tb.expectCall(1, "PATCH", "/channels/"+tChGeneral+"/messages/8100"); len(edit) == 1 &&
		!strings.Contains(string(edit[0].Body), "🌭") {
		t.Errorf("edit body = %s", edit[0].Body)
	}
	tb.expectCall(1, "DELETE", "/channels/"+tChGeneral+"/messages/8100/reactions/🍔")
	tb.expectCall(1, "PUT", "/channels/"+tChGeneral+"/messages/8100/reactions/🌭/@me")
	tb.expectCall(0, "PUT", "/channels/"+tChGeneral+"/messages/8100/reactions/🍕/@me")
}

func TestReactionRoleMovedAndDeletedMessage(t *testing.T) {
	tb := newTestBot(t)
	tb.savedRR("food", tChGeneral, "8100", db.RolePair{Emoji: "🍕", RoleID: tRoleStrip})
	tb.run(tMod, "reactionrole edit", strOpt("name", "food"))
	tb.click(tMod, tb.wizardID("chan"), tChSupport)
	save := tb.wizardID("save")
	tb.api.reset()
	tb.click(tMod, save)
	tb.expect(true, "Saved `food`. Post it to "+channelMention(tChSupport))
	tb.expectCall(1, "DELETE", "/channels/"+tChGeneral+"/messages/8100")
	if rr, _ := tb.db.ReactionRoleByName(tGuild, "food"); rr.ChannelID != tChSupport || rr.MessageID != "" {
		t.Errorf("after move = %+v", rr)
	}

	// A posted message that was deleted by hand is posted again.
	tb.savedRR("drinks", tChGeneral, "8200", db.RolePair{Emoji: "🍺", RoleID: tRoleStrip})
	tb.api.on("GET", "/channels/"+tChGeneral+"/messages/8200", 404, `{"message":"Unknown Message","code":10008}`)
	tb.api.reset()
	tb.run(tMod, "reactionrole post", strOpt("name", "drinks"))
	tb.expect(true, "Posted in")
	tb.expectCall(1, "POST", "/channels/"+tChGeneral+"/messages")
	if rr, _ := tb.db.ReactionRoleByName(tGuild, "drinks"); rr.MessageID != "8000" {
		t.Errorf("new message ID = %q", rr.MessageID)
	}

	tb.api.fail("GET", "/channels/"+tChGeneral+"/messages/8000")
	tb.run(tMod, "reactionrole post", strOpt("name", "drinks"))
	tb.expect(true, "I can't edit messages in")
}

func TestReactionRoleListPostDelete(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "reactionrole list")
	tb.expect(true, "None defined yet")
	tb.savedRR("a", tChGeneral, "8100", db.RolePair{Emoji: "🍕", RoleID: tRoleStrip})
	tb.savedRR("b", "", "")
	tb.run(tMod, "reactionrole list")
	tb.expect(true, "**2** defined", "🟢 posted • 1 pair(s) • "+channelMention(tChGeneral), "⚪ not posted • 0 pair(s) • *no channel*")

	tb.run(tMod, "reactionrole post", strOpt("name", "b"))
	tb.expect(true, "no emote/role pairs yet")
	tb.run(tMod, "reactionrole post", strOpt("name", "zzz"))
	tb.expect(true, "No reaction role named `zzz`")

	tb.run(tMod, "reactionrole delete", strOpt("name", "A"))
	tb.expect(true, "Reaction role `a` deleted")
	tb.expectCall(1, "DELETE", "/channels/"+tChGeneral+"/messages/8100")
	if rr, _ := tb.db.ReactionRoleByName(tGuild, "a"); rr != nil {
		t.Error("not deleted")
	}
}

func TestReactionRoleReactions(t *testing.T) {
	tb := newTestBot(t)
	tb.savedRR("food", tChGeneral, "8100", db.RolePair{Emoji: "🍕", RoleID: tRoleStrip}, db.RolePair{Emoji: "🍔", RoleID: "424242"})

	tb.react(tUser, tChGeneral, "8100", "🍕")
	if c := tb.expectCall(1, "PUT", "/guilds/100/members/"+tUser+"/roles/"+tRoleStrip); len(c) == 1 && c[0].Reason != "Reaction role: food" {
		t.Errorf("audit reason = %q", c[0].Reason)
	}
	tb.unreact(tUser, tChGeneral, "8100", "🍕")
	tb.expectCall(1, "DELETE", "/guilds/100/members/"+tUser+"/roles/"+tRoleStrip)

	tb.api.reset()
	tb.react(tUser, tChGeneral, "8100", "🌭")  // not a pair
	tb.react(tUser, tChGeneral, "8100", "🍔")  // its role was deleted
	tb.react(tUser, tChGeneral, "8101", "🍕")  // not a reaction role message
	tb.react(tBot, tChGeneral, "8100", "🍕")   // the bot's own reaction
	tb.unreact(tBot, tChGeneral, "8100", "🍕") // the bot's own reaction
	m, _ := tb.s.State.Member(tGuild, tUser)
	m.User.Bot = true
	tb.react(tUser, tChGeneral, "8100", "🍕")
	m.User.Bot = false
	for _, c := range tb.callList() {
		if strings.Contains(c, "/roles/") {
			t.Errorf("unexpected role change: %s", c)
		}
	}
}

func TestCuration(t *testing.T) {
	tb := newTestBot(t, withMessageContent)
	tb.config(map[string]string{"CURATED": tChSupport, "CURATED_THRESHOLD": "2"})
	message := func(id string, stars int) {
		tb.api.on("GET", "/channels/"+tChGeneral+"/messages/"+id, 200, fmt.Sprintf(
			`{"id":%q,"channel_id":%q,"content":"the best message","timestamp":"2026-10-29T12:00:00Z",
			  "author":{"id":"900","username":"user","global_name":"User Name","avatar":"abc"},
			  "reactions":[{"count":%d,"emoji":{"name":"⭐"}}]}`, id, tChGeneral, stars))
	}

	message("9001", 1)
	tb.react(tUser, tChGeneral, "9001", "⭐")
	tb.expectCall(0, "POST", "/channels/"+tChSupport+"/messages")

	message("9001", 2)
	tb.react(tUser, tChGeneral, "9001", "👍") // another emote
	tb.expectCall(0, "POST", "/channels/"+tChSupport+"/messages")
	tb.react(tUser, tChGeneral, "9001", "⭐")
	posts := tb.expectCall(1, "POST", "/channels/"+tChSupport+"/messages")
	if len(posts) == 1 {
		var post struct {
			Content string `json:"content"`
			Embeds  []struct {
				Title     string `json:"title"`
				URL       string `json:"url"`
				Timestamp string `json:"timestamp"`
				Author    struct {
					Name string `json:"name"`
				} `json:"author"`
				Description string `json:"description"`
			} `json:"embeds"`
			Ref *struct{} `json:"message_reference"`
		}
		_ = json.Unmarshal(posts[0].Body, &post)
		e := post.Embeds
		if post.Content != "⭐ **2** | "+channelMention(tChGeneral) || len(e) != 1 || post.Ref != nil ||
			e[0].Author.Name != "User Name" || e[0].Description != "the best message" || e[0].Title != "Jump to message" ||
			e[0].URL != "https://discord.com/channels/"+tGuild+"/"+tChGeneral+"/9001" || e[0].Timestamp == "" {
			t.Errorf("post = %s", posts[0].Body)
		}
	}

	// Curated once only.
	tb.api.reset()
	tb.react(tUser, tChGeneral, "9001", "⭐")
	tb.expectCall(0, "POST", ".*")

	// Two reactions at the same moment post it once.
	message("9002", 5)
	tb.api.reset()
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tb.react(tUser, tChGeneral, "9002", "⭐")
		}()
	}
	wg.Wait()
	tb.expectCall(1, "POST", "/channels/"+tChSupport+"/messages")

	// Nothing in the curated channel itself, and nothing (not even a
	// claim) when CURATED is not set.
	tb.api.reset()
	tb.react(tUser, tChSupport, "9003", "⭐")
	tb.config(map[string]string{"CURATED": ""})
	message("9004", 5)
	tb.react(tUser, tChGeneral, "9004", "⭐")
	tb.expectCall(0, "POST", ".*")
	if done, _ := tb.db.IsCurated("9004"); done {
		t.Error("claimed without a curated channel")
	}
}

func TestCurationMedia(t *testing.T) {
	tb := newTestBot(t, withMessageContent)
	tb.config(map[string]string{"CURATED": tChSupport, "CURATED_THRESHOLD": "1"})
	tb.api.on("GET", "/channels/"+tChGeneral+"/messages/9001", 200, `{"id":"9001","channel_id":"`+tChGeneral+`",
		"content":"look https://youtu.be/x ||secret||","author":{"id":"900","username":"user"},
		"referenced_message":{"id":"8000","author":{"id":"901","username":"other"}},
		"attachments":[
			{"id":"1","filename":"a.png","url":"https://cdn.discordapp.com/attachments/1/1/a.png","size":10,"content_type":"image/png"},
			{"id":"2","filename":"b.jpg","url":"https://cdn.discordapp.com/attachments/1/2/b.jpg","size":10},
			{"id":"3","filename":"SPOILER_c.png","url":"https://cdn.discordapp.com/attachments/1/3/SPOILER_c.png","size":10,"content_type":"image/png"},
			{"id":"4","filename":"clip.mp4","url":"https://cdn.discordapp.com/attachments/1/4/clip.mp4","size":10,"content_type":"video/mp4"},
			{"id":"5","filename":"huge.mp4","url":"https://cdn.discordapp.com/attachments/1/5/huge.mp4","size":999999999},
			{"id":"6","filename":"gone.png","url":"https://cdn.discordapp.com/attachments/1/6/gone.png","size":10}],
		"sticker_items":[{"id":"77","name":"wave","format_type":1}],
		"embeds":[
			{"type":"video","url":"https://youtu.be/x","title":"A video"},
			{"type":"rich","title":"Bot card","url":"https://example.com","description":"line one\nline two",
			 "fields":[{"name":"Score","value":"12"}],"footer":{"text":"by a bot"}}],
		"reactions":[{"count":1,"emoji":{"name":"⭐"}}]}`)
	for _, f := range []string{"1/a.png", "2/b.jpg", "3/SPOILER_c.png", "4/clip.mp4"} {
		tb.api.on("GET", "/attachments/1/"+f, 200, "data-"+f)
	}
	tb.api.on("GET", "/attachments/1/6/gone.png", 404, `{}`)
	tb.api.on("GET", "/stickers/77.png", 200, "sticker")
	tb.react(tUser, tChGeneral, "9001", "⭐")

	posts := tb.expectCall(1, "POST", "/channels/"+tChSupport+"/messages")
	if len(posts) != 1 {
		return
	}
	var post struct {
		Content string `json:"content"`
		Embeds  []struct {
			URL         string `json:"url"`
			Description string `json:"description"`
			Image       struct {
				URL string `json:"url"`
			} `json:"image"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(posts[0].Body, &post); err != nil {
		t.Fatal(err)
	}
	// The link preview is a link in the text; the bot embed is quoted.
	if !strings.HasSuffix(post.Content, "\nhttps://youtu.be/x") {
		t.Errorf("content = %q", post.Content)
	}
	desc := post.Embeds[0].Description
	for _, want := range []string{"-# ↩️ Replying to **other**", "look https://youtu.be/x ||secret||",
		"> **[Bot card](https://example.com)**\n> line one\n> line two\n> **Score**: 12\n> -# by a bot",
		"`huge.mp4` is too large", "`gone.png` could not be copied"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description has no %q:\n%s", want, desc)
		}
	}
	if strings.Contains(desc, "A video") {
		t.Error("the link preview is copied")
	}
	// Images: a gallery of a.png, b.jpg and the sticker; the spoiler and
	// the video are only files.
	var images []string
	for _, e := range post.Embeds {
		images = append(images, e.Image.URL)
		if e.URL != post.Embeds[0].URL {
			t.Errorf("gallery embed URL = %q", e.URL)
		}
	}
	if want := []string{"attachment://a.png", "attachment://b.jpg", "attachment://sticker_77.png"}; !slices.Equal(images, want) {
		t.Errorf("images = %v, want %v", images, want)
	}
	for name, want := range map[string]string{"a.png": "data-1/a.png", "SPOILER_c.png": "data-3/SPOILER_c.png", "clip.mp4": "data-4/clip.mp4", "sticker_77.png": "sticker"} {
		if got := string(posts[0].Files[name]); got != want {
			t.Errorf("file %s = %q, want %q", name, got, want)
		}
	}
	if len(posts[0].Files) != 5 {
		t.Errorf("files = %d, want 5", len(posts[0].Files))
	}
	if !strings.Contains(string(posts[0].Body), `"allowed_mentions":{`) {
		t.Error("the post can ping")
	}
}

func TestCurationFileRefused(t *testing.T) {
	tb := newTestBot(t, withMessageContent)
	tb.config(map[string]string{"CURATED": tChSupport, "CURATED_THRESHOLD": "1"})
	tb.api.on("GET", "/channels/"+tChGeneral+"/messages/9001", 200, `{"id":"9001","channel_id":"`+tChGeneral+`",
		"content":"hi","author":{"id":"900","username":"user"},
		"attachments":[{"id":"1","filename":"a.png","url":"https://cdn.discordapp.com/attachments/1/1/a.png","size":10}],
		"reactions":[{"count":1,"emoji":{"name":"⭐"}}]}`)
	tb.api.on("GET", "/attachments/1/1/a.png", 200, "png")
	tb.api.onBody("POST", "/channels/"+tChSupport+"/messages", "attachment://", 1, 413, `{"message":"Request entity too large","code":40005}`)
	tb.react(tUser, tChGeneral, "9001", "⭐")
	posts := tb.api.find("POST", "/channels/"+tChSupport+"/messages")
	if len(posts) != 2 || len(posts[1].Files) != 0 || !strings.Contains(string(posts[1].Body), "could not be copied") {
		t.Errorf("posts = %v", tb.callList())
	}
}

func TestCurationWithoutMessageContent(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"CURATED": tChSupport, "CURATED_THRESHOLD": "1"})
	tb.api.on("GET", "/channels/"+tChGeneral+"/messages/9001", 200, `{"id":"9001","channel_id":"500",
		"author":{"id":"202","username":"user"},"reactions":[{"count":1,"emoji":{"name":"⭐"}}]}`)
	tb.react(tUser, tChGeneral, "9001", "⭐")
	if calls := tb.callList(); len(calls) != 0 {
		t.Errorf("curation ran without the option: %v", calls)
	}
	if done, _ := tb.db.IsCurated("9001"); done {
		t.Error("message claimed as curated")
	}
}
