package dispatch

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// watchedGitLab: the fake GitLab of the tests, whose default branch's CI and alerts the test sets.
type watchedGitLab struct {
	*trackers.GitLab
	ci     trackers.BranchCI
	logs   map[int64]string
	vulns  []trackers.Vulnerability
	ciErr  error
	hang   bool // DefaultCI waits until it is given up
	asked  int
	vAsked int
}

func (w *watchedGitLab) DefaultCI(ctx context.Context, project string) (*trackers.BranchCI, error) {
	w.asked++
	if w.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ci := w.ci
	return &ci, w.ciErr
}

func (w *watchedGitLab) BranchJobLog(ctx context.Context, project string, job trackers.Job, lines int) (string, error) {
	return w.logs[job.ID], nil
}

func (w *watchedGitLab) Vulnerabilities(ctx context.Context, project string) ([]trackers.Vulnerability, error) {
	w.vAsked++
	return w.vulns, nil
}

func watchSetup(t *testing.T, watch config.Watch, routines ...config.Routine) (*Dispatcher, *fakeGitLab, *watchedGitLab) {
	d, f, _ := setup(t)
	oldCI, oldSecurity := watchCIEvery, watchSecurityEvery
	watchCIEvery, watchSecurityEvery = 0, 0
	t.Cleanup(func() { watchCIEvery, watchSecurityEvery = oldCI, oldSecurity })
	p := d.Cfg.Trackers[0].Projects["grp/app"]
	p.Watch, p.Routines = watch, routines
	w := &watchedGitLab{GitLab: d.Trackers[0].(*trackers.GitLab), ci: trackers.BranchCI{Branch: "main", CI: "success", URL: "pipe1"}, logs: map[int64]string{}}
	d.Trackers = []trackers.Tracker{w}
	return d, f, w
}

// made: the tasks the watch wrote, by their title.
func made(d *Dispatcher, by string) []core.Record {
	out := []core.Record{}
	for _, r := range d.Store.All() {
		if r.CreatedBy == by {
			out = append(out, r)
		}
	}
	return out
}

func closeIssue(f *fakeGitLab, r core.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var iid int
	fmt.Sscanf(r.Task.ID, "grp/app#%d", &iid)
	f.issues[iid]["state"] = "closed"
}

func red(w *watchedGitLab, log string) {
	w.ci = trackers.BranchCI{Branch: "main", CI: "failed", URL: "pipe2", Failed: []trackers.Job{{ID: 9, Name: "unit", URL: "job9", Status: "failed"}}}
	w.logs[9] = log
}

// main goes red after green: one task per failure, none more while it is open, a new one once it is closed.
func TestWatchCIRedAfterGreen(t *testing.T) {
	d, f, w := watchSetup(t, config.Watch{CI: true})
	ctx := context.Background()
	now := time.Now()

	// Red when first seen: no task, it may have been red for weeks.
	red(w, "--- FAIL: TestSave\n")
	d.Watch(ctx, now)
	if got := made(d, "ci"); len(got) != 0 {
		t.Fatalf("red at first sight made %d tasks", len(got))
	}
	w.ci = trackers.BranchCI{Branch: "main", CI: "success"}
	d.Watch(ctx, now)
	red(w, "ok   pkg/a\n--- FAIL: TestSave (0.01s)\n    save_test.go:12: got 1\nFAIL\n")
	d.Watch(ctx, now)
	got := made(d, "ci")
	if len(got) != 1 {
		t.Fatalf("red after green made %d tasks", len(got))
	}
	r := got[0]
	if r.Task.Title != "CI на main упав: unit" || !strings.Contains(r.Task.Description, "--- FAIL: TestSave (0.01s)") || !strings.Contains(r.Task.Description, "pipe2") {
		t.Fatalf("task %q\n%s", r.Task.Title, r.Task.Description)
	}
	if !strings.Contains(strings.Join(r.Task.Labels, ","), "claude-agent") || strings.Join(r.Task.Assignees, ",") != "serg" || r.Status != core.Snoozed {
		t.Fatalf("labels %v assignees %v status %s", r.Task.Labels, r.Task.Assignees, r.Status)
	}

	// Still red, then green and red again with the same failure: its task is open, no second one.
	d.Watch(ctx, now)
	w.ci = trackers.BranchCI{Branch: "main", CI: "success"}
	d.Watch(ctx, now)
	red(w, "--- FAIL: TestSave (0.02s)\n")
	d.Watch(ctx, now)
	if got := made(d, "ci"); len(got) != 1 {
		t.Fatalf("the same failure while its task is open made %d tasks", len(got))
	}

	// A different failure is a task of its own.
	w.ci = trackers.BranchCI{Branch: "main", CI: "success"}
	d.Watch(ctx, now)
	red(w, "panic: nil map\n")
	d.Watch(ctx, now)
	if got := made(d, "ci"); len(got) != 2 {
		t.Fatalf("another failure: %d tasks", len(got))
	}

	// Its task closed, the same failure again is written anew.
	closeIssue(f, r)
	w.ci = trackers.BranchCI{Branch: "main", CI: "success"}
	d.Watch(ctx, now)
	red(w, "--- FAIL: TestSave (0.03s)\n")
	d.Watch(ctx, now)
	if got := made(d, "ci"); len(got) != 3 {
		t.Fatalf("after the task was closed: %d tasks", len(got))
	}
}

