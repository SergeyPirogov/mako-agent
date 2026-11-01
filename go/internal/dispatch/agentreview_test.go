package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestSeen(t *testing.T) {
	at := func(state string) []core.AgentReview { return []core.AgentReview{{State: state, Commit: "a"}} }
	cases := []struct {
		list []core.AgentReview
		head string
		want bool
	}{
		{nil, "a", false},
		{at("notes"), "a", true},    // nothing new since
		{at("notes"), "b", false},   // the fixes
		{at("clean"), "a", true},    //
		{at("failed"), "a", false},  // the button tries it again
		{at("skipped"), "a", false}, // left by older versions of Mako
		{at("running"), "a", false}, // left by a restart
	}
	for i, c := range cases {
		if got := seen(c.list, c.head); got != c.want {
			t.Errorf("%d: seen = %v, want %v", i, got, c.want)
		}
	}
	// The next round over the commit a failed one read takes its place; over a new commit it is a new one.
	if !replaces(at("failed"), "a") || replaces(at("failed"), "b") || replaces(at("notes"), "a") || !replaces(at("running"), "b") {
		t.Error("replaces")
	}
}

// reviewSetup: Mako's agent on grp/app#5 in a worktree; prep changes the settings before it starts.
func reviewSetup(t *testing.T, prep ...func(*config.Config)) (d *Dispatcher, cfg *config.Config, id, wt string, idle func()) {
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	t.Setenv("HOME", t.TempDir())
	d, _, _ = setup(t)
	var repo string
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		repo = r.Path
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, filepath.Dir(origin), "init", "-q", "--bare", "-b", "main", origin)
	git(t, repo, "remote", "set-url", "origin", origin)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, repo, "push", "-q", "origin", "main")
	git(t, repo, "fetch", "-q", "origin")
	wt = filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", "fix/5-pay", wt, "main")

	// A stand-in for the agent's claude: one turn for the task, one for each message after it.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"), []byte(`#!/bin/sh
read init
while read msg; do
  echo "$msg" >> said.json
  case "$msg" in *повільно*) sleep 1;; esac
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Готово"}]},"parent_tool_use_id":null}'
  echo '{"type":"result","subtype":"success","duration_ms":10,"total_cost_usd":0}'
