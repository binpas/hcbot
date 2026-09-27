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

// Normalize is the form used to match a word against the blocklist and
// /markov forget: lowercase, without punctuation at either end.
func Normalize(word string) string {
	return strings.TrimFunc(strings.ToLower(word), func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSymbol(r)
	})
}

// Phrase splits text into normalized words, for the blocklist and forget.
func Phrase(text string) []string {
	var out []string
	for _, w := range Tokenize(text) {
		if n := Normalize(w); n != "" {
			out = append(out, n)
		}
	}
	return out
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
// match whole, after Normalize.
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
				if norm[i+j] != w {
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
