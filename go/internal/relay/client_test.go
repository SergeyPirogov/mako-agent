package relay

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadTokenKeepsOne(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadToken(dir)
	if err != nil || len(a) < 40 {
		t.Fatalf("%q %v", a, err)
	}
	b, _ := LoadToken(dir)
	if a != b {
		t.Fatal("a new token on the second start: the Mac's address would change")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "relay.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("relay.json is %v", fi.Mode().Perm())
	}
	if id := IDFor(a); !validID(id) || id != IDFor(b) || id == IDFor(a+"x") {
		t.Fatalf("id %q", id)
	}
}

func TestAddr(t *testing.T) {
	for env, want := range map[string]string{"": DefaultAddr, "off": "", "https://relay.example.com/": "https://relay.example.com"} {
		t.Setenv("MAKO_RELAY", env)
		if got := Addr(); got != want {
			t.Errorf("MAKO_RELAY=%q: %q, want %q", env, got, want)
		}
	}
}

// Mako on the Mac not answering is a 502 with a reason for that one request; the tunnel stays up.
func TestMakoNotAnswering(t *testing.T) {
	rs, rts, _ := newRelay(t, Limits{})
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	startMac(t, rts.URL, dead.URL, token(1))
	res := phone(t, rts.URL, IDFor(token(1)), "GET", "/", nil)
	if body := read(res); res.StatusCode != http.StatusBadGateway || !strings.Contains(body, "Mako on the Mac does not answer") {
		t.Fatalf("%d %q", res.StatusCode, body)
	}
	if rs.online() != 1 {
		t.Fatal("the tunnel went down with one failed request")
	}
}

// An answer over the limit is a 502 for that request. The limit is the relay's, which it tells in hello, so the
// tunnel is not broken by a frame the relay cannot take.
func TestAnswerOverTheLimitIsOneFailedRequest(t *testing.T) {
	check := func(t *testing.T, rs *Server, rts *httptest.Server, id string) {
		res := phone(t, rts.URL, id, "GET", "/big", nil)
		if body := read(res); res.StatusCode != http.StatusBadGateway || !strings.Contains(body, "too large for the relay") {
			t.Fatalf("%d %q", res.StatusCode, body)
		}
		if res := phone(t, rts.URL, id, "GET", "/echo", nil); res.StatusCode != 200 {
			t.Fatalf("the next request: %d", res.StatusCode)
		}
		if rs.online() != 1 {
			t.Fatal("the tunnel went down")
		}
	}
	t.Run("the relay's limit, lower than the Client's", func(t *testing.T) {
		rs, rts, _ := newRelay(t, Limits{MaxBody: 1024})
		startMac(t, rts.URL, fakeMako(t, nil).URL, token(1)) // the Client's own limit is 16 MB
		check(t, rs, rts, IDFor(token(1)))
	})
	t.Run("the Client's limit", func(t *testing.T) {
		rs, rts, _ := newRelay(t, Limits{})
		runClient(t, &Client{Addr: rts.URL, Token: token(1), Local: fakeMako(t, nil).URL, MaxBody: 1024})
		check(t, rs, rts, IDFor(token(1)))
	})
}

// A WebSocket Mako refuses (no Origin it accepts, no such path) is a 502 to the phone, with the reason.
func TestLocalWebSocketRefused(t *testing.T) {
	_, rts, _ := newRelay(t, Limits{})
	startMac(t, rts.URL, fakeMako(t, nil).URL, token(1))
	req, _ := http.NewRequest("GET", rts.URL+"/nope", nil)
	req.Host = IDFor(token(1)) + "." + domain
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	res, err := phoneTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if body := read(res); res.StatusCode != http.StatusBadGateway || !strings.Contains(body, "refused the connection") {
		t.Fatalf("%d %q", res.StatusCode, body)
	}
}

// A relay that refuses the tunnel is tried again with a pause that doubles up to MaxWait.
func TestReconnectBackoff(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{Addr: srv.URL, Token: token(1), Local: "http://127.0.0.1:1", MinWait: 30 * time.Millisecond, MaxWait: 60 * time.Millisecond, Log: log.New(io.Discard, "", 0)}
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	waitFor(t, "six attempts", func() bool { mu.Lock(); defer mu.Unlock(); return len(at) >= 6 })
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	gap := func(i int) time.Duration { return at[i+1].Sub(at[i]) }
	if gap(0) < 30*time.Millisecond || gap(1) < 60*time.Millisecond {
		t.Errorf("the pause did not double: %v, %v", gap(0), gap(1))
	}
	// Without the cap the fourth pause would be 240 ms.
	if gap(3) > 200*time.Millisecond || gap(4) > 200*time.Millisecond {
		t.Errorf("the pause grew past MaxWait: %v, %v", gap(3), gap(4))
	}
}

func connects(logs *syncWriter) int { return strings.Count(logs.String(), "a Mac connected") }

// When the relay closes the tunnel, the Mac connects again by itself and phones get through.
func TestReconnectsAfterTheRelayClosesTheTunnel(t *testing.T) {
	rs, rts, logs := newRelay(t, Limits{})
	c, _ := runClient(t, &Client{Addr: rts.URL, Token: token(1), Local: fakeMako(t, nil).URL, MinWait: 10 * time.Millisecond})
	rs.mu.Lock()
	ss := rs.sessions[IDFor(token(1))]
	rs.mu.Unlock()
	ss.close()
	waitFor(t, "a second connect", func() bool { return connects(logs) >= 2 && c.URL() != "" })
	if res := phone(t, rts.URL, IDFor(token(1)), "GET", "/echo", nil); res.StatusCode != 200 {
		t.Fatalf("after the reconnect: %d", res.StatusCode)
	}
}

// A tunnel the relay no longer pings is dropped by the Mac, which connects again.
func TestClientDropsASilentTunnel(t *testing.T) {
	_, rts, logs := newRelay(t, Limits{Ping: time.Hour})
	runClient(t, &Client{Addr: rts.URL, Token: token(1), Local: fakeMako(t, nil).URL, Idle: 150 * time.Millisecond, MinWait: 10 * time.Millisecond})
	waitFor(t, "a second connect", func() bool { return connects(logs) >= 2 })
}
