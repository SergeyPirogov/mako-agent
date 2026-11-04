package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// DefaultAddr: the shared relay Mako connects to when MAKO_RELAY is not set. Empty until the relay is deployed
// (docs/relay.md, «Розгортання»), so the relay is off without MAKO_RELAY.
const DefaultAddr = ""

// Addr: the relay this Mac connects to, "" for none. MAKO_RELAY=off turns the default off.
func Addr() string {
	a := strings.TrimSpace(os.Getenv("MAKO_RELAY"))
	if a == "off" {
		return ""
	}
	if a == "" {
		a = DefaultAddr
	}
	return strings.TrimRight(a, "/")
}

// Client keeps this Mac's tunnel to the relay and answers the phone's requests from Mako's own server at Local.
type Client struct {
	Addr    string // the relay, "https://relay.example.com"
	Token   string // LoadToken
	Local   string // Mako's own server, "http://127.0.0.1:8080"
	MaxBody int64  // an answer's body; 0: the relay's default
	Idle    time.Duration
	Log     *log.Logger

	mu  sync.Mutex
	url string
}

// URL: the address phones reach this Mac at, "" while the tunnel is down.
func (c *Client) URL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url
}

func (c *Client) setURL(u string) {
	c.mu.Lock()
	c.url = u
	c.mu.Unlock()
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// Run keeps the tunnel open until ctx ends, connecting again after 1 s, then up to a minute when it keeps failing.
func (c *Client) Run(ctx context.Context) {
	if c.MaxBody <= 0 {
		c.MaxBody = DefaultLimits().MaxBody
	}
	if c.Idle <= 0 {
		c.Idle = DefaultLimits().Idle
	}
	wait := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		c.setURL("")
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		c.logf("relay: %v, connecting again in %s", err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
}

func (c *Client) session(ctx context.Context) error {
	u, err := url.Parse(c.Addr)
	if err != nil {
		return err
	}
	origin := u.Scheme + "://" + u.Host
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = ConnectPath
	cfg, err := websocket.NewConfig(u.String(), origin)
	if err != nil {
		return err
	}
	cfg.Header = http.Header{"Authorization": {"Bearer " + c.Token}}
	cfg.Dialer = &net.Dialer{Timeout: 15 * time.Second}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		return err
	}
	ws.MaxPayloadBytes = DefaultLimits().maxFrame()
	t := &tunnel{c: c, ws: ws, streams: map[uint64]*localStream{}}
	stop := context.AfterFunc(ctx, func() { ws.Close() })
	defer stop()
	defer t.closeAll()
	defer ws.Close()
	for {
		ws.SetReadDeadline(time.Now().Add(c.Idle)) // the relay pings well within it
		var f Frame
		if err := websocket.JSON.Receive(ws, &f); err != nil {
			return err
		}
		switch f.T {
		case FrameHello:
			pu, err := url.Parse(f.U)
			if err != nil || pu.Host == "" {
				return errors.New("the relay gave no address")
			}
			t.host, t.scheme = pu.Host, pu.Scheme
			c.setURL(f.U)
			c.logf("relay: connected, phones reach Mako at %s", f.U)
		case FramePing:
			t.send(Frame{T: FramePong})
		case FrameRequest:
			go t.request(ctx, f)
		case FrameWSOpen:
			go t.open(f)
		case FrameWSMsg:
			if st := t.stream(f.I); st != nil {
				select {
				case st.in <- f:
				default:
					st.close()
				}
			}
		case FrameWSClose:
			if st := t.stream(f.I); st != nil {
				st.close()
			}
		}
	}
}

type tunnel struct {
	c       *Client
	ws      *websocket.Conn
	host    string // the Mac's host on the relay
	scheme  string
	wmu     sync.Mutex
	mu      sync.Mutex
	streams map[uint64]*localStream
}

type localStream struct {
	in   chan Frame
	gone chan struct{}
	once sync.Once
}

func (st *localStream) close() { st.once.Do(func() { close(st.gone) }) }

func (t *tunnel) send(f Frame) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	t.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return websocket.JSON.Send(t.ws, f)
}

