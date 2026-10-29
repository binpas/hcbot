// Package comic draws chat messages as a comic strip, in the style of
// Microsoft Comic Chat: a cast panel, then panels with one or two
// characters and their speech balloons. The art (characters and
// backgrounds) is not in the repository; LoadPack reads it from a folder
// (see deploy/comic-art.sh).
package comic

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // custom backgrounds
	_ "image/png"  // the art
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

// Pack is the art: the characters and the backgrounds.
type Pack struct {
	chars    []*Character // in cast order, then the custom ones
	public   []*Character // the chars that comics give to anyone
	byName   map[string]*Character
	backs    []*Background
	problems []string // custom art that could not be used, and why
}

// Character is one character's art. A complex character has heads and
// torsos that join at a connection point; a simple one has whole bodies.
type Character struct {
	Name   string
	icon   image.Image
	users  []string // when set, the only users that can use it (private)
	faces  []part
	torsos []part
	bodies []part
}

// part is one pose: a head, a torso, or a whole body.
type part struct {
	img       image.Image
	emotion   float64 // an angle on the emotion wheel, or a gesture code
	intensity float64
	cx, cy    int // heads and torsos: the connection point
	dcx, dcy  int // heads: the shift of the connection point
	fx, fy    int // heads and bodies: the middle of the face
}

// Background is one background image.
type Background struct {
	Name string
	img  image.Image
}

// The manifest files of the art pack (the format of comic-chat-web).
type avatarManifest struct {
	CastOrder []string `json:"castOrder"`
	Avatars   []struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		IconPoseID int    `json:"iconPoseID"`
		Poses      []struct {
			PoseID int `json:"poseID"`
			Width  int `json:"width"`
			Height int `json:"height"`
			Sprite struct {
				AtlasURL string `json:"atlasUrl"`
				X        int    `json:"x"`
				Y        int    `json:"y"`
			} `json:"sprite"`
		} `json:"poses"`
		Faces  []manifestPart `json:"faces"`
		Torsos []manifestPart `json:"torsos"`
		Bodies []manifestPart `json:"bodies"`
	} `json:"avatars"`
}

type manifestPart struct {
	PoseID    int     `json:"poseID"`
	Emotion   float64 `json:"emotion"`
	Intensity float64 `json:"intensity"`
	XCX       int     `json:"xCX"`
	YCX       int     `json:"yCX"`
	DeltaXCX  int     `json:"deltaXCX"`
	DeltaYCX  int     `json:"deltaYCX"`
	FaceX     int     `json:"faceX"`
	FaceY     int     `json:"faceY"`
}

