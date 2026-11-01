package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestRoundsLeft(t *testing.T) {
	notes := core.AgentReview{State: "notes", Commit: "a"}
	cases := []struct {
		list []core.AgentReview
		max  int
		want bool
	}{
		{nil, 1, true},
		{[]core.AgentReview{notes}, 1, false}, // the one round is used up
		{[]core.AgentReview{notes}, 2, true},  // the fixes get a second
		{[]core.AgentReview{notes, {State: "clean", Commit: "b"}}, 3, false},        // passed
		{[]core.AgentReview{{State: "failed"}, {State: "skipped"}}, 1, true},        // neither is a round
		{[]core.AgentReview{notes, notes, notes, notes, notes}, CycleRounds, false}, // a cycle used up
	}
	for i, c := range cases {
		if got := roundsLeft(c.list, c.max); got != c.want {
			t.Errorf("%d: roundsLeft = %v, want %v", i, got, c.want)
		}
	}
}

func TestSeen(t *testing.T) {
	at := func(state string) []core.AgentReview { return []core.AgentReview{{State: state, Commit: "a"}} }
	cases := []struct {
		list        []core.AgentReview
		head        string
		auto, asked bool // seen by the automatic rounds, and by the button
	}{
		{nil, "a", false, false},
		{at("notes"), "a", true, true},     // nothing new since
		{at("notes"), "b", false, false},   // the fixes
		{at("clean"), "a", true, true},     //
		{at("failed"), "a", true, false},   // not retried by itself, but by the button
		{at("skipped"), "a", true, false},  // the button reads it whatever the risk
		{at("running"), "a", false, false}, // left by a restart
	}
	for i, c := range cases {
		if got := seen(c.list, c.head, false); got != c.auto {
			t.Errorf("%d: seen = %v, want %v", i, got, c.auto)
		}
		if got := seen(c.list, c.head, true); got != c.asked {
			t.Errorf("%d: seen asked = %v, want %v", i, got, c.asked)
		}
	}
	// The next round over the commit a failed one read takes its place; over a new commit it is a new one.
	if !replaces(at("failed"), "a") || replaces(at("failed"), "b") || replaces(at("notes"), "a") || !replaces(at("running"), "b") {
		t.Error("replaces")
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
  case "$msg" in *повільно*) sleep 1;; esac
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

	// Nothing committed yet: not ready.
	reviewNow(t, d, id)
	if calls.Load() != 0 {
		t.Fatal("reviewed a branch with no commits")
	}
	os.WriteFile(filepath.Join(wt, "pay.go"), []byte("package app\n"), 0o644)
	git(t, wt, "add", "pay.go")
	git(t, wt, "commit", "-q", "-m", "Pay")
	// Committed, not pushed: the agent opens the MR itself, so not ready yet.
	reviewNow(t, d, id)
	if calls.Load() != 0 {
		t.Fatal("reviewed a branch not pushed")
	}
	git(t, wt, "push", "-q", "origin", "fix/5-pay")
	reviewNow(t, d, id)
	if calls.Load() != 1 {
		t.Fatalf("reviewer called %d times", calls.Load())
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
	reviewNow(t, d, id)
	if calls.Load() != 1 {
		t.Fatal("a second round over the maximum")
	}
	// With two rounds the fixes are reviewed again, and pass.
	setConfig(d, func(c *config.Config) { c.Reviewer.MaxRounds = 2 })
	reviewNow(t, d, id)
	r, _ = d.Store.Get(id)
	if calls.Load() != 2 || len(r.AgentReviews) != 2 || r.AgentReviews[1].State != "clean" {
		t.Fatalf("calls %d reviews %+v", calls.Load(), r.AgentReviews)
	}
	reviewNow(t, d, id)
	if calls.Load() != 2 {
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
	d, _, id, wt, _ := reviewSetup(t, func(c *config.Config) { c.Reviewer.When = config.ReviewMedium })
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	var calls atomic.Int32
	ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string) (string, error) {
		calls.Add(1)
		return "", nil
	}
	os.WriteFile(filepath.Join(wt, "pay.go"), []byte("package app\n"), 0o644)
	git(t, wt, "add", "pay.go")
	git(t, wt, "commit", "-q", "-m", "Pay")
	git(t, wt, "push", "-q", "origin", "fix/5-pay")

	// Ready, but the agent opens the MR itself: the risk comes with its assessment, so the reviewer waits.
	reviewNow(t, d, id)
	if r, _ := d.Store.Get(id); calls.Load() != 0 || len(r.AgentReviews) != 0 {
		t.Fatalf("calls %d reviews %+v before the MR", calls.Load(), r.AgentReviews)
	}
	// The MR is found but CI still runs (the task is handed off, no assessment yet): the reviewer waits as well.
	d.Store.Update(id, func(r *core.Record) { r.PRURL = "https://example.com/mr/1" })
	reviewNow(t, d, id)
	if r, _ := d.Store.Get(id); calls.Load() != 0 || len(r.AgentReviews) != 0 {
		t.Fatalf("calls %d reviews %+v while CI runs", calls.Load(), r.AgentReviews)
	}
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.PRURL = core.Done, "https://example.com/mr/1"
		r.Assessment = &core.Assessment{At: now(), State: "running"}
	})
	reviewNow(t, d, id)
	if calls.Load() != 0 {
		t.Fatal("reviewed while the assessment runs")
	}

	// Low risk: skipped, once.
	d.Store.Update(id, func(r *core.Record) {
		r.Assessment = &core.Assessment{At: now(), State: "done", Risk: core.Score{Value: 1}}
	})
	reviewNow(t, d, id)
	reviewNow(t, d, id)
	r, _ := d.Store.Get(id)
	if calls.Load() != 0 || len(r.AgentReviews) != 1 || r.AgentReviews[0].State != "skipped" || r.AgentReviews[0].Reason != "низький ризик" {
		t.Fatalf("calls %d reviews %+v", calls.Load(), r.AgentReviews)
	}

	// From the board's button: it runs whatever the risk, in place of the skipped round.
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { r, _ = d.Store.Get(id); return r.AgentReviews[0].State == "clean" }, "the review from the button did not end")
	if calls.Load() != 1 || len(r.AgentReviews) != 1 || r.AgentReviews[0].State != "clean" {
		t.Fatalf("calls %d reviews %+v after the button", calls.Load(), r.AgentReviews)
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
			d, _, id, wt, _ := reviewSetup(t, func(cfg *config.Config) { cfg.Reviewer.When = c.when })
			old := ReviewBranch
			t.Cleanup(func() { ReviewBranch = old })
			var calls atomic.Int32
			ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string) (string, error) {
				calls.Add(1)
				return "", nil
			}
			git(t, wt, "commit", "-q", "--allow-empty", "-m", "Pay")
			git(t, wt, "push", "-q", "origin", "fix/5-pay")
			d.Store.Update(id, func(r *core.Record) { r.Status, r.PRURL, r.Assessment = core.Done, "https://example.com/mr/1", c.a })
			reviewNow(t, d, id)
			if r, _ := d.Store.Get(id); calls.Load() != 1 || len(r.AgentReviews) != 1 || r.AgentReviews[0].State != "clean" {
				t.Fatalf("calls %d reviews %+v", calls.Load(), r.AgentReviews)
			}
		})
	}
}

