// Package web serves the board: one page on 127.0.0.1 and a small JSON API behind its buttons.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
)

//go:embed board.html
var board []byte

//go:embed style.css
var style []byte

//go:embed theme.js
var themeJS []byte

type Server struct {
	D       *dispatch.Dispatcher
	Version string
	Home    string // where mako.config.json and .env live
	// Show opens a page of the board for the user (Mako's window or the browser); a second start asks for it.
	Show func(url string)
}

type stateReply struct {
	Version  string                     `json:"version"`
	Now      int64                      `json:"now"`
	WipLimit int                        `json:"wipLimit"`
	Records  []recordView               `json:"records"`
	Searches map[string]dispatch.Search `json:"searches"`
	Trackers []trackerView              `json:"trackers"`
	Problems []string                   `json:"problems"`
	Paused   bool                       `json:"paused"`
	Agents   map[string]dispatch.Agent  `json:"agents"`
}

type recordView struct {
	core.Record
	Column    string `json:"column"`
	Postponed bool   `json:"postponed"`
}

type trackerView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.settingsRoutes(mux)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(board)
	})
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(style)
	})
	mux.HandleFunc("GET /theme.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(themeJS)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UnixMilli()
		reply := stateReply{Version: s.Version, Now: now, WipLimit: s.D.Config().WipLimit, Records: []recordView{}, Searches: s.D.Searches()}
		reply.Problems, reply.Paused = s.D.Status()
		reply.Agents = s.D.Agents()
		for _, rec := range s.D.Store.All() {
			reply.Records = append(reply.Records, recordView{Record: rec, Column: rec.Column(now), Postponed: rec.Postponed(now)})
		}
		for _, t := range s.D.TrackerList() {
			reply.Trackers = append(reply.Trackers, trackerView{ID: t.ID(), Name: t.Name()})
		}
		writeJSON(w, reply)
	})
	mux.HandleFunc("GET /api/log", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.D.Logs(r.URL.Query().Get("id")))
	})
	mux.HandleFunc("POST /api/{action}", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ctx := context.WithoutCancel(r.Context())
		action, id := r.PathValue("action"), r.URL.Query().Get("id")
		if action == "show" {
			if s.Show == nil {
				http.Error(w, "no window", http.StatusNotImplemented)
				return
			}
			s.Show(s.D.BoardURL)
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		if action == "poll" {
			s.D.Wake()
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		if _, ok := s.D.Store.Get(id); !ok {
			http.Error(w, "no such task", http.StatusNotFound)
			return
		}
		switch action {
		case "take":
			s.D.Take(ctx, id)
		case "later":
			min, _ := strconv.Atoi(r.URL.Query().Get("min"))
			s.D.Later(id, min)
		case "cancel":
			s.D.Cancel(id)
		case "retry":
			s.D.Retry(ctx, id)
		case "focus":
			if err := s.D.FocusAgent(ctx, id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "archive":
			s.D.Archive(id, true)
		case "unarchive":
			s.D.Archive(id, false)
		default:
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	return mux
}

// sameOrigin keeps other sites in the browser from pressing the board's buttons.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") == "" || r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
