package gitx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The worktree's pre-push hook asks Mako before any push, however it is made; it stops the push on anything but
// 204 and when Mako does not answer; the repository's own hooks keep running; other checkouts are not touched.
func TestPushHook(t *testing.T) {
	wt, _, g := syncRepos(t)
	var code atomic.Int32
	var asked atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		asked.Store(r.URL.Query().Get("token") + "|" + string(b))
		if c := int(code.Load()); c != http.StatusNoContent {
			w.WriteHeader(c)
			io.WriteString(w, "Mako stopped the push: secrets in app.go:3")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// A linked worktree of the clone, as Mako's tasks have them; the clone's own hooks are in .git/hooks.
	task := filepath.Join(t.TempDir(), "task")
	g(wt, "worktree", "add", "-q", "-b", "fix/2", task)
	ran := filepath.Join(t.TempDir(), "ran")
	own := "#!/bin/sh\necho \"$(basename \"$0\") $*\" >> " + ran + "\ncat >> " + ran + "\n"
	os.WriteFile(filepath.Join(wt, ".git", "hooks", "pre-push"), []byte(own), 0o755)
	os.WriteFile(filepath.Join(wt, ".git", "hooks", "pre-commit"), []byte(own), 0o755)

	dir := filepath.Join(t.TempDir(), "hooks dir") // a space in the path: the scripts quote it
	if err := WriteHooks(dir, srv.URL+"/prepush?id=t&token=abc"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := InstallHooks(ctx, task, dir); err != nil {
		t.Fatal(err)
	}
	if err := InstallHooks(ctx, task, dir); err != nil { // again, at the next start of the agent
		t.Fatal(err)
	}
	if got := g(task, "config", "--get", "core.hooksPath"); got != dir {
		t.Fatalf("worktree hooksPath %q", got)
	}
	if out, _ := exec.Command("git", "-C", wt, "config", "--get", "core.hooksPath").Output(); len(out) != 0 {
		t.Fatalf("the main checkout got the hooks too: %s", out)
	}

	push := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = task
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	os.WriteFile(filepath.Join(task, "b.txt"), []byte("b\n"), 0o644)
	g(task, "add", ".")
	g(task, "commit", "-q", "-m", "b")

	// Mako says yes: the push goes, the repository's pre-push ran after it with the refs on stdin.
	code.Store(http.StatusNoContent)
	if out, err := push("push", "-q", "origin", "HEAD:refs/heads/fix/2"); err != nil {
		t.Fatalf("clean push stopped: %v %s", err, out)
	}
	if a, _ := asked.Load().(string); !strings.HasPrefix(a, "abc|HEAD ") || !strings.Contains(a, " refs/heads/fix/2 ") {
		t.Fatalf("Mako asked with %q", a)
	}
	b, _ := os.ReadFile(ran)
	if !strings.Contains(string(b), "pre-commit") || !strings.Contains(string(b), "pre-push origin") || !strings.Contains(string(b), "refs/heads/fix/2 ") {
		t.Fatalf("the repository's own hooks: %q", b)
	}

	// Mako says no: stopped, with Mako's reason, whichever way the push is made.
	code.Store(http.StatusConflict)
	os.Remove(ran)
	for _, args := range [][]string{
		{"push", "origin", "HEAD:refs/heads/fix/3"},
		{"-c", "alias.p=push", "p", "origin", "HEAD:refs/heads/fix/3"},
		{"-c", "alias.ship=!git push origin HEAD:refs/heads/fix/3", "ship"},
	} {
		out, err := push(args...)
		if err == nil || !strings.Contains(out, "secrets in app.go:3") {
			t.Errorf("%v: not stopped: %v %s", args, err, out)
		}
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the repository's pre-push ran for a stopped push")
	}

	// Mako does not answer: stopped as well, and said why.
	srv.Close()
	out, err := push("push", "origin", "HEAD:refs/heads/fix/3")
	if err == nil || !strings.Contains(out, "Mako did not answer") {
		t.Fatalf("pushed with Mako down: %v %s", err, out)
	}
	if got := g(wt, "ls-remote", "origin", "fix/3"); got != "" {
		t.Fatalf("fix/3 reached origin: %s", got)
	}
}

func TestInstallHooksRefusesCoreWorktree(t *testing.T) {
	wt, _, g := syncRepos(t)
	g(wt, "config", "core.worktree", wt)
	if err := InstallHooks(context.Background(), wt, t.TempDir()); err == nil {
		t.Fatal("installed with core.worktree set")
	}
}
