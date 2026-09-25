package bot

// A fake Discord REST API and a test bot, for handler tests that need no
// network. The fake replaces the session's HTTP client: it records every
// request and answers with canned JSON. Interactions are fed straight into
// the real router (onInteraction), so tests exercise the same code path as
// a live slash command, button click, or modal submit.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/config"
	"github.com/binpas/hcbot/internal/db"
)

// apiCall is one recorded REST request.
type apiCall struct {
	Method string
	Path   string // without the /api/vN prefix or query string
	Query  string
	Body   []byte
	Reason string            // X-Audit-Log-Reason, decoded
	Files  map[string][]byte // multipart uploads, by file name
}

// JSON decodes the request body into a generic map.
func (a apiCall) JSON() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(a.Body, &m)
	return m
}

type route struct {
	method   string
	path     *regexp.Regexp
	contains string // when set, the request body must contain it
	times    int    // when > 0, the route answers this many times only
	status   int
	body     string
}

// fakeAPI is an http.RoundTripper that stands in for discord.com.
type fakeAPI struct {
	mu     sync.Mutex
	calls  []apiCall
	routes []route // checked newest first

	// dynamic, when set, may answer a request before the default responses.
	dynamic func(apiCall) (status int, body string, ok bool)
}

var (
	apiPrefix       = regexp.MustCompile(`^/api/v\d+`)
	editMessagePath = regexp.MustCompile(`^/channels/(\d+)/messages/(\d+)$`)
)

func (f *fakeAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	reason, err := url.PathUnescape(req.Header.Get("X-Audit-Log-Reason"))
	if err != nil {
		reason = "(bad escape) " + req.Header.Get("X-Audit-Log-Reason")
	}
	var files map[string][]byte
	if mt, params, err := mime.ParseMediaType(req.Header.Get("Content-Type")); err == nil && strings.HasPrefix(mt, "multipart/") {
		// A reply with files: the JSON is in the payload_json part.
		files = map[string][]byte{}
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		var payload []byte
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(part)
			if part.FormName() == "payload_json" {
				payload = data
			} else {
				files[part.FileName()] = data
			}
		}
		body = payload
	}
	call := apiCall{
		Files:  files,
		Method: req.Method,
		Path:   apiPrefix.ReplaceAllString(req.URL.Path, ""),
		Query:  req.URL.RawQuery,
		Body:   body,
		Reason: reason,
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	status, resp := 0, ""
	for i := len(f.routes) - 1; i >= 0; i-- {
		r := &f.routes[i]
		if r.method != call.Method || !r.path.MatchString(call.Path) ||
			(r.contains != "" && !strings.Contains(string(call.Body), r.contains)) || r.times < 0 {
			continue
		}
		if r.times > 0 {
			if r.times--; r.times == 0 {
				r.times = -1 // used up
			}
		}
		status, resp = r.status, r.body
		break
	}
	dynamic := f.dynamic
	f.mu.Unlock()
	if status == 0 && dynamic != nil {
		if st, body, ok := dynamic(call); ok {
			status, resp = st, body
		}
	}
	if status == 0 {
		status, resp = defaultResponse(call)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(resp)),
		Request:    req,
	}, nil
}

// defaultResponse answers the requests most handlers make.
func defaultResponse(c apiCall) (int, string) {
	switch {
	case c.Method == "DELETE":
		return http.StatusNoContent, ""
	case strings.HasSuffix(c.Path, "/callback"):
		return http.StatusNoContent, ""
	case c.Method == "GET" && strings.HasPrefix(c.Path, "/guilds/") && strings.Contains(c.Path, "/members/"):
		return http.StatusNotFound, `{"message":"Unknown Member","code":10007}`
	case c.Method == "GET" && strings.HasPrefix(c.Path, "/channels/") && strings.Count(c.Path, "/") == 2:
		return http.StatusNotFound, `{"message":"Unknown Channel","code":10003}`
	case c.Method == "GET" && strings.HasSuffix(c.Path, "/messages"):
		return http.StatusOK, `[]`
	case c.Method == "POST" && c.Path == "/users/@me/channels":
		return http.StatusOK, `{"id":"7000","type":1}`
	case strings.HasPrefix(c.Path, "/webhooks/") || strings.HasSuffix(c.Path, "/messages"):
		return http.StatusOK, `{"id":"8000","channel_id":"500"}`
	}
	return http.StatusOK, `{}`
}

