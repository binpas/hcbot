package bot

import (
	"fmt"
	"log"
	"math"
	"math/rand/v2"
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

// The Markov chat learns from messages in MARKOV_LEARN_CHANNELS and talks
// in MARKOV_REPLY_CHANNELS. Without the Message Content option it reads only
// messages that mention the bot, so it learns only from those; with the
// option it learns from every message. In both cases it replies to
// mentions, and after other messages it talks at random
// (MARKOV_REPLY_CHANCE), as a reply or just in the channel
// (MARKOV_RANDOM_AS_REPLY). MARKOV_COOLDOWN keeps it quiet in a channel
// for a while after it talks. MARKOV_MENTION_REPLY decides who gets a
// reply to a mention: always (skips the cooldown), cooldown, or mods only.
// Mentions from mods always get a reply and skip the cooldown. The chain lives in its own
// file, markov.db.

const (
	markovFlushEvery = 15 * time.Second
	markovMaxPending = 20000 // messages kept when writes keep failing
	markovImportStep = 2000  // messages for each Learn call during an import
)

// markovMentions lets a reply ping nobody, not even the replied-to author.
var markovMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}

type markovState struct {
	mu      sync.Mutex
	pending [][]string           // learned messages not yet written to markov.db
	spoke   map[string]time.Time // channel ID → last Markov message, for the cooldown
	block   atomic.Pointer[markov.Blocklist]
	logMu   sync.Mutex // keeps each forward and its result together in MARKOV_LOG_CHANNEL
}

// claim starts the cooldown in a channel and reports true, or reports
// false when the cooldown is still running. force claims it anyway.
func (st *markovState) claim(channelID string, cooldown time.Duration, force bool) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	if !force && now.Before(st.spoke[channelID].Add(cooldown)) {
		return false
	}
	if st.spoke == nil {
		st.spoke = map[string]time.Time{}
	}
	st.spoke[channelID] = now
	return true
}

// cooldownLeft is how long the channel stays quiet.
func (st *markovState) cooldownLeft(channelID string, cooldown time.Duration) time.Duration {
	st.mu.Lock()
	defer st.mu.Unlock()
	return max(0, time.Until(st.spoke[channelID].Add(cooldown)))
}

// percent is true with a chance of p percent (limited to 0-100).
func percent(p int) bool {
	return rand.IntN(100) < max(0, min(p, 100))
}

// percentF is percent for a decimal chance, e.g. 0.05.
func percentF(p float64) bool {
	return rand.Float64()*100 < max(0, min(p, 100))
}

// chanceText shows a decimal percent and, below 10%, the same as "1 in N".
func chanceText(p float64) string {
	p = max(0, min(p, 100))
	s := strconv.FormatFloat(p, 'f', -1, 64) + "%"
	if p > 0 && p < 10 {
		s += fmt.Sprintf(" (about 1 in %d)", int(math.Round(100/p)))
	}
	return s
}

func (b *Bot) markovCooldown() time.Duration {
	return time.Duration(max(0, b.cfg.Int("MARKOV_COOLDOWN"))) * time.Second
}

