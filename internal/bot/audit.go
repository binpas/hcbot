package bot

import (
	"bytes"
	stdcmp "cmp"
	"container/list"
	"fmt"
	"io"
	"log"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Audit log: message, channel, thread, role and voice events go to
// BIG_BROTHER; bans, unbans and invites go to MOD_LOG.

const (
	colourBlue     = 0x3498DB
	colourDarkBlue = 0x206694
	colourPurple   = 0x9B59B6

	// messageCacheSize matches discord.py's default message cache.
	messageCacheSize = 1000
)

// ---------------------------------------------------------------------
// Caches
// ---------------------------------------------------------------------

// cachedMessage is what the audit log needs to know about a message after
// it is edited or deleted. Content and Attachments are kept only with
// ENABLE_MESSAGE_CONTENT_FEATURES.
type cachedMessage struct {
	ID, ChannelID    string
	AuthorID, Author string
	Bot              bool
	Content          string
	EditedAt         time.Time
	Created          time.Time
	Attachments      []string
}

// lru is a small least-recently-added cache, safe for concurrent use.
type lru[V any] struct {
	mu    sync.Mutex
	order *list.List // front = newest
	items map[string]*list.Element
}

type lruEntry[V any] struct {
	key   string
	value V
}

func newLRU[V any]() *lru[V] {
	return &lru[V]{order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru[V]) put(key string, v V, max int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.Value = lruEntry[V]{key, v}
		c.order.MoveToFront(e)
	} else {
		c.items[key] = c.order.PushFront(lruEntry[V]{key, v})
	}
	for c.order.Len() > max {
		old := c.order.Back()
		c.order.Remove(old)
		delete(c.items, old.Value.(lruEntry[V]).key)
	}
}

func (c *lru[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		return e.Value.(lruEntry[V]).value, true
	}
	var zero V
	return zero, false
}

func (c *lru[V]) pop(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.Remove(e)
		delete(c.items, key)
		return e.Value.(lruEntry[V]).value, true
	}
	var zero V
	return zero, false
}

// cachedFile is one attachment kept so a deleted message's media can be
// posted again (Discord's CDN links stop working after a delete).
type cachedFile struct {
	Name string
	Data []byte
}

// mediaCache keeps the attachments of the newest messages, within a count
// limit (MEDIA_CACHE_SIZE) and a total size limit (MEDIA_CACHE_TOTAL_MB).
type mediaCache struct {
	mu    sync.Mutex
	order *list.List // front = newest; values are *mediaEntry
	items map[string]*list.Element
	total int64
}

type mediaEntry struct {
	id    string
	files []cachedFile
	size  int64
}

func newMediaCache() *mediaCache {
	return &mediaCache{order: list.New(), items: map[string]*list.Element{}}
}

func (c *mediaCache) put(id string, files []cachedFile, maxCount int, maxTotal int64) {
	var size int64
	for _, f := range files {
		size += int64(len(f.Data))
	}
	if size > maxTotal || maxCount <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[id] = c.order.PushFront(&mediaEntry{id, files, size})
	c.total += size
	for c.order.Len() > maxCount || c.total > maxTotal {
		old := c.order.Back().Value.(*mediaEntry)
		c.order.Remove(c.order.Back())
		delete(c.items, old.id)
		c.total -= old.size
	}
}

func (c *mediaCache) pop(id string) []cachedFile {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[id]
	if !ok {
		return nil
	}
	entry := e.Value.(*mediaEntry)
	c.order.Remove(e)
	delete(c.items, id)
	c.total -= entry.size
	return entry.files
}

// snapshots keep the last known roles and thread names. discordgo updates
// its state before handlers run and keeps no "before" for these events.
type snapshots struct {
	mu      sync.Mutex
	roles   map[string]discordgo.Role
	threads map[string]discordgo.Channel
}

func (s *snapshots) setRole(r *discordgo.Role) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roles[r.ID] = *r
}

