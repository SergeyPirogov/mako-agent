package web

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/setup"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

//go:embed settings.html
var settingsPage []byte

// Secrets the settings page may set; it only learns whether each is set, never the value.
var secretKeys = []string{"GITLAB_TOKEN", "GITHUB_TOKEN", "JIRA_EMAIL", "JIRA_API_TOKEN"}

type settingsReply struct {
	Config   json.RawMessage `json:"config"`
	Env      map[string]bool `json:"env"`
	JiraBase string          `json:"jiraBaseUrl"`
	Home     string          `json:"home"`
	Problems []string        `json:"problems"`
}

type settingsSave struct {
	Config json.RawMessage   `json:"config"`
	Env    map[string]string `json:"env"` // only the keys typed in: a new value, or "" to remove
}

func (s *Server) settingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(settingsPage)
	})
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		cfg := s.D.Config()
		raw, err := os.ReadFile(filepath.Join(s.Home, "mako.config.json"))
		if err != nil || !json.Valid(raw) {
			raw = []byte("{}")
		}
		reply := settingsReply{Config: raw, Env: map[string]bool{}, JiraBase: cfg.Env["JIRA_BASE_URL"], Home: s.Home}
		for _, k := range secretKeys {
			reply.Env[k] = cfg.Get(k) != ""
		}
		reply.Problems, _ = s.D.Status()
		writeJSON(w, reply)
	})
	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in settingsSave
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		env := s.envWith(in.Env)
		// Refuse what Mako could not read back, before anything is written.
		if _, err := config.Parse(in.Config, env); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, in.Config, "", "  "); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pretty.WriteByte('\n')
		if err := os.MkdirAll(s.Home, 0o755); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		path := filepath.Join(s.Home, "mako.config.json")
		if err := os.WriteFile(path+".tmp", pretty.Bytes(), 0o644); err == nil {
			err = os.Rename(path+".tmp", path)
		} else {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if len(in.Env) > 0 {
			if err := config.SaveEnv(s.Home, in.Env); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": ".env: " + err.Error()})
				return
			}
		}
		cfg, ts, problems := setup.Build(s.Home)
		s.D.Reload(cfg, ts, problems)
		writeJSON(w, map[string]any{"ok": true, "problems": problems})
	})
	// Check a tracker as it is in the form: who the token belongs to.
	mux.HandleFunc("POST /api/settings/check", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct {
			Tracker json.RawMessage   `json:"tracker"`
			Env     map[string]string `json:"env"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		me, err := s.check(r.Context(), in.Tracker, s.envWith(in.Env))
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "me": me})
	})
	mux.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		s.D.SetPaused(r.URL.Query().Get("on") == "1")
		writeJSON(w, map[string]bool{"ok": true})
	})
}

// envWith: .env as it is, with the values typed in the form on top.
func (s *Server) envWith(changes map[string]string) map[string]string {
	env := map[string]string{}
	for k, v := range s.D.Config().Env {
		env[k] = v
	}
	for k, v := range changes {
		env[k] = v
	}
	return env
}

func (s *Server) check(ctx context.Context, rawTracker json.RawMessage, env map[string]string) (string, error) {
	// Projects do not matter for a token check; allProjects keeps the parser from asking for one.
	var t map[string]any
	if err := json.Unmarshal(rawTracker, &t); err != nil {
		return "", err
	}
	t["allProjects"] = true
	one, _ := json.Marshal(map[string]any{"trackers": []any{t}})
	cfg, err := config.Parse(one, env)
	if err != nil {
		return "", err
	}
	if len(cfg.Trackers) != 1 {
		return "", errors.New("немає трекера")
	}
	tr, err := trackers.New(cfg.Trackers[0], cfg)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return tr.Me(ctx)
}
