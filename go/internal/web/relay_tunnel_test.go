package web

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
	"github.com/SergeyPirogov/mako-agent/go/internal/relay"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// With the relay's tunnel up, the settings show its address and the QR code pairs through it.
func TestPhoneSettingsUseTheRelay(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	phones, _ := phone.Open(t.TempDir())
	const public = "https://abcdefghijklmnop.relay.example.com"
	srv := httptest.NewServer((&Server{D: d, Phones: phones, Relay: func() string { return public }}).Handler())
	defer srv.Close()

	var st struct {
		URL   string            `json:"url"`
		Relay map[string]string `json:"relay"`
	}
	res, _ := http.Get(srv.URL + "/api/phone")
	json.NewDecoder(res.Body).Decode(&st)
	if st.URL != public || st.Relay["url"] != public {
		t.Fatalf("%+v", st)
	}
	var pairing struct {
		OK  bool   `json:"ok"`
		URL string `json:"url"`
		QR  string `json:"qr"`
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/phone/pairing", nil)
	req.Header.Set("Origin", srv.URL) // the settings page's own request, as the browser sends it
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("pairing: %v %v", res, err)
	}
	json.NewDecoder(res.Body).Decode(&pairing)
	if !pairing.OK || !strings.HasPrefix(pairing.URL, public+"/pair#code=") || !strings.Contains(pairing.URL, "&k=") || pairing.QR == "" {
		t.Fatalf("%+v", pairing)
	}
}

// Through the relay and the Mac's tunnel a phone is a phone: Mako asks it for its key and keeps the settings on the
// Mac, though the tunnel reaches Mako from 127.0.0.1.
func TestPhoneThroughTheTunnelIsRemote(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	phones, _ := phone.Open(t.TempDir())
	mako := httptest.NewServer((&Server{D: d, Phones: phones}).Handler())
	defer mako.Close()
	rs := httptest.NewServer(&relay.Server{Domain: "relay.test", Log: log.New(io.Discard, "", 0)})
	defer rs.Close()

	token, _ := relay.LoadToken(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &relay.Client{Addr: rs.URL, Token: token, Local: mako.URL, Log: log.New(io.Discard, "", 0)}
	go c.Run(ctx)
	for end := time.Now().Add(5 * time.Second); c.URL() == "" && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	if c.URL() == "" {
		t.Fatal("no tunnel")
	}
	get := func(path string) int {
		req, _ := http.NewRequest("GET", rs.URL+path, nil)
		req.Host = relay.IDFor(token) + ".relay.test"
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, p := range []string{"/", "/api/state", "/settings", "/api/phone"} {
		if got := get(p); got != http.StatusUnauthorized {
			t.Errorf("%s through the relay: %d, want 401", p, got)
		}
	}
	if got := get("/pair"); got != http.StatusOK {
		t.Errorf("/pair through the relay: %d", got)
	}
}
