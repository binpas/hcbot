package tweetshot

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/vector"
)

// Sizes are in logical pixels; the image has scale device pixels for each.
const (
	scale      = 2.0
	cardW      = 560.0
	pad        = 16.0
	avatarSize = 48.0
	radius     = 16.0
	mediaGap   = 2.0
)

// Dark theme colors, close to X's "Lights out".
var (
	colBG     = color.RGBA{0x00, 0x00, 0x00, 0xff}
	colBorder = color.RGBA{0x2f, 0x33, 0x36, 0xff}
	colText   = color.RGBA{0xe7, 0xe9, 0xea, 0xff}
	colDim    = color.RGBA{0x71, 0x76, 0x7b, 0xff}
	colLink   = color.RGBA{0x1d, 0x9b, 0xf0, 0xff}
	colEmpty  = color.RGBA{0x33, 0x36, 0x39, 0xff}
	colWhite  = color.RGBA{0xff, 0xff, 0xff, 0xff}
	colShade  = color.RGBA{0x00, 0x00, 0x00, 0xc0}
)

func textStyle(size float64, bold bool, c color.Color) style {
	return style{size: size, bold: bold, color: c}
}

var (
	stName   = textStyle(15, true, colText)
	stHandle = textStyle(15, false, colDim)
	stBody   = textStyle(17, false, colText)
	stLink   = textStyle(17, false, colLink)
	stSmall  = textStyle(15, false, colText)
	stSmallL = textStyle(15, false, colLink)
	stBadge  = textStyle(13, true, colWhite)
)

// PNG draws p and returns it as a PNG file.
func PNG(p *Post) ([]byte, error) {
	img, err := Render(p)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Render draws p as an image of a dark post card.
func Render(p *Post) (*image.RGBA, error) {
	fc, err := newFaces(scale)
	if err != nil {
		return nil, err
	}
	defer fc.close()
	c := &canvas{fc: fc}
	h := c.card(p) // measure
	c.dst = image.NewRGBA(image.Rect(0, 0, c.px(cardW), c.px(h)))
	c.card(p)
	return c.dst, nil
}

// canvas draws in logical pixels. With no dst, it only measures.
type canvas struct {
	dst *image.RGBA
	fc  *faces
}

func (c *canvas) px(v float64) int { return int(math.Round(v * scale)) }

func (c *canvas) rect(x, y, w, h float64) image.Rectangle {
	return image.Rect(c.px(x), c.py(y), c.px(x+w), c.py(y+h))
}

func (c *canvas) py(v float64) int { return c.px(v) }

// card lays out (and draws, with a dst) the full post, and returns its height.
func (c *canvas) card(p *Post) float64 {
	if c.dst != nil {
		b := c.dst.Bounds()
		h := float64(b.Dy()) / scale
		c.roundRect(0, 0, cardW, h, radius, colBorder)
		c.roundRect(1, 1, cardW-2, h-2, radius-1, colBG)
	}
	inner := cardW - 2*pad
	y := pad

	// Header: avatar, name with the verified mark, and @handle.
	c.avatar(p, pad, y, avatarSize)
	tx := pad + avatarSize + 12
	nameMax := cardW - pad - tx - 24
	nx := c.textLine([]styledText{{p.Name, stName}}, tx, y+20, nameMax)
	if p.Verified {
		c.verified(nx+4, y+4, 18)
	}
	c.textLine([]styledText{{"@" + p.Handle, stHandle}}, tx, y+41, cardW-pad-tx)
	y += avatarSize + 12

	if p.ReplyTo != "" {
		c.textLine([]styledText{{"Replying to ", textStyle(15, false, colDim)}, {"@" + p.ReplyTo, stSmallL}}, pad, y+15, inner)
		y += 24
	}

	if len(p.Text) > 0 {
		y = c.paragraph(spansText(p.Text, stBody, stLink), pad, y, inner, 23, 0)
	}
	if len(p.Media) > 0 {
		y += 12
		y += c.media(p.Media, pad, y, inner, p.Sensitive)
	}
	if p.Quote != nil {
		y += 12
		y += c.quote(p.Quote, pad, y, inner)
	}

	y += 16
	when := ""
	if !p.Created.IsZero() {
		when = p.Created.UTC().Format("3:04 PM · Jan 2, 2006") + " (UTC)"
	}
	if p.Edited {
		when += " · Edited"
	}
	if when != "" {
		c.textLine([]styledText{{when, stHandle}}, pad, y+15, inner)
		y += 20
	}

	y += 16
	if c.dst != nil {
		draw.Draw(c.dst, c.rect(pad, y, inner, 1), image.NewUniform(colBorder), image.Point{}, draw.Src)
	}
	y += 1 + 14
	var counts []styledText
	add := func(n int, one, many string) {
		if len(counts) > 0 {
			counts = append(counts, styledText{"    ", stHandle})
		}
		label := many
		if n == 1 {
			label = one
		}
		counts = append(counts, styledText{compact(n), textStyle(15, true, colText)}, styledText{" " + label, stHandle})
	}
	add(p.Replies, "Reply", "Replies")
	add(p.Likes, "Like", "Likes")
	c.textLine(counts, pad, y+15, inner)
	y += 20 + pad
	return y
}

// quote draws a quoted post in a box, and returns its height.
func (c *canvas) quote(q *Post, x, y, w float64) float64 {
	const qp = 12.0
	inner := w - 2*qp
	// Measure first, so the box can be drawn under the content.
	m := &canvas{fc: c.fc}
	h := m.quoteBody(q, x+qp, y+qp, inner) - y + qp
	if c.dst != nil {
		c.roundRect(x, y, w, h, radius, colBorder)
		c.roundRect(x+1, y+1, w-2, h-2, radius-1, colBG)
		c.quoteBody(q, x+qp, y+qp, inner)
	}
	return h
}

// quoteBody lays out the content of a quote box, and returns the y below it.
func (c *canvas) quoteBody(q *Post, x, y, w float64) float64 {
	c.avatar(q, x, y, 20)
	nx := c.textLine([]styledText{{q.Name, stName}}, x+26, y+15, w*0.55)
	if q.Verified {
		nx = c.verified(nx+3, y+1, 16) + 1
	}
	rest := " @" + q.Handle
	if !q.Created.IsZero() {
		rest += " · " + q.Created.UTC().Format("Jan 2, 2006")
	}
	c.textLine([]styledText{{rest, stHandle}}, nx, y+15, x+w-nx)
	y += 26
	if len(q.Text) > 0 {
		y = c.paragraph(spansText(q.Text, stSmall, stSmallL), x, y, w, 20, 8)
	}
	if len(q.Media) > 0 {
		y += 10
		y += c.media(q.Media, x, y, w, q.Sensitive)
	}
	return y
}

func spansText(spans []Span, plain, link style) []styledText {
	out := make([]styledText, len(spans))
	for i, s := range spans {
		st := plain
		if s.Link {
			st = link
		}
		out[i] = styledText{s.Text, st}
	}
	return out
}

// paragraph wraps text to w and draws it from top y, with lineH between
// lines. maxLines > 0 cuts the text with "…". It returns the y below it.
func (c *canvas) paragraph(parts []styledText, x, y, w, lineH float64, maxLines int) float64 {
	lines := c.fc.wrap(c.fc.words(parts), c.px(w))
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		last := &lines[maxLines-1]
		last.items = c.fc.ellipsize(append(itemsText(last.items), styledText{"  …", parts[0].st}), c.px(w))
	}
	for _, ln := range lines {
		if c.dst != nil && len(ln.items) > 0 {
			c.fc.drawItems(c.dst, ln.items, c.px(x), c.baseline(ln.items[0].st, y, lineH))
		}
		y += lineH
	}
	return y
}

