package bot

import (
	"strings"
	"testing"
)

func TestHelpLegalLinks(t *testing.T) {
	t.Setenv("PRIVACY_URL", "")
	t.Setenv("TERMS_URL", "")
	tb := newTestBot(t)
	lastField := func() string {
		r := tb.last()
		if len(r.Embeds) != 1 || len(r.Embeds[0].Fields) == 0 {
			t.Fatalf("help reply = %+v", r)
		}
		f := r.Embeds[0].Fields
		return f[len(f)-1].Value
	}
	want := "-# [Privacy Policy](https://github.com/binpas/hcbot/blob/main/PRIVACY.md) · " +
		"[Terms of Service](https://github.com/binpas/hcbot/blob/main/TERMS.md)"
	for _, user := range []string{tUser, tMod} {
		tb.run(user, "help")
		if got := lastField(); got != want {
			t.Errorf("help for %s ends with %q, want %q", user, got, want)
		}
	}

	tb.config(map[string]string{"PRIVACY_URL": "none", "TERMS_URL": "https://example.com/terms"})
	tb.run(tUser, "help")
	if got := lastField(); got != "-# [Terms of Service](https://example.com/terms)" {
		t.Errorf("help ends with %q", got)
	}

	tb.config(map[string]string{"PRIVACY_URL": "none", "TERMS_URL": "javascript:alert(1)"})
	tb.run(tUser, "help")
	if got := lastField(); strings.HasPrefix(got, "-# ") {
		t.Errorf("help still has a links line: %q", got)
	}
}
