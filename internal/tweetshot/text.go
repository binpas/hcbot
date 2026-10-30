package tweetshot

import (
	"sync"

	"github.com/binpas/hcbot/internal/richtext"
)

var fonts = sync.OnceValues(func() (richtext.Fonts, error) {
	return richtext.ParseFonts(richtext.NotoSansRegular, richtext.NotoSansBold)
})