func itemsText(items []item) []styledText {
	out := make([]styledText, len(items))
	for i, it := range items {
		out[i] = styledText{it.text, it.st}
	}
	return out
}

// baseline is the device y of the baseline of a line with top y.
func (c *canvas) baseline(st style, y, lineH float64) int {
	m := c.fc.face(st).Metrics()
	asc, desc := m.Ascent.Round(), m.Descent.Round()
	return c.px(y) + (c.px(lineH)-(asc+desc))/2 + asc
}

// textLine draws one line of text, cut with "…" to maxW, with the baseline
// at y. It returns the x after the text.
func (c *canvas) textLine(parts []styledText, x, y, maxW float64) float64 {
	items := c.fc.ellipsize(parts, c.px(maxW))
	if c.dst == nil {
		return x + float64(sumWidth(items).Round())/scale
	}
	end := c.fc.drawItems(c.dst, items, c.px(x), c.px(y))
	return float64(end) / scale
}

// avatar draws a round (or rounded square) avatar at x, y.
func (c *canvas) avatar(p *Post, x, y, size float64) {
	if c.dst == nil {
		return
	}
	r := size / 2
	if p.Square {
		r = size / 6
	}
	dr := c.rect(x, y, size, size)
	tile := image.NewRGBA(image.Rect(0, 0, dr.Dx(), dr.Dy()))
	if p.Avatar != nil {
		cover(tile, tile.Bounds(), p.Avatar)
	} else {
		draw.Draw(tile, tile.Bounds(), image.NewUniform(colEmpty), image.Point{}, draw.Src)
	}
	draw.DrawMask(c.dst, dr, tile, image.Point{}, roundMask(dr.Dx(), dr.Dy(), r*scale), image.Point{}, draw.Over)
}