func (t *tunnel) stream(i uint64) *localStream {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.streams[i]
}

func (t *tunnel) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, st := range t.streams {
		st.close()
		delete(t.streams, i)
	}
}

// header: what Mako gets with a phone's request. X-Forwarded-Host is always set, whatever the relay sent, so Mako
// treats the request as a phone's (guard, macOnly) and never as this Mac's own.
func (t *tunnel) header(h http.Header) http.Header {
	h = cleanHeader(h)
	h.Set("X-Forwarded-Host", t.host)
	h.Set("X-Forwarded-Proto", t.scheme)
	if h.Get("X-Forwarded-For") == "" {
		h.Set("X-Forwarded-For", "relay")
	}
	return h
}

var localHTTP = &http.Client{
	Timeout:       55 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, // the phone follows them
}

func (t *tunnel) request(ctx context.Context, f Frame) {
	reply := func(status int, h http.Header, body []byte) {
		t.send(Frame{T: FrameReply, I: f.I, S: status, H: h, B: body})
	}
	text := func(status int, msg string) {
		reply(status, http.Header{"Content-Type": {"text/plain; charset=utf-8"}}, []byte(msg))
	}
	if t.host == "" || !strings.HasPrefix(f.U, "/") {
		text(http.StatusBadRequest, "bad request")
		return
	}
	req, err := http.NewRequestWithContext(ctx, f.M, t.c.Local+f.U, bytes.NewReader(f.B))
	if err != nil {
		text(http.StatusBadRequest, "bad request")
		return
	}
	req.Header = t.header(f.H)
	res, err := localHTTP.Do(req)
	if err != nil {
		text(http.StatusBadGateway, "Mako on the Mac does not answer")
		return
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, t.c.MaxBody+1))
	if err != nil {
		text(http.StatusBadGateway, "Mako on the Mac does not answer")
		return
	}
	if int64(len(body)) > t.c.MaxBody {
		text(http.StatusBadGateway, "the answer is too large for the relay")
		return
	}
	reply(res.StatusCode, cleanHeader(res.Header), body)
}

// open: a phone's WebSocket, to Mako's own server, with the phone's Origin for it to check.
func (t *tunnel) open(f Frame) {
	fail := func() {
		t.send(Frame{T: FrameReply, I: f.I, S: http.StatusBadGateway, H: http.Header{"Content-Type": {"text/plain; charset=utf-8"}}, B: []byte("Mako on the Mac refused the connection")})
	}
	lu, err := url.Parse(t.c.Local)
	if err != nil || t.host == "" || !strings.HasPrefix(f.U, "/") {
		fail()
		return
	}
	lu.Scheme = strings.Replace(lu.Scheme, "http", "ws", 1)
	h := t.header(f.H)
	origin := h.Get("Origin")
	if origin == "" {
		origin = t.scheme + "://" + t.host
	}
	cfg, err := websocket.NewConfig(lu.Scheme+"://"+lu.Host+f.U, origin)
	if err != nil {
		fail()
		return
	}
	h.Del("Origin")
	cfg.Header = h
	local, err := websocket.DialConfig(cfg)
	if err != nil {
		fail()
		return
	}
	defer local.Close()
	st := &localStream{in: make(chan Frame, 256), gone: make(chan struct{})}
	t.mu.Lock()
	t.streams[f.I] = st
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.streams, f.I)
		t.mu.Unlock()
	}()
	if t.send(Frame{T: FrameReply, I: f.I, S: http.StatusSwitchingProtocols}) != nil {
		return
	}
	go func() { // the phone's messages to Mako
		defer local.Close()
		for {
			select {
			case m := <-st.in:
				if wsMessage.Send(local, wsMsg{data: m.B, text: m.X}) != nil {
					return
				}
			case <-st.gone:
				return
			}
		}
	}()
	for {
		var m wsMsg
		if wsMessage.Receive(local, &m) != nil {
			break
		}
		if t.send(Frame{T: FrameWSMsg, I: f.I, B: m.data, X: m.text}) != nil {
			break
		}
	}
	st.close()
	t.send(Frame{T: FrameWSClose, I: f.I})
}
