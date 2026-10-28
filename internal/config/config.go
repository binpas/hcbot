// Package config holds the bot's .env settings and the /setup-editable
// runtime configuration (database → .env → built-in default).
package config

import (
	"errors"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/joho/godotenv"
)

// Env holds the process-level settings that only come from .env.
type Env struct {
	BotToken string
	GuildID  string

	// While true, only /setup and /permsreport respond; every other command
	// and automatic behavior is disabled.
	SetupMode bool

	// Gates every feature that needs the Message Content privileged intent.
	// Restart required to change, since intents are set once at connect.
	MessageContent bool

	MediaCacheSize     int
	MediaCacheMaxBytes int

	// MarkovDBPath is the separate database file for the Markov chat.
	MarkovDBPath string

	// GiphyKey is the GIPHY API key for Markov GIFs. Empty turns them off.
	GiphyKey string

	// YtdlpCookies is a cookies file for yt-dlp (/download). Empty: none.
	YtdlpCookies string

	// TTSURL is the text to speech worker for /say. Empty turns /say off.
	TTSURL   string
	TTSToken string

	// ComicArtDir is the art pack for /comic (deploy/comic-art.sh). When
	// it has no art, the comic commands are not registered.
	ComicArtDir string
}

// LoadEnv reads .env (if present) into the process environment and returns
// the parsed settings.
func LoadEnv() Env {
	_ = godotenv.Load()
	return Env{
		BotToken:           os.Getenv("BOT_TOKEN"),
		GuildID:            strings.TrimSpace(os.Getenv("GUILD_ID")),
		SetupMode:          envBool("SETUP_MODE"),
		MessageContent:     envBool("ENABLE_MESSAGE_CONTENT_FEATURES"),
		MediaCacheSize:     envInt("MEDIA_CACHE_SIZE", 300),
		MediaCacheMaxBytes: envInt("MEDIA_CACHE_MAX_BYTES", 8*1024*1024),
		MarkovDBPath:       envStr("MARKOV_DB_PATH", "markov.db"),
		GiphyKey:           strings.TrimSpace(os.Getenv("GIPHY_API_KEY")),
		YtdlpCookies:       strings.TrimSpace(os.Getenv("YTDLP_COOKIES")),
		TTSURL:             strings.TrimRight(strings.TrimSpace(os.Getenv("TTS_URL")), "/"),
		TTSToken:           strings.TrimSpace(os.Getenv("TTS_TOKEN")),
		ComicArtDir:        envStr("COMIC_ART_DIR", "comic"),
	}
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return n
	}
	return def
}

// Setting kinds, which decide how /setup edits a value.
const (
	KindChannel  = "channel"
	KindCategory = "category"
	KindRole     = "role"
	KindRoleList = "role_list"
	KindChanList = "channel_list"
	KindBool     = "bool"   // "on" or "off"
	KindChoice   = "choice" // one value from Choices
	KindEmoji    = "emoji"
	KindInt      = "int"
	KindDecimal  = "decimal" // a number that can have a decimal part, e.g. 0.05
	KindText     = "text"
)

// SetupKey describes one /setup-editable setting.
type SetupKey struct {
	Key         string
	EnvKey      string // .env fallback
	Kind        string
	Default     string
	Description string
}

