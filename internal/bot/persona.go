package bot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/markov"
)

// The persona chat is a second Markov chain, in its own file (persona.db).
// It learns only from one member (PERSONA_USER), in every channel except
// the PERSONA_SKIP_CATEGORY category, and talks like that member in
// PERSONA_REPLY_CHANNELS: always when a message contains PERSONA_BOT_NAME,
// and at random (PERSONA_CHANCE, with PERSONA_COOLDOWN). Its posts are an
// embed with a red border and the persona's name and avatar, from the bot,
// so nobody thinks that the member wrote them. /persona backfill learns the
// member's old messages with Discord's message search. The feature needs
// the Message Content option.

const (
	personaMaxPending  = 5000
	personaAvatarMaxMB = 2
	personaSearchPage  = 25               // the search limit
	personaIndexTries  = 30               // 202 "not indexed yet" answers before backfill stops
	personaEditFor     = 14 * time.Minute // an interaction token works for 15 minutes

	personaCursorKey = "backfill_cursor" // the oldest message ID searched so far
	personaAvatarKey = "avatar"
)

// Waits of the backfill (short in tests).
var (
	personaSearchPause = 500 * time.Millisecond // between two search pages
	personaIndexWait   = 2 * time.Second        // the least wait when the server is not indexed yet
)

type personaState struct {
	cool     markovState // only its cooldown map is used
	mu       sync.Mutex
	pending  []markov.Item
	backfill atomic.Bool
}

func (b *Bot) registerPersona() {
	if !b.env.MessageContent || b.persona == nil {
		return
	}
	b.addGroup(&group{name: "persona", description: "Manage the persona chat (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{
		name:        "persona status",
		description: "Show the persona chat's settings and the size of its chain.",
		perm:        permMod,
		handler:     b.cmdPersonaStatus,
	})
	b.addCommand(&command{
		name:        "persona on",
		description: "Turn the persona chat on: it learns and talks again.",
		perm:        permMod,
		handler:     func(c *Ctx) { b.setPersonaEnabled(c, true) },
	})
	b.addCommand(&command{
		name:        "persona off",
		description: "Turn the persona chat off: it stops learning and talking. Its chain stays.",
		perm:        permMod,
		handler:     func(c *Ctx) { b.setPersonaEnabled(c, false) },
	})
	b.addCommand(&command{
		name:        "persona avatar",
		description: "Set the picture on the persona's posts.",
		options:     []*discordgo.ApplicationCommandOption{optAttachment("file", "A PNG, JPEG, GIF or WebP image (at most 2 MB)", true)},
		perm:        permMod,
		handler:     b.cmdPersonaAvatar,
	})
	b.addCommand(&command{
		name:        "persona backfill",
		description: "Learn the member's old messages with Discord's search. It runs in the background.",
		perm:        permMod,
		handler:     b.cmdPersonaBackfill,
	})
	b.addConfirm("pbackfill", b.onPersonaBackfillConfirm)
}

// personaUser is the member the persona learns from, or "" when the
// persona chat cannot run.
func (b *Bot) personaUser() string {
	if b.persona == nil || !b.env.MessageContent || !b.cfg.Bool("PERSONA_ENABLED") {
		return ""
	}
	return b.cfg.ID("PERSONA_USER")
}

func (b *Bot) personaName() string {
	if n := strings.TrimSpace(b.cfg.Str("PERSONA_BOT_NAME")); n != "" {
		return n
	}
	return "Persona"
}

// personaSkipped reports whether a channel is in PERSONA_SKIP_CATEGORY.
// parents gives the parent of threads that are not in the cache. A channel
// that the bot cannot find is skipped too, to be safe.
func (b *Bot) personaSkipped(channelID string, parents map[string]string) bool {
	skip := b.cfg.ID("PERSONA_SKIP_CATEGORY")
	if skip == "" {
		return false
	}
	ch, err := b.s.State.Channel(channelID)
	if err != nil {
		parent, ok := parents[channelID]
		if !ok {
			return true
		}
		if ch, err = b.s.State.Channel(parent); err != nil {
			return true
		}
	} else if ch.IsThread() {
		if ch, err = b.s.State.Channel(ch.ParentID); err != nil {
			return true
		}
	}
	return ch.ID == skip || ch.ParentID == skip
}

