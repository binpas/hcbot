package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/binpas/hcbot/internal/comic"
	"github.com/binpas/hcbot/internal/config"
)

const webTestURL = "https://hcbot.example.com"

type webTest struct {
	*testBot
	h http.Handler
}

func newWebTest(t *testing.T) *webTest {
	t.Helper()
	dir := t.TempDir()
	if err := comic.WriteTestPack(dir); err != nil {
		t.Fatal(err)
	}
	tb := newTestBot(t, func(e *config.Env) {
		e.MessageContent, e.ComicArtDir, e.WebURL, e.WebSecret, e.WebAddr = true, dir, webTestURL, "the-secret", ":0"
	})
	tb.config(map[string]string{"MOD_ROLE": tRoleMod, "MOD_LOG": tChModLog})
	tb.web.client = tb.s.Client
	tb.api.on("GET", "^/guilds/"+tGuild+"$", 200, fmt.Sprintf(`{"id":%q,"owner_id":%q}`, tGuild, tOwner))
	tb.api.on("GET", "^/guilds/"+tGuild+"/roles$", 200, fmt.Sprintf(
		`[{"id":%q,"permissions":"0"},{"id":%q,"permissions":"0"},{"id":"77","permissions":"8"}]`, tGuild, tRoleMod))
	w := &webTest{testBot: tb, h: tb.webHandler()}
	w.member(tMod, tRoleMod)
	return w
}

func (w *webTest) member(id string, roles ...string) {
	data, _ := json.Marshal(roles)
	w.api.on("GET", "^/guilds/"+tGuild+"/members/"+id+"$", 200,
		fmt.Sprintf(`{"user":{"id":%q,"username":"name%s"},"roles":%s}`, id, id, data))
}

