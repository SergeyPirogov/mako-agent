package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/notify"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// --- the watch: a project's default branch and its schedule make tasks nobody wrote (#527)
//
// The default branch's CI going red after green makes a task per failed job, the project's new security alerts
// one task for all of them, a routine its task at its time. Each kind is one open task at a time: while the
// last one with its key is open in the tracker, nothing new is written.

// How often a project's CI and its security alerts are asked; vars for the tests.
var (
	watchCIEvery       = 5 * time.Minute
	watchSecurityEvery = time.Hour
)

// ciLogLines: how much of a failed job's log goes into its task.
const ciLogLines = 60

// watchTimeout: how long one look at a project (its CI, its alerts, a routine's task) may take, so a slow code
// host does not hold up the rest of the round.
var watchTimeout = 30 * time.Second

// watchState is what the watch remembers between restarts, kept in watch.json.
type watchState struct {
	mu     sync.Mutex
	loaded bool
	file   string
	// recovered: watch.json was there but could not be read, so what the watch knew is lost. Until a project's
	// alerts are seen again they are only remembered, not turned into a task (they were most likely written up
	// already); not kept.
	recovered bool
	// asked: when each project's CI and alerts were last asked, by "ci|…", "security|…"; not kept.
	asked map[string]time.Time
	// CI: whether the default branch was green when last seen, by "tracker|project".
	CI map[string]bool `json:"ci"`
	// Open: the task written for each key ("ci|tracker|project|job|first error line", "security|tracker|project",
	// "routine|tracker|project|title").
	Open map[string]string `json:"open"`
	// Vulns: the security alerts already seen, by "tracker|project".
	Vulns map[string][]string `json:"vulns"`
	// Routines: when each routine was last looked at (ms), by "tracker|project|title".
	Routines map[string]int64 `json:"routines"`
}

// load reads watch.json once. A file that cannot be read (empty after a crash, broken) is kept aside as
// watch.json.broken and returned as an error: the watch then starts with nothing remembered, but w.recovered
// keeps it from writing tasks it has most likely written already.
func (w *watchState) load(dir string) error {
	if w.loaded {
		return nil
	}
	w.loaded = true
	w.asked = map[string]time.Time{}
	var problem error
	if dir != "" {
		w.file = filepath.Join(dir, "watch.json")
		data, err := os.ReadFile(w.file)
		if err == nil {
			if len(data) == 0 {
				err = fmt.Errorf("the file is empty")
			} else {
				err = json.Unmarshal(data, w)
			}
			if err != nil {
				w.CI, w.Open, w.Vulns, w.Routines = nil, nil, nil, nil
				to, rerr := keepAsideBroken(w.file)
				if rerr != nil {
					err = fmt.Errorf("%w; and it cannot be kept aside: %v", err, rerr)
				} else {
					err = fmt.Errorf("%w; kept as %s", err, to)
				}
			}
		}
		if err != nil && !os.IsNotExist(err) {
			w.recovered, problem = true, err
			log.Printf("watch.json: CANNOT BE READ (%v): the watch starts from nothing", err)
		}
	}
	if w.CI == nil {
		w.CI = map[string]bool{}
	}
	if w.Open == nil {
		w.Open = map[string]string{}
	}
	if w.Vulns == nil {
		w.Vulns = map[string][]string{}
	}
	if w.Routines == nil {
		w.Routines = map[string]int64{}
	}
	return problem
}

