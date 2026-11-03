package gitx

import (
	"context"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Worktree is one linked worktree of a repository: its folder and the branch checked out in it ("" when
// detached).
type Worktree struct {
	Path     string `json:"path"`
	Branch   string `json:"branch,omitempty"`
	Detached bool   `json:"detached,omitempty"`
	Locked   bool   `json:"locked,omitempty"`
	Prunable bool   `json:"prunable,omitempty"`
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
			} else if l == "detached" {
				w.Detached = true
			} else if l == "locked" || strings.HasPrefix(l, "locked ") {
				w.Locked = true
			} else if l == "prunable" || strings.HasPrefix(l, "prunable ") {
				w.Prunable = true
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
	out, err := Run(ctx, worktree, "--no-optional-locks", "status", "--porcelain")
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
	n, _ := DirStat(context.Background(), dir)
	return n
}

// DirStat: DirSize and the time the newest file under dir was changed (zero for none), from one walk; a
// cancelled ctx ends the walk with what it has counted.
func DirStat(ctx context.Context, dir string) (int64, time.Time) {
	var n int64
	var newest time.Time
	filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil {
			return nil
		}
		if e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
				if info.ModTime().After(newest) {
					newest = info.ModTime()
				}
			}
		}
		return nil
	})
	return n, newest
}

// AheadBehind: how far the worktree's HEAD is ahead of and behind the base branch (origin/<base> when the
// repository has it).
func AheadBehind(ctx context.Context, worktree, base string) (ahead, behind int, err error) {
	ref := "origin/" + base
	if _, e := Run(ctx, worktree, "rev-parse", "--verify", "-q", ref); e != nil {
		ref = base
	}
	out, err := Run(ctx, worktree, "rev-list", "--left-right", "--count", ref+"...HEAD")
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, nil
	}
	behind, _ = strconv.Atoi(f[0])
	ahead, _ = strconv.Atoi(f[1])
	return ahead, behind, nil
}

// LastCommit: when the worktree's last commit was made and its title.
func LastCommit(ctx context.Context, worktree string) (time.Time, string, error) {
	out, err := Run(ctx, worktree, "log", "-1", "--format=%cI%x00%s")
	if err != nil {
		return time.Time{}, "", err
	}
	at, title, _ := strings.Cut(out, "\x00")
	t, err := time.Parse(time.RFC3339, at)
	return t, title, err
}

// UnsavedCommits: commits that exist only in this worktree and would be lost with it. On a branch the remote
// has, those origin/<branch> lacks. On a branch the remote never had, every commit on top of the base branch
// (Unpushed counts none there). With a detached HEAD, those no branch and no remote holds.
func UnsavedCommits(ctx context.Context, worktree string, detached bool, base string) (int, error) {
	count := func(args ...string) (int, error) {
		out, err := Run(ctx, worktree, append([]string{"rev-list", "--count"}, args...)...)
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(strings.TrimSpace(out))
	}
	if detached {
		return count("HEAD", "--not", "--branches", "--remotes")
	}
	name, err := Run(ctx, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return 0, err
	}
	if HasRemoteBranch(ctx, worktree, name) {
		return Unpushed(ctx, worktree), nil
	}
	ahead, _, err := AheadBehind(ctx, worktree, base)
	return ahead, err
}

// HasRemoteBranch: the repository has origin/<branch>.
func HasRemoteBranch(ctx context.Context, worktree, branch string) bool {
	if branch == "" {
		return false
	}
	_, err := Run(ctx, worktree, "rev-parse", "--verify", "-q", "origin/"+branch)
	return err == nil
}

// Unpushed: commits of the worktree's branch that its remote branch (origin/<branch>) does not have yet.
// Zero when the remote branch is gone (a merged branch is often deleted there): nothing to compare with.
func Unpushed(ctx context.Context, worktree string) int {
	name, err := Run(ctx, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || name == "HEAD" {
		return 0
	}
	if _, err := Run(ctx, worktree, "rev-parse", "--verify", "-q", "origin/"+name); err != nil {
		return 0
	}
	out, err := Run(ctx, worktree, "rev-list", "--count", "origin/"+name+"..HEAD")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}
