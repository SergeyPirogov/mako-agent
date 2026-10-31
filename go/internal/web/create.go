package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

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
}