type backgroundManifest struct {
	Backdrops []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"backdrops"`
}

// LoadPack reads the art in dir: avatars/manifest.json with the sprite
// sheets next to it, and backgrounds/manifest.json with the images. A
// character or background with bad data is skipped. It is an error when
// no character is left.
//
// The custom art in dir/custom (see custom.go) is added after it. Either
// part can be missing, but not both.
func LoadPack(dir string) (*Pack, error) {
	p := &Pack{byName: map[string]*Character{}}
	err := p.loadAvatars(filepath.Join(dir, "avatars"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	p.loadBackgrounds(filepath.Join(dir, "backgrounds"))
	customErr := p.loadCustom(filepath.Join(dir, "custom"))
	if len(p.chars) == 0 {
		if err == nil {
			err = customErr
		}
		if err == nil {
			err = errors.New("comic: no characters in the art pack")
		}
		return nil, err
	}
	for _, c := range p.chars {
		if len(c.users) == 0 {
			p.public = append(p.public, c)
		}
	}
	if len(p.public) == 0 {
		return nil, errors.New("comic: all the characters are private; at least one must be public")
	}
	return p, nil
}

// Problems lists the custom art that could not be used, and why.
func (p *Pack) Problems() []string { return p.problems }

func (p *Pack) loadAvatars(dir string) error {
	var m avatarManifest
	if err := readJSON(filepath.Join(dir, "manifest.json"), &m); err != nil {
		return err
	}
	atlases := map[string]image.Image{}
	atlas := func(url string) image.Image {
		name := filepath.Base(url)
		if img, ok := atlases[name]; ok {
			return img
		}
		img, err := readImage(filepath.Join(dir, name))
		if err != nil {
			img = nil
		}
		atlases[name] = img
		return img
	}
	for _, a := range m.Avatars {
		poses := map[int]image.Image{}
		for _, ps := range a.Poses {
			img := atlas(ps.Sprite.AtlasURL)
			if img == nil {
				continue
			}
			r := image.Rect(ps.Sprite.X, ps.Sprite.Y, ps.Sprite.X+ps.Width, ps.Sprite.Y+ps.Height).Add(img.Bounds().Min)
			if r.Empty() || !r.In(img.Bounds()) {
				continue
			}
			poses[ps.PoseID] = subImage(img, r)
		}
		parts := func(in []manifestPart) []part {
			var out []part
			for _, mp := range in {
				img := poses[mp.PoseID]
				if img == nil {
					continue
				}
				out = append(out, part{img: img, emotion: mp.Emotion, intensity: mp.Intensity,
					cx: mp.XCX, cy: mp.YCX, dcx: mp.DeltaXCX, dcy: mp.DeltaYCX, fx: mp.FaceX, fy: mp.FaceY})
			}
			return out
		}
		c := &Character{Name: a.Name, icon: poses[a.IconPoseID]}
		if a.Type == "complex" {
			c.faces, c.torsos = parts(a.Faces), parts(a.Torsos)
		} else {
			c.bodies = parts(a.Bodies)
		}
		usable := len(c.bodies) > 0 || (len(c.faces) > 0 && len(c.torsos) > 0)
		if !usable || c.icon == nil || c.Name == "" || p.byName[c.Name] != nil {
			continue
		}
		p.byName[c.Name] = c
	}
	// The cast order first, then the others by name.
	for _, name := range m.CastOrder {
		if c := p.byName[name]; c != nil && !p.listed(c) {
			p.chars = append(p.chars, c)
		}
	}
	var rest []*Character
	for _, c := range p.byName {
		if !p.listed(c) {
			rest = append(rest, c)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	p.chars = append(p.chars, rest...)
	return nil
}

func (p *Pack) listed(c *Character) bool { return slices.Contains(p.chars, c) }

func (p *Pack) loadBackgrounds(dir string) {
	var m backgroundManifest
	if readJSON(filepath.Join(dir, "manifest.json"), &m) != nil {
		return // no backgrounds: the panels are plain
	}
	for _, b := range m.Backdrops {
		img, err := readImage(filepath.Join(dir, filepath.Base(b.URL)))
		if err != nil || b.Name == "" {
			continue
		}
		p.backs = append(p.backs, &Background{Name: b.Name, img: img})
	}
}

// Names returns the character names, in cast order.
func (p *Pack) Names() []string {
	out := make([]string, len(p.chars))
	for i, c := range p.chars {
		out[i] = c.Name
	}
	return out
}

// usableBy reports whether a user can use the character: it is public, or
// the user is on its list.
func (c *Character) usableBy(user string) bool {
	return len(c.users) == 0 || slices.Contains(c.users, user)
}

// CanUse reports whether the pack has a character that the user can use.
func (p *Pack) CanUse(name, user string) bool {
	c := p.byName[name]
	return c != nil && c.usableBy(user)
}

// NamesFor returns the names of the characters that a user can use, in
// cast order.
func (p *Pack) NamesFor(user string) []string {
	var out []string
	for _, c := range p.usable(user) {
		out = append(out, c.Name)
	}
	return out
}

// Private returns the number of private characters.
func (p *Pack) Private() int { return len(p.chars) - len(p.public) }

func (p *Pack) usable(user string) []*Character {
	return slices.DeleteFunc(slices.Clone(p.chars), func(c *Character) bool { return !c.usableBy(user) })
}

// Backgrounds returns the background names.
func (p *Pack) Backgrounds() []string {
	out := make([]string, len(p.backs))
	for i, b := range p.backs {
		out[i] = b.Name
	}
	return out
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func readImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	img, _, err := image.Decode(f)
	return img, err
}

func subImage(img image.Image, r image.Rectangle) image.Image {
	if s, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return s.SubImage(r)
	}
	return img
}

// angleDist is the distance between two angles on the emotion wheel.
func angleDist(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 2*math.Pi)
	return math.Min(d, 2*math.Pi-d)
}
