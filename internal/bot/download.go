package bot

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
// qualities. X keeps a GIF as a short MP4 with no sound; the bot finds it
// by its address (/tweet_video/), and uses ffmpeg to make a GIF again.
//
// yt-dlp's generic extractor is off, and links to local or private
// addresses are refused, so a link cannot make the bot fetch from its own
// network.

const (
	downloadMenu    = "Download video"
	downloadTimeout = 12 * time.Minute // the interaction token lasts 15
	downloadMargin  = 64 << 10         // room for the reply's JSON in the upload
)

// maxDownloadItems is the most files from one link: Discord permits 10
// files in one message.
const maxDownloadItems = 10

// downloadHeights are the qualities to try, best first. 0 is the best
// that yt-dlp can get.
var downloadHeights = []int{0, 720, 480, 360, 240}

var linkRE = regexp.MustCompile(`https?://[^\s<>]+`)

// cgnat is 100.64.0.0/10 (Tailscale and carrier NAT), which net.IP does
// not count as private.
var cgnat = &net.IPNet{IP: net.IP{100, 64, 0, 0}, Mask: net.CIDRMask(10, 32)}

// gifSteps are the GIF sizes to try, largest first.
var gifSteps = []struct{ width, fps int }{{480, 15}, {320, 12}, {240, 10}}

// toolRunner runs a program (yt-dlp or ffmpeg) with args, and returns its
// stdout and stderr.
type toolRunner func(ctx context.Context, name string, args []string) (stdout, stderr []byte, err error)

func execTool(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
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
	result := "ok"
	defer func() { b.m.downloads.WithLabelValues(result).Inc() }()

	link, err := b.checkDownloadLink(ctx, link)
	if err != nil {
		result = "bad_link"
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
			result = "queue_timeout"
			c.Reply(true, errEmbed("The download queue took too long. Try again later."))
			return
		}
	}
	defer func() { <-b.dlSlot }()

	maxBytes := int64(limitMB)<<20 - downloadMargin
	results, failed, err := b.fetchVideos(ctx, link, maxBytes, b.env.YtdlpCookies, b.downloadItems(ctx, link))
	if err != nil {
		result = "fetch_error"
		c.Reply(true, errEmbed(err.Error()))
		return
	}
	groups := groupBySize(results, maxBytes)
	for i, g := range groups {
		embed := makeEmbed(fmt.Sprintf("🎬 Part %d of %d", i+1, len(groups)), "", colourBlurple)
		if i == 0 {
			embed = downloadEmbed(link, results, failed, len(groups))
		}
		files := make([]*discordgo.File, len(g))
		var size int64
		for j, r := range g {
			files[j] = &discordgo.File{Name: r.name, Reader: bytes.NewReader(r.data)}
			size += int64(len(r.data))
			b.m.downloadSize.Observe(float64(len(r.data)))
		}
		if err := c.ReplyFiles(true, embed, files...); err != nil {
			result = "upload_error"
			c.Reply(true, errEmbed(fmt.Sprintf("Discord did not accept the files (%s). The server's upload limit can be smaller than `DOWNLOAD_MAX_MB`.",
				humanSize(size))))
			return
		}
	}
}

// downloadItems is the first --playlist-items value for link: at most
// maxDownloadItems, and for a post on X only the post's own videos and
// GIFs (yt-dlp puts the videos of a quoted post after them). A post with
// none of its own gets the quoted post's videos.
func (b *Bot) downloadItems(ctx context.Context, link string) string {
	n := maxDownloadItems
	if id := statusID(link); id != "" {
		if own, err := b.tweets.Videos(ctx, id); err == nil && own > 0 {
			n = min(own, maxDownloadItems)
		}
	}
	return fmt.Sprintf("1:%d", n)
}

// groupBySize puts the files in messages of at most maxBytes and
// maxDownloadItems files, in order. A file is never larger than maxBytes.
func groupBySize(results []*videoResult, maxBytes int64) [][]*videoResult {
	var groups [][]*videoResult
	var cur []*videoResult
	var size int64
	for _, r := range results {
		n := int64(len(r.data))
		if len(cur) > 0 && (size+n > maxBytes || len(cur) == maxDownloadItems) {
			groups = append(groups, cur)
			cur, size = nil, 0
		}
		cur = append(cur, r)
		size += n
	}
	return append(groups, cur)
}

