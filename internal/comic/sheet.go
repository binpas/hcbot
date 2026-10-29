package comic

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	"image/png"
	"math/rand/v2"

	xdraw "golang.org/x/image/draw"

	"github.com/binpas/hcbot/internal/richtext"
)

// ErrUnknownCharacter is the error for a name that is not in the pack.
var ErrUnknownCharacter = errors.New("comic: unknown character")

// PortraitPNG draws one character, neutral, with its name under it.
func (p *Pack) PortraitPNG(name string) ([]byte, error) {
	c := p.byName[name]
	if c == nil {
		return nil, ErrUnknownCharacter
	}
	f, err := fonts()
	if err != nil {
		return nil, err
	}
	fc := richtext.NewFaces(f, 1)
	defer fc.Close()
	const w, h, nameH = 260, 360, 40
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(colPanel), image.Point{}, draw.Src)
	fig := c.figure(mood{}, rand.New(rand.NewPCG(1, 2)))
	b := fig.img.Bounds()
	s := min(float64(w-20)/float64(b.Dx()), float64(h-nameH-16)/float64(b.Dy()), 1)
	fw, fh := int(float64(b.Dx())*s), int(float64(b.Dy())*s)
	fr := image.Rect((w-fw)/2, 10, (w-fw)/2+fw, 10+fh)
	xdraw.CatmullRom.Scale(dst, fr, fig.img, b, xdraw.Over, nil)
	r := &renderer{dst: dst, fc: fc}
	items := fc.Ellipsize([]richtext.Span{{Text: c.Name, Style: stCastTitle}}, w-20)
	fc.Draw(dst, items, (w-richtext.SumWidth(items).Ceil())/2, h-14)
	r.frame(dst.Bounds())
	return encode(dst)
}

// SheetPNG draws the head of each character that a user can use, with its
// name, in cast order.
func (p *Pack) SheetPNG(user string) ([]byte, error) {
	chars := p.usable(user)
	f, err := fonts()
	if err != nil {
		return nil, err
	}
	fc := richtext.NewFaces(f, 1)
	defer fc.Close()
	const sheetCols, cellW, cellH, iconPx = 8, 110, 96, 56
	rows := (len(chars) + sheetCols - 1) / sheetCols
	dst := image.NewRGBA(image.Rect(0, 0, 2*margin+sheetCols*cellW, 2*margin+rows*cellH))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(colPanel), image.Point{}, draw.Src)
	st := stCastName
	st.Size = 14
	for i, c := range chars {
		x := margin + (i%sheetCols)*cellW
		y := margin + (i/sheetCols)*cellH
		b := c.icon.Bounds()
		s := float64(iconPx) / float64(max(b.Dx(), b.Dy()))
		iw, ih := int(float64(b.Dx())*s), int(float64(b.Dy())*s)
		xdraw.CatmullRom.Scale(dst, image.Rect(x+(cellW-iw)/2, y+6, x+(cellW-iw)/2+iw, y+6+ih), c.icon, b, xdraw.Over, nil)
		items := fc.Ellipsize([]richtext.Span{{Text: c.Name, Style: st}}, cellW-8)
		fc.Draw(dst, items, x+(cellW-richtext.SumWidth(items).Ceil())/2, y+iconPx+28)
	}
	return encode(dst)
}

func encode(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
