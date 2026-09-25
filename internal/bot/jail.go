package bot

import (
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/config"
	"github.com/binpas/hcbot/internal/db"
)

// humaniseDuration renders a second count as e.g. "1d 2h 30m".
func humaniseDuration(seconds int64) string {
	if seconds <= 0 {
		return "0m"
	}
	var parts []string
	for _, u := range []struct {
		label string
		size  int64
	}{{"w", 604800}, {"d", 86400}, {"h", 3600}, {"m", 60}} {
		if seconds >= u.size {
			parts = append(parts, fmt.Sprintf("%d%s", seconds/u.size, u.label))
			seconds %= u.size
		}
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}
	return strings.Join(parts[:min(3, len(parts))], " ")
}

// arrestMember jails a member: it strips their other roles — except
// JAIL_PROTECTED_ROLES, managed roles, and anything at or above the bot's
// top role — records
// what was stripped so release can restore it, and assigns the jail role.
// seconds is 0 for an indefinite sentence.
func (b *Bot) arrestMember(guildID string, m *discordgo.Member, arrestedBy string, seconds int64, reason string) (ok bool, problem string, sentenceID int64) {
	jailRole := b.role(guildID, b.cfg.ID("JAIL_ROLE"))
	if jailRole == nil {
		return false, "No jail role configured. Run `/setup` and set `JAIL_ROLE`.", 0
	}
	top := b.botTopRole(guildID)
	if !roleBelow(jailRole, top) {
		return false, roleMention(jailRole.ID) + " is above my highest role — I can't assign it.", 0
	}
	if active, err := b.db.HasActiveSentence(guildID, m.User.ID); err != nil {
		log.Printf("arrest: check sentence: %v", err)
		return false, "I couldn't read the sentence records.", 0
	} else if active {
		return false, userMention(m.User.ID) + " is already serving a sentence.", 0
	}

	protected := map[string]bool{}
	for _, id := range b.cfg.List("JAIL_PROTECTED_ROLES") {
		protected[id] = true
	}
	var removed, kept []string
	for _, id := range m.Roles {
		r := b.role(guildID, id)
		switch {
		case id == jailRole.ID:
			// re-added below
		case r != nil && !protected[id] && !r.Managed && roleBelow(r, top):
			// Managed roles (bot integrations, Server Booster) can't be
			// removed or added by anyone, so they always stay.
			removed = append(removed, id)
		default:
			kept = append(kept, id)
		}
	}

	releaseAt := ""
	if seconds > 0 {
		releaseAt = time.Now().UTC().Add(time.Duration(seconds) * time.Second).Format(db.SentenceTimeFormat)
	}

	// One combined edit rather than a remove-then-add pair, so a failure
	// can't leave someone stripped of every role but not jailed.
	roles := append(kept, jailRole.ID)
	why := reason
	if why == "" {
		why = "Arrested"
	}
	if _, err := b.s.GuildMemberEdit(guildID, m.User.ID, &discordgo.GuildMemberParams{Roles: &roles},
		auditReason(why)); err != nil {
		log.Printf("arrest: edit roles: %v", err)
		return false, "I couldn't update that member's roles.", 0
	}

	sentenceID, err := b.db.CreateSentence(guildID, m.User.ID, m.User.Username, arrestedBy, reason, releaseAt, removed)
	if err != nil {
		log.Printf("arrest: create sentence: %v", err)
		return false, "I jailed them, but couldn't save the sentence record.", 0
	}

	if ch := b.channel(b.cfg.ID("JAIL_CHANNEL")); ch != nil {
		length := "an indefinite period"
		if seconds > 0 {
			length = humaniseDuration(seconds)
		}
		text := fmt.Sprintf("%s has been jailed for **%s**.", userMention(m.User.ID), length)
		if reason != "" {
			text += "\n**Reason:** " + reason
		}
		text += fmt.Sprintf("\n*Sentence `#%d`*", sentenceID)
		if len(removed) > 0 {
			text += fmt.Sprintf(" Stripped **%d** other role(s), restored on release.", len(removed))
		}
		if _, err := b.s.ChannelMessageSend(ch.ID, text); err != nil {
			log.Printf("arrest: jail channel notice: %v", err)
		}
	}
	return true, "", sentenceID
}

// maxSentence caps parsed durations so the release time can't overflow.
const maxSentence = 100 * 365 * 86400

