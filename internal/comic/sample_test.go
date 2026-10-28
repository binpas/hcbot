package comic

import (
	"fmt"
	"image"
	"image/color"
	"os"
	"testing"
)

// TestSample draws a comic with the real art pack, for a person to look
// at: COMIC_ART=<art folder> COMIC_SAMPLE=<out.png> go test -run Sample.
func TestSample(t *testing.T) {
	dir, out := os.Getenv("COMIC_ART"), os.Getenv("COMIC_SAMPLE")
	if dir == "" || out == "" {
		t.Skip("COMIC_ART and COMIC_SAMPLE are not set")
	}
	p, err := LoadPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	pic := func(c color.RGBA) image.Image {
		img := image.NewRGBA(image.Rect(0, 0, 64, 64))
		for i := range img.Pix {
			img.Pix[i] = []uint8{c.R, c.G, c.B, 255}[i%4]
		}
		return img
	}
	a, b, c := pic(color.RGBA{0xe0, 0x40, 0x40, 0}), pic(color.RGBA{0x40, 0x80, 0xe0, 0}), pic(color.RGBA{0x40, 0xb0, 0x60, 0})
	msgs := []Message{
		{Speaker: "1", Name: "binpas", Avatar: a, Text: "hey everyone, did you see the new comic command?"},
		{Speaker: "2", Name: "Skaldi", Avatar: b, Text: "LOL no 😂"},
		{Speaker: "1", Name: "binpas", Avatar: a, Text: "I made it today. It draws the chat as a comic"},
		{Speaker: "3", Name: "torquebot", Avatar: c, Text: "WHY WAS I NOT INFORMED"},
		{Speaker: "2", Name: "Skaldi", Avatar: b, Text: "idk, maybe because you are a bot? :("},
		{Speaker: "3", Name: "torquebot", Avatar: c, Text: "that is very sad. I will remember this forever and ever, and I will tell all the other bots about it."},
		{Speaker: "1", Name: "binpas", Avatar: a, Text: "ok"},
	}
	data, err := p.PNG(msgs, Options{Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// The layouts: 2 to 5 panels.
	for n := 1; n < len(msgs); n += 2 {
		data, err := p.PNG(msgs[:n], Options{Seed: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fmt.Sprintf("%s.%d.png", out, n), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, f := range map[string]func() ([]byte, error){
		"portrait": func() ([]byte, error) { return p.PortraitPNG("tiki") },
		"sheet":    p.SheetPNG,
	} {
		data, err := f()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out+"."+name+".png", data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
