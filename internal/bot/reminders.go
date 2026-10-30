package bot

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/db"
)

// Reminders: "@Bot remind (me|@user) (to|about|that) <thing> (in|on|at)
// <time>". A message that mentions the bot has its text without the
// Message Content intent. Times are Pacific (reminderZone). Only mods can
// remind another member. The bot sends the reminder in the same channel,
// as a reply to the message that set it. /reminders lists and deletes
// them.
//
// Custom IDs:
//
//	reminder:del:<all>   select menu: delete a reminder (all: 1 for a mod's server list)

const (
	maxReminderText    = 1000
	maxReminders       = 25 // active reminders that one member can set
	remindersShown     = 20
	reminderLateAfter  = 2 * time.Minute
	reminderGiveUp     = 24 * time.Hour // stop a retry after a send fails this long
	reminderTimeFormat = "Monday, January 2, 2006 at 3:04 PM MST"
)

var reminderHead = regexp.MustCompile(`(?is)^remind\s+(me|<@!?(\d+)>)\s+(?:(?:to|about|that)\s+)?(.*)$`)

const reminderUsage = "Use `@BotName remind me to <thing> in <time>`. Examples:\n" +
	"- `remind me to stretch in 2h` (also `30m`, `1h30m`, `3d`, `2 weeks`)\n" +
	"- `remind me about the stream on friday` (at the time of your message)\n" +
	"- `remind me that rent is due on nov 1 at 9am` (also `11/1`, `2026-11-01`, `tomorrow`, `at 5pm`)\n" +
	"Times are Pacific time."

func (b *Bot) registerReminders() {
	b.addCommand(&command{
		name:        "reminders",
		description: "Show your reminders, and delete them.",
		options:     []*discordgo.ApplicationCommandOption{optBool("all", "Show all reminders on the server (mod only)", false)},
		handler:     b.cmdReminders,
	})
	b.addComponent("reminder", b.onReminderComponent)
}

// remindMention handles "@Bot remind ...". It reports whether the message
// was such a request.
func (b *Bot) remindMention(m *discordgo.Message) bool {
	if m.GuildID == "" || b.mentionKeyword(m) != "remind" {
		return false
	}
	reply := func(text string) {
		if _, err := b.s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content: text, Reference: m.Reference(), AllowedMentions: &discordgo.MessageAllowedMentions{},
		}); err != nil {
			log.Printf("remind: reply: %v", err)
		}
	}
	rest := strings.TrimSpace(regexp.MustCompile(`^<@!?`+b.s.State.User.ID+`>\s*`).ReplaceAllString(m.Content, ""))
	p := reminderHead.FindStringSubmatch(rest)
	if p == nil {
		reply("I could not read that reminder. " + reminderUsage)
		return true
	}
	target := m.Author.ID
	if p[2] != "" && p[2] != m.Author.ID {
		if !b.messageFromMod(m) {
			reply("Only mods can set a reminder for another member. Use `remind me`.")
			return true
		}
		for _, u := range m.Mentions {
			if u.ID == p[2] && u.Bot {
				reply("I cannot remind a bot.")
				return true
			}
		}
		target = p[2]
	}
	thing, at, err := splitReminder(p[3], time.Now())
	if err != nil {
		reply(upperFirst(err.Error()) + ". " + reminderUsage)
		return true
	}
	if len([]rune(thing)) > maxReminderText {
		reply(fmt.Sprintf("The reminder text is too long. The limit is %d characters.", maxReminderText))
		return true
	}
	if n, err := b.db.CountReminders(m.Author.ID); err != nil {
		log.Printf("remind: count: %v", err)
		reply("I could not save the reminder.")
		return true
	} else if n >= maxReminders {
		reply(fmt.Sprintf("You have %d reminders, which is the limit. Use `/reminders` to delete some.", n))
		b.m.reminders.WithLabelValues("limit").Inc()
		return true
	}
	if _, err := b.db.AddReminder(db.Reminder{GuildID: m.GuildID, ChannelID: m.ChannelID, MessageID: m.ID,
		AuthorID: m.Author.ID, TargetID: target, Text: thing, At: at}); err != nil {
		log.Printf("remind: save: %v", err)
		reply("I could not save the reminder.")
		return true
	}
	b.m.reminders.WithLabelValues("set").Inc()
	who := "you"
	if target != m.Author.ID {
		who = userMention(target)
	}
	reply(fmt.Sprintf("⏰ OK. I will remind %s on **%s** (Pacific time), %s.\n-# Use `/reminders` to see or delete your reminders.",
		who, at.In(reminderZone).Format(reminderTimeFormat), discordTime(at, "R")))
	return true
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func discordTime(t time.Time, style string) string {
	return fmt.Sprintf("<t:%d:%s>", t.Unix(), style)
}

// runReminders sends the reminders that are due. A reminder that fails
// for a reason that can pass (Discord is down) is tried again on the next
// run, for at most reminderGiveUp.
func (b *Bot) runReminders() {
	now := time.Now()
	due, err := b.db.DueReminders(now)
	if err != nil {
		log.Printf("reminders: %v", err)
		return
	}
	for _, r := range due {
		err := b.sendReminder(r, now)
		switch {
		case err == nil:
			b.m.reminders.WithLabelValues("sent").Inc()
		case !reminderGone(err) && now.Sub(r.At) < reminderGiveUp:
			log.Printf("reminders: send %d (will try again): %v", r.ID, err)
			continue
		default:
			log.Printf("reminders: send %d: %v", r.ID, err)
			b.m.reminders.WithLabelValues("failed").Inc()
		}
		if err := b.db.DeleteReminder(r.ID); err != nil {
			log.Printf("reminders: delete %d: %v", r.ID, err)
		}
	}
}

