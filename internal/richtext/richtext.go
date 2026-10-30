// Package richtext lays out and draws text with color emoji (Twemoji): it
// measures words, wraps them into lines, cuts a line with "…", and draws
// the lines. The caller gives the main fonts. A character that the main
// font does not have comes from the first fallback font that has it
// (Noto Sans, Math, Symbols, Symbols 2, and CJK).
package richtext

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/png" // the Twemoji images
	"strings"
	"sync"
	"unicode"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/text/unicode/norm"
)

//go:embed assets/twemoji-72x72.zip
var emojiZip []byte

// Noto Sans (SIL Open Font License). Tweetshot uses it as its main font.
var (
	//go:embed assets/NotoSans-Regular.ttf
	NotoSansRegular []byte
	//go:embed assets/NotoSans-Bold.ttf
	NotoSansBold []byte
)

// The fallback fonts (SIL Open Font License), in order. The CJK font is a
// subset of Noto Sans CJK JP with only the CJK, kana, and Hangul blocks.
var (
	//go:embed assets/NotoSansMath-Regular.ttf
	mathTTF []byte
	//go:embed assets/NotoSansSymbols-Regular.ttf
	symbolsTTF []byte
	//go:embed assets/NotoSansSymbols2-Regular.ttf
	symbols2TTF []byte
	//go:embed assets/NotoSansCJKjp-Regular-subset.otf
	cjkOTF []byte
)

var fallbacks = sync.OnceValue(func() []*opentype.Font {
	var out []*opentype.Font
	for _, b := range [][]byte{NotoSansRegular, mathTTF, symbolsTTF, symbols2TTF, cjkOTF} {
		if f, err := opentype.Parse(b); err == nil {
			out = append(out, f)
		}
	}
	return out
})

// Fonts is a regular and a bold font.
type Fonts [2]*opentype.Font

// ParseFonts reads a regular and a bold TrueType or OpenType font.
func ParseFonts(regular, bold []byte) (Fonts, error) {
	var out Fonts
	for i, b := range [][]byte{regular, bold} {
		f, err := opentype.Parse(b)
		if err != nil {
			return out, fmt.Errorf("parse font: %w", err)
		}
		out[i] = f
	}
	return out, nil
}

// Style is how a piece of text looks. Size is in logical pixels.
// Thicken draws the text again that many logical pixels to the right, so
// thin letters look a little bolder (0: no).
type Style struct {
	Size    float64
	Bold    bool
	Color   color.Color
	Thicken float64
}

// Faces makes and keeps the sized font faces of one render. An opentype
// face is not safe for concurrent use, so each render has its own.
type Faces struct {
	scale float64
	fonts Fonts
	fall  []*opentype.Font
	cache map[faceKey]font.Face
	buf   sfnt.Buffer
}

type faceKey struct {
	size float64
	bold bool
	font int // 0: the main font; n: fallback n-1
}

// NewFaces makes the faces for one render. scale is device pixels for
// each logical pixel.
func NewFaces(fonts Fonts, scale float64) *Faces {
	return &Faces{scale: scale, fonts: fonts, fall: fallbacks(), cache: map[faceKey]font.Face{}}
}

// Face returns the main font face for a style.
func (fc *Faces) Face(st Style) font.Face { return fc.face(st, 0) }