func (b *Bot) registerMarkov() {
	if err := b.loadMarkovBlocklist(); err != nil {
		log.Printf("load markov blocklist: %v", err)
	}
	text := func(desc string) *discordgo.ApplicationCommandOption {
		return optStr("text", desc+" (end a word with * to match its start)", true)
	}
	b.addGroup(&group{name: "markov", description: "Manage the Markov chat (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{
		name:        "markov status",
		description: "Show the size of the Markov chain and its settings.",
		perm:        permMod,
		handler:     b.cmdMarkovStatus,
	})
	b.addCommand(&command{
		name:        "markov forget",
		description: "Remove a word, or a phrase of several words, from the Markov chain.",
		options:     []*discordgo.ApplicationCommandOption{text("The word or phrase to remove")},
		perm:        permMod,
		handler:     b.cmdMarkovForget,
	})
	b.addCommand(&command{
		name:        "markov lookup",
		description: "Look up a word or phrase in the Markov chain.",
		options:     []*discordgo.ApplicationCommandOption{text("The word or phrase to look up")},
		perm:        permMod,
		handler:     b.cmdMarkovLookup,
	})
	b.addCommand(&command{
		name:        "markov blockadd",
		description: "Block a word or phrase: the Markov chat never learns or says it.",
		options:     []*discordgo.ApplicationCommandOption{text("The word or phrase to block")},
		perm:        permMod,
		handler:     b.cmdMarkovBlockAdd,
	})
	b.addCommand(&command{
		name:        "markov blockremove",
		description: "Remove a word or phrase from the Markov blocklist.",
		options:     []*discordgo.ApplicationCommandOption{text("The blocked word or phrase")},
		perm:        permMod,
		handler:     b.cmdMarkovBlockRemove,
	})
	b.addCommand(&command{
		name:        "markov blocklist",
		description: "List the blocked words and phrases.",
		perm:        permMod,
		handler:     b.cmdMarkovBlocklist,
	})
	b.addCommand(&command{
		name:        "markov import",
		description: "Train the Markov chain from a text file (one message per line) or a DiscordChatExporter .json.",
		options:     []*discordgo.ApplicationCommandOption{optAttachment("file", "A .txt or .json file", true)},
		perm:        permMod,
		handler:     b.cmdMarkovImport,
	})
	b.addCommand(&command{
		name:        "markov prune",
		description: "Remove the word steps seen only once, to make the Markov database smaller.",
		perm:        permMod,
		handler:     b.cmdMarkovPrune,
	})
	b.addCommand(&command{
		name:        "markov reset",
		description: "Delete the whole Markov chain (admin only). The blocklist stays.",
		perm:        permAdmin,
		handler:     b.cmdMarkovReset,
	})
	b.addComponent("mklookup", b.onMarkovLookupForget)
	b.addConfirm("mkforget", b.onMarkovForgetConfirm)
	b.addConfirm("mkprune", b.onMarkovPruneConfirm)
	b.addConfirm("mkreset", b.onMarkovResetConfirm)
}

func (b *Bot) loadMarkovBlocklist() error {
	entries, err := b.db.MarkovBlocklist()
	if err != nil {
		return err
	}
	b.mk.block.Store(markov.NewBlocklist(entries))
	return nil
}

func (b *Bot) markovBlocks(words []string) bool {
	return b.mk.block.Load().Blocks(words)
}

// markovChannel reports whether channelID, or the parent of a thread, is in
// the channel list setting key.
func (b *Bot) markovChannel(key, channelID string) bool {
	list := b.cfg.List(key)
	if len(list) == 0 {
		return false
	}
	if slices.Contains(list, channelID) {
		return true
	}
	// State only: this runs for every message, so it must not call the API.
	ch, err := b.s.State.Channel(channelID)
	return err == nil && ch.IsThread() && slices.Contains(list, ch.ParentID)
}

// markovMessage learns from a message and replies to it when it should.
func (b *Bot) markovMessage(m *discordgo.Message) {
	if b.chain == nil || m.GuildID == "" {
		return
	}
	botID := b.s.State.User.ID
	mentioned := slices.ContainsFunc(m.Mentions, func(u *discordgo.User) bool { return u.ID == botID })
	// Without Message Content, Discord may still send the text of other
	// messages; it is not used.
	var words []string
	if mentioned || b.env.MessageContent {
		words = markov.Tokenize(markovNames(m, botID))
	}

	if b.markovChannel("MARKOV_REPLY_CHANNELS", m.ChannelID) {
		switch {
		case mentioned:
			mode, mod := b.cfg.Choice("MARKOV_MENTION_REPLY"), b.messageFromMod(m)
			if (mod || mode != "mods") && b.mk.claim(m.ChannelID, b.markovCooldown(), mod || mode == "always") {
				b.markovReply(m, words, true)
			}
		case percentF(b.cfg.Float("MARKOV_REPLY_CHANCE")) && b.mk.claim(m.ChannelID, b.markovCooldown(), false):
			b.markovReply(m, words, percent(b.cfg.Int("MARKOV_RANDOM_AS_REPLY")))
		}
	}
	// Without Message Content, only mentions are read, so only they are
	// learned or logged.
	if (!mentioned && !b.env.MessageContent) || !b.markovChannel("MARKOV_LEARN_CHANNELS", m.ChannelID) {
		return
	}
	var skipped string
	switch match := b.mk.block.Load().Match(words); {
	case len(words) == 0:
		skipped = "no words (only mentions, links, or attachments)"
	case match != "":
		skipped = "blocked by ||`" + match + "`||"
	default:
		b.mk.mu.Lock()
		if len(b.mk.pending) < markovMaxPending {
			b.mk.pending = append(b.mk.pending, words)
		} else {
			skipped = "the learn queue is full (writes to markov.db fail; examine the bot's log)"
		}
		b.mk.mu.Unlock()
	}
	b.markovLog(m, len(words), skipped)
}

