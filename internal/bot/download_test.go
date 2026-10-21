package bot

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// fakeYtdlp stands in for yt-dlp. sizes gives the file size for each
// height that is asked for (0: the best). A missing height writes no file,
// the same as yt-dlp when the file is larger than --max-filesize.
type fakeYtdlp struct {
	sizes    map[int]int64
	best     int          // the height of the best quality
	noFormat map[int]bool // heights with no format that small
	stderr   string
	calls    [][]string
}

var heightArgRE = regexp.MustCompile(`height<=\?(\d+)`)

func (f *fakeYtdlp) run(_ context.Context, args []string) ([]byte, []byte, error) {
	f.calls = append(f.calls, args)
	if f.stderr != "" {
		return nil, []byte(f.stderr), errors.New("exit status 1")
	}
	var out, format string
	for i, a := range args {
		switch a {
		case "-o":
			out = filepath.Dir(args[i+1])
		case "-f":
			format = args[i+1]
		}
	}
	h := 0
	if m := heightArgRE.FindStringSubmatch(format); m != nil {
		h, _ = strconv.Atoi(m[1])
	}
	if f.noFormat[h] {
		return nil, []byte("ERROR: [x] 1: Requested format is not available.\n"), errors.New("exit status 1")
	}
	size, ok := f.sizes[h]
	if !ok {
		return nil, nil, nil
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(out, "video.mp4"), make([]byte, size), 0o644); err != nil {
		return nil, nil, err
	}
	if h == 0 {
		h = f.best
	}
	return []byte(strconv.Itoa(h) + "\nA cat: plays piano!\n"), nil, nil
}

func newDownloadBot(t *testing.T, f *fakeYtdlp) *testBot {
	tb := newTestBot(t)
	tb.ytdlp = f.run
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
