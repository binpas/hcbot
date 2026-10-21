// Package tweetshot draws an image of a post on X (Twitter), in a dark
// style like X's own. It gets the post from X's embed data service (the
// service behind embedded posts on web pages), which needs no login.
//
// The fonts (Noto Sans, SIL Open Font License) and the emoji images
// (Twemoji, CC-BY 4.0) are in assets/ and go into the binary, so a render
// sends requests only to X.
package tweetshot

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/gif" // avatars and media can be GIF, JPEG, PNG or WebP
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	_ "golang.org/x/image/webp"
)

// DefaultDataURL is X's embed data service.
const DefaultDataURL = "https://cdn.syndication.twimg.com/tweet-result"

const maxImageBytes = 10 << 20

// ErrNotFound is returned for a post that does not exist, or that X does
// not show in embeds (deleted, protected, or age-limited).
var ErrNotFound = errors.New("post not found")

// Client gets posts and their images.
type Client struct {
	HTTP    *http.Client
	DataURL string

	// ImageHosts are the hosts that images may come from. The post data
	// gives the image addresses, so the bot fetches nothing else.
	ImageHosts []string
}

// NewClient returns a Client for X.
func NewClient() *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: 15 * time.Second},
		DataURL:    DefaultDataURL,
		ImageHosts: []string{"pbs.twimg.com"},
	}
}

// Post is one post, ready to draw.
type Post struct {
	ID       string
	Name     string
	Handle   string
	Verified bool
	Square   bool        // the avatar is a rounded square (organizations)
	Avatar   image.Image // nil: a grey circle
	ReplyTo  string      // the handle this post replies to, or ""
	Text     []Span
	Media    []Media
	Quote    *Post
	Created  time.Time
	Edited   bool
	Replies  int
	Likes    int

	// Sensitive media is drawn blurred.
	Sensitive bool

	avatarURL string
}

// Span is a part of a post's text. Link parts (links, mentions, hashtags)
// are blue.
type Span struct {
	Text string
	Link bool
}

// Media kinds.
const (
	KindPhoto = "photo"
	KindVideo = "video"
	KindGIF   = "animated_gif"
)

// Media is one photo, or the first frame of a video or GIF.
type Media struct {
	Kind          string
	Width, Height int
	Image         image.Image // nil: a grey box
	url           string
}

