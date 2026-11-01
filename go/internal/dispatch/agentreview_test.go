package dispatch

import (
	"context"
	"encoding/json"
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
func reviewSetup(t *testing.T, prep ...func(*config.Config)) (d *Dispatcher, cfg *config.Config, id, wt string, idle func(), f *fakeGitLab) {
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	t.Setenv("HOME", t.TempDir())
	d, f, _ = setup(t)
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

	// A stand-in for the agent's claude: one turn for the task, one for each message after it; it answers with
	// reply.json (a JSON string) when there is one, else «Готово»; with commit.flag it commits and pushes first
	// (only commits with nopush.flag).
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"), []byte(`#!/bin/sh
read init
while read msg; do
  echo "$msg" >> said.json
  case "$msg" in *повільно*) sleep 1;; esac
  if [ -f commit.flag ]; then git commit -q --allow-empty -m Fixes; [ -f nopush.flag ] || git push -q origin HEAD; fi
  text='"Готово"'
  if [ -f reply.json ]; then text=$(cat reply.json); fi
  printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":'"$text"'}]},"parent_tool_use_id":null}'
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
	return d, cfg, id, wt, idle, f
}

// The reviewer starts only from the board's «Рев'ю»; its remarks stay in the task until «Виправити» hands them
// to the agent, once; the agent's fixes get a round only from the button again.
func TestAgentReview(t *testing.T) {
	d, _, id, wt, idle, _ := reviewSetup(t)
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	var calls atomic.Int32
	answer := "- `pay.go`: немає тесту на порожній кошик"
	ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string, prior []core.Remark) (string, error) {
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
	d, _, id, wt, _, _ := reviewSetup(t)
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	var mu sync.Mutex
	var fail error
	says := func(err error) { mu.Lock(); fail = err; mu.Unlock() }
	ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string, prior []core.Remark) (string, error) {
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

// «Пропустити»: the round is kept as skipped, «Робити» can still start one, and it goes in its place.
func TestReviewSkip(t *testing.T) {
	d, _, id, wt, _, _ := reviewSetup(t)
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string, prior []core.Remark) (string, error) {
		return "", nil
	}
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "Pay")
	git(t, wt, "push", "-q", "origin", "fix/5-pay")

	if err := d.SkipReview(id); err != nil {
		t.Fatal(err)
	}
	r, _ := d.Store.Get(id)
	if len(r.AgentReviews) != 1 || r.AgentReviews[0].State != "skipped" || r.AgentReviews[0].Reason != SkippedByYou {
		t.Fatalf("reviews %+v", r.AgentReviews)
	}
	if err := d.SkipReview(id); err != nil {
		t.Fatalf("skipped again: %v", err)
	}
	if r, _ = d.Store.Get(id); len(r.AgentReviews) != 1 || d.ReviewStep(r) != "ready" {
		t.Fatalf("step %q reviews %+v", d.ReviewStep(r), r.AgentReviews)
	}
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	if r := reviewEnd(t, d, id, "clean"); len(r.AgentReviews) != 1 {
		t.Fatalf("the round did not take the skip's place: %+v", r.AgentReviews)
	}

	// Remarks waiting for «Виправити» are not skipped over.
	d.Store.Update(id, func(r *core.Record) {
		r.AgentReviews = []core.AgentReview{{At: now(), Commit: "x", State: "notes", Notes: "- щось"}}
	})
	if err := d.SkipReview(id); err == nil || !strings.Contains(err.Error(), "чекають") {
		t.Fatalf("skipped the remarks: %v", err)
	}
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

// numberedReview: the agent's branch committed and pushed, with the reviewer's numbered remarks waiting for
// «Виправити»; the reviewer's later rounds answer next(prior).
func numberedReview(t *testing.T, comments string, next func(prior []core.Remark) string) (d *Dispatcher, id, wt string, idle func(), f *fakeGitLab) {
	d, _, id, wt, idle, f = reviewSetup(t, func(c *config.Config) { c.Reviewer.Comments = comments })
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	var mu sync.Mutex
	first := true
	ReviewBranch = func(ctx context.Context, worktree, base string, task core.Task, model string, prior []core.Remark) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if first {
			first = false
			return "- R1 [треба] `pay.go:1` — немає тесту на порожній кошик\n- R2 [варто] — назва функції незрозуміла", nil
		}
		return next(prior), nil
	}
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "Pay")
	git(t, wt, "push", "-q", "origin", "fix/5-pay")
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	r := reviewEnd(t, d, id, "notes")
	if rs := r.AgentReviews[0].Remarks; len(rs) != 2 || rs[0].ID != "R1" || rs[0].Weight != core.Must || rs[0].File != "pay.go" || rs[1].ID != "R2" {
		t.Fatalf("remarks %+v", rs)
	}
	return d, id, wt, idle, f
}

