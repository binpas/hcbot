package bot

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	_ "image/gif" // animated pictures: the first frame
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	_ "golang.org/x/image/webp"

	"github.com/binpas/hcbot/internal/comic"
)

// Comics: /comic draws the last messages of a channel as a comic strip, in
// the style of Microsoft Comic Chat (internal/comic). "Start comic here"
// opens a private editor with the selected message and the next ones,
// where the member can remove messages, pick a background, and preview the
// comic before it is posted. "@Bot comic [count]" works like /comic.
// /character sets the member's own character. The first picture of a
// message (an image file, a sticker, or a GIF or image link) goes in its
// balloon; a spoiler picture is a "spoiler" box.
//
// The commands need the Message Content option (the comic shows the text
// of the messages) and the art pack in COMIC_ART_DIR. The bot keeps no
// message text: the editor keeps only message IDs, and reads the messages
// again when it draws.

const (
	comicDefaultCount = 4
	comicHardMax      = 20
	comicEditorMax    = 10               // the messages "Start comic here" takes
	comicEditorFor    = 14 * time.Minute // the interaction token lasts 15
	comicTextMax      = 300              // characters of one message in a comic
	comicAvatarFor    = 10 * time.Minute // how long a profile picture is cached
	comicFetchMax     = 8 << 20          // the largest picture file
	comicPicPixels    = 4096 * 4096      // the largest picture, in pixels
	comicPicWidth     = 480              // pictures are fetched at most this wide
	attachmentSpoiler = 1 << 3           // the IS_SPOILER attachment flag
	comicMenu         = "Start comic here"
	comicRandom       = "random"
)

// comicState is the art pack, the cooldowns, and the profile picture cache.
// /comicart reload replaces the pack while comics are drawn.
type comicState struct {
	pack atomic.Pointer[comic.Pack]

	mu      sync.Mutex
	last    map[string]time.Time
	avatars map[string]comicAvatar // by URL
}

type comicAvatar struct {
	img image.Image
	at  time.Time
}

// comicDraft is the state of a "Start comic here" editor.
type comicDraft struct {
	Channel    string   `json:"channel"`
	IDs        []string `json:"ids"`
	Removed    []string `json:"removed,omitempty"`
	Background string   `json:"background,omitempty"`
}

func (b *Bot) registerComics() {
	if !b.env.MessageContent {
		return
	}
	pack, err := comic.LoadPack(b.env.ComicArtDir)
	if errors.Is(err, fs.ErrNotExist) {
		log.Printf("Comics are off: no comic art in %s (see deploy/comic-art.sh)", b.env.ComicArtDir)
		return
	}
	if err != nil {
		log.Printf("Comics are off: comic art: %v", err)
		return
	}
	b.comics = &comicState{last: map[string]time.Time{}, avatars: map[string]comicAvatar{}}
	b.comics.pack.Store(pack)
	logComicProblems(pack)
	b.addCommand(&command{
		name:        "comic",
		description: "Draw the last messages of this channel as a comic.",
		options: []*discordgo.ApplicationCommandOption{{
			Type: discordgo.ApplicationCommandOptionInteger, Name: "count",
			Description: fmt.Sprintf("How many messages (default %d)", comicDefaultCount),
			MinValue:    new(float64(1)), MaxValue: comicHardMax,
		}},
		handler: b.cmdComic,
	})
	b.addCommand(&command{
		name:    comicMenu,
		kind:    discordgo.MessageApplicationCommand,
		handler: b.cmdComicHere,
	})
	b.addCommand(&command{
		name:        "character",
		description: "Show or set your character in comics.",
		options: []*discordgo.ApplicationCommandOption{{
			Type: discordgo.ApplicationCommandOptionString, Name: "name",
			Description: "The character to use. Leave it out to see yours.", Autocomplete: true,
		}},
		handler:      b.cmdCharacter,
		autocomplete: b.autocompleteCharacter,
	})
	b.addGroup(&group{name: "comicart", description: "Manage the comic art (admin only).",
		defaultPerm: int64(discordgo.PermissionAdministrator)})
	b.addCommand(&command{
		name: "comicart reload", description: "Read the comic art again, with your custom characters and backgrounds.",
		perm: permAdmin, handler: b.cmdComicArtReload,
	})
	for id, h := range map[string]func(*Ctx){
		"comicpick": b.onComicPick, "comicbg": b.onComicBackground, "comicpre": b.onComicPreview,
		"comicpost": b.onComicPost, "comicno": b.onComicCancel,
		"charall": b.onCharacterAll, "charreset": b.onCharacterReset,
	} {
		b.addComponent(id, h)
	}
	b.addModal("comicmsgs", b.onComicMessages)
}

// comicMax is COMIC_MAX_MESSAGES, from 2 to comicHardMax.
func (b *Bot) comicMax() int { return min(max(b.cfg.Int("COMIC_MAX_MESSAGES"), 2), comicHardMax) }

