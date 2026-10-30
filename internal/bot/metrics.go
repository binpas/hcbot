package bot

import (
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metrics, served on METRICS_ADDR (see README.md, "Metrics").
// Labels hold only names of commands, features, and results: never user,
// server, or channel names or IDs.

type botMetrics struct {
	reg *prometheus.Registry

	gatewayConnected prometheus.Gauge
	gatewayEvents    *prometheus.CounterVec // connect, disconnect, resume

	interactions  *prometheus.CounterVec   // kind, name, result
	answerSeconds *prometheus.HistogramVec // kind, name
	handlerSecs   *prometheus.HistogramVec // handler

	apiRequests *prometheus.CounterVec   // host, route, method, status
	apiSeconds  *prometheus.HistogramVec // host, route

	comics       *prometheus.CounterVec // source, result
	comicSeconds prometheus.Observer
	curated      *prometheus.CounterVec // result
	curatedFiles *prometheus.CounterVec // result
	ttsJobs      *prometheus.CounterVec // result
	ttsSeconds   prometheus.Observer
	downloads    *prometheus.CounterVec // result
	downloadSize prometheus.Observer
	markovReply  *prometheus.CounterVec // kind
	markovSeen   *prometheus.CounterVec // result: queued, no_words, blocked, queue_full
	markovLearn  *prometheus.CounterVec // what: messages, words, errors
	markovWords  prometheus.Gauge
	markovSteps  prometheus.Gauge
	rankSeconds  prometheus.Observer
	triggers     prometheus.Counter
	reminders    *prometheus.CounterVec // result: set, limit, sent, failed, deleted
	autoreacts   prometheus.Counter
	backups      *prometheus.CounterVec // result
	lastBackup   prometheus.Gauge
	nickLookups  *prometheus.CounterVec // result
}

func newBotMetrics(b *Bot) *botMetrics {
	reg := prometheus.NewRegistry()
	f := func(c prometheus.Collector) { reg.MustRegister(c) }
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		v := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hcbot_" + name, Help: help}, labels)
		f(v)
		return v
	}
	hist := func(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
		v := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "hcbot_" + name, Help: help, Buckets: buckets}, labels)
		f(v)
		return v
	}
	gaugeFunc := func(name, help string, fn func() float64) {
		f(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "hcbot_" + name, Help: help}, fn))
	}
	fast := []float64{.05, .1, .25, .5, 1, 2, 3, 5, 10, 30}
	slow := []float64{1, 5, 10, 20, 30, 45, 60, 90, 120, 300}

	m := &botMetrics{reg: reg}
	f(collectors.NewGoCollector())
	f(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	info := prometheus.NewGauge(prometheus.GaugeOpts{Name: "hcbot_build_info", Help: "The running build (always 1).",
		ConstLabels: prometheus.Labels{"version": Version}})
	info.Set(1)
	f(info)

	// Health and connection.
	gaugeFunc("gateway_latency_seconds", "The last heartbeat round trip to Discord.",
		func() float64 { return b.s.HeartbeatLatency().Seconds() })
	m.gatewayConnected = prometheus.NewGauge(prometheus.GaugeOpts{Name: "hcbot_gateway_connected", Help: "1 when the gateway is connected."})
	f(m.gatewayConnected)
	m.gatewayEvents = counter("gateway_events_total", "Gateway connects, disconnects, and resumes.", "event")

	// Speed.
	m.interactions = counter("interactions_total", "Interactions, by kind, name, and result (answered, no_answer, panic).", "kind", "name", "result")
	m.answerSeconds = hist("interaction_answer_seconds", "The time until the first answer to an interaction.", fast, "kind", "name")
	m.handlerSecs = hist("handler_seconds", "The time of the message and reaction handlers.", fast, "handler")

	// The Discord API.
	m.apiRequests = counter("discord_requests_total", "HTTP requests to Discord, by host (api, cdn, other), route, method, and status.",
		"host", "route", "method", "status")
	m.apiSeconds = hist("discord_request_seconds", "The time of HTTP requests to Discord.", fast, "host", "route")

	// Features.
	m.comics = counter("comics_total", "Comics, by source (slash, mention, preview, post) and result.", "source", "result")
	m.comicSeconds = hist("comic_seconds", "The time to read, download, and draw a comic.", fast).WithLabelValues()
	m.curated = counter("curated_total", "Curated posts, by result.", "result")
	m.curatedFiles = counter("curated_files_total", "Files of curated posts, by result (copied, too_large, failed).", "result")
	m.ttsJobs = counter("tts_jobs_total", "Text to speech jobs, by result.", "result")
	m.ttsSeconds = hist("tts_job_seconds", "The time from a /say request to the posted clip.", slow).WithLabelValues()
	m.downloads = counter("downloads_total", "/download requests, by result.", "result")
	m.downloadSize = hist("download_bytes", "The size of the files that /download posted.",
		prometheus.ExponentialBuckets(256<<10, 2, 10)).WithLabelValues()
	m.markovReply = counter("markov_replies_total", "Markov chat replies, by kind (mention, chance).", "kind")
	m.markovSeen = counter("markov_messages_total", "Messages in the learn channels, by result (queued, no_words, blocked, queue_full).", "result")
	m.markovLearn = counter("markov_learned_total", "What the chain learned: messages and words written to markov.db, and failed writes (errors).", "what")
	m.markovWords = prometheus.NewGauge(prometheus.GaugeOpts{Name: "hcbot_markov_chain_words", Help: "Different words in the chain."})
	m.markovSteps = prometheus.NewGauge(prometheus.GaugeOpts{Name: "hcbot_markov_chain_transitions", Help: "Word steps (transitions) in the chain."})
	f(m.markovWords)
	f(m.markovSteps)
	gaugeFunc("markov_pending_messages", "Messages waiting to be written to markov.db.", func() float64 {
		b.mk.mu.Lock()
		defer b.mk.mu.Unlock()
		return float64(len(b.mk.pending))
	})
	m.rankSeconds = hist("markov_rank_seconds", "The time of a rank request to the worker.", fast).WithLabelValues()
	m.triggers = counter("triggers_total", "Triggers that answered.").WithLabelValues()
	m.autoreacts = counter("autoreacts_total", "Autoreact emotes that the bot added.").WithLabelValues()
	m.reminders = counter("reminders_total", "Reminders set, refused at the limit, sent, failed, and deleted.", "result")

	// Data and caches.
	fileSize := func(path string) func() float64 {
		return func() float64 {
			if st, err := os.Stat(path); err == nil {
				return float64(st.Size())
			}
			return 0
		}
	}
	gaugeFunc("db_bytes", "The size of hcbot.db.", fileSize(b.db.Path))
	if b.chain != nil {
		gaugeFunc("markov_db_bytes", "The size of markov.db.", fileSize(b.chain.Path))
	}
	m.backups = counter("backups_total", "Database backups, by result.", "result")
	m.lastBackup = prometheus.NewGauge(prometheus.GaugeOpts{Name: "hcbot_last_backup_timestamp_seconds",
		Help: "When the last backup worked (0: none since the start)."})
	f(m.lastBackup)
	gaugeFunc("nick_cache_entries", "Member nicknames in the cache.", func() float64 { return float64(b.nicks.size()) })
	m.nickLookups = counter("nick_lookups_total", "Nickname lookups, by result (cached, fetched, left, error).", "result")
	gaugeFunc("message_cache_entries", "Messages in the audit log cache.", func() float64 { return float64(b.msgs.size()) })
	gaugeFunc("media_cache_bytes", "The size of the media in the audit log cache.", func() float64 { return float64(b.media.bytes()) })
	return m
}

