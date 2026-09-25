package bot

import (
	"net/http"
	"strings"
	"testing"
)

// A 1×1 PNG.
const tinyPNG = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\r\n-\xb4\x00\x00\x00\x00IEND\xaeB`\x82"

func TestBotProfile(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "botprofile avatar", tb.attachmentOpt("file", "a.png", tinyPNG))
	tb.expect(true, "don't have permission")

	tb.run(tAdmin, "botprofile avatar", tb.attachmentOpt("file", "a.txt", "hello"))
	tb.expect(true, "isn't a PNG, JPEG, GIF or WebP")

	for _, part := range []string{"avatar", "banner"} {
		tb.api.reset()
		tb.run(tAdmin, "botprofile "+part, tb.attachmentOpt("file", "a.png", tinyPNG))
		r := tb.expect(true, "Confirm New Bot", "The bot's "+part+" changes everywhere")
		if len(r.Embeds) != 1 || r.Embeds[0].Image == nil || !strings.HasSuffix(r.Embeds[0].Image.URL, "/a.png") {
			t.Errorf("%s: no preview image", part)
		}
		tb.click(tAdmin, tb.componentID(r, "confirm:yes:botprofile:"))
		tb.expect(true, "The bot's "+part+" is changed.")
		call := tb.expectCall(1, "PATCH", "^/users/@me$")[0].JSON()
		other := map[string]string{"avatar": "banner", "banner": "avatar"}[part]
		if v, _ := call[part].(string); !strings.HasPrefix(v, "data:image/png;base64,") || call[other] != nil {
			t.Errorf("%s: PATCH /users/@me = %v", part, call)
		}
		if !contains(tb.sent(tChModLog), "changed the bot's "+part) {
			t.Errorf("%s: no mod log entry: %v", part, tb.sent(tChModLog))
		}
	}

	tb.api.on("PATCH", "^/users/@me$", http.StatusTooManyRequests, `{"message":"You are being rate limited.","retry_after":1800,"global":false}`)
	tb.run(tAdmin, "botprofile avatar", tb.attachmentOpt("file", "a.png", tinyPNG))
	tb.click(tAdmin, tb.componentID(tb.last(), "confirm:yes:botprofile:"))
	tb.expect(true, "Discord is limiting avatar changes. Try again in 30m.")

	tb.api.on("PATCH", "^/users/@me$", http.StatusBadRequest,
		`{"message":"Invalid Form Body","code":50035,"errors":{"avatar":{"_errors":[{"code":"AVATAR_RATE_LIMIT","message":"You are changing your avatar too fast."}]}}}`)
	tb.run(tAdmin, "botprofile avatar", tb.attachmentOpt("file", "a.png", tinyPNG))
	tb.click(tAdmin, tb.componentID(tb.last(), "confirm:yes:botprofile:"))
	tb.expect(true, "Discord refused the change: Invalid Form Body")
}
