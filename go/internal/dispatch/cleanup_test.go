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

// The cleanup keeps what the branch changed: the worktree that could tell it is gone after.
func TestCleanTaskKeepsChangeStat(t *testing.T) {
	d, id, _, _, cur, _, _, _ := cleanupSetup(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(cur, "a.txt"), []byte("one\ntwo\n"), 0o644)
	git(t, cur, "add", "a.txt")
	git(t, cur, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "a")
	if _, err := d.CleanTask(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if exists(cur) {
		t.Fatal("the worktree is left")
	}
	r, _ := d.Store.Get(id)
	if r.Changed == nil || r.Changed.Files != 1 || r.Changed.Added != 2 || r.Changed.Removed != 0 || r.Changed.Commits != 1 {
		t.Fatalf("changed: %+v", r.Changed)
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

func commitIn(t *testing.T, dir, file string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, file), []byte("x"), 0o644)
	git(t, dir, "add", file)
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "add "+file)
}

// «Видалити вибрані» takes a worktree that is not a task's, with its branch.
func TestRemoveWorktreesNotMako(t *testing.T) {
	d, id, repo, _, cur, _, curBranch, other := cleanupSetup(t)
	ctx := context.Background()
	r, _ := d.Store.Get(id)
	name := d.repoOf(r).Name

	res := d.RemoveWorktrees(ctx, []WorktreeRef{{Repo: name, Path: other}}, false)
	if res.Removed != 1 || res.Branches != 1 || len(res.Kept) != 0 || len(res.Errors) != 0 {
		t.Fatalf("result: %+v", res)
	}
	if exists(other) || gitx.HasBranch(ctx, repo, "fix/9-other") {
		t.Fatal("the worktree or its branch is left")
	}
	if !exists(cur) || !gitx.HasBranch(ctx, repo, curBranch) {
		t.Fatal("removed a worktree that was not chosen")
	}
}

func TestRemoveWorktreesRefusesForeignPaths(t *testing.T) {
	d, id, repo, old, _, _, _, _ := cleanupSetup(t)
	ctx := context.Background()
	r, _ := d.Store.Get(id)
	name := d.repoOf(r).Name
	stray := t.TempDir()

	res := d.RemoveWorktrees(ctx, []WorktreeRef{
		{Repo: name, Path: repo},                                         // the main checkout
		{Repo: name, Path: stray},                                        // not in `git worktree list`
		{Repo: "no-such-repo", Path: old},                                // another repository
		{Repo: name, Path: filepath.Join(old, "..", "wt-1", "..", "..")}, // a parent folder
	}, true)
	if res.Removed != 0 || len(res.Errors) != 4 {
		t.Fatalf("result: %+v", res)
	}
	if !exists(repo) || !exists(stray) || !exists(old) {
		t.Fatal("removed a folder that is not a worktree of the repository")
	}
}

func TestRemoveWorktreesKeepsUnsavedWork(t *testing.T) {
	d, id, repo, _, _, _, _, other := cleanupSetup(t)
	ctx := context.Background()
	r, _ := d.Store.Get(id)
	name := d.repoOf(r).Name
	ref := []WorktreeRef{{Repo: name, Path: other}}

	// Uncommitted file.
	os.WriteFile(filepath.Join(other, "half.txt"), []byte("x"), 0o644)
	res := d.RemoveWorktrees(ctx, ref, false)
	if res.Removed != 0 || len(res.Kept) != 1 || res.Kept[0].Dirty != 1 || !exists(other) {
		t.Fatalf("dirty: %+v", res)
	}
	// An unpushed commit: the branch is on the remote, one commit ahead of it.
	remote := filepath.Join(t.TempDir(), "origin.git")
	git(t, repo, "init", "-q", "--bare", remote)
	git(t, repo, "remote", "set-url", "origin", remote)
	os.Remove(filepath.Join(other, "half.txt"))
	git(t, other, "push", "-q", "origin", "fix/9-other")
	commitIn(t, other, "new.txt")
	res = d.RemoveWorktrees(ctx, ref, false)
	if res.Removed != 0 || len(res.Kept) != 1 || res.Kept[0].Unpushed != 1 || !samePath(res.Kept[0].Path, other) || !exists(other) {
		t.Fatalf("unpushed: %+v", res)
	}
	// «Видалити разом зі змінами».
	res = d.RemoveWorktrees(ctx, ref, true)
	if res.Removed != 1 || res.Branches != 1 || exists(other) || gitx.HasBranch(ctx, repo, "fix/9-other") {
		t.Fatalf("forced: %+v", res)
	}
}

