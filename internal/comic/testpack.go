package comic

import (
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

// WriteTestPack writes a small art pack into dir, for tests: the
// characters bob, ann (complex) and sim (simple), one with a missing
// sprite sheet (skipped), and the background "plain". Each pose is a solid
// block of one color, so a test can see which pose was drawn.
func WriteTestPack(dir string) error {
	for _, d := range []string{"avatars", "backgrounds"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return err
		}
	}
	atlas := image.NewNRGBA(image.Rect(0, 0, 400, 100))
	block := func(x, w, h int, c color.NRGBA) {
		for yy := range h {
			for xx := range w {
				atlas.SetNRGBA(x+xx, yy, c)
			}
		}
	}
	// x positions: icon 0, faces 20/60/100, torsos 140/200, body 260/320.
	block(0, 16, 16, color.NRGBA{1, 1, 1, 255})
	block(20, 30, 30, color.NRGBA{10, 0, 0, 255})  // neutral face
	block(60, 30, 30, color.NRGBA{20, 0, 0, 255})  // laugh face
	block(100, 30, 30, color.NRGBA{30, 0, 0, 255}) // sad face
	block(140, 50, 90, color.NRGBA{0, 10, 0, 255}) // neutral torso
	block(200, 50, 90, color.NRGBA{0, 20, 0, 255}) // wave torso
	block(260, 50, 99, color.NRGBA{0, 0, 10, 255}) // neutral body
	block(320, 50, 99, color.NRGBA{0, 0, 20, 255}) // laugh body
	if err := writePNG(filepath.Join(dir, "avatars", "sheet.png"), atlas); err != nil {
		return err
	}
	if err := writePNG(filepath.Join(dir, "backgrounds", "plain.png"), image.NewNRGBA(image.Rect(0, 0, 50, 50))); err != nil {
		return err
	}

	pose := func(id, x, w, h int, sheet string) map[string]any {
		return map[string]any{"poseID": id, "width": w, "height": h,
			"sprite": map[string]any{"atlasUrl": "/assets/avatars/" + sheet, "x": x, "y": 0}}
	}
	poses := func(sheet string) []any {
		return []any{pose(1, 0, 16, 16, sheet), pose(2, 20, 30, 30, sheet), pose(3, 60, 30, 30, sheet),
			pose(4, 100, 30, 30, sheet), pose(5, 140, 50, 90, sheet), pose(6, 200, 50, 90, sheet),
			pose(7, 260, 50, 99, sheet), pose(8, 320, 50, 99, sheet)}
	}
	face := func(id int, em, in float64) map[string]any {
		return map[string]any{"poseID": id, "emotion": em, "intensity": in, "xCX": 15, "yCX": 28, "faceX": 15, "faceY": 15}
	}
	torso := func(id int, em float64) map[string]any {
		return map[string]any{"poseID": id, "emotion": em, "intensity": 0, "xCX": 25, "yCX": 2}
	}
	complexChar := func(name, sheet string) map[string]any {
		return map[string]any{"name": name, "type": "complex", "iconPoseID": 1, "poses": poses(sheet),
			"faces":  []any{face(2, 0, 0), face(3, emLaugh, 1), face(4, emSad, 1)},
			"torsos": []any{torso(5, 0), torso(6, gestWave)}}
	}
	m := map[string]any{
		"castOrder": []string{"bob", "ann", "sim"},
		"avatars": []any{
			complexChar("ann", "sheet.png"),
			complexChar("bob", "sheet.png"),
			map[string]any{"name": "sim", "type": "simple", "iconPoseID": 1, "poses": poses("sheet.png"),
				"bodies": []any{
					map[string]any{"poseID": 7, "emotion": 0, "intensity": 0, "faceX": 25, "faceY": 10},
					map[string]any{"poseID": 8, "emotion": emLaugh, "intensity": 1, "faceX": 25, "faceY": 10},
				}},
			complexChar("gone", "missing.png"),
		},
	}
	if err := writeJSON(filepath.Join(dir, "avatars", "manifest.json"), m); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "backgrounds", "manifest.json"), map[string]any{
		"backdrops": []any{map[string]any{"name": "plain", "url": "/assets/backgrounds/plain.png"},
			map[string]any{"name": "lost", "url": "/assets/backgrounds/lost.png"}}})
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
