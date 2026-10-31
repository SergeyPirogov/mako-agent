// Package web serves the board: one page on 127.0.0.1 and a small JSON API behind its buttons.
package web

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
	"github.com/SergeyPirogov/mako-agent/go/internal/update"
)

//go:embed board.html
var board []byte

//go:embed style.css
var style []byte

//go:embed theme.js
var themeJS []byte

// Mako's window keeps a cache across restarts: the pages ask for style.css and theme.js by their content, so a
// new Mako never shows old styles.
func init() {
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:4]) }
	r := strings.NewReplacer(`href="/style.css"`, `href="/style.css?v=`+sum(style)+`"`, `src="/theme.js"`, `src="/theme.js?v=`+sum(themeJS)+`"`)
	board, settingsPage = []byte(r.Replace(string(board))), []byte(r.Replace(string(settingsPage)))
}

type Server struct {
	D       *dispatch.Dispatcher
	Version string
	Home    string // where mako.config.json and .env live
	// Show opens a page of the board for the user (Mako's window or the browser); a second start asks for it.
	Show func(url string)
	// Phones: the paired phones (iPhone, Android); nil turns the phone off.
	Phones *phone.Phones
	// Update: Mako's own new commits and their install; nil when Mako is not built from a clone.
	Update *update.Updater
	// Release: a newer release of Mako, for one installed from a release (no clone); nil otherwise.
	Release *update.Releases
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
	Executor string                     `json:"executor"`            // who gets new tasks: "orca" or "terminal"
	Editor   string                     `json:"editor"`              // the code editor a worktree opens in, "" when none
	Limits   chat.Limits                `json:"limits"`              // Claude's usage limits, as the agents last reported them
	Hold     *dispatch.LimitHold        `json:"limitHold,omitempty"` // the queue waits for a Claude limit to reset
	LimitPct int                        `json:"limitPausePct"`       // the threshold of the settings, 0: none
	Remote   bool                       `json:"remote"`              // the page is open on a phone
	Stats    core.Stats                 `json:"stats"`               // how the taken tasks went, archived ones too
	Update   *update.Status             `json:"update,omitempty"`    // new commits of Mako itself; not on a phone
	Release  *update.Release            `json:"release,omitempty"`   // a newer release to download; not on a phone
	Restart  restartView                `json:"restart"`
	Faults   []dispatch.Fault           `json:"faults"` // Mako's own errors, the newest first
	// Digest: what happened while you were away, once, when the board is seen again after AwayHours.
	Digest *core.Digest `json:"digest,omitempty"`
}

// restartView: «Перезапустити, коли агенти закінчать хід» is pressed, and the tasks whose agents it waits for.
type restartView struct {
	Pending bool     `json:"pending"`
	Busy    []string `json:"busy"`
}

type recordView struct {
	core.Record
	Column    string `json:"column"`
	Postponed bool   `json:"postponed"`
	Type      string `json:"type"` // "fix" for a bug, "feature" for a task: as the branch is named
	Live      bool   `json:"live"` // its agent runs in Mako's terminal now
	// Blocked: why the task in work or in review is held up; the board shows it under «Заблоковані».
	Blocked string `json:"blocked,omitempty"`
}