// raw is the JSON of the embed data service.
type raw struct {
	Typename         string `json:"__typename"`
	ID               string `json:"id_str"`
	Text             string `json:"text"`
	DisplayTextRange []int  `json:"display_text_range"`
	CreatedAt        string `json:"created_at"`
	FavoriteCount    int    `json:"favorite_count"`
	ConversationCnt  int    `json:"conversation_count"`
	InReplyTo        string `json:"in_reply_to_screen_name"`
	Sensitive        bool   `json:"possibly_sensitive"`
	IsEdited         bool   `json:"isEdited"`
	User             struct {
		Name         string `json:"name"`
		ScreenName   string `json:"screen_name"`
		Verified     bool   `json:"verified"`
		BlueVerified bool   `json:"is_blue_verified"`
		ImageShape   string `json:"profile_image_shape"`
		ImageURL     string `json:"profile_image_url_https"`
	} `json:"user"`
	Entities struct {
		URLs []struct {
			Indices    []int  `json:"indices"`
			DisplayURL string `json:"display_url"`
		} `json:"urls"`
		Mentions []struct {
			Indices    []int  `json:"indices"`
			ScreenName string `json:"screen_name"`
		} `json:"user_mentions"`
		Hashtags []struct {
			Indices []int  `json:"indices"`
			Text    string `json:"text"`
		} `json:"hashtags"`
		Symbols []struct {
			Indices []int  `json:"indices"`
			Text    string `json:"text"`
		} `json:"symbols"`
		Media []struct {
			Indices []int `json:"indices"`
		} `json:"media"`
	} `json:"entities"`
	MediaDetails []struct {
		Type         string `json:"type"`
		URL          string `json:"media_url_https"`
		OriginalInfo struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"original_info"`
	} `json:"mediaDetails"`
	Quoted *raw `json:"quoted_tweet"`
}

// Fetch gets the post with the given ID and its images.
func (c *Client) Fetch(ctx context.Context, id string) (*Post, error) {
	u, err := url.Parse(c.DataURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("id", id)
	q.Set("lang", "en")
	q.Set("token", token(id))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("X answered %s", resp.Status)
	}
	var r raw
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("read the post data: %w", err)
	}
	if r.Typename != "Tweet" || r.ID == "" {
		return nil, ErrNotFound
	}
	p := r.post()
	c.fetchImages(ctx, p)
	return p, nil
}

// token is the token that X's embed script sends with a request:
// (id / 1e15 * π) in base 36, without zeros and the point.
func token(id string) string {
	n, err := strconv.ParseFloat(id, 64)
	if err != nil {
		return "a"
	}
	v := n / 1e15 * 3.141592653589793
	ip := int64(v)
	s := strconv.FormatInt(ip, 36)
	frac := v - float64(ip)
	var b strings.Builder
	b.WriteString(s)
	for range 11 {
		frac *= 36
		d := int(frac)
		b.WriteByte("0123456789abcdefghijklmnopqrstuvwxyz"[d])
		frac -= float64(d)
	}
	t := strings.ReplaceAll(b.String(), "0", "")
	return cmp.Or(t, "a")
}

func (r *raw) post() *Post {
	p := &Post{
		ID:        r.ID,
		Name:      r.User.Name,
		Handle:    r.User.ScreenName,
		Verified:  r.User.Verified || r.User.BlueVerified,
		Square:    r.User.ImageShape == "Square",
		ReplyTo:   r.InReplyTo,
		Text:      r.spans(),
		Edited:    r.IsEdited,
		Replies:   r.ConversationCnt,
		Likes:     r.FavoriteCount,
		Sensitive: r.Sensitive,
	}
	p.Created, _ = time.Parse(time.RFC3339, r.CreatedAt)
	for _, m := range r.MediaDetails {
		if len(p.Media) == 4 {
			break
		}
		kind := m.Type
		if kind != KindVideo && kind != KindGIF {
			kind = KindPhoto
		}
		p.Media = append(p.Media, Media{Kind: kind, Width: m.OriginalInfo.Width, Height: m.OriginalInfo.Height, url: m.URL})
	}
	if r.Quoted != nil && r.Quoted.ID != "" {
		p.Quote = r.Quoted.post()
	}
	if r.User.ImageURL != "" {
		// _normal is 48 px; the image is drawn at 96 px.
		p.avatarURL = strings.Replace(r.User.ImageURL, "_normal.", "_200x200.", 1)
	}
	return p
}

// spans splits the shown part of the text into plain and link parts. The
// entity indices count code points, in the text as X sends it (with &amp;
// and the others), so each part is unescaped after the split. But
// display_text_range counts UTF-16 units.
func (r *raw) spans() []Span {
	runes := []rune(r.Text)
	start, end := 0, len(runes)
	if len(r.DisplayTextRange) == 2 {
		start = utf16ToRune(runes, r.DisplayTextRange[0])
		end = max(start, utf16ToRune(runes, r.DisplayTextRange[1]))
	}

	type entity struct {
		from, to int
		text     string // "" removes the part (media links)
		link     bool
	}
	var ents []entity
	add := func(idx []int, text string, link bool) {
		if len(idx) == 2 && idx[0] >= start && idx[1] <= end && idx[0] < idx[1] {
			ents = append(ents, entity{idx[0], idx[1], text, link})
		}
	}
	for _, u := range r.Entities.URLs {
		add(u.Indices, u.DisplayURL, true)
	}
	for _, m := range r.Entities.Mentions {
		add(m.Indices, "@"+m.ScreenName, true)
	}
	for _, h := range r.Entities.Hashtags {
		add(h.Indices, "#"+h.Text, true)
	}
	for _, s := range r.Entities.Symbols {
		add(s.Indices, "$"+s.Text, true)
	}
	for _, m := range r.Entities.Media {
		add(m.Indices, "", false)
	}
	slices.SortFunc(ents, func(a, b entity) int { return a.from - b.from })

	var out []Span
	plain := func(s string) {
		if s = html.UnescapeString(s); s != "" {
			out = append(out, Span{Text: s})
		}
	}
	pos := start
	for _, e := range ents {
		if e.from < pos {
			continue // overlaps the one before
		}
		plain(string(runes[pos:e.from]))
		if e.text != "" {
			out = append(out, Span{Text: e.text, Link: e.link})
		}
		pos = e.to
	}
	plain(string(runes[pos:end]))
	// A media link at the end leaves a space or line break before it.
	if n := len(out); n > 0 && !out[n-1].Link {
		if t := strings.TrimRight(out[n-1].Text, " \n"); t != "" {
			out[n-1].Text = t
		} else {
			out = out[:n-1]
		}
	}
	return out
}

// utf16ToRune changes a UTF-16 offset in runes to a rune index.
func utf16ToRune(runes []rune, u int) int {
	n := 0
	for i, r := range runes {
		if n >= u {
			return i
		}
		n += utf16.RuneLen(r)
	}
	return len(runes)
}

// fetchImages gets the avatar and media images of p and its quote at the
// same time. An image that fails stays nil.
func (c *Client) fetchImages(ctx context.Context, p *Post) {
	var wg sync.WaitGroup
	get := func(dst *image.Image, u string) {
		if u == "" {
			return
		}
		wg.Go(func() { *dst = c.image(ctx, u) })
	}
	for _, q := range []*Post{p, p.Quote} {
		if q == nil {
			continue
		}
		get(&q.Avatar, q.avatarURL)
		for i := range q.Media {
			get(&q.Media[i].Image, mediaURL(q.Media[i].url))
		}
	}
	wg.Wait()
}

// mediaURL asks for the 1200 px version of a photo or thumbnail.
func mediaURL(u string) string {
	if u == "" {
		return ""
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "name=medium"
}

func (c *Client) image(ctx context.Context, u string) image.Image {
	pu, err := url.Parse(u)
	if err != nil || pu.Scheme != "https" || !slices.Contains(c.ImageHosts, pu.Host) {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	img, _, err := image.Decode(io.LimitReader(resp.Body, maxImageBytes))
	if err != nil {
		return nil
	}
	return img
}
