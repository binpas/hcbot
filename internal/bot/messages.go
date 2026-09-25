package bot

import (
	"log"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// triggerMentions lets a trigger response show @everyone, @here and role
// mentions without pinging anyone but users (and the replied-to author).
var triggerMentions = &discordgo.MessageAllowedMentions{
	Parse:       []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers},
	RepliedUser: true,
}

// onMessageCreate handles triggers and autoreacts. Section 7 adds the
// media cache here.
func (b *Bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer recoverHandler("message handler")
	if m.Author == nil || b.env.SetupMode {
		return
	}
	b.cacheMessage(m.Message) // for the audit log; bots are marked, not skipped
	if m.Author.Bot {
		return
	}
	if b.fireTrigger(m.Message) {
		return // a trigger fired: no autoreacts
	}
	b.autoreact(m.Message)
}

// mentionKeyword returns the word after a leading mention of the bot,
// lowercased, or "". Triggers use "@Bot <name>" instead of a prefix because
// Discord delivers the text of a message that mentions the bot even
// without the Message Content intent.
func (b *Bot) mentionKeyword(m *discordgo.Message) string {
	botID := b.s.State.User.ID
	if !slices.ContainsFunc(m.Mentions, func(u *discordgo.User) bool { return u.ID == botID }) {
		return ""
	}
	loc := regexp.MustCompile(`^<@!?` + botID + `>\s*`).FindStringIndex(m.Content)
	if loc == nil {
		return ""
	}
	words := strings.Fields(m.Content[loc[1]:])
	if len(words) == 0 {
		return ""
	}
	return strings.ToLower(words[0])
}

// fireTrigger replies with a random response when the message is
// "@Bot <trigger>". A trigger named like a command never fires.
func (b *Bot) fireTrigger(m *discordgo.Message) bool {
	keyword := b.mentionKeyword(m)
	if keyword == "" || slices.Contains(b.order, keyword) {
		return false
	}
	values, err := b.db.TriggerValues(keyword)
	if err != nil {
		log.Printf("trigger %q: %v", keyword, err)
		return false
	}
	if len(values) == 0 {
		return false
	}
	response := values[rand.IntN(len(values))]
	_, err = b.s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Content: response, Reference: m.Reference(), AllowedMentions: triggerMentions,
	})
	if err != nil {
		// The message may be gone; answer without the reply link.
		if _, err := b.s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content: response, AllowedMentions: triggerMentions,
		}); err != nil {
			log.Printf("trigger %q: send: %v", keyword, err)
		}
	}
	return true
}

// autoreact adds each matching autoreact emote. It needs the Message
// Content intent, so it runs only with ENABLE_MESSAGE_CONTENT_FEATURES,
// and never in ON_THE_REAL.
func (b *Bot) autoreact(m *discordgo.Message) {
	if !b.env.MessageContent || m.GuildID == "" || m.ChannelID == b.cfg.ID("ON_THE_REAL") {
		return
	}
	rows, err := b.db.Autoreacts(m.GuildID)
	if err != nil {
		log.Printf("autoreacts: %v", err)
		return
	}
	text := normalise(m.Content)
	for _, r := range rows {
		if strings.Contains(text, r.Phrase) {
			if err := b.s.MessageReactionAdd(m.ChannelID, m.ID, reactionEmoji(r.Emote)); err != nil {
				log.Printf("autoreact %q: %v", r.Phrase, err)
			}
		}
	}
}