// markovLog posts the result of a learn to MARKOV_LOG_CHANNEL: a forward
// of the message (only with Message Content; Discord refuses to forward a
// message the bot cannot read), then an embed with the author and the
// result. skipped is why the message was not learned, or "".
func (b *Bot) markovLog(m *discordgo.Message, words int, skipped string) {
	channelID := b.cfg.ID("MARKOV_LOG_CHANNEL")
	if channelID == "" || (skipped == "" && b.cfg.Choice("MARKOV_LOG_MODE") == "skipped") {
		return
	}
	e := makeEmbed("", fmt.Sprintf("✅ **Learned** (%d words)", words), colourGreen)
	if skipped != "" {
		e = makeEmbed("", "🚫 **Not learned:** "+skipped, colourRed)
	}
	e.Fields = []*discordgo.MessageEmbedField{
		{Name: "Author", Value: "<@" + m.Author.ID + ">", Inline: true},
		{Name: "Channel", Value: channelMention(m.ChannelID), Inline: true},
		{Name: "Message", Value: fmt.Sprintf("[Jump](https://discord.com/channels/%s/%s/%s)", m.GuildID, m.ChannelID, m.ID), Inline: true},
	}
	b.mk.logMu.Lock()
	defer b.mk.logMu.Unlock()
	if b.env.MessageContent {
		if _, err := b.s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
			Reference: &discordgo.MessageReference{
				Type: discordgo.MessageReferenceTypeForward, MessageID: m.ID, ChannelID: m.ChannelID, GuildID: m.GuildID,
			},
		}); err != nil {
			log.Printf("markov log forward %s: %v", m.ID, err)
		}
	}
	if _, err := b.s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{e}, AllowedMentions: markovMentions,
	}); err != nil {
		log.Printf("markov log: %v", err)
	}
}

// markovNames returns a message's text with each user mention replaced by
// that user's name, so the chat learns the name but can never ping. Mentions
// of the bot itself are removed. Discord sends the mentioned users with the
// message, so this needs no API call.
func markovNames(m *discordgo.Message, botID string) string {
	var pairs []string
	for _, u := range m.Mentions {
		name := u.GlobalName
		if name == "" {
			name = u.Username
		}
		if u.ID == botID {
			name = ""
		}
		pairs = append(pairs, "<@"+u.ID+">", " "+name+" ", "<@!"+u.ID+">", " "+name+" ")
	}
	return strings.NewReplacer(pairs...).Replace(m.Content)
}

// messageFromMod reports whether a message's author is a mod: MOD_ROLE, the
// Administrator permission, or the server owner. It uses the roles sent
// with the message and the cached server roles, so it needs no API call.
func (b *Bot) messageFromMod(m *discordgo.Message) bool {
	if m.Member == nil || m.Author == nil {
		return false
	}
	if role := b.cfg.ID("MOD_ROLE"); role != "" && slices.Contains(m.Member.Roles, role) {
		return true
	}
	g, err := b.s.State.Guild(m.GuildID)
	if err != nil {
		return false
	}
	if g.OwnerID == m.Author.ID {
		return true
	}
	for _, r := range g.Roles {
		if (r.ID == g.ID || slices.Contains(m.Member.Roles, r.ID)) && r.Permissions&discordgo.PermissionAdministrator != 0 {
			return true
		}
	}
	return false
}