func (s *snapshots) takeRole(id string, replace *discordgo.Role) (discordgo.Role, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.roles[id]
	if replace != nil {
		s.roles[id] = *replace
	} else {
		delete(s.roles, id)
	}
	return old, ok
}

func (s *snapshots) setThread(t *discordgo.Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads[t.ID] = *t
}

func (s *snapshots) takeThread(id string, replace *discordgo.Channel) (discordgo.Channel, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.threads[id]
	if replace != nil {
		s.threads[id] = *replace
	} else {
		delete(s.threads, id)
	}
	return old, ok
}

func (b *Bot) onGuildCreate(_ *discordgo.Session, g *discordgo.GuildCreate) {
	for _, r := range g.Roles {
		b.snap.setRole(r)
	}
	for _, t := range g.Threads {
		b.snap.setThread(t)
	}
}

// ---------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------

// cacheMessage remembers a new guild message for the edit and delete logs.
// Bot messages are kept too (marked), so their deletes aren't reported
// as "old message deleted".
func (b *Bot) cacheMessage(m *discordgo.Message) {
	if m.GuildID == "" || m.Author == nil {
		return
	}
	c := cachedMessage{
		ID: m.ID, ChannelID: m.ChannelID, AuthorID: m.Author.ID, Author: m.Author.String(),
		Bot: m.Author.Bot, Created: m.Timestamp,
	}
	if m.EditedTimestamp != nil {
		c.EditedAt = *m.EditedTimestamp
	}
	if b.env.MessageContent && !c.Bot {
		c.Content = m.Content
		for _, a := range m.Attachments {
			c.Attachments = append(c.Attachments, a.Filename)
		}
	}
	b.msgs.put(m.ID, c, messageCacheSize)
	if b.env.MessageContent && !c.Bot && len(m.Attachments) > 0 {
		go b.cacheMedia(m)
	}
}

