package bot

import (
	"slices"
	"strings"
)

// personaQuestion changes a message to the persona into a question that
// goes to the persona directly, for the ranker only:
//
//	can you ask johnbot how he feels about dolphins → how do you feel about dolphins?
//	tell johnbot he is wrong                          → you are wrong
//	johnbot, do you like tacos                        → do you like tacos
//
// words are the message's tokens (markov.Tokenize: lower case, with the
// punctuation); name is the persona's name. Text that no rule matches
// comes back with the name removed. The result can be "".
func personaQuestion(words []string, name string) string {
	nameWords := strings.Fields(strings.ToLower(name))
	at := findName(words, nameWords)
	if len(nameWords) == 0 || at < 0 {
		return joinWords(possessiveName(words, nameWords))
	}
	before, after := words[:at], words[at+len(nameWords):]
	// The punctuation on the name ("johnbot,") stays with the text before.
	if p := trailingPunct(words[at+len(nameWords)-1]); p != "" && len(before) > 0 {
		before = append(slices.Clone(before[:len(before)-1]), before[len(before)-1]+p)
	}

	if n := len(before); n > 0 {
		switch core(before[n-1]) {
		case "ask":
			return askForm(after)
		case "tell":
			return tellForm(after)
		}
	}
	// No "ask" or "tell": the message already talks to the persona.
	if onlyGreeting(before) {
		return joinWords(after)
	}
	if last := before[len(before)-1]; (len(after) == 0 || onlyPunct(after)) && strings.ContainsAny(trailingPunct(last), ",:;") {
		// "how are you, johnbot?" → "how are you?"
		out := slices.Clone(before)
		out[len(out)-1] = strings.TrimRight(last, punct) + strings.Trim(trailingPunct(last), ",:;")
		return joinWords(append(out, after...))
	}
	return joinWords(slices.Concat(before, []string{"you"}, after))
}

// askForm: the words after "ask <name>".
func askForm(rest []string) string {
	if len(rest) > 0 && core(rest[0]) == "to" {
		return question(slices.Concat([]string{"can", "you"}, secondPerson(rest[1:])))
	}
	rest = secondPerson(rest)
	if len(rest) == 0 {
		return ""
	}
	switch first := core(rest[0]); {
	case first == "about":
		return question(slices.Concat([]string{"what", "do", "you", "think"}, rest))
	case first == "if" || first == "whether":
		return question(invert(rest[1:]))
	case whWords[first]:
		// "how", "what album", "how much": up to 3 words, then "you".
		for j := 1; j < len(rest) && j <= 3; j++ {
			if core(rest[j]) == "you" {
				return question(slices.Concat(rest[:j], invert(rest[j:])))
			}
		}
		// "what your favourite album is" → "what is your favourite album".
		if n := len(rest); n > 2 && beVerbs[core(rest[n-1])] {
			return question(slices.Concat(rest[:1], []string{core(rest[n-1])}, rest[1:n-1]))
		}
	}
	return question(rest)
}

// tellForm: the words after "tell <name>".
func tellForm(rest []string) string {
	if len(rest) > 0 && (core(rest[0]) == "that" || core(rest[0]) == "to") {
		rest = rest[1:]
	}
	return joinWords(secondPerson(rest))
}

// invert makes a direct question from "you <verb> ...": "you are" →
// "are you", "you feel" → "do you feel", "you felt" → "did you feel".
func invert(w []string) []string {
	if len(w) < 2 || core(w[0]) != "you" {
		return w
	}
	verb := core(w[1])
	switch {
	case auxVerbs[verb]:
		return slices.Concat([]string{verb, "you"}, w[2:])
	case pastVerbs[verb] != "":
		return slices.Concat([]string{"did", "you", pastVerbs[verb] + trailingPunct(w[1])}, w[2:])
	}
	return slices.Concat([]string{"do"}, w)
}

// secondPerson changes he, she, they (and their other forms) into you,
// and makes the verb after them agree.
func secondPerson(w []string) []string {
	var out []string
	for i := 0; i < len(w); i++ {
		c, p := core(w[i]), trailingPunct(w[i])
		switch c {
		case "he", "she", "they":
			out = append(out, "you"+p)
			if p == "" {
				i = agree(w, i+1, &out)
			}
			continue
		case "he's", "she's", "they're":
			if i+1 < len(w) && (core(w[i+1]) == "been" || core(w[i+1]) == "got") {
				out = append(out, "you", "have"+p)
			} else {
				out = append(out, "you", "are"+p)
			}
			continue
		case "her":
			// "her band" → "your band"; "ask her about" → "you about".
			if p == "" && i+1 < len(w) && !notNoun[core(w[i+1])] {
				out = append(out, "your")
			} else {
				out = append(out, "you"+p)
			}
			continue
		}
		if r, ok := pronouns[c]; ok {
			out = append(out, r+p)
			continue
		}
		out = append(out, w[i])
	}
	return out
}

// agree adds w[i] (after an adverb or two) in the form for "you", and
// returns the index of the last word it used.
func agree(w []string, i int, out *[]string) int {
	for k := 0; k < 2 && i < len(w) && isAdverb(core(w[i])); k++ {
		*out = append(*out, w[i])
		i++
	}
	if i >= len(w) {
		return i - 1
	}
	*out = append(*out, youVerb(core(w[i]))+trailingPunct(w[i]))
	return i
}

