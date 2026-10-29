package bot

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/markov"
)

func (tb *testBot) markovStats() (words, steps int64) {
	tb.t.Helper()
	tb.flushMarkov()
	st, err := tb.chain.Stats()
	if err != nil {
		tb.t.Fatal(err)
	}
	return st.Words, st.Transitions
}

// markovSent returns the bodies of the messages posted to a channel.
func (tb *testBot) markovSent(channelID string) []discordgo.MessageSend {
	var out []discordgo.MessageSend
	for _, c := range tb.api.find("POST", "/channels/"+channelID+"/messages") {
		var m discordgo.MessageSend
		_ = json.Unmarshal(c.Body, &m)
		out = append(out, m)
	}
	return out
}

func TestMarkovMentionOnly(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"MARKOV_LEARN_CHANNELS": tChGeneral, "MARKOV_REPLY_CHANNELS": tChGeneral,
		"MARKOV_COOLDOWN": "0"})

	tb.say(tUser, tChGeneral, "not a mention", false)
	if w, _ := tb.markovStats(); w != 0 {
		t.Fatalf("learned %d words without a mention or Message Content", w)
	}

	// The chain is empty: it learns, but has nothing to say yet.
	tb.say(tUser, tChGeneral, "the cat sat on the mat <@&999> https://x.io", true)
	if len(tb.markovSent(tChGeneral)) != 0 {
		t.Fatal("replied from an empty chain")
	}
	if w, s := tb.markovStats(); w != 5 || s != 7 {
		t.Fatalf("stats = %d words, %d steps; want 5, 7", w, s)
	}

	tb.say(tUser, tChGeneral, "what about the mat?", true)
	sent := tb.markovSent(tChGeneral)
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	if !strings.Contains(sent[0].Content, "mat") || sent[0].Reference == nil {
		t.Errorf("reply = %+v", sent[0])
	}
	if am := sent[0].AllowedMentions; am == nil || len(am.Parse) != 0 || am.RepliedUser {
		t.Errorf("allowed mentions = %+v; want nobody pinged", am)
	}

	// A trigger wins over the Markov chat.
	if _, err := tb.db.AddTriggerValue("hello", "hi!"); err != nil {
		t.Fatal(err)
	}
	tb.say(tUser, tChGeneral, "hello", true)
	if sent := tb.markovSent(tChGeneral); len(sent) != 2 || sent[1].Content != "hi!" {
		t.Errorf("trigger reply = %+v", sent)
	}

	// Channels that are not in the lists: no reply, no learning.
	tb.say(tUser, tChVIP, "brand new words", true)
	if len(tb.markovSent(tChVIP)) != 0 {
		t.Error("replied outside MARKOV_REPLY_CHANNELS")
	}
	if n, _ := tb.chain.CountPhrase([]string{"brand"}); n != 0 {
		t.Error("learned outside MARKOV_LEARN_CHANNELS")
	}
}

func TestMarkovMessageContent(t *testing.T) {
	tb := newTestBot(t, withMessageContent)
	tb.config(map[string]string{"MARKOV_LEARN_CHANNELS": tChGeneral, "MARKOV_REPLY_CHANNELS": tChGeneral,
		"MARKOV_COOLDOWN": "0"})

	tb.say(tUser, tChGeneral, "every message is learned", false)
	if w, _ := tb.markovStats(); w != 4 {
		t.Fatalf("learned %d words, want 4", w)
	}
	if len(tb.markovSent(tChGeneral)) != 0 {
		t.Fatal("replied at random with MARKOV_REPLY_CHANCE 0")
	}
	tb.config(map[string]string{"MARKOV_REPLY_CHANCE": "100", "MARKOV_RANDOM_AS_REPLY": "0"})
	tb.say(tUser, tChGeneral, "message", false)
	sent := tb.markovSent(tChGeneral)
	if len(sent) != 1 || sent[0].Reference != nil || !strings.HasPrefix(sent[0].Content, "message") {
		t.Fatalf("random message = %+v", sent)
	}
	tb.config(map[string]string{"MARKOV_RANDOM_AS_REPLY": "100"})
	tb.say(tUser, tChGeneral, "message", false)
	if sent := tb.markovSent(tChGeneral); len(sent) != 2 || sent[1].Reference == nil {
		t.Fatalf("random reply = %+v", sent)
	}
}

