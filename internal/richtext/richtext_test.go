package richtext

import (
	"image/color"
	"strings"
	"testing"

	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/math/fixed"
)

func TestEmojiMatch(t *testing.T) {
	for in, want := range map[string]int{
		"👍🏽 ok":        2, // with a skin tone
		"👨‍👩‍👧 family": 5, // ZWJ sequence
		"🇺🇸!":          2, // flag
		"❤️x":          2, // with U+FE0F
		"❤x":           1,
		"1️⃣ one":      3, // keycap
		"©️":           2,
		"© 2026":       0, // text, not an emoji, without U+FE0F
		"1 one":        0,
		"a":            0,
		"中":            0,
	} {
		if n, _ := emojis.match([]rune(in)); n != want {
			t.Errorf("match(%q) = %d runes, want %d", in, n, want)
		}
	}
	if _, key := emojis.match([]rune("🎉")); emojis.image(key) == nil {
		t.Error("no image for 🎉")
	}
}

func TestWrap(t *testing.T) {
	f, err := ParseFonts(goregular.TTF, gobold.TTF)
	if err != nil {
		t.Fatal(err)
	}
	fc := NewFaces(f, 2)
	defer fc.Close()
	st := Style{Size: 17, Color: color.White}
	lines := fc.Wrap(fc.Words([]Span{{"aaaa bbbb cccc\n\ndddd " + strings.Repeat("x", 120), st}}), 300)
	if len(lines) < 5 {
		t.Fatalf("lines = %d", len(lines))
	}
	for i, ln := range lines {
		if ln.W > fixed.I(300) {
			t.Errorf("line %d is %v wide", i, ln.W)
		}
		if len(ln.Items) > 0 && ln.Items[0].Text == " " {
			t.Errorf("line %d starts with a space", i)
		}
	}
	// The blank line between the two paragraphs stays.
	var blank bool
	for _, ln := range lines {
		blank = blank || len(ln.Items) == 0
	}
	if !blank {
		t.Error("blank line lost")
	}
	cut := fc.Ellipsize([]Span{{strings.Repeat("long name ", 20), st}}, 200)
	if SumWidth(cut) > fixed.I(200) || cut[len(cut)-1].Text != "…" {
		t.Errorf("ellipsize = %+v", cut)
	}
}
