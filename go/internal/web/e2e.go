package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
	"github.com/SergeyPirogov/mako-agent/go/internal/relay"
)

// The phone's end-to-end tunnel (docs/relay-e2e.md): one WebSocket from its service worker, carrying relay.Sealed
// frames. The Mac opens each with the device's keys, runs it as a request of that device through the same guard as
// any other, and seals the answer. Whatever is between (tailscale serve, the relay) sees frames, not paths, cookies
// or bodies.

const (
	tunnelFrameMax   = 32 << 20  // the biggest frame the tunnel takes, once one frame has opened
	tunnelFrameFirst = 256 << 10 // the biggest before that: whoever has only the device's id gets no 32 MB buffers
	tunnelBadMax     = 5         // frames in a row that do not open, and the tunnel closes
	tunnelAnswerMax  = 24 << 20  // the biggest answer it seals: a bigger one would not fit in a frame
	tunnelInFlight   = 32        // requests of one tunnel at a time
	tunnelStreams    = 8         // WebSocket streams of one tunnel at a time
)

// tunnelBadPause: the wait after the first frame that did not open; it doubles with each next one in a row, up to a
// second. A var, so the tests need not wait.
var tunnelBadPause = 100 * time.Millisecond

type tunnelKey struct{}

// tunneled: the device whose sealed frame carried this request, nil for any other request.
func tunneled(r *http.Request) *phone.Device {
	d, _ := r.Context().Value(tunnelKey{}).(*phone.Device)
	return d
}

// The headers a sealed request may carry to Mako's handlers. Cookies, Origin, forwarding headers and the like are
// set by the Mac itself, never taken from the frame.
var tunnelHeaders = map[string]bool{
	"Content-Type": true, "Accept": true, "Accept-Language": true, "If-None-Match": true, "If-Modified-Since": true, "Range": true,
}

var tunnelMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// tunnel is one connection of a device's service worker.
type tunnel struct {
	c      *websocket.Conn
	sealer *relay.Sealer
	ctx    context.Context
	// mu keeps sealing and sending one step: the frames leave in the order of their counters, and the phone, which
	// takes only growing counters, gets them all.
	mu      sync.Mutex
	smu     sync.Mutex
	streams map[string]*tunnelStream
}

func (t *tunnel) send(typ, id string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := t.sealer.Seal(typ, id, payload)
	if err != nil {
		return err
	}
	return websocket.JSON.Send(t.c, f)
}

func (s *Server) tunnelRoute(inner http.Handler) http.Handler {
	return websocket.Server{
		Handshake: func(_ *websocket.Config, r *http.Request) error {
			if s.Phones == nil || r.Header.Get("Origin") == "" || !sameOrigin(r) {
				return errors.New("forbidden")
			}
			return nil
		},
		Handler: func(c *websocket.Conn) { s.serveTunnel(c, inner) },
	}
}

// takeTunnel makes t the device's tunnel, closing the one before: a device has one service worker, and two
// connections would split its counters. Only a connection whose frame opened takes the place, so one with forged
// frames cannot push the device off.
func (s *Server) takeTunnel(id string, t *tunnel) {
	s.tunMu.Lock()
	defer s.tunMu.Unlock()
	if s.tunnels == nil {
		s.tunnels = map[string]*tunnel{}
	}
	if old := s.tunnels[id]; old != nil && old != t {
		old.c.Close()
	}
	s.tunnels[id] = t
}

func (s *Server) dropTunnel(id string, t *tunnel) {
	s.tunMu.Lock()
	defer s.tunMu.Unlock()
	if s.tunnels[id] == t {
		delete(s.tunnels, id)
	}
}

