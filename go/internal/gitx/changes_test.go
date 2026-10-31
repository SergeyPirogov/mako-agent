package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestChanges(t *testing.T) {
	dir := t.TempDir()
	g := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	g("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "old.txt"), []byte("same\nsame\nsame\n"), 0o644)
	g("add", ".")
	g("commit", "-q", "-m", "init")
	g("checkout", "-q", "-b", "fix/1")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n2\n"), 0o644)
	g("mv", "old.txt", "new.txt")
	g("commit", "-q", "-am", "Fix two")
	os.WriteFile(filepath.Join(dir, "нове.txt"), []byte("x\ny\n"), 0o644)

	ctx := context.Background()
	cs, err := Changes(ctx, dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Commits) != 1 || cs.Commits[0].Subject != "Fix two" {
		t.Fatalf("commits %+v", cs.Commits)
	}
	got := map[string]ChangedFile{}
	for _, f := range cs.Files {
		got[f.Path] = f
	}
	if f := got["a.txt"]; f.Status != "M" || f.Additions != 1 || f.Deletions != 1 {
		t.Fatalf("a.txt %+v", f)
	}
	if got["new.txt"].Status != "R" || got["нове.txt"].Status != "?" || got["нове.txt"].Additions != 2 {
		t.Fatalf("files %+v", cs.Files)
	}
	if d, _ := FileDiff(ctx, dir, "main", "a.txt"); !strings.Contains(d, "-two\n+2") {
		t.Fatalf("diff %q", d)
	}
	if d, _ := FileDiff(ctx, dir, "main", "нове.txt"); !strings.Contains(d, "+x\n+y\n") {
		t.Fatalf("new file %q", d)
	}
	h, err := Log(ctx, dir, "main", 1)
	if err != nil || len(h.Commits) != 1 || !h.More || h.Commits[0].Subject != "Fix two" || !h.Commits[0].Mine || len(h.Commits[0].Parents) != 1 {
		t.Fatalf("log %+v %v", h, err)
	}
	if !strings.Contains(strings.Join(h.Commits[0].Refs, ","), "fix/1") {
		t.Fatalf("refs %v", h.Commits[0].Refs)
	}
	cd, err := CommitInfo(ctx, dir, h.Commits[0].Hash)
	if err != nil || strings.TrimSpace(cd.Message) != "Fix two" || len(cd.Files) != 2 {
		t.Fatalf("commit %+v %v", cd, err)
	}
	if _, err := CommitInfo(ctx, dir, "--all"); err == nil {
		t.Fatal("an option for a hash")
	}
	b, err := Branch(ctx, dir, "main")
	if err != nil || b.Name != "fix/1" || b.Ahead != 1 || b.Behind != 0 || b.Pushed || b.Unpushed != 1 || b.Dirty != 1 {
		t.Fatalf("branch %+v %v", b, err)
	}
	if GuessSetup(dir) != "" {
		t.Fatal("no lockfile, no setup")
	}
	os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), nil, 0o644)
	if GuessSetup(dir) != "pnpm install --frozen-lockfile" {
		t.Fatal(GuessSetup(dir))
	}
	// A path outside the worktree is never read.
	if d, _ := FileDiff(ctx, dir, "main", "../../etc/passwd"); strings.Contains(d, "root") {
		t.Fatal("read outside the worktree")
	}
}

func TestRemoveWorktree(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "app")
	os.MkdirAll(repo, 0o755)
	g := func(d string, args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = d
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	g(repo, "init", "-q", "-b", "main")
	g(repo, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(dir, "wt")
	g(repo, "worktree", "add", "-q", "-b", "fix/2", wt)
	ctx := context.Background()
	// Uncommitted work keeps it.
	os.WriteFile(filepath.Join(wt, "x.txt"), []byte("x"), 0o644)
	if err := RemoveWorktree(ctx, repo, wt, "fix/2"); err == nil {
		t.Fatal("removed a worktree with uncommitted work")
	}
	os.Remove(filepath.Join(wt, "x.txt"))
	if err := RemoveWorktree(ctx, repo, wt, "fix/2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Fatal("worktree still there")
	}
	if out, _ := Run(ctx, repo, "branch", "--list", "fix/2"); out != "" {
		t.Fatal("branch still there")
	}
}
