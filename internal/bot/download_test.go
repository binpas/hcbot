package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/tweetshot"
)

// fakeYtdlp stands in for yt-dlp and ffmpeg. sizes gives the file size for
// each height that is asked for (0: the best). A missing height writes no
// file, the same as yt-dlp when the file is larger than --max-filesize.
// The fields at the top are one video; list makes a list (a post with more
// than one video), with items numbered from 1.
type fakeYtdlp struct {
	sizes    map[int]int64
	best     int          // the height of the best quality
	noFormat map[int]bool // heights with no format that small
	mediaURL string       // the address yt-dlp prints
	stderr   string
	calls    [][]string // yt-dlp calls

	list []fakeItem

	gifSizes map[int]int64 // GIF size for each width
	gifFail  bool
	ffCalls  [][]string
}

type fakeItem struct {
	sizes    map[int]int64
	best     int
	noFormat map[int]bool
	mediaURL string
}

var heightArgRE = regexp.MustCompile(`height<=\?(\d+)`)

var gifWidthRE = regexp.MustCompile(`min\((\d+),iw\)`)

func (f *fakeYtdlp) run(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
	if name == "ffmpeg" {
		return f.ffmpeg(args)
	}
	f.calls = append(f.calls, args)
	if f.stderr != "" {
		return nil, []byte(f.stderr), errors.New("exit status 1")
	}
	var out, format, items string
	for i, a := range args {
		switch a {
		case "-o":
			out = filepath.Dir(args[i+1])
		case "-f":
			format = args[i+1]
		case "--playlist-items":
			items = args[i+1]
		}
	}
	h := 0
	if m := heightArgRE.FindStringSubmatch(format); m != nil {
		h, _ = strconv.Atoi(m[1])
	}
	list := map[int]fakeItem{}
	if f.list == nil {
		list[0] = fakeItem{f.sizes, f.best, f.noFormat, f.mediaURL}
	} else {
		for i, it := range f.list {
			if fakeSelected(items, i+1) {
				list[i+1] = it
			}
		}
	}
	var stdout, stderr strings.Builder
	var err error
	for idx := range len(f.list) + 1 {
		it, ok := list[idx]
		if !ok {
			continue
		}
		if it.noFormat[h] {
			stderr.WriteString("ERROR: [x] 1: Requested format is not available.\n")
			err = errors.New("exit status 1")
			continue
		}
		fmt.Fprintf(&stdout, "S %d\n", idx)
		size, ok := it.sizes[h]
		if !ok {
			continue
		}
		if err := os.MkdirAll(out, 0o755); err != nil {
			return nil, nil, err
		}
		path := filepath.Join(out, fmt.Sprintf("video-%03d.mp4", idx))
		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			return nil, nil, err
		}
		height := h
		if h == 0 {
			height = it.best
		}
		title := "A cat: plays piano!"
		if f.list != nil {
			title += fmt.Sprintf(" #%d", idx)
		}
		line, _ := json.Marshal(map[string]any{"playlist_index": nullIfZero(idx), "height": nullIfZero(height),
			"url": it.mediaURL, "title": title, "filepath": path})
		fmt.Fprintf(&stdout, "D %s\n", line)
	}
	return []byte(stdout.String()), []byte(stderr.String()), err
}

func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// fakeSelected reports whether a --playlist-items value ("1:3" or "2,4")
// selects item n. An empty value selects all.
func fakeSelected(items string, n int) bool {
	if items == "" {
		return true
	}
	if from, to, ok := strings.Cut(items, ":"); ok {
		a, _ := strconv.Atoi(from)
		b, _ := strconv.Atoi(to)
		return n >= a && n <= b
	}
	return slices.Contains(strings.Split(items, ","), strconv.Itoa(n))
}

func (f *fakeYtdlp) ffmpeg(args []string) ([]byte, []byte, error) {
	f.ffCalls = append(f.ffCalls, args)
	if f.gifFail {
		return nil, []byte("bad input"), errors.New("exit status 1")
	}
	var filter string
	for i, a := range args {
		if a == "-vf" {
			filter = args[i+1]
		}
	}
	w, _ := strconv.Atoi(gifWidthRE.FindStringSubmatch(filter)[1])
	return nil, nil, os.WriteFile(args[len(args)-1], make([]byte, f.gifSizes[w]), 0o644)
}

