package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
)

func TestTerminal(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	s, err := term.Start(term.Options{ID: "a#1", Worktree: t.TempDir(), Command: "/bin/sh", Token: "tok",
		Args: []string{"-c", `echo ready; read x; echo "typed $x"; sleep 5`}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	d.Sessions.Put(s)
	srv := httptest.NewServer((&Server{D: d}).Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/term?id=a%231"

	// Another site in the browser cannot type into the agent.
	if _, err := websocket.Dial(wsURL, "", "https://evil.example"); err == nil {
		t.Fatal("cross-site terminal opened")
	}
	c, err := websocket.Dial(wsURL, "", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	var got string
	read := func(want string) {
		for !strings.Contains(got, want) {
			var e struct{ T, D string }
			if err := websocket.JSON.Receive(c, &e); err != nil {
				t.Fatalf("waiting for %q in %q: %v", want, got, err)
			}
			got += e.D
		}
	}
	read("ready")
	websocket.Message.Send(c, `{"t":"i","d":"hello\r"}`)
	read("typed hello")

	// The agent's hooks need its token.
	res, _ := http.Post(srv.URL+"/hook?id=a%231&token=bad", "application/json", strings.NewReader(`{"hook_event_name":"Stop"}`))
	if res.StatusCode != 403 {
		t.Fatalf("hook with a wrong token: %d", res.StatusCode)
	}
	res, _ = http.Post(srv.URL+"/hook?id=a%231&token=tok", "application/json", strings.NewReader(`{"hook_event_name":"Notification","message":"Claude is waiting for your input"}`))
	if res.StatusCode != 204 || s.State().State != "waiting" {
		t.Fatalf("hook: %d, %+v", res.StatusCode, s.State())
	}
	if res, _ := http.Get(srv.URL + "/vendor/xterm.js"); res.StatusCode != 200 {
		t.Fatalf("xterm.js: %d", res.StatusCode)
	}
}
