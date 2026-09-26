package bot

import (
	"strings"
	"testing"
	"unicode/utf8"
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

// Discord refuses an embed over 6000 characters, or a field over 1024.
func TestHelpFitsEmbedLimits(t *testing.T) {
	tb := newTestBot(t, withMessageContent)
	tb.run(tMod, "help")
	e := tb.last().Embeds[0]
	total := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description) + utf8.RuneCountInString(e.Footer.Text)
	for _, f := range e.Fields {
		n := utf8.RuneCountInString(f.Value)
		if n > 1024 {
			t.Errorf("field %q is %d characters", f.Name, n)
		}
		total += utf8.RuneCountInString(f.Name) + n
	}
	if total > 6000 {
		t.Errorf("mod help embed is %d characters, over 6000", total)
	}
	t.Logf("mod help embed: %d characters", total)
}
