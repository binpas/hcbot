package bot

import (
	"strconv"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/db"
)

func TestParseTourFile(t *testing.T) {
	rows, problems := parseTourFile([]byte(tourSampleCSV))
	if len(problems) > 0 || len(rows) != 5 {
		t.Fatalf("sample: %d rows, %v", len(rows), problems)
	}
	if r := rows[3]; r.Tour != "Example Tour 2027" || r.Date != "2027-04-02" || r.Fields["city"] != "London" || r.Fields["support"] != "Other Band" {
		t.Errorf("row 4 = %+v", r)
	}

	// A BOM, other header names, and a JSON file.
	rows, problems = parseTourFile([]byte("\xef\xbb\xbfTour, Date ,Ticket Link\nT,2027-01-02,x\n\n"))
	if len(problems) > 0 || len(rows) != 1 || rows[0].Fields["ticket_link"] != "x" {
		t.Errorf("BOM file: %+v, %v", rows, problems)
	}
	rows, problems = parseTourFile([]byte(`[{"tour":"T","date":"2027-01-02","capacity":1200,"sold_out":false}]`))
	if len(problems) > 0 || rows[0].Fields["capacity"] != "1200" || rows[0].Fields["sold_out"] != "false" {
		t.Errorf("JSON file: %+v, %v", rows, problems)
	}

	for in, want := range map[string]string{
		"tour,date\nT,11/06/2027\n":                  "is not a YYYY-MM-DD date",
		"tour,date\nT,2027-02-30\n":                  "is not a YYYY-MM-DD date",
		"tour,date\n,2027-01-02\n":                   "has no `tour`",
		"tour,date\nT,2027-01-02\nt,2027-01-02\n":    "Line 3 has the same tour and date as line 2",
		"tour,date,weekday\nT,2027-01-02,x\n":        "token that the bot makes",
		"tour,date\nT,2027-01-02,extra\n":            "more values than the header",
		"tour,date,a!b\nT,2027-01-02,x\n":            "can have only letters",
		"tour,date\n":                                "no shows",
		`[{"tour":"T","date":"2027-01-02","x":[1]}]`: "must be text or a number",
		`[{"tour":"T"`:                               "is not an array",
	} {
		if _, problems := parseTourFile([]byte(in)); !contains(problems, want) {
			t.Errorf("%q: problems %v, want %q", in, problems, want)
		}
	}
}

func TestRenderTour(t *testing.T) {
	us := db.TourShow{Date: "2027-03-05", Fields: map[string]string{"city": "Portland", "region": "OR", "link": "https://x"}}
	eu := db.TourShow{Date: "2027-03-05", Fields: map[string]string{"city": "Berlin", "region": "Germany"}}
	cols := tourColumns([]db.TourShow{us, eu})
	for _, c := range []struct {
		tmpl string
		show db.TourShow
		want string
	}{
		{defaultTourTitle, us, "SPRING - 03/05 - PORTLAND - OR"},
		{defaultTourTitle, eu, "SPRING - 05/03 - BERLIN - GERMANY"},
		{"{tour} {city} {City} {link}", us, "Spring Portland Portland https://x"},
		{"{link}", eu, ""}, // a column that this show does not have
		{"{weekday} {MONTH} {date_us} {date_eu} {date_iso}", eu, "Friday MARCH 03/05 05/03 2027-03-05"},
		{"{date_local}", us, "<t:1804248000:D>"},
	} {
		if got, unknown := renderTour(c.tmpl, "Spring", c.show, cols); got != c.want || len(unknown) > 0 {
			t.Errorf("%q = %q (unknown %v), want %q", c.tmpl, got, unknown, c.want)
		}
	}
	if _, unknown := renderTour("{venue} {cty} {cty}", "Spring", us, cols); len(unknown) != 2 {
		t.Errorf("unknown = %v", unknown)
	}

	tour := &db.Tour{Name: strings.Repeat("x", 99), TitleTemplate: defaultTourTitle, PostTemplate: "{nope}"}
	text := renderTourShow(tour, us, cols)
	if !contains(text.Problems, "the title has") || !contains(text.Problems, "not columns: `{nope}`") {
		t.Errorf("problems = %v", text.Problems)
	}
	for region, want := range map[string]bool{"": true, "ca": true, "New York": true, "Washington DC": true, "UK": false, "QLD": false} {
		if usDates(region) != want {
			t.Errorf("usDates(%q) = %v", region, !want)
		}
	}
}

