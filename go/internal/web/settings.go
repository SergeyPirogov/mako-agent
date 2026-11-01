package web

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	AutoWip  int             `json:"autoWip"` // the WIP limit «автоматично» gives on this Mac
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
		reply := settingsReply{Config: raw, Env: map[string]bool{}, JiraBase: cfg.Env["JIRA_BASE_URL"], Home: s.Home, AutoWip: config.AutoWip()}
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
		problems, err := s.saveConfig(r.Context(), in.Config, in.Env)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "problems": problems})
	})
	// «Пауза» of one tracker from the board: its search stops (or goes on), the other trackers keep theirs.
	mux.HandleFunc("POST /api/tracker-pause", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := s.pauseTracker(r.Context(), r.URL.Query().Get("id"), r.URL.Query().Get("on") == "1"); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
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
	// The rules saved to the repos' CLAUDE.md from the agents' corrections, for each project's settings.
	mux.HandleFunc("GET /api/settings/lessons", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.D.Lessons())
	})
	// How much of the disk the agents' worktrees take, per project.
	mux.HandleFunc("GET /api/settings/worktrees", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.D.WorktreeUsage(r.Context()))
	})
	// «Прибрати змерджені»: the worktrees and local branches of merged and archived tasks, not those with
	// uncommitted work.
	mux.HandleFunc("POST /api/settings/worktrees/clean", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "cleanup": s.D.CleanMerged(r.Context())})
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

// saveConfig writes mako.config.json (and the .env keys typed in) and makes Mako work by them; the problems are
// what the new settings leave to fix.
func (s *Server) saveConfig(ctx context.Context, data json.RawMessage, envChanges map[string]string) ([]string, error) {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return nil, err
	}
	pretty.WriteByte('\n')
	if err := os.MkdirAll(s.Home, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(s.Home, "mako.config.json")
	if err := os.WriteFile(path+".tmp", pretty.Bytes(), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return nil, err
	}
	if len(envChanges) > 0 {
		if err := config.SaveEnv(s.Home, envChanges); err != nil {
			return nil, fmt.Errorf(".env: %w", err)
		}
	}
	cfg, ts, problems := setup.Build(s.Home)
	problems = append(problems, assigneeProblems(ctx, ts)...)
	s.D.Reload(cfg, ts, problems)
	return problems, nil
}

// pauseTracker sets "paused" of the tracker id (its "id", else its "type", as config.Tracker.ID) in
// mako.config.json, leaving the rest of the file as it is.
func (s *Server) pauseTracker(ctx context.Context, id string, on bool) error {
	return s.changeConfig(ctx, func(raw map[string]json.RawMessage) error {
		return pauseIn(raw, id, on)
	})
}

// setCodeModel: the model of «Запитати про код», kept in the settings for every repository; "" — the default.
func (s *Server) setCodeModel(ctx context.Context, model string) error {
	if model != "" {
		if err := config.CheckModel(model); err != nil {
			return err
		}
	}
	return s.changeConfig(ctx, func(raw map[string]json.RawMessage) error {
		var ca map[string]json.RawMessage
		if b, ok := raw["codeAsk"]; ok {
			if err := json.Unmarshal(b, &ca); err != nil {
				return fmt.Errorf("mako.config.json: codeAsk: %w", err)
			}
		}
		if ca == nil {
			ca = map[string]json.RawMessage{}
		}
		if model == "" {
			delete(ca, "model")
		} else {
			ca["model"], _ = json.Marshal(model)
		}
		var err error
		raw["codeAsk"], err = json.Marshal(ca)
		return err
	})
}

// changeConfig changes mako.config.json as change says, keeping the rest as it is, and Mako reloads it.
func (s *Server) changeConfig(ctx context.Context, change func(map[string]json.RawMessage) error) error {
	data, err := os.ReadFile(filepath.Join(s.Home, "mako.config.json"))
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("mako.config.json: %w", err)
	}
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	if err := change(raw); err != nil {
		return err
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	_, err = s.saveConfig(ctx, out, nil)
	return err
}

// pauseIn: the tracker id paused (or not) in the settings.
func pauseIn(raw map[string]json.RawMessage, id string, on bool) error {
	var err error
	var ts []map[string]json.RawMessage
	if err := json.Unmarshal(raw["trackers"], &ts); err != nil {
		return fmt.Errorf("mako.config.json: trackers: %w", err)
	}
	found := false
	for _, t := range ts {
		var tid, typ string
		_ = json.Unmarshal(t["id"], &tid)
		_ = json.Unmarshal(t["type"], &typ)
		if strings.TrimSpace(tid) == "" {
			tid = typ
		}
		if strings.TrimSpace(tid) != id {
			continue
		}
		found = true
		if on {
			t["paused"] = json.RawMessage("true")
		} else {
			delete(t, "paused")
		}
	}
	if !found {
		return fmt.Errorf("трекера %q немає в налаштуваннях", id)
	}
	raw["trackers"], err = json.Marshal(ts)
	return err
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

// assigneeProblems: the trackers whose «Лише призначені на» names a login the tracker does not know; their search
// would fail, or find nothing, from then on. A tracker out of reach is not checked: its search will tell.
func assigneeProblems(ctx context.Context, ts []trackers.Tracker) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out []string
	for _, t := range ts {
		uf, ok := t.(trackers.UserFinder)
		login := t.Config().Assignee
		if !ok || login == "" || strings.EqualFold(login, "me") {
			continue
		}
		if found, err := uf.UserExists(ctx, login); err == nil && !found {
			out = append(out, fmt.Sprintf("%s: користувача «%s» немає — виправте «Лише призначені на», інакше нових задач звідти не буде", t.Name(), login))
		}
	}
	return out
}

// setConfig sets one top-level key of mako.config.json (a default value removes it), writes the file as the
// settings page does and has Mako work with it.
func (s *Server) setConfig(key string, value bool) error {
	path := filepath.Join(s.Home, "mako.config.json")
	raw := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if value {
		delete(raw, key) // on is the default
	} else {
		raw[key] = value
	}
	pretty, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Home, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", append(pretty, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	cfg, ts, problems := setup.Build(s.Home)
	s.D.Reload(cfg, ts, problems)
	return nil
}
