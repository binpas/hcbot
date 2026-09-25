package bot

import (
	"log"
	"runtime/debug"

	"github.com/bwmarrin/discordgo"
)

func recoverHandler(what string) {
	if p := recover(); p != nil {
		log.Printf("panic in %s: %v\n%s", what, p, debug.Stack())
	}
}

// onReactionAdd is the one handler for added reactions, in the Python
// bot's order: reaction roles, tickets, invite deletion, then curation.
func (b *Bot) onReactionAdd(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	defer recoverHandler("reaction add handler")
	if b.env.SetupMode || r.GuildID == "" || r.UserID == s.State.User.ID {
		return
	}
	if b.onReactionRole(r.GuildID, r.MessageID, r.UserID, r.Emoji, true) {
		return
	}

	// Tickets ignore bots. Curation counts every reaction, as in Python.
	m := r.Member
	if m == nil || m.User == nil {
		m = b.resolveMember(r.GuildID, r.UserID)
	}
	human := m != nil && m.User != nil && !m.User.Bot
	if human && b.onTicketReaction(r, m) {
		return
	}
	if r.Emoji.Name == "❌" && r.Emoji.ID == "" {
		// ❌ deletes an invite from its MOD_LOG entry; it is never curated.
		if human {
			b.onInviteDeleteReaction(r, m)
		}
		return
	}
	b.curate(r.MessageReaction)
}

// onReactionRemove removes reaction roles.
func (b *Bot) onReactionRemove(s *discordgo.Session, r *discordgo.MessageReactionRemove) {
	defer recoverHandler("reaction remove handler")
	if b.env.SetupMode || r.GuildID == "" || r.UserID == s.State.User.ID {
		return
	}
	b.onReactionRole(r.GuildID, r.MessageID, r.UserID, r.Emoji, false)
}
