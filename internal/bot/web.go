package bot

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/comic"
)

// The web interface is a page on the local network, behind an HTTPS proxy
// (README.md, "Web interface"). Members log in with Discord. Mods can use
// it; destructive actions are for Administrators. It runs only when
// WEB_ADDR, WEB_URL, DISCORD_CLIENT_SECRET and GUILD_ID are set.

//go:embed web/index.html
var webPage []byte

const (
	webSessionLife  = 12 * time.Hour
	webRecheck      = 5 * time.Minute // how old a mod check can be
	webNameLife     = time.Hour
	webMaxUpload    = 9 << 20 // a file of 8 MB, and the form around it
	webMaxJSON      = 64 << 10
	webSessionName  = "__Host-hcbot_session"
	webStateName    = "__Host-hcbot_state"
	webThumbWidth   = 320
	webMaxSearch    = 10
	webUndoLife     = 15 * time.Minute // a mod can undo their own upload for this time
	webNameFetchers = 5
)

type webLevel int

const (
	webAny webLevel = iota
	webMod
	webAdmin
)

type webSession struct {
	userID, name string
	admin        bool
	expires      time.Time
	checked      time.Time
}

type webName struct {
	name, avatar string
	at           time.Time
}

// webUpload is a new art file that a member can still undo.
type webUpload struct {
	userID string
	at     time.Time
}

type webState struct {
	mu       sync.Mutex
	sessions map[string]*webSession
	names    map[string]webName
	uploads  map[string]webUpload // who uploaded new art, for the undo. In memory only.

	artMu sync.Mutex // one change to the art at a time

	client       *http.Client // the OAuth2 calls (a fake in tests)
	apiURL       string
	authorizeURL string
}

func newWebState() webState {
	return webState{
		sessions:     map[string]*webSession{},
		names:        map[string]webName{},
		uploads:      map[string]webUpload{},
		client:       &http.Client{Timeout: 10 * time.Second},
		apiURL:       "https://discord.com/api/v10",
		authorizeURL: "https://discord.com/oauth2/authorize",
	}
}

// startWeb serves the web interface, if it is configured.
func (b *Bot) startWeb() error {
	if b.env.WebAddr == "" {
		return nil
	}
	if b.env.SetupMode {
		return errors.New("setup mode is on")
	}
	u, err := url.Parse(b.env.WebURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("WEB_URL must be the https:// address of the page")
	}
	if b.env.WebSecret == "" {
		return errors.New("DISCORD_CLIENT_SECRET is not set")
	}
	if b.env.GuildID == "" {
		return errors.New("GUILD_ID is not set")
	}
	ln, err := net.Listen("tcp", b.env.WebAddr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           b.webHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("web: %v", err)
		}
	}()
	go func() {
		<-b.stop
		_ = srv.Close()
	}()
	log.Printf("Web interface on %s for %s", ln.Addr(), b.env.WebURL)
	return nil
}

// webHandler has the routes, inside the checks that all requests get.
func (b *Bot) webHandler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, level webLevel, h func(http.ResponseWriter, *http.Request, *webSession)) {
		mux.HandleFunc(pattern, b.webAuth(level, h))
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(webPage)
	})
	mux.HandleFunc("GET /auth/login", b.webLogin)
	mux.HandleFunc("GET /auth/callback", b.webCallback)
	route("POST /auth/logout", webAny, b.webLogout)
	route("GET /api/me", webMod, b.webMe)

	route("GET /api/art", webMod, b.webArt)
	route("GET /img/icon/{name}", webMod, b.webIcon)
	route("GET /img/portrait/{name}", webMod, b.webPortrait)
	route("GET /img/background/{name}", webMod, b.webBackground)
	route("GET /img/pose/{name}/{pose}", webMod, b.webPose)
	route("GET /api/preview", webMod, b.webPreview)

	route("POST /api/backgrounds", webMod, b.webAddBackground)
	route("DELETE /api/backgrounds/{name}", webMod, b.webRemoveBackground)
	route("POST /api/hidden", webMod, b.webSetHidden)

	route("POST /api/characters", webMod, b.webAddCharacter)
	route("DELETE /api/characters/{name}", webMod, b.webRemoveCharacter)
	route("POST /api/characters/{name}/poses/{pose}", webMod, b.webAddPose)
	route("DELETE /api/characters/{name}/poses/{pose}", webMod, b.webRemovePose)
	route("PATCH /api/characters/{name}", webMod, b.webCharacterSettings)

	route("GET /api/members", webMod, b.webMembers)
	route("GET /api/members/search", webMod, b.webSearchMembers)
	route("PUT /api/members/{id}", webMod, b.webSetMember)
	route("DELETE /api/members/{id}", webMod, b.webClearMember)

	return b.webGuard(mux)
}

