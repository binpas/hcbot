package bot

import (
	"bytes"
	"cmp"
	"fmt"
	"log"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Curation (a starboard): a message with CURATED_THRESHOLD of
// CURATED_EMOTE is copied to CURATED as one embed, with its pictures and
// files. The server owner chose a copy, not a native forward, so that the
// author, the count, and the media are in one embed. The bot must read the
// text to copy it, so curation runs only with
// ENABLE_MESSAGE_CONTENT_FEATURES; without it, curation is off completely.

// curatedGallery is the number of images that Discord shows together: the
// embeds of one message with the same URL make one gallery.
const curatedGallery = 4

func (b *Bot) curate(r *discordgo.MessageReaction) {
	if !b.env.MessageContent {
		return
	}
	emote := b.cfg.Str("CURATED_EMOTE")
	if emote == "" || r.Emoji.MessageFormat() != emote {
		return
	}
	if done, err := b.db.IsCurated(r.MessageID); err != nil || done {
		return
	}
	src := b.channel(r.ChannelID)
	if src == nil || (src.Type != discordgo.ChannelTypeGuildText && src.Type != discordgo.ChannelTypeGuildNews) {
		return
	}
	curatedID := b.cfg.ID("CURATED")
	if src.ID == curatedID {
		return // don't curate messages already in the curated channel
	}
	curated := b.channel(curatedID)
	if curated == nil || (curated.Type != discordgo.ChannelTypeGuildText && curated.Type != discordgo.ChannelTypeGuildNews) {
		return
	}
	msg, err := b.s.ChannelMessage(src.ID, r.MessageID)
	if err != nil {
		log.Printf("curate: fetch message: %v", err)
		return
	}
	count := 0
	for _, re := range msg.Reactions {
		if re.Emoji.MessageFormat() == emote {
			count = re.Count
		}
	}
	if count < b.cfg.Int("CURATED_THRESHOLD") {
		return
	}
	// Claim it first, in one step, so two reactions at once can't both post.
	if claimed, err := b.db.ClaimCurated(msg.ID); err != nil || !claimed {
		return
	}
	msg.ChannelID = src.ID
	b.postCurated(curated.ID, r.GuildID, msg, fmt.Sprintf("%s **%d** | %s", emote, count, channelMention(src.ID)))
}

// postCurated posts the copy of msg. head is the line above the embed.
func (b *Bot) postCurated(channelID, guildID string, msg *discordgo.Message, head string) {
	jump := fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, msg.ChannelID, msg.ID)
	text, links := curatedText(msg)
	media := b.curatedMedia(guildID, msg)
	if len(media.notes) > 0 {
		text = strings.TrimSpace(text + "\n\n" + strings.Join(media.notes, "\n"))
	}
	main := &discordgo.MessageEmbed{
		Author:      &discordgo.MessageEmbedAuthor{Name: b.curatedName(guildID, msg.Author), IconURL: msg.Author.AvatarURL("")},
		Title:       "Jump to message",
		URL:         jump,
		Description: cutText(text, 4096),
		Color:       colourGold,
	}
	if !msg.Timestamp.IsZero() {
		main.Timestamp = msg.Timestamp.Format(time.RFC3339)
	}
	embeds := []*discordgo.MessageEmbed{main}
	for i, name := range media.gallery {
		img := &discordgo.MessageEmbedImage{URL: "attachment://" + name}
		if i == 0 {
			main.Image = img
		} else {
			embeds = append(embeds, &discordgo.MessageEmbed{URL: jump, Image: img})
		}
	}
	// The links of link previews go in the text, so that Discord shows
	// its own previews (a copied preview cannot play a video or a GIF).
	content := head
	for _, l := range links {
		if len(content)+1+len(l) <= 2000 {
			content += "\n" + l
		}
	}
	send := &discordgo.MessageSend{Content: content, Embeds: embeds, Files: media.files,
		AllowedMentions: &discordgo.MessageAllowedMentions{}}
	_, err := b.s.ChannelMessageSendComplex(channelID, send)
	if err == nil {
		b.m.curated.WithLabelValues("ok").Inc()
		return
	}
	log.Printf("curate: post: %v", err)
	if len(media.files) == 0 {
		b.m.curated.WithLabelValues("error").Inc()
		return
	}
	// Again without the media: Discord can refuse a file.
	main.Image = nil
	main.Description = cutText(strings.TrimSpace(text+"\n\n"+"📎 The media could not be copied; see the original."), 4096)
	send.Embeds, send.Files = []*discordgo.MessageEmbed{main}, nil
	if _, err := b.s.ChannelMessageSendComplex(channelID, send); err != nil {
		log.Printf("curate: post: %v", err)
		b.m.curated.WithLabelValues("error").Inc()
		return
	}
	b.m.curated.WithLabelValues("without_files").Inc()
}

// curatedName is the author's name on the server.
func (b *Bot) curatedName(guildID string, u *discordgo.User) string {
	if !u.Bot && u.Discriminator != "0000" { // webhooks have no member
		if nick := b.memberNick(guildID, u.ID); nick != "" {
			return nick
		}
	}
	return cmp.Or(u.GlobalName, u.Username)
}

