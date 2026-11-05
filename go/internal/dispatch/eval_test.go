package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// evalClaude: Claude Code of an eval test. The reviewer (read-only tools) finds one must; the plan is a line; the
// agent writes feature.txt as the switches in $EVAL_LOG say (the word file: what it writes; fail: exits 1; slow:
// hangs; nothing: changes nothing; barrier: waits for a second agent to start) and commits it. Each call costs $1 ($0.5 the reviewer) and is logged in $EVAL_LOG.
const evalClaude = `#!/bin/sh
prompt=$(cat)
echo "$*" >> "$EVAL_LOG/args"
case "$*" in
*"--disallowedTools Edit"*)
	echo review >> "$EVAL_LOG/calls"
	printf '%s' "$prompt" > "$EVAL_LOG/review-prompt"
	printf '%s\n' '{"type":"result","result":"R1 [must] feature.txt:1 — the word is not checked\nR2 [should] feature.txt:1 — say why","total_cost_usd":0.5}'
	exit 0 ;;
*"--permission-mode plan"*)
	echo plan >> "$EVAL_LOG/calls"
	printf '%s\n' '{"type":"result","result":"1. Write the word to feature.txt","total_cost_usd":1}'
	exit 0 ;;
esac
echo work >> "$EVAL_LOG/calls"
printf '%s' "$prompt" > "$EVAL_LOG/work-prompt"
[ -f CLAUDE.md ] && cp CLAUDE.md "$EVAL_LOG/claude-md"
pwd > "$EVAL_LOG/dir"
[ -f "$EVAL_LOG/slow" ] && exec sleep 30
if [ -f "$EVAL_LOG/barrier" ]; then # two agents at work at once: each waits for the other to start
	touch "$EVAL_LOG/started.$$"
	n=0
	while [ "$(ls "$EVAL_LOG" | grep -c '^started\.')" -lt 2 ] && [ $n -lt 500 ]; do sleep 0.02; n=$((n+1)); done
fi
if [ -f "$EVAL_LOG/nothing" ]; then
	printf '%s\n' '{"type":"result","result":"nothing to do","total_cost_usd":1}'
	exit 0
fi
if [ -f "$EVAL_LOG/fail" ]; then
	printf '%s\n' '{"type":"result","result":"I could not","total_cost_usd":1}'
	exit 1
fi
cat "$EVAL_LOG/word" > feature.txt
git add -A && git commit -qm "Add the word"
printf '%s\n' '{"type":"result","result":"Done: wrote the word","total_cost_usd":1}'
`

type evalEnv struct {
	d    *Dispatcher
	f    *fakeGitLab
	cfg  *config.Config
	repo *config.Repo
	key  string
	log  string // $EVAL_LOG
	base string // the commit the task started from
	head string // its MR's head
}