// session logs a member in through the whole login.
func (w *webTest) session(id string) *http.Cookie {
	w.t.Helper()
	w.api.on("POST", "/oauth2/token", 200, `{"access_token":"tok","token_type":"Bearer"}`)
	w.api.on("GET", "^/users/@me$", 200, fmt.Sprintf(`{"id":%q,"username":"name%s"}`, id, id))
	r := httptest.NewRequest("GET", webTestURL+"/auth/callback?state=s&code=c", nil)
	r.AddCookie(&http.Cookie{Name: webStateName, Value: "s"})
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusFound {
		w.t.Fatalf("login of %s: %d %s", id, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == webSessionName && c.Value != "" {
			return c
		}
	}
	w.t.Fatal("no session cookie")
	return nil
}

func (w *webTest) do(method, path string, c *http.Cookie, body []byte, ctype string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, webTestURL+path, bytes.NewReader(body))
	if method != "GET" {
		r.Header.Set("Origin", webTestURL)
	}
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	if c != nil {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	return rec
}

func (w *webTest) json(method, path string, c *http.Cookie, v any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(v)
	return w.do(method, path, c, data, "application/json")
}

func (w *webTest) upload(path string, c *http.Cookie, data []byte, fields map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	part, _ := mw.CreateFormFile("file", "up.png")
	_, _ = part.Write(data)
	_ = mw.Close()
	return w.do("POST", path, c, buf.Bytes(), mw.FormDataContentType())
}

func webPNGFile(t *testing.T, width, height int, opaque bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	if opaque {
		for y := range height {
			for x := range width {
				img.SetNRGBA(x, y, color.NRGBA{10, 20, 30, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int, what string) {
	t.Helper()
	if rec.Code != want {
		t.Errorf("%s: status %d, want %d: %s", what, rec.Code, want, rec.Body)
	}
}

func TestWebGuard(t *testing.T) {
	w := newWebTest(t)
	c := w.session(tMod)

	r := httptest.NewRequest("GET", "https://192.168.4.40/api/me", nil)
	r.AddCookie(c)
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	wantStatus(t, rec, http.StatusMisdirectedRequest, "another host name")

	for _, origin := range []string{"", "https://evil.example.com", "null"} {
		r := httptest.NewRequest("DELETE", webTestURL+"/api/members/300", nil)
		r.AddCookie(c)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, r)
		wantStatus(t, rec, http.StatusForbidden, "a change from origin "+origin)
	}
	rec = w.do("GET", "/", nil, nil, "")
	wantStatus(t, rec, http.StatusOK, "the page")
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", rec.Header().Get("Content-Security-Policy"))
	}
	wantStatus(t, w.do("GET", "/api/art", nil, nil, ""), http.StatusUnauthorized, "art with no login")
	bad := &http.Cookie{Name: webSessionName, Value: "guess"}
	wantStatus(t, w.do("GET", "/api/art", bad, nil, ""), http.StatusUnauthorized, "art with a false session")
}

func TestWebLogin(t *testing.T) {
	w := newWebTest(t)

	rec := w.do("GET", "/auth/login", nil, nil, "")
	wantStatus(t, rec, http.StatusFound, "login")
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if q.Get("client_id") != tBot || q.Get("scope") != "identify" || q.Get("redirect_uri") != webTestURL+"/auth/callback" || q.Get("state") == "" {
		t.Errorf("authorize query = %v", q)
	}
	var state *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == webStateName {
			state = c
		}
	}
	if state == nil || state.Value != q.Get("state") || !state.Secure || !state.HttpOnly {
		t.Fatalf("state cookie = %+v", state)
	}

	// A wrong state does not call Discord.
	r := httptest.NewRequest("GET", webTestURL+"/auth/callback?state=wrong&code=c", nil)
	r.AddCookie(state)
	rec = httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	wantStatus(t, rec, http.StatusBadRequest, "wrong state")
	if n := len(w.api.find("POST", "/oauth2/token")); n != 0 {
		t.Errorf("%d token calls with a wrong state", n)
	}

	c := w.session(tMod)
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("session cookie = %+v", c)
	}
	calls := w.api.find("POST", "/oauth2/token")
	if len(calls) != 1 || !strings.Contains(string(calls[0].Body), "client_secret=the-secret") || !strings.Contains(string(calls[0].Body), "grant_type=authorization_code") {
		t.Errorf("token call = %+v", calls)
	}
	var who struct {
		ID    string `json:"id"`
		Admin bool   `json:"admin"`
	}
	rec = w.do("GET", "/api/me", c, nil, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &who); err != nil || who.ID != tMod || who.Admin {
		t.Errorf("/api/me = %s", rec.Body)
	}

	// Log out ends the session.
	wantStatus(t, w.do("POST", "/auth/logout", c, nil, ""), http.StatusNoContent, "log out")
	wantStatus(t, w.do("GET", "/api/me", c, nil, ""), http.StatusUnauthorized, "after log out")
}

func TestWebAccess(t *testing.T) {
	w := newWebTest(t)
	admin := func(id string, want bool) {
		t.Helper()
		var who struct {
			Admin bool `json:"admin"`
		}
		rec := w.do("GET", "/api/me", w.session(id), nil, "")
		if err := json.Unmarshal(rec.Body.Bytes(), &who); err != nil || who.Admin != want {
			t.Errorf("%s: /api/me = %s, want admin %v", id, rec.Body, want)
		}
	}
	admin(tMod, false)
	w.member(tOwner) // no roles, but the server owner
	admin(tOwner, true)
	w.member(tTarget, "77") // a role with Administrator
	admin(tTarget, true)

	// A member with no mod role, and someone who is not a member.
	for _, id := range []string{tUser, "999"} {
		if id == tUser {
			w.member(tUser, tRoleJail)
		}
		w.api.on("POST", "/oauth2/token", 200, `{"access_token":"tok"}`)
		w.api.on("GET", "^/users/@me$", 200, fmt.Sprintf(`{"id":%q}`, id))
		r := httptest.NewRequest("GET", webTestURL+"/auth/callback?state=s&code=c", nil)
		r.AddCookie(&http.Cookie{Name: webStateName, Value: "s"})
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, r)
		wantStatus(t, rec, http.StatusForbidden, "login of "+id)
		for _, c := range rec.Result().Cookies() {
			if c.Name == webSessionName {
				t.Errorf("%s got a session", id)
			}
		}
	}

	// A mod who loses the role loses access at the next check.
	c := w.session(tMod)
	wantStatus(t, w.do("GET", "/api/art", c, nil, ""), http.StatusOK, "art")
	w.member(tMod)
	wantStatus(t, w.do("GET", "/api/art", c, nil, ""), http.StatusOK, "before the next check")
	w.web.mu.Lock()
	w.web.sessions[c.Value].checked = time.Now().Add(-webRecheck - time.Minute)
	w.web.mu.Unlock()
	wantStatus(t, w.do("GET", "/api/art", c, nil, ""), http.StatusUnauthorized, "after the role is gone")
	wantStatus(t, w.do("GET", "/api/art", c, nil, ""), http.StatusUnauthorized, "the session is gone")
}

func (w *webTest) trashed(sub string) int {
	entries, _ := os.ReadDir(filepath.Join(w.env.ComicArtDir, "custom", ".trash", sub))
	return len(entries)
}

func TestWebBackgrounds(t *testing.T) {
	w := newWebTest(t)
	mod := w.session(tMod)
	w.member(tOwner)
	admin := w.session(tOwner)
	file := webPNGFile(t, 200, 200, true)

	wantStatus(t, w.upload("/api/backgrounds", mod, file, map[string]string{"name": "Beach"}), http.StatusOK, "add")
	if !strings.Contains(strings.Join(w.comics.pack.Load().Backgrounds(), " "), "beach") {
		t.Errorf("backgrounds = %v", w.comics.pack.Load().Backgrounds())
	}
	if got := w.sent(tChModLog); !contains(got, "saved background") {
		t.Errorf("mod log = %v", got)
	}
	wantStatus(t, w.do("GET", "/img/background/beach", mod, nil, ""), http.StatusOK, "picture")

	wantStatus(t, w.upload("/api/backgrounds", mod, file, map[string]string{"name": "beach"}), http.StatusConflict, "the same name")
	wantStatus(t, w.upload("/api/backgrounds", mod, file, map[string]string{"name": "plain"}), http.StatusConflict, "a built-in name")
	wantStatus(t, w.upload("/api/backgrounds", admin, file, map[string]string{"name": "plain", "replace": "true"}), http.StatusConflict, "replace a built-in")
	wantStatus(t, w.upload("/api/backgrounds", mod, file, map[string]string{"name": "beach", "replace": "true"}), http.StatusForbidden, "a mod replaces")
	wantStatus(t, w.upload("/api/backgrounds", mod, []byte("not an image"), map[string]string{"name": "x"}), http.StatusBadRequest, "not an image")
	wantStatus(t, w.upload("/api/backgrounds", mod, webPNGFile(t, 50, 50, true), map[string]string{"name": "small"}), http.StatusBadRequest, "too small")
	for _, name := range []string{"../../evil", "a/b", ".hidden", ""} {
		wantStatus(t, w.upload("/api/backgrounds", mod, file, map[string]string{"name": name}), http.StatusBadRequest, "name "+name)
	}
	if _, err := os.Stat(filepath.Join(w.env.ComicArtDir, "evil.png")); err == nil {
		t.Error("a file left the folder")
	}

	wantStatus(t, w.upload("/api/backgrounds", admin, file, map[string]string{"name": "beach", "replace": "true"}), http.StatusOK, "an admin replaces")
	if w.trashed("backgrounds") != 1 {
		t.Error("the old file is not in the trash")
	}

	wantStatus(t, w.do("DELETE", "/api/backgrounds/beach", mod, nil, ""), http.StatusForbidden, "a mod removes")
	wantStatus(t, w.do("DELETE", "/api/backgrounds/plain", admin, nil, ""), http.StatusNotFound, "remove a built-in")
	wantStatus(t, w.do("DELETE", "/api/backgrounds/beach", admin, nil, ""), http.StatusOK, "an admin removes")
	if strings.Contains(strings.Join(w.comics.pack.Load().Backgrounds(), " "), "beach") {
		t.Error("the background is still there")
	}
	if w.trashed("backgrounds") != 2 {
		t.Error("the removed file is not in the trash")
	}

	// Hide: a mod can do it; the pack does not offer it.
	wantStatus(t, w.json("POST", "/api/hidden", mod, map[string]any{"kind": "background", "name": "plain", "hidden": true}), http.StatusOK, "hide")
	if strings.Contains(strings.Join(w.comics.pack.Load().VisibleBackgrounds(), " "), "plain") {
		t.Error("a hidden background is offered")
	}
	wantStatus(t, w.json("POST", "/api/hidden", mod, map[string]any{"kind": "background", "name": "plain", "hidden": false}), http.StatusOK, "show")
	if !strings.Contains(strings.Join(w.comics.pack.Load().VisibleBackgrounds(), " "), "plain") {
		t.Error("the background did not come back")
	}
}

func TestWebCharacters(t *testing.T) {
	w := newWebTest(t)
	mod := w.session(tMod)
	w.member(tOwner)
	admin := w.session(tOwner)
	pose := webPNGFile(t, 100, 200, false)

	wantStatus(t, w.upload("/api/characters", mod, pose, map[string]string{"name": "newbie"}), http.StatusOK, "add")
	wantStatus(t, w.upload("/api/characters", mod, pose, map[string]string{"name": "ann"}), http.StatusConflict, "a built-in name")
	wantStatus(t, w.upload("/api/characters", mod, pose, map[string]string{"name": "../up"}), http.StatusBadRequest, "a bad name")

	wantStatus(t, w.upload("/api/characters/newbie/poses/happy", mod, pose, nil), http.StatusOK, "a pose")
	wantStatus(t, w.upload("/api/characters/newbie/poses/happy", mod, pose, nil), http.StatusConflict, "the pose again")
	wantStatus(t, w.upload("/api/characters/newbie/poses/happy", mod, pose, map[string]string{"replace": "true"}), http.StatusForbidden, "a mod replaces")
	wantStatus(t, w.upload("/api/characters/newbie/poses/happy", admin, pose, map[string]string{"replace": "true"}), http.StatusOK, "an admin replaces")
	wantStatus(t, w.upload("/api/characters/newbie/poses/nonsense", mod, pose, nil), http.StatusBadRequest, "an unknown pose")
	wantStatus(t, w.upload("/api/characters/nobody/poses/happy", mod, pose, nil), http.StatusNotFound, "no such character")
	jpegLike := webPNGFile(t, 100, 200, true)
	rec := w.upload("/api/characters/newbie/poses/sad", mod, jpegLike, nil)
	wantStatus(t, rec, http.StatusOK, "an opaque pose")
	if !strings.Contains(rec.Body.String(), "not transparent") {
		t.Errorf("no warning: %s", rec.Body)
	}

	// The face point and the other values of character.json.
	cfg := filepath.Join(w.env.ComicArtDir, "custom/characters/newbie/character.json")
	if err := os.WriteFile(cfg, []byte(`{"note":"keep me"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, w.json("PATCH", "/api/characters/newbie", mod, map[string]any{"face": []int{10, 20}}), http.StatusOK, "face")
	wantStatus(t, w.json("PATCH", "/api/characters/newbie", mod, map[string]any{"face": []int{500, 20}}), http.StatusBadRequest, "face outside")
	if data, _ := os.ReadFile(cfg); !strings.Contains(string(data), "keep me") || !strings.Contains(string(data), "face") {
		t.Errorf("character.json = %s", data)
	}

	// Hide: not offered, and not at random. A choice stays.
	wantStatus(t, w.json("POST", "/api/hidden", mod, map[string]any{"kind": "character", "name": "ann", "hidden": true}), http.StatusOK, "hide")
	pack := w.comics.pack.Load()
	if pack.CanChoose("ann", tUser) || strings.Contains(strings.Join(pack.NamesFor(tUser), " "), "ann") || pack.IsPublic("ann") {
		t.Error("a hidden character is offered")
	}
	if !pack.CanUse("ann", tUser) {
		t.Error("a member who chose it must keep it")
	}
	w.run(tUser, "character", strOpt("name", "ann"))
	w.expect(true, "no character")

	// At least one public character stays.
	for _, c := range pack.Characters() {
		if pack.IsPublic(c.Name) && pack.VisiblePublic() > 1 {
			wantStatus(t, w.json("POST", "/api/hidden", mod, map[string]any{"kind": "character", "name": c.Name, "hidden": true}), http.StatusOK, "hide "+c.Name)
			pack = w.comics.pack.Load()
		}
	}
	last := ""
	for _, c := range pack.Characters() {
		if pack.IsPublic(c.Name) {
			last = c.Name
		}
	}
	wantStatus(t, w.json("POST", "/api/hidden", mod, map[string]any{"kind": "character", "name": last, "hidden": true}), http.StatusBadRequest, "hide the last")

	// The hidden list survives a reload.
	w.run(tAdmin, "comicart reload")
	if w.comics.pack.Load().CanChoose("ann", tUser) {
		t.Error("reload lost the hidden list")
	}

	wantStatus(t, w.do("DELETE", "/api/characters/newbie/poses/sad", mod, nil, ""), http.StatusForbidden, "a mod removes a pose")
	wantStatus(t, w.do("DELETE", "/api/characters/newbie/poses/sad", admin, nil, ""), http.StatusOK, "an admin removes a pose")
	wantStatus(t, w.do("DELETE", "/api/characters/newbie/poses/neutral", admin, nil, ""), http.StatusBadRequest, "remove neutral")
	wantStatus(t, w.do("DELETE", "/api/characters/newbie", mod, nil, ""), http.StatusForbidden, "a mod removes")
	wantStatus(t, w.do("DELETE", "/api/characters/ann", admin, nil, ""), http.StatusNotFound, "remove a built-in")
	wantStatus(t, w.do("DELETE", "/api/characters/newbie", admin, nil, ""), http.StatusOK, "an admin removes")
	if w.comics.pack.Load().CanUse("newbie", tUser) {
		t.Error("the character is still there")
	}
	if w.trashed("characters") < 3 {
		t.Errorf("%d items in the trash, want the replaced pose, the removed pose, and the folder", w.trashed("characters"))
	}
}

func TestWebMembers(t *testing.T) {
	w := newWebTest(t)
	mod := w.session(tMod)
	w.member(tUser)

	// Assign with a lock: /character cannot change it.
	wantStatus(t, w.json("PUT", "/api/members/"+tUser, mod, map[string]any{"character": "ann", "locked": true}), http.StatusOK, "assign")
	wantStatus(t, w.json("PUT", "/api/members/"+tUser, mod, map[string]any{"character": "nobody"}), http.StatusBadRequest, "unknown character")
	wantStatus(t, w.json("PUT", "/api/members/abc", mod, map[string]any{"character": "ann"}), http.StatusBadRequest, "bad ID")
	w.run(tUser, "character", strOpt("name", "sim"))
	w.expect(true, "A mod set your comic character")
	w.click(tUser, "charreset")
	w.expect(true, "A mod set your comic character")
	if got, _ := w.db.ComicCharacter(tGuild, tUser); got != "ann" {
		t.Errorf("character = %q", got)
	}
	w.run(tUser, "character") // looking at it still works
	w.expect(true, "ann")

	rec := w.do("GET", "/api/members", mod, nil, "")
	var list []webMember
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0].ID != tUser || list[0].Name == "" || !list[0].Locked {
		t.Errorf("members = %s", rec.Body)
	}

	// Unlock: the member can change it. Without a lock they can too.
	wantStatus(t, w.json("PUT", "/api/members/"+tUser, mod, map[string]any{"character": "ann", "locked": false}), http.StatusOK, "unlock")
	w.run(tUser, "character", strOpt("name", "sim"))
	w.expect(true, "now")
	if got, _ := w.db.ComicCharacter(tGuild, tUser); got != "sim" {
		t.Errorf("character = %q", got)
	}

	// A private character: the assign adds the member to its list.
	dir := filepath.Join(w.env.ComicArtDir, "custom/characters/secret")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "neutral.png"), webPNGFile(t, 100, 200, false), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "character.json"), []byte(`{"users":["111"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w.run(tAdmin, "comicart reload")
	if w.comics.pack.Load().CanUse("secret", tUser) {
		t.Fatal("the private character is open to all")
	}
	wantStatus(t, w.json("PUT", "/api/members/"+tUser, mod, map[string]any{"character": "secret"}), http.StatusOK, "assign private")
	if !w.comics.pack.Load().CanUse("secret", tUser) {
		t.Error("the member is not on the list")
	}
	if !w.comics.pack.Load().CanUse("secret", "111") {
		t.Error("the old user is not on the list")
	}

	// Search: by name, and by a pasted ID.
	w.api.on("GET", "^/guilds/"+tGuild+"/members/search$", 200, fmt.Sprintf(`[{"user":{"id":%q,"username":"found"},"nick":"Found One","roles":[]}]`, tTarget))
	rec = w.do("GET", "/api/members/search?q=fou", mod, nil, "")
	if !strings.Contains(rec.Body.String(), "Found One") {
		t.Errorf("search = %s", rec.Body)
	}
	if calls := w.api.find("GET", "^/guilds/"+tGuild+"/members/search$"); len(calls) != 1 || !strings.Contains(calls[0].Query, "query=fou") {
		t.Errorf("search calls = %+v", calls)
	}
	rec = w.do("GET", "/api/members/search?q="+tUser, mod, nil, "")
	if !strings.Contains(rec.Body.String(), tUser) {
		t.Errorf("search by ID = %s", rec.Body)
	}
	rec = w.do("GET", "/api/members/search?q=", mod, nil, "")
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("empty search = %s", rec.Body)
	}

	// Clear removes the character and the lock.
	wantStatus(t, w.json("PUT", "/api/members/"+tUser, mod, map[string]any{"character": "ann", "locked": true}), http.StatusOK, "lock")
	wantStatus(t, w.do("DELETE", "/api/members/"+tUser, mod, nil, ""), http.StatusNoContent, "clear")
	if locked, _ := w.db.ComicLocked(tGuild, tUser); locked {
		t.Error("the lock is still set")
	}
}

func TestWebPreviewAndArt(t *testing.T) {
	w := newWebTest(t)
	mod := w.session(tMod)
	rec := w.do("GET", "/api/preview?character=ann&background=plain&seed=4", mod, nil, "")
	wantStatus(t, rec, http.StatusOK, "preview")
	if rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("type = %q", rec.Header().Get("Content-Type"))
	}
	wantStatus(t, w.do("GET", "/api/preview?character=nobody", mod, nil, ""), http.StatusNotFound, "unknown character")
	wantStatus(t, w.do("GET", "/api/preview", mod, nil, ""), http.StatusOK, "at random")
	wantStatus(t, w.do("GET", "/img/icon/ann", mod, nil, ""), http.StatusOK, "icon")
	wantStatus(t, w.do("GET", "/img/portrait/ann", mod, nil, ""), http.StatusOK, "portrait")
	wantStatus(t, w.do("GET", "/img/pose/ann/neutral", mod, nil, ""), http.StatusNotFound, "a built-in pose file")

	var art struct {
		Characters []comic.CharacterInfo  `json:"characters"`
		Backs      []comic.BackgroundInfo `json:"backgrounds"`
	}
	rec = w.do("GET", "/api/art", mod, nil, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &art); err != nil || len(art.Characters) == 0 || len(art.Backs) == 0 {
		t.Errorf("art = %s", rec.Body)
	}
}

func TestWebOff(t *testing.T) {
	tb := newTestBot(t) // no comics
	tb.env.WebURL, tb.env.WebSecret = webTestURL, "s"
	for _, e := range []func(){
		func() { tb.env.WebAddr = "" },
	} {
		e()
		if err := tb.startWeb(); err != nil {
			t.Errorf("no WEB_ADDR: %v", err)
		}
	}
	tb.env.WebAddr = "127.0.0.1:0"
	for name, breakIt := range map[string]func(){
		"no https": func() { tb.env.WebURL = "http://hcbot.example.com" },
		"secret":   func() { tb.env.WebURL, tb.env.WebSecret = webTestURL, "" },
		"guild":    func() { tb.env.WebSecret, tb.env.GuildID = "s", "" },
		"setup":    func() { tb.env.GuildID, tb.env.SetupMode = tGuild, true },
	} {
		breakIt()
		if err := tb.startWeb(); err == nil {
			t.Errorf("%s: the web interface started", name)
		}
		tb.env.WebURL, tb.env.WebSecret, tb.env.GuildID, tb.env.SetupMode = webTestURL, "s", tGuild, false
	}

	w := newWebTest(t)
	c := w.session(tMod)
	w.comics = nil
	rec := w.do("GET", "/api/art", c, nil, "")
	wantStatus(t, rec, http.StatusServiceUnavailable, "art with comics off")
}
