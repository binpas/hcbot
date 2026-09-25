package bot

import (
	"encoding/json"
	"fmt"
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
			`{"id":%q,"channel_id":%q,"author":{"id":"202","username":"user","global_name":"User Name","avatar":"abc"},
			  "reactions":[{"count":%d,"emoji":{"name":"⭐"}}]}`, id, tChGeneral, stars))
	}

	message("9001", 1)
	tb.react(tUser, tChGeneral, "9001", "⭐")
	tb.expectCall(0, "POST", "/channels/"+tChSupport+"/messages")

	message("9001", 2)
	tb.react(tUser, tChGeneral, "9001", "👍") // another emote
	tb.expectCall(0, "POST", "/channels/"+tChSupport+"/messages")
	tb.react(tUser, tChGeneral, "9001", "⭐")
	posts := tb.expectCall(2, "POST", "/channels/"+tChSupport+"/messages")
	if len(posts) == 2 {
		var card struct {
			Content string `json:"content"`
			Embeds  []struct {
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"embeds"`
		}
		_ = json.Unmarshal(posts[0].Body, &card)
		if card.Content != "⭐ **2** | "+channelMention(tChGeneral) || len(card.Embeds) != 1 || card.Embeds[0].Title != "User Name" {
			t.Errorf("info card = %s", posts[0].Body)
		}
		var fwd struct {
			Content string `json:"content"`
			Ref     struct {
				Type      int    `json:"type"`
				MessageID string `json:"message_id"`
				ChannelID string `json:"channel_id"`
			} `json:"message_reference"`
		}
		_ = json.Unmarshal(posts[1].Body, &fwd)
		if fwd.Ref.Type != 1 || fwd.Ref.MessageID != "9001" || fwd.Ref.ChannelID != tChGeneral || fwd.Content != "" {
			t.Errorf("forward = %s", posts[1].Body)
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
	tb.expectCall(2, "POST", "/channels/"+tChSupport+"/messages")

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

func TestCurationForwardRetryAndLink(t *testing.T) {
	setup := func(t *testing.T) *testBot {
		tb := newTestBot(t, withMessageContent)
		tb.config(map[string]string{"CURATED": tChSupport, "CURATED_THRESHOLD": "1"})
		tb.api.on("GET", "/channels/"+tChGeneral+"/messages/9001", 200, `{"id":"9001","channel_id":"500",
			"author":{"id":"202","username":"user"},"reactions":[{"count":1,"emoji":{"name":"⭐"}}]}`)
		return tb
	}
	forward := `"type":1`
	link := "https://discord.com/channels/" + tGuild + "/" + tChGeneral + "/9001"

	t.Run("refused once", func(t *testing.T) {
		tb := setup(t)
		tb.api.onBody("POST", "/channels/"+tChSupport+"/messages", forward, 1, 403,
			`{"message":"You cannot forward a message whose content you cannot read","code":160014}`)
		tb.react(tUser, tChGeneral, "9001", "⭐")
		if n := len(tb.api.find("POST", "/channels/"+tChSupport+"/messages")); n != 3 {
			t.Errorf("posts = %d, want card + 2 forward tries", n)
		}
		if contains(tb.sent(tChSupport), link) {
			t.Error("fallback link posted although the second forward worked")
		}
	})

	t.Run("late to show", func(t *testing.T) {
		tb := setup(t)
		// The first check misses the forward; the second finds it.
		tb.api.onBody("GET", "/channels/"+tChSupport+"/messages/8000", "", 1, 404, `{"message":"Unknown Message","code":10008}`)
		tb.react(tUser, tChGeneral, "9001", "⭐")
		if n := len(tb.api.find("POST", "/channels/"+tChSupport+"/messages")); n != 2 {
			t.Errorf("posts = %d, want card + 1 forward (it showed up on the second check)", n)
		}
	})

	t.Run("never shows", func(t *testing.T) {
		tb := setup(t)
		tb.api.onBody("POST", "/channels/"+tChSupport+"/messages", forward, 0, 403,
			`{"message":"You cannot forward a message whose content you cannot read","code":160014}`)
		tb.react(tUser, tChGeneral, "9001", "⭐")
		sent := tb.sent(tChSupport)
		if len(sent) != 4 || !strings.Contains(sent[3], link) {
			t.Errorf("posts = %q, want card + 2 forward tries + a link", sent)
		}
	})
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
