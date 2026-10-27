package bot

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/config"
)

const testTTSToken = "test-token-0123456789abcdef0123456789"

// fakeTTS is the text to speech worker. It records each /tts request.
type fakeTTS struct {
	mu      sync.Mutex
	voices  []string
	speaks  []map[string]string
	reloads int
	status  int // for /tts; 0 is 200

	ranks      []rankRequest
	rankStatus int // for /rank; 0 is 200
}

// rankRequest is one /rank request. The fake gives each candidate its
// length as the score, so the longest is the best.
type rankRequest struct {
	Context    string   `json:"context"`
	Candidates []string `json:"candidates"`
}

func (f *fakeTTS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testTTSToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/voices":
		_ = json.NewEncoder(w).Encode(map[string][]string{"voices": f.voices})
	case "/reload":
		f.reloads++
		f.voices = append(f.voices, "new")
		_ = json.NewEncoder(w).Encode(map[string]int{"voices": len(f.voices)})
	case "/rank":
		var req rankRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.ranks = append(f.ranks, req)
		if f.rankStatus != 0 {
			w.WriteHeader(f.rankStatus)
			return
		}
		scores := make([]float64, len(req.Candidates))
		for i, c := range req.Candidates {
			scores[i] = float64(len(c))
		}
		_ = json.NewEncoder(w).Encode(map[string][]float64{"scores": scores})
	case "/tts":
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.speaks = append(f.speaks, req)
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(testWAV(44100, 22050, 16384)) // 0.5 s at half volume
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeTTS) speakCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.speaks)
}