func TestParseShowText(t *testing.T) {
	s := db.TourShow{Date: "2027-03-05", Fields: map[string]string{"zeta": "z", "city": "Portland", "venue": "Crystal: Ballroom"}}
	text := showText(s)
	if text != "date: 2027-03-05\nvenue: Crystal: Ballroom\ncity: Portland\nzeta: z" {
		t.Errorf("showText = %q", text)
	}
	date, fields, err := parseShowText(text + "\nNew Column: yes\n")
	if err != nil || date != s.Date || fields["venue"] != "Crystal: Ballroom" || fields["new_column"] != "yes" {
		t.Errorf("parse = %q %v %v", date, fields, err)
	}
	for _, bad := range []string{"venue: x", "date: 2027-03-05\nno colon", "date: 2027-03-05\ntour: x", "date: 2027-03-05\nmonth: x"} {
		if _, _, err := parseShowText(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

const tForum = "700"

func newTourBot(t *testing.T) *testBot {
	tb := newTestBot(t)
	if err := tb.s.State.ChannelAdd(&discordgo.Channel{ID: tForum, GuildID: tGuild, Name: "tour", Type: discordgo.ChannelTypeGuildForum}); err != nil {
		t.Fatal(err)
	}
	tb.api.on("POST", "/channels/"+tForum+"/threads", 200, `{"id":"7001","type":11}`)
	return tb
}

func (tb *testBot) uploadTour(csv string) {
	tb.t.Helper()
	tb.run(tMod, "tour upload", tb.attachmentOpt("file", "shows.csv", csv))
	tb.click(tMod, tb.componentID(tb.last(), "confirm:yes:tourupload:"))
	tb.expect(true, "Shows saved")
}

func TestTourFlow(t *testing.T) {
	tb := newTourBot(t)
	csv := strings.ReplaceAll(tourSampleCSV, "2027", "2099")
	tb.run(tMod, "tour upload", tb.attachmentOpt("file", "shows.csv", csv))
	tb.expect(true, "**Example Tour 2099** (a new tour): 5 new, 0 changed")
	tb.click(tMod, tb.componentID(tb.last(), "confirm:yes:tourupload:"))
	tb.expect(true, "5 new, 0 changed")
	tour, _ := tb.db.TourByName(tGuild, "example tour 2099")
	if tour == nil || tour.TitleTemplate != defaultTourTitle {
		t.Fatalf("tour = %+v", tour)
	}
	id := strconv.FormatInt(tour.ID, 10)

	// Post before setup: no forum.
	tb.run(tMod, "tour post")
	tb.expect(true, "The tour has no forum")

	// Setup: one tour, so its view at once. Choose the forum.
	tb.run(tMod, "tour setup")
	tb.expect(true, "Not set", "EXAMPLE TOUR 2099 - 03/05 - SAN FRANCISCO - CA", "https://example.com/tickets/sf")
	tb.click(tMod, "tour:forum:"+id, tForum)
	tb.expect(false, "<#"+tForum+">", "Members can make new threads", "Members cannot reply")

	// A bad template shows the problem, and post refuses.
	tb.submit(tMod, "tourmodal:pt:"+id, map[string]string{"text": "{link} {nope}"})
	tb.expect(false, "not columns: `{nope}`")
	tb.run(tMod, "tour post")
	tb.expect(true, "Fix these problems")
	tb.submit(tMod, "tourmodal:pt:"+id, map[string]string{"text": "Tickets: {link}"})

	// Post: 5 threads, the last show first.
	tb.api.reset()
	tb.api.on("POST", "/channels/"+tForum+"/threads", 200, `{"id":"7001","type":11}`)
	tb.run(tMod, "tour post")
	tb.expect(true, "**5** new threads, **0** changed")
	tb.click(tMod, tb.componentID(tb.last(), "confirm:yes:tourpost:"))
	posts := tb.expectCall(5, "POST", "/channels/"+tForum+"/threads")
	if first := posts[0].JSON(); first["name"] != "EXAMPLE TOUR 2099 - 05/04 - BERLIN - GERMANY" {
		t.Errorf("first thread = %v", first)
	}
	if last := posts[4].JSON(); last["name"] != "EXAMPLE TOUR 2099 - 03/05 - SAN FRANCISCO - CA" ||
		last["message"].(map[string]any)["content"] != "Tickets: https://example.com/tickets/sf" {
		t.Errorf("last thread = %v", last)
	}
	tb.expect(false, "Made 5 threads and updated 0")

	// Nothing to post now.
	tb.run(tMod, "tour post")
	tb.expect(true, "There is nothing to post")

	// Upload again: a new date and a changed link. Post makes one thread and updates one.
	tb.uploadTour("tour,date,venue,city,region,link,support\n" +
		"Example Tour 2099,2099-03-05,The Fillmore,San Francisco,CA,https://example.com/new,Opening Band\n" +
		"Example Tour 2099,2099-03-06,Ace of Spades,Sacramento,CA,https://example.com/sac,\n")
	tb.api.reset()
	tb.api.on("POST", "/channels/"+tForum+"/threads", 200, `{"id":"7002","type":11}`)
	tb.run(tMod, "tour post")
	tb.expect(true, "**1** new threads, **1** changed")
	tb.click(tMod, tb.componentID(tb.last(), "confirm:yes:tourpost:"))
	tb.expectCall(1, "POST", "/channels/"+tForum+"/threads")
	edits := tb.expectCall(1, "PATCH", "/channels/7001")
	if e := edits[0].JSON(); e["archived"] != false || e["name"] != "EXAMPLE TOUR 2099 - 03/05 - SAN FRANCISCO - CA" {
		t.Errorf("thread edit = %v", e)
	}
	if m := tb.expectCall(1, "PATCH", "/channels/7001/messages/7001"); m[0].JSON()["content"] != "Tickets: https://example.com/new" {
		t.Errorf("post edit = %v", m[0].JSON())
	}
}

func TestTourEdit(t *testing.T) {
	tb := newTourBot(t)
	tb.uploadTour(strings.ReplaceAll(tourSampleCSV, "2027", "2099"))
	tour, _ := tb.db.TourByName(tGuild, "Example Tour 2099")
	id := strconv.FormatInt(tour.ID, 10)
	shows, _ := tb.db.TourShows(tour.ID)

	tb.run(tMod, "tour edit")
	r := tb.expect(true, "2099-03-05", "Shows 1–5")
	tb.componentID(r, "tour:page:"+id+":0")
	tb.click(tMod, "tour:page:"+id+":0")
	if got := tb.replies(); got[len(got)-1].Kind != "modal" {
		t.Fatalf("page button answered %+v", got[len(got)-1])
	}

	// Swap two dates, change a city, and remove a show.
	sid := func(i int) string { return "s" + strconv.FormatInt(shows[i].ID, 10) }
	tb.submit(tMod, "tourmodal:shows:"+id, map[string]string{
		sid(0): "date: 2099-03-07\ncity: Fresno",
		sid(1): "date: 2099-03-05\ncity: Portland",
		sid(2): "",
	})
	tb.expect(false, "Fresno")
	after, _ := tb.db.TourShows(tour.ID)
	if len(after) != 4 || after[0].Fields["city"] != "Portland" || after[1].Fields["city"] != "Fresno" || after[1].Date != "2099-03-07" {
		t.Errorf("after = %+v", after)
	}

	// Two shows with the same date are refused, and nothing is saved.
	tb.submit(tMod, "tourmodal:shows:"+id, map[string]string{sid(0): "date: 2099-03-05\ncity: X"})
	tb.expect(true, "Two shows have the date 2099-03-05")

	// Rename; a name that another tour has is refused.
	tb.submit(tMod, "tourmodal:name:"+id, map[string]string{"name": "  Big   Tour "})
	tb.expect(false, "Big Tour")
	tb.uploadTour("tour,date\nOther,2099-01-01\n")
	tb.submit(tMod, "tourmodal:name:"+id, map[string]string{"name": "other"})
	tb.expect(true, "already a tour with that name")

	// With two tours, the commands show a menu. A past tour is not in it.
	tb.uploadTour("tour,date\nOld,2020-01-01\n")
	tb.run(tMod, "tour setup")
	r = tb.expect(true, "Choose a tour")
	tb.componentID(r, "tour:pick:setup")
	if ts, _ := tb.db.ActiveTours(tGuild, tourToday()); len(ts) != 2 {
		t.Errorf("active tours = %+v", ts)
	}

	// Members cannot use the buttons.
	tb.click(tUser, "tour:page:"+id+":0")
	tb.expect(true, "must be a moderator")
}

func TestTourNewForumAndHelp(t *testing.T) {
	tb := newTourBot(t)
	tb.uploadTour("tour,date\nT,2099-01-01\n")
	tour, _ := tb.db.TourByName(tGuild, "T")
	tb.api.on("POST", "/guilds/"+tGuild+"/channels", 200, `{"id":"701","type":15}`)
	tb.submit(tMod, "tourmodal:newforum:"+strconv.FormatInt(tour.ID, 10), map[string]string{"name": "T Tour"})
	c := tb.expectCall(1, "POST", "/guilds/"+tGuild+"/channels")[0].JSON()
	if c["type"] != float64(discordgo.ChannelTypeGuildForum) || c["name"] != "T Tour" {
		t.Errorf("forum = %v", c)
	}
	ows := c["permission_overwrites"].([]any)
	everyone := ows[0].(map[string]any)
	if everyone["id"] != tGuild || everyone["deny"] != strconv.FormatInt(discordgo.PermissionSendMessages|discordgo.PermissionCreatePublicThreads|discordgo.PermissionCreatePrivateThreads, 10) ||
		everyone["allow"] != strconv.FormatInt(discordgo.PermissionSendMessagesInThreads, 10) || len(ows) != 2 {
		t.Errorf("overwrites = %v", ows)
	}
	if tour, _ = tb.db.TourByName(tGuild, "T"); tour.ForumID != "701" {
		t.Errorf("forum ID = %q", tour.ForumID)
	}

	tb.run(tMod, "tour help")
	help := tb.expect(true, "Tour threads", "{date_local}")
	total := len(help.Embeds[0].Description)
	for _, f := range help.Embeds[0].Fields {
		if n := len([]rune(f.Value)); n > 1024 {
			t.Errorf("help field %q has %d characters", f.Name, n)
		}
		total += len(f.Name) + len(f.Value)
	}
	if total > 6000 {
		t.Errorf("help embed has %d characters", total)
	}
	calls := tb.api.find("POST", ".*/callback")
	if len(calls) == 0 || !strings.Contains(string(calls[len(calls)-1].Files["tour-sample.csv"]), "tour,date,venue") {
		t.Errorf("no sample file: %v", tb.callList())
	}
}
