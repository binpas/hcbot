package bot

import (
	"errors"

	"github.com/bwmarrin/discordgo"
)

// The bot has no Server Members intent, so its member cache knows few
// members. For the names in comics and spoken text, it keeps the server
// nickname of each member that it sees in a message, an interaction, or a
// member fetch. This is a separate cache: the member cache must not say
// that a member is on the server after they left. "" is no nickname, or
// not a member.
const nickCacheMax = 5000

func nickKey(guildID, userID string) string { return guildID + "/" + userID }

// seeNick keeps the nickname of a member that the bot saw.
func (b *Bot) seeNick(guildID, userID, nick string) {
	if guildID != "" && userID != "" {
		b.nicks.put(nickKey(guildID, userID), nick, nickCacheMax)
	}
}

// cachedNick returns a member's nickname when the bot knows it.
func (b *Bot) cachedNick(guildID, userID string) (string, bool) {
	if nick, ok := b.nicks.get(nickKey(guildID, userID)); ok {
		return nick, true
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