func newDownloadBot(t *testing.T, f *fakeYtdlp) *testBot {
	tb := newTestBot(t)
	tb.runTool = f.run
	// X post data: post 3 has 2 videos of its own and 1 photo. Other posts
	// are not found, so the bot asks for all items.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") != "3" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"__typename":"Tweet","id_str":"3","text":"","user":{"screen_name":"a"},"entities":{},
			"mediaDetails":[{"type":"video"},{"type":"photo"},{"type":"animated_gif"}],
			"quoted_tweet":{"__typename":"Tweet","id_str":"4","mediaDetails":[{"type":"video"}]}}`))
	}))
	t.Cleanup(srv.Close)
	tb.tweets = &tweetshot.Client{HTTP: srv.Client(), DataURL: srv.URL + "/tweet-result"}
	tb.lookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "intranet.example" {
			return []net.IP{net.ParseIP("10.0.0.5")}, nil
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	return tb
}

// uploaded returns the files the bot sent in interaction replies.
func (tb *testBot) uploaded() map[string]int {
	got := map[string]int{}
	for _, c := range tb.api.calls {
		for name, data := range c.Files {
			got[name] = len(data)
		}
	}
	return got
}

func TestDownloadBestQuality(t *testing.T) {
	f := &fakeYtdlp{sizes: map[int]int64{0: 4 << 20}, best: 1080}
	tb := newDownloadBot(t, f)

	tb.run(tUser, "download", strOpt("url", "https://www.youtube.com/watch?v=abc"))
	tb.expect(true, "must be a moderator")
	if len(f.calls) != 0 {
		t.Fatal("yt-dlp ran for a member who is not a mod")
	}

	tb.run(tMod, "download", strOpt("url", "<https://www.youtube.com/watch?v=abc>"))
	r := tb.expect(true, "A cat: plays piano!", "1080p", "4.0 MB", "https://www.youtube.com/watch?v=abc")
	if strings.Contains(r.text(), "lower quality") {
		t.Error("best quality reported as lower")
	}
	if strings.Contains(strings.Join(f.calls[0], " "), "--cookies") {
		t.Error("--cookies without YTDLP_COOKIES")
	}
	if got := tb.uploaded(); got["A cat_ plays piano.mp4"] != 4<<20 {
		t.Errorf("uploaded = %v", got)
	}
	args := strings.Join(f.calls[0], " ")
	for _, want := range []string{"--ignore-config", "--no-playlist", "default,-generic", "-S vcodec:h264,",
		"-- https://www.youtube.com/watch?v=abc"} {
		if !strings.Contains(args, want) {
			t.Errorf("yt-dlp args %q do not contain %q", args, want)
		}
	}
}

func TestDownloadCookies(t *testing.T) {
	f := &fakeYtdlp{sizes: map[int]int64{0: 1 << 20}, best: 720}
	tb := newDownloadBot(t, f)
	tb.env.YtdlpCookies = "youtube-cookies.txt"
	tb.run(tMod, "download", strOpt("url", "https://youtu.be/abc"))
	tb.expect(true, "720p")
	if args := strings.Join(f.calls[0], " "); !strings.Contains(args, "--cookies youtube-cookies.txt") {
		t.Errorf("yt-dlp args %q have no cookies file", args)
	}
}

func TestDownloadLowerQuality(t *testing.T) {
	// The test guild has no boost, so the limit is 10 MB.
	f := &fakeYtdlp{sizes: map[int]int64{0: 30 << 20, 480: 6 << 20}, best: 1080, noFormat: map[int]bool{720: true}}
	tb := newDownloadBot(t, f)
	tb.run(tMod, "download", strOpt("url", "https://x.com/someone/status/1"))
	tb.expect(true, "480p", "lower quality")
	if len(f.calls) != 3 { // best, 720 (no format), 480
		t.Errorf("yt-dlp ran %d times, want 3", len(f.calls))
	}
}

func TestDownloadSkipsSameQuality(t *testing.T) {
	// The best is 360p and too large, so 720 and 480 give the same file.
	f := &fakeYtdlp{sizes: map[int]int64{0: 12 << 20, 240: 3 << 20}, best: 360}
	tb := newDownloadBot(t, f)
	tb.run(tMod, "download", strOpt("url", "https://example.com/v/1"))
	tb.expect(true, "240p")
	if len(f.calls) != 2 {
		t.Errorf("yt-dlp ran %d times, want 2 (best, then 240p)", len(f.calls))
	}
}

func TestDownloadTooLarge(t *testing.T) {
	f := &fakeYtdlp{sizes: map[int]int64{}}
	tb := newDownloadBot(t, f)
	tb.run(tMod, "download", strOpt("url", "https://example.com/v/1"))
	tb.expect(true, "larger than the upload limit (10.0 MB)")
	if len(f.calls) != len(downloadHeights) {
		t.Errorf("yt-dlp ran %d times, want %d", len(f.calls), len(downloadHeights))
	}
	if len(tb.uploaded()) != 0 {
		t.Error("a file was sent")
	}
}

func TestDownloadLimitSetting(t *testing.T) {
	f := &fakeYtdlp{sizes: map[int]int64{}}
	tb := newDownloadBot(t, f)
	tb.config(map[string]string{"DOWNLOAD_MAX_MB": "5"})
	tb.run(tMod, "download", strOpt("url", "https://example.com/v/1"))
	tb.expect(true, "(5.0 MB)")

	// A level 3 boost permits 100 MB, but not more than the setting.
	g, _ := tb.s.State.Guild(tGuild)
	g.PremiumTier = discordgo.PremiumTier3
	tb.config(map[string]string{"DOWNLOAD_MAX_MB": "500"})
	tb.run(tMod, "download", strOpt("url", "https://example.com/v/1"))
	tb.expect(true, "(100.0 MB)")
}

func TestDownloadErrors(t *testing.T) {
	f := &fakeYtdlp{}
	tb := newDownloadBot(t, f)
	for _, link := range []string{"ftp://example.com/a", "not a link", "https://user:pw@example.com/a"} {
		tb.run(tMod, "download", strOpt("url", link))
		tb.expect(true, "not a web link")
	}
	for _, link := range []string{"http://127.0.0.1:8080/", "http://[::1]/", "http://100.100.1.1/", "https://intranet.example/v"} {
		tb.run(tMod, "download", strOpt("url", link))
		tb.expect(true, "local or private")
	}
	if len(f.calls) != 0 {
		t.Fatal("yt-dlp ran for a refused link")
	}

	f.stderr = "WARNING: something\nERROR: [youtube] abc: Video unavailable\n"
	tb.run(tMod, "download", strOpt("url", "https://youtu.be/abc"))
	tb.expect(true, "could not download", "Video unavailable")

	f.stderr = "ERROR: No suitable extractor found for URL https://example.com/page\n"
	tb.run(tMod, "download", strOpt("url", "https://example.com/page"))
	tb.expect(true, "does not support this site")

	tb.config(map[string]string{"DOWNLOAD_ENABLED": "off"})
	tb.run(tMod, "download", strOpt("url", "https://youtu.be/abc"))
	tb.expect(true, "downloads are off")
}

func TestDownloadFromMessage(t *testing.T) {
	f := &fakeYtdlp{sizes: map[int]int64{0: 1 << 20}, best: 720}
	tb := newDownloadBot(t, f)

	tb.runMessageMenu(tMod, downloadMenu, &discordgo.Message{ID: "7001", ChannelID: tChGeneral, Content: "no links here"})
	tb.expect(true, "has no link")

	tb.runMessageMenu(tMod, downloadMenu, &discordgo.Message{ID: "7002", ChannelID: tChGeneral,
		Content: "look at this https://x.com/a/status/5 lol https://youtu.be/other"})
	tb.expect(true, "720p")
	if last := f.calls[len(f.calls)-1]; last[len(last)-1] != "https://x.com/a/status/5" {
		t.Errorf("downloaded %q, want the first link", last[len(last)-1])
	}

	// A link only in an embed (for example, a link preview).
	tb.runMessageMenu(tMod, downloadMenu, &discordgo.Message{ID: "7003", ChannelID: tChGeneral,
		Embeds: []*discordgo.MessageEmbed{{URL: "https://youtu.be/embed"}}})
	if last := f.calls[len(f.calls)-1]; last[len(last)-1] != "https://youtu.be/embed" {
		t.Errorf("downloaded %q, want the embed link", last[len(last)-1])
	}

	tb.runMessageMenu(tUser, downloadMenu, &discordgo.Message{ID: "7004", ChannelID: tChGeneral, Content: "https://youtu.be/x"})
	tb.expect(true, "must be a moderator")
}

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"A cat: plays piano!":   "A cat_ plays piano",
		"../../etc/passwd":      "etc_passwd",
		"日本語":                   "video",
		strings.Repeat("a", 90): strings.Repeat("a", 60),
	} {
		if got := fileName(in); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
}

const tGIFURL = "https://video.twimg.com/tweet_video/AbC123.mp4"

func TestDownloadTwitterGIF(t *testing.T) {
	// yt-dlp does not know the height of a GIF on X, so it prints an empty
	// first line.
	f := &fakeYtdlp{sizes: map[int]int64{0: 300 << 10}, mediaURL: tGIFURL,
		gifSizes: map[int]int64{480: 2 << 20}}
	tb := newDownloadBot(t, f)
	tb.run(tMod, "download", strOpt("url", "https://x.com/someone/status/1"))
	r := tb.expect(true, "GIF", "2.0 MB")
	if strings.Contains(r.text(), "lower quality") {
		t.Errorf("reply = %q", r.text())
	}
	if got := tb.uploaded(); got["A cat_ plays piano.gif"] != 2<<20 {
		t.Errorf("uploaded = %v", got)
	}
	if len(f.ffCalls) != 1 || !strings.Contains(strings.Join(f.ffCalls[0], " "), "-loop 0") {
		t.Errorf("ffmpeg calls = %v", f.ffCalls)
	}
}

func TestDownloadTwitterGIFSmaller(t *testing.T) {
	// The 480 and 320 px GIFs are larger than 10 MB; the 240 px GIF fits.
	f := &fakeYtdlp{sizes: map[int]int64{0: 1 << 20}, best: 480, mediaURL: tGIFURL,
		gifSizes: map[int]int64{480: 25 << 20, 320: 12 << 20, 240: 6 << 20}}
	tb := newDownloadBot(t, f)
	tb.run(tMod, "download", strOpt("url", "https://x.com/someone/status/1"))
	tb.expect(true, "GIF", "6.0 MB", "lower quality")
	if len(f.ffCalls) != 3 {
		t.Errorf("ffmpeg ran %d times, want 3", len(f.ffCalls))
	}
}

func TestDownloadTwitterGIFFallsBackToMP4(t *testing.T) {
	f := &fakeYtdlp{sizes: map[int]int64{0: 1 << 20}, best: 480, mediaURL: tGIFURL,
		gifSizes: map[int]int64{480: 30 << 20, 320: 20 << 20, 240: 11 << 20}}
	tb := newDownloadBot(t, f)
	tb.run(tMod, "download", strOpt("url", "https://x.com/someone/status/1"))
	tb.expect(true, "480p", "GIF file was larger than the upload limit")
	if got := tb.uploaded(); got["A cat_ plays piano.mp4"] != 1<<20 {
		t.Errorf("uploaded = %v", got)
	}

	f.gifFail = true
	tb.run(tMod, "download", strOpt("url", "https://x.com/someone/status/2"))
	tb.expect(true, "480p", "could not make a GIF file")
}

func TestDownloadVideoIsNotGIF(t *testing.T) {
	for _, u := range []string{"", "https://video.twimg.com/ext_tw_video/1/pu/vid/720x720/a.mp4",
		"https://video.twimg.com/amplify_video/1/vid/a.mp4", "https://example.com/tweet_video/a.mp4"} {
		f := &fakeYtdlp{sizes: map[int]int64{0: 1 << 20}, best: 720, mediaURL: u}
		tb := newDownloadBot(t, f)
		tb.run(tMod, "download", strOpt("url", "https://x.com/someone/status/1"))
		tb.expect(true, "720p")
		if len(f.ffCalls) != 0 {
			t.Errorf("%q: ffmpeg ran for a video", u)
		}
	}
}

// fileMessages returns the file names of each reply that has files.
func (tb *testBot) fileMessages() [][]string {
	var out [][]string
	for _, c := range tb.api.calls {
		if len(c.Files) == 0 {
			continue
		}
		var names []string
		for name := range c.Files {
			names = append(names, name)
		}
		slices.Sort(names)
		out = append(out, names)
	}
	return out
}

func argValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestDownloadList(t *testing.T) {
	// Item 2 is too large at 1080p and has no 720p file; item 3 is a GIF
	// on X. The limit is 10 MB, so items 1 and 2 (9 MB) go in one message
	// and the GIF in a second message.
	f := &fakeYtdlp{list: []fakeItem{
		{sizes: map[int]int64{0: 5 << 20}, best: 720},
		{sizes: map[int]int64{0: 30 << 20, 480: 4 << 20}, best: 1080},
		{sizes: map[int]int64{0: 200 << 10}, mediaURL: tGIFURL},
	}, gifSizes: map[int]int64{480: 1 << 20}}
	tb := newDownloadBot(t, f)
	tb.run(tMod, "download", strOpt("url", "https://example.com/v/1"))

	msgs := tb.fileMessages()
	want := [][]string{{"A cat_ plays piano-1.mp4", "A cat_ plays piano-2.mp4"}, {"A cat_ plays piano-3.gif"}}
	if fmt.Sprint(msgs) != fmt.Sprint(want) {
		t.Fatalf("messages = %v, want %v", msgs, want)
	}
	first := ""
	for _, r := range tb.replies() {
		if strings.Contains(r.text(), "**1.**") {
			first = r.text()
		}
	}
	for _, w := range []string{"A cat: plays piano!", "**1.** 5.0 MB · 720p", "**2.** 4.0 MB · 480p (lower quality)",
		"**3.** 1.0 MB · GIF", "come in 2 messages"} {
		if !strings.Contains(first, w) {
			t.Errorf("first message %q does not contain %q", first, w)
		}
	}
	if strings.Contains(first, "#1") {
		t.Errorf("title keeps the item number: %q", first)
	}

	// Only item 2 is tried again: at 720p (no file) and at 480p.
	if len(f.calls) != 3 {
		t.Fatalf("yt-dlp ran %d times, want 3", len(f.calls))
	}
	if got := argValue(f.calls[0], "--playlist-items"); got != "1:10" {
		t.Errorf("first --playlist-items = %q, want 1:10", got)
	}
	for _, c := range f.calls[1:] {
		if got := argValue(c, "--playlist-items"); got != "2" {
			t.Errorf("retry --playlist-items = %q, want 2", got)
		}
	}
}

func TestDownloadListPartFails(t *testing.T) {
	f := &fakeYtdlp{list: []fakeItem{
		{sizes: map[int]int64{0: 1 << 20}, best: 720},
		{sizes: map[int]int64{}},
	}}
	tb := newDownloadBot(t, f)
	tb.run(tMod, "download", strOpt("url", "https://example.com/v/1"))
	tb.expect(true, "**1.** 1.0 MB · 720p", "**2.** Not sent: larger than the upload limit")
	if msgs := tb.fileMessages(); len(msgs) != 1 || len(msgs[0]) != 1 {
		t.Errorf("messages = %v", msgs)
	}
}

func TestDownloadOwnVideosOnly(t *testing.T) {
	// Post 3 has 2 videos of its own, and a quoted post with a video.
	f := &fakeYtdlp{list: []fakeItem{
		{sizes: map[int]int64{0: 1 << 20}, best: 720},
		{sizes: map[int]int64{0: 1 << 20}, best: 720},
		{sizes: map[int]int64{0: 1 << 20}, best: 720}, // the quoted post's video
	}}
	tb := newDownloadBot(t, f)
	tb.run(tMod, "download", strOpt("url", "https://x.com/a/status/3"))
	if got := argValue(f.calls[0], "--playlist-items"); got != "1:2" {
		t.Errorf("--playlist-items = %q, want 1:2", got)
	}
	if msgs := tb.fileMessages(); len(msgs) != 1 || len(msgs[0]) != 2 {
		t.Errorf("messages = %v", msgs)
	}
}

func TestGroupBySize(t *testing.T) {
	mk := func(sizes ...int) []*videoResult {
		var out []*videoResult
		for _, s := range sizes {
			out = append(out, &videoResult{data: make([]byte, s)})
		}
		return out
	}
	count := func(gs [][]*videoResult) []int {
		var n []int
		for _, g := range gs {
			n = append(n, len(g))
		}
		return n
	}
	if got := count(groupBySize(mk(4, 4, 4), 10)); fmt.Sprint(got) != "[2 1]" {
		t.Errorf("groups = %v", got)
	}
	if got := count(groupBySize(mk(10, 1), 10)); fmt.Sprint(got) != "[1 1]" {
		t.Errorf("groups = %v", got)
	}
	if got := count(groupBySize(mk(1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1), 100)); fmt.Sprint(got) != "[10 2]" {
		t.Errorf("groups = %v", got)
	}
}
