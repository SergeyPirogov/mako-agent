package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// maxImage bounds a screenshot pasted into a new task.
const maxImage = 10 << 20

// createRoutes: «+ Задача» — a new issue in the tracker, and the screenshots pasted into its description.
func (s *Server) createRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/create", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct {
			Tracker     string `json:"tracker"`
			Project     string `json:"project"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Type        string `json:"type"` // "task" or "bug"
			Take        bool   `json:"take"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": "не вдалося прочитати задачу: " + err.Error()})
			return
		}
		n := trackers.NewIssue{Project: in.Project, Title: in.Title, Description: in.Description, Bug: in.Type == "bug"}
		id, err := s.D.CreateTask(context.WithoutCancel(r.Context()), in.Tracker, n, in.Take)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id})
	})
	mux.HandleFunc("POST /api/upload", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxImage+1))
		if err == nil && len(data) > maxImage {
			writeJSON(w, map[string]any{"ok": false, "error": "зображення більше 10 МБ"})
			return
		}
		q := r.URL.Query()
		md := ""
		if err == nil {
			md, err = s.D.UploadImage(r.Context(), q.Get("tracker"), q.Get("project"), q.Get("name"), data)
		}
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "markdown": md})
	})
	// `mako screenshot`: the task's agent hands over a screenshot of its UI, for the chat and the MR.
	mux.HandleFunc("POST /api/screenshot", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) || remote(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxImage+1))
		if err == nil && len(data) > maxImage {
			writeJSON(w, map[string]any{"ok": false, "error": "зображення більше 10 МБ"})
			return
		}
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		q := r.URL.Query()
		local, md, upErr, err := s.D.Screenshot(context.WithoutCancel(r.Context()), q.Get("id"), q.Get("name"), data)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		reply := map[string]any{"ok": true, "url": local, "markdown": md}
		if upErr != nil {
			reply["uploadError"] = upErr.Error()
		}
		writeJSON(w, reply)
	})
	mux.HandleFunc("GET /api/screenshots/{task}/{file}", func(w http.ResponseWriter, r *http.Request) {
		task, file := r.PathValue("task"), r.PathValue("file")
		if task != filepath.Base(task) || file != filepath.Base(file) || strings.HasPrefix(task, ".") || strings.HasPrefix(file, ".") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=86400")
		http.ServeFile(w, r, filepath.Join(s.D.Config().StateDir, "screenshots", task, file))
	})
}
