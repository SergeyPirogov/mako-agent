package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
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

// The push guard answers only the agent it gave a token to.
func TestGuardNeedsToken(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d}).Handler())
	defer srv.Close()
	for _, q := range []string{"id=a%231", "id=a%231&token=", "id=a%231&token=bad"} {
		res, err := http.Post(srv.URL+"/guard?"+q, "application/json", strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"git push"}}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 403 {
			t.Errorf("%s: %d", q, res.StatusCode)
		}
	}
}

// The worktree's pre-push hook: a token Mako did not give stops the push, and the body says why.
func TestPrePushNeedsToken(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d}).Handler())
	defer srv.Close()
	for _, q := range []string{"id=a%231", "id=a%231&token=", "id=a%231&token=bad"} {
		res, err := http.Post(srv.URL+"/prepush?"+q, "text/plain", strings.NewReader("refs/heads/x 1 refs/heads/x 0\n"))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 403 || !strings.Contains(string(body), "Mako stopped the push") {
			t.Errorf("%s: %d %q", q, res.StatusCode, body)
		}
	}
}

// The bar over the terminal asks for its status; Terminal.app is not opened from a phone.
func TestTermStatAndTerminalApp(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	store.Update("a#1", func(r *core.Record) { r.Executor, r.Worktree = "terminal", t.TempDir() })
	srv := httptest.NewServer((&Server{D: d}).Handler())
	defer srv.Close()
	var st dispatch.TermStatus
	res, err := http.Get(srv.URL + "/api/termstat?id=a%231")
	if err != nil || json.NewDecoder(res.Body).Decode(&st) != nil || st.State != "" || st.Live {
		t.Fatalf("termstat: %v %+v", err, st)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/terminalapp?id=a%231", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.2")
	req.Header.Set("Origin", srv.URL)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode == 200 {
		t.Fatal("Terminal.app opened from a phone")
	}
	if !macOnly("/api/terminalapp") {
		t.Fatal("Terminal.app is not Mac-only")
	}
}

// Finder and the editor open on the Mac: a paired phone gets no button for them, and no Finder if it asks anyway.
func TestOpenIsFromTheMacOnly(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	store.Update("a#1", func(r *core.Record) { r.Task, r.Worktree = core.Task{ID: "a#1"}, t.TempDir() })
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	phones, _ := phone.Open(t.TempDir())
	srv := httptest.NewServer((&Server{D: d, Version: "test", Phones: phones}).Handler())
	defer srv.Close()
	device := pairPhone(t, srv, phones)
	for _, in := range []string{"finder", "editor"} {
		req, _ := http.NewRequest("POST", srv.URL+"/api/open?id=a%231&in="+in, nil)
		req.Header.Set("Origin", srv.URL)
		req.Header.Set("X-Forwarded-For", "100.64.0.2")
		req.AddCookie(device)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != http.StatusForbidden {
			t.Fatalf("open in %s from a phone: %v %v", in, err, res)
		}
	}
	if !macOnly("/api/open") {
		t.Fatal("/api/open is not Mac-only")
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/state", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.2")
	req.AddCookie(device)
	res, err := http.DefaultClient.Do(req)
	var st stateReply
	if err != nil || json.NewDecoder(res.Body).Decode(&st) != nil || !st.Remote || st.Editor != "" {
		t.Fatalf("phone state: %v remote=%v editor=%q", err, st.Remote, st.Editor)
	}
}

// The board closes the terminal while the agent is quiet: its subscription goes at once, not at the agent's next output.
func TestTerminalLeavesWithTheBoard(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	s, err := term.Start(term.Options{ID: "a#1", Worktree: t.TempDir(), Command: "/bin/sh", Token: "tok",
		Args: []string{"-c", `echo ready; sleep 30`}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	d.Sessions.Put(s)
	srv := httptest.NewServer((&Server{D: d}).Handler())
	defer srv.Close()
	c, err := websocket.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/term?id=a%231", "", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The agent's output so far is in, and it says nothing more.
	c.SetDeadline(time.Now().Add(5 * time.Second))
	for got := ""; !strings.Contains(got, "ready"); {
		var e struct{ T, D string }
		if err := websocket.JSON.Receive(c, &e); err != nil {
			t.Fatalf("waiting for ready in %q: %v", got, err)
		}
		got += e.D
	}
	if n := s.Subscribers(); n != 1 {
		t.Fatalf("subscribers %d", n)
	}
	c.Close()
	for deadline := time.Now().Add(3 * time.Second); s.Subscribers() != 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the closed board still watches the session")
		}
	}
}
