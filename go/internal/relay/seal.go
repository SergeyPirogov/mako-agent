package relay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"sync"
)

// Version is the end-to-end protocol: a frame or pairing request of another version is not this one.
const Version = 1

const (
	label = "mako-relay-v1"
	block = 256 // the plaintext is padded to a multiple of it, so a frame's size tells less about what is in it
	// reserveAhead: how many counter values a Sealer reserves at once. They are written down before use, so after
	// a crash the sender continues above them and never reuses a nonce; at worst it skips some numbers.
	reserveAhead = 1000
)

// Frame types of the pairing.
const (
	TypePair   = "pair"   // phone → Mac, under K_pair, counter 1
	TypePaired = "paired" // Mac → phone, under K_pair, counter 2
)

// Dir is a frame's direction. Each has its own key, so the counters of the two never meet on one key and a frame
// cannot be reflected back to its sender.
type Dir byte

const (
	P2M Dir = 1 // phone → Mac
	M2P Dir = 2 // Mac → phone
)

var (
	// ErrReplay: the frame's counter is not above the last accepted one, a replay or a frame that came too late.
	ErrReplay = errors.New("relay: a frame came again or out of order")
	// ErrTag: the frame does not open with this key, the relay or someone else changed or made it.
	ErrTag = errors.New("relay: the frame does not decrypt")
)

// Keys are a device's keys, derived from the pairing secret in the QR code.
type Keys struct {
	Pair []byte // only for the pairing frames
	P2M  []byte // phone → Mac
	M2P  []byte // Mac → phone
}

// DeriveKeys: HKDF-SHA256 of the QR's secret, salted with the pairing code (its text as bytes).
func DeriveKeys(secret, code []byte) (Keys, error) {
	if len(secret) < 32 {
		return Keys{}, errors.New("relay: the pairing secret is too short")
	}
	var k Keys
	for _, x := range []struct {
		key  *[]byte
		info string
	}{{&k.Pair, "pair"}, {&k.P2M, "p2m"}, {&k.M2P, "m2p"}} {
		b, err := hkdf.Key(sha256.New, secret, code, label+" "+x.info, 32)
		if err != nil {
			return Keys{}, err
		}
		*x.key = b
	}
	return k, nil
}

// MacID: what names the Mac in a frame's AAD, the host the phone reaches it at through the relay
// ("abc.relay.example.com"), from an origin such as "https://abc.relay.example.com".
func MacID(origin string) string {
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return strings.ToLower(origin)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("relay: a key must be 32 bytes")
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// nonce: 0x00000000 ‖ n, big-endian. A counter is used once per key, so a nonce never repeats.
func nonce(n uint64) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint64(b[4:], n)
	return b
}

// aad: "mako-relay-v1" ‖ v ‖ dir ‖ n ‖ mac ‖ d ‖ t ‖ i, each string with its 2-byte length first, so no two
// frames' fields read the same.
func aad(f Sealed, mac string, dir Dir) ([]byte, error) {
	b := append([]byte(label), f.V, byte(dir))
	b = binary.BigEndian.AppendUint64(b, f.N)
	for _, s := range []string{mac, f.D, f.T, f.I} {
		if len(s) > math.MaxUint16 {
			return nil, errors.New("relay: a frame field is too long")
		}
		b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
		b = append(b, s...)
	}
	return b, nil
}

// pad: 4-byte length ‖ payload ‖ zeros, up to a multiple of block.
func pad(payload []byte) []byte {
	size := (4 + len(payload) + block - 1) / block * block
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b, uint32(len(payload)))
	copy(b[4:], payload)
	return b
}

func unpad(b []byte) ([]byte, error) {
	if len(b) < 4 {
		return nil, ErrTag
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, ErrTag
	}
	return b[4 : 4+n], nil
}

func seal(a cipher.AEAD, mac string, dir Dir, f Sealed, payload []byte) (Sealed, error) {
	if len(payload) > math.MaxUint32-block {
		return Sealed{}, errors.New("relay: the frame is too big")
	}
	ad, err := aad(f, mac, dir)
	if err != nil {
		return Sealed{}, err
	}
	f.C = a.Seal(nil, nonce(f.N), pad(payload), ad)
	return f, nil
}

func open(a cipher.AEAD, mac string, dir Dir, f Sealed) ([]byte, error) {
	ad, err := aad(f, mac, dir)
	if err != nil {
		return nil, ErrTag
	}
	b, err := a.Open(nil, nonce(f.N), f.C, ad)
	if err != nil {
		return nil, ErrTag
	}
	return unpad(b)
}

// Sealer seals one device's frames in one direction, numbering them.
type Sealer struct {
	mu       sync.Mutex
	aead     cipher.AEAD
	dir      Dir
	mac, dev string
	n        uint64 // the last counter used
	reserved uint64 // the counters up to it are written down
	reserve  func(upto uint64) error
}