var titleIndexRE = regexp.MustCompile(` #\d+$`)

// downloadEmbed describes the files: one line for one file, or a list.
// It also names the files, with the item number when there are more.
func downloadEmbed(link string, results []*videoResult, failed []int, messages int) *discordgo.MessageEmbed {
	if len(results) == 1 && len(failed) == 0 {
		r := results[0]
		desc := fmt.Sprintf("[Source](%s) · %s", link, fileInfo(r))
		if r.reduced {
			desc += "\nThe best quality was too large, so this is a lower quality."
		}
		if r.note != "" {
			desc += "\n" + r.note
		}
		return makeEmbed(truncate("🎬 "+r.title, 256), desc, colourBlurple)
	}
	title := titleIndexRE.ReplaceAllString(results[0].title, "")
	lines := []string{fmt.Sprintf("[Source](%s)", link)}
	type entry struct {
		index int
		text  string
	}
	var entries []entry
	for _, r := range results {
		r.name = fileName(title) + fmt.Sprintf("-%d", r.index) + filepath.Ext(r.name)
		text := fileInfo(r)
		if r.reduced {
			text += " (lower quality)"
		}
		if r.note != "" {
			text += "\n" + r.note
		}
		entries = append(entries, entry{r.index, text})
	}
	for _, idx := range failed {
		entries = append(entries, entry{idx, "Not sent: larger than the upload limit at all qualities."})
	}
	slices.SortFunc(entries, func(a, b entry) int { return a.index - b.index })
	for _, e := range entries {
		lines = append(lines, fmt.Sprintf("**%d.** %s", e.index, e.text))
	}
	if messages > 1 {
		lines = append(lines, fmt.Sprintf("The files are too large for one message, so they come in %d messages.", messages))
	}
	return makeEmbed(truncate("🎬 "+title, 256), truncate(strings.Join(lines, "\n"), 4000), colourBlurple)
}

// fileInfo is the size and the quality of a file.
func fileInfo(r *videoResult) string {
	s := humanSize(int64(len(r.data)))
	switch {
	case r.gif:
		s += " · GIF"
	case r.height > 0:
		s += fmt.Sprintf(" · %dp", r.height)
	}
	return s
}

