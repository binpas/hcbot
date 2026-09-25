package bot

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestHumaniseDuration(t *testing.T) {
	cases := map[int64]string{
		0:                          "0m",
		-5:                         "0m",
		30:                         "30s",
		60:                         "1m",
		3600 + 1800:                "1h 30m",
		86400 + 7200 + 60:          "1d 2h 1m",
		604800 + 86400 + 61:        "1w 1d 1m",
		604800 + 86400 + 3600 + 60: "1w 1d 1h", // at most three parts
	}
	for in, want := range cases {
		if got := humaniseDuration(in); got != want {
			t.Errorf("humaniseDuration(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRoleBelow(t *testing.T) {
	low := &discordgo.Role{ID: "5", Position: 1}
	high := &discordgo.Role{ID: "6", Position: 2}
	if !roleBelow(low, high) || roleBelow(high, low) {
		t.Error("position order wrong")
	}
	older := &discordgo.Role{ID: "100", Position: 3}
	newer := &discordgo.Role{ID: "200", Position: 3}
	if !roleBelow(newer, older) || roleBelow(older, newer) {
		t.Error("equal-position tie-break wrong: the older (lower ID) role should rank higher")
	}
}
