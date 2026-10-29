package bot

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAPIRoute(t *testing.T) {
	for raw, want := range map[string]string{
		"https://discord.com/api/v10/channels/123456789012345678/messages":                                "api /channels/:id/messages",
		"https://discord.com/api/v10/channels/1234567890123/messages/98765432101/reactions/%E2%AD%90/@me": "api /channels/:id/messages/:id/reactions/:emoji/@me",
		"https://discord.com/api/v10/interactions/1234567890123/aW50ZXJhY3Rpb246MTIzNDU2/callback":        "api /interactions/:id/:token/callback",
		"https://discord.com/api/v10/webhooks/1234567890123/abc.def-ghi/messages/@original":               "api /webhooks/:id/:token/messages/@original",
		"https://cdn.discordapp.com/attachments/1/2/cat.png":                                              "cdn /attachments",
		"https://example.com/secret/path":                                                                 "other other",
	} {
		u, _ := url.Parse(raw)
		host, route := apiRoute(&http.Request{URL: u})
		if got := host + " " + route; got != want {
			t.Errorf("%s: %q, want %q", raw, got, want)
		}
	}
}

func TestMetrics(t *testing.T) {
	tb, _ := newComicBot(t)
	tb.config(map[string]string{"COMIC_COOLDOWN": "0"})
	tb.api.on("GET", `/channels/`+tChGeneral+`/messages`, 200, chatJSON(1001, 1002))
	tb.run(tUser, "comic", intOpt("count", 2))
	tb.click(tUser, "comicpre:999") // an expired editor
	tb.say(tUser, tChGeneral, "hello", false)

	m := tb.m
	if got := testutil.ToFloat64(m.interactions.WithLabelValues("command", "comic", "answered")); got != 1 {
		t.Errorf("comic interactions = %v", got)
	}
	if got := testutil.ToFloat64(m.interactions.WithLabelValues("component", "comicpre", "answered")); got != 1 {
		t.Errorf("comicpre interactions = %v", got)
	}
	if got := testutil.ToFloat64(m.comics.WithLabelValues("slash", "ok")); got != 1 {
		t.Errorf("comics = %v", got)
	}
	if n := testutil.CollectAndCount(m.handlerSecs); n != 1 {
		t.Errorf("handler series = %d, want 1 (message)", n)
	}

	addr, err := tb.serveMetrics("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{`hcbot_build_info{version="dev"} 1`, `hcbot_interactions_total{kind="command",name="comic",result="answered"} 1`,
		"hcbot_interaction_answer_seconds_bucket", "hcbot_db_bytes", "hcbot_nick_cache_entries", "go_goroutines", "hcbot_gateway_latency_seconds"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
}
