package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/comic"
	"github.com/binpas/hcbot/internal/config"
)

// newComicBot is a test bot with the Message Content option, the test art
// pack, and a fake profile picture fetch that fails for user 666.
func newComicBot(t *testing.T) (*testBot, *[]string) {
	t.Helper()
	dir := t.TempDir()
	if err := comic.WriteTestPack(dir); err != nil {
		t.Fatal(err)
	}
	tb := newTestBot(t, func(e *config.Env) { e.MessageContent, e.ComicArtDir = true, dir })
	var mu sync.Mutex
	var fetched []string
	pic := testPNG(t)
	tb.fetchAvatar = func(_ context.Context, url string) ([]byte, error) {
		mu.Lock()
		fetched = append(fetched, url)
		mu.Unlock()
		if strings.Contains(url, "/666/") {
			return nil, errors.New("no picture")
		}
		return pic, nil
	}
	return tb, &fetched
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// chatJSON is a channel history (newest first, as Discord sends it):
// text from three people and the bot, a message with only a file, and a
// join message, which are skipped.
func chatJSON(ids ...int) string {
	authors := map[int]string{
		1001: `{"id":"` + tUser + `","username":"user","avatar":"abc"}`,
		1002: `{"id":"666","username":"nopic","avatar":"def"}`,
		1003: `{"id":"` + tUser + `","username":"user","avatar":"abc"}`,
		1004: `{"id":"` + tBot + `","username":"healthbot","bot":true}`,
		1005: `{"id":"` + tMod + `","username":"mod"}`,
		1006: `{"id":"` + tMod + `","username":"mod"}`,
		1007: `{"id":"` + tUser + `","username":"user","avatar":"abc"}`,
	}
	texts := map[int]string{1001: "hello there", 1002: "lol **bold** <@" + tUser + ">", 1003: "I think so",
		1004: "beep boop", 1005: "", 1006: "join", 1007: "ok then"}
	var parts []string
	for i := len(ids) - 1; i >= 0; i-- {
		id := ids[i]
		typ := 0
		if id == 1006 {
			typ = 7
		}
		mentions := "[]"
		if id == 1002 {
			mentions = `[{"id":"` + tUser + `","username":"user"}]`
		}
		parts = append(parts, fmt.Sprintf(`{"id":"%d","channel_id":"%s","type":%d,"content":%q,"author":%s,"mentions":%s}`,
			id, tChGeneral, typ, texts[id], authors[id], mentions))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

var allChat = []int{1001, 1002, 1003, 1004, 1005, 1006, 1007}

// pngFile returns the PNG uploaded with the given name, decoded.
func (tb *testBot) pngFile(name string) image.Image {
	tb.t.Helper()
	for _, c := range tb.api.calls {
		if data, ok := c.Files[name]; ok {
			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				tb.t.Fatalf("%s: %v", name, err)
			}
			return img
		}
	}
	tb.t.Fatalf("no file %s; calls %v", name, tb.callList())
	return nil
}

func TestComicNotRegistered(t *testing.T) {
	tb := newTestBot(t) // no Message Content
	if _, ok := tb.commands["comic"]; ok || tb.comics != nil {
		t.Error("comics are registered without Message Content")
	}
	tb = newTestBot(t, func(e *config.Env) { e.MessageContent, e.ComicArtDir = true, t.TempDir() })
	if _, ok := tb.commands["comic"]; ok {
		t.Error("comics are registered without art")
	}
	tb, _ = newComicBot(t)
	for _, name := range []string{"comic", comicMenu, "character"} {
		if _, ok := tb.commands[name]; !ok {
			t.Errorf("%s is not registered", name)
		}
	}
}

func TestComicCommand(t *testing.T) {
	tb, fetched := newComicBot(t)
	tb.api.on("GET", `/channels/`+tChGeneral+`/messages`, 200, chatJSON(allChat...))
	tb.run(tUser, "comic", intOpt("count", 3))
	if rs := tb.replies(); len(rs) == 0 || rs[0].Kind != "defer" || rs[0].Ephemeral {
		t.Fatalf("first reply = %+v", rs)
	}
	gets := tb.api.find("GET", `/channels/`+tChGeneral+`/messages`)
	if len(gets) != 1 || !strings.Contains(gets[0].Query, "limit=9") || strings.Contains(gets[0].Query, "before") {
		t.Errorf("history requests = %+v", gets)
	}
	img := tb.pngFile("comic.png")
	// The last 3 messages with text (1003, 1004, 1007): two panels, and
	// the cast panel, in one row.
	if b := img.Bounds(); b.Dx() < 900 || b.Dy() > 400 {
		t.Errorf("comic size = %v", b)
	}
	if len(*fetched) != 2 { // user and the bot
		t.Errorf("profile pictures = %v", *fetched)
	}

	// The cooldown.
	tb.run(tUser, "comic")
	tb.expect(true, "Wait")
	// A failing profile picture and markdown do not stop a comic.
	tb.config(map[string]string{"COMIC_COOLDOWN": "0"})
	tb.api.reset()
	tb.api.on("GET", `/channels/`+tChGeneral+`/messages`, 200, chatJSON(allChat...))
	tb.run(tUser, "comic", intOpt("count", 20))
	tb.pngFile("comic.png")
}

func TestComicChecks(t *testing.T) {
	tb, _ := newComicBot(t)
	tb.config(map[string]string{"COMIC_COOLDOWN": "0"})
	tb.run(tUser, "comic") // no messages
	tb.expect(true, "no messages with text")

	tb.config(map[string]string{"COMIC_CHANNELS": tChSupport})
	tb.run(tUser, "comic")
	tb.expect(true, "do not work in <#"+tChGeneral+">")
	tb.runMessageMenu(tUser, comicMenu, &discordgo.Message{ID: "1001", Content: "hi", Author: &discordgo.User{ID: tUser}})
	tb.expect(true, "do not work in <#"+tChGeneral+">")

	tb.config(map[string]string{"COMIC_CHANNELS": "", "COMIC_ENABLED": "off"})
	tb.run(tUser, "comic")
	tb.expect(true, "Comics are off")
}

func TestComicMention(t *testing.T) {
	tb, _ := newComicBot(t)
	tb.api.on("GET", `/channels/`+tChGeneral+`/messages`, 200, chatJSON(allChat...))
	tb.onMessageCreate(tb.s, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "1100", ChannelID: tChGeneral, GuildID: tGuild, Content: "<@" + tBot + "> comic 2",
		Author: &discordgo.User{ID: tUser}, Mentions: []*discordgo.User{{ID: tBot}}}})
	gets := tb.api.find("GET", `/channels/`+tChGeneral+`/messages`)
	if len(gets) != 1 || !strings.Contains(gets[0].Query, "before=1100") || !strings.Contains(gets[0].Query, "limit=6") {
		t.Errorf("history requests = %+v", gets)
	}
	posts := tb.api.find("POST", `/channels/`+tChGeneral+`/messages`)
	if len(posts) != 1 || posts[0].Files["comic.png"] == nil || !strings.Contains(string(posts[0].Body), `"message_id":"1100"`) {
		t.Fatalf("posts = %v", tb.callList())
	}

	// The cooldown answers in the channel.
	tb.onMessageCreate(tb.s, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "1101", ChannelID: tChGeneral, GuildID: tGuild, Content: "<@" + tBot + "> comic",
		Author: &discordgo.User{ID: tUser}, Mentions: []*discordgo.User{{ID: tBot}}}})
	if s := tb.sent(tChGeneral); len(s) != 2 || !strings.Contains(s[1], "Wait") {
		t.Errorf("sent = %q", s)
	}
}

