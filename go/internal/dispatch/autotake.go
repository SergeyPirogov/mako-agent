package dispatch

import (
	"context"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Auto-take: with «Віддавати нові задачі без питань» a new task goes to work by itself, all of them or, by the
// settings, only those the estimate found small (and clear, with no questions about the description). One
// that does not fit stays in «Нові» with the reason on its card; each poll looks at it again, so it goes once
// its estimate comes and fits. A task you put off yourself is yours: Mako does not take it.

// clearEnough: the estimate's clarity from which a task counts as clear (of 5).
const clearEnough = 4

// autoTakeGate: why the settings' AutoTakeOnly leaves a task with this estimate in «Нові»; "" to take it.
func autoTakeGate(only string, e *core.Estimate) string {
	if only != config.AutoTakeSmallClear && only != config.AutoTakeSmall {
		return ""
	}
	switch {
	case e == nil:
		return "not estimated yet"
	case e.State == "running":
		return "still being estimated"
	case e.State != "done":
		return "the estimate failed"
	}
	var why []string
	switch e.Size {
	case "small":
	case "medium":
		why = append(why, "medium")
	case "big":
		why = append(why, "large")
	default:
		why = append(why, "size unknown")
	}
	if only == config.AutoTakeSmallClear {
		if strings.TrimSpace(e.Questions) != "" {
			why = append(why, "the description has questions")
		} else if e.Clarity.Value < clearEnough {
			why = append(why, "the description is unclear")
		}
	}
	return strings.Join(why, ", ")
}

// autoTake takes a new task when the settings let it, or notes on it why not.
func (d *Dispatcher) autoTake(ctx context.Context, id string) {
	r, ok := d.Store.Get(id)
	if !ok || (r.Status != core.Asking && r.Status != core.Snoozed) {
		return
	}
	n := d.Config().Notifications
	why := autoTakeGate(n.AutoTakeOnly, r.Estimate)
	// An estimate of the task's earlier text says nothing of its present one.
	if why == "" && n.AutoTakeOnly != config.AutoTakeAll && !estimateFits(r.Estimate, r.Task) {
		why = "the description changed after the estimate"
	}
	if !n.AutoTake || r.PostponedByYou {
		why = ""
	}
	if n.AutoTake && !r.PostponedByYou && why == "" {
		d.take(ctx, id, "taken into work automatically")
		return
	}
	if why != r.AutoSkip {
		if why != "" {
			d.Log(id, "not taking it myself: "+why)
		}
		d.Store.Update(id, func(r *core.Record) { r.AutoSkip = why })
	}
}
