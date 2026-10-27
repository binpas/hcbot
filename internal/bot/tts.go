package bot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// Text to speech: a mod opens /say, a modal with the text, the voice, and
// the format (a voice message or an audio file). The bot asks the text to speech worker
// (tts/server.py, Echo-TTS on a GPU) for a clip. ffmpeg makes the WAV into
// Ogg/Opus, and the bot posts it in the channel: as a voice message
// (without the text), or as an audio file with the text.
// The worker makes one clip at a time, so the bot keeps a queue. A voice is
// a reference clip in the worker's voices folder; "random" uses none.
//
// The commands are registered only when TTS_URL is set. The worker does not
// keep the text, and the bot only sends the text and the voice name.

const (
	ttsRequestTimeout = 3 * time.Minute  // one clip takes about 15-30 s
	ttsTimeout        = 12 * time.Minute // the interaction token lasts 15
	ttsVoicesFor      = time.Minute      // how long the voice list is cached
	ttsHardMaxChars   = 600              // the worker's limit
	ttsRandomVoice    = "random"
)

var errTTSVoice = errors.New("unknown voice")

// ttsState is the queue, the cooldowns, and the cached voice list.
type ttsState struct {
	mu       sync.Mutex
	pending  int // clips in the queue, with the one that runs
	last     map[string]time.Time
	voices   []string
	voicesAt time.Time
	prefs    map[string]ttsPref
}

func (b *Bot) registerTTS() {
	if b.env.TTSURL == "" {
		return
	}
	b.addCommand(&command{
		name:        "say",
		description: "Make a speech clip or voice message and post it here (mod only).",
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdSay,
	})
	b.addCommand(&command{
		name:        "voices",
		description: "List the voices for /say (mod only).",
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdVoices,
	})
	b.addModal(sayModalID, b.onSayModal)
	b.addComponent("ttsreload", b.onTTSReload)
}

// The /say modal.
const (
	sayModalID   = "saymodal"
	sayText      = "text"
	sayVoice     = "voice"
	sayFormat    = "format"
	sayFormatMsg = "voice"
	sayFormatOgg = "file"
	// A select has at most 25 options: random and 24 voices.
	sayMaxVoices = 24
)

// ttsPref is a member's last /say choices, so the modal opens with them.
// It is kept in memory only.
type ttsPref struct {
	voice    string
	voiceMsg bool
}

func (b *Bot) cmdSay(c *Ctx) {
	if !b.sayAllowed(c) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // a modal must open in 3 s
	defer cancel()
	names, err := b.ttsVoices(ctx, false)
	if err != nil {
		log.Printf("say: voices: %v", err)
		c.Reply(true, errEmbed("The speech worker is not available. Try again later."))
		return
	}
	b.tts.mu.Lock()
	pref, seen := b.tts.prefs[c.UserID()]
	b.tts.mu.Unlock()
	if !seen {
		pref = ttsPref{voice: ttsRandomVoice, voiceMsg: true}
	}
	if pref.voice != ttsRandomVoice && !slices.Contains(names, pref.voice) {
		pref.voice = ttsRandomVoice
	}
	options := []discordgo.SelectMenuOption{{Label: ttsRandomVoice, Value: ttsRandomVoice,
		Description: "A new voice for each clip", Default: pref.voice == ttsRandomVoice}}
	for _, n := range names[:min(len(names), sayMaxVoices)] {
		options = append(options, discordgo.SelectMenuOption{Label: n, Value: n, Default: n == pref.voice})
	}
	maxChars := b.ttsMaxChars()
	text := modalLabel{Label: "Text", Description: fmt.Sprintf("What to say (at most %d characters)", maxChars),
		Component: modalText{CustomID: sayText, Style: discordgo.TextInputParagraph, MinLength: 1, MaxLength: maxChars}}
	voice := modalLabel{Label: "Voice", Component: modalSelect{CustomID: sayVoice, Options: options}}
	format := modalLabel{Label: "Format", Component: modalSelect{CustomID: sayFormat, Options: []discordgo.SelectMenuOption{
		{Label: "Voice message", Value: sayFormatMsg, Description: "Without the text", Default: pref.voiceMsg},
		{Label: "Audio file", Value: sayFormatOgg, Description: "With the text", Default: !pref.voiceMsg},
	}}}
	c.Modal(sayModalID, "Say something", text, voice, format)
}