// evalSetup: grp/app with a bare origin behind the fake GitLab's address, whose main has the base commit and the
// merged MR 1 (feature.txt with "pay" and its test tests/pay.sh); the merged tasks grp/app#5 (and #6) of that MR.
// The checks run tests.sh: every tests/*.sh must pass.
func evalSetup(t *testing.T, ids ...int) *evalEnv {
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	d, f, _ := setup(t)
	e := &evalEnv{d: d, f: f, cfg: d.Config(), log: t.TempDir()}
	t.Setenv("EVAL_LOG", e.log)
	claude := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(claude, []byte(evalClaude), 0o755)
	writer.Claude = claude
	os.WriteFile(filepath.Join(e.log, "word"), []byte("pay\n"), 0o644)

	for _, r := range e.cfg.Trackers[0].Projects["grp/app"].Repos {
		e.repo = r
	}
	e.repo.Checks = []string{"sh tests.sh"}
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	git(t, dir, "init", "-q", "--bare", "-b", "main", origin)
	up := filepath.Join(dir, "up")
	git(t, dir, "clone", "-q", origin, up)
	os.WriteFile(filepath.Join(up, "tests.sh"), []byte("for f in tests/*.sh; do sh \"$f\" || exit 1; done\n"), 0o644)
	os.MkdirAll(filepath.Join(up, "tests"), 0o755)
	os.WriteFile(filepath.Join(up, "tests", "base.sh"), []byte("true\n"), 0o644)
	os.WriteFile(filepath.Join(up, "CLAUDE.md"), []byte("the repository's rules\n"), 0o644)
	git(t, up, "add", ".")
	git(t, up, "commit", "-q", "-m", "base")
	git(t, up, "push", "-q", "origin", "main")
	e.base = strings.TrimSpace(gitOut(t, up, "rev-parse", "HEAD"))
	git(t, up, "checkout", "-q", "-b", "fix/5")
	os.WriteFile(filepath.Join(up, "feature.txt"), []byte("pay\n"), 0o644)
	os.WriteFile(filepath.Join(up, "tests", "pay.sh"), []byte("grep -q pay feature.txt\n"), 0o644)
	git(t, up, "add", ".")
	git(t, up, "commit", "-q", "-m", "Pay")
	e.head = strings.TrimSpace(gitOut(t, up, "rev-parse", "HEAD"))
	git(t, up, "push", "-q", "origin", "HEAD:refs/merge-requests/1/head")
	git(t, up, "checkout", "-q", "main")
	git(t, up, "merge", "-q", "--no-ff", "-m", "Merge fix/5", "fix/5")
	git(t, up, "push", "-q", "origin", "main")

	// Mako's clone: its origin is the fake GitLab's address, which git takes to the bare origin.
	address := strings.TrimSpace(gitOut(t, e.repo.Path, "config", "--get", "remote.origin.url"))
	git(t, e.repo.Path, "config", "url."+origin+".insteadOf", address)
	git(t, e.repo.Path, "fetch", "-q", "origin")
	git(t, e.repo.Path, "checkout", "-q", "-b", "main", "origin/main")

	f.mu.Lock()
	f.mrs = []map[string]any{{"iid": 1, "state": "merged", "web_url": f.base + "/grp/app/-/merge_requests/1",
		"diff_refs": map[string]string{"base_sha": e.base, "start_sha": e.base, "head_sha": e.head}}}
	for _, n := range ids[1:] {
		f.issues[n] = map[string]any{"id": 1000 + n, "iid": n, "state": "opened", "title": "Ще одна оплата", "description": "Те саме",
			"web_url": f.base + "/grp/app/-/issues/" + itoa(n), "labels": []string{"claude-agent"}, "issue_type": "issue",
			"assignees": []any{}, "references": map[string]string{"full": "grp/app#" + itoa(n)}}
	}
	f.mu.Unlock()
	d.Poll(context.Background())
	for _, n := range ids {
		id := "grp/app#" + itoa(n)
		d.Store.Update(id, func(r *core.Record) {
			r.Status, r.ReviewState, r.MergedAt = core.Done, "merged", now()
			r.PRURL = f.base + "/grp/app/-/merge_requests/1"
			r.Cost, r.StartedAt, r.FinishedAt = 2.5, "2026-11-01T10:00:00Z", "2026-11-01T10:30:00Z"
			r.Assessment = &core.Assessment{Size: "small"}
			r.Checks = &core.Checks{State: "passed"}
		})
		if err := d.AddEval(context.Background(), id); err != nil {
			t.Fatalf("AddEval(%s): %v", id, err)
		}
	}
	e.key = navKey(e.cfg.Trackers[0].ID, "grp/app", e.repo.Name)
	return e
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// wait: the run of the set is over and its worker gone.
func (e *evalEnv) wait(t *testing.T) core.EvalRun {
	t.Helper()
	waitFor(t, func() bool {
		e.d.evalRunMu.Lock()
		defer e.d.evalRunMu.Unlock()
		return len(e.d.evalRuns) == 0
	}, "the eval run did not end")
	s := e.d.evalSet(e.key)
	return s.Runs[len(s.Runs)-1]
}

func (e *evalEnv) calls() string {
	b, _ := os.ReadFile(filepath.Join(e.log, "calls"))
	return strings.Join(strings.Fields(string(b)), " ")
}

// noWorktrees: nothing of the run is left: no folder, no worktree or branch in the clone.
func (e *evalEnv) noWorktrees(t *testing.T) {
	t.Helper()
	if entries, _ := os.ReadDir(e.d.evalDir()); len(entries) != 0 {
		t.Errorf("left in %s: %v", e.d.evalDir(), entries)
	}
	if list := gitOut(t, e.repo.Path, "worktree", "list"); strings.Contains(list, "eval-wt") {
		t.Errorf("worktrees left: %s", list)
	}
	if branches := gitOut(t, e.repo.Path, "branch", "--list", "mako-eval/*"); strings.TrimSpace(branches) != "" {
		t.Errorf("branches left: %s", branches)
	}
}

func TestEvalSnapshot(t *testing.T) {
	e := evalSetup(t, 5)
	s := e.d.evalSet(e.key)
	if len(s.Cases) != 1 || s.Repo != e.repo.Name || s.Project != "grp/app" {
		t.Fatalf("set %+v", s)
	}
	c := s.Cases[0]
	if c.TaskID != "grp/app#5" || c.Base != e.base || c.Head != e.head || c.Size != "small" || c.Cost != 2.5 || c.Seconds != 1800 {
		t.Errorf("case %+v", c)
	}
	if c.Files != 2 || c.Added != 2 || c.Removed != 0 || c.Task.Title != "Кнопка оплати не працює" {
		t.Errorf("what the MR changed: %+v", c)
	}
	if len(c.Tests) != 1 || c.Tests[0].Path != "tests/pay.sh" || c.Tests[0].Content != "grep -q pay feature.txt\n" {
		t.Errorf("the oracle: %+v", c.Tests)
	}
	if got := strings.TrimSpace(gitOut(t, e.repo.Path, "rev-parse", c.Ref)); got != e.head {
		t.Errorf("the clone's %s is %s, want the MR's head", c.Ref, got)
	}
	if b, err := os.ReadFile(e.d.evalDiffFile(c.TaskID)); err != nil || !strings.Contains(string(b), "+pay") {
		t.Errorf("the diff kept: %v %q", err, b)
	}
	// The board: the set, its test files only, and the task is no candidate any more.
	states := e.d.Evals("grp/app", "", "")
	if len(states) != 1 || len(states[0].Cases) != 1 || states[0].Cases[0].Tests != nil || states[0].Cases[0].TestFiles[0] != "tests/pay.sh" {
		t.Fatalf("board %+v", states)
	}
	if len(states[0].Candidates) != 0 || !states[0].Checks || states[0].MaxCost != evalCaseCost {
		t.Errorf("board %+v", states[0])
	}
	// Added again: read anew, still one case.
	if err := e.d.AddEval(context.Background(), "grp/app#5"); err != nil || len(e.d.evalSet(e.key).Cases) != 1 {
		t.Errorf("added again: %v %+v", err, e.d.evalSet(e.key).Cases)
	}
	// Removed: the case, its diff and its ref go.
	if err := e.d.RemoveEval(e.key, "grp/app#5"); err != nil {
		t.Fatal(err)
	}
	if len(e.d.evalSet(e.key).Cases) != 0 {
		t.Error("the case stayed")
	}
	if _, err := os.Stat(e.d.evalDiffFile("grp/app#5")); err == nil {
		t.Error("the diff stayed")
	}
	if out := gitOut(t, e.repo.Path, "for-each-ref", "refs/mako/eval/"); strings.TrimSpace(out) != "" {
		t.Errorf("the ref stayed: %s", out)
	}
	if c := e.d.Evals("grp/app", "", "")[0].Candidates; len(c) != 1 || !c[0].Suggested || c[0].Size != "small" {
		t.Errorf("a candidate again: %+v", c)
	}
}

func TestEvalAddRefusesWhatIsNotMerged(t *testing.T) {
	d, _, _ := setup(t)
	d.Poll(context.Background())
	if err := d.AddEval(context.Background(), "grp/app#5"); err == nil || !strings.Contains(err.Error(), "merged") {
		t.Errorf("an open task: %v", err)
	}
	if err := d.AddEval(context.Background(), "grp/app#404"); err == nil {
		t.Error("no such task")
	}
}

// A run on the current settings: the agent's work passes the checks and the task's tests, the reviewer's remarks
// are counted, the cost is the calls', nothing reaches the tracker and nothing of the run is left.
func TestEvalRunBaseline(t *testing.T) {
	e := evalSetup(t, 5)
	notes, statuses := len(e.f.notes), len(e.f.statusSet)
	id, err := e.d.StartEval(context.Background(), e.key, core.EvalVariant{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	run := e.wait(t)
	if run.ID != id || run.State != core.EvalDone || run.MaxCost != evalCaseCost || run.FinishedAt == "" {
		t.Fatalf("run %+v", run)
	}
	r := run.Results[0]
	if r.State != core.EvalPassed || r.ChecksOK == nil || !*r.ChecksOK || r.OracleOK == nil || !*r.OracleOK || r.Planned {
		t.Fatalf("result %+v", r)
	}
	if r.Must != 1 || r.Should != 1 || r.Cost != 1.5 || run.Spent != 1.5 || r.Files != 1 || r.Added != 1 || !strings.Contains(r.Review, "R1") {
		t.Errorf("measured %+v", r)
	}
	if e.calls() != "work review" {
		t.Errorf("calls: %s", e.calls())
	}
	prompt, _ := os.ReadFile(filepath.Join(e.log, "work-prompt"))
	for _, want := range []string{"Кнопка оплати не працює", "Це eval-прогін", "mako-eval/" + id + "/grp-app-5"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("the prompt has no %q:\n%s", want, prompt)
		}
	}
	args, _ := os.ReadFile(filepath.Join(e.log, "args"))
	if !strings.Contains(string(args), "--permission-mode acceptEdits") || !strings.Contains(string(args), "--max-budget-usd 5.00") {
		t.Errorf("args: %s", args)
	}
	if md, _ := os.ReadFile(filepath.Join(e.log, "claude-md")); string(md) != "the repository's rules\n" {
		t.Errorf("CLAUDE.md the agent saw: %q", md)
	}
	if len(e.f.notes) != notes || len(e.f.statusSet) != statuses {
		t.Error("the eval run reached the tracker")
	}
	e.noWorktrees(t)
	if out := gitOut(t, e.repo.Path, "ls-remote", "origin"); strings.Contains(out, "mako-eval") {
		t.Errorf("pushed: %s", out)
	}
	if s := e.d.Evals("grp/app", "", "")[0]; len(s.Runs) != 1 || !s.Runs[0].Baseline || s.Runs[0].Summary.SuccessRate != 1 || s.Running {
		t.Errorf("board %+v", s.Runs)
	}
}

// A variant: its model, effort, CLAUDE.md and end of the prompt reach the agent; the plan, when the settings start
// with one, goes to the work approved; an agent whose work misses the task's own test fails on the oracle.
func TestEvalRunVariantAndOracle(t *testing.T) {
	e := evalSetup(t, 5)
	e.cfg.Terminal.PlanFirst = "always"
	os.WriteFile(filepath.Join(e.log, "word"), []byte("cash\n"), 0o644)
	v := core.EvalVariant{Name: "sonnet", Model: "sonnet", Effort: "high", ClaudeMD: "new rules\n", Instructions: "Finish my way"}
	if _, err := e.d.StartEval(context.Background(), e.key, v, 10); err != nil {
		t.Fatal(err)
	}
	run := e.wait(t)
	r := run.Results[0]
	if r.State != core.EvalFailed || !*r.ChecksOK || *r.OracleOK || !r.Planned || r.Route != "sonnet · high" || r.Cost != 2.5 {
		t.Fatalf("result %+v", r)
	}
	if run.Variant.ClaudeMDFrom != "text" || run.MaxCost != 10 {
		t.Errorf("run %+v", run)
	}
	if e.calls() != "plan work review" {
		t.Errorf("calls: %s", e.calls())
	}
	prompt, _ := os.ReadFile(filepath.Join(e.log, "work-prompt"))
	for _, want := range []string{"Finish my way", "План затверджено", "1. Write the word to feature.txt"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("the prompt has no %q:\n%s", want, prompt)
		}
	}
	args, _ := os.ReadFile(filepath.Join(e.log, "args"))
	if !strings.Contains(string(args), "--model sonnet --effort high") {
		t.Errorf("args: %s", args)
	}
	if md, _ := os.ReadFile(filepath.Join(e.log, "claude-md")); string(md) != "new rules\n" {
		t.Errorf("CLAUDE.md the agent saw: %q", md)
	}
	// The variant's CLAUDE.md is no work of the agent's: not in its diff, not in what the reviewer reads.
	if r.Files != 1 || r.Added != 1 {
		t.Errorf("the agent's diff counts the CLAUDE.md of the variant: %+v", r)
	}
	// The prompt's own words name CLAUDE.md among the conventions; what the branch changed is after "Commits:".
	review, _ := os.ReadFile(filepath.Join(e.log, "review-prompt"))
	_, changes, _ := strings.Cut(string(review), "Commits:")
	if !strings.Contains(changes, "feature.txt") || strings.Contains(changes, "CLAUDE.md") || strings.Contains(changes, "new rules") {
		t.Errorf("what the reviewer reads of the branch:\n%s", changes)
	}
	e.noWorktrees(t)
}

// The variant's CLAUDE.md differs from the repository's and the agent changes nothing: nothing was done, not a
// change of CLAUDE.md.
func TestEvalVariantAndNothingDone(t *testing.T) {
	e := evalSetup(t, 5)
	os.WriteFile(filepath.Join(e.log, "nothing"), nil, 0o644)
	if _, err := e.d.StartEval(context.Background(), e.key, core.EvalVariant{ClaudeMD: "new rules\n"}, 0); err != nil {
		t.Fatal(err)
	}
	run := e.wait(t)
	if r := run.Results[0]; r.State != core.EvalFailed || r.Error != "the agent changed nothing" || r.Files != 0 || r.Cost != 1 {
		t.Fatalf("result %+v", r)
	}
	if e.calls() != "work" {
		t.Errorf("calls: %s", e.calls())
	}
	e.noWorktrees(t)
}

// placeClaudeMD: the file is in the worktree for the agent, and neither committed with its work (commitLeft) nor
// seen as a change, whether the repository has a CLAUDE.md or not.
func TestPlaceClaudeMD(t *testing.T) {
	for _, tracked := range []bool{true, false} {
		name := map[bool]string{true: "tracked", false: "untracked"}[tracked]
		t.Run(name, func(t *testing.T) {
			for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
				t.Setenv(k, v)
			}
			ctx := context.Background()
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			os.MkdirAll(repo, 0o755)
			git(t, repo, "init", "-q", "-b", "main")
			os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644)
			if tracked {
				os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("the repository's rules\n"), 0o644)
			}
			git(t, repo, "add", ".")
			git(t, repo, "commit", "-q", "-m", "base")
			base := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD"))
			ensureWorktreeConfig(ctx, repo)
			wt := filepath.Join(root, "wt")
			git(t, repo, "worktree", "add", "-q", "--detach", wt, base)

			if err := placeClaudeMD(ctx, wt, wt+".exclude", "new rules\n"); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(filepath.Join(wt, "CLAUDE.md")); string(b) != "new rules\n" {
				t.Fatalf("CLAUDE.md in the worktree: %q", b)
			}
			// Nothing else changed: nothing to commit.
			if err := commitLeft(ctx, wt, "t#1"); err != nil {
				t.Fatal(err)
			}
			if head := strings.TrimSpace(gitOut(t, wt, "rev-parse", "HEAD")); head != base {
				t.Fatalf("CLAUDE.md was committed: %s", gitOut(t, wt, "show", "--stat", "HEAD"))
			}
			// The agent's own file goes in, CLAUDE.md does not.
			os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("x\n"), 0o644)
			if err := commitLeft(ctx, wt, "t#1"); err != nil {
				t.Fatal(err)
			}
			if files := strings.Fields(gitOut(t, wt, "diff", "--name-only", base, "HEAD")); len(files) != 1 || files[0] != "feature.txt" {
				t.Errorf("committed %v", files)
			}
			if b, _ := os.ReadFile(filepath.Join(wt, "CLAUDE.md")); string(b) != "new rules\n" {
				t.Errorf("CLAUDE.md after the commit: %q", b)
			}
			// The main checkout is untouched.
			if tracked {
				if b, _ := os.ReadFile(filepath.Join(repo, "CLAUDE.md")); string(b) != "the repository's rules\n" {
					t.Errorf("the clone's CLAUDE.md: %q", b)
				}
			} else if _, err := os.Stat(filepath.Join(repo, "CLAUDE.md")); err == nil {
				t.Error("the clone got a CLAUDE.md")
			}
		})
	}
}

