package bot

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/binpas/hcbot/internal/config"
	"github.com/binpas/hcbot/internal/markov"
)

const (
	shortReply = "apple short"
	longReply  = "apple much longer reply here"
)

// rankBot is a test bot with a fake worker, whose Markov chat replies to
// each mention with one of two replies (the fake worker likes the long one).
func rankBot(t *testing.T) (*testBot, *fakeTTS) {
	t.Helper()
	tb, f, _ := newTTSBot(t)
	tb.config(map[string]string{"MARKOV_REPLY_CHANNELS": tChGeneral, "MARKOV_COOLDOWN": "0", "MARKOV_PICK": "best"})
	learn(t, tb, shortReply, longReply)
	return tb, f
}

func (tb *testBot) lastMarkov() string {
	tb.t.Helper()
	sent := tb.markovSent(tChGeneral)
	if len(sent) == 0 {
		tb.t.Fatal("no Markov message")
	}
	return sent[len(sent)-1].Content
}

func TestMarkovRankBest(t *testing.T) {
	tb, f := rankBot(t)
	for range 5 {
		n := len(f.ranks)
		tb.say(tUser, tChGeneral, "hello apple", true)
		// The chain knows two replies, so its candidates can (by chance)
		// all be the same one. One candidate is not ranked.
		if len(f.ranks) == n {
			continue
		}
		if got := tb.lastMarkov(); got != longReply {
			t.Fatalf("reply = %q, want the best: %q", got, longReply)
		}
	}
	if len(f.ranks) < 3 {
		t.Errorf("only %d of 5 replies were ranked", len(f.ranks))
	}
	for _, r := range f.ranks {
		if r.Context != "hello apple" || len(r.Candidates) != 2 || !strings.HasPrefix(r.System, "This is a casual Discord chat.") {
			t.Errorf("rank request = %+v", r)
		}
	}
}

func TestMarkovRankTop3(t *testing.T) {
	tb, _ := rankBot(t)
	tb.config(map[string]string{"MARKOV_PICK": "top3"})
	seen := map[string]bool{}
	for range 40 {
		tb.say(tUser, tChGeneral, "apple", true)
		seen[tb.lastMarkov()] = true
	}
	if !seen[shortReply] || !seen[longReply] {
		t.Errorf("top3 with two candidates posted only %v", seen)
	}
}

func TestMarkovRankFallback(t *testing.T) {
	tb, f := rankBot(t)
	f.rankStatus = http.StatusInternalServerError
	tb.say(tUser, tChGeneral, "apple", true)
	if got := tb.lastMarkov(); got != shortReply && got != longReply {
		t.Errorf("reply = %q", got)
	}

	// One candidate: no ranking.
	f.rankStatus = 0
	tb.config(map[string]string{"MARKOV_CANDIDATES": "1"})
	n := len(f.ranks)
	tb.say(tUser, tChGeneral, "apple", true)
	if len(f.ranks) != n {
		t.Error("ranked with MARKOV_CANDIDATES 1")
	}
	tb.lastMarkov()
}

func TestMarkovNoWorker(t *testing.T) {
	tb := newTestBot(t)
	tb.config(map[string]string{"MARKOV_REPLY_CHANNELS": tChGeneral, "MARKOV_COOLDOWN": "0"})
	learn(t, tb, shortReply, longReply)
	tb.say(tUser, tChGeneral, "apple", true)
	tb.lastMarkov()
}

func TestMarkovCandidates(t *testing.T) {
	tb, _ := rankBot(t)
	tb.run(tUser, "markov candidates", strOpt("text", "apple"))
	tb.expect(true, "must be a moderator")

	tb.run(tMod, "markov candidates", strOpt("text", "apple"))
	r := tb.expect(true, "2 candidates", "1. `28.00` "+longReply, "2. `11.00` "+shortReply, "`best`")
	if strings.Count(r.text(), "\n1. ") != 1 {
		t.Errorf("reply = %q", r.text())
	}
}

// personaRankBot is a persona test bot with a fake worker. The persona
// chain knows two replies.
func personaRankBot(t *testing.T) (*testBot, *fakeTTS) {
	t.Helper()
	f := &fakeTTS{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	tb := newTestBot(t, withMessageContent, func(e *config.Env) { e.TTSURL, e.TTSToken = srv.URL, testTTSToken })
	tb.config(map[string]string{
		"PERSONA_ENABLED": "on", "PERSONA_USER": tTarget, "PERSONA_BOT_NAME": "Target-bot",
		"PERSONA_REPLY_CHANNELS": tChGeneral, "PERSONA_COOLDOWN": "0", "MARKOV_PICK": "best",
	})
	if _, err := tb.persona.LearnOnce([]markov.Item{
		{ID: "1", Words: markov.Tokenize(shortReply)}, {ID: "2", Words: markov.Tokenize(longReply)},
	}); err != nil {
		t.Fatal(err)
	}
	return tb, f
}

func TestPersonaRankQuestion(t *testing.T) {
	tb, f := personaRankBot(t)
	for range 5 {
		tb.say(tUser, tChGeneral, "can you ask target-bot how he feels about apple", true)
	}
	if len(f.ranks) == 0 {
		t.Fatal("no rank requests")
	}
	for _, r := range f.ranks {
		if r.Context != "how do you feel about apple?" {
			t.Errorf("context = %q", r.Context)
		}
		if want := "This is a casual Discord chat. Target-bot is one of the members. The next message is Target-bot's short answer to the question."; r.System != want {
			t.Errorf("system = %q", r.System)
		}
	}

	tb.config(map[string]string{"PERSONA_RANK_PROMPT": "{name} talks."})
	n := len(f.ranks)
	for range 5 {
		tb.run(tMod, "persona candidates", strOpt("text", "ask target-bot if he likes apple"))
	}
	r := tb.expect(true, "candidates", "The ranker gets:", "`do you like apple?`")
	if len(f.ranks) == n {
		t.Fatal("/persona candidates made no rank request")
	}
	if got := f.ranks[len(f.ranks)-1].System; got != "Target-bot talks." {
		t.Errorf("system = %q", got)
	}
	if !strings.Contains(r.text(), "`") {
		t.Errorf("reply = %q", r.text())
	}

	tb.run(tUser, "persona candidates", strOpt("text", "hi"))
	tb.expect(true, "must be a moderator")
}
