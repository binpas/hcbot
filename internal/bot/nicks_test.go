package bot

import (
	"testing"
	"time"

	"github.com/binpas/hcbot/internal/db"
)

func TestNickSave(t *testing.T) {
	tb := newTestBot(t)
	tb.seeNick(tGuild, "901", "Zap")
	tb.seeNick(tGuild, "902", "")
	tb.saveNicks()
	saved, err := tb.db.MemberNicks(time.Time{}, 10)
	if err != nil || len(saved) != 2 {
		t.Fatalf("saved = %v, %v", saved, err)
	}

	// The same name again: nothing to save. A new name: saved.
	tb.seeNick(tGuild, "901", "Zap")
	if len(tb.nicks.dirty) != 0 {
		t.Errorf("dirty = %v", tb.nicks.dirty)
	}
	tb.seeNick(tGuild, "901", "Zip")

	// An expired name is deleted with the next save.
	if err := tb.db.SaveMemberNicks([]db.MemberNick{{GuildID: tGuild, UserID: "903", Nick: "Old",
		Seen: time.Now().Add(-nickExpire - time.Hour)}}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	tb.saveNicks()
	saved, _ = tb.db.MemberNicks(time.Time{}, 10)
	if len(saved) != 2 {
		t.Errorf("saved = %v, want 901 and 902", saved)
	}

	// A start reads them.
	tb.nicks = newNickCache()
	tb.loadNicks()
	if nick, ok := tb.cachedNick(tGuild, "901"); !ok || nick != "Zip" {
		t.Errorf("901 = %q, %v", nick, ok)
	}
	if _, ok := tb.cachedNick(tGuild, "902"); !ok {
		t.Error("902 (not a member) is not known")
	}
	if _, ok := tb.cachedNick(tGuild, "903"); ok {
		t.Error("903 (expired) is known")
	}
}
