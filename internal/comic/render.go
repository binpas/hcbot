package comic

import (
	_ "embed"
	"errors"
	"hash/fnv"
	"image"
	"image/color"
	"image/draw"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"unicode/utf8"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/vector"

	"github.com/binpas/hcbot/internal/richtext"
)

var (
	//go:embed assets/LiberationSans-Regular.ttf
	regularTTF []byte
	//go:embed assets/LiberationSans-Bold.ttf
	boldTTF []byte
)

var fonts = sync.OnceValues(func() (richtext.Fonts, error) {
	return richtext.ParseFonts(regularTTF, boldTTF)
})

// Sizes in pixels. The comic follows comicgen (the library of the
// Septapus bot), whose comics read well when Discord shows them small in
// the chat: square cells, a cast in the free cells of the first row, and
// one or two speakers in each panel, with the balloons in the top half.
// They are variables so that a test can try other sizes.
var (
	cell       = 220 // a panel is cell × cell
	margin     = 5
	gap        = 5
	border     = 4
	charScale  = 0.27 // the art at zoom 1 (our figures are about twice as tall as comicgen's)
	avatarSize = 20   // the profile picture on each character
	areaPad    = 8    // the balloon area: the top half of the panel, inside this
	balloonPad = 6
	minFont    = 6.0 // text gets smaller until it fits, down to this
	pairChars  = 128 // two speakers share a panel only with shorter messages
	castRowH   = 30
)

var (
	colPage  = color.RGBA{0xff, 0xff, 0xff, 0xff}
	colInk   = color.RGBA{0x00, 0x00, 0x00, 0xff}
	colPanel = color.RGBA{0xfb, 0xf8, 0xee, 0xff} // when there is no background
	colWhite = color.RGBA{0xff, 0xff, 0xff, 0xff}
	colDark  = color.RGBA{0x20, 0x22, 0x25, 0xff} // the spoiler box

	// Liberation Sans has the sizes of Arial, the font of comicgen.
	stBalloon   = richtext.Style{Size: 14, Color: colInk}
	stCastTitle = richtext.Style{Size: 16, Color: colInk}
	stCastName  = richtext.Style{Size: 14, Color: colInk}
	stSpoiler   = richtext.Style{Size: 13, Bold: true, Color: colWhite}
)

// Message is one message of the comic.
type Message struct {
	Speaker   string      // a stable ID, for example the user ID
	Name      string      // the name in the cast panel
	Avatar    image.Image // the profile picture, or nil
	Character string      // the chosen character, or "" for one picked from Speaker
	Text      string
	Image     image.Image // a picture under the text in the balloon, or nil
	Spoiler   bool        // a "spoiler" box in place of the picture
}

// Options change a comic.
type Options struct {
	Background string // a background name, or "" for a random one
	Seed       uint64 // the same seed and messages give the same comic
}

// ErrNoMessages is the error for a comic with no messages.
var ErrNoMessages = errors.New("comic: no messages")

// PNG draws the comic and returns it as a PNG file.
func (p *Pack) PNG(msgs []Message, opt Options) ([]byte, error) {
	img, err := p.Render(msgs, opt)
	if err != nil {
		return nil, err
	}
	return encode(img)
}

// cast is one speaker of the comic, with their character.
type cast struct {
	id     string
	name   string
	avatar image.Image
	char   *Character
	order  int // the order of the first message
}

// Render draws the comic: the cast, then the panels.
func (p *Pack) Render(msgs []Message, opt Options) (*image.RGBA, error) {
	if len(msgs) == 0 {
		return nil, ErrNoMessages
	}
	f, err := fonts()
	if err != nil {
		return nil, err
	}
	fc := richtext.NewFaces(f, 1)
	defer fc.Close()
	rng := rand.New(rand.NewPCG(opt.Seed, 0x636f6d6963))

	people := p.assign(msgs)
	panels := split(msgs, rng)
	bg := p.background(opt.Background, rng)

	castRect, rects, size := layout(len(panels))
	dst := image.NewRGBA(image.Rectangle{Max: size})
	draw.Draw(dst, dst.Bounds(), image.NewUniform(colPage), image.Point{}, draw.Src)

	r := &renderer{dst: dst, fc: fc, rng: rng, bg: bg}
	r.castList(castRect, people)
	for i, rect := range rects {
		r.panel(rect, panels[i], people)
		r.frame(rect)
	}
	return dst, nil
}

