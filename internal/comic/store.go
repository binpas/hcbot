package comic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
)

// This file changes the custom art folder, for the web interface. The
// rules are the same as for files that a person copies there (custom.go).
// Nothing here is deleted: a removed file goes to custom/.trash.

// Errors of the functions that change the art. A web handler can show the
// text of ErrInvalid errors to the user.
var (
	ErrInvalid  = errors.New("not allowed")
	ErrExists   = errors.New("that name already exists")
	ErrNotFound = errors.New("not found")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// IsUserID reports whether s has the form of a Discord user ID.
func IsUserID(s string) bool { return userIDRE.MatchString(s) }

// PoseNames returns the file names that a custom character can have,
// without ".png".
func PoseNames() []string {
	out := []string{"neutral"}
	for name := range customPoses {
		if name != "neutral" {
			out = append(out, name)
		}
	}
	slices.Sort(out[1:])
	return append(out, "icon")
}

func checkName(name string) error {
	if !customNameRE.MatchString(name) {
		return invalidf("a name must be 1 to 32 of a-z, 0-9, - and _, and start with a letter or number")
	}
	return nil
}

// saveAtomic writes a file so that a reader never sees part of it.
func saveAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// toTrash moves a file or folder to custom/.trash/<kind>, with a time in
// its name. label is the name it gets there.
func toTrash(dir, kind, path, label string) error {
	trash := filepath.Join(dir, "custom", ".trash", kind)
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	stem, ext := splitExt(label)
	stamp := time.Now().UTC().Format("20060102T150405")
	for n := 0; ; n++ { // never over an older file
		target := filepath.Join(trash, fmt.Sprintf("%s-%s-%d%s", stem, stamp, n, ext))
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			return os.Rename(path, target)
		}
	}
}

// findEntry returns the entry in folder whose name without extension is
// name, in any case, or "".
func findEntry(folder, name string) string {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		stem, _ := splitExt(e.Name())
		if e.IsDir() {
			stem = e.Name()
		}
		if strings.EqualFold(stem, name) {
			return filepath.Join(folder, e.Name())
		}
	}
	return ""
}

func checkImage(data []byte, minW, maxW, minH, maxH int) (image.Config, string, error) {
	if len(data) > customMaxBytes {
		return image.Config{}, "", invalidf("the file is larger than %d MB", customMaxBytes>>20)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return cfg, "", invalidf("this is not a PNG or JPEG image")
	}
	if err := checkSize(cfg, minW, maxW, minH, maxH); err != nil {
		return cfg, "", invalidf("%v", err)
	}
	return cfg, format, nil
}

// SaveBackground saves a custom background (PNG or JPEG). It returns
// ErrExists when the name is in use and replace is false.
func SaveBackground(dir, name string, data []byte, replace bool) error {
	if err := checkName(name); err != nil {
		return err
	}
	_, format, err := checkImage(data, customBgMin, customBgMax, customBgMin, customBgMax)
	if err != nil {
		return err
	}
	folder := filepath.Join(dir, "custom", "backgrounds")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return err
	}
	old := findEntry(folder, name)
	if old != "" && !replace {
		return ErrExists
	}
	ext := ".jpg"
	if format == "png" {
		ext = ".png"
	}
	if old != "" {
		if err := toTrash(dir, "backgrounds", old, filepath.Base(old)); err != nil {
			return err
		}
	}
	return saveAtomic(filepath.Join(folder, name+ext), data)
}

// RemoveBackground moves a custom background to the trash.
func RemoveBackground(dir, name string) error {
	old := findEntry(filepath.Join(dir, "custom", "backgrounds"), name)
	if old == "" {
		return ErrNotFound
	}
	return toTrash(dir, "backgrounds", old, filepath.Base(old))
}

func characterDir(dir, name string) string {
	return findEntry(filepath.Join(dir, "custom", "characters"), name)
}

