package bot

import (
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/db"
)

// The bot has no Server Members intent, so its member cache knows few
// members. For the names in comics and spoken text, it keeps the server
// nickname of each member that it sees in a message, an interaction, or a
// member fetch. This is a separate cache: the member cache must not say
// that a member is on the server after they left. "" is no nickname, or
// not a member.
//
// The cache is saved in member_nicks every nickSaveEvery (only the changed
// names) and when the bot stops, and read when the bot starts. A name
// expires nickExpire after the bot last saw the member (see PRIVACY.md).
const (
	nickCacheMax  = 5000
	nickSaveEvery = 10 * time.Minute
	nickExpire    = 30 * 24 * time.Hour
	nickReseen    = 24 * time.Hour // a new seen time is saved at most this often
)

type nickEntry struct {
	nick string
	seen time.Time
}

type nickCache struct {
	*lru[nickEntry]
	dirtyMu sync.Mutex
	dirty   map[string]nickEntry // changed since the last save
}

func newNickCache() *nickCache {
	return &nickCache{lru: newLRU[nickEntry](), dirty: map[string]nickEntry{}}
}

func nickKey(guildID, userID string) string { return guildID + "/" + userID }

// seeNick keeps the nickname of a member that the bot saw.
func (b *Bot) seeNick(guildID, userID, nick string) {
	if guildID == "" || userID == "" {
		return
	}
	key, now := nickKey(guildID, userID), time.Now()
	if old, ok := b.nicks.get(key); ok && old.nick == nick && now.Sub(old.seen) < nickReseen {
		return // no change to save
	}
	e := nickEntry{nick: nick, seen: now}
	b.nicks.put(key, e, nickCacheMax)
	b.nicks.dirtyMu.Lock()
	b.nicks.dirty[key] = e
	b.nicks.dirtyMu.Unlock()
}

// cachedNick returns a member's nickname when the bot knows it.
func (b *Bot) cachedNick(guildID, userID string) (string, bool) {
	if e, ok := b.nicks.get(nickKey(guildID, userID)); ok && time.Since(e.seen) < nickExpire {
		return e.nick, true
	}
	if m, err := b.s.State.Member(guildID, userID); err == nil {
		return m.Nick, true
	}
	return "", false
}

// memberNick returns a member's nickname, and fetches the member only when
// the bot does not know it. A user who left is kept as "", so that the bot
// does not fetch them again.
func (b *Bot) memberNick(guildID, userID string) string {
	if nick, ok := b.cachedNick(guildID, userID); ok {
		return nick
	}
	m, err := b.s.GuildMember(guildID, userID)
	var rest *discordgo.RESTError
	switch {
	case err == nil:
		b.seeNick(guildID, userID, m.Nick)
		return m.Nick
	case errors.As(err, &rest) && rest.Message != nil && rest.Message.Code == discordgo.ErrCodeUnknownMember:
		b.seeNick(guildID, userID, "")
	}
	return ""
}

// loadNicks reads the saved nicknames into the cache.
func (b *Bot) loadNicks() {
	saved, err := b.db.MemberNicks(time.Now().Add(-nickExpire), nickCacheMax)
	if err != nil {
		log.Printf("nicknames: load: %v", err)
		return
	}
	for i := len(saved) - 1; i >= 0; i-- { // oldest first: the newest stay longest
		n := saved[i]
		b.nicks.put(nickKey(n.GuildID, n.UserID), nickEntry{nick: n.Nick, seen: n.Seen}, nickCacheMax)
	}
}

// saveNicks writes the changed nicknames and deletes the expired ones.
func (b *Bot) saveNicks() {
	b.nicks.dirtyMu.Lock()
	dirty := b.nicks.dirty
	b.nicks.dirty = map[string]nickEntry{}
	b.nicks.dirtyMu.Unlock()
	rows := make([]db.MemberNick, 0, len(dirty))
	for key, e := range dirty {
		guildID, userID, _ := strings.Cut(key, "/")
		rows = append(rows, db.MemberNick{GuildID: guildID, UserID: userID, Nick: e.nick, Seen: e.seen})
	}
	if err := b.db.SaveMemberNicks(rows, time.Now().Add(-nickExpire)); err != nil {
		log.Printf("nicknames: save: %v", err)
		b.nicks.dirtyMu.Lock()
		for key, e := range dirty { // try again next time, unless newer
			if _, newer := b.nicks.dirty[key]; !newer {
				b.nicks.dirty[key] = e
			}
		}
		b.nicks.dirtyMu.Unlock()
	}
}
