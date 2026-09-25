//go:build live

package bot

// Live tests against a real, separate Discord test server. They run only
// with the "live" build tag and the TEST_* keys in .env:
//
//	go test -tags live -run TestLive -count=1 -v ./internal/bot/
//
// A bot can't use slash commands, click buttons, or see ephemeral replies,
// so these tests call the bot's functions directly and then check the
// result through the API. The second bot (TEST_TARGET_TOKEN) is the member
// that gets arrested and released; its own view of the channels is used to
// check the permission calculation against Discord's.
//
// Safety: the tests refuse to run on GUILD_ID or on a server with more than
// 5 members. They make roles and channels named "test-…" and delete them
// afterwards (and any left over from an earlier failed run). Note that
// /jail setup adds jail role overwrites to every channel; deleting the test
// jail role at the end removes them again.
//
// Never print the tokens or a .env parser error: either can show a token.

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"

	"github.com/binpas/hcbot/internal/config"
	"github.com/binpas/hcbot/internal/db"
)

const liveMaxMembers = 5

type live struct {
	t       *testing.T // the running (sub)test
	root    *testing.T // TestLive itself; used by cleanup
	b       *Bot
	target  *discordgo.Session // the second bot
	guildID string
	botID   string
	userID  string // the target bot's user ID

	roles    map[string]string // short name → role ID
	channels map[string]string // short name → channel ID
}

