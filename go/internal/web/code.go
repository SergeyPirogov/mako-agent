package web

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
)

// codeRoutes: «Запитати про код» — the projects with a clone, their questions, asking one, and the files the
// answers point to. The conversation itself is /api/chat?id=<the question's id>.
func (s *Server) codeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/code/projects", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"projects": s.D.CodeProjects()})
	})
	mux.HandleFunc("GET /api/code/threads", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"threads": s.D.CodeThreads(r.URL.Query().Get("project"))})
	})
	mux.HandleFunc("GET /api/code/file", func(w http.ResponseWriter, r *http.Request) {
		text, path, err := s.D.CodeFile(r.Context(), r.URL.Query().Get("project"), r.URL.Query().Get("path"))
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"text": text, "path": path})
	})
	// A question: {project, thread, text}; no thread starts a new one.
	mux.HandleFunc("POST /api/code/ask", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct{ Project, Thread, Text string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 200<<10)).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		id, dirty, err := s.D.AskCode(in.Project, in.Thread, in.Text)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id, "dirty": dirty})
	})
	// The project's shared worktree: whether it is there and what is changed in it.
	mux.HandleFunc("GET /api/code/workspace", func(w http.ResponseWriter, r *http.Request) {
		ws, err := s.D.AskStatus(r.URL.Query().Get("project"))
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, ws)
	})
	// «Скинути й оновити» and «Видалити» of the shared worktree.
	for path, do := range map[string]func(string) error{"reset": s.D.ResetAsk, "remove": s.D.RemoveAsk} {
		mux.HandleFunc("POST /api/code/workspace/"+path, func(w http.ResponseWriter, r *http.Request) {
			if !sameOrigin(r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if err := do(r.URL.Query().Get("project")); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			writeJSON(w, map[string]bool{"ok": true})
		})
	}
	// The models the picker offers and the one picked, which the settings keep for every repository.
	mux.HandleFunc("GET /api/code/models", func(w http.ResponseWriter, r *http.Request) {
		cur := s.D.Config().CodeModel
		models := slices.Clone(dispatch.CodeModels)
		if cur != "" && !slices.Contains(models, cur) {
			models = append(models, cur)
		}
		writeJSON(w, map[string]any{"models": models, "current": cur})
	})
	mux.HandleFunc("POST /api/code/model", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct{ Model string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err := s.setCodeModel(r.Context(), strings.TrimSpace(in.Model)); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	// «Зупинити»: the answer's current step.
	mux.HandleFunc("POST /api/code/stop", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if !sameOrigin(r) || !dispatch.IsQuestion(id) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if c := s.D.Chats.Get(id); c != nil {
			c.Interrupt()
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}
