package comic

import (
	"errors"
	"image"
	"image/color"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

func testPack(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := WriteTestPack(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func loadTestPack(t *testing.T) *Pack {
	t.Helper()
	p, err := LoadPack(testPack(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadPack(t *testing.T) {
	p := loadTestPack(t)
	if got := p.Names(); !slices.Equal(got, []string{"bob", "ann", "sim"}) {
		t.Errorf("names = %v", got)
	}
	if got := p.Backgrounds(); !slices.Equal(got, []string{"plain"}) {
		t.Errorf("backgrounds = %v", got)
	}
	if !p.Has("ann") || p.Has("gone") {
		t.Error("Has is wrong")
	}
	if _, err := LoadPack(t.TempDir()); err == nil {
		t.Error("an empty folder loaded")
	}
}

func TestMood(t *testing.T) {
	for _, c := range []struct {
		text    string
		face    float64
		gesture float64
		neutral bool
	}{
		{text: "the weather is fine", neutral: true},
		{text: "lol that is great", face: emLaugh},
		{text: "haha 😂", face: emLaugh},
		{text: "WHY WAS I NOT INFORMED", face: emShout},
		{text: "that is sad :(", face: emSad},
		{text: "hello there", neutral: true, gesture: gestWave},
		{text: "are you sure", neutral: true, gesture: gestPointOther},
		{text: "I think so", neutral: true, gesture: gestPointSelf},
		{text: "idk 🤷", neutral: true, gesture: gestShrug},
		{text: "skilled at bowling", neutral: true}, // "lol" inside a word
		{text: "this is madness", neutral: true},    // "mad" inside a word
		{text: "I", neutral: true, gesture: gestPointSelf},
	} {
		m := moodOf(c.text)
		if c.neutral != (m.intensity == 0) || (!c.neutral && m.face != c.face) || m.gesture != c.gesture {
			t.Errorf("moodOf(%q) = %+v", c.text, m)
		}
	}
	if moodOf("lol").intensity >= moodOf("lol!").intensity {
		t.Error("! does not make the emotion stronger")
	}
}

func TestFigure(t *testing.T) {
	p := loadTestPack(t)
	rng := rand.New(rand.NewPCG(1, 1))
	pixel := func(f figure, x, y int) color.NRGBA { return f.img.NRGBAAt(x, y) }

	// Complex: the face joins the torso at the connection points (face
	// 30×30 with its point at 15,28; torso point at 25,2).
	f := p.byName["ann"].figure(moodOf("lol"), rng)
	if b := f.img.Bounds(); b.Dx() != 50 || b.Dy() != 26+90 {
		t.Errorf("figure size = %v", b)
	}
	if got := pixel(f, 25, 5); got.R != 20 {
		t.Errorf("face = %v, want the laugh face", got)
	}
	if got := pixel(f, 25, 100); got.G != 10 {
		t.Errorf("torso = %v, want the neutral torso", got)
	}
	if f.headX != 25 || f.headY != 15 {
		t.Errorf("head = %d,%d", f.headX, f.headY)
	}
	if got := pixel(p.byName["ann"].figure(moodOf("hi :("), rng), 25, 100); got.G != 20 {
		t.Errorf("torso = %v, want the wave torso", got)
	}
	if got := pixel(p.byName["ann"].figure(moodOf("hi :("), rng), 25, 5); got.R != 30 {
		t.Errorf("face = %v, want the sad face", got)
	}
	// Simple: the nearest body.
	if got := pixel(p.byName["sim"].figure(moodOf("lmao"), rng), 5, 5); got.B != 20 {
		t.Errorf("body = %v, want the laugh body", got)
	}
	if got := pixel(p.byName["sim"].figure(moodOf("hm"), rng), 5, 5); got.B != 10 {
		t.Errorf("body = %v, want the neutral body", got)
	}
}

func TestAssign(t *testing.T) {
	p := loadTestPack(t)
	msgs := []Message{
		{Speaker: "u1", Character: "ann"},
		{Speaker: "u2", Character: "ann"}, // the same choice: both keep it
		{Speaker: "u3"},
		{Speaker: "u4"},
		{Speaker: "u5"}, // more people than characters
	}
	people := p.assign(msgs)
	if people["u1"].char.Name != "ann" || people["u2"].char.Name != "ann" {
		t.Errorf("chosen characters changed: %s, %s", people["u1"].char.Name, people["u2"].char.Name)
	}
	if a, b := people["u3"].char.Name, people["u4"].char.Name; a == b || a == "ann" || b == "ann" {
		t.Errorf("picked characters = %s, %s", a, b)
	}
	if people["u5"].char == nil {
		t.Error("no character when all are used")
	}
	if again := p.assign(msgs); again["u3"].char != people["u3"].char {
		t.Error("the pick is not stable")
	}
	if got := p.CharacterFor("u3"); !p.Has(got) {
		t.Errorf("CharacterFor = %q", got)
	}
}

func TestSplit(t *testing.T) {
	m := func(s string) Message { return Message{Speaker: s} }
	in := []Message{m("a"), m("b"), m("a"), m("a"), m("b"), m("c")}
	shape := func() []string {
		var out []string
		for _, pn := range split(in) {
			var ids []string
			for _, x := range pn {
				ids = append(ids, x.Speaker)
			}
			out = append(out, strings.Join(ids, ""))
		}
		return out
	}
	if got, want := shape(), []string{"a", "b", "a", "a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("one per panel = %v, want %v", got, want)
	}
	defer func(n int) { perPanel = n }(perPanel)
	perPanel = 2
	if got, want := shape(), []string{"ab", "a", "ab", "c"}; !slices.Equal(got, want) {
		t.Errorf("two per panel = %v, want %v", got, want)
	}
}

func TestRender(t *testing.T) {
	p := loadTestPack(t)
	if _, err := p.Render(nil, Options{}); !errors.Is(err, ErrNoMessages) {
		t.Errorf("no messages: %v", err)
	}
	long := strings.Repeat("a very long message that goes on ", 30)
	msgs := []Message{
		{Speaker: "1", Name: "one", Text: "hi you"},
		{Speaker: "2", Name: "two", Text: long, Avatar: image.NewRGBA(image.Rect(0, 0, 8, 8))},
		{Speaker: "1", Name: "one", Text: "😂😂😂"},
		{Speaker: "3", Name: strings.Repeat("name", 30), Text: "x"},
		{Speaker: "3", Name: "three", Text: "y"},
	}
	img, err := p.Render(msgs, Options{Background: "plain", Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	// The cast panel and 3 panels: two rows of two.
	if b := img.Bounds(); b.Dx() != imageW() || b.Dy() != 2*margin+2*panelH+gap {
		t.Errorf("size = %v", b)
	}
	again, _ := p.Render(msgs, Options{Background: "plain", Seed: 3})
	if string(again.Pix) != string(img.Pix) {
		t.Error("the same seed gave a different comic")
	}
	one, err := p.Render(msgs[:1], Options{Background: "nothing"})
	if err != nil || one.Bounds().Dy() != 2*margin+panelH {
		t.Errorf("one message: %v, %v", one.Bounds(), err)
	}
}

func TestLayout(t *testing.T) {
	for n, want := range map[int][]int{1: {1}, 2: {2}, 3: {3}, 4: {2, 2}, 5: {3, 2}, 6: {3, 3},
		7: {3, 2, 2}, 8: {3, 3, 2}, 9: {3, 3, 3}, 10: {3, 3, 2, 2}, 11: {3, 3, 3, 2}} {
		if got := rows(n); !slices.Equal(got, want) {
			t.Errorf("rows(%d) = %v, want %v", n, got, want)
		}
		// Each row fills the width.
		rects := panelRects(n)
		if len(rects) != n {
			t.Fatalf("panelRects(%d) = %d rects", n, len(rects))
		}
		for i, r := range rects {
			last := i == n-1 || rects[i+1].Min.Y != r.Min.Y
			if r.Min.X == margin && r.Dx() < panelW {
				t.Errorf("n=%d: panel %d is %d wide", n, i, r.Dx())
			}
			if last && r.Max.X != imageW()-margin {
				t.Errorf("n=%d: row of panel %d ends at %d", n, i, r.Max.X)
			}
		}
	}
}

func TestPortraitAndSheet(t *testing.T) {
	p := loadTestPack(t)
	if _, err := p.PortraitPNG("nobody"); !errors.Is(err, ErrUnknownCharacter) {
		t.Errorf("unknown character: %v", err)
	}
	for _, f := range []func() ([]byte, error){func() ([]byte, error) { return p.PortraitPNG("sim") }, p.SheetPNG} {
		data, err := f()
		if err != nil || len(data) < 8 || string(data[1:4]) != "PNG" {
			t.Errorf("png = %d bytes, %v", len(data), err)
		}
	}
}

func TestAngleDist(t *testing.T) {
	if d := angleDist(emLaugh, emHappy); math.Abs(d-math.Pi/4) > 1e-9 {
		t.Errorf("laugh to happy = %v", d)
	}
}

// count returns how many pixels of img have the color c.
func count(img *image.RGBA, c color.RGBA) int {
	n := 0
	for i := 0; i+3 < len(img.Pix); i += 4 {
		if img.Pix[i] == c.R && img.Pix[i+1] == c.G && img.Pix[i+2] == c.B {
			n++
		}
	}
	return n
}

func TestPictures(t *testing.T) {
	p := loadTestPack(t)
	red := color.RGBA{0xf0, 0x10, 0x10, 0xff}
	pic := image.NewRGBA(image.Rect(0, 0, 600, 300))
	for i := 0; i < len(pic.Pix); i += 4 {
		copy(pic.Pix[i:], []uint8{red.R, red.G, red.B, 0xff})
	}
	// Only a picture, no text: it fits in picH × the balloon width, and
	// keeps its shape (2:1).
	img, err := p.Render([]Message{{Speaker: "1", Name: "one", Image: pic}}, Options{Background: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(img, red); n < 2*picH*picH*9/10 || n > 2*picH*picH {
		t.Errorf("red pixels = %d, want about %d", n, 2*picH*picH)
	}
	if got := pictureSize(Message{Image: pic}, 500, picH); got != image.Pt(2*picH, picH) {
		t.Errorf("size = %v", got)
	}
	if got := pictureSize(Message{Image: image.NewRGBA(image.Rect(0, 0, 20, 10))}, 500, picH); got != image.Pt(20, 10) {
		t.Errorf("small picture = %v, want it not enlarged", got)
	}

	// A spoiler: the dark box, not the picture.
	img, err = p.Render([]Message{{Speaker: "1", Name: "one", Text: "look", Image: pic, Spoiler: true},
		{Speaker: "2", Name: "two", Text: "wow"}}, Options{Background: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if count(img, red) != 0 || count(img, colDark) < 100*40 {
		t.Errorf("spoiler: red %d, dark %d", count(img, red), count(img, colDark))
	}
}
