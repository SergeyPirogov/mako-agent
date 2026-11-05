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

// A hooks folder the user set for all repositories (shared hooks in ~/.git-hooks) ran in the worktree before
// Mako's hooks; it still does, for hooks other than pre-push and for pre-push once Mako has said yes. A
// repository's own setting wins over the user's.
func TestHooksKeepTheUsersOwn(t *testing.T) {
	wt, _, g := syncRepos(t)
	shared := t.TempDir()
	ran := filepath.Join(t.TempDir(), "ran")
	script := func(name string) string { return "#!/bin/sh\necho " + name + " >> " + ran + "\n" }
	global := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(global, []byte("[core]\n\thooksPath = "+shared+"\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	os.WriteFile(filepath.Join(shared, "pre-commit"), []byte(script("shared-pre-commit")), 0o755)
	os.WriteFile(filepath.Join(shared, "pre-push"), []byte(script("shared-pre-push")), 0o755)

	var code atomic.Int32
	code.Store(http.StatusNoContent)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(int(code.Load())) }))
	defer srv.Close()
	task := filepath.Join(t.TempDir(), "task")
	g(wt, "worktree", "add", "-q", "-b", "fix/2", task)
	dir := t.TempDir()
	if err := WriteHooks(dir, srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := InstallHooks(context.Background(), task, dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(task, "b.txt"), []byte("b\n"), 0o644)
	g(task, "add", ".")
	g(task, "commit", "-q", "-m", "b")
	g(task, "push", "-q", "origin", "HEAD:refs/heads/fix/2")
	b, _ := os.ReadFile(ran)
	if string(b) != "shared-pre-commit\nshared-pre-push\n" {
		t.Fatalf("the user's hooks ran: %q", b)
	}

	// Mako says no: the user's pre-push does not run for a push that is stopped.
	os.Remove(ran)
	code.Store(http.StatusConflict)
	os.WriteFile(filepath.Join(task, "c.txt"), []byte("c\n"), 0o644)
	g(task, "add", ".")
	g(task, "commit", "-q", "-m", "c")
	cmd := exec.Command("git", "push", "origin", "HEAD:refs/heads/fix/2")
	cmd.Dir = task
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("pushed: %s", out)
	}
	if b, _ := os.ReadFile(ran); strings.Contains(string(b), "pre-push") {
		t.Fatalf("the user's pre-push ran for a stopped push: %q", b)
	}

	// The repository's own setting is the one that counts over the user's.
	own := t.TempDir()
	os.WriteFile(filepath.Join(own, "pre-commit"), []byte(script("repo-pre-commit")), 0o755)
	g(wt, "config", "core.hooksPath", own)
	os.Remove(ran)
	os.WriteFile(filepath.Join(task, "d.txt"), []byte("d\n"), 0o644)
	g(task, "add", ".")
	g(task, "commit", "-q", "-m", "d")
	if b, _ := os.ReadFile(ran); string(b) != "repo-pre-commit\n" {
		t.Fatalf("the repository's hooks: %q", b)
	}
}

