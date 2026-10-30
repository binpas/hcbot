// Package bot is the HEALTH Discord bot: session setup, the slash-command
// router, and every feature's handlers.
package bot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/config"
	"github.com/binpas/hcbot/internal/db"
	"github.com/binpas/hcbot/internal/markov"
	"github.com/binpas/hcbot/internal/tweetshot"
)

// Bot holds the shared state every handler needs.
type Bot struct {
	s     *discordgo.Session
	db    *db.DB
	chain *markov.Store // the Markov chat's own database
	env   config.Env
	cfg   *config.Store
	mk    markovState
	gif   gifState

	commands   map[string]*command // keyed by qualified name, e.g. "jail arrest"
	groups     map[string]*group
	order      []string // top-level names, in registration order
	components map[string]func(*Ctx)
	modals     map[string]func(*Ctx)
	confirms   map[string]func(*Ctx, []string)

	msgs  *lru[cachedMessage] // audit log message cache
	nicks *nickCache          // member nicknames (see nicks.go)
	m     *botMetrics         // Prometheus metrics (see metrics.go)
	media *mediaCache
	snap  *snapshots

	backupDir  string        // where /db backup writes
	assetDelay time.Duration // pause between emoji/sticker creates

	giphyURL  string // the GIPHY search endpoint (a fake server in tests)
	giphyHTTP *http.Client

	runTool  toolRunner                                               // a fake in tests
	lookupIP func(ctx context.Context, host string) ([]net.IP, error) // a fake in tests
	dlSlot   chan struct{}                                            // one video download at a time
	ttsHTTP  *http.Client
	tts      ttsState
	comics   *comicState       // nil when comics are off
	tweets   *tweetshot.Client // X post data for /tweetshot (a fake server in tests)

	// fetchImage gets a picture for a comic (a fake in tests).
	fetchImage func(ctx context.Context, url string) ([]byte, error)

	ttsPollEvery time.Duration // ttsPoll (short in tests)

	loopsOnce sync.Once
	tourBusy  sync.Map // tour IDs that /tour post is posting now
	stop      chan struct{}
}

// Version is the git commit of the build, for the log. main sets it.
var Version = "dev"

// New builds the bot. Call Run to connect.
func New(env config.Env, database *db.DB, chain *markov.Store) (*Bot, error) {
	values, err := database.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if set, remove := config.Migrate(values); len(set)+len(remove) > 0 {
		if err := database.UpdateConfig(set, remove); err != nil {
			return nil, fmt.Errorf("migrate config: %w", err)
		}
		for _, k := range remove {
			delete(values, k)
		}
		maps.Copy(values, set)
		log.Printf("Updated old settings: set %v, removed %v", set, remove)
	}
	s, err := discordgo.New("Bot " + env.BotToken)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	// Server Members (privileged) is intentionally never requested: member
	// lookups go through resolveMember's on-demand fetch instead. Message
	// Content (privileged) is requested only when explicitly opted into.
	s.Identify.Intents = discordgo.IntentsAllWithoutPrivileged
	if env.MessageContent {
		s.Identify.Intents |= discordgo.IntentMessageContent
	}

	b := &Bot{
		s:            s,
		db:           database,
		chain:        chain,
		env:          env,
		cfg:          config.NewStore(values),
		commands:     map[string]*command{},
		groups:       map[string]*group{},
		components:   map[string]func(*Ctx){},
		modals:       map[string]func(*Ctx){},
		confirms:     map[string]func(*Ctx, []string){},
		stop:         make(chan struct{}),
		ttsPollEvery: ttsPoll,
		giphyURL:     giphySearchURL,
		giphyHTTP:    &http.Client{Timeout: giphyTimeout},
		runTool:      execTool,
		lookupIP:     lookupIP,
		dlSlot:       make(chan struct{}, 1),
		ttsHTTP:      &http.Client{Timeout: ttsRequestTimeout},
		fetchImage:   httpFetch(&http.Client{Timeout: 5 * time.Second}),
		tweets:       tweetshot.NewClient(),
		backupDir:    "backups",
		assetDelay:   1200 * time.Millisecond,
		msgs:         newLRU[cachedMessage](),
		nicks:        newNickCache(),
		media:        newMediaCache(),
		snap:         &snapshots{roles: map[string]discordgo.Role{}, threads: map[string]discordgo.Channel{}},
	}
	b.m = newBotMetrics(b)
	b.s.Client.Transport = metricsTransport{next: cmp.Or[http.RoundTripper](b.s.Client.Transport, http.DefaultTransport), m: b.m}
	b.loadNicks()
	b.registerSetup()
	b.registerConfirm()
	b.registerModeration()
	b.registerNotes()
	b.registerJail()
	b.registerTickets()
	b.registerTriggers()
	b.registerReactionRoles()
	b.registerAdmin()
	b.registerAssets()
	b.registerBotProfile()
	b.registerMarkov()
	b.registerDownload()
	b.registerTweetshot()
	b.registerTTS()
	b.registerComics()
	b.registerReminders()
	b.registerTours()
	b.registerHelp()

	s.AddHandler(b.onReady)
	s.AddHandler(b.onConnect)
	s.AddHandler(b.onDisconnect)
	s.AddHandler(b.onResumed)
	s.AddHandler(b.onInteraction)
	s.AddHandler(b.onRawInteraction)
	s.AddHandler(b.onReactionAdd)
	s.AddHandler(b.onReactionRemove)
	s.AddHandler(b.onMessageCreate)
	b.registerAudit()
	return b, nil
}