// Without Message Content the bot cannot read other messages, but it can
// still talk after them. It does not learn from them.
func TestMarkovRandomWithoutMessageContent(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"MARKOV_LEARN_CHANNELS": tChGeneral, "MARKOV_REPLY_CHANNELS": tChGeneral,
		"MARKOV_COOLDOWN": "0", "MARKOV_RANDOM_AS_REPLY": "0"})
	tb.say(tUser, tChGeneral, "hello there friend", true) // learned; no reply from an empty chain
	tb.flushMarkov()
	tb.say(tUser, tChGeneral, "secret text", false)
	if len(tb.markovSent(tChGeneral)) != 0 {
		t.Fatalf("talked with MARKOV_REPLY_CHANCE 0: %+v", tb.markovSent(tChGeneral))
	}
	if n, _ := tb.chain.CountPhrase([]string{"secret"}); n != 0 {
		t.Error("learned a message without a mention or Message Content")
	}

	tb.config(map[string]string{"MARKOV_REPLY_CHANCE": "100"})
	tb.say(tUser, tChGeneral, "secret text", false)
	sent := tb.markovSent(tChGeneral)
	if len(sent) != 1 || sent[0].Content != "hello there friend" || sent[0].Reference != nil {
		t.Fatalf("random message = %+v", sent)
	}
}

func TestMarkovCooldown(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"MARKOV_REPLY_CHANNELS": tChGeneral + "," + tChVIP,
		"MARKOV_REPLY_CHANCE": "100", "MARKOV_COOLDOWN": "60"})
	learn(t, tb, "some words here")
	count := func(ch string) int { return len(tb.markovSent(ch)) }

	tb.say(tUser, tChGeneral, "one", false)
	tb.say(tUser, tChGeneral, "two", false)
	if count(tChGeneral) != 1 {
		t.Fatalf("random messages during the cooldown: %d", count(tChGeneral))
	}
	tb.say(tUser, tChVIP, "other channel", false)
	if count(tChVIP) != 1 {
		t.Error("the cooldown of one channel stopped another")
	}

	// MARKOV_MENTION_REPLY always (the default): mentions skip the cooldown.
	tb.say(tUser, tChGeneral, "hi", true)
	tb.say(tUser, tChGeneral, "hi", true)
	if count(tChGeneral) != 3 {
		t.Fatalf("mention replies = %d, want 2 more", count(tChGeneral)-1)
	}
	tb.run(tMod, "markov status")
	tb.expect(true, "Replies to @mentions always", "Cooldown: **60s** (", "s left in this channel)")

	// cooldown: mentions obey the cooldown too.
	tb.config(map[string]string{"MARKOV_MENTION_REPLY": "cooldown"})
	tb.say(tUser, tChGeneral, "hi", true)
	if count(tChGeneral) != 3 {
		t.Fatal("a mention skipped the cooldown with MARKOV_MENTION_REPLY cooldown")
	}
	// Mods (MOD_ROLE, Administrator, the owner) always get a reply.
	tb.say(tMod, tChGeneral, "hi", true)
	if count(tChGeneral) != 4 {
		t.Fatal("no reply to a mod during the cooldown")
	}
	tb.say(tUser, tChGeneral, "hi", true)
	if count(tChGeneral) != 4 {
		t.Fatal("a mod's reply did not start the cooldown for others")
	}
	g, _ := tb.s.State.Guild(tGuild)
	g.Roles = append(g.Roles, &discordgo.Role{ID: "19", Name: "Boss", Permissions: discordgo.PermissionAdministrator})
	if err := tb.s.State.MemberAdd(&discordgo.Member{GuildID: tGuild, User: &discordgo.User{ID: "210", Username: "boss"},
		Roles: []string{"19"}}); err != nil {
		t.Fatal(err)
	}
	if err := tb.s.State.MemberAdd(&discordgo.Member{GuildID: tGuild, User: &discordgo.User{ID: tOwner, Username: "owner"}}); err != nil {
		t.Fatal(err)
	}
	tb.say("210", tChGeneral, "hi", true)
	tb.say(tOwner, tChGeneral, "hi", true)
	if count(tChGeneral) != 6 {
		t.Fatalf("replies to an administrator and the owner = %d, want 2", count(tChGeneral)-4)
	}
	tb.run(tMod, "markov status")
	tb.expect(true, "only when the cooldown is over (mods: always)")

	tb.mk.spoke[tChGeneral] = time.Now().Add(-61 * time.Second) // the cooldown is over
	tb.say(tUser, tChGeneral, "hi", true)
	if count(tChGeneral) != 7 {
		t.Fatal("no reply to a mention after the cooldown")
	}
	// mods: only mods get a reply to a mention, also after the cooldown.
	tb.config(map[string]string{"MARKOV_MENTION_REPLY": "mods"})
	tb.mk.spoke[tChGeneral] = time.Now().Add(-61 * time.Second)
	tb.say(tUser, tChGeneral, "hi", true)
	if count(tChGeneral) != 7 {
		t.Fatal("a reply to a non-mod mention with MARKOV_MENTION_REPLY mods")
	}
	if left := tb.mk.cooldownLeft(tChGeneral, time.Minute); left > 0 {
		t.Error("an ignored mention started the cooldown")
	}
	tb.say(tMod, tChGeneral, "hi", true)
	tb.say(tMod, tChGeneral, "hi", true)
	if count(tChGeneral) != 9 {
		t.Fatalf("replies to a mod with MARKOV_MENTION_REPLY mods = %d, want 2", count(tChGeneral)-7)
	}
	tb.run(tMod, "markov status")
	tb.expect(true, "Replies to @mentions from mods only")

	tb.mk.spoke[tChGeneral] = time.Now().Add(-61 * time.Second)
	tb.config(map[string]string{"MARKOV_REPLY_CHANCE": "0"})
	tb.say(tUser, tChGeneral, "quiet", false)
	if count(tChGeneral) != 9 {
		t.Error("talked with MARKOV_REPLY_CHANCE 0")
	}
}