// What the watch saw survives a restart: green before it, red after it is still a red after a green.
func TestWatchRemembers(t *testing.T) {
	d, _, w := watchSetup(t, config.Watch{CI: true})
	ctx := context.Background()
	d.Watch(ctx, time.Now())
	store, _ := state.Open(d.Cfg.StateDir)
	d2 := New(d.Cfg, store, d.Trackers)
	red(w, "Error: boom\n")
	d2.Watch(ctx, time.Now())
	if got := made(d2, "ci"); len(got) != 1 {
		t.Fatalf("after a restart: %d tasks", len(got))
	}
}

func TestWatchCIEvery(t *testing.T) {
	d, _, w := watchSetup(t, config.Watch{CI: true})
	watchCIEvery = 5 * time.Minute
	ctx := context.Background()
	now := time.Now()
	d.Watch(ctx, now)
	d.Watch(ctx, now.Add(time.Minute))
	d.Watch(ctx, now.Add(6*time.Minute))
	if w.asked != 2 || w.vAsked != 0 {
		t.Fatalf("CI asked %d times, alerts %d", w.asked, w.vAsked)
	}
}

// Taken at once by Mako's own agent, whose spending the ceiling stops; with Orca, which reports none, it waits
// for «Take». Either way the task has the project's ceiling.
func TestWatchCIAutoTake(t *testing.T) {
	for _, executor := range []string{terminalExecutor, "orca"} {
		d, _, w := watchSetup(t, config.Watch{CI: true, AutoTake: true, MaxCost: 3})
		d.Cfg.Executor = executor
		d.Hold = func() string { return "a test: nothing starts" } // taken stays queued
		ctx := context.Background()
		d.Watch(ctx, time.Now())
		red(w, "Error: boom\n")
		d.Watch(ctx, time.Now())
		d.Wait()
		got := made(d, "ci")
		want := map[string]core.Status{terminalExecutor: core.Queued, "orca": core.Snoozed}[executor]
		if len(got) != 1 || got[0].Status != want || got[0].CostCap != 3 {
			t.Fatalf("%s: %+v", executor, got)
		}
	}
}

