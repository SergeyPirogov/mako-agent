package phone

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/SergeyPirogov/mako-agent/go/internal/relay"
)

func TestPairing(t *testing.T) {
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil || p.VAPIDPublic() == "" {
		t.Fatalf("%v %q", err, p.VAPIDPublic())
	}
	code := p.NewPairing()
	d, err := p.Pair(code, "iPhone", "https://mac.tail1234.ts.net")
	if err != nil || d.Token == "" {
		t.Fatal(err)
	}
	if _, err := p.Pair(code, "iPhone", "https://mac.tail1234.ts.net"); err == nil {
		t.Fatal("a pairing code worked twice")
	}
	if p.ByToken(d.Token) == nil || p.ByToken("nope") != nil || p.ByToken("") != nil {
		t.Fatal("token check")
	}
	// Kept across restarts, with the same push keys.
	again, _ := Open(dir)
	if again.ByToken(d.Token) == nil || again.VAPIDPublic() != p.VAPIDPublic() {
		t.Fatal("not kept")
	}
	if list := again.Devices(); len(list) != 1 || list[0].Token != "" {
		t.Fatalf("devices list must not carry tokens: %+v", list)
	}
	again.Remove(d.ID)
	if again.ByToken(d.Token) != nil {
		t.Fatal("removed device still in")
	}
}

