package phone

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
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
