package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// riskRepo: a repository whose main has a payment module with two reverts and hotfixes in its history, a migration,
// go.mod and a file to remove; each case branches off main.
func riskRepo(t *testing.T) string {
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	files := map[string]string{"app/pay.go": "package app\n", "migrations/001.sql": "create table t();\n", "go.mod": "module x\n", "old.go": "package x\n", "README.md": "# x\n"}
	for name, body := range files {
		write(t, dir, name, body)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	for i, subject := range []string{`Revert "Pay with cards"`, "Fix the cart", "hotfix: pay twice"} {
		write(t, dir, "app/pay.go", "package app\n"+strings.Repeat("// x\n", i+1))
		git(t, dir, "commit", "-q", "-am", subject)
	}
	return dir
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Each signal on its own diff: the paths of the repo's patterns by their levels, the size, code without a test,
// removed files, dependencies and the files' history of reverts and hotfixes; a change of the docs has none.
func TestRiskSignals(t *testing.T) {
	dir := riskRepo(t)
	repo := &config.Repo{
		RiskPaths: []config.RiskPath{{Pattern: "migrations/**", Level: core.RiskHigh}, {Pattern: "*.env*", Level: core.RiskBlock}, {Pattern: "docs/", Level: core.RiskMedium}},
		Risk:      config.RiskPolicy{MaxLines: 10, MaxFiles: 5},
	}
	type want struct{ kind, level string }
	for _, c := range []struct {
		name   string
		change func(wt string)
		want   []want
		files  map[string][]string // a signal's files, by kind
	}{
		{"docs only", func(wt string) { write(t, wt, "README.md", "# x\nmore\n") }, nil, nil},
		{"code with its test", func(wt string) {
			write(t, wt, "app/cart.go", "package app\n")
			write(t, wt, "app/cart_test.go", "package app\n")
		}, nil, nil},
		{"code without a test", func(wt string) { write(t, wt, "app/cart.go", "package app\n") },
			[]want{{core.SignalNoTests, core.RiskMedium}}, map[string][]string{core.SignalNoTests: {"app/cart.go"}}},
		{"a migration", func(wt string) { write(t, wt, "migrations/002.sql", "alter table t;\n") },
			[]want{{core.SignalPath, core.RiskHigh}}, map[string][]string{core.SignalPath: {"migrations/002.sql"}}},
		{"a protected path", func(wt string) { write(t, wt, "config/.env.local", "A=1\n") },
			[]want{{core.SignalPath, core.RiskBlock}}, nil},
		{"a folder pattern", func(wt string) { write(t, wt, "docs/guide.md", "x\n") }, []want{{core.SignalPath, core.RiskMedium}}, nil},
		{"big", func(wt string) { write(t, wt, "notes.md", strings.Repeat("line\n", 20)) }, []want{{core.SignalSize, core.RiskMedium}}, nil},
		{"huge", func(wt string) { write(t, wt, "notes.md", strings.Repeat("line\n", 40)) }, []want{{core.SignalSize, core.RiskHigh}}, nil},
		{"many files", func(wt string) {
			for i := range 6 {
				write(t, wt, filepath.Join("notes", string(rune('a'+i))+".md"), "x\n")
			}
		}, []want{{core.SignalSize, core.RiskMedium}}, nil},
		{"removed", func(wt string) { git(t, wt, "rm", "-q", "old.go") }, []want{{core.SignalRemoved, core.RiskMedium}},
			map[string][]string{core.SignalRemoved: {"old.go"}}},
		{"renamed", func(wt string) { git(t, wt, "mv", "old.go", "new_test.go"); write(t, wt, "app/x.go", "package app\n") },
			nil, nil}, // a file that became a test: no removal of a public one; code with a test
		{"dependencies", func(wt string) { write(t, wt, "go.mod", "module x\n\nrequire y v1\n") }, []want{{core.SignalDeps, core.RiskMedium}}, nil},
		{"history", func(wt string) {
			write(t, wt, "app/pay.go", "package app\n// new\n")
			write(t, wt, "app/pay_test.go", "package app\n")
		}, []want{{core.SignalHistory, core.RiskMedium}}, map[string][]string{core.SignalHistory: {"app/pay.go"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			wt := filepath.Join(t.TempDir(), "wt")
			git(t, dir, "worktree", "add", "-q", "-b", "b/"+strings.ReplaceAll(c.name, " ", "-"), wt, "main")
			c.change(wt)
			git(t, wt, "add", "-A")
			git(t, wt, "commit", "-q", "-m", c.name)
			cs, err := gitx.Changes(context.Background(), wt, "main")
			if err != nil {
				t.Fatal(err)
			}
			var got []want
			signals := riskSignals(context.Background(), wt, "main", cs.Files, repo)
			for _, s := range signals {
				got = append(got, want{s.Kind, s.Level})
				if s.Text == "" {
					t.Errorf("a signal with no text: %+v", s)
				}
				if f, ok := c.files[s.Kind]; ok && !slices.Equal(s.Files, f) {
					t.Errorf("%s: files %v, want %v", s.Kind, s.Files, f)
				}
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("signals %+v, want %+v", signals, c.want)
			}
		})
	}

	// Three more hotfixes of the same file: five in all is high.
	for i := range 3 {
		write(t, dir, "app/pay.go", "package app\n"+strings.Repeat("// y\n", i+1))
		git(t, dir, "commit", "-q", "-am", "Revert the hotfix")
	}
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, dir, "worktree", "add", "-q", "-b", "b/history-high", wt, "main")
	write(t, wt, "app/pay.go", "package app\n// again\n")
	git(t, wt, "commit", "-q", "-am", "again")
	cs, _ := gitx.Changes(context.Background(), wt, "main")
	signals := riskSignals(context.Background(), wt, "main", cs.Files, repo)
	if i := slices.IndexFunc(signals, func(s core.RiskSignal) bool { return s.Kind == core.SignalHistory }); i < 0 || signals[i].Level != core.RiskHigh || !strings.Contains(signals[i].Text, "5 reverts") {
		t.Fatalf("history: %+v", signals)
	}
	// No repo known: the default thresholds, no patterns.
	if s := riskSignals(context.Background(), wt, "main", []gitx.ChangedFile{{Path: "migrations/x.sql", Status: "A", Additions: 500}}, nil); len(s) != 1 || s[0].Kind != core.SignalSize {
		t.Fatalf("defaults: %+v", s)
	}
}

// The test files and the dependencies are told by their names.
func TestRiskFileKinds(t *testing.T) {
	for _, p := range []string{"a_test.go", "src/a.test.ts", "src/a.spec.js", "tests/a.py", "x/__tests__/a.js", "test_a.py", "spec/a_spec.rb", "src/FooTest.java", "go/internal/x/testdata/a.go"} {
		if !isTestFile(p) || isCodeFile(p) {
			t.Errorf("%s is a test", p)
		}
	}
	for _, p := range []string{"a.go", "src/contest.ts", "latest/a.py"} {
		if isTestFile(p) || !isCodeFile(p) {
			t.Errorf("%s is code", p)
		}
	}
	for _, p := range []string{"go.mod", "web/package.json", "yarn.lock", "requirements-dev.txt", "Cargo.lock"} {
		if !isDepFile(p) {
			t.Errorf("%s is a dependency", p)
		}
	}
	if isDepFile("package.go") || isCodeFile("README.md") {
		t.Error("not a dependency, not code")
	}
}

// riskSetup: an open MR of Mako's agent ready to merge (see selfSetup), with the selfCheck setting left as advice and
// no steps advised; policy sets the repo's risk policy.
func riskSetup(t *testing.T, policy config.RiskPolicy) (d *Dispatcher, id, wt string, f *fakeGitLab) {
	d, _, id, wt, f = selfSetup(t, nil, func(c *config.Config) {
		c.SelfCheck = config.SelfCheckAdvised
		for _, r := range c.Trackers[0].Projects["grp/app"].Repos {
			r.Risk = policy
		}
	})
	f.mu.Lock()
	f.mrs[0]["merge_status"] = "mergeable"
	f.mrs[0]["head_pipeline"] = map[string]any{"id": 1, "status": "success"}
	f.mu.Unlock()
	return d, id, wt, f
}

func assessedRisk(d *Dispatcher, id string, risk int) {
	d.Store.Update(id, func(r *core.Record) {
		r.Assessment = &core.Assessment{At: now(), State: "done", Risk: core.Score{Value: risk, Why: "why"}, Review: &core.ReviewAdvice{}}
	})
}

func mrState(f *fakeGitLab) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mrs[0]["state"]
}

// A high risk holds the merge for «Check it yourself», though the setting only advises it: the board says why, a
// skip with a reason lets it through, and the stats learn the level; with gateHigh off it is advice again.
func TestRiskGateHigh(t *testing.T) {
	d, id, _, f := riskSetup(t, config.RiskPolicy{GateHigh: true})
	assessedRisk(d, id, 4)
	r, _ := d.Store.Get(id)
	if v := d.RiskView(r); v == nil || v.Level != core.RiskHigh || v.Policy != "gate" || len(v.Signals) != 1 {
		t.Fatalf("view %+v", v)
	}
	if v := d.SelfView(r); v == nil || !v.Required || !v.Blocks || len(v.Steps) != 1 || d.MergeReady(r) {
		t.Fatalf("not held: %+v", v)
	}
	if err := d.MergeReview(context.Background(), id); err == nil || !strings.Contains(err.Error(), "check it yourself") {
		t.Fatalf("merged: %v", err)
	}
	if err := d.SelfSkip(id, "лише перейменування"); err != nil {
		t.Fatal(err)
	}
	if err := d.MergeReview(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if r, _ = d.Store.Get(id); mrState(f) != "merged" || r.RiskAtMerge != core.RiskHigh || r.MergedBy != "board" {
		t.Fatalf("MR %v, risk %q, by %q", mrState(f), r.RiskAtMerge, r.MergedBy)
	}

	d2, id2, _, _ := riskSetup(t, config.RiskPolicy{})
	assessedRisk(d2, id2, 5)
	r, _ = d2.Store.Get(id2)
	if v := d2.RiskView(r); v.Policy != "advice" || d2.SelfView(r) != nil || !d2.MergeReady(r) {
		t.Fatalf("gateHigh off: %+v %+v", v, d2.SelfView(r))
	}
	// Medium: as before, the merge is yours.
	assessedRisk(d2, id2, 3)
	if r, _ = d2.Store.Get(id2); d2.RiskView(r).Policy != "advice" || !d2.MergeReady(r) {
		t.Fatal("medium is held")
	}
}

// A protected path in the branch blocks the merge, from the board and by itself, and says so.
func TestRiskBlocksMerge(t *testing.T) {
	d, id, wt, f := riskSetup(t, config.RiskPolicy{GateHigh: true, AutoMergeLow: true})
	assessedRisk(d, id, 1)
	d.Store.Update(id, func(r *core.Record) {
		r.Risk = &core.RiskLook{Commit: headIn(t, wt), Signals: []core.RiskSignal{{Kind: core.SignalPath, Level: core.RiskBlock, Text: "Touches a protected path: .github/**", Files: []string{".github/ci.yml"}}}}
	})
	r, _ := d.Store.Get(id)
	if d.MergeReady(r) || !strings.Contains(d.Blocked(r), "protected paths: .github/ci.yml") || d.RiskView(r).Policy != "block" {
		t.Fatalf("ready %v, blocked %q, view %+v", d.MergeReady(r), d.Blocked(r), d.RiskView(r))
	}
	if err := d.MergeReview(context.Background(), id); err == nil || !strings.Contains(err.Error(), "protected paths") {
		t.Fatalf("merged: %v", err)
	}
	d.AutoMerge(context.Background())
	if mrState(f) != "opened" {
		t.Fatal("merged by itself")
	}
}

// Auto-merge: a low risk read at the branch's HEAD, a clean reviewer at it and a green CI merge the MR by itself,
// once; a red CI, the reviewer's remarks, a medium risk, a diff not read at HEAD or the policy off do not.
func TestAutoMerge(t *testing.T) {
	d, id, wt, f := riskSetup(t, config.RiskPolicy{GateHigh: true, AutoMergeLow: true})
	ctx := context.Background()
	head := headIn(t, wt)
	ready := func() {
		assessedRisk(d, id, 2)
		d.Store.Update(id, func(r *core.Record) {
			r.Risk = &core.RiskLook{Commit: head}
			r.AgentReviews = []core.AgentReview{{At: now(), Commit: head, State: "clean"}}
		})
		d.reviews[id] = reviewLook{at: time.Now(), check: &trackers.ReviewCheck{State: "open", CI: "success", Mergeable: true}}
	}
	for _, c := range []struct {
		name string
		prep func()
	}{
		{"CI red", func() { d.reviews[id].check.CI = "failed" }},
		{"CI running", func() { d.reviews[id].check.CI = "running" }},
		{"reviewer has remarks", func() {
			d.Store.Update(id, func(r *core.Record) {
				r.AgentReviews[0].State, r.AgentReviews[0].Notes = "notes", "- `pay.go`: no test"
			})
		}},
		{"reviewer at an older commit", func() { d.Store.Update(id, func(r *core.Record) { r.AgentReviews[0].Commit = "0ld" }) }},
		{"no reviewer", func() { d.Store.Update(id, func(r *core.Record) { r.AgentReviews = nil }) }},
		{"medium risk", func() { assessedRisk(d, id, 3) }},
		{"a medium signal", func() {
			d.Store.Update(id, func(r *core.Record) {
				r.Risk.Signals = []core.RiskSignal{{Kind: core.SignalDeps, Level: core.RiskMedium}}
			})
		}},
		{"diff read before HEAD", func() { d.Store.Update(id, func(r *core.Record) { r.Risk.Commit = "0ld" }) }},
		{"conflicts", func() { d.reviews[id].check.Conflicts = true }},
		{"policy off", func() {
			for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
				r.Risk.AutoMergeLow = false
			}
		}},
	} {
		ready()
		c.prep()
		d.AutoMerge(ctx)
		if mrState(f) != "opened" {
			t.Fatalf("%s: merged by itself", c.name)
		}
		for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
			r.Risk.AutoMergeLow = true
		}
	}
	if len(d.autoMerged) != 0 {
		t.Fatalf("a refused merge was tried: %v", d.autoMerged)
	}

	journalN := func(text string) (n int) {
		for _, l := range d.Logs(id) {
			if strings.Contains(l.Text, text) {
				n++
			}
		}
		return n
	}
	setCI := func(status string) {
		f.mu.Lock()
		f.mrs[0]["head_pipeline"] = map[string]any{"id": 2, "status": status}
		f.mu.Unlock()
	}

	// The host refuses the merge: that HEAD is not tried again (the journal says it once), a new commit is.
	f.mu.Lock()
	f.mergeRefused = true
	f.mu.Unlock()
	ready()
	d.AutoMerge(ctx)
	f.mu.Lock()
	f.mergeRefused = false
	f.mu.Unlock()
	ready()
	d.AutoMerge(ctx)
	if mrState(f) != "opened" || d.autoMerged[id] != head || journalN("MR not merged by itself") != 1 {
		t.Fatalf("a refused merge: %v, %v, journal %+v", mrState(f), d.autoMerged, d.Logs(id))
	}
	// Once the MR is not open (merged elsewhere) its entry goes, so the map does not grow with every task.
	d.Store.Update(id, func(r *core.Record) { r.ReviewState = "merged" })
	d.AutoMerge(ctx)
	d.Store.Update(id, func(r *core.Record) { r.ReviewState = "" })
	if len(d.autoMerged) != 0 {
		t.Fatalf("kept after the merge: %v", d.autoMerged)
	}

	// CI runs again since the card looked (a push a moment ago): the fresh look says wait, which is no refusal: the
	// HEAD is not marked, nothing is faulted, and once CI is green the next round merges.
	setCI("running")
	ready()
	d.AutoMerge(ctx)
	if mrState(f) != "opened" || len(d.autoMerged) != 0 || journalN("MR not merged by itself") != 1 {
		t.Fatalf("waited for CI: %v, %v, journal %+v", mrState(f), d.autoMerged, d.Logs(id))
	}
	setCI("failed")
	ready()
	d.AutoMerge(ctx)
	if mrState(f) != "opened" || len(d.autoMerged) != 0 {
		t.Fatalf("a red CI: %v, %v", mrState(f), d.autoMerged)
	}
	setCI("success")
	ready()
	r, _ := d.Store.Get(id)
	if v := d.RiskView(r); v.Level != core.RiskLow || v.Policy != "auto" {
		t.Fatalf("view %+v", v)
	}
	d.AutoMerge(ctx)
	if r, _ = d.Store.Get(id); mrState(f) != "merged" || r.MergedBy != "auto" || r.RiskAtMerge != core.RiskLow {
		t.Fatalf("MR %v, by %q, risk %q", mrState(f), r.MergedBy, r.RiskAtMerge)
	}
	if !journalHas(d, id, "MR merged by itself") {
		t.Fatalf("journal %+v", d.Logs(id))
	}
}

