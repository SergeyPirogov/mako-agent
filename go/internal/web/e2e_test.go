package web

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
	"github.com/SergeyPirogov/mako-agent/go/internal/relay"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

// fakePhone is the phone's service worker, in Go: paired end to end, it talks to Mako only in sealed frames.
type fakePhone struct {
	t     *testing.T
	srv   *httptest.Server
	id    string
	token string // the device's token on the Mac, which the phone was never given
	keys  relay.Keys
	mac   string
	out   *relay.Sealer
	in    *relay.Opener
	c     *websocket.Conn
}

func e2eServer(t *testing.T) (*httptest.Server, *phone.Phones, *dispatch.Dispatcher, string) {
	t.Helper()
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	dir := t.TempDir()
	phones, err := phone.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{D: d, Phones: phones}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		// A tunnel's handler outlives the server's Close and writes phones.json as it ends: wait, or the temporary
		// directory is removed under it.
		for range 200 {
			s.tunMu.Lock()
			n := len(s.tunnels)
			s.tunMu.Unlock()
			if n == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return srv, phones, d, dir
}

func pairE2E(t *testing.T, srv *httptest.Server, phones *phone.Phones, dir string) *fakePhone {
	t.Helper()
	code, secret := phones.NewPairingKey()
	raw, _ := base64.RawURLEncoding.DecodeString(secret)
	k, _ := relay.DeriveKeys(raw, []byte(code))
	mac := relay.MacID(srv.URL)
	nonce := make([]byte, 16)
	rand.Read(nonce)
	f, _ := relay.SealPair(k, mac, code, relay.PairRequest{Nonce: nonce})
	body, _ := json.Marshal(f)
	req, _ := http.NewRequest("POST", srv.URL+"/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL) // as the service worker's fetch carries it
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("pair: %v %v", err, res.StatusCode)
	}
	var sealed relay.Sealed
	json.NewDecoder(res.Body).Decode(&sealed)
	reply, err := relay.OpenPairReply(k, mac, code, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var kept struct{ Devices []phone.Device }
	data, _ := os.ReadFile(filepath.Join(dir, "phones.json"))
	json.Unmarshal(data, &kept)
	p := &fakePhone{t: t, srv: srv, id: reply.ID, keys: k, mac: mac}
	for _, d := range kept.Devices {
		if d.ID == reply.ID {
			p.token = d.Token
		}
	}
	p.out, _ = relay.NewSealer(k.P2M, relay.P2M, mac, reply.ID, 0, nil)
	p.in, _ = relay.NewOpener(k.M2P, relay.M2P, mac, reply.ID, 0, nil)
	p.dial()
	return p
}

func (p *fakePhone) dial() {
	p.t.Helper()
	c, err := websocket.Dial("ws"+strings.TrimPrefix(p.srv.URL, "http")+"/e2e?d="+url.QueryEscape(p.id), "", p.srv.URL)
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() { c.Close() })
	p.c = c
}

func (p *fakePhone) send(typ, id string, v any) relay.Sealed {
	p.t.Helper()
	b, _ := json.Marshal(v)
	f, err := p.out.Seal(typ, id, b)
	if err != nil {
		p.t.Fatal(err)
	}
	p.sendFrame(f)
	return f
}

func (p *fakePhone) sendFrame(f relay.Sealed) {
	p.t.Helper()
	if err := websocket.JSON.Send(p.c, f); err != nil {
		p.t.Fatal(err)
	}
}

// next: the next frame from the Mac, opened; ok false when none came in time.
func (p *fakePhone) next(wait time.Duration) (relay.Sealed, []byte, bool) {
	p.c.SetReadDeadline(time.Now().Add(wait))
	var f relay.Sealed
	if err := websocket.JSON.Receive(p.c, &f); err != nil {
		return f, nil, false
	}
	b, err := p.in.Open(f)
	if err != nil {
		p.t.Fatalf("a frame from the Mac does not open: %v", err)
	}
	return f, b, true
}

func (p *fakePhone) do(req relay.Request) relay.Response {
	p.t.Helper()
	id := rand.Text()
	p.send(relay.TypeRequest, id, req)
	for {
		f, b, ok := p.next(5 * time.Second)
		if !ok {
			p.t.Fatalf("no answer to %s %s", req.M, req.U)
		}
		if f.T == relay.TypeResponse && f.I == id {
			var res relay.Response
			json.Unmarshal(b, &res)
			return res
		}
	}
}

// A phone paired end to end gets the board through the tunnel: as itself, remote, without a cookie, and with the
// Mac-only pages closed as before.
func TestTunnelServesTheDevice(t *testing.T) {
	srv, phones, _, dir := e2eServer(t)
	p := pairE2E(t, srv, phones, dir)

	res := p.do(relay.Request{U: "/api/state"})
	var st struct{ Remote, E2E bool }
	json.Unmarshal(res.B, &st)
	if res.S != 200 || !st.Remote || !st.E2E {
		t.Fatalf("state through the tunnel: %d %s", res.S, res.B)
	}
	// The board's own look (X-Mako) counts as seen through the tunnel too.
	if res := p.do(relay.Request{U: "/api/state?seen=1", H: map[string]string{makoHeader: "1"}}); res.S != 200 {
		t.Fatalf("seen through the tunnel: %d", res.S)
	}
	if res := p.do(relay.Request{U: "/", Nav: true}); res.S != 200 || !bytes.Contains(res.B, []byte("function makoSocket")) {
		t.Fatalf("the board: %d", res.S)
	}
	for _, u := range []string{"/settings", "/api/settings", "/api/phone"} {
		if res := p.do(relay.Request{U: u}); res.S != http.StatusForbidden {
			t.Errorf("%s from the phone: %d", u, res.S)
		}
	}
	for _, req := range []relay.Request{
		{U: "//evil.example/api/state"},
		{U: "http://evil.example/api/state"},
		{U: "/ws/term?id=a"},
		{U: "/e2e"},
	} {
		if res := p.do(req); res.S != http.StatusBadRequest {
			t.Errorf("%s: %d", req.U, res.S)
		}
	}
	if res := p.do(relay.Request{M: "CONNECT", U: "/api/state"}); res.S != http.StatusMethodNotAllowed {
		t.Errorf("CONNECT: %d", res.S)
	}

	// Pushes subscribe through the tunnel; a navigation (another site's form) cannot.
	sub := []byte(`{"endpoint":"https://push.example/1","keys":{"p256dh":"x","auth":"y"}}`)
	h := map[string]string{"Content-Type": "application/json", "Cookie": "mako_device=" + p.token, makoHeader: "1"}
	if res := p.do(relay.Request{M: "POST", U: "/api/push/subscribe", H: h, B: sub, Nav: true}); res.S == 200 {
		t.Fatal("a navigation from another site subscribed")
	}
	if res := p.do(relay.Request{M: "POST", U: "/api/push/subscribe", H: h, B: sub}); res.S != 200 {
		t.Fatalf("subscribe: %d %s", res.S, res.B)
	}
	if d := phones.Devices(); len(d) != 1 || d[0].Push == nil || d[0].Push.Endpoint != "https://push.example/1" {
		t.Fatalf("no subscription kept: %+v", d)
	}

	// A cookie in an answer does not reach the phone: the old pairing sets one.
	code, _ := phones.NewPairingKey()
	res = p.do(relay.Request{M: "POST", U: "/pair", H: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, B: []byte("code=" + code)})
	if res.S != 200 || res.H["Set-Cookie"] != "" {
		t.Fatalf("an answer's cookie: %d %v", res.S, res.H)
	}
}

// What the relay replays, changes or makes gets no answer, and the tunnel goes on.
func TestTunnelRefusesReplayAndForgery(t *testing.T) {
	srv, phones, _, dir := e2eServer(t)
	p := pairE2E(t, srv, phones, dir)
	f := p.send(relay.TypeRequest, "first", relay.Request{U: "/api/i18n"})
	if g, _, ok := p.next(5 * time.Second); !ok || g.I != "first" {
		t.Fatal("no answer to the first request")
	}
	p.sendFrame(f) // again
	bad := f
	bad.N, bad.C = f.N+5, bytes.Clone(f.C)
	bad.C[3] ^= 1
	p.sendFrame(bad)
	if g, _, ok := p.next(500 * time.Millisecond); ok {
		t.Fatalf("answered a replayed or forged frame: %+v", g)
	}
	// The forged counter did not burn the numbers: the next real frame opens.
	if res := p.do(relay.Request{U: "/api/i18n"}); res.S != 200 {
		t.Fatalf("after a forgery: %d", res.S)
	}
	// A removed device's tunnel closes.
	phones.Remove(p.id)
	p.send(relay.TypeRequest, "gone", relay.Request{U: "/api/i18n"})
	if _, _, ok := p.next(2 * time.Second); ok {
		t.Fatal("a removed device was answered")
	}
}

// closed: whether the Mac has closed the connection (nothing but a read error comes from it any more).
func (p *fakePhone) closed(wait time.Duration) bool {
	p.c.SetReadDeadline(time.Now().Add(wait))
	var f relay.Sealed
	err := websocket.JSON.Receive(p.c, &f)
	if err == nil {
		return false
	}
	ne, timeout := err.(interface{ Timeout() bool })
	return !(timeout && ne.Timeout())
}

// Frames that do not open, one after another, close the tunnel: a relay or a stranger with the device's id cannot
// make the Mac read them for ever. A real frame in between starts the count again.
func TestTunnelClosesAfterBadFrames(t *testing.T) {
	tunnelBadPause = time.Millisecond
	t.Cleanup(func() { tunnelBadPause = 100 * time.Millisecond })
	srv, phones, _, dir := e2eServer(t)
	p := pairE2E(t, srv, phones, dir)
	f := p.send(relay.TypeRequest, "first", relay.Request{U: "/api/i18n"})
	if g, _, ok := p.next(5 * time.Second); !ok || g.I != "first" {
		t.Fatal("no answer to the first request")
	}
	for range tunnelBadMax - 1 { // one less than the limit, and a real frame after: still open
		p.sendFrame(f)
	}
	if res := p.do(relay.Request{U: "/api/i18n"}); res.S != 200 {
		t.Fatalf("after %d bad frames: %d", tunnelBadMax-1, res.S)
	}
	for range tunnelBadMax {
		p.sendFrame(f) // sent again: it never opens
	}
	if !p.closed(5 * time.Second) {
		t.Fatal("the tunnel is still open after a run of frames that do not open")
	}
}

// Until a frame has opened, the tunnel takes no big frames: the device's id is in the open in every frame, so it
// proves nothing. After, it takes what the phone sends.
func TestTunnelFirstFrameLimit(t *testing.T) {
	srv, phones, _, dir := e2eServer(t)
	p := pairE2E(t, srv, phones, dir)
	big := relay.Sealed{V: relay.Version, D: p.id, T: relay.TypeRequest, I: "x", N: 1, C: make([]byte, tunnelFrameFirst)}
	websocket.JSON.Send(p.c, big) // the write may fail too once the Mac has closed: the read below tells
	if !p.closed(5 * time.Second) {
		t.Fatal("a frame over the first limit did not close the tunnel")
	}

	p.dial()
	body := bytes.Repeat([]byte("a"), tunnelFrameFirst) // sealed and encoded, over the first limit
	if res := p.do(relay.Request{U: "/api/i18n"}); res.S != 200 {
		t.Fatal(res.S)
	}
	if res := p.do(relay.Request{M: "POST", U: "/api/i18n", B: body}); res.S == 0 || res.S == http.StatusServiceUnavailable {
		t.Fatalf("a big frame after the first: %d", res.S)
	}
}

// The counter of the phone's frames is written down in blocks: a run of requests does not rewrite phones.json.
func TestTunnelDoesNotRewritePhonesPerFrame(t *testing.T) {
	srv, phones, _, dir := e2eServer(t)
	p := pairE2E(t, srv, phones, dir)
	if res := p.do(relay.Request{U: "/api/i18n"}); res.S != 200 {
		t.Fatal(res.S)
	}
	file := filepath.Join(dir, "phones.json")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(file)
	for range 5 {
		if res := p.do(relay.Request{U: "/api/i18n"}); res.S != 200 {
			t.Fatal(res.S)
		}
	}
	after, _ := os.ReadFile(file)
	st2, _ := os.Stat(file)
	if !bytes.Equal(before, after) || !st.ModTime().Equal(st2.ModTime()) {
		t.Fatal("phones.json was written while the requests stayed in one block")
	}
}

// One tunnel per device: a new connection whose frame opens takes the place of the old one.
func TestTunnelOnePerDevice(t *testing.T) {
	srv, phones, _, dir := e2eServer(t)
	p := pairE2E(t, srv, phones, dir)
	if res := p.do(relay.Request{U: "/api/i18n"}); res.S != 200 {
		t.Fatal(res.S)
	}
	old := p.c
	p.dial()
	if res := p.do(relay.Request{U: "/api/i18n"}); res.S != 200 {
		t.Fatal(res.S)
	}
	old.SetReadDeadline(time.Now().Add(2 * time.Second))
	var f relay.Sealed
	if err := websocket.JSON.Receive(old, &f); err == nil {
		t.Fatal("the old tunnel is still open")
	}
}

// The terminal rides in the tunnel as a stream.
func TestTunnelTerminal(t *testing.T) {
	srv, phones, d, dir := e2eServer(t)
	s, err := term.Start(term.Options{ID: "a#1", Worktree: t.TempDir(), Command: "/bin/sh", Token: "tok",
		Args: []string{"-c", `echo ready; read x; echo "typed $x"; sleep 5`}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	d.Sessions.Put(s)
	p := pairE2E(t, srv, phones, dir)
	p.send(relay.TypeOpen, "t1", relay.Request{U: "/ws/term?id=a%231"})
	var got string
	read := func(want string) {
		for !strings.Contains(got, want) {
			f, b, ok := p.next(5 * time.Second)
			if !ok || f.I != "t1" || f.T == relay.TypeClose {
				t.Fatalf("waiting for %q in %q", want, got)
			}
			var m relay.Message
			json.Unmarshal(b, &m)
			var e struct{ D string }
			json.Unmarshal(m.WS, &e)
			got += e.D
		}
	}
	read("ready")
	p.send(relay.TypeMessage, "t1", relay.Message{WS: []byte(`{"t":"i","d":"hello\r"}`)})
	read("typed hello")

	// Only the terminal is a stream.
	p.send(relay.TypeOpen, "t2", relay.Request{U: "/api/state"})
	for {
		f, _, ok := p.next(5 * time.Second)
		if !ok {
			t.Fatal("another stream was not refused")
		}
		if f.I == "t2" {
			if f.T != relay.TypeClose {
				t.Fatalf("another stream: %s", f.T)
			}
			break
		}
	}
}

// A device paired end to end is never let in by its token: the phone was not given it, so whoever offers it got it
// some other way.
func TestE2EDeviceHasNoTokenWay(t *testing.T) {
	srv, phones, _, dir := e2eServer(t)
	p := pairE2E(t, srv, phones, dir)
	req, _ := http.NewRequest("GET", srv.URL+"/api/state", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.2")
	req.AddCookie(&http.Cookie{Name: deviceCookie, Value: p.token})
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the token in a cookie: %d", res.StatusCode)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/pair/d/"+p.token, nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.2")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if res, _ := client.Do(req); res.StatusCode != http.StatusUnauthorized || len(res.Cookies()) != 0 {
		t.Fatalf("the token in a link: %d", res.StatusCode)
	}
	// The manifest the installed app opens with names no token.
	res := p.do(relay.Request{U: "/manifest.webmanifest"})
	var m struct {
		Start string `json:"start_url"`
	}
	json.Unmarshal(res.B, &m)
	if m.Start != "/" {
		t.Fatalf("start_url %q", m.Start)
	}
	// A phone with no key gets the way to connect.
	req, _ = http.NewRequest("GET", srv.URL+"/", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.2")
	res2, _ := http.DefaultClient.Do(req)
	var b bytes.Buffer
	b.ReadFrom(res2.Body)
	if res2.StatusCode != http.StatusUnauthorized || !strings.Contains(b.String(), `href="/pair"`) {
		t.Fatalf("not connected: %d", res2.StatusCode)
	}
}

// The board compares the service worker it is given with the one the Mac sends.
func TestShellHash(t *testing.T) {
	srv, phones, _, dir := e2eServer(t)
	p := pairE2E(t, srv, phones, dir)
	res := p.do(relay.Request{U: "/api/shell"})
	var got struct{ SW string }
	json.Unmarshal(res.B, &got)
	sum := sha256.Sum256(serviceWorker)
	if got.SW != hex.EncodeToString(sum[:]) {
		t.Fatalf("shell %d %q", res.S, got.SW)
	}
}

// The pairing page reads the secret from the fragment and pairs through the service worker; without WebCrypto, the
// old way. With no link (the app from the Home Screen) it asks for one.
func TestPairPage(t *testing.T) {
	for _, want := range []string{`postMessage({t: "pair", code: code, k: k}`, `id="paste"`, `history.replaceState(null, "", "/pair")`} {
		if !strings.Contains(pairPage, want) {
			t.Errorf("the pair page has no %s", want)
		}
	}
	// The service worker carries the page, to show it itself once paired.
	if bytes.Contains(serviceWorker, []byte(`/*pair page*/`)) || !bytes.Contains(serviceWorker, []byte(`Connect the phone`)) {
		t.Fatal("sw.js does not carry the pairing page")
	}
}