// on makes requests matching method and the path regexp get this answer.
func (f *fakeAPI) on(method, pathRE string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = append(f.routes, route{method: method, path: regexp.MustCompile("^" + pathRE + "$"), status: status, body: body})
}

// onBody is on for requests whose body contains text, for n requests
// (n 0 means always).
func (f *fakeAPI) onBody(method, pathRE, contains string, n, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = append(f.routes, route{method: method, path: regexp.MustCompile("^" + pathRE + "$"),
		contains: contains, times: n, status: status, body: body})
}

// fail makes matching requests fail with 403 Missing Permissions.
func (f *fakeAPI) fail(method, pathRE string) {
	f.on(method, pathRE, http.StatusForbidden, `{"message":"Missing Permissions","code":50013}`)
}

// find returns the recorded calls matching method and the path regexp.
func (f *fakeAPI) find(method, pathRE string) []apiCall {
	re := regexp.MustCompile("^" + pathRE + "$")
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []apiCall
	for _, c := range f.calls {
		if c.Method == method && re.MatchString(c.Path) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeAPI) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// IDs of the test guild's fixtures.
const (
	tGuild   = "100"
	tBot     = "900"
	tOwner   = "901"
	tAdmin   = "201"
	tMod     = "200"
	tUser    = "202"
	tTarget  = "300"
	tVIPUser = "301"

	tRoleMod   = "10"
	tRoleJail  = "11"
	tRoleMOTD  = "12"
	tRoleKeep  = "13"                 // in JAIL_PROTECTED_ROLES
	tRoleStrip = "14"                 // stripped on arrest
	tRoleBot   = "15"                 // the bot's own top role
	tRoleVIP   = "16"                 // allows View Channel on tChVIP
	tRoleHigh  = "17"                 // above the bot's role
	tRoleBoost = "18"                 // managed (Server Booster)
	tRoleGamer = "123456789012345678" // a real-length ID, for mention/ID parsing

	tChGeneral  = "500"
	tChModLog   = "501"
	tChJail     = "502"
	tChSens     = "503"
	tCatArchive = "504"
	tChArchived = "505"
	tChTicket   = "506"
	tChVIP      = "507"
	tChSupport  = "508"
	tCatTickets = "509"
)

// testBot is a Bot wired to a fakeAPI, with a cached test guild.
type testBot struct {
	*Bot
	t          *testing.T
	api        *fakeAPI
	seq        int
	channelSeq int
	dbPath     string

	channelOverride string // channel for the next interaction; "" means general

	attachments map[string]*discordgo.MessageAttachment // for attachment options, by ID

	// invokerPerms overrides the interaction permissions of a fixture user.
	invokerPerms map[string]int64
}

// withMessageContent turns on ENABLE_MESSAGE_CONTENT_FEATURES before the
// bot is built (it decides which commands are registered).
func withMessageContent(e *config.Env) { e.MessageContent = true }

func newTestBot(t *testing.T, opts ...func(*config.Env)) *testBot {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	env := config.Env{BotToken: "test", GuildID: tGuild}
	for _, o := range opts {
		o(&env)
	}
	b, err := New(env, database)
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{}
	var tb *testBot
	b.forwardRetry = 0
	b.assetDelay = 0
	b.backupDir = filepath.Join(t.TempDir(), "backups")
	b.s.Client = &http.Client{Transport: api}
	b.s.State.User = &discordgo.User{ID: tBot, Username: "healthbot", Bot: true}

	view := int64(discordgo.PermissionViewChannel)
	role := discordgo.PermissionOverwriteTypeRole
	text := func(id, name, parent string, ows ...*discordgo.PermissionOverwrite) *discordgo.Channel {
		return &discordgo.Channel{ID: id, GuildID: tGuild, Name: name, Type: discordgo.ChannelTypeGuildText,
			ParentID: parent, PermissionOverwrites: ows}
	}
	member := func(id, name string, roles ...string) *discordgo.Member {
		return &discordgo.Member{GuildID: tGuild, User: &discordgo.User{ID: id, Username: name, Discriminator: "0"}, Roles: roles,
			JoinedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	g := &discordgo.Guild{
		ID: tGuild, Name: "Test Guild", OwnerID: tOwner, MemberCount: 7,
		Roles: []*discordgo.Role{
			{ID: tGuild, Name: "@everyone", Position: 0, Permissions: view | discordgo.PermissionSendMessages},
			{ID: tRoleMod, Name: "Mod", Position: 5},
			{ID: tRoleJail, Name: "Jail", Position: 4},
			{ID: tRoleMOTD, Name: "MOTD", Position: 3},
			{ID: tRoleKeep, Name: "Keep", Position: 2},
			{ID: tRoleStrip, Name: "Strip", Position: 1, Color: 0x123456},
			{ID: tRoleVIP, Name: "VIP", Position: 1},
			{ID: tRoleBot, Name: "Bot", Position: 10},
			{ID: tRoleHigh, Name: "High", Position: 11},
			{ID: tRoleBoost, Name: "Server Booster", Position: 1, Managed: true},
			{ID: tRoleGamer, Name: "Gamer", Position: 1},
		},
		Channels: []*discordgo.Channel{
			text(tChGeneral, "general", ""),
			text(tChModLog, "mod-log", ""),
			text(tChJail, "jail", ""),
			text(tChSens, "sensitive", ""),
			{ID: tCatArchive, GuildID: tGuild, Name: "Old Archive", Type: discordgo.ChannelTypeGuildCategory},
			text(tChArchived, "old-stuff", tCatArchive),
			text(tChTicket, "ticket-0001", ""),
			text(tChVIP, "vip", "",
				&discordgo.PermissionOverwrite{ID: tRoleVIP, Type: role, Allow: view},
				&discordgo.PermissionOverwrite{ID: tRoleJail, Type: role, Deny: view}),
			text(tChSupport, "support", ""),
			{ID: tCatTickets, GuildID: tGuild, Name: "Tickets", Type: discordgo.ChannelTypeGuildCategory},
		},
		Members: []*discordgo.Member{
			member(tBot, "healthbot", tRoleBot),
			member(tMod, "moddy", tRoleMod),
			member(tAdmin, "admin"),
			member(tUser, "user"),
			member(tTarget, "target", tRoleKeep, tRoleStrip),
			member(tVIPUser, "vipuser", tRoleVIP),
		},
	}
	g.Members[0].User.Bot = true // tBot
	if err := b.s.State.GuildAdd(g); err != nil {
		t.Fatal(err)
	}

	tb = &testBot{Bot: b, t: t, api: api, dbPath: dbPath}
	// Some requests are answered from the cache, like the real API would.
	api.dynamic = func(c apiCall) (int, string, bool) {
		if c.Method == "GET" && c.Path == "/guilds/"+tGuild+"/channels" {
			g, _ := b.s.State.Guild(tGuild)
			body, _ := json.Marshal(g.Channels)
			return http.StatusOK, string(body), true
		}
		if c.Method == "POST" && c.Path == "/guilds/"+tGuild+"/channels" {
			// Echo the new channel back with a fresh ID.
			var ch map[string]any
			_ = json.Unmarshal(c.Body, &ch)
			tb.channelSeq++
			ch["id"] = fmt.Sprint(600 + tb.channelSeq)
			ch["guild_id"] = tGuild
			body, _ := json.Marshal(ch)
			return http.StatusOK, string(body), true
		}
		if m := editMessagePath.FindStringSubmatch(c.Path); m != nil && c.Method == "PATCH" {
			// Echo an edited message back with its IDs.
			return http.StatusOK, fmt.Sprintf(`{"id":%q,"channel_id":%q}`, m[2], m[1]), true
		}
		userID, ok := strings.CutPrefix(c.Path, "/guilds/"+tGuild+"/members/")
		if c.Method != "GET" || !ok || strings.Contains(userID, "/") {
			return 0, "", false
		}
		m, err := b.s.State.Member(tGuild, userID)
		if err != nil {
			return 0, "", false
		}
		body, _ := json.Marshal(m)
		return http.StatusOK, string(body), true
	}
	tb.config(map[string]string{
		"MOD_ROLE":             tRoleMod,
		"MOD_LOG":              tChModLog,
		"JAIL_ROLE":            tRoleJail,
		"JAIL_CHANNEL":         tChJail,
		"JAIL_PROTECTED_ROLES": tRoleKeep,
		"MOTD_ROLE":            tRoleMOTD,
		"SENSITIVE_LOG":        tChSens,
		"TICKET_CAT":           tCatTickets,
	})
	return tb
}

func (tb *testBot) config(values map[string]string) {
	tb.t.Helper()
	for k, v := range values {
		if err := tb.setConfig(k, v); err != nil {
			tb.t.Fatal(err)
		}
	}
}

// invoker returns the interaction member for a fixture user. Admins get the
// Administrator permission; mods get the moderation permissions.
func (tb *testBot) invoker(userID string) *discordgo.Member {
	m, err := tb.s.State.Member(tGuild, userID)
	if err != nil {
		tb.t.Fatalf("no fixture member %s", userID)
	}
	cp := *m
	if p, ok := tb.invokerPerms[userID]; ok {
		cp.Permissions = p
		return &cp
	}
	switch userID {
	case tAdmin:
		cp.Permissions = discordgo.PermissionAdministrator
	case tMod:
		cp.Permissions = discordgo.PermissionManageMessages | discordgo.PermissionBanMembers |
			discordgo.PermissionKickMembers | discordgo.PermissionModerateMembers
	}
	return &cp
}

func (tb *testBot) interaction(userID string, typ discordgo.InteractionType, data discordgo.InteractionData) *discordgo.InteractionCreate {
	tb.seq++
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: fmt.Sprint(10000 + tb.seq), AppID: tBot, Token: fmt.Sprintf("tok%d", tb.seq),
		Type: typ, GuildID: tGuild, ChannelID: cmp.Or(tb.channelOverride, tChGeneral), Member: tb.invoker(userID), Data: data,
	}}
}

// run sends a slash command. name may hold a subcommand ("jail arrest").
func (tb *testBot) run(userID, name string, opts ...*discordgo.ApplicationCommandInteractionDataOption) {
	tb.t.Helper()
	data := discordgo.ApplicationCommandInteractionData{
		Name:     name,
		Resolved: &discordgo.ApplicationCommandInteractionDataResolved{Users: map[string]*discordgo.User{}, Members: map[string]*discordgo.Member{}},
	}
	if top, sub, ok := strings.Cut(name, " "); ok {
		data.Name = top
		data.Options = []*discordgo.ApplicationCommandInteractionDataOption{{
			Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts,
		}}
	} else {
		data.Options = opts
	}
	// Resolve user and attachment options the way Discord does.
	for _, o := range opts {
		if o.Type == discordgo.ApplicationCommandOptionAttachment {
			if data.Resolved.Attachments == nil {
				data.Resolved.Attachments = map[string]*discordgo.MessageAttachment{}
			}
			id := o.Value.(string)
			data.Resolved.Attachments[id] = tb.attachments[id]
			continue
		}
		if o.Type != discordgo.ApplicationCommandOptionUser {
			continue
		}
		id := o.Value.(string)
		if m, err := tb.s.State.Member(tGuild, id); err == nil {
			data.Resolved.Users[id] = m.User
			cp := *m
			cp.User = nil
			data.Resolved.Members[id] = &cp
		} else {
			data.Resolved.Users[id] = &discordgo.User{ID: id, Username: "stranger"}
		}
	}
	tb.onInteraction(tb.s, tb.interaction(userID, discordgo.InteractionApplicationCommand, data))
}

// click presses a button (or picks select values) with the given custom ID.
func (tb *testBot) click(userID, customID string, values ...string) {
	tb.t.Helper()
	tb.onInteraction(tb.s, tb.interaction(userID, discordgo.InteractionMessageComponent,
		discordgo.MessageComponentInteractionData{CustomID: customID, Values: values}))
}

// submit sends a modal with one text input per field.
func (tb *testBot) submit(userID, customID string, fields map[string]string) {
	tb.t.Helper()
	var rows []discordgo.MessageComponent
	for id, v := range fields {
		rows = append(rows, &discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			&discordgo.TextInput{CustomID: id, Value: v},
		}})
	}
	tb.onInteraction(tb.s, tb.interaction(userID, discordgo.InteractionModalSubmit,
		discordgo.ModalSubmitInteractionData{CustomID: customID, Components: rows}))
}