// sayAllowed checks TTS_ENABLED and TTS_CHANNELS, and tells the member
// when /say cannot be used.
func (b *Bot) sayAllowed(c *Ctx) bool {
	if !b.cfg.Bool("TTS_ENABLED") {
		c.Reply(true, errEmbed("Speech clips are off (`TTS_ENABLED` in `/setup`)."))
		return false
	}
	if chans := b.cfg.List("TTS_CHANNELS"); len(chans) > 0 && !slices.Contains(chans, c.i.ChannelID) {
		c.Reply(true, errEmbed("`/say` does not work in this channel (`TTS_CHANNELS` in `/setup`)."))
		return false
	}
	return true
}

func (b *Bot) ttsMaxChars() int {
	return min(max(b.cfg.Int("TTS_MAX_CHARS"), 1), ttsHardMaxChars)
}

func (b *Bot) onSayModal(c *Ctx) {
	if !b.isMod(c.Member()) {
		c.Reply(true, errEmbed("You must be a moderator to use that command."))
		return
	}
	if !b.sayAllowed(c) {
		return
	}
	text := strings.TrimSpace(c.ModalValue(sayText))
	maxChars := b.ttsMaxChars()
	if n := utf8.RuneCountInString(text); n == 0 || n > maxChars {
		c.Reply(true, errEmbed(fmt.Sprintf("The text must have 1 to %d characters.", maxChars)))
		return
	}
	voice := ttsRandomVoice
	if v := c.ModalValues(sayVoice); len(v) > 0 && v[0] != "" {
		voice = strings.ToLower(v[0])
	}
	voiceMsg := c.ModalValue(sayFormat) != sayFormatOgg

	b.tts.mu.Lock()
	if b.tts.prefs == nil {
		b.tts.prefs = map[string]ttsPref{}
	}
	b.tts.prefs[c.UserID()] = ttsPref{voice: voice, voiceMsg: voiceMsg}
	b.tts.mu.Unlock()
	b.say(c, text, voice, voiceMsg)
}

// say makes the clip and posts it. The member sees the queue and the
// errors in an ephemeral reply; the clip is a public follow-up.
func (b *Bot) say(c *Ctx, text, voice string, voiceMsg bool) {
	ahead, wait, ok := b.ttsEnqueue(c.UserID())
	if !ok {
		if wait > 0 {
			c.Reply(true, errEmbed(fmt.Sprintf("Wait %s before you use `/say` again.", humaniseDuration(int64((wait+time.Second-1)/time.Second)))))
		} else {
			c.Reply(true, errEmbed("The speech queue is full. Try again later."))
		}
		return
	}
	defer b.ttsDone()

	// The status message must replace the "thinking" reply before the clip
	// is posted: Discord makes the first follow-up of a deferred reply into
	// that (ephemeral) reply, and the clip would then be deleted with it.
	c.Defer(true)
	working := makeEmbed("🗣️ Making the clip", "This takes about 15–30 seconds.", colourBlurple)
	ctx, cancel := context.WithTimeout(context.Background(), ttsTimeout)
	defer cancel()
	select {
	case b.ttsSlot <- struct{}{}:
		c.EditResponse(working)
	default:
		c.EditResponse(makeEmbed("⏳ In the queue",
			fmt.Sprintf("Position %d. Each clip takes about 15–30 seconds.", max(ahead, 1)), colourBlurple))
		select {
		case b.ttsSlot <- struct{}{}:
			c.EditResponse(working)
		case <-ctx.Done():
			c.Reply(true, errEmbed("The speech queue took too long. Try again later."))
			return
		}
	}
	defer func() { <-b.ttsSlot }()

	wav, err := b.ttsSpeak(ctx, text, voice)
	if err != nil {
		msg := "The speech worker is not available. Try again later."
		if errors.Is(err, errTTSVoice) {
			msg = fmt.Sprintf("There is no voice named `%s`. Use `/voices` to see them.", voice)
		} else {
			log.Printf("say: %v", err)
		}
		c.Reply(true, errEmbed(msg))
		return
	}
	ogg, err := b.toOpus(ctx, wav)
	if err != nil {
		log.Printf("say: encode: %v", err)
		c.Reply(true, errEmbed("I could not encode the clip."))
		return
	}
	if voiceMsg {
		err := b.postVoiceMessage(c, ogg, wav)
		if err == nil {
			c.DeleteResponse()
			return
		}
		log.Printf("say: voice message: %v", err) // send it as a file instead
	}
	embed := makeEmbed("🗣️ "+voice, text, colourBlurple)
	if voiceMsg {
		embed.Description = "" // the member asked for no text
	}
	_, err = c.s.FollowupMessageCreate(c.i.Interaction, true, &discordgo.WebhookParams{
		Embeds: []*discordgo.MessageEmbed{embed},
		Files:  []*discordgo.File{{Name: "say.ogg", ContentType: "audio/ogg", Reader: bytes.NewReader(ogg)}},
	})
	if err != nil {
		log.Printf("say: post: %v", err)
		c.Reply(true, errEmbed("Discord did not accept the clip."))
		return
	}
	c.DeleteResponse()
}

