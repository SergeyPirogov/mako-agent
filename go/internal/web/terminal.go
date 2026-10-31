package web

import (
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
		if sess == nil || q.Get("token") == "" || q.Get("token") != sess.Token {
			http.Error(w, "unknown session", http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		sess.Hook(body)
		w.WriteHeader(http.StatusNoContent)
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

// terminal: what the agent shows goes out, what you type comes in. {"t":"init","d":…} first, then {"t":"o"}
// for output and {"t":"x"} when the agent ends; in: {"t":"i","d":…} keys, {"t":"r","cols":…,"rows":…} size.
func (s *Server) terminal(c *websocket.Conn) {
	defer c.Close()
	sess := s.D.Sessions.Get(c.Request().URL.Query().Get("id"))
	if sess == nil {
		websocket.JSON.Send(c, map[string]any{"t": "init", "d": "", "gone": true})
		return
	}
	past, events := sess.Subscribe()
	if err := websocket.JSON.Send(c, map[string]any{"t": "init", "d": past, "gone": events == nil}); err != nil || events == nil {
		return
	}
	defer sess.Unsubscribe(events)
	go func() {
		for {
			var m struct {
				T          string `json:"t"`
				D          string `json:"d"`
				Cols, Rows int
			}
			var raw string
			if err := websocket.Message.Receive(c, &raw); err != nil {
				c.Close()
				return
			}
			if json.Unmarshal([]byte(raw), &m) != nil {
				continue
			}
			switch m.T {
			case "i":
				sess.Input(m.D)
			case "r":
				sess.Resize(m.Cols, m.Rows)
			}
		}
	}()
	for e := range events {
		if websocket.JSON.Send(c, e) != nil {
			return
		}
	}
}
