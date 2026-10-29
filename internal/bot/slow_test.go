package bot

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestLogSlow(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	interaction := func(typ discordgo.InteractionType, data discordgo.InteractionData) *discordgo.InteractionCreate {
		return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{Type: typ, Data: data}}
	}

	fast := &Ctx{i: interaction(discordgo.InteractionApplicationCommand, nil), cmdName: "fast", start: time.Now()}
	fast.markResponded()
	fast.logSlow()

	late := &Ctx{i: interaction(discordgo.InteractionApplicationCommand, nil), cmdName: "comic", start: time.Now().Add(-3 * time.Second)}
	late.markResponded()
	late.start = late.start.Add(-time.Second) // a later answer does not change the first
	late.markResponded()
	late.logSlow()

	none := &Ctx{i: interaction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "comicx:5:6"}),
		start: time.Now().Add(-3 * time.Second)}
	none.logSlow()

	logSlow("message handler", time.Now().Add(-5*time.Second))
	logSlow("reaction add handler", time.Now())

	out := buf.String()
	for _, want := range []string{"slow: comic: first answer after 3", "slow: component comicx: no answer after 3", "slow: message handler took 5"} {
		if !strings.Contains(out, want) {
			t.Errorf("log has no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "fast") || strings.Contains(out, "reaction") {
		t.Errorf("a fast handler is logged:\n%s", out)
	}
}