// nameRE matches the persona name as a whole word, in any case.
func nameRE(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^\pL\pN])` + regexp.QuoteMeta(name) + `($|[^\pL\pN])`)
}

// personaCalled reports whether text contains the persona's name.
func (b *Bot) personaCalled(text string) bool {
	name := strings.TrimSpace(b.cfg.Str("PERSONA_BOT_NAME"))
	return name != "" && nameRE(name).MatchString(text)
}

// personaMessage learns the message when the persona member wrote it, and
// makes the persona talk when it should. It reports true when the persona
// talked, so the normal Markov chat stays quiet for that message.
func (b *Bot) personaMessage(m *discordgo.Message) bool {
	user := b.personaUser()
	if user == "" || m.GuildID == "" {
		return false
	}
	botID := b.s.State.User.ID
	words := markov.Tokenize(markovNames(m, botID))
	if m.Author.ID == user {
		result := "queued"
		switch {
		case b.personaSkipped(m.ChannelID, nil):
			result = "skipped_category"
		case len(words) == 0:
			result = "no_words"
		case b.markovBlocks(words):
			result = "blocked"
		default:
			b.pr.mu.Lock()
			if len(b.pr.pending) < personaMaxPending {
				b.pr.pending = append(b.pr.pending, markov.Item{ID: m.ID, Words: words})
			} else {
				result = "queue_full"
			}
			b.pr.mu.Unlock()
		}
		b.m.personaSeen.WithLabelValues(result).Inc()
	}
	if !b.markovChannel("PERSONA_REPLY_CHANNELS", m.ChannelID) {
		return false
	}
	cooldown := time.Duration(max(0, b.cfg.Int("PERSONA_COOLDOWN"))) * time.Second
	kind := "name"
	switch {
	case b.personaCalled(m.Content):
		b.pr.cool.claim(m.ChannelID, cooldown, true)
	case m.Author.ID != user && percentF(b.cfg.Float("PERSONA_CHANCE")) && b.pr.cool.claim(m.ChannelID, cooldown, false):
		kind = "chance"
	default:
		return false
	}
	b.personaReply(m, words, kind)
	return true
}

// personaReply posts a persona message as a reply to m. kind (name,
// chance) is for the metrics.
func (b *Bot) personaReply(m *discordgo.Message, words []string, kind string) {
	b.flushPersona() // the chain is small: it can use the newest words
	name := strings.ToLower(b.personaName())
	seed := slices.DeleteFunc(slices.Clone(words), func(w string) bool { return w == name })
	slices.SortStableFunc(seed, func(x, y string) int {
		return utf8.RuneCountInString(y) - utf8.RuneCountInString(x)
	})
	seed = seed[:min(len(seed), 5)]
	out, err := b.markovGenerate(b.persona, seed, strings.Join(words, " "))
	if err != nil {
		log.Printf("persona generate: %v", err)
		return
	}
	if out == "" {
		return
	}
	msg := b.personaPost(out)
	msg.Reference = m.Reference()
	if _, err := b.s.ChannelMessageSendComplex(m.ChannelID, msg); err != nil {
		// The message may be gone; answer without the reply link.
		msg.Reference = nil
		if _, err = b.s.ChannelMessageSendComplex(m.ChannelID, msg); err != nil {
			b.m.personaReply.WithLabelValues("failed").Inc()
			log.Printf("persona reply: %v", err)
			return
		}
	}
	b.m.personaReply.WithLabelValues(kind).Inc()
}

// personaPost builds a persona message: an embed with a red border, and
// the persona's name and avatar at the top.
func (b *Bot) personaPost(text string) *discordgo.MessageSend {
	e := makeEmbed("", truncate(text, 4000), colourRed)
	e.Author = &discordgo.MessageEmbedAuthor{Name: truncate(b.personaName(), 256)}
	msg := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{e}, AllowedMentions: markovMentions}
	img, err := b.persona.Meta(personaAvatarKey)
	if err != nil {
		log.Printf("persona avatar: %v", err)
	}
	if len(img) > 0 {
		file := "persona" + imageExt(img)
		e.Author.IconURL = "attachment://" + file
		msg.Files = []*discordgo.File{{Name: file, ContentType: http.DetectContentType(img), Reader: bytes.NewReader(img)}}
	}
	return msg
}

// imageExt returns the file extension of a supported image, or "".
func imageExt(img []byte) string {
	switch http.DetectContentType(img) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ""
}

// flushPersona writes the learned messages to persona.db.
func (b *Bot) flushPersona() {
	if b.persona == nil {
		return
	}
	b.pr.mu.Lock()
	batch := b.pr.pending
	b.pr.pending = nil
	b.pr.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	n, err := b.persona.LearnOnce(batch)
	if err != nil {
		b.m.personaLearn.WithLabelValues("errors").Inc()
		log.Printf("persona learn: %v", err)
		b.pr.mu.Lock()
		b.pr.pending = append(batch, b.pr.pending...)
		if len(b.pr.pending) > personaMaxPending {
			b.pr.pending = b.pr.pending[:personaMaxPending]
		}
		b.pr.mu.Unlock()
		return
	}
	words := 0
	for _, it := range batch {
		words += len(it.Words)
	}
	b.m.personaLearn.WithLabelValues("messages").Add(float64(n))
	b.m.personaLearn.WithLabelValues("words").Add(float64(words))
}

func (b *Bot) setPersonaEnabled(c *Ctx, on bool) {
	value := "off"
	if on {
		value = "on"
	}
	if err := b.setConfig("PERSONA_ENABLED", value); err != nil {
		c.dbFailed("save PERSONA_ENABLED", err)
		return
	}
	if !on {
		c.Reply(true, okEmbed("The persona chat is **off**. It does not learn or talk. Its chain stays: `/persona on` starts it again."))
		return
	}
	text := "The persona chat is **on**."
	if b.cfg.ID("PERSONA_USER") == "" {
		text += " Set `PERSONA_USER` in `/setup` before it can learn."
	}
	c.Reply(true, okEmbed(text))
}

func (b *Bot) cmdPersonaStatus(c *Ctx) {
	b.flushPersona()
	st, err := b.persona.Stats()
	if err != nil {
		c.dbFailed("persona stats", err)
		return
	}
	learned, err := b.persona.Learned()
	if err != nil {
		c.dbFailed("persona learned", err)
		return
	}
	state := "❌ Off"
	if b.cfg.Bool("PERSONA_ENABLED") {
		state = "✅ On"
	}
	user, skip := "*not set*", "*none*"
	if id := b.cfg.ID("PERSONA_USER"); id != "" {
		user = userMention(id)
	}
	if id := b.cfg.ID("PERSONA_SKIP_CATEGORY"); id != "" {
		skip = channelMention(id)
	}
	channels := "*none: it does not talk*"
	if list := b.cfg.List("PERSONA_REPLY_CHANNELS"); len(list) > 0 {
		ms := make([]string, len(list))
		for i, id := range list {
			ms[i] = channelMention(id)
		}
		channels = truncate(strings.Join(ms, ", "), 1000)
	}
	backfill := "Not running."
	if b.pr.backfill.Load() {
		backfill = "Running now."
	} else if cur, _ := b.persona.Meta(personaCursorKey); len(cur) > 0 {
		backfill = "Stopped before the end. `/persona backfill` continues it."
	}
	avatar := "*none*"
	if img, _ := b.persona.Meta(personaAvatarKey); len(img) > 0 {
		avatar = "Set"
	}
	e := makeEmbed("🎭 Persona chat", "", colourRed)
	e.Fields = []*discordgo.MessageEmbedField{
		{Name: "State", Value: state, Inline: true},
		{Name: "Learns from", Value: user, Inline: true},
		{Name: "Name", Value: "`" + b.personaName() + "`", Inline: true},
		{Name: "Messages learned", Value: strconv.FormatInt(learned, 10), Inline: true},
		{Name: "Words", Value: strconv.FormatInt(st.Words, 10), Inline: true},
		{Name: "Word steps", Value: strconv.FormatInt(st.Transitions, 10), Inline: true},
		{Name: "Talks in", Value: channels},
		{Name: "Chance and cooldown", Value: fmt.Sprintf("Chance: **%s** · Cooldown: **%ds**",
			chanceText(b.cfg.Float("PERSONA_CHANCE")), max(0, b.cfg.Int("PERSONA_COOLDOWN")))},
		{Name: "Skipped category", Value: skip, Inline: true},
		{Name: "Avatar", Value: avatar, Inline: true},
		{Name: "Backfill", Value: backfill},
	}
	c.Reply(true, e)
}

func (b *Bot) cmdPersonaAvatar(c *Ctx) {
	img, ok := b.readAttachment(c, c.Attachment("file"), personaAvatarMaxMB<<20,
		fmt.Sprintf("That file is too large. The limit is %d MB.", personaAvatarMaxMB))
	if !ok {
		return
	}
	if imageExt(img) == "" {
		c.Reply(true, errEmbed("That is not a PNG, JPEG, GIF or WebP image."))
		return
	}
	if err := b.persona.SetMeta(personaAvatarKey, img); err != nil {
		c.dbFailed("save persona avatar", err)
		return
	}
	msg := b.personaPost("This is how my posts look now.")
	e := msg.Embeds[0]
	if err := c.ReplyFiles(true, e, msg.Files...); err != nil {
		c.Reply(true, okEmbed("Saved the persona avatar."))
	}
}

func (b *Bot) cmdPersonaBackfill(c *Ctx) {
	user := b.cfg.ID("PERSONA_USER")
	if user == "" {
		c.Reply(true, errEmbed("Set `PERSONA_USER` in `/setup` first."))
		return
	}
	if !b.cfg.Bool("PERSONA_ENABLED") {
		c.Reply(true, errEmbed("The persona chat is off. Use `/persona on` first."))
		return
	}
	if b.pr.backfill.Load() {
		c.Reply(true, errEmbed("A backfill is running now. `/persona status` shows it."))
		return
	}
	text := fmt.Sprintf("Search the whole server for the messages of %s and learn them? "+
		"It skips the category in `PERSONA_SKIP_CATEGORY`, and messages it learned before. "+
		"It runs in the background and can take some minutes.", userMention(user))
	if cur, _ := b.persona.Meta(personaCursorKey); len(cur) > 0 {
		text += "\n\nThe last backfill stopped before the end. This one continues from there."
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("🎭 Persona backfill", text, colourOrange)},
		confirmButtons("pbackfill", c.UserID()))
}

func (b *Bot) onPersonaBackfillConfirm(c *Ctx, _ []string) {
	user := b.cfg.ID("PERSONA_USER")
	if user == "" || !b.pr.backfill.CompareAndSwap(false, true) {
		c.Update([]*discordgo.MessageEmbed{errEmbed("A backfill is running now, or `PERSONA_USER` is not set.")},
			[]discordgo.MessageComponent{})
		return
	}
	c.Update([]*discordgo.MessageEmbed{makeEmbed("🎭 Persona backfill", "Started…", colourBlurple)},
		[]discordgo.MessageComponent{})
	go func() {
		defer b.pr.backfill.Store(false)
		defer recoverHandler("persona backfill")
		start := time.Now()
		report := func(e *discordgo.MessageEmbed) {
			if time.Since(start) < personaEditFor {
				c.EditResponse(e)
			}
		}
		res, err := b.personaBackfill(c.GuildID(), user, func(r backfillResult) {
			report(makeEmbed("🎭 Persona backfill", "Running… "+r.String(), colourBlurple))
		})
		e := makeEmbed("🎭 Persona backfill done", res.String(), colourGreen)
		result := "done"
		switch {
		case errors.Is(err, errBackfillStopped):
			result = "stopped"
		case err != nil:
			result = "error"
		}
		b.m.backfillRuns.WithLabelValues(result).Inc()
		if err != nil {
			log.Printf("persona backfill: %v", err)
			e = makeEmbed("🎭 Persona backfill stopped", res.String()+"\n\n"+err.Error()+
				"\n`/persona backfill` continues from where it stopped.", colourOrange)
		}
		if time.Since(start) < personaEditFor {
			c.EditResponse(e)
		} else {
			b.modLog(e)
		}
	}()
}

// backfillResult counts the work of a backfill.
type backfillResult struct {
	found, learned, skipped int
}

func (r backfillResult) String() string {
	return fmt.Sprintf("Found **%d** messages, learned **%d** new ones, skipped **%d** (skipped category, blocked, or no words).",
		r.found, r.learned, r.skipped)
}

// errBackfillStopped is a backfill stopped by /persona off or a shutdown.
var errBackfillStopped = errors.New("the backfill stopped: the persona chat was turned off, or the bot stopped")

// personaBackfill searches the member's messages, newest first, and learns
// them. It keeps its place in persona.db, so a stopped backfill continues
// from there. progress gets the counts after each few pages.
func (b *Bot) personaBackfill(guildID, user string, progress func(backfillResult)) (backfillResult, error) {
	var res backfillResult
	cursor := ""
	if cur, err := b.persona.Meta(personaCursorKey); err != nil {
		return res, err
	} else if len(cur) > 0 {
		cursor = string(cur)
	}
	botID := b.s.State.User.ID
	offset, notIndexed := 0, 0
	for page := 1; ; page++ {
		select {
		case <-b.stop:
			return res, errBackfillStopped
		default:
		}
		if !b.cfg.Bool("PERSONA_ENABLED") || b.cfg.ID("PERSONA_USER") != user {
			return res, errBackfillStopped
		}
		sr, wait, err := b.searchMessages(guildID, user, cursor, offset)
		if err != nil {
			return res, err
		}
		if wait > 0 {
			if notIndexed++; notIndexed > personaIndexTries {
				return res, errors.New("the server is not indexed for search yet; try again later")
			}
			if !b.sleep(wait) {
				return res, errBackfillStopped
			}
			continue
		}
		var msgs []*discordgo.Message
		for _, group := range sr.Messages {
			for _, m := range group {
				if m != nil && m.Author != nil && m.Author.ID == user {
					msgs = append(msgs, m)
				}
			}
		}
		if len(msgs) == 0 {
			// A page can be empty while more results exist; look
			// further a few times, then stop.
			if sr.TotalResults == 0 || offset >= 3*personaSearchPage {
				break
			}
			offset += personaSearchPage
			continue
		}
		offset = 0
		parents := map[string]string{}
		for _, t := range sr.Threads {
			if t != nil {
				parents[t.ID] = t.ParentID
			}
		}
		var items []markov.Item
		oldest := uint64(0)
		b.m.backfillMsgs.WithLabelValues("found").Add(float64(len(msgs)))
		for _, m := range msgs {
			res.found++
			if id, err := strconv.ParseUint(m.ID, 10, 64); err == nil && (oldest == 0 || id < oldest) {
				oldest = id
			}
			m.GuildID = guildID
			words := markov.Tokenize(markovNames(m, botID))
			if b.personaSkipped(m.ChannelID, parents) || len(words) == 0 || b.markovBlocks(words) {
				res.skipped++
				b.m.backfillMsgs.WithLabelValues("skipped").Inc()
				continue
			}
			items = append(items, markov.Item{ID: m.ID, Words: words})
		}
		n, err := b.persona.LearnOnce(items)
		if err != nil {
			return res, err
		}
		res.learned += n
		b.m.backfillMsgs.WithLabelValues("learned").Add(float64(n))
		if oldest == 0 {
			return res, errors.New("the search gave messages with no valid IDs")
		}
		cursor = strconv.FormatUint(oldest, 10)
		if err := b.persona.SetMeta(personaCursorKey, []byte(cursor)); err != nil {
			return res, err
		}
		if page%10 == 0 && progress != nil {
			progress(res)
		}
		if !b.sleep(personaSearchPause) {
			return res, errBackfillStopped
		}
	}
	return res, b.persona.SetMeta(personaCursorKey, nil)
}

// sleep waits for d, and reports false when the bot stops first.
func (b *Bot) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-b.stop:
		return false
	}
}

// searchResult is the part of a message search answer the bot uses.
type searchResult struct {
	TotalResults int                    `json:"total_results"`
	Messages     [][]*discordgo.Message `json:"messages"`
	Threads      []*discordgo.Channel   `json:"threads"`
}

// searchMessages gets one page of the member's messages before the
// message ID before ("" for the newest), newest first. When Discord has
// not indexed the server yet, it returns how long to wait.
func (b *Bot) searchMessages(guildID, user, before string, offset int) (searchResult, time.Duration, error) {
	var sr searchResult
	q := url.Values{
		"author_id":    {user},
		"sort_by":      {"timestamp"},
		"sort_order":   {"desc"},
		"limit":        {strconv.Itoa(personaSearchPage)},
		"include_nsfw": {"true"},
	}
	if before != "" {
		q.Set("max_id", before)
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	endpoint := discordgo.EndpointGuild(guildID) + "/messages/search"
	body, err := b.s.RequestWithBucketID(http.MethodGet, endpoint+"?"+q.Encode(), nil, endpoint)
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusAccepted {
		var wait struct {
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(rest.ResponseBody, &wait)
		return sr, max(personaIndexWait, time.Duration(wait.RetryAfter*float64(time.Second))), nil
	}
	if err != nil {
		return sr, 0, fmt.Errorf("search: %w", err)
	}
	if err := json.Unmarshal(body, &sr); err != nil {
		return sr, 0, fmt.Errorf("search answer: %w", err)
	}
	return sr, 0, nil
}