// cacheMedia downloads a message's attachments (each up to
// MEDIA_CACHE_MAX_BYTES) into the media cache.
func (b *Bot) cacheMedia(m *discordgo.Message) {
	maxBytes := int64(b.env.MediaCacheMaxBytes)
	var files []cachedFile
	for _, a := range m.Attachments {
		if int64(a.Size) > maxBytes {
			continue
		}
		resp, err := b.s.Client.Get(a.URL)
		if err != nil {
			log.Printf("media cache: %s: %v", a.Filename, err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || int64(len(data)) > maxBytes {
			continue
		}
		files = append(files, cachedFile{a.Filename, data})
	}
	if len(files) > 0 {
		b.media.put(m.ID, files, b.env.MediaCacheSize, int64(max(b.cfg.Int("MEDIA_CACHE_TOTAL_MB"), 1))<<20)
	}
}

func jumpToMessage(guildID, channelID, messageID string) string {
	return fmt.Sprintf("[Jump to message](https://discord.com/channels/%s/%s/%s)", guildID, channelID, messageID)
}

func orEmpty(s, empty string) string {
	if s == "" {
		return empty
	}
	return truncate(s, 1024)
}

func (b *Bot) onMessageUpdate(_ *discordgo.Session, m *discordgo.MessageUpdate) {
	defer recoverHandler("message update handler")
	if b.env.SetupMode || m.GuildID == "" {
		return
	}
	before, ok := b.msgs.get(m.ID)
	if !ok || before.Bot {
		return
	}
	// edited_timestamp (not a text comparison) tells a real edit from a
	// link-preview update; it works with or without the message text.
	if m.EditedTimestamp == nil || m.EditedTimestamp.Equal(before.EditedAt) {
		return
	}
	after := before
	after.EditedAt = *m.EditedTimestamp
	if b.env.MessageContent {
		after.Content = m.Content
	}
	b.msgs.put(m.ID, after, messageCacheSize)

	e := makeEmbed("✏️ Message Edited",
		fmt.Sprintf("**Author:** %s (`%s`) in %s\n%s", userMention(before.AuthorID), before.AuthorID,
			channelMention(m.ChannelID), jumpToMessage(m.GuildID, m.ChannelID, m.ID)),
		colourBlue)
	if b.env.MessageContent {
		e.Fields = []*discordgo.MessageEmbedField{
			{Name: "Before", Value: orEmpty(before.Content, "*empty*")},
			{Name: "After", Value: orEmpty(after.Content, "*empty*")},
		}
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Message ID: " + m.ID}
	b.bbLog(e)
}

func (b *Bot) onMessageDelete(_ *discordgo.Session, m *discordgo.MessageDelete) {
	defer recoverHandler("message delete handler")
	if b.env.SetupMode || m.GuildID == "" {
		return
	}
	cached, ok := b.msgs.pop(m.ID)
	files := b.media.pop(m.ID)
	if ok && cached.Bot {
		return
	}
	footer := &discordgo.MessageEmbedFooter{Text: "Message ID: " + m.ID}
	if !ok {
		// Not in the cache: Discord says only which message and where.
		sent := "an unknown time"
		if t, err := discordgo.SnowflakeTimestamp(m.ID); err == nil {
			sent = fmt.Sprintf("%s (%s)", discordTimestamp(t.Unix(), "f"), discordTimestamp(t.Unix(), "R"))
		}
		e := makeEmbed("🗑️ Old Message Deleted",
			fmt.Sprintf("A message from %s was deleted in %s.\nAuthor unknown — the message is older than the bot's message cache.",
				sent, channelMention(m.ChannelID)),
			colourDarkBlue)
		e.Footer = footer
		b.bbLog(e)
		return
	}

	e := makeEmbed("🗑️ Message Deleted",
		fmt.Sprintf("**Author:** %s (`%s`) in %s", userMention(cached.AuthorID), cached.AuthorID, channelMention(m.ChannelID)),
		colourDarkBlue)
	e.Footer = footer
	var uploads []*discordgo.File
	if b.env.MessageContent {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Content", Value: orEmpty(cached.Content, "*empty / attachment only*")})
		for _, f := range files {
			uploads = append(uploads, &discordgo.File{Name: f.Name, Reader: bytes.NewReader(f.Data)})
		}
		if len(cached.Attachments) > 0 && len(files) == 0 {
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
				Name: "Attachments (not buffered)", Value: truncate(strings.Join(cached.Attachments, "\n"), 1024)})
		}
		if len(uploads) > 0 {
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
				Name: "Attachments", Value: fmt.Sprintf("%d file(s) recovered", len(uploads))})
		}
	}
	b.bbLog(e, uploads...)
}

func (b *Bot) onMessageDeleteBulk(_ *discordgo.Session, m *discordgo.MessageDeleteBulk) {
	defer recoverHandler("bulk delete handler")
	if b.env.SetupMode || m.GuildID == "" || len(m.Messages) == 0 {
		return
	}
	var cached []cachedMessage
	for _, id := range m.Messages {
		if c, ok := b.msgs.pop(id); ok {
			cached = append(cached, c)
		}
		b.media.pop(id)
	}
	e := makeEmbed("🗑️ Bulk Delete",
		fmt.Sprintf("**%d** messages deleted in %s.", len(m.Messages), channelMention(m.ChannelID)), colourDarkBlue)
	if !b.env.MessageContent || len(cached) == 0 {
		b.bbLog(e)
		return
	}
	sort.Slice(cached, func(i, j int) bool { return cached[i].Created.Before(cached[j].Created) })
	lines := make([]string, len(cached))
	for i, c := range cached {
		lines[i] = fmt.Sprintf("[%s] %s: %s", c.Created.UTC().Format("2006-01-02 15:04"), c.Author, cmpOrText(c.Content))
	}
	if uncached := len(m.Messages) - len(cached); uncached > 0 {
		lines = append(lines, fmt.Sprintf("(%d older message(s) were not in the bot's cache)", uncached))
	}
	name := m.ChannelID
	if ch := b.channel(m.ChannelID); ch != nil {
		name = ch.Name
	}
	b.bbLog(e, &discordgo.File{Name: "bulk-delete-" + name + ".txt", ContentType: "text/plain",
		Reader: strings.NewReader(strings.Join(lines, "\n"))})
}