// comicChannelOK checks COMIC_ENABLED and COMIC_CHANNELS (for a thread, its
// parent). It returns the reason when comics do not work there.
func (b *Bot) comicChannelOK(channelID string) string {
	if !b.cfg.Bool("COMIC_ENABLED") {
		return "Comics are off (`COMIC_ENABLED` in `/setup`)."
	}
	if chans := b.cfg.List("COMIC_CHANNELS"); len(chans) > 0 && !slices.Contains(chans, b.parentOf(channelID).ID) {
		return fmt.Sprintf("Comics do not work in <#%s> (`COMIC_CHANNELS` in `/setup`).", channelID)
	}
	return ""
}

// comicWait returns how long the member must wait before the next comic,
// or 0. take marks a new comic when the member need not wait.
func (b *Bot) comicWait(userID string, take bool) time.Duration {
	st := b.comics
	st.mu.Lock()
	defer st.mu.Unlock()
	cooldown := time.Duration(max(b.cfg.Int("COMIC_COOLDOWN"), 0)) * time.Second
	if t, ok := st.last[userID]; ok && time.Since(t) < cooldown {
		return cooldown - time.Since(t)
	}
	if take {
		st.last[userID] = time.Now()
	}
	return 0
}

func waitMessage(d time.Duration) string {
	return fmt.Sprintf("Wait %s before you make another comic.", humaniseDuration(int64((d+time.Second-1)/time.Second)))
}

// /comic: the last messages of the channel.
func (b *Bot) cmdComic(c *Ctx) {
	if why := b.comicChannelOK(c.i.ChannelID); why != "" {
		c.Reply(true, errEmbed(why))
		return
	}
	n := b.comicCount(int(c.Int("count", comicDefaultCount)))
	if d := b.comicWait(c.UserID(), true); d > 0 {
		c.Reply(true, errEmbed(waitMessage(d)))
		return
	}
	c.Defer(false)
	start := time.Now()
	data, err := b.lastComic(c.GuildID(), c.i.ChannelID, "", n)
	b.comicDone("slash", start, err)
	if err != nil {
		c.Reply(true, errEmbed(comicFailure("comic", err)))
		return
	}
	_, err = c.s.InteractionResponseEdit(c.i.Interaction, &discordgo.WebhookEdit{Files: comicFile("comic.png", data)})
	c.logErr("comic: post", err)
}

// comicCount is a requested number of messages, in the limits.
func (b *Bot) comicCount(n int) int { return min(max(n, 1), b.comicMax()) }

// errComicRead is the error when the messages of a channel cannot be read.
var errComicRead = errors.New("read the messages")

// comicDone counts a comic in the metrics, by source and result, and
// notes the time of a comic that was drawn.
func (b *Bot) comicDone(source string, start time.Time, err error) {
	result := "ok"
	switch {
	case errors.Is(err, comic.ErrNoMessages):
		result = "no_messages"
	case errors.Is(err, errComicRead):
		result = "read_error"
	case err != nil:
		result = "error"
	default:
		b.m.comicSeconds.Observe(time.Since(start).Seconds())
	}
	b.m.comics.WithLabelValues(source, result).Inc()
}

// comicFailure logs a comic error (but not "no messages") and returns the
// text for the user.
func comicFailure(what string, err error) string {
	switch {
	case errors.Is(err, comic.ErrNoMessages):
		return "There are no messages with text to draw."
	case errors.Is(err, errComicRead):
		log.Printf("%s: %v", what, err)
		return "I could not read the messages of this channel."
	}
	log.Printf("%s: %v", what, err)
	return "I could not draw the comic."
}

func comicFile(name string, data []byte) []*discordgo.File {
	return []*discordgo.File{{Name: name, ContentType: "image/png", Reader: bytes.NewReader(data)}}
}

// comicMention handles "@Bot comic [count]". It reports whether the
// message was such a request.
func (b *Bot) comicMention(m *discordgo.Message) bool {
	if b.comics == nil || b.mentionKeyword(m) != "comic" {
		return false
	}
	reply := func(text string) {
		if _, err := b.s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content: text, Reference: m.Reference(), AllowedMentions: &discordgo.MessageAllowedMentions{},
		}); err != nil {
			log.Printf("comic: reply: %v", err)
		}
	}
	if why := b.comicChannelOK(m.ChannelID); why != "" {
		reply(why)
		return true
	}
	n := comicDefaultCount
	if f := strings.Fields(m.Content); len(f) > 2 {
		if v, err := strconv.Atoi(f[2]); err == nil {
			n = v
		}
	}
	n = b.comicCount(n)
	if d := b.comicWait(m.Author.ID, true); d > 0 {
		reply(waitMessage(d))
		return true
	}
	_ = b.s.ChannelTyping(m.ChannelID)
	start := time.Now()
	data, err := b.lastComic(m.GuildID, m.ChannelID, m.ID, n)
	b.comicDone("mention", start, err)
	if err != nil {
		reply(comicFailure("comic", err))
		return true
	}
	if _, err := b.s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Files: comicFile("comic.png", data), Reference: m.Reference(), AllowedMentions: &discordgo.MessageAllowedMentions{},
	}); err != nil {
		log.Printf("comic: post: %v", err)
	}
	return true
}