// The reviewer fails: the case is measured all the same (its checks and tests are), but it says the reviewer did not
// read it, and no zero of remarks is counted for it.
func TestEvalReviewerFails(t *testing.T) {
	e := evalSetup(t, 5)
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	ReviewBranch = func(context.Context, string, string, core.Task, core.Route, []core.Remark) (string, error) {
		return "", errors.New("the reviewer did not answer")
	}
	if _, err := e.d.StartEval(context.Background(), e.key, core.EvalVariant{}, 0); err != nil {
		t.Fatal(err)
	}
	run := e.wait(t)
	r := run.Results[0]
	if r.State != core.EvalPassed || r.Reviewed() || !strings.Contains(r.ReviewError, "did not answer") || r.Must != 0 || r.Should != 0 {
		t.Fatalf("result %+v", r)
	}
	if s := core.SummarizeEval(run); s.Unreviewed != 1 || s.Must != 0 {
		t.Errorf("summary %+v", s)
	}
	e.noWorktrees(t)
}

func TestEvalClaudeMDFromBranchAndFile(t *testing.T) {
	e := evalSetup(t, 5)
	file := filepath.Join(t.TempDir(), "CLAUDE.md")
	os.WriteFile(file, []byte("from a file\n"), 0o644)
	if got, err := claudeMDFrom(context.Background(), e.repo, file); err != nil || got != "from a file\n" {
		t.Errorf("file: %q %v", got, err)
	}
	if got, err := claudeMDFrom(context.Background(), e.repo, "main"); err != nil || got != "the repository's rules\n" {
		t.Errorf("branch: %q %v", got, err)
	}
	for _, bad := range []string{"no-such-branch", "--upload-pack=x", "/no/such/file"} {
		if _, err := claudeMDFrom(context.Background(), e.repo, bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
	for _, v := range []core.EvalVariant{{Model: "--evil"}, {Effort: "huge"}} {
		if _, err := e.d.StartEval(context.Background(), e.key, v, 0); err == nil {
			t.Errorf("%+v started", v)
		}
	}
}

// The ceiling stops the run's new cases, not the one under way: the spent budget leaves the next case skipped, with
// why, and what was spent stays in the results. The call gets the whole rest of the ceiling: one worker holds it.
func TestEvalCostCeiling(t *testing.T) {
	e := evalSetup(t, 5, 6)
	e.cfg.WipLimit = 1
	if _, err := e.d.StartEval(context.Background(), e.key, core.EvalVariant{}, 1.2); err != nil {
		t.Fatal(err)
	}
	run := e.wait(t)
	if run.State != core.EvalStopped || run.Reason != "cost ceiling reached" || run.Spent != 1.5 {
		t.Fatalf("run %+v", run)
	}
	if r := run.Results[0]; r.State != core.EvalPassed || !r.Reviewed() || r.Cost != 1.5 {
		t.Errorf("the case under way: %+v", r)
	}
	if r := run.Results[1]; r.State != core.EvalSkipped || r.Error != "cost ceiling reached" || r.Cost != 0 {
		t.Errorf("the case after the ceiling: %+v", r)
	}
	if e.calls() != "work review" {
		t.Errorf("calls: %s", e.calls())
	}
	if args, _ := os.ReadFile(filepath.Join(e.log, "args")); !strings.Contains(string(args), "--max-budget-usd 1.20") {
		t.Errorf("args: %s", args)
	}
	e.noWorktrees(t)
}

// The ceiling is reached by the agent's work: the case goes on to its checks and its tests, which cost nothing, and is
// measured, only the reviewer is not asked (it has no budget of its own to be capped by), and the result says so.
func TestEvalCeilingMidCase(t *testing.T) {
	e := evalSetup(t, 5)
	if _, err := e.d.StartEval(context.Background(), e.key, core.EvalVariant{}, 1); err != nil {
		t.Fatal(err)
	}
	run := e.wait(t)
	r := run.Results[0]
	if run.State != core.EvalDone || run.Reason != "" || r.State != core.EvalPassed || r.Cost != 1 || run.Spent != 1 {
		t.Fatalf("run %+v", run)
	}
	if r.OracleOK == nil || !*r.OracleOK || r.Reviewed() || r.ReviewError != "cost ceiling reached" || r.Must != 0 {
		t.Errorf("result %+v", r)
	}
	if e.calls() != "work" {
		t.Errorf("calls: %s", e.calls())
	}
	if s := core.SummarizeEval(run); s.Ran != 1 || s.Unreviewed != 1 || s.Must != 0 {
		t.Errorf("summary %+v", s)
	}
	e.noWorktrees(t)
}

// Agents at work side by side each reserve their share of the ceiling, so together they cannot be given more than
// there is: two calls under a ceiling of 2 get 1 each, not 2 each; and once it is spent the third case is not begun.
func TestEvalParallelCeiling(t *testing.T) {
	e := evalSetup(t, 5, 6, 7)
	e.cfg.WipLimit = 2
	os.WriteFile(filepath.Join(e.log, "barrier"), nil, 0o644)
	if _, err := e.d.StartEval(context.Background(), e.key, core.EvalVariant{}, 2); err != nil {
		t.Fatal(err)
	}
	run := e.wait(t)
	if run.State != core.EvalStopped || run.Reason != "cost ceiling reached" {
		t.Fatalf("run %+v", run)
	}
	args, _ := os.ReadFile(filepath.Join(e.log, "args"))
	var shares []string
	for _, line := range strings.Split(string(args), "\n") {
		if strings.Contains(line, "acceptEdits") {
			shares = append(shares, line[strings.Index(line, "--max-budget-usd"):][:len("--max-budget-usd 1.00")])
		}
	}
	if len(shares) != 2 || shares[0] != "--max-budget-usd 1.00" || shares[1] != "--max-budget-usd 1.00" {
		t.Errorf("the work calls' budgets: %q", shares)
	}
	passed := 0
	for _, r := range run.Results {
		switch r.State {
		case core.EvalPassed:
			passed++
			if r.Cost < 1 || (r.Reviewed() && r.Must != 1) || (!r.Reviewed() && r.ReviewError != "cost ceiling reached") {
				t.Errorf("a case under way keeps its spend, and its review or the word that it has none: %+v", r)
			}
		case core.EvalSkipped:
			if r.Error != "cost ceiling reached" || r.Cost != 0 {
				t.Errorf("skipped %+v", r)
			}
		default:
			t.Errorf("result %+v", r)
		}
	}
	if passed != 2 || run.Results[2].State != core.EvalSkipped || strings.Count(e.calls(), "work") != 2 {
		t.Errorf("passed %d, results %+v, calls %s", passed, run.Results, e.calls())
	}
	if run.Spent > 3 { // two works and two reviews at most; never the 4 of two whole ceilings
		t.Errorf("spent %v", run.Spent)
	}
	e.noWorktrees(t)
}

// Claude fails: the case is an error, the worktree goes all the same; one run at a time per repository.
func TestEvalAgentFails(t *testing.T) {
	e := evalSetup(t, 5)
	os.WriteFile(filepath.Join(e.log, "fail"), nil, 0o644)
	if _, err := e.d.StartEval(context.Background(), e.key, core.EvalVariant{}, 0); err != nil {
		t.Fatal(err)
	}
	run := e.wait(t)
	if r := run.Results[0]; run.State != core.EvalDone || r.State != core.EvalError || !strings.Contains(r.Error, "agent") || r.Cost != 1 {
		t.Fatalf("run %+v", run)
	}
	if s := core.SummarizeEval(run); s.Errors != 1 || s.Ran != 0 {
		t.Errorf("summary %+v", s)
	}
	e.noWorktrees(t)
}

func TestEvalStop(t *testing.T) {
	e := evalSetup(t, 5)
	os.WriteFile(filepath.Join(e.log, "slow"), nil, 0o644)
	if _, err := e.d.StartEval(context.Background(), e.key, core.EvalVariant{}, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(e.log, "dir")); return err == nil }, "the agent did not start")
	if _, err := e.d.StartEval(context.Background(), e.key, core.EvalVariant{}, 0); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("a second run: %v", err)
	}
	if err := e.d.StopEval(e.key); err != nil {
		t.Fatal(err)
	}
	run := e.wait(t)
	if run.State != core.EvalStopped || run.Reason != "stopped by you" || run.Results[0].State != core.EvalSkipped {
		t.Fatalf("run %+v", run)
	}
	e.noWorktrees(t)
	if err := e.d.StopEval(e.key); err == nil {
		t.Error("stopped a run that is not going on")
	}
}

