package web

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
	"github.com/SergeyPirogov/mako-agent/go/internal/relay"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// recorder is an evil relay's tape: every byte it passes, both ways, a WebSocket's frames too.
type recorder struct {
	mu      sync.Mutex
	tape    bytes.Buffer
	lines   []string    // the HTTP requests it passed, "GET /path"
	sockets []*wsTape   // the WebSockets it carried
	tamper  atomic.Bool // hand out a changed /sw.js
}

// wsTape: the bytes of one WebSocket, up (phone → Mac, masked) and down.
type wsTape struct {
	io.ReadWriteCloser
	rec      *recorder
	up, down bytes.Buffer
}

func (w *wsTape) Read(p []byte) (int, error) {
	n, err := w.ReadWriteCloser.Read(p)
	w.rec.mu.Lock()
	w.down.Write(p[:n])
	w.rec.mu.Unlock()
	return n, err
}

func (w *wsTape) Write(p []byte) (int, error) {
	w.rec.mu.Lock()
	w.up.Write(p)
	w.rec.mu.Unlock()
	return w.ReadWriteCloser.Write(p)
}

func (rec *recorder) RoundTrip(r *http.Request) (*http.Response, error) {
	out, _ := httputil.DumpRequestOut(r, true)
	res, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.lines = append(rec.lines, r.Method+" "+r.URL.RequestURI())
	rec.tape.Write(out)
	if res.StatusCode == http.StatusSwitchingProtocols {
		in, _ := httputil.DumpResponse(res, false)
		rec.tape.Write(in)
		ws := &wsTape{ReadWriteCloser: res.Body.(io.ReadWriteCloser), rec: rec}
		rec.sockets = append(rec.sockets, ws)
		res.Body = ws
		return res, nil
	}
	if r.URL.Path == "/sw.js" && rec.tamper.Load() {
		body, _ := io.ReadAll(res.Body)
		body = append(body, "\n// the relay was here\n"...)
		res.Body = io.NopCloser(bytes.NewReader(body))
		res.ContentLength = int64(len(body))
		res.Header.Del("Content-Length")
	}
	in, _ := httputil.DumpResponse(res, true)
	rec.tape.Write(in)
	return res, nil
}

// wsFrames: the payloads of the data frames in one direction of a WebSocket's bytes, unmasked.
func wsFrames(b []byte) [][]byte {
	var out [][]byte
	for len(b) >= 2 {
		op, masked, n := b[0]&0x0f, b[1]&0x80 != 0, uint64(b[1]&0x7f)
		h := 2
		switch n {
		case 126:
			if len(b) < 4 {
				return out
			}
			n, h = uint64(binary.BigEndian.Uint16(b[2:])), 4
		case 127:
			if len(b) < 10 {
				return out
			}
			n, h = binary.BigEndian.Uint64(b[2:]), 10
		}
		var key []byte
		if masked {
			if len(b) < h+4 {
				return out
			}
			key, h = b[h:h+4], h+4
		}
		if uint64(len(b)-h) < n {
			return out
		}
		payload := bytes.Clone(b[h : h+int(n)])
		for i := range payload {
			if key != nil {
				payload[i] ^= key[i%4]
			}
		}
		if op == 1 || op == 2 || op == 0 {
			out = append(out, payload)
		}
		b = b[h+int(n):]
	}
	return out
}