// lastComic draws the last n messages with text of a channel, before the
// message before (or the newest ones). It is /comic and "@Bot comic".
func (b *Bot) lastComic(guildID, channelID, before string, n int) ([]byte, error) {
	msgs, err := b.s.ChannelMessages(channelID, min(n*3, 100), before, "", "")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errComicRead, err)
	}
	msgs = comicUsable(msgs)
	return b.drawComic(guildID, msgs[max(len(msgs)-n, 0):], "")
}

// comicUsable sorts messages oldest first, and keeps the normal messages
// and replies that have text or a picture.
func comicUsable(msgs []*discordgo.Message) []*discordgo.Message {
	out := slices.DeleteFunc(slices.Clone(msgs), func(m *discordgo.Message) bool {
		if m == nil || m.Author == nil || (m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply) {
			return true
		}
		_, pic := comicPicture(m)
		return strings.TrimSpace(m.Content) == "" && !pic
	})
	slices.SortFunc(out, func(a, b *discordgo.Message) int { return cmpSnowflake(a.ID, b.ID) })
	return out
}

func cmpSnowflake(a, b string) int {
	if len(a) != len(b) {
		return cmp.Compare(len(a), len(b))
	}
	return strings.Compare(a, b)
}

// drawComic makes the comic of msgs (oldest first): the text, the names,
// the profile pictures, and the chosen characters.
func (b *Bot) drawComic(guildID string, msgs []*discordgo.Message, background string) ([]byte, error) {
	if len(msgs) == 0 {
		return nil, comic.ErrNoMessages
	}
	var ids []string
	for _, m := range msgs {
		if !slices.Contains(ids, m.Author.ID) {
			ids = append(ids, m.Author.ID)
		}
	}
	chosen, err := b.db.ComicCharacters(guildID, ids)
	if err != nil {
		log.Printf("comic: characters: %v", err) // draw with picked ones
	}
	var avatars, pics map[string]image.Image
	var wg sync.WaitGroup
	wg.Go(func() { avatars = b.comicAvatars(guildID, msgs) })
	wg.Go(func() { pics = b.comicPictures(msgs) })
	wg.Wait()
	h := fnv.New64a()
	out := make([]comic.Message, 0, len(msgs))
	for _, m := range msgs {
		_, _ = h.Write([]byte(m.ID))
		pic, hasPic := comicPicture(m)
		cm := comic.Message{
			Speaker:   m.Author.ID,
			Name:      b.comicName(guildID, m.Author),
			Avatar:    avatars[m.Author.ID],
			Character: chosen[m.Author.ID],
			Text:      cutText(b.comicText(guildID, m, pic), comicTextMax),
			Image:     pics[m.ID],
			Spoiler:   pic.spoiler,
		}
		if hasPic && !pic.spoiler && cm.Image == nil {
			cm.Text = strings.TrimSpace(cm.Text + " 🖼️ image") // it could not be fetched
		}
		out = append(out, cm)
	}
	if background == comicRandom {
		background = ""
	}
	return b.comics.pack.Load().PNG(out, comic.Options{Background: background, Seed: h.Sum64()})
}

// comicName is the name a member has on the server: the nickname, the
// display name, or the user name.
func (b *Bot) comicName(guildID string, u *discordgo.User) string {
	if !u.Bot && u.Discriminator != "0000" { // webhooks have no member
		if nick := b.memberNick(guildID, u.ID); nick != "" {
			return nick
		}
	}
	return cmp.Or(u.GlobalName, u.Username)
}

// comicAvatars gets the profile picture of each author, at the same time.
// A picture that fails is nil (the comic draws a grey circle).
func (b *Bot) comicAvatars(guildID string, msgs []*discordgo.Message) map[string]image.Image {
	urls := map[string]string{}
	for _, m := range msgs {
		url := m.Author.AvatarURL("64")
		if m.Member != nil && m.Member.Avatar != "" {
			m.Member.GuildID = guildID
			m.Member.User = m.Author
			url = m.Member.AvatarURL("64")
		}
		urls[m.Author.ID] = url
	}
	return fetchAll(urls, b.comicAvatar)
}