func strOpt(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: v}
}

func intOpt(name string, v int) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionInteger, Value: float64(v)}
}

func userOpt(name, id string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionUser, Value: id}
}

// reply is one thing the bot said in answer to an interaction.
type reply struct {
	Kind       string // "respond", "update", "defer", "defer-update", "modal", "followup", "edit"
	Embeds     []*discordgo.MessageEmbed
	Ephemeral  bool
	Components []componentInfo
	Cleared    bool // the reply removed all components
}

type componentInfo struct {
	CustomID string
	Disabled bool
}

type replyBody struct {
	Embeds     []*discordgo.MessageEmbed `json:"embeds"`
	Components json.RawMessage           `json:"components"`
	Flags      int                       `json:"flags"`
}

func (r *reply) fill(b replyBody) {
	r.Embeds = b.Embeds
	r.Ephemeral = b.Flags&int(discordgo.MessageFlagsEphemeral) != 0
	if len(b.Components) > 0 && string(b.Components) != "null" {
		var comps []any
		_ = json.Unmarshal(b.Components, &comps)
		r.Cleared = len(comps) == 0
		walkComponents(comps, &r.Components)
	}
}

func walkComponents(v any, out *[]componentInfo) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			walkComponents(e, out)
		}
	case map[string]any:
		if id, ok := x["custom_id"].(string); ok {
			d, _ := x["disabled"].(bool)
			*out = append(*out, componentInfo{id, d})
		}
		walkComponents(x["components"], out)
	}
}

