package relay

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"
)

// OfflineText: what a phone gets from the address of a Mac that has no tunnel open.
const OfflineText = "This Mac is offline. Open Mako on it: it connects to the relay by itself."

// Server is the relay: Macs open tunnels to it, and a phone opening https://<id>.<Domain> reaches the Mac whose
// token gives that id. TLS ends before it (the platform's proxy).
type Server struct {
	Domain   string // "relay.example.com": the Macs are at <id>.relay.example.com
	Limits   Limits
	IPHeader string // the header the platform puts the client's address in ("Fly-Client-IP"); "" uses the socket's
	// HideMetrics: /_relay/metrics is not served here, only by Metrics() on a port the platform does not expose.
	HideMetrics bool
	Log         *log.Logger // nil: the standard one. It never gets a Mac's id or a phone's path

	once     sync.Once
	mu       sync.Mutex
	sessions map[string]*session
	macIPs   counter
	phoneIPs counter
	rate     rater
	m        *metrics
	streams  atomic.Int64 // phone WebSockets open now, for the tests
}

func (s *Server) init() {
	s.once.Do(func() {
		s.Limits = s.Limits.orDefault()
		s.sessions = map[string]*session{}
		s.rate = rater{rate: s.Limits.Rate, burst: float64(s.Limits.Burst)}
		s.m = newMetrics()
		if s.Log == nil {
			s.Log = log.Default()
		}
	})
}

func (s *Server) online() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.init()
	switch r.URL.Path {
	case "/_relay/health":
		io.WriteString(w, "ok\n")
		return
	case "/_relay/metrics":
		if s.HideMetrics {
			http.NotFound(w, r)
			return
		}
		s.Metrics().ServeHTTP(w, r)
		return
	case ConnectPath:
		s.connect(w, r)
		return
	}
	id, ok := s.macOf(r.Host)
	if !ok {
		http.Error(w, "Not a Mac's address on this relay. Open the one Mako shows in Settings → Phone.", http.StatusNotFound)
		return
	}
	s.phone(w, r, id)
}

// Metrics serves the counters in Prometheus' text format. They hold no Mac's id, but they are the relay's load, so on
// a shared relay serve them on a port that is not public (HideMetrics, RELAY_METRICS_PORT).
func (s *Server) Metrics() http.Handler {
	s.init()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.m.write(w, s.online())
	})
}

// macOf: the Mac's id in a host such as "<id>.relay.example.com:443".
func (s *Server) macOf(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	id, ok := strings.CutSuffix(host, "."+strings.ToLower(s.Domain))
	return id, ok && validID(id)
}

func (s *Server) clientIP(r *http.Request) string {
	if s.IPHeader != "" {
		if ip := strings.TrimSpace(r.Header.Get(s.IPHeader)); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func reject(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, text, status)
}

// tooMany: 429 with when to try again, in seconds.
func tooMany(w http.ResponseWriter, secs int, text string) {
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	reject(w, http.StatusTooManyRequests, text)
}

// connect: a Mac opens its tunnel.
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(token) < 16 {
		reject(w, http.StatusUnauthorized, "a Mac's token is needed")
		return
	}
	id := IDFor(token)
	ip := s.clientIP(r)
	if !s.macIPs.acquire(ip, s.Limits.MacsPerIP) {
		s.m.reject(rejectIP)
		tooMany(w, 30, "too many Macs from this address")
		return
	}
	defer s.macIPs.release(ip) // ServeHTTP below returns when the tunnel ends, or at once when it never opens
	s.mu.Lock()
	_, again := s.sessions[id]
	full := !again && len(s.sessions) >= s.Limits.MaxMacs
	s.mu.Unlock()
	if full {
		s.m.reject(rejectMacs)
		reject(w, http.StatusServiceUnavailable, "the relay is full, try again later")
		return
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	public := scheme + "://" + id + "." + s.Domain
	if _, port, err := net.SplitHostPort(r.Host); err == nil && scheme == "http" {
		public += ":" + port // a relay run by hand, without the platform in front
	}
	websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil }, // a Mac is no browser: no Origin
		Handler: func(ws *websocket.Conn) {
			ws.MaxPayloadBytes = s.Limits.maxFrame()
			s.tunnel(ws, id, public)
		},
	}.ServeHTTP(w, r)
}

// session is one Mac's tunnel.
type session struct {
	s       *Server
	ws      *websocket.Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan Frame
	streams map[uint64]*stream
	done    chan struct{}
	once    sync.Once
}

// stream: a phone's WebSocket through the tunnel; out holds what the Mac sends it.
type stream struct {
	out  chan Frame
	gone chan struct{}
	once sync.Once
}

func (st *stream) close() { st.once.Do(func() { close(st.gone) }) }

func (ss *session) send(f Frame) error {
	ss.wmu.Lock()
	defer ss.wmu.Unlock()
	ss.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return websocket.JSON.Send(ss.ws, f)
}

