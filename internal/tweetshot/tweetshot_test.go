package tweetshot

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fixture is a post in the format of X's embed data service. Its text has
// an astral emoji, so display_text_range (UTF-16) and the entity indices
// (code points) are different.
const fixture = `{
  "__typename": "Tweet",
  "id_str": "1234567890123456789",
  "text": "Hi @friend 🎉 Tom &amp; Jerry #cats https://t.co/abc https://t.co/media",
  "display_text_range": [0, 52],
  "created_at": "2026-10-21T14:05:00.000Z",
  "favorite_count": 15800,
  "conversation_count": 1,
  "in_reply_to_screen_name": "someone",
  "isEdited": true,
  "user": {
    "name": "Test User 🐱",
    "screen_name": "testuser",
    "is_blue_verified": true,
    "profile_image_shape": "Circle",
    "profile_image_url_https": "IMG/avatar_normal.png"
  },
  "entities": {
    "user_mentions": [{"indices": [3, 10], "screen_name": "friend"}],
    "hashtags": [{"indices": [29, 34], "text": "cats"}],
    "urls": [{"indices": [35, 51], "display_url": "example.com/page"}],
    "media": [{"indices": [52, 70]}]
  },
  "mediaDetails": [
    {"type": "photo", "media_url_https": "IMG/photo.png", "original_info": {"width": 800, "height": 400}},
    {"type": "animated_gif", "media_url_https": "https://evil.example/x.png", "original_info": {"width": 100, "height": 100}}
  ],
  "quoted_tweet": {
    "__typename": "Tweet",
    "id_str": "42",
    "text": "The quoted post",
    "display_text_range": [0, 15],
    "created_at": "2025-01-02T03:04:05.000Z",
    "user": {"name": "Quoted", "screen_name": "quoted", "profile_image_url_https": "IMG/q_normal.png"},
    "entities": {}
  }
}`

type fakeX struct {
	srv      *httptest.Server
	requests []string
}

func newFakeX(t *testing.T) (*fakeX, *Client) {
	f := &fakeX{}
	var pngBuf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	_ = png.Encode(&pngBuf, img)
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.String())
		switch {
		case r.URL.Path == "/tweet-result":
			switch r.URL.Query().Get("id") {
			case "1234567890123456789":
				_, _ = w.Write([]byte(strings.ReplaceAll(fixture, "IMG/", "https://"+r.Host+"/img/")))
			case "7":
				_, _ = w.Write([]byte(`{"__typename":"TweetTombstone"}`))
			case "8":
				w.WriteHeader(http.StatusInternalServerError)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case strings.HasPrefix(r.URL.Path, "/img/"):
			_, _ = w.Write(pngBuf.Bytes())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	u, _ := url.Parse(f.srv.URL)
	return f, &Client{HTTP: f.srv.Client(), DataURL: f.srv.URL + "/tweet-result", ImageHosts: []string{u.Host}}
}

func TestFetch(t *testing.T) {
	f, c := newFakeX(t)
	p, err := c.Fetch(context.Background(), "1234567890123456789")
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var links []string
	for _, s := range p.Text {
		text.WriteString(s.Text)
		if s.Link {
			links = append(links, s.Text)
		}
	}
	if got, want := text.String(), "Hi @friend 🎉 Tom & Jerry #cats example.com/page"; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	if got := strings.Join(links, ","); got != "@friend,#cats,example.com/page" {
		t.Errorf("links = %q", got)
	}
	if p.Name != "Test User 🐱" || p.Handle != "testuser" || !p.Verified || p.ReplyTo != "someone" || !p.Edited ||
		p.Likes != 15800 || p.Replies != 1 || p.Created.Hour() != 14 {
		t.Errorf("post = %+v", p)
	}
	if p.Avatar == nil || p.Quote == nil || p.Quote.Avatar == nil || p.Quote.Name != "Quoted" {
		t.Errorf("avatar or quote missing: %+v", p.Quote)
	}
	if len(p.Media) != 2 || p.Media[0].Kind != KindPhoto || p.Media[0].Image == nil || p.Media[1].Kind != KindGIF {
		t.Fatalf("media = %+v", p.Media)
	}
	if p.Media[1].Image != nil {
		t.Error("an image from a host that is not permitted was fetched")
	}
	for _, r := range f.requests {
		if strings.Contains(r, "avatar_normal") {
			t.Errorf("fetched the small avatar: %s", r)
		}
	}

	img, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != int(cardW*scale) || b.Dy() < 500 {
		t.Errorf("image size = %v", b)
	}
	// The corners are transparent; the middle of the card is black.
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Error("corner is not transparent")
	}
	if got := img.RGBAAt(img.Bounds().Dx()-6, 200); got != (color.RGBA{0, 0, 0, 0xff}) {
		t.Errorf("background = %v", got)
	}
}

func TestFetchNotFound(t *testing.T) {
	_, c := newFakeX(t)
	for _, id := range []string{"5", "7"} {
		if _, err := c.Fetch(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := c.Fetch(context.Background(), "8"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("server error: err = %v", err)
	}
}

func TestRenderPlainPost(t *testing.T) {
	// No avatar, no media, no date: the image still draws.
	data, err := PNG(&Post{ID: "1", Name: "A", Handle: "a", Text: []Span{{Text: strings.Repeat("word ", 200)}}})
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dy() < 800 {
		t.Errorf("long text height = %d", img.Bounds().Dy())
	}
}

func TestCompact(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1234: "1,234", 9999: "9,999", 15800: "15.8K",
		20000: "20K", 132881: "132.8K", 1_259_999: "1.2M", 2_000_000: "2M"} {
		if got := compact(n); got != want {
			t.Errorf("compact(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestToken(t *testing.T) {
	tok := token("1790637656616943991")
	if tok == "" || strings.ContainsAny(tok, "0.") {
		t.Errorf("token = %q", tok)
	}
	if token("not a number") != "a" {
		t.Error("bad id token")
	}
}