var durationRE = regexp.MustCompile(`(?i)(\d+)\s*([smhdw])`)

var durationUnits = map[string]int64{"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}

// parseDuration parses "30m", "2h30m", "1d", or a bare number (minutes)
// into seconds. ok is false when the text isn't a positive duration.
func parseDuration(text string) (seconds int64, ok bool) {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil && !strings.HasPrefix(text, "+") && !strings.HasPrefix(text, "-") {
		if n <= 0 || n > maxSentence/60 {
			return 0, false
		}
		return n * 60, true // a bare number means minutes
	}
	var total int64
	for _, m := range durationRE.FindAllStringSubmatch(text, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || n > maxSentence {
			return 0, false
		}
		total += n * durationUnits[strings.ToLower(m[2])]
		if total > maxSentence {
			return 0, false
		}
	}
	return total, total > 0
}

func parseSentenceTime(s string) (time.Time, bool) {
	t, err := time.ParseInLocation(db.SentenceTimeFormat, s, time.UTC)
	return t, err == nil
}

// sentenceRemaining is the time left on a sentence, in words.
func sentenceRemaining(s *db.Sentence) string {
	if s.ReleaseAt == "" {
		return "indefinite"
	}
	release, ok := parseSentenceTime(s.ReleaseAt)
	if !ok {
		return "unknown"
	}
	if left := int64(time.Until(release).Seconds()); left > 0 {
		return humaniseDuration(left)
	}
	return "due for release"
}

// sentenceServed is how long a sentence has been served so far.
func sentenceServed(s *db.Sentence) string {
	jailed, ok := parseSentenceTime(s.JailedAt)
	if !ok {
		return "unknown"
	}
	return humaniseDuration(int64(time.Since(jailed).Seconds()))
}

// guildChannels returns a snapshot of the guild's cached channels (no threads).
func (b *Bot) guildChannels(guildID string) (*discordgo.Guild, []*discordgo.Channel) {
	g, err := b.s.State.Guild(guildID)
	if err != nil {
		return nil, nil
	}
	b.s.State.RLock()
	defer b.s.State.RUnlock()
	return g, slices.Clone(g.Channels)
}

// applyJailOverwrites denies the jail role everywhere except the jail
// channel. Each change is merged into the role's current overwrite, so
// other fields already set on it (e.g. a View Channel deny from /jail
// audit) survive. It returns the counts, and the jail channel with its new
// overwrite (nil if the jail channel wasn't updated).
func (b *Bot) applyJailOverwrites(guildID string) (done, failed int, jailCh *discordgo.Channel) {
	jailRole := b.role(guildID, b.cfg.ID("JAIL_ROLE"))
	if jailRole == nil {
		return 0, 0, nil
	}
	jailChannelID := b.cfg.ID("JAIL_CHANNEL")
	_, chans := b.guildChannels(guildID)
	for _, ch := range chans {
		if ch.Type == discordgo.ChannelTypeGuildCategory {
			continue
		}
		allow := ch.ID == jailChannelID
		ow := roleOverwrite(ch, jailRole.ID)
		setPerm(&ow, discordgo.PermissionSendMessages, allow)
		setPerm(&ow, discordgo.PermissionSendMessagesInThreads, allow)
		setPerm(&ow, discordgo.PermissionCreatePublicThreads, false)
		setPerm(&ow, discordgo.PermissionCreatePrivateThreads, false)
		setPerm(&ow, discordgo.PermissionAddReactions, allow)
		setPerm(&ow, discordgo.PermissionVoiceSpeak, allow)
		setPerm(&ow, discordgo.PermissionVoiceConnect, allow)
		if err := b.s.ChannelPermissionSet(ch.ID, jailRole.ID, discordgo.PermissionOverwriteTypeRole,
			ow.Allow, ow.Deny, auditReason("Jail role restrictions")); err != nil {
			log.Printf("jail setup: #%s: %v", ch.Name, err)
			failed++
			continue
		}
		done++
		if allow {
			jailCh = withOverwrite(ch, ow)
		}
	}
	if err := b.setConfig(config.JailSetupKey, "1"); err != nil {
		log.Printf("jail setup: save %s: %v", config.JailSetupKey, err)
	}
	return done, failed, jailCh
}

// releaseMember restores the roles stripped at arrest, removes the jail
// role, and marks the sentence served. It returns the member, or nil if
// they have left the server.
func (b *Bot) releaseMember(guildID string, s *db.Sentence, reason string) *discordgo.Member {
	m := b.freshMember(guildID, s.UserID)
	if m != nil {
		jailRoleID := b.cfg.ID("JAIL_ROLE")
		top := b.botTopRole(guildID)
		var roles []string
		for _, id := range m.Roles {
			if id != jailRoleID && id != guildID {
				roles = append(roles, id)
			}
		}
		for _, id := range s.RemovedRoles {
			if r := b.role(guildID, id); r != nil && !r.Managed && roleBelow(r, top) && !slices.Contains(roles, id) {
				roles = append(roles, id)
			}
		}
		// One combined edit (drop the jail role, add back what was stripped),
		// so a partial failure can't leave someone half released.
		if _, err := b.s.GuildMemberEdit(guildID, s.UserID, &discordgo.GuildMemberParams{Roles: &roles},
			auditReason(reason)); err != nil {
			log.Printf("release sentence #%d: edit roles: %v", s.ID, err)
		}
	}
	if err := b.db.ReleaseSentence(s.ID); err != nil {
		log.Printf("release sentence #%d: %v", s.ID, err)
	}
	return m
}

// runJailReleases releases members whose sentences have expired.
func (b *Bot) runJailReleases() {
	rows, err := b.db.ExpiredSentences()
	if err != nil {
		log.Printf("jail release loop: %v", err)
		return
	}
	for _, s := range rows {
		g, err := b.s.State.Guild(s.GuildID)
		if err != nil {
			continue
		}
		m := b.releaseMember(s.GuildID, s, "Sentence served")
		desc := fmt.Sprintf("**%s** was released after serving **%s**.\n**Sentence:** `#%d`",
			s.Username, sentenceServed(s), s.ID)
		if s.Reason != "" {
			desc += "\n**Reason:** " + s.Reason
		}
		b.modLog(makeEmbed("🔓 Released", desc, colourGreen))
		if m != nil {
			if dm, err := b.s.UserChannelCreate(s.UserID); err == nil {
				_, _ = b.s.ChannelMessageSend(dm.ID, fmt.Sprintf("You've been released from jail in **%s**.", g.Name))
			}
		}
	}
}

func (b *Bot) registerJail() {
	arrestOpts := func() []*discordgo.ApplicationCommandOption {
		return []*discordgo.ApplicationCommandOption{
			optUser("member", "Member to jail", true),
			optStr("time", "How long, e.g. 30m, 2h, 1d. Omit to jail indefinitely.", false),
			optStr("reason", "Reason for the arrest", false),
		}
	}

	b.addGroup(&group{name: "jail", description: "Manage jail sentences (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{
		name:        "jail arrest",
		description: "Jail a member.",
		options:     arrestOpts(),
		perm:        permMod,
		handler:     b.cmdArrest,
	})
	b.addCommand(&command{
		name:        "jail list",
		description: "List everyone currently in jail.",
		perm:        permMod,
		handler:     b.cmdJailList,
	})
	b.addCommand(&command{
		name:        "jail amend",
		description: "Update a sentence's reason and/or length.",
		options: []*discordgo.ApplicationCommandOption{
			optInt("sentence_id", "Sentence ID from /jail list", true),
			optStr("time", "New total length from the arrest time, e.g. 2h. Use 'indefinite' to clear.", false),
			optStr("reason", "New reason for the sentence", false),
		},
		perm:    permMod,
		handler: b.cmdJailAmend,
	})
	b.addCommand(&command{
		name:        "jail release",
		description: "Release a member from jail early.",
		options:     []*discordgo.ApplicationCommandOption{optUser("member", "Member to release", true)},
		perm:        permMod,
		handler:     b.cmdJailRelease,
	})
	b.addCommand(&command{
		name:        "jail setup",
		description: "Apply jail role permission overwrites to every channel.",
		perm:        permMod,
		handler:     b.cmdJailSetup,
	})
	b.addCommand(&command{
		name:        "jail audit",
		description: "Check (and optionally fix) whether the jail role can see channels it shouldn't.",
		perm:        permMod,
		handler:     b.cmdJailAudit,
	})
	// Top-level alias of /jail arrest, kept by request.
	b.addCommand(&command{
		name:        "arrest",
		description: "Jail a member (mod only, alias of /jail arrest).",
		options:     arrestOpts(),
		perm:        permMod,
		defaultPerm: manageMessages,
		handler:     b.cmdArrest,
	})
	b.addConfirm("jailaudit", b.onJailAuditConfirm)
}

func (b *Bot) cmdArrest(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	reason, _ := c.Str("reason")
	var seconds int64
	if t, ok := c.Str("time"); ok {
		if s, ok := parseDuration(t); ok {
			seconds = s
		} else {
			// They probably skipped the duration and went straight to a reason.
			reason = strings.TrimSpace(t + " " + reason)
		}
	}

	ok, problem, sentenceID := b.arrestMember(c.GuildID(), m, c.UserID(), seconds, reason)
	if !ok {
		c.Reply(true, errEmbed(problem))
		return
	}
	length := "indefinite"
	if seconds > 0 {
		length = humaniseDuration(seconds)
	}
	if reason == "" {
		reason = noReason
	}
	embed := makeEmbed("🚔 Member Arrested",
		fmt.Sprintf("**%s** was jailed by %s.\n**Sentence:** `#%d` • **Length:** %s\n**Reason:** %s",
			m.User.String(), userMention(c.UserID()), sentenceID, length, reason),
		colourDarkOrange)
	c.Reply(false, embed)
	b.modLog(embed)
}

func (b *Bot) cmdJailList(c *Ctx) {
	c.Defer(true) // looking up each member can take a while
	rows, err := b.db.ActiveSentences(c.GuildID())
	if err != nil {
		c.logErr("list sentences", err)
		c.Reply(true, errEmbed("I couldn't read the sentence records."))
		return
	}
	if len(rows) == 0 {
		c.Reply(true, makeEmbed("🚔 Jail", "Nobody is currently in jail.", colourGreen))
		return
	}
	embed := makeEmbed("🚔 Current Sentences", fmt.Sprintf("**%d** member(s) in jail", len(rows)), colourDarkOrange)
	for _, s := range rows[:min(len(rows), maxEmbedFields)] {
		who := fmt.Sprintf("**%s** *(left server)*", s.Username)
		if b.resolveMember(c.GuildID(), s.UserID) != nil {
			who = userMention(s.UserID)
		}
		reason := s.Reason
		if reason == "" {
			reason = "*none given*"
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name: fmt.Sprintf("#%d — %s", s.ID, s.Username),
			Value: truncate(fmt.Sprintf("%s\n**Served:** %s • **Remaining:** %s\n**Reason:** %s",
				who, sentenceServed(s), sentenceRemaining(s), reason), 1024),
		})
	}
	if len(rows) > maxEmbedFields {
		embed.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Showing %d of %d.", maxEmbedFields, len(rows))}
	}
	c.Reply(true, embed)
}