// replies returns everything the bot sent as interaction answers, in order.
func (tb *testBot) replies() []reply {
	tb.api.mu.Lock()
	calls := append([]apiCall(nil), tb.api.calls...)
	tb.api.mu.Unlock()
	var out []reply
	for _, c := range calls {
		var r reply
		switch {
		case c.Method == "POST" && strings.HasSuffix(c.Path, "/callback"):
			var resp struct {
				Type int       `json:"type"`
				Data replyBody `json:"data"`
			}
			_ = json.Unmarshal(c.Body, &resp)
			r.Kind = map[int]string{4: "respond", 5: "defer", 6: "defer-update", 7: "update", 9: "modal"}[resp.Type]
			r.fill(resp.Data)
		case c.Method == "POST" && strings.HasPrefix(c.Path, "/webhooks/"):
			r.Kind = "followup"
			var body replyBody
			_ = json.Unmarshal(c.Body, &body)
			r.fill(body)
		case c.Method == "PATCH" && strings.HasSuffix(c.Path, "/messages/@original"):
			r.Kind = "edit"
			var body replyBody
			_ = json.Unmarshal(c.Body, &body)
			r.fill(body)
		default:
			continue
		}
		out = append(out, r)
	}
	return out
}

// last returns the last reply that carries embeds.
func (tb *testBot) last() reply {
	tb.t.Helper()
	rs := tb.replies()
	for i := len(rs) - 1; i >= 0; i-- {
		if len(rs[i].Embeds) > 0 {
			return rs[i]
		}
	}
	tb.t.Fatalf("no reply with embeds; calls: %v", tb.callList())
	return reply{}
}

