package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

const domain = "relay.test"

// fakeMako: what a Mac's Mako answers, enough to see what reaches it through the relay.
func fakeMako(t *testing.T, slow chan struct{}) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Set-Cookie", "mako_device=x; Path=/")
		json.NewEncoder(w).Encode(map[string]string{
			"method": r.Method, "uri": r.URL.RequestURI(), "body": string(body), "host": r.Host,
			"xfh": r.Header.Get("X-Forwarded-Host"), "xfp": r.Header.Get("X-Forwarded-Proto"),
			"xff": r.Header.Get("X-Forwarded-For"), "cookie": r.Header.Get("Cookie"),
		})
	})
	mux.HandleFunc("/teapot", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "short and stout", http.StatusTeapot) })
	mux.HandleFunc("/go", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/echo", http.StatusFound) })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-slow:
		case <-r.Context().Done():
		}
	})
	mux.Handle("/ws", websocket.Server{
		Handshake: func(c *websocket.Config, r *http.Request) error {
			if r.Header.Get("Origin") == "" || r.Header.Get("X-Forwarded-Host") == "" {
				return fmt.Errorf("no origin")
			}
			return nil
		},
		Handler: func(c *websocket.Conn) {
			for {
				var m wsMsg
				if wsMessage.Receive(c, &m) != nil {
					return
				}
				m.data = append([]byte("echo:"), m.data...)
				if wsMessage.Send(c, m) != nil {
					return
				}
			}
		},
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func newRelay(t *testing.T, l Limits) (*Server, *httptest.Server, *syncWriter) {
	logs := &syncWriter{}
	rs := &Server{Domain: domain, Limits: l, Log: log.New(logs, "", 0)}
	ts := httptest.NewServer(rs)
	t.Cleanup(ts.Close)
	return rs, ts, logs
}

// syncWriter: the relay's log, written from its goroutines and read by the test.
type syncWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(b)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func token(n int) string { return fmt.Sprintf("test-token-of-mac-%04d-xxxxxxxxxxxxxx", n) }

// startMac starts a real Client and waits for its tunnel.
func startMac(t *testing.T, relayURL, local, tok string) (*Client, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{Addr: relayURL, Token: tok, Local: local, Log: log.New(io.Discard, "", 0)}
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	waitFor(t, "the tunnel", func() bool { return c.URL() != "" })
	return c, stop
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("no %s in 5 s", what)
}

// phoneTransport keeps no connection open, so the test sees what the relay itself holds.
var phoneTransport = &http.Transport{DisableKeepAlives: true}

// phone: a request to Mac id's address on the relay.
func phone(t *testing.T, relayURL, id, method, path string, body io.Reader) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, relayURL+path, body)
	req.Host = id + "." + domain
	res, err := phoneTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func read(res *http.Response) string {
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return string(b)
}

// rawMac: a tunnel opened by hand, to play a Mac that misbehaves.
func rawMac(t *testing.T, relayURL, tok string) *websocket.Conn {
	t.Helper()
	cfg, _ := websocket.NewConfig(strings.Replace(relayURL, "http", "ws", 1)+ConnectPath, relayURL)
	cfg.Header = http.Header{"Authorization": {"Bearer " + tok}}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	var hello Frame
	if err := websocket.JSON.Receive(ws, &hello); err != nil || hello.T != FrameHello {
		t.Fatalf("hello: %+v %v", hello, err)
	}
	return ws
}

// connectStatus: what the relay answers a Mac's connect before the WebSocket (400 means it let it through).
func connectStatus(t *testing.T, relayURL, tok string) int {
	req, _ := http.NewRequest("GET", relayURL+ConnectPath, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestRequestsReachTheMac(t *testing.T) {
	_, rts, _ := newRelay(t, Limits{})
	m := fakeMako(t, nil)
	c, _ := startMac(t, rts.URL, m.URL, token(1))
	id := IDFor(token(1))
	if !strings.HasPrefix(c.URL(), "http://"+id+"."+domain+":") {
		t.Fatalf("public address %q", c.URL())
	}

	res := phone(t, rts.URL, id, "POST", "/echo?a=1", strings.NewReader("hi"))
	var got map[string]string
	json.NewDecoder(res.Body).Decode(&got)
	res.Body.Close()
	if res.StatusCode != 200 || got["method"] != "POST" || got["uri"] != "/echo?a=1" || got["body"] != "hi" {
		t.Fatalf("%d %v", res.StatusCode, got)
	}
	// Mako sees a phone's request, never its own Mac's: the forwarded host is the Mac's on the relay.
	if "http://"+got["xfh"] != c.URL() || got["xfp"] != "http" || got["xff"] != "127.0.0.1" {
		t.Fatalf("forwarded headers %v", got)
	}
	if res.Header.Get("Set-Cookie") == "" {
		t.Fatal("the cookie did not come back")
	}
	if res := phone(t, rts.URL, id, "GET", "/teapot", nil); res.StatusCode != http.StatusTeapot || !strings.Contains(read(res), "stout") {
		t.Fatalf("status %d", res.StatusCode)
	}
	// The phone follows a redirect itself: the Mac passes it on.
	if res := phone(t, rts.URL, id, "GET", "/go", nil); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/echo" {
		t.Fatalf("redirect %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestPhoneHeadersCannotPassForTheMac(t *testing.T) {
	tn := &tunnel{host: "abc." + domain, scheme: "https"}
	h := tn.header(http.Header{"X-Forwarded-Host": {"127.0.0.1"}, "Connection": {"close, X-Secret"}, "X-Secret": {"1"}})
	if h.Get("X-Forwarded-Host") != "abc."+domain || h.Get("X-Forwarded-Proto") != "https" || h.Get("X-Forwarded-For") == "" {
		t.Fatalf("%v", h)
	}
	if h.Get("X-Secret") != "" || h.Get("Connection") != "" {
		t.Fatalf("hop-by-hop headers passed: %v", h)
	}
}

func dialPhone(t *testing.T, relayURL, id, path string) (*websocket.Conn, error) {
	cfg, _ := websocket.NewConfig("ws://"+id+"."+domain+path, "http://"+id+"."+domain)
	conn, err := net.Dial("tcp", strings.TrimPrefix(relayURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := websocket.NewClient(cfg, conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	t.Cleanup(func() { ws.Close() })
	return ws, nil
}

func TestWebSocketThroughTheRelay(t *testing.T) {
	rs, rts, _ := newRelay(t, Limits{MaxMessage: 1024})
	m := fakeMako(t, nil)
	startMac(t, rts.URL, m.URL, token(1))
	id := IDFor(token(1))
	ws, err := dialPhone(t, rts.URL, id, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []wsMsg{{data: []byte("text"), text: true}, {data: []byte{0, 1, 2}}} {
		if err := wsMessage.Send(ws, msg); err != nil {
			t.Fatal(err)
		}
		var got wsMsg
		if err := wsMessage.Receive(ws, &got); err != nil {
			t.Fatal(err)
		}
		if string(got.data) != "echo:"+string(msg.data) || got.text != msg.text {
			t.Fatalf("got %q text=%v", got.data, got.text)
		}
	}
	// A message over MaxMessage ends the stream, not the relay.
	wsMessage.Send(ws, wsMsg{data: bytes.Repeat([]byte("x"), 4096), text: true})
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got wsMsg
	if err := wsMessage.Receive(ws, &got); err == nil {
		t.Fatalf("the stream lives on after a message over the limit: %q", got.data)
	}
	waitFor(t, "the stream closed", func() bool { return rs.streams.Load() == 0 })
	if res := phone(t, rts.URL, id, "GET", "/echo", nil); res.StatusCode != 200 {
		t.Fatalf("after: %d", res.StatusCode)
	}
	// A WebSocket the Mac refuses is refused to the phone too.
	if _, err := dialPhone(t, rts.URL, id, "/nope"); err == nil {
		t.Fatal("a refused WebSocket opened")
	}
}

func TestOfflineMacAndWrongAddress(t *testing.T) {
	_, rts, _ := newRelay(t, Limits{})
	res := phone(t, rts.URL, IDFor(token(9)), "GET", "/", nil)
	if body := read(res); res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, OfflineText) {
		t.Fatalf("%d %q", res.StatusCode, body)
	}
	for _, id := range []string{"short", "UPPERCASEWRONG18", "a.b"} {
		if res := phone(t, rts.URL, id, "GET", "/", nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d", id, res.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", rts.URL+"/", nil)
	req.Host = "evil.example"
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusNotFound {
		t.Fatalf("another domain: %d", res.StatusCode)
	}
	if res, _ := http.Get(rts.URL + "/_relay/health"); res.StatusCode != 200 {
		t.Fatalf("health %d", res.StatusCode)
	}
	if got := connectStatus(t, rts.URL, ""); got != http.StatusUnauthorized {
		t.Fatalf("connect without a token: %d", got)
	}
}

func TestTheSameMacReplacesItsTunnel(t *testing.T) {
	rs, rts, _ := newRelay(t, Limits{})
	first := rawMac(t, rts.URL, token(1))
	rawMac(t, rts.URL, token(1))
	first.SetReadDeadline(time.Now().Add(3 * time.Second))
	var f Frame
	if err := websocket.JSON.Receive(first, &f); err == nil {
		t.Fatalf("the old tunnel is still open: %+v", f)
	}
	if rs.online() != 1 {
		t.Fatalf("online %d", rs.online())
	}
}

func TestRateLimitPerMac(t *testing.T) {
	_, rts, _ := newRelay(t, Limits{Rate: 0.001, Burst: 3})
	m := fakeMako(t, nil)
	startMac(t, rts.URL, m.URL, token(1))
	startMac(t, rts.URL, m.URL, token(2))
	for i := range 3 {
		if res := phone(t, rts.URL, IDFor(token(1)), "GET", "/echo", nil); res.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, res.StatusCode)
		}
	}
	res := phone(t, rts.URL, IDFor(token(1)), "GET", "/echo", nil)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("over the rate: %d", res.StatusCode)
	}
	if res := phone(t, rts.URL, IDFor(token(2)), "GET", "/echo", nil); res.StatusCode != 200 {
		t.Fatalf("another Mac: %d", res.StatusCode)
	}
}

func TestMacLimits(t *testing.T) {
	t.Run("per address", func(t *testing.T) {
		_, rts, _ := newRelay(t, Limits{MacsPerIP: 2})
		rawMac(t, rts.URL, token(1))
		rawMac(t, rts.URL, token(2))
		if got := connectStatus(t, rts.URL, token(3)); got != http.StatusTooManyRequests {
			t.Fatalf("a third Mac from one address: %d", got)
		}
	})
	t.Run("in all", func(t *testing.T) {
		_, rts, _ := newRelay(t, Limits{MaxMacs: 1})
		rawMac(t, rts.URL, token(1))
		if got := connectStatus(t, rts.URL, token(2)); got != http.StatusServiceUnavailable {
			t.Fatalf("a Mac over MaxMacs: %d", got)
		}
		rawMac(t, rts.URL, token(1)) // the same Mac comes back
	})
}

func TestPhoneLimits(t *testing.T) {
	slow := make(chan struct{})
	_, rts, _ := newRelay(t, Limits{ConnsPerIP: 2, MaxPending: 2, MaxBody: 1024})
	m := fakeMako(t, slow)
	startMac(t, rts.URL, m.URL, token(1))
	id := IDFor(token(1))

	for _, body := range []io.Reader{bytes.NewReader(make([]byte, 2048)), io.MultiReader(strings.NewReader(strings.Repeat("x", 2048)))} {
		if res := phone(t, rts.URL, id, "POST", "/echo", body); res.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("a body over MaxBody: %d", res.StatusCode)
		}
	}

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { read(phone(t, rts.URL, id, "GET", "/slow", nil)) })
	}
	defer wg.Wait()
	defer close(slow)
	time.Sleep(200 * time.Millisecond)
	if res := phone(t, rts.URL, id, "GET", "/echo", nil); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a third connection from one address: %d", res.StatusCode)
	}
}

func TestBusyMac(t *testing.T) {
	slow := make(chan struct{})
	_, rts, _ := newRelay(t, Limits{MaxPending: 1})
	m := fakeMako(t, slow)
	startMac(t, rts.URL, m.URL, token(1))
	id := IDFor(token(1))
	done := make(chan struct{})
	go func() { read(phone(t, rts.URL, id, "GET", "/slow", nil)); close(done) }()
	time.Sleep(200 * time.Millisecond)
	if res := phone(t, rts.URL, id, "GET", "/echo", nil); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over MaxPending: %d", res.StatusCode)
	}
	close(slow)
	<-done
	if res := phone(t, rts.URL, id, "GET", "/echo", nil); res.StatusCode != 200 {
		t.Fatalf("after: %d", res.StatusCode)
	}
}