// SetupKeys lists every /setup-editable setting. Add new settings here so
// they show up in /setup on their own.
var SetupKeys = []SetupKey{
	{"MOD_LOG", "MOD_LOG_CHANNEL_ID", KindChannel, "",
		"Channel ID for the channel to log mod operations"},
	{"BIG_BROTHER", "BIG_BROTHER_CHANNEL_ID", KindChannel, "",
		"Channel ID for the channel to log message edits/deletions"},
	{"SENSITIVE_LOG", "SENSITIVE_LOG_CHANNEL_ID", KindChannel, "",
		"Channel ID for sensitive command output (userinfo). Falls back to BIG_BROTHER if unset"},
	{"CURATED", "CURATED_CHANNEL_ID", KindChannel, "",
		"Channel ID for the channel to post curated messages to"},
	{"TICKET_CAT", "TICKET_CAT_ID", KindCategory, "",
		"Category ID for the category to create user tickets in"},
	{"MOD_SUPPORT", "MOD_SUPPORT_CHANNEL_ID", KindChannel, "",
		"Channel ID for the channel to use for the mod support embed"},
	{"MOD_ROLE", "MOD_ROLE_ID", KindRole, "",
		"Role ID for the moderator role"},
	{"MOD_DM_PUNISHMENT", "MOD_DM_PUNISHMENT", KindBool, "on",
		"DM a member the reason and length when they are banned, kicked, muted, warned or jailed"},
	{"ON_THE_REAL", "ON_THE_REAL_CHANNEL_ID", KindChannel, "",
		"Channel ID for the channel where autoreacts are disabled"},
	{"MOTD_ROLE", "MOTD_ROLE_ID", KindRole, "",
		"Role ID for the Member Of The Day role"},
	{"CURATED_EMOTE", "CURATED_EMOTE", KindEmoji, "⭐",
		"Emote that counts toward curating a message to the curated channel"},
	{"CURATED_THRESHOLD", "CURATED_THRESHOLD", KindInt, "7",
		"Number of reactions needed before a message is curated"},
	{"JAIL_ROLE", "JAIL_ROLE_ID", KindRole, "",
		"Role ID for the jail role (restricted to the jail channel)"},
	{"JAIL_CHANNEL", "JAIL_CHANNEL_ID", KindChannel, "",
		"Channel ID for the only channel jailed members may speak in"},
	{"JAIL_PROTECTED_ROLES", "JAIL_PROTECTED_ROLE_IDS", KindRoleList, "",
		"Roles never stripped when jailing someone (e.g. Patreon/Nitro booster tiers) — pick up to 25"},
	{"WARN_ARREST_THRESHOLD", "WARN_ARREST_THRESHOLD", KindInt, "3",
		"Number of warnings before a member is automatically arrested"},
	{"WARN_ARREST_DURATION", "WARN_ARREST_DURATION", KindInt, "60",
		"Length of the automatic arrest, in minutes"},
	{"WARN_KICK_THRESHOLD", "WARN_KICK_THRESHOLD", KindInt, "5",
		"Number of warnings before a member is automatically kicked"},
	{"TRIGGER_IMPORT_MAX_MB", "TRIGGER_IMPORT_MAX_MB", KindInt, "5",
		"Largest file /trigger batchimport accepts, in MB"},
	{"MEDIA_CACHE_TOTAL_MB", "MEDIA_CACHE_TOTAL_MB", KindInt, "200",
		"Memory limit for all cached attachments (for deleted-message logs), in MB"},
	{"ASSETS_IMPORT_MAX_MB", "ASSETS_IMPORT_MAX_MB", KindInt, "25",
		"Largest zip file /assets import accepts, in MB"},
	{"README_PATH", "README_PATH", KindText, "./README.md",
		"File that /readme shows (relative to the bot's working folder)"},
	{"PRIVACY_URL", "PRIVACY_URL", KindText, "https://github.com/binpas/hcbot/blob/main/PRIVACY.md",
		"Privacy Policy link at the bottom of /help (set to none to hide it)"},
	{"TERMS_URL", "TERMS_URL", KindText, "https://github.com/binpas/hcbot/blob/main/TERMS.md",
		"Terms of Service link at the bottom of /help (set to none to hide it)"},
	{"MARKOV_LEARN_CHANNELS", "MARKOV_LEARN_CHANNEL_IDS", KindChanList, "",
		"Channels the Markov chat learns from — pick up to 25"},
	{"MARKOV_REPLY_CHANNELS", "MARKOV_REPLY_CHANNEL_IDS", KindChanList, "",
		"Channels where the Markov chat replies to @mentions — pick up to 25"},
	{"MARKOV_REPLY_CHANCE", "MARKOV_REPLY_CHANCE", KindDecimal, "0",
		"Percent chance (0-100, decimals allowed, e.g. 0.05) that the Markov chat talks after a normal message in a reply channel"},
	{"MARKOV_RANDOM_AS_REPLY", "MARKOV_RANDOM_AS_REPLY", KindInt, "50",
		"Percent (0-100) of those random messages sent as a reply to the message; the rest are posted in the channel"},
	{"MARKOV_COOLDOWN", "MARKOV_COOLDOWN", KindInt, "60",
		"Seconds after a Markov message before the chat talks again in that channel"},
	{"MARKOV_MENTION_REPLY", "MARKOV_MENTION_REPLY", KindChoice, "always",
		"Who gets a reply to an @mention: always (even during the cooldown), cooldown (only when the cooldown is over), or mods (only mods). Mods always get a reply"},
	{"MARKOV_CANDIDATES", "MARKOV_CANDIDATES", KindInt, "8",
		"How many replies the Markov chat makes before it picks one (1-20). The GPU worker (TTS_URL) picks the one that answers best, or sounds most natural. 1: no picking"},
	{"MARKOV_PICK", "MARKOV_PICK", KindChoice, "top3",
		"Which candidate the Markov chat posts: best (the highest score) or top3 (a random one of the best three, for more variety)"},
	{"MARKOV_GIF_CHANCE", "MARKOV_GIF_CHANCE", KindDecimal, "0",
		"Percent chance (0-100, decimals allowed) that a Markov message also gets a GIF from GIPHY, found with the message text. Needs GIPHY_API_KEY in .env"},
	{"MARKOV_GIF_COOLDOWN", "MARKOV_GIF_COOLDOWN", KindInt, "40",
		"Seconds between GIPHY requests, for the full bot (the API key has an hourly limit)"},
	{"MARKOV_GIF_RATING", "MARKOV_GIF_RATING", KindChoice, "pg-13",
		"Highest GIPHY rating for Markov GIFs: g, pg, pg-13 or r"},
	{"MARKOV_IMPORT_MAX_MB", "MARKOV_IMPORT_MAX_MB", KindInt, "25",
		"Largest file /markov import accepts, in MB"},
	{"MARKOV_LOG_CHANNEL", "MARKOV_LOG_CHANNEL_ID", KindChannel, "",
		"Mod-only channel where the Markov chat logs each message it reads in a learn channel, and if it learned it. Empty: no log"},
	{"MARKOV_LOG_MODE", "MARKOV_LOG_MODE", KindChoice, "all",
		"Which messages go to MARKOV_LOG_CHANNEL: all, skipped (only the messages it did not learn), or off"},
	{"DOWNLOAD_ENABLED", "DOWNLOAD_ENABLED", KindBool, "on",
		"Let mods download videos with /download (needs yt-dlp and ffmpeg on the server)"},
	{"DOWNLOAD_MAX_MB", "DOWNLOAD_MAX_MB", KindInt, "100",
		"Largest video /download sends, in MB. The server's boost level can make the limit smaller (10 MB, 50 MB at level 2, 100 MB at level 3)"},
	{"TTS_ENABLED", "TTS_ENABLED", KindBool, "on",
		"Let mods make speech clips with /say (needs TTS_URL in .env)"},
	{"TTS_CHANNELS", "TTS_CHANNEL_IDS", KindChanList, "",
		"Channels where /say works — pick up to 25. Empty: all channels"},
	{"TTS_MAX_CHARS", "TTS_MAX_CHARS", KindInt, "300",
		"Longest text /say accepts, in characters (at most 600)"},
	{"TTS_COOLDOWN", "TTS_COOLDOWN", KindInt, "30",
		"Seconds a member must wait between /say requests (0: no wait)"},
	{"TTS_QUEUE_MAX", "TTS_QUEUE_MAX", KindInt, "5",
		"Most /say clips that can wait in the queue (each clip takes about 15–30 seconds)"},
	{"COMIC_ENABLED", "COMIC_ENABLED", KindBool, "on",
		"Let members make comics of the chat with /comic and \"Start comic here\" (needs the Message Content option and the comic art)"},
	{"COMIC_CHANNELS", "COMIC_CHANNEL_IDS", KindChanList, "",
		"Channels where comics work — pick up to 25. Empty: all channels"},
	{"COMIC_MAX_MESSAGES", "COMIC_MAX_MESSAGES", KindInt, "10",
		"Most messages in one comic (2-20)"},
	{"COMIC_COOLDOWN", "COMIC_COOLDOWN", KindInt, "30",
		"Seconds a member must wait between comics (0: no wait)"},
}

