package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// AutoFixReview: the reviewer agent's remarks go to the agent by themselves, once, only with Terminal.AutoFix, only
// while the agent is free, not past the rounds a person decides on, and not the suggestions of a clean round.
func TestAutoFixReview(t *testing.T) {
	d, id, wt, idle, _ := numberedReview(t, "", func([]core.Remark) string { return "" })
	// received: how many times the agent got the reviewer's remark.
	received := func() int {
		b, _ := os.ReadFile(filepath.Join(wt, "said.json"))
		return strings.Count(string(b), "порожній кошик")
	}
	tick := func() { d.AutoFixReview(time.Now()) }
	setRound := func(f func(*core.AgentReview)) {
		d.Store.Update(id, func(r *core.Record) { r.AgentReviews = withLast(r.AgentReviews, f) })
	}
	untouched := func(what string) {
		t.Helper()
		idle()
		if r, _ := d.Store.Get(id); lastReview(r).Sent || r.AutoFixes != 0 || received() != 0 {
			t.Fatalf("%s: sent %v, fixes %d, the agent got %d", what, lastReview(r).Sent, r.AutoFixes, received())
		}
	}

	tick()
	untouched("AutoFix off")
	d.Cfg.Terminal.AutoFix = true

	d.Store.Update(id, func(r *core.Record) { r.AutoFixes = autoFixLimit })
	tick()
	d.Store.Update(id, func(r *core.Record) { r.AutoFixes = 0 })
	untouched("past the limit")

	setRound(func(a *core.AgentReview) { a.Escalated = true })
	tick()
	setRound(func(a *core.AgentReview) { a.Escalated = false })
	untouched("a round left to a person")

	setRound(func(a *core.AgentReview) { a.State = "clean" }) // its remarks are suggestions now
	tick()
	setRound(func(a *core.AgentReview) { a.State = "notes" })
	untouched("the suggestions of a clean round")

	_ = d.Chats.Get(id).Send("повільно")
	tick()
	untouched("the agent works")

	// Free: sent once.
	tick()
	r, _ := d.Store.Get(id)
	if !lastReview(r).Sent || r.AutoFixes != 1 {
		t.Fatalf("not sent: %+v fixes %d", lastReview(r), r.AutoFixes)
	}
	idle()
	if received() != 1 {
		t.Fatalf("the agent got the remarks %d times", received())
	}
	tick()
	idle()
	if r, _ = d.Store.Get(id); r.AutoFixes != 1 || received() != 1 {
		t.Fatalf("sent twice: fixes %d, the agent got %d", r.AutoFixes, received())
	}
	if !slices.ContainsFunc(d.Logs(id), func(l LogLine) bool { return l.Text == "autofix: reviewer remarks sent to the agent" }) {
		t.Fatalf("journal %+v", d.Logs(id))
	}
}

// A failed send is said once a round, is not counted, and is tried again; after it went through, a later failure is
// said again.
func TestAutoFixReviewFailure(t *testing.T) {
	d, id, wt, idle, _ := numberedReview(t, "", func([]core.Remark) string { return "" })
	d.Cfg.Terminal.AutoFix = true
	failures := func() int {
		n := 0
		for _, l := range d.Logs(id) {
			if strings.HasPrefix(l.Text, "reviewer remarks not sent by itself") {
				n++
			}
		}
		return n
	}
	gone := wt + ".gone"
	if err := os.Rename(wt, gone); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		d.AutoFixReview(time.Now())
	}
	r, _ := d.Store.Get(id)
	if failures() != 1 || r.AutoFixes != 0 || lastReview(r).Sent {
		t.Fatalf("failures %d, fixes %d, round %+v", failures(), r.AutoFixes, lastReview(r))
	}
	if err := os.Rename(gone, wt); err != nil {
		t.Fatal(err)
	}
	d.AutoFixReview(time.Now())
	idle()
	if r, _ = d.Store.Get(id); !lastReview(r).Sent || r.AutoFixes != 1 {
		t.Fatalf("not sent after the worktree came back: %+v fixes %d", lastReview(r), r.AutoFixes)
	}
	d.mu.Lock()
	left := len(d.autoFixReviewFailed)
	d.mu.Unlock()
	if left != 0 {
		t.Fatalf("the failure is still remembered: %d", left)
	}
}
