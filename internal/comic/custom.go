package comic

import (
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Custom art is a simple folder format for your own characters and
// backgrounds, in <art folder>/custom (see README.md, "Custom comic art"):
//
//	characters/<name>/neutral.png     the character, facing right (required)
//	characters/<name>/<pose>.png      other poses (see customPoses)
//	characters/<name>/icon.png        the head for the cast list (optional)
//	characters/<name>/character.json  {"face": [x, y]} in neutral.png (optional)
//	backgrounds/<name>.png or .jpg
//
// A file that cannot be used is skipped, and Problems says why.

// The limits of custom art, in pixels.
const (
	customMinW, customMaxW = 40, 1000  // a pose
	customMinH, customMaxH = 100, 1000 // a pose
	customIconMin          = 16
	customIconMax          = 400
	customBgMin            = 160 // a background, each side
	customBgMax            = 2000
	customMaxBytes         = 8 << 20
)

// customPoses are the pose file names: an emotion with an intensity, or a
// gesture.
var customPoses = map[string]struct{ emotion, intensity float64 }{
	"neutral": {0, 0},
	"happy":   {emHappy, 0.8},
	"coy":     {emCoy, 0.8},
	"bored":   {emBored, 0.8},
	"scared":  {emScared, 0.8},
	"sad":     {emSad, 0.8},
	"angry":   {emAngry, 0.8},
	"shout":   {emShout, 0.8},
	"laugh":   {emLaugh, 0.8},
	"wave":    {gestWave, 0},
	"point":   {gestPointOther, 0},
	"self":    {gestPointSelf, 0},
	"shrug":   {gestShrug, 0},
}

// customNameRE is a character or background name: the folder or file name.
var customNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func customPoseNames() string {
	names := make([]string, 0, len(customPoses))
	for n := range customPoses {
		names = append(names, n)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

func (p *Pack) problem(format string, args ...any) {
	p.problems = append(p.problems, fmt.Sprintf(format, args...))
}

// loadCustom adds the custom characters and backgrounds in dir. It returns
// an error only when dir cannot be read.
func (p *Pack) loadCustom(dir string) error {
	chars, err := os.ReadDir(filepath.Join(dir, "characters"))
	if err != nil && !os.IsNotExist(err) {
		p.problem("custom/characters: %v", err)
	}
	for _, e := range chars {
		if e.IsDir() {
			p.loadCustomCharacter(filepath.Join(dir, "characters", e.Name()), e.Name())
		}
	}
	backs, berr := os.ReadDir(filepath.Join(dir, "backgrounds"))
	if berr != nil && !os.IsNotExist(berr) {
		p.problem("custom/backgrounds: %v", berr)
	}
	for _, e := range backs {
		if !e.IsDir() {
			p.loadCustomBackground(filepath.Join(dir, "backgrounds", e.Name()), e.Name())
		}
	}
	if err != nil && berr != nil {
		return err
	}
	return nil
}

func (p *Pack) loadCustomCharacter(dir, folder string) {
	where := "custom/characters/" + folder
	name := strings.ToLower(folder)
	if !customNameRE.MatchString(name) {
		p.problem("%s: the folder name must be 1 to 32 of a-z, 0-9, - and _ (it is the character name)", where)
		return
	}
	if p.byName[name] != nil {
		p.problem("%s: there is already a character named %q", where, name)
		return
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		p.problem("%s: %v", where, err)
		return
	}
	var face *[2]int
	if data, err := os.ReadFile(filepath.Join(dir, "character.json")); err == nil {
		var cfg struct {
			Face *[2]int `json:"face"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			p.problem("%s/character.json: %v", where, err)
		} else {
			face = cfg.Face
		}
	}
	poses := map[string]image.Image{}
	var icon image.Image
	for _, f := range files {
		if f.IsDir() || f.Name() == "character.json" || strings.HasPrefix(f.Name(), ".") {
			continue
		}
		file := where + "/" + f.Name()
		stem, ext := splitExt(f.Name())
		if ext != ".png" {
			p.problem("%s: use PNG files (with a transparent background)", file)
			continue
		}
		if stem == "icon" {
			img, err := readLimited(filepath.Join(dir, f.Name()), customIconMin, customIconMax, customIconMin, customIconMax)
			if err != nil {
				p.problem("%s: %v", file, err)
				continue
			}
			icon = img
			continue
		}
		if _, ok := customPoses[stem]; !ok {
			p.problem("%s: unknown pose; use one of: %s, icon", file, customPoseNames())
			continue
		}
		img, err := readLimited(filepath.Join(dir, f.Name()), customMinW, customMaxW, customMinH, customMaxH)
		if err != nil {
			p.problem("%s: %v", file, err)
			continue
		}
		if !transparent(img) {
			p.problem("%s: the background is not transparent (it is used, but it hides the panel)", file)
		}
		poses[stem] = img
	}
	neutral := poses["neutral"]
	if neutral == nil {
		p.problem("%s: no neutral.png (it is required)", where)
		return
	}
	nb := neutral.Bounds()
	fx, fy := nb.Dx()/2, nb.Dy()/10 // the middle of the top fifth
	if face != nil {
		if face[0] < 0 || face[0] >= nb.Dx() || face[1] < 0 || face[1] >= nb.Dy() {
			p.problem("%s/character.json: the face point %v is not in neutral.png (%d×%d)", where, *face, nb.Dx(), nb.Dy())
		} else {
			fx, fy = face[0], face[1]
		}
	}
	c := &Character{Name: name, icon: icon}
	// neutral first, then by name, so that a comic is the same after a
	// reload.
	stems := make([]string, 0, len(poses))
	for stem := range poses {
		if stem != "neutral" {
			stems = append(stems, stem)
		}
	}
	slices.Sort(stems)
	for _, stem := range append([]string{"neutral"}, stems...) {
		img, pose := poses[stem], customPoses[stem]
		b := img.Bounds()
		// The face point scales with the pose, when its size differs.
		c.bodies = append(c.bodies, part{img: img, emotion: pose.emotion, intensity: pose.intensity,
			fx: fx * b.Dx() / nb.Dx(), fy: fy * b.Dy() / nb.Dy()})
	}
	if c.icon == nil {
		c.icon = headCrop(neutral, fx, fy)
	}
	p.byName[name] = c
	p.chars = append(p.chars, c)
}

// headCrop cuts a square around the face point of a body, for the icon.
func headCrop(img image.Image, fx, fy int) image.Image {
	b := img.Bounds()
	side := min(b.Dx(), max(b.Dy()/4, 20))
	x0 := min(max(fx-side/2, 0), b.Dx()-side)
	y0 := min(max(fy-side/2, 0), b.Dy()-side)
	return subImage(img, image.Rect(x0, y0, x0+side, y0+side).Add(b.Min))
}

func (p *Pack) loadCustomBackground(path, file string) {
	where := "custom/backgrounds/" + file
	if strings.HasPrefix(file, ".") {
		return
	}
	stem, ext := splitExt(file)
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
		p.problem("%s: use a PNG or JPEG file", where)
		return
	}
	name := strings.ToLower(stem)
	if !customNameRE.MatchString(name) {
		p.problem("%s: the file name must be 1 to 32 of a-z, 0-9, - and _ (it is the background name)", where)
		return
	}
	if slices.Contains(p.Backgrounds(), name) {
		p.problem("%s: there is already a background named %q", where, name)
		return
	}
	img, err := readLimited(path, customBgMin, customBgMax, customBgMin, customBgMax)
	if err != nil {
		p.problem("%s: %v", where, err)
		return
	}
	p.backs = append(p.backs, &Background{Name: name, img: img})
}

func splitExt(name string) (string, string) {
	ext := strings.ToLower(filepath.Ext(name))
	return strings.TrimSuffix(name, filepath.Ext(name)), ext
}

// readLimited reads an image whose file and size are in the limits.
func readLimited(path string, minW, maxW, minH, maxH int) (image.Image, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > customMaxBytes {
		return nil, fmt.Errorf("the file is larger than %d MB", customMaxBytes>>20)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, fmt.Errorf("not an image: %v", err)
	}
	if cfg.Width < minW || cfg.Width > maxW || cfg.Height < minH || cfg.Height > maxH {
		return nil, fmt.Errorf("the size is %d×%d; it must be %d to %d wide and %d to %d high",
			cfg.Width, cfg.Height, minW, maxW, minH, maxH)
	}
	return readImage(path)
}

// transparent reports whether the corners of an image are transparent.
func transparent(img image.Image) bool {
	b := img.Bounds()
	for _, pt := range []image.Point{b.Min, {b.Max.X - 1, b.Min.Y}, {b.Min.X, b.Max.Y - 1}, {b.Max.X - 1, b.Max.Y - 1}} {
		if _, _, _, a := img.At(pt.X, pt.Y).RGBA(); a > 0x8000 {
			return false
		}
	}
	return true
}