// NewSealer continues after from: the highest counter reserved before (0 for a new device), so nothing a previous
// run might have used is used again. reserve writes a new bound down before the counters under it are used; nil
// keeps them only in memory (tests, the phone side).
func NewSealer(key []byte, dir Dir, mac, dev string, from uint64, reserve func(upto uint64) error) (*Sealer, error) {
	a, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: a, dir: dir, mac: mac, dev: dev, n: from, reserved: from, reserve: reserve}, nil
}

// Seal: the next frame of type t for request or stream i, carrying payload.
func (s *Sealer) Seal(t, i string, payload []byte) (Sealed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n >= math.MaxUint64-reserveAhead {
		return Sealed{}, errors.New("relay: the counter ran out, pair the phone again")
	}
	next := s.n + 1
	if next > s.reserved {
		upto := s.n + reserveAhead
		if s.reserve != nil {
			if err := s.reserve(upto); err != nil {
				return Sealed{}, err
			}
		}
		s.reserved = upto
	}
	s.n = next
	return seal(s.aead, s.mac, s.dir, Sealed{V: Version, D: s.dev, T: t, I: i, N: next}, payload)
}

// Opener opens one device's frames in one direction, each counter once and only growing.
type Opener struct {
	mu       sync.Mutex
	aead     cipher.AEAD
	dir      Dir
	mac, dev string
	last     uint64
	accept   func(n uint64) error
}

// NewOpener takes frames above last, the last counter accepted before. accept writes a newly accepted counter down
// before the frame is let through (nil: only in memory), so a restart does not open the door to old frames.
func NewOpener(key []byte, dir Dir, mac, dev string, last uint64, accept func(n uint64) error) (*Opener, error) {
	a, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Opener{aead: a, dir: dir, mac: mac, dev: dev, last: last, accept: accept}, nil
}

// Open: the frame's payload. A counter that does not grow is refused before decrypting (ErrReplay); a frame that
// does not decrypt (ErrTag) leaves the counter as it was, so a forged frame cannot burn the numbers ahead.
func (o *Opener) Open(f Sealed) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if f.V != Version || f.D != o.dev {
		return nil, ErrTag
	}
	if f.N <= o.last {
		return nil, ErrReplay
	}
	payload, err := open(o.aead, o.mac, o.dir, f)
	if err != nil {
		return nil, err
	}
	if o.accept != nil {
		if err := o.accept(f.N); err != nil {
			return nil, err
		}
	}
	o.last = f.N
	return payload, nil
}

// PairRequest is what the phone sends sealed under K_pair: the code itself travels open in the frame's I, the
// relay needs it to find the Mac.
type PairRequest struct {
	Name  string `json:"name,omitempty"`
	Nonce []byte `json:"nonce"` // random, at least 16 bytes; the Mac's reply returns it
}

// PairReply is the Mac's sealed answer: the device it made, and the phone's nonce back, which proves the reply
// came from the Mac that knows the secret, not from a relay that saw the request go by.
type PairReply struct {
	ID    string `json:"id"`
	Nonce []byte `json:"nonce"`
}

// SealPair: the phone's pairing frame. A retry must send these same bytes again, not seal anew: a second sealing
// would reuse counter 1 under K_pair.
func SealPair(k Keys, mac, code string, req PairRequest) (Sealed, error) {
	a, err := newAEAD(k.Pair)
	if err != nil {
		return Sealed{}, err
	}
	b, _ := json.Marshal(req)
	return seal(a, mac, P2M, Sealed{V: Version, T: TypePair, I: code, N: 1}, b)
}

// OpenPair: the pairing request in the phone's frame, on the Mac.
func OpenPair(k Keys, mac string, f Sealed) (PairRequest, error) {
	var req PairRequest
	if f.V != Version || f.T != TypePair || f.N != 1 || f.D != "" {
		return req, ErrTag
	}
	a, err := newAEAD(k.Pair)
	if err != nil {
		return req, err
	}
	b, err := open(a, mac, P2M, f)
	if err != nil {
		return req, err
	}
	if err := json.Unmarshal(b, &req); err != nil || len(req.Nonce) < 16 {
		return req, fmt.Errorf("relay: a bad pairing request")
	}
	return req, nil
}

// SealPairReply: the Mac's answer to the pairing frame of code.
func SealPairReply(k Keys, mac, code string, r PairReply) (Sealed, error) {
	a, err := newAEAD(k.Pair)
	if err != nil {
		return Sealed{}, err
	}
	b, _ := json.Marshal(r)
	return seal(a, mac, M2P, Sealed{V: Version, T: TypePaired, I: code, N: 2}, b)
}

// OpenPairReply: the Mac's answer, on the phone.
func OpenPairReply(k Keys, mac, code string, f Sealed) (PairReply, error) {
	var r PairReply
	if f.V != Version || f.T != TypePaired || f.I != code || f.N != 2 || f.D != "" {
		return r, ErrTag
	}
	a, err := newAEAD(k.Pair)
	if err != nil {
		return r, err
	}
	b, err := open(a, mac, M2P, f)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, ErrTag
	}
	return r, nil
}