// RefsDiff reads what git says it will push, whichever branch is checked out: a new ref from the base branch on,
// an old one from what the remote has; a deleted ref carries nothing, a bad line is an error.
func TestRefsDiff(t *testing.T) {
	wt, _, g := syncRepos(t) // on fix/1, pushed
	ctx := context.Background()
	zero := strings.Repeat("0", 40)
	put := func(file, body string) string {
		os.WriteFile(filepath.Join(wt, file), []byte(body), 0o644)
		g(wt, "add", ".")
		g(wt, "commit", "-q", "-m", "Change "+file)
		return g(wt, "rev-parse", "HEAD")
	}
	remoteTip := g(wt, "rev-parse", "HEAD")
	first := put("one.go", "package one // first\n")
	second := put("two.go", "package two // second\n")
	g(wt, "switch", "-q", "-c", "elsewhere", "main")
	other := put("other.go", "package other // elsewhere\n")
	g(wt, "switch", "-q", "fix/1")

	has := func(diff string, ss ...string) bool {
		for _, s := range ss {
			if !strings.Contains(diff, s) {
				return false
			}
		}
		return true
	}
	// An old ref: only what the remote does not have.
	d, err := RefsDiff(ctx, wt, "main", "refs/heads/fix/1 "+second+" refs/heads/fix/1 "+first+"\n")
	if err != nil || !has(d, "+package two // second") || has(d, "first") {
		t.Fatalf("old ref: %v %q", err, d)
	}
	// A new ref, from another branch than the checked-out one: everything since the base branch.
	d, err = RefsDiff(ctx, wt, "main", "refs/heads/elsewhere "+other+" refs/heads/elsewhere "+zero+"\n")
	if err != nil || !has(d, "+package other // elsewhere", "commit "+other) || has(d, "first", "second") {
		t.Fatalf("new ref: %v %q", err, d)
	}
	// The remote's old commit is one this clone never had: read from the base branch on.
	d, err = RefsDiff(ctx, wt, "main", "refs/heads/fix/1 "+second+" refs/heads/fix/1 "+strings.Repeat("ab", 20)+"\n")
	if err != nil || !has(d, "package one // first", "package two // second") {
		t.Fatalf("unknown remote tip: %v %q", err, d)
	}
	// Several refs at once, a deleted one among them; the same one twice is read once.
	two := "refs/heads/fix/1 " + second + " refs/heads/fix/1 " + remoteTip + "\n"
	d, err = RefsDiff(ctx, wt, "main", two+"(delete) "+zero+" refs/heads/gone "+first+"\n"+"refs/heads/elsewhere "+other+" refs/heads/elsewhere "+zero+"\n"+two)
	if err != nil || !has(d, "package one", "package two", "package other") || strings.Count(d, "commit "+second) != 1 {
		t.Fatalf("several refs: %v %q", err, d)
	}
	// Nothing to push, or only a deletion.
	for _, refs := range []string{"", "\n", "(delete) " + zero + " refs/heads/gone " + first + "\n"} {
		if d, err := RefsDiff(ctx, wt, "main", refs); err != nil || strings.TrimSpace(d) != "" {
			t.Errorf("%q: %v %q", refs, err, d)
		}
	}
	// A line git does not write, or a commit that is not there: an error, never an empty answer.
	for _, refs := range []string{"not a line\n", "refs/heads/x " + strings.Repeat("cd", 20) + " refs/heads/x " + zero + "\n"} {
		if _, err := RefsDiff(ctx, wt, "main", refs); err == nil {
			t.Errorf("%q: no error", refs)
		}
	}
}

// An eval run's worktree: every push is refused, the repository's other hooks still run.
func TestNoPushHook(t *testing.T) {
	wt, _, g := syncRepos(t)
	task := filepath.Join(t.TempDir(), "eval")
	g(wt, "worktree", "add", "-q", "--detach", task)
	ran := filepath.Join(t.TempDir(), "ran")
	own := "#!/bin/sh\necho \"$(basename \"$0\")\" >> " + ran + "\n"
	os.WriteFile(filepath.Join(wt, ".git", "hooks", "pre-commit"), []byte(own), 0o755)
	os.WriteFile(filepath.Join(wt, ".git", "hooks", "pre-push"), []byte(own), 0o755)
	dir := filepath.Join(t.TempDir(), "hooks")
	if err := WriteNoPushHooks(dir); err != nil {
		t.Fatal(err)
	}
	if err := InstallHooks(context.Background(), task, dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(task, "c.txt"), []byte("c\n"), 0o644)
	g(task, "add", ".")
	g(task, "commit", "-q", "-m", "c")
	for _, args := range [][]string{
		{"push", "origin", "HEAD:refs/heads/eval"},
		{"-c", "alias.ship=!git push origin HEAD:refs/heads/eval", "ship"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = task
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "this is an eval run") {
			t.Errorf("%v: not stopped: %v %s", args, err, out)
		}
	}
	if got := g(wt, "ls-remote", "origin", "eval"); got != "" {
		t.Fatalf("the eval branch reached origin: %s", got)
	}
	if b, _ := os.ReadFile(ran); string(b) != "pre-commit\n" {
		t.Fatalf("the repository's hooks that ran: %q", b)
	}
}
