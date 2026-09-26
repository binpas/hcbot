package bot

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
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
		"MARKOV_REPLY_CHANCE": "100"})

	tb.say(tUser, tChGeneral, "not a mention", false)
	if w, _ := tb.markovStats(); w != 0 {
		t.Fatalf("learned %d words without a mention or Message Content", w)
	}
	if len(tb.markovSent(tChGeneral)) != 0 {
		t.Fatal("replied without a mention or Message Content")
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
	if err := tb.db.SetTrigger("hello", "hi!"); err != nil {
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
	tb.config(map[string]string{"MARKOV_LEARN_CHANNELS": tChGeneral, "MARKOV_REPLY_CHANNELS": tChGeneral})

	tb.say(tUser, tChGeneral, "every message is learned", false)
	if w, _ := tb.markovStats(); w != 4 {
		t.Fatalf("learned %d words, want 4", w)
	}
	if len(tb.markovSent(tChGeneral)) != 0 {
		t.Fatal("replied at random with MARKOV_REPLY_CHANCE 0")
	}
	tb.config(map[string]string{"MARKOV_REPLY_CHANCE": "100"})
	tb.say(tUser, tChGeneral, "message", false)
	sent := tb.markovSent(tChGeneral)
	if len(sent) != 1 || sent[0].Reference != nil {
		t.Fatalf("random reply = %+v", sent)
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
