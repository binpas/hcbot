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

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/vector"

	"github.com/binpas/hcbot/internal/richtext"
)

var (
	//go:embed assets/ComicNeue-Regular.ttf
	regularTTF []byte
	//go:embed assets/ComicNeue-Bold.ttf
	boldTTF []byte
)

var fonts = sync.OnceValues(func() (richtext.Fonts, error) {
	return richtext.ParseFonts(regularTTF, boldTTF)
})

// Sizes in pixels. The art is drawn at its own size, so the image is not
// scaled.
const (
	cols       = 3
	panelW     = 300
	panelH     = 340
	margin     = 12
	gap        = 10
	border     = 4
	figScale   = 0.8 // the characters, a bit smaller than the art
	avatarSize = 30  // the profile picture on each character
	balloonPad = 9
	balloonGap = 8
	maxLines   = 7
	picH       = 120 // the highest picture in a balloon; with two speakers, picH2
	picH2      = 90
	picGap     = 6
)

var (
	colPage  = color.RGBA{0xff, 0xff, 0xff, 0xff}
	colInk   = color.RGBA{0x00, 0x00, 0x00, 0xff}
	colPanel = color.RGBA{0xfb, 0xf8, 0xee, 0xff} // when there is no background
	colWhite = color.RGBA{0xff, 0xff, 0xff, 0xff}
	colDark  = color.RGBA{0x20, 0x22, 0x25, 0xff} // the spoiler box

	stBalloon      = richtext.Style{Size: 17, Color: colInk, Thicken: 0.8} // Comic Neue Regular is thin
	stBalloonSmall = richtext.Style{Size: 15, Color: colInk, Thicken: 0.8}
	stCastTitle    = richtext.Style{Size: 21, Bold: true, Color: colInk}
	stCastName     = richtext.Style{Size: 17, Bold: true, Color: colInk}
	stSpoiler      = richtext.Style{Size: 15, Bold: true, Color: colWhite}
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

// Render draws the comic: the cast panel, then the panels.
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
	panels := split(msgs)
	bg := p.background(opt.Background, rng)

	rects := panelRects(len(panels) + 1)
	dst := image.NewRGBA(image.Rect(0, 0, imageW, rects[len(rects)-1].Max.Y+margin))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(colPage), image.Point{}, draw.Src)

	r := &renderer{dst: dst, fc: fc, rng: rng, bg: bg}
	for i, rect := range rects {
		if i == 0 {
			r.castPanel(rect, people)
		} else {
			r.panel(rect, panels[i-1], people)
		}
		r.frame(rect)
	}
	return dst, nil
}

// imageW is the width of a comic: three panels side by side.
const imageW = 2*margin + cols*panelW + (cols-1)*gap

// rows splits n panels into rows of at most cols panels, as evenly as
// possible, the larger rows first: 4 is 2+2, and 7 is 3+2+2.
func rows(n int) []int {
	out := make([]int, (n+cols-1)/cols)
	for i := range out {
		out[i] = n / len(out)
		if i < n%len(out) {
			out[i]++
		}
	}
	return out
}

