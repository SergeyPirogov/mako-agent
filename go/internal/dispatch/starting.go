package dispatch

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

// From «Взяти в роботу» to a running agent Mako prepares the start: the tracker, the base branch, the worktree,
// the setup. The board shows those steps («запускаю») instead of «у черзі», and the journal keeps their times.

// StartStep: one step of a start; Done is 0 while it goes on (ms since the epoch).
type StartStep struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	At    int64  `json:"at"`
	Done  int64  `json:"done,omitempty"`
}

// StartProgress: a task being started. Executor: "orca" or "terminal", for the steps still to come; Setup: the
// setup the start runs ("" none).
// Base: the branch the worktree starts from, for the step that brings it up to date.
type StartProgress struct {
	Since    int64       `json:"since"`
	Executor string      `json:"executor,omitempty"`
	Setup    bool        `json:"setup,omitempty"`
	Base     string      `json:"base,omitempty"`
	Steps    []StartStep `json:"steps"`
	// branchMade: the start created the task's branch, so a cancelled start deletes it.
	branchMade bool
}

// step: the start of id goes on to the next step; the one before is done and the journal gets its time.
func (d *Dispatcher) step(id, key, label string) {
	at := time.Now()
	d.mu.Lock()
	p := d.progress[id]
	if p == nil {
		p = &StartProgress{Since: at.UnixMilli()}
		d.progress[id] = p
	}
	done := d.finishStep(p, at)
	p.Steps = append(p.Steps, StartStep{Key: key, Label: label, At: at.UnixMilli()})
	d.mu.Unlock()
	if done != "" {
		d.Log(id, done)
	}
}

// startPlan: what the start of id will go through, for the steps the board shows ahead.
func (d *Dispatcher) startPlan(id, executor, base string, setup bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.progress[id]
	if p == nil {
		p = &StartProgress{Since: time.Now().UnixMilli()}
		d.progress[id] = p
	}
	p.Executor, p.Base, p.Setup = executor, base, setup
}

// madeBranch: the start of id created its branch.
func (d *Dispatcher) madeBranch(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p := d.progress[id]; p != nil {
		p.branchMade = true
	}
}

// endSteps: the last step of the start is done (its time to the journal); the progress goes with the start.
func (d *Dispatcher) endSteps(id string) {
	d.mu.Lock()
	done := ""
	if p := d.progress[id]; p != nil {
		done = d.finishStep(p, time.Now())
	}
	d.mu.Unlock()
	if done != "" {
		d.Log(id, done)
	}
}

// finishStep marks the current step done; the journal's line for it, "" when none was going on. Under d.mu.
func (d *Dispatcher) finishStep(p *StartProgress, at time.Time) string {
	if len(p.Steps) == 0 || p.Steps[len(p.Steps)-1].Done != 0 {
		return ""
	}
	s := &p.Steps[len(p.Steps)-1]
	s.Done = at.UnixMilli()
	return fmt.Sprintf("%s · %s", s.Label, seconds(time.Duration(s.Done-s.At)*time.Millisecond))
}

func seconds(d time.Duration) string {
	if d < 10*time.Second {
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	return fmt.Sprintf("%.0f s", d.Seconds())
}

// Starting: how far the start of id has got, nil when it is not being started. A start the scheduler has just
// picked (or a redo clearing its last attempt) has no steps of its own yet.
func (d *Dispatcher) Starting(id string) *StartProgress {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.starting[id] {
		return nil
	}
	if p := d.progress[id]; p != nil {
		c := *p
		c.Steps = slices.Clone(p.Steps)
		return &c
	}
	return &StartProgress{Since: time.Now().UnixMilli(), Steps: []StartStep{{Key: "prepare", Label: "preparing the start", At: time.Now().UnixMilli()}}}
}

// stopStart: «Скасувати» while the task is being started stops its start; false when none goes on.
func (d *Dispatcher) stopStart(id string) bool {
	d.mu.Lock()
	stop := d.stops[id]
	d.mu.Unlock()
	if stop != nil {
		stop()
	}
	return stop != nil
}

// cancelled: the start was stopped by «Скасувати»: its worktree goes (Orca's workspace with it), the task stays
// cancelled.
func (d *Dispatcher) cancelled(ctx context.Context, id, repoPath, worktree, branch string, terminal bool) {
	d.endSteps(id)
	d.StopTerminal(id)
	if worktree == "" {
		d.Log(id, "запуск скасовано")
		return
	}
	ctx = context.WithoutCancel(ctx)
	var err error
	if terminal {
		time.Sleep(200 * time.Millisecond) // the agent lets go of the folder
		// The branch goes only if this start made it: one an earlier attempt left may have work on it.
		d.mu.Lock()
		made := d.progress[id] != nil && d.progress[id].branchMade
		d.mu.Unlock()
		if !made {
			branch = ""
		}
		if err = gitx.DropWorktree(ctx, repoPath, worktree, branch); err != nil {
			// Cancelled halfway through `git worktree add`: git may not know the folder yet.
			if rmErr := os.RemoveAll(worktree); rmErr == nil {
				gitx.Run(ctx, repoPath, "worktree", "prune")
				if branch != "" {
					gitx.Run(ctx, repoPath, "branch", "-D", branch)
				}
				err = nil
			}
		}
	} else {
		err = d.RemoveWorkspace(ctx, repoPath, worktree, branch)
	}
	if err != nil {
		d.Fault(id, "запуск скасовано, але worktree лишився", err)
		return
	}
	d.Store.Update(id, func(r *core.Record) { r.Worktree = "" })
	d.Log(id, "запуск скасовано, worktree прибрано")
}

// QueueReason: why a task taken and not being started waits in the queue: Mako waits to update, a Claude limit,
// the WIP limit; "" when nothing holds it.
func (d *Dispatcher) QueueReason() string {
	if why := d.HoldReason(); why != "" {
		return why
	}
	if h := d.LimitHold(); h != nil {
		return fmt.Sprintf("Claude limit (%s) used up to %d%%, until %s", h.Window, h.Used, time.Unix(h.ResetsAt, 0).Format("15:04"))
	}
	limit := d.Config().WipLimit
	if limit <= 0 {
		return ""
	}
	d.mu.Lock()
	active := len(d.starting)
	d.mu.Unlock()
	for _, r := range d.Store.All() {
		if r.Status == core.HandedOff {
			active++
		}
	}
	if active >= limit {
		return fmt.Sprintf("WIP limit: %d of %d in work", active, limit)
	}
	return ""
}
