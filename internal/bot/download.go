package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Video downloads: a mod gives a link (/download, or a right-click on a
// message), and the bot runs yt-dlp and sends the video back in an
// ephemeral reply. yt-dlp (and ffmpeg, to join video and audio) must be on
// the PATH. One download runs at a time; the others wait in a queue. When
// the best quality is larger than the upload limit, the bot tries lower
// qualities.
//
// yt-dlp's generic extractor is off, and links to local or private
// addresses are refused, so a link cannot make the bot fetch from its own
// network.

const (
	downloadMenu    = "Download video"
	downloadTimeout = 12 * time.Minute // the interaction token lasts 15
	downloadMargin  = 64 << 10         // room for the reply's JSON in the upload
)

// downloadHeights are the qualities to try, best first. 0 is the best
// that yt-dlp can get.
var downloadHeights = []int{0, 720, 480, 360, 240}

var linkRE = regexp.MustCompile(`https?://[^\s<>]+`)

// cgnat is 100.64.0.0/10 (Tailscale and carrier NAT), which net.IP does
// not count as private.
var cgnat = &net.IPNet{IP: net.IP{100, 64, 0, 0}, Mask: net.CIDRMask(10, 32)}

// ytdlpRunner runs yt-dlp with args, and returns its stdout and stderr.
type ytdlpRunner func(ctx context.Context, args []string) (stdout, stderr []byte, err error)

func execYtdlp(ctx context.Context, args []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	killGroupOnCancel(cmd) // yt-dlp starts ffmpeg; stop it too
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

func (b *Bot) registerDownload() {
	b.addCommand(&command{
		name:        "download",
		description: "Download a video from YouTube, X, or another site that yt-dlp supports (mod only).",
		options:     []*discordgo.ApplicationCommandOption{optStr("url", "Link to the video", true)},
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdDownload,
	})
	b.addCommand(&command{
		name:        downloadMenu,
		kind:        discordgo.MessageApplicationCommand,
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdDownloadFromMessage,
	})
}

func (b *Bot) cmdDownload(c *Ctx) {
	link, _ := c.Str("url")
	b.downloadVideo(c, strings.TrimSpace(link))
}

func (b *Bot) cmdDownloadFromMessage(c *Ctx) {
	msg := c.TargetMessage()
	link := ""
	if msg != nil {
		link = firstLink(msg)
	}
	if link == "" {
		c.Reply(true, errEmbed("That message has no link."))
		return
	}
	b.downloadVideo(c, link)
}

// firstLink returns the first link in a message's text, or else in its
// embeds.
func firstLink(m *discordgo.Message) string {
	if l := linkRE.FindString(m.Content); l != "" {
		return l
	}
	for _, e := range m.Embeds {
		if e.URL != "" {
			return e.URL
		}
	}
	return ""
}

func (b *Bot) downloadVideo(c *Ctx, link string) {
	if !b.cfg.Bool("DOWNLOAD_ENABLED") {
		c.Reply(true, errEmbed("Video downloads are off (`DOWNLOAD_ENABLED` in `/setup`)."))
		return
	}
	c.Defer(true)
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()

	link, err := b.checkDownloadLink(ctx, link)
	if err != nil {
		c.Reply(true, errEmbed(err.Error()))
		return
	}
	limitMB := b.uploadLimitMB(c.GuildID())

	select {
	case b.dlSlot <- struct{}{}:
	default:
		c.EditResponse(makeEmbed("⏳ In the queue", "Another download is running. Yours starts when it is done.", colourBlurple))
		select {
		case b.dlSlot <- struct{}{}:
		case <-ctx.Done():
			c.Reply(true, errEmbed("The download queue took too long. Try again later."))
			return
		}
	}
	defer func() { <-b.dlSlot }()

	res, err := b.fetchVideo(ctx, link, int64(limitMB)<<20-downloadMargin, b.env.YtdlpCookies)
	if err != nil {
		c.Reply(true, errEmbed(err.Error()))
		return
	}
	desc := fmt.Sprintf("[Source](%s) · %s", link, humanSize(int64(len(res.data))))
	if res.height > 0 {
		desc += fmt.Sprintf(" · %dp", res.height)
	}
	if res.reduced {
		desc += "\nThe best quality was too large, so this is a lower quality."
	}
	embed := makeEmbed(truncate("🎬 "+res.title, 256), desc, colourBlurple)
	if err := c.ReplyFile(true, embed, &discordgo.File{Name: res.name, Reader: bytes.NewReader(res.data)}); err != nil {
		c.Reply(true, errEmbed(fmt.Sprintf("Discord did not accept the file (%s). The server's upload limit can be smaller than `DOWNLOAD_MAX_MB`.",
			humanSize(int64(len(res.data))))))
	}
}

// checkDownloadLink returns the link without <> around it, or an error for a
// link that is not http(s) or that goes to a local or private address.
func (b *Bot) checkDownloadLink(ctx context.Context, raw string) (string, error) {
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, "<"), ">")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", errors.New("That is not a web link. Give a link that starts with `https://`.")
	}
	ips := []net.IP{net.ParseIP(u.Hostname())}
	if ips[0] == nil {
		addrs, err := b.lookupIP(ctx, u.Hostname())
		if err != nil || len(addrs) == 0 {
			return "", fmt.Errorf("I cannot find the site `%s`.", u.Hostname())
		}
		ips = addrs
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsMulticast() || cgnat.Contains(ip) {
			return "", errors.New("I do not download from local or private addresses.")
		}
	}
	return raw, nil
}

