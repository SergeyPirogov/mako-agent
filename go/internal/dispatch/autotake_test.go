package dispatch

import (
	"context"
	"fmt"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func est(size string, clarity int, questions string) *core.Estimate {
	return &core.Estimate{State: "done", Size: size, Clarity: core.Score{Value: clarity}, Questions: questions}
}

// freshFor: the estimate made for the task's present text.
func freshFor(r *core.Record, e *core.Estimate) *core.Estimate {
	e.For = EstimateKey(r.Task)
	return e
}

func TestAutoTakeGate(t *testing.T) {
	small := est("small", 5, "")
	cases := []struct {
		only string
		e    *core.Estimate
		want string
	}{
		{config.AutoTakeAll, nil, ""},
		{config.AutoTakeAll, est("big", 1, "- що?"), ""},
		{config.AutoTakeSmall, nil, "not estimated yet"},
		{config.AutoTakeSmall, &core.Estimate{State: "running"}, "still being estimated"},
		{config.AutoTakeSmall, &core.Estimate{State: "failed", Error: "timeout"}, "the estimate failed"},
		{config.AutoTakeSmall, small, ""},
		{config.AutoTakeSmall, est("small", 2, "- який трекер?"), ""},
		{config.AutoTakeSmall, est("big", 5, ""), "large"},
		{config.AutoTakeSmall, est("medium", 5, ""), "medium"},
		{config.AutoTakeSmallClear, nil, "not estimated yet"},
		{config.AutoTakeSmallClear, small, ""},
		{config.AutoTakeSmallClear, est("small", 5, "- який трекер?"), "the description has questions"},
		{config.AutoTakeSmallClear, est("small", 3, ""), "the description is unclear"},
		{config.AutoTakeSmallClear, est("big", 5, "- який трекер?"), "large, the description has questions"},
	}
	for _, c := range cases {
		if got := autoTakeGate(config.Notifications{AutoTakeOnly: c.only}, c.e, nil); got != c.want {
			t.Errorf("%s %+v: %q, want %q", c.only, c.e, got, c.want)
		}
	}
}

// «Ймовірно вдасться»: clear, and the history of tasks like it gives the first pass and the cost the settings ask.
func TestAutoTakeGateLikely(t *testing.T) {
	n := config.Notifications{AutoTakeOnly: config.AutoTakeLikely, AutoTakeFirstPass: 70, AutoTakeMaxCost: 2}
	good := &core.Outlook{Tasks: 8, FirstPass: 0.75, Cost: 1.2}
	none := "too little history for a forecast (fewer than 5 ended tasks like it)"
	cases := []struct {
		n    config.Notifications
		e    *core.Estimate
		o    *core.Outlook
		want string
	}{
		{n, nil, good, "not estimated yet"},
		{n, &core.Estimate{State: "running"}, good, "still being estimated"},
		{n, &core.Estimate{State: "failed"}, good, "the estimate failed"},
		{n, est("big", 5, ""), good, ""}, // the size is the history's to weigh
		{n, est("small", 5, ""), nil, none},
		{n, est("small", 5, "- який трекер?"), good, "the description has questions"},
		{n, est("small", 3, ""), nil, "the description is unclear, " + none},
		{n, est("small", 5, ""), &core.Outlook{Tasks: 8, FirstPass: 0.4, Cost: 1.2}, "first pass by the history 40%, below 70%"},
		{n, est("small", 5, ""), &core.Outlook{Tasks: 8, FirstPass: 0.695, Cost: 1.2}, ""}, // 70% rounded
		{n, est("small", 5, ""), &core.Outlook{Tasks: 8, FirstPass: 0.9, Cost: 3.1}, "expected cost $3.10, above $2.00"},
		{n, est("small", 5, ""), &core.Outlook{Tasks: 8, FirstPass: 0.5, Cost: 3.1}, "first pass by the history 50%, below 70%, expected cost $3.10, above $2.00"},
		{config.Notifications{AutoTakeOnly: config.AutoTakeLikely, AutoTakeFirstPass: 70}, est("small", 5, ""), &core.Outlook{Tasks: 8, FirstPass: 0.9, Cost: 30}, ""}, // no ceiling
	}
	for _, c := range cases {
		if got := autoTakeGate(c.n, c.e, c.o); got != c.want {
			t.Errorf("%+v %+v: %q, want %q", c.e, c.o, got, c.want)
		}
	}
}

// «Ймовірно вдасться» on the board: with too short a history the task waits with the reason; once the project has
// enough ended tasks like it that went through, the next poll takes it.
func TestAutoTakeLikely(t *testing.T) {
	d, _, jobs := setup(t)
	d.Cfg.Notifications.AutoTake, d.Cfg.Notifications.AutoTakeOnly = true, config.AutoTakeLikely
	d.Cfg.Notifications.AutoTakeFirstPass = 70
	ctx := context.Background()
	id := "grp/app#5"
	d.Poll(ctx)
	d.Store.Update(id, func(r *core.Record) { r.Estimate = freshFor(r, est("medium", 5, "")) })
	d.Poll(ctx)
	d.Wait()
	r, _ := d.Store.Get(id)
	if r.AutoSkip != "too little history for a forecast (fewer than 5 ended tasks like it)" || len(*jobs) != 0 {
		t.Fatalf("no history: %s %q, %d jobs", r.Status, r.AutoSkip, len(*jobs))
	}
	project := r.Task.Project
	for i := 1; i <= core.OutlookMin; i++ {
		past := fmt.Sprintf("grp/app#%d", 900+i)
		d.Store.Update(past, func(r *core.Record) {
			*r = core.Record{ID: past, Status: core.Done, ReviewState: "merged", Attempt: 1, ArchivedAt: 1,
				StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "2026-10-01T10:30:00Z", Cost: 1,
				Task: core.Task{ID: past, Project: project, Title: "past"}, Estimate: &core.Estimate{State: "done", Size: "medium"}}
		})
	}
	d.Poll(ctx)
	d.Wait()
	if r, _ = d.Store.Get(id); r.Status != core.HandedOff || r.AutoSkip != "" || len(*jobs) != 1 {
		t.Fatalf("likely, not taken: %s %q, %d jobs", r.Status, r.AutoSkip, len(*jobs))
	}
}

// «Усі»: a new task goes to work at once, estimated or not, as before.
func TestAutoTakeAll(t *testing.T) {
	d, _, jobs := setup(t)
	d.Cfg.Notifications.AutoTake, d.Cfg.Notifications.AutoTakeOnly = true, config.AutoTakeAll
	d.Poll(context.Background())
	d.Wait()
	if r, _ := d.Store.Get("grp/app#5"); r.Status != core.HandedOff || len(*jobs) != 1 {
		t.Fatalf("not taken: %s, %d jobs", r.Status, len(*jobs))
	}
}

// «Лише малі й зрозумілі»: a task with no estimate stays in «Нові» with the reason; once its estimate comes and
// fits, the next poll takes it.
func TestAutoTakeWaitsForTheEstimate(t *testing.T) {
	d, _, jobs := setup(t)
	d.Cfg.Notifications.AutoTake, d.Cfg.Notifications.AutoTakeOnly = true, config.AutoTakeSmallClear
	ctx := context.Background()
	id := "grp/app#5"
	d.Poll(ctx)
	d.Wait()
	r, _ := d.Store.Get(id)
	if r.Column(0) != "new" || r.AutoSkip != "not estimated yet" || len(*jobs) != 0 {
		t.Fatalf("taken with no estimate: %s %q, %d jobs", r.Status, r.AutoSkip, len(*jobs))
	}

	d.Store.Update(id, func(r *core.Record) { r.Estimate = freshFor(r, est("small", 5, "- який трекер?")) })
	d.Poll(ctx)
	d.Wait()
	if r, _ = d.Store.Get(id); r.AutoSkip != "the description has questions" || len(*jobs) != 0 {
		t.Fatalf("taken with questions: %s %q", r.Status, r.AutoSkip)
	}

	d.Store.Update(id, func(r *core.Record) { r.Estimate = freshFor(r, est("small", 5, "")) })
	d.Poll(ctx)
	d.Wait()
	if r, _ = d.Store.Get(id); r.Status != core.HandedOff || r.AutoSkip != "" || len(*jobs) != 1 {
		t.Fatalf("small and clear not taken: %s %q, %d jobs", r.Status, r.AutoSkip, len(*jobs))
	}
}

// «Лише малі»: a big task stays; a task you put off yourself is not taken even once it fits.
func TestAutoTakeSmallOnly(t *testing.T) {
	d, _, jobs := setup(t)
	d.Cfg.Notifications.AutoTake, d.Cfg.Notifications.AutoTakeOnly = true, config.AutoTakeSmall
	ctx := context.Background()
	id := "grp/app#5"
	d.Poll(ctx)
	d.Store.Update(id, func(r *core.Record) { r.Estimate = freshFor(r, est("big", 5, "")) })
	d.Poll(ctx)
	d.Wait()
	if r, _ := d.Store.Get(id); r.AutoSkip != "large" || len(*jobs) != 0 {
		t.Fatalf("big task: %s %q", r.Status, r.AutoSkip)
	}

	d.Later(id, 60)
	d.Store.Update(id, func(r *core.Record) { r.Estimate = freshFor(r, est("small", 2, "- що саме?")) })
	d.Poll(ctx)
	d.Wait()
	if r, _ := d.Store.Get(id); r.Status == core.HandedOff || r.AutoSkip != "" || len(*jobs) != 0 {
		t.Fatalf("put off by you, still taken or flagged: %s %q", r.Status, r.AutoSkip)
	}
}

// Auto-take keeps to the WIP limit: what does not fit waits in the queue.
func TestAutoTakeWipLimit(t *testing.T) {
	d, f, jobs := setup(t)
	d.Cfg.Notifications.AutoTake, d.Cfg.Notifications.AutoTakeOnly = true, config.AutoTakeSmall
	ctx := context.Background()
	delete(f.issues, 5)
	for i := 10; i < 13; i++ {
		f.issues[i] = map[string]any{"id": 1000 + i, "iid": i, "state": "opened", "title": fmt.Sprintf("Task %d", i), "web_url": "u",
			"labels": []string{}, "assignees": []any{}, "references": map[string]string{"full": fmt.Sprintf("grp/app#%d", i)}}
	}
	d.Poll(ctx)
	for i := 10; i < 13; i++ {
		d.Store.Update(fmt.Sprintf("grp/app#%d", i), func(r *core.Record) { r.Estimate = freshFor(r, est("small", 5, "")) })
	}
	d.Poll(ctx)
	d.Wait()
	if len(*jobs) != 2 {
		t.Fatalf("WIP 2, handed %d", len(*jobs))
	}
	queued := 0
	for i := 10; i < 13; i++ {
		if r, _ := d.Store.Get(fmt.Sprintf("grp/app#%d", i)); r.Status == core.Queued {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("queued %d, want 1", queued)
	}
}

// An estimate of the task's earlier text does not count: the task stays until it is estimated again.
func TestAutoTakeStaleEstimate(t *testing.T) {
	d, _, jobs := setup(t)
	d.Cfg.Estimate = false
	d.Cfg.Notifications.AutoTake, d.Cfg.Notifications.AutoTakeOnly = true, config.AutoTakeSmall
	ctx := context.Background()
	id := "grp/app#5"
	d.Poll(ctx)
	d.Store.Update(id, func(r *core.Record) {
		old := *r
		old.Task.Description = "інший опис"
		r.Estimate = freshFor(&old, est("small", 5, ""))
	})
	d.Poll(ctx)
	d.Wait()
	if r, _ := d.Store.Get(id); r.AutoSkip != "the description changed after the estimate" || len(*jobs) != 0 {
		t.Fatalf("estimate of old text: %s %q, %d jobs", r.Status, r.AutoSkip, len(*jobs))
	}
}

// The board's outlook and the auto-take gate weigh a model against the same one: the one picked for the task,
// else its work's route.
func TestOutlookModelPickedOrDefault(t *testing.T) {
	d, _, _ := setup(t)
	r := core.Record{ID: "grp/app#5", Status: core.Asking, Task: core.Task{ID: "grp/app#5"}}
	def := d.OutlookModel(r)
	if def == "" || def != d.DefaultModel(r) {
		t.Fatalf("default %q, route %q", def, d.DefaultModel(r))
	}
	r.Model = "haiku"
	if got := d.OutlookModel(r); got != "haiku" {
		t.Fatalf("picked: %q", got)
	}
}