// The reviewer agent starts by itself on a high risk and on a low one that may merge by itself, once per HEAD; not
// on a medium one, and not again after you skipped it.
func TestAutoRiskReview(t *testing.T) {
	d, id, _, _ := riskSetup(t, config.RiskPolicy{GateHigh: true})
	old := ReviewBranch
	t.Cleanup(func() { ReviewBranch = old })
	var calls atomic.Int32
	ReviewBranch = func(context.Context, string, string, core.Task, core.Route, []core.Remark) (string, error) {
		calls.Add(1)
		return "", nil
	}
	reviews := func() int { r, _ := d.Store.Get(id); return len(r.AgentReviews) }
	assessedRisk(d, id, 3)
	d.AutoRiskReview()
	if reviews() != 0 {
		t.Fatal("reviewed a medium risk")
	}
	assessedRisk(d, id, 5)
	lookAgain(d, id)
	if r, _ := d.Store.Get(id); d.ReviewStep(r) != "ready" {
		t.Fatalf("step %q", d.ReviewStep(r))
	}
	d.AutoRiskReview()
	settle(t, d, id)
	waitFor(t, func() bool {
		r, _ := d.Store.Get(id)
		return len(r.AgentReviews) == 1 && r.AgentReviews[0].State == "clean"
	}, "the reviewer did not run")
	d.AutoRiskReview()
	settle(t, d, id)
	if reviews() != 1 || calls.Load() != 1 || !journalHas(d, id, "reviewer agent started by itself: high risk") {
		t.Fatalf("reviews %d, calls %d", reviews(), calls.Load())
	}

	// A review you skipped is not started over you.
	d2, id2, _, _ := riskSetup(t, config.RiskPolicy{GateHigh: true})
	assessedRisk(d2, id2, 5)
	if err := d2.SkipReview(id2); err != nil {
		t.Fatal(err)
	}
	lookAgain(d2, id2)
	d2.AutoRiskReview()
	settle(t, d2, id2)
	if r, _ := d2.Store.Get(id2); calls.Load() != 1 || len(r.AgentReviews) != 1 || r.AgentReviews[0].State != "skipped" {
		t.Fatalf("started over a skip: calls %d, reviews %+v", calls.Load(), r.AgentReviews)
	}
}

