package dispatch

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// Assess has the agent rate a finished task (again): how well it was described, how hard it was, how much of
// it is done and how risky the change is. It runs in the background; the board shows it running.
func (d *Dispatcher) Assess(id string) error {
	if err := d.startAssess(id); err != nil {
		return err
	}
	go d.assess(context.Background(), id)
	return nil
}

// startAssess marks the rating as running, unless it cannot or need not start.
func (d *Dispatcher) startAssess(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("no such task")
	}
	if r.Status != core.Done {
		return errors.New("only a completed task can be assessed")
	}
	if a := r.Assessment; a != nil && a.State == "running" && !stale(a.At) {
		return errors.New("the task is already being assessed")
	}
	d.Store.Update(id, func(r *core.Record) { r.Assessment = &core.Assessment{At: now(), State: "running"} })
	return nil
}

// stale: a rating that started long ago and never ended, Mako having stopped meanwhile.
func stale(at string) bool {
	t, err := time.Parse(time.RFC3339Nano, at)
	return err != nil || time.Since(t) > 5*time.Minute
}

// assess rates the task, as summarize writes its summary: in the step that found its MR.
func (d *Dispatcher) assess(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	r, ok := d.Store.Get(id)
	if !ok {
		return
	}
	// A merged task's worktree may be gone: then the task and the summary are what is left to read.
	dir, base, err := d.workOf(id)
	if err != nil {
		dir, base = os.TempDir(), ""
	}
	a, err := writer.Assess(d.spending(ctx, id, core.PhaseAssess), dir, base, r.Task, d.agentWords(ctx, r), r.Summary)
	a.At = now()
	if err != nil {
		d.Fault(id, "task not assessed", err)
		a = core.Assessment{At: a.At, State: "failed", Error: err.Error()}
	} else {
		d.Log(id, "agent assessed the task")
	}
	d.Store.Update(id, func(r *core.Record) { r.Assessment = &a })
}