func cmpOrText(s string) string {
	if s == "" {
		return "[no text]"
	}
	return s
}

// ---------------------------------------------------------------------
// Channels and threads
// ---------------------------------------------------------------------

var channelTypeNames = map[discordgo.ChannelType]string{
	discordgo.ChannelTypeGuildText: "text", discordgo.ChannelTypeGuildVoice: "voice",
	discordgo.ChannelTypeGuildCategory: "category", discordgo.ChannelTypeGuildNews: "news",
	discordgo.ChannelTypeGuildNewsThread: "news_thread", discordgo.ChannelTypeGuildPublicThread: "public_thread",
	discordgo.ChannelTypeGuildPrivateThread: "private_thread", discordgo.ChannelTypeGuildStageVoice: "stage_voice",
	discordgo.ChannelTypeGuildForum: "forum", discordgo.ChannelTypeGuildMedia: "media",
}

func channelTypeName(t discordgo.ChannelType) string {
	if n, ok := channelTypeNames[t]; ok {
		return n
	}
	return fmt.Sprint(int(t))
}

func (b *Bot) categoryName(parentID string) string {
	if ch := b.channel(parentID); ch != nil {
		return ch.Name
	}
	return "None"
}

// describeOverwrites lists a channel's overwrites as "target: +allow, -deny".
func (b *Bot) describeOverwrites(ch *discordgo.Channel) string {
	var out []string
	for _, ow := range ch.PermissionOverwrites {
		var flags []string
		for _, n := range permNames(ow.Allow) {
			flags = append(flags, "+"+n)
		}
		for _, n := range permNames(ow.Deny) {
			flags = append(flags, "-"+n)
		}
		if len(flags) == 0 {
			continue
		}
		target := userMention(ow.ID)
		if ow.Type == discordgo.PermissionOverwriteTypeRole {
			target = "`" + ow.ID + "`"
			if r := b.role(ch.GuildID, ow.ID); r != nil {
				target = r.Name
			}
		}
		out = append(out, target+": "+strings.Join(flags[:min(len(flags), 6)], ", "))
		if len(out) == 8 {
			break
		}
	}
	if len(out) == 0 {
		return "*none*"
	}
	return strings.Join(out, "\n")
}

func sameOverwrites(a, b []*discordgo.PermissionOverwrite) bool {
	key := func(ows []*discordgo.PermissionOverwrite) []string {
		var out []string
		for _, o := range ows {
			out = append(out, fmt.Sprintf("%d:%s:%d:%d", o.Type, o.ID, o.Allow, o.Deny))
		}
		sort.Strings(out)
		return out
	}
	return slices.Equal(key(a), key(b))
}

func (b *Bot) onChannelCreate(_ *discordgo.Session, c *discordgo.ChannelCreate) {
	defer recoverHandler("channel create handler")
	if b.env.SetupMode || c.GuildID == "" {
		return
	}
	e := makeEmbed("📁 Channel Created",
		fmt.Sprintf("**%s** (`%s`)\n**Type:** %s • **Category:** %s",
			channelMention(c.ID), c.Name, channelTypeName(c.Type), b.categoryName(c.ParentID)),
		colourGreen)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Channel ID: " + c.ID}
	b.bbLog(e)
}

func (b *Bot) onChannelDelete(_ *discordgo.Session, c *discordgo.ChannelDelete) {
	defer recoverHandler("channel delete handler")
	if b.env.SetupMode || c.GuildID == "" {
		return
	}
	e := makeEmbed("📁 Channel Deleted",
		fmt.Sprintf("**#%s**\n**Type:** %s • **Category:** %s", c.Name, channelTypeName(c.Type), b.categoryName(c.ParentID)),
		colourRed)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Channel ID: " + c.ID}
	b.bbLog(e)
}