// The ceiling spent, the agent is stopped and the task fails; it does not start again either.
func TestCostCeiling(t *testing.T) {
	d, _, _ := setup(t)
	d.Store.Update("grp/app#5", func(r *core.Record) {
		r.Task = core.Task{ID: "grp/app#5", TrackerID: d.TrackerList()[0].ID(), Project: "grp/app", Title: "x"}
		r.Status, r.CostCap, r.Cost = core.HandedOff, 2, 1.5
	})
	d.checkCeiling("grp/app#5")
	if r, _ := d.Store.Get("grp/app#5"); r.Status != core.HandedOff {
		t.Fatalf("under the ceiling: %s", r.Status)
	}
	d.Store.Update("grp/app#5", func(r *core.Record) { r.Cost = 2.1 })
	d.checkCeiling("grp/app#5")
	r, _ := d.Store.Get("grp/app#5")
	if r.Status != core.Failed || !strings.Contains(r.Error, "$2.10 of $2.00") {
		t.Fatalf("over the ceiling: %s %q", r.Status, r.Error)
	}
	d.Store.Update("grp/app#5", func(r *core.Record) { r.Status, r.Error = core.Snoozed, "" })
	d.Take(context.Background(), "grp/app#5")
	d.Wait()
	if r, _ := d.Store.Get("grp/app#5"); r.Status != core.Failed || !strings.Contains(r.Error, "cost ceiling") {
		t.Fatalf("taken again over the ceiling: %s %q", r.Status, r.Error)
	}
	// A task without a ceiling has none.
	if ceilingSpent(core.Record{Cost: 100}) != "" {
		t.Error("no ceiling")
	}
}

// A failure whose log could not be read is the same as an open task of its job, and the other way round.
func TestWatchCIWithoutLog(t *testing.T) {
	d, _, w := watchSetup(t, config.Watch{CI: true})
	ctx := context.Background()
	now := time.Now()
	d.Watch(ctx, now)
	red(w, "") // the log is not there
	d.Watch(ctx, now)
	w.ci = trackers.BranchCI{Branch: "main", CI: "success"}
	d.Watch(ctx, now)
	red(w, "--- FAIL: TestSave\n") // the same failure, the log there this time
	d.Watch(ctx, now)
	if got := made(d, "ci"); len(got) != 1 {
		t.Fatalf("without the log, then with it: %d tasks", len(got))
	}
	w.ci = trackers.BranchCI{Branch: "main", CI: "success"}
	d.Watch(ctx, now)
	red(w, "")
	d.Watch(ctx, now)
	if got := made(d, "ci"); len(got) != 1 {
		t.Fatalf("with the log, then without it: %d tasks", len(got))
	}
}

// New alerts make one task for all of them; more alerts while it is open, none.
func TestWatchSecurity(t *testing.T) {
	d, f, w := watchSetup(t, config.Watch{Security: true})
	ctx := context.Background()
	now := time.Now()
	d.Watch(ctx, now)
	if got := made(d, "security"); len(got) != 0 {
		t.Fatalf("no alerts: %d tasks", len(got))
	}
	w.vulns = []trackers.Vulnerability{
		{ID: "1", Package: "lodash", Severity: "high", Summary: "Prototype pollution", URL: "a1"},
		{ID: "2", Package: "express", Severity: "medium", Summary: "Open redirect", URL: "a2"},
	}
	d.Watch(ctx, now)
	got := made(d, "security")
	if len(got) != 1 || got[0].Task.Title != "Оновити залежності: 2 вразливості" || !strings.Contains(got[0].Task.Description, "**lodash** (high) Prototype pollution — a1") {
		t.Fatalf("tasks %+v", got)
	}
	d.Watch(ctx, now)
	w.vulns = append(w.vulns, trackers.Vulnerability{ID: "3", Package: "qs", Summary: "ReDoS"})
	d.Watch(ctx, now)
	if got := made(d, "security"); len(got) != 1 {
		t.Fatalf("more alerts while the task is open: %d tasks", len(got))
	}
	// The task closed: the alert that came while it was open is still news, with all of them in the task.
	first := got[0].ID
	closeIssue(f, got[0])
	d.Watch(ctx, now)
	got = made(d, "security")
	if len(got) != 2 {
		t.Fatalf("the alert that came while the task was open: %d tasks", len(got))
	}
	for _, r := range got {
		if r.ID != first && (r.Task.Title != "Оновити залежності: 3 вразливості" || !strings.Contains(r.Task.Description, "**qs**")) {
			t.Fatalf("the new task: %q\n%s", r.Task.Title, r.Task.Description)
		}
	}
	// Closed again with nothing new: the alerts seen are no news.
	for _, r := range got {
		closeIssue(f, r)
	}
	d.Watch(ctx, now)
	if got := made(d, "security"); len(got) != 2 {
		t.Fatalf("no new alerts: %d tasks", len(got))
	}
}

