package bot

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/textproto"
	"path"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// /assets export|import (admin only). The zip holds the images and a
// manifest.json in the Python bot's format, so exports from either bot
// can be imported by the other.

const (
	assetLargeImport  = 10 // warn when more new items than this
	assetConflictWait = time.Hour
)

// assetEntry is one manifest.json entry.
type assetEntry struct {
	Type        string `json:"type"` // "emoji" or "sticker"
	Name        string `json:"name"`
	Filename    string `json:"filename"`
	Animated    bool   `json:"animated,omitempty"`
	Description string `json:"description,omitempty"`
	Emoji       string `json:"emoji,omitempty"`
}

// Emoji and sticker slots by boost tier. Discord counts static and
// animated emojis separately.
var (
	emojiSlots   = []int{50, 100, 150, 250}
	stickerSlots = []int{5, 15, 30, 60}
)

func tierSlots(slots []int, tier discordgo.PremiumTier) int {
	return slots[min(max(int(tier), 0), len(slots)-1)]
}

func emojiURL(e *discordgo.Emoji) string {
	if e.Animated {
		return discordgo.EndpointEmojiAnimated(e.ID)
	}
	return discordgo.EndpointEmoji(e.ID)
}

func stickerURL(id string) string { return discordgo.EndpointCDN + "stickers/" + id + ".png" }

// download fetches a CDN file.
func (b *Bot) download(url string, limit int64) ([]byte, error) {
	resp, err := b.s.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("larger than %d bytes", limit)
	}
	return data, err
}

// buildAssetsZip downloads the guild's emojis and PNG/APNG stickers into
// a zip with a manifest.json.
func (b *Bot) buildAssetsZip(guildID string) (data []byte, emojiCount, stickerCount, skipped int, err error) {
	g, err := b.s.State.Guild(guildID)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	b.s.State.RLock()
	emojis, stickers := append([]*discordgo.Emoji(nil), g.Emojis...), append([]*discordgo.Sticker(nil), g.Stickers...)
	b.s.State.RUnlock()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	manifest := []assetEntry{}
	add := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err == nil {
			_, err = w.Write(data)
		}
		return err
	}
	for _, e := range emojis {
		data, err := b.download(emojiURL(e), 10<<20)
		if err != nil {
			log.Printf("assets export: emoji %s: %v", e.Name, err)
			continue
		}
		ext := "png"
		if e.Animated {
			ext = "gif"
		}
		file := fmt.Sprintf("emoji_%s.%s", e.ID, ext)
		if add(file, data) == nil {
			manifest = append(manifest, assetEntry{Type: "emoji", Name: e.Name, Filename: file, Animated: e.Animated})
			emojiCount++
		}
	}
	for _, s := range stickers {
		// Lottie and GIF stickers can't be uploaded again through the bot API.
		if s.FormatType != discordgo.StickerFormatTypePNG && s.FormatType != discordgo.StickerFormatTypeAPNG {
			skipped++
			continue
		}
		data, err := b.download(stickerURL(s.ID), 10<<20)
		if err != nil {
			log.Printf("assets export: sticker %s: %v", s.Name, err)
			continue
		}
		file := fmt.Sprintf("sticker_%s.png", s.ID)
		if add(file, data) == nil {
			manifest = append(manifest, assetEntry{Type: "sticker", Name: s.Name, Filename: file,
				Description: s.Description, Emoji: s.Tags})
			stickerCount++
		}
	}
	var mbuf bytes.Buffer
	enc := json.NewEncoder(&mbuf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err = enc.Encode(manifest)
	if err == nil {
		err = add("manifest.json", mbuf.Bytes())
	}
	if err == nil {
		err = zw.Close()
	}
	return buf.Bytes(), emojiCount, stickerCount, skipped, err
}

