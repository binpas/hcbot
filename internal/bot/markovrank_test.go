package bot

import (
	"net/http"
	"strings"
	"testing"
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
		if r.Context != "hello apple" || len(r.Candidates) != 2 {
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
