package comic

import (
	"image"
	"image/draw"
	"math"
	"math/rand/v2"
	"sort"
)

// figure is a whole character, ready to draw: the image, and the middle of
// the face in it.
type figure struct {
	img          *image.NRGBA
	headX, headY int
}

// figure builds the character for a mood. A complex character gets the
// head nearest to the emotion and the torso of the gesture (or a neutral
// one); its parts join as in Comic Chat (bodycam.cpp). A simple character
// gets the body of the gesture, or the one nearest to the emotion.
func (c *Character) figure(m mood, rng *rand.Rand) figure {
	if len(c.bodies) > 0 {
		b := pickGesture(c.bodies, m.gesture, rng)
		if b == nil {
			b = pickNearest(c.bodies, m.face, m.intensity, rng)
		}
		return figure{img: toNRGBA(b.img), headX: b.fx, headY: b.fy}
	}
	face := pickNearest(c.faces, m.face, m.intensity, rng)
	torso := pickGesture(c.torsos, m.gesture, rng)
	if torso == nil {
		torso = pickNearest(c.torsos, 0, 0, rng)
	}
	fb, tb := face.img.Bounds(), torso.img.Bounds()
	xOff := torso.cx + face.dcx - face.cx
	yOff := torso.cy + face.dcy - face.cy
	r := image.Rect(0, 0, tb.Dx(), tb.Dy()).Union(image.Rect(xOff, yOff, xOff+fb.Dx(), yOff+fb.Dy()))
	img := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(img, image.Rect(-r.Min.X, -r.Min.Y, -r.Min.X+tb.Dx(), -r.Min.Y+tb.Dy()), torso.img, tb.Min, draw.Over)
	draw.Draw(img, image.Rect(xOff-r.Min.X, yOff-r.Min.Y, xOff-r.Min.X+fb.Dx(), yOff-r.Min.Y+fb.Dy()), face.img, fb.Min, draw.Over)
	return figure{img: img, headX: face.fx + xOff - r.Min.X, headY: face.fy + yOff - r.Min.Y}
}

// pickNearest returns a part with the emotion nearest to em, and then the
// intensity nearest to in. Parts with a gesture code are not used. When
// several are as near, it picks one at random (Comic Chat cycled them).
func pickNearest(parts []part, em, in float64, rng *rand.Rand) *part {
	const eps = 1e-3
	var best []*part
	bestA, bestI := math.Inf(1), math.Inf(1)
	for i := range parts {
		p := &parts[i]
		if p.emotion > 2*math.Pi {
			continue
		}
		a, d := angleDist(p.emotion, em), math.Abs(p.intensity-in)
		switch {
		case a < bestA-eps || (a < bestA+eps && d < bestI-eps):
			best, bestA, bestI = []*part{p}, a, d
		case a < bestA+eps && d < bestI+eps:
			best = append(best, p)
		}
	}
	if len(best) == 0 {
		return &parts[0] // only gestures
	}
	return best[rng.IntN(len(best))]
}

// pickGesture returns a part with the gesture code, or nil.
func pickGesture(parts []part, gesture float64, rng *rand.Rand) *part {
	if gesture == 0 {
		return nil
	}
	var found []*part
	for i := range parts {
		if parts[i].emotion == gesture {
			found = append(found, &parts[i])
		}
	}
	if len(found) == 0 {
		return nil
	}
	return found[rng.IntN(len(found))]
}

func toNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
	return out
}

func sortCast(cs []*cast) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].order < cs[j].order })
}
