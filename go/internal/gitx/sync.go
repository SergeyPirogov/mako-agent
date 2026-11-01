package gitx

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// Bringing a task's branch up to its base branch while its MR waits for review.

// ErrBranchMoved: origin has commits of the branch the worktree does not: someone pushed it from elsewhere,
// and a force push would drop them.
var ErrBranchMoved = errors.New("the branch on origin was updated from another worktree")

// ErrDirty: the worktree has uncommitted changes; they are not Mako's to move.
var ErrDirty = errors.New("the worktree has uncommitted changes")

// Sync is what UpdateFromBase did.
type Sync struct {
	Behind    int      // commits of origin/<base> the branch did not have
	Base      string   // origin/<base>'s commit it was brought to (or conflicted with)
	Updated   bool     // the branch has origin/<base> now and is pushed
	Conflicts []string // the files that conflicted; the branch is left as it was
}

// Behind: how many commits of origin/<base> the branch in worktree lacks, after fetching it; and that commit.
func Behind(ctx context.Context, worktree, base string) (int, string, error) {
	if _, err := Run(ctx, worktree, "fetch", "-q", "origin", base); err != nil {
		return 0, "", err
	}
	sha, err := Run(ctx, worktree, "rev-parse", "origin/"+base)
	if err != nil {
		return 0, "", err
	}
	out, err := Run(ctx, worktree, "rev-list", "--count", "HEAD..origin/"+base)
	if err != nil {
		return 0, "", err
	}
	n, _ := strconv.Atoi(out)
	return n, sha, nil
}

// UpdateFromBase brings branch in worktree up to origin/<base> by mode ("rebase" or "merge") and pushes it;
// with conflicts it undoes the attempt and says which files conflicted. A branch that is up to date is left
// alone.
func UpdateFromBase(ctx context.Context, worktree, base, branch, mode string) (*Sync, error) {
	behind, sha, err := Behind(ctx, worktree, base)
	if err != nil {
		return nil, err
	}
	s := &Sync{Behind: behind, Base: sha}
	if behind == 0 {
		return s, nil
	}
	if out, err := Run(ctx, worktree, "--no-optional-locks", "status", "--porcelain", "--untracked-files=no"); err != nil {
		return nil, err
	} else if out != "" {
		return s, ErrDirty
	}
	// What origin has of the branch must be in it: the lease below is on that commit.
	lease := ""
	if _, err := Run(ctx, worktree, "fetch", "-q", "origin", branch); err == nil {
		remote, err := Run(ctx, worktree, "rev-parse", "origin/"+branch)
		if err != nil {
			return nil, err
		}
		if _, err := Run(ctx, worktree, "merge-base", "--is-ancestor", remote, "HEAD"); err != nil {
			return s, ErrBranchMoved
		}
		lease = remote
	}
	quiet := []string{"-c", "core.editor=true"}
	if mode == "merge" {
		_, err = Run(ctx, worktree, append(quiet, "merge", "--no-edit", "-m", "Merge origin/"+base+" into "+branch, "origin/"+base)...)
	} else {
		_, err = Run(ctx, worktree, append(quiet, "rebase", "origin/"+base)...)
	}
	if err != nil {
		files, _ := Run(ctx, worktree, "diff", "--name-only", "--diff-filter=U")
		if mode == "merge" {
			Run(ctx, worktree, "merge", "--abort")
		} else {
			Run(ctx, worktree, "rebase", "--abort")
		}
		if files == "" {
			return s, err
		}
		s.Conflicts = lines(files)
		return s, nil
	}
	push := []string{"push", "-q", "origin", "HEAD:refs/heads/" + branch}
	if mode != "merge" && lease != "" {
		push = append(push, "--force-with-lease=refs/heads/"+branch+":"+lease)
	}
	if _, err := Run(ctx, worktree, push...); err != nil {
		return s, err
	}
	s.Updated = true
	return s, nil
}

// Contains: the branch in worktree has commit.
func Contains(ctx context.Context, worktree, commit string) bool {
	_, err := Run(ctx, worktree, "merge-base", "--is-ancestor", strings.TrimSpace(commit), "HEAD")
	return err == nil
}
