package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/binpas/hcbot/internal/comic"
	"github.com/binpas/hcbot/internal/config"
)

// newAreasTest is a web test with a fake speech worker and the chats on.
func newAreasTest(t *testing.T) (*webTest, *fakeTTS) {
	t.Helper()
	f := &fakeTTS{voices: []string{"alex", "sam"}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if err := comic.WriteTestPack(dir); err != nil {
		t.Fatal(err)
	}
	tb := newTestBot(t, func(e *config.Env) {
		e.MessageContent, e.ComicArtDir, e.WebURL, e.WebSecret, e.WebAddr = true, dir, webTestURL, "the-secret", ":0"
		e.TTSURL, e.TTSToken = srv.URL, testTTSToken
	})
	tb.ttsPollEvery = 0
	tb.config(map[string]string{"MOD_ROLE": tRoleMod, "MOD_LOG": tChModLog,
		"MARKOV_LEARN_CHANNELS": tChGeneral, "MARKOV_REPLY_CHANNELS": tChGeneral, "MARKOV_CANDIDATES": "3",
		"PERSONA_ENABLED": "on", "PERSONA_USER": tTarget, "PERSONA_BOT_NAME": "Target-bot",
		"PERSONA_REPLY_CHANNELS": tChGeneral, "TTS_ENABLED": "on", "TTS_COOLDOWN": "0"})
	tb.runTool = func(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
		return nil, nil, os.WriteFile(args[len(args)-1], testWAV(8000, 800, 1000), 0o600)
	}
	tb.web.client = tb.s.Client
	tb.api.on("GET", "^/guilds/"+tGuild+"$", 200, fmt.Sprintf(`{"id":%q,"owner_id":%q}`, tGuild, tOwner))
	tb.api.on("GET", "^/guilds/"+tGuild+"/roles$", 200, fmt.Sprintf(
		`[{"id":%q,"permissions":"0"},{"id":%q,"permissions":"0"},{"id":"77","permissions":"8"}]`, tGuild, tRoleMod))
	w := &webTest{testBot: tb, h: tb.webHandler()}
	w.member(tMod, tRoleMod)
	return w, f
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return v
}

func TestWebSettings(t *testing.T) {
	w, _ := newAreasTest(t)
	mod := w.session(tMod)
	w.member(tOwner)
	admin := w.session(tOwner)

	type settingsBody struct {
		Sections []webSection `json:"sections"`
	}
	body := decode[settingsBody](t, w.do("GET", "/api/settings", mod, nil, ""))
	seen := map[string]webSetting{}
	for _, sec := range body.Sections {
		for _, k := range sec.Settings {
			seen[k.Key] = k
		}
	}
	if len(seen) != len(config.SetupKeys) {
		t.Errorf("%d settings on the page, want %d", len(seen), len(config.SetupKeys))
	}
	if seen["MOD_ROLE"].Value != tRoleMod || seen["MARKOV_PICK"].Choices == nil {
		t.Errorf("settings = %+v, %+v", seen["MOD_ROLE"], seen["MARKOV_PICK"])
	}

	// A mod can read but not change.
	wantStatus(t, w.json("PUT", "/api/settings/TTS_COOLDOWN", mod, map[string]string{"value": "5"}), http.StatusForbidden, "a mod edits")
	if w.cfg.Str("TTS_COOLDOWN") != "0" {
		t.Error("a mod changed a setting")
	}
	set := func(key, value string, want int) {
		t.Helper()
		wantStatus(t, w.json("PUT", "/api/settings/"+key, admin, map[string]string{"value": value}), want, key+"="+value)
	}
	set("TTS_COOLDOWN", "45", 200)
	if w.cfg.Str("TTS_COOLDOWN") != "45" {
		t.Errorf("TTS_COOLDOWN = %q", w.cfg.Str("TTS_COOLDOWN"))
	}
	if got := w.sent(tChModLog); !contains(got, "TTS_COOLDOWN") || !contains(got, "45") {
		t.Errorf("mod log = %v", got)
	}
	set("NOPE", "1", 404)
	set("TTS_COOLDOWN", "soon", 400)
	set("TTS_ENABLED", "maybe", 400)
	set("MARKOV_PICK", "worst", 400)
	set("MOD_LOG", "424242", 400)      // no such channel
	set("MOD_LOG", tCatArchive, 400)   // a category, not a channel
	set("TICKET_CAT", tChGeneral, 400) // a channel, not a category
	set("MOD_ROLE", "424242", 400)     // no such role
	set("TTS_CHANNELS", tChGeneral+",424242", 400)
	set("PERSONA_USER", "not-an-id", 400)
	set("MARKOV_RANK_PROMPT", strings.Repeat("x", 501), 400)
	set("TTS_CHANNELS", tChGeneral+","+tChGeneral, 200)
	if got := w.cfg.Str("TTS_CHANNELS"); got != tChGeneral {
		t.Errorf("TTS_CHANNELS = %q, want no second copy", got)
	}
	set("TTS_CHANNELS", "", 200)
	if w.cfg.Str("TTS_CHANNELS") != "" {
		t.Error("the list was not cleared")
	}
	wantStatus(t, w.do("GET", "/api/guild", mod, nil, ""), 200, "guild")
}

func TestWebSpeechSend(t *testing.T) {
	w, f := newAreasTest(t)
	mod := w.session(tMod)
	type speechBody struct {
		Voices   []string   `json:"voices"`
		Channels []webNamed `json:"channels"`
		MaxChars int        `json:"maxChars"`
	}
	body := decode[speechBody](t, w.do("GET", "/api/speech", mod, nil, ""))
	if len(body.Voices) != 2 || !webHasChannel(body.Channels, tChGeneral) || body.MaxChars == 0 {
		t.Fatalf("speech = %+v", body)
	}
	send := func(text, voice, ch string) *httptest.ResponseRecorder {
		return w.json("POST", "/api/speech/send", mod, map[string]any{"text": text, "voice": voice, "channel": ch, "voiceMsg": false})
	}
	wantStatus(t, send("", "alex", tChGeneral), 400, "no text")
	wantStatus(t, send("hi", "alex", ""), 400, "no channel")
	wantStatus(t, send(strings.Repeat("x", body.MaxChars+1), "alex", tChGeneral), 400, "too long")

	// Only the channels of TTS_CHANNELS.
	w.config(map[string]string{"TTS_CHANNELS": tChVIP})
	wantStatus(t, send("hi", "alex", tChGeneral), 403, "not in TTS_CHANNELS")
	w.config(map[string]string{"TTS_CHANNELS": ""})

	rec := send("hello web", "alex", tChGeneral)
	wantStatus(t, rec, http.StatusAccepted, "send")
	w.waitTTSJobs()
	if len(f.speaks) != 1 || f.speaks[0]["text"] != "hello web" {
		t.Errorf("worker got %v", f.speaks)
	}
	if calls := w.api.find("POST", "/channels/"+tChGeneral+"/messages"); len(calls) != 1 {
		t.Errorf("%d posts in the channel", len(calls))
	}
	if n := len(w.api.find("PATCH", "/webhooks/.*")); n != 0 {
		t.Errorf("a web job tried to edit an interaction: %d calls", n)
	}
	jobs := decode[[]webJob](t, w.do("GET", "/api/speech/jobs", mod, nil, ""))
	if len(jobs) != 1 || jobs[0].State != "done" {
		t.Errorf("jobs = %+v", jobs)
	}
	if got := w.sent(tChModLog); len(got) == 0 {
		t.Error("no mod log")
	}

	// The cooldown of /say also works here.
	w.config(map[string]string{"TTS_COOLDOWN": "600"})
	wantStatus(t, send("two", "alex", tChGeneral), http.StatusTooManyRequests, "in the cooldown")

	// A failed job shows on the page.
	w.config(map[string]string{"TTS_COOLDOWN": "0"})
	f.mu.Lock()
	f.steps = []ttsJobState{{State: "failed", Error: "x"}}
	f.mu.Unlock()
	wantStatus(t, send("fails", "alex", tChGeneral), http.StatusAccepted, "failing")
	w.waitTTSJobs()
	jobs = decode[[]webJob](t, w.do("GET", "/api/speech/jobs", mod, nil, ""))
	if jobs[0].State != "failed" || jobs[0].Text != "fails" {
		t.Errorf("newest job = %+v", jobs[0])
	}
	// Another member does not see these jobs.
	w.member(tOwner)
	if got := decode[[]webJob](t, w.do("GET", "/api/speech/jobs", w.session(tOwner), nil, "")); len(got) != 0 {
		t.Errorf("another member sees %+v", got)
	}
}

func TestWebSpeechOff(t *testing.T) {
	w, _ := newAreasTest(t)
	mod := w.session(tMod)
	w.config(map[string]string{"TTS_ENABLED": "off"})
	wantStatus(t, w.do("GET", "/api/speech", mod, nil, ""), http.StatusServiceUnavailable, "speech off")
	type meBody struct {
		Off map[string]string `json:"off"`
	}
	if m := decode[meBody](t, w.do("GET", "/api/me", mod, nil, "")); m.Off["speech"] == "" || m.Off["markov"] != "" {
		t.Errorf("off = %v", m.Off)
	}
}

func TestWebClips(t *testing.T) {
	w, _ := newAreasTest(t)
	mod := w.session(tMod)
	wantStatus(t, w.do("GET", "/api/speech/days", mod, nil, ""), 200, "days")
	rec := w.do("GET", "/api/speech/clips?day=2026-11-04", mod, nil, "")
	wantStatus(t, rec, 200, "clips")
	if !strings.Contains(rec.Body.String(), "hello there") {
		t.Errorf("clips = %s", rec.Body)
	}
	wantStatus(t, w.do("GET", "/api/speech/clips?day=../../etc", mod, nil, ""), 400, "a bad day")
	wantStatus(t, w.do("GET", "/api/speech/clips?day=2020-01-01", mod, nil, ""), 404, "an unknown day")
	rec = w.do("GET", "/audio/clip/2026-11-04/120000-abc_alex_hello.ogg", mod, nil, "")
	wantStatus(t, rec, 200, "audio")
	if rec.Header().Get("Content-Type") != "audio/ogg" || rec.Body.String() != "OggS saved" {
		t.Errorf("audio = %q %q", rec.Header().Get("Content-Type"), rec.Body)
	}
	wantStatus(t, w.do("GET", "/audio/clip/2026-11-04/..%2F..%2Fsecret.ogg", mod, nil, ""), 404, "a bad file")
	wantStatus(t, w.do("GET", "/audio/clip/2026-11-04/x.ogg", nil, nil, ""), 401, "no login")
	if !strings.Contains(w.do("GET", "/", nil, nil, "").Header().Get("Content-Security-Policy"), "media-src 'self'") {
		t.Error("the page cannot play audio")
	}

	post := func(file, ch string, voice bool) *httptest.ResponseRecorder {
		return w.json("POST", "/api/speech/clips/post", mod, map[string]any{"day": "2026-11-04", "file": file, "channel": ch, "voiceMsg": voice})
	}
	wantStatus(t, post("../x.ogg", tChGeneral, false), 400, "a bad file")
	wantStatus(t, post("120000-abc_alex_hello.ogg", "", false), 400, "no channel")
	w.config(map[string]string{"TTS_CHANNELS": tChVIP})
	wantStatus(t, post("120000-abc_alex_hello.ogg", tChGeneral, false), 403, "not an approved channel")
	w.config(map[string]string{"TTS_CHANNELS": ""})

	w.api.reset()
	wantStatus(t, post("120000-abc_alex_hello.ogg", tChGeneral, false), 200, "post a clip")
	calls := w.api.find("POST", "/channels/"+tChGeneral+"/messages")
	if len(calls) != 1 || string(calls[0].Files["say.ogg"]) != "OggS saved" {
		t.Fatalf("post = %+v", calls)
	}
	if got := w.sent(tChModLog); !contains(got, "hello there") {
		t.Errorf("mod log = %v", got)
	}
	// An old WAV clip is encoded first.
	w.api.reset()
	wantStatus(t, post("110000-def_sam_old.wav", tChGeneral, false), 200, "post a WAV clip")
	if calls := w.api.find("POST", "/channels/"+tChGeneral+"/messages"); len(calls) != 1 || len(calls[0].Files["say.ogg"]) == 0 {
		t.Errorf("WAV post = %+v", calls)
	}
}

func TestWebVoices(t *testing.T) {
	w, f := newAreasTest(t)
	mod := w.session(tMod)
	w.member(tOwner)
	admin := w.session(tOwner)
	file := func(c *http.Cookie, name, filename, data string, replace bool) *httptest.ResponseRecorder {
		fields := map[string]string{"name": name}
		if replace {
			fields["replace"] = "true"
		}
		return w.uploadNamed("/api/speech/voices", c, filename, []byte(data), fields)
	}
	wantStatus(t, file(mod, "newv", "a.mp3", "audio", false), http.StatusForbidden, "a mod uploads")
	wantStatus(t, file(admin, "Bad Name!", "a.mp3", "audio", false), 400, "a bad name")
	wantStatus(t, file(admin, "random", "a.mp3", "audio", false), 400, "the name random")
	wantStatus(t, file(admin, "newv", "a.exe", "audio", false), 400, "a bad type")
	wantStatus(t, file(admin, "broken", "a.mp3", "not audio", false), 400, "not audio")
	wantStatus(t, file(admin, "alex", "a.mp3", "audio", false), http.StatusConflict, "the name is in use")
	rec := file(admin, "NewV", "a.MP3", "audio", false)
	wantStatus(t, rec, 200, "upload")
	last := f.uploads[len(f.uploads)-1]
	if last.name != "newv" || last.ext != ".mp3" || string(last.data) != "audio" || last.replace {
		t.Errorf("upload = %+v", last)
	}
	if got := w.sent(tChModLog); !contains(got, "saved the voice") {
		t.Errorf("mod log = %v", got)
	}
	wantStatus(t, file(admin, "alex", "a.wav", "audio", true), 200, "replace")
	if !f.uploads[len(f.uploads)-1].replace {
		t.Error("replace did not reach the worker")
	}
	wantStatus(t, w.do("DELETE", "/api/speech/voices/alex", mod, nil, ""), http.StatusForbidden, "a mod removes")
	wantStatus(t, w.do("DELETE", "/api/speech/voices/nobody", admin, nil, ""), 404, "an unknown voice")
	wantStatus(t, w.do("DELETE", "/api/speech/voices/alex", admin, nil, ""), 200, "remove")
	// The voice list of /say has the change at once.
	if names, _ := w.ttsVoices(t.Context(), false); strings.Contains(strings.Join(names, " "), "alex") {
		t.Errorf("voices = %v", names)
	}
}

type chatBody struct {
	Candidates []webCandOut `json:"candidates"`
	Channels   []webNamed   `json:"channels"`
	Question   string       `json:"question"`
}

func TestWebChat(t *testing.T) {
	for _, chain := range []string{"markov", "persona"} {
		t.Run(chain, func(t *testing.T) {
			w, f := newAreasTest(t)
			mod := w.session(tMod)
			speaker := tUser
			if chain == "persona" {
				speaker = tTarget
			}
			for range 3 {
				w.say(speaker, tChGeneral, "my guitar is very loud today and my guitar is red", false)
			}
			w.flushMarkov()
			w.flushPersona()
			gen := func(text string) *httptest.ResponseRecorder {
				return w.json("POST", "/api/chat/"+chain+"/generate", mod, map[string]string{"text": text})
			}
			wantStatus(t, gen(""), 400, "no text")
			rec := gen("ask target-bot how he feels about the guitar")
			wantStatus(t, rec, 200, "generate")
			out := decode[chatBody](t, rec)
			if len(out.Candidates) == 0 || !webHasChannel(out.Channels, tChGeneral) {
				t.Fatalf("generate = %s", rec.Body)
			}
			if out.Candidates[0].Score == nil || len(f.ranks) == 0 {
				t.Errorf("no scores: %s", rec.Body)
			}
			if chain == "persona" && !strings.Contains(out.Question, "you") {
				t.Errorf("question = %q", out.Question)
			}
			id := out.Candidates[0].ID
			send := func(c *http.Cookie, id, ch string) *httptest.ResponseRecorder {
				return w.json("POST", "/api/chat/"+chain+"/send", c, map[string]string{"id": id, "channel": ch})
			}
			wantStatus(t, send(mod, id, tChVIP), http.StatusForbidden, "not an approved channel")
			wantStatus(t, send(mod, "made-up", tChGeneral), http.StatusGone, "not a candidate: any text cannot be sent")
			w.member(tOwner)
			wantStatus(t, send(w.session(tOwner), id, tChGeneral), http.StatusGone, "another member's candidate")
			other := "markov"
			if chain == "markov" {
				other = "persona"
			}
			wantStatus(t, w.json("POST", "/api/chat/"+other+"/send", mod, map[string]string{"id": id, "channel": tChGeneral}), http.StatusGone, "the other chain")
			w.api.reset()
			wantStatus(t, send(mod, id, tChGeneral), 200, "send")
			calls := w.api.find("POST", "/channels/"+tChGeneral+"/messages")
			if len(calls) != 1 || !strings.Contains(string(calls[0].Body), out.Candidates[0].Text[:5]) {
				t.Fatalf("post = %+v", calls)
			}
			if got := w.sent(tChModLog); !contains(got, "reply to") {
				t.Errorf("mod log = %v", got)
			}
			wantStatus(t, send(mod, id, tChGeneral), http.StatusGone, "the same candidate again")

			// With an empty list, no channel is approved.
			w.config(map[string]string{"MARKOV_REPLY_CHANNELS": "", "PERSONA_REPLY_CHANNELS": ""})
			if got := decode[chatBody](t, gen("the guitar")); len(got.Channels) != 0 {
				t.Errorf("channels = %+v", got.Channels)
			}

			// With no worker answer, the candidates still show.
			w.config(map[string]string{"MARKOV_REPLY_CHANNELS": tChGeneral, "PERSONA_REPLY_CHANNELS": tChGeneral})
			f.mu.Lock()
			f.rankStatus = 500
			f.mu.Unlock()
			if got := decode[chatBody](t, gen("the guitar")); len(got.Candidates) == 0 || got.Candidates[0].Score != nil {
				t.Errorf("no worker: %+v", got)
			}
		})
	}
}

func TestWebChatOff(t *testing.T) {
	w, _ := newAreasTest(t)
	mod := w.session(tMod)
	w.config(map[string]string{"PERSONA_ENABLED": "off"})
	wantStatus(t, w.json("POST", "/api/chat/persona/generate", mod, map[string]string{"text": "hi"}), http.StatusServiceUnavailable, "persona off")
	wantStatus(t, w.json("POST", "/api/chat/nothing/generate", mod, map[string]string{"text": "hi"}), http.StatusServiceUnavailable, "unknown chain")
}
