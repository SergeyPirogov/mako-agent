package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/net/websocket"
)

// The tunnel: the Mac keeps one WebSocket to the relay (ConnectPath) and the relay hands it what a phone opens at
// https://<id>.<domain>. Each message on it is a Frame in JSON. Whatever the phone seals end to end (docs/relay-e2e.md)
// rides inside B, so the relay sees no more than a proxy in front of Mako would.

// ConnectPath: where the Mac opens its tunnel, with "Authorization: Bearer <token>". Paths under /_relay/ belong to
// the relay on every host; Mako has none of its own.
const ConnectPath = "/_relay/connect"

// Frame types of the tunnel.
const (
	FrameHello   = "hello" // relay → Mac, first: U is the Mac's public address, https://<id>.<domain>
	FrameRequest = "req"   // relay → Mac: a phone's HTTP request (M, U, H, B)
	FrameReply   = "res"   // Mac → relay: the answer to request I (S, H, B)
	FrameWSOpen  = "wso"   // relay → Mac: a phone opened a WebSocket at U with H
	FrameWSMsg   = "wsm"   // either way: a message of stream I, X tells it is text
	FrameWSClose = "wsc"   // either way: stream I is over
	FramePing    = "ping"  // relay → Mac, every Limits.Ping: the tunnel is alive
	FramePong    = "pong"  // Mac → relay
)

// Frame is one message of the tunnel.
type Frame struct {
	T string      `json:"t"`
	I uint64      `json:"i,omitempty"` // the request or stream
	M string      `json:"m,omitempty"` // HTTP method
	U string      `json:"u,omitempty"` // path and query; in hello, the public address
	H http.Header `json:"h,omitempty"`
	B []byte      `json:"b,omitempty"`
	S int         `json:"s,omitempty"` // HTTP status
	X bool        `json:"x,omitempty"` // a text WebSocket message (else binary)
}

// IDFor: the Mac's name on the relay, from its token. The relay derives it too, so nobody claims another Mac's
// address without its token, and the relay keeps no list of Macs.
func IDFor(token string) string {
	sum := sha256.Sum256([]byte(token))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:10]))
}

// validID: what IDFor can return, 16 characters of lower-case base32.
func validID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// LoadToken: the Mac's token for the relay, made once and kept in <stateDir>/relay.json.
func LoadToken(stateDir string) (string, error) {
	file := filepath.Join(stateDir, "relay.json")
	var saved struct {
		Token string `json:"token"`
	}
	if b, err := os.ReadFile(file); err == nil {
		if err := json.Unmarshal(b, &saved); err == nil && saved.Token != "" {
			return saved.Token, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	saved.Token = base64.RawURLEncoding.EncodeToString(raw)
	b, _ := json.Marshal(saved)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(file, b, 0o600); err != nil {
		return "", err
	}
	return saved.Token, nil
}

// wsMessage reads a WebSocket message and tells whether it was text: websocket.Message hides the frame's type.
var wsMessage = websocket.Codec{
	Marshal: func(v any) ([]byte, byte, error) {
		m := v.(wsMsg)
		if m.text {
			return m.data, websocket.TextFrame, nil
		}
		return m.data, websocket.BinaryFrame, nil
	},
	Unmarshal: func(data []byte, payloadType byte, v any) error {
		m := v.(*wsMsg)
		m.data, m.text = append([]byte(nil), data...), payloadType == websocket.TextFrame
		return nil
	},
}

type wsMsg struct {
	data []byte
	text bool
}

// hopHeaders: what belongs to one hop and is not passed on.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version",
	"Sec-Websocket-Extensions", "Sec-Websocket-Accept", "Sec-Websocket-Protocol"}

func cleanHeader(h http.Header) http.Header {
	h = h.Clone()
	if h == nil {
		h = http.Header{}
	}
	for _, f := range strings.Split(h.Get("Connection"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			h.Del(f)
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
	return h
}
