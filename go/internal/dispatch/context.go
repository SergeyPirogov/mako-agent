package dispatch

import (
	"context"

	"github.com/SergeyPirogov/mako-agent/go/internal/contextstat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
)

// recordContext reads how the task's agent filled its context from its Claude Code transcript (its session's,
// else the worktree's newest one: an agent in Orca) and keeps it, by phase, in place of what it read before.
func (d *Dispatcher) recordContext(id string) {
	r, ok := d.Store.Get(id)
	if !ok || r.Worktree == "" {
		return
	}
	path := orca.Transcript(r.Worktree, r.SessionID)
	if path == "" {
		return
	}
	phase := core.PhaseWork
	if r.Planning {
		phase = core.PhasePlan
	}
	res := contextstat.AnalyzeFile(path, phase)
	if len(res.Runs) == 0 {
		return
	}
	window := res.Window
	if s := d.Chats.Get(id); s != nil && window == 0 {
		window = s.Snapshot().Window
	}
	d.Store.Update(id, func(r *core.Record) {
		for _, run := range res.Runs {
			if run.Window = window; run.Window == 0 {
				run.Window = core.ContextWindow(r.AgentModel, run.Peak)
			}
			r.SetContext(run)
		}
	})
}

// backfillContext reads the context of the tasks taken before it was kept, from the transcripts still there.
func (d *Dispatcher) backfillContext(ctx context.Context) {
	for _, r := range d.Store.All() {
		if ctx.Err() != nil {
			return
		}
		if r.StartedAt == "" || r.Worktree == "" || r.Metrics != nil && len(r.Metrics.Context) > 0 {
			continue
		}
		d.recordContext(r.ID)
	}
}
