package relay

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const mac = "abc.relay.example.com"

func keys(t *testing.T, code string) Keys {
	t.Helper()
	secret := make([]byte, 32)
	rand.Read(secret)
	k, err := DeriveKeys(secret, []byte(code))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pair(t *testing.T, k Keys, dev string) (*Sealer, *Opener) {
	t.Helper()
	s, err := NewSealer(k.P2M, P2M, mac, dev, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	o, err := NewOpener(k.P2M, P2M, mac, dev, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, o
}

func TestDeriveKeys(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	a, _ := DeriveKeys(secret, []byte("code"))
	b, _ := DeriveKeys(secret, []byte("code"))
	if !bytes.Equal(a.P2M, b.P2M) || !bytes.Equal(a.M2P, b.M2P) || !bytes.Equal(a.Pair, b.Pair) {
		t.Fatal("the same secret and code must give the same keys on the phone and on the Mac")
	}
	if bytes.Equal(a.P2M, a.M2P) || bytes.Equal(a.P2M, a.Pair) || bytes.Equal(a.M2P, a.Pair) || len(a.P2M) != 32 {
		t.Fatal("each direction needs its own 32-byte key")
	}
	if c, _ := DeriveKeys(secret, []byte("other")); bytes.Equal(c.P2M, a.P2M) {
		t.Fatal("the code is the salt")
	}
	if _, err := DeriveKeys(secret[:16], nil); err == nil {
		t.Fatal("a short secret was taken")
	}
}

func TestSealRoundTripAndPadding(t *testing.T) {
	s, o := pair(t, keys(t, "c"), "dev1")
	for _, size := range []int{0, 1, 251, 252, 253, 5000} {
		payload := bytes.Repeat([]byte{'x'}, size)
		f, err := s.Seal("req", "r1", payload)
		if err != nil {
			t.Fatal(err)
		}
		if (len(f.C)-16)%block != 0 {
			t.Fatalf("%d bytes sealed into %d: not padded to %d", size, len(f.C), block)
		}
		got, err := o.Open(f)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("%d bytes: %v", size, err)
		}
	}
}

// Whatever the relay changes in a frame, open or sealed, the frame does not open.
func TestTamperedFrameIsRefused(t *testing.T) {
	k := keys(t, "c")
	s, o := pair(t, k, "dev1")
	f, _ := s.Seal("req", "r1", []byte(`{"m":"GET","u":"/api/state"}`))
	for name, change := range map[string]func(*Sealed){
		"ciphertext": func(f *Sealed) { f.C[3] ^= 1 },
		"tag":        func(f *Sealed) { f.C[len(f.C)-1] ^= 1 },
		"type":       func(f *Sealed) { f.T = "res" },
		"request":    func(f *Sealed) { f.I = "r2" },
		"device":     func(f *Sealed) { f.D = "dev2" },
		"counter":    func(f *Sealed) { f.N = 2 },
		"version":    func(f *Sealed) { f.V = 2 },
		"truncated":  func(f *Sealed) { f.C = f.C[:len(f.C)-1] },
	} {
		g := f
		g.C = bytes.Clone(f.C)
		change(&g)
		if _, err := o.Open(g); !errors.Is(err, ErrTag) {
			t.Errorf("%s changed: %v", name, err)
		}
	}
	// The forgeries did not burn the counter: the true frame still opens.
	if _, err := o.Open(f); err != nil {
		t.Fatalf("the true frame after forgeries: %v", err)
	}
	// The same frame for another Mac does not open either.
	other, _ := NewOpener(k.P2M, P2M, "xyz.relay.example.com", "dev1", 0, nil)
	if _, err := other.Open(f); !errors.Is(err, ErrTag) {
		t.Fatalf("another Mac's id: %v", err)
	}
}

func TestReplayIsRefused(t *testing.T) {
	s, o := pair(t, keys(t, "c"), "dev1")
	f1, _ := s.Seal("req", "1", []byte("a"))
	f2, _ := s.Seal("req", "2", []byte("b"))
	f3, _ := s.Seal("req", "3", []byte("c"))
	if _, err := o.Open(f2); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Open(f2); !errors.Is(err, ErrReplay) {
		t.Fatalf("the same frame twice: %v", err)
	}
	if _, err := o.Open(f1); !errors.Is(err, ErrReplay) {
		t.Fatalf("an older frame after a newer one: %v", err)
	}
	// A gap is allowed: the relay may have lost a frame.
	if _, err := o.Open(f3); err != nil {
		t.Fatal(err)
	}
}

func TestAnotherDeviceDoesNotOpen(t *testing.T) {
	s, _ := pair(t, keys(t, "c1"), "dev1")
	_, o := pair(t, keys(t, "c2"), "dev1")
	f, _ := s.Seal("req", "1", []byte("hello"))
	if _, err := o.Open(f); !errors.Is(err, ErrTag) {
		t.Fatalf("another device's key opened the frame: %v", err)
	}
}

// A frame cannot be sent back to its sender: the other direction has another key.
func TestReflectionIsRefused(t *testing.T) {
	k := keys(t, "c")
	s, _ := NewSealer(k.P2M, P2M, mac, "dev1", 0, nil)
	back, _ := NewOpener(k.M2P, M2P, mac, "dev1", 0, nil)
	f, _ := s.Seal("req", "1", []byte("hello"))
	if _, err := back.Open(f); !errors.Is(err, ErrTag) {
		t.Fatalf("a reflected frame opened: %v", err)
	}
}

// Counters, and so nonces, never repeat under a key, a restart in the middle included: the sender continues above
// what it reserved, not above what it used.
func TestNoncesDoNotRepeat(t *testing.T) {
	k := keys(t, "c")
	var stored uint64
	reserve := func(from, upto uint64) error {
		if from != stored {
			return errors.New("another sealer took the counters")
		}
		stored = upto
		return nil
	}
	seen := map[uint64]bool{}
	s, _ := NewSealer(k.M2P, M2P, mac, "dev1", 0, reserve)
	for range 2500 {
		f, _ := s.Seal("wsmsg", "s1", nil)
		if seen[f.N] {
			t.Fatalf("counter %d twice", f.N)
		}
		seen[f.N] = true
	}
	if stored < 2500 {
		t.Fatalf("reserved up to %d after 2500 frames", stored)
	}
	// The Mac crashes and starts again from what phones.json says.
	s, _ = NewSealer(k.M2P, M2P, mac, "dev1", stored, reserve)
	o, _ := NewOpener(k.M2P, M2P, mac, "dev1", 2500, nil)
	for range 7500 {
		f, _ := s.Seal("wsmsg", "s1", nil)
		if seen[f.N] {
			t.Fatalf("counter %d twice after a restart", f.N)
		}
		seen[f.N] = true
		if _, err := o.Open(f); err != nil {
			t.Fatalf("the phone refused a frame after the Mac's restart: %v", err)
		}
	}
	// A failed reservation seals nothing.
	s, _ = NewSealer(k.M2P, M2P, mac, "dev1", stored, func(uint64, uint64) error { return errors.New("disk full") })
	if _, err := s.Seal("res", "1", nil); err == nil {
		t.Fatal("sealed without writing the counter down")
	}
}

func TestAcceptedCounterIsWrittenFirst(t *testing.T) {
	k := keys(t, "c")
	s, _ := NewSealer(k.P2M, P2M, mac, "dev1", 0, nil)
	f, _ := s.Seal("req", "1", nil)
	o, _ := NewOpener(k.P2M, P2M, mac, "dev1", 0, func(uint64, uint64) error { return errors.New("disk full") })
	if _, err := o.Open(f); err == nil {
		t.Fatal("let a frame through without writing its counter down")
	}
	if _, err := o.Open(f); err == nil {
		t.Fatal("a frame that was not written down was taken the second time")
	}
	var from, upto uint64
	o, _ = NewOpener(k.P2M, P2M, mac, "dev1", 0, func(f, u uint64) error { from, upto = f, u; return nil })
	if _, err := o.Open(f); err != nil || from != 0 || upto < f.N {
		t.Fatalf("%v, written %d..%d", err, from, upto)
	}
}

// The counters are written down in blocks, not one by one: a frame under the written bound costs no write.
func TestOpenerWritesCountersInBlocks(t *testing.T) {
	k := keys(t, "blocks")
	s, _ := NewSealer(k.P2M, P2M, mac, "dev1", 0, nil)
	var calls [][2]uint64
	bound := uint64(0)
	o, _ := NewOpener(k.P2M, P2M, mac, "dev1", 0, func(from, upto uint64) error {
		if from != bound {
			t.Fatalf("reserved from %d, the bound was %d", from, bound)
		}
		calls = append(calls, [2]uint64{from, upto})
		bound = upto
		return nil
	})
	var last Sealed
	for range recvAhead {
		last, _ = s.Seal("req", "1", nil)
		if _, err := o.Open(last); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 1 || calls[0][1] < last.N {
		t.Fatalf("%d frames in one block wrote %v", recvAhead, calls)
	}
	for last.N < calls[0][1] { // up to the bound still costs no write
		last, _ = s.Seal("req", "1", nil)
		if _, err := o.Open(last); err != nil || len(calls) != 1 {
			t.Fatalf("%v, wrote %v", err, calls)
		}
	}
	f, _ := s.Seal("req", "1", nil) // above the bound: the next block
	if _, err := o.Open(f); err != nil || len(calls) != 2 || calls[1][0] != calls[0][1] {
		t.Fatalf("%v, wrote %v", err, calls)
	}
	// A restart continues from the bound: what was under it is not opened again, even if never taken.
	again, _ := NewOpener(k.P2M, P2M, mac, "dev1", bound, nil)
	g, _ := s.Seal("req", "1", nil)
	if _, err := again.Open(g); !errors.Is(err, ErrReplay) {
		t.Fatalf("a frame under the bound after a restart: %v", err)
	}
}

// A counter that could not be written down does not move: the same frame opens once the disk is back.
func TestOpenerRefusedWriteKeepsTheCounter(t *testing.T) {
	k := keys(t, "refused")
	s, _ := NewSealer(k.P2M, P2M, mac, "dev1", 0, nil)
	fail := true
	o, _ := NewOpener(k.P2M, P2M, mac, "dev1", 0, func(uint64, uint64) error {
		if fail {
			return errors.New("disk full")
		}
		return nil
	})
	f, _ := s.Seal("req", "1", nil)
	if _, err := o.Open(f); err == nil {
		t.Fatal("opened without writing down")
	}
	fail = false
	if _, err := o.Open(f); err != nil {
		t.Fatalf("after the disk came back: %v", err)
	}
}

// Flush brings the bound down to the last counter taken, so a restart after a clean close refuses only what was taken.
func TestOpenerFlush(t *testing.T) {
	k := keys(t, "flush")
	s, _ := NewSealer(k.P2M, P2M, mac, "dev1", 0, nil)
	bound := uint64(0)
	o, _ := NewOpener(k.P2M, P2M, mac, "dev1", 0, func(from, upto uint64) error {
		if from != bound {
			t.Fatalf("reserved from %d, the bound was %d", from, bound)
		}
		bound = upto
		return nil
	})
	var last Sealed
	for range 3 {
		last, _ = s.Seal("req", "1", nil)
		o.Open(last)
	}
	if bound <= last.N {
		t.Fatalf("the bound %d is not ahead of %d", bound, last.N)
	}
	if err := o.Flush(); err != nil || bound != last.N {
		t.Fatalf("flush: %v, bound %d, last %d", err, bound, last.N)
	}
	if err := o.Flush(); err != nil { // nothing to write the second time
		t.Fatal(err)
	}
	again, _ := NewOpener(k.P2M, P2M, mac, "dev1", bound, nil)
	next, _ := s.Seal("req", "1", nil)
	if _, err := again.Open(next); err != nil {
		t.Fatalf("the next frame after a restart: %v", err)
	}
	if _, err := again.Open(last); !errors.Is(err, ErrReplay) {
		t.Fatalf("the last frame again: %v", err)
	}
}

func TestPairing(t *testing.T) {
	k := keys(t, "code1")
	nonce := bytes.Repeat([]byte{1}, 16)
	f, err := SealPair(k, mac, "code1", PairRequest{Name: "iPhone", Nonce: nonce})
	if err != nil || f.I != "code1" || f.T != TypePair {
		t.Fatalf("%v %+v", err, f)
	}
	req, err := OpenPair(k, mac, f)
	if err != nil || req.Name != "iPhone" || !bytes.Equal(req.Nonce, nonce) {
		t.Fatalf("%v %+v", err, req)
	}
	if _, err := OpenPair(keys(t, "code1"), mac, f); err == nil {
		t.Fatal("another secret opened the pairing")
	}
	bad := f
	bad.C = bytes.Clone(f.C)
	bad.C[0] ^= 1
	if _, err := OpenPair(k, mac, bad); err == nil {
		t.Fatal("a changed pairing frame opened")
	}
	short, _ := SealPair(k, mac, "code1", PairRequest{Nonce: []byte{1}})
	if _, err := OpenPair(k, mac, short); err == nil {
		t.Fatal("a pairing without a proper nonce")
	}
	reply, _ := SealPairReply(k, mac, "code1", PairReply{ID: "dev1", Nonce: req.Nonce})
	if bytes.Equal(nonceOf(reply), nonceOf(f)) {
		t.Fatal("the request and the reply share a nonce under K_pair")
	}
	got, err := OpenPairReply(k, mac, "code1", reply)
	if err != nil || got.ID != "dev1" || !bytes.Equal(got.Nonce, nonce) {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := OpenPairReply(k, mac, "code2", reply); err == nil {
		t.Fatal("a reply for another code")
	}
	// The request reflected back as a reply does not open.
	f.T, f.N = TypePaired, 2
	if _, err := OpenPairReply(k, mac, "code1", f); err == nil {
		t.Fatal("the phone's own frame opened as the Mac's reply")
	}
}

func nonceOf(f Sealed) []byte { return nonce(f.N) }

// An evil relay records everything both ways: neither the device's cookie, nor the paths, nor the bodies are in it.
func TestEvilRelaySeesNothing(t *testing.T) {
	k := keys(t, "c")
	const cookie, path, body, answer = "mako_device=s3cr3t-t0ken", "/api/tasks/42/comment", "the plan is in the drawer", "you have 3 new chats"
	toMac, _ := NewSealer(k.P2M, P2M, mac, "dev1", 0, nil)
	toPhone, _ := NewSealer(k.M2P, M2P, mac, "dev1", 0, nil)
	request, _ := json.Marshal(map[string]any{"m": "POST", "u": path, "h": map[string]string{"Cookie": cookie}, "b": []byte(body)})
	response, _ := json.Marshal(map[string]any{"s": 200, "b": []byte(answer)})
	var wire bytes.Buffer
	for _, f := range []func() (Sealed, error){
		func() (Sealed, error) { return toMac.Seal("req", "1", request) },
		func() (Sealed, error) { return toPhone.Seal("res", "1", response) },
		func() (Sealed, error) { return toPhone.Seal("wsmsg", "2", []byte("ls -la ~/secret")) },
	} {
		sealed, err := f()
		if err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(&wire).Encode(sealed)
	}
	for _, s := range []string{cookie, "s3cr3t", path, "/api/", body, "drawer", answer, "ls -la", "Cookie", "POST"} {
		if strings.Contains(wire.String(), s) {
			t.Errorf("the relay sees %q", s)
		}
	}
}
