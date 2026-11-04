package bot

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binpas/hcbot/internal/config"
)

// freezeProxy forwards TCP connections, and can make the open ones stop
// without closing them, as when the internet goes down.
type freezeProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []*atomic.Bool
}

func (p *freezeProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		dead := &atomic.Bool{}
		p.mu.Lock()
		p.conns = append(p.conns, dead)
		p.mu.Unlock()
		pipe := func(dst, src net.Conn) {
			buf := make([]byte, 32<<10)
			for {
				n, err := src.Read(buf)
				if err != nil {
					return
				}
				for dead.Load() { // a black hole: no data and no close
					time.Sleep(50 * time.Millisecond)
				}
				if _, err := dst.Write(buf[:n]); err != nil {
					return
				}
			}
		}
		go pipe(up, c)
		go pipe(c, up)
	}
}

func (p *freezeProxy) freezeOpen() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.conns {
		d.Store(true)
	}
}

func TestHTTP2DeadConnectionIsReplaced(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	proxy := &freezeProxy{ln: ln, target: srv.Listener.Addr().String()}
	go proxy.serve()

	tr := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, // the test server
		// The same mechanism as http2Health, with short times.
		HTTP2: &http.HTTP2Config{SendPingTimeout: 200 * time.Millisecond, PingTimeout: 200 * time.Millisecond},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	url := "https://" + ln.Addr().String()

	get := func() (int, error) {
		resp, err := client.Get(url)
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.ProtoMajor, nil
	}
	if major, err := get(); err != nil || major != 2 {
		t.Fatalf("first request: HTTP/%d, %v", major, err)
	}
	proxy.freezeOpen()
	// The ping finds the dead connection, so a new request makes a new one.
	deadline := time.Now().Add(5 * time.Second)
	for {
		major, err := get()
		if err == nil && major == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no request worked after the connection died: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestBotUsesHTTP2Health(t *testing.T) {
	_ = newTestBot(t, func(*config.Env) {})
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok || tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout <= 0 || tr.HTTP2.PingTimeout <= 0 {
		t.Fatalf("the default transport has no HTTP/2 health check: %+v", tr.HTTP2)
	}
	if tr.HTTP2.SendPingTimeout+tr.HTTP2.PingTimeout > 30*time.Second {
		t.Errorf("the check is too slow: %v + %v", tr.HTTP2.SendPingTimeout, tr.HTTP2.PingTimeout)
	}
}
