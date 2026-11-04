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

	"github.com/binpas/hcbot/internal/richtext"
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
	if p.byName["ann"] == nil || p.byName["gone"] != nil {
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
	if got := p.CharacterFor("u3"); p.byName[got] == nil {
		t.Errorf("CharacterFor = %q", got)
	}
}

func TestSplit(t *testing.T) {
	m := func(s, text string) Message { return Message{Speaker: s, Text: text} }
	in := []Message{m("a", "hi"), m("b", "yo"), m("a", "x"), m("a", "y"), m("b", strings.Repeat("long ", 30)),
		m("c", "z"), {Speaker: "d", Image: image.NewRGBA(image.Rect(0, 0, 1, 1))}, m("e", "w")}
	pairs := 0
	for seed := range uint64(20) {
		var got []Message
		for _, pn := range split(in, rand.New(rand.NewPCG(seed, 1))) {
			got = append(got, pn...)
			if len(pn) == 2 {
				pairs++
				if !canPair(pn[0], pn[1]) {
					t.Errorf("seed %d: %s and %s share a panel", seed, pn[0].Speaker, pn[1].Speaker)
				}
			}
			if len(pn) < 1 || len(pn) > 2 {
				t.Errorf("seed %d: a panel with %d messages", seed, len(pn))
			}
		}
		if len(got) != len(in) {
			t.Fatalf("seed %d: %d messages, want %d", seed, len(got), len(in))
		}
		for i := range got {
			if got[i].Speaker != in[i].Speaker || got[i].Text != in[i].Text {
				t.Fatalf("seed %d: the order changed", seed)
			}
		}
	}
	if pairs == 0 {
		t.Error("no panel with two speakers")
	}
	if canPair(m("a", "x"), m("a", "y")) || canPair(m("a", "x"), m("b", strings.Repeat("y", pairChars))) {
		t.Error("canPair is wrong")
	}
}

func TestStage(t *testing.T) {
	m := func(s string) Message { return Message{Speaker: s} }
	panels := [][]Message{{m("a")}, {m("b")}, {m("b")}, {m("c"), m("d")}, {m("e")}, {m("f")}, {m("f")}}
	var turned, again, silent, single int
	from := map[[2]int]bool{} // {panel, neighbor panel} that gave a silent actor
	for seed := range uint64(2000) {
		acts := stage(panels, rand.New(rand.NewPCG(seed, 1)))
		var prev actor
		for i, as := range acts {
			var sp []actor
			for _, a := range as {
				if a.msg != nil {
					sp = append(sp, a)
				}
			}
			if len(as) == 2 && (as[0].left || !as[1].left) {
				t.Fatalf("seed %d panel %d: the two do not face each other: %+v", seed, i, as)
			}
			if len(sp) != len(panels[i]) {
				t.Fatalf("seed %d panel %d: %d speakers", seed, i, len(sp))
			}
			if i == 0 && sp[0].left {
				t.Fatalf("seed %d: the first panel looks left", seed)
			}
			if i > 0 && len(panels[i]) == 1 {
				if sp[0].speaker == prev.speaker {
					again++
					if sp[0].left != prev.left {
						turned++
					}
				} else if sp[0].left == prev.left {
					t.Fatalf("seed %d panel %d: a new speaker looks the same way", seed, i)
				}
			}
			if len(panels[i]) == 1 {
				single++
				if len(as) == 2 {
					silent++
					s := as[0]
					if as[0].msg != nil {
						s = as[1]
					}
					if s.speaker == sp[0].speaker {
						t.Fatalf("seed %d panel %d: the speaker is also silent", seed, i)
					}
					near := false
					for _, j := range []int{i - 1, i + 1} {
						if j >= 0 && j < len(panels) && slices.ContainsFunc(panels[j], func(n Message) bool { return n.Speaker == s.speaker }) {
							near = true
							from[[2]int{i, j}] = true
						}
					}
					if !near {
						t.Fatalf("seed %d panel %d: silent %s is not from a neighbor panel", seed, i, s.speaker)
					}
				}
			}
			prev = sp[len(sp)-1]
		}
	}
	if got := float64(turned) / float64(again); got < 0.25 || got > 0.35 {
		t.Errorf("a speaker who talks again turned %.2f of the time, want about %.2f", got, turnChance)
	}
	// 5 of the 6 one-speaker panels have someone to show (not panel 6), one time in two.
	if got := float64(silent) / float64(single); got < 0.38 || got > 0.45 {
		t.Errorf("silent actors in %.2f of the one-speaker panels", got)
	}
	// Every panel with a neighbor, in both directions: not only the first panels.
	for _, want := range [][2]int{{0, 1}, {1, 0}, {2, 3}, {4, 3}, {4, 5}, {5, 4}} {
		if !from[want] {
			t.Errorf("panel %d never showed someone from panel %d", want[0], want[1])
		}
	}
	if from[[2]int{6, 5}] {
		t.Error("the last panel showed its own speaker")
	}
}

