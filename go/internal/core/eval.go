package core

import (
	"path"
	"strings"
)

// Eval: the agent's settings (the route's model and effort, CLAUDE.md, the end of its prompt) checked the way code
// is: merged tasks of a repository are done again by an agent with no person, in a worktree of their own, and what
// came of it is compared with a run on the current settings.

// EvalFile is a file of the task's tests as the merged MR left it; Deleted: the MR removed it.
type EvalFile struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

// EvalCase is a merged task kept for eval runs: what it asked for, the commit it started from and what its MR
// changed; the tests it added or changed are the oracle the agent's result is checked with.
type EvalCase struct {
	TaskID string `json:"taskId"`
	Task   Task   `json:"task"`
	Size   string `json:"size,omitempty"` // small, medium or big; "" unknown
	PRURL  string `json:"prUrl,omitempty"`
	// Base: the commit the task's branch started from; Head: its MR's last commit; Ref: the clone's ref that keeps
	// Head (refs/mako/eval/…) once the branch is gone.
	Base string `json:"base"`
	Head string `json:"head"`
	Ref  string `json:"ref"`
	// What the merged MR changed, the size the agent's result is compared with.
	Added   int `json:"added"`
	Removed int `json:"removed"`
	Files   int `json:"files"`
	// Tests: the oracle, the test files the MR added, changed or removed.
	Tests []EvalFile `json:"tests,omitempty"`
	// TestsCut: the oracle was over the limit, so some test files are left out.
	TestsCut bool `json:"testsCut,omitempty"`
	// What the task cost and took when it was done: from the hand-off to the MR.
	Cost    float64 `json:"cost,omitempty"`
	Seconds int     `json:"seconds,omitempty"`
	AddedAt string  `json:"addedAt"`
}

