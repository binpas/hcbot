package bot

import (
	"testing"

	"github.com/binpas/hcbot/internal/markov"
)

func TestPersonaQuestion(t *testing.T) {
	for in, want := range map[string]string{
		"can you ask johnbot how he feels about dolphins":   "how do you feel about dolphins?",
		"ask johnbot what his favourite album is":           "what is your favourite album?",
		"johnbot, do you like tacos":                        "do you like tacos",
		"hey JohnBot, how is the guitar?":                   "how is the guitar?",
		"tell johnbot he is wrong about the new album":      "you are wrong about the new album",
		"tell johnbot that he always watches too much tv":   "you always watch too much tv",
		"ask johnbot if he likes the new record":            "do you like the new record?",
		"could you please ask johnbot whether he is ok":     "are you ok?",
		"ask johnbot about the tour":                        "what do you think about the tour?",
		"ask johnbot to sing a song":                        "can you sing a song?",
		"ask johnbot why he felt sad":                       "why did you feel sad?",
		"ask johnbot what her band is called":               "what your band is called?",
		"ask johnbot how he's been":                         "how have you been?",
		"how are you, johnbot?":                             "how are you?",
		"i love johnbot":                                    "i love you",
		"what does johnbot think of this":                   "what does you think of this",
		"what is johnbot's favourite song":                  "what is your favourite song",
		"nothing about the persona here":                    "nothing about the persona here",
		"ask johnbot":                                       "",
		"johnbot":                                           "",
		"ask johnbot what they think of the band they like": "what do you think of the band you like?",
	} {
		if got := personaQuestion(markov.Tokenize(in), "JohnBot"); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
	if got := personaQuestion(markov.Tokenize("ask target bot how he is"), "Target Bot"); got != "how are you?" {
		t.Errorf("two-word name: %q", got)
	}
}