func TestSilentActorHasNoBalloon(t *testing.T) {
	p := loadTestPack(t)
	f, err := fonts()
	if err != nil {
		t.Fatal(err)
	}
	fc := richtext.NewFaces(f, 1)
	defer fc.Close()
	msgs := []Message{{Speaker: "1", Text: "hi"}, {Speaker: "2", Text: "yo"}}
	people := p.assign(msgs)
	rect := image.Rect(0, 0, cell, cell)
	white := func(as []actor) int {
		dst := image.NewRGBA(rect)
		r := &renderer{dst: dst, fc: fc, rng: rand.New(rand.NewPCG(1, 2))}
		r.panel(rect, as, people)
		n := 0
		for y := range cell/2 - 10 { // above the characters
			for x := range cell {
				if dst.RGBAAt(x, y) == colWhite {
					n++
				}
			}
		}
		return n
	}
	if n := white([]actor{{speaker: "1"}, {speaker: "2", left: true}}); n != 0 {
		t.Errorf("silent actors drew a balloon (%d white pixels)", n)
	}
	if white([]actor{{speaker: "1", msg: &msgs[0]}, {speaker: "2", left: true}}) == 0 {
		t.Error("the speaker has no balloon")
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
	// Whole cells.
	if b := img.Bounds(); (b.Dx()-2*margin+gap)%(cell+gap) != 0 || (b.Dy()-2*margin+gap)%(cell+gap) != 0 {
		t.Errorf("size = %v", b)
	}
	again, _ := p.Render(msgs, Options{Background: "plain", Seed: 3})
	if string(again.Pix) != string(img.Pix) {
		t.Error("the same seed gave a different comic")
	}
	one, err := p.Render(msgs[:1], Options{Background: "nothing"})
	if _, _, size := layout(1); err != nil || one.Bounds().Size() != size {
		t.Errorf("one message: %v, %v", one.Bounds(), err)
	}
}

func TestLayout(t *testing.T) {
	// comicgen's chat comics: columns and rows of cells, and the free
	// cells for the cast at the start.
	for n, want := range map[int][3]int{1: {2, 1, 1}, 2: {3, 1, 1}, 3: {2, 2, 1}, 4: {3, 2, 2}, 5: {3, 2, 1},
		6: {4, 2, 2}, 7: {3, 3, 2}, 8: {3, 3, 1}, 9: {3, 4, 3}, 10: {4, 3, 2}, 12: {4, 4, 4}} {
		castRect, panels, size := layout(n)
		cols, rows := want[0], want[1]
		if wantSize := image.Pt(2*margin+cols*(cell+gap)-gap, 2*margin+rows*(cell+gap)-gap); size != wantSize {
			t.Errorf("n=%d: size = %v, want %v", n, size, wantSize)
		}
		if free := (castRect.Dx() + gap) / (cell + gap); free != want[2] || castRect.Min != image.Pt(margin, margin) {
			t.Errorf("n=%d: cast = %v (%d cells), want %d", n, castRect, free, want[2])
		}
		if len(panels) != n || panels[n-1].Max != size.Sub(image.Pt(margin, margin)) {
			t.Errorf("n=%d: panels = %v", n, panels)
		}
	}
}

func TestPortraitAndSheet(t *testing.T) {
	p := loadTestPack(t)
	if _, err := p.PortraitPNG("nobody"); !errors.Is(err, ErrUnknownCharacter) {
		t.Errorf("unknown character: %v", err)
	}
	for _, f := range []func() ([]byte, error){func() ([]byte, error) { return p.PortraitPNG("sim") }, func() ([]byte, error) { return p.SheetPNG("") }} {
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
	// Only a picture, no text: it fills the balloon area (the top half of
	// the panel), and keeps its shape (2:1).
	img, err := p.Render([]Message{{Speaker: "1", Name: "one", Image: pic}}, Options{Background: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	maxH := cell/2 - 18 - 2*balloonPad
	if n := count(img, red); n < 2*maxH*maxH*9/10 || n > 2*maxH*maxH {
		t.Errorf("red pixels = %d, want about %d", n, 2*maxH*maxH)
	}
	if got := pictureSize(Message{Image: pic}, 500, 80); got != image.Pt(160, 80) {
		t.Errorf("size = %v", got)
	}
	if got := pictureSize(Message{Image: image.NewRGBA(image.Rect(0, 0, 20, 10))}, 500, 80); got != image.Pt(20, 10) {
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