// EvalVariant is the settings a run goes with; all empty: the current ones (the baseline).
type EvalVariant struct {
	Name   string `json:"name,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// ClaudeMD: the CLAUDE.md the agent gets instead of the repository's; ClaudeMDFrom: where it was read from (a
	// branch or a file), to show.
	ClaudeMD     string `json:"claudeMd,omitempty"`
	ClaudeMDFrom string `json:"claudeMdFrom,omitempty"`
	// Instructions: the end of the agent's prompt instead of the executor's.
	Instructions string `json:"instructions,omitempty"`
}

// Baseline: the variant changes nothing.
func (v EvalVariant) Baseline() bool {
	return v.Model == "" && v.Effort == "" && v.ClaudeMD == "" && v.Instructions == ""
}

// Eval run and result states.
const (
	EvalRunning = "running"
	EvalDone    = "done"
	EvalStopped = "stopped"
	EvalFailed  = "failed" // the result: the agent's work was measured and did not pass
	EvalPassed  = "passed"
	EvalError   = "error"   // the result: the case could not be run (setup, Claude, git)
	EvalSkipped = "skipped" // the result: the run stopped before the case
)

// EvalResult is what one case of a run came to.
type EvalResult struct {
	TaskID  string `json:"taskId"`
	State   string `json:"state"` // running, passed, failed, error, skipped
	Error   string `json:"error,omitempty"`
	Planned bool   `json:"planned,omitempty"`
	// Route: the model and the effort the agent worked with ("sonnet · high").
	Route string `json:"route,omitempty"`
	// ChecksOK: the repository's checks over the agent's result; OracleOK: the task's own tests laid over it.
	// nil: the repository has no checks.
	ChecksOK *bool         `json:"checksOk,omitempty"`
	OracleOK *bool         `json:"oracleOk,omitempty"`
	Checks   []CheckResult `json:"checks,omitempty"`
	Oracle   []CheckResult `json:"oracle,omitempty"`
	// The reviewer's remarks on the agent's result. ReviewError: the reviewer did not read it (it failed, or the
	// ceiling left it no budget): then no remarks were counted, and zero of them says nothing.
	Must        int    `json:"must"`
	Should      int    `json:"should"`
	Review      string `json:"review,omitempty"`
	ReviewError string `json:"reviewError,omitempty"`
	// What the agent changed, what it cost and how long the case took.
	Added   int     `json:"added"`
	Removed int     `json:"removed"`
	Files   int     `json:"files"`
	Cost    float64 `json:"cost"`
	Seconds int     `json:"seconds"`
}

// Reviewed: the reviewer read the result, so Must and Should count its remarks.
func (r EvalResult) Reviewed() bool { return r.ReviewError == "" }

// Passed: the agent's result passes the checks and the task's tests (those the repository has).
func (r EvalResult) Passed() bool {
	return r.State == EvalPassed
}

// Judge sets the result's state from its checks: passed when none of them failed.
func (r *EvalResult) Judge() {
	if (r.ChecksOK == nil || *r.ChecksOK) && (r.OracleOK == nil || *r.OracleOK) {
		r.State = EvalPassed
	} else {
		r.State = EvalFailed
	}
}

// EvalRun is one run of a repository's eval set with a variant.
type EvalRun struct {
	ID         string       `json:"id"`
	Variant    EvalVariant  `json:"variant"`
	MaxCost    float64      `json:"maxCost"`
	State      string       `json:"state"` // running, done, stopped
	Reason     string       `json:"reason,omitempty"`
	StartedAt  string       `json:"startedAt"`
	FinishedAt string       `json:"finishedAt,omitempty"`
	Spent      float64      `json:"spent"`
	Results    []EvalResult `json:"results"`
}

// Result: the run's result of the task, nil when it has none.
func (r EvalRun) Result(taskID string) *EvalResult {
	for i := range r.Results {
		if r.Results[i].TaskID == taskID {
			return &r.Results[i]
		}
	}
	return nil
}

// EvalSummary is a run in numbers, for the card.
type EvalSummary struct {
	Cases  int `json:"cases"`  // in the run
	Ran    int `json:"ran"`    // measured: passed or failed
	Passed int `json:"passed"` // of Ran
	Errors int `json:"errors"`
	// SuccessRate: Passed of Ran, 0..1.
	SuccessRate   float64 `json:"successRate"`
	MedianCost    float64 `json:"medianCost"`
	MedianSeconds float64 `json:"medianSeconds"`
	// Must and Should: the reviewer's remarks, over the results it read; Unreviewed: the measured results it did not.
	Must       int     `json:"must"`
	Should     int     `json:"should"`
	Unreviewed int     `json:"unreviewed"`
	Cost       float64 `json:"cost"`
}

// SummarizeEval: the run's success rate, the medians of what a case cost and took, the reviewer's remarks.
func SummarizeEval(run EvalRun) EvalSummary {
	s := EvalSummary{Cases: len(run.Results)}
	var costs, secs []float64
	for _, r := range run.Results {
		s.Cost += r.Cost
		switch r.State {
		case EvalPassed, EvalFailed:
		case EvalError:
			s.Errors++
			continue
		default:
			continue
		}
		s.Ran++
		if r.Passed() {
			s.Passed++
		}
		if r.Reviewed() {
			s.Must += r.Must
			s.Should += r.Should
		} else {
			s.Unreviewed++
		}
		costs = append(costs, r.Cost)
		secs = append(secs, float64(r.Seconds))
	}
	if s.Ran > 0 {
		s.SuccessRate = float64(s.Passed) / float64(s.Ran)
	}
	s.MedianCost, s.MedianSeconds = median(costs), median(secs)
	return s
}

// EvalDelta is how a task went in the variant against the baseline: "better", "worse", "same" or "" (one of the
// runs did not measure it).
type EvalDelta struct {
	TaskID  string      `json:"taskId"`
	Change  string      `json:"change"`
	Base    *EvalResult `json:"base,omitempty"`
	Variant *EvalResult `json:"variant,omitempty"`
}

func measured(r *EvalResult) bool {
	return r != nil && (r.State == EvalPassed || r.State == EvalFailed)
}

// CompareEval: each task of either run, the baseline's order first, and whether the variant did it better: it
// passes where the baseline did not, or passes as well with fewer blockers from the reviewer.
func CompareEval(base, variant EvalRun) []EvalDelta {
	var out []EvalDelta
	seen := map[string]bool{}
	add := func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		d := EvalDelta{TaskID: id, Base: base.Result(id), Variant: variant.Result(id)}
		if measured(d.Base) && measured(d.Variant) {
			switch b, v := d.Base, d.Variant; {
			case v.Passed() && !b.Passed():
				d.Change = "better"
			case b.Passed() && !v.Passed():
				d.Change = "worse"
			case !b.Reviewed() || !v.Reviewed(): // no remarks to compare: zero is not "no blockers"
				d.Change = "same"
			case v.Must < b.Must:
				d.Change = "better"
			case v.Must > b.Must:
				d.Change = "worse"
			default:
				d.Change = "same"
			}
		}
		out = append(out, d)
	}
	for _, r := range base.Results {
		add(r.TaskID)
	}
	for _, r := range variant.Results {
		add(r.TaskID)
	}
	return out
}

// IsTestFile: the path is a test, by the usual names: Go's _test.go and testdata, test and spec files of JS/TS,
// Python's test_*.py, folders named test, tests, spec or __tests__.
func IsTestFile(p string) bool {
	p = strings.ToLower(path.Clean("/" + p))
	for _, dir := range []string{"/test/", "/tests/", "/spec/", "/__tests__/", "/testdata/"} {
		if strings.Contains(p, dir) {
			return true
		}
	}
	name := path.Base(p)
	switch {
	case strings.HasSuffix(name, "_test.go"), strings.HasSuffix(name, "_test.py"), strings.HasSuffix(name, "_spec.rb"):
		return true
	case strings.HasPrefix(name, "test_") && strings.HasSuffix(name, ".py"):
		return true
	case strings.Contains(name, ".test."), strings.Contains(name, ".spec."):
		return true
	}
	return false
}
