// Package web serves the board: one page on 127.0.0.1 and a small JSON API behind its buttons.
package web

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/awake"
	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
	"github.com/SergeyPirogov/mako-agent/go/internal/update"
)

// maxLaterMin is the longest a task can be put off: 365 days.
const maxLaterMin = 365 * 24 * 60

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
	// TunnelBadPause: the wait after a phone-tunnel frame that does not decrypt; 0 is the default (tests shorten it).
	TunnelBadPause time.Duration
	// Update: Mako's own new commits and their install; nil when Mako is not built from a clone.
	Update *update.Updater
	// Release: a newer release of Mako, for one installed from a release (no clone); nil otherwise.
	Release *update.Releases
	// Relay: the address phones reach this Mac at through the relay ("https://<id>.relay.example.com"), "" while
	// its tunnel is down; nil when the relay is off (no MAKO_RELAY, docs/relay.md).
	Relay func() string
	// Try: this is a copy of Mako to try a branch on («Try this build»); the board says so.
	Try bool

	cfgMu sync.Mutex // mako.config.json is read, changed and written by one request at a time

	tunMu   sync.Mutex
	tunnels map[string]*tunnel // the phones' end-to-end tunnels, by device: one each
}

type stateReply struct {
	Version string `json:"version"`
	// Try: a copy of Mako to try a branch on: the board says so in its header.
	Try      bool         `json:"try,omitempty"`
	Now      int64        `json:"now"`
	WipLimit int          `json:"wipLimit"`
	Records  []recordView `json:"records"`
	Models   []string     `json:"models"` // the agent's models to pick from
	// ReviewRequests: others' MRs where I am a reviewer («Рев'ю для мене»), the finished ones too.
	ReviewRequests []core.ReviewRequest       `json:"reviewRequests"`
	Searches       map[string]dispatch.Search `json:"searches"`
	Trackers       []trackerView              `json:"trackers"`
	Problems       []string                   `json:"problems"`
	Paused         bool                       `json:"paused"`
	Agents         map[string]dispatch.Agent  `json:"agents"`
	Executor       string                     `json:"executor"`            // who gets new tasks: "orca" or "terminal"
	Editor         string                     `json:"editor"`              // the code editor a worktree opens in, "" when none or on a phone
	Limits         chat.Limits                `json:"limits"`              // Claude's usage limits, as the agents last reported them
	Hold           *dispatch.LimitHold        `json:"limitHold,omitempty"` // the queue waits for a Claude limit to reset
	LimitPct       int                        `json:"limitPausePct"`       // the threshold of the settings, 0: none
	AuthHold       *dispatch.AuthHold         `json:"authHold,omitempty"`  // the queue waits for you to log in to Claude again
	Remote         bool                       `json:"remote"`              // the page is open on a phone
	E2E            bool                       `json:"e2e,omitempty"`       // …through its sealed tunnel (docs/relay-e2e.md)
	Stats          core.Stats                 `json:"stats"`               // how the taken tasks went, archived ones too
	RoutesNow      core.RouteTable            `json:"routesNow"`           // the route each phase and size gets now
	CostAdvice     []core.Advice              `json:"costAdvice"`          // how to spend less, by the period's stats
	NavRetros      []dispatch.NavRetroState   `json:"navRetros,omitempty"` // the navigation retros of the stats' project
	Evals          []dispatch.EvalState       `json:"evals,omitempty"`     // the eval sets and runs of the stats' project
	Update         *update.Status             `json:"update,omitempty"`    // new commits of Mako itself; not on a phone
	Release        *update.Release            `json:"release,omitempty"`   // a newer release to download; not on a phone
	Restart        restartView                `json:"restart"`
	QueueHold      string                     `json:"queueHold,omitempty"` // why no new agent starts now
	// Awake: the settings keep the Mac awake while agents work (on), and it is being kept awake now (active).
	Awake struct {
		On     bool `json:"on"`
		Active bool `json:"active"`
	} `json:"awake"`
	Faults []dispatch.Fault `json:"faults"` // Mako's own errors, the newest first
	// LessonOffers: the lessons from the reviewer's remarks waiting for an answer in the settings, by project.
	LessonOffers map[string]int `json:"lessonOffers,omitempty"`
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
	// DefaultModel: for a task not taken yet, the model its agent gets when none is picked: its work's route.
	DefaultModel string `json:"defaultModel,omitempty"`
	// Outlook: for a task not taken yet, how the project's ended tasks like it went (core.OutlookOf); nil when
	// the history is too short.
	Outlook *core.Outlook `json:"outlook,omitempty"`
	// ReviewStep: what the reviewer agent's row offers: "running", "notes", "fixing", "ready", "idle" or "" (dispatch.ReviewStep).
	ReviewStep string `json:"reviewStep,omitempty"`
	// Reviewable: the reviewer's remarks can still be passed to the agent (dispatch.Reviewable): the agent is Mako's,
	// with a worktree, and the task is in work or in an open MR.
	Reviewable bool `json:"reviewable,omitempty"`
	// SelfCheckNew: how many commits the branch got after your «Check it yourself» tick; 0: up to date or no tick.
	SelfCheckNew int `json:"selfCheckNew,omitempty"`
	// Self: the «Check it yourself» steps and their ticks; nil when nothing is asked of you.
	Self *dispatch.SelfCheckView `json:"self,omitempty"`
	// Risk: the change's risk, why and what it does here (dispatch.RiskView); nil while nothing is known of it.
	Risk *dispatch.RiskView `json:"risk,omitempty"`
	// Blocked: why the task in work or in review is held up; the board shows it under «Заблоковані».
	Blocked string `json:"blocked,omitempty"`
	// CIWait: the MR in review is open and its CI still runs.
	CIWait string `json:"ciWait,omitempty"`
	// MergeReady: the MR in review was checked and can be merged now (CI green, no conflicts, no open threads).
	MergeReady bool `json:"mergeReady,omitempty"`
	// ReviewBlocks: the reviewer's last round left blockers not yet passed to the agent: the merge waits for Fix.
	ReviewBlocks bool `json:"reviewBlocks,omitempty"`
	// Starting: the steps of its start while Mako prepares it («запускаю»); QueueWhy: why a queued task that is
	// not being started waits.
	Starting *dispatch.StartProgress `json:"starting,omitempty"`
	QueueWhy string                  `json:"queueWhy,omitempty"`
	// Waits: the task it waits for before it may start (a blocker in the tracker, a part of a split before it).
	Waits *dispatch.Wait `json:"waits,omitempty"`
	// WillPlan: the queued task's agent starts with a plan (dispatch.WillPlan): the card puts the plan next.
	WillPlan bool `json:"-"`
	// Card: the card's main action and its stages for the task's state.
	Card cardView `json:"card"`
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

// makoInfo: the settings' «Mako» section. Kind: "clone" (built from a clone, updates by commits), "release"
// (installed from a release) or "dev" (neither can update). Checked is the last look that got an answer; Error
// is why the last look failed, or err of the look just made.
type makoInfo struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Kind    string          `json:"kind"`
	Version string          `json:"version"`
	Checked int64           `json:"checked,omitempty"`
	Update  *update.Status  `json:"update,omitempty"`
	Release *update.Release `json:"release,omitempty"` // found even when its banner was hidden
}