// A run that was going on when Mako stopped is shown stopped by the next Mako; what it left goes at the next run.
func TestEvalAfterRestart(t *testing.T) {
	e := evalSetup(t, 5)
	e.d.updateEval(e.key, func(s *EvalSet) error {
		s.Runs = append(s.Runs, core.EvalRun{ID: "old", State: core.EvalRunning, Results: []core.EvalResult{{TaskID: "grp/app#5", State: core.EvalRunning}}})
		return nil
	})
	left := filepath.Join(e.d.evalDir(), "old", "grp-app-5")
	git(t, e.repo.Path, "worktree", "add", "-q", "-b", "mako-eval/old/grp-app-5", left, e.base)

	store, _ := state.Open(e.cfg.StateDir)
	gl, _ := trackers.New(e.cfg.Trackers[0], e.cfg)
	d := New(e.cfg, store, []trackers.Tracker{gl})
	runs := d.Evals("grp/app", "", "")[0].Runs
	if len(runs) != 1 || runs[0].State != core.EvalStopped || runs[0].Reason != "Mako restarted" || runs[0].Results[0].State != core.EvalSkipped {
		t.Fatalf("runs %+v", runs)
	}
	if _, err := d.StartEval(context.Background(), e.key, core.EvalVariant{}, 0); err != nil {
		t.Fatal(err)
	}
	e.d = d
	e.wait(t)
	e.noWorktrees(t)
}