// learn writes messages straight into the chain.
func learn(t *testing.T, tb *testBot, lines ...string) {
	t.Helper()
	kept, _ := markov.Prepare(lines, nil)
	if err := tb.chain.Learn(kept); err != nil {
		t.Fatal(err)
	}
}

func TestSetupChoice(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tAdmin, "setup")
	for page := 0; page < 20; page++ {
		r := tb.last()
		for _, c := range r.Components {
			if strings.HasPrefix(c.CustomID, "setup:choice:") && strings.HasSuffix(c.CustomID, ":MARKOV_MENTION_REPLY") {
				if !strings.Contains(r.text(), "`always`: Always reply") {
					t.Errorf("default not shown as always: %q", r.text())
				}
				tb.click(tAdmin, c.CustomID, "mods")
				if tb.cfg.Choice("MARKOV_MENTION_REPLY") != "mods" || tb.cfg.Get("MARKOV_MENTION_REPLY") != "mods" {
					t.Fatal("the menu did not save mods")
				}
				tb.expect(true, "`mods`: Reply only to mods")
				return
			}
		}
		tb.click(tAdmin, tb.componentID(r, "setup:next:"))
	}
	t.Fatal("no MARKOV_MENTION_REPLY menu in /setup")
}

// The old on/off setting moves to MARKOV_MENTION_REPLY when the bot starts.
func TestMentionReplyMigrate(t *testing.T) {
	for _, tc := range []struct{ old, newValue, want string }{
		{"off", "", "cooldown"},
		{"on", "", "always"},
		{"off", "mods", "mods"},
	} {
		tb := newTestBot(t)
		values := map[string]string{"MARKOV_ALWAYS_REPLY_MENTION": tc.old}
		if tc.newValue != "" {
			values["MARKOV_MENTION_REPLY"] = tc.newValue
		}
		if err := tb.db.ImportConfig(values, false); err != nil {
			t.Fatal(err)
		}
		b, err := New(tb.env, tb.db, tb.chain)
		if err != nil {
			t.Fatal(err)
		}
		saved, _ := tb.db.LoadConfig()
		if b.cfg.Choice("MARKOV_MENTION_REPLY") != tc.want || saved["MARKOV_MENTION_REPLY"] != tc.want {
			t.Errorf("old %q, new %q: got %q, saved %q, want %q", tc.old, tc.newValue,
				b.cfg.Choice("MARKOV_MENTION_REPLY"), saved["MARKOV_MENTION_REPLY"], tc.want)
		}
		if _, ok := saved["MARKOV_ALWAYS_REPLY_MENTION"]; ok {
			t.Error("the old key was not removed")
		}
	}
}

