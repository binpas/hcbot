package bot

import (
	"bytes"
	"cmp"
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
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/db"
)

// Text to speech: a mod opens /say, a modal with the text, the voice, and
// the format (a voice message or an audio file). The bot asks the text to speech worker
// (tts/server.py, Echo-TTS on a GPU) for a clip. ffmpeg makes the WAV into
// Ogg/Opus, and the bot posts it in the channel: as a voice message
// (without the text), or as an audio file with the text.
// The worker keeps the queue and makes one clip at a time. The bot adds the
// clip to it, keeps the job in tts_jobs (so a restart of the bot does not
// lose it), and asks for its state until it is done. A voice is a reference
// clip in the worker's voices folder; "random" uses none.
//
// The commands are registered only when TTS_URL is set. The bot only sends
// the text and the voice name. The worker can save the clips with their
// text for the operator (CLIPS_DIR).

const (
	ttsRequestTimeout = 30 * time.Second // one request to the worker
	ttsTimeout        = 12 * time.Minute // the interaction token lasts 15
	ttsPoll           = 2 * time.Second  // how often the bot asks for a job's state
	ttsVoicesFor      = time.Minute      // how long the voice list is cached
	ttsHardMaxChars   = 600              // the worker's limit
	ttsRandomVoice    = "random"
)

var (
	errTTSVoice = errors.New("unknown voice")
	errTTSGone  = errors.New("the worker does not know the job")
)

// ttsState is the queue, the cooldowns, and the cached voice list.
type ttsState struct {
	mu       sync.Mutex
	pending  int // this bot's open clip jobs
	last     map[string]time.Time
	voices   []string
	voicesAt time.Time
	prefs    map[string]ttsPref
	speak    map[string]speakText // texts of open "Speak this message" forms
}

type speakText struct {
	text string
	at   time.Time
}