// A phone pairs end to end through a relay that records all the traffic: it gets its device back sealed, with no
// cookie, and the tape holds neither the secret, nor the device's token, nor its cookie.
func TestPairingThroughAnEvilRelay(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	phonesDir := t.TempDir()
	phones, _ := phone.Open(phonesDir)
	mac := httptest.NewServer((&Server{D: d, Phones: phones}).Handler())
	defer mac.Close()
	target, _ := url.Parse(mac.URL)
	rec := &recorder{}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = rec
	evil := httptest.NewServer(proxy)
	defer evil.Close()

	// What the phone reads from the QR link's fragment.
	code, secret := phones.NewPairingKey()
	raw, _ := base64.RawURLEncoding.DecodeString(secret)
	k, _ := relay.DeriveKeys(raw, []byte(code))
	macID := relay.MacID(evil.URL)
	nonce := make([]byte, 16)
	rand.Read(nonce)

	post := func(f relay.Sealed) *http.Response {
		body, _ := json.Marshal(f)
		// The phone's page, served through the relay: its origin is the relay's.
		req, _ := http.NewRequest("POST", evil.URL+"/pair", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", evil.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	f, _ := relay.SealPair(k, macID, code, relay.PairRequest{Name: "iPhone", Nonce: nonce})
	// The relay changes a byte: no pairing, and the code is not burnt.
	bad := f
	bad.C = bytes.Clone(f.C)
	bad.C[5] ^= 1
	if res := post(bad); res.StatusCode != 401 {
		t.Fatalf("a changed pairing frame: %d", res.StatusCode)
	}
	if res := post(relay.Sealed{V: 2, T: relay.TypePair, I: code}); res.StatusCode != 400 {
		t.Fatalf("another protocol version: %d", res.StatusCode)
	}
	res := post(f)
	if res.StatusCode != 200 {
		t.Fatalf("pair: %d", res.StatusCode)
	}
	if len(res.Cookies()) != 0 {
		t.Fatalf("an end-to-end pairing set a cookie the relay reads: %v", res.Cookies())
	}
	var sealed relay.Sealed
	json.NewDecoder(res.Body).Decode(&sealed)
	reply, err := relay.OpenPairReply(k, macID, code, sealed)
	if err != nil || !bytes.Equal(reply.Nonce, nonce) || reply.ID == "" {
		t.Fatalf("reply %v %+v", err, reply)
	}
	// The reply got lost and the phone sends the same bytes again: the same reply, not a second device.
	res = post(f)
	var again relay.Sealed
	json.NewDecoder(res.Body).Decode(&again)
	if res.StatusCode != 200 || !bytes.Equal(again.C, sealed.C) || len(phones.Devices()) != 1 {
		t.Fatalf("the same pairing frame again: %d, %d devices", res.StatusCode, len(phones.Devices()))
	}
	// Another frame for the used code pairs nothing.
	other, _ := relay.SealPair(k, macID, code, relay.PairRequest{Name: "iPad", Nonce: nonce})
	if res := post(other); res.StatusCode != 401 {
		t.Fatalf("a used code paired again: %d", res.StatusCode)
	}
	// The Mac keeps the device with its keys: the phone's first frame opens.
	in, err := phones.Opener(reply.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := relay.NewSealer(k.P2M, relay.P2M, macID, reply.ID, 0, nil)
	first, _ := out.Seal("req", "1", []byte(`{"m":"GET","u":"/api/state"}`))
	if _, err := in.Open(first); err != nil {
		t.Fatalf("the phone's first frame: %v", err)
	}

	var kept struct{ Devices []phone.Device }
	data, _ := os.ReadFile(filepath.Join(phonesDir, "phones.json"))
	json.Unmarshal(data, &kept)
	if len(kept.Devices) != 1 || kept.Devices[0].ID != reply.ID || kept.Devices[0].Token == "" {
		t.Fatal("no device made")
	}
	token := kept.Devices[0].Token
	tape := rec.tape.String()
	if !strings.Contains(tape, "POST /pair") {
		t.Fatal("the relay recorded nothing")
	}
	for name, s := range map[string]string{"secret": secret, "token": token, "cookie": deviceCookie, "name": "iPhone"} {
		if strings.Contains(tape, s) {
			t.Errorf("the relay saw the %s", name)
		}
	}
}

// Until the phone's encrypted client is here, the old pairing through the relay still works.
func TestPairingTheOldWayStillWorks(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	phones, _ := phone.Open(t.TempDir())
	srv := httptest.NewServer((&Server{D: d, Phones: phones}).Handler())
	defer srv.Close()
	code, _ := phones.NewPairingKey()
	req, _ := http.NewRequest("POST", srv.URL+"/pair", strings.NewReader("code="+code))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", "100.64.0.2")
	req.Header.Set("Origin", srv.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 || len(res.Cookies()) == 0 {
		t.Fatalf("old pairing: %v %d", err, res.StatusCode)
	}
	// Whatever the page does with the secret (#475), POST /pair carries the code alone.
	if !strings.Contains(pairPage, `body: "code=" + encodeURIComponent(code)}`) {
		t.Fatal("the pair page must send the code alone")
	}
}