type trackerView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host,omitempty"` // "git.example.com": tells two trackers of one kind apart
	// Projects: where «+ Задача» can write a task; nil when the tracker cannot open issues (Jira).
	Projects []string `json:"projects,omitempty"`
	// Paused: its search for new tasks is on pause, the other trackers are searched.
	Paused bool `json:"paused,omitempty"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.settingsRoutes(mux)
	s.terminalRoutes(mux)
	s.phoneRoutes(mux)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(board)
	})
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache") // Mako's window keeps its cache: a new Mako must not show old styles
		w.Write(style)
	})
	mux.HandleFunc("GET /theme.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(themeJS)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UnixMilli()
		reply := stateReply{Version: s.Version, Now: now, WipLimit: s.D.Config().WipLimit, Records: []recordView{}, Searches: s.D.Searches()}
		reply.Problems, reply.Paused = s.D.Status()
		reply.Agents = s.D.Agents()
		reply.Executor, reply.Editor, reply.Limits = s.D.Config().Executor, dispatch.Editor(), s.D.Chats.Limits()
		reply.Hold, reply.LimitPct = s.D.LimitHold(), s.D.Config().LimitPausePct
		reply.Remote = remote(r)
		// seen=1: the board is on screen (not a hidden tab), so you are back.
		if r.URL.Query().Get("seen") == "1" {
			reply.Digest = s.D.Seen(time.Now())
		}
		if s.Update != nil && !reply.Remote {
			st := s.Update.Status()
			reply.Update = &st
		}
		if s.Release != nil && !reply.Remote {
			reply.Release = s.Release.Latest()
		}
		reply.Restart = restartView{Pending: s.D.RestartPending(), Busy: s.D.BusyTasks()}
		all := s.D.Store.All()
		// days: the board's period for the statistics, the last N days (none: all the time).
		var since time.Time
		if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n > 0 {
			since = time.Now().AddDate(0, 0, -n)
		}
		reply.Stats = core.Summarize(all, time.Now(), since)
		reply.Faults = s.D.Faults()
		for _, rec := range all {
			v := recordView{Record: rec, Column: rec.Column(now), Postponed: rec.Postponed(now), Type: gitx.TaskType(rec.Task)}
			v.Live = s.D.Live(rec.ID)
			if rec.ArchivedAt == 0 {
				v.Blocked = s.D.Blocked(rec)
			}
			reply.Records = append(reply.Records, v)
		}
		projects := s.D.NewTaskProjects()
		for _, t := range s.D.TrackerList() {
			reply.Trackers = append(reply.Trackers, trackerView{ID: t.ID(), Name: t.Name(), Host: dispatch.TrackerHost(t), Projects: projects[t.ID()], Paused: t.Config().Paused})
		}
		writeJSON(w, reply)
	})
	// The digest from since (ms) until now: the morning push's link opens it.
	mux.HandleFunc("GET /api/digest", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		since := now.Add(-12 * time.Hour)
		if ms, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && ms > 0 && ms < now.UnixMilli() {
			since = time.UnixMilli(ms)
		}
		writeJSON(w, s.D.Digest(since, now))
	})
	mux.HandleFunc("GET /api/changes", func(w http.ResponseWriter, r *http.Request) {
		cs, err := s.D.Changes(r.Context(), r.URL.Query().Get("id"))
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, cs)
	})
	// The agent's conversation: everything when it changed since the board's version, else only the version.
	mux.HandleFunc("GET /api/chat", func(w http.ResponseWriter, r *http.Request) {
		c := s.D.Chats.Get(r.URL.Query().Get("id"))
		if c == nil {
			writeJSON(w, chat.Snapshot{State: "exited", Items: []chat.Item{}})
			return
		}
		snap := c.Snapshot()
		if strconv.Itoa(snap.Version) == r.URL.Query().Get("v") {
			writeJSON(w, map[string]any{"version": snap.Version, "same": true})
			return
		}
		writeJSON(w, snap)
	})
	mux.HandleFunc("GET /api/commits", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		h, err := s.D.History(r.Context(), r.URL.Query().Get("id"), n)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, h)
	})
	mux.HandleFunc("GET /api/commit", func(w http.ResponseWriter, r *http.Request) {
		c, err := s.D.CommitInfo(r.Context(), r.URL.Query().Get("id"), r.URL.Query().Get("hash"))
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, c)
	})
	mux.HandleFunc("GET /api/review", func(w http.ResponseWriter, r *http.Request) {
		c, err := s.D.Review(r.Context(), r.URL.Query().Get("id"))
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"review": c})
	})
	mux.HandleFunc("GET /api/diff", func(w http.ResponseWriter, r *http.Request) {
		diff, err := s.D.FileDiff(r.Context(), r.URL.Query().Get("id"), r.URL.Query().Get("path"), r.URL.Query().Get("whole") == "1")
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]string{"diff": diff})
	})
	// A task's whole Claude Code session from its worktree, for a task in Orca (Mako's own agents have /api/chat).
	mux.HandleFunc("GET /api/conversation", func(w http.ResponseWriter, r *http.Request) {
		rec, ok := s.D.Store.Get(r.URL.Query().Get("id"))
		turns := []orca.Turn{}
		if ok && rec.Worktree != "" && s.D.Conversation != nil {
			if t := s.D.Conversation(rec.Worktree, 300); t != nil {
				turns = t
			}
		}
		writeJSON(w, map[string]any{"turns": turns})
	})
	mux.HandleFunc("GET /api/log", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.D.Logs(r.URL.Query().Get("id")))
	})
	// «Оновити Mako»: test, build and restart on the new commits; from the Mac only.
	mux.HandleFunc("POST /api/update", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) || remote(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.Update == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "Mako зібраний не з репозиторію"})
			return
		}
		writeJSON(w, map[string]bool{"ok": s.Update.Start()})
	})
	// «Сховати» on the new release's banner: until a later release, from the Mac only.
	mux.HandleFunc("POST /api/release/dismiss", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) || remote(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.Release == nil {
			writeJSON(w, map[string]bool{"ok": false})
			return
		}
		if err := s.Release.Dismiss(r.URL.Query().Get("version")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	s.createRoutes(mux)
	// Mako's own errors: «На аналіз» opens an issue with them in Mako's repository, «Очистити» forgets them.
	mux.HandleFunc("POST /api/faults/{action}", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var err error
		id := ""
		switch r.PathValue("action") {
		case "report":
			id, err = s.D.ReportFaults(context.WithoutCancel(r.Context()))
		case "clear":
			err = s.D.ClearFaults()
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id})
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
		if action == "restart" {
			s.D.RequestRestart(r.URL.Query().Get("on") == "1")
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
		case "redo":
			// «Повторити інакше»: {note, model}.
			var in struct{ Note, Model string }
			json.NewDecoder(io.LimitReader(r.Body, 200<<10)).Decode(&in)
			if err := s.D.Redo(ctx, id, in.Note, in.Model); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "resume":
			if err := s.D.ResumeTerminal(id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "stop":
			s.D.StopTerminal(id)
		case "say", "permit", "answer", "choose", "interrupt", "view", "mode", "plan":
			var err error
			c := s.D.Chats.Get(id)
			switch {
			case action == "view":
				err = s.D.SwitchView(id, r.URL.Query().Get("to"))
			case action == "say":
				// To a closed agent too: the message resumes its conversation.
				body, _ := io.ReadAll(io.LimitReader(r.Body, 200<<10))
				err = s.D.Say(id, strings.TrimSpace(string(body)))
			case action == "plan":
				// The agent's plan: do=approve, or do=revise with your notes as the body.
				if r.URL.Query().Get("do") == "approve" {
					err = s.D.ApprovePlan(id, r.URL.Query().Get("req"))
				} else {
					body, _ := io.ReadAll(io.LimitReader(r.Body, 200<<10))
					err = s.D.RevisePlan(id, r.URL.Query().Get("req"), string(body))
				}
			case c == nil:
				err = fmt.Errorf("агент не запущений")
			case action == "answer":
				var answers map[string]string
				json.NewDecoder(io.LimitReader(r.Body, 200<<10)).Decode(&answers)
				err = c.Answer(r.URL.Query().Get("req"), answers)
			case action == "choose":
				// An option of the agent's question, from a notification's button.
				option, convErr := strconv.Atoi(r.URL.Query().Get("option"))
				if err = convErr; err == nil {
					err = c.Choose(r.URL.Query().Get("req"), option)
				}
			case action == "mode":
				err = c.SetMode(r.URL.Query().Get("to"))
			case action == "permit":
				err = c.Permit(r.URL.Query().Get("req"), r.URL.Query().Get("decision"))
			default:
				err = c.Interrupt()
			}
			if err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "open":
			if err := s.D.OpenWorktree(id, r.URL.Query().Get("in"), r.URL.Query().Get("file")); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "ask", "merge", "close":
			var err error
			switch action {
			case "ask":
				err = s.D.AskAgent(ctx, id, r.URL.Query().Get("kind"))
			case "merge":
				err = s.D.MergeReview(ctx, id)
			default:
				err = s.D.CloseReview(ctx, id)
			}
			if err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "lesson":
			// «Зберегти» (to the task's branch or the base one) or «Ні» for the lesson Mako offers; the body is the rule.
			body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<10))
			if err := s.D.SaveLesson(ctx, id, r.URL.Query().Get("to"), string(body)); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "focus":
			if err := s.D.FocusAgent(ctx, id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "assess":
			if err := s.D.Assess(id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "retro":
			if err := s.D.Retro(id); err != nil {
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
	return s.guard(mux)
}

// sameOrigin keeps other sites in the browser from pressing the board's buttons.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") == "" || r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, err := url.Parse(origin)
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = fh // through `tailscale serve`
	}
	return err == nil && (strings.EqualFold(u.Host, host) || strings.EqualFold(u.Host, r.Host))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