func TestRemoveWorktreesLeavesWorkingAgents(t *testing.T) {
	d, id, repo, _, cur, _, curBranch, _ := cleanupSetup(t)
	ctx := context.Background()
	r, _ := d.Store.Get(id)
	name := d.repoOf(r).Name
	ref := []WorktreeRef{{Repo: name, Path: cur}}

	d.Store.Update(id, func(r *core.Record) { r.Executor = "orca" })
	d.mu.Lock()
	d.agents[id] = &Agent{State: "working"}
	d.mu.Unlock()
	if res := d.RemoveWorktrees(ctx, ref, true); res.Removed != 0 || len(res.Errors) != 1 || !exists(cur) {
		t.Fatalf("under a working agent: %+v", res)
	}
	d.mu.Lock()
	d.agents[id].State = "done"
	d.starting[id] = true
	d.mu.Unlock()
	if res := d.RemoveWorktrees(ctx, ref, true); res.Removed != 0 || len(res.Errors) != 1 || !exists(cur) {
		t.Fatalf("while starting: %+v", res)
	}
	d.mu.Lock()
	delete(d.starting, id)
	d.mu.Unlock()

	// Idle: an Orca task goes through RemoveWorkspace, and stops asking about its worktree.
	var through []string
	d.RemoveWorkspace = func(ctx context.Context, repoPath, worktree, branch string) error {
		through = append(through, branch)
		return gitx.DropWorktree(ctx, repoPath, worktree, branch)
	}
	d.Store.Update(id, func(r *core.Record) { r.WorktreeKept = 3 })
	if res := d.RemoveWorktrees(ctx, ref, false); res.Removed != 1 || len(through) != 1 || through[0] != curBranch {
		t.Fatalf("through Orca: %+v %v", res, through)
	}
	if exists(cur) || gitx.HasBranch(ctx, repo, curBranch) {
		t.Fatal("the worktree or its branch is left")
	}
	if r, _ := d.Store.Get(id); r.WorktreeKept != 0 {
		t.Fatal("the task still asks about the removed worktree")
	}
}

// The settings read what each worktree holds.
func TestWorktreeUsageMetadata(t *testing.T) {
	d, id, repo, _, cur, _, _, other := cleanupSetup(t)
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "origin.git")
	git(t, repo, "init", "-q", "--bare", remote)
	git(t, repo, "remote", "set-url", "origin", remote)
	git(t, other, "push", "-q", "origin", "fix/9-other")
	commitIn(t, other, "new.txt")
	os.WriteFile(filepath.Join(other, "half.txt"), []byte("x"), 0o644)

	var got, task WorktreeSize
	for _, w := range d.WorktreeUsage(ctx)[0].Worktrees {
		switch {
		case samePath(w.Path, other):
			got = w
		case samePath(w.Path, cur):
			task = w
		}
	}
	if got.Dirty != 1 || got.Unpushed != 1 || !got.HasRemote || got.Ahead != 1 || got.Behind != 0 {
		t.Fatalf("git state: %+v", got)
	}
	if got.LastCommitTitle != "add new.txt" || got.LastCommitAt == "" || got.ModifiedAt == "" || got.Bytes == 0 || got.Task != "" {
		t.Fatalf("commit and files: %+v", got)
	}
	if task.Task != id || task.Executor != terminalExecutor || task.ReviewState != "merged" || task.TaskStatus != core.Done || task.HasRemote || task.AgentBusy {
		t.Fatalf("task: %+v", task)
	}
}

// A branch the remote never had: its commits are lost with the worktree, so the clean tree is not enough.
func TestRemoveWorktreesKeepsCommitsNoRemoteHas(t *testing.T) {
	d, id, repo, _, _, _, _, other := cleanupSetup(t)
	ctx := context.Background()
	r, _ := d.Store.Get(id)
	ref := []WorktreeRef{{Repo: d.repoOf(r).Name, Path: other}}

	commitIn(t, other, "local.txt") // never pushed, no origin/fix/9-other at all
	res := d.RemoveWorktrees(ctx, ref, false)
	if res.Removed != 0 || len(res.Kept) != 1 || res.Kept[0].Unpushed != 1 || res.Kept[0].Dirty != 0 || !exists(other) {
		t.Fatalf("local commits: %+v", res)
	}
	if res = d.RemoveWorktrees(ctx, ref, true); res.Removed != 1 || gitx.HasBranch(ctx, repo, "fix/9-other") {
		t.Fatalf("forced: %+v", res)
	}
}

