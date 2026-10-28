package comic

import (
	"math"
	"strings"
	"unicode"
)

// The emotion wheel of Comic Chat: eight emotions, 45° apart. Neutral is
// happy with no intensity. A gesture code (above 2π) selects a torso.
const (
	emHappy  = 0 * math.Pi / 4
	emCoy    = 1 * math.Pi / 4
	emBored  = 2 * math.Pi / 4
	emScared = 3 * math.Pi / 4
	emSad    = 4 * math.Pi / 4
	emAngry  = 5 * math.Pi / 4
	emShout  = 6 * math.Pi / 4
	emLaugh  = 7 * math.Pi / 4

	gestWave       = 1001
	gestPointOther = 1002
	gestPointSelf  = 1003
	gestShrug      = 1005
)

// mood is the face emotion and the gesture for one message.
type mood struct {
	face      float64
	intensity float64 // 0 is neutral
	gesture   float64 // 0 is none
}

// A rule finds one emotion or gesture in a message. The strongest face
// rule and the strongest gesture rule win. The rules follow Comic Chat's
// (textpose.cpp and chat.rc in Microsoft's source), with chat words and
// emoji of today.
type rule struct {
	kind     ruleKind
	arg      string
	emotion  float64
	strength int
}

type ruleKind int

const (
	ruleWord  ruleKind = iota // a whole word or phrase, any case
	ruleFind                  // anywhere, any case (emoticons and emoji)
	ruleStart                 // the first word of the message, any case
	ruleCaps                  // all capitals (two or more letters)
)

func words(em float64, strength int, ws ...string) []rule {
	out := make([]rule, len(ws))
	for i, w := range ws {
		out[i] = rule{ruleWord, w, em, strength}
	}
	return out
}

func finds(em float64, strength int, fs ...string) []rule {
	out := make([]rule, len(fs))
	for i, f := range fs {
		out[i] = rule{ruleFind, f, em, strength}
	}
	return out
}

func starts(em float64, strength int, ss ...string) []rule {
	out := make([]rule, len(ss))
	for i, s := range ss {
		out[i] = rule{ruleStart, s, em, strength}
	}
	return out
}

var rules = func() []rule {
	var r []rule
	add := func(rs ...[]rule) {
		for _, x := range rs {
			r = append(r, x...)
		}
	}
	add(
		[]rule{{ruleCaps, "", emShout, 9}},
		finds(emShout, 9, "!!"),
		words(emLaugh, 11, "lol", "lmao", "lmfao", "rofl", "rotfl", "lul", "kek", "xd", "haha", "hahaha", "ahaha", "hehe", "heh"),
		finds(emLaugh, 11, "😂", "🤣", "😆", "😹", "💀"),
		finds(emHappy, 10, ":)", ":-)", ":d", "=)", "(:", "<3", "😀", "😃", "😄", "😁", "😊", "🙂", "😍", "🥰", "❤", "🥳", "👍"),
		words(emHappy, 8, "yay", "nice", "great", "awesome", "cool", "love", "thanks", "thank you", "ty", "good"),
		finds(emCoy, 10, ";)", ";-)", "😉", "😏", "😘", "😜"),
		words(emCoy, 8, "uwu", "owo", "hmm", "hmmm", "maybe", "perhaps"),
		finds(emBored, 9, "🙄", "😴", "😑", "😐", "🥱", "zzz"),
		words(emBored, 7, "meh", "sigh", "boring", "bored", "whatever", "ok", "k", "kk", "okay"),
		finds(emScared, 10, "😱", "😨", "😰", "😳", "😬"),
		words(emScared, 9, "omg", "yikes", "eek", "oh no", "scary", "scared", "help", "afraid"),
		finds(emSad, 10, ":(", ":-(", ":'(", "😢", "😭", "😞", "😔", "☹", "🙁", "💔"),
		words(emSad, 9, "sad", "sorry", "rip", "unfortunately", "sadly", "cry", "crying"),
		finds(emAngry, 10, ">:(", "😠", "😡", "🤬", "👿"),
		words(emAngry, 9, "grr", "angry", "mad", "hate", "wtf", "stfu", "damn", "dammit", "fuck", "fucking", "shit", "ugh"),

		starts(gestWave, 5, "hi", "hey", "hello", "heya", "hiya", "yo", "bye", "goodbye", "cya", "welcome", "howdy", "gn", "gm", "morning", "goodnight"),
		finds(gestWave, 5, "👋"),
		starts(gestPointOther, 4, "you", "u"),
		words(gestPointOther, 8, "are you", "will you", "did you", "aren't you", "don't you", "do you", "can you", "you're", "youre"),
		starts(gestPointSelf, 3, "i", "me"),
		words(gestPointSelf, 7, "i'm", "im", "i am", "i will", "i'll", "i've", "me too"),
		words(gestShrug, 9, "idk", "dunno", "no idea", "who knows"),
		finds(gestShrug, 9, "🤷", "¯\\_(ツ)_/¯"),
	)
	return r
}()

// moodOf finds the face emotion and the gesture of a message.
func moodOf(text string) mood {
	lower := strings.ToLower(text)
	first := firstWord(lower)
	var m mood
	faceStrength, gestStrength := 0, 0
	for _, r := range rules {
		var hit bool
		switch r.kind {
		case ruleWord:
			hit = hasWord(lower, r.arg)
		case ruleFind:
			hit = strings.Contains(lower, r.arg)
		case ruleStart:
			hit = first == r.arg
		case ruleCaps:
			hit = allCaps(text)
		}
		if !hit {
			continue
		}
		if r.emotion > 2*math.Pi {
			if r.strength > gestStrength {
				m.gesture, gestStrength = r.emotion, r.strength
			}
		} else if r.strength > faceStrength {
			m.face, faceStrength = r.emotion, r.strength
		}
	}
	if faceStrength > 0 {
		m.intensity = 0.6
		if strings.Contains(text, "!") || allCaps(text) {
			m.intensity = 1
		}
	}
	return m
}

// hasWord reports whether w is in s as a whole word (or phrase): not
// inside a longer word.
func hasWord(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(w)
		if (j == 0 || !isWordRune(lastRune(s[:j]))) && (end == len(s) || !isWordRune(firstRune(s[end:]))) {
			return true
		}
		i = j + 1
	}
}

func firstWord(s string) string {
	s = strings.TrimLeftFunc(s, func(r rune) bool { return !isWordRune(r) })
	end := strings.IndexFunc(s, func(r rune) bool { return !isWordRune(r) && r != '\'' })
	if end < 0 {
		return s
	}
	return s[:end]
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

func lastRune(s string) rune {
	rs := []rune(s)
	if len(rs) == 0 {
		return 0
	}
	return rs[len(rs)-1]
}

// allCaps reports whether the text has two or more capital letters and no
// small ones.
func allCaps(s string) bool {
	upper := 0
	for _, r := range s {
		if unicode.IsLower(r) {
			return false
		}
		if unicode.IsUpper(r) {
			upper++
		}
	}
	return upper > 1
}
