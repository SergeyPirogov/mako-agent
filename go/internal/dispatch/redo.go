package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// «Повторити інакше»: the agent went the wrong way. Mako drops the attempt (the agent, its MR, its worktree)
// and starts the task over on a new branch (-try2) with your note and a short account of the last attempt.

// redoable: in work, in review, or not done; not new, queued or merged.
func redoable(r core.Record) bool {
	switch r.Status {
	case core.HandedOff, core.Failed, core.Cancelled:
		return true
	case core.Done:
		return r.ReviewState != "merged"
	}
	return false
}

// Redo starts the task over differently: note is what to do differently, model the agent's model for the new
// attempt ("" — the configured one). The old attempt is cleaned up in the background, then the task is queued.
func (d *Dispatcher) Redo(ctx context.Context, id, note, model string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return fmt.Errorf("немає такої задачі")
	}
	if !redoable(r) {
		return fmt.Errorf("повторити можна задачу в роботі, на рев'ю чи не виконану")
	}
	d.mu.Lock()
	if d.starting[id] {
		d.mu.Unlock()
		return fmt.Errorf("задача вже починається")
	}
	// Starting: the scheduler leaves it alone (and counts it in the WIP) until the old attempt is gone.
	d.starting[id] = true
	d.mu.Unlock()
	note, model = strings.TrimSpace(note), strings.TrimSpace(model)
	d.Store.Update(id, func(r *core.Record) { r.Status, r.Error, r.ArchivedAt = core.Queued, "", 0 })
	d.Log(id, "повторюю інакше"+map[bool]string{true: ": " + clipText(firstLine(note), 120)}[note != ""])
	go func() {
		ctx := context.WithoutCancel(ctx)
		last := d.dropAttempt(ctx, r, note)
		d.Store.Update(id, func(rec *core.Record) {
			rec.Attempt = max(r.Attempt, 1) + 1
			rec.RetryNote, rec.LastAttempt, rec.Model = note, last, model
			rec.PRURL, rec.ReviewState, rec.MergedAt, rec.Worktree, rec.Branch, rec.Gone = "", "", "", "", "", ""
			rec.AgentReviews, rec.Assessment = nil, nil
			rec.Summary, rec.SessionID, rec.FinishedAt = "", "", ""
		})
		d.forgetReview(id)
		d.mu.Lock()
		delete(d.agents, id)
		d.mu.Unlock()
		d.release(ctx, id)
	}()
	return nil
}

// dropAttempt stops the agent, sums up what it did, closes its MR and removes its worktree; it returns the
// account of the attempt for the next one's prompt.
func (d *Dispatcher) dropAttempt(ctx context.Context, r core.Record, note string) string {
	words := d.agentWords(ctx, r)
	d.StopTerminal(r.ID)
	t := d.tracker(r.Task.TrackerID)
	var repoPath, base string
	if t != nil {
		if repo, err := d.route(t, r.Task); err == nil {
			repoPath, base = repo.Path, repo.BaseBranch
		}
	}

	last := ""
	if r.Worktree != "" && exists(r.Worktree) {
		if base == "" {
			base = gitx.DetectBaseBranch(r.Worktree)
		}
		s, err := writer.AttemptSummary(ctx, r.Worktree, base, r.Task, words, note)
		if err != nil {
			d.Fault(r.ID, "підсумок минулої спроби не написано", err)
		} else {
			last = s
		}
	}
	if last == "" {
		last = r.Summary // written when its MR appeared
	}

	if r.PRURL != "" && r.ReviewState == "" {
		if desk, err := d.desk(ctx, r.ID); err != nil {
			d.Fault(r.ID, "MR не закрито", err)
		} else if err := desk.Close(ctx, r.PRURL); err != nil {
			d.Fault(r.ID, "MR не закрито", err)
		} else {
			d.Log(r.ID, "MR минулої спроби закрито: "+r.PRURL)
		}
	}

	if r.Worktree == "" || !exists(r.Worktree) || repoPath == "" {
		return last
	}
	var err error
	if onTerminal(r) {
		time.Sleep(500 * time.Millisecond) // the agent lets go of the folder
		err = gitx.DropWorktree(ctx, repoPath, r.Worktree, branchOf(r))
	} else {
		err = d.RemoveWorkspace(ctx, repoPath, r.Worktree, branchOf(r))
	}
	if err != nil {
		d.Fault(r.ID, "worktree минулої спроби лишився", err)
	} else {
		d.Log(r.ID, "worktree минулої спроби прибрано")
	}
	return last
}

// orcaRemove: the Orca workspace goes with its agent.
func (d *Dispatcher) orcaRemove(ctx context.Context, repoPath, worktree, branch string) error {
	return orca.Remove(ctx, d.Config().Orca, repoPath, worktree, branch)
}
