package markov

import (
	"bufio"
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

var (
	// Mentions and URLs are never learned: a reply must not ping anyone or
	// post links.
	mentionRE = regexp.MustCompile(`^<(@[!&]?|#)\d+>$`)
	urlRE     = regexp.MustCompile(`(?i)^(https?://|www\.)`)
	emojiRE   = regexp.MustCompile(`^<a?:\w+:\d+>$`)
)

// maxTokenLen is the longest word the chain keeps, in bytes.
const maxTokenLen = 64

// Tokenize splits a message into the words the chain learns. Words are
// lowercased (custom emojis keep their case). Mentions, @everyone, @here,
// URLs and very long words are left out, and a leading @ is removed
// ("@justin" becomes "justin"), so a reply never looks like a mention.
func Tokenize(text string) []string {
	var out []string
	for _, f := range strings.Fields(text) {
		switch {
		case len(f) > maxTokenLen, mentionRE.MatchString(f), urlRE.MatchString(f):
			continue
		case emojiRE.MatchString(f):
			out = append(out, f)
			continue
		}
		lower := strings.ToLower(f)
		if strings.Contains(lower, "@everyone") || strings.Contains(lower, "@here") {
			continue
		}
		if lower = strings.TrimLeft(lower, "@"); lower != "" {
			out = append(out, lower)
		}
	}
	return out
}

// Normalize is the form used to match a word against the blocklist,
// /markov forget and /markov lookup: lowercase, without punctuation and
// symbols at either end ("Cat!" and "(cat)" become "cat"). A colon,
// semicolon or equals sign at the start stays, so emoticons such as ":3"
// and ":D" do not become "3" and "d". A word made only of symbols, such as
// an emoji or ":)", loses only the sentence punctuation at its end ("🍆!"
// becomes "🍆").
func Normalize(word string) string {
	lower := strings.ToLower(word)
	edge := func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }
	core := strings.TrimRightFunc(strings.TrimLeftFunc(lower, func(r rune) bool {
		return edge(r) && r != ':' && r != ';' && r != '='
	}), edge)
	if letters(core) > 0 {
		return core
	}
	if t := strings.TrimRight(lower, ".,!?"); t != "" {
		return t
	}
	return lower
}

// letters counts the letters and numbers in s.
func letters(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			n++
		}
	}
	return n
}

// MinPrefix is the fewest letters a prefix ("word*") needs, so that a
// mistake such as "a*" cannot match almost every word.
const MinPrefix = 3

// prefix returns the start a pattern word matches ("retard*" gives
// "retard"), or false when the pattern is a whole word. A "*" after only
// symbols (":*") is an emoticon, not a prefix.
func prefix(pattern string) (string, bool) {
	p, ok := strings.CutSuffix(pattern, "*")
	return p, ok && letters(p) > 0
}

// wordMatches reports whether a normalized word matches a pattern word
// from Phrase: the same word, or for a prefix, any word that starts with it.
func wordMatches(pattern, word string) bool {
	if p, ok := prefix(pattern); ok {
		return strings.HasPrefix(word, p)
	}
	return pattern == word
}

// Phrase splits text into normalized words, for the blocklist, forget and
// lookup. A word that ends with "*" stays a prefix ("Retard*" becomes
// "retard*").
func Phrase(text string) []string {
	var out []string
	for _, w := range Tokenize(text) {
		if base := strings.TrimRight(w, "*"); base != w {
			if n := Normalize(base); letters(n) > 0 {
				out = append(out, n+"*")
				continue
			}
		}
		if n := Normalize(w); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ShortPrefix returns the first prefix in phrase with fewer than MinPrefix
// letters, or "".
func ShortPrefix(phrase []string) string {
	for _, w := range phrase {
		if p, ok := prefix(w); ok && letters(p) < MinPrefix {
			return w
		}
	}
	return ""
}

// Blocklist is a set of blocked words and phrases.
type Blocklist struct {
	phrases [][]string
}

// NewBlocklist builds a blocklist from stored entries.
func NewBlocklist(entries []string) *Blocklist {
	bl := &Blocklist{}
	for _, e := range entries {
		if p := Phrase(e); len(p) > 0 {
			bl.phrases = append(bl.phrases, p)
		}
	}
	return bl
}

// Len is the number of entries.
func (bl *Blocklist) Len() int {
	if bl == nil {
		return 0
	}
	return len(bl.phrases)
}

// Blocks reports whether words contain a blocked word or phrase. Words
// match whole, after Normalize, or by their start for a prefix ("word*").
func (bl *Blocklist) Blocks(words []string) bool {
	if bl.Len() == 0 {
		return false
	}
	norm := make([]string, len(words))
	for i, w := range words {
		norm[i] = Normalize(w)
	}
	for _, p := range bl.phrases {
		for i := 0; i+len(p) <= len(norm); i++ {
			match := true
			for j, w := range p {
				if !wordMatches(w, norm[i+j]) {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

// ParseImport reads training text. A .json file is a DiscordChatExporter
// export (bot messages are skipped); anything else is plain text with one
// message on each line.
func ParseImport(filename string, data []byte) ([]string, error) {
	data = bytes.ToValidUTF8(data, nil)
	if strings.HasSuffix(strings.ToLower(filename), ".json") {
		var export struct {
			Messages []struct {
				Content string `json:"content"`
				Author  struct {
					IsBot bool `json:"isBot"`
				} `json:"author"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(data, &export); err != nil {
			return nil, err
		}
		var out []string
		for _, m := range export.Messages {
			if !m.Author.IsBot && strings.TrimSpace(m.Content) != "" {
				out = append(out, m.Content)
			}
		}
		return out, nil
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

// Prepare tokenizes messages for Learn and drops the empty and blocked
// ones. It returns the kept messages and how many were blocked.
func Prepare(messages []string, bl *Blocklist) (kept [][]string, blocked int) {
	for _, m := range messages {
		words := Tokenize(m)
		switch {
		case len(words) == 0:
		case bl.Blocks(words):
			blocked++
		default:
			kept = append(kept, words)
		}
	}
	return kept, blocked
}
