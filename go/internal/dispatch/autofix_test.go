package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/term"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

func TestToFix(t *testing.T) {
	failed := func(ids ...int64) []trackers.Job {
		var js []trackers.Job
		for _, id := range ids {
			js = append(js, trackers.Job{ID: id, Status: "failed"})
		}
		return js
	}
	threads := func(urls ...string) []trackers.Thread {
		var ts []trackers.Thread
		for _, u := range urls {
			ts = append(ts, trackers.Thread{URL: u})
		}
		return ts
	}
	all := &trackers.ReviewCheck{Conflicts: true, CI: "failed", Failed: failed(2, 1), Threads: threads("b", "a")}
	// The conflicts first, then CI, then the comments; each once.
	kind, key, seen := toFix(all, nil)
	if kind != "conflicts" || key != "1" || len(seen) != 0 {
		t.Fatalf("first %q %q %v", kind, key, seen)
	}
	if kind, key, _ = toFix(all, map[string]string{"conflicts": "1"}); kind != "ci" || key != "1,2" {
		t.Fatalf("second %q %q", kind, key)
	}
	if kind, key, _ = toFix(all, map[string]string{"conflicts": "1", "ci": "1,2"}); kind != "comments" || key != "a b" {
		t.Fatalf("third %q %q", kind, key)
	}
	asked := map[string]string{"conflicts": "1", "ci": "1,2", "comments": "a b"}
	if kind, _, _ = toFix(all, asked); kind != "" {
		t.Fatalf("asked again about %q", kind)
	}
	// A new pipeline that failed is a new problem; a running one is none yet.
	if kind, key, _ = toFix(&trackers.ReviewCheck{CI: "failed", Failed: failed(3)}, asked); kind != "ci" || key != "3" {
		t.Fatalf("new pipeline %q %q", kind, key)
	}
	if kind, _, seen = toFix(&trackers.ReviewCheck{CI: "running", Threads: threads("a")}, asked); kind != "" || seen["ci"] != "" || seen["conflicts"] != "" || seen["comments"] != "a b" {
		t.Fatalf("running %q %v", kind, seen)
	}
	// A resolved comment is no request; a new one is.
	if kind, _, _ = toFix(&trackers.ReviewCheck{Threads: threads("b")}, asked); kind != "" {
		t.Fatalf("resolved comment asked about %q", kind)
	}
	if kind, key, _ = toFix(&trackers.ReviewCheck{Threads: threads("b", "c")}, asked); kind != "comments" || key != "b c" {
		t.Fatalf("new comment %q %q", kind, key)
	}
	// Conflicts that come back after a fix are asked about again.
	_, _, seen = toFix(&trackers.ReviewCheck{}, asked)
	if kind, _, _ = toFix(&trackers.ReviewCheck{Conflicts: true}, seen); kind != "conflicts" {
		t.Fatalf("conflicts again %q", kind)
	}
}

func TestAutoFix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
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

	// An MR in review whose agent closed after its turn; its conversation is kept.
	wt := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(wt, "-"))
	os.MkdirAll(projects, 0o755)
	os.WriteFile(filepath.Join(projects, "sess-1.jsonl"), []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Готово"}]}}`+"\n"), 0o644)
	id := "grp/app#5"
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.Executor, r.Worktree, r.View, r.SessionID = core.Done, "terminal", wt, "terminal", "sess-1"
		r.Task = core.Task{ID: id, TrackerID: "gitlab", Project: "grp/app", Title: "Кнопка"}
		r.PRURL, r.Branch = "https://gitlab.example/grp/app/-/merge_requests/1", "fix/5-knopka"
	})
	conflicts := &trackers.ReviewCheck{State: "open", Conflicts: true}
	look := func(c *trackers.ReviewCheck) {
		d.mu.Lock()
		d.reviews[id] = reviewLook{at: time.Now(), check: c}
		d.mu.Unlock()
	}

	// Off in the settings: nothing.
	look(conflicts)
	d.AutoFix(ctx, time.Now())
	if d.Live(id) {
		t.Fatal("asked with autofix off")
	}

	cfg.Terminal.AutoFix = true
	look(conflicts)
	d.AutoFix(ctx, time.Now())
	if !d.Live(id) {
		t.Fatal("agent not resumed")
	}
	defer d.StopTerminal(id)
	var args string
	waitFor(t, func() bool {
		b, _ := os.ReadFile(argsFile)
		args = string(b)
		return strings.Contains(args, "конфліктує")
	}, "the agent was not told of the conflict")
	if !strings.Contains(args, "--resume\nsess-1\nMR https://gitlab.example/grp/app/-/merge_requests/1 конфліктує з main") {
		t.Fatalf("args %q", args)
	}
	r, _ := d.Store.Get(id)
	if r.AutoFixed["conflicts"] != "1" || r.AutoFixes != 1 {
		t.Fatalf("recorded %v %d", r.AutoFixed, r.AutoFixes)
	}
	logged := false
	for _, l := range d.Logs(id) {
		logged = logged || strings.Contains(l.Text, "autofix: the agent was asked to resolve the conflicts")
	}
	if !logged {
		t.Fatalf("logs %+v", d.Logs(id))
	}
}