// reminderGone reports whether a send can never work (the channel is gone,
// or the bot cannot post there).
func reminderGone(err error) bool {
	var rest *discordgo.RESTError
	return errors.As(err, &rest) && rest.Response != nil &&
		rest.Response.StatusCode >= 400 && rest.Response.StatusCode < 500 && rest.Response.StatusCode != http.StatusTooManyRequests
}

func (b *Bot) sendReminder(r db.Reminder, now time.Time) error {
	from := "you asked me to remind you"
	if r.AuthorID != r.TargetID {
		from = userMention(r.AuthorID) + " asked me to remind you"
	}
	text := fmt.Sprintf("⏰ %s, %s: %s", userMention(r.TargetID), from, r.Text)
	if now.Sub(r.At) > reminderLateAfter {
		text += fmt.Sprintf("\n-# This is late. I was offline at %s.", discordTime(r.At, "f"))
	}
	no := false
	_, err := b.s.ChannelMessageSendComplex(r.ChannelID, &discordgo.MessageSend{
		Content: text,
		// Without the message, Discord sends it with no reply link.
		Reference:       &discordgo.MessageReference{MessageID: r.MessageID, ChannelID: r.ChannelID, GuildID: r.GuildID, FailIfNotExists: &no},
		AllowedMentions: &discordgo.MessageAllowedMentions{Users: []string{r.TargetID}},
	})
	return err
}

// cmdReminders shows the member's reminders (the ones they set or will
// get), or all of the server's for a mod with all:true.
func (b *Bot) cmdReminders(c *Ctx) {
	all := c.Bool("all", false)
	if all && !b.isMod(c.Member()) {
		c.Reply(true, errEmbed("Only mods can see all reminders."))
		return
	}
	embed, components, err := b.reminderList(c.GuildID(), c.UserID(), all)
	if err != nil {
		c.logErr("read reminders", err)
		c.Reply(true, errEmbed("I could not read the reminders."))
		return
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{embed}, components)
}

func (b *Bot) reminderList(guildID, userID string, all bool) (*discordgo.MessageEmbed, []discordgo.MessageComponent, error) {
	who, title := userID, "⏰ Your reminders"
	if all {
		who, title = "", "⏰ All reminders"
	}
	list, err := b.db.Reminders(guildID, who)
	if err != nil {
		return nil, nil, err
	}
	if len(list) == 0 {
		return makeEmbed(title, "There are no reminders. Set one with `@BotName remind me to <thing> in <time>`.", colourBlurple),
			[]discordgo.MessageComponent{}, nil // [] removes an old menu
	}
	var sb strings.Builder
	var options []discordgo.SelectMenuOption
	for i, r := range list {
		if i == remindersShown {
			fmt.Fprintf(&sb, "…and %d more.", len(list)-remindersShown)
			break
		}
		people := ""
		switch {
		case r.AuthorID != r.TargetID:
			people = fmt.Sprintf(" · for %s from %s", userMention(r.TargetID), userMention(r.AuthorID))
		case all:
			people = " · for " + userMention(r.TargetID)
		}
		fmt.Fprintf(&sb, "**#%d** · %s (%s)%s\n> %s\n", r.ID, discordTime(r.At, "f"), discordTime(r.At, "R"), people,
			truncateEllipsis(strings.Join(strings.Fields(r.Text), " "), 80))
		options = append(options, discordgo.SelectMenuOption{
			Label:       fmt.Sprintf("#%d · %s", r.ID, r.At.In(reminderZone).Format("Mon Jan 2, 3:04 PM MST")),
			Description: truncateEllipsis(strings.Join(strings.Fields(r.Text), " "), 100),
			Value:       strconv.FormatInt(r.ID, 10),
		})
	}
	flag := "0"
	if all {
		flag = "1"
	}
	components := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
		MenuType: discordgo.StringSelectMenu, CustomID: "reminder:del:" + flag, Placeholder: "Delete a reminder", Options: options,
	}}}}
	e := makeEmbed(title, sb.String(), colourBlurple)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Times show in your time zone."}
	return e, components, nil
}

// onReminderComponent deletes the chosen reminder, then shows the list
// again. The member who set it, the member who gets it, and mods can
// delete it.
func (b *Bot) onReminderComponent(c *Ctx) {
	vals := c.Values()
	if len(c.Args) < 2 || c.Args[0] != "del" || len(vals) != 1 {
		return
	}
	all := c.Args[1] == "1"
	mod := b.isMod(c.Member())
	if all && !mod {
		c.Reply(true, errEmbed("Only mods can see all reminders."))
		return
	}
	id, _ := strconv.ParseInt(vals[0], 10, 64)
	r, err := b.db.GetReminder(id)
	if err != nil {
		c.logErr("read reminder", err)
		c.Reply(true, errEmbed("I could not read the reminder."))
		return
	}
	if r != nil {
		if r.GuildID != c.GuildID() || (!mod && r.AuthorID != c.UserID() && r.TargetID != c.UserID()) {
			c.Reply(true, errEmbed("You cannot delete that reminder."))
			return
		}
		if err := b.db.DeleteReminder(id); err != nil {
			c.logErr("delete reminder", err)
			c.Reply(true, errEmbed("I could not delete the reminder."))
			return
		}
		b.m.reminders.WithLabelValues("deleted").Inc()
	}
	embed, components, err := b.reminderList(c.GuildID(), c.UserID(), all)
	if err != nil {
		c.logErr("read reminders", err)
		c.Reply(true, errEmbed("I could not read the reminders."))
		return
	}
	embed.Description = fmt.Sprintf("Deleted reminder #%d.\n\n", id) + embed.Description
	c.Update([]*discordgo.MessageEmbed{embed}, components)
}