// layout places the cast and n panels in a grid of cells, with comicgen's
// rule for the columns: 3 panels in rows of 2, 4 in rows of 3, and more in
// rows of 4, or of 3 when n is odd or n+1 is a multiple of 3. The cast
// fills the free cells at the start of the first row.
func layout(n int) (castRect image.Rectangle, panels []image.Rectangle, size image.Point) {
	total := n + 1
	cols := min(total, 3)
	if total <= 4 {
		cols = total
	}
	switch {
	case n == 3:
		cols = 2
	case n == 4:
		cols = 3
	case n > 4:
		cols = 4
		if n%2 == 1 || (n+1)%3 == 0 {
			cols--
		}
	}
	rows := (total + cols - 1) / cols
	free := cols*rows - n
	at := func(i int) image.Point {
		return image.Pt(margin+(i%cols)*(cell+gap), margin+(i/cols)*(cell+gap))
	}
	castRect = image.Rectangle{Min: at(0), Max: at(0).Add(image.Pt(free*cell+(free-1)*gap, cell))}
	for i := range n {
		pt := at(free + i)
		panels = append(panels, image.Rectangle{Min: pt, Max: pt.Add(image.Pt(cell, cell))})
	}
	size = image.Pt(2*margin+cols*(cell+gap)-gap, 2*margin+rows*(cell+gap)-gap)
	return castRect, panels, size
}

// assign gives each speaker a character: the chosen one, or one picked
// from a hash of the speaker ID. Two picked characters are never the
// same, and a picked one is not a chosen one, while there are free
// characters. The cast is in the order of the first message.
func (p *Pack) assign(msgs []Message) map[string]*cast {
	people := map[string]*cast{}
	var order []*cast
	used := map[*Character]bool{}
	for _, m := range msgs {
		if people[m.Speaker] != nil {
			continue
		}
		c := &cast{id: m.Speaker, name: m.Name, avatar: m.Avatar, char: p.byName[m.Character], order: len(order)}
		people[m.Speaker] = c
		order = append(order, c)
		if c.char != nil {
			used[c.char] = true
		}
	}
	for _, c := range order {
		if c.char != nil {
			continue
		}
		i := int(hash(c.id) % uint64(len(p.chars)))
		for range len(p.chars) {
			if !used[p.chars[i]] {
				break
			}
			i = (i + 1) % len(p.chars)
		}
		c.char = p.chars[i]
		used[c.char] = true
	}
	return people
}

// CharacterFor returns the character that a speaker gets when they chose
// none and nobody else in the comic has it.
func (p *Pack) CharacterFor(speaker string) string {
	return p.chars[hash(speaker)%uint64(len(p.chars))].Name
}

func hash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// split puts the messages in panels. As in comicgen, two messages share a
// panel at random (one time in two) when they are from two speakers, are
// short, and have no picture.
func split(msgs []Message, rng *rand.Rand) [][]Message {
	var out [][]Message
	for i := 0; i < len(msgs); {
		if i+1 < len(msgs) && canPair(msgs[i], msgs[i+1]) && rng.IntN(2) == 0 {
			out = append(out, msgs[i:i+2])
			i += 2
			continue
		}
		out = append(out, msgs[i:i+1])
		i++
	}
	return out
}

func canPair(a, b Message) bool {
	short := func(m Message) bool {
		return m.Image == nil && !m.Spoiler && utf8.RuneCountInString(m.Text) < pairChars
	}
	return a.Speaker != b.Speaker && short(a) && short(b)
}

func (p *Pack) background(name string, rng *rand.Rand) image.Image {
	for _, b := range p.backs {
		if b.Name == name {
			return b.img
		}
	}
	if len(p.backs) == 0 {
		return nil
	}
	return p.backs[rng.IntN(len(p.backs))].img
}

type renderer struct {
	dst *image.RGBA
	fc  *richtext.Faces
	rng *rand.Rand
	bg  image.Image
}