func (b *Bot) cmdJailAmend(c *Ctx) {
	id := c.Int("sentence_id", 0)
	s, err := b.db.SentenceByID(id)
	if err != nil {
		c.logErr("get sentence", err)
		c.Reply(true, errEmbed("I couldn't read the sentence records."))
		return
	}
	if s == nil || s.GuildID != c.GuildID() {
		c.Reply(true, errEmbed(fmt.Sprintf("No sentence with ID `%d`.", id)))
		return
	}
	if !s.Active {
		c.Reply(true, errEmbed(fmt.Sprintf("Sentence `#%d` has already been served.", id)))
		return
	}

	var releaseAt, newReason *string
	var changes []string
	reason, _ := c.Str("reason")
	if t, ok := c.Str("time"); ok && strings.TrimSpace(t) != "" {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "indefinite", "forever", "none", "clear":
			releaseAt = new(string)
			changes = append(changes, "length → **indefinite**")
		default:
			if secs, ok := parseDuration(t); !ok {
				// Not a duration — treat the whole thing as part of the reason.
				reason = strings.TrimSpace(t + " " + reason)
			} else if jailed, ok := parseSentenceTime(s.JailedAt); ok {
				r := jailed.Add(time.Duration(secs) * time.Second).Format(db.SentenceTimeFormat)
				releaseAt = &r
				changes = append(changes, fmt.Sprintf("length → **%s** from arrest time", humaniseDuration(secs)))
			}
		}
	}
	if reason != "" {
		newReason = &reason
		changes = append(changes, fmt.Sprintf("reason → **%s**", reason))
	}
	if len(changes) == 0 {
		c.Reply(true, errEmbed("Nothing to change. Provide a new time, a new reason, or both."))
		return
	}
	if err := b.db.AmendSentence(id, releaseAt, newReason); err != nil {
		c.logErr("amend sentence", err)
		c.Reply(true, errEmbed("I couldn't save the change."))
		return
	}
	embed := makeEmbed("📝 Sentence Amended",
		fmt.Sprintf("Sentence `#%d` for **%s** updated by %s.\n• %s",
			id, s.Username, userMention(c.UserID()), strings.Join(changes, "\n• ")),
		colourYellow)
	c.Reply(false, embed)
	b.modLog(embed)
}

