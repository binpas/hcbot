package bot

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// A Markov reply can carry a GIF: at MARKOV_GIF_CHANCE, the reply text is
// the GIPHY search, and the first result goes in an embed on the reply.
// The API key (GIPHY_API_KEY in .env) has an hourly limit for the full
// bot, so MARKOV_GIF_COOLDOWN is for the full bot, not each channel. Any
// failure leaves the GIF out.

const (
	giphySearchURL = "https://api.giphy.com/v1/gifs/search"
	giphyTimeout   = 3 * time.Second
	giphyMaxQuery  = 50 // GIPHY accepts at most 50 characters
)

type gifState struct {
	mu   sync.Mutex
	last time.Time // last GIPHY request, for the cooldown
}

// claim starts the cooldown and reports true, or reports false when the
// cooldown is still running.
func (st *gifState) claim(cooldown time.Duration) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.last.IsZero() && time.Since(st.last) < cooldown {
		return false
	}
	st.last = time.Now()
	return true
}

// cooldownLeft is the time until the next GIPHY request is permitted.
func (st *gifState) cooldownLeft(cooldown time.Duration) time.Duration {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.last.IsZero() {
		return 0
	}
	return max(0, cooldown-time.Since(st.last))
}

func (b *Bot) gifCooldown() time.Duration {
	return time.Duration(max(0, b.cfg.Int("MARKOV_GIF_COOLDOWN"))) * time.Second
}

// gifQuery cuts text to GIPHY's limit at the last full word.
func gifQuery(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= giphyMaxQuery {
		return text
	}
	cut := truncate(text, giphyMaxQuery+1)
	if i := strings.LastIndex(cut, " "); i > 0 {
		return cut[:i]
	}
	return truncate(text, giphyMaxQuery)
}

// markovGIF rolls MARKOV_GIF_CHANCE and returns a GIF embed for the reply
// text, or nil.
func (b *Bot) markovGIF(text string) *discordgo.MessageEmbed {
	if b.env.GiphyKey == "" || !percentF(b.cfg.Float("MARKOV_GIF_CHANCE")) {
		return nil
	}
	q := gifQuery(text)
	if q == "" || !b.gif.claim(b.gifCooldown()) {
		return nil
	}
	gif, err := b.searchGIF(q)
	if err != nil {
		log.Printf("markov gif: %v", err)
		return nil
	}
	if gif == "" {
		return nil
	}
	return &discordgo.MessageEmbed{
		Image:  &discordgo.MessageEmbedImage{URL: gif},
		Footer: &discordgo.MessageEmbedFooter{Text: "Powered by GIPHY"},
	}
}

// searchGIF returns the image URL of GIPHY's first result for q, or "".
func (b *Bot) searchGIF(q string) (string, error) {
	params := url.Values{
		"api_key": {b.env.GiphyKey},
		"q":       {q},
		"limit":   {"1"},
		"rating":  {b.cfg.Choice("MARKOV_GIF_RATING")},
		"lang":    {"en"},
	}
	resp, err := b.giphyHTTP.Get(b.giphyURL + "?" + params.Encode())
	if err != nil {
		// The URL holds the API key; keep it out of the log.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GIPHY answered %s", resp.Status)
	}
	type image struct {
		URL string `json:"url"`
	}
	var body struct {
		Data []struct {
			Images struct {
				Downsized image `json:"downsized"`
				Original  image `json:"original"`
			} `json:"images"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("read GIPHY answer: %w", err)
	}
	if len(body.Data) == 0 {
		return "", nil
	}
	// Downsized is under 2 MB; some results have only the original.
	if u := body.Data[0].Images.Downsized.URL; u != "" {
		return u, nil
	}
	return body.Data[0].Images.Original.URL, nil
}