// checkDownloadLink returns the link without <> around it, or an error for a
// link that is not http(s) or that goes to a local or private address.
func (b *Bot) checkDownloadLink(ctx context.Context, raw string) (string, error) {
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, "<"), ">")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", errors.New("That is not a web link. Give a link that starts with `https://`.") //nolint:staticcheck // shown to the user
	}
	ips := []net.IP{net.ParseIP(u.Hostname())}
	if ips[0] == nil {
		addrs, err := b.lookupIP(ctx, u.Hostname())
		if err != nil || len(addrs) == 0 {
			return "", fmt.Errorf("I cannot find the site `%s`.", u.Hostname()) //nolint:staticcheck // shown to the user
		}
		ips = addrs
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsMulticast() || cgnat.Contains(ip) {
			return "", errors.New("I do not download from local or private addresses.") //nolint:staticcheck // shown to the user
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
	return min(max(b.cfg.Int("DOWNLOAD_MAX_MB"), 1), b.serverUploadMB(guildID))
}

// serverUploadMB is the upload limit of a server's boost level.
func (b *Bot) serverUploadMB(guildID string) int {
	if g, err := b.s.State.Guild(guildID); err == nil {
		switch g.PremiumTier {
		case discordgo.PremiumTier2:
			return 50
		case discordgo.PremiumTier3:
			return 100
		}
	}
	return 10
}

type videoResult struct {
	index   int // the position in the post or list (0: not a list)
	title   string
	name    string // file name for the upload
	height  int
	reduced bool   // not the best quality, or a smaller GIF
	gif     bool   // data is a GIF
	note    string // why a GIF on X came as an MP4
	data    []byte
}

// ytdlpItem is the line that yt-dlp prints for each finished file.
type ytdlpItem struct {
	Index    int    `json:"playlist_index"`
	Height   int    `json:"height"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Filepath string `json:"filepath"`
}

// parseYtdlp reads the lines of downloadArgs: "S <index>" when yt-dlp
// starts an item, and "D <json>" when it finished one. An item that was
// larger than --max-filesize has only an S line.
func parseYtdlp(stdout []byte) (started []int, done []ytdlpItem) {
	for _, line := range strings.Split(string(stdout), "\n") {
		switch {
		case strings.HasPrefix(line, "S "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[2:])); err == nil {
				started = append(started, n)
			}
		case strings.HasPrefix(line, "D "):
			var it ytdlpItem
			if json.Unmarshal([]byte(line[2:]), &it) == nil {
				done = append(done, it)
			}
		}
	}
	return started, done
}

// fetchVideos downloads the items of link (at most maxDownloadItems) at the
// best quality that is at most maxBytes each. items is the first
// --playlist-items value. Each item that is too large is tried again alone
// at the next quality. It returns the files in list order, and the items
// that were too large at all qualities. cookies is a cookies file for
// yt-dlp, or "".
func (b *Bot) fetchVideos(ctx context.Context, link string, maxBytes int64, cookies, items string) ([]*videoResult, []int, error) {
	dir, err := os.MkdirTemp("", "hcbot-dl-")
	if err != nil {
		return nil, nil, fmt.Errorf("I cannot make a temporary folder: %v", err) //nolint:staticcheck // shown to the user
	}
	defer func() { _ = os.RemoveAll(dir) }()

	type state struct {
		got  int // the height of the last file that was too large
		done bool
	}
	states := map[int]*state{}
	var order []int
	var results []*videoResult
	for step, h := range downloadHeights {
		spec := items
		if step > 0 {
			var want []string
			for _, idx := range order {
				if st := states[idx]; !st.done && (st.got == 0 || h < st.got) {
					want = append(want, strconv.Itoa(idx))
				}
			}
			if len(want) == 0 {
				continue // nothing to try at this quality
			}
			spec = strings.Join(want, ",")
			if spec == "0" {
				spec = "" // one video, not a list
			}
		}
		out := filepath.Join(dir, strconv.Itoa(step))
		stdout, stderr, err := b.runTool(ctx, "yt-dlp", downloadArgs(link, out, h, maxBytes, cookies, spec))
		if ctx.Err() != nil {
			return nil, nil, errors.New("The download took too long.") //nolint:staticcheck // shown to the user
		}
		started, done := parseYtdlp(stdout)
		if step == 0 {
			if len(started) == 0 {
				if err != nil {
					return nil, nil, fmt.Errorf("yt-dlp could not download the video: %s", ytdlpError(stderr, err))
				}
				return nil, nil, errors.New("I found no video at this link. Live streams are not supported.") //nolint:staticcheck // shown to the user
			}
			for _, idx := range started {
				if states[idx] == nil && len(order) < maxDownloadItems {
					states[idx] = &state{}
					order = append(order, idx)
				}
			}
		}
		for _, it := range done {
			st := states[it.Index]
			if st == nil || st.done {
				continue
			}
			info, err := os.Stat(it.Filepath)
			if err != nil || !strings.HasPrefix(it.Filepath, out+string(filepath.Separator)) {
				continue
			}
			if info.Size() > maxBytes {
				if it.Height > 0 {
					st.got = it.Height
				}
				continue
			}
			data, err := os.ReadFile(it.Filepath)
			if err != nil {
				return nil, nil, fmt.Errorf("I cannot read the video file: %v", err) //nolint:staticcheck // shown to the user
			}
			title := cmp.Or(strings.TrimSpace(truncate(it.Title, 200)), "Video")
			res := &videoResult{index: it.Index, title: title, name: fileName(title) + filepath.Ext(it.Filepath),
				height: it.Height, reduced: step > 0, data: data}
			if isTwitterGIF(it.URL) {
				b.toGIF(ctx, res, it.Filepath, maxBytes)
			}
			st.done = true
			results = append(results, res)
		}
	}
	var failed []int
	for _, idx := range order {
		if !states[idx].done {
			failed = append(failed, idx)
		}
	}
	if len(results) == 0 {
		return nil, nil, fmt.Errorf("All qualities of this video are larger than the upload limit (%s).", //nolint:staticcheck // shown to the user
			humanSize(maxBytes+downloadMargin))
	}
	slices.SortFunc(results, func(a, b *videoResult) int { return a.index - b.index })
	return results, failed, nil
}

// downloadArgs are the yt-dlp options to download link into the folder out,
// at most height pixels high (0: the best), and at most maxBytes for each
// file. items selects list items (--playlist-items), or "" for all. yt-dlp
// prints the lines that parseYtdlp reads. It prefers H.264 video and AAC
// audio, because some Discord clients play only the sound of AV1 or VP9
// video. --no-playlist keeps a YouTube link with a list to one video; X
// ignores it when a post has more than one video, unless the link names one
// (/video/2). The cache is in the temporary folder, because the service
// sandbox does not let the bot write its home folder. yt-dlp sends the
// cookies in the cookies file (if any) only to the sites they are for, and
// saves changed cookies back to the file.
func downloadArgs(link, out string, height int, maxBytes int64, cookies, items string) []string {
	format := "bv*+ba/b"
	if height > 0 {
		format = fmt.Sprintf("bv*[height<=?%d]+ba/b[height<=?%d]", height, height)
	}
	var args []string
	if cookies != "" {
		args = []string{"--cookies", cookies}
	}
	if items != "" {
		args = append(args, "--playlist-items", items)
	}
	return append(args,
		"--ignore-config", "--no-playlist", "--ignore-errors", "--no-progress", "--no-mtime",
		"--use-extractors", "default,-generic",
		"--match-filter", "!is_live",
		"--cache-dir", filepath.Join(os.TempDir(), "hcbot-yt-dlp-cache"),
		"-f", format, "-S", "vcodec:h264,res,acodec:aac,ext:mp4:m4a", "--merge-output-format", "mp4",
		"--max-filesize", strconv.FormatInt(maxBytes, 10),
		"-o", filepath.Join(out, "video-%(playlist_index|0)03d.%(ext)s"),
		"--no-simulate", "--print", "video:S %(playlist_index|0)s",
		"--print", "after_move:D %(.{playlist_index,height,url,title,filepath})j",
		"--", link,
	)
}

// isTwitterGIF reports whether a media address is a GIF on X. X keeps a
// GIF as an MP4 in tweet_video; videos are in ext_tw_video or amplify_video.
func isTwitterGIF(mediaURL string) bool {
	u, err := url.Parse(strings.TrimSpace(mediaURL))
	return err == nil && u.Hostname() == "video.twimg.com" && strings.HasPrefix(u.Path, "/tweet_video/")
}

// toGIF changes res (an MP4 at path) to a GIF of at most maxBytes, at the
// largest of gifSteps that fits. If no GIF fits, or ffmpeg fails, res stays
// an MP4 and gets a note.
func (b *Bot) toGIF(ctx context.Context, res *videoResult, path string, maxBytes int64) {
	out := strings.TrimSuffix(path, filepath.Ext(path)) + ".gif"
	for i, st := range gifSteps {
		_, stderr, err := b.runTool(ctx, "ffmpeg", gifArgs(path, out, st.width, st.fps))
		if err != nil {
			log.Printf("download: ffmpeg GIF: %v: %s", err, truncate(strings.TrimSpace(string(stderr)), 500))
			res.note = "This is a GIF on X, but I could not make a GIF file, so this is the MP4."
			return
		}
		data, err := os.ReadFile(out)
		if err != nil {
			log.Printf("download: read GIF: %v", err)
			res.note = "This is a GIF on X, but I could not make a GIF file, so this is the MP4."
			return
		}
		if int64(len(data)) <= maxBytes {
			res.data, res.gif, res.reduced = data, true, i > 0
			res.name = strings.TrimSuffix(res.name, filepath.Ext(res.name)) + ".gif"
			return
		}
	}
	res.note = "This is a GIF on X, but the GIF file was larger than the upload limit, so this is the MP4."
}

// gifArgs are the ffmpeg options to make a GIF from the video in, at most
// width pixels wide and fps frames each second, with a palette made for
// the clip. The GIF repeats without end, the same as on X.
func gifArgs(in, out string, width, fps int) []string {
	filter := fmt.Sprintf("fps=%d,scale='min(%d,iw)':-1:flags=lanczos,split[a][b];"+
		"[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle", fps, width)
	return []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", in, "-vf", filter, "-loop", "0", out}
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
