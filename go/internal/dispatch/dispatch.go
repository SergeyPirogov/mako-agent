// Package dispatch is Mako's loop: it polls the trackers for my tasks, hands the ones I take to Orca, and
// moves their issues along as merge requests appear and get merged.
package dispatch

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// Hand is the hand-off to Orca; tests replace it.
type Hand func(ctx context.Context, o config.Orca, j orca.Job, log func(string)) (*orca.Result, error)

// Search is the last poll of one tracker, for the board.
type Search struct {
	At    time.Time `json:"at"`
	Found int       `json:"found"`
	Error string    `json:"error,omitempty"`
}

type Dispatcher struct {
	Cfg      *config.Config
	Store    *state.Store
	Trackers []trackers.Tracker
	Hand     Hand
	Term     Terminals
	// Sessions: the agents in Mako's own terminal.
	Sessions *term.Manager
	// Chats: the agents in Mako's chat, and the account's usage limits they report.
	Chats *chat.Manager
	// LastMessage: a Claude agent's whole last message from its transcript (Orca shows 200 characters).
	LastMessage func(worktree string) string
	// Conversation: the agent's whole Claude Code session, for the card.
	Conversation func(worktree string, limit int) []orca.Turn
	BoardURL     string
	// Hosts: code hosts made for repos whose tracker is not one (Jira tasks), by web host.
	hosts map[string]trackers.CodeHost

	cfgMu    sync.RWMutex // Cfg, Trackers, Problems, Paused: the settings page replaces them
	Problems []string     // why Mako cannot work as set up: no config, a tracker without a token
	Paused   bool         // «Пауза»: no searching for tasks

	mu       sync.Mutex // the trackers' projects, searches, logs
	searches map[string]Search
	logs     []LogLine
	starting map[string]bool
	agents   map[string]*Agent
	reviews  map[string]reviewLook
	wake     chan struct{}
}

type LogLine struct {
	At   time.Time `json:"at"`
	ID   string    `json:"id"`
	Text string    `json:"text"`
}

func New(cfg *config.Config, store *state.Store, ts []trackers.Tracker) *Dispatcher {
	return &Dispatcher{
		Cfg: cfg, Store: store, Trackers: ts, Hand: orca.Hand, Term: orcaTerminals{}, LastMessage: orca.LastMessage, Conversation: orca.Conversation,
		hosts: map[string]trackers.CodeHost{}, searches: map[string]Search{}, starting: map[string]bool{}, agents: map[string]*Agent{}, reviews: map[string]reviewLook{},
		wake: make(chan struct{}, 1), Sessions: term.NewManager(filepath.Join(cfg.StateDir, "terminal")), Chats: chat.NewManager(filepath.Join(cfg.StateDir, "limits.json")),
	}
}

// Config and TrackerList: the current settings (the settings page may replace them at any time).
func (d *Dispatcher) Config() *config.Config {
	d.cfgMu.RLock()
	defer d.cfgMu.RUnlock()
	return d.Cfg
}

func (d *Dispatcher) TrackerList() []trackers.Tracker {
	d.cfgMu.RLock()
	defer d.cfgMu.RUnlock()
	return d.Trackers
}

func (d *Dispatcher) Status() (problems []string, paused bool) {
	d.cfgMu.RLock()
	defer d.cfgMu.RUnlock()
	return d.Problems, d.Paused
}

func (d *Dispatcher) SetPaused(on bool) {
	d.cfgMu.Lock()
	d.Paused = on
	d.cfgMu.Unlock()
	if !on {
		d.Wake()
	}
}

// Reload applies new settings; hand-offs already under way keep the ones they started with.
func (d *Dispatcher) Reload(cfg *config.Config, ts []trackers.Tracker, problems []string) {
	d.cfgMu.Lock()
	d.Cfg, d.Trackers, d.Problems = cfg, ts, problems
	d.cfgMu.Unlock()
	d.mu.Lock()
	d.hosts = map[string]trackers.CodeHost{}
	d.searches = map[string]Search{}
	d.mu.Unlock()
	d.Wake()
}

