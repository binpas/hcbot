package bot

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// A Confirm/Cancel prompt keeps no state in memory. The buttons' custom IDs
// hold the action, the invoker, and an expiry, plus any extra fields:
//
//	confirm:<yes|no>:<action>:<invoker>:<expiry>[:<extra>...]
//
// Register the action's handler with addConfirm, then attach
// confirmButtons(...) to an ephemeral reply. The handler runs only when the
// invoker clicks Confirm before the expiry; it gets the extra fields and
// must answer with c.Update or c.DeferUpdate. Cancel and a late click are
// handled here.

const confirmTimeout = 120 * time.Second

func (b *Bot) registerConfirm() {
	b.addComponent("confirm", b.onConfirmComponent)
}

// addConfirm registers what a Confirm click on the given action does.
func (b *Bot) addConfirm(action string, onConfirm func(c *Ctx, extra []string)) {
	b.confirms[action] = onConfirm
}

// confirmButtons builds the Confirm/Cancel row for a prompt.
func confirmButtons(action, invoker string, extra ...string) []discordgo.MessageComponent {
	return confirmRow(action, invoker, time.Now().Add(confirmTimeout).Unix(), extra, false)
}

func confirmRow(action, invoker string, expiry int64, extra []string, disabled bool) []discordgo.MessageComponent {
	id := func(choice string) string {
		return strings.Join(append([]string{"confirm", choice, action, invoker, strconv.FormatInt(expiry, 10)}, extra...), ":")
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Confirm", Style: discordgo.DangerButton, Emoji: &discordgo.ComponentEmoji{Name: "✅"},
			CustomID: id("yes"), Disabled: disabled},
		discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, Emoji: &discordgo.ComponentEmoji{Name: "❌"},
			CustomID: id("no"), Disabled: disabled},
	}}}
}

func (b *Bot) onConfirmComponent(c *Ctx) {
	if len(c.Args) < 4 {
		return
	}
	choice, action, invoker := c.Args[0], c.Args[1], c.Args[2]
	expiry, _ := strconv.ParseInt(c.Args[3], 10, 64)
	extra := c.Args[4:]
	onConfirm, ok := b.confirms[action]
	if !ok {
		c.logErr("confirm", fmt.Errorf("no handler for action %q", action))
		return
	}

	if c.UserID() != invoker {
		c.Reply(true, errEmbed("This isn't your prompt to answer."))
		return
	}
	if time.Now().Unix() > expiry {
		c.Update([]*discordgo.MessageEmbed{errEmbed("Timed out. No changes made.")},
			confirmRow(action, invoker, expiry, extra, true))
		return
	}
	if choice != "yes" {
		c.Update([]*discordgo.MessageEmbed{errEmbed("Cancelled. No changes made.")}, []discordgo.MessageComponent{})
		return
	}
	onConfirm(c, extra)
}

// pendingTimeout is how long data behind a button is kept.
const pendingTimeout = confirmTimeout + time.Minute

// savePending stores data for a button and returns the ID to put in its
// custom ID.
func (b *Bot) savePending(kind, userID string, payload any) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	id, err := b.db.SavePending(kind, userID, string(data), time.Now().Add(pendingTimeout))
	return strconv.FormatInt(id, 10), err
}

// takePending loads (and deletes) the data behind a button into out. When
// it is gone or expired, it answers "Timed out" and returns false.
func (b *Bot) takePending(c *Ctx, kind, id string, out any) bool {
	n, _ := strconv.ParseInt(id, 10, 64)
	payload, ok, err := b.db.TakePending(n, kind, c.UserID())
	if err == nil && ok {
		err = json.Unmarshal([]byte(payload), out)
	}
	if err != nil {
		c.logErr("load pending "+kind, err)
	}
	if err != nil || !ok {
		c.Update([]*discordgo.MessageEmbed{errEmbed("Timed out. No changes made.")}, []discordgo.MessageComponent{})
		return false
	}
	return true
}
