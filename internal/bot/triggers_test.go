package bot

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestParseTriggerValue(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		unknown []string
	}{
		{"  plain text  ", []string{"plain text"}, nil},
		{"{rand:a, b ,c}", []string{"a", "b", "c"}, nil},
		{"hi {random:x~y} there", []string{"hi x there", "hi y there"}, nil},
		{"{RAND:a,b}", []string{"a", "b"}, nil},
		{"{rand(|):a,b|c}", []string{"a,b", "c"}, nil},
		{"{rand:solo}", []string{"solo"}, nil},
		{"{rand:,}", []string{"{rand:,}"}, nil},
		{"{user} says {rand:a,b}", []string{"{user} says a", "{user} says b"}, []string{"{user}"}},
		{"{args:1} and {user}", []string{"{args:1} and {user}"}, []string{"{args:1}", "{user}"}},
	}
	for _, c := range cases {
		p := parseTriggerValue(c.in)
		if p.Ambiguous || !slices.Equal(p.Values, c.want) || !slices.Equal(p.Unknown, c.unknown) {
			t.Errorf("parseTriggerValue(%q) = %q unknown %q ambiguous %v; want %q unknown %q",
				c.in, p.Values, p.Unknown, p.Ambiguous, c.want, c.unknown)
		}
	}
	p := parseTriggerValue("pre {rand:a,b~c} post")
	if !p.Ambiguous || p.Before != "pre " || p.Options != "a,b~c" || p.After != " post" {
		t.Fatalf("ambiguous parse = %+v", p)
	}
	if got := p.splitOptions("~"); !slices.Equal(got, []string{"pre a,b post", "pre c post"}) {
		t.Errorf("split on ~ = %q", got)
	}
}