// newTTSBot is a test bot with a fake worker and a fake ffmpeg. The fake
// ffmpeg writes "OggS" to its output file and records its calls.
func newTTSBot(t *testing.T) (*testBot, *fakeTTS, *[][]string) {
	t.Helper()
	f := &fakeTTS{voices: []string{"alex", "sam"}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	tb := newTestBot(t, func(e *config.Env) { e.TTSURL, e.TTSToken = srv.URL, testTTSToken })
	var ff [][]string
	tb.runTool = func(_ context.Context, name string, args []string) ([]byte, []byte, error) {
		if name != "ffmpeg" {
			t.Fatalf("ran %s", name)
		}
		ff = append(ff, args)
		return nil, nil, os.WriteFile(args[len(args)-1], []byte("OggS"), 0o600)
	}
	return tb, f, &ff
}

// testWAV is a mono 16-bit WAV file of n samples, each +amp or -amp.
func testWAV(rate, n int, amp int16) []byte {
	var buf bytes.Buffer
	le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	buf.WriteString("RIFF")
	le(uint32(36 + 2*n))
	buf.WriteString("WAVEfmt ")
	le(uint32(16))
	le(uint16(1))
	le(uint16(1))
	le(uint32(rate))
	le(uint32(rate * 2))
	le(uint16(2))
	le(uint16(16))
	buf.WriteString("data")
	le(uint32(2 * n))
	for i := range n {
		if i%2 == 0 {
			le(amp)
		} else {
			le(-amp)
		}
	}
	return buf.Bytes()
}

// openSay runs /say and returns the modal's JSON.
func (tb *testBot) openSay(userID string) string {
	tb.t.Helper()
	tb.api.reset()
	tb.run(userID, "say")
	modal := "" // the last one: the first can be refused
	for _, c := range tb.api.find("POST", `/interactions/.*/callback`) {
		if strings.Contains(string(c.Body), `"type":9`) {
			modal = string(c.Body)
		}
	}
	return modal
}

// submitSay sends the /say modal for the general channel. format is
// sayFormatMsg or sayFormatOgg.
func (tb *testBot) submitSay(userID, text, voice, format string) {
	tb.t.Helper()
	tb.submitSayModal(userID, sayModalID, tChGeneral, text, voice, format)
}

// submitSayModal sends a /say modal the way Discord does, with Labels, as a
// raw gateway event. channel is the channel select's value, and "" leaves
// the field out (the message menu's modal has none); "" text leaves the
// text field out (the short "Speak this message" modal has none).
func (tb *testBot) submitSayModal(userID, customID, channel, text, voice, format string) {
	tb.t.Helper()
	tb.seq++
	label := func(c map[string]any) map[string]any { return map[string]any{"type": 18, "component": c} }
	var fields []any
	if text != "" {
		fields = append(fields, label(map[string]any{"type": 4, "custom_id": sayText, "value": text}))
	}
	fields = append(fields,
		label(map[string]any{"type": 3, "custom_id": sayVoice, "values": []string{voice}}),
		label(map[string]any{"type": 3, "custom_id": sayFormat, "values": []string{format}}),
	)
	if channel != "" {
		fields = append(fields, label(map[string]any{"type": 8, "custom_id": sayChannel, "values": []string{channel}}))
	}
	member, _ := json.Marshal(tb.invoker(userID))
	raw, _ := json.Marshal(map[string]any{
		"id": fmt.Sprint(10000 + tb.seq), "application_id": tBot, "type": 5, "token": fmt.Sprintf("tok%d", tb.seq),
		"guild_id": tGuild, "channel_id": cmp.Or(tb.channelOverride, tChGeneral), "member": json.RawMessage(member),
		"data": map[string]any{"custom_id": customID, "components": fields},
	})
	var check discordgo.Interaction
	if json.Unmarshal(raw, &check) == nil {
		tb.t.Fatal("discordgo can read the modal submit; the test does not test the raw path")
	}
	tb.onRawInteraction(tb.s, &discordgo.Event{Type: "INTERACTION_CREATE", RawData: raw})
}

// voiceMessages returns the payloads of the voice messages that were sent,
// with "path" set to the request path.
func (tb *testBot) voiceMessages() []map[string]any {
	var out []map[string]any
	for _, c := range tb.api.calls {
		var p map[string]any
		if c.Files != nil && json.Unmarshal(c.Body, &p) == nil && p["flags"] == float64(discordgo.MessageFlagsIsVoiceMessage) {
			p["path"] = c.Path
			out = append(out, p)
		}
	}
	return out
}

// denyView takes View Channel from @everyone in a cached channel.
func (tb *testBot) denyView(channelID string) {
	ch, err := tb.s.State.Channel(channelID)
	if err != nil {
		tb.t.Fatal(err)
	}
	ch.PermissionOverwrites = append(ch.PermissionOverwrites, &discordgo.PermissionOverwrite{
		ID: tGuild, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel})
}

func TestSayNotRegisteredWithoutWorker(t *testing.T) {
	tb := newTestBot(t)
	for _, name := range []string{"say", "voices"} {
		if _, ok := tb.commands[name]; ok {
			t.Errorf("/%s is registered without TTS_URL", name)
		}
	}
	for _, c := range tb.modCommandCategories() {
		if c.title == "Speech" {
			t.Error("help shows /say without TTS_URL")
		}
	}
}

func TestSayModal(t *testing.T) {
	tb, _, _ := newTTSBot(t)
	if m := tb.openSay(tUser); m != "" {
		t.Errorf("non-mod got a modal: %s", m)
	}
	tb.expect(true, "must be a moderator")

	m := tb.openSay(tMod)
	for _, want := range []string{`"custom_id":"saymodal"`, `"type":18`, `"value":"random","description":"A new voice for each clip","default":true`,
		`"value":"alex"`, `"value":"sam"`, `"value":"voice","description":"Without the text","default":true`, `"max_length":300`,
		`"type":8,"custom_id":"channel","channel_types":[0,5],"default_values":[{"id":"` + tChGeneral + `","type":"channel"}]`} {
		if !strings.Contains(m, want) {
			t.Errorf("modal has no %s: %s", want, m)
		}
	}

	// The modal opens with the member's last choices.
	tb.submitSay(tMod, "hi", "sam", sayFormatOgg)
	m = tb.openSay(tMod)
	if !strings.Contains(m, `"value":"sam","description":"","default":true`) {
		t.Errorf("sam is not the default: %s", m)
	}
	if !strings.Contains(m, `"value":"file","description":"With the text","default":true`) {
		t.Errorf("audio file is not the default: %s", m)
	}
}

func TestSayWorkerDown(t *testing.T) {
	tb, _, _ := newTTSBot(t)
	tb.env.TTSURL = "http://127.0.0.1:1"
	tb.run(tMod, "say")
	tb.expect(true, "not available")
}

func TestSayVoiceMessage(t *testing.T) {
	tb, f, ff := newTTSBot(t)
	tb.submitSay(tMod, "  hello there  ", "Alex", sayFormatMsg)
	if len(f.speaks) != 1 || f.speaks[0]["text"] != "hello there" || f.speaks[0]["voice"] != "alex" {
		t.Errorf("worker requests = %v", f.speaks)
	}
	if len(*ff) != 1 || !slices.Contains((*ff)[0], "libopus") {
		t.Errorf("ffmpeg calls = %v", *ff)
	}
	vm := tb.voiceMessages()
	if len(vm) != 1 {
		t.Fatalf("voice messages = %v; calls %v", vm, tb.callList())
	}
	if vm[0]["path"] != "/channels/"+tChGeneral+"/messages" {
		t.Errorf("voice message sent to %v", vm[0]["path"])
	}
	att := vm[0]["attachments"].([]any)[0].(map[string]any)
	if att["duration_secs"] != 0.5 || att["waveform"] != base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{127}, 5)) {
		t.Errorf("attachment = %v", att)
	}
	if _, ok := vm[0]["embeds"]; ok {
		t.Errorf("voice message has embeds: %v", vm[0])
	}
	if _, ok := vm[0]["message_reference"]; ok {
		t.Errorf("voice message is a reply: %v", vm[0])
	}
	if len(tb.api.find("POST", `/webhooks/.*`)) != 0 {
		t.Errorf("sent a follow-up: %v", tb.callList())
	}
	if len(tb.api.find("DELETE", `/webhooks/`+tBot+`/tok\d+/messages/@original`)) != 1 {
		t.Errorf("the status reply was not deleted: %v", tb.callList())
	}
	logs := tb.sent(tChModLog)
	if len(logs) != 1 || !strings.Contains(logs[0], "<@"+tMod+"> posted a voice message (voice: `alex`) in <#"+tChGeneral+">") ||
		!strings.Contains(logs[0], "/"+tChGeneral+"/8000") || !strings.Contains(logs[0], "hello there") {
		t.Errorf("mod log = %q", logs)
	}
	if tb.tts.pending != 0 || len(tb.ttsSlot) != 0 {
		t.Errorf("queue not empty: pending %d, slot %d", tb.tts.pending, len(tb.ttsSlot))
	}
}