func (s *Server) serveTunnel(c *websocket.Conn, inner http.Handler) {
	defer c.Close()
	c.MaxPayloadBytes = tunnelFrameFirst
	id := c.Request().URL.Query().Get("d")
	if s.Phones.ByID(id) == nil {
		return
	}
	opener, err := s.Phones.Opener(id)
	if err != nil {
		return
	}
	sealer, err := s.Phones.Sealer(id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t := &tunnel{c: c, sealer: sealer, ctx: ctx, streams: map[string]*tunnelStream{}}
	defer t.closeStreams()
	busy := make(chan struct{}, tunnelInFlight)
	took := false
	bad := 0
	for {
		var f relay.Sealed
		if err := websocket.JSON.Receive(c, &f); err != nil {
			return
		}
		payload, err := opener.Open(f)
		if errors.Is(err, relay.ErrReplay) || errors.Is(err, relay.ErrTag) {
			// A frame the relay made, changed or sent again: nothing to answer, but not endlessly many.
			if bad++; bad >= tunnelBadMax {
				return
			}
			time.Sleep(min(tunnelBadPause<<(bad-1), time.Second))
			continue
		}
		if err != nil {
			return // the device is gone, or its counter cannot be written down
		}
		bad = 0
		d := s.Phones.ByID(id)
		if d == nil {
			return
		}
		if !took {
			took = true
			c.MaxPayloadBytes = tunnelFrameMax
			s.takeTunnel(id, t)
			defer s.dropTunnel(id, t)
			// The exact counter, not the bound ahead of it: a restart then refuses only what was taken. It is written
			// before the tunnel is dropped, so whoever sees the tunnel gone knows the file is final.
			defer opener.Flush()
		}
		switch f.T {
		case relay.TypeRequest:
			var req relay.Request
			if json.Unmarshal(payload, &req) != nil {
				t.send(relay.TypeResponse, f.I, relay.Response{S: http.StatusBadRequest})
				continue
			}
			select {
			case busy <- struct{}{}:
			default:
				t.send(relay.TypeResponse, f.I, relay.Response{S: http.StatusServiceUnavailable, B: []byte("too many requests at once")})
				continue
			}
			go func(i string) {
				defer func() { <-busy }()
				t.send(relay.TypeResponse, i, s.tunnelRequest(ctx, inner, d, req))
			}(f.I)
		case relay.TypeOpen:
			var req relay.Request
			if json.Unmarshal(payload, &req) != nil {
				t.send(relay.TypeClose, f.I, struct{}{})
				continue
			}
			t.openStream(s, f.I, req.U)
		case relay.TypeMessage:
			var m relay.Message
			if json.Unmarshal(payload, &m) == nil {
				t.deliver(f.I, string(m.WS))
			}
		case relay.TypeClose:
			if st := t.stream(f.I); st != nil {
				st.end(false)
			}
		}
	}
}

// tunnelURL: the path and query of a sealed request, nothing that could name another host.
func tunnelURL(u string) (*url.URL, error) {
	if !strings.HasPrefix(u, "/") || strings.HasPrefix(u, "//") {
		return nil, errors.New("bad path")
	}
	p, err := url.ParseRequestURI(u)
	if err != nil || p.Host != "" || p.Scheme != "" {
		return nil, errors.New("bad path")
	}
	return &url.URL{Path: p.Path, RawPath: p.RawPath, RawQuery: p.RawQuery}, nil
}

// tunnelRequest runs a sealed request as the device's own: remote, so the Mac-only pages stay closed, from the
// device's address, with no cookie. A navigation may come from another site's link, so it is not same-origin: what
// guards a POST from other sites guards it.
func (s *Server) tunnelRequest(ctx context.Context, inner http.Handler, d *phone.Device, req relay.Request) (res relay.Response) {
	m := strings.ToUpper(req.M)
	if m == "" {
		m = http.MethodGet
	}
	if !tunnelMethods[m] {
		return relay.Response{S: http.StatusMethodNotAllowed}
	}
	u, err := tunnelURL(req.U)
	if err != nil || strings.HasPrefix(u.Path, "/ws/") || u.Path == "/e2e" {
		return relay.Response{S: http.StatusBadRequest, B: []byte("bad request")}
	}
	r, err := http.NewRequestWithContext(context.WithValue(ctx, tunnelKey{}, d), m, u.String(), bytes.NewReader(req.B))
	if err != nil {
		return relay.Response{S: http.StatusBadRequest, B: []byte("bad request")}
	}
	r.RequestURI = u.RequestURI()
	r.Host = relay.MacID(d.Origin)
	r.RemoteAddr = "relay"
	for k, v := range req.H {
		if k = http.CanonicalHeaderKey(k); tunnelHeaders[k] {
			r.Header.Set(k, v)
		}
	}
	// The board's own mark (makoHeader) comes only with the page's own requests: another site's link or form, which
	// a navigation may be, cannot set a header.
	if req.Nav {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	} else {
		r.Header.Set("Origin", d.Origin)
		if v, ok := req.H[makoHeader]; ok {
			r.Header.Set(makoHeader, v)
		}
	}
	w := &tunnelWriter{h: http.Header{}}
	defer func() {
		if p := recover(); p != nil {
			log.Printf("tunnel: %s %s: %v", m, u.Path, p)
			res = relay.Response{S: http.StatusInternalServerError}
		}
	}()
	inner.ServeHTTP(w, r)
	if w.over {
		return relay.Response{S: http.StatusBadGateway, B: []byte("the answer is too big for the phone")}
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	res = relay.Response{S: w.status, H: map[string]string{}, B: w.body.Bytes()}
	for k, v := range w.h {
		if k == "Set-Cookie" {
			continue // a cookie is not how a device paired end to end is let in
		}
		res.H[k] = strings.Join(v, ", ")
	}
	return res
}

// tunnelWriter keeps an answer to seal it whole.
type tunnelWriter struct {
	h      http.Header
	status int
	body   bytes.Buffer
	over   bool
}

func (w *tunnelWriter) Header() http.Header { return w.h }
func (w *tunnelWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *tunnelWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len()+len(b) > tunnelAnswerMax {
		w.over = true
		return 0, errors.New("the answer is too big")
	}
	return w.body.Write(b)
}

// tunnelStream is a WebSocket of the page carried inside the tunnel: today only the terminal.
type tunnelStream struct {
	t    *tunnel
	id   string
	in   chan string
	done chan struct{}
	once sync.Once
}

func (t *tunnel) stream(id string) *tunnelStream {
	t.smu.Lock()
	defer t.smu.Unlock()
	return t.streams[id]
}

func (t *tunnel) openStream(s *Server, id, u string) {
	p, err := tunnelURL(u)
	t.smu.Lock()
	if err != nil || p.Path != "/ws/term" || t.streams[id] != nil || len(t.streams) >= tunnelStreams {
		t.smu.Unlock()
		t.send(relay.TypeClose, id, struct{}{})
		return
	}
	st := &tunnelStream{t: t, id: id, in: make(chan string, 256), done: make(chan struct{})}
	t.streams[id] = st
	t.smu.Unlock()
	go s.termStream(p.Query().Get("id"), st)
}

func (t *tunnel) deliver(id, msg string) {
	st := t.stream(id)
	if st == nil {
		return
	}
	select {
	case st.in <- msg:
	case <-st.done:
	default:
		st.end(true) // the page sends faster than the agent reads: not a terminal any more
	}
}

func (t *tunnel) closeStreams() {
	t.smu.Lock()
	all := make([]*tunnelStream, 0, len(t.streams))
	for _, st := range t.streams {
		all = append(all, st)
	}
	t.smu.Unlock()
	for _, st := range all {
		st.end(false)
	}
}

// end closes the stream; tell: let the phone know (it did not close it itself).
func (st *tunnelStream) end(tell bool) {
	st.once.Do(func() {
		close(st.done)
		st.t.smu.Lock()
		delete(st.t.streams, st.id)
		st.t.smu.Unlock()
		if tell {
			st.t.send(relay.TypeClose, st.id, struct{}{})
		}
	})
}

func (st *tunnelStream) Send(v any) error {
	select {
	case <-st.done:
		return errors.New("the stream is closed")
	default:
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := st.t.send(relay.TypeMessage, st.id, relay.Message{WS: b}); err != nil {
		return fmt.Errorf("the tunnel: %w", err)
	}
	return nil
}

func (st *tunnelStream) Receive() (string, error) {
	select {
	case m := <-st.in:
		return m, nil
	case <-st.done:
		return "", errors.New("the stream is closed")
	case <-st.t.ctx.Done():
		return "", errors.New("the tunnel is closed")
	}
}

func (st *tunnelStream) Close() error {
	st.end(true)
	return nil
}