// Choice is one value of a KindChoice setting.
type Choice struct {
	Value, Label string
}

// Choices lists the values of each KindChoice setting, in menu order.
var Choices = map[string][]Choice{
	"MARKOV_MENTION_REPLY": {
		{"always", "Always reply, even during the cooldown"},
		{"cooldown", "Reply only when the cooldown is over"},
		{"mods", "Reply only to mods"},
	},
	"MARKOV_LOG_MODE": {
		{"all", "All messages it reads"},
		{"skipped", "Only the messages it did not learn"},
		{"off", "No log"},
	},
	"MARKOV_PICK": {
		{"best", "The candidate with the best score"},
		{"top3", "A random one of the best three"},
	},
	"MARKOV_GIF_RATING": {
		{"g", "G: all ages"},
		{"pg", "PG"},
		{"pg-13", "PG-13"},
		{"r", "R: adults"},
	},
}

// ChoiceLabel returns the label of a KindChoice value, or "".
func ChoiceLabel(key, value string) string {
	for _, c := range Choices[key] {
		if c.Value == value {
			return c.Label
		}
	}
	return ""
}

// legacyMentionKey is the on/off setting that MARKOV_MENTION_REPLY replaced:
// on meant always, off meant cooldown.
const legacyMentionKey = "MARKOV_ALWAYS_REPLY_MENTION"