func TestNormaliseAndEmoji(t *testing.T) {
	if got := normalise("  Hello \t Big\nWorld "); got != "hellobigworld" {
		t.Errorf("normalise = %q", got)
	}
	for in, want := range map[string]string{"👍": "👍", "<:blob:123>": "blob:123", "<a:dance:456>": "dance:456", " x ": "x"} {
		if got := reactionEmoji(in); got != want {
			t.Errorf("reactionEmoji(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTriggerCommands(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "trigger list")
	tb.expect(true, "No triggers are defined yet")

	tb.run(tMod, "trigger add", strOpt("name", "Hello"), strOpt("value", "hi there"))
	tb.expect(true, "Trigger `Hello` saved with a single response")
	tb.run(tMod, "trigger addvalue", strOpt("name", "hello"), strOpt("value", "hey!"))
	tb.expect(true, "now has **2** possible responses")
	tb.run(tMod, "trigger addvalue", strOpt("name", "solo"), strOpt("value", "x"))
	tb.expect(true, "created with 1 response")

	tb.run(tMod, "trigger list")
	tb.expect(true, "**2** trigger(s)", "`hello` 🔀×2", "`solo`")
	tb.run(tMod, "trigger info", strOpt("name", "HELLO"))
	tb.expect(true, "🔀 Trigger: HELLO", "hi there", "hey!")

	tb.run(tMod, "trigger removevalue", strOpt("name", "hello"), strOpt("value", "nope"))
	tb.expect(true, "No matching response")
	tb.run(tMod, "trigger removevalue", strOpt("name", "hello"), strOpt("value", "hey!"))
	tb.expect(true, "**1** response(s) remain")

	tb.run(tMod, "trigger add", strOpt("name", "hello"), strOpt("value", "only this"))
	if v, _ := tb.db.TriggerValues("hello"); !slices.Equal(v, []string{"only this"}) {
		t.Errorf("add did not replace: %q", v)
	}
	tb.run(tMod, "trigger delete", strOpt("name", "hello"))
	tb.run(tMod, "trigger info", strOpt("name", "hello"))
	tb.expect(true, "No trigger named `hello`")

	tb.run(tUser, "trigger add", strOpt("name", "x"), strOpt("value", "y"))
	tb.expect(true, "must be a moderator")
}

func TestTriggerFires(t *testing.T) {
	tb := newTestBot(t)
	for _, v := range []string{"one", "two"} {
		if _, err := tb.db.AddTriggerValue("hello", v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tb.db.AddTriggerValue("help", "a trigger named like a command"); err != nil {
		t.Fatal(err)
	}

	tb.say(tUser, tChGeneral, "HELLO world", true)
	sent := tb.expectCall(1, "POST", "/channels/"+tChGeneral+"/messages")
	if len(sent) != 1 {
		t.FailNow()
	}
	var body struct {
		Content   string `json:"content"`
		Reference struct {
			MessageID string `json:"message_id"`
		} `json:"message_reference"`
		Allowed struct {
			Parse       []string `json:"parse"`
			RepliedUser bool     `json:"replied_user"`
		} `json:"allowed_mentions"`
	}
	_ = json.Unmarshal(sent[0].Body, &body)
	if body.Content != "one" && body.Content != "two" {
		t.Errorf("content = %q", body.Content)
	}
	if body.Reference.MessageID == "" {
		t.Error("not sent as a reply")
	}
	if !slices.Equal(body.Allowed.Parse, []string{"users"}) || !body.Allowed.RepliedUser {
		t.Errorf("allowed mentions = %+v; @everyone, @here and roles must not ping", body.Allowed)
	}

	tb.api.reset()
	tb.say(tUser, tChGeneral, "help", true)    // command names win
	tb.say(tUser, tChGeneral, "unknown", true) // no such trigger
	tb.say(tUser, tChGeneral, "", true)        // mention only
	tb.say(tUser, tChGeneral, "hello", false)  // no mention
	tb.say(tBot, tChGeneral, "hello", true)    // bots are ignored
	tb.env.SetupMode = true
	tb.say(tUser, tChGeneral, "hello", true)
	tb.env.SetupMode = false
	if calls := tb.callList(); len(calls) != 0 {
		t.Errorf("unexpected calls: %v", calls)
	}
}

func TestTriggerImportModal(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "trigger import")
	if r := tb.replies(); len(r) != 1 || r[0].Kind != "modal" {
		t.Fatalf("no modal: %+v", r)
	}

	tb.submit(tMod, "trigimport", map[string]string{"name": " My Trig ", "value": "{rand:a,b} {user}"})
	tb.expect(true, "Trigger `mytrig` saved with **2** response(s)", "Picks one at random", "`{user}`")
	if v, _ := tb.db.TriggerValues("mytrig"); len(v) != 2 {
		t.Errorf("values = %q", v)
	}
	tb.submit(tMod, "trigimport", map[string]string{"name": "mytrig", "value": "c"})
	tb.expect(true, "had **2** response(s) — now has **3** after adding **1** more")

	tb.submit(tMod, "trigimport", map[string]string{"name": "   ", "value": "x"})
	tb.expect(true, "can't be empty")

	tb.submit(tMod, "trigimport", map[string]string{"name": "amb", "value": "{rand:a,b~c}"})
	r := tb.expect(true, "Which separator?")
	tilde := tb.componentID(r, "trigsep:tilde:")
	comma := tb.componentID(r, "trigsep:comma:")

	tb.click(tUser, tilde)
	tb.expect(true, "This isn't your import")

	tb.click(tMod, tilde)
	r = tb.expect(true, "Trigger `amb` saved with **2** response(s)")
	if r.Kind != "update" || !r.Cleared {
		t.Errorf("separator answer = %+v", r)
	}
	if v, _ := tb.db.TriggerValues("amb"); !slices.Equal(v, []string{"a,b", "c"}) {
		t.Errorf("values = %q", v)
	}

	// The data is used once; a second click finds nothing.
	tb.click(tMod, comma)
	tb.expect(true, "Timed out")

	tb.submit(tMod, "trigimport", map[string]string{"name": "amb2", "value": "{rand:a,b~c}"})
	none := tb.componentID(tb.last(), "trigsep:none:")
	parts := strings.Split(none, ":")
	parts[3] = "1" // expired
	tb.click(tMod, strings.Join(parts, ":"))
	r = tb.expect(true, "Timed out")
	if len(r.Components) != 3 || !r.Components[0].Disabled {
		t.Errorf("expired buttons = %+v", r.Components)
	}
	tb.click(tMod, none)
	if v, _ := tb.db.TriggerValues("amb2"); !slices.Equal(v, []string{"{rand:a,b~c}"}) {
		t.Errorf("don't split = %q", v)
	}
}

func TestTriggerExportAndBatchImport(t *testing.T) {
	tb := newTestBot(t)
	tb.run(tMod, "trigger export")
	tb.expect(true, "No triggers are defined yet")

	for _, kv := range [][2]string{{"a", "x <b>&"}, {"a", "y"}, {"b", "z"}} {
		if _, err := tb.db.AddTriggerValue(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	tb.run(tMod, "trigger export")
	tb.expect(true, "Exported **2** trigger(s) with **3** total response(s)")
	calls := tb.api.find("POST", "/interactions/.*/callback")
	file := calls[len(calls)-1].Files["triggers-export.json"]
	var exported []triggerExport
	if err := json.Unmarshal(file, &exported); err != nil || len(exported) != 2 || exported[0].Name != "a" {
		t.Fatalf("export = %s, %v", file, err)
	}
	if !strings.Contains(string(file), "x <b>&") {
		t.Error("export escapes HTML characters")
	}

	// Import into a fresh bot: one value already exists, one entry is bad.
	tb2 := newTestBot(t)
	if _, err := tb2.db.AddTriggerValue("a", "y"); err != nil {
		t.Fatal(err)
	}
	content := strings.TrimSuffix(string(file), "]\n") + `, {"name": "", "values": ["q"]}]`
	tb2.run(tMod, "trigger batchimport", tb2.attachmentOpt("file", "t.json", content))
	r := tb2.expect(true, "Found **2** new response(s) across **2** trigger(s)", "**1** response(s) already exist",
		"**1** entry skipped (malformed)")
	yes := tb2.componentID(r, "confirm:yes:trigbatch:")
	tb2.click(tMod, yes)
	tb2.expect(true, "Imported **2** new response(s) across **2** trigger(s). Skipped **1**")
	if v, _ := tb2.db.TriggerValues("a"); len(v) != 2 {
		t.Errorf("a = %q", v)
	}

	tb2.run(tMod, "trigger batchimport", tb2.attachmentOpt("file", "t.json", content))
	tb2.expect(true, "Every response in that file already exists")
	tb2.run(tMod, "trigger batchimport", tb2.attachmentOpt("file", "bad.json", "{nope"))
	tb2.expect(true, "isn't valid JSON")
	tb2.run(tMod, "trigger batchimport", tb2.attachmentOpt("file", "obj.json", `{"name":"a"}`))
	tb2.expect(true, "Expected a JSON list")

	tb2.config(map[string]string{"TRIGGER_IMPORT_MAX_MB": "1"})
	tb2.run(tMod, "trigger batchimport", tb2.attachmentOpt("file", "big.json", strings.Repeat(" ", 1<<20+1)))
	tb2.expect(true, "larger than **1 MB**")
	tb2.expectCall(0, "GET", "/attachments/1/.*/big.json")
}

func TestAutoreacts(t *testing.T) {
	tb := newTestBot(t, withMessageContent)
	tb.run(tMod, "autoreact list")
	tb.expect(true, "No autoreacts saved yet")

	tb.run(tMod, "autoreact add", strOpt("emote", "🍕"), strOpt("phrase", "Pizza Time"))
	tb.expect(false, "🍕 will now be added whenever someone says **Pizza Time**")
	tb.expectCall(1, "PUT", "/channels/500/messages/8000/reactions/🍕/@me")
	tb.run(tMod, "autoreact add", strOpt("emote", "🍕"), strOpt("phrase", "pizzatime"))
	tb.expect(true, "already exists")

	tb.run(tMod, "autoreact edit", strOpt("emote", "<:blob:123>"), strOpt("phrase", "pizza time"))
	tb.expectCall(1, "PUT", "/channels/500/messages/8000/reactions/blob:123/@me")
	tb.run(tMod, "autoreact edit", strOpt("emote", "🍕"), strOpt("phrase", "nothing"))
	tb.expect(true, "No autoreact exists for **nothing**")

	tb.api.fail("PUT", "/channels/500/messages/8000/reactions/.*")
	tb.run(tMod, "autoreact add", strOpt("emote", "notanemote"), strOpt("phrase", "bad"))
	if edit := tb.api.find("PATCH", "/webhooks/.*/messages/@original"); len(edit) != 1 ||
		!strings.Contains(string(edit[0].Body), "doesn't look like an emote") {
		t.Errorf("bad emote not reported: %v", edit)
	}
	if ok, _ := tb.db.AutoreactExists(tGuild, "bad"); ok {
		t.Error("bad emote saved")
	}

	tb.run(tMod, "autoreact list")
	tb.expect(true, "Autoreacts (1)", "<:blob:123> — **pizzatime**")

	tb.api.reset()
	tb.say(tUser, tChGeneral, "is it PIZZA   time yet", false)
	tb.expectCall(1, "PUT", "/channels/"+tChGeneral+"/messages/[0-9]+/reactions/blob:123/@me")

	tb.api.reset()
	tb.config(map[string]string{"ON_THE_REAL": tChSens})
	tb.say(tUser, tChSens, "pizza time", false)
	tb.env.MessageContent = false
	tb.say(tUser, tChGeneral, "pizza time", false)
	tb.expectCall(0, "PUT", ".*")

	tb.run(tMod, "autoreact remove", strOpt("phrase", "Pizza Time"))
	tb.expect(true, "Removed the autoreact")
	tb.run(tMod, "autoreact remove", strOpt("phrase", "Pizza Time"))
	tb.expect(true, "No autoreact exists")
}

func TestAutoreactsNeedMessageContent(t *testing.T) {
	off, on := newTestBot(t), newTestBot(t, withMessageContent)
	has := func(tb *testBot) bool {
		for _, d := range tb.commandDefs() {
			if d.Name == "autoreact" {
				return true
			}
		}
		return false
	}
	if has(off) || !has(on) {
		t.Errorf("/autoreact registered: off=%v on=%v", has(off), has(on))
	}
	off.run(tMod, "help")
	if strings.Contains(off.last().text(), "/autoreact") {
		t.Error("/help lists /autoreact without the option")
	}
	on.run(tMod, "help")
	if !strings.Contains(on.last().text(), "/autoreact") {
		t.Error("/help misses /autoreact with the option")
	}
}
