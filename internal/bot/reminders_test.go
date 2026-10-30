package bot

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/binpas/hcbot/internal/db"
)

func TestRemindMention(t *testing.T) {
	tb := newTestBot(t)

	tb.say(tUser, tChGeneral, "remind me to stretch in 2h", true)
	sent := tb.sent(tChGeneral)
	if len(sent) != 1 || !strings.Contains(sent[0], "I will remind you on **") || !strings.Contains(sent[0], "(Pacific time)") {
		t.Fatalf("reply = %q", sent)
	}
	list, _ := tb.db.Reminders(tGuild, tUser)
	if len(list) != 1 || list[0].Text != "stretch" || list[0].TargetID != tUser || time.Until(list[0].At) < 119*time.Minute {
		t.Fatalf("reminders = %+v", list)
	}

	// A bad time gets the help, and nothing is saved.
	tb.api.reset()
	tb.say(tUser, tChGeneral, "remind me to stretch someday", true)
	if s := tb.sent(tChGeneral); len(s) != 1 || !strings.Contains(s[0], "The time is not one I can read") {
		t.Errorf("bad time reply = %q", s)
	}

	// Only a mod can remind another member.
	tb.api.reset()
	tb.say(tUser, tChGeneral, "remind <@"+tTarget+"> to stretch in 2h", true)
	if s := tb.sent(tChGeneral); len(s) != 1 || !strings.Contains(s[0], "Only mods") {
		t.Errorf("non-mod reply = %q", s)
	}
	tb.api.reset()
	tb.say(tMod, tChGeneral, "remind <@"+tTarget+"> about the rules on friday", true)
	if s := tb.sent(tChGeneral); len(s) != 1 || !strings.Contains(s[0], "I will remind <@"+tTarget+">") {
		t.Errorf("mod reply = %q", s)
	}
	if list, _ := tb.db.Reminders(tGuild, tTarget); len(list) != 1 || list[0].AuthorID != tMod || list[0].Text != "the rules" {
		t.Errorf("mod reminders = %+v", list)
	}

	// The limit.
	for range maxReminders - 1 {
		if _, err := tb.db.AddReminder(db.Reminder{GuildID: tGuild, ChannelID: tChGeneral, MessageID: "1",
			AuthorID: tUser, TargetID: tUser, Text: "x", At: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	tb.api.reset()
	tb.say(tUser, tChGeneral, "remind me to stretch in 2h", true)
	if s := tb.sent(tChGeneral); len(s) != 1 || !strings.Contains(s[0], "which is the limit") {
		t.Errorf("limit reply = %q", s)
	}
}

func TestRunReminders(t *testing.T) {
	tb := newTestBot(t)
	add := func(author, target string, at time.Time) {
		if _, err := tb.db.AddReminder(db.Reminder{GuildID: tGuild, ChannelID: tChGeneral, MessageID: "777",
			AuthorID: author, TargetID: target, Text: "drink water", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	add(tUser, tUser, time.Now().Add(-10*time.Second))
	add(tMod, tTarget, time.Now().Add(-time.Hour)) // the bot was offline
	add(tUser, tUser, time.Now().Add(time.Hour))   // not due
	tb.runReminders()

	posts := tb.api.find("POST", "/channels/"+tChGeneral+"/messages")
	if len(posts) != 2 {
		t.Fatalf("posts = %v", tb.callList())
	}
	second, first := posts[0].JSON(), posts[1].JSON() // the oldest first
	if c := first["content"].(string); c != "⏰ <@"+tUser+">, you asked me to remind you: drink water" {
		t.Errorf("first = %q", c)
	}
	if ref := first["message_reference"].(map[string]any); ref["message_id"] != "777" || ref["fail_if_not_exists"] != false {
		t.Errorf("reference = %v", ref)
	}
	if am := first["allowed_mentions"].(map[string]any); len(am["users"].([]any)) != 1 || am["users"].([]any)[0] != tUser {
		t.Errorf("allowed mentions = %v", am)
	}
	if c := second["content"].(string); !strings.Contains(c, "<@"+tMod+"> asked me to remind you") || !strings.Contains(c, "This is late") {
		t.Errorf("second = %q", c)
	}
	if list, _ := tb.db.Reminders(tGuild, ""); len(list) != 1 {
		t.Errorf("left = %+v", list)
	}

	// A failed send is tried again; a channel that is gone is not.
	add(tUser, tUser, time.Now().Add(-time.Minute))
	tb.api.reset()
	tb.api.on("POST", "/channels/"+tChGeneral+"/messages", 500, `{}`)
	tb.runReminders()
	if list, _ := tb.db.Reminders(tGuild, ""); len(list) != 2 {
		t.Errorf("after a 500: %d left, want 2", len(list))
	}
	tb.api.reset()
	tb.api.on("POST", "/channels/"+tChGeneral+"/messages", 404, `{"code":10003}`)
	tb.runReminders()
	if list, _ := tb.db.Reminders(tGuild, ""); len(list) != 1 {
		t.Errorf("after a 404: %d left, want 1", len(list))
	}
}

func TestRemindersCommand(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tUser, "reminders")
	tb.expect(true, "There are no reminders")

	mine, _ := tb.db.AddReminder(db.Reminder{GuildID: tGuild, ChannelID: tChGeneral, MessageID: "1",
		AuthorID: tUser, TargetID: tUser, Text: "feed the cat", At: time.Now().Add(time.Hour)})
	theirs, _ := tb.db.AddReminder(db.Reminder{GuildID: tGuild, ChannelID: tChGeneral, MessageID: "2",
		AuthorID: tMod, TargetID: tTarget, Text: "read the rules", At: time.Now().Add(2 * time.Hour)})

	tb.run(tUser, "reminders")
	r := tb.expect(true, "feed the cat")
	if strings.Contains(r.text(), "read the rules") {
		t.Error("a member sees another member's reminder")
	}
	tb.run(tUser, "reminders", boolOpt("all", true))
	tb.expect(true, "Only mods")

	// A member cannot delete another member's reminder, even with a forged menu value.
	tb.click(tUser, "reminder:del:0", strconv.FormatInt(theirs, 10))
	tb.expect(true, "You cannot delete")

	tb.click(tUser, "reminder:del:0", strconv.FormatInt(mine, 10))
	tb.expect(false, "Deleted reminder #"+strconv.FormatInt(mine, 10), "There are no reminders")

	tb.run(tMod, "reminders", boolOpt("all", true))
	tb.expect(true, "read the rules", "All reminders")
	tb.click(tMod, "reminder:del:1", strconv.FormatInt(theirs, 10))
	if list, _ := tb.db.Reminders(tGuild, ""); len(list) != 0 {
		t.Errorf("left = %+v", list)
	}
}