func (b *Bot) cmdAssetsExport(c *Ctx) {
	g, err := b.s.State.Guild(c.GuildID())
	if err != nil {
		c.Reply(true, errEmbed("I couldn't read this server's emojis."))
		return
	}
	b.s.State.RLock()
	empty := len(g.Emojis) == 0 && len(g.Stickers) == 0
	b.s.State.RUnlock()
	if empty {
		c.Reply(true, errEmbed("This server has no custom emojis or stickers to export."))
		return
	}
	c.Defer(true)
	data, emojiCount, stickerCount, skipped, err := b.buildAssetsZip(c.GuildID())
	if err != nil {
		c.logErr("assets export: zip", err)
		c.Reply(true, errEmbed("I couldn't build the zip file."))
		return
	}
	summary := fmt.Sprintf("Exported **%d** emoji(s) and **%d** sticker(s).", emojiCount, stickerCount)
	if skipped > 0 {
		summary += fmt.Sprintf("\n**%d** sticker(s) skipped — Lottie/GIF stickers can't be re-uploaded through the bot API, only PNG/APNG ones.", skipped)
	}
	if err := c.ReplyFile(true, okEmbed(summary), &discordgo.File{Name: "assets-export.zip", ContentType: "application/zip",
		Reader: bytes.NewReader(data)}); err != nil {
		c.Reply(true, errEmbed("Export built, but the zip file is too large to send through Discord."))
	}
}

// parseAssetManifest reads manifest.json from an export zip. It returns
// nil entries when the manifest is missing or unreadable.
func parseAssetManifest(zr *zip.Reader) (emojis, stickers []assetEntry, problems int, ok bool) {
	files := map[string]bool{}
	var mf *zip.File
	for _, f := range zr.File {
		files[f.Name] = true
		if f.Name == "manifest.json" {
			mf = f
		}
	}
	if mf == nil {
		return nil, nil, 0, false
	}
	rc, err := mf.Open()
	if err != nil {
		return nil, nil, 0, false
	}
	defer func() { _ = rc.Close() }()
	var raw []json.RawMessage
	if err := json.NewDecoder(rc).Decode(&raw); err != nil {
		return nil, nil, 0, false
	}
	for _, r := range raw {
		var e assetEntry
		if json.Unmarshal(r, &e) != nil || strings.TrimSpace(e.Name) == "" || !files[e.Filename] ||
			(e.Type != "emoji" && e.Type != "sticker") {
			problems++
			continue
		}
		e.Name = strings.TrimSpace(e.Name)
		if e.Type == "emoji" {
			e.Animated = e.Animated || strings.EqualFold(path.Ext(e.Filename), ".gif")
			emojis = append(emojis, e)
		} else {
			stickers = append(stickers, e)
		}
	}
	return emojis, stickers, problems, true
}

func readZipFile(zr *zip.Reader, name string) ([]byte, error) {
	f, err := zr.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, 20<<20))
}

// createEmoji uploads a custom emoji.
func (b *Bot) createEmoji(guildID, name, filename string, data []byte, reason string) error {
	mime := "image/png"
	if strings.EqualFold(path.Ext(filename), ".gif") {
		mime = "image/gif"
	}
	_, err := b.s.GuildEmojiCreate(guildID, &discordgo.EmojiParams{
		Name: name, Image: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
	}, auditReason(reason))
	return err
}

// createSticker uploads a guild sticker. discordgo has no function for
// this, so it sends the multipart request itself.
func (b *Bot) createSticker(guildID, name, description, tags, filename string, data []byte, reason string) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range map[string]string{"name": name, "description": description, "tags": tags} {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", "image/png")
	part, err := w.CreatePart(h)
	if err == nil {
		_, err = part.Write(data)
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return err
	}
	endpoint := discordgo.EndpointGuild(guildID) + "/stickers"
	_, err = b.s.RequestRaw("POST", endpoint, w.FormDataContentType(), body.Bytes(), endpoint, 0, auditReason(reason))
	return err
}

// assetConflict is the data behind one Replace/Keep review (in pending_actions).
type assetConflict struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Filename    string `json:"filename"`
	Image       []byte `json:"image"`
	ExistingURL string `json:"existing_url"`
	GuildID     string `json:"guild_id"`
	Description string `json:"description"`
	EmojiTag    string `json:"emoji_tag"`
}

