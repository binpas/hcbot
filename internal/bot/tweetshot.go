package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/tweetshot"
)

// Post images: a mod gives a link to a post on X, and the bot draws the
// post as an image (see internal/tweetshot). The server does not allow X
// links, so a public reply has only the image; the ephemeral reply also
// has a link to the post.

const (
	tweetshotMenu    = "Screenshot post"
	tweetshotTimeout = 30 * time.Second
)

// xHosts are the hosts of X links, and of the sites that show X posts in
// Discord.
var xHosts = map[string]bool{
	"x.com": true, "twitter.com": true, "mobile.twitter.com": true, "mobile.x.com": true,
	"fxtwitter.com": true, "vxtwitter.com": true, "fixupx.com": true, "fixvx.com": true,
}

var statusPathRE = regexp.MustCompile(`^/(?:[A-Za-z0-9_]{1,15}|i(?:/web)?)/status(?:es)?/(\d{1,20})(?:/|$)`)

func (b *Bot) registerTweetshot() {
	b.addCommand(&command{
		name:        "tweetshot",
		description: "Make an image of a post on X (mod only).",
		options: []*discordgo.ApplicationCommandOption{
			optStr("url", "Link to the post", true),
			optBool("public", "Post the image in the channel (default: only you see it)", false),
		},
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdTweetshot,
	})
	b.addCommand(&command{
		name:        tweetshotMenu,
		kind:        discordgo.MessageApplicationCommand,
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdTweetshotFromMessage,
	})
}

func (b *Bot) cmdTweetshot(c *Ctx) {
	link, _ := c.Str("url")
	id := statusID(strings.TrimSpace(link))
	if id == "" {
		c.Reply(true, errEmbed("That is not a link to a post on X."))
		return
	}
	b.tweetshot(c, id, c.Bool("public", false))
}

func (b *Bot) cmdTweetshotFromMessage(c *Ctx) {
	msg := c.TargetMessage()
	id := ""
	if msg != nil {
		id = firstStatusID(msg)
	}
	if id == "" {
		c.Reply(true, errEmbed("That message has no link to a post on X."))
		return
	}
	b.tweetshot(c, id, false)
}

// statusID returns the post ID of an X link, or "".
func statusID(raw string) string {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimPrefix(raw, "<"), ">"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if !xHosts[strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")] {
		return ""
	}
	if m := statusPathRE.FindStringSubmatch(u.Path); m != nil {
		return m[1]
	}
	return ""
}

// firstStatusID returns the post ID of the first X link in a message's text
// or embeds, or "".
func firstStatusID(m *discordgo.Message) string {
	for _, l := range linkRE.FindAllString(m.Content, -1) {
		if id := statusID(l); id != "" {
			return id
		}
	}
	for _, e := range m.Embeds {
		if id := statusID(e.URL); id != "" {
			return id
		}
	}
	return ""
}

func (b *Bot) tweetshot(c *Ctx, id string, public bool) {
	c.Defer(!public)
	ctx, cancel := context.WithTimeout(context.Background(), tweetshotTimeout)
	defer cancel()

	p, err := b.tweets.Fetch(ctx, id)
	if err != nil {
		msg := "I could not get the post from X. Try again later."
		if errors.Is(err, tweetshot.ErrNotFound) {
			msg = "X does not show this post. It can be deleted, protected, or age-limited."
		} else {
			log.Printf("tweetshot %s: %v", id, err)
		}
		c.Reply(true, errEmbed(msg))
		return
	}
	data, err := tweetshot.PNG(p)
	if err != nil {
		log.Printf("tweetshot %s: render: %v", id, err)
		c.Reply(true, errEmbed("I could not make the image."))
		return
	}
	name := "post-" + id + ".png"
	embed := &discordgo.MessageEmbed{Color: 0x000000, Image: &discordgo.MessageEmbedImage{URL: "attachment://" + name}}
	if !public {
		embed.Description = fmt.Sprintf("[Open the post on X](https://x.com/%s/status/%s)", p.Handle, id)
	}
	if err := c.ReplyFile(!public, embed, &discordgo.File{Name: name, ContentType: "image/png", Reader: bytes.NewReader(data)}); err != nil {
		c.Reply(true, errEmbed("Discord did not accept the image."))
	}
}