// agentSays: what the stand-in agent answers from now on.
func agentSays(t *testing.T, wt, text string) {
	b, _ := json.Marshal(text)
	os.WriteFile(filepath.Join(wt, "reply.json"), b, 0o644)
}

// replied waits for Mako to read the agent's report on the last round.
func replied(t *testing.T, d *Dispatcher, id string) core.AgentReview {
	t.Helper()
	var last core.AgentReview
	waitFor(t, func() bool {
		r, _ := d.Store.Get(id)
		last = lastReview(r)
		return last.ReplyAt != ""
	}, "the agent's report was not read")
	settle(t, d, id)
	return last
}

func notesWith(f *fakeGitLab, s string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.notes {
		if strings.Contains(b, s) {
			n++
		}
	}
	return n
}

// The agent answers on each remark; «fixed» with no new commit is marked; the remarks and the report go to the
// issue as comments, the report after the reviewer's, with the link to it.
func TestReviewReport(t *testing.T) {
	d, id, wt, idle, f := numberedReview(t, "issue", func([]core.Remark) string { return "" })
	if notesWith(f, "Рев'ю агентом, раунд 1") != 1 || notesWith(f, "R1 [треба] `pay.go:1`") != 1 {
		t.Fatalf("no review comment: %q", f.notes)
	}
	r, _ := d.Store.Get(id)
	if r.AgentReviews[0].CommentURL == "" {
		t.Fatalf("no link to the comment: %+v", r.AgentReviews[0])
	}
	agentSays(t, wt, "Зробив.\nR1 — виправлено (додав тест)\nR2 — не виправлено: назва з задачі")
	if err := d.FixReview(id); err != nil {
		t.Fatal(err)
	}
	idle()
	last := replied(t, d, id)
	if last.Remarks[0].Reply != core.Fixed || last.Remarks[0].ReplyNote != "додав тест" || last.Remarks[1].Reply != core.NotFixed || last.Remarks[1].ReplyNote != "назва з задачі" {
		t.Fatalf("answers %+v", last.Remarks)
	}
	if !last.Unverified || last.Asked {
		t.Fatalf("«fixed» with no commit: unverified %v asked %v", last.Unverified, last.Asked)
	}
	b, _ := os.ReadFile(filepath.Join(wt, "said.json"))
	if !strings.Contains(string(b), "R1 [треба]") || !strings.Contains(string(b), "R1 — виправлено (коротко як") {
		t.Fatalf("the agent got %s", b)
	}
	if notesWith(f, "Відповідь агента на зауваження") != 1 || notesWith(f, "назва з задачі") != 1 || notesWith(f, last.CommentURL) != 1 || notesWith(f, "немає нових комітів") != 1 {
		t.Fatalf("no reply comment: %q", f.notes)
	}
}

// No report: Mako asks the agent once to write it; still none, the round keeps the agent's words, no answers.
func TestReviewNoReport(t *testing.T) {
	d, id, wt, idle, f := numberedReview(t, "", func([]core.Remark) string { return "" })
	if err := d.FixReview(id); err != nil {
		t.Fatal(err)
	}
	idle()
	last := replied(t, d, id)
	if !last.Asked || last.Reply != "Готово" || last.Remarks[0].Reply != "" || last.Unverified {
		t.Fatalf("round %+v", last)
	}
	b, _ := os.ReadFile(filepath.Join(wt, "said.json"))
	if n := strings.Count(string(b), "Ти не написав звіту"); n != 1 {
		t.Fatalf("asked %d times: %s", n, b)
	}
	if len(f.notes) != 0 {
		t.Fatalf("comments with the setting off: %q", f.notes)
	}
}

// The second round checks the first one's remarks with the agent's answers and ends clean once each «must» is
// closed or its explanation accepted.
func TestReviewSecondRound(t *testing.T) {
	var got []core.Remark
	d, id, wt, idle, _ := numberedReview(t, "", func(prior []core.Remark) string {
		got = prior
		return "R1: закрито\nR2: приймаю пояснення\nR3 [варто] pay.go — додай коментар"
	})
	agentSays(t, wt, "R1 — виправлено (тест)\nR2 — не виправлено: назва з задачі")
	os.WriteFile(filepath.Join(wt, "commit.flag"), nil, 0o644)
	if err := d.FixReview(id); err != nil {
		t.Fatal(err)
	}
	idle()
	if last := replied(t, d, id); last.Unverified {
		t.Fatal("unverified with a commit")
	}
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	r := reviewEnd(t, d, id, "clean")
	if len(got) != 2 || got[0].ID != "R1" || got[0].Reply != core.Fixed || got[1].Reply != core.NotFixed {
		t.Fatalf("the second round got %+v", got)
	}
	a := r.AgentReviews[1]
	if len(a.Checks) != 2 || !a.Checks[0].Resolved() || a.Checks[1].Verdict != core.Accepted || len(a.Remarks) != 1 || a.Remarks[0].ID != "R3" || a.Escalated {
		t.Fatalf("second round %+v", a)
	}
}