// A broken evals.json is kept aside and the sets start empty.
func TestEvalBrokenFile(t *testing.T) {
	d, _, _ := setup(t)
	os.MkdirAll(d.Config().StateDir, 0o755)
	os.WriteFile(d.evalFile(), []byte("{broken"), 0o600)
	if s := d.evalSet("x"); len(s.Cases) != 0 {
		t.Fatalf("set %+v", s)
	}
	if _, err := os.Stat(d.evalFile() + ".broken"); err != nil {
		t.Errorf("not kept aside: %v", err)
	}
	if fs := d.Faults(); len(fs) != 1 || !strings.Contains(fs[0].What, "evals.json") {
		t.Errorf("not among Mako's errors: %+v", fs)
	}
}

func TestCompareRuns(t *testing.T) {
	base := func(id, state string) core.EvalRun {
		return core.EvalRun{ID: id, State: state, Results: []core.EvalResult{{TaskID: "a", State: core.EvalPassed}}}
	}
	variant := func(id string) core.EvalRun {
		r := base(id, core.EvalDone)
		r.Variant.Model = "sonnet"
		r.Results[0].State = core.EvalFailed
		return r
	}
	stopped := func(id string) core.EvalRun {
		r := variant(id)
		r.State = core.EvalStopped
		return r
	}
	pair := func(c *EvalComparison) string {
		if c == nil {
			return ""
		}
		return c.Base + "|" + c.Variant
	}
	for _, c := range []struct {
		name         string
		runs         []core.EvalRun
		base, varian string
		want         string
	}{
		{"one run", []core.EvalRun{base("b1", core.EvalDone)}, "", "", ""},
		{"last baseline, last variant", []core.EvalRun{base("b1", core.EvalDone), variant("v1"), base("b2", core.EvalDone), variant("v2")}, "", "", "b2|v2"},
		{"a running baseline is not the one", []core.EvalRun{base("b1", core.EvalDone), variant("v1"), base("b2", core.EvalRunning)}, "", "", "b1|v1"},
		{"a variant before any baseline", []core.EvalRun{variant("v1"), base("b1", core.EvalDone)}, "", "", "b1|v1"},
		{"two baselines", []core.EvalRun{base("b1", core.EvalDone), base("b2", core.EvalDone)}, "", "", "b1|b2"},
		{"no baseline yet", []core.EvalRun{variant("v1"), variant("v2")}, "", "", "v1|v2"},
		{"a finished variant over a stopped one", []core.EvalRun{base("b1", core.EvalDone), variant("v1"), stopped("v2")}, "", "", "b1|v1"},
		{"picked", []core.EvalRun{base("b1", core.EvalDone), variant("v1"), base("b2", core.EvalDone), variant("v2")}, "b1", "v1", "b1|v1"},
		{"picked from another set", []core.EvalRun{base("b1", core.EvalDone), variant("v1")}, "x", "y", "b1|v1"},
		{"the same run twice", []core.EvalRun{base("b1", core.EvalDone), variant("v1")}, "v1", "v1", "v1|b1"},
	} {
		got := compareRuns(c.runs, c.base, c.varian)
		if pair(got) != c.want {
			t.Errorf("%s: %q, want %q", c.name, pair(got), c.want)
		}
		if got != nil && (len(got.Deltas) != 1 || got.Deltas[0].TaskID != "a") {
			t.Errorf("%s: deltas %+v", c.name, got.Deltas)
		}
	}
}

