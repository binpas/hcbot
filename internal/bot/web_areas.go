package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/comic"
	"github.com/binpas/hcbot/internal/config"
	"github.com/binpas/hcbot/internal/db"
	"github.com/binpas/hcbot/internal/markov"
)

// The other areas of the web interface: Settings, Speech, Markov and
// Persona. Everything that posts checks the same limits as the slash
// commands, and writes the mod log: the Discord post does not show which
// mod made it.

const (
	webMaxVoice      = 21 << 20 // a voice of 20 MB, and the form around it
	webMaxChatInput  = 2000
	webCandLife      = 15 * time.Minute // how long a generated candidate can be sent
	webJobsKept      = 20
	webClipTimeout   = time.Minute
	webVoiceTimeout  = 2 * time.Minute
	webSettingMaxLen = 1000
	webLongTextMax   = 500
	webListMax       = 25 // the same limit as the select menus of /setup
)

var (
	webClipDayRE  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	webClipFileRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.(ogg|wav)$`)
	webVoiceRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	webVoiceExts  = []string{".wav", ".mp3", ".flac", ".ogg", ".m4a"}
)

// webCand is a generated candidate. Only text that the chain made can be
// sent, so that the page is not a way to post any text as the bot.
type webCand struct {
	chain, text, userID string
	at                  time.Time
}

// webJob is the state of a clip that was started on the page, for the Send
// view. In memory only.
type webJob struct {
	ID      string `json:"id"`
	UserID  string `json:"-"`
	Channel string `json:"channel"`
	Voice   string `json:"voice"`
	Text    string `json:"text"`
	State   string `json:"state"` // working, done, or failed
	Detail  string `json:"detail"`
	Link    string `json:"link,omitempty"`
	at      time.Time
}

func (b *Bot) registerWebAreas(route func(string, webLevel, func(http.ResponseWriter, *http.Request, *webSession))) {
	route("GET /api/guild", webMod, b.webGuild)
	route("GET /api/settings", webMod, b.webSettings)
	route("PUT /api/settings/{key}", webAdmin, b.webSetSetting)

	route("GET /api/speech", webMod, b.webSpeech)
	route("POST /api/speech/send", webMod, b.webSpeechSend)
	route("GET /api/speech/jobs", webMod, b.webSpeechJobs)
	route("GET /api/speech/days", webMod, b.webClipDays)
	route("GET /api/speech/clips", webMod, b.webClipList)
	route("GET /audio/clip/{day}/{file}", webMod, b.webClipAudio)
	route("POST /api/speech/clips/post", webMod, b.webClipPost)
	route("POST /api/speech/voices", webAdmin, b.webVoiceUpload)
	route("DELETE /api/speech/voices/{name}", webAdmin, b.webVoiceRemove)

	route("POST /api/chat/{chain}/generate", webMod, b.webChatGenerate)
	route("POST /api/chat/{chain}/send", webMod, b.webChatSend)
}

// webFeatures tells the page which areas work, with the reason when one is
// off.
func (b *Bot) webFeatures() map[string]string {
	off := map[string]string{}
	if b.comics == nil {
		off["comics"] = "Comics are off on this bot. An operator must turn comics on."
	}
	if why := b.webSpeechOff(); why != "" {
		off["speech"] = why
	}
	if why := b.webChainOff("markov"); why != "" {
		off["markov"] = why
	}
	if why := b.webChainOff("persona"); why != "" {
		off["persona"] = why
	}
	return off
}

// ---- Channels and roles. ----

type webNamed struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

func (b *Bot) webGuild(w http.ResponseWriter, _ *http.Request, _ *webSession) {
	g, err := b.s.State.Guild(b.env.GuildID)
	if err != nil {
		webFail(w, http.StatusBadGateway, "I could not read the server from Discord. Try again.")
		return
	}
	chans := make([]*discordgo.Channel, 0, len(g.Channels))
	for _, ch := range g.Channels {
		if !ch.IsThread() {
			chans = append(chans, ch)
		}
	}
	sort.SliceStable(chans, func(i, j int) bool { return chans[i].Position < chans[j].Position })
	channels := []webNamed{}
	for _, ch := range chans {
		kind := "channel"
		if ch.Type == discordgo.ChannelTypeGuildCategory {
			kind = "category"
		}
		channels = append(channels, webNamed{ID: ch.ID, Name: ch.Name, Kind: kind})
	}
	roles := slices.Clone(g.Roles)
	sort.SliceStable(roles, func(i, j int) bool { return roles[i].Position > roles[j].Position })
	roleList := []webNamed{}
	for _, r := range roles {
		if r.ID != g.ID && !r.Managed { // not @everyone, and not a bot's role
			roleList = append(roleList, webNamed{ID: r.ID, Name: r.Name})
		}
	}
	webJSON(w, http.StatusOK, map[string]any{"channels": channels, "roles": roleList})
}

// webPostableFilter gives a test for the channels where a member can post, as /say does.
func (b *Bot) webPostableFilter(s *webSession) (func(*discordgo.Channel) bool, error) {
	if s.admin {
		return func(*discordgo.Channel) bool { return true }, nil
	}
	g, err := b.s.State.Guild(b.env.GuildID)
	if err != nil {
		return nil, err
	}
	m, err := b.s.GuildMember(b.env.GuildID, s.userID)
	if err != nil {
		return nil, err
	}
	need := int64(discordgo.PermissionViewChannel | discordgo.PermissionSendMessages)
	return func(ch *discordgo.Channel) bool {
		return channelPerms(g, ch, s.userID, m.Roles)&need == need
	}, nil
}

// webApproved lists the channels where a member can post: of the types that
// /say uses, in the list (an empty list with anyWhenEmpty means all), and
// where the member can post.
func (b *Bot) webApproved(s *webSession, list []string, anyWhenEmpty bool) ([]webNamed, error) {
	can, err := b.webPostableFilter(s)
	if err != nil {
		return nil, err
	}
	g, err := b.s.State.Guild(b.env.GuildID)
	if err != nil {
		return nil, err
	}
	chans := slices.Clone(g.Channels)
	sort.SliceStable(chans, func(i, j int) bool { return chans[i].Position < chans[j].Position })
	out := []webNamed{}
	for _, ch := range chans {
		if !slices.Contains(sayChannelTypes, ch.Type) || !can(ch) {
			continue
		}
		if slices.Contains(list, ch.ID) || (len(list) == 0 && anyWhenEmpty) {
			out = append(out, webNamed{ID: ch.ID, Name: ch.Name})
		}
	}
	return out, nil
}

func webHasChannel(list []webNamed, id string) bool {
	return slices.ContainsFunc(list, func(n webNamed) bool { return n.ID == id })
}

// ---- Settings. ----

type webSetting struct {
	Key         string          `json:"key"`
	Kind        string          `json:"kind"`
	Description string          `json:"description"`
	Value       string          `json:"value"`
	Default     string          `json:"default"`
	Display     string          `json:"display,omitempty"` // a member's name
	Choices     []config.Choice `json:"choices,omitempty"`
}

type webSection struct {
	Title    string       `json:"title"`
	Settings []webSetting `json:"settings"`
}

func (b *Bot) webSettingKey(key string) (config.SetupKey, bool) {
	if !b.env.MessageContent && contentOnlySettings[key] {
		return config.SetupKey{}, false
	}
	for _, k := range config.SetupKeys {
		if k.Key == key {
			return k, true
		}
	}
	return config.SetupKey{}, false
}

func (b *Bot) webSettings(w http.ResponseWriter, _ *http.Request, _ *webSession) {
	sections := []webSection{}
	for _, sec := range setupSections {
		out := webSection{Title: sec.title, Settings: []webSetting{}}
		for _, key := range sec.keys {
			k, ok := b.webSettingKey(key)
			if !ok {
				continue
			}
			item := webSetting{Key: k.Key, Kind: k.Kind, Description: k.Description, Value: b.cfg.Str(k.Key), Default: k.Default, Choices: config.Choices[k.Key]}
			if k.Kind == config.KindUser && item.Value != "" {
				item.Display, _ = b.webLookupName(item.Value)
			}
			out.Settings = append(out.Settings, item)
		}
		if len(out.Settings) > 0 {
			sections = append(sections, out)
		}
	}
	webJSON(w, http.StatusOK, map[string]any{"sections": sections})
}

// webCheckSetting checks a new value, with the same rules as /setup. It
// returns the value to save, or the text of the problem.
func (b *Bot) webCheckSetting(k config.SetupKey, raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	g, err := b.s.State.Guild(b.env.GuildID)
	if err != nil {
		return "", "I could not read the server from Discord. Try again."
	}
	channelOK := func(id string, category bool) bool {
		return slices.ContainsFunc(g.Channels, func(ch *discordgo.Channel) bool {
			return ch.ID == id && (ch.Type == discordgo.ChannelTypeGuildCategory) == category
		})
	}
	roleOK := func(id string) bool {
		return slices.ContainsFunc(g.Roles, func(r *discordgo.Role) bool { return r.ID == id })
	}
	list := func(ok func(string) bool, what string) (string, string) {
		var out []string
		for _, id := range config.ParseIDList(raw) {
			if !ok(id) {
				return "", fmt.Sprintf("`%s` is not a %s of this server.", id, what)
			}
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		if len(out) > webListMax {
			return "", fmt.Sprintf("Select at most %d.", webListMax)
		}
		return strings.Join(out, ","), ""
	}
	switch k.Kind {
	case config.KindInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return "", "Type a whole number."
		}
		return strconv.Itoa(n), ""
	case config.KindDecimal:
		f, err := config.ParseDecimal(raw)
		if err != nil {
			return "", "Type a number, in a form like 0.05."
		}
		return strconv.FormatFloat(f, 'f', -1, 64), ""
	case config.KindBool:
		if _, ok := config.ParseBool(raw); !ok {
			return "", "The value must be on or off."
		}
		return raw, ""
	case config.KindChoice:
		if config.ChoiceLabel(k.Key, raw) == "" {
			return "", "Select one of the values in the list."
		}
		return raw, ""
	case config.KindChannel, config.KindCategory:
		if raw != "" && !channelOK(raw, k.Kind == config.KindCategory) {
			return "", "That is not a " + k.Kind + " of this server."
		}
		return raw, ""
	case config.KindRole:
		if raw != "" && !roleOK(raw) {
			return "", "That is not a role of this server."
		}
		return raw, ""
	case config.KindUser:
		if raw != "" && !comic.IsUserID(raw) {
			return "", "That is not a Discord user ID."
		}
		return raw, ""
	case config.KindChanList:
		return list(func(id string) bool { return channelOK(id, false) }, "channel")
	case config.KindRoleList:
		return list(roleOK, "role")
	case config.KindLongText:
		if utf8.RuneCountInString(raw) > webLongTextMax {
			return "", fmt.Sprintf("The text can have at most %d characters.", webLongTextMax)
		}
		return raw, ""
	}
	if utf8.RuneCountInString(raw) > webSettingMaxLen {
		return "", fmt.Sprintf("The text can have at most %d characters.", webSettingMaxLen)
	}
	return raw, ""
}

func (b *Bot) webSetSetting(w http.ResponseWriter, r *http.Request, s *webSession) {
	k, ok := b.webSettingKey(r.PathValue("key"))
	if !ok {
		webFail(w, http.StatusNotFound, "There is no setting with that name.")
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	if !webReadJSON(w, r, &req) {
		return
	}
	value, problem := b.webCheckSetting(k, req.Value)
	if problem != "" {
		webFail(w, http.StatusBadRequest, problem)
		return
	}
	if err := config.CheckValue(k.Key, value); err != nil {
		webFail(w, http.StatusBadRequest, err.Error())
		return
	}
	old := b.cfg.Str(k.Key)
	if err := b.setConfig(k.Key, value); err != nil {
		log.Printf("web: save %s: %v", k.Key, err)
		webFail(w, http.StatusInternalServerError, "I could not save the change. Try again.")
		return
	}
	if old != value {
		b.webAudit(s, fmt.Sprintf("changed the setting `%s` from `%s` to `%s`", k.Key, cutText(webOrNone(old), 200), cutText(webOrNone(value), 200)))
	}
	webJSON(w, http.StatusOK, map[string]string{"value": value})
}

func webOrNone(s string) string {
	if s == "" {
		return "(not set)"
	}
	return s
}

// ---- Speech. ----

func (b *Bot) webSpeechOff() string {
	switch {
	case b.env.TTSURL == "":
		return "Speech is off on this bot: the worker address is not set. An operator must set TTS_URL."
	case !b.cfg.Bool("TTS_ENABLED"):
		return "Speech is off. An Administrator can turn on TTS_ENABLED in Settings."
	}
	return ""
}

func (b *Bot) webSpeechReady(w http.ResponseWriter) bool {
	if why := b.webSpeechOff(); why != "" {
		webFail(w, http.StatusServiceUnavailable, why)
		return false
	}
	return true
}

func (b *Bot) webSpeechChannels(s *webSession) ([]webNamed, error) {
	return b.webApproved(s, b.cfg.List("TTS_CHANNELS"), true)
}

func (b *Bot) webSpeech(w http.ResponseWriter, r *http.Request, s *webSession) {
	if !b.webSpeechReady(w) {
		return
	}
	channels, err := b.webSpeechChannels(s)
	if err != nil {
		log.Printf("web: speech channels: %v", err)
		webFail(w, http.StatusBadGateway, "I could not read your channels from Discord. Try again.")
		return
	}
	voices, err := b.ttsVoices(r.Context(), r.URL.Query().Get("fresh") == "1")
	if err != nil {
		log.Printf("web: voices: %v", err)
		webFail(w, http.StatusBadGateway, "The speech worker is not available. Try again later.")
		return
	}
	webJSON(w, http.StatusOK, map[string]any{"voices": voices, "channels": channels,
		"maxChars": b.ttsMaxChars(), "cooldown": max(b.cfg.Int("TTS_COOLDOWN"), 0)})
}

func (b *Bot) webSpeechSend(w http.ResponseWriter, r *http.Request, s *webSession) {
	if !b.webSpeechReady(w) {
		return
	}
	var req struct {
		Text     string `json:"text"`
		Voice    string `json:"voice"`
		Channel  string `json:"channel"`
		VoiceMsg bool   `json:"voiceMsg"`
	}
	if !webReadJSON(w, r, &req) {
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		webFail(w, http.StatusBadRequest, "Type the text.")
		return
	}
	if n := b.ttsMaxChars(); utf8.RuneCountInString(text) > n {
		webFail(w, http.StatusBadRequest, fmt.Sprintf("The text can have at most %d characters.", n))
		return
	}
	voice := strings.ToLower(strings.TrimSpace(req.Voice))
	if voice == "" {
		voice = ttsRandomVoice
	}
	if !b.webSpeechChannelOK(w, s, req.Channel) {
		return
	}
	wait, ok := b.ttsEnqueue(s.userID)
	if !ok {
		if wait > 0 {
			webFail(w, http.StatusTooManyRequests, fmt.Sprintf("Wait %s before you make another clip.", humaniseDuration(int64((wait+time.Second)/time.Second))))
		} else {
			webFail(w, http.StatusTooManyRequests, "The speech queue is full. Try again later.")
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ttsRequestTimeout)
	id, err := b.ttsSubmit(ctx, text, voice)
	cancel()
	if err != nil {
		b.ttsDone()
		if errors.Is(err, errTTSVoice) {
			webFail(w, http.StatusBadRequest, fmt.Sprintf("There is no voice named %s.", cutText(voice, 40)))
			return
		}
		log.Printf("web: say: %v", err)
		webFail(w, http.StatusBadGateway, "The speech worker is not available. Try again later.")
		return
	}
	// No interaction token: this job reports to the page, and not to Discord.
	j := db.TTSJob{WorkerID: id, GuildID: b.env.GuildID, UserID: s.userID, ChannelID: req.Channel,
		Voice: voice, Text: text, VoiceMsg: req.VoiceMsg, Created: time.Now()}
	if err := b.db.AddTTSJob(j); err != nil {
		log.Printf("web: say: save job: %v", err) // it runs, but not after a restart
	}
	b.webJobSet(webJob{ID: id, UserID: s.userID, Channel: req.Channel, Voice: voice, Text: text, State: "working", Detail: "In the queue."})
	go b.ttsWatch(j)
	webJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

// webSpeechChannelOK checks that a member can post a clip in a channel.
func (b *Bot) webSpeechChannelOK(w http.ResponseWriter, s *webSession, id string) bool {
	if id == "" {
		webFail(w, http.StatusBadRequest, "Select a channel.")
		return false
	}
	channels, err := b.webSpeechChannels(s)
	if err != nil {
		log.Printf("web: speech channels: %v", err)
		webFail(w, http.StatusBadGateway, "I could not read your channels from Discord. Try again.")
		return false
	}
	if !webHasChannel(channels, id) {
		webFail(w, http.StatusForbidden, "You cannot post speech clips in that channel. See TTS_CHANNELS in Settings.")
		return false
	}
	return true
}

func (b *Bot) webJobSet(j webJob) {
	j.at = time.Now()
	b.web.mu.Lock()
	defer b.web.mu.Unlock()
	if b.web.jobs == nil {
		b.web.jobs = map[string]webJob{}
	}
	b.web.jobs[j.ID] = j
	if len(b.web.jobs) > webJobsKept {
		var oldest string
		for id, o := range b.web.jobs {
			if oldest == "" || o.at.Before(b.web.jobs[oldest].at) {
				oldest = id
			}
		}
		delete(b.web.jobs, oldest)
	}
}

// webJobUpdate changes a job's state. A job from before a restart has no
// entry yet, so it gets one.
func (b *Bot) webJobUpdate(tj db.TTSJob, state, detail, link string) {
	b.webJobSet(webJob{ID: tj.WorkerID, UserID: tj.UserID, Channel: tj.ChannelID, Voice: tj.Voice, Text: tj.Text, State: state, Detail: detail, Link: link})
}

func (b *Bot) webSpeechJobs(w http.ResponseWriter, _ *http.Request, s *webSession) {
	b.web.mu.Lock()
	jobs := []webJob{}
	for _, j := range b.web.jobs {
		if j.UserID == s.userID {
			jobs = append(jobs, j)
		}
	}
	b.web.mu.Unlock()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].at.After(jobs[j].at) })
	webJSON(w, http.StatusOK, jobs)
}

// webWorkerGet makes a GET to the worker and returns the JSON as it is.
func (b *Bot) webWorkerGet(ctx context.Context, path string) ([]byte, int, error) {
	resp, err := b.ttsDo(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return data, resp.StatusCode, err
}

func webPassJSON(w http.ResponseWriter, data []byte, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (b *Bot) webClipDays(w http.ResponseWriter, r *http.Request, _ *webSession) {
	if !b.webSpeechReady(w) {
		return
	}
	data, status, err := b.webWorkerGet(r.Context(), "/clips/days")
	if err != nil || status != http.StatusOK {
		log.Printf("web: clip days: %d %v", status, err)
		webFail(w, http.StatusBadGateway, "The speech worker is not available. Try again later.")
		return
	}
	webPassJSON(w, data, http.StatusOK)
}

func (b *Bot) webClipList(w http.ResponseWriter, r *http.Request, _ *webSession) {
	if !b.webSpeechReady(w) {
		return
	}
	day := r.URL.Query().Get("day")
	if !webClipDayRE.MatchString(day) {
		webFail(w, http.StatusBadRequest, "That is not a day.")
		return
	}
	data, status, err := b.webWorkerGet(r.Context(), "/clips/list?day="+url.QueryEscape(day))
	if err != nil || (status != http.StatusOK && status != http.StatusNotFound) {
		log.Printf("web: clip list: %d %v", status, err)
		webFail(w, http.StatusBadGateway, "The speech worker is not available. Try again later.")
		return
	}
	if status == http.StatusNotFound {
		webFail(w, http.StatusNotFound, "There are no clips for that day.")
		return
	}
	webPassJSON(w, data, http.StatusOK)
}

func webClipPath(day, file string) (string, bool) {
	if !webClipDayRE.MatchString(day) || !webClipFileRE.MatchString(file) || strings.Contains(file, "..") {
		return "", false
	}
	return "/clips/file/" + url.PathEscape(day) + "/" + url.PathEscape(file), true
}

func (b *Bot) webClipAudio(w http.ResponseWriter, r *http.Request, _ *webSession) {
	if !b.webSpeechReady(w) {
		return
	}
	path, ok := webClipPath(r.PathValue("day"), r.PathValue("file"))
	if !ok {
		webFail(w, http.StatusNotFound, "Not found.")
		return
	}
	data, status, err := b.webWorkerGet(r.Context(), path)
	if err != nil || status != http.StatusOK {
		webFail(w, http.StatusNotFound, "I could not get this clip.")
		return
	}
	w.Header().Set("Content-Type", webClipType(r.PathValue("file")))
	_, _ = w.Write(data)
}

func webClipType(file string) string {
	if strings.HasSuffix(file, ".wav") {
		return "audio/wav"
	}
	return "audio/ogg"
}

// fromOpus makes a clip into 16-bit WAV data, for the waveform of a voice
// message.
func (b *Bot) fromOpus(ctx context.Context, ogg []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "hcbot-clip-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	in, out := filepath.Join(dir, "in.ogg"), filepath.Join(dir, "out.wav")
	if err := os.WriteFile(in, ogg, 0o600); err != nil {
		return nil, err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", in, "-c:a", "pcm_s16le", "-ac", "1", "-ar", "24000", out}
	if _, stderr, err := b.runTool(ctx, "ffmpeg", args); err != nil {
		return nil, fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr))
	}
	return os.ReadFile(out)
}

// webClipPost posts a saved clip. It makes nothing new, so it uses no GPU
// time and has no cooldown.
func (b *Bot) webClipPost(w http.ResponseWriter, r *http.Request, s *webSession) {
	if !b.webSpeechReady(w) {
		return
	}
	var req struct {
		Day      string `json:"day"`
		File     string `json:"file"`
		Channel  string `json:"channel"`
		VoiceMsg bool   `json:"voiceMsg"`
	}
	if !webReadJSON(w, r, &req) {
		return
	}
	path, ok := webClipPath(req.Day, req.File)
	if !ok {
		webFail(w, http.StatusBadRequest, "That is not a saved clip.")
		return
	}
	if !b.webSpeechChannelOK(w, s, req.Channel) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), webClipTimeout)
	defer cancel()
	voice, text := "", ""
	listed, status, err := b.webWorkerGet(ctx, "/clips/list?day="+url.QueryEscape(req.Day))
	if err == nil && status == http.StatusOK {
		var out struct {
			Clips []struct {
				File  string `json:"file"`
				Text  string `json:"text"`
				Voice string `json:"voice"`
			} `json:"clips"`
		}
		if json.Unmarshal(listed, &out) == nil {
			for _, c := range out.Clips {
				if c.File == req.File {
					voice, text = c.Voice, c.Text
				}
			}
		}
	}
	data, status, err := b.webWorkerGet(ctx, path)
	if err != nil || status != http.StatusOK {
		log.Printf("web: clip: %d %v", status, err)
		webFail(w, http.StatusNotFound, "I could not get this clip from the speech worker.")
		return
	}
	var ogg, wav []byte
	if strings.HasSuffix(req.File, ".wav") {
		wav = data
		ogg, err = b.toOpus(ctx, wav)
	} else {
		ogg = data
		if req.VoiceMsg {
			wav, err = b.fromOpus(ctx, ogg)
		}
	}
	if err != nil {
		log.Printf("web: clip encode: %v", err)
		webFail(w, http.StatusInternalServerError, "I could not read this clip.")
		return
	}
	target := sayTarget{channelID: req.Channel}
	posted, sentAsVoice, err := b.postClip(target, ogg, wav, cmpOr(voice, "clip"), text, req.VoiceMsg)
	if err != nil {
		log.Printf("web: clip post: %v", err)
		webFail(w, http.StatusBadGateway, "I could not post in that channel. I need Send Messages, Attach Files, and Send Voice Messages there.")
		return
	}
	b.logSay(b.env.GuildID, s.userID, target, posted, cmpOr(voice, "clip"), cmpOr(text, "(no text saved)"), sentAsVoice)
	webJSON(w, http.StatusOK, map[string]string{"link": b.webMessageLink(req.Channel, posted)})
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (b *Bot) webMessageLink(channelID string, m *discordgo.Message) string {
	if m == nil || m.ID == "" {
		return ""
	}
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", b.env.GuildID, channelID, m.ID)
}

// webVoiceUpload sends a voice file to the worker, which checks it and adds
// it. A voice can copy a real person's voice, so only Administrators can do
// this.
func (b *Bot) webVoiceUpload(w http.ResponseWriter, r *http.Request, s *webSession) {
	if !b.webSpeechReady(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, webMaxVoice)
	f, hdr, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			webFail(w, http.StatusRequestEntityTooLarge, "The file is larger than 20 MB.")
		} else {
			webFail(w, http.StatusBadRequest, "Select a file.")
		}
		return
	}
	defer func() { _ = f.Close() }()
	name := webArtName(r.FormValue("name"))
	if !webVoiceRE.MatchString(name) || name == ttsRandomVoice {
		webFail(w, http.StatusBadRequest, "The name must have 1 to 32 of a–z, 0–9, - and _, start with a letter or number, and not be \"random\".")
		return
	}
	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if !slices.Contains(webVoiceExts, ext) {
		webFail(w, http.StatusBadRequest, "Use a WAV, MP3, FLAC, OGG or M4A file.")
		return
	}
	replace := r.FormValue("replace") == "true"
	ctx, cancel := context.WithTimeout(r.Context(), webVoiceTimeout)
	defer cancel()
	q := url.Values{"ext": {ext}}
	if replace {
		q.Set("replace", "1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, b.env.TTSURL+"/voices/"+url.PathEscape(name)+"?"+q.Encode(), f)
	if err != nil {
		webFail(w, http.StatusInternalServerError, "I could not send the file.")
		return
	}
	req.ContentLength = hdr.Size
	req.Header.Set("Authorization", "Bearer "+b.env.TTSToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	b.webVoiceAnswer(w, req, s, fmt.Sprintf("saved the voice `%s`", name))
}

func (b *Bot) webVoiceRemove(w http.ResponseWriter, r *http.Request, s *webSession) {
	if !b.webSpeechReady(w) {
		return
	}
	name := r.PathValue("name")
	if !webVoiceRE.MatchString(name) {
		webFail(w, http.StatusNotFound, "There is no voice with that name.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), webVoiceTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.env.TTSURL+"/voices/"+url.PathEscape(name), nil)
	if err != nil {
		webFail(w, http.StatusInternalServerError, "I could not remove the voice.")
		return
	}
	req.Header.Set("Authorization", "Bearer "+b.env.TTSToken)
	b.webVoiceAnswer(w, req, s, fmt.Sprintf("removed the voice `%s`", name))
}

// webVoiceAnswer sends a voice change to the worker, and answers with the
// new list of voices.
func (b *Bot) webVoiceAnswer(w http.ResponseWriter, req *http.Request, s *webSession, what string) {
	// The worker reads the audio, which can take longer than a usual request.
	client := &http.Client{Timeout: webVoiceTimeout, Transport: b.ttsHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("web: voice: %v", err)
		webFail(w, http.StatusBadGateway, "The speech worker is not available. Try again later.")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Error  string   `json:"error"`
		Voices []string `json:"voices"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusConflict:
			webFail(w, http.StatusConflict, "That name is already in use, so type another name, or use Replace on that voice.")
		case http.StatusNotFound:
			webFail(w, http.StatusNotFound, "There is no voice with that name.")
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
			webFail(w, http.StatusBadRequest, cmpOr(out.Error, "The speech worker cannot use this file."))
		default:
			log.Printf("web: voice: worker: %s %s", resp.Status, out.Error)
			webFail(w, http.StatusBadGateway, "The speech worker could not save the change. An operator must check that the voices folder is writable.")
		}
		return
	}
	if out.Voices == nil {
		out.Voices = []string{}
	}
	b.tts.mu.Lock()
	b.tts.voices, b.tts.voicesAt = out.Voices, time.Now()
	b.tts.mu.Unlock()
	b.webAudit(s, what)
	webJSON(w, http.StatusOK, map[string]any{"voices": out.Voices})
}

