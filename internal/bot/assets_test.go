package bot

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func assetBot(t *testing.T) *testBot {
	tb := newTestBot(t)
	g, _ := tb.s.State.Guild(tGuild)
	g.Emojis = []*discordgo.Emoji{{ID: "e1", Name: "blob"}, {ID: "e2", Name: "dance", Animated: true}}
	g.Stickers = []*discordgo.Sticker{
		{ID: "s1", Name: "stick", FormatType: discordgo.StickerFormatTypePNG, Tags: "😀", Description: "a stick"},
		{ID: "s2", Name: "lottie", FormatType: discordgo.StickerFormatTypeLottie},
	}
	tb.api.on("GET", "/emojis/e1.png", 200, "PNG-e1")
	tb.api.on("GET", "/emojis/e2.gif", 200, "GIF-e2")
	tb.api.on("GET", "/stickers/s1.png", 200, "PNG-s1")
	return tb
}

func buildZip(t *testing.T, manifest string, files map[string]string) string {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if manifest != "" {
		files["manifest.json"] = manifest
	}
	for name, data := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestAssetsExport(t *testing.T) {
	tb := assetBot(t)
	tb.run(tMod, "assets export")
	tb.expect(true, "don't have permission")

	tb.run(tAdmin, "assets export")
	tb.expect(true, "Exported **2** emoji(s) and **1** sticker(s).", "**1** sticker(s) skipped")
	data := tb.lastFiles()["assets-export.zip"]
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	emojis, stickers, problems, ok := parseAssetManifest(zr)
	if !ok || len(emojis) != 2 || len(stickers) != 1 || problems != 0 {
		t.Fatalf("manifest: %+v %+v %d %v", emojis, stickers, problems, ok)
	}
	if !emojis[1].Animated || emojis[1].Filename != "emoji_e2.gif" || stickers[0].Emoji != "😀" || stickers[0].Description != "a stick" {
		t.Errorf("entries = %+v %+v", emojis, stickers)
	}
	if got, _ := readZipFile(zr, "sticker_s1.png"); string(got) != "PNG-s1" {
		t.Errorf("sticker file = %q", got)
	}
}

func TestAssetsImport(t *testing.T) {
	tb := assetBot(t)
	manifest := `[
		{"type":"emoji","name":"newone","filename":"a.png"},
		{"type":"emoji","name":"blob","filename":"b.png"},
		{"type":"sticker","name":"newstick","filename":"c.png","description":"d","emoji":"🔥"},
		{"type":"emoji","name":"","filename":"a.png"}
	]`
	zipData := buildZip(t, manifest, map[string]string{"a.png": "A", "b.png": "B", "c.png": "C"})
	tb.api.on("POST", "/guilds/100/emojis", 200, `{"id":"e9","name":"x"}`)

	tb.run(tAdmin, "assets import", tb.attachmentOpt("file", "a.zip", zipData))
	tb.expect(true, "Added **1** emoji(s) and **1** sticker(s).", "**1** entry in the file were skipped",
		"**1** item(s) share a name", "check your DMs")
	if c := tb.expectCall(1, "POST", "/guilds/100/emojis"); len(c) == 1 {
		if img, _ := c[0].JSON()["image"].(string); img != "data:image/png;base64,QQ==" {
			t.Errorf("emoji image = %q", img)
		}
	}
	if c := tb.expectCall(1, "POST", "/guilds/100/stickers"); len(c) == 1 && string(c[0].Files["c.png"]) != "C" {
		t.Errorf("sticker upload = %+v", c[0].Files)
	}

	// The conflict review went to the invoker's DM, with the new image.
	dm := tb.api.find("POST", "/channels/7000/messages")
	if len(dm) != 2 || string(dm[1].Files["b.png"]) != "B" || !strings.Contains(string(dm[1].Body), "asset:replace:") {
		t.Fatalf("DM = %d messages", len(dm))
	}
	var body struct {
		Components []struct {
			Components []struct {
				CustomID string `json:"custom_id"`
			} `json:"components"`
		} `json:"components"`
	}
	_ = json.Unmarshal(dm[1].Body, &body)
	replace := body.Components[0].Components[0].CustomID

	tb.api.reset()
	tb.clickDM(tAdmin, replace)
	tb.expectCall(1, "DELETE", "/guilds/100/emojis/e1")
	if c := tb.expectCall(1, "POST", "/guilds/100/emojis"); len(c) == 1 && c[0].JSON()["name"] != "blob" {
		t.Errorf("replacement = %s", c[0].Body)
	}
	r := tb.expect(true, "Replaced `blob`.")
	if r.Kind != "edit" || !r.Cleared {
		t.Errorf("review not closed: %+v", r)
	}
	edit := tb.api.find("PATCH", "/webhooks/.*/messages/@original")
	if !strings.Contains(string(edit[len(edit)-1].Body), `"attachments":[]`) {
		t.Error("the image was not removed from the review")
	}

	// Used once: a second click finds nothing.
	tb.clickDM(tAdmin, replace)
	tb.expect(true, "Timed out")
}

func TestAssetsImportKeepAndClosedDMs(t *testing.T) {
	tb := assetBot(t)
	zipData := buildZip(t, `[{"type":"sticker","name":"stick","filename":"s.png"}]`, map[string]string{"s.png": "S"})
	tb.api.fail("POST", "/users/@me/channels")
	tb.run(tAdmin, "assets import", tb.attachmentOpt("file", "a.zip", zipData))
	tb.expect(true, "") // the review is shown here instead
	rs := tb.replies()
	last := rs[len(rs)-1]
	if !strings.Contains(last.text(), "name conflict: `stick`") || !last.Ephemeral || len(last.Components) != 2 {
		t.Fatalf("fallback review = %+v", last)
	}
	if !strings.Contains(rs[len(rs)-2].text(), "Couldn't DM you") {
		t.Error("no note about the closed DMs")
	}
	tb.click(tAdmin, last.Components[1].CustomID) // Keep Existing
	tb.expect(true, "Kept the existing `stick`")
	tb.expectCall(0, "DELETE", ".*")
}

func TestAssetsImportChecks(t *testing.T) {
	tb := assetBot(t)
	tb.run(tAdmin, "assets import", tb.attachmentOpt("file", "bad.zip", "not a zip"))
	tb.expect(true, "doesn't look like a valid zip")
	tb.run(tAdmin, "assets import", tb.attachmentOpt("file", "n.zip", buildZip(t, "", map[string]string{"a.png": "A"})))
	tb.expect(true, "Couldn't find a valid `manifest.json`")

	// Static and animated emoji slots are counted separately.
	g, _ := tb.s.State.Guild(tGuild)
	for i := 0; i < 49; i++ {
		g.Emojis = append(g.Emojis, &discordgo.Emoji{ID: fmt.Sprint("x", i), Name: fmt.Sprint("static", i)})
	}
	two := buildZip(t, `[{"type":"emoji","name":"s1","filename":"a.png"},{"type":"emoji","name":"s2","filename":"a.png"},
		{"type":"emoji","name":"anim","filename":"b.gif"}]`, map[string]string{"a.png": "A", "b.gif": "G"})
	tb.run(tAdmin, "assets import", tb.attachmentOpt("file", "two.zip", two))
	tb.expect(true, "need 2 more static emoji slot(s)", "Free animated emoji slots: **49**", "No changes were made")
	tb.expectCall(0, "POST", "/guilds/100/emojis")

	tb.config(map[string]string{"ASSETS_IMPORT_MAX_MB": "1"})
	tb.run(tAdmin, "assets import", tb.attachmentOpt("file", "big.zip", strings.Repeat("x", 1<<20+1)))
	tb.expect(true, "larger than **1 MB**")
}
