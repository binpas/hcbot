package tweetshot

import (
	_ "embed"
	"sync"

	"github.com/binpas/hcbot/internal/richtext"
)

var (
	//go:embed assets/NotoSans-Regular.ttf
	regularTTF []byte
	//go:embed assets/NotoSans-Bold.ttf
	boldTTF []byte
)

var fonts = sync.OnceValues(func() (richtext.Fonts, error) {
	return richtext.ParseFonts(regularTTF, boldTTF)
})
