package bot

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestWebTickets(t *testing.T) {
	w := newWebTest(t)
	w.env.TicketFilesDir = t.TempDir()
	w.ticket("650", tTarget, "target", 1, "open", true)
	w.modTicket("660", 2, tUser)
	w.sqlExec(`INSERT INTO ticket_archive (guild_id, user_id, username, channel_name, number, transcript)
		VALUES (?, ?, 'old-user', 'x', 1, 'an old line')`, tGuild, tVIPUser)
	pic := w.cdnFile("31", "pic.png", "image/png", "PNGDATA")
	page := w.cdnFile("32", "evil.html", "text/html", "<script>alert(1)</script>")
	w.ticketMessage("20100", "650", tTarget, "help me please", pic, page)
	w.ticketMessage("20101", "660", tUser, "a mod ticket line")
	mod := w.session(tMod)

	if rec := w.do("GET", "/api/tickets", nil, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no login: %d", rec.Code)
	}

	list := func(query string) []webTicketRow {
		t.Helper()
		rec := w.do("GET", "/api/tickets"+query, mod, nil, "")
		wantStatus(t, rec, http.StatusOK, "list "+query)
		var body struct{ Tickets []webTicketRow }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Tickets
	}
	if all := list(""); len(all) != 3 {
		t.Errorf("all = %+v", all)
	}
	if got := list("?user=" + tUser); len(got) != 1 || got[0].Ref != "mod-2" || got[0].Members != "user"+tUser {
		t.Errorf("by user ID = %+v", got)
	}
	if got := list("?user=targ"); len(got) != 1 || got[0].Ref != "target-1" || got[0].Messages != 1 || got[0].Link == "" {
		t.Errorf("by name = %+v", got)
	}
	if got := list("?q=old+line"); len(got) != 1 || got[0].Key[0] != 'l' {
		t.Errorf("legacy text = %+v", got)
	}
	if got := list("?q=please&user=old"); len(got) != 0 {
		t.Errorf("both filters = %+v", got)
	}

	tk, _ := w.db.TicketByChannel("650")
	rec := w.do("GET", fmt.Sprintf("/api/tickets/t%d", tk.ID), mod, nil, "")
	wantStatus(t, rec, http.StatusOK, "ticket")
	var d struct {
		Ref      string
		Messages []webTicketMessage
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Ref != "target-1" || len(d.Messages) != 1 || len(d.Messages[0].Files) != 2 {
		t.Fatalf("ticket = %s", rec.Body)
	}
	img, html := d.Messages[0].Files[0], d.Messages[0].Files[1]
	if img.Media != "img" || html.Media != "" || !img.Saved {
		t.Errorf("files = %+v", d.Messages[0].Files)
	}

	rec = w.do("GET", fmt.Sprintf("/files/ticket/%d", img.ID), mod, nil, "")
	wantStatus(t, rec, http.StatusOK, "picture")
	if rec.Body.String() != "PNGDATA" || rec.Header().Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "inline") {
		t.Errorf("picture: %v %q", rec.Header(), rec.Body)
	}
	rec = w.do("GET", fmt.Sprintf("/files/ticket/%d", html.ID), mod, nil, "")
	if rec.Header().Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("an HTML file must be a download: %v", rec.Header())
	}

	rec = w.do("GET", "/api/tickets/l1", mod, nil, "")
	if wantStatus(t, rec, http.StatusOK, "legacy"); !strings.Contains(rec.Body.String(), "an old line") {
		t.Errorf("legacy = %s", rec.Body)
	}
	for _, bad := range []string{"/api/tickets/x1", "/api/tickets/t999", "/api/tickets/l999", "/files/ticket/999", "/files/ticket/abc"} {
		wantStatus(t, w.do("GET", bad, mod, nil, ""), http.StatusNotFound, bad)
	}
}
