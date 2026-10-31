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
	// A path outside the worktree is never read.
	if d, _ := FileDiff(ctx, dir, "main", "../../etc/passwd"); strings.Contains(d, "root") {
		t.Fatal("read outside the worktree")
	}
}