func (b *Bot) cmdJailRelease(c *Ctx) {
	m := c.targetMember()
	if m == nil {
		return
	}
	s, err := b.db.ActiveSentence(c.GuildID(), m.User.ID)
	if err != nil {
		c.logErr("get sentence", err)
		c.Reply(true, errEmbed("I couldn't read the sentence records."))
		return
	}
	if s == nil {
		c.Reply(true, errEmbed(userMention(m.User.ID)+" isn't currently in jail."))
		return
	}

	served := sentenceServed(s)
	by := c.Member().User.String()
	b.releaseMember(c.GuildID(), s, "Commuted by "+by)

	// Record the jailing on the member's rap sheet.
	note := "Jailed for " + served
	if s.Reason != "" {
		note += " — " + s.Reason
	}
	note += fmt.Sprintf(" (sentence #%d, commuted by %s)", s.ID, by)
	if _, err := b.db.AddWarning(m.User.ID, c.GuildID(), note); err != nil {
		c.logErr("add rap sheet note", err)
	}

	reason := s.Reason
	if reason == "" {
		reason = noReason
	}
	embed := makeEmbed("🔓 Sentence Commuted",
		fmt.Sprintf("**%s** was released by %s after serving **%s**.\n**Sentence:** `#%d`\n**Reason:** %s\n*Logged to their rap sheet.*",
			m.User.String(), userMention(c.UserID()), served, s.ID, reason),
		colourGreen)
	c.Reply(false, embed)
	b.modLog(embed)
}