// SaveCharacterFile saves a pose or the icon of a custom character. The
// neutral pose makes a new character; another file needs one that exists.
// The warning is for a pose with no transparent background.
func SaveCharacterFile(dir, name, stem string, data []byte, replace bool) (warning string, err error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	minW, maxW, minH, maxH := customMinW, customMaxW, customMinH, customMaxH
	if stem == "icon" {
		minW, maxW, minH, maxH = customIconMin, customIconMax, customIconMin, customIconMax
	} else if _, ok := customPoses[stem]; !ok {
		return "", invalidf("unknown pose %q", stem)
	}
	_, format, err := checkImage(data, minW, maxW, minH, maxH)
	if err != nil {
		return "", err
	}
	if format != "png" {
		return "", invalidf("use a PNG file, with a transparent background")
	}
	if stem != "icon" {
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return "", invalidf("the PNG file is damaged")
		}
		if !transparent(img) {
			warning = "The background is not transparent, so this pose hides the panel behind it."
		}
	}
	folder := characterDir(dir, name)
	if folder == "" {
		if stem != "neutral" {
			return "", ErrNotFound
		}
		folder = filepath.Join(dir, "custom", "characters", name)
		if err := os.MkdirAll(folder, 0o700); err != nil {
			return "", err
		}
	}
	path := filepath.Join(folder, stem+".png")
	if _, err := os.Stat(path); err == nil {
		if !replace {
			return "", ErrExists
		}
		if err := toTrash(dir, "characters", path, name+"-"+stem+".png"); err != nil {
			return "", err
		}
	}
	return warning, saveAtomic(path, data)
}

// RemoveCharacterFile moves a pose or the icon to the trash. A character
// must keep its neutral pose.
func RemoveCharacterFile(dir, name, stem string) error {
	if stem == "neutral" {
		return invalidf("a character must have a neutral pose")
	}
	folder := characterDir(dir, name)
	if folder == "" {
		return ErrNotFound
	}
	if _, ok := customPoses[stem]; !ok && stem != "icon" {
		return ErrNotFound
	}
	path := filepath.Join(folder, stem+".png")
	if _, err := os.Stat(path); err != nil {
		return ErrNotFound
	}
	return toTrash(dir, "characters", path, name+"-"+stem+".png")
}

// RemoveCharacter moves a custom character's folder to the trash.
func RemoveCharacter(dir, name string) error {
	folder := characterDir(dir, name)
	if folder == "" {
		return ErrNotFound
	}
	return toTrash(dir, "characters", folder, name)
}

// CharacterSettings are the values in character.json to change. A nil
// value stays as it is.
type CharacterSettings struct {
	Face    *[2]int
	Users   *[]string // an empty list makes the character public
	AddUser string
}

