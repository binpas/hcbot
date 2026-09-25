package bot

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/bwmarrin/discordgo"
)

// profileImageLimit is the largest avatar or banner /botprofile accepts.
const profileImageLimit = 8 << 20

var profileTitles = map[string]string{"avatar": "Avatar", "banner": "Banner"}

var profileImageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// profileChange is the data behind the /botprofile Confirm button.
type profileChange struct {
	Part    string `json:"part"` // "avatar" or "banner"
	DataURI string `json:"data_uri"`
}

func (b *Bot) registerBotProfile() {
	admin := int64(discordgo.PermissionAdministrator)
	b.addGroup(&group{name: "botprofile", description: "Change the bot's profile (admin only).", defaultPerm: admin})
	for _, part := range []string{"avatar", "banner"} {
		b.addCommand(&command{
			name: "botprofile " + part, description: fmt.Sprintf("Change the bot's %s everywhere.", part), perm: permAdmin,
			options: []*discordgo.ApplicationCommandOption{
				optAttachment("file", "A PNG, JPEG, GIF or WebP image (8 MB max)", true),
			},
			handler: func(c *Ctx) { b.cmdBotProfile(c, part) },
		})
	}
	b.addConfirm("botprofile", b.onBotProfileConfirm)
}

func (b *Bot) cmdBotProfile(c *Ctx, part string) {
	file := c.Attachment("file")
	raw, ok := b.readAttachment(c, file, profileImageLimit, "That image is larger than **8 MB**.")
	if !ok {
		return
	}
	kind := http.DetectContentType(raw)
	if !slices.Contains(profileImageTypes, kind) {
		c.Reply(true, errEmbed("That file isn't a PNG, JPEG, GIF or WebP image."))
		return
	}
	job := profileChange{Part: part, DataURI: "data:" + kind + ";base64," + base64.StdEncoding.EncodeToString(raw)}
	id, err := b.savePending("botprofile", c.UserID(), job)
	if err != nil {
		c.dbFailed("save pending change", err)
		return
	}
	e := makeEmbed("🖼️ Confirm New Bot "+profileTitles[part],
		fmt.Sprintf("The bot's %s changes everywhere. Discord allows only a few changes each hour.", part), colourOrange)
	e.Image = &discordgo.MessageEmbedImage{URL: file.URL}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{e}, confirmButtons("botprofile", c.UserID(), id))
}

func (b *Bot) onBotProfileConfirm(c *Ctx, extra []string) {
	if len(extra) < 1 {
		return
	}
	var job profileChange
	if !b.takePending(c, "botprofile", extra[0], &job) {
		return
	}
	c.DeferUpdate()
	avatar, banner := "", ""
	if job.Part == "banner" {
		banner = job.DataURI
	} else {
		avatar = job.DataURI
	}
	// No retry: a rate limit on profile changes can last a long time.
	user, err := b.s.UserUpdate("", avatar, banner, discordgo.WithRetryOnRatelimit(false))
	if err != nil {
		c.logErr("change bot "+job.Part, err)
		var rest *discordgo.RESTError
		var limited *discordgo.RateLimitError
		msg := "Discord refused the change. Check the bot's log."
		switch {
		case errors.As(err, &limited):
			msg = fmt.Sprintf("Discord is limiting %s changes. Try again in %s.", job.Part,
				humaniseDuration(max(int64(limited.RetryAfter.Seconds()), 1)))
		case errors.As(err, &rest) && rest.Message != nil && rest.Message.Message != "":
			msg = "Discord refused the change: " + rest.Message.Message
		}
		c.FinishUpdate(errEmbed(msg))
		return
	}
	if b.s.State.User != nil && user != nil {
		b.s.State.User.Avatar, b.s.State.User.Banner = user.Avatar, user.Banner
	}
	c.FinishUpdate(okEmbed(fmt.Sprintf("The bot's %s is changed.", job.Part)))
	b.modLog(makeEmbed("🖼️ Bot "+profileTitles[job.Part]+" Changed",
		fmt.Sprintf("<@%s> changed the bot's %s.", c.UserID(), job.Part), colourBlurple))
}