func (ss *session) close() {
	ss.once.Do(func() {
		close(ss.done)
		ss.ws.Close()
	})
}

// open: room for a request or stream to this Mac, with its number; false when MaxPending are in flight.
func (ss *session) open(st *stream) (uint64, chan Frame, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if len(ss.pending)+len(ss.streams) >= ss.s.Limits.MaxPending {
		return 0, nil, false
	}
	ss.next++
	ch := make(chan Frame, 1)
	ss.pending[ss.next] = ch
	if st != nil {
		ss.streams[ss.next] = st
	}
	return ss.next, ch, true
}

func (ss *session) forget(i uint64) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.pending, i)
	if st := ss.streams[i]; st != nil {
		st.close()
		delete(ss.streams, i)
	}
}

func (s *Server) tunnel(ws *websocket.Conn, id, public string) {
	ss := &session{s: s, ws: ws, pending: map[uint64]chan Frame{}, streams: map[uint64]*stream{}, done: make(chan struct{})}
	s.mu.Lock()
	old := s.sessions[id]
	if old == nil && len(s.sessions) >= s.Limits.MaxMacs {
		// Many Macs passed connect's check at once: this one finds the relay full.
		s.mu.Unlock()
		s.m.reject(rejectMacs)
		return
	}
	s.sessions[id] = ss
	n := len(s.sessions)
	s.mu.Unlock()
	if old != nil {
		old.close() // the same Mac again: its old tunnel is dead or about to be
	}
	s.Log.Printf("relay: a Mac connected, %d online", n)
	defer func() {
		ss.close()
		s.mu.Lock()
		if s.sessions[id] == ss {
			delete(s.sessions, id)
		}
		n := len(s.sessions)
		s.mu.Unlock()
		ss.mu.Lock()
		for i, st := range ss.streams {
			st.close()
			delete(ss.streams, i)
		}
		ss.mu.Unlock()
		s.Log.Printf("relay: a Mac disconnected, %d online", n)
	}()
	if err := ss.send(Frame{T: FrameHello, U: public, L: s.Limits.MaxBody}); err != nil {
		return
	}
	go func() {
		t := time.NewTicker(s.Limits.Ping)
		defer t.Stop()
		for {
			select {
			case <-ss.done:
				return
			case <-t.C:
				if ss.send(Frame{T: FramePing}) != nil {
					ss.close()
					return
				}
			}
		}
	}()
	for {
		ws.SetReadDeadline(time.Now().Add(s.Limits.Idle))
		var f Frame
		if err := websocket.JSON.Receive(ws, &f); err != nil {
			if errors.Is(err, websocket.ErrFrameTooLarge) {
				s.m.reject(rejectSize)
				s.Log.Printf("relay: a Mac sent a frame over %d bytes, its tunnel is closed", s.Limits.maxFrame())
			}
			return
		}
		ss.mu.Lock()
		switch f.T {
		case FrameReply:
			if ch := ss.pending[f.I]; ch != nil {
				delete(ss.pending, f.I)
				ch <- f
			}
		case FrameWSMsg:
			if st := ss.streams[f.I]; st != nil {
				select {
				case st.out <- f:
				default: // the phone does not keep up: it gets the stream closed rather than the tunnel stalled
					st.close()
				}
			}
		case FrameWSClose:
			if st := ss.streams[f.I]; st != nil {
				st.close()
				delete(ss.streams, f.I)
			}
		}
		ss.mu.Unlock()
	}
}

