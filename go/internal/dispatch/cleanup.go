package dispatch

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

// A merged or archived task leaves nothing behind on the Mac: its worktree, its local branch, and the
// branches (and worktrees) of its earlier attempts go. A worktree with uncommitted work stays until you say so,
// and so does everything of a task whose agent still works.

// Cleanup: what one cleaning took away and what it left.
type Cleanup struct {
	Worktrees int `json:"worktrees"`
	Branches  int `json:"branches"`
	// Kept: uncommitted files that kept a worktree; KeptTasks: the tasks they belong to.
	Kept      int      `json:"kept"`
	KeptTasks []string `json:"keptTasks,omitempty"`
}

func (c *Cleanup) add(o Cleanup) {
	c.Worktrees += o.Worktrees
	c.Branches += o.Branches
	c.Kept += o.Kept
	c.KeptTasks = append(c.KeptTasks, o.KeptTasks...)
}

// cleanable: the task is over for good: merged, or taken off the board.
func cleanable(r core.Record) bool {
	return r.ReviewState == "merged" || r.ArchivedAt != 0
}

// agentBusy: the task's agent is still at it, in Mako's terminal or in Orca.
func (d *Dispatcher) agentBusy(r core.Record) bool {
	if onTerminal(r) {
		return !d.agentIdle(r.ID)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.agents[r.ID]
	return a != nil && (a.State == "working" || a.State == "waiting")
}

// CleanTask takes away the task's worktree, its local branch and those of its earlier attempts; force takes
// a worktree with uncommitted work too. Without force such a worktree stays and the task asks about it
// (WorktreeKept).
func (d *Dispatcher) CleanTask(ctx context.Context, id string, force bool) (Cleanup, error) {
	var c Cleanup
	r, ok := d.Store.Get(id)
	if !ok {
		return c, fmt.Errorf("немає такої задачі")
	}
	switch r.Status {
	case core.Asking, core.Snoozed, core.Queued, core.HandedOff:
		return c, fmt.Errorf("задача ще в роботі")
	}
	d.mu.Lock()
	starting := d.starting[id]
	d.mu.Unlock()
	if starting || d.agentBusy(r) {
		return c, fmt.Errorf("агент ще працює")
	}
	repo := d.repoOf(r)
	if repo == nil || !exists(repo.Path) {
		return c, nil
	}
	wts, err := gitx.Worktrees(ctx, repo.Path)
	if err != nil {
		return c, err
	}

	branches := attemptBranches(r, repo)
	mine, had := map[string]bool{}, map[string]bool{}
	for _, b := range branches {
		mine[b], had[b] = true, gitx.HasBranch(ctx, repo.Path, b)
	}
	stopped := false
	busy := map[string]bool{} // branches still checked out somewhere
	for _, w := range wts {
		if !samePath(w.Path, r.Worktree) && !mine[w.Branch] {
			busy[w.Branch] = true
			continue
		}
		n, err := gitx.Dirty(ctx, w.Path)
		if err != nil || (n > 0 && !force) {
			c.Kept += n
			busy[w.Branch] = true
			continue
		}
		if onTerminal(r) {
			if !stopped && (d.Sessions.Get(id) != nil || d.Chats.Get(id) != nil) {
				d.StopTerminal(id)
				time.Sleep(500 * time.Millisecond) // the agent lets go of the folder
			}
			stopped = true
			err = gitx.DropWorktree(ctx, repo.Path, w.Path, w.Branch)
		} else {
			err = d.RemoveWorkspace(ctx, repo.Path, w.Path, w.Branch)
		}
		if err != nil {
			d.Fault(id, "worktree лишився", err)
			busy[w.Branch] = true
			continue
		}
		c.Worktrees++
	}
	for _, b := range branches {
		if !had[b] {
			continue
		}
		if !busy[b] && gitx.HasBranch(ctx, repo.Path, b) {
			gitx.DeleteBranch(ctx, repo.Path, b)
		}
		if !gitx.HasBranch(ctx, repo.Path, b) {
			c.Branches++
		}
	}

	if c.Kept > 0 {
		c.KeptTasks = []string{id}
	}
	d.Store.Update(id, func(rec *core.Record) { rec.WorktreeKept = c.Kept })
	switch {
	case c.Kept > 0:
		d.Log(id, fmt.Sprintf("worktree лишився: %d незакомічених змін", c.Kept))
	case c.Worktrees > 0 || c.Branches > 0:
		d.Log(id, fmt.Sprintf("прибрано worktree: %d, локальних гілок: %d", c.Worktrees, c.Branches))
	}
	return c, nil
}

// KeepWorktree: you said to leave the task's worktree with its uncommitted work; Mako stops asking.
func (d *Dispatcher) KeepWorktree(id string) {
	d.Store.Update(id, func(r *core.Record) { r.WorktreeKept = 0 })
	d.Log(id, "worktree залишено")
}

// cleanUp: a task that is over cleans after itself without asking, unless it is not quite over.
func (d *Dispatcher) cleanUp(ctx context.Context, id string) {
	if r, ok := d.Store.Get(id); !ok || !cleanable(r) {
		return
	}
	if _, err := d.CleanTask(ctx, id, false); err != nil {
		d.Log(id, "worktree не прибрано: "+err.Error())
	}
}

// CleanMerged: «Прибрати змерджені» in the settings: every merged or archived task's leftovers.
func (d *Dispatcher) CleanMerged(ctx context.Context) Cleanup {
	var all Cleanup
	for _, r := range d.Store.All() {
		if !cleanable(r) {
			continue
		}
		if c, err := d.CleanTask(ctx, r.ID, false); err == nil {
			all.add(c)
		}
	}
	return all
}

// attemptBranches: the task's branch now and those of its earlier attempts (-try2, …).
func attemptBranches(r core.Record, repo *config.Repo) []string {
	seen := map[string]bool{}
	var out []string
	add := func(b string) {
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	add(r.Branch)
	add(r.BranchName)
	for k := 1; k < r.Attempt; k++ {
		add(gitx.BranchName(repo.BranchPattern, r.Task, k))
	}
	return out
}

// repoOf: the repository the task was done in, nil when it is not configured any more.
func (d *Dispatcher) repoOf(r core.Record) *config.Repo {
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return nil
	}
	repo, err := d.route(t, r.Task)
	if err != nil {
		return nil
	}
	return repo
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// WorktreeUse: how much of the disk one project's worktrees take, for the settings.
type WorktreeUse struct {
	Repo      string         `json:"repo"`
	Path      string         `json:"path"`
	Bytes     int64          `json:"bytes"`
	Worktrees []WorktreeSize `json:"worktrees"`
	// Cleanable: the worktrees of merged or archived tasks, which «Прибрати змерджені» takes away.
	Cleanable      int   `json:"cleanable"`
	CleanableBytes int64 `json:"cleanableBytes"`
}

type WorktreeSize struct {
	gitx.Worktree
	Bytes     int64  `json:"bytes"`
	Task      string `json:"task,omitempty"`
	Cleanable bool   `json:"cleanable,omitempty"`
}

// WorktreeUsage: the worktrees of every configured repository and their size, the biggest project first.
func (d *Dispatcher) WorktreeUsage(ctx context.Context) []WorktreeUse {
	records := d.Store.All()
	seen := map[string]bool{}
	out := []WorktreeUse{}
	for _, t := range d.Config().Trackers {
		for _, p := range t.Projects {
			for _, repo := range p.Repos {
				if seen[repo.Path] || !exists(repo.Path) {
					continue
				}
				seen[repo.Path] = true
				wts, err := gitx.Worktrees(ctx, repo.Path)
				if err != nil || len(wts) == 0 {
					continue
				}
				u := WorktreeUse{Repo: repo.Name, Path: repo.Path}
				for _, w := range wts {
					s := WorktreeSize{Worktree: w, Bytes: gitx.DirSize(w.Path)}
					for _, r := range records {
						if r.Repo != "" && r.Repo != repo.Name {
							continue
						}
						if samePath(w.Path, r.Worktree) || (w.Branch != "" && slices.Contains(attemptBranches(r, repo), w.Branch)) {
							s.Task, s.Cleanable = r.ID, cleanable(r)
							break
						}
					}
					u.Bytes += s.Bytes
					if s.Cleanable {
						u.Cleanable++
						u.CleanableBytes += s.Bytes
					}
					u.Worktrees = append(u.Worktrees, s)
				}
				sort.Slice(u.Worktrees, func(i, j int) bool { return u.Worktrees[i].Bytes > u.Worktrees[j].Bytes })
				out = append(out, u)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}
