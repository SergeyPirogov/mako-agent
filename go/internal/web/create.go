package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
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
			FromPlan    bool   `json:"fromPlan"` // made from an agent's plan: not from the phone, and no plan mode
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": "could not read the task: " + err.Error()})
			return
		}
		if in.FromPlan && remote(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		n := trackers.NewIssue{Project: in.Project, Title: in.Title, Description: in.Description, Bug: in.Type == "bug"}
		id, err := s.D.CreateTaskWith(context.WithoutCancel(r.Context()), in.Tracker, n, in.Take, in.FromPlan)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id})
	})
	// «Чому немає задачі?»: a link to an issue, and what of the search keeps it off the board.
	mux.HandleFunc("POST /api/why", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": "could not read the link: " + err.Error()})
			return
		}
		why, err := s.D.WhyNot(r.Context(), in.URL)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "why": why})
	})
	mux.HandleFunc("POST /api/upload", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxImage+1))
		if err == nil && len(data) > maxImage {
			writeJSON(w, map[string]any{"ok": false, "error": "the image is larger than 10 MB"})
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
	// A picture you attach to a message for the task's agent: kept beside its screenshots, the message names its path.
	mux.HandleFunc("POST /api/attach", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "image/") {
			writeJSON(w, map[string]any{"ok": false, "error": "only a picture can be attached"})
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxImage+1))
		if err == nil && len(data) > maxImage {
			writeJSON(w, map[string]any{"ok": false, "error": "the image is larger than 10 MB"})
			return
		}
		path, local := "", ""
		if err == nil {
			q := r.URL.Query()
			path, local, err = s.D.Attach(q.Get("id"), q.Get("name"), data)
		}
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "path": path, "url": local, "line": chat.ImageLine(path)})
	})
	// `mako screenshot`: the task's agent hands over a screenshot of its UI, for the chat and the MR.
	mux.HandleFunc("POST /api/screenshot", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) || remote(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxImage+1))
		if err == nil && len(data) > maxImage {
			writeJSON(w, map[string]any{"ok": false, "error": "the image is larger than 10 MB"})
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
	// A picture in a task's text that the tracker keeps behind its token (GitLab's /uploads/…), for the board.
	mux.HandleFunc("GET /api/tracker-image", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		data, err := s.D.TrackerImage(r.Context(), q.Get("tracker"), q.Get("project"), q.Get("path"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		kind := http.DetectContentType(data)
		if !strings.HasPrefix(kind, "image/") || strings.HasPrefix(kind, "image/svg") {
			http.Error(w, "not a picture", http.StatusUnsupportedMediaType) // the tracker's page (a login), not the picture
			return
		}
		w.Header().Set("Content-Type", kind)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Write(data)
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