// Without checks in the repository, a small or medium merged task is suggested by its size alone.
func TestEvalCandidatesWithoutChecks(t *testing.T) {
	e := evalSetup(t, 5)
	e.repo.Checks = nil
	if err := e.d.RemoveEval(e.key, "grp/app#5"); err != nil {
		t.Fatal(err)
	}
	e.d.Store.Update("grp/app#5", func(r *core.Record) { r.Checks = nil })
	s := e.d.Evals("grp/app", "", "")[0]
	if s.Checks || len(s.Candidates) != 1 || !s.Candidates[0].Suggested {
		t.Fatalf("eval %+v", s)
	}
	e.d.Store.Update("grp/app#5", func(r *core.Record) { r.Assessment.Size = "big" })
	if c := e.d.Evals("grp/app", "", "")[0].Candidates; c[0].Suggested {
		t.Errorf("a big task suggested: %+v", c)
	}
}

// No push guard, no agent: the case is an error and its worktree goes. The guard of a real run refuses a push.
func TestEvalNeedsThePushGuard(t *testing.T) {
	e := evalSetup(t, 5)
	old := guardEval
	t.Cleanup(func() { guardEval = old })
	var guarded string
	guardEval = func(ctx context.Context, dir, hooks string) error {
		if err := old(ctx, dir, hooks); err != nil {
			return err
		}
		guarded = dir
		cmd := exec.Command("git", "push", "origin", "HEAD:refs/heads/leak")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "eval run") {
			t.Errorf("the eval worktree pushed: %v %s", err, out)
		}
		return errors.New("no hooks here")
	}
	if _, err := e.d.StartEval(context.Background(), e.key, core.EvalVariant{}, 0); err != nil {
		t.Fatal(err)
	}
	run := e.wait(t)
	if r := run.Results[0]; r.State != core.EvalError || !strings.Contains(r.Error, "push guard") || e.calls() != "" {
		t.Fatalf("result %+v, calls %q", r, e.calls())
	}
	if guarded == "" {
		t.Fatal("the guard was not asked")
	}
	e.noWorktrees(t)
}