// postVoiceMessage posts ogg as a voice message: first as a follow-up of
// the interaction, and if Discord refuses that, in the channel. A voice
// message has no text and no embeds.
func (b *Bot) postVoiceMessage(c *Ctx, ogg, wav []byte) error {
	secs, wave := wavWaveform(wav)
	payload := map[string]any{
		"flags": discordgo.MessageFlagsIsVoiceMessage,
		"attachments": []map[string]any{{
			"id": "0", "filename": "voice-message.ogg", "duration_secs": secs, "waveform": wave,
		}},
	}
	files := []*discordgo.File{{Name: "voice-message.ogg", ContentType: "audio/ogg", Reader: bytes.NewReader(ogg)}}
	ctype, body, err := discordgo.MultipartBodyWithJSON(payload, files)
	if err != nil {
		return err
	}
	url := discordgo.EndpointWebhookToken(c.i.AppID, c.i.Token)
	_, err = c.s.RequestRaw(http.MethodPost, url+"?wait=true", ctype, body, url, 0)
	if err == nil {
		return nil
	}
	log.Printf("say: voice message follow-up: %v", err)
	url = discordgo.EndpointChannelMessages(c.i.ChannelID)
	_, err = c.s.RequestRaw(http.MethodPost, url, ctype, body, url, 0)
	return err
}

// wavWaveform reads 16-bit PCM WAV data and returns its length in seconds
// and a Discord voice message waveform: one byte (0-255) for each 100 ms,
// at most 256 bytes, base64. Bad data gives 0 and an empty waveform.
func wavWaveform(wav []byte) (float64, string) {
	rate, pcm := wavPCM(wav)
	if rate == 0 || len(pcm) < 2 {
		return 0, base64.StdEncoding.EncodeToString([]byte{0})
	}
	n := len(pcm) / 2
	secs := float64(n) / float64(rate)
	points := min(max(int(secs*10), 1), 256)
	out := make([]byte, points)
	for p := range points {
		lo, hi := p*n/points, (p+1)*n/points
		var peak int
		for i := lo; i < hi; i++ {
			v := int(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
			peak = max(peak, v, -v)
		}
		out[p] = byte(min(peak*255/32767, 255))
	}
	return secs, base64.StdEncoding.EncodeToString(out)
}

// wavPCM returns the sample rate and the samples of a mono 16-bit WAV file,
// or 0 and nil when it is not one.
func wavPCM(wav []byte) (int, []byte) {
	if len(wav) < 12 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return 0, nil
	}
	var rate int
	for pos := 12; pos+8 <= len(wav); {
		id, size := string(wav[pos:pos+4]), int(binary.LittleEndian.Uint32(wav[pos+4:]))
		body := wav[pos+8 : min(pos+8+size, len(wav))]
		switch id {
		case "fmt ":
			if len(body) < 16 || binary.LittleEndian.Uint16(body[0:]) != 1 ||
				binary.LittleEndian.Uint16(body[2:]) != 1 || binary.LittleEndian.Uint16(body[14:]) != 16 {
				return 0, nil
			}
			rate = int(binary.LittleEndian.Uint32(body[4:]))
		case "data":
			return rate, body
		}
		pos += 8 + size + size%2
	}
	return 0, nil
}

