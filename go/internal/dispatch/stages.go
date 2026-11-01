package dispatch

import (
	"context"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// Where a taken task's time goes, for the statistics: the agent works, it waits for you, the MR waits for CI or
// for a review. The stage is kept in the record (core.Metrics), so a restart loses nothing.

// workStage: the stage of a task in work (no MR yet) by its agent's state; "" when the state says nothing (Orca
// did not answer).
func workStage(r core.Record, state string) string {
	switch state {
	case "working":
		return core.StageAgent
	case "waiting":
		return core.StageYou
	case "done", "stopped", "idle":
		// Its turn is over: Mako runs the repo's checks before the MR, else it is yours to go on.
		if r.Checks != nil && r.Checks.State == "running" {
			return core.StageCI
		}
		return core.StageYou
	}
	return ""
}

// mrStage: the stage of a task whose MR is open, by its agent's state ("" unknown: an agent in Orca) and the MR's
// check (nil unknown); "" when neither says.
func mrStage(state string, c *trackers.ReviewCheck) string {
	switch state {
	case "working":
		return core.StageAgent
	case "waiting":
		return core.StageYou
	}
	if c == nil {
		return ""
	}
	switch c.CI {
	case "running":
		return core.StageCI
	case "failed":
		return core.StageYou // someone has to ask for the fix
	}
	return core.StageReview
}

// enterStage moves the task to stage, when it is another one.
func (d *Dispatcher) enterStage(r core.Record, stage string, now time.Time) {
	if stage == "" && r.Metrics == nil || r.Metrics != nil && r.Metrics.Stage == stage {
		return
	}
	d.Store.Update(r.ID, func(rec *core.Record) { rec.EnterStage(stage, now) })
}

// TrackStages keeps the stage of the tasks with an open MR and ends it for the ones that came to an end: merged
// (at the merge), closed, failed, cancelled or back in the queue. The tasks in work follow their agent (setAgent).
func (d *Dispatcher) TrackStages(ctx context.Context, now time.Time) {
	for _, r := range d.Store.All() {
		switch {
		case r.Status == core.Done && r.PRURL != "" && r.ReviewState == "":
			state := ""
			if onTerminal(r) {
				state = d.lookTerminal(r, now).State
			}
			var c *trackers.ReviewCheck
			if state != "working" && state != "waiting" {
				c, _ = d.Review(ctx, r.ID)
				if c != nil && c.State != "open" {
					c = nil // merged or closed: CheckMerges records it, and the next round ends the stage
				}
			}
			if s := mrStage(state, c); s != "" {
				d.enterStage(r, s, now)
			}
		case r.Status == core.HandedOff:
			// setAgent follows it.
		case r.Metrics != nil && r.Metrics.Stage != "":
			end := now
			if t, err := time.Parse(time.RFC3339Nano, r.MergedAt); err == nil && r.ReviewState == "merged" && t.Before(now) {
				end = t
			}
			d.enterStage(r, "", end)
		}
	}
}