// keepAsideBroken moves a file that cannot be read to file.broken, or to file.broken-<time> when an earlier one
// is already there, so no broken copy is ever written over. It returns where the file went.
func keepAsideBroken(file string) (string, error) {
	to := file + ".broken"
	if _, err := os.Lstat(to); err == nil {
		to = file + ".broken-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	return to, os.Rename(file, to)
}

func (w *watchState) save() {
	if w.file == "" {
		return
	}
	data, err := json.MarshalIndent(w, "", "  ")
	if err == nil {
		err = fsx.WriteFile(w.file, data, 0o600)
	}
	if err != nil {
		// What the watch knows stays in memory only: a restart now would forget it.
		log.Printf("watch.json: not saved: %v", err)
	}
}

// due: key was not asked for every; it is marked as asked now.
func (w *watchState) due(key string, every time.Duration, now time.Time) bool {
	if at, ok := w.asked[key]; ok && now.Sub(at) < every {
		return false
	}
	w.asked[key] = now
	return true
}

// watched: a project with something to watch, as the config has it now.
type watched struct {
	key      string
	watch    config.Watch
	routines []config.Routine
}

// Watch looks at the watched projects' default branches, their security alerts and their routines; it runs in
// every Tick and asks the code hosts only as often as watchCIEvery and watchSecurityEvery say.
func (d *Dispatcher) Watch(ctx context.Context, now time.Time) {
	if _, paused := d.Status(); paused {
		return
	}
	w := &d.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.load(d.Config().StateDir); err != nil {
		d.Fault("watch", "watch.json could not be read", err)
	}
	for _, t := range d.TrackerList() {
		if t.Config().Paused {
			continue
		}
		if _, ok := t.(trackers.Creator); !ok {
			continue
		}
		watcher, _ := t.(trackers.Watcher)
		for _, p := range d.watchedProjects(t) {
			if watcher != nil && p.watch.CI && w.due("ci|"+t.ID()+"|"+p.key, watchCIEvery, now) {
				d.timed(ctx, func(ctx context.Context) { d.watchCI(ctx, t, watcher, p) })
			}
			if watcher != nil && p.watch.Security && w.due("security|"+t.ID()+"|"+p.key, watchSecurityEvery, now) {
				d.timed(ctx, func(ctx context.Context) { d.watchSecurity(ctx, t, watcher, p) })
			}
			for _, r := range p.routines {
				d.timed(ctx, func(ctx context.Context) { d.watchRoutine(ctx, t, p, r, now) })
			}
		}
	}
}

func (d *Dispatcher) timed(ctx context.Context, look func(context.Context)) {
	ctx, cancel := context.WithTimeout(ctx, watchTimeout)
	defer cancel()
	look(ctx)
}

func (d *Dispatcher) watchedProjects(t trackers.Tracker) []watched {
	d.mu.Lock() // ensureProject adds projects under it
	defer d.mu.Unlock()
	out := []watched{}
	for key, p := range t.Config().Projects {
		if p.Watch.CI || p.Watch.Security || len(p.Routines) > 0 {
			out = append(out, watched{key: key, watch: p.Watch, routines: p.Routines})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// openTask: the task written for key is still open in the tracker. When the tracker cannot say, it is taken
// as open: better no task than a second one.
func (d *Dispatcher) openTask(ctx context.Context, t trackers.Tracker, key string) bool {
	id := d.watch.Open[key]
	if id == "" {
		return false
	}
	r, ok := d.Store.Get(id)
	if !ok || r.Gone != "" {
		delete(d.watch.Open, key)
		return false
	}
	st, err := t.IssueState(ctx, r.Task)
	if err == nil && st != "open" {
		delete(d.watch.Open, key)
		return false
	}
	return true
}

// writeTask writes a task of the watch and remembers it under key; the task gets the project's cost ceiling.
// Taken at once only by Mako's own agent: it alone reports what it spends, so the ceiling can stop it; an agent in
// Orca is not watched, its task waits for «Take» in «New».
func (d *Dispatcher) writeTask(ctx context.Context, t trackers.Tracker, key, by string, n trackers.NewIssue, take bool, w config.Watch) bool {
	if take && d.Config().Executor != terminalExecutor {
		take = false
		d.Log(t.ID(), "«"+n.Title+"» is not taken at once: the cost ceiling needs Mako's own agent (Orca does not report its spending)")
	}
	id, err := d.createTask(ctx, t.ID(), n, take, false, by, w.MaxCost, "", nil)
	if err != nil {
		d.Fault(t.ID(), "could not write the task «"+n.Title+"»", err)
		return false
	}
	d.watch.Open[key] = id
	d.watch.save()
	notify.Inform(notify.NewTask, i18n.T("notify.newTask"), id+": "+n.Title, d.BoardURL)
	return true
}

func (d *Dispatcher) watchCI(ctx context.Context, t trackers.Tracker, w trackers.Watcher, p watched) {
	ci, err := w.DefaultCI(ctx, p.key)
	if err != nil {
		d.Fault(t.ID(), "could not read the CI of "+p.key, err)
		return
	}
	state := t.ID() + "|" + p.key
	wasGreen, seen := d.watch.CI[state]
	switch ci.CI {
	case "success":
		if !wasGreen || !seen {
			d.watch.CI[state] = true
			d.watch.save()
		}
		return
	case "failed":
	default:
		return // running, canceled, none: says nothing yet
	}
	if !wasGreen {
		// Red when first seen, or red still: only a red after a green is news.
		if !seen {
			d.watch.CI[state] = false
			d.watch.save()
		}
		return
	}
	failed := ci.Failed
	if len(failed) == 0 {
		failed = []trackers.Job{{Name: "CI", URL: ci.URL}} // failed with no job to point at
	}
	all := true
	for _, job := range failed {
		log := ""
		if job.ID != 0 {
			log, _ = w.BranchJobLog(ctx, p.key, job, ciLogLines)
		}
		first := firstErrorLine(log)
		key := "ci|" + state + "|" + job.Name + "|" + errorKey(first)
		if d.ciTaskOpen(ctx, t, "ci|"+state+"|"+job.Name+"|", key) {
			d.Log(t.ID(), "CI of "+p.key+" failed again in "+job.Name+": its task is still open")
			continue
		}
		body := i18n.TA("watch.ciBody", ci.Branch, ci.URL, job.Name, orStr(job.URL, ci.URL), orStr(first, "—"), orStr(log, "—"))
		n := trackers.NewIssue{Project: p.key, Title: i18n.TA("watch.ciTitle", ci.Branch, job.Name), Description: body, Bug: true}
		if !d.writeTask(ctx, t, key, "ci", n, p.watch.AutoTake, p.watch) {
			all = false
		}
	}
	if all {
		// Not before every task is written: one that failed to be is tried again on the next look.
		d.watch.CI[state] = false
		d.watch.save()
	}
}

// ciTaskOpen: a task for this failure of the job is open. A failure whose log could not be read (key ends with
// the bare job) is the same as any open one of the job, and the other way round: without its first line nobody
// can tell them apart, and one task too few is better than one too many.
func (d *Dispatcher) ciTaskOpen(ctx context.Context, t trackers.Tracker, job, key string) bool {
	if key != job {
		return d.openTask(ctx, t, key) || d.openTask(ctx, t, job)
	}
	keys := []string{}
	for k := range d.watch.Open {
		if strings.HasPrefix(k, job) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if d.openTask(ctx, t, k) {
			return true
		}
	}
	return false
}

func (d *Dispatcher) watchSecurity(ctx context.Context, t trackers.Tracker, w trackers.Watcher, p watched) {
	vs, err := w.Vulnerabilities(ctx, p.key)
	if err != nil {
		d.Fault(t.ID(), "could not read the security alerts of "+p.key, err)
		return
	}
	state := t.ID() + "|" + p.key
	_, seen := d.watch.Vulns[state]
	known := map[string]bool{}
	for _, id := range d.watch.Vulns[state] {
		known[id] = true
	}
	ids, fresh := []string{}, 0
	for _, v := range vs {
		ids = append(ids, v.ID)
		if !known[v.ID] {
			fresh++
		}
	}
	sort.Strings(ids)
	if d.watch.recovered && !seen {
		// The memory of the watch was lost: these alerts were most likely written up already. They are only
		// remembered, so a new task comes for the alerts that appear from now on.
		d.Log(t.ID(), "security alerts of "+p.key+": watch.json was lost, "+fmt.Sprint(len(ids))+" known alerts are remembered without a task")
		d.watch.Vulns[state] = ids
		d.watch.save()
		return
	}
	key := "security|" + state
	if fresh > 0 && d.openTask(ctx, t, key) {
		// Its task is open: the new alerts stay new, so they make the next task once this one is closed.
		return
	}
	if fresh > 0 {
		lines := []string{}
		for _, v := range vs {
			line := "- "
			if v.Package != "" {
				line += "**" + v.Package + "** "
			}
			if v.Severity != "" {
				line += "(" + v.Severity + ") "
			}
			line += v.Summary
			if v.URL != "" {
				line += " — " + v.URL
			}
			lines = append(lines, line)
		}
		n := trackers.NewIssue{Project: p.key, Title: vulnTitle(len(vs)), Description: i18n.TA("watch.vulnBody", len(vs), strings.Join(lines, "\n"))}
		if !d.writeTask(ctx, t, key, "security", n, p.watch.AutoTake, p.watch) {
			return // the alerts stay new: tried again on the next look
		}
	}
	if strings.Join(ids, ",") != strings.Join(d.watch.Vulns[state], ",") {
		d.watch.Vulns[state] = ids
		d.watch.save()
	}
}

// vulnTitle: «Оновити залежності: N вразливостей», the number's form as the agent's language wants it.
func vulnTitle(n int) string {
	form := "Many"
	switch {
	case n == 1 && i18n.AgentLanguage() != i18n.UK, i18n.AgentLanguage() == i18n.UK && n%10 == 1 && n%100 != 11:
		form = "One"
	case i18n.AgentLanguage() == i18n.UK && n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
		form = "Few"
	}
	return i18n.TA("watch.vulnTitle"+form, n)
}

func (d *Dispatcher) watchRoutine(ctx context.Context, t trackers.Tracker, p watched, r config.Routine, now time.Time) {
	project := p.key
	state := t.ID() + "|" + project + "|" + r.Title
	last, ok := d.watch.Routines[state]
	if !ok {
		// First seen: its time is counted from now, not from the past.
		d.watch.Routines[state] = now.UnixMilli()
		d.watch.save()
		return
	}
	next := r.Cron.Next(time.UnixMilli(last))
	if next.IsZero() || next.After(now) {
		return
	}
	key := "routine|" + state
	if d.openTask(ctx, t, key) {
		d.Log(t.ID(), "routine «"+r.Title+"» of "+project+": the last one is still open")
	} else {
		body := strings.TrimSpace(r.Body + "\n\n" + i18n.TA("watch.routineNote", r.Cron.Spec))
		if !d.writeTask(ctx, t, key, "routine", trackers.NewIssue{Project: project, Title: r.Title, Description: body}, r.AutoTake, p.watch) {
			return // tried again on the next round
		}
	}
	// The times missed meanwhile (the Mac slept) are one.
	d.watch.Routines[state] = now.UnixMilli()
	d.watch.save()
}

// ceilingSpent: why the task's agent may not work on it any more, its cost ceiling spent; "" when it may.
func ceilingSpent(r core.Record) string {
	if r.CostCap <= 0 || r.Cost < r.CostCap {
		return ""
	}
	return fmt.Sprintf("the cost ceiling of the task is reached: $%.2f of $%.2f spent, the agent is stopped", r.Cost, r.CostCap)
}

// checkCeiling stops the task's agent and fails the task once it has spent its ceiling (#502 for the tasks Mako
// wrote itself).
func (d *Dispatcher) checkCeiling(id string) {
	r, ok := d.Store.Get(id)
	over := ceilingSpent(r)
	if !ok || over == "" || r.Status == core.Failed {
		return
	}
	d.Log(id, over)
	d.StopTerminal(id)
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.Error, r.ErrorKind, r.FinishedAt = core.Failed, over, core.KindCostCeiling, now()
	})
	notify.Inform(notify.Stopped, i18n.T("notify.attention", id), over, d.BoardURL)
}

var (
	errorLine = regexp.MustCompile(`(?i)\b(error|fail(ed|ure)?|panic|fatal)\b`)
	// logStamp: the time GitHub Actions puts before every line of a log.
	logStamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[\d:.]+Z\s*`)
	numbers  = regexp.MustCompile(`\d+`)
)

// errorKey: a failure's first line without what changes from run to run (its times, the addresses, the
// counts), so the same failure is the same key.
func errorKey(line string) string {
	return numbers.ReplaceAllString(line, "N")
}

// firstErrorLine: the first line of a job's log that says what failed, else its last line; to tell one failure
// from another.
func firstErrorLine(log string) string {
	last := ""
	for _, l := range strings.Split(log, "\n") {
		l = strings.TrimSpace(logStamp.ReplaceAllString(strings.TrimSpace(l), ""))
		if l == "" {
			continue
		}
		if errorLine.MatchString(l) {
			return clipText(l, 200)
		}
		last = l
	}
	return clipText(last, 200)
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
