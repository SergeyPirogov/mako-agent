package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestReviewDue(t *testing.T) {
	notes := core.AgentReview{State: "notes", Commit: "a"}
	cases := []struct {
		list []core.AgentReview
		head string
		max  int
		want bool
	}{
		{nil, "a", 1, true},
		{[]core.AgentReview{notes}, "b", 1, false},                          // the one round is used up
		{[]core.AgentReview{notes}, "b", 2, true},                           // the fixes get a second
		{[]core.AgentReview{notes}, "a", 2, false},                          // nothing new since
		{[]core.AgentReview{{State: "clean", Commit: "a"}}, "b", 3, false},  // passed
		{[]core.AgentReview{{State: "failed", Commit: "a"}}, "a", 1, false}, // not again on the same commit
		{[]core.AgentReview{{State: "failed", Commit: "a"}}, "b", 1, true},  // a failure is no round
		{[]core.AgentReview{{State: "running", Commit: "a"}}, "a", 1, true}, // left by a restart
	}
	for i, c := range cases {
		if got := reviewDue(c.list, c.head, c.max); got != c.want {
			t.Errorf("%d: reviewDue = %v, want %v", i, got, c.want)
		}
	}
}

func TestAgentReview(t *testing.T) {
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	t.Setenv("HOME", t.TempDir())
	d, _, _ := setup(t)
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
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", "fix/5-pay", wt, "main")

	// A stand-in for the agent's claude: one turn for the task, one for each message after it.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"), []byte(`#!/bin/sh
read init
while read msg; do
  echo "$msg" >> said.json
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Готово"}]},"parent_tool_use_id":null}'
  echo '{"type":"result","subtype":"success","duration_ms":10,"total_cost_usd":0}'
done
`), 0o755)
	cfg := *d.Config()
	cfg.Executor, cfg.Terminal.Command, cfg.StateDir = "terminal", filepath.Join(bin, "claude"), t.TempDir()
	cfg.Reviewer.Enabled, cfg.Reviewer.MaxRounds = true, 1
	d.Cfg = &cfg
	d.BoardURL = "http://127.0.0.1:1"
	d.Poll(context.Background())
	const id = "grp/app#5"
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.Worktree, r.Branch, r.Executor, r.View = core.HandedOff, wt, "fix/5-pay", terminalExecutor, "chat"
	})
	if err := d.runAgent(id, wt, "chat", "Зроби задачу", false, ""); err != nil {
		t.Fatal(err)
	}
	defer func() { d.StopTerminal(id); d.Chats.Get(id).Wait() }()
	idle := func() {
		t.Helper()
		for i := 0; i < 300 && !d.agentIdle(id); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if !d.agentIdle(id) {
			t.Fatal("the agent did not end its turn")
		}
	}
	idle()

	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	calls := 0
	answer := "- `pay.go`: немає тесту на порожній кошик"
	ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string) (string, error) {
		calls++
		return answer, nil
	}

	// Nothing committed yet: not ready.
	d.maybeReview(id)
	if calls != 0 {
		t.Fatal("reviewed a branch with no commits")
	}
	os.WriteFile(filepath.Join(wt, "pay.go"), []byte("package app\n"), 0o644)
	git(t, wt, "add", "pay.go")
	git(t, wt, "commit", "-q", "-m", "Pay")
	// Committed, not pushed: the agent opens the MR itself, so not ready yet.
	d.maybeReview(id)
	if calls != 0 {
		t.Fatal("reviewed a branch not pushed")
	}
	git(t, wt, "push", "-q", "origin", "fix/5-pay")
	d.maybeReview(id)
	if calls != 1 {
		t.Fatalf("reviewer called %d times", calls)
	}
	r, _ := d.Store.Get(id)
	if len(r.AgentReviews) != 1 || r.AgentReviews[0].State != "notes" || r.AgentReviews[0].Notes != answer {
		t.Fatalf("reviews %+v", r.AgentReviews)
	}
	idle()
	if b, _ := os.ReadFile(filepath.Join(wt, "said.json")); !strings.Contains(string(b), "порожній кошик") || !strings.Contains(string(b), "запуш гілку fix/5-pay") {
		t.Fatalf("the agent got %s", b)
	}

	// The one round is used up, even after the fixes.
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "Test the empty cart")
	git(t, wt, "push", "-q", "origin", "fix/5-pay")
	d.maybeReview(id)
	if calls != 1 {
		t.Fatal("a second round over the maximum")
	}
	// With two rounds the fixes are reviewed again, and pass.
	cfg.Reviewer.MaxRounds = 2
	answer = ""
	d.maybeReview(id)
	r, _ = d.Store.Get(id)
	if calls != 2 || len(r.AgentReviews) != 2 || r.AgentReviews[1].State != "clean" {
		t.Fatalf("calls %d reviews %+v", calls, r.AgentReviews)
	}
	d.maybeReview(id)
	if calls != 2 {
		t.Fatal("reviewed after it passed")
	}
}