// ttsEnqueue adds a clip to the queue. It returns the number of clips
// before it. When the user must wait (wait > 0) or the queue is full, ok
// is false.
func (b *Bot) ttsEnqueue(userID string) (ahead int, wait time.Duration, ok bool) {
	st := &b.tts
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	cooldown := time.Duration(max(b.cfg.Int("TTS_COOLDOWN"), 0)) * time.Second
	if t, seen := st.last[userID]; seen && now.Sub(t) < cooldown {
		return 0, cooldown - now.Sub(t), false
	}
	// One clip runs; at most TTS_QUEUE_MAX wait.
	if st.pending > max(b.cfg.Int("TTS_QUEUE_MAX"), 0) {
		return 0, 0, false
	}
	if st.last == nil {
		st.last = map[string]time.Time{}
	}
	st.last[userID] = now
	ahead = st.pending
	st.pending++
	return ahead, 0, true
}

func (b *Bot) ttsDone() {
	b.tts.mu.Lock()
	b.tts.pending--
	b.tts.mu.Unlock()
}

// ttsDo sends one request to the worker.
func (b *Bot) ttsDo(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.env.TTSURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.env.TTSToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return b.ttsHTTP.Do(req)
}

// ttsSpeak asks the worker for a clip and returns the WAV data.
func (b *Bot) ttsSpeak(ctx context.Context, text, voice string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ttsRequestTimeout)
	defer cancel()
	resp, err := b.ttsDo(ctx, http.MethodPost, "/tts", map[string]string{"text": text, "voice": voice})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errTTSVoice
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("worker: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// ttsVoices returns the worker's voice names. The list is cached for a
// minute, unless fresh is true.
func (b *Bot) ttsVoices(ctx context.Context, fresh bool) ([]string, error) {
	st := &b.tts
	st.mu.Lock()
	if !fresh && st.voices != nil && time.Since(st.voicesAt) < ttsVoicesFor {
		names := st.voices
		st.mu.Unlock()
		return names, nil
	}
	st.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := b.ttsDo(ctx, http.MethodGet, "/voices", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worker: %s", resp.Status)
	}
	var out struct {
		Voices []string `json:"voices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if out.Voices == nil {
		out.Voices = []string{}
	}
	st.mu.Lock()
	st.voices, st.voicesAt = out.Voices, time.Now()
	st.mu.Unlock()
	return out.Voices, nil
}

// toOpus makes WAV data into an Ogg/Opus file with ffmpeg.
func (b *Bot) toOpus(ctx context.Context, wav []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "hcbot-say-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	in, out := filepath.Join(dir, "in.wav"), filepath.Join(dir, "out.ogg")
	if err := os.WriteFile(in, wav, 0o600); err != nil {
		return nil, err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", in,
		"-c:a", "libopus", "-b:a", "64k", "-ar", "48000", out}
	if _, stderr, err := b.runTool(ctx, "ffmpeg", args); err != nil {
		return nil, fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr))
	}
	return os.ReadFile(out)
}

func (b *Bot) cmdVoices(c *Ctx) {
	b.replyVoices(c, false)
}

func (b *Bot) onTTSReload(c *Ctx) {
	if !isAdmin(c.Member()) {
		c.Reply(true, errEmbed("Only admins can reload the voices."))
		return
	}
	c.DeferUpdate()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	resp, err := b.ttsDo(ctx, http.MethodPost, "/reload", nil)
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("worker: %s", resp.Status)
		}
	}
	if err != nil {
		log.Printf("voices: reload: %v", err)
		c.Reply(true, errEmbed("The speech worker is not available. Try again later."))
		return
	}
	b.replyVoices(c, true)
}

// replyVoices shows the voice list, with a reload button for admins.
func (b *Bot) replyVoices(c *Ctx, fresh bool) {
	names, err := b.ttsVoices(context.Background(), fresh)
	if err != nil {
		log.Printf("voices: %v", err)
		c.Reply(true, errEmbed("The speech worker is not available. Try again later."))
		return
	}
	desc := "No voices yet. Put a short clip of one speaker (5–10 seconds) in the voices folder; the file name is the voice name."
	if len(names) > 0 {
		desc = "`" + strings.Join(names, "`, `") + "`"
	}
	desc += "\n\n`random` makes a new voice for each clip."
	embed := makeEmbed(fmt.Sprintf("🗣️ Voices (%d)", len(names)), desc, colourBlurple)
	var comps []discordgo.MessageComponent
	if isAdmin(c.Member()) {
		comps = []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Reload voices", Style: discordgo.SecondaryButton, CustomID: "ttsreload"},
		}}}
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{embed}, comps)
}
