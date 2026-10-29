package bot

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/markov"
)

// Markov ranking: the chat makes MARKOV_CANDIDATES replies, and the GPU
// worker (tts/server.py, /rank) gives each one a score: how probable a
// small language model finds it as an answer to the message (or, with no
// message text, as a chat message). The bot posts the best one, or a
// random one of the best three (MARKOV_PICK). When the worker is not set,
// is slow, or fails, the bot posts the first candidate, as before.

const (
	markovRankTimeout = 3 * time.Second
	markovMaxCands    = 20 // the worker's limit
)

// rankedReply is one candidate and its score.
type rankedReply struct {
	text  string
	score float64
}

// markovGenerate makes a reply from the seed words, as an answer to
// msgText ("" when the bot cannot read the message).
func (b *Bot) markovGenerate(seed []string, msgText string) (string, error) {
	ok := func(w []string) bool { return !b.markovBlocks(w) }
	n := min(max(b.cfg.Int("MARKOV_CANDIDATES"), 1), markovMaxCands)
	if n == 1 || b.env.TTSURL == "" {
		return b.chain.Generate(seed, ok)
	}
	cands, err := b.chain.GenerateN(seed, ok, n)
	if len(cands) == 0 {
		return "", err
	}
	if err != nil {
		log.Printf("markov generate: %v", err) // use what was made
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	ranked, err := b.markovRank(msgText, cands)
	if err != nil {
		log.Printf("markov rank: %v", err)
		return cands[0], nil
	}
	top := 1
	if b.cfg.Choice("MARKOV_PICK") == "top3" {
		top = min(3, len(ranked))
	}
	return ranked[rand.IntN(top)].text, nil
}

// markovRank asks the worker for a score for each candidate, and returns
// the candidates with the best first.
func (b *Bot) markovRank(msgText string, cands []string) ([]rankedReply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), markovRankTimeout)
	defer cancel()
	defer func(start time.Time) { b.m.rankSeconds.Observe(time.Since(start).Seconds()) }(time.Now())
	resp, err := b.ttsDo(ctx, http.MethodPost, "/rank", map[string]any{"context": msgText, "candidates": cands})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worker: %s", resp.Status)
	}
	var out struct {
		Scores []float64 `json:"scores"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Scores) != len(cands) {
		return nil, fmt.Errorf("worker: %d scores for %d candidates", len(out.Scores), len(cands))
	}
	ranked := make([]rankedReply, len(cands))
	for i, c := range cands {
		ranked[i] = rankedReply{c, out.Scores[i]}
	}
	slices.SortStableFunc(ranked, func(x, y rankedReply) int { return cmp.Compare(y.score, x.score) })
	return ranked, nil
}

// cmdMarkovCandidates shows the candidates for a text and their scores,
// best first, to see how the picking works.
func (b *Bot) cmdMarkovCandidates(c *Ctx) {
	text, _ := c.Str("text")
	words := markov.Tokenize(text)
	seed := slices.Clone(words)
	slices.SortStableFunc(seed, func(x, y string) int { return len(y) - len(x) })
	seed = seed[:min(len(seed), 5)]
	n := min(max(b.cfg.Int("MARKOV_CANDIDATES"), 2), markovMaxCands)
	c.Defer(true)
	cands, err := b.chain.GenerateN(seed, func(w []string) bool { return !b.markovBlocks(w) }, n)
	if err != nil || len(cands) == 0 {
		c.Reply(true, errEmbed("The Markov chain made no reply. Is it empty?"))
		return
	}
	var lines []string
	note := ""
	if b.env.TTSURL == "" {
		note = "No worker (`TTS_URL`), so there are no scores."
	} else if ranked, err := b.markovRank(strings.Join(words, " "), cands); err != nil {
		log.Printf("markov candidates: %v", err)
		note = "The worker did not answer, so there are no scores. The chat posts the first one."
	} else {
		for i, r := range ranked {
			lines = append(lines, fmt.Sprintf("%d. `%.2f` %s", i+1, r.score, truncateEllipsis(r.text, 300)))
		}
		note = fmt.Sprintf("Higher is better. `MARKOV_PICK` is `%s`.", b.cfg.Choice("MARKOV_PICK"))
	}
	if lines == nil {
		for i, t := range cands {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, truncateEllipsis(t, 300)))
		}
	}
	e := makeEmbed(fmt.Sprintf("🎲 %d candidates", len(cands)), truncateEllipsis(strings.Join(lines, "\n"), 3800)+"\n\n"+note, colourBlurple)
	c.ReplyComplex(true, []*discordgo.MessageEmbed{e}, nil)
}