// Two rounds with a «must» still open: the decision is a person's: a comment for them, no third round.
func TestReviewEscalates(t *testing.T) {
	d, id, wt, idle, f := numberedReview(t, "issue", func([]core.Remark) string {
		return "R1: не закрито: тест не перевіряє nil\nR2: закрито"
	})
	agentSays(t, wt, "R1 — виправлено\nR2 — виправлено")
	os.WriteFile(filepath.Join(wt, "commit.flag"), nil, 0o644)
	if err := d.FixReview(id); err != nil {
		t.Fatal(err)
	}
	idle()
	replied(t, d, id)
	if err := d.ReviewNow(id); err != nil {
		t.Fatal(err)
	}
	r := reviewEnd(t, d, id, "notes")
	a := r.AgentReviews[1]
	if !a.Escalated || len(a.Remarks) != 1 || a.Remarks[0].ID != "R1" || !a.Remarks[0].Again || a.Remarks[0].Reply != "" {
		t.Fatalf("second round %+v", a)
	}
	if notesWith(f, "Рішення за людиною") != 1 || notesWith(f, "Рев'ю агентом, раунд 2") != 1 {
		t.Fatalf("no comment for a person: %q", f.notes)
	}
	// «Виправити» still hands this round's remarks over; another round is then the person's choice, and it
	// does not leave them to a person again.
	if err := d.FixReview(id); err != nil {
		t.Fatal(err)
	}
	idle()
	replied(t, d, id)
	d.mu.Lock()
	delete(d.branches, id)
	d.mu.Unlock()
	if r, _ := d.Store.Get(id); d.ReviewStep(r) != "ready" {
		t.Fatalf("step %q", d.ReviewStep(r))
	}
	if err := d.ReviewNow(id); err != nil {
		t.Fatalf("a third round: %v", err)
	}
	if r := reviewEnd(t, d, id, "notes"); len(r.AgentReviews) != 3 || r.AgentReviews[2].Escalated {
		t.Fatalf("third round %+v", r.AgentReviews)
	}
	if notesWith(f, "Рішення за людиною") != 1 {
		t.Fatalf("a person told twice: %q", f.notes)
	}
}

// LGTM over earlier remarks closes them; a «must» the reviewer gave no verdict on stays open and goes on.
func TestJudgeVerdicts(t *testing.T) {
	prior := []core.Remark{{ID: "R1", Weight: core.Must, Reply: core.Fixed}, {ID: "R2", Weight: core.Should}}
	a := judge("", prior, 3)
	if a.State != "clean" || len(a.Checks) != 2 || !a.Checks[0].Resolved() || !a.Checks[1].Resolved() {
		t.Fatalf("LGTM %+v", a)
	}
	rounds := []core.AgentReview{{State: "notes", Remarks: prior}, a}
	if open := core.OpenRemarks(rounds); len(open) != 0 {
		t.Fatalf("still open after LGTM: %+v", open)
	}
	// A verdict on the «should» only: the «must» is not closed, the round is not clean.
	a = judge("R2: закрито\nR3 [варто] a.go — дрібниця", prior, 3)
	if a.State != "notes" || len(a.Remarks) != 2 || a.Remarks[1].ID != "R1" || !a.Remarks[1].Again || !strings.Contains(a.Remarks[1].Text, "не дав по ньому вердикту") {
		t.Fatalf("no verdict on R1: %+v", a)
	}
	// Free text with no verdicts at all is a round of remarks as before, nothing carried over.
	if a = judge("- щось не так", prior, 3); a.State != "notes" || len(a.Remarks) != 0 {
		t.Fatalf("free text %+v", a)
	}
}

// «Fixed» committed but not pushed, where the branch is to be pushed, is not a fix the reviewer can see.
func TestReviewUnpushed(t *testing.T) {
	d, id, wt, idle, _ := numberedReview(t, "", func([]core.Remark) string { return "" })
	agentSays(t, wt, "R1 — виправлено\nR2 — виправлено")
	os.WriteFile(filepath.Join(wt, "commit.flag"), nil, 0o644)
	os.WriteFile(filepath.Join(wt, "nopush.flag"), nil, 0o644)
	if err := d.FixReview(id); err != nil {
		t.Fatal(err)
	}
	idle()
	if last := replied(t, d, id); !last.Unverified || last.SentHead == "" {
		t.Fatalf("unpushed fix: %+v", last)
	}
}