// face returns the face of font n (0: the main font; n: fallback n-1). A
// fallback font has no bold; Draw thickens it.
func (fc *Faces) face(st Style, n int) font.Face {
	k := faceKey{st.Size, st.Bold && n == 0, n}
	if f, ok := fc.cache[k]; ok {
		return f
	}
	src := fc.fonts[0]
	switch {
	case n > 0:
		src = fc.fall[n-1]
	case st.Bold:
		src = fc.fonts[1]
	}
	f, err := opentype.NewFace(src, &opentype.FaceOptions{Size: st.Size * fc.scale, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		panic(err) // only for a bad size, which the code never gives
	}
	fc.cache[k] = f
	return f
}

// fontFor returns the font for a character: 0 (the main font) if it has
// the character or no font has it, else the first fallback that has it.
func (fc *Faces) fontFor(r rune, bold bool) int {
	main := fc.fonts[0]
	if bold {
		main = fc.fonts[1]
	}
	if fc.has(main, r) || unicode.IsControl(r) {
		return 0
	}
	for i, f := range fc.fall {
		if fc.has(f, r) {
			return i + 1
		}
	}
	return 0
}

func (fc *Faces) has(f *opentype.Font, r rune) bool {
	g, err := f.GlyphIndex(&fc.buf, r)
	return err == nil && g != 0
}

// Close frees the faces.
func (fc *Faces) Close() {
	for _, f := range fc.cache {
		_ = f.Close()
	}
}

// EmojiPx is the emoji size in device pixels for a text style.
func (fc *Faces) EmojiPx(st Style) int { return int(st.Size * fc.scale * 1.15) }

// Emoji images (Twemoji, 72×72 PNG). The file names are the code points
// in hex, joined with "-".
type emojiSet struct {
	once  sync.Once
	files map[string]*zip.File

	mu    sync.Mutex
	cache map[string]image.Image
}

var emojis emojiSet

const maxEmojiRunes = 10 // the longest ZWJ sequences, with modifiers

func (e *emojiSet) load() {
	e.once.Do(func() {
		e.files = map[string]*zip.File{}
		e.cache = map[string]image.Image{}
		zr, err := zip.NewReader(bytes.NewReader(emojiZip), int64(len(emojiZip)))
		if err != nil {
			return // no emojis; they draw as text
		}
		for _, f := range zr.File {
			e.files[strings.TrimSuffix(f.Name, ".png")] = f
		}
	})
}

// match reports how many runes at the start of rs make one emoji, and its
// file key. It returns 0 when rs does not start with an emoji.
func (e *emojiSet) match(rs []rune) (int, string) {
	if len(rs) == 0 || (rs[0] < 0x80 && !strings.ContainsRune("#*0123456789", rs[0])) {
		return 0, ""
	}
	e.load()
	for n := min(len(rs), maxEmojiRunes); n >= 1; n-- {
		if rs[n-1] == 0x200d {
			continue // a sequence does not end with a joiner
		}
		// ©, ®, digits, # and * are an emoji only with U+FE0F (or as a
		// keycap, which is longer).
		if n == 1 && rs[0] < 0x2000 && (len(rs) < 2 || rs[1] != 0xfe0f) {
			continue
		}
		if key, ok := e.lookup(rs[:n]); ok {
			if n < len(rs) && rs[n] == 0xfe0f {
				n++
			}
			return n, key
		}
	}
	return 0, ""
}

// lookup finds the file for a sequence. Twemoji keeps U+FE0F in some file
// names and not in others, so it tries both.
func (e *emojiSet) lookup(seq []rune) (string, bool) {
	var all, bare []string
	for _, r := range seq {
		h := fmt.Sprintf("%x", r)
		all = append(all, h)
		if r != 0xfe0f {
			bare = append(bare, h)
		}
	}
	for _, k := range []string{strings.Join(all, "-"), strings.Join(bare, "-")} {
		if _, ok := e.files[k]; ok && k != "" {
			return k, true
		}
	}
	return "", false
}

// image returns the emoji image for a key, or nil.
func (e *emojiSet) image(key string) image.Image {
	e.load()
	e.mu.Lock()
	defer e.mu.Unlock()
	if img, ok := e.cache[key]; ok {
		return img
	}
	var img image.Image
	if f := e.files[key]; f != nil {
		if rc, err := f.Open(); err == nil {
			img, _, _ = image.Decode(rc)
			_ = rc.Close()
		}
	}
	e.cache[key] = img
	return img
}

// Item is one piece of a line: text in one style, or one emoji.
type Item struct {
	Text  string
	Emoji string // the emoji key; Text is then the emoji's runes
	Style Style
	W     fixed.Int26_6 // the width in device pixels
	font  int           // 0: the main font; n: fallback n-1
}

// Word is the items between two spaces. It does not break, unless it is
// longer than a full line.
type Word struct {
	Items []Item
	W     fixed.Int26_6
	space bool // a space, not a word
	br    bool // a line break
}

// Span is text in one style.
type Span struct {
	Text  string
	Style Style
}

// Words splits text into words, spaces and line breaks, and measures them.
func (fc *Faces) Words(parts []Span) []Word {
	var out []Word
	var cur Word
	flush := func() {
		if len(cur.Items) > 0 {
			out = append(out, cur)
		}
		cur = Word{}
	}
	addRun := func(s string, st Style, f int) {
		if n := len(cur.Items); n > 0 && cur.Items[n-1].Emoji == "" && cur.Items[n-1].Style == st && cur.Items[n-1].font == f {
			cur.Items[n-1].Text += s
			cur.Items[n-1].W = font.MeasureString(fc.face(st, f), cur.Items[n-1].Text)
		} else {
			cur.Items = append(cur.Items, Item{Text: s, Style: st, W: font.MeasureString(fc.face(st, f), s), font: f})
		}
	}
	// addText adds text in runs of one font each.
	addText := func(s string, st Style) {
		if s == "" {
			return
		}
		start, f := 0, -1
		for i, r := range s {
			if g := fc.fontFor(r, st.Bold); g != f {
				if i > start {
					addRun(s[start:i], st, f)
				}
				start, f = i, g
			}
		}
		addRun(s[start:], st, f)
		cur.W = SumWidth(cur.Items)
	}
	for _, p := range parts {
		rs := []rune(norm.NFC.String(p.Text))
		var run []rune
		for i := 0; i < len(rs); {
			r := rs[i]
			switch {
			case r == '\n':
				addText(string(run), p.Style)
				run = run[:0]
				flush()
				out = append(out, Word{br: true})
				i++
				continue
			case unicode.IsSpace(r):
				addText(string(run), p.Style)
				run = run[:0]
				flush()
				sw := font.MeasureString(fc.Face(p.Style), " ")
				out = append(out, Word{space: true, W: sw, Items: []Item{{Text: " ", Style: p.Style, W: sw}}})
				i++
				continue
			}
			if n, key := emojis.match(rs[i:]); n > 0 {
				addText(string(run), p.Style)
				run = run[:0]
				w := fixed.I(fc.EmojiPx(p.Style) + int(p.Style.Size*fc.scale*0.1))
				cur.Items = append(cur.Items, Item{Text: string(rs[i : i+n]), Emoji: key, Style: p.Style, W: w})
				cur.W = SumWidth(cur.Items)
				i += n
				continue
			}
			if unicode.Is(unicode.Variation_Selector, r) || r == 0x200d {
				i++ // a lone selector or joiner draws as a box
				continue
			}
			run = append(run, r)
			i++
		}
		addText(string(run), p.Style)
	}
	flush()
	return out
}

// SumWidth is the width of items in device pixels.
func SumWidth(items []Item) fixed.Int26_6 {
	var w fixed.Int26_6
	for _, it := range items {
		w += it.W
	}
	return w
}

// Line is one line of laid-out text.
type Line struct {
	Items []Item
	W     fixed.Int26_6
}

// Wrap breaks words into lines at most maxW device pixels wide.
func (fc *Faces) Wrap(ws []Word, maxW int) []Line {
	limit := fixed.I(maxW)
	var lines []Line
	var cur Line
	var pending *Word // a space, added only if a word follows on the line
	push := func() {
		lines = append(lines, cur)
		cur = Line{}
		pending = nil
	}
	for i := range ws {
		w := &ws[i]
		switch {
		case w.br:
			push()
			continue
		case w.space:
			if len(cur.Items) > 0 {
				pending = w
			}
			continue
		}
		gap := fixed.Int26_6(0)
		if pending != nil {
			gap = pending.W
		}
		if len(cur.Items) > 0 && cur.W+gap+w.W > limit {
			push()
			gap = 0
		}
		if len(cur.Items) == 0 && w.W > limit {
			// A word longer than a line: break it between characters.
			for _, part := range fc.splitWord(w.Items, limit) {
				if len(cur.Items) > 0 {
					push()
				}
				cur.Items, cur.W = part, SumWidth(part)
			}
			continue
		}
		if pending != nil && len(cur.Items) > 0 {
			cur.Items = append(cur.Items, pending.Items...)
			cur.W += pending.W
		}
		pending = nil
		cur.Items = append(cur.Items, w.Items...)
		cur.W += w.W
	}
	if len(cur.Items) > 0 || len(lines) == 0 {
		lines = append(lines, cur)
	}
	return lines
}

// splitWord breaks the items of one long word into pieces at most limit
// wide.
func (fc *Faces) splitWord(items []Item, limit fixed.Int26_6) [][]Item {
	var out [][]Item
	var cur []Item
	var w fixed.Int26_6
	add := func(it Item) {
		if len(cur) > 0 && w+it.W > limit {
			out = append(out, cur)
			cur, w = nil, 0
		}
		cur = append(cur, it)
		w += it.W
	}
	for _, it := range items {
		if it.Emoji != "" {
			add(it)
			continue
		}
		face := fc.face(it.Style, it.font)
		for _, r := range it.Text {
			s := string(r)
			add(Item{Text: s, Style: it.Style, W: font.MeasureString(face, s), font: it.font})
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// Ellipsize returns the items of a one-line text, cut with "…" to at most
// maxW device pixels.
func (fc *Faces) Ellipsize(parts []Span, maxW int) []Item {
	var items []Item
	for _, w := range fc.Words(parts) {
		if w.br {
			break
		}
		items = append(items, w.Items...)
	}
	limit := fixed.I(maxW)
	if SumWidth(items) <= limit || len(items) == 0 {
		return items
	}
	st := items[0].Style
	dots := Item{Text: "…", Style: st, W: font.MeasureString(fc.Face(st), "…")}
	var out []Item
	var w fixed.Int26_6
	for _, it := range fc.splitWord(items, fixed.I(1<<20))[0] {
		if w+it.W+dots.W > limit {
			break
		}
		out = append(out, it)
		w += it.W
	}
	return append(out, dots)
}

// Draw draws items on one line, from x, with the baseline at y (device
// pixels). It returns the x after the last item.
func (fc *Faces) Draw(dst draw.Image, items []Item, x, y int) int {
	dot := fixed.I(x)
	for _, it := range items {
		if it.Emoji != "" {
			if img := emojis.image(it.Emoji); img != nil {
				px := fc.EmojiPx(it.Style)
				gap := int(it.Style.Size * fc.scale * 0.05)
				ex := dot.Round() + gap
				ey := y - int(float64(px)*0.84)
				xdraw.CatmullRom.Scale(dst, image.Rect(ex, ey, ex+px, ey+px), img, img.Bounds(), xdraw.Over, nil)
			}
			dot += it.W
			continue
		}
		d := font.Drawer{Dst: dst, Src: image.NewUniform(it.Style.Color), Face: fc.face(it.Style, it.font), Dot: fixed.Point26_6{X: dot, Y: fixed.I(y)}}
		d.DrawString(it.Text)
		thicken := it.Style.Thicken
		if it.Style.Bold && it.font > 0 {
			thicken = max(thicken, it.Style.Size/20) // a fallback font has no bold
		}
		if thicken > 0 {
			d.Dot = fixed.Point26_6{X: dot + fixed.Int26_6(thicken*fc.scale*64), Y: fixed.I(y)}
			d.DrawString(it.Text)
		}
		dot += it.W
	}
	return dot.Round()
}