// jailConfig returns the configured jail role and channel, or replies with
// an error and returns nils.
func (b *Bot) jailConfig(c *Ctx) (*discordgo.Role, *discordgo.Channel) {
	role := b.role(c.GuildID(), b.cfg.ID("JAIL_ROLE"))
	if role == nil {
		c.Reply(true, errEmbed("No jail role configured. Run `/setup` first."))
		return nil, nil
	}
	ch := b.channel(b.cfg.ID("JAIL_CHANNEL"))
	if ch == nil || ch.GuildID != c.GuildID() {
		c.Reply(true, errEmbed("No jail channel configured. Run `/setup` first."))
		return nil, nil
	}
	return role, ch
}

func (b *Bot) cmdJailSetup(c *Ctx) {
	role, jailCh := b.jailConfig(c)
	if role == nil {
		return
	}
	c.Reply(true, makeEmbed("🚔 Applying…", "Setting jail role permissions on every channel.", colourBlurple))
	done, failed, updated := b.applyJailOverwrites(c.GuildID())
	if updated != nil {
		jailCh = updated
	}

	// Check that the role can really speak in the jail channel: a category
	// overwrite, a missed channel, or a permission the bot lacks can all
	// leave this broken without an error.
	g, _ := b.s.State.Guild(c.GuildID())
	speakNote := ""
	if g != nil {
		perms := channelPerms(g, jailCh, "", []string{role.ID})
		canSpeak := perms&discordgo.PermissionSendMessages != 0
		if isVoice(jailCh) {
			canSpeak = perms&discordgo.PermissionVoiceConnect != 0 && perms&discordgo.PermissionVoiceSpeak != 0
		}
		if !canSpeak {
			speakNote = fmt.Sprintf("\n🚫 %s still doesn't appear able to speak in %s — "+
				"check for a conflicting category or channel overwrite.", roleMention(role.ID), channelMention(jailCh.ID))
		}
	}

	msg := fmt.Sprintf("Jail restrictions applied to **%d** channel(s).", done)
	if failed > 0 {
		msg += fmt.Sprintf(" **%d** failed (missing permissions).", failed)
	}
	msg += speakNote + fmt.Sprintf("\n\n⚠️ Double-check %s's permissions yourself, especially on any "+
		"categories or channels with their own overwrites — those can still override this.", roleMention(role.ID))
	c.Reply(true, okEmbed(msg))
}

