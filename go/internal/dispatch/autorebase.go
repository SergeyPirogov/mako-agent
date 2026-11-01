package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// Auto-rebase: with Terminal.AutoRebase, the branch of an MR in review is kept up to its base branch. Behind
// without conflicts, Mako rebases (or merges the base in) and pushes by itself; with conflicts the attempt is
// undone and the agent is asked to resolve them, once per commit of the base. Never while the agent works or
// the reviewer agent reads the branch, never after the MR is merged or closed, at most every RebaseEveryMin.

// rebaseEvery: how often a branch is looked at when the settings do not say.
const rebaseEvery = 30 * time.Minute

type baseLook struct {
	at   time.Time
	said string // the last reason the branch was left alone, said once
}

// AutoRebase brings the branches of the MRs in review up to their base branches.
func (d *Dispatcher) AutoRebase(ctx context.Context, now time.Time) {
	cfg := d.Config().Terminal
	mode := cfg.AutoRebase
	if mode == "" {
		return
	}
	every := rebaseEvery
	if cfg.RebaseEveryMin > 0 {
		every = time.Duration(cfg.RebaseEveryMin) * time.Minute
	}
	for _, r := range d.Store.All() {
		if r.Status != core.Done || r.PRURL == "" || r.ReviewState != "" || !onTerminal(r) || branchOf(r) == "" {
			continue
		}
		if r.Worktree == "" || !exists(r.Worktree) {
			continue
		}
		d.mu.Lock()
		reviewing, last := d.reviewing[r.ID], d.rebased[r.ID]
		d.mu.Unlock()
		if reviewing {
			continue
		}
		// Working, or waiting for your answer: its branch is its own now.
		if st := d.lookTerminal(r, now).State; st != "done" && st != "stopped" {
			continue
		}
		if c := r.BaseConflict; c != nil && gitx.Contains(ctx, r.Worktree, c.Base) {
			d.Log(r.ID, "conflict resolved: files "+strings.Join(c.Files, ", "))
			d.Store.Update(r.ID, func(rec *core.Record) { rec.BaseConflict = nil })
			r.BaseConflict = nil
		}
		if now.Sub(last.at) < every {
			continue
		}
		c, err := d.Review(ctx, r.ID)
		if err != nil || c == nil || c.State != "open" {
			continue
		}
		last.at = now
		d.rebaseOne(ctx, r, mode, &last)
		d.mu.Lock()
		d.rebased[r.ID] = last
		d.mu.Unlock()
	}
}

// rebaseOne brings one task's branch up to its base branch, or asks its agent to when it conflicts.
func (d *Dispatcher) rebaseOne(ctx context.Context, r core.Record, mode string, last *baseLook) {
	wt, base, err := d.workOf(r.ID)
	if err != nil {
		return
	}
	gctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	s, err := gitx.UpdateFromBase(gctx, wt, base, branchOf(r), mode)
	if err != nil {
		why := err.Error()
		if errors.Is(err, gitx.ErrDirty) || errors.Is(err, gitx.ErrBranchMoved) {
			why = fmt.Sprintf("branch not updated from %s: %s", base, why)
		} else {
			why = fmt.Sprintf("could not update the branch from %s: %s", base, why)
		}
		if why != last.said {
			d.Log(r.ID, why)
			last.said = why
		}
		return
	}
	last.said = ""
	switch {
	case s.Updated:
		d.forgetReview(r.ID)
		d.Log(r.ID, fmt.Sprintf("updated from %s (%s, %s)", base, mode, commits(s.Behind)))
	case len(s.Conflicts) > 0:
		if r.BaseConflict != nil && r.BaseConflict.Base == s.Base {
			return // asked about this commit of the base already
		}
		if d.LimitHold() != nil || r.AutoFixes >= autoFixLimit {
			return
		}
		text := conflictRequest(r.PRURL, branchOf(r), base, mode, s.Conflicts)
		if d.Live(r.ID) {
			err = d.sayTo(r.ID, text)
		} else {
			err = d.resume(r.ID, text)
		}
		if err != nil {
			d.Log(r.ID, fmt.Sprintf("conflict with %s in files %s, could not ask the agent: %s", base, strings.Join(s.Conflicts, ", "), err))
			return
		}
		d.asked(r.ID, "conflicts")
		d.Store.Update(r.ID, func(rec *core.Record) {
			rec.BaseConflict = &core.BaseConflict{Base: s.Base, Files: s.Conflicts}
			// Autofix need not ask about the same conflicts again.
			if rec.AutoFixed == nil {
				rec.AutoFixed = map[string]string{}
			}
			rec.AutoFixed["conflicts"] = "1"
			rec.AutoFixes++
		})
		d.Log(r.ID, fmt.Sprintf("conflict with %s in files %s: asked the agent to resolve it", base, strings.Join(s.Conflicts, ", ")))
	}
}

// conflictRequest: what the agent is told when its branch conflicts with the base branch.
func conflictRequest(url, branch, base, mode string, files []string) string {
	push := i18n.TA("agent.pushForce")
	how := i18n.TA("agent.howRebase", branch, base)
	if mode == "merge" {
		push = i18n.TA("agent.pushPlain")
		how = i18n.TA("agent.howMerge", base, branch)
	}
	return i18n.TA("agent.conflictBehind", branch, url, base, strings.Join(files, ", "), base, how, push)
}

func commits(n int) string {
	if n == 1 {
		return "1 commit"
	}
	return fmt.Sprintf("%d commits", n)
}