// The round reads the diff again once the branch has new commits, and the assessment reads it with its rating.
func TestCheckRisk(t *testing.T) {
	d, id, wt, _ := riskSetup(t, config.RiskPolicy{GateHigh: true})
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		r.RiskPaths = []config.RiskPath{{Pattern: "migrations/**", Level: core.RiskHigh}}
	}
	d.CheckRisk(context.Background())
	r, _ := d.Store.Get(id)
	if r.Risk == nil || r.Risk.Commit != headIn(t, wt) || len(r.Risk.Signals) != 0 {
		t.Fatalf("risk %+v", r.Risk)
	}
	write(t, wt, "migrations/001.sql", "create table t();\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "migration")
	d.CheckRisk(context.Background())
	if r, _ = d.Store.Get(id); r.Risk.Commit != headIn(t, wt) || r.Risk.Level() != core.RiskHigh {
		t.Fatalf("after a commit: %+v", r.Risk)
	}
	if !journalHas(d, id, "change risk from the diff: high") {
		t.Fatalf("journal %+v", d.Logs(id))
	}
	if v := d.RiskView(r); v.Level != core.RiskHigh || v.Stale {
		t.Fatalf("view %+v", v)
	}
	// A commit the round has not read yet: the card says the risk is of an older diff.
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "More")
	lookAgain(d, id)
	if v := d.RiskView(r); !v.Stale {
		t.Fatalf("not stale: %+v", v)
	}
}