// fetchAll gets the picture of each URL at the same time, by key. A
// picture that fails (nil) is missing.
func fetchAll(urls map[string]string, fetch func(url string) image.Image) map[string]image.Image {
	out := map[string]image.Image{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for key, url := range urls {
		wg.Go(func() {
			if img := fetch(url); img != nil {
				mu.Lock()
				out[key] = img
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return out
}

// comicPic is the first picture of a message.
type comicPic struct {
	url     string
	spoiler bool
	link    string // the link in the text that shows the picture
}

// comicPicture finds the first picture of a message: an image file, a
// sticker (not an animated Lottie one), or a GIF or image link. Link
// previews of pages are not pictures.
func comicPicture(m *discordgo.Message) (comicPic, bool) {
	for _, a := range m.Attachments {
		if a == nil || !isImageFile(a) {
			continue
		}
		return comicPic{
			url:     sizedProxy(cmp.Or(a.ProxyURL, a.URL), a.Width, a.Height),
			spoiler: strings.HasPrefix(a.Filename, "SPOILER_") || a.Flags&attachmentSpoiler != 0,
		}, true
	}
	for _, s := range m.StickerItems {
		ext := map[discordgo.StickerFormat]string{discordgo.StickerFormatTypePNG: ".png",
			discordgo.StickerFormatTypeAPNG: ".png", discordgo.StickerFormatTypeGIF: ".gif"}[s.FormatType]
		if s != nil && ext != "" {
			return comicPic{url: "https://media.discordapp.net/stickers/" + s.ID + ext + "?size=160"}, true
		}
	}
	for _, e := range m.Embeds {
		if e == nil || (e.Type != discordgo.EmbedTypeGifv && e.Type != discordgo.EmbedTypeImage) || e.Thumbnail == nil {
			continue
		}
		t := e.Thumbnail
		if src := cmp.Or(t.ProxyURL, t.URL); src != "" {
			return comicPic{url: sizedProxy(src, t.Width, t.Height), link: e.URL, spoiler: inSpoiler(m.Content, e.URL)}, true
		}
	}
	return comicPic{}, false
}

func isImageFile(a *discordgo.MessageAttachment) bool {
	if strings.HasPrefix(a.ContentType, "image/") {
		return true
	}
	ext := strings.ToLower(a.Filename[strings.LastIndex(a.Filename, ".")+1:])
	return slices.Contains([]string{"png", "jpg", "jpeg", "gif", "webp"}, ext)
}

// sizedProxy asks Discord's media server for a picture at most
// comicPicWidth wide.
func sizedProxy(url string, w, h int) string {
	if w <= comicPicWidth || h <= 0 || !strings.Contains(url, "media.discordapp.net") {
		return url
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%swidth=%d&height=%d", url, sep, comicPicWidth, max(h*comicPicWidth/w, 1))
}

// inSpoiler reports whether a link is inside ||spoiler|| marks.
func inSpoiler(text, link string) bool {
	if link == "" {
		return false
	}
	parts := strings.Split(text, "||")
	for i := 1; i < len(parts)-1; i += 2 {
		if strings.Contains(parts[i], link) {
			return true
		}
	}
	return false
}

// comicText is the text of a message for its balloon: the link of its
// picture is removed, and the rest is made readable (spokenText).
func (b *Bot) comicText(guildID string, m *discordgo.Message, pic comicPic) string {
	if pic.link != "" {
		cp := *m
		cp.Content = strings.ReplaceAll(strings.ReplaceAll(m.Content, pic.link, ""), "||||", "")
		m = &cp
	}
	return b.spokenText(guildID, m)
}

// comicPictures gets the pictures of the messages that have one (not the
// spoilers), at the same time, by message ID. A picture that fails is
// missing.
func (b *Bot) comicPictures(msgs []*discordgo.Message) map[string]image.Image {
	urls := map[string]string{}
	for _, m := range msgs {
		if pic, ok := comicPicture(m); ok && !pic.spoiler {
			urls[m.ID] = pic.url
		}
	}
	return fetchAll(urls, func(url string) image.Image {
		img, err := b.fetchPicture(url)
		if err != nil {
			log.Printf("comic: picture: %v", err)
		}
		return img
	})
}

// fetchPicture gets and decodes a picture (PNG, JPEG, GIF, or WebP). It
// refuses a picture with too many pixels.
func (b *Bot) fetchPicture(url string) (image.Image, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := b.fetchImage(ctx, url)
	if err != nil {
		return nil, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > comicPicPixels {
		return nil, fmt.Errorf("%dx%d is too large", cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}

func (b *Bot) comicAvatar(url string) image.Image {
	st := b.comics
	st.mu.Lock()
	if a, ok := st.avatars[url]; ok && time.Since(a.at) < comicAvatarFor {
		st.mu.Unlock()
		return a.img
	}
	st.mu.Unlock()
	img, err := b.fetchPicture(url)
	if err != nil {
		log.Printf("comic: profile picture: %v", err)
		return nil // not cached: the next comic tries again
	}
	st.mu.Lock()
	for k, a := range st.avatars {
		if time.Since(a.at) >= comicAvatarFor {
			delete(st.avatars, k)
		}
	}
	st.avatars[url] = comicAvatar{img: img, at: time.Now()}
	st.mu.Unlock()
	return img
}

// httpFetch gets a small file (a picture from Discord's media server).
func httpFetch(client *http.Client) func(ctx context.Context, url string) ([]byte, error) {
	return func(ctx context.Context, url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", url, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, comicFetchMax))
	}
}

// "Start comic here": the editor.

func (b *Bot) cmdComicHere(c *Ctx) {
	msg := c.TargetMessage()
	if msg == nil {
		return
	}
	if why := b.comicChannelOK(c.i.ChannelID); why != "" {
		c.Reply(true, errEmbed(why))
		return
	}
	limit := min(comicEditorMax, b.comicMax())
	after, err := b.s.ChannelMessages(c.i.ChannelID, limit-1, "", msg.ID, "")
	if err != nil {
		c.logErr("comic: messages", err)
		c.Reply(true, errEmbed("I could not read the messages of this channel."))
		return
	}
	msg.ChannelID = c.i.ChannelID
	msgs := comicUsable(append([]*discordgo.Message{msg}, after...))
	if len(msgs) > limit {
		msgs = msgs[:limit]
	}
	if len(msgs) == 0 {
		c.Reply(true, errEmbed("There are no messages with text to draw here."))
		return
	}
	d := comicDraft{Channel: c.i.ChannelID}
	for _, m := range msgs {
		d.IDs = append(d.IDs, m.ID)
	}
	data, _ := json.Marshal(d)
	n, err := b.db.SavePending("comic", c.UserID(), string(data), time.Now().Add(comicEditorFor))
	if err != nil {
		c.dbFailed("save comic", err)
		return
	}
	id := strconv.FormatInt(n, 10)
	err = c.s.InteractionRespond(c.i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags:      discordgo.MessageFlagsEphemeral | discordgo.MessageFlagsIsComponentsV2,
			Components: b.comicEditor(c.GuildID(), id, d, msgs, false),
		},
	})
	c.markResponded()
	c.logErr("comic: editor", err)
}

// comicEditor is the editor message: one line for each message with a
// Remove (or Add back) button, the background, and Preview, Post, and
// Cancel. preview adds the image "preview.png".
func (b *Bot) comicEditor(guildID, id string, d comicDraft, msgs []*discordgo.Message, preview bool) []discordgo.MessageComponent {
	parts := []discordgo.MessageComponent{discordgo.TextDisplay{Content: "### 🎞️ Comic editor\n" +
		"**Choose messages** shows a checkbox for each message. **Preview** shows the comic only to you. **Post** sends it to the channel."}}
	kept := 0
	var lines []string
	for _, m := range msgs {
		pic, hasPic := comicPicture(m)
		text := cutText(b.comicText(guildID, m, pic), 150)
		switch {
		case hasPic && pic.spoiler:
			text = strings.TrimSpace(text + " 🖼️ spoiler")
		case hasPic:
			text = strings.TrimSpace(text + " 🖼️ image")
		}
		name := b.comicName(guildID, m.Author)
		line := fmt.Sprintf("**%s**: %s", escapeMarkdown(name), escapeMarkdown(text))
		removed := slices.Contains(d.Removed, m.ID)
		if removed {
			line = "~~" + line + "~~"
		} else {
			kept++
		}
		lines = append(lines, line)
	}
	if len(msgs) == 0 {
		parts = append(parts, discordgo.TextDisplay{Content: "-# The messages were deleted."})
	} else {
		parts = append(parts, discordgo.TextDisplay{Content: strings.Join(lines, "\n")})
	}
	options := []discordgo.SelectMenuOption{{Label: "Random background", Value: comicRandom, Default: d.Background == ""}}
	for _, name := range b.comics.pack.Load().Backgrounds() {
		if len(options) == 25 { // a Discord limit
			break
		}
		options = append(options, discordgo.SelectMenuOption{Label: name, Value: name, Default: d.Background == name})
	}
	parts = append(parts,
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
			MenuType: discordgo.StringSelectMenu, CustomID: "comicbg:" + id, Options: options}}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Choose messages", Style: discordgo.SecondaryButton, CustomID: "comicpick:" + id, Disabled: len(msgs) == 0},
			discordgo.Button{Label: "Preview", Style: discordgo.SecondaryButton, CustomID: "comicpre:" + id, Disabled: kept == 0},
			discordgo.Button{Label: "Post", Style: discordgo.PrimaryButton, CustomID: "comicpost:" + id, Disabled: kept == 0},
			discordgo.Button{Label: "Cancel", Style: discordgo.DangerButton, CustomID: "comicno:" + id},
		}},
	)
	if preview {
		parts = append(parts, discordgo.MediaGallery{Items: []discordgo.MediaGalleryItem{{
			Media: discordgo.UnfurledMediaItem{URL: "attachment://preview.png"}}}})
	}
	return []discordgo.MessageComponent{discordgo.Container{Components: parts}}
}

func escapeMarkdown(s string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, ">", `\>`, "#", `\#`).Replace(s)
}

// comicNotice is an editor that only shows a line of text.
func comicNotice(text string) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.Container{Components: []discordgo.MessageComponent{discordgo.TextDisplay{Content: text}}}}
}