// serveMetrics serves /metrics on addr until the bot closes. It returns
// the address that it listens on.
func (b *Bot) serveMetrics(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(b.m.reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics: %v", err)
		}
	}()
	go func() {
		<-b.stop
		_ = srv.Close()
	}()
	log.Printf("Metrics on %s/metrics", ln.Addr())
	return ln.Addr().String(), nil
}

func (b *Bot) onConnect(_ *discordgo.Session, _ *discordgo.Connect) {
	b.m.gatewayConnected.Set(1)
	b.m.gatewayEvents.WithLabelValues("connect").Inc()
}

func (b *Bot) onDisconnect(_ *discordgo.Session, _ *discordgo.Disconnect) {
	b.m.gatewayConnected.Set(0)
	b.m.gatewayEvents.WithLabelValues("disconnect").Inc()
}

func (b *Bot) onResumed(_ *discordgo.Session, _ *discordgo.Resumed) {
	b.m.gatewayConnected.Set(1)
	b.m.gatewayEvents.WithLabelValues("resume").Inc()
}

// observeHandler notes the time of an event handler (with defer).
func (b *Bot) observeHandler(handler string, start time.Time) {
	b.m.handlerSecs.WithLabelValues(handler).Observe(time.Since(start).Seconds())
}

// metricsTransport counts and times the bot's HTTP requests: to Discord's
// API, its CDN, and other hosts (the files that curation copies).
type metricsTransport struct {
	next http.RoundTripper
	m    *botMetrics
}

func (t metricsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	host, route := apiRoute(req)
	status := "error"
	if err == nil {
		status = strconv.Itoa(resp.StatusCode)
	}
	t.m.apiRequests.WithLabelValues(host, route, req.Method, status).Inc()
	t.m.apiSeconds.WithLabelValues(host, route).Observe(time.Since(start).Seconds())
	return resp, err
}

var apiVersionRE = regexp.MustCompile(`^/api/v\d+`)

// apiRoute is the host kind and the path of a request, with IDs, tokens,
// and emoji taken out, so that the labels stay few.
func apiRoute(req *http.Request) (host, route string) {
	switch h := req.URL.Hostname(); {
	case h == "discord.com" || strings.HasSuffix(h, ".discord.com"):
		host = "api"
	case h == "cdn.discordapp.com" || h == "media.discordapp.net":
		return "cdn", "/" + strings.SplitN(strings.TrimPrefix(req.URL.Path, "/"), "/", 2)[0]
	default:
		return "other", "other"
	}
	parts := strings.Split(strings.Trim(apiVersionRE.ReplaceAllString(req.URL.Path, ""), "/"), "/")
	for i, p := range parts {
		switch {
		case i > 0 && parts[i-1] == "reactions":
			parts[i] = ":emoji"
		case isDigits(p):
			parts[i] = ":id"
		case len(p) > 40 || (i >= 2 && parts[i-1] == ":id" && (parts[i-2] == "interactions" || parts[i-2] == "webhooks")):
			parts[i] = ":token"
		}
	}
	return host, "/" + strings.Join(parts, "/")
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