// A reviewer that cannot start by itself is tried again in the next round, and the journal says why once per HEAD.
func TestAutoRiskReviewFailureSaidOnce(t *testing.T) {
	d, id, wt, _ := riskSetup(t, config.RiskPolicy{GateHigh: true})
	assessedRisk(d, id, 5)
	lookAgain(d, id)
	// A round already running for the task: the start is refused.
	d.mu.Lock()
	d.reviewing[id] = true
	d.mu.Unlock()
	said := func() (n int) {
		for _, l := range d.Logs(id) {
			if strings.Contains(l.Text, "reviewer agent not started by itself") {
				n++
			}
		}
		return n
	}
	if r, _ := d.Store.Get(id); d.ReviewStep(r) != "ready" {
		t.Fatalf("step %q", d.ReviewStep(r))
	}
	for range 3 {
		d.AutoRiskReview()
	}
	if said() != 1 || d.autoReviewFailed[id] != headIn(t, wt) {
		t.Fatalf("said %d times: %+v", said(), d.Logs(id))
	}
	// Not ready is not asked about at all, and the entry goes with the open MR.
	d.mu.Lock()
	delete(d.reviewing, id)
	d.mu.Unlock()
	d.Store.Update(id, func(r *core.Record) { r.ReviewState = "closed" })
	d.AutoRiskReview()
	if len(d.autoReviewFailed) != 0 {
		t.Fatalf("kept after the MR closed: %v", d.autoReviewFailed)
	}
}