// updateComic replaces the editor message and removes its image. The
// editor's handlers defer first (loadComic), so a slow answer cannot fail.
func (c *Ctx) updateComic(components []discordgo.MessageComponent) {
	c.editComic(components, nil)
}

// editComic replaces the editor message after DeferUpdate, with an
// optional new image.
func (c *Ctx) editComic(components []discordgo.MessageComponent, files []*discordgo.File) {
	attachments := []*discordgo.MessageAttachment{}
	for i, f := range files {
		attachments = append(attachments, &discordgo.MessageAttachment{ID: strconv.Itoa(i), Filename: f.Name})
	}
	_, err := c.s.InteractionResponseEdit(c.i.Interaction, &discordgo.WebhookEdit{
		Components: &components, Files: files, Attachments: &attachments})
	c.logErr("comic: edit", err)
}

// loadComic defers the answer to an editor click, then reads its draft. It
// answers and returns false when the draft is gone.
func (b *Bot) loadComic(c *Ctx) (string, comicDraft, bool) {
	var d comicDraft
	if b.comics == nil || len(c.Args) < 1 {
		return "", d, false
	}
	c.DeferUpdate()
	d, ok := b.readComic(c, c.Args[0])
	if !ok {
		c.updateComic(comicNotice("This comic editor has expired. Use **" + comicMenu + "** again."))
		return "", d, false
	}
	return c.Args[0], d, true
}

