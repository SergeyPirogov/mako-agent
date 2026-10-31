package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
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

// reviewSetup: a task on Mako's terminal with its agent's turn over, its worktree on a branch with no commits
// of its own, the reviewer on; idle waits for the agent's turn to end.
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
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Готово"}]},"parent_tool_use_id":null}'
  echo '{"type":"result","subtype":"success","duration_ms":10,"total_cost_usd":0}'
done
`), 0o755)
	c := *d.Config()
	cfg = &c
	cfg.Executor, cfg.Terminal.Command, cfg.StateDir = "terminal", filepath.Join(bin, "claude"), t.TempDir()
	cfg.Reviewer.Enabled, cfg.Reviewer.MaxRounds = true, 1
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
	if err := d.runAgent(id, wt, "chat", "Зроби задачу", false, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.StopTerminal(id); d.Chats.Get(id).Wait() })
	idle = func() {
		t.Helper()
		for i := 0; i < 300 && !d.agentIdle(id); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if !d.agentIdle(id) {
			t.Fatal("the agent did not end its turn")
		}
	}
	idle()
	return d, cfg, id, wt, idle
}

func TestAgentReview(t *testing.T) {
	d, cfg, id, wt, idle := reviewSetup(t)

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

func TestRiskGate(t *testing.T) {
	rated := func(risk int) *core.Assessment {
		return &core.Assessment{At: now(), State: "done", Risk: core.Score{Value: risk}}
	}
	cases := []struct {
		name, when string
		a          *core.Assessment
		skip       string
		wait       bool
	}{
		{"always, low risk", config.ReviewAlways, rated(1), "", false},
		{"medium+, low risk", config.ReviewMedium, rated(2), "низький ризик", false},
		{"medium+, medium risk", config.ReviewMedium, rated(3), "", false},
		{"medium+, high risk", config.ReviewMedium, rated(5), "", false},
		{"high only, low risk", config.ReviewHigh, rated(1), "низький ризик", false},
		{"high only, medium risk", config.ReviewHigh, rated(3), "середній ризик", false},
		{"high only, high risk", config.ReviewHigh, rated(4), "", false},
		{"no assessment", config.ReviewHigh, nil, "", false},
		{"assessment failed", config.ReviewHigh, &core.Assessment{State: "failed"}, "", false},
		{"no risk score", config.ReviewHigh, rated(0), "", false},
		{"assessment running", config.ReviewHigh, &core.Assessment{At: now(), State: "running"}, "", true},
		{"assessment stuck", config.ReviewHigh, &core.Assessment{At: time.Now().Add(-time.Hour).Format(time.RFC3339Nano), State: "running"}, "", false},
	}
	for _, c := range cases {
		if skip, wait := riskGate(c.when, c.a); skip != c.skip || wait != c.wait {
			t.Errorf("%s: skip %q wait %v, want %q %v", c.name, skip, wait, c.skip, c.wait)
		}
	}
}

func TestAgentReviewByRisk(t *testing.T) {
	d, cfg, id, wt, _ := reviewSetup(t)
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	calls := 0
	ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string) (string, error) {
		calls++
		return "", nil
	}
	cfg.Reviewer.When = config.ReviewMedium
	os.WriteFile(filepath.Join(wt, "pay.go"), []byte("package app\n"), 0o644)
	git(t, wt, "add", "pay.go")
	git(t, wt, "commit", "-q", "-m", "Pay")
	git(t, wt, "push", "-q", "origin", "fix/5-pay")

	// Ready, but the agent opens the MR itself: the risk comes with its assessment, so the reviewer waits.
	d.maybeReview(id)
	if r, _ := d.Store.Get(id); calls != 0 || len(r.AgentReviews) != 0 {
		t.Fatalf("calls %d reviews %+v before the MR", calls, r.AgentReviews)
	}
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.PRURL = core.Done, "https://example.com/mr/1"
		r.Assessment = &core.Assessment{At: now(), State: "running"}
	})
	d.maybeReview(id)
	if calls != 0 {
		t.Fatal("reviewed while the assessment runs")
	}

	// Low risk: skipped, once.
	d.Store.Update(id, func(r *core.Record) {
		r.Assessment = &core.Assessment{At: now(), State: "done", Risk: core.Score{Value: 1}}
	})
	d.maybeReview(id)
	d.maybeReview(id)
	r, _ := d.Store.Get(id)
	if calls != 0 || len(r.AgentReviews) != 1 || r.AgentReviews[0].State != "skipped" || r.AgentReviews[0].Reason != "низький ризик" {
		t.Fatalf("calls %d reviews %+v", calls, r.AgentReviews)
	}

	// From the board's button: it runs whatever the risk, in place of the skipped round.
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if r, _ = d.Store.Get(id); r.AgentReviews[0].State == "clean" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls != 1 || len(r.AgentReviews) != 1 || r.AgentReviews[0].State != "clean" {
		t.Fatalf("calls %d reviews %+v after the button", calls, r.AgentReviews)
	}
	if err := d.ReviewNow(id); err == nil {
		t.Fatal("the button ran a review that was not skipped")
	}
}

func TestAgentReviewByRiskRuns(t *testing.T) {
	for _, c := range []struct {
		name, when string
		a          *core.Assessment
	}{
		{"high risk", config.ReviewHigh, &core.Assessment{State: "done", Risk: core.Score{Value: 5}}},
		{"no assessment", config.ReviewHigh, nil},
		{"failed assessment", config.ReviewMedium, &core.Assessment{State: "failed"}},
		{"always, low risk", config.ReviewAlways, &core.Assessment{State: "done", Risk: core.Score{Value: 1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, cfg, id, wt, _ := reviewSetup(t)
			old := ReviewBranch
			t.Cleanup(func() { ReviewBranch = old })
			calls := 0
			ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string) (string, error) {
				calls++
				return "", nil
			}
			cfg.Reviewer.When = c.when
			git(t, wt, "commit", "-q", "--allow-empty", "-m", "Pay")
			git(t, wt, "push", "-q", "origin", "fix/5-pay")
			d.Store.Update(id, func(r *core.Record) { r.Status, r.PRURL, r.Assessment = core.Done, "https://example.com/mr/1", c.a })
			d.maybeReview(id)
			if r, _ := d.Store.Get(id); calls != 1 || len(r.AgentReviews) != 1 || r.AgentReviews[0].State != "clean" {
				t.Fatalf("calls %d reviews %+v", calls, r.AgentReviews)
			}
		})
	}
}