// The history signal counts the commits that began as a revert or a hotfix, not merges of a hotfix branch and not
// a mention of the word.
func TestRiskHistoryIgnoresMentions(t *testing.T) {
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "pay.go", "package app\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	touch := func(subject string) {
		write(t, dir, "pay.go", "package app\n// "+subject+"\n")
		git(t, dir, "commit", "-q", "-am", subject)
	}
	touch("Document the hotfix process")
	touch("Add rollback notes to pay")
	git(t, dir, "checkout", "-q", "-b", "hotfix/pay")
	touch("Plain change on a hotfix branch")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "Merge branch 'hotfix/pay'", "hotfix/pay")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, dir, "worktree", "add", "-q", "-b", "b/pay", wt, "main")
	files := []gitx.ChangedFile{{Path: "pay.go", Status: "M", Additions: 1}}
	if s := historySignal(context.Background(), wt, "main", files); s != nil {
		t.Fatalf("mentions and merges counted: %+v", s)
	}
	touch("Revert \"Pay with cards\"")
	touch("[Hotfix] pay twice")
	git(t, wt, "merge", "-q", "main")
	s := historySignal(context.Background(), wt, "main", files)
	if s == nil || s.Level != core.RiskMedium || !strings.Contains(s.Text, "2 reverts") {
		t.Fatalf("two real ones: %+v", s)
	}
}

