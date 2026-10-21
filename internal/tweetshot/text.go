package tweetshot

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"sync"
	"unicode"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/text/unicode/norm"
)

var (
	//go:embed assets/NotoSans-Regular.ttf
	regularTTF []byte
	//go:embed assets/NotoSans-Bold.ttf
	boldTTF []byte
	//go:embed assets/twemoji-72x72.zip
	emojiZip []byte
)

var fonts = sync.OnceValues(func() ([2]*opentype.Font, error) {
	var out [2]*opentype.Font
	for i, b := range [][]byte{regularTTF, boldTTF} {
		f, err := opentype.Parse(b)
		if err != nil {
			return out, fmt.Errorf("parse font: %w", err)
		}
		out[i] = f
	}
	return out, nil
})

// style is how a piece of text looks. size is in logical pixels.
type style struct {
	size  float64
	bold  bool
	color color.Color
}

// faces makes and keeps the sized font faces of one render. An opentype
// face is not safe for concurrent use, so each render has its own.
type faces struct {
	scale float64
	fonts [2]*opentype.Font
	cache map[faceKey]font.Face
}

type faceKey struct {
	size float64
	bold bool
}

func newFaces(scale float64) (*faces, error) {
	f, err := fonts()
	if err != nil {
		return nil, err
	}
	return &faces{scale: scale, fonts: f, cache: map[faceKey]font.Face{}}, nil
}

func (fc *faces) face(st style) font.Face {
	k := faceKey{st.size, st.bold}
	if f, ok := fc.cache[k]; ok {
		return f
	}
	i := 0
	if st.bold {
		i = 1
	}
	f, err := opentype.NewFace(fc.fonts[i], &opentype.FaceOptions{Size: st.size * fc.scale, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		panic(err) // only for a bad size, which the code never gives
	}
	fc.cache[k] = f
	return f
}

func (fc *faces) close() {
	for _, f := range fc.cache {
		f.Close()
	}
}

// emojiPx is the emoji size in device pixels for a text style.
func (fc *faces) emojiPx(st style) int { return int(st.size * fc.scale * 1.15) }

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
			rc.Close()
		}
	}
	e.cache[key] = img
	return img
}

// item is one piece of a line: text in one style, or one emoji.
type item struct {
	text  string
	emoji string // the emoji key; text is then the emoji's runes
	st    style
	w     fixed.Int26_6
}

// word is the items between two spaces. It does not break, unless it is
// longer than a full line.
type word struct {
	items []item
	w     fixed.Int26_6
	space bool // a space, not a word
	br    bool // a line break
}

// styledText is text in one style.
type styledText struct {
	text string
	st   style
}

