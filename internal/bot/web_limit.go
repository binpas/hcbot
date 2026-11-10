package bot

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// The login is the only part of the web interface that works with no
// session, and each callback makes a request to Discord. These limits keep
// a flood away from Discord: too many refused requests make Discord block
// the bot's address.

const (
	webLoginBurst = 10              // login requests for each address, at one time
	webLoginEvery = 6 * time.Second // then one more for each of these
	webTokenBurst = 20              // token requests to Discord, for all addresses
	webTokenEvery = 3 * time.Second
	webLimitKeys  = 10000 // the most addresses that a limiter keeps
)

type webBucket struct {
	tokens float64
	at     time.Time
	warned bool
}

// webLimiter is a token bucket for each key.
type webLimiter struct {
	mu      sync.Mutex
	burst   float64
	every   time.Duration
	buckets map[string]*webBucket
}

func newWebLimiter(burst int, every time.Duration) *webLimiter {
	return &webLimiter{burst: float64(burst), every: every, buckets: map[string]*webBucket{}}
}

// allow takes one token of a key. first is true for the first refusal after
// an allowed request, so that a flood makes one log line and not thousands.
func (l *webLimiter) allow(key string) (ok, first bool) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bk := l.buckets[key]
	if bk == nil {
		if len(l.buckets) >= webLimitKeys {
			for k, old := range l.buckets { // remove the full buckets
				if now.Sub(old.at) >= time.Duration(l.burst)*l.every {
					delete(l.buckets, k)
				}
			}
		}
		if len(l.buckets) >= webLimitKeys {
			key = "" // too many addresses at one time: they share a bucket
		}
		if bk = l.buckets[key]; bk == nil {
			bk = &webBucket{tokens: l.burst, at: now}
			l.buckets[key] = bk
		}
	}
	bk.tokens = min(l.burst, bk.tokens+float64(now.Sub(bk.at))/float64(l.every))
	bk.at = now
	if bk.tokens < 1 {
		first = !bk.warned
		bk.warned = true
		return false, first
	}
	bk.tokens--
	bk.warned = false
	return true, false
}

// webClientIP is the address of the browser. The bot is behind a proxy, so
// the address of the connection is the proxy's. The proxy puts the real
// address last in WEB_IP_HEADER; values before it can come from the client.
func (b *Bot) webClientIP(r *http.Request) string {
	if h := b.env.WebIPHeader; h != "" {
		if vals := r.Header.Values(h); len(vals) > 0 {
			parts := strings.Split(vals[len(vals)-1], ",")
			if addr, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil {
				return addr.Unmap().String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap().String()
	}
	return host
}

// webLimitKey is the key of an address in a limiter. One IPv6 customer
// has a /64 network, so the limit is for the network.
func webLimitKey(ip string) string {
	if addr, err := netip.ParseAddr(ip); err == nil && addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return ip
}