// phone: a phone's request to Mac id, over its tunnel.
func (s *Server) phone(w http.ResponseWriter, r *http.Request, id string) {
	s.m.requests.Add(1)
	ip := s.clientIP(r)
	if !s.phoneIPs.acquire(ip, s.Limits.ConnsPerIP) {
		s.m.reject(rejectIP)
		tooMany(w, 5, "too many connections from this address")
		return
	}
	defer s.phoneIPs.release(ip)
	s.m.phoneConns.Add(1)
	defer s.m.phoneConns.Add(-1)
	// Per Mac and address: whoever knows a Mac's address (it is in the QR) spends only their own share of it.
	if !s.rate.allow(id+"|"+ip, time.Now()) {
		s.m.reject(rejectRate)
		tooMany(w, 1, "too many requests to this Mac from this address, slow down")
		return
	}
	s.mu.Lock()
	ss := s.sessions[id]
	s.mu.Unlock()
	if ss == nil {
		s.m.reject(rejectOffline)
		reject(w, http.StatusServiceUnavailable, OfflineText)
		return
	}
	if r.ContentLength > s.Limits.MaxBody {
		s.m.reject(rejectSize)
		reject(w, http.StatusRequestEntityTooLarge, "the body is too large for the relay")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.Limits.MaxBody+1))
	if err != nil {
		reject(w, http.StatusBadRequest, "could not read the body")
		return
	}
	if int64(len(body)) > s.Limits.MaxBody {
		s.m.reject(rejectSize)
		reject(w, http.StatusRequestEntityTooLarge, "the body is too large for the relay")
		return
	}
	s.m.bytesIn.Add(int64(len(body)))

	h := cleanHeader(r.Header)
	for _, k := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"} {
		h.Del(k)
	}
	if s.IPHeader != "" {
		h.Del(s.IPHeader)
	}
	h.Set("X-Forwarded-For", ip)
	h.Set("X-Forwarded-Host", r.Host)
	proto := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		proto = "https"
	}
	h.Set("X-Forwarded-Proto", proto)

	ws := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	var st *stream
	if ws {
		st = &stream{out: make(chan Frame, 256), gone: make(chan struct{})}
	}
	i, ch, ok := ss.open(st)
	if !ok {
		s.m.reject(rejectBusy)
		w.Header().Set("Retry-After", "1")
		reject(w, http.StatusServiceUnavailable, "this Mac is busy, try again")
		return
	}
	defer ss.forget(i)
	f := Frame{T: FrameRequest, I: i, M: r.Method, U: r.URL.RequestURI(), H: h, B: body}
	if ws {
		f = Frame{T: FrameWSOpen, I: i, U: r.URL.RequestURI(), H: h}
	}
	if ss.send(f) != nil {
		ss.close()
		s.m.reject(rejectGone)
		reject(w, http.StatusBadGateway, "the Mac's connection broke, try again")
		return
	}
	timer := time.NewTimer(s.Limits.Response)
	defer timer.Stop()
	var res Frame
	select {
	case res = <-ch:
	case <-ss.done:
		s.m.reject(rejectGone)
		reject(w, http.StatusBadGateway, "the Mac's connection broke, try again")
		return
	case <-timer.C:
		s.m.reject(rejectTimeout)
		reject(w, http.StatusGatewayTimeout, "the Mac did not answer in time")
		return
	case <-r.Context().Done():
		return
	}
	if ws && res.S == http.StatusSwitchingProtocols {
		s.pipe(w, r, ss, i, st)
		return
	}
	if int64(len(res.B)) > s.Limits.MaxBody {
		// A Mac that does not keep to the limit it was told in hello: its answer is not passed on.
		s.m.reject(rejectSize)
		reject(w, http.StatusBadGateway, "the Mac's answer is too large for the relay")
		return
	}
	for k, v := range cleanHeader(res.H) {
		if k != "Content-Length" {
			w.Header()[k] = v
		}
	}
	if res.S < 200 || res.S > 999 || res.S == http.StatusSwitchingProtocols {
		res.S = http.StatusBadGateway
	}
	w.WriteHeader(res.S)
	n, _ := w.Write(res.B)
	s.m.bytesOut.Add(int64(n))
}

// pipe: the phone's WebSocket, once the Mac took it, goes both ways until either side or StreamIdle ends it.
func (s *Server) pipe(w http.ResponseWriter, r *http.Request, ss *session, i uint64, st *stream) {
	websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil }, // the Mac checked the Origin
		Handler: func(c *websocket.Conn) {
			s.streams.Add(1)
			defer s.streams.Add(-1)
			c.MaxPayloadBytes = s.Limits.MaxMessage
			var last atomic.Int64
			last.Store(time.Now().UnixNano())
			defer c.Close()
			go func() { // the Mac's messages to the phone, and the end of the stream
				defer c.Close()
				t := time.NewTicker(s.Limits.StreamIdle / 4)
				defer t.Stop()
				for {
					select {
					case f := <-st.out:
						last.Store(time.Now().UnixNano())
						c.SetWriteDeadline(time.Now().Add(10 * time.Second))
						if wsMessage.Send(c, wsMsg{data: f.B, text: f.X}) != nil {
							return
						}
						s.m.bytesOut.Add(int64(len(f.B)))
					case <-t.C:
						if time.Since(time.Unix(0, last.Load())) > s.Limits.StreamIdle {
							return
						}
					case <-st.gone:
						return
					case <-ss.done:
						return
					}
				}
			}()
			for {
				var m wsMsg
				if err := wsMessage.Receive(c, &m); err != nil {
					if errors.Is(err, websocket.ErrFrameTooLarge) {
						s.m.reject(rejectSize)
					}
					break
				}
				last.Store(time.Now().UnixNano())
				s.m.bytesIn.Add(int64(len(m.data)))
				if ss.send(Frame{T: FrameWSMsg, I: i, B: m.data, X: m.text}) != nil {
					break
				}
			}
			select {
			case <-ss.done:
			default:
				ss.send(Frame{T: FrameWSClose, I: i})
			}
		},
	}.ServeHTTP(w, r)
}

// stats: what the relay holds now, for the tests that check it lets go of everything.
func (s *Server) stats() string {
	s.init()
	return fmt.Sprintf("sessions=%d macIPs=%d phoneIPs=%d buckets=%d streams=%d",
		s.online(), s.macIPs.len(), s.phoneIPs.len(), s.rate.len(time.Now().Add(time.Hour)), s.streams.Load())
}