func TestMarkovBlocklistAndForget(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"MARKOV_LEARN_CHANNELS": tChGeneral})
	tb.say(tUser, tChGeneral, "i like cats a lot", true)
	tb.say(tUser, tChGeneral, "we like dogs", true)

	tb.run(tUser, "markov blocklist")
	tb.expect(true, "must be a moderator")

	tb.run(tMod, "markov blockadd", strOpt("text", "CATS!"))
	r := tb.expect(true, "`cats` is now blocked", "Remove **")
	tb.click(tMod, tb.componentID(r, "confirm:yes:mkforget:"))
	tb.expect(true, "word step(s) with `cats`")
	if n, _ := tb.chain.CountPhrase([]string{"cats"}); n != 0 {
		t.Errorf("%d steps with cats left", n)
	}

	// Blocked text is not learned.
	tb.say(tUser, tChGeneral, "more cats please", true)
	if n, _ := tb.chain.CountPhrase([]string{"please"}); n != 0 {
		t.Error("learned a message with a blocked word")
	}

	tb.run(tMod, "markov blockadd", strOpt("text", "cats"))
	tb.expect(true, "`cats` was already blocked", "does not contain")
	tb.run(tMod, "markov blocklist")
	tb.expect(true, "||cats||")

	tb.run(tMod, "markov forget", strOpt("text", "like dogs"))
	r = tb.expect(true, "Remove **2** word step(s)")
	tb.click(tMod, tb.componentID(r, "confirm:no:mkforget:"))
	tb.expect(true, "Cancelled")
	tb.run(tMod, "markov forget", strOpt("text", "<@1> https://x.io"))
	tb.expect(true, "no words to use")

	tb.run(tMod, "markov blockremove", strOpt("text", "Cats"))
	tb.expect(true, "no longer blocked")
	tb.run(tMod, "markov blockremove", strOpt("text", "cats"))
	tb.expect(true, "not on the blocklist")
}

func TestMarkovLookup(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"MARKOV_LEARN_CHANNELS": tChGeneral})
	tb.say(tUser, tChGeneral, "i like cats a lot", true)
	tb.say(tUser, tChGeneral, "nice 🍆!", true)

	tb.run(tUser, "markov lookup", strOpt("text", "cats"))
	tb.expect(true, "must be a moderator")

	tb.run(tMod, "markov lookup", strOpt("text", "Cats!"))
	tb.expect(true, "contains `cats`", "`cats` (1×)", "`like`", "`a`", "Blocked", "No")
	tb.run(tMod, "markov lookup", strOpt("text", "dogs"))
	tb.expect(true, "does not contain `dogs`")
	tb.run(tMod, "markov lookup", strOpt("text", "like cats"))
	tb.expect(true, "contains `like cats`", "Word steps")

	// An emoji: look it up, then forget it with the button.
	tb.run(tMod, "markov lookup", strOpt("text", "🍆"))
	r := tb.expect(true, "contains `🍆`", "`🍆!` (1×)")
	tb.click(tMod, tb.componentID(r, "mklookup:"))
	r = tb.expect(true, "Remove **")
	tb.click(tMod, tb.componentID(r, "confirm:yes:mkforget:"))
	tb.expect(true, "word step(s) with `🍆`")
	tb.run(tMod, "markov lookup", strOpt("text", "🍆"))
	tb.expect(true, "does not contain `🍆`")

	tb.run(tMod, "markov blockadd", strOpt("text", "cats"))
	tb.run(tMod, "markov lookup", strOpt("text", "cats"))
	tb.expect(true, "Yes: the bot does not learn or say it.")
}