func TestSayOtherChannel(t *testing.T) {
	tb, _, _ := newTTSBot(t)
	tb.submitSayModal(tMod, sayModalID, tChSupport, "hi", "random", sayFormatMsg)
	vm := tb.voiceMessages()
	if len(vm) != 1 || vm[0]["path"] != "/channels/"+tChSupport+"/messages" {
		t.Errorf("voice messages = %v", vm)
	}

	// A mod cannot post where they cannot see; an admin can.
	tb.denyView(tChSens)
	tb.submitSayModal(tMod, sayModalID, tChSens, "hi", "random", sayFormatMsg)
	tb.expect(true, "cannot send messages in <#"+tChSens+">")
	tb.config(map[string]string{"TTS_COOLDOWN": "0"})
	tb.submitSayModal(tAdmin, sayModalID, tChSens, "hi", "random", sayFormatMsg)
	if n := len(tb.voiceMessages()); n != 2 {
		t.Errorf("voice messages = %d, want 2", n)
	}
}

func TestSayReplyMenu(t *testing.T) {
	tb, _, _ := newTTSBot(t)
	msg := &discordgo.Message{ID: "7001", ChannelID: tChGeneral, Content: "say something"}
	tb.runMessageMenu(tUser, sayMenu, msg)
	tb.expect(true, "must be a moderator")

	tb.api.reset()
	tb.runMessageMenu(tMod, sayMenu, msg)
	calls := tb.api.find("POST", `/interactions/.*/callback`)
	if len(calls) != 1 || !strings.Contains(string(calls[0].Body), `"custom_id":"saymodal:`+tChGeneral+`:7001"`) ||
		strings.Contains(string(calls[0].Body), `"type":8`) {
		t.Fatalf("modal = %v", calls)
	}

	tb.submitSayModal(tMod, sayModalID+":"+tChGeneral+":7001", "", "hello", "sam", sayFormatMsg)
	vm := tb.voiceMessages()
	if len(vm) != 1 {
		t.Fatalf("voice messages = %v; %v", vm, tb.callList())
	}
	ref, _ := vm[0]["message_reference"].(map[string]any)
	if ref["message_id"] != "7001" || ref["fail_if_not_exists"] != false {
		t.Errorf("reference = %v", vm[0]["message_reference"])
	}
	if am, _ := vm[0]["allowed_mentions"].(map[string]any); am["replied_user"] != false {
		t.Errorf("allowed mentions = %v", vm[0]["allowed_mentions"])
	}
	if logs := tb.sent(tChModLog); len(logs) != 1 || !strings.Contains(logs[0], "as a reply to [this message]") {
		t.Errorf("mod log = %q", logs)
	}
}

