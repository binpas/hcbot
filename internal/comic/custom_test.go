package comic

import (
	"image"
	"image/color"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// block is a w×h image: transparent, with a solid block of c in the
// middle (or all c when opaque).
func block(w, h int, c color.NRGBA, opaque bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			if opaque || (x > w/4 && x < 3*w/4 && y > 2 && y < h-2) {
				img.SetNRGBA(x, y, c)
			}
		}
	}
	return img
}

func writeCustom(t *testing.T, dir, path string, img image.Image) {
	t.Helper()
	full := filepath.Join(dir, "custom", path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePNG(full, img); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, dir, path, text string) {
	t.Helper()
	full := filepath.Join(dir, "custom", path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCustomArt(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTestPack(dir); err != nil {
		t.Fatal(err)
	}
	// Zed: neutral, happy (half the size), and wave, with a face point.
	writeCustom(t, dir, "characters/Zed/neutral.png", block(200, 400, color.NRGBA{1, 0, 0, 255}, false))
	writeCustom(t, dir, "characters/Zed/happy.png", block(100, 200, color.NRGBA{2, 0, 0, 255}, false))
	writeCustom(t, dir, "characters/Zed/wave.png", block(200, 400, color.NRGBA{3, 0, 0, 255}, false))
	writeFile(t, dir, "characters/Zed/character.json", `{"face": [120, 50]}`)
	// Problems.
	writeCustom(t, dir, "characters/Zed/dance.png", block(200, 400, color.NRGBA{4, 0, 0, 255}, false))
	writeCustom(t, dir, "characters/Zed/sad.png", block(200, 1200, color.NRGBA{5, 0, 0, 255}, false))
	writeCustom(t, dir, "characters/Zed/shout.png", block(200, 400, color.NRGBA{6, 0, 0, 255}, true))
	writeFile(t, dir, "characters/Zed/notes.txt", "hi")
	writeCustom(t, dir, "characters/nopose/happy.png", block(200, 400, color.NRGBA{7, 0, 0, 255}, false))
	writeCustom(t, dir, "characters/ann/neutral.png", block(200, 400, color.NRGBA{8, 0, 0, 255}, false))
	writeCustom(t, dir, "characters/bad name/neutral.png", block(200, 400, color.NRGBA{9, 0, 0, 255}, false))
	// Backgrounds.
	writeCustom(t, dir, "backgrounds/Park.png", block(400, 400, color.NRGBA{0, 9, 0, 255}, true))
	writeCustom(t, dir, "backgrounds/tiny.png", block(50, 50, color.NRGBA{0, 9, 0, 255}, true))
	writeCustom(t, dir, "backgrounds/plain.png", block(400, 400, color.NRGBA{0, 9, 0, 255}, true))

	p, err := LoadPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Names(); !slices.Equal(got, []string{"bob", "ann", "sim", "zed"}) {
		t.Errorf("names = %v", got)
	}
	if got := p.Backgrounds(); !slices.Equal(got, []string{"plain", "park"}) {
		t.Errorf("backgrounds = %v", got)
	}
	problems := strings.Join(p.Problems(), "\n")
	for _, want := range []string{
		"Zed/dance.png: unknown pose",
		"Zed/sad.png: the size is 200×1200",
		"Zed/shout.png: the background is not transparent",
		"Zed/notes.txt: use PNG files",
		"nopose: no neutral.png",
		`characters/ann: there is already a character named "ann"`,
		"bad name: the folder name must be",
		"backgrounds/tiny.png: the size is 50×50",
		`backgrounds/plain.png: there is already a background named "plain"`,
	} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems have no %q:\n%s", want, problems)
		}
	}

	zed := p.byName["zed"]
	rng := rand.New(rand.NewPCG(1, 1))
	color := func(f figure) uint8 { return f.img.NRGBAAt(f.img.Bounds().Dx()/2, f.img.Bounds().Dy()/2).R }
	if f := zed.figure(moodOf("the weather"), rng); color(f) != 1 || f.headX != 120 || f.headY != 50 {
		t.Errorf("neutral: color %d, head %d,%d", color(f), f.headX, f.headY)
	}
	if f := zed.figure(moodOf("yay"), rng); color(f) != 2 || f.headX != 60 || f.headY != 25 {
		t.Errorf("happy: color %d, head %d,%d (the face point scales)", color(f), f.headX, f.headY)
	}
	if f := zed.figure(moodOf("hello there"), rng); color(f) != 3 {
		t.Errorf("wave: color %d", color(f))
	}
	if f := zed.figure(moodOf("idk"), rng); color(f) != 1 {
		t.Errorf("a gesture it does not have: color %d, want neutral", color(f))
	}
	if b := zed.icon.Bounds(); b.Dx() != 100 || b.Dy() != 100 {
		t.Errorf("icon = %v, want a 100 px square from the head", b)
	}
	// The comic works with it.
	if _, err := p.PNG([]Message{{Speaker: "1", Character: "zed", Text: "yay"}}, Options{Background: "park"}); err != nil {
		t.Fatal(err)
	}
}

func TestCustomArtOnly(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadPack(dir); err == nil {
		t.Error("an empty folder loaded")
	}
	writeCustom(t, dir, "characters/solo/neutral.png", block(200, 400, color.NRGBA{1, 0, 0, 255}, false))
	writeCustom(t, dir, "characters/solo/icon.png", block(40, 40, color.NRGBA{1, 0, 0, 255}, false))
	p, err := LoadPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Names(); !slices.Equal(got, []string{"solo"}) || len(p.Backgrounds()) != 0 || len(p.Problems()) != 0 {
		t.Errorf("names %v, backgrounds %v, problems %v", got, p.Backgrounds(), p.Problems())
	}
	if b := p.byName["solo"].icon.Bounds(); b.Dx() != 40 {
		t.Errorf("icon = %v, want icon.png", b)
	}
}

func TestCustomPrivate(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTestPack(dir); err != nil {
		t.Fatal(err)
	}
	body := block(200, 400, color.NRGBA{1, 0, 0, 255}, false)
	writeCustom(t, dir, "characters/zed/neutral.png", body)
	writeFile(t, dir, "characters/zed/character.json", `{"users": ["5", "bob"]}`)
	writeCustom(t, dir, "characters/typo/neutral.png", body)
	writeFile(t, dir, "characters/typo/character.json", `{"users": ["5"`)
	writeCustom(t, dir, "characters/nobody/neutral.png", body)
	writeFile(t, dir, "characters/nobody/character.json", `{"users": ["ann"]}`)

	p, err := LoadPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Names(); !slices.Equal(got, []string{"bob", "ann", "sim", "zed"}) || p.Private() != 1 {
		t.Errorf("names = %v, private %d", got, p.Private())
	}
	problems := strings.Join(p.Problems(), "\n")
	for _, want := range []string{`zed/character.json: "bob" is not a Discord user ID`,
		"typo/character.json: unexpected end", `nobody/character.json: no user in "users"`} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems have no %q:\n%s", want, problems)
		}
	}
	if got := p.NamesFor("1"); slices.Contains(got, "zed") {
		t.Errorf("names for another user = %v", got)
	}
	if got := p.NamesFor("5"); !slices.Contains(got, "zed") {
		t.Errorf("names for the owner = %v", got)
	}
	if p.CanUse("zed", "1") || !p.CanUse("zed", "5") || !p.CanUse("ann", "1") {
		t.Error("CanUse")
	}
	cast := p.assign([]Message{{Speaker: "1", Character: "zed"}, {Speaker: "5", Character: "zed"}})
	if cast["1"].char.Name == "zed" || cast["5"].char.Name != "zed" {
		t.Errorf("cast: 1 is %s, 5 is %s", cast["1"].char.Name, cast["5"].char.Name)
	}
	// Comics never give it at random, also when the public ones are used.
	var msgs []Message
	for i := range 20 {
		msgs = append(msgs, Message{Speaker: strconv.Itoa(100 + i)})
	}
	for id, c := range p.assign(msgs) {
		if c.char.Name == "zed" || p.CharacterFor(id) == "zed" {
			t.Errorf("speaker %s got zed", id)
		}
	}
	if _, err := p.SheetPNG("1"); err != nil {
		t.Fatal(err)
	}

	// Only private characters: no pack.
	only := t.TempDir()
	writeCustom(t, only, "characters/zed/neutral.png", body)
	writeFile(t, only, "characters/zed/character.json", `{"users": ["5"]}`)
	if _, err := LoadPack(only); err == nil || !strings.Contains(err.Error(), "private") {
		t.Errorf("err = %v", err)
	}
}
