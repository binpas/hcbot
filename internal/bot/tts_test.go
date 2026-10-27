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

// submitSay sends the /say modal the way Discord does, with Labels, as a
// raw gateway event. format is sayFormatMsg or sayFormatOgg.
func (tb *testBot) submitSay(userID, text, voice, format string) {
	tb.t.Helper()
	tb.seq++
	label := func(c map[string]any) map[string]any { return map[string]any{"type": 18, "component": c} }
	member, _ := json.Marshal(tb.invoker(userID))
	raw, _ := json.Marshal(map[string]any{
		"id": fmt.Sprint(10000 + tb.seq), "application_id": tBot, "type": 5, "token": fmt.Sprintf("tok%d", tb.seq),
		"guild_id": tGuild, "channel_id": cmp.Or(tb.channelOverride, tChGeneral), "member": json.RawMessage(member),
		"data": map[string]any{"custom_id": sayModalID, "components": []any{
			label(map[string]any{"type": 4, "custom_id": sayText, "value": text}),
			label(map[string]any{"type": 3, "custom_id": sayVoice, "values": []string{voice}}),
			label(map[string]any{"type": 3, "custom_id": sayFormat, "values": []string{format}}),
		}},
	})
	var check discordgo.Interaction
	if json.Unmarshal(raw, &check) == nil {
		tb.t.Fatal("discordgo can read the modal submit; the test does not test the raw path")
	}
	tb.onRawInteraction(tb.s, &discordgo.Event{Type: "INTERACTION_CREATE", RawData: raw})
}

// voiceMessages returns the payloads of the voice messages that were sent.
func (tb *testBot) voiceMessages() []map[string]any {
	var out []map[string]any
	for _, c := range tb.api.calls {
		var p map[string]any
		if c.Files != nil && json.Unmarshal(c.Body, &p) == nil && p["flags"] == float64(discordgo.MessageFlagsIsVoiceMessage) {
			out = append(out, p)
		}
	}
	return out
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
		`"value":"alex"`, `"value":"sam"`, `"value":"voice","description":"Without the text","default":true`, `"max_length":300`} {
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
	att := vm[0]["attachments"].([]any)[0].(map[string]any)
	if att["duration_secs"] != 0.5 || att["waveform"] != base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{127}, 5)) {
		t.Errorf("attachment = %v", att)
	}
	if _, ok := vm[0]["embeds"]; ok {
		t.Errorf("voice message has embeds: %v", vm[0])
	}
	if len(tb.api.find("POST", `/webhooks/`+tBot+`/tok\d+`)) != 1 {
		t.Errorf("not sent as a follow-up: %v", tb.callList())
	}
	if len(tb.api.find("DELETE", `/webhooks/`+tBot+`/tok\d+/messages/@original`)) != 1 {
		t.Errorf("the thinking reply was not deleted: %v", tb.callList())
	}
	if tb.tts.pending != 0 || len(tb.ttsSlot) != 0 {
		t.Errorf("queue not empty: pending %d, slot %d", tb.tts.pending, len(tb.ttsSlot))
	}
}

func TestSayVoiceMessageFallbacks(t *testing.T) {
	tb, _, _ := newTTSBot(t)
	tb.config(map[string]string{"TTS_COOLDOWN": "0"})

	// The follow-up is refused: the voice message goes to the channel.
	tb.api.onBody("POST", `/webhooks/.*`, `duration_secs`, 1, http.StatusBadRequest, `{"code":50035}`)
	tb.submitSay(tMod, "hi", "random", sayFormatMsg)
	if n := len(tb.api.find("POST", `/channels/`+tChGeneral+`/messages`)); n != 1 {
		t.Errorf("channel posts = %d; %v", n, tb.callList())
	}

	// Both are refused: an audio file without the text.
	tb.api.reset()
	tb.api.onBody("POST", `/webhooks/.*`, `duration_secs`, 1, http.StatusBadRequest, `{"code":50035}`)
	tb.api.onBody("POST", `/channels/.*/messages`, `duration_secs`, 1, http.StatusForbidden, `{"code":50013}`)
	tb.submitSay(tMod, "secret words", "random", sayFormatMsg)
	if got := tb.uploaded(); got["say.ogg"] == 0 {
		t.Errorf("uploaded = %v", got)
	}
	r := tb.last()
	if strings.Contains(r.text(), "secret words") || !strings.Contains(r.text(), "random") {
		t.Errorf("fallback reply = %q", r.text())
	}
}

func TestSayAudioFile(t *testing.T) {
	tb, _, _ := newTTSBot(t)
	tb.submitSay(tMod, "hello there", "sam", sayFormatOgg)
	if len(tb.voiceMessages()) != 0 {
		t.Error("sent a voice message")
	}
	r := tb.last()
	if r.Kind != "followup" || r.Ephemeral || !strings.Contains(r.text(), "sam") || !strings.Contains(r.text(), "hello there") {
		t.Errorf("reply = %+v", r)
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
	tb.run(tMod, "say")
	tb.expect(true, "does not work in this channel")
	tb.submitSay(tMod, "hi", "random", sayFormatMsg)
	tb.expect(true, "does not work in this channel")

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
