package bot

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func personaBot(t *testing.T) *testBot {
	t.Helper()
	tb := newTestBot(t, withMessageContent)
	tb.config(map[string]string{
		"PERSONA_ENABLED": "on", "PERSONA_USER": tTarget, "PERSONA_BOT_NAME": "Target-bot",
		"PERSONA_REPLY_CHANNELS": tChGeneral, "PERSONA_SKIP_CATEGORY": tCatArchive, "PERSONA_COOLDOWN": "0",
		"MARKOV_LEARN_CHANNELS": tChGeneral, "MARKOV_REPLY_CHANNELS": tChGeneral, "MARKOV_COOLDOWN": "0",
	})
	return tb
}

func (tb *testBot) personaKnows(word string) bool {
	tb.t.Helper()
	tb.flushPersona()
	n, err := tb.persona.CountPhrase([]string{word})
	if err != nil {
		tb.t.Fatal(err)
	}
	return n > 0
}

func TestPersonaLearnsAndTalks(t *testing.T) {
	tb := personaBot(t)

	tb.say(tTarget, tChGeneral, "my guitar is very loud today", false)
	tb.say(tTarget, tChArchived, "secret mod words", false)
	tb.say(tUser, tChGeneral, "other people talk too", false)
	if !tb.personaKnows("guitar") {
		t.Error("did not learn the persona member's message")
	}
	if tb.personaKnows("secret") {
		t.Error("learned a message in PERSONA_SKIP_CATEGORY")
	}
	if tb.personaKnows("people") {
		t.Error("learned a message from another member")
	}
	if len(tb.markovSent(tChGeneral)) != 0 {
		t.Fatal("talked without a call or chance")
	}

	// The name (any case) gets a persona reply, also with a bot mention;
	// the normal Markov chat stays quiet.
	tb.say(tUser, tChGeneral, "hey target-BOT, what about the guitar?", true)
	sent := tb.markovSent(tChGeneral)
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1 persona reply", len(sent))
	}
	m := sent[0]
	if m.Content != "" || len(m.Embeds) != 1 || m.Reference == nil {
		t.Fatalf("persona post = %+v", m)
	}
	e := m.Embeds[0]
	if e.Color != colourRed || e.Author == nil || e.Author.Name != "Target-bot" || !strings.Contains(e.Description, "guitar") {
		t.Errorf("persona embed = %+v, author %+v", e, e.Author)
	}
	if am := m.AllowedMentions; am == nil || len(am.Parse) != 0 {
		t.Errorf("allowed mentions = %+v; want nobody pinged", am)
	}

	// Part of a word is not the name.
	tb.say(tUser, tChGeneral, "target-bots everywhere", false)
	if n := len(tb.markovSent(tChGeneral)); n != 1 {
		t.Errorf("replied to a longer word: %d messages", n)
	}

	// Not in a persona reply channel: no reply.
	tb.say(tUser, tChVIP, "target-bot?", false)
	if len(tb.markovSent(tChVIP)) != 0 {
		t.Error("replied outside PERSONA_REPLY_CHANNELS")
	}

	// /persona off: no learning, no talking.
	tb.run(tMod, "persona off")
	tb.expect(true, "off")
	tb.say(tTarget, tChGeneral, "banana", false)
	tb.say(tUser, tChGeneral, "target-bot banana", false)
	if tb.personaKnows("banana") {
		t.Error("learned while off")
	}
	for _, m := range tb.markovSent(tChGeneral)[1:] {
		if len(m.Embeds) > 0 {
			t.Error("the persona talked while off")
		}
	}
	tb.run(tMod, "persona on")
	tb.expect(true, "on")
}

func TestPersonaChance(t *testing.T) {
	tb := personaBot(t)
	tb.say(tTarget, tChGeneral, "hello there friends", false)
	tb.config(map[string]string{"PERSONA_CHANCE": "100"})
	tb.say(tUser, tChGeneral, "anything", false)
	sent := tb.markovSent(tChGeneral)
	if len(sent) != 1 || len(sent[0].Embeds) != 1 || sent[0].Embeds[0].Color != colourRed {
		t.Fatalf("chance reply = %+v", sent)
	}
	// The cooldown stops the next chance reply.
	tb.config(map[string]string{"PERSONA_COOLDOWN": "600"})
	tb.say(tUser, tChGeneral, "anything again", false)
	if n := len(tb.markovSent(tChGeneral)); n != 1 {
		t.Errorf("%d messages during the cooldown, want 1", n)
	}
}

func TestPersonaNeedsMessageContent(t *testing.T) {
	tb := newTestBot(t)
	if _, ok := tb.commands["persona status"]; ok {
		t.Error("/persona is registered without the Message Content option")
	}
}