func (b *Bot) onChannelUpdate(_ *discordgo.Session, c *discordgo.ChannelUpdate) {
	defer recoverHandler("channel update handler")
	before, after := c.BeforeUpdate, c.Channel
	if b.env.SetupMode || c.GuildID == "" || before == nil {
		return
	}
	var changes []string
	if before.Name != after.Name {
		changes = append(changes, fmt.Sprintf("**Name:** `%s` → `%s`", before.Name, after.Name))
	}
	if before.Topic != after.Topic {
		changes = append(changes, fmt.Sprintf("**Topic:** %s → %s",
			truncate(stdcmp.Or(before.Topic, "*none*"), 200), truncate(stdcmp.Or(after.Topic, "*none*"), 200)))
	}
	if before.ParentID != after.ParentID {
		changes = append(changes, fmt.Sprintf("**Category (moved):** `%s` → `%s`", b.categoryName(before.ParentID), b.categoryName(after.ParentID)))
	}
	if before.Position != after.Position {
		changes = append(changes, fmt.Sprintf("**Position (moved):** `%d` → `%d`", before.Position, after.Position))
	}
	if before.NSFW != after.NSFW {
		changes = append(changes, fmt.Sprintf("**NSFW:** `%s` → `%s`", pyBool(before.NSFW), pyBool(after.NSFW)))
	}
	if before.RateLimitPerUser != after.RateLimitPerUser {
		changes = append(changes, fmt.Sprintf("**Slowmode:** `%ds` → `%ds`", before.RateLimitPerUser, after.RateLimitPerUser))
	}
	if !sameOverwrites(before.PermissionOverwrites, after.PermissionOverwrites) {
		changes = append(changes, "**Permissions changed:**\n"+b.describeOverwrites(after))
	}
	if len(changes) == 0 {
		return
	}
	e := makeEmbed("📝 Channel Updated", channelMention(after.ID)+"\n\n"+truncate(strings.Join(changes, "\n"), 3800), colourBlue)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Channel ID: " + after.ID}
	b.bbLog(e)
}

// pyBool writes a bool the way the Python bot's log did.
func pyBool(v bool) string {
	if v {
		return "True"
	}
	return "False"
}

func (b *Bot) onThreadCreate(_ *discordgo.Session, t *discordgo.ThreadCreate) {
	defer recoverHandler("thread create handler")
	b.snap.setThread(t.Channel)
	if b.env.SetupMode || t.GuildID == "" || !t.NewlyCreated {
		return
	}
	parent, creator := "unknown", "unknown"
	if t.ParentID != "" {
		parent = channelMention(t.ParentID)
	}
	if t.OwnerID != "" {
		creator = userMention(t.OwnerID)
	}
	e := makeEmbed("🧵 Thread Created",
		fmt.Sprintf("**%s** (`%s`)\n**Parent:** %s\n**Creator:** %s", channelMention(t.ID), t.Name, parent, creator),
		colourGreen)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Thread ID: " + t.ID}
	b.bbLog(e)
}

func (b *Bot) onThreadDelete(_ *discordgo.Session, t *discordgo.ThreadDelete) {
	defer recoverHandler("thread delete handler")
	old, known := b.snap.takeThread(t.ID, nil)
	if b.env.SetupMode || t.GuildID == "" {
		return
	}
	name := "`" + t.ID + "`"
	if known {
		name = old.Name
	}
	parent := "unknown"
	if t.ParentID != "" {
		parent = channelMention(t.ParentID)
	}
	e := makeEmbed("🧵 Thread Deleted", fmt.Sprintf("**%s**\n**Parent:** %s", name, parent), colourRed)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Thread ID: " + t.ID}
	b.bbLog(e)
}

