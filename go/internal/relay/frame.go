// Package relay carries Mako between a phone and the Mac through a shared server in the middle. The relay passes
// bytes it cannot read or change: everything between the phone and the Mac is sealed with the device's keys
// (docs/relay-e2e.md).
package relay

// Sealed is a frame on the wire. The relay routes it by T, I and D and never reads C: the request or the answer
// itself (method, path, headers with the device's cookie, body, WebSocket message) lies encrypted inside C.
// V, D, T, I and N travel in the clear but are authenticated: changing any of them breaks the tag.
type Sealed struct {
	V uint8  `json:"v"`           // protocol version (Version)
	D string `json:"d,omitempty"` // the device, so the Mac knows whose keys open the frame; empty while pairing
	T string `json:"t"`           // the frame's type
	I string `json:"i,omitempty"` // the request or stream; an answer carries its request's I
	N uint64 `json:"n"`           // the sender's counter: it grows with every frame in a direction
	C []byte `json:"c"`           // ciphertext ‖ GCM tag
}

// Frame types of the tunnel, after the pairing. Each request, answer or stream message is one Sealed frame; its
// I is the request's or the stream's id, random so a replayed old answer finds nobody waiting for it.
const (
	TypeRequest  = "req" // phone → Mac: an HTTP request (Request)
	TypeResponse = "res" // Mac → phone: its answer (Response), with the request's I
	TypeOpen     = "wso" // phone → Mac: open a WebSocket stream (Request with U only)
	TypeMessage  = "wsm" // either way: a message of the stream (Message)
	TypeClose    = "wsc" // either way: the stream ends
)

// Request is what a "req" or "wso" frame carries: what would be seen in HTTP, now inside the ciphertext.
type Request struct {
	M   string            `json:"m,omitempty"`   // the method; GET when empty
	U   string            `json:"u"`             // the path and query
	H   map[string]string `json:"h,omitempty"`   // the headers
	B   []byte            `json:"b,omitempty"`   // the body
	Nav bool              `json:"nav,omitempty"` // a page navigation: may come from another site's link
}

// Response is the Mac's answer to a Request.
type Response struct {
	S int               `json:"s"` // the status
	H map[string]string `json:"h,omitempty"`
	B []byte            `json:"b,omitempty"`
}

// Message is one WebSocket message of a stream.
type Message struct {
	WS []byte `json:"ws"`
}