func legacyMentionMode(raw string) string {
	if on, ok := ParseBool(raw); ok && !on {
		return "cooldown"
	}
	return "always"
}

// Migrate changes old settings in values to their new keys. It returns the
// keys to set and the keys to remove; values itself does not change. A new
// key that is already set wins over the old one.
func Migrate(values map[string]string) (set map[string]string, remove []string) {
	set = map[string]string{}
	if old, ok := values[legacyMentionKey]; ok {
		if values["MARKOV_MENTION_REPLY"] == "" && strings.TrimSpace(old) != "" {
			set["MARKOV_MENTION_REPLY"] = legacyMentionMode(old)
		}
		remove = append(remove, legacyMentionKey)
	}
	return set, remove
}

// Extra keys the bot writes itself (not prompted for during /setup).
const (
	ModSupportMsgKey = "MOD_SUPPORT_MESSAGE"
	JailSetupKey     = "JAIL_OVERWRITES_APPLIED"
)

var byKey = func() map[string]SetupKey {
	m := make(map[string]SetupKey, len(SetupKeys))
	for _, k := range SetupKeys {
		m[k.Key] = k
	}
	return m
}()

// Known reports whether the bot uses key: a /setup setting or an extra key
// it writes itself.
func Known(key string) bool {
	_, ok := byKey[key]
	return ok || key == ModSupportMsgKey || key == JailSetupKey
}

// CheckValue reports why value is not valid for key, or returns nil. An
// empty value is always valid. Unknown keys accept any value.
func CheckValue(key, value string) error {
	if value == "" {
		return nil
	}
	isID := func(v string) bool {
		n, err := strconv.ParseUint(v, 10, 64)
		return err == nil && n != 0
	}
	kind := byKey[key].Kind
	if key == ModSupportMsgKey {
		kind = KindChannel // any snowflake ID
	}
	switch kind {
	case KindInt:
		if _, err := strconv.Atoi(value); err != nil {
			return errors.New("must be a whole number")
		}
	case KindDecimal:
		if _, err := ParseDecimal(value); err != nil {
			return err
		}
	case KindChannel, KindCategory, KindRole:
		if !isID(value) {
			return errors.New("must be a Discord ID")
		}
	case KindBool:
		if _, ok := ParseBool(value); !ok {
			return errors.New("must be on or off")
		}
	case KindChoice:
		if ChoiceLabel(key, strings.ToLower(strings.TrimSpace(value))) == "" {
			var names []string
			for _, c := range Choices[key] {
				names = append(names, c.Value)
			}
			return errors.New("must be one of: " + strings.Join(names, ", "))
		}
	case KindRoleList, KindChanList:
		for _, tok := range listSep.Split(strings.TrimSpace(value), -1) {
			if !isID(tok) {
				return errors.New("must be a list of Discord IDs")
			}
		}
	}
	return nil
}

