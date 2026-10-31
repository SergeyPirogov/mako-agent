package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

func TestAutoRebase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
	d, _, _ := setup(t)
	ctx := context.Background()
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\nexec sleep 30\n"), 0o755)
	cfg := *d.Config()
	cfg.Executor, cfg.Terminal.Command, cfg.StateDir = "terminal", filepath.Join(bin, "claude"), t.TempDir()
	d.Cfg = &cfg
	d.Sessions = term.NewManager(t.TempDir())
	d.BoardURL = "http://127.0.0.1:1"

	// A remote with main, the task's worktree with its branch pushed, and a clone where main moves on.
	dir := t.TempDir()
	remote, other, wt := filepath.Join(dir, "origin.git"), filepath.Join(dir, "other"), filepath.Join(dir, "wt")
	git(t, dir, "init", "-q", "--bare", "-b", "main", remote)
	git(t, dir, "clone", "-q", remote, other)
	put := func(repo, file, body string) {
		os.WriteFile(filepath.Join(repo, file), []byte(body), 0o644)
		git(t, repo, "add", ".")
		git(t, repo, "commit", "-q", "-m", "Change "+file)
		git(t, repo, "push", "-q", "origin", "HEAD")
	}
	put(other, "a.txt", "one\n")
	git(t, dir, "clone", "-q", remote, wt)
	git(t, wt, "checkout", "-q", "-b", "fix/5-knopka")
	put(wt, "b.txt", "mine\n")
	behind := func() string {
		git(t, wt, "fetch", "-q", "origin")
		out, _ := exec.Command("git", "-C", wt, "rev-list", "--count", "origin/fix/5-knopka..origin/main").Output()
		return strings.TrimSpace(string(out))
	}

	projects := filepath.Join(home, ".claude", "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(wt, "-"))
	os.MkdirAll(projects, 0o755)
	os.WriteFile(filepath.Join(projects, "sess-1.jsonl"), []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Готово"}]}}`+"\n"), 0o644)
	id := "grp/app#5"
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.Executor, r.Worktree, r.View, r.SessionID = core.Done, "terminal", wt, "terminal", "sess-1"
		r.Task = core.Task{ID: id, TrackerID: "gitlab", Project: "grp/app", Title: "Кнопка"}
		r.PRURL, r.Branch = "https://gitlab.example/grp/app/-/merge_requests/1", "fix/5-knopka"
	})
	round := func() {
		d.mu.Lock()
		d.reviews[id] = reviewLook{at: time.Now(), check: &trackers.ReviewCheck{State: "open"}}
		delete(d.rebased, id) // the N minutes have passed
		d.mu.Unlock()
		d.AutoRebase(ctx, time.Now())
	}
	logged := func(s string) bool {
		for _, l := range d.Logs(id) {
			if strings.Contains(l.Text, s) {
				return true
			}
		}
		return false
	}

	// Off in the settings: nothing.
	put(other, "c.txt", "theirs\n")
	round()
	if behind() != "1" {
		t.Fatal("updated with auto-rebase off")
	}

	// Behind without conflicts: rebased and pushed by Mako.
	cfg.Terminal.AutoRebase = "rebase"
	round()
	if behind() != "0" || !logged("оновлено з main (rebase, 1 коміт)") {
		t.Fatalf("clean rebase: behind %s, logs %+v", behind(), d.Logs(id))
	}
	if d.Live(id) {
		t.Fatal("agent started for a clean rebase")
	}
	// Not more often than every N minutes.
	put(other, "d.txt", "theirs\n")
	d.mu.Lock()
	d.reviews[id] = reviewLook{at: time.Now(), check: &trackers.ReviewCheck{State: "open"}}
	d.mu.Unlock()
	d.AutoRebase(ctx, time.Now())
	if behind() != "1" {
		t.Fatal("looked again too soon")
	}

	// The agent at work: its branch is left alone.
	put(other, "b.txt", "theirs\n")
	if err := d.resume(id, "працюй"); err != nil {
		t.Fatal(err)
	}
	round()
	if behind() != "2" || logged("конфлікт") {
		t.Fatalf("touched while the agent works: behind %s", behind())
	}
	d.StopTerminal(id)
	for i := 0; i < 200 && d.Live(id); i++ {
		time.Sleep(20 * time.Millisecond)
	}

	// Merged: left alone.
	d.mu.Lock()
	d.reviews[id] = reviewLook{at: time.Now(), check: &trackers.ReviewCheck{State: "merged"}}
	delete(d.rebased, id)
	d.mu.Unlock()
	d.AutoRebase(ctx, time.Now())
	if logged("конфлікт") {
		t.Fatal("touched a merged MR")
	}

	// A conflict: undone, and the agent is asked to resolve it.
	os.Remove(argsFile)
	round()
	if !d.Live(id) {
		t.Fatalf("agent not asked: %+v", d.Logs(id))
	}
	defer d.StopTerminal(id)
	var args string
	for i := 0; i < 200 && !strings.Contains(args, "конфліктує"); i++ {
		b, _ := os.ReadFile(argsFile)
		args = string(b)
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(args, "--resume\nsess-1\n") || !strings.Contains(args, "конфліктує з нею у файлах: b.txt") || !strings.Contains(args, "--force-with-lease") {
		t.Fatalf("args %q", args)
	}
	r, _ := d.Store.Get(id)
	if r.BaseConflict == nil || len(r.BaseConflict.Files) != 1 || r.AutoFixed["conflicts"] != "1" || r.AutoFixes != 1 {
		t.Fatalf("recorded %+v %v %d", r.BaseConflict, r.AutoFixed, r.AutoFixes)
	}
	if !logged("конфлікт з main у файлах b.txt: агента попрошено розвʼязати") {
		t.Fatalf("logs %+v", d.Logs(id))
	}
	if out, _ := exec.Command("git", "-C", wt, "status", "--porcelain").Output(); len(out) != 0 {
		t.Fatalf("rebase left half-way: %s", out)
	}

	// The agent resolves it and its turn ends: the journal says so.
	git(t, wt, "rebase", "-X", "theirs", "origin/main")
	git(t, wt, "push", "-q", "--force", "origin", "HEAD")
	d.StopTerminal(id)
	for i := 0; i < 200 && d.Live(id); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	round()
	if !logged("конфлікт розвʼязано: файли b.txt") {
		t.Fatalf("logs %+v", d.Logs(id))
	}
	if r, _ := d.Store.Get(id); r.BaseConflict != nil {
		t.Fatal("conflict still recorded")
	}
}
