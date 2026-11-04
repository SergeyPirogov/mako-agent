package web

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
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
	AutoWip  int             `json:"autoWip"` // the WIP limit “automatically” gives on this Mac
	Routes   routeChoices    `json:"routes"`
}

// routeChoices: what the routes' table offers, and its defaults.
type routeChoices struct {
	Defaults core.RouteTable `json:"defaults"`
	Phases   []string        `json:"phases"`
	Sizes    []string        `json:"sizes"`
	Models   []string        `json:"models"`
	Efforts  []string        `json:"efforts"`
	Modes    []string        `json:"modes"`
	Shifts   map[string]int  `json:"shifts"` // how many levels each mode moves the table (core.ModeShift)
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
		reply := settingsReply{Config: config.MigrateRaw(raw), Env: map[string]bool{}, JiraBase: cfg.Env["JIRA_BASE_URL"], Home: s.Home, AutoWip: config.AutoWip(),
			Routes: routeChoices{Defaults: core.DefaultRoutes(), Phases: core.RoutePhases, Sizes: core.Sizes, Models: []string{"haiku", "sonnet", "opus"}, Efforts: core.Efforts, Modes: core.Modes, Shifts: modeShifts()}}
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
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		in.Config = config.MigrateRaw(in.Config)
		env := s.envWith(in.Env)
		// Refuse what Mako could not read back, before anything is written.
		if _, err := config.Parse(in.Config, env); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err := config.CheckEnv(in.Env); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": ".env: " + err.Error()})
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
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
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
	// «Зберегти в CLAUDE.md» (to=base) or «Відхилити» (to=no) for a lesson from the reviewer's remarks; the body
	// is the rule as you edited it.
	mux.HandleFunc("POST /api/settings/lessons/answer", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		if err := s.D.AnswerRepoLesson(ctx, r.URL.Query().Get("id"), r.URL.Query().Get("to"), string(body)); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
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
	// «Видалити вибрані»: the chosen worktrees, Mako's or not; one with uncommitted or unpushed work stays
	// (and is listed) unless force.
	mux.HandleFunc("POST /api/settings/worktrees/remove", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct {
			Items []dispatch.WorktreeRef `json:"items"`
			Force bool                   `json:"force"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, s.D.RemoveWorktrees(r.Context(), in.Items, in.Force))
	})
	// «Finder» on a worktree: from the Mac only.
	mux.HandleFunc("POST /api/settings/worktrees/open", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) || remote(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in dispatch.WorktreeRef
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.D.OpenWorktreePath(r.Context(), in.Repo, in.Path); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
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
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.saveConfigLocked(ctx, data, envChanges)
}

// saveConfigLocked: saveConfig with cfgMu held.
func (s *Server) saveConfigLocked(ctx context.Context, data json.RawMessage, envChanges map[string]string) ([]string, error) {
	if err := s.writeConfig(data); err != nil {
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

// writeConfig: the only writer of mako.config.json, indented, atomically; cfgMu is held.
func (s *Server) writeConfig(data json.RawMessage) error {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return err
	}
	pretty.WriteByte('\n')
	if err := os.MkdirAll(s.Home, 0o755); err != nil {
		return err
	}
	path := filepath.Join(s.Home, "mako.config.json")
	return fsx.WriteFile(path, pretty.Bytes(), 0o600)
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
		if !slices.Contains(dispatch.CodeModels, model) {
			return errors.New("unknown model")
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

// changeConfig changes mako.config.json as change says, keeping the rest as it is, and Mako reloads it. The
// whole read-change-write holds cfgMu: two changes at once (☕ and a tracker's pause) both stay. No file yet: {}.
func (s *Server) changeConfig(ctx context.Context, change func(map[string]json.RawMessage) error) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	var raw map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join(s.Home, "mako.config.json"))
	if err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("mako.config.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
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
	_, err = s.saveConfigLocked(ctx, out, nil)
	return err
}

// allowTools adds rules to «Commands without asking» of Mako's terminal agents in the settings, each once.
func (s *Server) allowTools(ctx context.Context, rules []string) error {
	return s.changeConfig(ctx, func(raw map[string]json.RawMessage) error {
		return allowIn(raw, rules)
	})
}

// allowIn: the rules added to the allowedTools of every terminal executor in the settings.
func allowIn(raw map[string]json.RawMessage, rules []string) error {
	var ex map[string]map[string]json.RawMessage
	if b, ok := raw["executors"]; ok {
		if err := json.Unmarshal(b, &ex); err != nil {
			return fmt.Errorf("mako.config.json: executors: %w", err)
		}
	}
	found := false
	for name, e := range ex {
		var typ string
		json.Unmarshal(e["type"], &typ)
		if typ != "terminal" {
			continue
		}
		found = true
		var have []string
		if b, ok := e["allowedTools"]; ok {
			if err := json.Unmarshal(b, &have); err != nil {
				return fmt.Errorf("mako.config.json: executors.%s.allowedTools: %w", name, err)
			}
		}
		for _, r := range rules {
			if !slices.Contains(have, r) {
				have = append(have, r)
			}
		}
		e["allowedTools"], _ = json.Marshal(have)
	}
	if !found {
		return errors.New("the settings have no Mako terminal agent: add the rules in Settings → Commands without asking")
	}
	var err error
	raw["executors"], err = json.Marshal(ex)
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
		return fmt.Errorf("tracker %q is not in the settings", id)
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
		return "", errors.New("no tracker")
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
			out = append(out, fmt.Sprintf("%s: the user «%s» does not exist — fix “Only assigned to”, or no new tasks will come from there", t.Name(), login))
		}
	}
	return out
}

// setConfig sets one top-level key of mako.config.json (a default value removes it), as changeConfig does.
func (s *Server) setConfig(ctx context.Context, key string, value bool) error {
	return s.changeConfig(ctx, func(raw map[string]json.RawMessage) error {
		if value {
			delete(raw, key) // on is the default
		} else {
			raw[key] = json.RawMessage("false")
		}
		return nil
	})
}

// modeShifts: the levels each route mode moves the table, for the page to show the effective routes.
func modeShifts() map[string]int {
	shifts := map[string]int{}
	for _, m := range core.Modes {
		shifts[m] = core.ModeShift(m)
	}
	return shifts
}
