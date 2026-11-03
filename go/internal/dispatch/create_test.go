package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
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