// The board's «Зробити рев'ю» with the reviewer off in the settings: a cycle that reads every push of the
// agent's fixes, up to CycleRounds rounds with remarks.
func TestReviewCycle(t *testing.T) {
	d, _, id, wt, idle := reviewSetup(t, func(c *config.Config) { c.Reviewer.Enabled = false })
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	var calls atomic.Int32
	var clean atomic.Bool
	ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string) (string, error) {
		calls.Add(1)
		if clean.Load() {
			return "", nil
		}
		return "- ще не так", nil
	}
	rounds := func(n int) core.Record {
		t.Helper()
		var r core.Record
		waitFor(t, func() bool {
			d.mu.Lock()
			busy := d.reviewing[id]
			d.mu.Unlock()
			r, _ = d.Store.Get(id)
			return !busy && len(r.AgentReviews) == n && r.AgentReviews[n-1].State != "running"
		}, "the round did not end")
		return r
	}
	push := func(msg string) {
		git(t, wt, "commit", "-q", "--allow-empty", "-m", msg)
		git(t, wt, "push", "-q", "origin", "fix/5-pay")
	}

	if r, _ := d.Store.Get(id); d.reviewWanted(r) || d.ReviewStep(r) != "idle" { // no commits: no button
		t.Fatalf("off in the settings: wanted %v, step %q", d.reviewWanted(r), d.ReviewStep(r))
	}
	if err := d.ReviewNow(id); err == nil || !strings.Contains(err.Error(), "не готова") {
		t.Fatalf("a branch with no commits: %v", err)
	}
	push("Pay")
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	r := rounds(1)
	if r.ReviewCycle == nil || r.ReviewCycle.Rounds != CycleRounds || r.AgentReviews[0].State != "notes" {
		t.Fatalf("cycle %+v reviews %+v", r.ReviewCycle, r.AgentReviews)
	}
	idle()
	if err := d.ReviewNow(id); err == nil || !strings.Contains(err.Error(), "не змінив") {
		t.Fatalf("the same commit again: %v", err)
	}
	// Each push of the fixes is read again by itself, as watchTerminals does, until the rounds are used up.
	for n := 2; n <= CycleRounds; n++ {
		push("Fix " + strconv.Itoa(n))
		if r, _ := d.Store.Get(id); !d.reviewWanted(r) {
			t.Fatalf("round %d not wanted", n)
		}
		reviewNow(t, d, id)
		rounds(n)
		idle()
	}
	push("Fix again")
	reviewNow(t, d, id)
	if r, _ := d.Store.Get(id); d.reviewWanted(r) || calls.Load() != CycleRounds {
		t.Fatalf("over the cycle: wanted %v, calls %d", d.reviewWanted(r), calls.Load())
	}

	// Another click starts a new cycle, which a clean review ends: pressed while the quick look whether a round
	// is due (after each turn of the agent) holds the flag, it waits for it rather than fail or be lost.
	clean.Store(true)
	d.mu.Lock()
	d.reviewing[id] = true
	d.mu.Unlock()
	if err := d.ReviewNow(id); err != nil {
		t.Fatalf("pressed during the quick look: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	d.mu.Lock()
	delete(d.reviewing, id)
	d.mu.Unlock()
	r = rounds(CycleRounds + 1)
	if r.AgentReviews[CycleRounds].State != "clean" || r.ReviewCycle.From != CycleRounds {
		t.Fatalf("cycle %+v reviews %+v", r.ReviewCycle, r.AgentReviews)
	}
	push("More")
	if r, _ := d.Store.Get(id); d.reviewWanted(r) {
		t.Fatal("reviewed after it passed")
	}
}

// The button after a failed round and after one a restart of Mako cut short, and the steps the board shows.
func TestReviewRetryAndSteps(t *testing.T) {
	d, _, id, wt, idle := reviewSetup(t, func(c *config.Config) { c.Reviewer.Enabled = false })
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	// What the reviewer says, set between its rounds: the agent's turns start rounds in the background too.
	var mu sync.Mutex
	var fail error
	answer := "- повільно: немає тесту"
	says := func(notes string, err error) { mu.Lock(); answer, fail = notes, err; mu.Unlock() }
	ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return answer, fail
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
	wait := func(state string) core.Record {
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

	step("idle") // no commits yet: no button
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "Pay")
	step("idle") // not pushed
	git(t, wt, "push", "-q", "origin", "fix/5-pay")
	step("ready")

	// The reviewer fails: the automatic rounds leave the commit, the button tries it again in its place.
	says(answer, errors.New("ліміт"))
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	wait("failed")
	step("ready")
	says(answer, nil)
	if err := d.ReviewNow(id); err != nil {
		t.Fatalf("again after a failure: %v", err)
	}
	// The remarks keep the agent busy for a while: it fixes them.
	r := wait("notes")
	if len(r.AgentReviews) != 1 || r.ReviewCycle.From != 0 {
		t.Fatalf("the retry did not take the failed round's place: cycle %+v reviews %+v", r.ReviewCycle, r.AgentReviews)
	}
	step("fixing")
	idle()
	step("idle") // it answered, nothing new pushed
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "Fix")
	git(t, wt, "push", "-q", "origin", "fix/5-pay")
	step("due") // the cycle reads it by itself

	// A round cut short by a restart: the board offers it again, and the button runs it.
	d.Store.Update(id, func(r *core.Record) {
		r.AgentReviews = append(r.AgentReviews, core.AgentReview{At: now(), Commit: "old", State: "running"})
		r.ReviewCycle = nil
	})
	step("ready")
	says("", nil)
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	if r := wait("clean"); len(r.AgentReviews) != 2 {
		t.Fatalf("reviews %+v", r.AgentReviews)
	}
	step("idle")
	// Only a round in progress is the reviewer at work, not the quick look whether one is due.
	d.mu.Lock()
	d.reviewing[id] = true
	d.mu.Unlock()
	step("idle")
	d.Store.Update(id, func(r *core.Record) {
		r.AgentReviews = append(r.AgentReviews, core.AgentReview{At: now(), Commit: "new", State: "running"})
	})
	step("running")
	d.mu.Lock()
	delete(d.reviewing, id) // for the cleanup's settle
	d.mu.Unlock()
}