// verified draws a blue verified mark (a circle with a check) with its
// top-left at x, y, and returns the x after it.
func (c *canvas) verified(x, y, size float64) float64 {
	if c.dst != nil {
		c.roundRect(x, y, size, size, size/2, colLink)
		s := size / 18
		check := [][2]float64{{4.6 * s, 9.4 * s}, {7.6 * s, 12.4 * s}, {13.4 * s, 6.2 * s}}
		c.polyline(x, y, check, 2.2*s, colWhite)
	}
	return x + size
}

// media draws up to 4 photos or video frames in X's grid, and returns the
// height.
func (c *canvas) media(ms []Media, x, y, w float64, sensitive bool) float64 {
	h := w * 0.5625
	if len(ms) == 1 {
		mw, mh := ms[0].Width, ms[0].Height
		if ms[0].Image != nil && (mw == 0 || mh == 0) {
			b := ms[0].Image.Bounds()
			mw, mh = b.Dx(), b.Dy()
		}
		if mw > 0 && mh > 0 {
			h = w * float64(mh) / float64(mw)
		}
		h = max(w*0.4, min(h, w*1.15))
	}
	if c.dst == nil {
		return h
	}
	half := (w - mediaGap) / 2
	halfH := (h - mediaGap) / 2
	var tiles [][4]float64 // x, y, w, h relative to the grid
	switch len(ms) {
	case 1:
		tiles = [][4]float64{{0, 0, w, h}}
	case 2:
		tiles = [][4]float64{{0, 0, half, h}, {half + mediaGap, 0, half, h}}
	case 3:
		tiles = [][4]float64{{0, 0, half, h}, {half + mediaGap, 0, half, halfH}, {half + mediaGap, halfH + mediaGap, half, halfH}}
	default:
		tiles = [][4]float64{{0, 0, half, halfH}, {half + mediaGap, 0, half, halfH},
			{0, halfH + mediaGap, half, halfH}, {half + mediaGap, halfH + mediaGap, half, halfH}}
	}

	area := image.NewRGBA(image.Rect(0, 0, c.px(w), c.px(h)))
	draw.Draw(area, area.Bounds(), image.NewUniform(colBG), image.Point{}, draw.Src)
	for i, t := range tiles {
		m := ms[i]
		tr := image.Rect(c.px(t[0]), c.px(t[1]), c.px(t[0]+t[2]), c.px(t[1]+t[3]))
		if m.Image != nil {
			if sensitive {
				blurCover(area, tr, m.Image)
			} else {
				cover(area, tr, m.Image)
			}
		} else {
			draw.Draw(area, tr, image.NewUniform(colEmpty), image.Point{}, draw.Src)
		}
		sub := &canvas{dst: area, fc: c.fc}
		switch {
		case sensitive:
			sub.label("Sensitive media", t[0]+t[2]/2, t[1]+t[3]/2)
		case m.Kind == KindVideo:
			sub.playMark(t[0]+t[2]/2, t[1]+t[3]/2)
		case m.Kind == KindGIF:
			sub.gifBadge(t[0]+8, t[1]+t[3]-8)
		}
	}
	c.roundRect(x, y, w, h, radius, colBorder)
	dr := c.rect(x+1, y+1, w-2, h-2)
	draw.DrawMask(c.dst, dr, area, image.Pt(c.px(1), c.px(1)), roundMask(dr.Dx(), dr.Dy(), (radius-1)*scale), image.Point{}, draw.Over)
	return h
}

// playMark draws X's blue play circle, centered at cx, cy.
func (c *canvas) playMark(cx, cy float64) {
	const d = 56.0
	c.roundRect(cx-d/2, cy-d/2, d, d, d/2, colLink)
	c.polygon(cx, cy, [][2]float64{{-8, -13}, {-8, 13}, {14, 0}}, colWhite)
}

// gifBadge draws a "GIF" mark with its bottom-left at x, y.
func (c *canvas) gifBadge(x, y float64) {
	c.chip("GIF", x, y-22)
}

// label draws text on a dark chip, centered at cx, cy.
func (c *canvas) label(text string, cx, cy float64) {
	w := float64(font.MeasureString(c.fc.face(stBadge), text).Round())/scale + 16
	c.chip(text, cx-w/2, cy-11)
}

// chip draws text on a small dark rounded box with its top-left at x, y.
func (c *canvas) chip(text string, x, y float64) {
	w := float64(font.MeasureString(c.fc.face(stBadge), text).Round())/scale + 16
	c.roundRect(x, y, w, 22, 4, colShade)
	c.fc.drawItems(c.dst, []item{{text: text, st: stBadge}}, c.px(x+8), c.baseline(stBadge, y, 22))
}