func (d *Dispatcher) Log(id, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// The same complaint every minute (a repo not cloned yet, a tracker down) is said once.
	for i := len(d.logs) - 1; i >= 0; i-- {
		if d.logs[i].ID == id {
			if d.logs[i].Text == text {
				return
			}
			break
		}
	}
	log.Printf("[%s] %s", id, text)
	d.logs = append(d.logs, LogLine{At: time.Now(), ID: id, Text: text})
	if len(d.logs) > 500 {
		d.logs = d.logs[len(d.logs)-500:]
	}
}

func (d *Dispatcher) Logs(id string) []LogLine {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []LogLine{}
	for _, l := range d.logs {
		if id == "" || l.ID == id {
			out = append(out, l)
		}
	}
	return out
}

func (d *Dispatcher) Searches() map[string]Search {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]Search{}
	for k, v := range d.searches {
		out[k] = v
	}
	return out
}

func (d *Dispatcher) tracker(id string) trackers.Tracker {
	for _, t := range d.TrackerList() {
		if t.ID() == id {
			return t
		}
	}
	return nil
}

// Wake makes the loop poll now.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run polls until ctx ends: trackers every PollIntervalSec, merge requests every minute.
func (d *Dispatcher) Run(ctx context.Context) {
	// The agents in Orca are watched more often than the trackers: a question should not wait a minute.
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			d.WatchAgents(ctx, time.Now())
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	for {
		d.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-time.After(time.Duration(d.Config().PollIntervalSec) * time.Second):
		}
	}
}