// A commit after the round's look that touches a protected path stops the merge at once: the merge reads the risk
// of the branch as it is, not the board's older look; the same for a risk raised to high and its gate.
func TestMergeReadsFreshRisk(t *testing.T) {
	d, id, wt, f := riskSetup(t, config.RiskPolicy{GateHigh: true})
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		r.RiskPaths = []config.RiskPath{{Pattern: ".github/**", Level: core.RiskBlock}, {Pattern: "migrations/**", Level: core.RiskHigh}}
	}
	assessedRisk(d, id, 1)
	d.CheckRisk(context.Background())
	if r, _ := d.Store.Get(id); r.Risk == nil || r.Risk.Level() != "" || !d.MergeReady(r) {
		t.Fatalf("before: %+v", r.Risk)
	}
	write(t, wt, ".github/ci.yml", "on: push\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "ci")
	// No CheckRisk since the commit: the record still says nothing is protected.
	if r, _ := d.Store.Get(id); r.Risk.Level() != "" {
		t.Fatalf("read early: %+v", r.Risk)
	}
	err := d.MergeReview(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "protected paths: .github/ci.yml") || mrState(f) != "opened" {
		t.Fatalf("merged: %v, %v", err, mrState(f))
	}
	if r, _ := d.Store.Get(id); r.Risk.Level() != core.RiskBlock {
		t.Fatalf("the risk was not kept: %+v", r.Risk)
	}

	// A commit that makes the risk high: its gate holds the merge, with no round in between.
	d2, id2, wt2, f2 := riskSetup(t, config.RiskPolicy{GateHigh: true})
	for _, r := range d2.Config().Trackers[0].Projects["grp/app"].Repos {
		r.RiskPaths = []config.RiskPath{{Pattern: "migrations/**", Level: core.RiskHigh}}
	}
	assessedRisk(d2, id2, 1)
	d2.CheckRisk(context.Background())
	write(t, wt2, "migrations/001.sql", "create table t();\n")
	git(t, wt2, "add", "-A")
	git(t, wt2, "commit", "-q", "-m", "migration")
	if err := d2.MergeReview(context.Background(), id2); err == nil || !strings.Contains(err.Error(), "check it yourself") || mrState(f2) != "opened" {
		t.Fatalf("merged a high risk: %v, %v", err, mrState(f2))
	}
}