// Store is the in-memory cache of the config table.
type Store struct {
	mu     sync.RWMutex
	values map[string]string
}

// NewStore returns a Store seeded with the given database values.
func NewStore(values map[string]string) *Store {
	if values == nil {
		values = map[string]string{}
	}
	return &Store{values: values}
}

// Get returns the database value for key, without fallbacks.
func (s *Store) Get(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.values[key]
}

// Set updates the cached database value for key. The caller persists it.
func (s *Store) Set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
}

// Replace swaps in a new set of database values.
func (s *Store) Replace(values map[string]string) {
	if values == nil {
		values = map[string]string{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = values
}

// Raw resolves a config value: database → .env → built-in default.
func (s *Store) Raw(key string) string {
	if v := s.Get(key); v != "" {
		return v
	}
	sk, ok := byKey[key]
	if !ok {
		return ""
	}
	if v := os.Getenv(sk.EnvKey); v != "" {
		return v
	}
	return sk.Default
}

// ID returns a configured snowflake ID, or "" when unset or not a number.
func (s *Store) ID(key string) string {
	raw := strings.TrimSpace(s.Raw(key))
	if _, err := strconv.ParseUint(raw, 10, 64); err != nil || raw == "0" {
		return ""
	}
	return raw
}

// Int returns a configured integer setting, falling back to its default.
func (s *Store) Int(key string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s.Raw(key))); err == nil {
		return n
	}
	n, _ := strconv.Atoi(byKey[key].Default)
	return n
}

// ParseDecimal reads a KindDecimal value. It accepts "0.05" and "0,05".
func ParseDecimal(v string) (float64, error) {
	f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(v), ",", "."), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("must be a number, for example 0.05")
	}
	return f, nil
}

// Float returns a configured decimal setting, falling back to its default.
func (s *Store) Float(key string) float64 {
	if f, err := ParseDecimal(s.Raw(key)); err == nil {
		return f
	}
	f, _ := ParseDecimal(byKey[key].Default)
	return f
}

// Str returns a configured string setting (e.g. an emote).
func (s *Store) Str(key string) string {
	return s.Raw(key)
}

// Bool returns a configured on/off setting, falling back to its default.
func (s *Store) Bool(key string) bool {
	if v, ok := ParseBool(s.Raw(key)); ok {
		return v
	}
	v, _ := ParseBool(byKey[key].Default)
	return v
}

// Choice returns a configured KindChoice value in lower case, falling back
// to its default. MARKOV_MENTION_REPLY also reads the old
// MARKOV_ALWAYS_REPLY_MENTION from .env when nothing newer is set.
func (s *Store) Choice(key string) string {
	if key == "MARKOV_MENTION_REPLY" && s.Source(key) == "default" {
		if old := strings.TrimSpace(os.Getenv(legacyMentionKey)); old != "" {
			return legacyMentionMode(old)
		}
	}
	if v := strings.ToLower(strings.TrimSpace(s.Raw(key))); ChoiceLabel(key, v) != "" {
		return v
	}
	return byKey[key].Default
}

// ParseBool reads an on/off value. It also accepts true/false, yes/no and
// 1/0. ok is false for anything else.
func ParseBool(raw string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "true", "yes", "1":
		return true, true
	case "off", "false", "no", "0":
		return false, true
	}
	return false, false
}

// List returns a configured ID list setting (roles or channels).
func (s *Store) List(key string) []string {
	return ParseIDList(s.Raw(key))
}

// Source reports where a setting's current value comes from.
func (s *Store) Source(key string) string {
	if s.Get(key) != "" {
		return "database"
	}
	if sk, ok := byKey[key]; ok && os.Getenv(sk.EnvKey) != "" {
		return ".env"
	}
	return "default"
}

var listSep = regexp.MustCompile(`[,\s]+`)

// ParseIDList parses a comma/whitespace-separated string of IDs, ignoring
// anything that isn't a valid snowflake.
func ParseIDList(raw string) []string {
	var ids []string
	for _, tok := range listSep.Split(strings.TrimSpace(raw), -1) {
		if _, err := strconv.ParseUint(tok, 10, 64); err == nil {
			ids = append(ids, tok)
		}
	}
	return ids
}