// readComic reads the draft of an editor, when it is the user's and is
// not expired.
func (b *Bot) readComic(c *Ctx, id string) (comicDraft, bool) {
	var d comicDraft
	n, _ := strconv.ParseInt(id, 10, 64)
	payload, ok, err := b.db.LoadPending(n, "comic", c.UserID())
	if err == nil && ok {
		err = json.Unmarshal([]byte(payload), &d)
	}
	if err != nil {
		c.logErr("comic: load", err)
	}
	return d, err == nil && ok
}

func (b *Bot) saveComic(c *Ctx, id string, d comicDraft) bool {
	data, _ := json.Marshal(d)
	n, _ := strconv.ParseInt(id, 10, 64)
	if err := b.db.UpdatePending(n, string(data), time.Now().Add(comicEditorFor)); err != nil {
		c.logErr("comic: save", err)
		c.updateComic(comicNotice("Something went wrong. Try again."))
		return false
	}
	return true
}

// draftMessages reads the editor's messages again (one request: they are
// next to each other), in order. Deleted ones are gone.
func (b *Bot) draftMessages(d comicDraft) ([]*discordgo.Message, error) {
	if len(d.IDs) == 0 {
		return nil, nil
	}
	last, err := strconv.ParseUint(d.IDs[len(d.IDs)-1], 10, 64)
	if err != nil {
		return nil, err
	}
	msgs, err := b.s.ChannelMessages(d.Channel, 100, strconv.FormatUint(last+1, 10), "", "")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errComicRead, err)
	}
	msgs = slices.DeleteFunc(comicUsable(msgs), func(m *discordgo.Message) bool { return !slices.Contains(d.IDs, m.ID) })
	return msgs, nil
}

func keptMessages(d comicDraft, msgs []*discordgo.Message) []*discordgo.Message {
	return slices.DeleteFunc(slices.Clone(msgs), func(m *discordgo.Message) bool { return slices.Contains(d.Removed, m.ID) })
}

// redrawComic answers a click with the editor for the changed draft.
func (b *Bot) redrawComic(c *Ctx, id string, d comicDraft) {
	msgs, err := b.draftMessages(d)
	if err != nil {
		c.logErr("comic: messages", err)
		c.updateComic(comicNotice("I could not read the messages. Try again."))
		return
	}
	c.updateComic(b.comicEditor(c.GuildID(), id, d, msgs, false))
}

// onComicPick opens the "Choose messages" form: a checkbox for each
// message, checked when it is in the comic. A form must be the first
// answer, so this click is not deferred.
func (b *Bot) onComicPick(c *Ctx) {
	if b.comics == nil || len(c.Args) < 1 {
		return
	}
	id := c.Args[0]
	d, ok := b.readComic(c, id)
	if !ok {
		c.Reply(true, errEmbed("This comic editor has expired. Use **"+comicMenu+"** again."))
		return
	}
	msgs, err := b.draftMessages(d)
	if err != nil || len(msgs) == 0 {
		c.logErr("comic: messages", err)
		c.Reply(true, errEmbed("I could not read the messages. Try again."))
		return
	}
	var boxes []discordgo.SelectMenuOption
	for _, m := range msgs[:min(len(msgs), 10)] { // a Discord limit
		pic, hasPic := comicPicture(m)
		text := b.comicText(c.GuildID(), m, pic)
		if hasPic {
			text = strings.TrimSpace(text + " 🖼️")
		}
		// A checkbox shows plain text, up to 100 characters.
		boxes = append(boxes, discordgo.SelectMenuOption{Label: cutText(b.comicName(c.GuildID(), m.Author)+": "+text, 100),
			Value: m.ID, Default: !slices.Contains(d.Removed, m.ID)})
	}
	c.Modal("comicmsgs:"+id, "Choose messages", modalLabel{Label: "Messages in the comic",
		Description: "Uncheck the messages that you do not want.", Component: modalCheckboxes{CustomID: "msgs", Options: boxes}})
}