// markovReply posts a generated message, as a reply to m when asReply is
// set. It tries to start from a word of the message, longer words first.
func (b *Bot) markovReply(m *discordgo.Message, words []string, asReply bool) {
	seed := slices.Clone(words)
	slices.SortStableFunc(seed, func(x, y string) int {
		return utf8.RuneCountInString(y) - utf8.RuneCountInString(x)
	})
	seed = seed[:min(len(seed), 5)]
	out, err := b.chain.Generate(seed, func(w []string) bool { return !b.markovBlocks(w) })
	if err != nil {
		log.Printf("markov generate: %v", err)
		return
	}
	if out == "" {
		return
	}
	msg := &discordgo.MessageSend{Content: truncate(out, 2000), AllowedMentions: markovMentions}
	if gif := b.markovGIF(out); gif != nil {
		msg.Embeds = []*discordgo.MessageEmbed{gif}
	}
	if asReply {
		msg.Reference = m.Reference()
	}
	if _, err := b.s.ChannelMessageSendComplex(m.ChannelID, msg); err != nil && asReply {
		// The message may be gone; answer without the reply link.
		msg.Reference = nil
		_, err = b.s.ChannelMessageSendComplex(m.ChannelID, msg)
		if err != nil {
			log.Printf("markov reply: %v", err)
		}
	} else if err != nil {
		log.Printf("markov reply: %v", err)
	}
}

// flushMarkov writes the learned messages to markov.db.
func (b *Bot) flushMarkov() {
	if b.chain == nil {
		return
	}
	b.mk.mu.Lock()
	batch := b.mk.pending
	b.mk.pending = nil
	b.mk.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	if err := b.chain.Learn(batch); err != nil {
		log.Printf("markov learn: %v", err)
		b.mk.mu.Lock()
		b.mk.pending = append(batch, b.mk.pending...)
		if len(b.mk.pending) > markovMaxPending {
			b.mk.pending = b.mk.pending[:markovMaxPending]
		}
		b.mk.mu.Unlock()
	}
}

func (b *Bot) cmdMarkovStatus(c *Ctx) {
	b.flushMarkov()
	st, err := b.chain.Stats()
	if err != nil {
		c.dbFailed("markov stats", err)
		return
	}
	channels := func(key string) string {
		ids := b.cfg.List(key)
		if len(ids) == 0 {
			return "*none — set it in `/setup`*"
		}
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = channelMention(id)
		}
		return truncate(strings.Join(out, ", "), 1000)
	}
	e := makeEmbed("🦜 Markov Chat", "", colourBlurple)
	e.Fields = []*discordgo.MessageEmbedField{
		{Name: "Words", Value: fmt.Sprint(st.Words), Inline: true},
		{Name: "Word steps", Value: fmt.Sprint(st.Transitions), Inline: true},
		{Name: "Database", Value: fmt.Sprintf("%.1f MB", float64(st.Bytes)/(1<<20)), Inline: true},
		{Name: "Learns from", Value: channels("MARKOV_LEARN_CHANNELS")},
		{Name: "Replies in", Value: channels("MARKOV_REPLY_CHANNELS")},
		{Name: "Blocklist", Value: fmt.Sprintf("%d word(s) or phrase(s)", b.mk.block.Load().Len())},
	}
	mention := "always, even during the cooldown"
	switch b.cfg.Choice("MARKOV_MENTION_REPLY") {
	case "cooldown":
		mention = "only when the cooldown is over (mods: always)"
	case "mods":
		mention = "from mods only"
	}
	talk := fmt.Sprintf("Replies to @mentions %s. Talks after **%s** of other messages (**%d%%** of those as a reply). "+
		"Cooldown: **%ds**", mention, chanceText(b.cfg.Float("MARKOV_REPLY_CHANCE")),
		max(0, min(b.cfg.Int("MARKOV_RANDOM_AS_REPLY"), 100)), int(b.markovCooldown().Seconds()))
	if left := b.mk.cooldownLeft(c.i.ChannelID, b.markovCooldown()); left > 0 {
		talk += fmt.Sprintf(" (%ds left in this channel)", int(left.Seconds()+0.5))
	}
	e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Talking", Value: talk + "."})
	gifs := "Off: `GIPHY_API_KEY` is not set in `.env`."
	if b.env.GiphyKey != "" {
		gifs = fmt.Sprintf("Adds a GIF to **%s** of Markov messages. Rating up to **%s**. Cooldown: **%ds** for the full bot",
			chanceText(b.cfg.Float("MARKOV_GIF_CHANCE")), b.cfg.Choice("MARKOV_GIF_RATING"), int(b.gifCooldown().Seconds()))
		if left := b.gif.cooldownLeft(b.gifCooldown()); left > 0 {
			gifs += fmt.Sprintf(" (%ds left)", int(left.Seconds()+0.5))
		}
		gifs += "."
	}
	e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "GIFs", Value: gifs})
	learn := "Message Content is off: learns only from messages that @mention the bot, and from `/markov import`."
	if b.env.MessageContent {
		learn = "Learns from all messages in the learn channels."
	}
	e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Learning", Value: learn})
	c.Reply(true, e)
}