// UpdateCharacter changes character.json and keeps its other values.
func UpdateCharacter(dir, name string, set CharacterSettings) error {
	folder := characterDir(dir, name)
	if folder == "" {
		return ErrNotFound
	}
	path := filepath.Join(folder, "character.json")
	cfg := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return invalidf("character.json has an error, so I did not change it: %v", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if set.Face != nil {
		neutral, err := os.ReadFile(filepath.Join(folder, "neutral.png"))
		if err != nil {
			return err
		}
		c, _, err := image.DecodeConfig(bytes.NewReader(neutral))
		if err != nil {
			return err
		}
		if set.Face[0] < 0 || set.Face[0] >= c.Width || set.Face[1] < 0 || set.Face[1] >= c.Height {
			return invalidf("the face point is not in the neutral pose (%d×%d)", c.Width, c.Height)
		}
		cfg["face"] = *set.Face
	}
	var users []string
	if raw, ok := cfg["users"].([]any); ok {
		for _, u := range raw {
			if s, ok := u.(string); ok {
				users = append(users, s)
			}
		}
	}
	if set.Users != nil {
		users = *set.Users
	}
	if set.AddUser != "" && !slices.Contains(users, set.AddUser) {
		users = append(users, set.AddUser)
	}
	for _, u := range users {
		if !IsUserID(u) {
			return invalidf("%q is not a Discord user ID", u)
		}
	}
	if len(users) > 0 {
		cfg["users"] = users
	} else {
		delete(cfg, "users")
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return saveAtomic(path, append(data, '\n'))
}

// CharacterFile returns a custom character's file, as it is on disk.
func CharacterFile(dir, name, stem string) ([]byte, error) {
	if _, ok := customPoses[stem]; !ok && stem != "icon" {
		return nil, ErrNotFound
	}
	folder := characterDir(dir, name)
	if folder == "" {
		return nil, ErrNotFound
	}
	return os.ReadFile(filepath.Join(folder, stem+".png"))
}

// CharacterInfo describes a character, for the web interface.
type CharacterInfo struct {
	Name     string   `json:"name"`
	Custom   bool     `json:"custom"`
	Hidden   bool     `json:"hidden"`
	Users    []string `json:"users"`
	Poses    []string `json:"poses"`
	IconFile bool     `json:"iconFile"`
	Face     [2]int   `json:"face"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
}

// BackgroundInfo describes a background, for the web interface.
type BackgroundInfo struct {
	Name   string `json:"name"`
	Custom bool   `json:"custom"`
	Hidden bool   `json:"hidden"`
}

// Characters describes all the characters, in cast order.
func (p *Pack) Characters() []CharacterInfo {
	out := make([]CharacterInfo, 0, len(p.chars))
	for _, c := range p.chars {
		info := CharacterInfo{Name: c.Name, Custom: c.custom, Hidden: p.hiddenC[c.Name], Users: c.users, Poses: c.poses, IconFile: c.iconFile}
		if c.custom && len(c.bodies) > 0 {
			b := c.bodies[0]
			info.Face = [2]int{b.fx, b.fy}
			info.Width, info.Height = b.img.Bounds().Dx(), b.img.Bounds().Dy()
		}
		out = append(out, info)
	}
	return out
}

// BackgroundList describes all the backgrounds.
func (p *Pack) BackgroundList() []BackgroundInfo {
	out := make([]BackgroundInfo, 0, len(p.backs))
	for _, b := range p.backs {
		out = append(out, BackgroundInfo{Name: b.Name, Custom: b.custom, Hidden: p.hiddenB[b.Name]})
	}
	return out
}

// VisiblePublic returns the number of characters that comics can give to
// anyone.
func (p *Pack) VisiblePublic() int { return len(p.public) }

// IsPublic reports whether comics can give the character to anyone.
func (p *Pack) IsPublic(name string) bool {
	return slices.ContainsFunc(p.public, func(c *Character) bool { return c.Name == name })
}

// IconPNG draws a character's head.
func (p *Pack) IconPNG(name string) ([]byte, error) {
	c := p.byName[name]
	if c == nil {
		return nil, ErrNotFound
	}
	return encode(c.icon)
}

// BackgroundPNG draws a background, at most maxW wide.
func (p *Pack) BackgroundPNG(name string, maxW int) ([]byte, error) {
	for _, b := range p.backs {
		if b.Name != name {
			continue
		}
		src := b.img.Bounds()
		w, h := src.Dx(), src.Dy()
		if w > maxW {
			w, h = maxW, max(h*maxW/w, 1)
		}
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), b.img, src, xdraw.Src, nil)
		return encode(dst)
	}
	return nil, ErrNotFound
}

// PreviewPNG draws a short test comic. Empty names pick at random.
func (p *Pack) PreviewPNG(character, background string, seed uint64) ([]byte, error) {
	speaker := "preview"
	if c := p.byName[character]; c != nil {
		if len(c.users) > 0 {
			speaker = c.users[0]
		}
	} else if character != "" {
		return nil, ErrNotFound
	}
	if background != "" && !slices.Contains(p.Backgrounds(), background) {
		return nil, ErrNotFound
	}
	if background == "" && len(p.backs) > 0 {
		background = p.backs[rand.New(rand.NewPCG(seed, 1)).IntN(len(p.backs))].Name
	}
	other := "preview-other"
	name := character
	if name == "" {
		name = "Speaker"
	}
	msgs := []Message{
		{Speaker: speaker, Name: name, Character: character, Text: "hi! did you see the new comic?"},
		{Speaker: other, Name: "Other", Text: "LOL no 😂"},
		{Speaker: speaker, Name: name, Character: character, Text: "I am so sad about this :("},
		{Speaker: other, Name: "Other", Text: "WHY WAS I NOT INFORMED"},
		{Speaker: speaker, Name: name, Character: character, Text: "idk"},
	}
	return p.PNG(msgs, Options{Background: background, Seed: seed})
}
