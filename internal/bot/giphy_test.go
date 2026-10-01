package bot

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/binpas/hcbot/internal/config"
)

func withGiphyKey(e *config.Env) { e.GiphyKey = "test-key" }

// fakeGiphy is a GIPHY search server. It records each query.
type fakeGiphy struct {
	mu      sync.Mutex
	queries []url.Values
	status  int
	body    string
	delay   time.Duration
}

func (f *fakeGiphy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.queries = append(f.queries, r.URL.Query())
	status, body, delay := f.status, f.body, f.delay
	f.mu.Unlock()
	time.Sleep(delay)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *fakeGiphy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

const giphyResult = `{"data":[{"images":{"downsized":{"url":"https://media.giphy.com/small.gif"},` +
	`"original":{"url":"https://media.giphy.com/big.gif"}}}]}`

// gifBot is a test bot whose Markov chat replies to each mention, with a
// fake GIPHY server.
func gifBot(t *testing.T, opts ...func(*config.Env)) (*testBot, *fakeGiphy) {
	tb := newTestBot(t, opts...)
	f := &fakeGiphy{status: http.StatusOK, body: giphyResult}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	tb.giphyURL = srv.URL
	tb.config(map[string]string{"MARKOV_REPLY_CHANNELS": tChGeneral, "MARKOV_COOLDOWN": "0",
		"MARKOV_GIF_CHANCE": "100"})
	learn(t, tb, "dancing cats are great")
	return tb, f
}

// lastGIF returns the GIF image URL of the last Markov message, or "".
func lastGIF(t *testing.T, tb *testBot) string {
	t.Helper()
	sent := tb.markovSent(tChGeneral)
	if len(sent) == 0 {
		t.Fatal("no Markov message")
	}
	m := sent[len(sent)-1]
	if m.Content == "" {
		t.Errorf("message has no text: %+v", m)
	}
	if len(m.Embeds) == 0 {
		return ""
	}
	if f := m.Embeds[0].Footer; f == nil || f.Text != "Powered by GIPHY" {
		t.Errorf("GIF embed has no GIPHY attribution: %+v", m.Embeds[0])
	}
	return m.Embeds[0].Image.URL
}

func TestMarkovGIF(t *testing.T) {
	tb, f := gifBot(t, withGiphyKey)

	tb.say(tUser, tChGeneral, "cats", true)
	if got := lastGIF(t, tb); got != "https://media.giphy.com/small.gif" {
		t.Errorf("GIF = %q", got)
	}
	if f.count() != 1 {
		t.Fatalf("GIPHY requests = %d", f.count())
	}
	q := f.queries[0]
	if q.Get("api_key") != "test-key" || q.Get("q") != tb.markovSent(tChGeneral)[0].Content || q.Get("limit") != "1" ||
		q.Get("rating") != "pg-13" {
		t.Errorf("query = %v", q)
	}

	// The cooldown is for the full bot: the next reply has no GIF and
	// sends no request.
	tb.say(tUser, tChGeneral, "cats", true)
	if got := lastGIF(t, tb); got != "" || f.count() != 1 {
		t.Errorf("GIF during the cooldown: %q, %d requests", got, f.count())
	}

	// With no downsized image, the original is used. The rating setting
	// is sent.
	tb.config(map[string]string{"MARKOV_GIF_COOLDOWN": "0", "MARKOV_GIF_RATING": "g"})
	f.body = `{"data":[{"images":{"original":{"url":"https://media.giphy.com/big.gif"}}}]}`
	tb.say(tUser, tChGeneral, "cats", true)
	if got := lastGIF(t, tb); got != "https://media.giphy.com/big.gif" {
		t.Errorf("GIF = %q", got)
	}
	if r := f.queries[1].Get("rating"); r != "g" {
		t.Errorf("rating = %q", r)
	}

	tb.run(tMod, "markov status")
	tb.expect(true, "GIFs", "100%", "Rating up to **g**")
}

func TestMarkovGIFFailures(t *testing.T) {
	cases := map[string]func(*fakeGiphy, *testBot){
		"error status": func(f *fakeGiphy, _ *testBot) { f.status = http.StatusTooManyRequests },
		"no results":   func(f *fakeGiphy, _ *testBot) { f.body = `{"data":[]}` },
		"bad body":     func(f *fakeGiphy, _ *testBot) { f.body = `not json` },
		"too slow": func(f *fakeGiphy, tb *testBot) {
			f.delay = 200 * time.Millisecond
			tb.giphyHTTP = &http.Client{Timeout: 20 * time.Millisecond}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			tb, f := gifBot(t, withGiphyKey)
			setup(f, tb)
			tb.say(tUser, tChGeneral, "cats", true)
			if got := lastGIF(t, tb); got != "" {
				t.Errorf("GIF = %q", got)
			}
		})
	}
}

func TestMarkovGIFOff(t *testing.T) {
	// No API key: no request.
	tb, f := gifBot(t)
	tb.say(tUser, tChGeneral, "cats", true)
	if lastGIF(t, tb) != "" || f.count() != 0 {
		t.Errorf("GIF without a key: %d requests", f.count())
	}
	tb.run(tMod, "markov status")
	tb.expect(true, "GIPHY_API_KEY")

	// Chance 0: no request.
	tb, f = gifBot(t, withGiphyKey)
	tb.config(map[string]string{"MARKOV_GIF_CHANCE": "0"})
	tb.say(tUser, tChGeneral, "cats", true)
	if lastGIF(t, tb) != "" || f.count() != 0 {
		t.Errorf("GIF with chance 0: %d requests", f.count())
	}
}

func TestGIFQuery(t *testing.T) {
	long := strings.Repeat("word ", 20)
	for in, want := range map[string]string{
		"  short   text ":              "short text",
		long:                           strings.TrimSpace(strings.Repeat("word ", 10)),
		strings.Repeat("x", 60):        strings.Repeat("x", 50),
		strings.Repeat("é", 48) + " b": strings.Repeat("é", 48) + " b",
		strings.Repeat("é", 49) + " b": strings.Repeat("é", 49),
	} {
		if got := gifQuery(in); got != want {
			t.Errorf("gifQuery(%q) = %q, want %q", in, got, want)
		}
		if n := len([]rune(gifQuery(in))); n > giphyMaxQuery {
			t.Errorf("gifQuery(%q) has %d characters", in, n)
		}
	}
}