// jailAuditTargets lists the channels to check for jail role visibility.
// It skips categories, the jail channel, channels in a category with
// "archive" in its name, and live ticket channels (looked up in the
// tickets table, not guessed from the name).
func (b *Bot) jailAuditTargets(guildID, jailChannelID string) (*discordgo.Guild, []*discordgo.Channel) {
	g, chans := b.guildChannels(guildID)
	categories := categoryNames(chans)
	var targets []*discordgo.Channel
	for _, ch := range chans {
		if ch.Type == discordgo.ChannelTypeGuildCategory || ch.ID == jailChannelID {
			continue
		}
		if name, ok := categories[ch.ParentID]; ok && strings.Contains(strings.ToLower(name), "archive") {
			continue
		}
		if ticket, err := b.db.IsTicketChannel(ch.ID); err != nil {
			log.Printf("jail audit: ticket lookup: %v", err)
		} else if ticket {
			continue
		}
		targets = append(targets, ch)
	}
	return g, targets
}

func categoryNames(chans []*discordgo.Channel) map[string]string {
	names := map[string]string{}
	for _, ch := range chans {
		if ch.Type == discordgo.ChannelTypeGuildCategory {
			names[ch.ID] = ch.Name
		}
	}
	return names
}

// jailVisible lists the targets the jail role can see on its own.
func jailVisible(g *discordgo.Guild, targets []*discordgo.Channel, roleID string) (visible, hidden []*discordgo.Channel) {
	for _, ch := range targets {
		if channelPerms(g, ch, "", []string{roleID})&discordgo.PermissionViewChannel != 0 {
			visible = append(visible, ch)
		} else {
			hidden = append(hidden, ch)
		}
	}
	return visible, hidden
}

func (b *Bot) cmdJailAudit(c *Ctx) {
	role, jailCh := b.jailConfig(c)
	if role == nil {
		return
	}
	c.Reply(true, makeEmbed("🔍 Auditing…", "Checking every channel for jail-role visibility.", colourBlurple))

	g, targets := b.jailAuditTargets(c.GuildID(), jailCh.ID)
	if g == nil {
		c.Reply(true, errEmbed("I couldn't read this server's channels."))
		return
	}
	needsFix, hidden := jailVisible(g, targets, role.ID)

	// Arresting someone only adds the jail role and strips some others; it
	// can keep protected roles. Discord applies the combined role allows
	// over the combined role denies, so an explicit Allow: View Channel on
	// another role a jailed member holds wins over the jail role's Deny.
	// Check the real jailed members to catch this.
	var jailed []*discordgo.Member
	rows, err := b.db.ActiveSentences(c.GuildID())
	if err != nil {
		c.logErr("list sentences", err)
	}
	for _, s := range rows {
		if m := b.resolveMember(c.GuildID(), s.UserID); m != nil {
			jailed = append(jailed, m)
		}
	}
	var conflicts []string
	for _, ch := range hidden {
		for _, m := range jailed {
			if channelPerms(g, ch, m.User.ID, m.Roles)&discordgo.PermissionViewChannel == 0 {
				continue
			}
			var culprits []string
			for _, r := range b.memberRoles(c.GuildID(), m) {
				if r.ID == role.ID || r.ID == c.GuildID() {
					continue
				}
				if ow := roleOverwrite(ch, r.ID); ow.Allow&discordgo.PermissionViewChannel != 0 {
					culprits = append(culprits, r.Name)
				}
			}
			via := "*an overwrite I couldn't pin down*"
			if len(culprits) > 0 {
				via = "**" + strings.Join(culprits, ", ") + "**"
			}
			conflicts = append(conflicts, fmt.Sprintf("• %s — still visible to %s via %s",
				channelMention(ch.ID), userMention(m.User.ID), via))
			break
		}
	}

	visibilityNote := ""
	if channelPerms(g, jailCh, "", []string{role.ID})&discordgo.PermissionViewChannel == 0 {
		visibilityNote = fmt.Sprintf("\n\n⚠️ On top of that, %s doesn't currently appear able to see "+
			"%s itself — double-check its permissions too.", roleMention(role.ID), channelMention(jailCh.ID))
	}

	if len(conflicts) > 0 {
		tail := "\n\nDenying **View Channel** on the jail role won't fix these — the role(s) named above already " +
			"grant an explicit allow there, and Discord applies allow-over-deny when combining a member's roles. " +
			"Either remove that role's allow overwrite on the channel, or make sure jailed members don't keep " +
			"that role."
		c.Reply(true, makeEmbed(
			fmt.Sprintf("⚠️ %d channel(s) still visible to a jailed member despite the jail role's own deny", len(conflicts)),
			trimLines(conflicts, 4096-len(tail))+tail,
			colourOrange))
	}

	if len(needsFix) == 0 {
		if len(conflicts) == 0 {
			c.Reply(true, okEmbed(fmt.Sprintf("%s can't see anything outside %s. Nothing to fix.",
				roleMention(role.ID), channelMention(jailCh.ID))+visibilityNote))
		}
		return
	}

	_, chans := b.guildChannels(c.GuildID())
	categories := categoryNames(chans)
	lines := make([]string, len(needsFix))
	for i, ch := range needsFix {
		lines[i] = "• " + channelMention(ch.ID)
		if name, ok := categories[ch.ParentID]; ok {
			lines[i] += fmt.Sprintf(" *(category: %s)*", name)
		}
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed(
		fmt.Sprintf("🚨 %d channel(s) still visible to @%s", len(needsFix), role.Name),
		trimLines(lines, 3500)+"\n\nConfirm to deny **View Channel** for the jail role on all of these."+visibilityNote,
		colourOrange,
	)}, confirmButtons("jailaudit", c.UserID()))
}

