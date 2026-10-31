package dispatch

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// Autofix: with Terminal.AutoFix, the agent in Mako is asked by itself to fix what its MR in review needs: the
// conflicts, a failed CI, new review comments; the same requests as the MR card's ✨ buttons. Each problem is
// asked about once (a new pipeline or a new comment is a new problem), one request a round, only when the agent
// has finished its turn: an agent that closed while idle is resumed with the request.

// autoFixLimit: requests a task gets by itself at most, so an MR the agent cannot fix does not burn the budget.
const autoFixLimit = 5

// AutoFix hands the agents what their MRs need.
func (d *Dispatcher) AutoFix(ctx context.Context, now time.Time) {
	if !d.Config().Terminal.AutoFix || d.LimitHold() != nil {
		return
	}
	for _, r := range d.Store.All() {
		if r.Status != core.Done || r.PRURL == "" || r.ReviewState != "" || !onTerminal(r) || r.AutoFixes >= autoFixLimit {
			continue
		}
		if r.Worktree == "" || !exists(r.Worktree) {
			continue
		}
		// The reviewer agent reads the branch now: its remarks go first.
		d.mu.Lock()
		reviewing := d.reviewing[r.ID]
		d.mu.Unlock()
		if reviewing {
			continue
		}
		// Working, or waiting for your answer: not now. Done or closed: its turn is over.
		if st := d.lookTerminal(r, now).State; st != "done" && st != "stopped" {
			continue
		}
		c, err := d.Review(ctx, r.ID)
		if err != nil || c == nil || c.State != "open" {
			continue
		}
		kind, key, seen := toFix(c, r.AutoFixed)
		if !maps.Equal(seen, r.AutoFixed) {
			// Fixed meanwhile: a problem that comes back is asked about again.
			d.Store.Update(r.ID, func(rec *core.Record) { rec.AutoFixed = seen })
		}
		if kind == "" {
			continue
		}
		seen[kind] = key
		if err := d.autoAsk(ctx, r.ID, kind); err != nil {
			// Said once: the same problem is not tried again every round; the ✨ button still is there.
			d.Log(r.ID, "автофікс MR не вдався: "+err.Error())
			d.Store.Update(r.ID, func(rec *core.Record) { rec.AutoFixed = seen })
			continue
		}
		d.Store.Update(r.ID, func(rec *core.Record) { rec.AutoFixed, rec.AutoFixes = seen, rec.AutoFixes+1 })
		if r.AutoFixes+1 >= autoFixLimit {
			d.Log(r.ID, fmt.Sprintf("автофікс MR: це %d-й запит, далі — лише кнопками ✨", autoFixLimit))
		}
	}
}

// autoAsk gives the agent the request for kind, resuming it when it is not running.
func (d *Dispatcher) autoAsk(ctx context.Context, id, kind string) error {
	text, err := d.request(ctx, id, kind)
	if err != nil {
		return err
	}
	if d.Live(id) {
		err = d.sayTo(id, text)
	} else {
		err = d.resume(id, text)
	}
	if err != nil {
		return err
	}
	d.asked(id, kind)
	d.Log(id, "автофікс: "+askedLog[kind])
	return nil
}

// toFix: what the MR needs the agent for first, as a kind and a key that names the problem, unless the agent
// was already asked about it (asked); seen is asked without the problems that are gone.
func toFix(c *trackers.ReviewCheck, asked map[string]string) (kind, key string, seen map[string]string) {
	now := map[string]string{}
	if c.Conflicts {
		now["conflicts"] = "1"
	}
	if c.CI == "failed" && len(c.Failed) > 0 {
		ids := make([]string, len(c.Failed))
		for i, j := range c.Failed {
			ids[i] = strconv.FormatInt(j.ID, 10)
		}
		slices.Sort(ids)
		now["ci"] = strings.Join(ids, ",")
	}
	if len(c.Threads) > 0 {
		urls := make([]string, len(c.Threads))
		for i, t := range c.Threads {
			urls[i] = t.URL
		}
		slices.Sort(urls)
		now["comments"] = strings.Join(urls, " ")
	}
	seen = map[string]string{}
	for k, v := range asked {
		if _, still := now[k]; still {
			seen[k] = v
		}
	}
	// The conflicts first: the rebase brings a new pipeline anyway.
	for _, k := range []string{"conflicts", "ci", "comments"} {
		v, ok := now[k]
		if !ok || !fresh(k, v, seen[k]) {
			continue
		}
		return k, v, seen
	}
	return "", "", seen
}

// fresh: the problem v of kind was not asked about yet. Comments are new when one of them is: resolved ones
// alone are not a new request.
func fresh(kind, v, asked string) bool {
	if asked == "" {
		return true
	}
	if kind != "comments" {
		return v != asked
	}
	old := strings.Fields(asked)
	for _, u := range strings.Fields(v) {
		if !slices.Contains(old, u) {
			return true
		}
	}
	return false
}