// markovPhrase reads the text option as normalized words. When it returns
// false, it has already answered.
func markovPhrase(c *Ctx) ([]string, bool) {
	text, _ := c.Str("text")
	phrase := markov.Phrase(text)
	if len(phrase) == 0 {
		c.Reply(true, errEmbed("That has no words to use (mentions and links are ignored)."))
		return nil, false
	}
	if w := markov.ShortPrefix(phrase); w != "" {
		c.Reply(true, errEmbed(fmt.Sprintf("`%s` is too short. Put at least %d letters before the `*`.", w, markov.MinPrefix)))
		return nil, false
	}
	return phrase, true
}

func (b *Bot) cmdMarkovForget(c *Ctx) {
	phrase, ok := markovPhrase(c)
	if !ok {
		return
	}
	b.askMarkovForget(c, phrase, "", false)
}

// askMarkovForget counts the steps a phrase makes and asks to remove them.
// intro goes before the question. With update, the question replaces the
// message of the clicked button.
func (b *Bot) askMarkovForget(c *Ctx, phrase []string, intro string, update bool) {
	respond := func(e *discordgo.MessageEmbed, components []discordgo.MessageComponent) {
		if update {
			c.Update([]*discordgo.MessageEmbed{e}, components)
		} else {
			c.ReplyComplex(true, []*discordgo.MessageEmbed{e}, components)
		}
	}
	b.flushMarkov()
	quoted := "`" + strings.Join(phrase, " ") + "`"
	n, err := b.chain.CountPhrase(phrase)
	if err != nil {
		c.dbFailed("markov count", err)
		return
	}
	if n == 0 {
		respond(okEmbed(intro+"The Markov chain does not contain "+quoted+"."), []discordgo.MessageComponent{})
		return
	}
	id, err := b.savePending("mkforget", c.UserID(), phrase)
	if err != nil {
		c.dbFailed("save pending forget", err)
		return
	}
	respond(makeEmbed("🧹 Remove from the Markov chain?",
		fmt.Sprintf("%sRemove **%d** word step(s) that contain %s?", intro, n, quoted), colourOrange),
		confirmButtons("mkforget", c.UserID(), id))
}

