package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// Auto-take: with «Віддавати нові задачі без питань» a new task goes to work by itself, all of them or, by the
// settings, only those the estimate found small (and clear, with no questions about the description), or clear
// ones the history of tasks like them says will likely go through (core.Outlook: first pass, expected cost). One
// that does not fit stays in «Нові» with the reason on its card; each poll looks at it again, so it goes once
// its estimate comes and fits. A task you put off yourself is yours: Mako does not take it.

// clearEnough: the estimate's clarity from which a task counts as clear (of 5).
const clearEnough = 4

// autoTakeGate: why the settings' AutoTakeOnly leaves a task with this estimate and this outlook by the history
// in «Нові»; "" to take it. The outlook counts only for "likely".
func autoTakeGate(n config.Notifications, e *core.Estimate, o *core.Outlook) string {
	only := n.AutoTakeOnly
	if only != config.AutoTakeSmallClear && only != config.AutoTakeSmall && only != config.AutoTakeLikely {
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
	if only == config.AutoTakeLikely {
		return likelyGate(n, e, o)
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
		if c := unclearWhy(e); c != "" {
			why = append(why, c)
		}
	}
	return strings.Join(why, ", ")
}

// unclearWhy: why the estimate does not find the task clear, "" when it does.
func unclearWhy(e *core.Estimate) string {
	switch {
	case strings.TrimSpace(e.Questions) != "":
		return "the description has questions"
	case e.Clarity.Value < clearEnough:
		return "the description is unclear"
	}
	return ""
}

// likelyGate: "likely" takes a clear task whose outlook gives the settings' first pass or more at their ceiling
// of cost or less.
func likelyGate(n config.Notifications, e *core.Estimate, o *core.Outlook) string {
	var why []string
	if c := unclearWhy(e); c != "" {
		why = append(why, c)
	}
	switch {
	case o == nil:
		why = append(why, fmt.Sprintf("too little history for a forecast (fewer than %d ended tasks like it)", core.OutlookMin))
	default:
		if pass := int(o.FirstPass*100 + 0.5); pass < n.AutoTakeFirstPass {
			why = append(why, fmt.Sprintf("first pass by the history %d%%, below %d%%", pass, n.AutoTakeFirstPass))
		}
		if n.AutoTakeMaxCost > 0 && o.Cost > n.AutoTakeMaxCost {
			why = append(why, fmt.Sprintf("expected cost $%.2f, above $%.2f", o.Cost, n.AutoTakeMaxCost))
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
	var outlook *core.Outlook
	if n.AutoTake && n.AutoTakeOnly == config.AutoTakeLikely {
		outlook = core.OutlookOf(d.Store.All(), r, d.DefaultModel(r))
	}
	why := autoTakeGate(n, r.Estimate, outlook)
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