func TestPush(t *testing.T) {
	p, _ := Open(t.TempDir())
	a, _ := p.Pair(p.NewPairing(), "iPhone", "")
	b, _ := p.Pair(p.NewPairing(), "iPad", "")
	p.Pair(p.NewPairing(), "no pushes", "")
	p.Subscribe(a.Token, &webpush.Subscription{Endpoint: "https://web.push.apple.com/a"})
	p.Subscribe(b.Token, &webpush.Subscription{Endpoint: "https://web.push.apple.com/gone"})
	var sent []string
	p.Send = func(ctx context.Context, msg []byte, sub *webpush.Subscription, o *webpush.Options) (*http.Response, error) {
		var m Message
		json.Unmarshal(msg, &m)
		sent = append(sent, sub.Endpoint+" "+m.Title+" "+m.URL)
		if o.VAPIDPrivateKey == "" || !strings.HasPrefix(o.Subscriber, "https://") {
			t.Errorf("vapid options %+v", o)
		}
		status := 201
		if strings.HasSuffix(sub.Endpoint, "gone") {
			status = 410
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	errs := p.Push(context.Background(), Message{Title: "✋ grp/app#5: агент чекає на вас", URL: "/#grp/app#5"})
	if len(sent) != 2 || len(errs) != 1 {
		t.Fatalf("sent %v errs %v", sent, errs)
	}
	// The gone subscription is dropped: the next push goes to one phone.
	sent = nil
	p.Push(context.Background(), Message{Title: "x"})
	if len(sent) != 1 {
		t.Fatalf("after 410: %v", sent)
	}
}

// A phones.json from before Mako kept the origin opens fine, keeps its phones and marks them as paired through
// Tailscale; a phone paired at another address is not marked.
func TestOldPhonesFileIsTailscale(t *testing.T) {
	dir := t.TempDir()
	old := `{"vapidPublic":"pub","vapidPrivate":"priv","devices":[{"id":"a","name":"iPhone","token":"tok","created":1,"lastSeen":2}]}`
	if err := os.WriteFile(filepath.Join(dir, "phones.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	list := p.Devices()
	if len(list) != 1 || list[0].ID != "a" || !list[0].ViaTailscale() || !p.HasTailscaleDevices() {
		t.Fatalf("old device: %+v", list)
	}
	if p.ByToken("tok") == nil {
		t.Fatal("old device lost its key")
	}
	p.Remove("a")
	d, err := p.Pair(p.NewPairing(), "Android", "https://relay.example.com")
	if err != nil || d.ViaTailscale() || p.HasTailscaleDevices() {
		t.Fatalf("a device paired through another address: %+v %v", d, err)
	}
	again, _ := Open(dir)
	if got := again.Devices(); len(got) != 1 || got[0].Origin != "https://relay.example.com" {
		t.Fatalf("origin not kept: %+v", got)
	}
}

// The device keeps the keys derived from the QR's secret, the same ones the phone derives from the link.
func TestPairingKeepsTheKeys(t *testing.T) {
	dir := t.TempDir()
	p, _ := Open(dir)
	code, secret := p.NewPairingKey()
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(raw) != 32 {
		t.Fatalf("secret %q: %v", secret, err)
	}
	want, _ := relay.DeriveKeys(raw, []byte(code))
	d, err := p.Pair(code, "iPhone", "https://abc.relay.example.com")
	if err != nil || !bytes.Equal(d.KeyP2M, want.P2M) || !bytes.Equal(d.KeyM2P, want.M2P) {
		t.Fatalf("keys not kept: %v", err)
	}
	again, _ := Open(dir)
	if list := again.Devices(); len(list) != 1 || list[0].KeyP2M != nil || list[0].KeyM2P != nil {
		t.Fatalf("the devices list must not carry keys: %+v", list)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "phones.json"))
	if strings.Contains(string(data), secret) || strings.Contains(string(data), code) {
		t.Fatal("the secret or the one-time code is kept once paired")
	}
}

func TestPairSealed(t *testing.T) {
	p, _ := Open(t.TempDir())
	const origin = "https://abc.relay.example.com"
	code, secret := p.NewPairingKey()
	raw, _ := base64.RawURLEncoding.DecodeString(secret)
	k, _ := relay.DeriveKeys(raw, []byte(code))
	nonce := bytes.Repeat([]byte{9}, 16)
	f, _ := relay.SealPair(k, relay.MacID(origin), code, relay.PairRequest{Name: "Serhii's iPhone", Nonce: nonce})
	// A changed frame does not pair and does not burn the code.
	bad := f
	bad.C = bytes.Clone(f.C)
	bad.C[0] ^= 1
	if _, _, err := p.PairSealed(bad, "iPhone", origin); err == nil {
		t.Fatal("a changed frame paired")
	}
	d, reply, err := p.PairSealed(f, "iPhone", origin)
	if err != nil || d.Name != "Serhii's iPhone" || !bytes.Equal(d.KeyP2M, k.P2M) {
		t.Fatalf("%v %+v", err, d)
	}
	got, err := relay.OpenPairReply(k, relay.MacID(origin), code, reply)
	if err != nil || got.ID != d.ID || !bytes.Equal(got.Nonce, nonce) {
		t.Fatalf("reply %v %+v", err, got)
	}
	if _, _, err := p.PairSealed(f, "iPhone", origin); err == nil {
		t.Fatal("a pairing code worked twice")
	}
}

// The counters live in phones.json: after a restart the Mac neither reuses a nonce nor takes an old frame again,
// and a removed device's frames stop.
func TestCountersSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	p, _ := Open(dir)
	const origin = "https://abc.relay.example.com"
	code, secret := p.NewPairingKey()
	raw, _ := base64.RawURLEncoding.DecodeString(secret)
	k, _ := relay.DeriveKeys(raw, []byte(code))
	d, _ := p.Pair(code, "iPhone", origin)

	phoneOut, _ := relay.NewSealer(k.P2M, relay.P2M, relay.MacID(origin), d.ID, 0, nil)
	f1, _ := phoneOut.Seal("req", "1", []byte("a"))
	in, err := p.Opener(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Open(f1); err != nil {
		t.Fatal(err)
	}
	out, _ := p.Sealer(d.ID)
	used := map[uint64]bool{}
	for range 3 {
		r, _ := out.Seal("res", "1", nil)
		used[r.N] = true
	}

	again, _ := Open(dir)
	in, _ = again.Opener(d.ID)
	if _, err := in.Open(f1); !errors.Is(err, relay.ErrReplay) {
		t.Fatalf("an old frame after a restart: %v", err)
	}
	out, _ = again.Sealer(d.ID)
	phoneIn, _ := relay.NewOpener(k.M2P, relay.M2P, relay.MacID(origin), d.ID, 3, nil)
	r, err := out.Seal("res", "2", []byte("b"))
	if err != nil || used[r.N] {
		t.Fatalf("counter %d after a restart: %v", r.N, err)
	}
	if _, err := phoneIn.Open(r); err != nil {
		t.Fatalf("the phone refused the Mac's frame: %v", err)
	}

	again.Remove(d.ID)
	f2, _ := phoneOut.Seal("req", "2", nil)
	if _, err := in.Open(f2); err == nil {
		t.Fatal("a removed device's frame went through")
	}
	if _, err := again.Opener(d.ID); err == nil {
		t.Fatal("a removed device still has keys")
	}
}