// ---- Markov and Persona. ----

func (b *Bot) webChainOff(chain string) string {
	switch chain {
	case "markov":
		if b.chain == nil || !b.env.MessageContent {
			return "The Markov chat is off on this bot. An operator must turn on the message content features."
		}
	case "persona":
		if b.persona == nil || !b.env.MessageContent {
			return "The persona chat is off on this bot. An operator must turn on the message content features."
		}
		if b.personaUser() == "" {
			return "The persona chat is off. An Administrator can set PERSONA_ENABLED and PERSONA_USER in Settings."
		}
	default:
		return "There is no chain with that name."
	}
	return ""
}

func (b *Bot) webChainChannels(s *webSession, chain string) ([]webNamed, error) {
	key := "MARKOV_REPLY_CHANNELS"
	if chain == "persona" {
		key = "PERSONA_REPLY_CHANNELS"
	}
	return b.webApproved(s, b.cfg.List(key), false)
}

type webCandOut struct {
	ID    string   `json:"id"`
	Text  string   `json:"text"`
	Score *float64 `json:"score,omitempty"`
}

func (b *Bot) webChatGenerate(w http.ResponseWriter, r *http.Request, s *webSession) {
	chain := r.PathValue("chain")
	if why := b.webChainOff(chain); why != "" {
		webFail(w, http.StatusServiceUnavailable, why)
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if !webReadJSON(w, r, &req) {
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		webFail(w, http.StatusBadRequest, "Type a message.")
		return
	}
	if utf8.RuneCountInString(text) > webMaxChatInput {
		webFail(w, http.StatusBadRequest, fmt.Sprintf("The message can have at most %d characters.", webMaxChatInput))
		return
	}
	channels, err := b.webChainChannels(s, chain)
	if err != nil {
		log.Printf("web: chat channels: %v", err)
		webFail(w, http.StatusBadGateway, "I could not read your channels from Discord. Try again.")
		return
	}
	words := markov.Tokenize(text)
	n := min(max(b.cfg.Int("MARKOV_CANDIDATES"), 2), markovMaxCands)
	ok := func(w []string) bool { return !b.markovBlocks(w) }
	var store *markov.Store
	var seed []string
	var question, system string
	if chain == "persona" {
		b.flushPersona()
		store, seed = b.persona, b.personaSeed(words)
		question, system = personaQuestion(words, b.personaName()), b.personaRankPrompt()
	} else {
		store, seed = b.chain, markovSeed(words)
		question, system = strings.Join(words, " "), b.cfg.Str("MARKOV_RANK_PROMPT")
	}
	cands, err := store.GenerateN(seed, ok, n)
	if len(cands) == 0 {
		if err != nil {
			log.Printf("web: generate: %v", err)
		}
		webFail(w, http.StatusUnprocessableEntity, "The chain made no reply. It can be empty, or it knows none of these words.")
		return
	}
	out := make([]webCandOut, 0, len(cands))
	note := ""
	var ranked []rankedReply
	if b.env.TTSURL == "" {
		note = "There are no scores, because the worker is not set."
	} else if ranked, err = b.markovRank(question, cands, system); err != nil {
		log.Printf("web: rank: %v", err)
		note = "There are no scores, because the worker did not answer."
		ranked = nil
	}
	add := func(text string, score *float64) {
		id := webRandom()
		b.web.mu.Lock()
		b.web.cands[id] = webCand{chain: chain, text: text, userID: s.userID, at: time.Now()}
		b.web.mu.Unlock()
		out = append(out, webCandOut{ID: id, Text: text, Score: score})
	}
	b.webExpireCands()
	if ranked != nil {
		for _, c := range ranked {
			score := c.score
			add(c.text, &score)
		}
	} else {
		for _, c := range cands {
			add(c, nil)
		}
	}
	body := map[string]any{"candidates": out, "channels": channels, "note": note}
	if chain == "persona" {
		body["question"] = question
		body["name"] = b.personaName()
	}
	webJSON(w, http.StatusOK, body)
}

func (b *Bot) webExpireCands() {
	b.web.mu.Lock()
	defer b.web.mu.Unlock()
	for id, c := range b.web.cands {
		if time.Since(c.at) >= webCandLife {
			delete(b.web.cands, id)
		}
	}
}

// webChatSend posts one generated candidate, once.
func (b *Bot) webChatSend(w http.ResponseWriter, r *http.Request, s *webSession) {
	chain := r.PathValue("chain")
	if why := b.webChainOff(chain); why != "" {
		webFail(w, http.StatusServiceUnavailable, why)
		return
	}
	var req struct {
		ID      string `json:"id"`
		Channel string `json:"channel"`
	}
	if !webReadJSON(w, r, &req) {
		return
	}
	channels, err := b.webChainChannels(s, chain)
	if err != nil {
		log.Printf("web: chat channels: %v", err)
		webFail(w, http.StatusBadGateway, "I could not read your channels from Discord. Try again.")
		return
	}
	if !webHasChannel(channels, req.Channel) {
		webFail(w, http.StatusForbidden, "You cannot post in that channel. Only the reply channels in Settings are approved.")
		return
	}
	b.web.mu.Lock()
	c, ok := b.web.cands[req.ID]
	if ok && c.chain == chain && c.userID == s.userID && time.Since(c.at) < webCandLife {
		delete(b.web.cands, req.ID) // one post for each candidate
	} else {
		ok = false
	}
	b.web.mu.Unlock()
	if !ok {
		webFail(w, http.StatusGone, "This candidate expired or was already sent. Generate again.")
		return
	}
	var msg *discordgo.MessageSend
	if chain == "persona" {
		msg = b.personaPost(c.text)
	} else {
		msg = &discordgo.MessageSend{Content: truncate(c.text, 2000), AllowedMentions: markovMentions}
	}
	posted, err := b.s.ChannelMessageSendComplex(req.Channel, msg)
	if err != nil {
		// It can be sent again: nothing was posted.
		b.web.mu.Lock()
		b.web.cands[req.ID] = c
		b.web.mu.Unlock()
		log.Printf("web: chat send: %v", err)
		webFail(w, http.StatusBadGateway, "I could not post in that channel. Try again.")
		return
	}
	if chain == "persona" {
		b.m.personaReply.WithLabelValues("web").Inc()
	} else {
		b.m.markovReply.WithLabelValues("web").Inc()
	}
	b.webAudit(s, fmt.Sprintf("sent a %s reply to <#%s>: %s", chain, req.Channel, cutText(c.text, 300)))
	webJSON(w, http.StatusOK, map[string]string{"link": b.webMessageLink(req.Channel, posted)})
}