// onComicMessages is the "Choose messages" form: the unchecked messages
// are removed.
func (b *Bot) onComicMessages(c *Ctx) {
	id, d, ok := b.loadComic(c)
	if !ok {
		return
	}
	checked, sent := c.fields["msgs"]
	if !sent {
		b.redrawComic(c, id, d)
		return
	}
	d.Removed = slices.DeleteFunc(slices.Clone(d.IDs), func(id string) bool { return slices.Contains(checked, id) })
	if b.saveComic(c, id, d) {
		b.redrawComic(c, id, d)
	}
}

func (b *Bot) onComicBackground(c *Ctx) {
	id, d, ok := b.loadComic(c)
	if !ok {
		return
	}
	d.Background = ""
	if v := c.Values(); len(v) == 1 && v[0] != comicRandom && slices.Contains(b.comics.pack.Load().Backgrounds(), v[0]) {
		d.Background = v[0]
	}
	if b.saveComic(c, id, d) {
		b.redrawComic(c, id, d)
	}
}

// comicImage reads the draft's messages and draws the kept ones.
func (b *Bot) comicImage(c *Ctx, d comicDraft) ([]*discordgo.Message, []byte, error) {
	msgs, err := b.draftMessages(d)
	if err != nil {
		return nil, nil, err
	}
	data, err := b.drawComic(c.GuildID(), keptMessages(d, msgs), d.Background)
	return msgs, data, err
}

func (b *Bot) onComicPreview(c *Ctx) {
	id, d, ok := b.loadComic(c)
	if !ok {
		return
	}
	start := time.Now()
	msgs, data, err := b.comicImage(c, d)
	b.comicDone("preview", start, err)
	if err != nil {
		c.editComic(append(b.comicEditor(c.GuildID(), id, d, msgs, false),
			discordgo.TextDisplay{Content: "-# " + comicFailure("comic: preview", err)}), nil)
		return
	}
	c.editComic(b.comicEditor(c.GuildID(), id, d, msgs, true), comicFile("preview.png", data))
}

func (b *Bot) onComicPost(c *Ctx) {
	id, d, ok := b.loadComic(c)
	if !ok {
		return
	}
	if why := b.comicChannelOK(d.Channel); why != "" {
		c.updateComic(comicNotice(why))
		return
	}
	if w := b.comicWait(c.UserID(), false); w > 0 {
		msgs, err := b.draftMessages(d)
		if err != nil {
			c.updateComic(comicNotice(waitMessage(w)))
			return
		}
		c.updateComic(append(b.comicEditor(c.GuildID(), id, d, msgs, false), discordgo.TextDisplay{Content: "-# " + waitMessage(w)}))
		return
	}
	start := time.Now()
	_, data, err := b.comicImage(c, d)
	b.comicDone("post", start, err)
	if err != nil {
		c.editComic(comicNotice(comicFailure("comic: post", err)), nil)
		return
	}
	_, err = b.s.ChannelMessageSendComplex(d.Channel, &discordgo.MessageSend{
		Content: "-# Comic by " + userMention(c.UserID()), Files: comicFile("comic.png", data),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	if err != nil {
		c.logErr("comic: post", err)
		c.editComic(comicNotice(fmt.Sprintf("I could not post in <#%s>. I need Send Messages and Attach Files there.", d.Channel)), nil)
		return
	}
	b.comicWait(c.UserID(), true)
	n, _ := strconv.ParseInt(id, 10, 64)
	_ = b.db.DeletePending(n)
	c.editComic(comicNotice(fmt.Sprintf("✅ Posted the comic in <#%s>.", d.Channel)), nil)
}

func (b *Bot) onComicCancel(c *Ctx) {
	id, _, ok := b.loadComic(c)
	if !ok {
		return
	}
	n, _ := strconv.ParseInt(id, 10, 64)
	_ = b.db.DeletePending(n)
	c.updateComic(comicNotice("Cancelled."))
}

// /character

func (b *Bot) cmdCharacter(c *Ctx) {
	pack := b.comics.pack.Load()
	name, set := c.Str("name")
	name = strings.ToLower(strings.TrimSpace(name))
	text := ""
	switch {
	case set && !pack.CanUse(name, c.UserID()): // a private one looks missing
		c.ReplyComplex(true, []*discordgo.MessageEmbed{errEmbed(fmt.Sprintf("There is no character named `%s`.", cutText(name, 40)))},
			characterButtons())
		return
	case set:
		if err := b.db.SetComicCharacter(c.GuildID(), c.UserID(), name); err != nil {
			c.dbFailed("set character", err)
			return
		}
		text = fmt.Sprintf("Your character in comics is now **%s**.", name)
	default:
		cur, err := b.db.ComicCharacter(c.GuildID(), c.UserID())
		if err != nil {
			c.dbFailed("get character", err)
			return
		}
		name = cur
		text = fmt.Sprintf("Your character in comics is **%s**. Use `/character name:` to change it.", name)
		if !pack.CanUse(name, c.UserID()) {
			name = pack.CharacterFor(c.UserID())
			text = fmt.Sprintf("You have not chosen a character, so comics give you one. Usually it is **%s**. Use `/character name:` to choose one.", name)
		}
	}
	data, err := pack.PortraitPNG(name)
	if err != nil {
		c.logErr("character: draw", err)
		c.Reply(true, makeEmbed("🎭 Your character", text, colourBlurple))
		return
	}
	e := makeEmbed("🎭 Your character", text, colourBlurple)
	e.Image = &discordgo.MessageEmbedImage{URL: "attachment://character.png"}
	_ = c.reply(true, []*discordgo.MessageEmbed{e}, characterButtons(), comicFile("character.png", data))
}

func characterButtons() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Show all", Style: discordgo.SecondaryButton, CustomID: "charall"},
		discordgo.Button{Label: "Reset", Style: discordgo.SecondaryButton, CustomID: "charreset"},
	}}}
}