// speakModal uses "Speak this message" on msg and returns the modal's
// custom ID and JSON.
func (tb *testBot) speakModal(userID string, msg *discordgo.Message) (string, string) {
	tb.t.Helper()
	tb.api.reset()
	tb.runMessageMenu(userID, speakMenu, msg)
	for _, c := range tb.api.find("POST", `/interactions/.*/callback`) {
		var resp struct {
			Type int `json:"type"`
			Data struct {
				CustomID string `json:"custom_id"`
			} `json:"data"`
		}
		if json.Unmarshal(c.Body, &resp) == nil && resp.Type == 9 {
			return resp.Data.CustomID, string(c.Body)
		}
	}
	return "", ""
}

func TestSpeakMessage(t *testing.T) {
	tb, f, _ := newTTSBot(t)
	msg := &discordgo.Message{ID: "7001", ChannelID: tChGeneral,
		Content:  "hi <@" + tUser + "> and <@999>, see https://example.com/x <:wave:123> in <#" + tChSupport + ">",
		Mentions: []*discordgo.User{{ID: tUser, Username: "user"}}}
	id, m := tb.speakModal(tMod, msg)
	if !strings.HasPrefix(id, sayModalID+":"+tChGeneral+":7001:") || strings.Contains(m, `"custom_id":"text"`) {
		t.Fatalf("modal %q = %s", id, m)
	}
	tb.submitSayModal(tMod, id, "", "", "sam", sayFormatMsg)
	want := "hi @user and someone, see link wave in #support"
	if len(f.speaks) != 1 || f.speaks[0]["text"] != want {
		t.Errorf("worker requests = %v, want %q", f.speaks, want)
	}
	vm := tb.voiceMessages()
	if ref, _ := vm[0]["message_reference"].(map[string]any); len(vm) != 1 || ref["message_id"] != "7001" {
		t.Errorf("voice messages = %v", vm)
	}
	if logs := tb.sent(tChModLog); len(logs) != 1 || !strings.Contains(logs[0], want) {
		t.Errorf("mod log = %q", logs)
	}

	// The text is used once.
	tb.config(map[string]string{"TTS_COOLDOWN": "0"})
	tb.submitSayModal(tMod, id, "", "", "sam", sayFormatMsg)
	tb.expect(true, "open too long")
}