// The board asks for the sets with every /api/state: the file is parsed again only when it changed, and what goes out
// is what the board shows (the reviewer's notes cut, no tails of commands' output), while the file keeps it whole.
func TestEvalsAreReadWhenChanged(t *testing.T) {
	e := evalSetup(t, 5)
	long := strings.Repeat("remark ", 300)
	e.d.updateEval(e.key, func(s *EvalSet) error {
		s.Runs = append(s.Runs, core.EvalRun{ID: "r1", State: core.EvalDone, Results: []core.EvalResult{{
			TaskID: "grp/app#5", State: core.EvalPassed, Review: long, Checks: []core.CheckResult{{Name: "tests", OK: true, Tail: "tail of the output"}}}}})
		return nil
	})
	reads := func() int {
		e.d.evalMu.Lock()
		defer e.d.evalMu.Unlock()
		return e.d.evalReads
	}
	first := e.d.Evals("grp/app", "", "")
	n := reads()
	for range 5 {
		e.d.Evals("grp/app", "", "")
	}
	if reads() != n {
		t.Errorf("the file was parsed %d times more for the same state", reads()-n)
	}
	r := first[0].Runs[0].Results[0]
	if len(r.Review) >= len(long) || len(r.Review) > 3*evalNotesInView || r.Checks[0].Tail != "" || r.Checks[0].Name != "tests" {
		t.Errorf("the result as the board gets it: %d bytes of notes, %+v", len(r.Review), r.Checks)
	}
	if kept := e.d.evalSet(e.key).Runs[0].Results[0]; kept.Review != long || kept.Checks[0].Tail != "tail of the output" {
		t.Errorf("the file lost what the board does not get: %d bytes, %+v", len(kept.Review), kept.Checks)
	}

	// A change by Mako is seen at once; so is one made to the file by someone else.
	e.d.updateEval(e.key, func(s *EvalSet) error { s.Runs[0].Variant.Name = "renamed"; return nil })
	if got := e.d.Evals("grp/app", "", "")[0].Runs[0].Variant.Name; got != "renamed" {
		t.Errorf("after Mako's change: %q", got)
	}
	b, _ := os.ReadFile(e.d.evalFile())
	os.WriteFile(e.d.evalFile(), []byte(strings.Replace(string(b), "renamed", "edited!", 1)), 0o600)
	later := time.Now().Add(time.Hour)
	os.Chtimes(e.d.evalFile(), later, later)
	if got := e.d.Evals("grp/app", "", "")[0].Runs[0].Variant.Name; got != "edited!" {
		t.Errorf("after an edit of the file: %q", got)
	}
}