func (b *Bot) autocompleteCharacter(c *Ctx) {
	typed, _ := c.Str("name")
	typed = strings.ToLower(strings.TrimSpace(typed))
	var choices []*discordgo.ApplicationCommandOptionChoice
	for _, name := range b.comics.pack.Load().NamesFor(c.UserID()) {
		if strings.Contains(name, typed) && len(choices) < 25 {
			choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: name, Value: name})
		}
	}
	c.logErr("autocomplete", c.s.InteractionRespond(c.i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	}))
}

func (b *Bot) onCharacterAll(c *Ctx) {
	if b.comics == nil {
		return
	}
	data, err := b.comics.pack.Load().SheetPNG(c.UserID())
	if err != nil {
		c.logErr("character: sheet", err)
		c.Reply(true, errEmbed("I could not draw the characters."))
		return
	}
	e := makeEmbed("🎭 Characters", "Use `/character name:` with one of these names.", colourBlurple)
	e.Image = &discordgo.MessageEmbedImage{URL: "attachment://characters.png"}
	_ = c.reply(true, []*discordgo.MessageEmbed{e}, nil, comicFile("characters.png", data))
}

func (b *Bot) onCharacterReset(c *Ctx) {
	if b.comics == nil {
		return
	}
	if err := b.db.DeleteComicCharacter(c.GuildID(), c.UserID()); err != nil {
		c.dbFailed("reset character", err)
		return
	}
	c.Reply(true, makeEmbed("🎭 Your character", fmt.Sprintf(
		"Reset. Comics now give you a character. Usually it is **%s**.", b.comics.pack.Load().CharacterFor(c.UserID())), colourBlurple))
}

// logComicProblems logs the custom art that could not be used.
func logComicProblems(p *comic.Pack) {
	for _, pr := range p.Problems() {
		log.Printf("comic art: %s", pr)
	}
}

// /comicart reload reads the art folder again, so that new custom
// characters and backgrounds work without a restart. When the art cannot
// be read, the old art stays.
func (b *Bot) cmdComicArtReload(c *Ctx) {
	c.Defer(true)
	pack, err := comic.LoadPack(b.env.ComicArtDir)
	if err != nil {
		c.Reply(true, errEmbed(fmt.Sprintf("I could not read the comic art, so the old art stays: %s", cutText(err.Error(), 300))))
		return
	}
	b.comics.pack.Store(pack)
	logComicProblems(pack)
	e := makeEmbed("🎨 Comic art reloaded", fmt.Sprintf("**%d** characters (**%d** private) and **%d** backgrounds.",
		len(pack.Names()), pack.Private(), len(pack.Backgrounds())), colourGreen)
	if pr := pack.Problems(); len(pr) > 0 {
		var lines []string
		for _, p := range pr[:min(len(pr), 15)] {
			lines = append(lines, "• "+escapeMarkdown(cutText(p, 200)))
		}
		if len(pr) > 15 {
			lines = append(lines, fmt.Sprintf("…and %d more (see the log)", len(pr)-15))
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Not used", Value: cutText(strings.Join(lines, "\n"), 1024)})
		e.Color = colourRed
	}
	c.Reply(true, e)
}