func TestVulnTitle(t *testing.T) {
	for n, want := range map[int]string{1: "1 вразливість", 3: "3 вразливості", 5: "5 вразливостей", 11: "11 вразливостей", 12: "12 вразливостей", 21: "21 вразливість", 24: "24 вразливості"} {
		if got := vulnTitle(n); got != "Оновити залежності: "+want {
			t.Errorf("%d: %q", n, got)
		}
	}
}

// A routine is written at its time, once for the times missed, and not while the last one is open.
func TestWatchRoutine(t *testing.T) {
	cron, _ := core.ParseCron("0 9 * * 1")
	d, f, _ := watchSetup(t, config.Watch{}, config.Routine{Cron: cron, Title: "Оновити залежності", Body: "npm outdated", AutoTake: false})
	ctx := context.Background()
	at := func(s string) time.Time {
		v, _ := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
		return v
	}
	d.Watch(ctx, at("2026-11-04 10:00")) // Wednesday: first seen
	d.Watch(ctx, at("2026-11-08 23:00"))
	if got := made(d, "routine"); len(got) != 0 {
		t.Fatalf("before its time: %d tasks", len(got))
	}
	d.Watch(ctx, at("2026-11-09 09:00"))
	got := made(d, "routine")
	if len(got) != 1 || got[0].Task.Title != "Оновити залежності" || !strings.Contains(got[0].Task.Description, "npm outdated") || !strings.Contains(got[0].Task.Description, "`0 9 * * 1`") {
		t.Fatalf("at its time: %+v", got)
	}
	d.Watch(ctx, at("2026-11-09 09:01"))
	d.Watch(ctx, at("2026-11-16 09:00")) // a week on, the last one still open
	if got := made(d, "routine"); len(got) != 1 {
		t.Fatalf("while the last one is open: %d tasks", len(got))
	}
	closeIssue(f, got[0])
	d.Watch(ctx, at("2026-11-20 12:00")) // closed meanwhile, but the next Monday is not here yet
	if got := made(d, "routine"); len(got) != 1 {
		t.Fatalf("between its times: %d tasks", len(got))
	}
	d.Watch(ctx, at("2026-12-07 10:00")) // three Mondays missed: one task
	if got := made(d, "routine"); len(got) != 2 {
		t.Fatalf("after the times missed: %d tasks", len(got))
	}
}

// A code host that does not answer is given up: the round goes on.
func TestWatchTimeout(t *testing.T) {
	d, _, w := watchSetup(t, config.Watch{CI: true})
	old := watchTimeout
	watchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { watchTimeout = old })
	w.hang = true
	start := time.Now()
	d.Watch(context.Background(), time.Now())
	if took := time.Since(start); took > 2*time.Second || w.asked != 1 {
		t.Fatalf("took %s, asked %d", took, w.asked)
	}
}

func TestWatchPaused(t *testing.T) {
	d, _, w := watchSetup(t, config.Watch{CI: true, Security: true})
	d.SetPaused(true)
	d.Watch(context.Background(), time.Now())
	if w.asked != 0 || w.vAsked != 0 {
		t.Fatal("paused Mako asked the code host")
	}
}

func TestFirstErrorLine(t *testing.T) {
	cases := map[string]string{
		"": "",
		"setup\n  Error: cannot find module x  \nexit 1\n":                            "Error: cannot find module x",
		"ok a\nok b\nexit status 2\n":                                                 "exit status 2",
		"--- FAIL: TestX (0.1s)\nFAIL\n":                                              "--- FAIL: TestX (0.1s)",
		"errors.go compiled\npanic: boom\n":                                           "panic: boom",
		"2026-11-04T10:00:01.1234567Z ##[error]Process completed with exit code 1.\n": "##[error]Process completed with exit code 1.",
	}
	for log, want := range cases {
		if got := firstErrorLine(log); got != want {
			t.Errorf("%q: %q, want %q", log, got, want)
		}
	}
	if errorKey("--- FAIL: TestSave (0.01s)") != errorKey("--- FAIL: TestSave (0.30s)") || errorKey("--- FAIL: TestSave") == errorKey("--- FAIL: TestLoad") {
		t.Error("the key of a failure")
	}
}
