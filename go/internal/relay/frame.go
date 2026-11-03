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