// youVerb is the verb for "you": is → are, likes → like, watches → watch.
func youVerb(v string) string {
	if r, ok := thirdPerson[v]; ok {
		return r
	}
	if len(v) <= 3 || !strings.HasSuffix(v, "s") || strings.HasSuffix(v, "ss") || notVerbS[v] {
		return v
	}
	switch {
	case strings.HasSuffix(v, "ies"):
		return strings.TrimSuffix(v, "ies") + "y"
	case strings.HasSuffix(v, "ches"), strings.HasSuffix(v, "shes"), strings.HasSuffix(v, "xes"),
		strings.HasSuffix(v, "sses"), strings.HasSuffix(v, "oes"):
		return strings.TrimSuffix(v, "es")
	}
	return strings.TrimSuffix(v, "s")
}

var (
	beVerbs  = map[string]bool{"is": true, "are": true, "was": true, "were": true}
	whWords  = map[string]bool{"how": true, "what": true, "why": true, "where": true, "when": true, "who": true, "which": true, "whom": true}
	auxVerbs = map[string]bool{"are": true, "were": true, "can": true, "could": true, "will": true, "would": true,
		"should": true, "have": true, "had": true, "might": true, "must": true, "do": true, "did": true}
	pastVerbs = map[string]string{"felt": "feel", "thought": "think", "said": "say", "went": "go", "saw": "see",
		"got": "get", "made": "make", "knew": "know", "heard": "hear", "liked": "like", "loved": "love",
		"hated": "hate", "wanted": "want", "played": "play", "listened": "listen", "watched": "watch"}
	thirdPerson = map[string]string{"is": "are", "was": "were", "has": "have", "does": "do", "isn't": "aren't",
		"wasn't": "weren't", "hasn't": "haven't", "doesn't": "don't", "goes": "go"}
	pronouns = map[string]string{"him": "you", "them": "you", "his": "your", "their": "your", "hers": "yours",
		"theirs": "yours", "himself": "yourself", "herself": "yourself", "themselves": "yourself",
		"themself": "yourself", "he'd": "you'd", "she'd": "you'd", "they'd": "you'd", "he'll": "you'll",
		"she'll": "you'll", "they'll": "you'll", "they've": "you've"}
	// Words that end in "s" but are not verbs for "he".
	notVerbS = map[string]bool{"always": true, "sometimes": true, "perhaps": true, "nowadays": true,
		"afterwards": true, "besides": true, "plus": true, "unless": true, "yes": true, "this": true, "thus": true}
	// Words after "her" that show that "her" is not "her <noun>".
	notNoun = map[string]bool{"about": true, "to": true, "for": true, "if": true, "whether": true, "why": true,
		"how": true, "what": true, "and": true, "or": true, "that": true, "with": true, "in": true, "on": true, "at": true}
	adverbs   = map[string]bool{"really": true, "always": true, "never": true, "still": true, "also": true, "just": true, "even": true, "ever": true}
	greetings = map[string]bool{"hey": true, "hi": true, "hello": true, "yo": true, "ok": true, "okay": true, "so": true, "oh": true, "dear": true}
)

func isAdverb(w string) bool { return adverbs[w] || (len(w) > 4 && strings.HasSuffix(w, "ly")) }

// findName returns the index of the name's words in words, or -1.
func findName(words, name []string) int {
	if len(name) == 0 {
		return -1
	}
	for i := 0; i+len(name) <= len(words); i++ {
		ok := true
		for j, n := range name {
			if core(words[i+j]) != n {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// possessiveName changes "<name>'s" into "your" ("what is johnbot's
// favourite album" → "what is your favourite album").
func possessiveName(words, name []string) []string {
	if len(name) == 0 {
		return words
	}
	last := name[len(name)-1] + "'s"
	full := append(slices.Clone(name[:len(name)-1]), last)
	if at := findName(words, full); at >= 0 {
		return slices.Concat(words[:at], []string{"your" + trailingPunct(words[at+len(full)-1])}, words[at+len(full):])
	}
	return words
}

func onlyGreeting(w []string) bool {
	for _, x := range w {
		if c := core(x); c != "" && !greetings[c] {
			return false
		}
	}
	return true
}

func onlyPunct(w []string) bool {
	for _, x := range w {
		if core(x) != "" {
			return false
		}
	}
	return true
}

const punct = ".,!?:;\"()"

// core is a word without its punctuation.
func core(w string) string { return strings.Trim(w, punct) }

// trailingPunct is the punctuation at the end of a word.
func trailingPunct(w string) string { return w[len(strings.TrimRight(w, punct)):] }

// question joins the words and ends them with one "?".
func question(w []string) string {
	s := strings.TrimRight(joinWords(w), punct+" ")
	if s == "" {
		return ""
	}
	return s + "?"
}

// joinWords joins the words, without punctuation at the start.
func joinWords(w []string) string {
	return strings.TrimLeft(strings.Join(w, " "), punct+" ")
}