func (b *Bot) onThreadUpdate(_ *discordgo.Session, t *discordgo.ThreadUpdate) {
	defer recoverHandler("thread update handler")
	old, known := b.snap.takeThread(t.ID, t.Channel)
	before := t.BeforeUpdate
	if before == nil && known {
		before = &old
	}
	if b.env.SetupMode || t.GuildID == "" || before == nil {
		return
	}
	meta := func(c *discordgo.Channel) discordgo.ThreadMetadata {
		if c.ThreadMetadata != nil {
			return *c.ThreadMetadata
		}
		return discordgo.ThreadMetadata{}
	}
	bm, am := meta(before), meta(t.Channel)
	var changes []string
	if before.Name != t.Name {
		changes = append(changes, fmt.Sprintf("**Name:** `%s` → `%s`", before.Name, t.Name))
	}
	if bm.Archived != am.Archived {
		changes = append(changes, fmt.Sprintf("**Archived:** `%s` → `%s`", pyBool(bm.Archived), pyBool(am.Archived)))
	}
	if bm.Locked != am.Locked {
		changes = append(changes, fmt.Sprintf("**Locked:** `%s` → `%s`", pyBool(bm.Locked), pyBool(am.Locked)))
	}
	if before.RateLimitPerUser != t.RateLimitPerUser {
		changes = append(changes, fmt.Sprintf("**Slowmode:** `%ds` → `%ds`", before.RateLimitPerUser, t.RateLimitPerUser))
	}
	if len(changes) == 0 {
		return
	}
	e := makeEmbed("🧵 Thread Updated", channelMention(t.ID)+"\n\n"+strings.Join(changes, "\n"), colourBlue)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Thread ID: " + t.ID}
	b.bbLog(e)
}

// ---------------------------------------------------------------------
// Roles
// ---------------------------------------------------------------------

func colourHex(c int) string { return fmt.Sprintf("#%06x", c) }

func (b *Bot) onRoleCreate(_ *discordgo.Session, r *discordgo.GuildRoleCreate) {
	defer recoverHandler("role create handler")
	b.snap.setRole(r.Role)
	if b.env.SetupMode {
		return
	}
	e := makeEmbed("🎭 Role Created",
		fmt.Sprintf("**%s** • **Colour:** `%s` • **Position:** `%d`", r.Role.Name, colourHex(r.Role.Color), r.Role.Position),
		colourGreen)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Role ID: " + r.Role.ID}
	b.bbLog(e)
}

func (b *Bot) onRoleDelete(_ *discordgo.Session, r *discordgo.GuildRoleDelete) {
	defer recoverHandler("role delete handler")
	old, known := b.snap.takeRole(r.RoleID, nil)
	if b.env.SetupMode {
		return
	}
	name := "`" + r.RoleID + "`"
	if known {
		name = old.Name
	}
	e := makeEmbed("🎭 Role Deleted", "**"+name+"**", colourRed)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Role ID: " + r.RoleID}
	b.bbLog(e)
}

func (b *Bot) onRoleUpdate(_ *discordgo.Session, r *discordgo.GuildRoleUpdate) {
	defer recoverHandler("role update handler")
	before, known := b.snap.takeRole(r.Role.ID, r.Role)
	after := r.Role
	if b.env.SetupMode || !known {
		return
	}
	var changes []string
	if before.Name != after.Name {
		changes = append(changes, fmt.Sprintf("**Name:** `%s` → `%s`", before.Name, after.Name))
	}
	if before.Color != after.Color {
		changes = append(changes, fmt.Sprintf("**Colour:** `%s` → `%s`", colourHex(before.Color), colourHex(after.Color)))
	}
	if before.Position != after.Position {
		changes = append(changes, fmt.Sprintf("**Position (moved):** `%d` → `%d`", before.Position, after.Position))
	}
	if before.Hoist != after.Hoist {
		changes = append(changes, fmt.Sprintf("**Hoisted:** `%s` → `%s`", pyBool(before.Hoist), pyBool(after.Hoist)))
	}
	if before.Mentionable != after.Mentionable {
		changes = append(changes, fmt.Sprintf("**Mentionable:** `%s` → `%s`", pyBool(before.Mentionable), pyBool(after.Mentionable)))
	}
	if added := permNames(after.Permissions &^ before.Permissions); len(added) > 0 {
		changes = append(changes, "**Permissions granted:** "+strings.Join(added, ", "))
	}
	if removed := permNames(before.Permissions &^ after.Permissions); len(removed) > 0 {
		changes = append(changes, "**Permissions revoked:** "+strings.Join(removed, ", "))
	}
	if len(changes) == 0 {
		return
	}
	e := makeEmbed("🎭 Role Updated", roleMention(after.ID)+"\n\n"+truncate(strings.Join(changes, "\n"), 3800), colourBlue)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Role ID: " + after.ID}
	b.bbLog(e)
}