// cmdMarkovLookup shows if the chain contains a word or phrase. For one
// word it shows each stored form ("cat", "cat!") and the words around it.
func (b *Bot) cmdMarkovLookup(c *Ctx) {
	phrase, ok := markovPhrase(c)
	if !ok {
		return
	}
	b.flushMarkov()
	quoted := "`" + strings.Join(phrase, " ") + "`"
	e := makeEmbed("🔎 Markov lookup", "", colourBlurple)
	list := func(words []string) string {
		if len(words) == 0 {
			return "*none*"
		}
		return truncate("`"+strings.Join(words, "`, `")+"`", 1000)
	}
	var found bool
	if len(phrase) == 1 {
		info, err := b.chain.LookupWord(phrase[0])
		if err != nil {
			c.dbFailed("markov lookup", err)
			return
		}
		if found = len(info.Forms) > 0; found {
			forms := make([]string, len(info.Forms))
			for i, f := range info.Forms {
				forms[i] = fmt.Sprintf("`%s` (%d×)", f.Word, f.Seen)
			}
			e.Fields = append(e.Fields,
				&discordgo.MessageEmbedField{Name: fmt.Sprintf("Stored forms (times seen): %d", info.Total),
					Value: truncate(strings.Join(forms, ", "), 1000)},
				&discordgo.MessageEmbedField{Name: "Often before it", Value: list(info.Before), Inline: true},
				&discordgo.MessageEmbedField{Name: "Often after it", Value: list(info.After), Inline: true})
		}
	} else {
		n, err := b.chain.CountPhrase(phrase)
		if err != nil {
			c.dbFailed("markov lookup", err)
			return
		}
		if found = n > 0; found {
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Word steps", Value: fmt.Sprint(n)})
		}
	}
	e.Description = "The Markov chain does not contain " + quoted + "."
	if found {
		e.Description = "The Markov chain contains " + quoted + "."
	}
	blocked := "No"
	if b.markovBlocks(phrase) {
		blocked = "Yes: the bot does not learn or say it."
	}
	e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Blocked", Value: blocked})
	var components []discordgo.MessageComponent
	if found {
		id, err := b.savePending("mklookup", c.UserID(), phrase)
		if err != nil {
			c.dbFailed("save pending lookup", err)
			return
		}
		components = []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Forget this", Style: discordgo.DangerButton,
				Emoji: &discordgo.ComponentEmoji{Name: "🧹"}, CustomID: "mklookup:" + id},
		}}}
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{e}, components)
}

// onMarkovLookupForget is the "Forget this" button of /markov lookup. It
// replaces the lookup with the usual forget question.
func (b *Bot) onMarkovLookupForget(c *Ctx) {
	if len(c.Args) < 1 {
		return
	}
	var phrase []string
	if !b.takePending(c, "mklookup", c.Args[0], &phrase) {
		return
	}
	b.askMarkovForget(c, phrase, "", true)
}

func (b *Bot) onMarkovForgetConfirm(c *Ctx, extra []string) {
	if len(extra) < 1 {
		return
	}
	var phrase []string
	if !b.takePending(c, "mkforget", extra[0], &phrase) {
		return
	}
	c.DeferUpdate()
	b.flushMarkov()
	n, err := b.chain.Forget(phrase)
	if err != nil {
		c.logErr("markov forget", err)
		c.FinishUpdate(errEmbed("Something went wrong with the Markov database — check the bot's log."))
		return
	}
	c.FinishUpdate(okEmbed(fmt.Sprintf("Removed **%d** word step(s) with `%s`.", n, strings.Join(phrase, " "))))
}

func (b *Bot) cmdMarkovBlockAdd(c *Ctx) {
	phrase, ok := markovPhrase(c)
	if !ok {
		return
	}
	entry := strings.Join(phrase, " ")
	added, err := b.db.AddMarkovBlock(entry, c.UserID())
	if err == nil {
		err = b.loadMarkovBlocklist()
	}
	if err != nil {
		c.dbFailed("add markov block", err)
		return
	}
	intro := "`" + entry + "` is now blocked.\n\n"
	if !added {
		intro = "`" + entry + "` was already blocked.\n\n"
	}
	b.askMarkovForget(c, phrase, intro, false)
}