func TestMisbehavingMac(t *testing.T) {
	t.Run("frame over the limit", func(t *testing.T) {
		rs, rts, _ := newRelay(t, Limits{MaxBody: 1024})
		ws := rawMac(t, rts.URL, token(1))
		go func() {
			var f Frame
			for websocket.JSON.Receive(ws, &f) == nil {
				if f.T == FrameRequest {
					websocket.JSON.Send(ws, Frame{T: FrameReply, I: f.I, S: 200, B: make([]byte, 200<<10)})
				}
			}
		}()
		if res := phone(t, rts.URL, IDFor(token(1)), "GET", "/", nil); res.StatusCode != http.StatusBadGateway {
			t.Fatalf("waiting when the tunnel broke: %d", res.StatusCode)
		}
		waitFor(t, "the tunnel closed", func() bool { return rs.online() == 0 })
		if res := phone(t, rts.URL, IDFor(token(1)), "GET", "/", nil); res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("after: %d", res.StatusCode)
		}
	})
	t.Run("no answer", func(t *testing.T) {
		_, rts, _ := newRelay(t, Limits{Response: 200 * time.Millisecond})
		rawMac(t, rts.URL, token(1))
		if res := phone(t, rts.URL, IDFor(token(1)), "GET", "/", nil); res.StatusCode != http.StatusGatewayTimeout {
			t.Fatalf("%d", res.StatusCode)
		}
	})
	t.Run("silent tunnel", func(t *testing.T) {
		rs, rts, _ := newRelay(t, Limits{Ping: time.Hour, Idle: 200 * time.Millisecond})
		rawMac(t, rts.URL, token(1))
		waitFor(t, "the silent tunnel closed", func() bool { return rs.online() == 0 })
	})
}