done
`), 0o755)
	c := *d.Config()
	cfg = &c
	cfg.Executor, cfg.Terminal.Command, cfg.StateDir = "terminal", filepath.Join(bin, "claude"), t.TempDir()
	for _, f := range prep {
		f(cfg)
	}
	d.Cfg = cfg
	d.BoardURL = "http://127.0.0.1:1"
	d.Poll(context.Background())
	id = "grp/app#5"
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.Worktree, r.Branch, r.Executor, r.View = core.HandedOff, wt, "fix/5-pay", terminalExecutor, "chat"
	})
	if err := d.runAgent(id, wt, "chat", "Зроби задачу", false, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.StopTerminal(id); d.Chats.Get(id).Wait(); settle(t, d, id) })
	idle = func() {
		t.Helper()
		waitFor(t, func() bool { return d.agentIdle(id) }, "the agent did not end its turn")
	}
	idle()
	return d, cfg, id, wt, idle
}

// The reviewer starts only from the board's «Рев'ю»; its remarks stay in the task until «Виправити» hands them
// to the agent, once; the agent's fixes get a round only from the button again.
func TestAgentReview(t *testing.T) {
	d, _, id, wt, idle := reviewSetup(t)
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	var calls atomic.Int32
	answer := "- `pay.go`: немає тесту на порожній кошик"
	ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string) (string, error) {
		if calls.Add(1) > 1 {
			return "", nil // the fixes pass
		}
		return answer, nil
	}
	step := func(want string) {
		t.Helper()
		d.mu.Lock()
		delete(d.branches, id) // as after 15 seconds
		d.mu.Unlock()
		if r, _ := d.Store.Get(id); d.ReviewStep(r) != want {
			t.Fatalf("step %q, want %q (reviews %+v)", d.ReviewStep(r), want, r.AgentReviews)
		}
	}
	said := func() string { b, _ := os.ReadFile(filepath.Join(wt, "said.json")); return string(b) }

	step("idle") // no commits: no button
	if err := d.ReviewNow(id); err == nil || !strings.Contains(err.Error(), "не готова") {
		t.Fatalf("a branch with no commits: %v", err)
	}
	os.WriteFile(filepath.Join(wt, "pay.go"), []byte("package app\n"), 0o644)
	git(t, wt, "add", "pay.go")
	git(t, wt, "commit", "-q", "-m", "Pay")
	step("idle") // not pushed: the agent opens the MR itself
	git(t, wt, "push", "-q", "origin", "fix/5-pay")

	// Ready, and neither the agent's turns nor the MR start a round by themselves.
	d.watchTerminals(time.Now())
	d.Store.Update(id, func(r *core.Record) { r.Status, r.PRURL = core.Done, "https://example.com/mr/1" })
	d.watchTerminals(time.Now())
	settle(t, d, id)
	if r, _ := d.Store.Get(id); calls.Load() != 0 || len(r.AgentReviews) != 0 {
		t.Fatalf("reviewed by itself: calls %d reviews %+v", calls.Load(), r.AgentReviews)
	}
	step("ready")

	// The button: the remarks stay in the task, the agent does not get them.
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	r := reviewEnd(t, d, id, "notes")
	if calls.Load() != 1 || len(r.AgentReviews) != 1 || r.AgentReviews[0].Notes != answer || r.AgentReviews[0].Sent {
		t.Fatalf("calls %d reviews %+v", calls.Load(), r.AgentReviews)
	}
	if strings.Contains(said(), "порожній кошик") {
		t.Fatal("the remarks went to the agent by themselves")
	}
	step("notes")
	if err := d.ReviewNow(id); err == nil || !strings.Contains(err.Error(), "не змінив") {
		t.Fatalf("the same commit again: %v", err)
	}

	// «Виправити»: the agent gets them, once.
	if err := d.FixReview(id); err != nil {
		t.Fatal(err)
	}
	if err := d.FixReview(id); err == nil || !strings.Contains(err.Error(), "вже передано") && !strings.Contains(err.Error(), "ще працює") {
		t.Fatalf("sent twice: %v", err)
	}
	if r, _ := d.Store.Get(id); !r.AgentReviews[0].Sent {
		t.Fatalf("reviews %+v", r.AgentReviews)
	}
	idle()
	if err := d.FixReview(id); err == nil || !strings.Contains(err.Error(), "вже передано") {
		t.Fatalf("sent twice: %v", err)
	}
	if b := said(); !strings.Contains(b, "порожній кошик") || !strings.Contains(b, "запуш гілку fix/5-pay") {
		t.Fatalf("the agent got %s", b)
	}
	step("idle") // it answered, nothing new pushed

	// The fixes: no round by itself; the button reads them and they pass.
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "Test the empty cart")
	git(t, wt, "push", "-q", "origin", "fix/5-pay")
	d.watchTerminals(time.Now())
	settle(t, d, id)
	if calls.Load() != 1 {
		t.Fatal("the fixes reviewed by themselves")
	}
	step("ready")
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	if r := reviewEnd(t, d, id, "clean"); calls.Load() != 2 || len(r.AgentReviews) != 2 {
		t.Fatalf("calls %d reviews %+v", calls.Load(), r.AgentReviews)
	}
	step("idle")
	if err := d.FixReview(id); err == nil || !strings.Contains(err.Error(), "немає зауважень") {
		t.Fatalf("fix of a clean review: %v", err)
	}
}

// The button after a failed round and after one a restart of Mako cut short, and while one runs.
func TestReviewRetry(t *testing.T) {
	d, _, id, wt, _ := reviewSetup(t)
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	var mu sync.Mutex
	var fail error
	says := func(err error) { mu.Lock(); fail = err; mu.Unlock() }
	ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return "", fail
	}
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "Pay")
	git(t, wt, "push", "-q", "origin", "fix/5-pay")

	says(errors.New("ліміт"))
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	reviewEnd(t, d, id, "failed")
	says(nil)
	if err := d.ReviewNow(id); err != nil {
		t.Fatalf("again after a failure: %v", err)
	}
	if r := reviewEnd(t, d, id, "clean"); len(r.AgentReviews) != 1 {
		t.Fatalf("the retry did not take the failed round's place: %+v", r.AgentReviews)
	}

	// A round cut short by a restart: the board offers it again.
	d.Store.Update(id, func(r *core.Record) {
		r.AgentReviews = []core.AgentReview{{At: now(), Commit: "old", State: "running"}}
	})
	if r, _ := d.Store.Get(id); d.ReviewStep(r) != "ready" {
		t.Fatalf("step %q", d.ReviewStep(r))
	}
	// While a round runs the button refuses another.
	d.mu.Lock()
	d.reviewing[id] = true
	d.mu.Unlock()
	if r, _ := d.Store.Get(id); d.ReviewStep(r) != "running" {
		t.Fatalf("step %q", d.ReviewStep(r))
	}
	if err := d.ReviewNow(id); err == nil || !strings.Contains(err.Error(), "уже переглядає") {
		t.Fatalf("a second round: %v", err)
	}
	d.mu.Lock()
	delete(d.reviewing, id) // for the cleanup's settle
	d.mu.Unlock()
}

// reviewEnd waits for the reviewer's round to end as state.
func reviewEnd(t *testing.T, d *Dispatcher, id, state string) core.Record {
	t.Helper()
	var r core.Record
	waitFor(t, func() bool {
		d.mu.Lock()
		busy := d.reviewing[id]
		d.mu.Unlock()
		r, _ = d.Store.Get(id)
		return !busy && len(r.AgentReviews) > 0 && r.AgentReviews[len(r.AgentReviews)-1].State == state
	}, "the round did not end as "+state)
	return r
}