func lookupIP(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return ips, err
}

// uploadLimitMB is DOWNLOAD_MAX_MB, but not more than the server's boost
// level permits.
func (b *Bot) uploadLimitMB(guildID string) int {
	tierMB := 10
	if g, err := b.s.State.Guild(guildID); err == nil {
		switch g.PremiumTier {
		case discordgo.PremiumTier2:
			tierMB = 50
		case discordgo.PremiumTier3:
			tierMB = 100
		}
	}
	return min(max(b.cfg.Int("DOWNLOAD_MAX_MB"), 1), tierMB)
}

type videoResult struct {
	title   string
	name    string // file name for the upload
	height  int
	reduced bool // not the best quality
	data    []byte
}

// fetchVideo downloads link at the best quality that is at most maxBytes.
// cookies is a cookies file for yt-dlp, or "".
func (b *Bot) fetchVideo(ctx context.Context, link string, maxBytes int64, cookies string) (*videoResult, error) {
	dir, err := os.MkdirTemp("", "hcbot-dl-")
	if err != nil {
		return nil, fmt.Errorf("I cannot make a temporary folder: %v", err)
	}
	defer os.RemoveAll(dir)

	got := 0 // the height of the last file that was too large
	for i, h := range downloadHeights {
		if got > 0 && h >= got {
			continue // this quality gives the same file again
		}
		out := filepath.Join(dir, strconv.Itoa(i))
		stdout, stderr, err := b.ytdlp(ctx, downloadArgs(link, out, h, maxBytes, cookies))
		if ctx.Err() != nil {
			return nil, errors.New("The download took too long.")
		}
		if err != nil && h > 0 && bytes.Contains(stderr, []byte("Requested format is not available")) {
			continue // no format this small; try the next quality
		}
		if err != nil {
			return nil, fmt.Errorf("yt-dlp could not download the video: %s", ytdlpError(stderr, err))
		}
		path, size := downloadedFile(out)
		if path == "" {
			continue // larger than --max-filesize, so yt-dlp stopped
		}
		lines := strings.SplitN(strings.TrimSpace(string(stdout)), "\n", 2)
		height, _ := strconv.Atoi(strings.TrimSpace(lines[0]))
		if size > maxBytes {
			if height > 0 {
				got = height
			}
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("I cannot read the video file: %v", err)
		}
		title := "Video"
		if len(lines) == 2 && strings.TrimSpace(lines[1]) != "" {
			title = strings.TrimSpace(lines[1])
		}
		return &videoResult{title: title, name: fileName(title) + filepath.Ext(path), height: height, reduced: i > 0, data: data}, nil
	}
	return nil, fmt.Errorf("All qualities of this video are larger than the upload limit (%s). Live streams are not supported.",
		humanSize(maxBytes+downloadMargin))
}

// downloadArgs are the yt-dlp options to download link into the folder out,
// at most height pixels high (0: the best), and at most maxBytes. yt-dlp
// prints the height, then the title. It prefers H.264 video and AAC audio,
// because some Discord clients play only the sound of AV1 or VP9 video.
// The cache is in the temporary folder, because the service sandbox does
// not let the bot write its home folder. yt-dlp sends the cookies in the
// cookies file (if any) only to the sites they are for, and saves changed
// cookies back to the file.
func downloadArgs(link, out string, height int, maxBytes int64, cookies string) []string {
	format := "bv*+ba/b"
	if height > 0 {
		format = fmt.Sprintf("bv*[height<=?%d]+ba/b[height<=?%d]", height, height)
	}
	var args []string
	if cookies != "" {
		args = []string{"--cookies", cookies}
	}
	return append(args,
		"--ignore-config", "--no-playlist", "--no-progress", "--no-mtime",
		"--use-extractors", "default,-generic",
		"--match-filter", "!is_live",
		"--cache-dir", filepath.Join(os.TempDir(), "hcbot-yt-dlp-cache"),
		"-f", format, "-S", "vcodec:h264,res,acodec:aac,ext:mp4:m4a", "--merge-output-format", "mp4",
		"--max-filesize", strconv.FormatInt(maxBytes, 10),
		"-o", filepath.Join(out, "video.%(ext)s"),
		"--no-simulate", "--print", "after_move:%(height|)s", "--print", "after_move:%(title).200s",
		"--", link,
	)
}

// downloadedFile returns the finished file in dir and its size, or "".
func downloadedFile(dir string) (string, int64) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".ytdl") {
			continue
		}
		if info, err := e.Info(); err == nil {
			return filepath.Join(dir, name), info.Size()
		}
	}
	return "", 0
}

// ytdlpError is the last ERROR line from yt-dlp, or err.
func ytdlpError(stderr []byte, err error) string {
	if errors.Is(err, exec.ErrNotFound) {
		return "yt-dlp is not installed on the bot's server."
	}
	if bytes.Contains(stderr, []byte("No suitable extractor found")) {
		return "yt-dlp does not support this site."
	}
	msg := err.Error()
	for _, line := range strings.Split(string(stderr), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ERROR: "); ok {
			msg = rest
		}
	}
	return truncate(msg, 500)
}

var unsafeNameRE = regexp.MustCompile(`[^A-Za-z0-9._ -]+`)

// fileName makes a safe file name (without an extension) from a title.
func fileName(title string) string {
	name := strings.Trim(unsafeNameRE.ReplaceAllString(title, "_"), "_. ")
	if len(name) > 60 {
		name = strings.TrimRight(name[:60], "_. ")
	}
	if name == "" {
		return "video"
	}
	return name
}