func newLive(t *testing.T) *live {
	t.Helper()
	env, err := godotenv.Read(filepath.Join("..", "..", ".env"))
	if err != nil {
		t.Skip("cannot read .env") // no error text: it can hold a token
	}
	token, targetToken, guildID := env["BOT_TOKEN"], env["TEST_TARGET_TOKEN"], strings.TrimSpace(env["TEST_GUILD_ID"])
	if token == "" || targetToken == "" || guildID == "" {
		t.Skip("BOT_TOKEN, TEST_TARGET_TOKEN or TEST_GUILD_ID is not set")
	}
	if guildID == strings.TrimSpace(env["GUILD_ID"]) {
		t.Fatal("TEST_GUILD_ID is the same as GUILD_ID; live tests need a separate server")
	}

	database, err := db.Open(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	b, err := New(config.Env{BotToken: token, GuildID: guildID}, database)
	if err != nil {
		t.Fatal(err)
	}
	b.loopsOnce.Do(func() {}) // the tests run the loops themselves
	if err := b.s.Open(); err != nil {
		t.Fatalf("connect main bot: %v", err)
	}
	t.Cleanup(func() { _ = b.s.Close() })

	target, err := discordgo.New("Bot " + targetToken)
	if err != nil {
		t.Fatal(err)
	}
	target.Identify.Intents = discordgo.IntentsNone
	if err := target.Open(); err != nil { // a bot must connect once before it can send
		t.Fatalf("connect target bot: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	lv := &live{t: t, root: t, b: b, target: target, guildID: guildID, botID: b.s.State.User.ID,
		userID: target.State.User.ID, roles: map[string]string{}, channels: map[string]string{}}

	lv.waitFor("the test server in the cache", func() bool {
		_, err := b.s.State.Guild(guildID)
		return err == nil
	})
	g, _ := b.s.State.Guild(guildID)
	if g.MemberCount > liveMaxMembers {
		t.Fatalf("the test server has %d members (max %d); refusing to run", g.MemberCount, liveMaxMembers)
	}
	if b.freshMember(guildID, lv.userID) == nil {
		t.Fatal("the target bot is not in the test server")
	}

	lv.sweep()
	t.Cleanup(lv.sweep)
	lv.fixtures()
	return lv
}

// waitFor polls until cond is true, for at most 15 seconds.
func (lv *live) waitFor(what string, cond func() bool) {
	lv.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			lv.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// sweep deletes every "test-" channel and role, the "Ticket Archive N" and
// "Channel Archive N" categories with everything in them, and the test
// emojis ("test_…") and stickers ("test-…").
func (lv *live) sweep() {
	t := lv.root
	chans, err := lv.b.s.GuildChannels(lv.guildID)
	if err != nil {
		t.Logf("sweep: list channels: %v", err)
	}
	// Channels before categories, so a category is empty when it goes.
	slices.SortFunc(chans, func(a, b *discordgo.Channel) int {
		return boolInt(a.Type == discordgo.ChannelTypeGuildCategory) - boolInt(b.Type == discordgo.ChannelTypeGuildCategory)
	})
	archive := regexp.MustCompile(`^(Ticket|Channel) Archive \d+$`)
	ours := map[string]bool{} // categories made by the tests
	for _, ch := range chans {
		if ch.Type == discordgo.ChannelTypeGuildCategory && (strings.HasPrefix(ch.Name, "test-") || archive.MatchString(ch.Name)) {
			ours[ch.ID] = true
		}
	}
	for _, ch := range chans {
		if strings.HasPrefix(ch.Name, "test-") || ours[ch.ID] || ours[ch.ParentID] {
			if _, err := lv.b.s.ChannelDelete(ch.ID); err != nil {
				t.Logf("sweep: delete #%s: %v", ch.Name, err)
			}
		}
	}
	if emojis, err := lv.b.s.GuildEmojis(lv.guildID); err == nil {
		for _, e := range emojis {
			if strings.HasPrefix(e.Name, "test_") {
				if err := lv.b.s.GuildEmojiDelete(lv.guildID, e.ID); err != nil {
					t.Logf("sweep: delete emoji %s: %v", e.Name, err)
				}
			}
		}
	}
	for _, st := range lv.stickers() {
		if strings.HasPrefix(st.Name, "test-") {
			if err := lv.b.deleteSticker(lv.guildID, st.ID, "live test cleanup"); err != nil {
				t.Logf("sweep: delete sticker %s: %v", st.Name, err)
			}
		}
	}
	roles, err := lv.b.s.GuildRoles(lv.guildID)
	if err != nil {
		t.Logf("sweep: list roles: %v", err)
	}
	for _, r := range roles {
		if strings.HasPrefix(r.Name, "test-") {
			if err := lv.b.s.GuildRoleDelete(lv.guildID, r.ID); err != nil {
				t.Logf("sweep: delete @%s: %v", r.Name, err)
			}
		}
	}
}

// stickers lists the guild's stickers through the API.
func (lv *live) stickers() []*discordgo.Sticker {
	var out []*discordgo.Sticker
	body, err := lv.b.s.RequestWithBucketID("GET", discordgo.EndpointGuild(lv.guildID)+"/stickers", nil,
		discordgo.EndpointGuild(lv.guildID)+"/stickers")
	if err == nil {
		_ = json.Unmarshal(body, &out)
	}
	return out
}

// testPNG makes a size×size PNG of one colour.
func testPNG(size int, c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (lv *live) fixtures() {
	t := lv.t
	none := int64(0)
	for _, name := range []string{"jail", "keep", "strip", "vip", "motd", "rr"} {
		r, err := lv.b.s.GuildRoleCreate(lv.guildID, &discordgo.RoleParams{Name: "test-" + name, Permissions: &none})
		if err != nil {
			t.Fatalf("create role %s: %v", name, err)
		}
		lv.roles[name] = r.ID
	}

	view := int64(discordgo.PermissionViewChannel)
	everyoneView := &discordgo.PermissionOverwrite{ID: lv.guildID, Type: discordgo.PermissionOverwriteTypeRole, Allow: view}
	roleOW := func(role string, allow, deny int64) *discordgo.PermissionOverwrite {
		return &discordgo.PermissionOverwrite{ID: lv.roles[role], Type: discordgo.PermissionOverwriteTypeRole, Allow: allow, Deny: deny}
	}
	create := func(name string, typ discordgo.ChannelType, parent string, ows ...*discordgo.PermissionOverwrite) {
		ch, err := lv.b.s.GuildChannelCreateComplex(lv.guildID, discordgo.GuildChannelCreateData{
			Name: "test-" + name, Type: typ, ParentID: parent, PermissionOverwrites: ows,
		})
		if err != nil {
			t.Fatalf("create channel %s: %v", name, err)
		}
		lv.channels[name] = ch.ID
	}
	create("archive", discordgo.ChannelTypeGuildCategory, "")
	create("tickets", discordgo.ChannelTypeGuildCategory, "", everyoneView)
	create("jail", discordgo.ChannelTypeGuildText, "", everyoneView)
	create("modlog", discordgo.ChannelTypeGuildText, "", everyoneView)
	create("open", discordgo.ChannelTypeGuildText, "", everyoneView)
	create("archived", discordgo.ChannelTypeGuildText, lv.channels["archive"], everyoneView)
	create("vip", discordgo.ChannelTypeGuildText, "", everyoneView, roleOW("vip", view, 0), roleOW("jail", 0, view))
	create("chat", discordgo.ChannelTypeGuildText, "", everyoneView)
	create("curated", discordgo.ChannelTypeGuildText, "", everyoneView)
	create("bb", discordgo.ChannelTypeGuildText, "", everyoneView)
	create("merge", discordgo.ChannelTypeGuildText, "", everyoneView, roleOW("jail", discordgo.PermissionEmbedLinks, view))

	lv.waitFor("the test roles and channels in the cache", func() bool {
		for _, id := range lv.roles {
			if lv.b.role(lv.guildID, id) == nil {
				return false
			}
		}
		for _, id := range lv.channels {
			if _, err := lv.b.s.State.Channel(id); err != nil {
				return false
			}
		}
		return true
	})

	for key, value := range map[string]string{
		"JAIL_ROLE":            lv.roles["jail"],
		"JAIL_CHANNEL":         lv.channels["jail"],
		"MOD_LOG":              lv.channels["modlog"],
		"JAIL_PROTECTED_ROLES": lv.roles["keep"] + "," + lv.roles["vip"],
		"MOTD_ROLE":            lv.roles["motd"],
		"TICKET_CAT":           lv.channels["tickets"],
	} {
		if err := lv.b.setConfig(key, value); err != nil {
			t.Fatal(err)
		}
	}

	// The target starts with its own roles plus keep, strip and vip.
	m := lv.member()
	roles := append(slices.Clone(m.Roles), lv.roles["keep"], lv.roles["strip"], lv.roles["vip"])
	if _, err := lv.b.s.GuildMemberEdit(lv.guildID, lv.userID, &discordgo.GuildMemberParams{Roles: &roles}); err != nil {
		t.Fatalf("give the target its roles: %v", err)
	}
}

// member fetches the target from the API.
func (lv *live) member() *discordgo.Member {
	lv.t.Helper()
	m := lv.b.freshMember(lv.guildID, lv.userID)
	if m == nil {
		lv.t.Fatal("target left the server")
	}
	return m
}

// expectRoles checks which of the named test roles the target has.
func (lv *live) expectRoles(has []string, hasNot []string) {
	lv.t.Helper()
	m := lv.member()
	for _, name := range has {
		if !slices.Contains(m.Roles, lv.roles[name]) {
			lv.t.Errorf("target should have test-%s", name)
		}
	}
	for _, name := range hasNot {
		if slices.Contains(m.Roles, lv.roles[name]) {
			lv.t.Errorf("target should not have test-%s", name)
		}
	}
}

func (lv *live) activeSentence() *db.Sentence {
	lv.t.Helper()
	s, err := lv.b.db.ActiveSentence(lv.guildID, lv.userID)
	if err != nil {
		lv.t.Fatal(err)
	}
	return s
}

// lastEmbedTitle returns the title of the newest embed in a channel.
func (lv *live) lastEmbedTitle(channelID string) string {
	msgs, err := lv.b.s.ChannelMessages(channelID, 1, "", "", "")
	if err != nil || len(msgs) == 0 || len(msgs[0].Embeds) == 0 {
		return ""
	}
	return msgs[0].Embeds[0].Title
}

// waitLog waits up to 15 seconds for a bot message in a channel whose
// first embed has the title and contains every want string.
func (lv *live) waitLog(channelID, title string, want ...string) *discordgo.MessageEmbed {
	lv.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _ := lv.b.s.ChannelMessages(channelID, 20, "", "", "")
		for _, m := range msgs {
			if len(m.Embeds) == 0 || m.Embeds[0].Title != title {
				continue
			}
			text := embedText(m.Embeds[0])
			ok := true
			for _, w := range want {
				ok = ok && strings.Contains(text, w)
			}
			if ok {
				return m.Embeds[0]
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	lv.t.Errorf("no %q entry containing %q", title, want)
	return nil
}

// canView asks Discord whether the target bot can see a channel.
func (lv *live) canView(channelID string) bool {
	_, err := lv.target.Channel(channelID)
	return err == nil
}

// errCode returns the Discord error code of a REST error, or 0.
func errCode(err error) int {
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Message != nil {
		return rest.Message.Code
	}
	return 0
}

func TestLive(t *testing.T) {
	lv := newLive(t)
	g := lv.guildID

	t.Run("ArrestAndRelease", func(t *testing.T) {
		lv.t = t
		reason := "live test — ünïcode 100%"
		ok, problem, id := lv.b.arrestMember(g, lv.member(), lv.botID, 0, reason)
		if !ok {
			t.Fatalf("arrest failed: %s", problem)
		}
		lv.expectRoles([]string{"jail", "keep", "vip"}, []string{"strip"})

		// The audit log shows the reason exactly (it is sent URL-encoded).
		logs, err := lv.b.s.GuildAuditLog(g, lv.botID, "", int(discordgo.AuditLogActionMemberRoleUpdate), 5)
		if err != nil {
			t.Fatalf("read audit log: %v", err)
		}
		found := false
		for _, e := range logs.AuditLogEntries {
			if e.TargetID == lv.userID && e.Reason == reason {
				found = true
			}
		}
		if !found {
			t.Errorf("no role update with reason %q in the audit log", reason)
		}

		msgs, err := lv.b.s.ChannelMessages(lv.channels["jail"], 1, "", "", "")
		if err != nil || len(msgs) == 0 || !strings.Contains(msgs[0].Content, "<@"+lv.userID+">") {
			t.Errorf("no jail channel notice: %v", err)
		}

		s := lv.activeSentence()
		if s == nil || s.ID != id {
			t.Fatalf("sentence = %+v", s)
		}
		lv.b.releaseMember(g, s, "Live test release")
		lv.expectRoles([]string{"keep", "strip", "vip"}, []string{"jail"})
		if lv.activeSentence() != nil {
			t.Error("sentence still active")
		}
	})

	t.Run("ReleaseLoop", func(t *testing.T) {
		lv.t = t
		if ok, problem, _ := lv.b.arrestMember(g, lv.member(), lv.botID, 1, "loop test"); !ok {
			t.Fatalf("arrest failed: %s", problem)
		}
		time.Sleep(2500 * time.Millisecond)
		lv.b.runJailReleases()
		lv.expectRoles([]string{"keep", "strip", "vip"}, []string{"jail"})
		if title := lv.lastEmbedTitle(lv.channels["modlog"]); title != "🔓 Released" {
			t.Errorf("mod log title = %q", title)
		}
	})

	t.Run("JailOverwrites", func(t *testing.T) {
		lv.t = t
		// Jail the target (indefinitely) for this test and the next.
		if ok, problem, _ := lv.b.arrestMember(g, lv.member(), lv.botID, 0, "overwrite test"); !ok {
			t.Fatalf("arrest failed: %s", problem)
		}
		done, failed, _ := lv.b.applyJailOverwrites(g)
		if done == 0 || failed != 0 {
			t.Errorf("applied %d, failed %d", done, failed)
		}

		merge, err := lv.b.s.Channel(lv.channels["merge"])
		if err != nil {
			t.Fatal(err)
		}
		ow := roleOverwrite(merge, lv.roles["jail"])
		if ow.Allow&discordgo.PermissionEmbedLinks == 0 || ow.Deny&discordgo.PermissionViewChannel == 0 {
			t.Errorf("merge lost the old overwrite fields: allow=%d deny=%d", ow.Allow, ow.Deny)
		}
		if ow.Deny&discordgo.PermissionSendMessages == 0 {
			t.Errorf("merge: send not denied: deny=%d", ow.Deny)
		}

		// The jailed target can talk in the jail channel and nowhere else.
		if _, err := lv.target.ChannelMessageSend(lv.channels["jail"], "live test: I am in jail"); err != nil {
			t.Errorf("target can't send in the jail channel: %v", err)
		}
		if _, err := lv.target.ChannelMessageSend(lv.channels["open"], "live test: this should fail"); errCode(err) != discordgo.ErrCodeMissingPermissions {
			t.Errorf("target send in test-open: got %v, want Missing Permissions", err)
		}
	})

	t.Run("AuditMatchesDiscord", func(t *testing.T) {
		lv.t = t
		lv.waitFor("the jail overwrites in the cache", func() bool {
			ch, err := lv.b.s.State.Channel(lv.channels["merge"])
			return err == nil && roleOverwrite(ch, lv.roles["jail"]).Deny&discordgo.PermissionSendMessages != 0
		})
		guild, targets := lv.b.jailAuditTargets(g, lv.channels["jail"])
		ids := map[string]bool{}
		for _, ch := range targets {
			ids[ch.ID] = true
		}
		if !ids[lv.channels["open"]] || ids[lv.channels["archived"]] || ids[lv.channels["jail"]] {
			t.Errorf("audit targets wrong: open=%v archived=%v jail=%v",
				ids[lv.channels["open"]], ids[lv.channels["archived"]], ids[lv.channels["jail"]])
		}

		// channelPerms must agree with what Discord lets the jailed target see.
		m := lv.member()
		for _, name := range []string{"jail", "modlog", "open", "archived", "vip", "merge"} {
			ch, err := lv.b.s.State.Channel(lv.channels[name])
			if err != nil {
				t.Fatal(err)
			}
			predicted := channelPerms(guild, ch, lv.userID, m.Roles)&discordgo.PermissionViewChannel != 0
			if actual := lv.canView(ch.ID); predicted != actual {
				t.Errorf("test-%s: channelPerms says view=%v, Discord says %v", name, predicted, actual)
			}
		}

		visible, _ := jailVisible(guild, targets, lv.roles["jail"])
		var fix []*discordgo.Channel
		for _, ch := range visible {
			if ch.ID == lv.channels["vip"] || ch.ID == lv.channels["merge"] {
				t.Errorf("jail role alone should not see #%s", ch.Name)
			}
			if ch.ID == lv.channels["open"] {
				fix = append(fix, ch)
			}
		}
		if len(fix) != 1 {
			t.Fatal("test-open not found as visible to the jail role")
		}
		if fixed, failed := lv.b.denyJailView(fix, lv.roles["jail"], "Live test audit"); fixed != 1 || failed != 0 {
			t.Fatalf("fixed %d, failed %d", fixed, failed)
		}
		if lv.canView(lv.channels["open"]) {
			t.Error("target still sees test-open after the fix")
		}
		// VIP is a protected role with an explicit allow: a conflict the fix can't solve.
		if !lv.canView(lv.channels["vip"]) {
			t.Error("the VIP conflict should still let the target see test-vip")
		}

		lv.b.releaseMember(g, lv.activeSentence(), "Live test release")
		lv.expectRoles([]string{"keep", "strip", "vip"}, []string{"jail"})
	})

	t.Run("MOTDTimer", func(t *testing.T) {
		lv.t = t
		if err := lv.b.s.GuildMemberRoleAdd(g, lv.userID, lv.roles["motd"]); err != nil {
			t.Fatal(err)
		}
		lv.expectRoles([]string{"motd"}, nil)
		if err := lv.b.db.ScheduleAction(g, lv.userID, "target", db.ActionRemoveRole, lv.roles["motd"], time.Now()); err != nil {
			t.Fatal(err)
		}
		lv.b.runScheduledActions()
		lv.expectRoles(nil, []string{"motd"})
		if title := lv.lastEmbedTitle(lv.channels["modlog"]); title != "🌟 MOTD Expired" {
			t.Errorf("mod log title = %q", title)
		}
	})

	t.Run("Tickets", func(t *testing.T) {
		lv.t = t
		reactions := make(chan *discordgo.MessageReactionAdd, 10)
		remove := lv.b.s.AddHandler(func(_ *discordgo.Session, r *discordgo.MessageReactionAdd) {
			if r.UserID == lv.userID {
				reactions <- r
			}
		})
		defer remove()

		ch := lv.b.openTicket(g, lv.member())
		if ch == nil {
			t.Fatal("openTicket failed")
		}
		if !strings.HasPrefix(ch.Name, "open-ticket-") || ch.ParentID != lv.channels["tickets"] {
			t.Errorf("ticket channel = %q in %s", ch.Name, ch.ParentID)
		}
		if !lv.canView(ch.ID) {
			t.Fatal("the reporter can't see their ticket")
		}
		if _, err := lv.target.ChannelMessageSend(ch.ID, "live test: my problem"); err != nil {
			t.Errorf("the reporter can't write in their ticket: %v", err)
		}
		msgs, err := lv.b.s.ChannelMessages(ch.ID, 5, "", "", "")
		if err != nil || len(msgs) < 2 {
			t.Fatalf("messages: %v", err)
		}
		welcome := msgs[len(msgs)-1]
		if !strings.Contains(welcome.Content, "What's the issue?") || len(welcome.Reactions) == 0 ||
			welcome.Reactions[0].Emoji.Name != ticketCloseEmoji {
			t.Errorf("welcome message wrong: %q, reactions %d", welcome.Content, len(welcome.Reactions))
		}

		// The bot gets reaction events from Discord (Guild Message Reactions intent).
		if err := lv.target.MessageReactionAdd(ch.ID, welcome.ID, "👍"); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-reactions:
			if r.Emoji.Name != "👍" || r.MessageID != welcome.ID {
				t.Errorf("reaction event = %+v", r.MessageReaction)
			}
		case <-time.After(10 * time.Second):
			t.Error("no reaction event within 10 seconds")
		}

		if !lv.b.closeTicket(ch.ID, lv.b.freshMember(g, lv.botID)) {
			t.Fatal("closeTicket returned false")
		}
		if lv.canView(ch.ID) {
			t.Error("the reporter still sees the closed ticket")
		}
		closed, err := lv.b.s.Channel(ch.ID)
		if err != nil || !strings.HasPrefix(closed.Name, "closed-ticket-") {
			t.Errorf("closed name = %v, %v", closed, err)
		}

		tk, err := lv.b.db.TicketByChannel(ch.ID)
		if err != nil || tk == nil || tk.Status != "closed" {
			t.Fatalf("ticket = %+v, %v", tk, err)
		}
		ok, note := lv.b.archiveTicket(g, ch.ID, tk, "live test")
		if !ok {
			t.Fatalf("archive: %s", note)
		}
		moved, err := lv.b.s.Channel(ch.ID)
		if err != nil {
			t.Fatal(err)
		}
		cat, err := lv.b.s.Channel(moved.ParentID)
		if err != nil || !regexp.MustCompile(`^Ticket Archive \d+$`).MatchString(cat.Name) {
			t.Fatalf("archive category = %v, %v", cat, err)
		}
		if len(moved.PermissionOverwrites) != len(cat.PermissionOverwrites) {
			t.Errorf("channel not synced: %d overwrites, category has %d", len(moved.PermissionOverwrites), len(cat.PermissionOverwrites))
		}
		for _, o := range cat.PermissionOverwrites {
			if m := roleOverwrite(moved, o.ID); m.Allow != o.Allow || m.Deny != o.Deny {
				t.Errorf("overwrite %s differs from the category", o.ID)
			}
		}
		if lv.canView(ch.ID) {
			t.Error("the reporter sees the archived ticket")
		}
	})

	t.Run("MentionTriggers", func(t *testing.T) {
		lv.t = t
		chat := lv.channels["chat"]
		events := make(chan *discordgo.Message, 10)
		remove := lv.b.s.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageCreate) {
			if m.ChannelID == chat {
				events <- m.Message
			}
		})
		defer remove()
		next := func(what string) *discordgo.Message {
			select {
			case m := <-events:
				return m
			case <-time.After(10 * time.Second):
				t.Fatalf("no message event for %s", what)
				return nil
			}
		}

		if err := lv.b.db.SetTrigger("livetest", "pong @everyone <@&"+lv.roles["vip"]+">"); err != nil {
			t.Fatal(err)
		}

		// Without the Message Content intent, Discord still sends the text
		// of a message that mentions the bot (design pattern 4)...
		text := "<@" + lv.botID + "> livetest please"
		if _, err := lv.target.ChannelMessageSend(chat, text); err != nil {
			t.Fatal(err)
		}
		mention := next("the mention")
		if mention.Content != text {
			t.Errorf("mention content = %q, want %q", mention.Content, text)
		}
		if lv.b.mentionKeyword(mention) != "livetest" {
			t.Errorf("keyword = %q", lv.b.mentionKeyword(mention))
		}

		// ...but not of other messages.
		if _, err := lv.target.ChannelMessageSend(chat, "no mention here"); err != nil {
			t.Fatal(err)
		}
		// Discord decides this from the app's Message Content switch in the
		// Developer Portal, not from the intents the code requests.
		plain := next("the plain message")
		if app, err := lv.b.s.Application("@me"); err == nil && app.Flags&(1<<19) == 0 && plain.Content != "" {
			t.Errorf("plain content = %q although the Message Content switch is off", plain.Content)
		}

		// The handler ignores bots: the target's mention got no reply.
		select {
		case m := <-events:
			t.Errorf("unexpected message from %s: %q", m.Author.Username, m.Content)
		case <-time.After(2 * time.Second):
		}

		if !lv.b.fireTrigger(mention) {
			t.Fatal("trigger did not fire")
		}
		reply := next("the trigger reply")
		if reply.Author.ID != lv.botID || reply.Content != "pong @everyone <@&"+lv.roles["vip"]+">" {
			t.Errorf("reply = %q by %s", reply.Content, reply.Author.ID)
		}
		if reply.MessageReference == nil || reply.MessageReference.MessageID != mention.ID {
			t.Error("reply is not linked to the mention")
		}
		if reply.MentionEveryone || len(reply.MentionRoles) != 0 {
			t.Errorf("reply pinged: everyone=%v roles=%v", reply.MentionEveryone, reply.MentionRoles)
		}

		// The autoreact emote check: Discord refuses emotes it doesn't know.
		for emote, valid := range map[string]bool{"👍": true, "notanemote": false, "<:fake:123>": false} {
			err := lv.b.s.MessageReactionAdd(chat, reply.ID, reactionEmoji(emote))
			if (err == nil) != valid {
				t.Errorf("reacting with %q: err = %v, want valid=%v", emote, err, valid)
			}
		}
	})

	t.Run("ReactionRoles", func(t *testing.T) {
		lv.t = t
		chat := lv.channels["chat"]
		type event struct {
			add   bool
			emoji discordgo.Emoji
			msg   string
		}
		events := make(chan event, 10)
		removeAdd := lv.b.s.AddHandler(func(_ *discordgo.Session, r *discordgo.MessageReactionAdd) {
			if r.UserID == lv.userID {
				events <- event{true, r.Emoji, r.MessageID}
			}
		})
		defer removeAdd()
		removeRemove := lv.b.s.AddHandler(func(_ *discordgo.Session, r *discordgo.MessageReactionRemove) {
			if r.UserID == lv.userID {
				events <- event{false, r.Emoji, r.MessageID}
			}
		})
		defer removeRemove()
		next := func(what string) event {
			select {
			case e := <-events:
				return e
			case <-time.After(10 * time.Second):
				t.Fatalf("no reaction event for %s", what)
				return event{}
			}
		}

		pairs := []db.RolePair{{Emoji: "👍", RoleID: lv.roles["rr"]}}
		id, err := lv.b.db.SaveReactionRole(0, g, "test-rr", chat, "Pick a role", pairs)
		if err != nil {
			t.Fatal(err)
		}
		rr := &db.ReactionRole{ID: id, GuildID: g, Name: "test-rr", ChannelID: chat, Text: "Pick a role"}
		if ok, note := lv.b.publishReactionRole(g, rr, pairs); !ok {
			t.Fatalf("publish: %s", note)
		}
		rr, _ = lv.b.db.ReactionRoleByName(g, "test-rr")

		// The real gateway event's emoji matches the stored pair.
		if err := lv.target.MessageReactionAdd(chat, rr.MessageID, "👍"); err != nil {
			t.Fatal(err)
		}
		e := next("the 👍 reaction")
		if !e.add || e.msg != rr.MessageID {
			t.Fatalf("event = %+v", e)
		}
		lv.b.applyReactionRole(g, lv.userID, rr, e.emoji, true)
		lv.expectRoles([]string{"rr"}, nil)

		if err := lv.target.MessageReactionRemove(chat, rr.MessageID, "👍", "@me"); err != nil {
			t.Fatal(err)
		}
		if e = next("removing 👍"); e.add {
			t.Fatalf("event = %+v", e)
		}
		lv.b.applyReactionRole(g, lv.userID, rr, e.emoji, false)
		lv.expectRoles(nil, []string{"rr"})

		// Changing the pairs clears the old reaction and adds the new one.
		pairs = []db.RolePair{{Emoji: "🎉", RoleID: lv.roles["rr"]}}
		if ok, note := lv.b.publishReactionRole(g, rr, pairs); !ok {
			t.Fatalf("republish: %s", note)
		}
		msg, err := lv.b.s.ChannelMessage(chat, rr.MessageID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range msg.Reactions {
			got = append(got, r.Emoji.Name)
		}
		if len(got) != 1 || got[0] != "🎉" {
			t.Errorf("reactions after republish = %v, want [🎉]", got)
		}
	})

	t.Run("Curation", func(t *testing.T) {
		lv.t = t
		chat, curated := lv.channels["chat"], lv.channels["curated"]
		for k, v := range map[string]string{"CURATED": curated, "CURATED_THRESHOLD": "1", "CURATED_EMOTE": "⭐"} {
			if err := lv.b.setConfig(k, v); err != nil {
				t.Fatal(err)
			}
		}
		events := make(chan *discordgo.MessageReaction, 10)
		remove := lv.b.s.AddHandler(func(_ *discordgo.Session, r *discordgo.MessageReactionAdd) {
			if r.UserID == lv.userID {
				events <- r.MessageReaction
			}
		})
		defer remove()

		text := "live test: a message worth curating"
		msg, err := lv.target.ChannelMessageSend(chat, text)
		if err != nil {
			t.Fatal(err)
		}
		if err := lv.target.MessageReactionAdd(chat, msg.ID, "⭐"); err != nil {
			t.Fatal(err)
		}
		var r *discordgo.MessageReaction
		select {
		case r = <-events:
		case <-time.After(10 * time.Second):
			t.Fatal("no ⭐ event")
		}
		lv.b.curate(r)
		lv.b.curate(r) // a second event does nothing

		// The live bot runs without ENABLE_MESSAGE_CONTENT_FEATURES (and the
		// test app has the intent switch off), so curation is off: nothing
		// is posted. The check below shows why: Discord refuses the forward.
		posted, err := lv.b.s.ChannelMessages(curated, 10, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(posted) != 0 {
			t.Errorf("curated channel has %d messages; curation must be off without the option", len(posted))
		}
		if done, _ := lv.b.db.IsCurated(msg.ID); done {
			t.Error("message claimed as curated")
		}
		_, err = lv.b.s.ChannelMessageSendComplex(curated, &discordgo.MessageSend{Reference: &discordgo.MessageReference{
			Type: discordgo.MessageReferenceTypeForward, MessageID: msg.ID, ChannelID: chat, GuildID: g}})
		if app, aerr := lv.b.s.Application("@me"); aerr == nil && app.Flags&(1<<19) == 0 {
			if errCode(err) != 160014 {
				t.Errorf("forward with the switch off: err = %v, want Discord error 160014", err)
			}
		} else {
			t.Logf("the Message Content switch is on; forward err = %v", err)
		}
	})

	t.Run("AuditLog", func(t *testing.T) {
		lv.t = t
		bb, modlog, chat := lv.channels["bb"], lv.channels["modlog"], lv.channels["chat"]
		if err := lv.b.setConfig("BIG_BROTHER", bb); err != nil {
			t.Fatal(err)
		}

		// Channels: real events through the real handlers.
		ch, err := lv.b.s.GuildChannelCreate(g, "test-audit-a", discordgo.ChannelTypeGuildText)
		if err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "📁 Channel Created", "`test-audit-a`", "**Type:** text")
		topic := "audit topic"
		if _, err := lv.b.s.ChannelEdit(ch.ID, &discordgo.ChannelEdit{Name: "test-audit-b", Topic: topic}); err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "📝 Channel Updated", "`test-audit-a` → `test-audit-b`", "*none* → audit topic")
		if err := lv.b.s.ChannelPermissionSet(ch.ID, lv.roles["vip"], discordgo.PermissionOverwriteTypeRole,
			discordgo.PermissionViewChannel, discordgo.PermissionSendMessages); err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "📝 Channel Updated", "test-vip: +read_messages, -send_messages")
		if _, err := lv.b.s.ChannelDelete(ch.ID); err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "📁 Channel Deleted", "**#test-audit-b**")

		// Roles.
		none := int64(0)
		role, err := lv.b.s.GuildRoleCreate(g, &discordgo.RoleParams{Name: "test-audit-role", Permissions: &none})
		if err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "🎭 Role Created", "**test-audit-role**")
		perms := int64(discordgo.PermissionKickMembers)
		if _, err := lv.b.s.GuildRoleEdit(g, role.ID, &discordgo.RoleParams{Name: "test-audit-role2", Permissions: &perms}); err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "🎭 Role Updated", "`test-audit-role` → `test-audit-role2`", "**Permissions granted:** kick_members")
		if err := lv.b.s.GuildRoleDelete(g, role.ID); err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "🎭 Role Deleted", "**test-audit-role2**")

		// Threads.
		th, err := lv.b.s.ThreadStart(chat, "test-audit-thread", discordgo.ChannelTypeGuildPublicThread, 60)
		if err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "🧵 Thread Created", "`test-audit-thread`", "<@"+lv.botID+">")
		archived := true
		if _, err := lv.b.s.ChannelEdit(th.ID, &discordgo.ChannelEdit{Archived: &archived}); err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "🧵 Thread Updated", "**Archived:** `False` → `True`")
		if _, err := lv.b.s.ChannelDelete(th.ID); err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "🧵 Thread Deleted", "**test-audit-thread**")

		// Messages: the handlers ignore bots, so the target's message is
		// marked as a person's message in the cache first.
		msg, err := lv.target.ChannelMessageSend(chat, "audit me")
		if err != nil {
			t.Fatal(err)
		}
		lv.waitFor("the message in the cache", func() bool { _, ok := lv.b.msgs.get(msg.ID); return ok })
		c, _ := lv.b.msgs.get(msg.ID)
		c.Bot = false
		lv.b.msgs.put(msg.ID, c, messageCacheSize)
		if _, err := lv.target.ChannelMessageEdit(chat, msg.ID, "audit me (edited)"); err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "✏️ Message Edited", "<@"+lv.userID+">", "Message ID: "+msg.ID)
		if err := lv.target.ChannelMessageDelete(chat, msg.ID); err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "🗑️ Message Deleted", "<@"+lv.userID+">", "Message ID: "+msg.ID)

		old, err := lv.target.ChannelMessageSend(chat, "an old message")
		if err != nil {
			t.Fatal(err)
		}
		lv.waitFor("the old message in the cache", func() bool { _, ok := lv.b.msgs.get(old.ID); return ok })
		lv.b.msgs.pop(old.ID) // as if sent before the bot started
		if err := lv.target.ChannelMessageDelete(chat, old.ID); err != nil {
			t.Fatal(err)
		}
		lv.waitLog(bb, "🗑️ Old Message Deleted", "Author unknown", "Message ID: "+old.ID)

		// Invites: the log entry, then ❌ deletes the invite.
		inv, err := lv.b.s.ChannelInviteCreate(chat, discordgo.Invite{MaxAge: 600, MaxUses: 3})
		if err != nil {
			t.Fatal(err)
		}
		lv.waitLog(modlog, inviteLogTitle, "`"+inv.Code+"`", "**Max uses:** 3")
		logs, _ := lv.b.s.ChannelMessages(modlog, 5, "", "", "")
		var entry *discordgo.Message
		for _, m := range logs {
			if len(m.Embeds) > 0 && strings.Contains(m.Embeds[0].Description, inv.Code) {
				entry = m
				break
			}
		}
		if entry == nil || len(entry.Reactions) == 0 || entry.Reactions[0].Emoji.Name != "❌" {
			t.Fatalf("invite entry has no ❌: %+v", entry)
		}
		lv.b.onInviteDeleteReaction(&discordgo.MessageReactionAdd{MessageReaction: &discordgo.MessageReaction{
			UserID: lv.userID, MessageID: entry.ID, ChannelID: modlog, GuildID: g, Emoji: discordgo.Emoji{Name: "❌"},
		}}, lv.member())
		if _, err := lv.b.s.Invite(inv.Code); err == nil {
			t.Error("the invite still exists after ❌")
		}
		lv.waitLog(modlog, "🗑️ Invite Deleted", "`"+inv.Code+"`")
	})

	t.Run("AdminTools", func(t *testing.T) {
		lv.t = t
		// /permsreport
		report, _, skip := lv.b.permsReport(g, "live test")
		if !strings.Contains(report, "# test-vip  (no category)") || strings.Contains(report, "test-archived") {
			t.Errorf("report does not classify the test channels correctly")
		}
		if !strings.Contains(skip, "'archive'") {
			t.Errorf("skip note = %q", skip)
		}

		// /permtemplate: replace onto test-open, add onto test-merge.
		vip, err := lv.b.s.Channel(lv.channels["vip"])
		if err != nil {
			t.Fatal(err)
		}
		data, err := lv.b.templateData(vip)
		if err != nil {
			t.Fatal(err)
		}
		resolved, missing, err := lv.b.resolveTemplate(g, data)
		if err != nil || len(missing) != 0 {
			t.Fatalf("resolve: %v, missing %v", err, missing)
		}
		open, _ := lv.b.s.Channel(lv.channels["open"])
		result, _ := templatePlan(open.PermissionOverwrites, resolved, "replace")
		if err := lv.b.setOverwrites(open.ID, result, "live test replace"); err != nil {
			t.Fatal(err)
		}
		if open, _ = lv.b.s.Channel(open.ID); !sameOverwrites(open.PermissionOverwrites, vip.PermissionOverwrites) {
			t.Error("replace: test-open does not match the template")
		}
		merge, _ := lv.b.s.Channel(lv.channels["merge"])
		result, _ = templatePlan(merge.PermissionOverwrites, resolved, "add")
		if err := lv.b.setOverwrites(merge.ID, result, "live test add"); err != nil {
			t.Fatal(err)
		}
		merge, _ = lv.b.s.Channel(merge.ID)
		jail := roleOverwrite(merge, lv.roles["jail"])
		if jail.Allow&discordgo.PermissionEmbedLinks == 0 || jail.Deny&discordgo.PermissionViewChannel == 0 {
			t.Errorf("add: jail overwrite lost its old values: allow=%d deny=%d", jail.Allow, jail.Deny)
		}
		if roleOverwrite(merge, lv.roles["vip"]).Allow&discordgo.PermissionViewChannel == 0 {
			t.Error("add: the template's vip overwrite is missing")
		}

		// /channelarchive
		src, err := lv.b.s.GuildChannelCreateComplex(g, discordgo.GuildChannelCreateData{Name: "test-arch", Type: discordgo.ChannelTypeGuildText,
			Topic: "archive me", PermissionOverwrites: []*discordgo.PermissionOverwrite{{ID: lv.roles["vip"],
				Type: discordgo.PermissionOverwriteTypeRole, Allow: discordgo.PermissionViewChannel}}})
		if err != nil {
			t.Fatal(err)
		}
		lv.waitFor("test-arch in the cache", func() bool { _, err := lv.b.s.State.Channel(src.ID); return err == nil })
		cached, _ := lv.b.s.State.Channel(src.ID)
		if ok, note := lv.b.archiveChannel(g, cached, "live test"); !ok {
			t.Fatalf("archive: %s", note)
		}
		chans, _ := lv.b.s.GuildChannels(g)
		var clone *discordgo.Channel
		for _, c := range chans {
			if c.Name == "test-arch" && c.ID != src.ID {
				clone = c
			}
		}
		if clone == nil || clone.Topic != "archive me" || !sameOverwrites(clone.PermissionOverwrites, src.PermissionOverwrites) {
			t.Errorf("clone = %+v", clone)
		}
		moved, _ := lv.b.s.Channel(src.ID)
		cat, err := lv.b.s.Channel(moved.ParentID)
		if err != nil || !strings.HasPrefix(cat.Name, "Channel Archive ") || !sameOverwrites(moved.PermissionOverwrites, cat.PermissionOverwrites) {
			t.Errorf("original not archived and synced: parent %v", cat)
		}

		// /assets: upload, export through the real CDN, replace.
		if err := lv.b.createEmoji(g, "test_live_emoji", "e.png", testPNG(64, color.RGBA{255, 0, 0, 255}), "live test"); err != nil {
			t.Fatalf("create emoji: %v", err)
		}
		if err := lv.b.createSticker(g, "test-live-sticker", "live test", "🧪", "s.png", testPNG(320, color.RGBA{0, 0, 255, 255}), "live test"); err != nil {
			t.Fatalf("create sticker: %v", err)
		}
		inState := func(emoji, sticker string) func() bool {
			return func() bool {
				gs, err := lv.b.s.State.Guild(g)
				if err != nil {
					return false
				}
				lv.b.s.State.RLock()
				defer lv.b.s.State.RUnlock()
				found := 0
				for _, e := range gs.Emojis {
					if e.Name == emoji {
						found++
					}
				}
				for _, s := range gs.Stickers {
					if s.Name == sticker {
						found++
					}
				}
				return found == 2
			}
		}
		lv.waitFor("the test emoji and sticker in the cache", inState("test_live_emoji", "test-live-sticker"))

		zipData, emojis, stickers, _, err := lv.b.buildAssetsZip(g)
		if err != nil || emojis < 1 || stickers < 1 {
			t.Fatalf("export: %v (%d emojis, %d stickers)", err, emojis, stickers)
		}
		zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
		if err != nil {
			t.Fatal(err)
		}
		me, ms, _, ok := parseAssetManifest(zr)
		found := 0
		for _, e := range append(me, ms...) {
			if e.Name == "test_live_emoji" || e.Name == "test-live-sticker" {
				if data, err := readZipFile(zr, e.Filename); err == nil && len(data) > 0 {
					found++
				}
			}
		}
		if !ok || found != 2 {
			t.Errorf("export zip has %d of the 2 test assets", found)
		}

		oldEmojis, _ := lv.b.s.GuildEmojis(g)
		for _, p := range []assetConflict{
			{Kind: "emoji", Name: "test_live_emoji", Filename: "e.png", Image: testPNG(64, color.RGBA{0, 255, 0, 255}), GuildID: g},
			{Kind: "sticker", Name: "test-live-sticker", Filename: "s.png", Image: testPNG(320, color.RGBA{0, 255, 0, 255}),
				GuildID: g, Description: "replaced", EmojiTag: "🧪"},
		} {
			if err := lv.b.replaceAsset(p); err != nil {
				t.Errorf("replace %s: %v", p.Name, err)
			}
		}
		newEmojis, _ := lv.b.s.GuildEmojis(g)
		idOf := func(list []*discordgo.Emoji) string {
			for _, e := range list {
				if e.Name == "test_live_emoji" {
					return e.ID
				}
			}
			return ""
		}
		if before, after := idOf(oldEmojis), idOf(newEmojis); after == "" || after == before {
			t.Errorf("emoji not replaced: %s → %s", before, after)
		}
		n := 0
		for _, st := range lv.stickers() {
			if st.Name == "test-live-sticker" {
				n++
				if st.Description != "replaced" {
					t.Errorf("sticker description = %q", st.Description)
				}
			}
		}
		if n != 1 {
			t.Errorf("%d test stickers after replace, want 1", n)
		}
	})

}
