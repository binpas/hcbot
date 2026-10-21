package bot

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/tweetshot"
)

func newTweetshotBot(t *testing.T) *testBot {
	tb := newTestBot(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("id") {
		case "111":
			_, _ = w.Write([]byte(`{"__typename":"Tweet","id_str":"111","text":"Hello there","display_text_range":[0,11],
				"created_at":"2026-10-21T14:05:00.000Z","favorite_count":3,"conversation_count":0,
				"user":{"name":"Poster","screen_name":"poster"},"entities":{}}`))
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	tb.tweets = &tweetshot.Client{HTTP: srv.Client(), DataURL: srv.URL + "/tweet-result"}
	return tb
}

func TestStatusID(t *testing.T) {
	for in, want := range map[string]string{
		"https://x.com/poster/status/111":                      "111",
		"https://twitter.com/poster/status/111?s=20":           "111",
		"<https://www.x.com/poster/status/111/photo/1>":        "111",
		"https://mobile.twitter.com/poster/statuses/111":       "111",
		"https://fxtwitter.com/poster/status/111":              "111",
		"https://vxtwitter.com/i/web/status/111":               "111",
		"https://x.com/i/status/111":                           "111",
		"https://x.com/poster":                                 "",
		"https://example.com/poster/status/111":                "",
		"https://x.com.evil.example/poster/status/111":         "",
		"ftp://x.com/poster/status/111":                        "",
		"https://x.com/poster/status/111111111111111111111111": "",
	} {
		if got := statusID(in); got != want {
			t.Errorf("statusID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTweetshotEphemeral(t *testing.T) {
	tb := newTweetshotBot(t)
	tb.run(tUser, "tweetshot", strOpt("url", "https://x.com/poster/status/111"))
	tb.expect(true, "must be a moderator")

	tb.run(tMod, "tweetshot", strOpt("url", "https://x.com/poster/status/111"))
	tb.expect(true, "https://x.com/poster/status/111")
	if d := tb.replies()[len(tb.replies())-2]; d.Kind != "defer" || !d.Ephemeral {
		t.Errorf("defer = %+v, want ephemeral", d)
	}
	if got := tb.uploaded(); got["post-111.png"] == 0 {
		t.Errorf("uploaded = %v", got)
	}
}

func TestTweetshotPublic(t *testing.T) {
	tb := newTweetshotBot(t)
	tb.run(tMod, "tweetshot", strOpt("url", "https://twitter.com/poster/status/111"),
		&discordgo.ApplicationCommandInteractionDataOption{Name: "public", Type: discordgo.ApplicationCommandOptionBoolean, Value: true})
	r := tb.last()
	if strings.Contains(r.text(), "x.com") {
		t.Errorf("public reply has a link: %q", r.text())
	}
	if d := tb.replies()[len(tb.replies())-2]; d.Kind != "defer" || d.Ephemeral {
		t.Errorf("defer = %+v, want public", d)
	}
	if got := tb.uploaded(); got["post-111.png"] == 0 {
		t.Errorf("uploaded = %v", got)
	}
}

func TestTweetshotErrors(t *testing.T) {
	tb := newTweetshotBot(t)
	tb.run(tMod, "tweetshot", strOpt("url", "https://example.com/poster/status/111"))
	tb.expect(true, "not a link to a post on X")

	tb.run(tMod, "tweetshot", strOpt("url", "https://x.com/poster/status/404"))
	tb.expect(true, "deleted, protected, or age-limited")

	tb.run(tMod, "tweetshot", strOpt("url", "https://x.com/poster/status/500"))
	tb.expect(true, "could not get the post")
	if len(tb.uploaded()) != 0 {
		t.Error("a file was sent")
	}
}

func TestTweetshotFromMessage(t *testing.T) {
	tb := newTweetshotBot(t)
	tb.runMessageMenu(tMod, tweetshotMenu, &discordgo.Message{ID: "8001", ChannelID: tChGeneral,
		Content: "see https://example.com/a and https://x.com/poster/status/111"})
	tb.expect(true, "https://x.com/poster/status/111")
	if got := tb.uploaded(); got["post-111.png"] == 0 {
		t.Errorf("uploaded = %v", got)
	}

	tb.runMessageMenu(tMod, tweetshotMenu, &discordgo.Message{ID: "8002", ChannelID: tChGeneral, Content: "https://example.com/a"})
	tb.expect(true, "no link to a post on X")

	tb.runMessageMenu(tUser, tweetshotMenu, &discordgo.Message{ID: "8003", ChannelID: tChGeneral, Content: "https://x.com/poster/status/111"})
	tb.expect(true, "must be a moderator")
}