// sent returns the embeds (and content) posted to a channel.
func (tb *testBot) sent(channelID string) []string {
	var out []string
	for _, c := range tb.api.find("POST", "/channels/"+channelID+"/messages") {
		var m struct {
			Content string                    `json:"content"`
			Embeds  []*discordgo.MessageEmbed `json:"embeds"`
			Embed   *discordgo.MessageEmbed   `json:"embed"`
		}
		_ = json.Unmarshal(c.Body, &m)
		text := m.Content
		for _, e := range append(m.Embeds, m.Embed) {
			if e != nil {
				text += embedText(e)
			}
		}
		out = append(out, text)
	}
	return out
}

func (tb *testBot) callList() []string {
	tb.api.mu.Lock()
	defer tb.api.mu.Unlock()
	var out []string
	for _, c := range tb.api.calls {
		out = append(out, c.Method+" "+c.Path)
	}
	return out
}

// embedText flattens an embed to text for substring checks.
func embedText(e *discordgo.MessageEmbed) string {
	var sb strings.Builder
	sb.WriteString(e.Title + "\n" + e.Description + "\n")
	for _, f := range e.Fields {
		sb.WriteString(f.Name + "\n" + f.Value + "\n")
	}
	if e.Footer != nil {
		sb.WriteString(e.Footer.Text)
	}
	return sb.String()
}

func (r reply) text() string {
	var sb strings.Builder
	for _, e := range r.Embeds {
		sb.WriteString(embedText(e))
	}
	return sb.String()
}

// expect checks that the last reply contains every want string.
func (tb *testBot) expect(ephemeral bool, want ...string) reply {
	tb.t.Helper()
	r := tb.last()
	if r.Ephemeral != ephemeral && r.Kind != "edit" && r.Kind != "update" {
		tb.t.Errorf("reply ephemeral = %v, want %v (%q)", r.Ephemeral, ephemeral, r.text())
	}
	for _, w := range want {
		if !strings.Contains(r.text(), w) {
			tb.t.Errorf("reply %q does not contain %q", r.text(), w)
		}
	}
	return r
}

