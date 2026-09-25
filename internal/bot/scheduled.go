package bot

import (
	"fmt"
	"log"

	"github.com/binpas/hcbot/internal/db"
)

// runScheduledActions runs every stored timer that has come due. Each one
// is tried once and then removed, so a failure (e.g. the ban was already
// lifted by hand) can't make it retry forever.
func (b *Bot) runScheduledActions() {
	if err := b.db.DeleteExpiredPending(); err != nil {
		log.Printf("pending actions: %v", err)
	}
	due, err := b.db.DueActions()
	if err != nil {
		log.Printf("scheduled actions: %v", err)
		return
	}
	for _, a := range due {
		switch a.Action {
		case db.ActionUnban:
			if err := b.s.GuildBanDelete(a.GuildID, a.UserID, auditReason("Temp-ban expired")); err != nil {
				log.Printf("tempban: unban %s: %v", a.Username, err)
			} else {
				b.modLog(makeEmbed("✅ Temp-Ban Expired",
					fmt.Sprintf("**%s** has been automatically unbanned.", a.Username), colourGreen))
			}
		case db.ActionRemoveRole:
			if err := b.s.GuildMemberRoleRemove(a.GuildID, a.UserID, a.RoleID,
				auditReason("MOTD expired")); err != nil {
				log.Printf("motd: remove role from %s: %v", a.Username, err)
			} else {
				b.modLog(makeEmbed("🌟 MOTD Expired",
					fmt.Sprintf("Removed MOTD role from %s.", userMention(a.UserID)), colourLightGrey))
			}
		default:
			log.Printf("scheduled actions: unknown action %q (id %d)", a.Action, a.ID)
		}
		if err := b.db.DeleteAction(a.ID); err != nil {
			log.Printf("scheduled actions: delete %d: %v", a.ID, err)
		}
	}
}