func TestPersonaAvatar(t *testing.T) {
	tb := personaBot(t)
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 50)
	tb.run(tMod, "persona avatar", tb.attachmentOpt("file", "me.png", png))
	if files := tb.lastFiles(); string(files["persona.png"]) != png {
		t.Errorf("preview files = %v", files)
	}
	tb.run(tMod, "persona avatar", tb.attachmentOpt("file", "me.txt", "not an image"))
	tb.expect(true, "not a PNG")

	tb.say(tTarget, tChGeneral, "hello there", false)
	tb.say(tUser, tChGeneral, "target-bot", false)
	calls := tb.api.find("POST", "/channels/"+tChGeneral+"/messages")
	if len(calls) != 1 || string(calls[0].Files["persona.png"]) != png {
		t.Fatalf("persona post files = %v", calls)
	}
	if e := tb.markovSent(tChGeneral)[0].Embeds[0]; e.Author.IconURL != "attachment://persona.png" {
		t.Errorf("author icon = %q", e.Author.IconURL)
	}
}

func searchMsg(id, channel, author, text string) string {
	return fmt.Sprintf(`[{"id":%q,"channel_id":%q,"content":%q,"author":{"id":%q}}]`, id, channel, text, author)
}

func TestPersonaBackfill(t *testing.T) {
	oldPause, oldWait := personaSearchPause, personaIndexWait
	personaSearchPause, personaIndexWait = 0, time.Millisecond
	t.Cleanup(func() { personaSearchPause, personaIndexWait = oldPause, oldWait })

	tb := personaBot(t)
	tb.say(tTarget, tChGeneral, "already learned live", false)
	tb.flushPersona()
	live := tb.lastMessageID()

	path := "/guilds/" + tGuild + "/messages/search"
	// Newest route first: the fallback has no more results.
	tb.api.on("GET", path, http.StatusOK, `{"total_results":0,"messages":[]}`)
	page2 := fmt.Sprintf(`{"total_results":1,"messages":[%s],"threads":[{"id":"777","parent_id":%q,"type":11}]}`,
		searchMsg("1000", "777", tTarget, "an old thread message"), tChGeneral)
	page1 := fmt.Sprintf(`{"total_results":4,"messages":[%s,%s,%s]}`,
		searchMsg(live, tChGeneral, tTarget, "already learned live"),
		searchMsg("3000", tChGeneral, tTarget, "old pickle words"),
		searchMsg("2000", tChArchived, tTarget, "hidden mod talk"))
	tb.api.onBody("GET", path, "", 1, http.StatusOK, page2)
	tb.api.onBody("GET", path, "", 1, http.StatusOK, page1)
	tb.api.onBody("GET", path, "", 1, http.StatusAccepted, `{"code":110000,"retry_after":0}`)

	tb.run(tMod, "persona backfill")
	r := tb.expect(true, "Search the whole server")
	tb.click(tMod, tb.componentID(r, "confirm:yes:pbackfill:"))
	deadline := time.Now().Add(5 * time.Second)
	for tb.pr.backfill.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if tb.pr.backfill.Load() {
		t.Fatal("the backfill did not end")
	}
	tb.expect(false, "done", "Found **4**", "learned **2**", "skipped **1**")
	for result, want := range map[string]float64{"found": 4, "learned": 2, "skipped": 1} {
		if got := testutil.ToFloat64(tb.m.backfillMsgs.WithLabelValues(result)); got != want {
			t.Errorf("backfill %s = %v, want %v", result, got, want)
		}
	}
	if got := testutil.ToFloat64(tb.m.backfillRuns.WithLabelValues("done")); got != 1 {
		t.Errorf("backfill runs done = %v, want 1", got)
	}
	if !tb.personaKnows("pickle") || !tb.personaKnows("thread") {
		t.Error("did not learn the old messages")
	}
	if tb.personaKnows("hidden") {
		t.Error("learned a message in PERSONA_SKIP_CATEGORY")
	}

	calls := tb.api.find("GET", path)
	if len(calls) != 4 {
		t.Fatalf("%d search calls, want 4", len(calls))
	}
	for _, want := range []string{"author_id=" + tTarget, "sort_order=desc", "include_nsfw=true"} {
		if !strings.Contains(calls[1].Query, want) {
			t.Errorf("query %q has no %q", calls[1].Query, want)
		}
	}
	if !strings.Contains(calls[2].Query, "max_id=2000") || !strings.Contains(calls[3].Query, "max_id=1000") {
		t.Errorf("pages did not go back in time: %q, %q", calls[2].Query, calls[3].Query)
	}
	if cur, _ := tb.persona.Meta(personaCursorKey); cur != nil {
		t.Errorf("cursor = %q after the end, want none", cur)
	}

	tb.run(tMod, "persona status")
	tb.expect(true, "Messages learned", "3", "Not running")
}

func TestPersonaBackfillNeedsUser(t *testing.T) {
	tb := newTestBot(t, withMessageContent)
	tb.config(map[string]string{"PERSONA_ENABLED": "on"})
	tb.run(tMod, "persona backfill")
	tb.expect(true, "PERSONA_USER")
	if len(tb.api.find("GET", "/guilds/"+tGuild+"/messages/search")) != 0 {
		t.Error("searched without a user")
	}
}
