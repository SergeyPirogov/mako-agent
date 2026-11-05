package core

import "testing"

func ok(b bool) *bool { return &b }

func TestSummarizeEval(t *testing.T) {
	run := EvalRun{Results: []EvalResult{
		{TaskID: "a", State: EvalPassed, ChecksOK: ok(true), OracleOK: ok(true), Must: 0, Should: 2, Cost: 1, Seconds: 100},
		{TaskID: "b", State: EvalFailed, ChecksOK: ok(true), OracleOK: ok(false), Must: 1, Should: 1, Cost: 3, Seconds: 300},
		{TaskID: "c", State: EvalPassed, Cost: 2, Seconds: 200},
		{TaskID: "d", State: EvalError, Error: "setup failed", Cost: 0.5},
		{TaskID: "e", State: EvalSkipped},
	}}
	s := SummarizeEval(run)
	if s.Cases != 5 || s.Ran != 3 || s.Passed != 2 || s.Errors != 1 {
		t.Errorf("counts %+v", s)
	}
	if s.SuccessRate < 0.66 || s.SuccessRate > 0.67 {
		t.Errorf("success rate %v", s.SuccessRate)
	}
	if s.MedianCost != 2 || s.MedianSeconds != 200 || s.Must != 1 || s.Should != 3 || s.Cost != 6.5 {
		t.Errorf("summary %+v", s)
	}
	if (SummarizeEval(EvalRun{}) != EvalSummary{}) {
		t.Error("an empty run must sum to nothing")
	}
}

// A result the reviewer did not read has no remarks to count: not as "no blockers" in the sums, not as "better".
func TestEvalUnreviewed(t *testing.T) {
	run := EvalRun{Results: []EvalResult{
		{TaskID: "a", State: EvalPassed, Must: 2, Should: 1},
		{TaskID: "b", State: EvalPassed, ReviewError: "the reviewer failed"},
		{TaskID: "c", State: EvalFailed, ReviewError: "cost ceiling reached", Must: 0},
	}}
	s := SummarizeEval(run)
	if s.Ran != 3 || s.Must != 2 || s.Should != 1 || s.Unreviewed != 2 {
		t.Errorf("summary %+v", s)
	}
	base := EvalRun{Results: []EvalResult{{TaskID: "a", State: EvalPassed, Must: 2}, {TaskID: "b", State: EvalPassed, Must: 3}, {TaskID: "c", State: EvalPassed, Must: 1}}}
	variant := EvalRun{Results: []EvalResult{{TaskID: "a", State: EvalPassed, ReviewError: "x"}, {TaskID: "b", State: EvalPassed, Must: 0}, {TaskID: "c", State: EvalFailed, ReviewError: "x"}}}
	want := map[string]string{"a": "same", "b": "better", "c": "worse"} // a: no remarks to compare; c: fails now
	for _, d := range CompareEval(base, variant) {
		if d.Change != want[d.TaskID] {
			t.Errorf("%s: %q, want %q", d.TaskID, d.Change, want[d.TaskID])
		}
	}
	// And the other way round: the baseline unread.
	for _, d := range CompareEval(variant, base) {
		if d.TaskID == "a" && d.Change != "same" {
			t.Errorf("a, baseline unread: %q", d.Change)
		}
	}
}

func TestEvalResultJudge(t *testing.T) {
	cases := []struct {
		checks, oracle *bool
		want           string
	}{
		{nil, nil, EvalPassed},
		{ok(true), nil, EvalPassed},
		{ok(true), ok(true), EvalPassed},
		{ok(false), ok(true), EvalFailed},
		{ok(true), ok(false), EvalFailed},
	}
	for _, c := range cases {
		r := EvalResult{ChecksOK: c.checks, OracleOK: c.oracle}
		r.Judge()
		if r.State != c.want {
			t.Errorf("checks %v oracle %v: %s, want %s", c.checks, c.oracle, r.State, c.want)
		}
	}
}

func TestCompareEval(t *testing.T) {
	base := EvalRun{Results: []EvalResult{
		{TaskID: "a", State: EvalPassed},
		{TaskID: "b", State: EvalFailed},
		{TaskID: "c", State: EvalPassed, Must: 2},
		{TaskID: "d", State: EvalPassed, Must: 0},
		{TaskID: "e", State: EvalPassed},
		{TaskID: "f", State: EvalError},
	}}
	variant := EvalRun{Results: []EvalResult{
		{TaskID: "a", State: EvalFailed},
		{TaskID: "b", State: EvalPassed},
		{TaskID: "c", State: EvalPassed, Must: 1},
		{TaskID: "d", State: EvalPassed, Must: 1},
		{TaskID: "e", State: EvalPassed},
		{TaskID: "f", State: EvalPassed},
		{TaskID: "g", State: EvalPassed},
	}}
	want := map[string]string{"a": "worse", "b": "better", "c": "better", "d": "worse", "e": "same", "f": "", "g": ""}
	got := CompareEval(base, variant)
	if len(got) != len(want) {
		t.Fatalf("deltas %+v", got)
	}
	for i, d := range got {
		if d.Change != want[d.TaskID] {
			t.Errorf("%s: %q, want %q", d.TaskID, d.Change, want[d.TaskID])
		}
		if i < 6 && d.TaskID != base.Results[i].TaskID {
			t.Errorf("order: %d is %s", i, d.TaskID)
		}
	}
	if got[6].Base != nil || got[6].Variant == nil {
		t.Errorf("a task only the variant ran: %+v", got[6])
	}
}

func TestIsTestFile(t *testing.T) {
	for p, want := range map[string]bool{
		"go/internal/core/eval_test.go":   true,
		"go/internal/core/eval.go":        false,
		"go/internal/web/testdata/a.json": true,
		"src/app.test.ts":                 true,
		"src/app.spec.js":                 true,
		"src/__tests__/app.js":            true,
		"tests/test_api.py":               true,
		"pkg/test_api.py":                 true,
		"pkg/api_test.py":                 true,
		"spec/models/user_spec.rb":        true,
		"docs/testing.md":                 false,
		"src/contest.ts":                  false,
		"README.md":                       false,
	} {
		if IsTestFile(p) != want {
			t.Errorf("%s: %v", p, !want)
		}
	}
}

func TestEvalVariantBaseline(t *testing.T) {
	if !(EvalVariant{Name: "current"}).Baseline() {
		t.Error("a variant with a name only is the baseline")
	}
	if (EvalVariant{Model: "sonnet"}).Baseline() || (EvalVariant{ClaudeMD: "x"}).Baseline() || (EvalVariant{Instructions: "x"}).Baseline() {
		t.Error("a variant that changes something is not the baseline")
	}
}