// curatedText is the text of the copy: a reply line, the message text, and
// the text of its own embeds (from bots), quoted. Link previews are not
// copied: links returns their links.
func curatedText(msg *discordgo.Message) (text string, links []string) {
	var parts []string
	if ref := msg.ReferencedMessage; ref != nil && ref.Author != nil {
		parts = append(parts, fmt.Sprintf("-# ↩️ Replying to **%s**", cmp.Or(ref.Author.GlobalName, ref.Author.Username)))
	}
	if t := strings.TrimSpace(msg.Content); t != "" {
		parts = append(parts, t)
	}
	for _, e := range msg.Embeds {
		if e == nil {
			continue
		}
		if e.Type != discordgo.EmbedTypeRich {
			if e.URL != "" && !slices.Contains(links, e.URL) {
				links = append(links, e.URL)
			}
			continue
		}
		if t := quotedEmbed(e); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n"), links
}

// quotedEmbed is the text of an embed, as a quote.
func quotedEmbed(e *discordgo.MessageEmbed) string {
	var lines []string
	if e.Author != nil && e.Author.Name != "" {
		lines = append(lines, "-# "+e.Author.Name)
	}
	switch {
	case e.Title != "" && e.URL != "":
		lines = append(lines, fmt.Sprintf("**[%s](%s)**", e.Title, e.URL))
	case e.Title != "":
		lines = append(lines, "**"+e.Title+"**")
	}
	if e.Description != "" {
		lines = append(lines, e.Description)
	}
	for _, f := range e.Fields {
		if f != nil {
			lines = append(lines, fmt.Sprintf("**%s**: %s", f.Name, f.Value))
		}
	}
	if e.Footer != nil && e.Footer.Text != "" {
		lines = append(lines, "-# "+e.Footer.Text)
	}
	if len(lines) == 0 {
		return ""
	}
	return "> " + strings.ReplaceAll(strings.Join(lines, "\n"), "\n", "\n> ")
}

// curatedFiles are the copied files of a message: gallery holds the names of
// the images to show in the embeds, and notes the files that were not copied.
type curatedFiles struct {
	files   []*discordgo.File
	gallery []string
	notes   []string
}

// curatedMedia downloads the files, stickers, and embed images of a
// message, up to the server's upload limit. Discord's links to them expire,
// so they are uploaded again.
func (b *Bot) curatedMedia(guildID string, msg *discordgo.Message) curatedFiles {
	var out curatedFiles
	left := int64(b.serverUploadMB(guildID)) << 20
	add := func(name, url string, size int64, image bool) {
		if size > left {
			out.notes = append(out.notes, fmt.Sprintf("📎 `%s` is too large to copy; see the original.", name))
			b.m.curatedFiles.WithLabelValues("too_large").Inc()
			return
		}
		data, err := b.download(url, left)
		if err != nil {
			log.Printf("curate: copy %s: %v", name, err)
			out.notes = append(out.notes, fmt.Sprintf("📎 `%s` could not be copied; see the original.", name))
			b.m.curatedFiles.WithLabelValues("failed").Inc()
			return
		}
		b.m.curatedFiles.WithLabelValues("copied").Inc()
		left -= int64(len(data))
		for slices.ContainsFunc(out.files, func(f *discordgo.File) bool { return f.Name == name }) {
			name = "1_" + name
		}
		out.files = append(out.files, &discordgo.File{Name: name, Reader: bytes.NewReader(data)})
		if image && len(out.gallery) < curatedGallery {
			out.gallery = append(out.gallery, name)
		}
	}
	for _, a := range msg.Attachments {
		if a == nil {
			continue
		}
		spoiler := strings.HasPrefix(a.Filename, "SPOILER_") || a.Flags&attachmentSpoiler != 0
		name := a.Filename
		if spoiler && !strings.HasPrefix(name, "SPOILER_") {
			name = "SPOILER_" + name // an embed image cannot be hidden, so a spoiler stays a file
		}
		add(name, cmp.Or(a.URL, a.ProxyURL), int64(a.Size), !spoiler && isImageFile(a))
	}
	for _, s := range msg.StickerItems {
		ext := map[discordgo.StickerFormat]string{discordgo.StickerFormatTypePNG: ".png",
			discordgo.StickerFormatTypeAPNG: ".png", discordgo.StickerFormatTypeGIF: ".gif"}[s.FormatType]
		if s != nil && ext != "" {
			add("sticker_"+s.ID+ext, "https://media.discordapp.net/stickers/"+s.ID+ext, 0, true)
		}
	}
	for i, e := range msg.Embeds {
		if e == nil || e.Type != discordgo.EmbedTypeRich || e.Image == nil {
			continue
		}
		src := cmp.Or(e.Image.ProxyURL, e.Image.URL)
		ext := ".png"
		if u, err := url.Parse(src); err == nil && path.Ext(u.Path) != "" {
			ext = strings.ToLower(path.Ext(u.Path))
		}
		add(fmt.Sprintf("embed_%d%s", i+1, ext), src, 0, true)
	}
	return out
}
