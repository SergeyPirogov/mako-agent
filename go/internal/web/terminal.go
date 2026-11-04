package web

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"golang.org/x/net/websocket"
)

// xterm.js 6.0.0 with its fit and web-links addons, from npm (MIT, see vendor/LICENSE-xterm).
//
//go:embed vendor
var vendor embed.FS

// terminalRoutes: Mako's own terminal on the board. The agent's hooks report its state; a WebSocket carries
// the terminal both ways.
func (s *Server) terminalRoutes(mux *http.ServeMux) {
	files := http.FileServerFS(vendor)
	mux.HandleFunc("GET /vendor/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("POST /hook", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sess := s.D.Sessions.Get(q.Get("id"))
		if sess == nil || !tokenIs(q.Get("token"), sess.Token) {
			http.Error(w, "unknown session", http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		sess.Hook(body)
		w.WriteHeader(http.StatusNoContent)
	})
	// The push guard: the agent's PreToolUse hook before a Bash command; a push carrying secrets gets «deny».
	mux.HandleFunc("POST /guard", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if !tokenIs(q.Get("token"), s.D.GuardToken(q.Get("id"))) {
			http.Error(w, "unknown session", http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if answer := s.D.GuardPush(r.Context(), q.Get("id"), body); answer != nil {
			writeJSON(w, answer)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// The worktree's pre-push hook: git asks before any push from a task's worktree; anything but 204 stops it,
	// the body says why.
	mux.HandleFunc("POST /prepush", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// What git is about to push, a line per ref; one byte over the limit tells PrePush it was too much.
		refs, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		code, reason := s.D.PrePush(r.Context(), q.Get("id"), q.Get("token"), string(refs))
		if code == http.StatusNoContent {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		io.WriteString(w, reason)
	})
	// The bar over the terminal: what the agent does, its turn so far, its plan and context.
	mux.HandleFunc("GET /api/termstat", func(w http.ResponseWriter, r *http.Request) {
		st, ok := s.D.TerminalStatus(r.URL.Query().Get("id"))
		if !ok {
			writeJSON(w, map[string]any{"error": "the task is not in a Mako terminal"})
			return
		}
		writeJSON(w, st)
	})
	ws := websocket.Server{
		// Only the board's own pages: another site in the browser must not type into the agent.
		Handshake: func(_ *websocket.Config, r *http.Request) error {
			if r.Header.Get("Origin") == "" || !sameOrigin(r) {
				return errors.New("forbidden")
			}
			return nil
		},
		Handler: s.terminal,
	}
	mux.Handle("GET /ws/term", ws)
}

// termConn is the terminal's way to the page: a WebSocket, or a stream of the phone's sealed tunnel.
type termConn interface {
	Send(v any) error         // one JSON message out
	Receive() (string, error) // the next message in
	Close() error
}

type wsTerm struct{ c *websocket.Conn }

func (w wsTerm) Send(v any) error { return websocket.JSON.Send(w.c, v) }
func (w wsTerm) Receive() (string, error) {
	var raw string
	err := websocket.Message.Receive(w.c, &raw)
	return raw, err
}
func (w wsTerm) Close() error { return w.c.Close() }

func (s *Server) terminal(c *websocket.Conn) {
	s.termStream(c.Request().URL.Query().Get("id"), wsTerm{c})
}

// termStream: what the agent shows goes out, what you type comes in. {"t":"init","d":…} first, then {"t":"o"}
// for output and {"t":"x"} when the agent ends; in: {"t":"i","d":…} keys, {"t":"s","d":…} a line typed and Enter
// (the phone's input, /compact), {"t":"r","cols":…,"rows":…} size.
func (s *Server) termStream(id string, c termConn) {
	defer c.Close()
	sess := s.D.Sessions.Get(id)
	if sess == nil {
		c.Send(map[string]any{"t": "init", "d": "", "gone": true})
		return
	}
	past, events := sess.Subscribe()
	if err := c.Send(map[string]any{"t": "init", "d": past, "gone": events == nil}); err != nil || events == nil {
		return
	}
	defer sess.Unsubscribe(events)
	// done: the board went away; the writer below stops then, not at the session's next output.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var m struct {
				T          string `json:"t"`
				D          string `json:"d"`
				Cols, Rows int
			}
			raw, err := c.Receive()
			if err != nil {
				c.Close()
				return
			}
			if json.Unmarshal([]byte(raw), &m) != nil {
				continue
			}
			switch m.T {
			case "i":
				sess.Input(m.D)
			case "s":
				sess.Say(m.D)
			case "r":
				sess.Resize(m.Cols, m.Rows)
			}
		}
	}()
	for {
		select {
		case e, ok := <-events:
			if !ok || c.Send(e) != nil {
				return
			}
		case <-done:
			return
		}
	}
}

// tokenIs: got is the token want, compared in constant time; no token is never one.
func tokenIs(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
