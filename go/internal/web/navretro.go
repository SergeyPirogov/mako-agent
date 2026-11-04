package web

import (
	"context"
	"net/http"
)

// navRoutes: «Ретро навігації» of a repository (key: tracker/project/repo) in the stats of its project: start it,
// save a line it suggests to CLAUDE.md, or answer a suggestion («Ні», the task made of it).
func (s *Server) navRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/navretro", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := s.D.NavRetro(r.URL.Query().Get("key")); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	// The body is the line as you edited it.
	mux.HandleFunc("POST /api/navretro/save", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		body, ok := readBody(w, r, 8<<10)
		if !ok {
			return
		}
		q := r.URL.Query()
		if err := s.D.SaveNavRule(context.WithoutCancel(r.Context()), q.Get("key"), q.Get("id"), string(body)); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	// to: "no", or "task:<id>" once a task is made of it.
	mux.HandleFunc("POST /api/navretro/mark", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		if err := s.D.MarkNav(q.Get("key"), q.Get("id"), q.Get("to")); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
}