// panelRects places n panels: each row fills the width of the image, so
// a row with fewer panels has wider ones.
func panelRects(n int) []image.Rectangle {
	var out []image.Rectangle
	for ri, k := range rows(n) {
		y := margin + ri*(panelH+gap)
		free := imageW - 2*margin - (k-1)*gap
		x := margin
		for j := range k {
			w := free / k
			if j < free%k {
				w++
			}
			out = append(out, image.Rect(x, y, x+w, y+panelH))
			x += w + gap
		}
	}
	return out
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

// split puts the messages in panels: at most two in a panel, from two
// different speakers.
func split(msgs []Message) [][]Message {
	var out [][]Message
	var cur []Message
	for _, m := range msgs {
		if len(cur) == 2 || (len(cur) == 1 && cur[0].Speaker == m.Speaker) {
			out = append(out, cur)
			cur = nil
		}
		cur = append(cur, m)
	}
	return append(out, cur)
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

func (r *renderer) fill(rect image.Rectangle) {
	if r.bg == nil {
		draw.Draw(r.dst, rect, image.NewUniform(colPanel), image.Point{}, draw.Src)
		return
	}
	cover(r.dst, rect, r.bg)
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

// castPanel lists each speaker: the character's head, the profile
// picture, and the name.
// The cast list is one block in the middle of the panel: the title,
// then a row for each person. The rows are aligned on the left, so that
// the heads and the pictures make columns.
func (r *renderer) castPanel(rect image.Rectangle, people map[string]*cast) {
	draw.Draw(r.dst, rect, image.NewUniform(colPanel), image.Point{}, draw.Src)
	order := castOrder(people)
	const pad, titleH = 14, 34
	rowH := min(52, (rect.Dy()-2*pad-titleH)/max(len(order), 1))
	icon := rowH - 6
	pic := min(avatarSize, icon)
	st := stCastName
	if rowH < 30 {
		st.Size = 13
	}
	nameX := icon + 8 + pic + 8 // from the left of the block
	maxName := rect.Dx() - 2*pad - nameX
	names := make([][]richtext.Item, len(order))
	blockW := 0
	for i, c := range order {
		names[i] = r.fc.Ellipsize([]richtext.Span{{Text: c.name, Style: st}}, maxName)
		blockW = max(blockW, nameX+richtext.SumWidth(names[i]).Ceil())
	}
	title := r.fc.Ellipsize([]richtext.Span{{Text: "Starring", Style: stCastTitle}}, rect.Dx()-2*pad)
	titleW := richtext.SumWidth(title).Ceil()
	blockW = max(blockW, titleW)
	x0 := rect.Min.X + (rect.Dx()-blockW)/2
	y := rect.Min.Y + (rect.Dy()-titleH-len(order)*rowH)/2
	r.fc.Draw(r.dst, title, rect.Min.X+(rect.Dx()-titleW)/2, y+18)
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
		px := x0 + icon + 8
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

// panel draws one or two characters and their balloons.
func (r *renderer) panel(rect image.Rectangle, msgs []Message, people map[string]*cast) {
	r.fill(rect)
	inner := rect.Inset(border)
	two := len(msgs) == 2

	// The balloons, from the top, in message order.
	type balloon struct {
		lines []richtext.Line
		st    richtext.Style
		box   image.Rectangle
		pic   image.Point // the picture (or spoiler box) size; 0 for none
		msg   Message
	}
	maxW := inner.Dx() - 2*balloonPad - 12
	maxPic := picH
	if two {
		maxW = inner.Dx()*64/100 - 2*balloonPad
		maxPic = picH2
	}
	var bs []balloon
	y := inner.Min.Y + 8
	for i, m := range msgs {
		st := stBalloon
		var lines []richtext.Line
		if strings.TrimSpace(m.Text) != "" {
			lines = r.fc.Wrap(r.fc.Words([]richtext.Span{{Text: m.Text, Style: st}}), maxW)
			if len(lines) > 4 {
				st = stBalloonSmall
				lines = r.fc.Wrap(r.fc.Words([]richtext.Span{{Text: m.Text, Style: st}}), maxW)
			}
			if len(lines) > maxLines {
				lines = lines[:maxLines]
				last := &lines[maxLines-1]
				last.Items = r.fc.Ellipsize(append(spans(last.Items), richtext.Span{Text: " …", Style: st}), maxW)
			}
		}
		var tw int
		for _, ln := range lines {
			tw = max(tw, ln.W.Ceil())
		}
		pic := pictureSize(m, maxW, maxPic)
		lh := lineHeight(st)
		bw := max(tw, pic.X) + 2*balloonPad
		bh := len(lines)*lh + 2*balloonPad - 4
		if pic.Y > 0 {
			bh += pic.Y + 4
			if len(lines) > 0 {
				bh += picGap
			}
		}
		bx := inner.Min.X + (inner.Dx()-bw)/2
		if two && i == 0 {
			bx = inner.Min.X + 8
		} else if two {
			bx = inner.Max.X - 8 - bw
		}
		bs = append(bs, balloon{lines: lines, st: st, box: image.Rect(bx, y, bx+bw, y+bh), pic: pic, msg: m})
		y += bh + balloonGap
	}

	// The characters stand below the balloons; the panel cuts their legs.
	top := max(y+14, inner.Min.Y+panelH*28/100)
	type placed struct {
		fig  figure
		rect image.Rectangle
		head image.Point
	}
	var figs []placed
	for i, m := range msgs {
		c := people[m.Speaker]
		f := c.char.figure(moodOf(m.Text), r.rng)
		flip := two && i == 1 // the right one faces left
		fw := int(float64(f.img.Bounds().Dx()) * figScale)
		fh := int(float64(f.img.Bounds().Dy()) * figScale)
		hx := int(float64(f.headX) * figScale)
		if flip {
			hx = fw - hx
		}
		cx := inner.Min.X + inner.Dx()/2
		if two {
			cx = inner.Min.X + inner.Dx()*28/100
			if i == 1 {
				cx = inner.Min.X + inner.Dx()*72/100
			}
		}
		fx := cx - hx
		fr := image.Rect(fx, top, fx+fw, top+fh)
		img := image.Image(f.img)
		if flip {
			img = flipH(f.img)
		}
		xdraw.CatmullRom.Scale(r.dst, fr.Intersect(inner), img, srcRect(img.Bounds(), fr, inner, figScale), xdraw.Over, nil)
		figs = append(figs, placed{fig: f, rect: fr, head: image.Pt(fx+hx, top+int(float64(f.headY)*figScale))})
	}

	// The profile pictures, at the bottom middle of each character.
	for i, m := range msgs {
		fr := figs[i].rect
		cx := fr.Min.X + fr.Dx()/2
		cy := min(fr.Max.Y, inner.Max.Y) - avatarSize/2 - 8
		r.avatar(people[m.Speaker].avatar, cx, cy, avatarSize)
	}

	// The balloons and their tails.
	for i, b := range bs {
		head := figs[i].head
		r.balloon(b.box, head, top)
		ty := b.box.Min.Y + balloonPad - 2
		for _, ln := range b.lines {
			lh := lineHeight(b.st)
			m := r.fc.Face(b.st).Metrics()
			r.fc.Draw(r.dst, ln.Items, b.box.Min.X+balloonPad, ty+m.Ascent.Round())
			ty += lh
		}
		if b.pic.Y > 0 {
			if len(b.lines) > 0 {
				ty += picGap
			}
			pr := image.Rectangle{Max: b.pic}.Add(image.Pt(b.box.Min.X+(b.box.Dx()-b.pic.X)/2, ty+2))
			r.picture(pr, b.msg)
		}
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

// balloon draws a speech balloon with a tail to just above the head.
func (r *renderer) balloon(box image.Rectangle, head image.Point, figTop int) {
	tipY := figTop - 2
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

// cover scales src to fill r, cut in the middle.
func cover(dst draw.Image, r image.Rectangle, src image.Image) {
	sb := src.Bounds()
	s := math.Max(float64(r.Dx())/float64(sb.Dx()), float64(r.Dy())/float64(sb.Dy()))
	cw, ch := int(float64(r.Dx())/s), int(float64(r.Dy())/s)
	x0 := sb.Min.X + (sb.Dx()-cw)/2
	y0 := sb.Min.Y + (sb.Dy()-ch)/2
	xdraw.CatmullRom.Scale(dst, r, src, image.Rect(x0, y0, x0+cw, y0+ch), xdraw.Src, nil)
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