// ---------------------------------------------------------------------
// Voice
// ---------------------------------------------------------------------

func (b *Bot) channelName(id string) string {
	if ch := b.channel(id); ch != nil {
		return ch.Name
	}
	return id
}

func (b *Bot) onVoiceStateUpdate(_ *discordgo.Session, v *discordgo.VoiceStateUpdate) {
	defer recoverHandler("voice handler")
	if b.env.SetupMode || v.GuildID == "" {
		return
	}
	m := v.Member
	if m == nil || m.User == nil {
		m = b.resolveMember(v.GuildID, v.UserID)
	}
	if m == nil || m.User == nil || m.User.Bot {
		return
	}
	before := v.BeforeUpdate
	if before == nil {
		before = &discordgo.VoiceState{}
	}
	after := v.VoiceState
	who := userMention(v.UserID)

	var e *discordgo.MessageEmbed
	switch {
	case before.ChannelID == "" && after.ChannelID != "":
		e = makeEmbed("🔊 Joined Voice", fmt.Sprintf("%s joined **%s**.", who, b.channelName(after.ChannelID)), colourGreen)
	case before.ChannelID != "" && after.ChannelID == "":
		e = makeEmbed("🔇 Left Voice", fmt.Sprintf("%s left **%s**.", who, b.channelName(before.ChannelID)), colourDarkGrey)
	case before.ChannelID != after.ChannelID:
		e = makeEmbed("🔀 Moved Voice", fmt.Sprintf("%s: **%s** → **%s**", who,
			b.channelName(before.ChannelID), b.channelName(after.ChannelID)), colourBlue)
	default:
		var states []string
		flip := func(was, is bool, on, off string) {
			if was != is {
				if is {
					states = append(states, on)
				} else {
					states = append(states, off)
				}
			}
		}
		flip(before.SelfMute, after.SelfMute, "self-muted", "self-unmuted")
		flip(before.SelfDeaf, after.SelfDeaf, "self-deafened", "self-undeafened")
		flip(before.Mute, after.Mute, "server-muted", "server-unmuted")
		flip(before.Deaf, after.Deaf, "server-deafened", "server-undeafened")
		flip(before.SelfStream, after.SelfStream, "started streaming", "stopped streaming")
		flip(before.SelfVideo, after.SelfVideo, "turned camera on", "turned camera off")
		if len(states) == 0 {
			return
		}
		name := "voice"
		if after.ChannelID != "" {
			name = b.channelName(after.ChannelID)
		}
		e = makeEmbed("🎙️ Voice State Changed", fmt.Sprintf("%s %s in **%s**.", who, strings.Join(states, ", "), name), colourPurple)
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: "User ID: " + v.UserID}
	b.bbLog(e)
}

// ---------------------------------------------------------------------
// Bans and invites (MOD_LOG)
// ---------------------------------------------------------------------

func (b *Bot) onBanAdd(_ *discordgo.Session, e *discordgo.GuildBanAdd) {
	defer recoverHandler("ban handler")
	if b.env.SetupMode || e.User == nil {
		return
	}
	b.modLog(makeEmbed("🔨 Member Banned", fmt.Sprintf("**%s** (`%s`) was banned.", e.User, e.User.ID), colourRed))
}