// roundRect fills a rounded rectangle.
func (c *canvas) roundRect(x, y, w, h, r float64, col color.Color) {
	dr := c.rect(x, y, w, h)
	mask := roundMask(dr.Dx(), dr.Dy(), r*scale)
	draw.DrawMask(c.dst, dr, image.NewUniform(col), image.Point{}, mask, image.Point{}, draw.Over)
}

// polygon fills a polygon with points relative to cx, cy.
func (c *canvas) polygon(cx, cy float64, pts [][2]float64, col color.Color) {
	b := c.dst.Bounds()
	z := vector.NewRasterizer(b.Dx(), b.Dy())
	for i, p := range pts {
		px, py := float32((cx+p[0])*scale), float32((cy+p[1])*scale)
		if i == 0 {
			z.MoveTo(px, py)
		} else {
			z.LineTo(px, py)
		}
	}
	z.ClosePath()
	z.Draw(c.dst, b, image.NewUniform(col), image.Point{})
}

// polyline strokes a line through pts (relative to x, y) with width sw.
func (c *canvas) polyline(x, y float64, pts [][2]float64, sw float64, col color.Color) {
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		dx, dy := b[0]-a[0], b[1]-a[1]
		l := math.Hypot(dx, dy)
		nx, ny := -dy/l*sw/2, dx/l*sw/2
		// Each segment is a quad, a little longer at the ends so the
		// joint has no gap.
		ex, ey := dx/l*sw/2, dy/l*sw/2
		c.polygon(x, y, [][2]float64{
			{a[0] - ex + nx, a[1] - ey + ny}, {b[0] + ex + nx, b[1] + ey + ny},
			{b[0] + ex - nx, b[1] + ey - ny}, {a[0] - ex - nx, a[1] - ey - ny},
		}, col)
	}
}

// roundMask is an antialiased rounded rectangle of w×h device pixels.
func roundMask(w, h int, r float64) *image.Alpha {
	mask := image.NewAlpha(image.Rect(0, 0, w, h))
	if w <= 0 || h <= 0 {
		return mask
	}
	z := vector.NewRasterizer(w, h)
	fw, fh := float32(w), float32(h)
	rr := float32(min(r, float64(w)/2, float64(h)/2))
	k := rr * 0.5523
	z.MoveTo(rr, 0)
	z.LineTo(fw-rr, 0)
	z.CubeTo(fw-rr+k, 0, fw, rr-k, fw, rr)
	z.LineTo(fw, fh-rr)
	z.CubeTo(fw, fh-rr+k, fw-rr+k, fh, fw-rr, fh)
	z.LineTo(rr, fh)
	z.CubeTo(rr-k, fh, 0, fh-rr+k, 0, fh-rr)
	z.LineTo(0, rr)
	z.CubeTo(0, rr-k, rr-k, 0, rr, 0)
	z.ClosePath()
	z.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	return mask
}

// cover scales src to fill r of dst, and crops the parts that do not fit.
func cover(dst draw.Image, r image.Rectangle, src image.Image) {
	sb := src.Bounds()
	if sb.Empty() || r.Empty() {
		return
	}
	s := max(float64(r.Dx())/float64(sb.Dx()), float64(r.Dy())/float64(sb.Dy()))
	cw, ch := int(float64(r.Dx())/s), int(float64(r.Dy())/s)
	x0 := sb.Min.X + (sb.Dx()-cw)/2
	y0 := sb.Min.Y + (sb.Dy()-ch)/2
	xdraw.CatmullRom.Scale(dst, r, src, image.Rect(x0, y0, x0+cw, y0+ch), xdraw.Src, nil)
}

// blurCover is cover with a strong blur: the image is made very small, then
// large again.
func blurCover(dst draw.Image, r image.Rectangle, src image.Image) {
	small := image.NewRGBA(image.Rect(0, 0, max(r.Dx()/40, 1), max(r.Dy()/40, 1)))
	cover(small, small.Bounds(), src)
	xdraw.BiLinear.Scale(dst, r, small, small.Bounds(), xdraw.Src, nil)
}

// compact writes a count the way X does: 1,234, then 12.3K and 1.2M
// (cut, not rounded).
func compact(n int) string {
	switch {
	case n >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n/100_000)/10)) + "M"
	case n >= 10_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n/100)/10)) + "K"
	case n >= 1000:
		return strconv.Itoa(n/1000) + "," + fmt.Sprintf("%03d", n%1000)
	}
	return strconv.Itoa(n)
}

func trimZero(s string) string {
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		return s[:len(s)-2]
	}
	return s
}
