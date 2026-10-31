package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// syncRepos: a remote with main, a clone of it with branch fix/1 pushed, and a second clone to move main in.
func syncRepos(t *testing.T) (wt, other string, g func(dir string, args ...string) string) {
	dir := t.TempDir()
	g = func(d string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = d
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	remote := filepath.Join(dir, "origin.git")
	g(dir, "init", "-q", "--bare", "-b", "main", remote)
	other = filepath.Join(dir, "other")
	g(dir, "clone", "-q", remote, other)
	os.WriteFile(filepath.Join(other, "a.txt"), []byte("one\n"), 0o644)
	g(other, "add", ".")
	g(other, "commit", "-q", "-m", "init")
	g(other, "push", "-q", "origin", "main")
	wt = filepath.Join(dir, "wt")
	g(dir, "clone", "-q", remote, wt)
	g(wt, "checkout", "-q", "-b", "fix/1")
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("mine\n"), 0o644)
	g(wt, "add", ".")
	g(wt, "commit", "-q", "-m", "Add b")
	g(wt, "push", "-q", "origin", "fix/1")
	return wt, other, g
}

func commitTo(t *testing.T, g func(string, ...string) string, dir, file, body string) {
	os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644)
	g(dir, "add", ".")
	g(dir, "commit", "-q", "-m", "Change "+file)
	g(dir, "push", "-q", "origin", "HEAD")
}

func TestUpdateFromBaseClean(t *testing.T) {
	for _, mode := range []string{"rebase", "merge"} {
		t.Run(mode, func(t *testing.T) {
			wt, other, g := syncRepos(t)
			ctx := context.Background()
			// Up to date: left alone.
			if s, err := UpdateFromBase(ctx, wt, "main", "fix/1", mode); err != nil || s.Updated || s.Behind != 0 {
				t.Fatalf("up to date: %+v %v", s, err)
			}
			commitTo(t, g, other, "c.txt", "theirs\n")
			s, err := UpdateFromBase(ctx, wt, "main", "fix/1", mode)
			if err != nil || !s.Updated || s.Behind != 1 || len(s.Conflicts) != 0 {
				t.Fatalf("update: %+v %v", s, err)
			}
			// origin has the branch with main in it.
			g(wt, "fetch", "-q", "origin")
			g(wt, "merge-base", "--is-ancestor", "origin/main", "origin/fix/1")
			if g(wt, "rev-parse", "HEAD") != g(wt, "rev-parse", "origin/fix/1") {
				t.Fatal("not pushed")
			}
			if !Contains(ctx, wt, s.Base) {
				t.Fatal("base not in the branch")
			}
		})
	}
}

func TestUpdateFromBaseConflict(t *testing.T) {
	wt, other, g := syncRepos(t)
	ctx := context.Background()
	commitTo(t, g, other, "b.txt", "theirs\n")
	before := g(wt, "rev-parse", "HEAD")
	s, err := UpdateFromBase(ctx, wt, "main", "fix/1", "rebase")
	if err != nil || s.Updated || len(s.Conflicts) != 1 || s.Conflicts[0] != "b.txt" {
		t.Fatalf("conflict: %+v %v", s, err)
	}
	// Undone: the branch is as it was and no rebase is left half-way.
	if g(wt, "rev-parse", "HEAD") != before || g(wt, "status", "--porcelain") != "" {
		t.Fatal("branch touched")
	}
	if _, err := os.Stat(filepath.Join(wt, ".git", "rebase-merge")); err == nil {
		t.Fatal("rebase left in progress")
	}
	if Contains(ctx, wt, s.Base) {
		t.Fatal("contains the base")
	}
	if s, err := UpdateFromBase(ctx, wt, "main", "fix/1", "merge"); err != nil || len(s.Conflicts) != 1 || g(wt, "rev-parse", "HEAD") != before {
		t.Fatalf("merge conflict: %+v %v", s, err)
	}
}

func TestUpdateFromBaseLeavesAlone(t *testing.T) {
	wt, other, g := syncRepos(t)
	ctx := context.Background()
	commitTo(t, g, other, "c.txt", "theirs\n")
	// Uncommitted work.
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("half\n"), 0o644)
	if _, err := UpdateFromBase(ctx, wt, "main", "fix/1", "rebase"); !errors.Is(err, ErrDirty) {
		t.Fatalf("dirty: %v", err)
	}
	g(wt, "checkout", "--", "b.txt")
	// The branch pushed from elsewhere: no force push over it.
	g(other, "fetch", "-q", "origin")
	g(other, "checkout", "-q", "-b", "fix/1", "origin/fix/1")
	commitTo(t, g, other, "d.txt", "elsewhere\n")
	before := g(other, "rev-parse", "HEAD")
	if _, err := UpdateFromBase(ctx, wt, "main", "fix/1", "rebase"); !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("moved: %v", err)
	}
	if g(wt, "ls-remote", "origin", "refs/heads/fix/1") != before+"\trefs/heads/fix/1" {
		t.Fatal("remote branch changed")
	}
}
