package markov

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "markov.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func learn(t *testing.T, s *Store, lines ...string) {
	t.Helper()
	kept, _ := Prepare(lines, nil)
	if err := s.Learn(kept); err != nil {
		t.Fatal(err)
	}
}

func TestTokenize(t *testing.T) {
	got := Tokenize("Hello <@123> <@&4> <#5> @everyone https://x.io www.y.com <:Pog:99> World! @Justin @@ @")
	want := []string{"hello", "<:Pog:99>", "world!", "justin"}
	if !slices.Equal(got, want) {
		t.Fatalf("Tokenize = %q, want %q", got, want)
	}
}

func TestGenerateFollowsChain(t *testing.T) {
	s := openTest(t)
	if out, err := s.Generate(nil, nil); err != nil || out != "" {
		t.Fatalf("empty chain: %q, %v", out, err)
	}
	learn(t, s, "the cat sat on the mat")
	out, err := s.Generate(nil, nil)
	if err != nil || out != "the cat sat on the mat" {
		t.Fatalf("Generate = %q, %v", out, err)
	}
	out, _ = s.Generate([]string{"zzz", "sat"}, nil)
	if out != "sat on the mat" {
		t.Fatalf("seeded Generate = %q", out)
	}
	if out, _ := s.Generate(nil, func([]string) bool { return false }); out != "" {
		t.Fatalf("refused Generate = %q", out)
	}
}

func TestCountsAndStats(t *testing.T) {
	s := openTest(t)
	learn(t, s, "a b c", "a b c", "a b d")
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	// (0,0)->a (0,a)->b (a,b)->c (a,b)->d (b,c)->0 (b,d)->0
	if st.Words != 4 || st.Transitions != 6 || st.Bytes == 0 {
		t.Fatalf("Stats = %+v", st)
	}
	n, err := s.Prune()
	if err != nil || n != 2 { // (a,b)->d and (b,d)->0
		t.Fatalf("Prune = %d, %v", n, err)
	}
	st, _ = s.Stats()
	if st.Words != 3 || st.Transitions != 4 {
		t.Fatalf("after Prune: %+v", st)
	}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stats(); st.Words != 0 || st.Transitions != 0 {
		t.Fatalf("after Reset: %+v", st)
	}
}

func TestForget(t *testing.T) {
	s := openTest(t)
	learn(t, s, "i like Cats a lot", "cats, again", "we like dogs")

	n, err := s.CountPhrase(Phrase("CATS"))
	if err != nil || n == 0 {
		t.Fatalf("CountPhrase(cats) = %d, %v", n, err)
	}
	if got, _ := s.Forget(Phrase("cats")); got != n {
		t.Fatalf("Forget removed %d, counted %d", got, n)
	}
	if n, _ := s.CountPhrase(Phrase("cats")); n != 0 {
		t.Fatalf("cats left: %d", n)
	}
	// "like dogs": removes (we,like)->dogs and (like,dogs)->end only.
	if n, _ := s.Forget(Phrase("like dogs")); n != 2 {
		t.Fatalf("Forget(like dogs) = %d", n)
	}
	if n, _ := s.Forget(Phrase("unknown words")); n != 0 {
		t.Fatalf("Forget(unknown) = %d", n)
	}
	// Three words: only the step inside the phrase.
	learn(t, s, "one two three four")
	if n, _ := s.Forget(Phrase("two three four")); n != 1 {
		t.Fatalf("Forget(two three four) = %d", n)
	}
}

func TestBlocklist(t *testing.T) {
	bl := NewBlocklist([]string{"Badword", "very bad phrase", "  "})
	if bl.Len() != 2 {
		t.Fatalf("Len = %d", bl.Len())
	}
	cases := map[string]bool{
		"this has BADWORD! in it":    true,
		"badwords are fine":          false,
		"a very bad phrase here":     true,
		"very bad, but not a phrase": false,
		"nothing":                    false,
	}
	for text, want := range cases {
		if got := bl.Blocks(Tokenize(text)); got != want {
			t.Errorf("Blocks(%q) = %v, want %v", text, got, want)
		}
	}
	kept, blocked := Prepare([]string{"ok words", "badword", "", "https://only.link"}, bl)
	if len(kept) != 1 || blocked != 1 {
		t.Fatalf("Prepare = %q, %d blocked", kept, blocked)
	}
}

func TestParseImport(t *testing.T) {
	lines, err := ParseImport("train.txt", []byte("first line\n\n  second line  \r\n"))
	if err != nil || !slices.Equal(lines, []string{"first line", "second line"}) {
		t.Fatalf("text = %q, %v", lines, err)
	}
	export := `{"messages":[{"content":"hi there","author":{"isBot":false}},
		{"content":"beep","author":{"isBot":true}},{"content":"","author":{}}]}`
	lines, err = ParseImport("Export.JSON", []byte(export))
	if err != nil || !slices.Equal(lines, []string{"hi there"}) {
		t.Fatalf("json = %q, %v", lines, err)
	}
	if _, err := ParseImport("x.json", []byte("not json")); err == nil {
		t.Fatal("bad json: want an error")
	}
	if lines, _ := ParseImport("x.txt", []byte("bad \xff byte")); !strings.Contains(lines[0], "bad") {
		t.Fatalf("invalid UTF-8 = %q", lines)
	}
}