// Tick is one round: new tasks, hand-offs that got an MR, merges, issues closed meanwhile, the archive.
func (d *Dispatcher) Tick(ctx context.Context) {
	d.Poll(ctx)
	d.CheckHandoffs(ctx)
	d.CheckMerges(ctx)
	d.CheckGone(ctx)
	d.AutoArchive(time.Now())
	d.remind(time.Now())
	d.schedule(ctx)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// --- new tasks

func (d *Dispatcher) Poll(ctx context.Context) {
	if _, paused := d.Status(); paused {
		return
	}
	for _, t := range d.TrackerList() {
		tasks, err := t.FetchMine(ctx)
		s := Search{At: time.Now(), Found: len(tasks)}
		if err != nil {
			s.Error = err.Error()
			d.Log(t.ID(), "пошук не вдався: "+err.Error())
		}
		d.mu.Lock()
		d.searches[t.ID()] = s
		d.mu.Unlock()
		if err != nil {
			continue
		}
		me := ""
		if t.Config().SkipOthersTasks {
			me, _ = t.Me(ctx)
		}
		seen := map[string]bool{}
		for _, task := range tasks {
			seen[task.ID] = true
			if me != "" && len(task.Assignees) > 0 && !contains(task.Assignees, me) {
				continue // a colleague's: theirs to take
			}
			d.ensureProject(t, task.Project)
			d.seen(ctx, t, task)
		}
		// A task waiting for «Взяти в роботу» that left the search: closed or deleted ones stay on the board
		// as such; the rest (reassigned, label removed) are forgotten.
		for _, r := range d.Store.All() {
			if r.Task.TrackerID != t.ID() || seen[r.ID] || (r.Status != core.Asking && r.Status != core.Snoozed) {
				continue
			}
			st, err := t.IssueState(ctx, r.Task)
			switch {
			case err != nil:
			case st == "open":
				d.Log(r.ID, "більше не підходить під пошук, прибираю")
				d.Store.Delete(r.ID)
			default:
				d.markGone(r, st)
			}
		}
	}
}

func (d *Dispatcher) seen(ctx context.Context, t trackers.Tracker, task core.Task) {
	r, known := d.Store.Get(task.ID)
	switch {
	case !known:
		d.Store.Update(task.ID, func(r *core.Record) {
			r.Task, r.Status = task, core.Snoozed
		})
		d.Log(task.ID, "нова задача: "+task.Title)
		notify.Inform("🦈 Нова задача", task.ID+": "+task.Title, d.BoardURL)
		if d.Config().Notifications.AutoTake {
			d.Take(ctx, task.ID)
		}
	case r.Status == core.Cancelled && r.Gone != "":
		d.Log(task.ID, "знову відкрита в трекері")
		d.Store.Update(task.ID, func(r *core.Record) {
			r.Task, r.Status, r.SnoozedUntil, r.PostponedByYou, r.Gone, r.Error, r.ArchivedAt = task, core.Snoozed, 0, false, "", "", 0
		})
	case r.Status == core.Asking || r.Status == core.Snoozed:
		d.Store.Update(task.ID, func(r *core.Record) { r.Task = task })
	}
}

// ensureProject: with «Мої задачі з усіх проєктів», a task from a project not in the config gets one: your
// existing clone if there is one, else ~/mako/projects/<name>, cloned on «Взяти в роботу».
func (d *Dispatcher) ensureProject(t trackers.Tracker, project string) {
	tc := t.Config()
	d.mu.Lock()
	defer d.mu.Unlock()
	if tc.Projects[project] != nil || !tc.AllProjects || !strings.Contains(project, "/") {
		return
	}
	taken := map[string]bool{}
	for _, tr := range d.TrackerList() {
		for _, p := range tr.Config().Projects {
			for _, r := range p.Repos {
				taken[r.Path] = true
			}
		}
	}
	parts := strings.Split(project, "/")
	name := parts[len(parts)-1]
	// Two projects called «frontend» in different groups must not share a folder.
	for _, n := range []string{name, strings.Join(parts[len(parts)-2:], "-"), strings.ReplaceAll(project, "/", "-")} {
		if !taken[filepath.Join(d.Config().ProjectsDir, n)] {
			name = n
			break
		}
	}
	existing := gitx.FindClone(d.Config().Home, project)
	repo := d.Config().NewRepo(name, existing, config.DefaultCloneURL(tc, project), "", "")
	if tc.Projects == nil {
		tc.Projects = map[string]*config.Project{}
	}
	tc.Projects[project] = &config.Project{Key: project, Repos: map[string]*config.Repo{name: repo}}
	where := repo.Path + " (склоную при передачі)"
	if existing != "" {
		where = "ваш клон " + existing
	}
	log.Printf("[%s] новий проєкт %s → %s", t.ID(), project, where)
}

var repoLabel = regexp.MustCompile(`(?i)^repo:(.+)$`)

// route picks the task's repository: a repo:<name> label, the project's default, or its only repo.
func (d *Dispatcher) route(t trackers.Tracker, task core.Task) (*config.Repo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := t.Config().Projects[task.Project]
	if p == nil {
		return nil, fmt.Errorf("проєкт %s не налаштований: додайте його в налаштуваннях або увімкніть «Мої задачі з усіх проєктів»", task.Project)
	}
	names := []string{}
	for n := range p.Repos {
		names = append(names, n)
	}
	for _, l := range task.Labels {
		if m := repoLabel.FindStringSubmatch(l); m != nil {
			for n, r := range p.Repos {
				if strings.EqualFold(n, m[1]) {
					return r, nil
				}
			}
			return nil, fmt.Errorf("мітка repo:%s називає невідомий репозиторій (є: %s)", m[1], strings.Join(names, ", "))
		}
	}
	if r := p.Repos[p.Default]; r != nil {
		return r, nil
	}
	if len(p.Repos) == 1 {
		for _, r := range p.Repos {
			return r, nil
		}
	}
	return nil, fmt.Errorf("не визначено репозиторій: додайте мітку repo:<назва> (%s) або вкажіть основний", strings.Join(names, ", "))
}

// --- taking a task

// Take: «Взяти в роботу». Within the WIP limit it starts now, otherwise it waits in the queue.
func (d *Dispatcher) Take(ctx context.Context, id string) {
	r, ok := d.Store.Get(id)
	if !ok || (r.Status != core.Asking && r.Status != core.Snoozed && r.Status != core.Failed && r.Status != core.Cancelled) {
		return
	}
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.Error, r.SnoozedUntil, r.PostponedByYou, r.ArchivedAt = core.Queued, "", 0, false, 0
	})
	d.schedule(ctx)
}

func (d *Dispatcher) schedule(ctx context.Context) {
	d.mu.Lock()
	active := len(d.starting)
	for _, r := range d.Store.All() {
		if r.Status == core.HandedOff {
			active++
		}
	}
	var start []string
	for _, r := range d.Store.All() {
		if r.Status == core.Queued && !d.starting[r.ID] && (d.Config().WipLimit <= 0 || active < d.Config().WipLimit) {
			d.starting[r.ID] = true
			start = append(start, r.ID)
			active++
		}
	}
	d.mu.Unlock()
	for _, id := range start {
		go func(id string) {
			defer func() {
				d.mu.Lock()
				delete(d.starting, id)
				d.mu.Unlock()
			}()
			d.start(context.WithoutCancel(ctx), id)
		}(id)
	}
}