func TestLiveTunnelOutlastsIdle(t *testing.T) {
	rs, rts, _ := newRelay(t, Limits{Ping: 50 * time.Millisecond, Idle: 300 * time.Millisecond})
	startMac(t, rts.URL, fakeMako(t, nil).URL, token(1))
	time.Sleep(time.Second)
	if rs.online() != 1 {
		t.Fatal("the relay closed a tunnel that answers its pings")
	}
}

func TestIdleStreamClosed(t *testing.T) {
	rs, rts, _ := newRelay(t, Limits{StreamIdle: 200 * time.Millisecond})
	startMac(t, rts.URL, fakeMako(t, nil).URL, token(1))
	ws, err := dialPhone(t, rts.URL, IDFor(token(1)), "/ws")
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	var m wsMsg
	if err := wsMessage.Receive(ws, &m); err == nil {
		t.Fatal("got a message instead of the end")
	}
	waitFor(t, "the stream forgotten", func() bool { return rs.streams.Load() == 0 })
}

func TestMetricsAndLogsKeepNoID(t *testing.T) {
	_, rts, logs := newRelay(t, Limits{})
	m := fakeMako(t, nil)
	_, stop := startMac(t, rts.URL, m.URL, token(1))
	id := IDFor(token(1))
	read(phone(t, rts.URL, id, "POST", "/echo", strings.NewReader("12345")))
	read(phone(t, rts.URL, IDFor(token(2)), "GET", "/", nil))
	res, _ := http.Get(rts.URL + "/_relay/metrics")
	out := read(res)
	for _, want := range []string{"relay_macs_online 1", "relay_requests_total 2", `relay_rejected_total{reason="offline"} 1`, "relay_bytes_in_total 5"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	stop()
	waitFor(t, "the disconnect logged", func() bool { return strings.Contains(logs.String(), "disconnected") })
	if strings.Contains(out, id) || strings.Contains(logs.String(), id) || strings.Contains(logs.String(), "/echo") {
		t.Fatalf("an id or a path leaked:\n%s\n%s", out, logs.String())
	}
}

// 100 Macs at once, each with requests in flight; once they are gone the relay holds nothing of them.
func TestManyMacsLeaveNothingBehind(t *testing.T) {
	before := runtime.NumGoroutine()
	rs, rts, _ := newRelay(t, Limits{MacsPerIP: 1000, ConnsPerIP: 1000})
	m := fakeMako(t, nil)
	var stops []context.CancelFunc
	for i := range 100 {
		_, stop := startMac(t, rts.URL, m.URL, token(i))
		stops = append(stops, stop)
	}
	if rs.online() != 100 {
		t.Fatalf("online %d", rs.online())
	}
	var wg sync.WaitGroup
	errs := make(chan string, 1000)
	for i := range 100 {
		for range 5 {
			wg.Go(func() {
				res := phone(t, rts.URL, IDFor(token(i)), "GET", "/echo", nil)
				if body := read(res); res.StatusCode != 200 || !strings.Contains(body, IDFor(token(i))) {
					errs <- fmt.Sprintf("mac %d: %d %s", i, res.StatusCode, body)
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	for _, stop := range stops {
		stop()
	}
	waitFor(t, "the relay empty", func() bool {
		return rs.stats() == "sessions=0 macIPs=0 phoneIPs=0 buckets=0 streams=0"
	})
	http.DefaultClient.CloseIdleConnections()
	waitFor(t, "the goroutines gone", func() bool { return runtime.NumGoroutine() <= before+20 })
}