// expectCall checks that exactly n matching requests were made and returns them.
func (tb *testBot) expectCall(n int, method, pathRE string) []apiCall {
	tb.t.Helper()
	got := tb.api.find(method, pathRE)
	if len(got) != n {
		tb.t.Errorf("%s %s: %d calls, want %d; all calls: %v", method, pathRE, len(got), n, tb.callList())
	}
	return got
}

// contains reports whether any string in list contains sub.
func contains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// rolesIn decodes the "roles" array of a member edit request.
func rolesIn(c apiCall) []string {
	var body struct {
		Roles []string `json:"roles"`
	}
	_ = json.Unmarshal(c.Body, &body)
	return body.Roles
}

// react adds a reaction as a fixture member, through the real handler.
func (tb *testBot) react(userID, channelID, messageID, emoji string) {
	tb.t.Helper()
	m, err := tb.s.State.Member(tGuild, userID)
	if err != nil {
		tb.t.Fatalf("no fixture member %s", userID)
	}
	tb.onReactionAdd(tb.s, &discordgo.MessageReactionAdd{
		MessageReaction: &discordgo.MessageReaction{
			UserID: userID, MessageID: messageID, ChannelID: channelID, GuildID: tGuild,
			Emoji: discordgo.Emoji{Name: emoji},
		},
		Member: m,
	})
}

// runIn sends a slash command from a given channel.
func (tb *testBot) runIn(channelID, userID, name string, opts ...*discordgo.ApplicationCommandInteractionDataOption) {
	tb.t.Helper()
	tb.channelOverride = channelID
	defer func() { tb.channelOverride = "" }()
	tb.run(userID, name, opts...)
}

// attachmentOpt adds an attachment the fake CDN serves with this content.
func (tb *testBot) attachmentOpt(name, filename, content string) *discordgo.ApplicationCommandInteractionDataOption {
	if tb.attachments == nil {
		tb.attachments = map[string]*discordgo.MessageAttachment{}
	}
	id := fmt.Sprint(900 + len(tb.attachments))
	tb.attachments[id] = &discordgo.MessageAttachment{ID: id, Filename: filename, Size: len(content),
		URL: "https://cdn.discordapp.com/attachments/1/" + id + "/" + filename}
	tb.api.on("GET", "/attachments/1/"+id+"/"+regexp.QuoteMeta(filename), 200, content)
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionAttachment, Value: id}
}

// say sends a message from a fixture member through the real handler.
// With mention, it starts with a mention of the bot.
func (tb *testBot) say(userID, channelID, text string, mention bool) {
	tb.t.Helper()
	m, err := tb.s.State.Member(tGuild, userID)
	if err != nil {
		tb.t.Fatalf("no fixture member %s", userID)
	}
	tb.seq++
	msg := &discordgo.Message{ID: fmt.Sprint(20000 + tb.seq), ChannelID: channelID, GuildID: tGuild,
		Author: m.User, Content: text}
	if mention {
		msg.Content = "<@" + tBot + "> " + text
		msg.Mentions = []*discordgo.User{{ID: tBot}}
	}
	tb.onMessageCreate(tb.s, &discordgo.MessageCreate{Message: msg})
}

// unreact removes a reaction as a fixture member, through the real handler.
func (tb *testBot) unreact(userID, channelID, messageID, emoji string) {
	tb.t.Helper()
	tb.onReactionRemove(tb.s, &discordgo.MessageReactionRemove{MessageReaction: &discordgo.MessageReaction{
		UserID: userID, MessageID: messageID, ChannelID: channelID, GuildID: tGuild,
		Emoji: discordgo.Emoji{Name: emoji},
	}})
}

// clickDM presses a button in a DM: the interaction has a user, no member
// and no guild.
func (tb *testBot) clickDM(userID, customID string) {
	tb.t.Helper()
	tb.seq++
	tb.onInteraction(tb.s, &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: fmt.Sprint(10000 + tb.seq), AppID: tBot, Token: fmt.Sprintf("tok%d", tb.seq),
		Type: discordgo.InteractionMessageComponent, ChannelID: "7000", User: &discordgo.User{ID: userID},
		Data: discordgo.MessageComponentInteractionData{CustomID: customID},
	}})
}