func (b *Bot) cmdAssetsImport(c *Ctx) {
	file := c.Attachment("file")
	if file == nil {
		c.Reply(true, errEmbed("Couldn't read that attachment."))
		return
	}
	limitMB := max(b.cfg.Int("ASSETS_IMPORT_MAX_MB"), 1)
	limit := int64(limitMB) << 20
	if int64(file.Size) > limit {
		c.Reply(true, errEmbed(fmt.Sprintf("That file is larger than **%d MB** (`ASSETS_IMPORT_MAX_MB` in `/setup`).", limitMB)))
		return
	}
	c.Defer(true)
	raw, err := b.download(file.URL, limit)
	if err != nil {
		c.logErr("download attachment", err)
		c.Reply(true, errEmbed("Couldn't read that attachment."))
		return
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		c.Reply(true, errEmbed("That doesn't look like a valid zip file."))
		return
	}
	emojis, stickers, problems, ok := parseAssetManifest(zr)
	if !ok {
		c.Reply(true, errEmbed("Couldn't find a valid `manifest.json` in that zip — is it one `/assets export` made?"))
		return
	}
	if len(emojis) == 0 && len(stickers) == 0 {
		c.Reply(true, errEmbed("No valid emoji or sticker entries found in that file."))
		return
	}

	g, err := b.s.State.Guild(c.GuildID())
	if err != nil {
		c.Reply(true, errEmbed("I couldn't read this server's emojis."))
		return
	}
	b.s.State.RLock()
	existingEmojis := map[string]*discordgo.Emoji{}
	static, animated := 0, 0
	for _, e := range g.Emojis {
		existingEmojis[e.Name] = e
		if e.Animated {
			animated++
		} else {
			static++
		}
	}
	existingStickers := map[string]*discordgo.Sticker{}
	for _, s := range g.Stickers {
		existingStickers[s.Name] = s
	}
	tier := g.PremiumTier
	b.s.State.RUnlock()

	var newEmojis, conflictEmojis, newStickers, conflictStickers []assetEntry
	newStatic, newAnimated := 0, 0
	for _, e := range emojis {
		if _, taken := existingEmojis[e.Name]; taken {
			conflictEmojis = append(conflictEmojis, e)
			continue
		}
		newEmojis = append(newEmojis, e)
		if e.Animated {
			newAnimated++
		} else {
			newStatic++
		}
	}
	for _, s := range stickers {
		if _, taken := existingStickers[s.Name]; taken {
			conflictStickers = append(conflictStickers, s)
		} else {
			newStickers = append(newStickers, s)
		}
	}

	freeStatic := tierSlots(emojiSlots, tier) - static
	freeAnimated := tierSlots(emojiSlots, tier) - animated
	freeStickers := tierSlots(stickerSlots, tier) - len(existingStickers)
	var shortfalls []string
	if newStatic > freeStatic {
		shortfalls = append(shortfalls, fmt.Sprintf("%d more static emoji slot(s)", newStatic-freeStatic))
	}
	if newAnimated > freeAnimated {
		shortfalls = append(shortfalls, fmt.Sprintf("%d more animated emoji slot(s)", newAnimated-freeAnimated))
	}
	if len(newStickers) > freeStickers {
		shortfalls = append(shortfalls, fmt.Sprintf("%d more sticker slot(s)", len(newStickers)-freeStickers))
	}
	if len(shortfalls) > 0 {
		c.Reply(true, errEmbed(fmt.Sprintf("Not enough room in this server — need %s.\n"+
			"Free static emoji slots: **%d**. Free animated emoji slots: **%d**. Free sticker slots: **%d**.\n"+
			"Free up space or trim the zip file, then try again. No changes were made.",
			strings.Join(shortfalls, " and "), max(freeStatic, 0), max(freeAnimated, 0), max(freeStickers, 0))))
		return
	}

	if total := len(newEmojis) + len(newStickers); total > assetLargeImport {
		c.Reply(true, makeEmbed("⏳ Importing…", fmt.Sprintf("Adding **%d** new item(s). Discord limits how fast these "+
			"can be created, so this will take a bit — I'll post a summary here when it's done.", total), colourBlurple))
	}

	reason := "/assets import by " + c.Member().User.String()
	addedEmojis, addedStickers := 0, 0
	var failures []string
	for _, e := range newEmojis {
		data, err := readZipFile(zr, e.Filename)
		if err == nil {
			err = b.createEmoji(c.GuildID(), e.Name, e.Filename, data, reason)
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("`%s` — %v", e.Name, err))
		} else {
			addedEmojis++
		}
		time.Sleep(b.assetDelay)
	}
	for _, s := range newStickers {
		data, err := readZipFile(zr, s.Filename)
		if err == nil {
			err = b.createSticker(c.GuildID(), s.Name, s.Description, cmpOrDefault(s.Emoji, "❔"), s.Filename, data, reason)
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("`%s` — %v", s.Name, err))
		} else {
			addedStickers++
		}
		time.Sleep(b.assetDelay)
	}

	parts := []string{fmt.Sprintf("Added **%d** emoji(s) and **%d** sticker(s).", addedEmojis, addedStickers)}
	if problems > 0 {
		word := "entries"
		if problems == 1 {
			word = "entry"
		}
		parts = append(parts, fmt.Sprintf("**%d** %s in the file were skipped (malformed).", problems, word))
	}
	if len(failures) > 0 {
		lines := strings.Join(failures[:min(len(failures), 10)], "\n")
		if len(failures) > 10 {
			lines += fmt.Sprintf("\n… and %d more.", len(failures)-10)
		}
		parts = append(parts, fmt.Sprintf("**%d** item(s) failed to import:\n%s", len(failures), lines))
	}

	var conflicts []assetConflict
	for _, e := range conflictEmojis {
		data, err := readZipFile(zr, e.Filename)
		if err != nil {
			continue
		}
		conflicts = append(conflicts, assetConflict{Kind: "emoji", Name: e.Name, Filename: e.Filename, Image: data,
			ExistingURL: emojiURL(existingEmojis[e.Name]), GuildID: c.GuildID()})
	}
	for _, s := range conflictStickers {
		data, err := readZipFile(zr, s.Filename)
		if err != nil {
			continue
		}
		conflicts = append(conflicts, assetConflict{Kind: "sticker", Name: s.Name, Filename: s.Filename, Image: data,
			ExistingURL: stickerURL(existingStickers[s.Name].ID), GuildID: c.GuildID(),
			Description: s.Description, EmojiTag: cmpOrDefault(s.Emoji, "❔")})
	}
	if len(conflicts) > 0 {
		parts = append(parts, fmt.Sprintf("**%d** item(s) share a name with something that already exists — "+
			"check your DMs to decide what to do with each.", len(conflicts)))
	}
	c.Reply(true, okEmbed(strings.Join(parts, "\n\n")))
	if len(conflicts) > 0 {
		b.sendConflictReviews(c, g.Name, conflicts)
	}
}

func cmpOrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// conflictMessage builds one Replace/Keep review; the image of the new
// asset is attached, the existing one is the thumbnail.
func conflictMessage(p assetConflict, pendingID string) (*discordgo.MessageEmbed, *discordgo.File, []discordgo.MessageComponent) {
	e := makeEmbed(fmt.Sprintf("⚠️ %s name conflict: `%s`", strings.ToUpper(p.Kind[:1])+p.Kind[1:], p.Name),
		fmt.Sprintf("A %s named `%s` already exists in the server.\n"+
			"Small picture (top right) = the current one. Large picture (below) = the one from your zip file.", p.Kind, p.Name),
		colourOrange)
	if p.ExistingURL != "" {
		e.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: p.ExistingURL}
	}
	e.Image = &discordgo.MessageEmbedImage{URL: "attachment://" + p.Filename}
	return e, &discordgo.File{Name: p.Filename, Reader: bytes.NewReader(p.Image)},
		[]discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Replace", Style: discordgo.DangerButton, Emoji: &discordgo.ComponentEmoji{Name: "🔁"},
				CustomID: "asset:replace:" + pendingID},
			discordgo.Button{Label: "Keep Existing", Style: discordgo.SecondaryButton, Emoji: &discordgo.ComponentEmoji{Name: "✅"},
				CustomID: "asset:keep:" + pendingID},
		}}}
}

// sendConflictReviews sends each conflict to the invoker by DM, or in the
// channel (ephemeral) when the DM can't be delivered.
func (b *Bot) sendConflictReviews(c *Ctx, guildName string, conflicts []assetConflict) {
	ids := make([]string, 0, len(conflicts))
	for _, p := range conflicts {
		data, _ := json.Marshal(p)
		id, err := b.db.SavePending("assetconflict", c.UserID(), string(data), time.Now().Add(assetConflictWait))
		if err != nil {
			c.logErr("save conflict", err)
			return
		}
		ids = append(ids, fmt.Sprint(id))
	}
	sendAll := func(send func(*discordgo.MessageEmbed, *discordgo.File, []discordgo.MessageComponent) error) error {
		for i, p := range conflicts {
			e, f, comps := conflictMessage(p, ids[i])
			if err := send(e, f, comps); err != nil {
				return err
			}
		}
		return nil
	}

	dm, err := b.s.UserChannelCreate(c.UserID())
	if err == nil {
		_, err = b.s.ChannelMessageSendEmbed(dm.ID, okEmbed(fmt.Sprintf(
			"`/assets import` in **%s** found **%d** name conflict(s). Review each one below.", guildName, len(conflicts))))
	}
	if err == nil {
		err = sendAll(func(e *discordgo.MessageEmbed, f *discordgo.File, comps []discordgo.MessageComponent) error {
			_, err := b.s.ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
				Embeds: []*discordgo.MessageEmbed{e}, Files: []*discordgo.File{f}, Components: comps})
			return err
		})
	}
	if err == nil {
		return
	}
	c.logErr("conflict DM", err)
	c.Reply(true, errEmbed("Couldn't DM you the name-conflict review (your DMs may be closed) — showing it here instead."))
	_ = sendAll(func(e *discordgo.MessageEmbed, f *discordgo.File, comps []discordgo.MessageComponent) error {
		return c.reply(true, []*discordgo.MessageEmbed{e}, comps, []*discordgo.File{f})
	})
}

