package bot

import (
	"fmt"
	"slices"
	"strconv"
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

// helpPages runs /help as userID and clicks Next until the last page. It
// returns each page's reply.
func (tb *testBot) helpPages(userID string) []reply {
	tb.t.Helper()
	tb.run(userID, "help")
	pages := []reply{tb.last()}
	for len(pages) < 50 {
		next := fmt.Sprintf("help:%s:%d", userID, len(pages))
		if !slices.Contains(pages[len(pages)-1].Components, componentInfo{CustomID: next}) {
			break // no Next button, or it is disabled
		}
		tb.click(userID, next)
		pages = append(pages, tb.last())
	}
	return pages
}

// helpText is the text of every /help page, joined.
func (tb *testBot) helpText(userID string) string {
	var sb strings.Builder
	for _, r := range tb.helpPages(userID) {
		sb.WriteString(r.text())
	}
	return sb.String()
}

// Discord refuses an embed over 6000 characters, or a field over 1024.
func TestHelpFitsEmbedLimits(t *testing.T) {
	tb := newTestBot(t, withMessageContent)
	pages := tb.helpPages(tMod)
	if want := len(tb.modCommandCategories()) + 1; len(pages) != want {
		t.Fatalf("got %d help pages, want %d", len(pages), want)
	}
	for i, r := range pages {
		e := r.Embeds[0]
		if !strings.Contains(e.Footer.Text, fmt.Sprintf("Page %d/%d", i+1, len(pages))) {
			t.Errorf("page %d footer = %q", i+1, e.Footer.Text)
		}
		total := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description) + utf8.RuneCountInString(e.Footer.Text)
		for _, f := range e.Fields {
			n := utf8.RuneCountInString(f.Value)
			if n > 1024 {
				t.Errorf("page %d: field %q is %d characters", i+1, f.Name, n)
			}
			total += utf8.RuneCountInString(f.Name) + n
		}
		if total > 6000 {
			t.Errorf("page %d is %d characters, over 6000", i+1, total)
		}
	}
}

func TestHelpPages(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "help")
	r := tb.expect(true, "General", "Sections", "Markov Chat", "Page 1/")
	if strings.Contains(r.text(), "/ban") {
		t.Error("page 1 shows a section's commands")
	}

	// The menu goes straight to a section.
	sel := tb.componentID(r, "help:"+tMod+":sel")
	cats := tb.modCommandCategories()
	jail := slices.IndexFunc(cats, func(c helpCategory) bool { return c.title == "Jail" }) + 1
	tb.click(tMod, sel, strconv.Itoa(jail))
	r = tb.expect(true, "/jail arrest", fmt.Sprintf("Page %d/%d", jail+1, len(cats)+1))
	if strings.Contains(r.text(), "/timeout") {
		t.Error("a section page shows the General commands")
	}
	tb.click(tMod, tb.componentID(r, fmt.Sprintf("help:%s:%d", tMod, jail-1)))
	tb.expect(true, cats[jail-2].title)

	// Someone else cannot turn the pages, and a member who lost the mod
	// role cannot either.
	before := len(tb.replies())
	tb.click(tAdmin, sel, "1")
	tb.expect(true, "isn't yours")
	tb.click(tUser, "help:"+tUser+":1")
	tb.expect(true, "must be a moderator")
	for _, r := range tb.replies()[before:] {
		if r.Kind == "update" {
			t.Errorf("page changed for another user: %q", r.text())
		}
	}

	// Members who are not mods get one page without buttons.
	tb.run(tUser, "help")
	if r := tb.expect(true, "/timeout"); len(r.Components) != 0 {
		t.Errorf("non-mod help has components: %+v", r.Components)
	}
}