// fill draws the background, stretched to the panel and zoomed in from
// the left, at the middle height (as comicgen does).
func (r *renderer) fill(rect image.Rectangle, zoom float64) {
	if r.bg == nil {
		draw.Draw(r.dst, rect, image.NewUniform(colPanel), image.Point{}, draw.Src)
		return
	}
	sb := r.bg.Bounds()
	sw, sh := int(float64(sb.Dx())/zoom), int(float64(sb.Dy())/zoom)
	y0 := sb.Min.Y + (sb.Dy()-sh)/2
	xdraw.CatmullRom.Scale(r.dst, rect, r.bg, image.Rect(sb.Min.X, y0, sb.Min.X+sw, y0+sh), xdraw.Src, nil)
}

func (r *renderer) frame(rect image.Rectangle) {
	for _, side := range []image.Rectangle{
		image.Rect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Min.Y+border),
		image.Rect(rect.Min.X, rect.Max.Y-border, rect.Max.X, rect.Max.Y),
		image.Rect(rect.Min.X, rect.Min.Y, rect.Min.X+border, rect.Max.Y),
		image.Rect(rect.Max.X-border, rect.Min.Y, rect.Max.X, rect.Max.Y),
	} {
		draw.Draw(r.dst, side, image.NewUniform(colInk), image.Point{}, draw.Src)
	}
}

// castList draws "A comic starring:" and a row for each speaker (the
// character's head, the profile picture, and the name), as one block in
// the middle of rect, with no frame. The rows are aligned on the left, so
// that the heads and the pictures make columns.
func (r *renderer) castList(rect image.Rectangle, people map[string]*cast) {
	order := castOrder(people)
	const pad, titleH = 6, 24
	rowH := min(castRowH, (rect.Dy()-2*pad-titleH)/max(len(order), 1))
	icon := rowH - 4
	pic := min(avatarSize, icon)
	st := stCastName
	if rowH < 24 {
		st.Size = max(9, float64(rowH)-8)
	}
	nameX := icon + 4 + pic + 6 // from the left of the block
	maxName := rect.Dx() - 2*pad - nameX
	names := make([][]richtext.Item, len(order))
	blockW := 0
	for i, c := range order {
		names[i] = r.fc.Ellipsize([]richtext.Span{{Text: c.name, Style: st}}, maxName)
		blockW = max(blockW, nameX+richtext.SumWidth(names[i]).Ceil())
	}
	title := r.fc.Ellipsize([]richtext.Span{{Text: "A comic starring:", Style: stCastTitle}}, rect.Dx()-2*pad)
	titleW := richtext.SumWidth(title).Ceil()
	x0 := rect.Min.X + (rect.Dx()-blockW)/2
	y := rect.Min.Y + (rect.Dy()-titleH-len(order)*rowH)/2
	r.fc.Draw(r.dst, title, rect.Min.X+(rect.Dx()-titleW)/2, y+16)
	y += titleH
	m := r.fc.Face(st).Metrics()
	for i, c := range order {
		cy := y + rowH/2
		if c.char.icon != nil {
			b := c.char.icon.Bounds()
			s := float64(icon) / float64(max(b.Dx(), b.Dy()))
			iw, ih := int(float64(b.Dx())*s), int(float64(b.Dy())*s)
			dr := image.Rect(0, 0, iw, ih).Add(image.Pt(x0+(icon-iw)/2, cy-ih/2))
			xdraw.CatmullRom.Scale(r.dst, dr, c.char.icon, b, xdraw.Over, nil)
		}
		px := x0 + icon + 4
		r.avatar(c.avatar, px+pic/2, cy, pic)
		r.fc.Draw(r.dst, names[i], x0+nameX, cy+(m.Ascent.Round()-m.Descent.Round())/2)
		y += rowH
	}
}

func castOrder(people map[string]*cast) []*cast {
	out := make([]*cast, 0, len(people))
	for _, c := range people {
		out = append(out, c)
	}
	sortCast(out)
	return out
}

// balloonFit is a balloon's text and picture, fitted to a space.
type balloonFit struct {
	lines []richtext.Line
	st    richtext.Style
	pic   image.Point // the picture (or spoiler box) size; 0 for none
	size  image.Point // the whole balloon
}