func TestMarkovPrefixBlock(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"MARKOV_LEARN_CHANNELS": tChGeneral})
	tb.say(tUser, tChGeneral, "what the hecking heck", true)
	tb.say(tUser, tChGeneral, "check this out", true)

	tb.run(tMod, "markov blockadd", strOpt("text", "he*"))
	tb.expect(true, "`he*` is too short")
	tb.run(tMod, "markov lookup", strOpt("text", "Heck*"))
	tb.expect(true, "contains `heck*`", "Stored forms (times seen): 2", "`hecking` (1×)", "`heck` (1×)")

	tb.run(tMod, "markov blockadd", strOpt("text", "heck*"))
	r := tb.expect(true, "`heck*` is now blocked", "Remove **")
	tb.click(tMod, tb.componentID(r, "confirm:yes:mkforget:"))
	tb.expect(true, "word step(s) with `heck*`")
	tb.run(tMod, "markov lookup", strOpt("text", "heck*"))
	tb.expect(true, "does not contain `heck*`", "Yes: the bot does not learn or say it.")
	tb.run(tMod, "markov lookup", strOpt("text", "check"))
	tb.expect(true, "contains `check`")

	tb.say(tUser, tChGeneral, "oh heckers please", true)
	if n, _ := tb.chain.CountPhrase([]string{"please"}); n != 0 {
		t.Error("learned a message with a blocked prefix")
	}
}

func TestMarkovLearnLog(t *testing.T) {
	tb := newTestBot(t, withMessageContent)
	tb.config(map[string]string{"MARKOV_LEARN_CHANNELS": tChGeneral, "MARKOV_LOG_CHANNEL": tChSens})
	if _, err := tb.db.AddMarkovBlock("heck*", tMod); err != nil {
		t.Fatal(err)
	}
	if err := tb.loadMarkovBlocklist(); err != nil {
		t.Fatal(err)
	}

	tb.say(tUser, tChGeneral, "hello there friend", false)
	tb.say(tUser, tChGeneral, "what the hecking", false)
	tb.say(tUser, tChGeneral, "https://x.io", false)
	tb.say(tUser, tChVIP, "not a learn channel", false)
	// One embed for each message, with its text; no forward.
	sent := tb.sent(tChSens)
	if len(sent) != 3 || !strings.Contains(sent[0], "Learned** (3 words)") || !strings.Contains(sent[0], "hello there friend") ||
		!strings.Contains(sent[0], "<@"+tUser+">") || !strings.Contains(sent[1], "blocked by ||`heck*`||") ||
		!strings.Contains(sent[1], "what the hecking") || !strings.Contains(sent[2], "no words") {
		t.Fatalf("log = %q", sent)
	}
	for _, c := range tb.api.find("POST", "/channels/"+tChSens+"/messages") {
		if strings.Contains(string(c.Body), "message_reference") {
			t.Errorf("forwarded: %s", c.Body)
		}
	}

	tb.config(map[string]string{"MARKOV_LOG_MODE": "skipped"})
	tb.say(tUser, tChGeneral, "learned again", false)
	tb.say(tUser, tChGeneral, "heck", false)
	if sent := tb.sent(tChSens); len(sent) != 4 || !strings.Contains(sent[3], "heck*") {
		t.Fatalf("skipped mode log = %q", sent[3:])
	}
	tb.config(map[string]string{"MARKOV_LOG_MODE": "off"})
	tb.say(tUser, tChGeneral, "heck again", false)
	tb.say(tUser, tChGeneral, "learned once more", false)
	if sent := tb.sent(tChSens); len(sent) != 4 {
		t.Fatalf("off mode log = %q", sent[4:])
	}
}

// Without Message Content only mentions are read, so other messages are
// not logged.
func TestMarkovLearnLogWithoutMessageContent(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"MARKOV_LEARN_CHANNELS": tChGeneral, "MARKOV_LOG_CHANNEL": tChSens})
	tb.say(tUser, tChGeneral, "not read", false)
	tb.say(tUser, tChGeneral, "hello bot", true)
	if sent := tb.sent(tChSens); len(sent) != 1 || !strings.Contains(sent[0], "Learned** (2 words)") ||
		!strings.Contains(sent[0], "hello bot") {
		t.Fatalf("log = %q", sent)
	}
}