// A detached HEAD with a commit no branch holds.
func TestRemoveWorktreesKeepsDetachedCommits(t *testing.T) {
	d, id, repo, _, _, _, _, _ := cleanupSetup(t)
	ctx := context.Background()
	r, _ := d.Store.Get(id)
	name := d.repoOf(r).Name
	det := filepath.Join(filepath.Dir(repo), "wt-detached")
	git(t, repo, "worktree", "add", "-q", "--detach", det)
	ref := []WorktreeRef{{Repo: name, Path: det}}

	// Nothing of its own: it goes.
	clean := filepath.Join(filepath.Dir(repo), "wt-detached-clean")
	git(t, repo, "worktree", "add", "-q", "--detach", clean)
	if res := d.RemoveWorktrees(ctx, []WorktreeRef{{Repo: name, Path: clean}}, false); res.Removed != 1 || exists(clean) {
		t.Fatalf("detached without commits: %+v", res)
	}
	commitIn(t, det, "lone.txt")
	res := d.RemoveWorktrees(ctx, ref, false)
	if res.Removed != 0 || len(res.Kept) != 1 || res.Kept[0].Unpushed != 1 || !exists(det) {
		t.Fatalf("detached with a commit: %+v", res)
	}
}

// A locked worktree is not taken: git refuses and the error comes back; a worktree whose folder is gone is
// cleared with its branch.
func TestRemoveWorktreesLockedAndPrunable(t *testing.T) {
	d, id, repo, _, _, _, _, other := cleanupSetup(t)
	ctx := context.Background()
	r, _ := d.Store.Get(id)
	name := d.repoOf(r).Name

	git(t, repo, "worktree", "lock", other)
	res := d.RemoveWorktrees(ctx, []WorktreeRef{{Repo: name, Path: other}}, true)
	if res.Removed != 0 || len(res.Errors) != 1 || !exists(other) || !gitx.HasBranch(ctx, repo, "fix/9-other") {
		t.Fatalf("locked: %+v", res)
	}
	git(t, repo, "worktree", "unlock", other)

	// The settings give the path as git lists it.
	listed := ""
	wts, _ := gitx.Worktrees(ctx, repo)
	for _, w := range wts {
		if samePath(w.Path, other) {
			listed = w.Path
		}
	}
	os.RemoveAll(other)
	res = d.RemoveWorktrees(ctx, []WorktreeRef{{Repo: name, Path: listed}}, false)
	if res.Removed != 1 || res.Branches != 1 || len(res.Errors) != 0 || gitx.HasBranch(ctx, repo, "fix/9-other") {
		t.Fatalf("prunable: %+v", res)
	}
	wts, _ = gitx.Worktrees(ctx, repo)
	for _, w := range wts {
		if w.Path == listed {
			t.Fatalf("git still lists %s", other)
		}
	}
}

// The repository's base branch is not taken with the worktree that has it checked out.
func TestRemoveWorktreesKeepsBaseBranch(t *testing.T) {
	d, id, repo, _, _, _, _, _ := cleanupSetup(t)
	ctx := context.Background()
	r, _ := d.Store.Get(id)
	rp := d.repoOf(r)
	base := rp.BaseBranch
	if base == "" {
		base = gitx.DetectBaseBranch(repo)
	}
	if !gitx.HasBranch(ctx, repo, base) {
		git(t, repo, "branch", base)
	}
	git(t, repo, "checkout", "-q", "--detach") // the main checkout lets go of it
	wt := filepath.Join(filepath.Dir(repo), "wt-base")
	git(t, repo, "worktree", "add", "-q", wt, base)

	res := d.RemoveWorktrees(ctx, []WorktreeRef{{Repo: rp.Name, Path: wt}}, false)
	if res.Removed != 1 || exists(wt) || !gitx.HasBranch(ctx, repo, base) {
		t.Fatalf("base branch: %+v", res)
	}
}