func TestSpeakLongMessage(t *testing.T) {
	tb, f, _ := newTTSBot(t)
	tb.config(map[string]string{"TTS_MAX_CHARS": "10"})
	id, m := tb.speakModal(tMod, &discordgo.Message{ID: "7001", ChannelID: tChGeneral, Content: "one two three four"})
	if !strings.Contains(m, `"custom_id":"text","style":2,"min_length":1,"max_length":10,"value":"one two"`) {
		t.Fatalf("modal = %s", m)
	}
	tb.submitSayModal(tMod, id, "", "one two!", "random", sayFormatMsg)
	if len(f.speaks) != 1 || f.speaks[0]["text"] != "one two!" {
		t.Errorf("worker requests = %v", f.speaks)
	}
}

func TestSpeakChecks(t *testing.T) {
	tb, _, _ := newTTSBot(t)
	tb.speakModal(tUser, &discordgo.Message{ID: "7001", ChannelID: tChGeneral, Content: "hi"})
	tb.expect(true, "must be a moderator")
	if id, _ := tb.speakModal(tMod, &discordgo.Message{ID: "7001", ChannelID: tChGeneral, Content: "  "}); id != "" {
		t.Errorf("modal for an empty message: %s", id)
	}
	tb.expect(true, "no text to speak")
}

func TestCutText(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"one two three", 10, "one two"},
		{"abcdefghijklmnop", 10, "abcdefghij"},
		{"héllo wörld again", 12, "héllo wörld"},
	} {
		if got := cutText(c.in, c.n); got != c.want {
			t.Errorf("cutText(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestSayVoiceMessageFallback(t *testing.T) {
	tb, _, _ := newTTSBot(t)
	tb.api.onBody("POST", `/channels/.*/messages`, `duration_secs`, 1, http.StatusBadRequest, `{"code":50035}`)
	tb.submitSay(tMod, "secret words", "random", sayFormatMsg)
	if got := tb.uploaded(); got["say.ogg"] == 0 {
		t.Errorf("uploaded = %v", got)
	}
	posts := tb.sent(tChGeneral)
	if len(posts) != 2 || strings.Contains(posts[1], "secret words") || !strings.Contains(posts[1], "random") {
		t.Errorf("posts = %q", posts)
	}
	if logs := tb.sent(tChModLog); len(logs) != 1 || !strings.Contains(logs[0], "an audio clip") {
		t.Errorf("mod log = %q", logs)
	}
}

func TestSayPostFails(t *testing.T) {
	tb, _, _ := newTTSBot(t)
	tb.api.onBody("POST", `/channels/`+tChGeneral+`/messages`, ``, 0, http.StatusForbidden, `{"code":50013}`)
	tb.submitSay(tMod, "hi", "random", sayFormatMsg)
	tb.expect(true, "could not post", "Send Voice Messages")
	if len(tb.sent(tChModLog)) != 0 {
		t.Error("a failed post was logged")
	}
}

func TestSayAudioFile(t *testing.T) {
	tb, _, _ := newTTSBot(t)
	tb.submitSay(tMod, "hello there", "sam", sayFormatOgg)
	if len(tb.voiceMessages()) != 0 {
		t.Error("sent a voice message")
	}
	posts := tb.sent(tChGeneral)
	if len(posts) != 1 || !strings.Contains(posts[0], "sam") || !strings.Contains(posts[0], "hello there") {
		t.Errorf("posts = %q", posts)
	}
	if got := tb.uploaded(); got["say.ogg"] == 0 {
		t.Errorf("uploaded = %v", got)
	}
}

func TestWavWaveform(t *testing.T) {
	secs, wave := wavWaveform(testWAV(1000, 3000, 32767))
	if secs != 3 || wave != base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{255}, 30)) {
		t.Errorf("3 s = %v, %q", secs, wave)
	}
	secs, wave = wavWaveform(testWAV(1000, 60000, 0)) // 60 s: at most 256 points
	if raw, _ := base64.StdEncoding.DecodeString(wave); secs != 60 || len(raw) != 256 {
		t.Errorf("60 s = %v, %d points", secs, len(raw))
	}
	if secs, _ := wavWaveform([]byte("not a wav file")); secs != 0 {
		t.Errorf("bad data = %v", secs)
	}
}