// fitBalloon lays out a message to fit in w × h: the text at 14 px, or
// smaller until it fits (as comicgen does), and the picture under it in
// the space that is left.
func (r *renderer) fitBalloon(m Message, w, h int) balloonFit {
	maxW, maxH := w-2*balloonPad, h-2*balloonPad
	hasPic := m.Image != nil || m.Spoiler
	text := strings.TrimSpace(m.Text)
	var f balloonFit
	for size := stBalloon.Size; ; size -= 0.5 {
		f = balloonFit{st: stBalloon}
		f.st.Size = size
		var textH, textW int
		if text != "" {
			f.lines = r.fc.Wrap(r.fc.Words([]richtext.Span{{Text: text, Style: f.st}}), maxW)
			textH = len(f.lines) * lineHeight(f.st)
			for _, ln := range f.lines {
				textW = max(textW, ln.W.Ceil())
			}
		}
		left := maxH - textH
		if hasPic && text != "" {
			left -= balloonPad
		}
		if hasPic {
			f.pic = pictureSize(m, maxW, left)
		}
		fits := left >= 0 && (!hasPic || left >= maxH/3)
		if fits || size-0.5 < minFont {
			if !fits && text != "" {
				f.cut(r.fc, maxW, maxH-f.pic.Y)
				textH = len(f.lines) * lineHeight(f.st)
			}
			bh := textH + f.pic.Y + 2*balloonPad
			if hasPic && text != "" {
				bh += balloonPad
			}
			f.size = image.Pt(max(textW, f.pic.X)+2*balloonPad, bh)
			return f
		}
	}
}

// cut keeps the lines that fit in h, and ends the last one with "…".
func (f *balloonFit) cut(fc *richtext.Faces, maxW, h int) {
	n := max(h/lineHeight(f.st), 1)
	if len(f.lines) <= n {
		return
	}
	f.lines = f.lines[:n]
	last := &f.lines[n-1]
	last.Items = fc.Ellipsize(append(spans(last.Items), richtext.Span{Text: " …", Style: f.st}), maxW)
}

// panel draws one or two characters and their balloons, as comicgen does:
// a random zoom (1 to 2) for the background and the characters, the
// characters from the middle height down, and the balloons in the top
// half, the second under the first.
func (r *renderer) panel(rect image.Rectangle, msgs []Message, people map[string]*cast) {
	zoom := 1 + r.rng.Float64()
	r.fill(rect, zoom)
	inner := rect.Inset(border)
	two := len(msgs) == 2
	areaW, areaH := cell-2*areaPad, cell/2-18
	figTop := rect.Min.Y + cell/2 - 10
	scale := charScale * zoom

	// The characters: one in the middle, or two that face each other.
	type placed struct {
		rect image.Rectangle
		head image.Point
	}
	var figs []placed
	for i, m := range msgs {
		f := people[m.Speaker].char.figure(moodOf(m.Text), r.rng)
		flip := two && i == 1
		fw := int(float64(f.img.Bounds().Dx()) * scale)
		fh := int(float64(f.img.Bounds().Dy()) * scale)
		slotW := cell
		if two {
			slotW = cell / 2
		}
		fx := rect.Min.X + i*slotW + (slotW-fw)/2
		fr := image.Rect(fx, figTop, fx+fw, figTop+fh)
		img := image.Image(f.img)
		hx := int(float64(f.headX) * scale)
		if flip {
			img = flipH(f.img)
			hx = fw - hx
		}
		xdraw.CatmullRom.Scale(r.dst, fr.Intersect(inner), img, srcRect(img.Bounds(), fr, inner, scale), xdraw.Over, nil)
		figs = append(figs, placed{rect: fr, head: image.Pt(fx+hx, figTop+int(float64(f.headY)*scale))})
	}

	// The profile pictures, at the bottom middle of each character.
	for i, m := range msgs {
		fr := figs[i].rect
		cy := min(fr.Max.Y, inner.Max.Y) - avatarSize/2 - 6
		r.avatar(people[m.Speaker].avatar, fr.Min.X+fr.Dx()/2, cy, avatarSize)
	}

	// The balloons: near their speaker, at a random place across.
	y := rect.Min.Y + areaPad
	for i, m := range msgs {
		h := areaH
		if two {
			h = areaH / 2
		}
		if i == 1 {
			h = max(h, rect.Min.Y+areaPad+areaH-y)
		}
		f := r.fitBalloon(m, areaW, h)
		arrowX := figs[i].head.X
		minX, maxX := rect.Min.X+areaPad, rect.Max.X-areaPad-f.size.X
		bx := min(max(arrowX-int(float64(f.size.X)*r.rng.Float64()), minX), max(maxX, minX))
		box := image.Rectangle{Min: image.Pt(bx, y), Max: image.Pt(bx, y).Add(f.size)}
		r.balloon(box, figs[i].head, figs[i].rect.Min.Y)
		r.balloonContent(box, f, m)
		y = box.Max.Y + 4
	}
}

