package bot

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestParseDuration(t *testing.T) {
	good := map[string]int64{
		"30":     1800, // a bare number means minutes
		"30m":    1800,
		"2h30m":  9000,
		"1d":     86400,
		" 1W ":   604800,
		"2h 15s": 7215,
		"5 m":    300,
	}
	for in, want := range good {
		if got, ok := parseDuration(in); !ok || got != want {
			t.Errorf("parseDuration(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "0", "-5", "+5", "0m", "spamming", "99999999999999999999", "999999999999w"} {
		if got, ok := parseDuration(in); ok {
			t.Errorf("parseDuration(%q) = %d, want not ok", in, got)
		}
	}
}

func TestChannelPerms(t *testing.T) {
	const (
		view  = discordgo.PermissionViewChannel
		send  = discordgo.PermissionSendMessages
		guild = "1"
		jail  = "10"
		vip   = "11"
		admin = "12"
	)
	g := &discordgo.Guild{ID: guild, OwnerID: "999", Roles: []*discordgo.Role{
		{ID: guild, Permissions: view | send},
		{ID: jail},
		{ID: vip},
		{ID: admin, Permissions: discordgo.PermissionAdministrator},
	}}
	role := discordgo.PermissionOverwriteTypeRole
	ch := &discordgo.Channel{ID: "50", PermissionOverwrites: []*discordgo.PermissionOverwrite{
		{ID: jail, Type: role, Deny: view},
		{ID: vip, Type: role, Allow: view},
	}}

	if channelPerms(g, ch, "", []string{jail})&view != 0 {
		t.Error("jail role alone should not see the channel")
	}
	// Role allows win over role denies.
	if channelPerms(g, ch, "5", []string{jail, vip})&view == 0 {
		t.Error("jail + vip should see the channel")
	}
	if channelPerms(g, ch, "5", []string{jail, admin}) != discordgo.PermissionAll {
		t.Error("administrator should have everything")
	}
	if channelPerms(g, ch, "999", []string{jail}) != discordgo.PermissionAll {
		t.Error("owner should have everything")
	}
	// A member overwrite comes last.
	withMember := withOverwrite(ch, discordgo.PermissionOverwrite{ID: "5", Type: discordgo.PermissionOverwriteTypeMember, Deny: view})
	if channelPerms(g, withMember, "5", []string{jail, vip})&view != 0 {
		t.Error("member deny should win")
	}
	// No view means no other permissions either.
	if p := channelPerms(g, ch, "", []string{jail}); p != 0 {
		t.Errorf("perms without view = %d, want 0", p)
	}
	if len(ch.PermissionOverwrites) != 2 {
		t.Error("withOverwrite changed the original channel")
	}
}

func TestSetPermMerges(t *testing.T) {
	ow := discordgo.PermissionOverwrite{Allow: discordgo.PermissionAddReactions, Deny: discordgo.PermissionViewChannel}
	setPerm(&ow, discordgo.PermissionSendMessages, false)
	setPerm(&ow, discordgo.PermissionAddReactions, false)
	if ow.Deny != discordgo.PermissionViewChannel|discordgo.PermissionSendMessages|discordgo.PermissionAddReactions || ow.Allow != 0 {
		t.Errorf("got allow=%d deny=%d", ow.Allow, ow.Deny)
	}
}

func TestTrimLines(t *testing.T) {
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = strings.Repeat("x", 49)
	}
	got := trimLines(lines, 1000)
	if len(got) > 1000 || !strings.HasSuffix(got, "more.") {
		t.Errorf("trimLines: len %d, tail %q", len(got), got[len(got)-10:])
	}
	if trimLines(lines[:3], 1000) != strings.Join(lines[:3], "\n") {
		t.Error("short input should be unchanged")
	}
}
