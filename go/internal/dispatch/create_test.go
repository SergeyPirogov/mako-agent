package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// «+ Задача»: the issue opens in the tracker and is on the board at once; the next search keeps it.
func TestCreateTask(t *testing.T) {
	d, _, jobs := setup(t)
	ctx := context.Background()
	if got := d.NewTaskProjects()[d.TrackerList()[0].ID()]; strings.Join(got, ",") != "grp/app" {
		t.Fatalf("projects %v", got)
	}
	if _, err := d.CreateTask(ctx, d.TrackerList()[0].ID(), trackers.NewIssue{Project: "grp/app", Title: "  "}, false); err == nil {
		t.Fatal("a task without a title was created")
	}
	id, err := d.CreateTask(ctx, d.TrackerList()[0].ID(), trackers.NewIssue{Project: "grp/app", Title: "Експорт у CSV", Description: "Потрібен"}, false)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := d.Store.Get(id)
	if !ok || r.Status != core.Snoozed || r.Task.Title != "Експорт у CSV" || r.Column(0) != "new" {
		t.Fatalf("record %+v", r)
	}
	if !strings.Contains(strings.Join(r.Task.Labels, ","), "claude-agent") || strings.Join(r.Task.Assignees, ",") != "serg" {
		t.Errorf("labels %v assignees %v", r.Task.Labels, r.Task.Assignees)
	}
	d.Poll(ctx)
	if _, ok := d.Store.Get(id); !ok {
		t.Fatal("the next search dropped the created task")
	}

	id2, err := d.CreateTask(ctx, d.TrackerList()[0].ID(), trackers.NewIssue{Project: "grp/app", Title: "Падає збереження", Bug: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	d.Wait()
	if len(*jobs) != 1 || !strings.HasPrefix((*jobs)[0].Branch, "fix/") {
		t.Fatalf("taken at once: %+v", *jobs)
	}
	if r, _ := d.Store.Get(id2); r.Status != core.HandedOff {
		t.Errorf("status %s", r.Status)
	}
}

// A task written on the board is estimated at once, without waiting for the search, when the settings want it.
func TestCreateTaskEstimate(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		d, _, _ := setup(t)
		estimateClaude(t)
		d.Cfg.Estimate = enabled
		ctx := context.Background()
		id, err := d.CreateTask(ctx, d.TrackerList()[0].ID(), trackers.NewIssue{Project: "grp/app", Title: "Експорт у CSV", Description: "Потрібен"}, false)
		if err != nil {
			t.Fatal(err)
		}
		d.WaitEstimates()
		r, _ := d.Store.Get(id)
		if enabled && (r.Estimate == nil || r.Estimate.State != "done" || r.Estimate.For != EstimateKey(r.Task)) {
			t.Errorf("estimate on: %+v", r.Estimate)
		}
		if !enabled && r.Estimate != nil {
			t.Errorf("estimate off, yet: %+v", r.Estimate)
		}
	}
}

// A task made from an agent's plan keeps the plan out of plan mode: the plan is its description already.
func TestCreateTaskFromPlan(t *testing.T) {
	d, _, _ := setup(t)
	ctx := context.Background()
	n := trackers.NewIssue{Project: "grp/app", Title: "Run the plan", Description: "1. Do it\n\n_Plan from chat \"x\"._"}
	plain, err := d.CreateTask(ctx, d.TrackerList()[0].ID(), n, false)
	if err != nil {
		t.Fatal(err)
	}
	fromPlan, err := d.CreateTaskWith(ctx, d.TrackerList()[0].ID(), n, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := d.Store.Get(plain); r.NoPlan {
		t.Error("an ordinary task is marked as made from a plan")
	}
	r, _ := d.Store.Get(fromPlan)
	if !r.NoPlan {
		t.Fatalf("record %+v", r)
	}
	// Even with plan-first "always" its agent does not plan.
	always := New(&config.Config{StateDir: t.TempDir(), Executor: "terminal", Terminal: config.Terminal{PlanFirst: "always"}}, d.Store, nil)
	if always.WillPlan(r) {
		t.Error("made from a plan, and it plans again")
	}
	r.NoPlan = false
	if !always.WillPlan(r) {
		t.Error("an ordinary task does not plan with plan-first always")
	}
}

// A task just made on the board is open but not in the tracker's search yet (GitHub indexes with a delay): the next
// poll keeps it. One that stays out of the search after the lag is removed.
func TestCreatedTaskSurvivesSearchLag(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	id, err := d.CreateTask(ctx, d.TrackerList()[0].ID(), trackers.NewIssue{Project: "grp/app", Title: "Експорт у CSV"}, false)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.unindexed = map[int]bool{101: true}
	f.mu.Unlock()
	d.Poll(ctx)
	if r, ok := d.Store.Get(id); !ok || r.Task.Title != "Експорт у CSV" {
		t.Fatalf("the poll dropped the fresh task: %+v, %v", r, ok)
	}

	old := searchLag
	searchLag = 0
	defer func() { searchLag = old }()
	d.Poll(ctx)
	if _, ok := d.Store.Get(id); ok {
		t.Fatal("a task out of the search long since stayed")
	}
}

// A search cut off at its limit does not list everything: a task missing from it is not removed (it may be past the
// cut), and once the search is complete again the missing one goes as before.
func TestCutOffSearchRemovesNothing(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	if _, ok := d.Store.Get("grp/app#5"); !ok {
		t.Fatal("the task did not come to the board")
	}
	oldLag, oldCap := searchLag, searchCap
	searchLag, searchCap = 0, 1
	defer func() { searchLag, searchCap = oldLag, oldCap }()
	f.mu.Lock()
	f.unindexed = map[int]bool{5: true} // open, but the search does not list it
	f.issues[6] = map[string]any{
		"id": 1006, "iid": 6, "state": "opened", "title": "Інша", "description": "", "web_url": f.base + "/grp/app/-/issues/6",
		"labels": []string{"claude-agent"}, "issue_type": "issue", "assignees": []any{}, "references": map[string]string{"full": "grp/app#6"},
	}
	f.mu.Unlock()
	d.Poll(ctx) // one task found = the limit: cut off
	if _, ok := d.Store.Get("grp/app#5"); !ok {
		t.Fatal("a cut-off search removed a task that may be past the cut")
	}
	searchCap = 2
	d.Poll(ctx) // one found of two possible: complete
	if _, ok := d.Store.Get("grp/app#5"); ok {
		t.Fatal("a complete search kept a task that is not in it")
	}
}

// The estimate of a task that was removed meanwhile does not bring back an empty record.
func TestEstimateOfRemovedTaskLeavesNoRecord(t *testing.T) {
	d, _, _ := setup(t)
	dir := t.TempDir()
	release := filepath.Join(dir, "go")
	script := "#!/bin/sh\ncat >/dev/null\nwhile [ ! -e " + release + " ]; do sleep 0.05; done\n" +
		`echo '{"clarity":{"score":4,"why":"ok"},"questions":[],"size":"small","parts":"web","risk":{"score":2,"why":"UI"},"plan":false,"split":false,"model":"sonnet","advice":""}'` + "\n"
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755)
	old := writer.Claude
	writer.Claude = filepath.Join(dir, "claude")
	defer func() { writer.Claude = old }()
	d.Cfg.Estimate = true

	id, err := d.CreateTask(context.Background(), d.TrackerList()[0].ID(), trackers.NewIssue{Project: "grp/app", Title: "Експорт у CSV"}, false)
	if err != nil {
		t.Fatal(err)
	}
	d.Store.Delete(id)
	os.WriteFile(release, nil, 0o600)
	d.WaitEstimates()
	if r, ok := d.Store.Get(id); ok {
		t.Fatalf("the estimate brought the task back: %+v", r)
	}
	for _, r := range d.Store.All() {
		if r.ID == id {
			t.Fatalf("phantom: %+v", r)
		}
	}
}

// An empty record left by an old stray write (only an estimate in it) is filled with the real task on the next search.
func TestPhantomRecordIsReplaced(t *testing.T) {
	d, _, _ := setup(t)
	d.Store.Update("grp/app#5", func(r *core.Record) { r.Estimate = &core.Estimate{State: "done", Size: "small"} })
	d.Poll(context.Background())
	r, _ := d.Store.Get("grp/app#5")
	if r.Status != core.Snoozed || r.Task.Project != "grp/app" || r.Task.Title == "" {
		t.Fatalf("the phantom stayed: %+v", r)
	}
}

// An empty record whose issue is not in the search at all (closed, reassigned) does not stay in state.json forever.
func TestPhantomRecordIsDropped(t *testing.T) {
	d, _, _ := setup(t)
	d.Store.Update("grp/app#99", func(r *core.Record) { r.Estimate = &core.Estimate{State: "done", Size: "small"} })
	d.Poll(context.Background())
	if r, ok := d.Store.Get("grp/app#99"); ok {
		t.Fatalf("the phantom stayed: %+v", r)
	}
	if _, ok := d.Store.Get("grp/app#5"); !ok {
		t.Fatal("a real task was dropped")
	}
}