func (r *renderer) balloonContent(box image.Rectangle, f balloonFit, m Message) {
	ty := box.Min.Y + balloonPad
	mt := r.fc.Face(f.st).Metrics()
	for _, ln := range f.lines {
		r.fc.Draw(r.dst, ln.Items, box.Min.X+balloonPad, ty+mt.Ascent.Round())
		ty += lineHeight(f.st)
	}
	if f.pic.Y > 0 {
		if len(f.lines) > 0 {
			ty += balloonPad
		}
		pr := image.Rectangle{Max: f.pic}.Add(image.Pt(box.Min.X+(box.Dx()-f.pic.X)/2, ty))
		r.picture(pr, m)
	}
}

// pictureSize is the size of a message's picture in a balloon: the image
// scaled to fit maxW × maxH (never larger than it is), or the spoiler box.
func pictureSize(m Message, maxW, maxH int) image.Point {
	if m.Spoiler {
		return image.Pt(min(140, maxW), min(56, maxH))
	}
	if m.Image == nil {
		return image.Point{}
	}
	b := m.Image.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return image.Point{}
	}
	s := min(float64(maxW)/float64(b.Dx()), float64(maxH)/float64(b.Dy()), 1)
	return image.Pt(max(int(float64(b.Dx())*s), 1), max(int(float64(b.Dy())*s), 1))
}

// picture draws a message's picture, or a dark "spoiler" box, in r.
func (r *renderer) picture(pr image.Rectangle, m Message) {
	if m.Spoiler {
		r.roundRect(pr, 8, colDark)
		items := r.fc.Ellipsize([]richtext.Span{{Text: "spoiler", Style: stSpoiler}}, pr.Dx()-8)
		mt := r.fc.Face(stSpoiler).Metrics()
		r.fc.Draw(r.dst, items, pr.Min.X+(pr.Dx()-richtext.SumWidth(items).Ceil())/2,
			pr.Min.Y+(pr.Dy()+mt.Ascent.Round()-mt.Descent.Round())/2)
		return
	}
	xdraw.CatmullRom.Scale(r.dst, pr, m.Image, m.Image.Bounds(), xdraw.Over, nil)
}

func lineHeight(st richtext.Style) int { return int(st.Size*1.25 + 0.5) }

func spans(items []richtext.Item) []richtext.Span {
	out := make([]richtext.Span, len(items))
	for i, it := range items {
		out[i] = richtext.Span{Text: it.Text, Style: it.Style}
	}
	return out
}

// srcRect is the part of the source that is in the panel, when the source
// is drawn at dr with scale s and cut to clip.
func srcRect(src, dr, clip image.Rectangle, s float64) image.Rectangle {
	vis := dr.Intersect(clip)
	x0 := src.Min.X + int(float64(vis.Min.X-dr.Min.X)/s)
	y0 := src.Min.Y + int(float64(vis.Min.Y-dr.Min.Y)/s)
	x1 := src.Min.X + int(math.Ceil(float64(vis.Max.X-dr.Min.X)/s))
	y1 := src.Min.Y + int(math.Ceil(float64(vis.Max.Y-dr.Min.Y)/s))
	return image.Rect(x0, y0, min(x1, src.Max.X), min(y1, src.Max.Y))
}

