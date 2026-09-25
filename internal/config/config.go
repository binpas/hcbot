// Package config holds the bot's .env settings and the /setup-editable
// runtime configuration (database → .env → built-in default).
package config

import (
	"errors"
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
	}
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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
	KindEmoji    = "emoji"
	KindInt      = "int"
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
	case KindChannel, KindCategory, KindRole:
		if !isID(value) {
			return errors.New("must be a Discord ID")
		}
	case KindRoleList:
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

// Str returns a configured string setting (e.g. an emote).
func (s *Store) Str(key string) string {
	return s.Raw(key)
}

// List returns a configured role-ID list setting.
func (s *Store) List(key string) []string {
	return ParseRoleList(s.Raw(key))
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

// ParseRoleList parses a comma/whitespace-separated string of role IDs,
// ignoring anything that isn't a valid snowflake.
func ParseRoleList(raw string) []string {
	var ids []string
	for _, tok := range listSep.Split(strings.TrimSpace(raw), -1) {
		if _, err := strconv.ParseUint(tok, 10, 64); err == nil {
			ids = append(ids, tok)
		}
	}
	return ids
}
