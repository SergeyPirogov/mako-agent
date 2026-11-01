package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BranchState: where a task's branch stands against its base branch and its remote.
type BranchState struct {
	Name     string `json:"name"`
	Base     string `json:"base"`
	Ahead    int    `json:"ahead"`    // commits of the task on top of origin/<base>
	Behind   int    `json:"behind"`   // commits on origin/<base> the branch does not have yet
	Pushed   bool   `json:"pushed"`   // origin has the branch
	Unpushed int    `json:"unpushed"` // commits not on origin/<branch> yet
	Dirty    int    `json:"dirty"`    // files changed and not committed
}

// The remote is asked at most every 2 minutes per worktree: the board asks every 5 s.
var (
	fetchedMu sync.Mutex
	fetched   = map[string]time.Time{}
)

const fetchEvery = 2 * time.Minute

// Branch: the state of the branch checked out in worktree.
func Branch(ctx context.Context, worktree, baseBranch string) (*BranchState, error) {
	name, err := Run(ctx, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, err
	}
	b := &BranchState{Name: name, Base: baseBranch}
	fetchedMu.Lock()
	stale := time.Since(fetched[worktree]) > fetchEvery
	if stale {
		fetched[worktree] = time.Now()
	}
	fetchedMu.Unlock()
	// The fetch runs aside: the board shows the branch from the refs it has now, the next ask sees the fresh ones.
	if stale {
		go func() {
			fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			Run(fctx, worktree, "fetch", "-q", "origin", baseBranch)
			Run(fctx, worktree, "fetch", "-q", "origin", name)
		}()
	}
	base := "origin/" + baseBranch
	if _, err := Run(ctx, worktree, "rev-parse", "--verify", "-q", base); err != nil {
		base = baseBranch
	}
	if out, err := Run(ctx, worktree, "rev-list", "--left-right", "--count", base+"...HEAD"); err == nil {
		if f := strings.Fields(out); len(f) == 2 {
			b.Behind, _ = strconv.Atoi(f[0])
			b.Ahead, _ = strconv.Atoi(f[1])
		}
	}
	b.Unpushed = b.Ahead
	if _, err := Run(ctx, worktree, "rev-parse", "--verify", "-q", "origin/"+name); err == nil {
		b.Pushed = true
		if out, err := Run(ctx, worktree, "rev-list", "--count", "origin/"+name+"..HEAD"); err == nil {
			b.Unpushed, _ = strconv.Atoi(out)
		}
	}
	if out, err := Run(ctx, worktree, "--no-optional-locks", "status", "--porcelain"); err == nil {
		b.Dirty = len(lines(out))
	}
	return b, nil
}

// RemoveWorktree takes a finished task's worktree and its local branch away. A worktree with uncommitted
// work stays: git refuses, and that is the point.
func RemoveWorktree(ctx context.Context, repo, worktree, branch string) error {
	if _, err := Run(ctx, repo, "worktree", "remove", worktree); err != nil {
		return err
	}
	fetchedMu.Lock()
	delete(fetched, worktree)
	fetchedMu.Unlock()
	if branch != "" {
		Run(ctx, repo, "branch", "-D", branch)
	}
	return nil
}

// DropWorktree takes a dropped attempt's worktree and local branch away, uncommitted work and all.
func DropWorktree(ctx context.Context, repo, worktree, branch string) error {
	if _, err := Run(ctx, repo, "worktree", "remove", "--force", worktree); err != nil {
		return err
	}
	fetchedMu.Lock()
	delete(fetched, worktree)
	fetchedMu.Unlock()
	if branch != "" {
		Run(ctx, repo, "branch", "-D", branch)
	}
	return nil
}

// GuessSetup: how a new worktree of a JavaScript project gets its dependencies, from its lockfile.
func GuessSetup(worktree string) string {
	for _, l := range []struct{ file, cmd string }{
		{"pnpm-lock.yaml", "pnpm install --frozen-lockfile"},
		{"bun.lock", "bun install --frozen-lockfile"},
		{"bun.lockb", "bun install --frozen-lockfile"},
		{"yarn.lock", "yarn install --frozen-lockfile"},
		{"package-lock.json", "npm ci"},
	} {
		if _, err := os.Stat(filepath.Join(worktree, l.file)); err == nil {
			return l.cmd
		}
	}
	return ""
}
