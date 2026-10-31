package gitx

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
)

// Worktree is one linked worktree of a repository: its folder and the branch checked out in it ("" when
// detached).
type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
}

// Worktrees: the repository's linked worktrees, not its main checkout.
func Worktrees(ctx context.Context, repo string) ([]Worktree, error) {
	out, err := Run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var all []Worktree
	for i, block := range strings.Split(out, "\n\n") {
		var w Worktree
		for _, l := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(l, "worktree "); ok {
				w.Path = p
			} else if b, ok := strings.CutPrefix(l, "branch "); ok {
				w.Branch = strings.TrimPrefix(b, "refs/heads/")
			}
		}
		// The first one is the main checkout.
		if i > 0 && w.Path != "" {
			all = append(all, w)
		}
	}
	return all, nil
}

// Dirty: how many files of the worktree are changed and not committed (untracked ones too).
func Dirty(ctx context.Context, worktree string) (int, error) {
	out, err := Run(ctx, worktree, "status", "--porcelain")
	if err != nil {
		return 0, err
	}
	return len(lines(out)), nil
}

// HasBranch: the repository has the local branch.
func HasBranch(ctx context.Context, repo, branch string) bool {
	_, err := Run(ctx, repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	return branch != "" && err == nil
}

// DeleteBranch takes a local branch away, merged or not.
func DeleteBranch(ctx context.Context, repo, branch string) error {
	_, err := Run(ctx, repo, "branch", "-D", branch)
	return err
}

// DirSize: the bytes of the files under dir, without following links; what cannot be read counts as nothing.
func DirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