var comicIDRE = regexp.MustCompile(`comicx:(\d+):`)

// editorBodies returns the bodies of the editor messages (the menu answer,
// the updates, and the edits), in order.
func (tb *testBot) editorBodies() []string {
	var out []string
	for _, c := range tb.api.calls {
		if (c.Method == "POST" && strings.HasSuffix(c.Path, "/callback")) || (c.Method == "PATCH" && strings.HasSuffix(c.Path, "/@original")) {
			out = append(out, string(c.Body))
		}
	}
	return out
}

func TestComicEditor(t *testing.T) {
	tb, _ := newComicBot(t)
	tb.config(map[string]string{"COMIC_COOLDOWN": "0"})
	tb.api.on("GET", `/channels/`+tChGeneral+`/messages`, 200, chatJSON(1002, 1003, 1004, 1005, 1006, 1007))
	start := &discordgo.Message{ID: "1001", Content: "hello there", Author: &discordgo.User{ID: tUser, Username: "user"}}
	tb.runMessageMenu(tUser, comicMenu, start)

	gets := tb.api.find("GET", `/channels/`+tChGeneral+`/messages`)
	if len(gets) != 1 || !strings.Contains(gets[0].Query, "after=1001") || !strings.Contains(gets[0].Query, "limit=9") {
		t.Errorf("history requests = %+v", gets)
	}
	bodies := tb.editorBodies()
	if len(bodies) != 1 {
		t.Fatalf("editor = %v", tb.callList())
	}
	var resp struct {
		Data struct {
			Flags int `json:"flags"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(bodies[0]), &resp)
	if resp.Data.Flags != int(discordgo.MessageFlagsEphemeral|discordgo.MessageFlagsIsComponentsV2) {
		t.Errorf("flags = %d", resp.Data.Flags)
	}
	ed := bodies[0]
	for _, want := range []string{"comicx:", ":1001\"", ":1007\"", "comicbg:", "comicpre:", "comicpost:", "comicno:", `lol \\*\\*bold\\*\\* @user`, "beep boop"} {
		if !strings.Contains(ed, want) {
			t.Errorf("editor has no %s: %s", want, ed)
		}
	}
	for _, skip := range []string{":1005\"", ":1006\""} {
		if strings.Contains(ed, skip) {
			t.Errorf("editor has the skipped message %s", skip)
		}
	}
	m := comicIDRE.FindStringSubmatch(ed)
	if m == nil {
		t.Fatal("no editor ID")
	}
	id := m[1]

	// Remove one message, then add it back.
	tb.api.reset()
	tb.api.on("GET", `/channels/`+tChGeneral+`/messages`, 200, chatJSON(allChat...))
	tb.click(tUser, "comicx:"+id+":1004")
	ed = strings.Join(tb.editorBodies(), "")
	if !strings.Contains(ed, `"type":7`) || !strings.Contains(ed, "~~**healthbot**: beep boop~~") || !strings.Contains(ed, "Add back") {
		t.Errorf("after remove: %s", ed)
	}
	if g := tb.api.find("GET", `/channels/`+tChGeneral+`/messages`); len(g) != 1 || !strings.Contains(g[0].Query, "before=1008") {
		t.Errorf("reread = %+v", g)
	}

	// Background, then Preview: the kept messages, with the image.
	tb.click(tUser, "comicbg:"+id, "plain")
	if ed = strings.Join(tb.editorBodies(), ""); !strings.Contains(ed, `"value":"plain","description":"","default":true`) {
		t.Errorf("background not selected: %s", ed)
	}
	tb.api.reset()
	tb.api.on("GET", `/channels/`+tChGeneral+`/messages`, 200, chatJSON(allChat...))
	tb.click(tUser, "comicpre:"+id)
	if rs := tb.replies(); len(rs) == 0 || rs[0].Kind != "defer-update" {
		t.Errorf("preview replies = %+v", rs)
	}
	ed = strings.Join(tb.editorBodies(), "")
	if !strings.Contains(ed, "attachment://preview.png") || !strings.Contains(ed, `"filename":"preview.png"`) {
		t.Errorf("preview edit = %s", ed)
	}
	tb.pngFile("preview.png")

	// Someone else cannot use the editor.
	tb.api.reset()
	tb.click(tMod, "comicpost:"+id)
	if ed = strings.Join(tb.editorBodies(), ""); !strings.Contains(ed, "expired") {
		t.Errorf("other member = %s", ed)
	}

	// Post: the comic goes to the channel, and the editor says so.
	tb.api.reset()
	tb.api.on("GET", `/channels/`+tChGeneral+`/messages`, 200, chatJSON(allChat...))
	tb.click(tUser, "comicpost:"+id)
	posts := tb.api.find("POST", `/channels/`+tChGeneral+`/messages`)
	if len(posts) != 1 || posts[0].Files["comic.png"] == nil || !strings.Contains(tb.sent(tChGeneral)[0], "Comic by <@"+tUser+">") {
		t.Fatalf("post = %v", tb.callList())
	}
	if ed = strings.Join(tb.editorBodies(), ""); !strings.Contains(ed, "Posted the comic") {
		t.Errorf("after post = %s", ed)
	}
	// The editor is gone.
	tb.api.reset()
	tb.click(tUser, "comicx:"+id+":1001")
	if ed = strings.Join(tb.editorBodies(), ""); !strings.Contains(ed, "expired") {
		t.Errorf("after post, a click = %s", ed)
	}
}

func TestComicEditorCancelAndCooldown(t *testing.T) {
	tb, _ := newComicBot(t)
	tb.api.on("GET", `/channels/`+tChGeneral+`/messages`, 200, chatJSON(1002))
	start := &discordgo.Message{ID: "1001", Content: "hello there", Author: &discordgo.User{ID: tUser, Username: "user"}}
	tb.runMessageMenu(tUser, comicMenu, start)
	id := comicIDRE.FindStringSubmatch(strings.Join(tb.editorBodies(), ""))[1]

	tb.comicWait(tUser, true) // a comic was just made
	tb.api.reset()
	tb.api.on("GET", `/channels/`+tChGeneral+`/messages`, 200, chatJSON(1001, 1002))
	tb.click(tUser, "comicpost:"+id)
	if len(tb.api.find("POST", `/channels/`+tChGeneral+`/messages`)) != 0 || !strings.Contains(strings.Join(tb.editorBodies(), ""), "Wait") {
		t.Errorf("post during the cooldown: %v", tb.callList())
	}
	tb.api.reset()
	tb.click(tUser, "comicno:"+id)
	if ed := strings.Join(tb.editorBodies(), ""); !strings.Contains(ed, "Cancelled") {
		t.Errorf("cancel = %s", ed)
	}
}

func TestCharacter(t *testing.T) {
	tb, _ := newComicBot(t)
	auto := tb.comics.pack.CharacterFor(tUser)
	tb.run(tUser, "character")
	tb.expect(true, "You have not chosen", "**"+auto+"**")
	tb.pngFile("character.png")

	tb.run(tUser, "character", strOpt("name", "Nobody"))
	tb.expect(true, "no character named `nobody`")

	tb.run(tUser, "character", strOpt("name", "Ann"))
	tb.expect(true, "now **ann**")
	if c, _ := tb.db.ComicCharacter(tGuild, tUser); c != "ann" {
		t.Errorf("stored = %q", c)
	}
	tb.run(tUser, "character")
	tb.expect(true, "Your character in comics is **ann**")

	tb.click(tUser, "charall")
	tb.pngFile("characters.png")
	tb.click(tUser, "charreset")
	tb.expect(true, "Reset")
	if c, _ := tb.db.ComicCharacter(tGuild, tUser); c != "" {
		t.Errorf("after reset = %q", c)
	}
}

func TestCharacterAutocomplete(t *testing.T) {
	tb, _ := newComicBot(t)
	tb.api.reset()
	tb.onInteraction(tb.s, tb.interaction(tUser, discordgo.InteractionApplicationCommandAutocomplete, discordgo.ApplicationCommandInteractionData{
		Name: "character", Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Name: "name", Type: discordgo.ApplicationCommandOptionString, Value: "B", Focused: true}},
	}))
	calls := tb.api.find("POST", `/interactions/.*/callback`)
	if len(calls) != 1 {
		t.Fatalf("calls = %v", tb.callList())
	}
	var resp struct {
		Type int `json:"type"`
		Data struct {
			Choices []struct {
				Name string `json:"name"`
			} `json:"choices"`
		} `json:"data"`
	}
	_ = json.Unmarshal(calls[0].Body, &resp)
	if resp.Type != 8 || len(resp.Data.Choices) != 1 || resp.Data.Choices[0].Name != "bob" {
		t.Errorf("autocomplete = %s", calls[0].Body)
	}
}

func TestComicUsesChosenCharacter(t *testing.T) {
	tb, _ := newComicBot(t)
	if err := tb.db.SetComicCharacter(tGuild, tUser, "sim"); err != nil {
		t.Fatal(err)
	}
	msgs := comicUsable([]*discordgo.Message{
		{ID: "2", Content: "lol", Author: &discordgo.User{ID: tUser}},
		{ID: "1", Content: "hi", Author: &discordgo.User{ID: tMod}},
	})
	if msgs[0].ID != "1" {
		t.Errorf("order = %s, %s", msgs[0].ID, msgs[1].ID)
	}
	if _, err := tb.drawComic(tGuild, msgs, "plain"); err != nil {
		t.Fatal(err)
	}
	if cmpSnowflake("999", "1000") >= 0 || cmpSnowflake("1001", "1000") <= 0 {
		t.Error("cmpSnowflake is wrong")
	}
}
