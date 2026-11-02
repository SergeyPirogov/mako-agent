package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
}

func TestWorktreeStates(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	root := t.TempDir()
	plain, det, locked, gone := filepath.Join(root, "plain"), filepath.Join(root, "det"), filepath.Join(root, "locked"), filepath.Join(root, "gone")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "fix/1", plain)
	gitIn(t, repo, "worktree", "add", "-q", "--detach", det)
	gitIn(t, repo, "worktree", "add", "-q", "-b", "fix/2", locked)
	gitIn(t, repo, "worktree", "lock", "--reason", "usb", locked)
	gitIn(t, repo, "worktree", "add", "-q", "-b", "fix/3", gone)
	os.RemoveAll(gone)

	wts, err := Worktrees(ctx, repo)
	if err != nil || len(wts) != 4 {
		t.Fatalf("worktrees: %+v %v", wts, err)
	}
	by := map[string]Worktree{}
	for _, w := range wts {
		by[filepath.Base(w.Path)] = w
	}
	if w := by["plain"]; w.Branch != "fix/1" || w.Detached || w.Locked || w.Prunable {
		t.Fatalf("plain: %+v", w)
	}
	if w := by["det"]; !w.Detached || w.Branch != "" {
		t.Fatalf("detached: %+v", w)
	}
	if w := by["locked"]; !w.Locked {
		t.Fatalf("locked: %+v", w)
	}
	if w := by["gone"]; !w.Prunable {
		t.Fatalf("prunable: %+v", w)
	}
}

func TestWorktreeMetadata(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "fix/1", wt)
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("12345"), 0o644)
	gitIn(t, wt, "add", "a.txt")
	gitIn(t, wt, "commit", "-q", "-m", "Add a")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "on main")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "on main again")

	ahead, behind, err := AheadBehind(ctx, wt, "main")
	if err != nil || ahead != 1 || behind != 2 {
		t.Fatalf("ahead/behind: %d %d %v", ahead, behind, err)
	}
	at, title, err := LastCommit(ctx, wt)
	if err != nil || title != "Add a" || time.Since(at) > time.Minute {
		t.Fatalf("last commit: %v %q %v", at, title, err)
	}
	if HasRemoteBranch(ctx, wt, "fix/1") || HasRemoteBranch(ctx, wt, "") {
		t.Fatal("a remote branch out of nowhere")
	}
	n, mod := DirStat(wt)
	if n < 5 || mod.IsZero() || time.Since(mod) > time.Minute {
		t.Fatalf("dir stat: %d %v", n, mod)
	}
}