// balloon draws a speech balloon with a tail to just above the head (or
// a short one, when the balloon is low).
func (r *renderer) balloon(box image.Rectangle, head image.Point, figTop int) {
	tipY := max(figTop-2, box.Max.Y+8)
	baseX := min(max(head.X, box.Min.X+18), box.Max.X-18)
	tipX := head.X + (baseX-head.X)/3
	tail := func(half, grow float64) [][2]float64 {
		return [][2]float64{
			{float64(baseX) - half, float64(box.Max.Y) - 6},
			{float64(baseX) + half, float64(box.Max.Y) - 6},
			{float64(tipX), float64(tipY) + grow},
		}
	}
	if tipY > box.Max.Y+2 {
		r.polygon(tail(10, 0), colInk)
	}
	r.roundRect(box, 14, colInk)
	if tipY > box.Max.Y+2 {
		r.polygon(tail(7, -3), colWhite)
	}
	r.roundRect(box.Inset(2), 12, colWhite)
}

func (r *renderer) roundRect(b image.Rectangle, rad float64, col color.Color) {
	z := vector.NewRasterizer(b.Dx(), b.Dy())
	w, h := float64(b.Dx()), float64(b.Dy())
	rad = math.Min(rad, math.Min(w, h)/2)
	k := float32(rad * 0.5523)
	R := float32(rad)
	W, H := float32(w), float32(h)
	z.MoveTo(R, 0)
	z.LineTo(W-R, 0)
	z.CubeTo(W-R+k, 0, W, R-k, W, R)
	z.LineTo(W, H-R)
	z.CubeTo(W, H-R+k, W-R+k, H, W-R, H)
	z.LineTo(R, H)
	z.CubeTo(R-k, H, 0, H-R+k, 0, H-R)
	z.LineTo(0, R)
	z.CubeTo(0, R-k, R-k, 0, R, 0)
	z.ClosePath()
	z.Draw(r.dst, b, image.NewUniform(col), image.Point{})
}

func (r *renderer) polygon(pts [][2]float64, col color.Color) {
	b := r.dst.Bounds()
	z := vector.NewRasterizer(b.Dx(), b.Dy())
	z.MoveTo(float32(pts[0][0]), float32(pts[0][1]))
	for _, p := range pts[1:] {
		z.LineTo(float32(p[0]), float32(p[1]))
	}
	z.ClosePath()
	z.Draw(r.dst, b, image.NewUniform(col), image.Point{})
}

// avatar draws a round profile picture with a white ring, centered at
// (cx, cy). A nil picture draws a grey circle.
func (r *renderer) avatar(img image.Image, cx, cy, size int) {
	ring := image.Rect(cx-size/2-2, cy-size/2-2, cx+size/2+2, cy+size/2+2)
	draw.DrawMask(r.dst, ring, image.NewUniform(colWhite), image.Point{}, circle(ring.Dx()), image.Point{}, draw.Over)
	in := image.Rect(cx-size/2, cy-size/2, cx+size/2, cy+size/2)
	pic := image.NewRGBA(image.Rect(0, 0, in.Dx(), in.Dy()))
	if img == nil {
		draw.Draw(pic, pic.Bounds(), image.NewUniform(color.RGBA{0x99, 0x99, 0x99, 0xff}), image.Point{}, draw.Src)
	} else {
		xdraw.CatmullRom.Scale(pic, pic.Bounds(), img, img.Bounds(), xdraw.Src, nil)
	}
	draw.DrawMask(r.dst, in, pic, image.Point{}, circle(in.Dx()), image.Point{}, draw.Over)
}

func circle(d int) *image.Alpha {
	z := vector.NewRasterizer(d, d)
	rad := float32(d) / 2
	const k = 0.5523
	z.MoveTo(rad, 0)
	z.CubeTo(rad+rad*k, 0, 2*rad, rad-rad*k, 2*rad, rad)
	z.CubeTo(2*rad, rad+rad*k, rad+rad*k, 2*rad, rad, 2*rad)
	z.CubeTo(rad-rad*k, 2*rad, 0, rad+rad*k, 0, rad)
	z.CubeTo(0, rad-rad*k, rad-rad*k, 0, rad, 0)
	z.ClosePath()
	m := image.NewAlpha(image.Rect(0, 0, d, d))
	z.Draw(m, m.Bounds(), image.Opaque, image.Point{})
	return m
}

func flipH(src *image.NRGBA) *image.NRGBA {
	b := src.Bounds()
	out := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			i, j := src.PixOffset(x, y), out.PixOffset(b.Max.X-1-(x-b.Min.X), y)
			copy(out.Pix[j:j+4], src.Pix[i:i+4])
		}
	}
	return out
}