// words splits text into words, spaces and line breaks, and measures them.
func (fc *faces) words(parts []styledText) []word {
	var out []word
	var cur word
	flush := func() {
		if len(cur.items) > 0 {
			out = append(out, cur)
		}
		cur = word{}
	}
	addText := func(s string, st style) {
		if s == "" {
			return
		}
		w := font.MeasureString(fc.face(st), s)
		if n := len(cur.items); n > 0 && cur.items[n-1].emoji == "" && cur.items[n-1].st == st {
			cur.items[n-1].text += s
			cur.items[n-1].w = font.MeasureString(fc.face(st), cur.items[n-1].text)
		} else {
			cur.items = append(cur.items, item{text: s, st: st, w: w})
		}
		cur.w = sumWidth(cur.items)
	}
	for _, p := range parts {
		rs := []rune(norm.NFC.String(p.text))
		var run []rune
		for i := 0; i < len(rs); {
			r := rs[i]
			switch {
			case r == '\n':
				addText(string(run), p.st)
				run = run[:0]
				flush()
				out = append(out, word{br: true})
				i++
				continue
			case unicode.IsSpace(r):
				addText(string(run), p.st)
				run = run[:0]
				flush()
				sw := font.MeasureString(fc.face(p.st), " ")
				out = append(out, word{space: true, w: sw, items: []item{{text: " ", st: p.st, w: sw}}})
				i++
				continue
			}
			if n, key := emojis.match(rs[i:]); n > 0 {
				addText(string(run), p.st)
				run = run[:0]
				w := fixed.I(fc.emojiPx(p.st) + int(p.st.size*fc.scale*0.1))
				cur.items = append(cur.items, item{text: string(rs[i : i+n]), emoji: key, st: p.st, w: w})
				cur.w = sumWidth(cur.items)
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
		addText(string(run), p.st)
	}
	flush()
	return out
}

func sumWidth(items []item) fixed.Int26_6 {
	var w fixed.Int26_6
	for _, it := range items {
		w += it.w
	}
	return w
}

// line is one line of laid-out text.
type line struct {
	items []item
	w     fixed.Int26_6
}

// wrap breaks words into lines at most maxW device pixels wide.
func (fc *faces) wrap(ws []word, maxW int) []line {
	limit := fixed.I(maxW)
	var lines []line
	var cur line
	var pending *word // a space, added only if a word follows on the line
	push := func() {
		lines = append(lines, cur)
		cur = line{}
		pending = nil
	}
	for i := range ws {
		w := &ws[i]
		switch {
		case w.br:
			push()
			continue
		case w.space:
			if len(cur.items) > 0 {
				pending = w
			}
			continue
		}
		gap := fixed.Int26_6(0)
		if pending != nil {
			gap = pending.w
		}
		if len(cur.items) > 0 && cur.w+gap+w.w > limit {
			push()
			gap = 0
		}
		if len(cur.items) == 0 && w.w > limit {
			// A word longer than a line: break it between characters.
			for _, part := range fc.splitWord(w.items, limit) {
				if len(cur.items) > 0 {
					push()
				}
				cur.items, cur.w = part, sumWidth(part)
			}
			continue
		}
		if pending != nil && len(cur.items) > 0 {
			cur.items = append(cur.items, pending.items...)
			cur.w += pending.w
		}
		pending = nil
		cur.items = append(cur.items, w.items...)
		cur.w += w.w
	}
	if len(cur.items) > 0 || len(lines) == 0 {
		lines = append(lines, cur)
	}
	return lines
}

// splitWord breaks the items of one long word into pieces at most limit
// wide.
func (fc *faces) splitWord(items []item, limit fixed.Int26_6) [][]item {
	var out [][]item
	var cur []item
	var w fixed.Int26_6
	add := func(it item) {
		if len(cur) > 0 && w+it.w > limit {
			out = append(out, cur)
			cur, w = nil, 0
		}
		cur = append(cur, it)
		w += it.w
	}
	for _, it := range items {
		if it.emoji != "" {
			add(it)
			continue
		}
		face := fc.face(it.st)
		for _, r := range it.text {
			s := string(r)
			add(item{text: s, st: it.st, w: font.MeasureString(face, s)})
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// ellipsize returns the items of a one-line text, cut with "…" to at most
// maxW device pixels.
func (fc *faces) ellipsize(parts []styledText, maxW int) []item {
	var items []item
	for _, w := range fc.words(parts) {
		if w.br {
			break
		}
		items = append(items, w.items...)
	}
	limit := fixed.I(maxW)
	if sumWidth(items) <= limit || len(items) == 0 {
		return items
	}
	st := items[0].st
	dots := item{text: "…", st: st, w: font.MeasureString(fc.face(st), "…")}
	var out []item
	var w fixed.Int26_6
	for _, it := range fc.splitWord(items, fixed.I(1<<20))[0] {
		if w+it.w+dots.w > limit {
			break
		}
		out = append(out, it)
		w += it.w
	}
	return append(out, dots)
}

// drawItems draws items on one line, from x, with the baseline at y (device
// pixels). It returns the x after the last item.
func (fc *faces) drawItems(dst draw.Image, items []item, x, y int) int {
	dot := fixed.I(x)
	for _, it := range items {
		if it.emoji != "" {
			if img := emojis.image(it.emoji); img != nil {
				px := fc.emojiPx(it.st)
				gap := int(it.st.size * fc.scale * 0.05)
				ex := dot.Round() + gap
				ey := y - int(float64(px)*0.84)
				xdraw.CatmullRom.Scale(dst, image.Rect(ex, ey, ex+px, ey+px), img, img.Bounds(), xdraw.Over, nil)
			}
			dot += it.w
			continue
		}
		d := font.Drawer{Dst: dst, Src: image.NewUniform(it.st.color), Face: fc.face(it.st), Dot: fixed.Point26_6{X: dot, Y: fixed.I(y)}}
		d.DrawString(it.text)
		dot += it.w
	}
	return dot.Round()
}