func TestMarkovImportPruneReset(t *testing.T) {
	tb := newTestBot(t)
	if _, err := tb.db.AddMarkovBlock("nope", tMod); err != nil {
		t.Fatal(err)
	}
	if err := tb.loadMarkovBlocklist(); err != nil {
		t.Fatal(err)
	}
	tb.run(tMod, "markov import", tb.attachmentOpt("file", "train.txt", "a b c\na b c\nnope nope\n\na b d\n"))
	tb.expect(true, "Learned **3** message(s) from `train.txt`. Skipped **1** blocked and **0** empty")
	if w, s := tb.markovStats(); w != 4 || s != 6 {
		t.Fatalf("stats = %d, %d", w, s)
	}
	tb.run(tMod, "markov import", tb.attachmentOpt("file", "bad.json", "{nope"))
	tb.expect(true, "Couldn't read that JSON file")

	tb.run(tMod, "markov status")
	tb.expect(true, "Words", "4", "learns only from messages that @mention the bot")

	tb.run(tMod, "markov prune")
	r := tb.expect(true, "Prune the Markov chain?")
	tb.click(tMod, tb.componentID(r, "confirm:yes:mkprune"))
	tb.expect(true, "Removed **2** word step(s)")

	tb.run(tMod, "markov reset")
	tb.expect(true, "don't have permission")
	tb.run(tAdmin, "markov reset")
	r = tb.expect(true, "Delete the whole Markov chain?")
	tb.click(tAdmin, tb.componentID(r, "confirm:yes:mkreset"))
	tb.expect(true, "now empty")
	if w, s := tb.markovStats(); w != 0 || s != 0 {
		t.Fatalf("after reset: %d, %d", w, s)
	}
	if bl, _ := tb.db.MarkovBlocklist(); len(bl) != 1 {
		t.Error("reset removed the blocklist")
	}
}

func TestSetupChannelList(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tAdmin, "setup")
	for page := 0; ; page++ {
		r := tb.last()
		for _, c := range r.Components {
			if strings.HasPrefix(c.CustomID, "setup:clist:") && strings.HasSuffix(c.CustomID, ":MARKOV_LEARN_CHANNELS") {
				tb.click(tAdmin, c.CustomID, tChGeneral, tChVIP)
				if got := tb.cfg.List("MARKOV_LEARN_CHANNELS"); len(got) != 2 {
					t.Fatalf("MARKOV_LEARN_CHANNELS = %v", got)
				}
				tb.expect(true, "<#"+tChGeneral+">, <#"+tChVIP+">")
				return
			}
		}
		if page > 20 {
			t.Fatal("no MARKOV_LEARN_CHANNELS select in /setup")
		}
		tb.click(tAdmin, tb.componentID(r, "setup:next:"))
	}
}

// A user mention is learned as the user's name, never as a mention.
func TestMarkovLearnsNamesNotMentions(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"MARKOV_LEARN_CHANNELS": tChGeneral})
	tb.seq++
	tb.onMessageCreate(tb.s, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "29999", ChannelID: tChGeneral, GuildID: tGuild, Author: &discordgo.User{ID: tUser, Username: "user"},
		Content: "<@" + tBot + "> say hi to <@!" + tMod + "> and <@" + tTarget + "> @Lurker",
		Mentions: []*discordgo.User{{ID: tBot, Username: "healthbot"}, {ID: tMod, Username: "moddy", GlobalName: "Big Mod"},
			{ID: tTarget, Username: "target"}},
	}})
	tb.flushMarkov()
	out, err := tb.chain.Generate([]string{"say"}, nil)
	if err != nil || out != "say hi to big mod and target lurker" {
		t.Fatalf("learned %q, %v", out, err)
	}
	if n, _ := tb.chain.CountPhrase([]string{"healthbot"}); n != 0 {
		t.Error("learned the bot's own name")
	}
}

func TestMarkovChanceText(t *testing.T) {
	for p, want := range map[float64]string{
		0:    "0%",
		0.05: "0.05% (about 1 in 2000)",
		1:    "1% (about 1 in 100)",
		25:   "25%",
		150:  "100%",
	} {
		if got := chanceText(p); got != want {
			t.Errorf("chanceText(%v) = %q, want %q", p, got, want)
		}
	}
}