func TestSayChecks(t *testing.T) {
	tb, f, _ := newTTSBot(t)

	tb.submitSay(tUser, "hi", "random", sayFormatMsg)
	tb.expect(true, "must be a moderator")

	tb.config(map[string]string{"TTS_MAX_CHARS": "5"})
	tb.submitSay(tMod, "too long", "random", sayFormatMsg)
	tb.expect(true, "1 to 5 characters")

	tb.config(map[string]string{"TTS_CHANNELS": tChJail})
	if m := tb.openSay(tMod); strings.Contains(m, "default_values") {
		t.Errorf("the modal selects a channel that TTS_CHANNELS does not allow: %s", m)
	}
	tb.submitSay(tMod, "hi", "random", sayFormatMsg)
	tb.expect(true, "do not work in <#"+tChGeneral+">")
	tb.runMessageMenu(tMod, sayMenu, &discordgo.Message{ID: "7001", ChannelID: tChGeneral})
	tb.expect(true, "do not work in <#"+tChGeneral+">")

	tb.config(map[string]string{"TTS_ENABLED": "off"})
	tb.run(tMod, "say")
	tb.expect(true, "Speech clips are off")

	if n := f.speakCount(); n != 0 {
		t.Errorf("worker got %d requests", n)
	}
}

func TestSayCooldownAndQueue(t *testing.T) {
	tb, f, _ := newTTSBot(t)
	tb.config(map[string]string{"TTS_COOLDOWN": "60"})
	tb.submitSay(tMod, "one", "random", sayFormatMsg)
	tb.submitSay(tMod, "two", "random", sayFormatMsg)
	tb.expect(true, "Wait")
	tb.submitSay(tAdmin, "three", "random", sayFormatMsg)
	if n := f.speakCount(); n != 2 {
		t.Errorf("worker got %d requests, want 2", n)
	}

	// One clip runs and TTS_QUEUE_MAX wait: the next one is refused.
	tb.config(map[string]string{"TTS_COOLDOWN": "0", "TTS_QUEUE_MAX": "2"})
	tb.tts.pending = 3
	tb.submitSay(tMod, "four", "random", sayFormatMsg)
	tb.expect(true, "queue is full")
	tb.tts.pending = 0
}

func TestSayWorkerError(t *testing.T) {
	tb, f, _ := newTTSBot(t)
	tb.config(map[string]string{"TTS_COOLDOWN": "0"})
	f.status = http.StatusInternalServerError
	tb.submitSay(tMod, "hi", "random", sayFormatMsg)
	tb.expect(true, "not available")
	if len(tb.uploaded()) != 0 {
		t.Error("a file was sent")
	}

	f.status = http.StatusNotFound // the voice was removed after the list was cached
	tb.submitSay(tMod, "hi", "sam", sayFormatMsg)
	tb.expect(true, "no voice named `sam`")
}

func TestVoices(t *testing.T) {
	tb, f, _ := newTTSBot(t)
	tb.run(tMod, "voices")
	r := tb.expect(true, "`alex`, `sam`")
	if len(r.Components) != 0 {
		t.Errorf("mod sees buttons: %+v", r.Components)
	}
	tb.click(tMod, "ttsreload")
	tb.expect(true, "Only admins")

	tb.run(tAdmin, "voices")
	r = tb.expect(true, "Voices (2)")
	if len(r.Components) != 1 || r.Components[0].CustomID != "ttsreload" {
		t.Errorf("admin buttons = %+v", r.Components)
	}
	tb.click(tAdmin, "ttsreload")
	tb.expect(false, "Voices (3)", "`new`")
	if f.reloads != 1 {
		t.Errorf("reloads = %d", f.reloads)
	}
}