func (b *Bot) registerTTS() {
	if b.env.TTSURL == "" {
		return
	}
	b.addCommand(&command{
		name:        "say",
		description: "Make a voice message or an audio clip and post it in a channel (mod only).",
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdSay,
	})
	b.addCommand(&command{
		name:        sayMenu,
		kind:        discordgo.MessageApplicationCommand,
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdSayReply,
	})
	b.addCommand(&command{
		name:        speakMenu,
		kind:        discordgo.MessageApplicationCommand,
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdSpeak,
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

// The /say modal. From the message menu, its custom ID also has the
// channel and the message to reply to: saymodal:<channel>:<message>. From
// "Speak this message", it also has the key of the text in ttsState.speak.
const (
	sayMenu      = "Respond with Voice"
	speakMenu    = "Speak this message"
	speakFor     = 15 * time.Minute // how long the text of an open form is kept
	sayModalID   = "saymodal"
	sayText      = "text"
	sayVoice     = "voice"
	sayFormat    = "format"
	sayChannel   = "channel"
	sayFormatMsg = "voice"
	sayFormatOgg = "file"
	// A select has at most 25 options: random and 24 voices.
	sayMaxVoices = 24
)

// sayChannelTypes are the channels /say can post in.
var sayChannelTypes = []discordgo.ChannelType{discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews}

// ttsPref is a member's last /say choices, so the modal opens with them.
// It is kept in memory only.
type ttsPref struct {
	voice    string
	voiceMsg bool
}

// modalChannelSelect is a required channel select (one channel) in a
// Label, with a channel selected first.
type modalChannelSelect struct {
	CustomID string
	Types    []discordgo.ChannelType
	Default  string
}

func (s modalChannelSelect) Type() discordgo.ComponentType {
	return discordgo.ChannelSelectMenuComponent
}

func (s modalChannelSelect) MarshalJSON() ([]byte, error) {
	type value struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	var defaults []value
	if s.Default != "" {
		defaults = []value{{s.Default, "channel"}}
	}
	return json.Marshal(struct {
		Type     discordgo.ComponentType `json:"type"`
		CustomID string                  `json:"custom_id"`
		Types    []discordgo.ChannelType `json:"channel_types"`
		Defaults []value                 `json:"default_values,omitempty"`
		Min      int                     `json:"min_values"`
		Max      int                     `json:"max_values"`
		Required bool                    `json:"required"`
	}{s.Type(), s.CustomID, s.Types, defaults, 1, 1, true})
}

func (b *Bot) cmdSay(c *Ctx) {
	if !b.ttsEnabled(c) {
		return
	}
	def := c.i.ChannelID
	if chans := b.cfg.List("TTS_CHANNELS"); len(chans) > 0 && !slices.Contains(chans, def) {
		def = ""
	}
	channel := modalLabel{Label: "Channel", Description: "Where to post the clip",
		Component: modalChannelSelect{CustomID: sayChannel, Types: sayChannelTypes, Default: def}}
	b.openSayModal(c, sayModalID, "Say something", "", true, channel)
}

func (b *Bot) cmdSayReply(c *Ctx) {
	msg := c.TargetMessage()
	if msg == nil || !b.ttsEnabled(c) || !b.ttsChannelAllowed(c, c.i.ChannelID) {
		return
	}
	if !b.canPostIn(c, c.i.ChannelID) {
		c.Reply(true, errEmbed("You cannot send messages in this channel."))
		return
	}
	b.openSayModal(c, sayModalID+":"+c.i.ChannelID+":"+msg.ID, "Respond with voice", "", true)
}

// cmdSpeak speaks a message's text, as a reply to it. Discord sends the
// text of the selected message in the interaction, also without the
// Message Content intent. The form has only the voice and the format,
// unless the text is too long: then it also has the text, cut, to change.
func (b *Bot) cmdSpeak(c *Ctx) {
	msg := c.TargetMessage()
	if msg == nil || !b.ttsEnabled(c) || !b.ttsChannelAllowed(c, c.i.ChannelID) {
		return
	}
	if !b.canPostIn(c, c.i.ChannelID) {
		c.Reply(true, errEmbed("You cannot send messages in this channel."))
		return
	}
	text := b.spokenText(c.GuildID(), msg)
	if text == "" {
		c.Reply(true, errEmbed("That message has no text to speak."))
		return
	}
	b.tts.mu.Lock()
	if b.tts.speak == nil {
		b.tts.speak = map[string]speakText{}
	}
	for k, v := range b.tts.speak {
		if time.Since(v.at) > speakFor {
			delete(b.tts.speak, k)
		}
	}
	key := c.i.ID
	b.tts.speak[key] = speakText{text, time.Now()}
	b.tts.mu.Unlock()

	customID := sayModalID + ":" + c.i.ChannelID + ":" + msg.ID + ":" + key
	maxChars := b.ttsMaxChars()
	if utf8.RuneCountInString(text) <= maxChars {
		b.openSayModal(c, customID, "Speak this message", "", false)
		return
	}
	b.openSayModal(c, customID, "Speak this message (too long)", cutText(text, maxChars), true)
}

// takeSpeakText returns and forgets the text of a "Speak this message"
// form, or "" when it is not kept (any more).
func (b *Bot) takeSpeakText(key string) string {
	b.tts.mu.Lock()
	defer b.tts.mu.Unlock()
	v, ok := b.tts.speak[key]
	delete(b.tts.speak, key)
	if !ok || time.Since(v.at) > speakFor {
		return ""
	}
	return v.text
}

var (
	userMentionRE    = regexp.MustCompile(`<@!?(\d+)>`)
	roleMentionRE    = regexp.MustCompile(`<@&(\d+)>`)
	channelMentionRE = regexp.MustCompile(`<#(\d+)>`)
	inlineEmojiRE    = regexp.MustCompile(`<a?:(\w+):\d+>`)
	timestampRE      = regexp.MustCompile(`<t:\d+(?::\w)?>`)
)

// spokenText makes a message's text ready to speak: mentions become
// names, custom emoji their names, and links the word "link".
func (b *Bot) spokenText(guildID string, m *discordgo.Message) string {
	text := userMentionRE.ReplaceAllStringFunc(m.Content, func(s string) string {
		id := userMentionRE.FindStringSubmatch(s)[1]
		for _, u := range m.Mentions {
			if u.ID == id {
				if nick, _ := b.cachedNick(guildID, id); nick != "" {
					return "@" + nick
				}
				return "@" + cmp.Or(u.GlobalName, u.Username)
			}
		}
		return "someone"
	})
	text = roleMentionRE.ReplaceAllStringFunc(text, func(s string) string {
		if r, err := b.s.State.Role(guildID, roleMentionRE.FindStringSubmatch(s)[1]); err == nil {
			return "@" + r.Name
		}
		return "a role"
	})
	text = channelMentionRE.ReplaceAllStringFunc(text, func(s string) string {
		if ch, err := b.s.State.Channel(channelMentionRE.FindStringSubmatch(s)[1]); err == nil {
			return "#" + ch.Name
		}
		return "a channel"
	})
	text = inlineEmojiRE.ReplaceAllString(text, "$1")
	text = timestampRE.ReplaceAllString(text, "a time")
	text = linkRE.ReplaceAllString(text, "link")
	return strings.Join(strings.Fields(text), " ")
}

// cutText cuts text to at most n characters, at a full word when it can.
func cutText(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	cut := string(r[:n])
	if i := strings.LastIndexAny(cut, " \n"); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut)
}

// openSayModal opens the /say modal with the text (when withText is true,
// filled with prefill), the voice, and the format, and the extra fields
// after them.
func (b *Bot) openSayModal(c *Ctx, customID, title, prefill string, withText bool, extra ...discordgo.MessageComponent) {
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
	var fields []discordgo.MessageComponent
	if withText {
		fields = append(fields, modalLabel{Label: "Text", Description: fmt.Sprintf("What to say (at most %d characters)", maxChars),
			Component: modalText{CustomID: sayText, Style: discordgo.TextInputParagraph, MinLength: 1, MaxLength: maxChars, Value: prefill}})
	}
	fields = append(fields,
		modalLabel{Label: "Voice", Component: modalSelect{CustomID: sayVoice, Options: options}},
		modalLabel{Label: "Format", Component: modalSelect{CustomID: sayFormat, Options: []discordgo.SelectMenuOption{
			{Label: "Voice message", Value: sayFormatMsg, Description: "Without the text", Default: pref.voiceMsg},
			{Label: "Audio file", Value: sayFormatOgg, Description: "With the text", Default: !pref.voiceMsg},
		}}},
	)
	c.Modal(customID, title, append(fields, extra...)...)
}

// ttsEnabled checks TTS_ENABLED, and tells the member when it is off.
func (b *Bot) ttsEnabled(c *Ctx) bool {
	if !b.cfg.Bool("TTS_ENABLED") {
		c.Reply(true, errEmbed("Speech clips are off (`TTS_ENABLED` in `/setup`)."))
		return false
	}
	return true
}

// ttsChannelAllowed checks TTS_CHANNELS for the channel to post in (for a
// thread, its parent), and tells the member when it is not allowed.
func (b *Bot) ttsChannelAllowed(c *Ctx, channelID string) bool {
	if chans := b.cfg.List("TTS_CHANNELS"); len(chans) > 0 && !slices.Contains(chans, b.parentOf(channelID).ID) {
		c.Reply(true, errEmbed(fmt.Sprintf("Speech clips do not work in <#%s> (`TTS_CHANNELS` in `/setup`).", channelID)))
		return false
	}
	return true
}

func (b *Bot) ttsMaxChars() int {
	return min(max(b.cfg.Int("TTS_MAX_CHARS"), 1), ttsHardMaxChars)
}

// sayTarget is where a clip goes: a channel, and a message to reply to
// ("" for none).
type sayTarget struct {
	channelID, replyTo string
}

func (b *Bot) onSayModal(c *Ctx) {
	if !b.isMod(c.Member()) {
		c.Reply(true, errEmbed("You must be a moderator to use that command."))
		return
	}
	if !b.ttsEnabled(c) {
		return
	}
	var target sayTarget
	if len(c.Args) >= 2 {
		target = sayTarget{c.Args[0], c.Args[1]} // from the message menu
	} else if v := c.ModalValues(sayChannel); len(v) == 1 {
		target.channelID = v[0]
		if !b.canPostIn(c, target.channelID) {
			c.Reply(true, errEmbed(fmt.Sprintf("You cannot send messages in <#%s>.", target.channelID)))
			return
		}
	}
	if target.channelID == "" {
		c.Reply(true, errEmbed("Select a channel."))
		return
	}
	if !b.ttsChannelAllowed(c, target.channelID) {
		return
	}
	text := strings.TrimSpace(c.ModalValue(sayText))
	if len(c.Args) == 3 {
		// Speak this message: the kept text, unless the form had a text box.
		kept := b.takeSpeakText(c.Args[2])
		if text == "" {
			text = kept
		}
		if text == "" {
			c.Reply(true, errEmbed("The form was open too long. Try again."))
			return
		}
	}
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
	b.say(c, target, text, voice, voiceMsg)
}

// canPostIn reports whether the member may view and send messages in a
// channel (for a thread, in its parent), so /say cannot post where the mod
// cannot.
func (b *Bot) canPostIn(c *Ctx, channelID string) bool {
	g, err := b.s.State.Guild(c.GuildID())
	if err != nil {
		return false
	}
	ch := b.parentOf(channelID)
	if ch.GuildID != c.GuildID() || !slices.Contains(sayChannelTypes, ch.Type) {
		return false
	}
	if isAdmin(c.Member()) {
		return true
	}
	need := int64(discordgo.PermissionViewChannel | discordgo.PermissionSendMessages)
	return channelPerms(g, ch, c.UserID(), c.Member().Roles)&need == need
}

// parentOf returns a cached channel, or the parent of a thread. An unknown
// channel gives an empty channel with only its ID.
func (b *Bot) parentOf(channelID string) *discordgo.Channel {
	ch, err := b.s.State.Channel(channelID)
	if err != nil {
		return &discordgo.Channel{ID: channelID}
	}
	if ch.IsThread() {
		if p, err := b.s.State.Channel(ch.ParentID); err == nil {
			return p
		}
	}
	return ch
}

// say sends the clip to the worker's queue and follows it (ttsWatch). The
// member sees the queue position, the status, and the errors in an
// ephemeral reply, which is deleted when the clip is posted.
func (b *Bot) say(c *Ctx, target sayTarget, text, voice string, voiceMsg bool) {
	wait, ok := b.ttsEnqueue(c.UserID())
	if !ok {
		if wait > 0 {
			c.Reply(true, errEmbed(fmt.Sprintf("Wait %s before you use `/say` again.", humaniseDuration(int64((wait+time.Second-1)/time.Second)))))
		} else {
			c.Reply(true, errEmbed("The speech queue is full. Try again later."))
		}
		return
	}

	c.Defer(true)
	ctx, cancel := context.WithTimeout(context.Background(), ttsRequestTimeout)
	id, err := b.ttsSubmit(ctx, text, voice)
	cancel()
	if err != nil {
		b.ttsDone()
		msg := "The speech worker is not available. Try again later."
		if errors.Is(err, errTTSVoice) {
			msg = fmt.Sprintf("There is no voice named `%s`. Use `/voices` to see them.", voice)
		} else {
			log.Printf("say: %v", err)
		}
		c.Reply(true, errEmbed(msg))
		return
	}
	j := db.TTSJob{WorkerID: id, GuildID: c.GuildID(), UserID: c.UserID(), ChannelID: target.channelID,
		ReplyTo: target.replyTo, Voice: voice, Text: text, VoiceMsg: voiceMsg,
		AppID: c.i.AppID, Token: c.i.Token, Created: time.Now()}
	if err := b.db.AddTTSJob(j); err != nil {
		log.Printf("say: save job: %v", err) // it runs, but not after a restart
	}
	b.ttsWatch(j)
}

// resumeTTSJobs follows the clip jobs that were open when the bot stopped.
func (b *Bot) resumeTTSJobs() {
	if b.env.TTSURL == "" {
		return
	}
	jobs, err := b.db.TTSJobs()
	if err != nil {
		log.Printf("say: load jobs: %v", err)
		return
	}
	if len(jobs) > 0 {
		log.Printf("Speech: %d clips from before the restart", len(jobs))
	}
	b.tts.mu.Lock()
	b.tts.pending += len(jobs)
	b.tts.mu.Unlock()
	for _, j := range jobs {
		go b.ttsWatch(j)
	}
}

// ttsWatch asks the worker for the state of a clip job every ttsPoll, and
// shows the member the queue position when it changes. When the clip is
// done, it posts it and deletes the status. It ends the job on the worker,
// in tts_jobs, and in the bot's queue count. If the worker does not
// answer (for example, while it restarts), it tries again until ttsTimeout
// after the /say. When the bot stops, the job stays for the next start.
func (b *Bot) ttsWatch(j db.TTSJob) {
	defer b.ttsDone()
	it := &discordgo.Interaction{AppID: j.AppID, Token: j.Token}
	status := func(e *discordgo.MessageEmbed) {
		if _, err := b.s.InteractionResponseEdit(it, &discordgo.WebhookEdit{Embeds: &[]*discordgo.MessageEmbed{e}}); err != nil {
			log.Printf("say: status: %v", err)
		}
	}
	finish := func(result, errMsg string) {
		b.m.ttsJobs.WithLabelValues(result).Inc()
		if result == "ok" {
			b.m.ttsSeconds.Observe(time.Since(j.Created).Seconds())
		}
		if errMsg != "" {
			status(errEmbed(errMsg))
		}
		ctx, cancel := context.WithTimeout(context.Background(), ttsRequestTimeout)
		defer cancel()
		if err := b.ttsDelete(ctx, j.WorkerID); err != nil {
			log.Printf("say: delete job: %v", err)
		}
		if err := b.db.DeleteTTSJob(j.WorkerID); err != nil {
			log.Printf("say: delete job: %v", err)
		}
	}

	deadline := time.NewTimer(time.Until(j.Created.Add(ttsTimeout)))
	defer deadline.Stop()
	shown := -1 // the position on the status; 0 is "making"
	for done, logged := false, false; !done; {
		st, err := b.ttsStatus(j.WorkerID)
		switch {
		case errors.Is(err, errTTSGone):
			finish("lost", "The speech worker lost the clip. Try again.")
			return
		case err != nil:
			if !logged {
				log.Printf("say: job state: %v", err)
				logged = true
			}
		case st.State == "done":
			done = true
			continue
		case st.State == "failed":
			msg := "The speech worker could not make the clip. Try again later."
			if st.Error == "unknown voice" {
				msg = fmt.Sprintf("There is no voice named `%s`. Use `/voices` to see them.", j.Voice)
			}
			finish("failed", msg)
			return
		case st.State == "running" && shown != 0:
			shown = 0
			status(makeEmbed("🗣️ Making the clip", "This takes about 15–30 seconds.", colourBlurple))
		case st.State == "queued" && shown != max(st.Ahead, 1):
			shown = max(st.Ahead, 1)
			status(makeEmbed("⏳ In the queue",
				fmt.Sprintf("Position %d. Each clip takes about 15–30 seconds.", shown), colourBlurple))
		}
		select {
		case <-time.After(b.ttsPollEvery):
		case <-deadline.C:
			finish("timeout", "The speech queue took too long. Try again later.")
			return
		case <-b.stop:
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), ttsRequestTimeout)
	defer cancel()
	wav, err := b.ttsAudio(ctx, j.WorkerID)
	if err != nil {
		log.Printf("say: %v", err)
		finish("worker_error", "The speech worker is not available. Try again later.")
		return
	}
	ogg, err := b.toOpus(ctx, wav)
	if err != nil {
		log.Printf("say: encode: %v", err)
		finish("encode_error", "I could not encode the clip.")
		return
	}
	target := sayTarget{j.ChannelID, j.ReplyTo}
	posted, sentAsVoice, err := b.postClip(target, ogg, wav, j.Voice, j.Text, j.VoiceMsg)
	if err != nil {
		log.Printf("say: post: %v", err)
		finish("post_error", fmt.Sprintf("I could not post in <#%s>. I need Send Messages, Attach Files, and Send Voice Messages there.", target.channelID))
		return
	}
	if err := b.s.InteractionResponseDelete(it); err != nil {
		log.Printf("say: delete status: %v", err)
	}
	b.logSay(j.GuildID, j.UserID, target, posted, j.Voice, j.Text, sentAsVoice)
	finish("ok", "")
}

// postClip posts the clip in the target channel: as a voice message when
// voiceMsg is true, and as an audio file with the voice name (and the text,
// unless a voice message was asked for) when Discord refuses that or
// voiceMsg is false. It returns the message and whether it is a voice
// message.
func (b *Bot) postClip(target sayTarget, ogg, wav []byte, voice, text string, voiceMsg bool) (*discordgo.Message, bool, error) {
	var ref *discordgo.MessageReference
	if target.replyTo != "" {
		ref = &discordgo.MessageReference{MessageID: target.replyTo, ChannelID: target.channelID, FailIfNotExists: new(bool)}
	}
	noPing := &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
	if voiceMsg {
		m, err := b.postVoiceMessage(target.channelID, ref, noPing, ogg, wav)
		if err == nil {
			return m, true, nil
		}
		log.Printf("say: voice message: %v", err) // send it as a file instead
	}
	embed := makeEmbed("🗣️ "+voice, text, colourBlurple)
	if voiceMsg {
		embed.Description = "" // the member asked for no text
	}
	m, err := b.s.ChannelMessageSendComplex(target.channelID, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{embed},
		Files:           []*discordgo.File{{Name: "say.ogg", ContentType: "audio/ogg", Reader: bytes.NewReader(ogg)}},
		Reference:       ref,
		AllowedMentions: noPing,
	})
	return m, false, err
}

// postVoiceMessage posts ogg as a voice message. discordgo cannot send the
// attachment's length and waveform, so the request is made by hand. A
// voice message has no text and no embeds.
func (b *Bot) postVoiceMessage(channelID string, ref *discordgo.MessageReference, mentions *discordgo.MessageAllowedMentions, ogg, wav []byte) (*discordgo.Message, error) {
	secs, wave := wavWaveform(wav)
	payload := map[string]any{
		"flags":            discordgo.MessageFlagsIsVoiceMessage,
		"allowed_mentions": mentions,
		"attachments": []map[string]any{{
			"id": "0", "filename": "voice-message.ogg", "duration_secs": secs, "waveform": wave,
		}},
	}
	if ref != nil {
		payload["message_reference"] = ref
	}
	files := []*discordgo.File{{Name: "voice-message.ogg", ContentType: "audio/ogg", Reader: bytes.NewReader(ogg)}}
	ctype, body, err := discordgo.MultipartBodyWithJSON(payload, files)
	if err != nil {
		return nil, err
	}
	url := discordgo.EndpointChannelMessages(channelID)
	resp, err := b.s.RequestRaw(http.MethodPost, url, ctype, body, url, 0)
	if err != nil {
		return nil, err
	}
	var m discordgo.Message
	return &m, json.Unmarshal(resp, &m)
}

// logSay tells the mod log who posted a clip and what it says, with a link
// to it.
func (b *Bot) logSay(guildID, userID string, target sayTarget, posted *discordgo.Message, voice, text string, voiceMsg bool) {
	kind := "an audio clip"
	if voiceMsg {
		kind = "a voice message"
	}
	desc := fmt.Sprintf("<@%s> posted %s (voice: `%s`) in <#%s>", userID, kind, voice, target.channelID)
	if target.replyTo != "" {
		desc += fmt.Sprintf(", as a reply to [this message](https://discord.com/channels/%s/%s/%s)",
			guildID, target.channelID, target.replyTo)
	}
	if posted != nil && posted.ID != "" {
		desc += fmt.Sprintf(".\n[Jump to the clip](https://discord.com/channels/%s/%s/%s)", guildID, target.channelID, posted.ID)
	}
	embed := makeEmbed("🗣️ Speech clip", desc, colourBlurple)
	embed.Fields = []*discordgo.MessageEmbedField{{Name: "Text", Value: text}}
	b.modLog(embed)
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

// ttsEnqueue counts a new clip job. When the user must wait (wait > 0) or
// the queue is full, ok is false.
func (b *Bot) ttsEnqueue(userID string) (wait time.Duration, ok bool) {
	st := &b.tts
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	cooldown := time.Duration(max(b.cfg.Int("TTS_COOLDOWN"), 0)) * time.Second
	if t, seen := st.last[userID]; seen && now.Sub(t) < cooldown {
		return cooldown - now.Sub(t), false
	}
	// One clip runs; at most TTS_QUEUE_MAX wait.
	if st.pending > max(b.cfg.Int("TTS_QUEUE_MAX"), 0) {
		return 0, false
	}
	if st.last == nil {
		st.last = map[string]time.Time{}
	}
	st.last[userID] = now
	st.pending++
	return 0, true
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

// ttsJobState is the worker's answer to GET /jobs/<id>.
type ttsJobState struct {
	State string `json:"state"` // queued, running, done, or failed
	Ahead int    `json:"ahead"` // clips before it, with the one that runs
	Error string `json:"error"`
}

// ttsSubmit adds a clip to the worker's queue and returns the job ID.
func (b *Bot) ttsSubmit(ctx context.Context, text, voice string) (string, error) {
	resp, err := b.ttsDo(ctx, http.MethodPost, "/jobs", map[string]string{"text": text, "voice": voice})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusAccepted:
	case http.StatusNotFound:
		return "", errTTSVoice
	default:
		return "", ttsStatusErr(resp)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.ID == "" {
		return "", fmt.Errorf("worker: bad job answer: %v", err)
	}
	return out.ID, nil
}

// ttsStatus returns the state of a clip job, or errTTSGone when the
// worker does not know it.
func (b *Bot) ttsStatus(id string) (ttsJobState, error) {
	var st ttsJobState
	ctx, cancel := context.WithTimeout(context.Background(), ttsRequestTimeout)
	defer cancel()
	resp, err := b.ttsDo(ctx, http.MethodGet, "/jobs/"+id, nil)
	if err != nil {
		return st, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return st, errTTSGone
	default:
		return st, ttsStatusErr(resp)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// ttsAudio returns the WAV data of a done clip job.
func (b *Bot) ttsAudio(ctx context.Context, id string) ([]byte, error) {
	resp, err := b.ttsDo(ctx, http.MethodGet, "/jobs/"+id+"/audio", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ttsStatusErr(resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// ttsDelete tells the worker that the bot does not need a job any more.
func (b *Bot) ttsDelete(ctx context.Context, id string) error {
	resp, err := b.ttsDo(ctx, http.MethodDelete, "/jobs/"+id, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return ttsStatusErr(resp)
	}
	return nil
}

func ttsStatusErr(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("worker: %s: %s", resp.Status, bytes.TrimSpace(msg))
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