var errNoGuild = errors.New("not in that server")

// replaceAsset deletes the guild's emoji or sticker with the conflict's
// name (if it still exists) and uploads the new one.
func (b *Bot) replaceAsset(p assetConflict) error {
	g, err := b.s.State.Guild(p.GuildID)
	if err != nil {
		return errNoGuild
	}
	reason := "Replaced via /assets import"
	b.s.State.RLock()
	var oldID string
	if p.Kind == "emoji" {
		for _, e := range g.Emojis {
			if e.Name == p.Name {
				oldID = e.ID
			}
		}
	} else {
		for _, s := range g.Stickers {
			if s.Name == p.Name {
				oldID = s.ID
			}
		}
	}
	b.s.State.RUnlock()

	if p.Kind == "emoji" {
		if oldID != "" {
			if err := b.s.GuildEmojiDelete(p.GuildID, oldID, auditReason(reason)); err != nil {
				return err
			}
		}
		return b.createEmoji(p.GuildID, p.Name, p.Filename, p.Image, reason)
	}
	if oldID != "" {
		if err := b.deleteSticker(p.GuildID, oldID, reason); err != nil {
			return err
		}
	}
	return b.createSticker(p.GuildID, p.Name, p.Description, cmpOrDefault(p.EmojiTag, "❔"), p.Filename, p.Image, reason)
}

// deleteSticker deletes a guild sticker (discordgo has no function for it).
func (b *Bot) deleteSticker(guildID, id, reason string) error {
	endpoint := discordgo.EndpointGuild(guildID) + "/stickers/" + id
	_, err := b.s.RequestWithBucketID("DELETE", endpoint, nil, discordgo.EndpointGuild(guildID)+"/stickers/", auditReason(reason))
	return err
}

// onAssetConflict handles Replace / Keep Existing (asset:<choice>:<pending id>).
// It also works in a DM, where there is no guild in the interaction.
func (b *Bot) onAssetConflict(c *Ctx) {
	if len(c.Args) < 2 {
		return
	}
	var p assetConflict
	if !b.takePending(c, "assetconflict", c.Args[1], &p) {
		return
	}
	c.DeferUpdate()
	if c.Args[0] != "replace" {
		c.FinishUpdate(okEmbed(fmt.Sprintf("Kept the existing `%s` — nothing changed.", p.Name)))
		return
	}
	if err := b.replaceAsset(p); err != nil {
		c.logErr("replace asset", err)
		if errors.Is(err, errNoGuild) {
			c.FinishUpdate(errEmbed("I'm no longer in that server."))
		} else {
			c.FinishUpdate(errEmbed(fmt.Sprintf("Couldn't replace `%s`: %v", p.Name, err)))
		}
		return
	}
	c.FinishUpdate(okEmbed(fmt.Sprintf("Replaced `%s`.", p.Name)))
}

func (b *Bot) registerAssets() {
	admin := int64(discordgo.PermissionAdministrator)
	b.addGroup(&group{name: "assets", description: "Export/import server emojis and stickers (admin only).", defaultPerm: admin})
	b.addCommand(&command{name: "assets export", description: "Export all custom emojis and stickers to a zip file.",
		perm: permAdmin, handler: b.cmdAssetsExport})
	b.addCommand(&command{name: "assets import", description: "Import emojis/stickers from a zip made by /assets export.",
		perm: permAdmin, handler: b.cmdAssetsImport,
		options: []*discordgo.ApplicationCommandOption{optAttachment("file", "A .zip file previously created by /assets export", true)}})
	b.addComponent("asset", b.onAssetConflict)
}
