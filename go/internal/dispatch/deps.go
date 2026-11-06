package dispatch

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// Wait: what a task waits for before it may start: another task, and until what of it.
type Wait struct {
	On string `json:"on"`
	// Why: "merge" until On is merged; "mr" until On has an MR (the task starts from its branch); "closed" On's
	// MR was closed without merging; "open" On is open in the tracker and not on the board.
	Why string `json:"why"`
	URL string `json:"url,omitempty"` // On's issue, when the board has it
}

func (w Wait) String() string {
	switch w.Why {
	case "mr":
		return "waits for an MR of " + w.On
	case "closed":
		return "waits for " + w.On + ": its MR was closed without merging"
	case "open":
		return "blocked by " + w.On + " in the tracker"
	}
	return "waits for " + w.On + " to be merged"
}

// blockers: what the task waits for, the tracker's links first, then the tasks Mako put before it; each once.
func blockers(r core.Record) []string {
	var ids []string
	for _, id := range append(slices.Clone(r.Task.BlockedBy), r.After...) {
		if id != "" && id != r.ID && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// waitingFor: what the task still waits for (nil: nothing) and, when it may start from the branch of its StackOn
// in review, that task. get gives the board's record of a task.
func waitingFor(r core.Record, get func(string) (core.Record, bool)) (*Wait, *core.Record) {
	var stack *core.Record
	for _, id := range blockers(r) {
		b, ok := get(id)
		if !ok || (b.Status == "" && b.Task.Project == "") {
			if slices.Contains(r.Task.BlockedBy, id) {
				return &Wait{On: id, Why: "open"}, nil
			}
			continue // a task Mako put first that is not on the board any more
		}
		if b.ReviewState == "merged" || b.Gone != "" {
			continue // closed in the tracker without an MR: whoever closed it decided it is not in the way
		}
		w := &Wait{On: id, Why: "merge", URL: b.Task.URL}
		switch {
		case b.ReviewState == "closed":
			w.Why = "closed"
		case id == r.StackOn && b.PRURL != "" && branchOf(b) != "":
			stack = &b
			continue
		case id == r.StackOn:
			w.Why = "mr"
		}
		return w, nil
	}
	return nil, stack
}

// WaitsFor: what the task waits for before it may start; nil when nothing (or when it starts from the branch of
// a task in review).
func (d *Dispatcher) WaitsFor(r core.Record) *Wait {
	w, _ := waitingFor(r, d.Store.Get)
	return w
}

// stackRepo: the repo the task starts in, from the branch of stack (the task it waits for, in review) instead
// of the base branch: the worktree, the agent's MR and its diff all go from that branch. The commit it starts
// from is said, to move only the task's own commits once that task is merged.
func (d *Dispatcher) stackRepo(ctx context.Context, id string, repo *config.Repo, stack core.Record) (*config.Repo, *core.Stack) {
	rc := *repo
	rc.BaseBranch = branchOf(stack)
	fetchBase(ctx, &rc, func(s string) { d.Log(id, s) })
	tip, err := gitx.Run(ctx, repo.Path, "rev-parse", "origin/"+rc.BaseBranch)
	if err != nil {
		tip = ""
	}
	return &rc, &core.Stack{On: stack.ID, Branch: rc.BaseBranch, Tip: strings.TrimSpace(tip)}
}

// taskBase: the branch the task's work goes into: the branch of the task it started from while that one is in
// review, else the repo's base branch.
func taskBase(r core.Record, repo *config.Repo) string {
	if r.Stack != nil && r.Stack.Branch != "" {
		return r.Stack.Branch
	}
	return repo.BaseBranch
}

// stackNote: what the journal says of a task starting from another's branch.
func stackNote(s *core.Stack) string {
	return fmt.Sprintf("starts from the branch %s of %s, in review: its MR goes there until %s is merged", s.Branch, s.On, s.On)
}

// stackMerged: the task started from the branch of a task that is merged now: its branch and its MR are to move
// onto the base branch.
func (d *Dispatcher) stackMerged(r core.Record) bool {
	if r.Stack == nil {
		return false
	}
	on, ok := d.Store.Get(r.Stack.On)
	return ok && on.ReviewState == "merged"
}

// Restack: the tasks started from the branch of a task merged since. A task not started yet just starts from the
// base branch; a started one gets its own commits moved onto the base branch (rebase --onto the commit it started
// from: a squashed merge leaves no commits of the other task to rebase over) and its MR pointed there. A task
// whose agent works waits for it; conflicts go to its agent, once.
func (d *Dispatcher) Restack(ctx context.Context, now time.Time) {
	for _, r := range d.Store.All() {
		if !d.stackMerged(r) {
			continue
		}
		if r.Worktree == "" || !exists(r.Worktree) {
			if r.PRURL == "" || r.ReviewState != "" {
				d.Store.Update(r.ID, func(rec *core.Record) { rec.Stack = nil })
				d.Log(r.ID, r.Stack.On+" is merged: the task goes from "+r.Stack.Branch+" no more")
			} else {
				d.restackSay(r.ID, "MR not moved onto the base branch: the task has no worktree")
			}
			continue
		}
		if d.restackBusy(r, now) {
			continue
		}
		d.restackOne(ctx, r)
	}
}

// restackBusy: the task's branch is not Mako's to move now: its agent works on it, or the reviewer reads it.
func (d *Dispatcher) restackBusy(r core.Record, now time.Time) bool {
	d.mu.Lock()
	reviewing, starting := d.reviewing[r.ID], d.starting[r.ID]
	d.mu.Unlock()
	if reviewing || starting {
		return true
	}
	if !onTerminal(r) {
		return r.Status == core.HandedOff // Orca's agent: its own until it has an MR
	}
	st := d.lookTerminal(r, now).State
	return st != "done" && st != "stopped"
}

func (d *Dispatcher) restackOne(ctx context.Context, r core.Record) {
	st := *r.Stack
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return
	}
	repo, err := d.route(t, r.Task)
	if err != nil {
		return
	}
	base := baseOf(repo)
	gctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if found, err := d.CheckSecrets(gctx, r.ID); err != nil || len(found) > 0 {
		d.restackSay(r.ID, fmt.Sprintf("branch not moved onto %s: the changes look like they contain secrets", base))
		return
	}
	s, err := gitx.UpdateOnto(gctx, r.Worktree, base, st.Tip, branchOf(r))
	if err != nil {
		d.restackSay(r.ID, fmt.Sprintf("branch not moved onto %s after %s was merged: %s", base, st.On, err))
		return
	}
	if len(s.Conflicts) > 0 {
		if st.Asked == st.Tip || d.LimitHold() != nil || d.AuthHold() != nil {
			return
		}
		text := conflictRequest(r.PRURL, branchOf(r), base, "rebase", st.Tip, s.Conflicts)
		if d.Live(r.ID) {
			err = d.sayTo(r.ID, text)
		} else {
			err = d.resume(r.ID, text)
		}
		if err != nil {
			d.restackSay(r.ID, fmt.Sprintf("conflict with %s in files %s, could not ask the agent: %s", base, strings.Join(s.Conflicts, ", "), err))
			return
		}
		d.Store.Update(r.ID, func(rec *core.Record) {
			if rec.Stack != nil {
				rec.Stack.Asked = st.Tip
			}
		})
		d.Log(r.ID, fmt.Sprintf("%s is merged; moving the branch onto %s conflicts in files %s: asked the agent to resolve it", st.On, base, strings.Join(s.Conflicts, ", ")))
		return
	}
	if r.PRURL != "" && r.ReviewState == "" {
		host, _, err := d.host(ctx, t, repo)
		if err == nil {
			if rt, ok := host.(trackers.Retargeter); ok {
				err = rt.Retarget(ctx, r.PRURL, base)
			} else {
				err = fmt.Errorf("%s cannot change an MR's target branch", host.Host())
			}
		}
		if err != nil {
			d.restackSay(r.ID, fmt.Sprintf("branch moved onto %s, the MR still goes into %s: %s", base, st.Branch, err))
			return // tried again next round: the branch is up to date by then, only the MR is left
		}
	}
	d.Store.Update(r.ID, func(rec *core.Record) { rec.Stack = nil })
	d.forgetReview(r.ID)
	d.mu.Lock()
	delete(d.rebased, "stack:"+r.ID)
	d.mu.Unlock()
	d.Log(r.ID, fmt.Sprintf("%s is merged: the branch is moved onto %s (%s) and its MR goes there", st.On, base, commits(s.Behind)))
}

// restackSay logs why a task's branch was not moved, once until the reason changes.
func (d *Dispatcher) restackSay(id, why string) {
	key := "stack:" + id
	d.mu.Lock()
	last := d.rebased[key]
	same := last.said == why
	last.said = why
	d.rebased[key] = last
	d.mu.Unlock()
	if !same {
		d.Log(id, why)
	}
}