func (b *Bot) onBanRemove(_ *discordgo.Session, e *discordgo.GuildBanRemove) {
	defer recoverHandler("unban handler")
	if b.env.SetupMode || e.User == nil {
		return
	}
	b.modLog(makeEmbed("✅ Member Unbanned", fmt.Sprintf("**%s** (`%s`) was unbanned.", e.User, e.User.ID), colourGreen))
}

const inviteLogTitle = "📨 Invite Created"

var inviteCodeRE = regexp.MustCompile("\\*\\*Code:\\*\\* `([^`]+)`")

// onInviteCreate logs a new invite in MOD_LOG with a ❌ to delete it. The
// code is read back from the embed on ❌, so this keeps no state and still
// works after a restart.
func (b *Bot) onInviteCreate(_ *discordgo.Session, inv *discordgo.InviteCreate) {
	defer recoverHandler("invite handler")
	id := b.cfg.ID("MOD_LOG")
	if b.env.SetupMode || id == "" || inv.GuildID == "" {
		return
	}
	creator, creatorID, defaultAvatar := "Unknown", "?", "Yes ⚠️"
	if u := inv.Inviter; u != nil {
		creator, creatorID = userMention(u.ID), u.ID
		if u.Avatar != "" {
			defaultAvatar = "No"
		}
	}
	maxUses := "Unlimited"
	if inv.MaxUses > 0 {
		maxUses = fmt.Sprint(inv.MaxUses)
	}
	msg, err := b.s.ChannelMessageSendEmbed(id, makeEmbed(inviteLogTitle, fmt.Sprintf(
		"**Creator:** %s (`%s`)\n**Default avatar:** %s\n**Code:** `%s`\n**Max uses:** %s\n\nReact ❌ to delete this invite.",
		creator, creatorID, defaultAvatar, inv.Code, maxUses), colourPurple))
	if err != nil {
		log.Printf("invite log: %v", err)
		return
	}
	if err := b.s.MessageReactionAdd(id, msg.ID, "❌"); err != nil {
		log.Printf("invite log: add ❌: %v", err)
	}
}

// onInviteDeleteReaction deletes the invite of an invite log entry when a
// member reacts ❌ on it in MOD_LOG.
func (b *Bot) onInviteDeleteReaction(r *discordgo.MessageReactionAdd, m *discordgo.Member) {
	if r.ChannelID != b.cfg.ID("MOD_LOG") {
		return
	}
	msg, err := b.s.ChannelMessage(r.ChannelID, r.MessageID)
	if err != nil || msg.Author == nil || msg.Author.ID != b.s.State.User.ID ||
		len(msg.Embeds) == 0 || msg.Embeds[0].Title != inviteLogTitle {
		return
	}
	match := inviteCodeRE.FindStringSubmatch(msg.Embeds[0].Description)
	if match == nil {
		return
	}
	if _, err := b.s.InviteDelete(match[1], auditReason("Deleted via reaction by "+m.User.String())); err != nil {
		log.Printf("invite ❌ %s: %v", match[1], err) // already deleted or expired
		return
	}
	b.modLog(makeEmbed("🗑️ Invite Deleted",
		fmt.Sprintf("Invite `%s` deleted by %s.", match[1], userMention(m.User.ID)), colourRed))
}

func (b *Bot) registerAudit() {
	for _, h := range []any{
		b.onGuildCreate, b.onMessageUpdate, b.onMessageDelete, b.onMessageDeleteBulk,
		b.onChannelCreate, b.onChannelDelete, b.onChannelUpdate,
		b.onThreadCreate, b.onThreadDelete, b.onThreadUpdate,
		b.onRoleCreate, b.onRoleDelete, b.onRoleUpdate,
		b.onVoiceStateUpdate, b.onBanAdd, b.onBanRemove, b.onInviteCreate,
	} {
		b.s.AddHandler(h)
	}
}