func (b *Bot) cmdMarkovBlockRemove(c *Ctx) {
	phrase, ok := markovPhrase(c)
	if !ok {
		return
	}
	entry := strings.Join(phrase, " ")
	removed, err := b.db.RemoveMarkovBlock(entry)
	if err == nil {
		err = b.loadMarkovBlocklist()
	}
	if err != nil {
		c.dbFailed("remove markov block", err)
		return
	}
	if !removed {
		c.Reply(true, errEmbed("`"+entry+"` is not on the blocklist."))
		return
	}
	c.Reply(true, okEmbed("`"+entry+"` is no longer blocked."))
}

func (b *Bot) cmdMarkovBlocklist(c *Ctx) {
	entries, err := b.db.MarkovBlocklist()
	if err != nil {
		c.dbFailed("markov blocklist", err)
		return
	}
	if len(entries) == 0 {
		c.Reply(true, okEmbed("The blocklist is empty. Add to it with `/markov blockadd`."))
		return
	}
	var sb strings.Builder
	for i, e := range entries {
		line := "||" + e + "||\n"
		if sb.Len()+len(line) > 3900 {
			fmt.Fprintf(&sb, "…and %d more", len(entries)-i)
			break
		}
		sb.WriteString(line)
	}
	c.Reply(true, makeEmbed(fmt.Sprintf("🚫 Markov blocklist (%d)", len(entries)), sb.String(), colourRed))
}

func (b *Bot) cmdMarkovImport(c *Ctx) {
	file := c.Attachment("file")
	maxMB := b.cfg.Int("MARKOV_IMPORT_MAX_MB")
	raw, ok := b.readAttachment(c, file, int64(maxMB)<<20,
		fmt.Sprintf("That file is too large (the limit is %d MB, set by `MARKOV_IMPORT_MAX_MB`).", maxMB))
	if !ok {
		return
	}
	lines, err := markov.ParseImport(file.Filename, raw)
	if err != nil {
		c.Reply(true, errEmbed("Couldn't read that JSON file: "+err.Error()))
		return
	}
	kept, blocked := markov.Prepare(lines, b.mk.block.Load())
	for i := 0; i < len(kept); i += markovImportStep {
		if err := b.chain.Learn(kept[i:min(i+markovImportStep, len(kept))]); err != nil {
			c.dbFailed("markov import", err)
			return
		}
	}
	c.Reply(true, okEmbed(fmt.Sprintf("Learned **%d** message(s) from `%s`. Skipped **%d** blocked and **%d** empty.",
		len(kept), file.Filename, blocked, len(lines)-len(kept)-blocked)))
}

func (b *Bot) cmdMarkovPrune(c *Ctx) {
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("✂️ Prune the Markov chain?",
		"This removes every word step seen only once. The database gets smaller, and replies stay about the same.",
		colourOrange)}, confirmButtons("mkprune", c.UserID()))
}

func (b *Bot) onMarkovPruneConfirm(c *Ctx, _ []string) {
	c.DeferUpdate()
	b.flushMarkov()
	before, _ := b.chain.Stats()
	n, err := b.chain.Prune()
	if err != nil {
		c.logErr("markov prune", err)
		c.FinishUpdate(errEmbed("Something went wrong with the Markov database — check the bot's log."))
		return
	}
	after, _ := b.chain.Stats()
	c.FinishUpdate(okEmbed(fmt.Sprintf("Removed **%d** word step(s). The database went from %.1f MB to %.1f MB.",
		n, float64(before.Bytes)/(1<<20), float64(after.Bytes)/(1<<20))))
}

func (b *Bot) cmdMarkovReset(c *Ctx) {
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("⚠️ Delete the whole Markov chain?",
		"The bot forgets everything it learned. The blocklist stays. This cannot be undone.", colourRed)},
		confirmButtons("mkreset", c.UserID()))
}

func (b *Bot) onMarkovResetConfirm(c *Ctx, _ []string) {
	c.DeferUpdate()
	b.mk.mu.Lock()
	b.mk.pending = nil
	b.mk.mu.Unlock()
	if err := b.chain.Reset(); err != nil {
		c.logErr("markov reset", err)
		c.FinishUpdate(errEmbed("Something went wrong with the Markov database — check the bot's log."))
		return
	}
	c.FinishUpdate(okEmbed("The Markov chain is now empty."))
}