// Open connects to Discord and syncs slash commands.
func (b *Bot) Open() error {
	if b.env.MetricsAddr != "" {
		if _, err := b.serveMetrics(b.env.MetricsAddr); err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
	}
	if err := b.s.Open(); err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	if err := b.syncCommands(); err != nil {
		return fmt.Errorf("sync commands: %w", err)
	}
	log.Print("Slash commands synced.")
	return nil
}

// Close stops the background loops and disconnects from Discord.
func (b *Bot) Close() error {
	close(b.stop)
	b.flushMarkov()
	b.saveNicks()
	return b.s.Close()
}

// every runs fn now and then at each interval until the bot closes.
func (b *Bot) every(interval time.Duration, fn func()) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			fn()
			select {
			case <-t.C:
			case <-b.stop:
				return
			}
		}
	}()
}

// startLoops starts the background loops once, on the first Ready.
func (b *Bot) startLoops() {
	b.loopsOnce.Do(func() {
		b.every(30*time.Second, b.runScheduledActions)
		b.every(30*time.Second, b.runJailReleases)
		b.every(30*time.Second, b.runReminders)
		b.daily(4, 0, b.runDailyBackup)
		b.every(markovFlushEvery, b.flushMarkov)
		b.every(markovStatsEvery, b.updateMarkovStats)
		b.every(nickSaveEvery, b.saveNicks)
		b.resumeTTSJobs()
	})
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("Logged in as %s (ID: %s), version %s", r.User.String(), r.User.ID, Version)
	status := "HEALTH"
	if b.env.SetupMode {
		log.Print("SETUP_MODE is on — only /setup and /permsreport will respond.")
		status = "🚧 setup mode"
	} else {
		b.startLoops()
	}
	if err := s.UpdateGameStatus(0, status); err != nil {
		log.Printf("set presence: %v", err)
	}
}

// setConfig persists a config value and updates the in-memory cache.
func (b *Bot) setConfig(key, value string) error {
	if err := b.db.SetConfig(key, value); err != nil {
		return err
	}
	b.cfg.Set(key, value)
	return nil
}

// channel looks up a channel: state cache first, then the API.
func (b *Bot) channel(id string) *discordgo.Channel {
	if id == "" {
		return nil
	}
	if ch, err := b.s.State.Channel(id); err == nil {
		return ch
	}
	ch, err := b.s.Channel(id)
	if err != nil {
		return nil
	}
	return ch
}

// resolveMember looks up a member by ID: state cache first, then an
// on-demand API fetch. Single lookups like this don't need the Server
// Members intent. Use this instead of reading the state directly.
func (b *Bot) resolveMember(guildID, userID string) *discordgo.Member {
	if m, err := b.s.State.Member(guildID, userID); err == nil {
		return m
	}
	m, err := b.s.GuildMember(guildID, userID)
	if err != nil {
		return nil
	}
	return m
}

// isDiscordError reports whether err is a Discord API error with a JSON
// error code (discordgo.ErrCode...).
func isDiscordError(err error, code int) bool {
	var rest *discordgo.RESTError
	return errors.As(err, &rest) && rest.Message != nil && rest.Message.Code == code
}

// freshMember fetches a member from the API, skipping the cache. Use it
// before writing a member's whole role list: without the Server Members
// intent the cache never sees role changes. It returns nil when the member
// has left; on any other API error it falls back to resolveMember.
func (b *Bot) freshMember(guildID, userID string) *discordgo.Member {
	m, err := b.s.GuildMember(guildID, userID)
	if err == nil {
		m.GuildID = guildID
		return m
	}
	if isDiscordError(err, discordgo.ErrCodeUnknownMember) {
		return nil
	}
	log.Printf("fetch member %s: %v", userID, err)
	return b.resolveMember(guildID, userID)
}

// modLog sends an entry to the MOD_LOG channel. A missing or inaccessible
// channel is ignored so it never breaks the caller.
func (b *Bot) modLog(embed *discordgo.MessageEmbed) {
	id := b.cfg.ID("MOD_LOG")
	if id == "" {
		return
	}
	if _, err := b.s.ChannelMessageSendEmbed(id, embed); err != nil {
		log.Printf("mod log: %v", err)
	}
}

// bbLog sends an audit entry to the BIG_BROTHER channel. If the files are
// too large to send, it falls back to the embed alone.
func (b *Bot) bbLog(embed *discordgo.MessageEmbed, files ...*discordgo.File) {
	id := b.cfg.ID("BIG_BROTHER")
	if id == "" {
		return
	}
	_, err := b.s.ChannelMessageSendComplex(id, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		Files:  files,
	})
	if err != nil && len(files) > 0 {
		_, err = b.s.ChannelMessageSendEmbed(id, embed)
	}
	if err != nil {
		log.Printf("big brother log: %v", err)
	}
}