func (s *Server) makoInfo(err error) makoInfo {
	info := makoInfo{Kind: "dev", Version: s.Version}
	switch {
	case s.Update != nil:
		st := s.Update.Status()
		info.Kind, info.Update, info.Checked, info.Error = "clone", &st, st.Checked, st.CheckError
	case s.Release != nil:
		info.Kind, info.Release = "release", s.Release.Found()
		info.Checked, info.Error = s.Release.Checked()
	}
	if err != nil {
		info.Error = err.Error()
	}
	info.OK = info.Kind != "dev" && info.Error == ""
	return info
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
	// The words of the pages: the interface is English only.
	mux.HandleFunc("GET /api/i18n", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"strings": i18n.WebStrings()})
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UnixMilli()
		reply := stateReply{Version: s.Version, Try: s.Try, Now: now, WipLimit: s.D.Config().WipLimit, Records: []recordView{}, Searches: s.D.Searches()}
		reply.Problems, reply.Paused = s.D.Status()
		reply.Agents = s.D.Agents()
		reply.Executor, reply.Editor, reply.Limits = s.D.Config().Executor, dispatch.Editor(), s.D.Chats.Limits()
		reply.Hold, reply.LimitPct = s.D.LimitHold(), s.D.Config().LimitPausePct
		reply.AuthHold = s.D.AuthHold()
		reply.Remote = remote(r)
		if reply.Remote {
			reply.Editor = "" // the editor opens on the Mac: a phone gets no button for it
		}
		reply.E2E = tunneled(r) != nil
		// seen=1: the board is on screen (not a hidden tab), so you are back; from the board itself only.
		if r.URL.Query().Get("seen") == "1" && fromMako(r) {
			reply.Digest = s.D.Seen(time.Now())
		}
		if s.Update != nil && !reply.Remote {
			st := s.Update.Status()
			reply.Update = &st
		}
		if s.Release != nil && !reply.Remote {
			reply.Release = s.Release.Latest()
		}
		reply.QueueHold = s.D.HoldReason()
		queueWhy := s.D.QueueReason()
		reply.Restart = restartView{Pending: s.D.RestartPending(), Busy: s.D.BusyTasks()}
		reply.Awake.On, reply.Awake.Active = s.D.Config().KeepAwake, awake.Active()
		all := s.D.Store.All()
		// days: the board's period for the statistics: "today" (since the local midnight), the last N days
		// (none: all the time).
		var since time.Time
		if d := r.URL.Query().Get("days"); d == "today" {
			n := time.Now()
			since = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location())
		} else if n, err := strconv.Atoi(d); err == nil && n > 0 {
			since = time.Now().AddDate(0, 0, -n)
		}
		reply.Models = core.Models
		// project: the statistics of one project only (none: all of them).
		reply.Stats = core.SummarizePriced(core.OfProject(all, r.URL.Query().Get("project")), time.Now(), since, s.D.Config().Prices)
		if s.D.Spends != nil { // the reviewer over others' MRs: no task's, a phase of its own
			core.AddSpends(&reply.Stats, s.D.Spends.All(), since, r.URL.Query().Get("project"))
		}
		reply.RoutesNow = s.D.RoutesNow()
		reply.CostAdvice = core.CostAdvice(reply.Stats, reply.RoutesNow, s.D.Config().RouteMode)
		reply.NavRetros = s.D.NavRetros(r.URL.Query().Get("project"))
		reply.Evals = s.D.Evals(r.URL.Query().Get("project"), r.URL.Query().Get("evalBase"), r.URL.Query().Get("evalVariant"))
		reply.ReviewRequests = s.D.ReviewList()
		reply.Faults = s.D.Faults()
		// sel: the task whose card the board has open: its copy to try is kept while it is.
		if sel := r.URL.Query().Get("sel"); sel != "" {
			s.D.TryKeep(sel)
		}
		reply.LessonOffers = s.D.LessonOffers()
		var history *core.History
		for _, rec := range all {
			v := recordView{Record: rec, Column: rec.Column(now), Postponed: rec.Postponed(now), Type: gitx.TaskType(rec.Task)}
			v.Live = s.D.Live(rec.ID)
			if v.Column == "new" {
				v.DefaultModel = s.D.DefaultModel(rec)
				if history == nil { // indexed once for all the new cards of this reply
					history = core.NewHistory(all)
				}
				v.Outlook = history.Outlook(rec, s.D.OutlookModel(rec))
			}
			v.ReviewStep = s.D.ReviewStep(rec)
			v.ReviewBlocks = s.D.ReviewBlocks(rec)
			v.Reviewable = s.D.Reviewable(rec)
			v.SelfCheckNew = s.D.SelfCheckBehind(rec)
			v.Self = s.D.SelfView(rec)
			v.Risk = s.D.RiskView(rec)
			if rec.ArchivedAt == 0 {
				v.Blocked = s.D.Blocked(rec)
				v.CIWait = s.D.CIWait(rec)
				v.MergeReady = s.D.MergeReady(rec)
			}
			if v.Column == "new" || rec.Status == core.Queued {
				v.Waits = s.D.WaitsFor(rec)
			}
			if rec.Status == core.Queued {
				v.WillPlan = s.D.WillPlan(rec)
				if v.Starting = s.D.Starting(rec.ID); v.Starting == nil && v.Waits == nil {
					v.QueueWhy = queueWhy
				}
			}
			v.Card = cardOf(v, time.Now())
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
		id := r.URL.Query().Get("id")
		c := s.D.Chats.Get(id)
		var snap chat.Snapshot
		if c != nil {
			snap = c.Snapshot()
		} else if q, ok := s.D.QuestionChat(id); ok {
			snap = q // a question asked before Mako restarted: its transcript
		} else if f, ok := s.D.FreeChatSnapshot(id); ok {
			snap = f // a chat without a task whose agent is not running
		} else if t, ok := s.D.TaskChat(id); ok {
			snap = t // a task whose agent is not running: its conversation so far
		} else {
			snap = chat.Snapshot{State: "exited", Items: []chat.Item{}}
		}
		if c == nil {
			snap = s.D.WithLimit(id, snap) // cut by the limit before Mako restarted: it still goes on by itself
		}
		if strconv.Itoa(snap.Version) == r.URL.Query().Get("v") {
			writeJSON(w, map[string]any{"version": snap.Version, "same": true})
			return
		}
		if snap.Plan != nil {
			snap.WorkMode = dispatch.WorkMode(s.D.Config().Terminal)
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
	// An MR to review for its card: CI, open threads, the files with their diffs; it is seen from now on when the
	// board itself asks.
	mux.HandleFunc("GET /api/review-request", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.D.ReviewRequest(r.Context(), r.URL.Query().Get("id"), fromMako(r))
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, v)
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
	// «Я увійшов — продовжити»: the queue stops waiting for the Claude login; from the Mac only (the login is there).
	mux.HandleFunc("POST /api/auth/continue", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) || remote(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]bool{"ok": s.D.ClearAuth()})
	})
	// «Оновити Mako»: test, build and restart on the new commits; from the Mac only.
	mux.HandleFunc("POST /api/update", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) || remote(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.Update == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "Mako was not built from the repository"})
			return
		}
		writeJSON(w, map[string]bool{"ok": s.Update.Start()})
	})
	// «Скасувати» and «Перезапустити зараз» while the update builds or waits for the agents; from the Mac only.
	for path, act := range map[string]func(*update.Updater) bool{
		"/api/update/cancel": (*update.Updater).Cancel,
		"/api/update/now":    (*update.Updater).Force,
	} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			if !sameOrigin(r) || remote(r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if s.Update == nil {
				writeJSON(w, map[string]any{"ok": false, "error": "Mako was not built from the repository"})
				return
			}
			ok := act(s.Update)
			s.D.Wake()
			writeJSON(w, map[string]bool{"ok": ok})
		})
	}
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
	// «Mako» in the settings: what is installed and what the last look for an update found (GET), and a look
	// now (POST); from the Mac only.
	mux.HandleFunc("GET /api/update/info", func(w http.ResponseWriter, r *http.Request) {
		if remote(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		writeJSON(w, s.makoInfo(nil))
	})
	mux.HandleFunc("POST /api/update/check", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) || remote(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		var err error
		switch {
		case s.Update != nil:
			err = s.Update.Check(ctx)
		case s.Release != nil:
			err = s.Release.Check(ctx)
		default:
			err = errors.New("updates are unavailable: dev build")
		}
		writeJSON(w, s.makoInfo(err))
	})
	s.createRoutes(mux)
	s.navRoutes(mux)
	s.evalRoutes(mux)
	s.codeRoutes(mux)
	s.chatRoutes(mux)
	// «Рев'ю для мене»: «Підготувати ревʼю» (the reviewer drafts my review), a comment of the draft edited or
	// struck out, the chosen comments sent to the MR, «Схвалити».
	mux.HandleFunc("POST /api/review-request/{action}", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ctx, id := context.WithoutCancel(r.Context()), r.URL.Query().Get("id")
		var in struct {
			Comment string   `json:"comment"`
			Text    string   `json:"text"`
			Dropped bool     `json:"dropped"`
			IDs     []string `json:"ids"`
		}
		if r.ContentLength != 0 {
			if err := decodeJSON(r, 200<<10, &in); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": "bad request: " + err.Error()})
				return
			}
		}
		var err error
		reply := map[string]any{"ok": true}
		switch r.PathValue("action") {
		case "prepare":
			err = s.D.PrepareReview(id)
		case "draft":
			err = s.D.EditDraft(id, in.Comment, in.Text, in.Dropped)
		case "send":
			var rq core.ReviewRequest
			rq, err = s.D.SendDrafts(ctx, id, in.IDs)
			reply["draft"] = rq.Draft
		case "approve":
			err = s.D.ApproveReview(ctx, id)
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, reply)
	})
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
		if action == "say" && r.URL.Query().Get("plan") == "1" && remote(r) {
			// An agent's plan handed over to this session: from the Mac only.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
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
		if action == "awake" {
			// The header's ☕: the settings' «Не давати Mac заснути», saved like the settings page saves it.
			if err := s.setConfig(r.Context(), "keepAwake", r.URL.Query().Get("on") == "1"); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		if action == "restart" {
			s.D.RequestRestart(r.URL.Query().Get("on") == "1")
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		if action == "resumeonreset" {
			// «Продовжити після скидання» of a chat after the Claude limit: a task's, a free chat's or a question's.
			if err := s.D.SetResumeOnReset(id, r.URL.Query().Get("on") == "1"); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		if dispatch.IsFreeChat(id) {
			s.freeChatAction(w, r, action, id)
			return
		}
		if _, ok := s.D.Store.Get(id); !ok {
			http.Error(w, "no such task", http.StatusNotFound)
			return
		}
		switch action {
		case "take":
			s.D.Take(ctx, id)
		case "push":
			s.D.Push(ctx, id)
		case "later":
			// No min: the default snooze. Anything else must be a number: -1 brings it back, 1..maxLaterMin puts it off.
			min := 0
			if v := r.URL.Query().Get("min"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < -1 || n == 0 || n > maxLaterMin {
					writeJSON(w, map[string]any{"ok": false, "error": "min must be -1 or a number of minutes from 1 to " + strconv.Itoa(maxLaterMin)})
					return
				}
				min = n
			}
			s.D.Later(id, min)
		case "pick":
			// The model from the arrow and the estimate's advice on the card: ?model=sonnet&plan=1 (empty model: as the
			// settings say). Without plan, «план спочатку» stays as it is now.
			rec, _ := s.D.Store.Get(id)
			// Taken already (another tab, the phone): the model is not changed for a task that is going.
			if !dispatch.CanTake(rec) {
				writeJSON(w, map[string]any{"ok": false, "error": "the task is already taken"})
				return
			}
			q := r.URL.Query()
			plan := rec.PlanFirst
			if q.Has("plan") {
				plan = q.Get("plan") == "1"
			}
			if err := s.D.Pick(id, strings.TrimSpace(q.Get("model")), plan); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "cancel":
			s.D.Cancel(id)
		case "retry":
			s.D.Retry(ctx, id)
		case "redo":
			// «Повторити інакше»: {note, model}.
			var in struct{ Note, Model string }
			if err := decodeJSON(r, 200<<10, &in); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
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
		case "fresh":
			// «New conversation»: the agent starts over in the same worktree, its full context dropped.
			if err := s.D.NewConversation(id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "say", "permit", "answer", "choose", "interrupt", "view", "mode", "plan":
			var err error
			c := s.D.Chats.Get(id)
			if action == "interrupt" {
				s.D.ForgetLimit(id) // stopped by you: it does not go on by itself after the limit either
			}
			switch {
			case action == "view":
				// In the middle of a turn the switch waits for it to end, unless now=1 («Switch now»).
				err = s.D.SwitchView(id, r.URL.Query().Get("to"), r.URL.Query().Get("now") == "1")
			case action == "say":
				// To a closed agent too: the message resumes its conversation.
				body, ok := readBody(w, r, 200<<10)
				if !ok {
					return
				}
				err = s.D.Say(id, strings.TrimSpace(string(body)))
			case action == "plan":
				do, o, perr := readPlan(r)
				req := r.URL.Query().Get("req")
				switch {
				case perr != nil:
					err = perr
				case do == "approve":
					err = s.D.ApprovePlan(id, req, o)
				case do == "revise":
					err = s.D.RevisePlan(id, req, o.Notes.Text())
				case do == "reject":
					err = s.D.RejectPlan(id, req)
				default:
					err = fmt.Errorf("unknown plan action %q", do)
				}
			default:
				err = s.controlChat(c, action, r)
			}
			if err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "terminalapp":
			// The Mac's own Terminal.app: from the Mac only.
			if remote(r) {
				writeJSON(w, map[string]any{"ok": false, "error": "Terminal.app opens on the Mac, not from the phone"})
				return
			}
			if err := s.D.OpenInTerminal(id); err != nil {
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
			body, ok := readBody(w, r, 8<<10)
			if !ok {
				return
			}
			if err := s.D.SaveLesson(ctx, id, r.URL.Query().Get("to"), string(body)); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "notsecret":
			// «Це не секрет»: the line of a finding of the push guard goes through.
			if err := s.D.AllowSecret(id, r.URL.Query().Get("key")); err != nil {
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
		case "estimate":
			if err := s.D.Estimate(id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "split":
			// «Розбити»: the agent proposes the task's subtasks.
			if err := s.D.SplitTask(id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "splitdrop":
			if err := s.D.DropSplit(id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "splitcreate":
			// «Створити»: the subtasks as edited on the board, opened in the tracker.
			var in struct {
				Items []core.SplitItem `json:"items"`
			}
			if err := decodeJSON(r, 64<<10, &in); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": "could not read the subtasks: " + err.Error()})
				return
			}
			if err := s.D.CreateSplit(ctx, id, in.Items); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "answers":
			// «Надіслати в трекер»: the answers to the estimate's questions, each with its question.
			var in struct {
				Answers []dispatch.Answer `json:"answers"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": "could not read the answers: " + err.Error()})
				return
			}
			if err := s.D.AnswerQuestions(ctx, id, in.Answers); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "agentreview":
			if err := s.D.ReviewNow(id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "reviewskip":
			// «Пропустити»: you decided no review is needed.
			if err := s.D.SkipReview(id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "selfcheck":
			// «Check it yourself»: the tick is the task's, with the branch's HEAD it was set at.
			if err := s.D.SelfCheck(id, r.URL.Query().Get("on") == "1"); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "selfstep":
			// A step of «Check it yourself»: n from 0; why, when it is ticked without «Try this build».
			q := r.URL.Query()
			n, err := strconv.Atoi(q.Get("n"))
			if err == nil {
				err = s.D.SelfStep(id, n, q.Get("on") == "1", strings.TrimSpace(q.Get("why")))
			}
			if err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "trybuild":
			// «Try this build»: opens on the Mac, not on the phone that asked.
			if remote(r) {
				writeJSON(w, map[string]any{"ok": false, "error": "a build to try opens on the Mac: start it from there"})
				return
			}
			if err := s.D.TryBuild(id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "trystop":
			s.D.TryStop(id)
		case "selfskip":
			// «Skip» of the check: the merge goes on without it, for the reason given.
			if err := s.D.SelfSkip(id, r.URL.Query().Get("why")); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "reviewfix":
			// «Виправити»: the reviewer's remarks go to the agent.
			if err := s.D.FixReview(id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "retro":
			if err := s.D.Retro(id); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "clean":
			// The worktree Mako kept for its uncommitted work: remove it anyway.
			if _, err := s.D.CleanTask(ctx, id, true); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		case "keepworktree":
			s.D.KeepWorktree(id)
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
	h := s.guard(mux)
	// The sealed requests go through the guard as well, as the device's own.
	mux.Handle("GET /e2e", s.tunnelRoute(h))
	return h
}

// makoHeader: what the board's own requests (and Mako's command line) carry. Another site in the browser cannot
// send it: a header of its own makes the browser ask first (CORS), and the board answers no such question.
const makoHeader = "X-Mako"

// fromMako: the request carries makoHeader, so it is the board's own; a GET does what it changes only then.
func fromMako(r *http.Request) bool { return r.Header.Get(makoHeader) == "1" }

// sameOrigin keeps other sites in the browser from pressing the board's buttons. Without Origin, only a request
// the browser marks same-origin, or one with makoHeader (`mako show`, `mako screenshot`), gets through.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") == "same-origin" || fromMako(r)
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