// onJailAuditConfirm denies View Channel for the jail role on every audit
// target it can still see. The channels are checked again at click time,
// because the prompt keeps no list.
func (b *Bot) onJailAuditConfirm(c *Ctx, _ []string) {
	role := b.role(c.GuildID(), b.cfg.ID("JAIL_ROLE"))
	jailCh := b.channel(b.cfg.ID("JAIL_CHANNEL"))
	if role == nil || jailCh == nil {
		c.Update([]*discordgo.MessageEmbed{errEmbed("No jail role or jail channel configured. Run `/setup` first.")},
			[]discordgo.MessageComponent{})
		return
	}
	c.DeferUpdate()
	g, targets := b.jailAuditTargets(c.GuildID(), jailCh.ID)
	var needsFix []*discordgo.Channel
	if g != nil {
		needsFix, _ = jailVisible(g, targets, role.ID)
	}

	fixed, failed := b.denyJailView(needsFix, role.ID, "Jail visibility audit by "+c.Member().User.String())
	msg := fmt.Sprintf("Denied View Channel for %s on **%d** channel(s).", roleMention(role.ID), fixed)
	if failed > 0 {
		msg += fmt.Sprintf(" **%d** failed (missing permissions).", failed)
	}
	embed := okEmbed(msg)
	c.ReplyComplex(true, []*discordgo.MessageEmbed{embed}, []discordgo.MessageComponent{})
	b.modLog(embed)
}

// denyJailView denies View Channel for the jail role on each channel,
// merged into the role's current overwrite.
func (b *Bot) denyJailView(chans []*discordgo.Channel, roleID, reason string) (fixed, failed int) {
	for _, ch := range chans {
		ow := roleOverwrite(ch, roleID)
		setPerm(&ow, discordgo.PermissionViewChannel, false)
		if err := b.s.ChannelPermissionSet(ch.ID, roleID, discordgo.PermissionOverwriteTypeRole,
			ow.Allow, ow.Deny, auditReason(reason)); err != nil {
			log.Printf("jail audit: #%s: %v", ch.Name, err)
			failed++
			continue
		}
		fixed++
	}
	return fixed, failed
}

// trimLines joins lines with newlines, dropping lines from the end (with a
// "…and N more." note) to stay within limit characters.
func trimLines(lines []string, limit int) string {
	joined := strings.Join(lines, "\n")
	if len([]rune(joined)) <= limit {
		return joined
	}
	var kept []string
	total := 0
	for _, l := range lines {
		n := len([]rune(l)) + 1
		if total+n > limit-30 {
			break
		}
		kept = append(kept, l)
		total += n
	}
	return strings.Join(kept, "\n") + fmt.Sprintf("\n…and %d more.", len(lines)-len(kept))
}