// webGuard protects against other sites: a page on the internet can send
// requests to a LAN address. Only the configured host name works, and a
// change must come from this page.
func (b *Bot) webGuard(next http.Handler) http.Handler {
	base, _ := url.Parse(b.env.WebURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != base.Host {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != b.env.WebURL {
			webFail(w, http.StatusForbidden, "I refused this request, because it did not come from this page. Reload the page and try again.")
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; "+
			"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; "+
			"img-src 'self' https://cdn.discordapp.com; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func webJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func webFail(w http.ResponseWriter, status int, text string) {
	webJSON(w, status, map[string]string{"error": text})
}

func webRandom() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf) // never fails (Go 1.24)
	return hex.EncodeToString(buf)
}

func webCookie(name, value string, age time.Duration) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: int(age.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

// webAuth finds the session and checks the level. A mod check is at most
// webRecheck old, so a member who loses the role loses access soon.
func (b *Bot) webAuth(level webLevel, h func(http.ResponseWriter, *http.Request, *webSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := b.webCurrent(r)
		if s == nil {
			webFail(w, http.StatusUnauthorized, "Please log in.")
			return
		}
		if level != webAny && time.Since(s.checked) > webRecheck {
			mod, admin, err := b.webAccess(s.userID)
			if err != nil {
				log.Printf("web: access check: %v", err)
				webFail(w, http.StatusServiceUnavailable, "I could not ask Discord if you are a mod. Try again.")
				return
			}
			if !mod {
				b.webEnd(r)
				webFail(w, http.StatusUnauthorized, "You no longer have the mod role, so your login ended.")
				return
			}
			s.admin, s.checked = admin, time.Now()
			if c, err := r.Cookie(webSessionName); err == nil {
				b.web.mu.Lock()
				if live := b.web.sessions[c.Value]; live != nil {
					live.admin, live.checked = admin, s.checked
				}
				b.web.mu.Unlock()
			}
		}
		if level == webAdmin && !s.admin {
			webFail(w, http.StatusForbidden, "Only Administrators can do this.")
			return
		}
		h(w, r, s)
	}
}

// webCurrent returns a copy of the session of the request, or nil.
func (b *Bot) webCurrent(r *http.Request) *webSession {
	c, err := r.Cookie(webSessionName)
	if err != nil {
		return nil
	}
	b.web.mu.Lock()
	defer b.web.mu.Unlock()
	s := b.web.sessions[c.Value]
	if s == nil {
		return nil
	}
	if time.Now().After(s.expires) {
		delete(b.web.sessions, c.Value)
		return nil
	}
	cp := *s
	return &cp
}

func (b *Bot) webEnd(r *http.Request) {
	if c, err := r.Cookie(webSessionName); err == nil {
		b.web.mu.Lock()
		delete(b.web.sessions, c.Value)
		b.web.mu.Unlock()
	}
}

// webAccess asks Discord who the member is now. The Permissions value of a
// member is set only in interactions, so this works out Administrator from
// the roles and the owner.
func (b *Bot) webAccess(userID string) (mod, admin bool, err error) {
	gid := b.env.GuildID
	m, err := b.s.GuildMember(gid, userID)
	if isDiscordError(err, discordgo.ErrCodeUnknownMember) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	g, err := b.s.Guild(gid)
	if err != nil {
		return false, false, err
	}
	admin = g.OwnerID == userID
	roles, err := b.s.GuildRoles(gid)
	if err != nil {
		return false, false, err
	}
	for _, role := range roles {
		if (role.ID == gid || slices.Contains(m.Roles, role.ID)) && role.Permissions&discordgo.PermissionAdministrator != 0 {
			admin = true
		}
	}
	modRole := b.cfg.ID("MOD_ROLE")
	return admin || (modRole != "" && slices.Contains(m.Roles, modRole)), admin, nil
}

func (b *Bot) webRedirect() string { return b.env.WebURL + "/auth/callback" }

func (b *Bot) webLogin(w http.ResponseWriter, r *http.Request) {
	state := webRandom()
	http.SetCookie(w, webCookie(webStateName, state, 10*time.Minute))
	q := url.Values{
		"client_id":     {b.s.State.User.ID},
		"redirect_uri":  {b.webRedirect()},
		"response_type": {"code"},
		"scope":         {"identify"},
		"state":         {state},
	}
	http.Redirect(w, r, b.web.authorizeURL+"?"+q.Encode(), http.StatusFound)
}

// webLoginFailed shows why the login stopped, in the colors of the page, with
// a way back.
func webLoginFailed(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>HEALTH Bot</title>
<style>body{margin:0;min-height:100vh;display:grid;place-content:center;gap:16px;padding:16px;text-align:center;background:#000;color:#f7ecec;font:16px Roboto,system-ui,sans-serif}a{color:#ff6b66}</style>
<p>%s</p><p><a href="/">Back to the login page</a></p></html>`, html.EscapeString(text))
}

// webCallback is where Discord sends the browser after the login. The
// access token is used once, to get the user ID, and it is not kept.
func (b *Bot) webCallback(w http.ResponseWriter, r *http.Request) {
	state, err := r.Cookie(webStateName)
	if err != nil || state.Value == "" || state.Value != r.URL.Query().Get("state") {
		webLoginFailed(w, http.StatusBadRequest, "The login expired or is not correct. Log in again.")
		return
	}
	http.SetCookie(w, webCookie(webStateName, "", -time.Second))
	code := r.URL.Query().Get("code")
	if code == "" {
		webLoginFailed(w, http.StatusForbidden, "Discord did not log you in. Try again, and select Authorize on the Discord page.")
		return
	}
	user, err := b.webDiscordUser(r.Context(), code)
	if err != nil {
		log.Printf("web: login: %v", err)
		webLoginFailed(w, http.StatusBadGateway, "I could not complete the login with Discord. Try again.")
		return
	}
	mod, admin, err := b.webAccess(user.ID)
	if err != nil {
		log.Printf("web: access check: %v", err)
		webLoginFailed(w, http.StatusBadGateway, "I could not ask Discord if you are a mod. Try again.")
		return
	}
	if !mod {
		webLoginFailed(w, http.StatusForbidden, "This page is only for mods of the server. You need the mod role to log in.")
		return
	}
	id := webRandom()
	now := time.Now()
	b.web.mu.Lock()
	for k, s := range b.web.sessions {
		if now.After(s.expires) {
			delete(b.web.sessions, k)
		}
	}
	b.web.sessions[id] = &webSession{userID: user.ID, name: user.Username, admin: admin,
		expires: now.Add(webSessionLife), checked: now}
	b.web.mu.Unlock()
	http.SetCookie(w, webCookie(webSessionName, id, webSessionLife))
	http.Redirect(w, r, "/", http.StatusFound)
}

func (b *Bot) webDiscordUser(ctx context.Context, code string) (*discordgo.User, error) {
	form := url.Values{
		"client_id":     {b.s.State.User.ID},
		"client_secret": {b.env.WebSecret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {b.webRedirect()},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.web.apiURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := b.webDo(req, &tok); err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("token: empty")
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, b.web.apiURL+"/users/@me", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	var u discordgo.User
	if err := b.webDo(req, &u); err != nil {
		return nil, fmt.Errorf("user: %w", err)
	}
	if u.ID == "" {
		return nil, errors.New("user: empty")
	}
	return &u, nil
}

// webDo makes a request. Its errors never hold the secret or the token,
// only the status.
func (b *Bot) webDo(req *http.Request, out any) error {
	resp, err := b.web.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, webMaxJSON)).Decode(out)
}

func (b *Bot) webLogout(w http.ResponseWriter, r *http.Request, _ *webSession) {
	b.webEnd(r)
	http.SetCookie(w, webCookie(webSessionName, "", -time.Second))
	w.WriteHeader(http.StatusNoContent)
}

func (b *Bot) webMe(w http.ResponseWriter, _ *http.Request, s *webSession) {
	webJSON(w, http.StatusOK, map[string]any{
		"id": s.userID, "name": s.name, "admin": s.admin,
		"comics": b.comics != nil, "poses": comic.PoseNames(),
	})
}

// webPack returns the art, or answers that comics are off.
func (b *Bot) webPack(w http.ResponseWriter) *comic.Pack {
	if b.comics == nil {
		webFail(w, http.StatusServiceUnavailable, "Comics are off on this bot.")
		return nil
	}
	return b.comics.pack.Load()
}

// The keys of the upload records.
func webBackgroundKey(name string) string { return "background/" + name }
func webCharacterKey(name string) string  { return "character/" + name }
func webPoseKey(name, pose string) string { return "pose/" + name + "/" + pose }

// webNoteUpload records who uploaded a new file. A replaced file has no
// undo: that needs an Administrator, and the undo must not remove their work.
func (b *Bot) webNoteUpload(s *webSession, key string, replaced bool) {
	b.web.mu.Lock()
	defer b.web.mu.Unlock()
	for k, u := range b.web.uploads {
		if time.Since(u.at) >= webUndoLife {
			delete(b.web.uploads, k)
		}
	}
	if replaced {
		delete(b.web.uploads, key)
		return
	}
	b.web.uploads[key] = webUpload{userID: s.userID, at: time.Now()}
}

func (b *Bot) webForgetUpload(key string) {
	b.web.mu.Lock()
	delete(b.web.uploads, key)
	b.web.mu.Unlock()
}

// webCanRemove allows a removal: Administrators can remove all custom art,
// and a mod can undo their own new upload for a short time.
func (b *Bot) webCanRemove(w http.ResponseWriter, s *webSession, key string) bool {
	if s.admin {
		return true
	}
	b.web.mu.Lock()
	u, ok := b.web.uploads[key]
	b.web.mu.Unlock()
	if ok && u.userID == s.userID && time.Since(u.at) < webUndoLife {
		return true
	}
	webFail(w, http.StatusForbidden, fmt.Sprintf("Only an Administrator can remove this. You can undo your own upload for %d minutes.", int(webUndoLife.Minutes())))
	return false
}

// webUndoable lists the uploads that a member can undo now, with the
// seconds that are left.
func (b *Bot) webUndoable(s *webSession) map[string]int {
	out := map[string]int{}
	b.web.mu.Lock()
	defer b.web.mu.Unlock()
	for k, u := range b.web.uploads {
		if left := webUndoLife - time.Since(u.at); u.userID == s.userID && left > 0 {
			out[k] = int(left.Seconds())
		}
	}
	return out
}

func (b *Bot) webArtBody(pack *comic.Pack, s *webSession) (map[string]any, error) {
	members, err := b.db.ComicMembers(b.env.GuildID)
	if err != nil {
		return nil, err
	}
	used := map[string]int{}
	for _, m := range members {
		used[m.Character]++
	}
	return map[string]any{
		"characters":  pack.Characters(),
		"backgrounds": pack.BackgroundList(),
		"problems":    pack.Problems(),
		"used":        used,
		"undo":        b.webUndoable(s),
	}, nil
}

func (b *Bot) webArt(w http.ResponseWriter, _ *http.Request, s *webSession) {
	pack := b.webPack(w)
	if pack == nil {
		return
	}
	body, err := b.webArtBody(pack, s)
	if err != nil {
		log.Printf("web: art: %v", err)
		webFail(w, http.StatusInternalServerError, "I could not read the database. Try again. If this continues, tell an operator.")
		return
	}
	webJSON(w, http.StatusOK, body)
}

func webPNG(w http.ResponseWriter, data []byte, err error) {
	if err != nil {
		webFail(w, http.StatusNotFound, "Not found.")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(data)
}

func (b *Bot) webIcon(w http.ResponseWriter, r *http.Request, _ *webSession) {
	if pack := b.webPack(w); pack != nil {
		data, err := pack.IconPNG(r.PathValue("name"))
		webPNG(w, data, err)
	}
}

func (b *Bot) webPortrait(w http.ResponseWriter, r *http.Request, _ *webSession) {
	if pack := b.webPack(w); pack != nil {
		data, err := pack.PortraitPNG(r.PathValue("name"))
		webPNG(w, data, err)
	}
}

func (b *Bot) webBackground(w http.ResponseWriter, r *http.Request, _ *webSession) {
	if pack := b.webPack(w); pack != nil {
		data, err := pack.BackgroundPNG(r.PathValue("name"), webThumbWidth)
		webPNG(w, data, err)
	}
}

func (b *Bot) webPose(w http.ResponseWriter, r *http.Request, _ *webSession) {
	if b.webPack(w) != nil {
		data, err := comic.CharacterFile(b.env.ComicArtDir, r.PathValue("name"), r.PathValue("pose"))
		webPNG(w, data, err)
	}
}

func (b *Bot) webPreview(w http.ResponseWriter, r *http.Request, _ *webSession) {
	pack := b.webPack(w)
	if pack == nil {
		return
	}
	q := r.URL.Query()
	seed, _ := strconv.ParseUint(q.Get("seed"), 10, 64)
	data, err := pack.PreviewPNG(q.Get("character"), q.Get("background"), seed)
	if err != nil && !errors.Is(err, comic.ErrNotFound) {
		log.Printf("web: preview: %v", err)
	}
	webPNG(w, data, err)
}

// webChange runs a change to the art, reads the art again, writes the mod
// log, and answers with the new art. The old art stays if the new art
// cannot be read.
func (b *Bot) webChange(w http.ResponseWriter, s *webSession, what string, change func(pack *comic.Pack) (string, error)) {
	if b.webPack(w) == nil {
		return
	}
	b.web.artMu.Lock()
	defer b.web.artMu.Unlock()
	warning, err := change(b.comics.pack.Load())
	if err != nil {
		b.webChangeFailed(w, what, err)
		return
	}
	pack, err := b.loadComicPack()
	if err != nil {
		log.Printf("web: reload art: %v", err)
		webFail(w, http.StatusInternalServerError, "The change is saved, but comics still use the old art. Tell an operator.")
		return
	}
	b.comics.pack.Store(pack)
	logComicProblems(pack)
	b.webAudit(s, what)
	body, err := b.webArtBody(pack, s)
	if err != nil {
		log.Printf("web: art: %v", err)
		webFail(w, http.StatusInternalServerError, "I could not read the database. Try again. If this continues, tell an operator.")
		return
	}
	if warning != "" {
		body["warning"] = warning
	}
	webJSON(w, http.StatusOK, body)
}

func (b *Bot) webChangeFailed(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, comic.ErrInvalid):
		text := strings.TrimPrefix(err.Error(), comic.ErrInvalid.Error()+": ")
		webFail(w, http.StatusBadRequest, strings.ToUpper(text[:1])+text[1:]+".")
	case errors.Is(err, comic.ErrExists):
		webFail(w, http.StatusConflict, "That name is already in use, so type another name. If this is your own upload, you can undo it. An Administrator can replace custom art.")
	case errors.Is(err, comic.ErrNotFound):
		webFail(w, http.StatusNotFound, "This art does not exist, or it is built-in art, which cannot be changed.")
	default:
		log.Printf("web: %s: %v", what, err)
		webFail(w, http.StatusInternalServerError, "I could not save the change. Try again.")
	}
}

// webAudit records a change in the log and the mod log.
func (b *Bot) webAudit(s *webSession, what string) {
	log.Printf("web: %s (%s): %s", s.name, s.userID, what)
	b.modLog(makeEmbed("🌐 Web interface", fmt.Sprintf("**%s** (<@%s>): %s", escapeMarkdown(s.name), s.userID, what), colourBlurple))
}

// webFile reads the uploaded file of a form.
func webFile(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, webMaxUpload)
	f, _, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			webFail(w, http.StatusRequestEntityTooLarge, "The file is larger than 8 MB.")
		} else {
			webFail(w, http.StatusBadRequest, "Select a file.")
		}
		return nil, false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		webFail(w, http.StatusBadRequest, "I could not read the file.")
		return nil, false
	}
	return data, true
}

// webReplace reads the form's "replace" value. Replacing art is
// destructive, so it is only for Administrators.
func webReplace(w http.ResponseWriter, r *http.Request, s *webSession) (replace, ok bool) {
	replace = r.FormValue("replace") == "true"
	if replace && !s.admin {
		webFail(w, http.StatusForbidden, "Only Administrators can replace art.")
		return false, false
	}
	return replace, true
}

func webArtName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func (b *Bot) webAddBackground(w http.ResponseWriter, r *http.Request, s *webSession) {
	data, ok := webFile(w, r)
	if !ok {
		return
	}
	replace, ok := webReplace(w, r, s)
	if !ok {
		return
	}
	name := webArtName(r.FormValue("name"))
	b.webChange(w, s, "saved background `"+cutText(name, 40)+"`", func(pack *comic.Pack) (string, error) {
		for _, bg := range pack.BackgroundList() {
			if bg.Name == name && !bg.Custom {
				return "", comic.ErrExists
			}
		}
		if err := comic.SaveBackground(b.env.ComicArtDir, name, data, replace); err != nil {
			return "", err
		}
		b.webNoteUpload(s, webBackgroundKey(name), replace)
		return "", nil
	})
}

func (b *Bot) webRemoveBackground(w http.ResponseWriter, r *http.Request, s *webSession) {
	name := r.PathValue("name")
	if !b.webCanRemove(w, s, webBackgroundKey(name)) {
		return
	}
	b.webChange(w, s, "removed background `"+cutText(name, 40)+"`", func(*comic.Pack) (string, error) {
		if err := comic.RemoveBackground(b.env.ComicArtDir, name); err != nil {
			return "", err
		}
		b.webForgetUpload(webBackgroundKey(name))
		return "", nil
	})
}

func (b *Bot) webSetHidden(w http.ResponseWriter, r *http.Request, s *webSession) {
	var req struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Hidden bool   `json:"hidden"`
	}
	if !webReadJSON(w, r, &req) {
		return
	}
	verb := "showed"
	if req.Hidden {
		verb = "hid"
	}
	b.webChange(w, s, fmt.Sprintf("%s %s `%s`", verb, req.Kind, cutText(req.Name, 40)), func(pack *comic.Pack) (string, error) {
		switch req.Kind {
		case "character":
			known := false
			for _, c := range pack.Characters() {
				known = known || c.Name == req.Name
			}
			if !known {
				return "", comic.ErrNotFound
			}
			if req.Hidden && pack.IsPublic(req.Name) && pack.VisiblePublic() == 1 {
				return "", fmt.Errorf("%w: at least one public character must stay visible", comic.ErrInvalid)
			}
		case "background":
			if !slices.Contains(pack.Backgrounds(), req.Name) {
				return "", comic.ErrNotFound
			}
		default:
			return "", fmt.Errorf("%w: unknown kind", comic.ErrInvalid)
		}
		return "", b.db.SetComicHidden(req.Kind, req.Name, req.Hidden)
	})
}

func webReadJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, webMaxJSON)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		webFail(w, http.StatusBadRequest, "The request is not correct.")
		return false
	}
	return true
}

func (b *Bot) webAddCharacter(w http.ResponseWriter, r *http.Request, s *webSession) {
	data, ok := webFile(w, r)
	if !ok {
		return
	}
	name := webArtName(r.FormValue("name"))
	b.webChange(w, s, "added character `"+cutText(name, 40)+"`", func(pack *comic.Pack) (string, error) {
		for _, c := range pack.Characters() {
			if c.Name == name {
				return "", comic.ErrExists
			}
		}
		warning, err := comic.SaveCharacterFile(b.env.ComicArtDir, name, "neutral", data, false)
		if err == nil {
			b.webNoteUpload(s, webCharacterKey(name), false)
		}
		return warning, err
	})
}

func (b *Bot) webRemoveCharacter(w http.ResponseWriter, r *http.Request, s *webSession) {
	name := r.PathValue("name")
	if !b.webCanRemove(w, s, webCharacterKey(name)) {
		return
	}
	b.webChange(w, s, "removed character `"+cutText(name, 40)+"`", func(*comic.Pack) (string, error) {
		if err := comic.RemoveCharacter(b.env.ComicArtDir, name); err != nil {
			return "", err
		}
		b.webForgetUpload(webCharacterKey(name))
		return "", nil
	})
}

func (b *Bot) webAddPose(w http.ResponseWriter, r *http.Request, s *webSession) {
	data, ok := webFile(w, r)
	if !ok {
		return
	}
	replace, ok := webReplace(w, r, s)
	if !ok {
		return
	}
	name, pose := r.PathValue("name"), r.PathValue("pose")
	b.webChange(w, s, fmt.Sprintf("saved pose `%s` of `%s`", cutText(pose, 20), cutText(name, 40)), func(*comic.Pack) (string, error) {
		warning, err := comic.SaveCharacterFile(b.env.ComicArtDir, name, pose, data, replace)
		if err == nil {
			b.webNoteUpload(s, webPoseKey(name, pose), replace)
		}
		return warning, err
	})
}

func (b *Bot) webRemovePose(w http.ResponseWriter, r *http.Request, s *webSession) {
	name, pose := r.PathValue("name"), r.PathValue("pose")
	if !b.webCanRemove(w, s, webPoseKey(name, pose)) {
		return
	}
	b.webChange(w, s, fmt.Sprintf("removed pose `%s` of `%s`", cutText(pose, 20), cutText(name, 40)), func(*comic.Pack) (string, error) {
		if err := comic.RemoveCharacterFile(b.env.ComicArtDir, name, pose); err != nil {
			return "", err
		}
		b.webForgetUpload(webPoseKey(name, pose))
		return "", nil
	})
}

func (b *Bot) webCharacterSettings(w http.ResponseWriter, r *http.Request, s *webSession) {
	var req struct {
		Face  *[2]int   `json:"face"`
		Users *[]string `json:"users"`
	}
	if !webReadJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	b.webChange(w, s, "changed the settings of character `"+cutText(name, 40)+"`", func(*comic.Pack) (string, error) {
		return "", comic.UpdateCharacter(b.env.ComicArtDir, name, comic.CharacterSettings{Face: req.Face, Users: req.Users})
	})
}

type webMember struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Avatar    string `json:"avatar"`
	Character string `json:"character,omitempty"`
	Locked    bool   `json:"locked"`
}

func webMemberName(m *discordgo.Member) (name, avatar string) {
	return m.DisplayName(), m.AvatarURL("64")
}

// webLookupName returns a member's name and picture from a short cache.
// The name is "" when the member cannot be found.
func (b *Bot) webLookupName(id string) (string, string) {
	b.web.mu.Lock()
	n, ok := b.web.names[id]
	b.web.mu.Unlock()
	if ok && time.Since(n.at) < webNameLife {
		return n.name, n.avatar
	}
	m, err := b.s.GuildMember(b.env.GuildID, id)
	n = webName{at: time.Now()}
	if err == nil {
		n.name, n.avatar = webMemberName(m)
	} else if !isDiscordError(err, discordgo.ErrCodeUnknownMember) {
		log.Printf("web: member %s: %v", id, err)
		return "", "" // do not keep this one
	}
	b.web.mu.Lock()
	b.web.names[id] = n
	b.web.mu.Unlock()
	return n.name, n.avatar
}

func (b *Bot) webMembers(w http.ResponseWriter, _ *http.Request, _ *webSession) {
	rows, err := b.db.ComicMembers(b.env.GuildID)
	if err != nil {
		log.Printf("web: members: %v", err)
		webFail(w, http.StatusInternalServerError, "I could not read the database. Try again. If this continues, tell an operator.")
		return
	}
	out := make([]webMember, len(rows))
	var wg sync.WaitGroup
	slots := make(chan struct{}, webNameFetchers)
	for i, row := range rows {
		out[i] = webMember{ID: row.UserID, Character: row.Character, Locked: row.Locked}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			out[i].Name, out[i].Avatar = b.webLookupName(row.UserID)
		})
	}
	wg.Wait()
	webJSON(w, http.StatusOK, out)
}

// webSearchMembers finds members by name, or by a pasted user ID. It uses
// Discord's search: the bot has no Server Members intent and no member list.
func (b *Bot) webSearchMembers(w http.ResponseWriter, r *http.Request, _ *webSession) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	rows, err := b.db.ComicMembers(b.env.GuildID)
	if err != nil {
		log.Printf("web: members: %v", err)
		webFail(w, http.StatusInternalServerError, "I could not read the database. Try again. If this continues, tell an operator.")
		return
	}
	// A result must show the member's current character and lock, or an
	// assign could replace them with no warning.
	found := func(id, name, avatar string) webMember {
		m := webMember{ID: id, Name: name, Avatar: avatar}
		for _, row := range rows {
			if row.UserID == id {
				m.Character, m.Locked = row.Character, row.Locked
			}
		}
		return m
	}
	out := []webMember{}
	switch {
	case q == "":
	case comic.IsUserID(q):
		if name, avatar := b.webLookupName(q); name != "" {
			out = append(out, found(q, name, avatar))
		}
	default:
		members, err := b.s.GuildMembersSearch(b.env.GuildID, q, webMaxSearch)
		if err != nil {
			log.Printf("web: member search: %v", err)
			webFail(w, http.StatusBadGateway, "The Discord search did not work. Paste the user ID, or try again.")
			return
		}
		for _, m := range members {
			if m.User != nil {
				name, avatar := webMemberName(m)
				out = append(out, found(m.User.ID, name, avatar))
			}
		}
	}
	webJSON(w, http.StatusOK, out)
}

// webSetMember assigns a character and sets the lock. A private character
// gets the member on its users list: without that, comics ignore it.
func (b *Bot) webSetMember(w http.ResponseWriter, r *http.Request, s *webSession) {
	var req struct {
		Character string `json:"character"`
		Locked    bool   `json:"locked"`
	}
	if !webReadJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if !comic.IsUserID(id) {
		webFail(w, http.StatusBadRequest, "That is not a Discord user ID.")
		return
	}
	lock := "unlocked"
	if req.Locked {
		lock = "locked"
	}
	b.webChange(w, s, fmt.Sprintf("set the character of <@%s> to `%s` (%s)", id, cutText(req.Character, 40), lock), func(pack *comic.Pack) (string, error) {
		var char *comic.CharacterInfo
		chars := pack.Characters()
		for i := range chars {
			if chars[i].Name == req.Character {
				char = &chars[i]
			}
		}
		if char == nil {
			return "", fmt.Errorf("%w: there is no character named %q", comic.ErrInvalid, req.Character)
		}
		if len(char.Users) > 0 && !slices.Contains(char.Users, id) {
			if err := comic.UpdateCharacter(b.env.ComicArtDir, char.Name, comic.CharacterSettings{AddUser: id}); err != nil {
				return "", err
			}
		}
		return "", b.db.SetComicMember(b.env.GuildID, id, req.Character, req.Locked)
	})
}

// webClearMember removes a member's character and lock.
func (b *Bot) webClearMember(w http.ResponseWriter, r *http.Request, s *webSession) {
	id := r.PathValue("id")
	if !comic.IsUserID(id) {
		webFail(w, http.StatusBadRequest, "That is not a Discord user ID.")
		return
	}
	if err := b.db.DeleteComicCharacter(b.env.GuildID, id); err != nil {
		log.Printf("web: clear member: %v", err)
		webFail(w, http.StatusInternalServerError, "I could not save the change. Try again.")
		return
	}
	b.webAudit(s, fmt.Sprintf("cleared the character of <@%s>", id))
	w.WriteHeader(http.StatusNoContent)
}