// Wait blocks until no hand-off is starting (tests).
func (d *Dispatcher) Wait() {
	for {
		d.mu.Lock()
		n := len(d.starting)
		d.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (d *Dispatcher) start(ctx context.Context, id string) {
	r, _ := d.Store.Get(id)
	executor := "orca"
	if d.Config().Executor == terminalExecutor {
		executor = terminalExecutor
	}
	fail := func(err error) {
		d.Log(id, "не передано: "+err.Error())
		d.Store.Update(id, func(r *core.Record) { r.Status, r.Error, r.FinishedAt = core.Failed, err.Error(), now() })
		notify.Inform("❌ "+id+": не передано "+map[string]string{"orca": "в Orca", terminalExecutor: "агенту"}[executor], err.Error(), d.BoardURL)
	}
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		fail(fmt.Errorf("трекер %s прибрано з налаштувань", r.Task.TrackerID))
		return
	}
	repo, err := d.route(t, r.Task)
	if err != nil {
		fail(err)
		return
	}
	logf := func(s string) { d.Log(id, s) }
	if err := gitx.EnsureClone(ctx, repo, logf); err != nil {
		fail(err)
		return
	}
	attempt := max(r.Attempt, 1)
	branch := gitx.BranchName(repo.BranchPattern, r.Task, attempt)
	d.Store.Update(id, func(r *core.Record) {
		r.Repo, r.Executor, r.BranchName, r.StartedAt, r.Attempt = repo.Name, executor, branch, now(), attempt
	})

	if t.Config().AssignOnTake {
		if me, _ := t.Me(ctx); me == "" || !contains(r.Task.Assignees, me) {
			if err := t.AssignToMe(ctx, r.Task); err != nil {
				d.Log(id, "не вдалось призначити задачу на вас: "+err.Error())
			}
		}
	}
	d.move(ctx, t, r.Task, trackers.AtStart)

	job := orca.Job{Task: r.Task, TrackerName: t.Name(), Repo: repo, Branch: branch}
	var res *orca.Result
	if executor == terminalExecutor {
		res, err = d.handTerminal(ctx, job, logf)
	} else {
		res, err = d.Hand(ctx, d.Config().Orca, job, logf)
	}
	if err != nil {
		d.move(ctx, t, r.Task, trackers.AtFailed)
		fail(err)
		return
	}
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.Worktree, r.Branch = core.HandedOff, res.Worktree, res.Branch
	})
	d.Log(id, map[string]string{"orca": "передано в Orca: ", terminalExecutor: "агент працює в терміналі: "}[executor]+res.Note)
	d.comment(ctx, t, r.Task, d.Config().Comments.OnStart, map[string]string{"branch": res.Branch})
}

var commentVar = regexp.MustCompile(`\{(\w+)\}`)

