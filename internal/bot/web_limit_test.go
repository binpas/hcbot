package bot

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// callback makes the last request of a login, from an address.
func (w *webTest) callback(ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", webTestURL+"/auth/callback?state=s&code=c", nil)
	r.AddCookie(&http.Cookie{Name: webStateName, Value: "s"})
	if ip != "" {
		r.Header.Set("X-Forwarded-For", ip)
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	return rec
}

func TestWebLoginLimit(t *testing.T) {
	w := newWebTest(t)
	w.web.loginLimit = newWebLimiter(3, time.Hour)
	w.api.on("POST", "/oauth2/token", 400, `{"error":"invalid_grant"}`)

	// One address gets its limit. A false first value in the header does
	// not give it a new one: the proxy puts the real address last.
	for i := range 3 {
		wantStatus(t, w.callback(fmt.Sprintf("10.9.9.%d, 203.0.113.5", i)), http.StatusBadGateway, "a login in the limit")
	}
	rec := w.callback("10.9.9.9, 203.0.113.5")
	wantStatus(t, rec, http.StatusTooManyRequests, "over the limit")
	if rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), `href="/"`) {
		t.Errorf("the refusal has no wait time or no way back: %v %s", rec.Header(), rec.Body)
	}
	rec = httptest.NewRecorder()
	r := httptest.NewRequest("GET", webTestURL+"/auth/login", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.5")
	w.h.ServeHTTP(rec, r)
	wantStatus(t, rec, http.StatusTooManyRequests, "the login page, over the limit")
	if n := len(w.api.find("POST", "/oauth2/token")); n != 3 {
		t.Errorf("%d token calls, want 3", n)
	}
	// Another address, and another IPv6 network, are not affected.
	wantStatus(t, w.callback("203.0.113.6"), http.StatusBadGateway, "another address")
	for range 3 {
		wantStatus(t, w.callback("2001:db8:1:2::1"), http.StatusBadGateway, "an IPv6 address")
	}
	wantStatus(t, w.callback("2001:db8:1:2:ffff::2"), http.StatusTooManyRequests, "the same IPv6 network")
	wantStatus(t, w.callback("2001:db8:1:3::1"), http.StatusBadGateway, "another IPv6 network")

	// The limit for all addresses together keeps a flood away from Discord.
	w.web.loginLimit = newWebLimiter(10000, time.Hour)
	w.web.tokenLimit = newWebLimiter(2, time.Hour)
	before := len(w.api.find("POST", "/oauth2/token"))
	for i := range 5 {
		w.callback(fmt.Sprintf("198.51.100.%d", i))
	}
	if n := len(w.api.find("POST", "/oauth2/token")) - before; n != 2 {
		t.Errorf("%d token calls from 5 addresses, want 2", n)
	}
	// A wrong state uses none of that limit.
	w.web.tokenLimit = newWebLimiter(1, time.Hour)
	for range 3 {
		r := httptest.NewRequest("GET", webTestURL+"/auth/callback?state=wrong&code=c", nil)
		r.AddCookie(&http.Cookie{Name: webStateName, Value: "s"})
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, r)
		wantStatus(t, rec, http.StatusBadRequest, "wrong state")
	}
	if ok, _ := w.web.tokenLimit.allow(""); !ok {
		t.Error("a wrong state used the limit for all addresses")
	}
}

func TestWebLimiter(t *testing.T) {
	l := newWebLimiter(2, time.Minute)
	for i, want := range []bool{true, true, false, false} {
		ok, first := l.allow("a")
		if ok != want || first != (i == 2) {
			t.Errorf("request %d: ok %v, first %v", i, ok, first)
		}
	}
	l.mu.Lock()
	l.buckets["a"].at = time.Now().Add(-time.Minute)
	l.mu.Unlock()
	if ok, _ := l.allow("a"); !ok {
		t.Error("no new token after the wait")
	}
	if ok, first := l.allow("a"); ok || !first {
		t.Errorf("after the new token: ok %v, first %v", ok, first)
	}
	// It does not keep more keys than its limit.
	l = newWebLimiter(1, time.Hour)
	for i := range webLimitKeys + 50 {
		l.allow(fmt.Sprint(i))
	}
	if len(l.buckets) > webLimitKeys+1 {
		t.Errorf("%d buckets", len(l.buckets))
	}
}

func TestWebClientIP(t *testing.T) {
	w := newWebTest(t)
	for _, c := range []struct{ header, name, value, want string }{
		{"X-Forwarded-For", "X-Forwarded-For", "203.0.113.5", "203.0.113.5"},
		{"X-Forwarded-For", "X-Forwarded-For", "1.2.3.4, 203.0.113.5", "203.0.113.5"},
		{"X-Forwarded-For", "X-Forwarded-For", "not an address", "192.0.2.1"},
		{"X-Forwarded-For", "", "", "192.0.2.1"},
		{"CF-Connecting-IP", "Cf-Connecting-Ip", "2001:db8::1", "2001:db8::1"},
		{"CF-Connecting-IP", "X-Forwarded-For", "203.0.113.5", "192.0.2.1"},
		{"", "X-Forwarded-For", "203.0.113.5", "192.0.2.1"},
	} {
		w.env.WebIPHeader = c.header
		r := httptest.NewRequest("GET", webTestURL+"/", nil) // from 192.0.2.1
		if c.name != "" {
			r.Header.Set(c.name, c.value)
		}
		if got := w.webClientIP(r); got != c.want {
			t.Errorf("header %q, %s: %q = %q, want %q", c.header, c.name, c.value, got, c.want)
		}
	}
}

// A mod with no two-factor authentication cannot log in.
func TestWebLoginNeedsMFA(t *testing.T) {
	w := newWebTest(t)
	w.api.on("POST", "/oauth2/token", 200, `{"access_token":"tok"}`)
	for _, user := range []string{`{"id":%q,"username":"mod"}`, `{"id":%q,"username":"mod","mfa_enabled":false}`} {
		w.api.on("GET", "^/users/@me$", 200, fmt.Sprintf(user, tMod))
		rec := w.callback("")
		wantStatus(t, rec, http.StatusForbidden, "login with no two-factor authentication")
		if !strings.Contains(rec.Body.String(), "two-factor") {
			t.Errorf("the page does not say why: %s", rec.Body)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == webSessionName {
				t.Error("a member with no two-factor authentication got a session")
			}
		}
	}
}

func TestWebEndAll(t *testing.T) {
	w := newWebTest(t)
	w.member(tOwner)
	mod, other, admin := w.session(tMod), w.session(tMod), w.session(tOwner)
	wantStatus(t, w.do("POST", "/api/sessions/end", mod, nil, ""), http.StatusForbidden, "a mod")
	wantStatus(t, w.do("GET", "/api/me", other, nil, ""), http.StatusOK, "after the refusal")
	wantStatus(t, w.do("POST", "/api/sessions/end", admin, nil, ""), http.StatusNoContent, "an Administrator")
	for _, c := range []*http.Cookie{mod, other, admin} {
		wantStatus(t, w.do("GET", "/api/me", c, nil, ""), http.StatusUnauthorized, "after the end of all logins")
	}
	if got := w.sent(tChModLog); !contains(got, "ended all logins") {
		t.Error("no mod log entry")
	}
}
