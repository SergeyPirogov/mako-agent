package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

// A merged task on its second attempt: the worktree and branch of the first attempt and of this one, and
// someone else's worktree beside them.
func cleanupSetup(t *testing.T) (d *Dispatcher, id, repo, old, cur, oldBranch, curBranch, other string) {
	d, _, _ = setup(t)
	ctx := context.Background()
	d.Poll(ctx)
	id = "grp/app#5"
	r, _ := d.Store.Get(id)
	rp := d.repoOf(r)
	repo = rp.Path
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	oldBranch, curBranch = gitx.BranchName(rp.BranchPattern, r.Task, 1), gitx.BranchName(rp.BranchPattern, r.Task, 2)
	dir := filepath.Dir(repo)
	old, cur, other = filepath.Join(dir, "wt-1"), filepath.Join(dir, "wt-2"), filepath.Join(dir, "wt-other")
	git(t, repo, "worktree", "add", "-q", "-b", oldBranch, old)
	git(t, repo, "worktree", "add", "-q", "-b", curBranch, cur)
	git(t, repo, "worktree", "add", "-q", "-b", "fix/9-other", other)
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.ReviewState, r.Executor, r.Attempt = core.Done, "merged", terminalExecutor, 2
		r.Worktree, r.Branch = cur, curBranch
	})
	return
}

func TestCleanTaskKeepsUncommittedWork(t *testing.T) {
	d, id, repo, old, cur, oldBranch, curBranch, other := cleanupSetup(t)
	ctx := context.Background()

	use := d.WorktreeUsage(ctx)
	if len(use) != 1 || len(use[0].Worktrees) != 3 || use[0].Cleanable != 2 {
		t.Fatalf("usage: %+v", use)
	}

	os.WriteFile(filepath.Join(cur, "half-done.txt"), []byte("x"), 0o644)
	c, err := d.CleanTask(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Worktrees != 1 || c.Branches != 1 || c.Kept != 1 {
		t.Fatalf("cleanup: %+v", c)
	}
	if exists(old) || gitx.HasBranch(ctx, repo, oldBranch) {
		t.Fatal("the first attempt's worktree or branch is left")
	}
	if !exists(filepath.Join(cur, "half-done.txt")) || !gitx.HasBranch(ctx, repo, curBranch) {
		t.Fatal("removed a worktree with uncommitted work")
	}
	if r, _ := d.Store.Get(id); r.WorktreeKept != 1 {
		t.Fatalf("the task does not ask about it: %+v", r.WorktreeKept)
	}

	// «Прибрати все одно».
	if _, err := d.CleanTask(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if exists(cur) || gitx.HasBranch(ctx, repo, curBranch) {
		t.Fatal("forced cleanup left the worktree or branch")
	}
	if r, _ := d.Store.Get(id); r.WorktreeKept != 0 {
		t.Fatal("still asks after the cleanup")
	}
	if !exists(other) || !gitx.HasBranch(ctx, repo, "fix/9-other") {
		t.Fatal("removed another task's worktree")
	}
}

func TestCleanMergedLeavesWorkingAgents(t *testing.T) {
	d, id, repo, old, cur, _, curBranch, _ := cleanupSetup(t)
	ctx := context.Background()

	// The task is in work again.
	d.Store.Update(id, func(r *core.Record) { r.Status = core.HandedOff })
	if _, err := d.CleanTask(ctx, id, true); err == nil {
		t.Fatal("cleaned a task in work")
	}
	// Merged, but its agent in Orca still works.
	d.Store.Update(id, func(r *core.Record) { r.Status, r.Executor = core.Done, "orca" })
	d.mu.Lock()
	d.agents[id] = &Agent{State: "working"}
	d.mu.Unlock()
	if c := d.CleanMerged(ctx); c.Worktrees != 0 || c.Branches != 0 {
		t.Fatalf("cleaned under a working agent: %+v", c)
	}
	if !exists(old) || !exists(cur) || !gitx.HasBranch(ctx, repo, curBranch) {
		t.Fatal("a worktree of a working agent is gone")
	}

	// The agent is done: «Прибрати змерджені» takes both attempts away, through Orca.
	d.mu.Lock()
	d.agents[id].State = "done"
	d.mu.Unlock()
	var removed []string
	d.RemoveWorkspace = func(ctx context.Context, repoPath, worktree, branch string) error {
		removed = append(removed, branch)
		return gitx.DropWorktree(ctx, repoPath, worktree, branch)
	}
	if c := d.CleanMerged(ctx); c.Worktrees != 2 || c.Kept != 0 {
		t.Fatalf("cleanup: %+v (removed %v)", c, removed)
	}
	if exists(old) || exists(cur) || len(removed) != 2 {
		t.Fatalf("left: %v %v, through Orca: %v", exists(old), exists(cur), removed)
	}
}