// comment leaves a comment from a template of the settings ("-": none).
func (d *Dispatcher) comment(ctx context.Context, t trackers.Tracker, task core.Task, template string, values map[string]string) {
	if strings.TrimSpace(template) == "-" || strings.TrimSpace(template) == "" {
		return
	}
	all := map[string]string{"taskId": task.ID, "title": task.Title, "url": task.URL}
	for k, v := range values {
		all[k] = v
	}
	text := commentVar.ReplaceAllStringFunc(template, func(m string) string {
		if v, ok := all[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
	if err := t.Comment(ctx, task, text); err != nil {
		d.Log(task.ID, "коментар не додано: "+err.Error())
	}
}

// move changes the issue's status for a moment and keeps the outcome for the card.
func (d *Dispatcher) move(ctx context.Context, t trackers.Tracker, task core.Task, moment string) {
	status, err := t.Move(ctx, task, moment)
	if err != nil {
		d.Log(task.ID, "статус у трекері не змінено: "+err.Error())
		d.Store.Update(task.ID, func(r *core.Record) { r.TrackerStatus = &core.TrackerState{OK: false, Text: err.Error()} })
		return
	}
	if status != "" {
		d.Log(task.ID, fmt.Sprintf("%s: статус «%s»", t.Name(), status))
		d.Store.Update(task.ID, func(r *core.Record) { r.TrackerStatus = &core.TrackerState{OK: true, Text: status} })
	}
}

// --- after the hand-off

// host: where the MRs of a repo live. The tracker itself for GitLab and GitHub on the same host; otherwise a
// client for the origin's host with GITLAB_TOKEN / GITHUB_TOKEN (Jira tasks).
func (d *Dispatcher) host(ctx context.Context, t trackers.Tracker, repo *config.Repo) (trackers.CodeHost, string, error) {
	origin := gitx.OriginURL(ctx, repo.Path)
	project := gitx.RemoteProject(origin)
	if project == "" {
		return nil, "", fmt.Errorf("не розібрати origin репозиторію %s: %q", repo.Path, origin)
	}
	h := remoteHost(origin)
	if ch, ok := t.(trackers.CodeHost); ok && (h == "" || ch.Host() == h) {
		return ch, project, nil
	}
	for _, other := range d.TrackerList() {
		if ch, ok := other.(trackers.CodeHost); ok && ch.Host() == h {
			return ch, project, nil
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if ch := d.hosts[h]; ch != nil {
		return ch, project, nil
	}
	var ch trackers.CodeHost
	var err error
	if strings.Contains(h, "github") {
		cfg := &config.Tracker{Type: "github", ID: "github@" + h}
		if h != "github.com" {
			cfg.APIURL = "https://" + h + "/api/v3"
		}
		ch, err = trackers.NewGitHub(cfg, trackers.GitHubToken(d.Config()))
	} else {
		ch, err = trackers.NewGitLab(&config.Tracker{Type: "gitlab", ID: "gitlab@" + h, BaseURL: "https://" + h}, d.Config().Get("GITLAB_TOKEN"))
	}
	if err != nil {
		return nil, "", err
	}
	d.hosts[h] = ch
	return ch, project, nil
}

var hostOf = regexp.MustCompile(`^(?:[a-z+]+://)?(?:[^@/]+@)?([^/:]+)`)

func remoteHost(remote string) string {
	if m := hostOf.FindStringSubmatch(remote); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// CheckHandoffs: a task in Orca is done on Mako's side once an MR from its branch exists. The issue moves to
// review, gets a comment with the link, and a bare MR description is written from the branch's changes.
func (d *Dispatcher) CheckHandoffs(ctx context.Context) {
	for _, r := range d.Store.All() {
		if r.Status != core.HandedOff {
			continue
		}
		t := d.tracker(r.Task.TrackerID)
		if t == nil {
			continue
		}
		repo, err := d.route(t, r.Task)
		if err != nil {
			continue
		}
		host, project, err := d.host(ctx, t, repo)
		if err != nil {
			d.Log(r.ID, "MR не перевірено: "+err.Error())
			continue
		}
		branch := r.Branch
		if branch == "" {
			branch = r.BranchName
		}
		review, err := host.FindReview(ctx, project, branch)
		if err != nil {
			d.Log(r.ID, "MR не перевірено: "+err.Error())
			continue
		}
		if review == nil {
			// Orca's workspace is gone and nothing was opened from it: the hand-off was abandoned.
			if r.Worktree != "" && !exists(r.Worktree) {
				d.Log(r.ID, "workspace видалено, MR немає")
				d.Store.Update(r.ID, func(r *core.Record) {
					r.Status, r.Error, r.FinishedAt = core.Cancelled, "Workspace видалено ("+r.Worktree+"), MR не відкрито", now()
				})
				notify.Inform("🦈 "+r.ID+": передачу скасовано", "Workspace видалено, MR немає. Задачу можна повторити з дошки.", d.BoardURL)
			}
			continue
		}
		d.Log(r.ID, "є MR: "+review.URL)
		if err := d.describeIfBare(ctx, host, r, repo, review.URL); err != nil {
			d.Log(r.ID, "опис MR не написано: "+err.Error())
		}
		d.comment(ctx, t, r.Task, d.Config().Comments.OnReview, map[string]string{"branch": branch, "mr": review.URL})
		d.move(ctx, t, r.Task, trackers.AtReview)
		d.Store.Update(r.ID, func(r *core.Record) { r.Status, r.PRURL, r.FinishedAt = core.Done, review.URL, now() })
		d.summarize(ctx, r, repo.BaseBranch, review.URL)
		if !onTerminal(r) {
			orca.Mark(ctx, d.Config().Orca, repo.Path, r.Worktree, branch, "in-review")
		}
		notify.Inform("✅ "+r.ID+": є MR", review.URL, review.URL)
		if review.State != "open" {
			d.CheckMerges(ctx)
		}
	}
}

var closingLine = regexp.MustCompile(`(?im)^\s*(closes|fixes|resolves|refs?)\b.*$`)
var structured = regexp.MustCompile(`(?m)^(#{1,3} |[-*] )`)

// describeIfBare: an MR opened with Orca's button often has an empty or one-line description; Mako writes one
// from the branch's changes. A real description and a real title are kept.
func (d *Dispatcher) describeIfBare(ctx context.Context, host trackers.CodeHost, r core.Record, repo *config.Repo, url string) error {
	if r.Worktree == "" || !exists(r.Worktree) {
		return nil
	}
	title, description, err := host.ReadReview(ctx, url)
	if err != nil {
		return err
	}
	body := strings.TrimSpace(closingLine.ReplaceAllString(description, ""))
	if len(body) >= 120 || (len(body) >= 40 && structured.MatchString(body)) {
		return nil
	}
	newTitle, summary, err := writer.ReviewText(ctx, r.Worktree, repo.BaseBranch, r.Task)
	if err != nil || summary == "" {
		return err
	}
	closing := r.Task.ClosingRef
	if closing == "" {
		closing = r.Task.URL
	}
	text := strings.Join([]string{summary, closing, fmt.Sprintf("🦈 Mako · [%s](%s)", r.Task.ID, r.Task.URL)}, "\n\n")
	// «fix/28-pagination», «WIP», «Update»: not a title a reviewer can use.
	draft := regexp.MustCompile(`(?i)^draft:\s*`)
	bare := strings.TrimSpace(draft.ReplaceAllString(title, ""))
	if bare == "" || !strings.ContainsAny(bare, " \t") {
		if draft.MatchString(title) {
			newTitle = "Draft: " + newTitle
		}
		title = newTitle
	}
	d.Log(r.ID, "опис MR написано зі змін гілки")
	return host.DescribeReview(ctx, url, title, text)
}

// CheckMerges: MRs in review. Merged: the issue gets its final status (Done) and closes; closed without a
// merge: the task did not work out.
func (d *Dispatcher) CheckMerges(ctx context.Context) {
	for _, r := range d.Store.All() {
		if r.Status != core.Done || r.PRURL == "" || r.ReviewState != "" {
			continue
		}
		t := d.tracker(r.Task.TrackerID)
		if t == nil {
			continue
		}
		repo, err := d.route(t, r.Task)
		if err != nil {
			continue
		}
		host, _, err := d.host(ctx, t, repo)
		if err != nil {
			continue
		}
		st, err := host.ReviewState(ctx, r.PRURL)
		if err != nil {
			d.Log(r.ID, "стан MR не перевірено: "+err.Error())
			continue
		}
		switch st {
		case "merged":
			d.Log(r.ID, "MR змерджено")
			d.Store.Update(r.ID, func(r *core.Record) { r.ReviewState, r.MergedAt = "merged", now() })
			d.move(ctx, t, r.Task, trackers.AtMerged)
			if !onTerminal(r) {
				orca.Mark(ctx, d.Config().Orca, repo.Path, r.Worktree, r.Branch, "done")
			}
			d.removeWorktree(ctx, r, repo)
			notify.Inform("🎉 "+r.ID+": змерджено", r.Task.Title, r.PRURL)
		case "closed":
			d.Log(r.ID, "MR закрито без мерджу")
			d.Store.Update(r.ID, func(r *core.Record) {
				r.ReviewState, r.Status, r.Error = "closed", core.Failed, "MR закрито без мерджу"
			})
		}
	}
}

// CheckGone: taken tasks whose issue was closed or deleted in the tracker meanwhile.
func (d *Dispatcher) CheckGone(ctx context.Context) {
	for _, r := range d.Store.All() {
		if r.Status != core.Queued && r.Status != core.HandedOff {
			continue
		}
		t := d.tracker(r.Task.TrackerID)
		if t == nil {
			continue
		}
		st, err := t.IssueState(ctx, r.Task)
		if err != nil {
			continue // better to wait than to drop a task
		}
		if st == "open" && r.Gone != "" {
			d.Store.Update(r.ID, func(r *core.Record) { r.Gone = "" })
		} else if st != "open" && r.Gone != st {
			d.markGone(r, st)
		}
	}
}

func (d *Dispatcher) markGone(r core.Record, st string) {
	what := "Задачу закрито в трекері"
	if st == "deleted" {
		what = "Задачу видалено в трекері"
	}
	d.Log(r.ID, what)
	if r.Status == core.Asking || r.Status == core.Snoozed || r.Status == core.Queued {
		d.Store.Update(r.ID, func(r *core.Record) { r.Status, r.Gone, r.Error, r.FinishedAt = core.Cancelled, st, what, now() })
		return
	}
	d.Store.Update(r.ID, func(r *core.Record) { r.Gone = st })
	notify.Inform("🦈 "+r.ID+": "+strings.ToLower(what), "Агент ще працює над нею в Orca.", d.BoardURL)
}

// AutoArchive takes finished tasks off the board ArchiveAfterDays after they finished (not those in review).
func (d *Dispatcher) AutoArchive(at time.Time) int {
	days := d.Config().ArchiveAfterDays
	if days <= 0 {
		return 0
	}
	n := 0
	for _, r := range d.Store.All() {
		if r.ArchivedAt != 0 || (r.Status != core.Done && r.Status != core.Cancelled) || (r.Status == core.Done && r.PRURL != "" && r.ReviewState == "") {
			continue
		}
		when := r.MergedAt
		if when == "" {
			when = r.FinishedAt
		}
		if when == "" {
			when = r.UpdatedAt
		}
		t, err := time.Parse(time.RFC3339Nano, when)
		if err != nil || at.Sub(t) < time.Duration(days)*24*time.Hour {
			continue
		}
		d.Store.Update(r.ID, func(r *core.Record) { r.ArchivedAt = at.UnixMilli() })
		n++
	}
	return n
}

// --- the developer's buttons

// Later postpones a new task for minutes (the notification's snooze when 0).
func (d *Dispatcher) Later(id string, minutes int) {
	if minutes < 0 {
		// «Повернути»: the task is not put off any more.
		d.Store.Update(id, func(r *core.Record) { r.PostponedByYou, r.SnoozedUntil = false, 0 })
		d.Log(id, "більше не відкладена")
		return
	}
	if minutes == 0 {
		minutes = d.Config().Notifications.SnoozeMin
	}
	until := time.Now().Add(time.Duration(minutes) * time.Minute)
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.PostponedByYou, r.SnoozedUntil = core.Snoozed, true, until.UnixMilli()
	})
	d.Log(id, "відкладено до "+until.Format("02.01 15:04"))
}

// remind: a task you put off whose time has come is back on the board, with a notification.
func (d *Dispatcher) remind(now time.Time) {
	for _, r := range d.Store.All() {
		if r.Status != core.Snoozed || !r.PostponedByYou || r.SnoozedUntil == 0 || r.SnoozedUntil > now.UnixMilli() || r.ArchivedAt != 0 {
			continue
		}
		d.Store.Update(r.ID, func(r *core.Record) { r.PostponedByYou, r.SnoozedUntil = false, 0 })
		d.Log(r.ID, "час повернутися до задачі")
		notify.Inform("⏰ "+r.ID+": час повернутися до задачі", r.Task.Title, d.BoardURL+"/#"+r.ID)
	}
}

// Cancel: Mako stops following the task (the work in Orca stays). An agent in Mako's terminal is stopped; its
// worktree stays.
func (d *Dispatcher) Cancel(id string) {
	d.StopTerminal(id)
	d.Store.Update(id, func(r *core.Record) { r.Status, r.Error, r.FinishedAt = core.Cancelled, "Скасовано", now() })
}

// Retry hands a failed or cancelled task to Orca again, on a new branch (-try2).
func (d *Dispatcher) Retry(ctx context.Context, id string) {
	r, ok := d.Store.Get(id)
	if !ok {
		return
	}
	d.Store.Update(id, func(rec *core.Record) {
		rec.Attempt = max(r.Attempt, 1) + 1
		rec.PRURL, rec.ReviewState, rec.Worktree, rec.Branch, rec.Gone = "", "", "", "", ""
	})
	d.Take(ctx, id)
}

func (d *Dispatcher) Archive(id string, on bool) {
	d.Store.Update(id, func(r *core.Record) {
		r.ArchivedAt = 0
		if on {
			r.ArchivedAt = time.Now().UnixMilli()
		}
	})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
